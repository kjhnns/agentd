package watch

// CONVERSATIONS (R4, plan section 4 and 4b).
//
// A conversation is a thread of the watch channel: an id, a title, its
// messages. "main" is the primary thread (the wrist, Telegram, old app
// builds); every other id is an iOS side conversation. Metadata lives in the
// message log as ONE kind "conv" record per conversation (id "conv-<id>"),
// re-appended on every create, rename, archive and unarchive, so the
// long-poll carries it and there is no second file. Trim never evicts a conv
// record and compaction keeps the latest snapshot of every id, so a quiet
// conversation keeps its title forever.
//
// Retention is per conversation (maxIDs each). Before trim evicts a message
// its latest snapshot is appended to watch-archive/<conv>.jsonl (0600,
// fsync). The archive index (message id -> archived seq) is exact, because
// eviction runs in insertion order, not seq order, and it is rebuilt from
// the archive files on open, so a repeated replay never archives a snapshot
// twice. A failed archive write stops the eviction.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ConvMain is the primary conversation (an absent conv).
const ConvMain = "main"

// convIDPattern is the shape of a client-made conversation id (a lowercased
// UUID in practice). It is checked before any path is built from an id.
var convIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidConvID reports whether id may name a side conversation ("main" is
// valid as a target but is not a side conversation).
func ValidConvID(id string) bool { return convIDPattern.MatchString(id) }

// convOf is a message's conversation ("main" when absent).
func convOf(m Message) string {
	if m.Conv == "" {
		return ConvMain
	}
	return m.Conv
}

// convRecordID is the message id of a conversation's record.
func convRecordID(id string) string { return "conv-" + id }

// Title bounds.
const (
	maxTitleRunes     = 120
	defaultTitleRunes = 60
	previewRunes      = 120
	// DefaultConvTitle is shown for a conversation without a title yet.
	DefaultConvTitle = "New conversation"
	mainTitle        = "Main"
)

// cleanTitle collapses whitespace and bounds a title.
func cleanTitle(t string, max int) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > max {
		t = string(r[:max])
	}
	return t
}

// ---- archive ----

