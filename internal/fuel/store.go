package fuel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Op states (spec section 14 [C2]). pending -> done, or pending -> uncertain
// -> done | retry (-> done | uncertain ...); a non-done op after 24 h -> failed.
const (
	OpPending   = "pending"
	OpUncertain = "uncertain"
	OpRetry     = "retry"
	OpDone      = "done"
	OpFailed    = "failed"
)

func terminal(state string) bool { return state == OpDone || state == OpFailed }

// Entry statuses.
const (
	StatusDone    = "done"
	StatusPending = "pending_reconciliation"
	StatusFailed  = "failed"
)

// Item is one logged food (the original row).
type Item struct {
	ID            string    `json:"id"`
	EntryID       string    `json:"entry_id"`
	Date          string    `json:"date"`
	Name          string    `json:"item"`
	PortionG      *float64  `json:"portion_g"`
	Basis         string    `json:"portion_basis"`
	NeedsFraction bool      `json:"needs_fraction"`
	StapleKey     string    `json:"staple_key,omitempty"`
	Kind          string    `json:"kind,omitempty"` // food | drink | supplement
	Orig          Macros    `json:"macros"`
	EatenAt       time.Time `json:"eaten_at"`
	// Check is set when the estimate stayed implausible after a re-ask.
	Check string `json:"check,omitempty"`

	// Lever amounts and brew method of a NEW item (spec 18.6). They are not
	// journaled here: they live in the row data of the original op, so the
	// journal gets no new line shape and a v6 binary replays it.
	levers leverVals
	brew   string
}

// FixLine is one line of a chat correction reply: the success text of an
// op (rendered by the op's CURRENT state), or a note with no op.
type FixLine struct {
	OpID   string `json:"op_id,omitempty"`
	ItemID string `json:"item_id,omitempty"`
	Text   string `json:"text"`
	// Move is the target date when the line is one item of a move inside an
	// agent chat entry (spec 22.5); Text is then the item's name.
	Move string `json:"move,omitempty"`
}

// KindOr is the item's kind, "food" when unset (items journaled before kinds).
func (it Item) KindOr() string {
	if it.Kind == "" {
		return "food"
	}
	return it.Kind
}

// Entry is one log request's result.
type Entry struct {
	ID         string    `json:"id"`
	ClientID   string    `json:"client_id"`
	Date       string    `json:"date"`
	EatenAt    time.Time `json:"eaten_at"`
	CreatedAt  time.Time `json:"created_at"`
	Intent     string    `json:"intent"`
	Transcript *string   `json:"transcript"`
	PhotoIDs   []string  `json:"photo_ids"`
	ItemIDs    []string  `json:"item_ids"`
	Failed     bool      `json:"failed,omitempty"`
	ReqHash    string    `json:"req_hash,omitempty"` // idempotency hash of the request
	// What the conversation needs to rebuild this entry's feed lines.
	UserText  string `json:"user_text,omitempty"`
	ModelText string `json:"model_text,omitempty"`
	NoFood    bool   `json:"no_food,omitempty"` // a photo-only log where no food was seen
	// Intent "correct": the code-generated reply and the correction ops.
	FixText  string    `json:"fix_text,omitempty"`
	FixOps   []string  `json:"fix_ops,omitempty"`
	FixLines []FixLine `json:"fix_lines,omitempty"`
	// MoveTo is the target date of intent "move".
	MoveTo string `json:"move_to,omitempty"`
	// DayLabel is set when a log was for another day than the day it was
	// sent ("Wed 30 Sep"); the reply then names the day.
	DayLabel string `json:"day_label,omitempty"`
	// Note replaces the status line with a code-generated refusal (a day in
	// the future or too far back); nothing was written.
	Note    string   `json:"note,omitempty"`
	Widgets []string `json:"widgets,omitempty"`
	// Checks are code-generated lines of a log reply: a scale reading that
	// replaced an estimate, items whose numbers look implausible.
	Checks []string `json:"checks,omitempty"`
	// Clinical marks a turn the chat guard handled (spec 18.4): the model
	// text was dropped and the reply ends with the fixed line.
	Clinical bool `json:"clinical,omitempty"`
	// Agent is set on a question that went to the agent session (spec 21):
	// pending | done | fallback. AgentText is the agent's answer (done).
	Agent     string `json:"agent,omitempty"`
	AgentText string `json:"agent_text,omitempty"`
	// Chat is "agent" on an entry of the agent chat broker (spec 22): every
	// write of the turn is bound to this ONE entry. Agent is then pending |
	// done | failed, and AgentText is the agent's answer or the failure text.
	Chat string `json:"chat,omitempty"`
	// IntentHint is what the user marked at the input (spec 22.13): "log",
	// "ask" (a question: the turn can write nothing) or "" (not marked).
	IntentHint string `json:"intent_hint,omitempty"`
	// MovedIDs are the new items (on the target day) of the moves of an
	// agent chat entry. They are not items of the entry's cards.
	MovedIDs []string `json:"moved_ids,omitempty"`
}

