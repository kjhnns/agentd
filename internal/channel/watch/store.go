// Package watch is agentd's WRIST channel: the transport behind the Apple
// Watch (and any other thin, mostly-offline client). It is deliberately the
// opposite of the web channel's live WebSocket: the client POSTs one message
// and gets an immediate receipt (the transcript of what it delegated), then
// fetches the conversation with a cheap long-poll whenever it is awake. Every
// reply, failure notice and status change is a durable entry in a small
// append-only store, so a client that was asleep for an hour reads what it
// missed instead of losing it. The channel rides session.RouteInbound like
// Telegram does; nothing here talks to the harness directly.
package watch

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kjhnns/agentd/internal/channel"
)

// Message is one entry of the wrist conversation. A user message carries the
// lifecycle Status of the turn it started (mapped from the channel reaction
// chain, see Adapter.Ack); an agent or system message never has a status.
type Message struct {
	Seq       int64     `json:"seq"`
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	Role      string    `json:"role"` // user | agent | system
	Kind      string    `json:"kind"` // voice | text | reply | failure | notice
	Text      string    `json:"text"`
	Status    string    `json:"status,omitempty"` // user only: queued | working | needs_input | done | failed
	ReplyTo   string    `json:"reply_to,omitempty"`
	DurationS int       `json:"duration_s,omitempty"`
	// Replies are the agent's proposed short answers to this message (agent
	// role, last message of a reply only). Old rows load without it.
	Replies   []string  `json:"replies,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// RequestID is the client's idempotency key (its outbox item id) on a
	// user message (D5). A repeat of the same request answers this message.
	RequestID string `json:"request_id,omitempty"`
	// Inbound is the full inbound payload of a user message, persisted at
	// admission so a restart can re-enqueue a queued request. Never served to
	// clients (see Public).
	Inbound *channel.InboundMsg `json:"inbound,omitempty"`
	// Evicted marks a snapshot that compaction kept ONLY for the request_id
	// index (D5b): replay indexes it but does not put it back into the
	// conversation.
	Evicted bool `json:"evicted,omitempty"`
}

// Public is the message as clients see it: without the persisted inbound
// payload (media paths stay on the server).
func (m Message) Public() Message {
	m.Inbound = nil
	m.Evicted = false
	return m
}

// publicAll maps Public over a page.
func publicAll(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m.Public()
	}
	return out
}

// Roles, kinds and statuses. Strings on the wire so the client stays trivial.
const (
	RoleUser   = "user"
	RoleAgent  = "agent"
	RoleSystem = "system"

	KindVoice   = "voice"
	KindText    = "text"
	KindReply   = "reply"
	KindFailure = "failure"
	KindNotice  = "notice"
	KindSummary = "summary" // the short half of a split reply; the long half precedes it

	StatusQueued     = "queued"
	StatusWorking    = "working"
	StatusNeedsInput = "needs_input"
	StatusDone       = "done"
	StatusFailed     = "failed"
	// StatusRejected: the request was stored but the inbound queue was full.
	// It is retryable: a repeat with the same request_id re-admits it.
	StatusRejected = "rejected"
)

// Terminal reports whether a user-message status will not change again
// without a new request (a rejected request changes only on a retry).
func Terminal(status string) bool {
	return status == StatusDone || status == StatusFailed || status == StatusRejected
}

// inFlight reports a user-message status whose turn is not finished: such a
// message is never evicted by trim or compaction (D5b), so the restart
// reconciliation and the status updates always find it.
func inFlight(m Message) bool {
	return m.Role == RoleUser && (m.Status == StatusQueued || m.Status == StatusWorking || m.Status == StatusNeedsInput)
}

// Store is the append-only conversation log. Every append (a new message OR a
// status update, which is re-appended as a full snapshot with a fresh seq)
// gets a monotonically increasing seq, so a client that remembers the last
// seq it saw asks for "everything after N" and upserts by id. Persistence is
// one JSONL line per append, fsync'd, compacted to the latest snapshot per id
// when the file is reopened.
type Store struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	seq    int64
	log    []Message          // every append, seq ascending
	latest map[string]Message // newest snapshot per id
	order  []string           // ids in first-seen order
	wake   chan struct{}      // closed + replaced on every append (long-poll wakeup)
	// reqs is the request_id index (D5b): request id -> newest snapshot of
	// its message, kept when the message itself is evicted.
	reqs map[string]Message
	// failWrite is a test hook that fails the next persisted write.
	failWrite func() error

	maxLog    int // in-memory append log bound
	maxIDs    int // distinct messages kept
	compactAt int // JSONL lines that trigger a compaction on open
}

// Record and line bounds. One oversized record must never be able to take the
// whole store (and with it the channel) down: text above maxTextBytes is cut
// on append, and a line above maxLineBytes found on disk is skipped on replay
// instead of aborting it.
const (
	maxTextBytes = 256 << 10 // 256 KiB of message text is far beyond any real reply
	maxLineBytes = 8 << 20
)

// OpenStore opens (or creates) the JSONL store at path. An empty path gives a
// memory-only store (tests).
func OpenStore(path string) (*Store, error) { return openStore(path, 5000) }

func openStore(path string, compactAt int) (*Store, error) {
	return openStoreWith(path, compactAt, 500)
}

func openStoreWith(path string, compactAt, maxIDs int) (*Store, error) {
	s := &Store{
		path:      path,
		latest:    map[string]Message{},
		reqs:      map[string]Message{},
		wake:      make(chan struct{}),
		maxLog:    4000,
		maxIDs:    maxIDs,
		compactAt: compactAt,
	}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	lines, skipped, err := s.replay()
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		log.Printf("watch store: skipped %d unreadable or oversized line(s) in %s", skipped, path)
	}
	if lines > s.compactAt || skipped > 0 {
		// Compacting also drops the bad lines, so the next open is clean.
		if err := s.compact(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := repairTornTail(f); err != nil {
		f.Close()
		return nil, err
	}
	s.f = f
	return s, nil
}

// repairTornTail makes sure the file ends with a newline, so a record cut
// short by a crash cannot swallow the next append into one unreadable line.
func repairTornTail(f *os.File) error {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err != nil {
		// Opened write-only on some platforms: fall back to a read handle.
		rf, rerr := os.Open(f.Name())
		if rerr != nil {
			return rerr
		}
		defer rf.Close()
		if _, err := rf.ReadAt(buf, st.Size()-1); err != nil {
			return err
		}
	}
	if buf[0] != '\n' {
		_, err = f.Write([]byte("\n"))
	}
	return err
}

// replay loads the JSONL file into memory. Returns the number of lines read.
// replay loads the JSONL file into memory. It returns the number of records
// absorbed and the number of lines skipped. A bad line (torn, corrupt, or
// oversized) is skipped, never fatal: a bufio.Scanner would abort the whole
// replay on one long line, which used to take the channel down at startup.
func (s *Store) replay() (int, int, error) {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	n, skipped := 0, 0
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := readLine(r, maxLineBytes)
		if line != nil {
			var m Message
			if len(line) > maxLineBytes || json.Unmarshal(line, &m) != nil || m.ID == "" {
				skipped++
			} else {
				n++
				s.absorb(m)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, skipped, nil
			}
			return n, skipped, rerr
		}
	}
}

// readLine reads one newline-terminated line of any length. Bytes beyond
// limit are consumed and discarded but counted, so the caller can skip the
// line without the reader losing its place.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	over := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !over {
			out = append(out, chunk...)
			if len(out) > limit {
				over = true
				out = out[:limit+1] // keep it recognisably oversized, drop the rest
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(out) > 0 && out[len(out)-1] == '\n' {
			out = out[:len(out)-1]
		}
		if len(out) == 0 && err != nil {
			return nil, err
		}
		return out, err
	}
}

// absorb applies one replayed snapshot (caller holds no lock: open only).
func (s *Store) absorb(m Message) {
	if m.Seq > s.seq {
		s.seq = m.Seq
	}
	s.indexRequest(m)
	if m.Evicted {
		return // kept for the request index only
	}
	if _, seen := s.latest[m.ID]; !seen {
		s.order = append(s.order, m.ID)
	}
	s.latest[m.ID] = m
	s.log = append(s.log, m)
	s.trimLocked()
}

// compact rewrites the file with the newest snapshot per id only.
func (s *Store) compact() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	// The request index of messages no longer in the conversation goes first,
	// marked evicted, so dedupe survives compaction and restart (D5b).
	for _, m := range s.evictedRequests() {
		m.Evicted = true
		b, _ := json.Marshal(m)
		w.Write(b)
		w.WriteByte('\n')
	}
	for _, id := range s.order {
		b, _ := json.Marshal(s.latest[id])
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, s.path)
}

// Close releases the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Append stores a NEW message (an empty ID gets one), assigns its seq and
// returns the stored copy. A write error is logged; the message stays in
// memory (callers that must know use admit).
func (s *Store) Append(m Message) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		m.ID = newID()
	}
	return s.appendLocked(m)
}

func (s *Store) appendLocked(m Message) Message {
	out, err := s.appendDurable(m, false)
	if err != nil {
		log.Printf("watch store: append %s: %v", m.ID, err)
	}
	return out
}

// appendDurable writes the snapshot (fsync) and then applies it in memory.
// With strict, a write or fsync error leaves the store unchanged (memory and
// file) and is returned; otherwise the snapshot is applied anyway and the
// error only reported.
func (s *Store) appendDurable(m Message, strict bool) (Message, error) {
	if len(m.Text) > maxTextBytes {
		// Cut on a rune boundary and say so, rather than persist a record the
		// replay would have to skip (which would lose the message entirely).
		cut := []rune(m.Text)
		for len(string(cut)) > maxTextBytes-24 {
			cut = cut[:len(cut)*9/10]
		}
		m.Text = string(cut) + "\n\u2026 [truncated by agentd]"
	}
	now := time.Now().UTC()
	if m.TS.IsZero() {
		m.TS = now
	}
	m.UpdatedAt = now
	m.Seq = s.seq + 1
	m.Evicted = false
	werr := s.writeLocked(m)
	if werr != nil && strict {
		return Message{}, werr
	}
	s.seq = m.Seq
	if _, seen := s.latest[m.ID]; !seen {
		s.order = append(s.order, m.ID)
	}
	s.latest[m.ID] = m
	s.indexRequest(m)
	s.log = append(s.log, m)
	s.trimLocked()
	close(s.wake)
	s.wake = make(chan struct{})
	return m, werr
}

// writeLocked appends one JSONL line and fsyncs it. On a failed write the
// file is cut back to its previous size so a partial line cannot glue onto
// the next record.
func (s *Store) writeLocked(m Message) error {
	if s.failWrite != nil {
		if err := s.failWrite(); err != nil {
			return err
		}
	}
	if s.f == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var size int64 = -1
	if st, err := s.f.Stat(); err == nil {
		size = st.Size()
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		if size >= 0 {
			_ = s.f.Truncate(size)
		}
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	return nil
}

// indexRequest records the newest snapshot of a request's message.
func (s *Store) indexRequest(m Message) {
	if m.RequestID == "" || m.Role != RoleUser {
		return
	}
	if cur, ok := s.reqs[m.RequestID]; ok && cur.Seq > m.Seq {
		return
	}
	s.reqs[m.RequestID] = m
}

// maxEvictedRequests bounds the request index entries kept for messages that
// left the conversation (newest first).
const maxEvictedRequests = 2000

// evictedRequests returns the indexed snapshots whose message is no longer
// in the conversation, newest maxEvictedRequests, oldest first.
func (s *Store) evictedRequests() []Message {
	out := []Message{}
	for _, m := range s.reqs {
		if _, live := s.latest[m.ID]; !live {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > maxEvictedRequests {
		out = out[len(out)-maxEvictedRequests:]
	}
	return out
}

// trimLocked bounds memory: the append log and the distinct-id set. A user
// message whose turn is still in flight is never evicted (D5b); the request
// index keeps the snapshot of an evicted request.
func (s *Store) trimLocked() {
	if len(s.log) > s.maxLog {
		s.log = append([]Message(nil), s.log[len(s.log)-s.maxLog/2:]...)
	}
	if len(s.order) <= s.maxIDs {
		return
	}
	keep := make([]string, 0, len(s.order))
	excess := len(s.order) - s.maxIDs
	for _, id := range s.order {
		if excess > 0 && !inFlight(s.latest[id]) {
			delete(s.latest, id)
			excess--
			continue
		}
		keep = append(keep, id)
	}
	s.order = keep
	if len(s.reqs) > 2*maxEvictedRequests+s.maxIDs {
		// Bound the in-memory index too: drop the oldest evicted entries.
		ev := s.evictedRequests()
		keepEv := map[string]bool{}
		for _, m := range ev {
			keepEv[m.RequestID] = true
		}
		for rid, m := range s.reqs {
			if _, live := s.latest[m.ID]; !live && !keepEv[rid] {
				delete(s.reqs, rid)
			}
		}
	}
}

// Update applies fn to the newest snapshot of id and, when fn reports a
// change, re-appends it with a fresh seq (so pollers see the update).
func (s *Store) Update(id string, fn func(m *Message) bool) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.latest[id]
	if !ok {
		return Message{}, false
	}
	if !fn(&m) {
		return m, false
	}
	return s.appendLocked(m), true
}

// Get returns the newest snapshot of id.
func (s *Store) Get(id string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.latest[id]
	return m, ok
}

// clampLimit bounds a client-supplied page size.
func clampLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

// tailLocked returns the newest `limit` distinct messages (oldest first) and
// the index they start at in the conversation order.
func (s *Store) tailLocked(limit int) ([]Message, int) {
	start := 0
	if len(s.order) > limit {
		start = len(s.order) - limit
	}
	out := make([]Message, 0, len(s.order)-start)
	for _, id := range s.order[start:] {
		out = append(out, s.latest[id])
	}
	return out, start
}

// Tail returns the newest `limit` distinct messages (oldest first), whether
// OLDER messages remain in the store, and the current seq. This is the first
// page a client loads; it then pages backwards with Page and forwards with
// Since, so a long conversation is never sent in one piece.
func (s *Store) Tail(limit int) (msgs []Message, hasMore bool, latest int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, start := s.tailLocked(clampLimit(limit))
	return out, start > 0, s.seq
}

// Page returns up to limit messages that sit immediately BEFORE beforeID in
// conversation order (oldest first) plus whether older ones remain. The anchor
// is a message ID, not a seq, because a seq changes whenever that message's
// status is updated. An unknown or empty anchor yields the tail, so a client
// whose anchor has already been trimmed out of the store still gets a usable
// page instead of an error.
func (s *Store) Page(beforeID string, limit int) (msgs []Message, hasMore bool, latest int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit = clampLimit(limit)
	end := -1
	for i, id := range s.order {
		if id == beforeID {
			end = i
			break
		}
	}
	if end < 0 {
		out, start := s.tailLocked(limit)
		return out, start > 0, s.seq
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := make([]Message, 0, end-start)
	for _, id := range s.order[start:end] {
		out = append(out, s.latest[id])
	}
	return out, start > 0, s.seq
}

// Since returns the newest snapshot of every message touched after seq
// `after` (oldest change first, at most limit) and the cursor to ask from
// next. after <= 0 means "the tail of the conversation" (the last limit
// distinct messages). reset is true when after is older than what the
// in-memory log still covers: the caller gets the tail and must replace, not
// merge, its local copy. See SincePage for the paging contract.
func (s *Store) Since(after int64, limit int) (msgs []Message, cursor int64, reset bool) {
	msgs, cursor, reset, _ = s.SincePage(after, limit)
	return msgs, cursor, reset
}

// SincePage pages over the CHANGES after seq `after` (D3). Every message's
// newest snapshot carries the seq of its last change; the snapshots changed
// after `after` are sorted by that seq and the first limit are returned. The
// cursor is the seq of the last returned snapshot when more remain (more is
// true), else the store's high-water seq. A client that asks again from the
// cursor therefore never skips a change: the old code returned the global
// high-water seq even when the page was cut, and a message whose change sat
// behind the cut was never seen.
func (s *Store) SincePage(after int64, limit int) (msgs []Message, cursor int64, reset, more bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit = clampLimit(limit)
	if after <= 0 {
		out, _ := s.tailLocked(limit)
		return out, s.seq, false, false
	}
	if len(s.log) > 0 && after < s.log[0].Seq-1 {
		out, _ := s.tailLocked(limit)
		return out, s.seq, true, false
	}
	changed := []Message{}
	for _, id := range s.order {
		if m := s.latest[id]; m.Seq > after {
			changed = append(changed, m)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Seq < changed[j].Seq })
	if len(changed) > limit {
		return changed[:limit], changed[limit-1].Seq, false, true
	}
	return changed, s.seq, false, false
}

// Wait blocks until an append lands with seq > after, ctx ends, or d elapses.
func (s *Store) Wait(ctx context.Context, after int64, d time.Duration) {
	s.mu.Lock()
	if s.seq > after {
		s.mu.Unlock()
		return
	}
	wake := s.wake
	s.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-wake:
	case <-ctx.Done():
	case <-t.C:
	}
}

// Pending counts user messages whose turn has not reached a terminal status.
func (s *Store) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.order {
		m := s.latest[id]
		if m.Role == RoleUser && !Terminal(m.Status) {
			n++
		}
	}
	return n
}

// Attribute picks the user message an outbound reply belongs to: the oldest
// one still in flight (working / needs_input), else the most recently updated
// user message (the failure-notice case: its status already flipped to failed
// before the notice is sent).
func (s *Store) Attribute() (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recent Message
	found := false
	for _, id := range s.order {
		m := s.latest[id]
		if m.Role != RoleUser {
			continue
		}
		if m.Status == StatusWorking || m.Status == StatusNeedsInput {
			return m, true
		}
		if !found || m.UpdatedAt.After(recent.UpdatedAt) {
			recent, found = m, true
		}
	}
	return recent, found
}

// replayCount reads the JSONL file and returns its parseable line count
// (tests: proves compaction without poking at file internals).
func (s *Store) replayCount() (int, error) {
	probe := &Store{path: s.path, latest: map[string]Message{}, reqs: map[string]Message{}, wake: make(chan struct{}), maxLog: 1 << 20, maxIDs: 1 << 20}
	n, _, err := probe.replay()
	return n, err
}