// loadArchiveIndex rebuilds the archive index from watch-archive/*.jsonl.
func (s *Store) loadArchiveIndex() error {
	entries, err := os.ReadDir(s.archiveDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		conv := strings.TrimSuffix(name, ".jsonl")
		if !ValidConvID(conv) {
			continue
		}
		if err := s.indexArchiveFile(filepath.Join(s.archiveDir, name), conv); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) indexArchiveFile(path, conv string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := readLine(r, maxLineBytes)
		if len(line) > 0 && len(line) <= maxLineBytes {
			var m Message
			if json.Unmarshal(line, &m) == nil && m.ID != "" {
				if m.Seq > s.archived[m.ID] {
					s.archived[m.ID] = m.Seq
				}
				s.archivedConv[m.ID] = conv
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// archiveLocked appends m's snapshot to its conversation's archive (fsync)
// unless that exact snapshot is already there. Caller holds s.mu.
func (s *Store) archiveLocked(m Message) error {
	if s.archiveDir == "" {
		return nil // memory-only store (tests)
	}
	if s.archived[m.ID] >= m.Seq && m.Seq > 0 {
		return nil
	}
	if s.failArchive != nil {
		if err := s.failArchive(); err != nil {
			return err
		}
	}
	conv := convOf(m)
	if !ValidConvID(conv) {
		return fmt.Errorf("conversation id %q cannot name an archive file", conv)
	}
	if _, err := os.Stat(s.archiveDir); os.IsNotExist(err) {
		if err := os.MkdirAll(s.archiveDir, 0o700); err != nil {
			return err
		}
		// The new directory entry must be durable before anything in it
		// counts as archived.
		if err := syncDir(filepath.Dir(s.archiveDir)); err != nil {
			return err
		}
	}
	path := filepath.Join(s.archiveDir, conv+".jsonl")
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := repairTornTail(f); err != nil {
		f.Close()
		return err
	}
	m.Inbound = nil
	m.Evicted = false
	b, err := json.Marshal(m)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if os.IsNotExist(statErr) {
		// A new archive file: its directory entry must be durable before
		// the eviction (and a later compaction) drops the live snapshot.
		if err := syncDir(s.archiveDir); err != nil {
			return err
		}
	}
	s.archived[m.ID] = m.Seq
	s.archivedConv[m.ID] = conv
	return nil
}

// syncDir fsyncs a directory (its entries become durable).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readArchive returns the archived snapshots of one conversation.
func (s *Store) readArchive(conv string) ([]Message, error) {
	if s.archiveDir == "" || !ValidConvID(conv) {
		return nil, nil
	}
	f, err := os.Open(filepath.Join(s.archiveDir, conv+".jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Message
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, rerr := readLine(r, maxLineBytes)
		if len(line) > 0 && len(line) <= maxLineBytes {
			var m Message
			if json.Unmarshal(line, &m) == nil && m.ID != "" {
				out = append(out, m)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return out, nil
			}
			return out, rerr
		}
	}
}

// Transcript returns every message of a conversation, archive and store
// merged by id (the newest snapshot wins), in conversation order (by
// timestamp, then seq), without the conversation record. Used by the
// artifact export (plan section 5).
func (s *Store) Transcript(conv string) ([]Message, error) {
	s.mu.Lock()
	arch, err := s.readArchive(conv)
	byID := map[string]Message{}
	for _, m := range arch {
		if cur, ok := byID[m.ID]; !ok || m.Seq > cur.Seq {
			byID[m.ID] = m
		}
	}
	for _, id := range s.order {
		m := s.latest[id]
		if convOf(m) != conv {
			continue
		}
		if cur, ok := byID[m.ID]; !ok || m.Seq > cur.Seq {
			byID[m.ID] = m
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(byID))
	for _, m := range byID {
		if m.Kind == KindConv {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TS.Equal(out[j].TS) {
			return out[i].TS.Before(out[j].TS)
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// ---- conversation records ----

// ErrUnknownConv: no record of that conversation.
var ErrUnknownConv = errors.New("unknown conversation")

// ErrMainConv: main cannot be renamed or archived.
var ErrMainConv = errors.New("main cannot be renamed or archived")

// convRecordLocked returns the conversation record of id.
func (s *Store) convRecordLocked(id string) (Message, bool) {
	m, ok := s.latest[convRecordID(id)]
	return m, ok && m.Kind == KindConv
}

// ConvExists reports whether a conversation is known ("main" always is).
func (s *Store) ConvExists(id string) bool {
	if id == ConvMain {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.convRecordLocked(id)
	return ok
}

// ConvTitle returns the display title of a conversation.
func (s *Store) ConvTitle(id string) string {
	if id == ConvMain {
		return mainTitle
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.convRecordLocked(id); ok && r.Title != "" {
		return r.Title
	}
	return DefaultConvTitle
}

// ConvPatch is a change to a conversation record (nil = unchanged).
type ConvPatch struct {
	Title    *string
	Archived *bool
}

// UpsertConv applies a patch to a conversation record. With create an
// unknown id is created; without it an unknown id is ErrUnknownConv. "main"
// is ErrMainConv. The record is durable before it is visible.
func (s *Store) UpsertConv(id string, p ConvPatch, create bool) (ConvInfo, error) {
	if id == ConvMain {
		return ConvInfo{}, ErrMainConv
	}
	if !ValidConvID(id) {
		return ConvInfo{}, fmt.Errorf("bad conversation id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.convRecordLocked(id)
	if !ok && !create {
		return ConvInfo{}, ErrUnknownConv
	}
	changed := !ok
	if !ok {
		f := false
		rec = Message{ID: convRecordID(id), Role: RoleSystem, Kind: KindConv, Conv: id, Archived: &f}
	}
	if p.Title != nil {
		if t := cleanTitle(*p.Title, maxTitleRunes); t != "" && (t != rec.Title || !rec.TitleSet) {
			rec.Title, rec.TitleSet = t, true
			changed = true
		}
	}
	if p.Archived != nil && (rec.Archived == nil || *rec.Archived != *p.Archived) {
		v := *p.Archived
		rec.Archived = &v
		changed = true
	}
	if changed {
		if _, err := s.appendDurable(rec, true); err != nil {
			return ConvInfo{}, err
		}
	}
	return s.convInfoLocked(id), nil
}

// ensureConvLocked creates a side conversation's record when a message
// names it, and gives it the default title from its first user message
// unless a title was set by PUT/PATCH. Caller holds s.mu.
func (s *Store) ensureConvLocked(id, firstText string) error {
	if id == ConvMain {
		return nil
	}
	rec, ok := s.convRecordLocked(id)
	if !ok {
		f := false
		rec = Message{ID: convRecordID(id), Role: RoleSystem, Kind: KindConv, Conv: id, Archived: &f}
	}
	// The default title comes from the first user message ADMITTED: a
	// record left behind by an admission whose message write failed is
	// renamed by the next first message.
	title := rec.Title
	if !rec.TitleSet && !s.hasUserMessageLocked(id) {
		title = cleanTitle(firstText, defaultTitleRunes)
	}
	if ok && title == rec.Title {
		return nil
	}
	rec.Title = title
	_, err := s.appendDurable(rec, true)
	return err
}

// hasUserMessageLocked reports whether a conversation holds a user message
// (live, or evicted into the archive).
func (s *Store) hasUserMessageLocked(id string) bool {
	for _, oid := range s.order {
		if m := s.latest[oid]; m.Role == RoleUser && convOf(m) == id {
			return true
		}
	}
	for mid, c := range s.archivedConv {
		if c == id {
			if _, live := s.latest[mid]; !live {
				return true // archived messages of a side conversation imply a first message
			}
		}
	}
	return false
}

// ---- listing ----

// ConvInfo is one row of GET /watch/conversations.
type ConvInfo struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created"`
	LastTS   time.Time `json:"last_ts"`
	Archived bool      `json:"archived"`
	Count    int       `json:"count"`
	Preview  string    `json:"preview"`
}

// convStatsLocked computes every conversation's row in one pass.
func (s *Store) convStatsLocked() map[string]*ConvInfo {
	rows := map[string]*ConvInfo{ConvMain: {ID: ConvMain, Title: mainTitle}}
	get := func(id string) *ConvInfo {
		r, ok := rows[id]
		if !ok {
			r = &ConvInfo{ID: id, Title: DefaultConvTitle}
			rows[id] = r
		}
		return r
	}
	for _, id := range s.order {
		m := s.latest[id]
		c := convOf(m)
		r := get(c)
		if m.Kind == KindConv {
			if m.Title != "" {
				r.Title = m.Title
			}
			r.Archived = m.Archived != nil && *m.Archived
			if r.Created.IsZero() || m.TS.Before(r.Created) {
				r.Created = m.TS
			}
			if m.TS.After(r.LastTS) {
				r.LastTS = m.TS
			}
			continue
		}
		r.Count++
		if c == ConvMain && (r.Created.IsZero() || m.TS.Before(r.Created)) {
			r.Created = m.TS
		}
		if m.TS.After(r.LastTS) {
			r.LastTS = m.TS
		}
		if (m.Role == RoleUser || m.Role == RoleAgent) && strings.TrimSpace(m.Text) != "" {
			// The newest by position in the thread (insertion order).
			r.Preview = cleanPreview(m.Text)
		}
	}
	for id, c := range s.archivedConv {
		if _, live := s.latest[id]; !live {
			get(c).Count++
		}
	}
	return rows
}

func cleanPreview(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > previewRunes {
		t = string(r[:previewRunes])
	}
	return t
}

func (s *Store) convInfoLocked(id string) ConvInfo {
	if r, ok := s.convStatsLocked()[id]; ok {
		return *r
	}
	return ConvInfo{ID: id, Title: DefaultConvTitle}
}

// ConvInfo returns one conversation's row.
func (s *Store) ConvInfo(id string) (ConvInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != ConvMain {
		if _, ok := s.convRecordLocked(id); !ok {
			return ConvInfo{}, false
		}
	}
	return s.convInfoLocked(id), true
}

// ErrBadCursor: the conversations cursor could not be read.
var ErrBadCursor = errors.New("bad cursor")

// Conversations returns one page of conversations, newest activity first
// (last_ts descending, then id). On the first page of the non-archived
// list, main is always the first row; it is never on a later page or in the
// archived list. before is the cursor of the previous page ("" = first);
// next is "" when no more rows follow.
func (s *Store) Conversations(archived bool, before string, limit int) ([]ConvInfo, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var curTS time.Time
	curID := ""
	if before != "" {
		ts, id, err := decodeConvCursor(before)
		if err != nil {
			return nil, "", err
		}
		curTS, curID = ts, id
	}
	s.mu.Lock()
	rows := s.convStatsLocked()
	s.mu.Unlock()

	var list []ConvInfo
	for id, r := range rows {
		if id == ConvMain {
			continue
		}
		// Only conversations with a record are listed (a message never names
		// an unknown one: admission creates the record).
		if r.Archived != archived || r.Created.IsZero() {
			continue
		}
		list = append(list, *r)
	}
	sort.Slice(list, func(i, j int) bool { return convLess(list[i], list[j]) })
	if before != "" {
		i := sort.Search(len(list), func(i int) bool {
			r := list[i]
			return r.LastTS.Before(curTS) || (r.LastTS.Equal(curTS) && r.ID > curID)
		})
		list = list[i:]
	}
	var out []ConvInfo
	room := limit
	if !archived && before == "" {
		out = append(out, *rows[ConvMain])
		room--
		if room < 1 {
			room = 1
		}
	}
	next := ""
	if len(list) > room {
		list = list[:room]
		last := list[len(list)-1]
		next = encodeConvCursor(last.LastTS, last.ID)
	}
	out = append(out, list...)
	return out, next, nil
}

func convLess(a, b ConvInfo) bool {
	if !a.LastTS.Equal(b.LastTS) {
		return a.LastTS.After(b.LastTS)
	}
	return a.ID < b.ID
}

// The cursor is "<last_ts>,<id>" (RFC 3339 with nanoseconds), base64url so
// it travels in a query string unchanged.
func encodeConvCursor(ts time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "," + id))
}

func decodeConvCursor(c string) (time.Time, string, error) {
	raw := []byte(c)
	if b, err := base64.RawURLEncoding.DecodeString(c); err == nil {
		raw = b
	}
	tsPart, id, ok := strings.Cut(string(raw), ",")
	if !ok || !ValidConvID(id) {
		return time.Time{}, "", ErrBadCursor
	}
	ts, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return ts, id, nil
}