// Op is one journaled row write.
type Op struct {
	ID        string `json:"op_id"`
	Kind      string `json:"kind"` // original | correction
	EntryID   string `json:"entry_id"`
	ItemID    string `json:"item_id"`     // the ORIGINAL item this op belongs to
	RowItemID string `json:"row_item_id"` // the item_id written on the row
	Reason    string `json:"reason,omitempty"`
	// Compensates is the op a compensation row cancels (one per op).
	Compensates string `json:"compensates,omitempty"`
	// PairOp (the undo half of a move) names the op of the NEW row: the
	// undo is posted only after that op is done.
	PairOp    string         `json:"pair_op,omitempty"`
	Fraction  *float64       `json:"fraction,omitempty"`
	Date      string         `json:"date"`
	ClientID  string         `json:"client_id,omitempty"` // undo / fraction request that made it
	ReqHash   string         `json:"req_hash,omitempty"`
	Data      map[string]any `json:"data"`
	Macros    Macros         `json:"macros"`
	CreatedAt time.Time      `json:"created_at"`

	State    string    `json:"state"`
	ValueID  string    `json:"value_id,omitempty"`
	Attempts int       `json:"attempts"`
	LastTry  time.Time `json:"last_try"`
	DoneAt   time.Time `json:"done_at,omitempty"`
}

// FractionChoice records a fraction pick (with or without a row) and the
// request that made it.
type FractionChoice struct {
	ItemID   string    `json:"item_id"`
	F        float64   `json:"f"`
	ClientID string    `json:"client_id,omitempty"`
	Hash     string    `json:"hash,omitempty"`
	At       time.Time `json:"at"`
}

// IdentRec is a request identity journaled without a row.
type IdentRec struct {
	ClientID string    `json:"client_id"`
	Kind     string    `json:"kind"`
	Hash     string    `json:"hash"`
	ItemID   string    `json:"item_id"`
	EntryID  string    `json:"entry_id"`
	At       time.Time `json:"at"`
}

// journalRec is one line of journal.jsonl. A "txn" line carries everything a
// request creates (entry, items, ops, fraction choice) so it is durable as a
// whole or not at all; "attempt" and "state" lines move one op.
type journalRec struct {
	T        string          `json:"t"` // txn | attempt | state
	At       time.Time       `json:"at"`
	Entry    *Entry          `json:"entry,omitempty"`
	Items    []Item          `json:"items,omitempty"`
	Ops      []Op            `json:"ops,omitempty"`
	Fraction *FractionChoice `json:"fraction,omitempty"`
	// Ident records a request that wrote nothing (a no-op fix), so its
	// client_id is durable and replays.
	Ident    *IdentRec `json:"ident,omitempty"`
	OpID     string    `json:"op_id,omitempty"`
	State    string    `json:"state,omitempty"`
	ValueID  string    `json:"value_id,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
}

// Journal is the source of truth for entries, items and ops (spec 14 [C2]).
// Everything else (entry status, fraction state, idempotency) is derived from
// it on startup replay.
type Journal struct {
	mu        sync.Mutex
	f         *os.File
	entries   map[string]*Entry
	items     map[string]*Item
	ops       map[string]*Op
	itemOps   map[string][]string // original item id -> op ids in order
	fractions map[string]FractionChoice
	doneByDay map[string]int // date -> done ops (the revision)
	idents    map[string]Identity
	pairUndo  map[string]string    // a move's new-row op id -> its undo op id
	syncFn    func(*os.File) error // (*os.File).Sync; tests inject failures
	size      int64                // file size after the last good append
	broken    error                // set when durability became uncertain; no more appends
}

// Identity is a request identity (client_id) as the journal knows it: the
// durable half of idempotency, consulted on every claim.
type Identity struct {
	Kind    string
	Hash    string
	At      time.Time
	EntryID string
	ItemID  string
	OpID    string
}

// ErrJournalBroken means an append could not be made durable; the service
// stops accepting writes until a restart replays the file.
var ErrJournalBroken = errors.New("journal durability uncertain")

// ErrDeadline means the request budget ran out before the txn was written.
var ErrDeadline = errors.New("deadline passed before the durable write")

func openAppend(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// loadLines reads a JSONL file and calls fn for every complete line. A torn
// or unparsable LAST line (a crash mid-append) is cut off the file so the
// next append starts on a clean line. With strict, an unparsable line in the
// middle is an error (the journal); otherwise it is logged and skipped.
func loadLines(path string, strict bool, fn func([]byte) error) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	validEnd := 0
	pos := 0
	for pos < len(data) {
		nl := bytes.IndexByte(data[pos:], '\n')
		if nl < 0 {
			break // no newline: a torn tail
		}
		line := data[pos : pos+nl]
		next := pos + nl + 1
		if len(bytes.TrimSpace(line)) == 0 {
			pos, validEnd = next, next
			continue
		}
		if perr := fn(line); perr != nil {
			if next >= len(data) {
				break // the last line is torn: truncate it
			}
			if strict {
				return fmt.Errorf("%s: corrupt record at byte %d: %v", filepath.Base(path), pos, perr)
			}
			log.Printf("fuel: %s: skipped a corrupt line at byte %d", filepath.Base(path), pos)
		}
		pos, validEnd = next, next
	}
	if validEnd < len(data) {
		log.Printf("fuel: %s: truncating a torn tail of %d bytes", filepath.Base(path), len(data)-validEnd)
		if err := os.Truncate(path, int64(validEnd)); err != nil {
			return err
		}
	}
	return nil
}

// OpenJournal replays journal.jsonl.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{
		entries: map[string]*Entry{}, items: map[string]*Item{}, ops: map[string]*Op{},
		itemOps: map[string][]string{}, fractions: map[string]FractionChoice{}, doneByDay: map[string]int{},
		idents: map[string]Identity{}, pairUndo: map[string]string{},
	}
	err := loadLines(path, true, func(b []byte) error {
		var r journalRec
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.T != "txn" && r.T != "attempt" && r.T != "state" {
			return fmt.Errorf("unknown record type %q", r.T)
		}
		j.apply(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		j.size = st.Size()
	}
	j.f = f
	j.syncFn = (*os.File).Sync
	return j, nil
}

func (j *Journal) apply(r journalRec) {
	switch r.T {
	case "txn":
		if r.Entry != nil {
			e := *r.Entry
			j.entries[e.ID] = &e
			if e.ClientID != "" {
				j.idents[e.ClientID] = Identity{Kind: "log", Hash: e.ReqHash, At: e.CreatedAt, EntryID: e.ID}
			}
		}
		for i := range r.Items {
			it := r.Items[i]
			j.items[it.ID] = &it
		}
		for i := range r.Ops {
			op := r.Ops[i]
			if _, dup := j.ops[op.ID]; dup {
				continue
			}
			j.ops[op.ID] = &op
			j.itemOps[op.ItemID] = append(j.itemOps[op.ItemID], op.ID)
			if op.PairOp != "" {
				j.pairUndo[op.PairOp] = op.ID
			}
			if op.ClientID != "" {
				j.idents[op.ClientID] = Identity{Kind: op.Reason, Hash: op.ReqHash, At: op.CreatedAt, EntryID: op.EntryID, ItemID: op.ItemID, OpID: op.ID}
			}
		}
		if r.Fraction != nil {
			fc := *r.Fraction
			j.fractions[fc.ItemID] = fc
			if fc.ClientID != "" {
				// The identity comes from THIS txn: its own op if it has one,
				// never an older (possibly expired) record of the client_id.
				eid := ""
				if it := j.items[fc.ItemID]; it != nil {
					eid = it.EntryID
				}
				id := Identity{Kind: "fraction", Hash: fc.Hash, At: fc.At, EntryID: eid, ItemID: fc.ItemID}
				for _, op := range r.Ops {
					if op.ClientID == fc.ClientID {
						id.OpID = op.ID
					}
				}
				j.idents[fc.ClientID] = id
			}
		}

		if r.Ident != nil && r.Ident.ClientID != "" {
			id := r.Ident
			j.idents[id.ClientID] = Identity{Kind: id.Kind, Hash: id.Hash, At: id.At, EntryID: id.EntryID, ItemID: id.ItemID}
		}
	case "attempt":
		if op := j.ops[r.OpID]; op != nil && !terminal(op.State) {
			op.Attempts = r.Attempts
			op.LastTry = r.At
		}
	case "state":
		op := j.ops[r.OpID]
		if op == nil || terminal(op.State) {
			return // terminal states never move
		}
		op.State = r.State
		op.LastTry = r.At
		if r.Attempts > 0 {
			op.Attempts = r.Attempts
		}
		switch r.State {
		case OpDone:
			op.ValueID = r.ValueID
			op.DoneAt = r.At
			j.doneByDay[op.Date]++
		case OpFailed:
			// Entry failure is DERIVED from a failed ORIGINAL op, so no
			// second record can be lost between the two. A failed
			// correction (undo, fraction, fix, compensation) fails only
			// itself: it must never cancel the rest of the meal.
			// A move entry never fails as a whole either: each item moves
			// (or stays) on its own. The same holds for an entry of the
			// agent chat (spec 22.6): each write of the turn stands alone.
			if e := j.entries[op.EntryID]; e != nil && op.Kind == "original" && e.Intent != "move" && e.Chat == "" {
				e.Failed = true
			}
		}
	}
}

// write appends one line. A failed write is cut back to the last good size;
// if that or the fsync fails, durability is uncertain and the journal refuses
// further appends (the record may or may not replay after a restart).
func (j *Journal) write(r journalRec) (applied bool, err error) {
	b, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	b = append(b, '\n')
	if _, werr := j.f.Write(b); werr != nil {
		if terr := j.f.Truncate(j.size); terr != nil {
			j.broken = fmt.Errorf("%w: write failed and could not be cut back", ErrJournalBroken)
		}
		return false, werr
	}
	if serr := j.syncFn(j.f); serr != nil {
		// The line may be on disk: treat it as written (so its identities
		// hold) and stop.
		j.broken = fmt.Errorf("%w: fsync failed", ErrJournalBroken)
		j.size += int64(len(b))
		return true, j.broken
	}
	j.size += int64(len(b))
	return true, nil
}

// Append journals a record and applies it (under the journal lock).
func (j *Journal) Append(r journalRec) error { return j.AppendCtx(context.Background(), r) }

// AppendCtx is Append with the request deadline checked AFTER the lock is
// held, immediately before the write: an expired request writes nothing.
func (j *Journal) AppendCtx(ctx context.Context, r journalRec) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.broken != nil {
		return j.broken
	}
	if ctx.Err() != nil {
		return ErrDeadline
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	applied, err := j.write(r)
	if applied {
		j.apply(r)
	}
	return err
}

// Ident returns the journal's record of a request identity.
func (j *Journal) Ident(clientID string) (Identity, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	id, ok := j.idents[clientID]
	return id, ok
}

func (j *Journal) Close() error { return j.f.Close() }

// ---- read helpers (take the lock) ----

func (j *Journal) Entry(id string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

func (j *Journal) Item(id string) (Item, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	it, ok := j.items[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

func (j *Journal) Op(id string) (Op, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[id]
	if !ok {
		return Op{}, false
	}
	return *op, true
}

// ItemOps returns copies of every op of an original item, in journal order.
func (j *Journal) ItemOps(itemID string) []Op {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Op
	for _, id := range j.itemOps[itemID] {
		out = append(out, *j.ops[id])
	}
	return out
}

// Revision is the number of done ops for a date (spec 14 [C14]).
func (j *Journal) Revision(date string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.doneByDay[date]
}

// NonTerminal lists ops that are not done or failed.
func (j *Journal) NonTerminal() []Op {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Op
	for _, op := range j.ops {
		if !terminal(op.State) {
			out = append(out, *op)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out
}

// DoneRowsSince returns done ops whose DoneAt is after t (for cache re-adds).
func (j *Journal) DoneRowsFor(date string, since time.Time) []Op {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Op
	for _, op := range j.ops {
		if op.State == OpDone && op.Date == date && op.DoneAt.After(since) {
			out = append(out, *op)
		}
	}
	return out
}

// Entries returns copies of every entry.
func (j *Journal) Entries() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Entry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, *e)
	}
	return out
}

// Ops returns copies of every op.
func (j *Journal) Ops() []Op {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Op, 0, len(j.ops))
	for _, op := range j.ops {
		out = append(out, *op)
	}
	return out
}

// FailedEntries lists failed entries.
func (j *Journal) FailedEntries() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Entry
	for _, e := range j.entries {
		if e.Failed || ((e.Intent == "move" || e.Chat != "") && j.anyFailedOriginalLocked(e)) {
			out = append(out, *e)
		}
	}
	return out
}

func (j *Journal) anyFailedOriginalLocked(e *Entry) bool {
	for _, id := range append(append([]string{}, e.ItemIDs...), e.MovedIDs...) {
		for _, opID := range j.itemOps[id] {
			if op := j.ops[opID]; op != nil && op.Kind == "original" && op.State == OpFailed {
				return true
			}
		}
	}
	return false
}

// PairUndo returns the undo op paired with a move's new-row op.
func (j *Journal) PairUndo(newOpID string) (Op, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	id, ok := j.pairUndo[newOpID]
	if !ok || j.ops[id] == nil {
		return Op{}, false
	}
	return *j.ops[id], true
}

// OriginalFailed reports whether the item's own original row op failed.
func (j *Journal) OriginalFailed(itemID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, opID := range j.itemOps[itemID] {
		if op := j.ops[opID]; op != nil && op.Kind == "original" && op.State == OpFailed {
			return true
		}
	}
	return false
}

func (j *Journal) Fraction(itemID string) (float64, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, ok := j.fractions[itemID]
	if !ok {
		return 0, false
	}
	// A choice whose correction row was definitively rejected does not
	// count: the original amount is still what is logged.
	for _, id := range j.itemOps[itemID] {
		if op := j.ops[id]; op != nil && op.Reason == "fraction" && op.ClientID == f.ClientID && op.State == OpFailed {
			return 0, false
		}
	}
	return f.F, true
}

// Fractions returns every recorded fraction choice.
func (j *Journal) Fractions() []FractionChoice {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]FractionChoice, 0, len(j.fractions))
	for _, f := range j.fractions {
		out = append(out, f)
	}
	return out
}

// ---- ids ----

func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// ---- idempotency ----

// idemRec is one line of idem.jsonl.
type idemRec struct {
	ClientID string          `json:"client_id"`
	Hash     string          `json:"hash"`
	Kind     string          `json:"kind"`
	At       time.Time       `json:"at"`
	EntryID  string          `json:"entry_id,omitempty"`
	ItemID   string          `json:"item_id,omitempty"`
	ItemIDs  []string        `json:"item_ids,omitempty"`
	Status   int             `json:"status,omitempty"` // 0 = reserved, in progress
	Response json.RawMessage `json:"response,omitempty"`
}

const idemRetention = 7 * 24 * time.Hour

// Idem is the idempotency store (spec section 6).
type Idem struct {
	putMu sync.Mutex // serializes PutIfNoResponse
	mu    sync.Mutex
	f     *os.File
	recs  map[string]*idemRec
}

// OpenIdem loads idem.jsonl, drops records older than 7 days and compacts.
func OpenIdem(path string, now time.Time) (*Idem, error) {
	recs := map[string]*idemRec{}
	err := loadLines(path, false, func(b []byte) error {
		var r idemRec
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		recs[r.ClientID] = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Compact: rewrite the live records to a temp file, then rename.
	tmp := path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(recs))
	for k, r := range recs {
		if now.Sub(r.At) > idemRetention {
			delete(recs, k)
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b, _ := json.Marshal(recs[k])
		_, _ = tf.Write(append(b, '\n'))
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return nil, err
	}
	tf.Close()
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &Idem{f: f, recs: recs}, nil
}

func (s *Idem) Get(clientID string) (idemRec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[clientID]
	if !ok {
		return idemRec{}, false
	}
	return *r, true
}

func (s *Idem) Put(r idemRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	cp := r
	s.recs[r.ClientID] = &cp
	return nil
}

// sameRequest: a stored record answers rec only if it is the same request
// (a reused, expired client_id is a different one).
func sameRequest(a, b idemRec) bool {
	return a.Kind == b.Kind && a.Hash == b.Hash && a.EntryID == b.EntryID && a.ItemID == b.ItemID
}

// PutIfNoResponse stores rec unless the client_id already has a final
// response (atomic check and write); it returns the record that holds.
func (s *Idem) PutIfNoResponse(rec idemRec) (idemRec, bool) {
	s.mu.Lock()
	if cur, ok := s.recs[rec.ClientID]; ok && len(cur.Response) > 0 && sameRequest(*cur, rec) {
		out := *cur
		s.mu.Unlock()
		return out, true
	}
	s.mu.Unlock()
	// Put re-takes the lock; a racing PutIfNoResponse is serialized by the
	// caller's claim (one request per client_id) or harmlessly identical.
	s.putMu.Lock()
	defer s.putMu.Unlock()
	s.mu.Lock()
	if cur, ok := s.recs[rec.ClientID]; ok && len(cur.Response) > 0 && sameRequest(*cur, rec) {
		out := *cur
		s.mu.Unlock()
		return out, true
	}
	s.mu.Unlock()
	if err := s.Put(rec); err != nil {
		return idemRec{}, false
	}
	return rec, true
}

func (s *Idem) Close() error { return s.f.Close() }

// ---- feed ----

// FeedItem is one line of feed.jsonl (the conversation).
type FeedItem struct {
	Seq      int64     `json:"seq"`
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Role     string    `json:"role"` // user | fuel | coach
	Text     *string   `json:"text"`
	PhotoIDs []string  `json:"photo_ids"`
	EntryID  *string   `json:"entry_id"`
	Blocks   []Block   `json:"blocks"`
	// IntentHint of a user line: what the user marked ("log" | "ask").
	IntentHint string `json:"intent_hint,omitempty"`
	// ShowItems marks the fuel reply that carries the entry's item cards.
	ShowItems bool `json:"show_items,omitempty"`
	// NoticeOp is the failed op a "could not save" line reports (once per op).
	NoticeOp string `json:"notice_op,omitempty"`
	// Key is the client_id of the undo / fraction a reply line belongs to.
	Key string `json:"key,omitempty"`
	// WriteLine is the text of the write line of an agent chat reply as it
	// was stored (spec 22.6); the feed route replaces it with the current one.
	// Not on the wire.
	WriteLine string `json:"write_line,omitempty"`
}

// Feed is the append-only conversation store.
type Feed struct {
	mu     sync.Mutex
	f      *os.File
	items  []FeedItem
	size   int64
	broken bool
	syncFn func(*os.File) error
	keys   map[string]bool // Key and "notice:"+NoticeOp of every line
}

func (fd *Feed) index(it FeedItem) {
	if it.Key != "" {
		fd.keys[it.Key] = true
	}
	if it.NoticeOp != "" {
		fd.keys["notice:"+it.NoticeOp] = true
	}
}

func OpenFeed(path string) (*Feed, error) {
	fd := &Feed{keys: map[string]bool{}}
	err := loadLines(path, false, func(b []byte) error {
		var it FeedItem
		if err := json.Unmarshal(b, &it); err != nil {
			return err
		}
		fd.items = append(fd.items, it)
		fd.index(it)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(fd.items, func(a, b int) bool { return fd.items[a].Seq < fd.items[b].Seq })
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil {
		fd.size = st.Size()
	}
	fd.f = f
	fd.syncFn = (*os.File).Sync
	return fd, nil
}

func (fd *Feed) Append(it FeedItem) (FeedItem, error) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.broken {
		return it, errors.New("feed store unavailable")
	}
	// Keyed lines are appended once, atomically (the handler and the
	// recovery pass may race for the same line).
	if (it.Key != "" && fd.keys[it.Key]) || (it.NoticeOp != "" && fd.keys["notice:"+it.NoticeOp]) {
		return it, nil
	}
	it.Seq = 1
	if n := len(fd.items); n > 0 {
		it.Seq = fd.items[n-1].Seq + 1
	}
	it.ID = "f" + strconv.FormatInt(it.Seq, 10)
	if it.PhotoIDs == nil {
		it.PhotoIDs = []string{}
	}
	if it.Blocks == nil {
		it.Blocks = []Block{}
	}
	b, err := json.Marshal(it)
	if err != nil {
		return it, err
	}
	b = append(b, '\n')
	if _, err := fd.f.Write(b); err != nil {
		// Cut a partial line back so the next append starts clean.
		if terr := fd.f.Truncate(fd.size); terr != nil {
			fd.broken = true
		}
		return it, err
	}
	fd.size += int64(len(b))
	// Written: keep it (its seq is used) even if the fsync fails.
	fd.items = append(fd.items, it)
	fd.index(it)
	if err := fd.syncFn(fd.f); err != nil {
		fd.broken = true
		return it, err
	}
	return it, nil
}

// Page returns up to limit items older than before (0 = newest), newest last,
// and the cursor for the next older page ("" when none).
func (fd *Feed) Page(before int64, limit int) ([]FeedItem, string) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	end := len(fd.items)
	if before > 0 {
		end = sort.Search(len(fd.items), func(i int) bool { return fd.items[i].Seq >= before })
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := append([]FeedItem(nil), fd.items[start:end]...)
	next := ""
	if start > 0 {
		next = strconv.FormatInt(fd.items[start].Seq, 10)
	}
	return out, next
}

// HasKey reports whether a line with the key exists (indexed).
func (fd *Feed) HasKey(key string) bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.keys[key]
}

// HasEntry reports whether the user line of an entry exists.
func (fd *Feed) HasEntry(entryID string) bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for i := len(fd.items) - 1; i >= 0; i-- {
		if fd.items[i].EntryID != nil && *fd.items[i].EntryID == entryID && fd.items[i].Role == "user" {
			return true
		}
	}
	return false
}

// HasNotice reports whether a failure line for the op exists (indexed).
func (fd *Feed) HasNotice(opID string) bool {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.keys["notice:"+opID]
}

func (fd *Feed) Close() error { return fd.f.Close() }

// appendJSONLine appends one JSON line to a file (coach events).
func appendJSONLine(path string, v any) error {
	f, err := openAppend(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

var _ = fmt.Sprintf
