package fuel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The turn-bound mode of the deterministic write routes (spec 22.5). The
// agent of the chat broker writes ONLY through these routes, with the agent
// token and the capability of its open turn (header X-Fuel-Turn). A
// turn-bound write makes no entry and no feed line of its own: it is added
// to the entry of the turn, in the same journal line as its rows.

const turnHeader = "X-Fuel-Turn"

// chatTurn is one open turn of the agent chat. mu is held by a write for its
// whole request and by the close: after the close no write of the turn can
// start, and none is in flight.
type chatTurn struct {
	mu       sync.Mutex
	cap      string
	entryID  string
	closed   bool
	at       time.Time // the receipt time of the user's message
	received time.Time // the server clock when the message was accepted
	date     string    // the log date of the turn

	calls    map[string]*turnCall       // client_id -> the stored answer
	cur      string                     // the client_id of the request that holds mu
	names    map[string]bool            // normalized names of the items written in the turn
	kinds    map[string]map[string]bool // item id -> the kinds of change it took (fix, move, undo)
	newItems int
	writes   int
}

// turnCall is one admitted call of a turn (idempotency by client_id inside
// the turn): the hash of its request and what its answer is rendered from.
type turnCall struct {
	hash    string
	date    string
	opIDs   []string
	itemIDs []string
	lines   []string
	set     bool // the answer was rendered once
}

type turnCtxKey struct{}

func turnOf(r *http.Request) *chatTurn {
	t, _ := r.Context().Value(turnCtxKey{}).(*chatTurn)
	return t
}

// capPrefix names the instance in a capability: a capability of the other
// instance is refused before any lookup.
func (s *Service) capPrefix() string {
	if s.o.TestMode {
		return "e_"
	}
	return "p_"
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// turnOpen registers a new open turn for an entry.
func (s *Service) turnOpen(e Entry, received time.Time) *chatTurn {
	t := &chatTurn{cap: s.capPrefix() + randHex(16), entryID: e.ID, at: e.EatenAt, received: received, date: e.Date,
		calls: map[string]*turnCall{}, names: map[string]bool{}, kinds: map[string]map[string]bool{}}
	s.chat.mu.Lock()
	s.chat.turns[t.cap] = t
	s.chat.mu.Unlock()
	return t
}

// turnClose closes a turn: it waits for a write in flight, then no write of
// the turn is admitted any more. Idempotent.
func (s *Service) turnClose(t *chatTurn) {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	s.chat.mu.Lock()
	delete(s.chat.turns, t.cap)
	s.chat.mu.Unlock()
}

var errTurnClosed = errf(http.StatusConflict, "turn_closed", false, "this turn is closed or unknown; nothing was written. Write nothing more for it.")

// tokenRole is "app" for the Fuel token, "agent" for the agent token of
// 22.5, "" for anything else.
func (s *Service) tokenRole(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	got := []byte(strings.TrimPrefix(h, "Bearer "))
	if len(got) == 0 {
		return ""
	}
	if s.o.Token != "" && subtle.ConstantTimeCompare(got, []byte(s.o.Token)) == 1 {
		return "app"
	}
	if tok := s.o.Chat.OpToken; tok != "" && subtle.ConstantTimeCompare(got, []byte(tok)) == 1 {
		return "agent"
	}
	return ""
}

// agentScope checks a request that carries the agent token: reads, the
// preview, and the turn-bound writes. Everything else is 403.
func agentScope(r *http.Request) *apiError {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet:
		switch p {
		case "/fuel/day", "/fuel/week", "/fuel/snapshot", "/fuel/recent", "/fuel/feed":
			return nil
		}
	case http.MethodPost:
		switch p {
		case "/fuel/preview":
			return nil
		case "/fuel/items", "/fuel/fix", "/fuel/undo", "/fuel/move", "/fuel/relog":
			if r.Header.Get(turnHeader) != "" {
				return nil
			}
			return errf(http.StatusForbidden, "agent_scope", false, "the agent token writes only inside a turn (header X-Fuel-Turn)")
		}
	}
	return errf(http.StatusForbidden, "agent_scope", false, "the agent token is not valid for this route")
}

// turnBound wraps a write route: without the header the route is unchanged.
// With it, the turn is looked up and LOCKED for the whole request.
func (s *Service) turnBound(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := r.Header.Get(turnHeader)
		if c == "" {
			h(w, r)
			return
		}
		if !strings.HasPrefix(c, s.capPrefix()) {
			writeErr(w, errTurnClosed)
			return
		}
		s.chat.mu.Lock()
		t := s.chat.turns[c]
		s.chat.mu.Unlock()
		if t == nil {
			writeErr(w, errTurnClosed)
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			writeErr(w, errTurnClosed)
			return
		}
		t.cur = ""
		h(w, r.WithContext(context.WithValue(r.Context(), turnCtxKey{}, t)))
		t.cur = ""
	}
}

// replay answers a repeated client_id of the turn: the CURRENT state of the
// first call's writes (sections 6 and 20), never a second write. Another
// request with that client_id is 409. true = answered.
func (s *Service) turnReplay(ctx context.Context, w http.ResponseWriter, t *chatTurn, clientID, hash string) bool {
	c, ok := t.calls[clientID]
	if !ok {
		return false
	}
	if c.hash != hash {
		writeErr(w, errf(http.StatusConflict, "idempotency_conflict", false, "client_id was used for a different request in this turn"))
		return true
	}
	s.turnAnswer(ctx, w, t, c.date, c.opIDs, c.itemIDs, c.lines)
	return true
}

// admit marks the call of clientID as admitted. Call it right before the
// journal line of the write.
func (t *chatTurn) admit(clientID, hash string) {
	t.calls[clientID] = &turnCall{hash: hash}
	t.cur = clientID
	t.writes++
}

// unadmit takes an admission back when the journal line failed (nothing was
// written, the call may come again).
func (t *chatTurn) unadmit(clientID string) {
	delete(t.calls, clientID)
	t.cur = ""
	t.writes--
}

// took reports whether the item took a change of this kind in the turn (or
// was undone in it), and marks it otherwise.
func (t *chatTurn) changed(itemID, kind string) *apiError {
	k := t.kinds[itemID]
	switch {
	case k["undo"]:
		return errf(http.StatusConflict, "item_changed_in_turn", false, "this item was removed in this turn; it takes no other change")
	case k[kind]:
		return errf(http.StatusConflict, "item_changed_in_turn", false, "this item already took one %s in this turn; a second one needs a new message of the user", kind)
	}
	return nil
}

func (t *chatTurn) mark(itemID, kind string) {
	if t.kinds[itemID] == nil {
		t.kinds[itemID] = map[string]bool{}
	}
	t.kinds[itemID][kind] = true
}

// inherit gives the new row of a move the change history of the old item.
func (t *chatTurn) inherit(oldID, newID string) {
	k := map[string]bool{}
	for kind := range t.kinds[oldID] {
		k[kind] = true
	}
	t.kinds[newID] = k
}

// turnWriteResponse is the answer of a turn-bound write (spec 22.5).
type turnWriteResponse struct {
	Result   string      `json:"result"` // written | pending | failed | nothing
	EntryID  string      `json:"entry_id"`
	Items    []ItemState `json:"items"`
	Lines    []string    `json:"lines"`
	Snapshot Snapshot    `json:"snapshot"`
	Date     string      `json:"date"`
}

// turnEntry is the CURRENT entry of the turn (with every earlier write).
func (s *Service) turnEntry(t *chatTurn) (Entry, *apiError) {
	e, ok := s.journal.Entry(t.entryID)
	if !ok {
		return Entry{}, errTurnClosed
	}
	return e, nil
}

// turnAnswer renders the answer of a turn-bound write: the result of ITS ops,
// the items it touched, the snapshot of the date.
func (s *Service) turnAnswer(ctx context.Context, w http.ResponseWriter, t *chatTurn, date string, opIDs, itemIDs []string, lines []string) {
	if c := t.calls[t.cur]; c != nil && !c.set {
		// The first answer of an admitted call: kept for its replay.
		c.date, c.opIDs, c.itemIDs, c.lines, c.set = date, opIDs, itemIDs, lines, true
	}
	t.cur = ""
	s.renderReady(ctx, date)
	s.stateMu.RLock()
	snap, err := s.snapshotFor(date)
	res := "written"
	if len(opIDs) == 0 {
		res = "nothing"
	}
	for _, id := range opIDs {
		op, ok := s.journal.Op(id)
		switch {
		case !ok:
		case op.State == OpFailed:
			res = "failed"
		case !terminal(op.State) && res != "failed":
			res = "pending"
		}
	}
	items := []ItemState{}
	for _, id := range itemIDs {
		if it, ok := s.journal.Item(id); ok {
			items = append(items, s.viewItem(it).state)
		}
	}
	s.stateMu.RUnlock()
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the write itself was saved"))
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, turnWriteResponse{Result: res, EntryID: t.entryID, Items: items, Lines: lines, Snapshot: snap, Date: date})
}

// ---- the duplicate guard of POST /fuel/items in a turn ----

var dupStop = map[string]bool{"and": true, "the": true, "with": true, "from": true, "for": true, "some": true, "small": true,
	"large": true, "little": true, "big": true, "plain": true, "fresh": true, "one": true, "two": true, "cup": true, "glass": true,
	"slice": true, "piece": true, "bowl": true, "plate": true, "mit": true, "und": true, "ohne": true, "without": true, "half": true}

// nameWords are the comparable words of a food name: lower case, letters
// only, a plural s removed, 3 letters or more, no filler word.
func nameWords(name string) map[string]bool {
	out := map[string]bool{}
	f := func(r rune) bool { return !(r >= 'a' && r <= 'z') && r < 128 }
	for _, w := range strings.FieldsFunc(strings.ToLower(name), f) {
		if dupStop[w] {
			continue
		}
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = strings.TrimSuffix(w, "s")
		}
		if len(w) >= 3 && !dupStop[w] {
			out[w] = true
		}
	}
	return out
}

// likelyDuplicate finds an active item of the date that shares a name word
// with `name` and was logged at most 10 minutes before the turn was
// received (spec 22.5). Items of this turn's own entry are skipped.
func (s *Service) likelyDuplicate(t *chatTurn, date, name string) (DayItem, bool) {
	words := nameWords(name)
	if len(words) == 0 {
		return DayItem{}, false
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	for _, d := range s.dayItems(date) {
		logged := d.EatenAt
		if d.EntryID != nil {
			if *d.EntryID == t.entryID {
				continue
			}
			if e, ok := s.journal.Entry(*d.EntryID); ok {
				logged = e.CreatedAt
			}
		}
		if logged.Before(t.received.Add(-10*time.Minute)) || logged.After(t.received.Add(time.Minute)) {
			continue
		}
		for w := range nameWords(d.Item) {
			if words[w] {
				return d, true
			}
		}
	}
	return DayItem{}, false
}

// ---- POST /fuel/preview ----

type previewBudget struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Unit      string   `json:"unit"`
	Kind      string   `json:"kind"`
	Consumed  float64  `json:"consumed"`
	Target    *float64 `json:"target"`
	Adds      *float64 `json:"adds"`
	After     *float64 `json:"after"`
	LeftAfter *float64 `json:"left_after"`
}

type previewItem struct {
	Item     string   `json:"item"`
	PortionG *float64 `json:"portion_g"`
	Macros   Macros   `json:"macros"`
	Levers   Levers   `json:"levers"`
}

// handlePreview serves POST /fuel/preview: what a list of items WOULD add to
// a day. It writes nothing and journals nothing (spec 22.5).
func (s *Service) handlePreview(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Items []json.RawMessage `json:"items"`
		Day   *string           `json:"day"`
	}
	if err := decodeStrict(r.Body, &body); err != nil || len(body.Items) == 0 {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "a JSON object with at least one item is required"))
		return
	}
	items := make([]json.RawMessage, 0, len(body.Items))
	for _, raw := range body.Items {
		it, err := itemDefaults(raw)
		if err != nil {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%v", err))
			return
		}
		items = append(items, it)
	}
	synth, _ := json.Marshal(map[string]any{"intent": "log", "items": items, "text": "", "widgets": []string{}, "clinical_topic": false, "day": body.Day})
	out, verr := validateOutput(synth)
	if verr != nil {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%v", verr))
		return
	}
	targets, terr := s.loadTargets()
	if terr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.Budget)
	defer cancel()
	now := s.o.Now()
	date := now.In(targets.loc).Format("2006-01-02")
	if out.Day != nil {
		d, _, refuse := resolveDay(out.Day, nil, now, targets.loc, date, now)
		if refuse != "" {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%s", refuse))
			return
		}
		date = d
	}
	if !s.freshen(ctx, date) || !s.ensureHistory(ctx, date) {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the food log; retry"))
		return
	}
	built := s.buildItems(out, Entry{ID: "preview", Date: date, EatenAt: now})
	var ms []Macros
	pis := []previewItem{}
	for _, it := range built {
		ms = append(ms, it.Orig)
		pis = append(pis, previewItem{Item: it.Name, PortionG: it.PortionG, Macros: it.Orig, Levers: wireLevers(it.levers, it.brew)})
	}
	s.stateMu.RLock()
	snap, err := s.snapshotFor(date)
	s.stateMu.RUnlock()
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	sum := func(key string) *float64 {
		var t tenth
		for i, m := range ms {
			if i == 0 {
				t = m.Get(key)
			} else {
				t = t.add(m.Get(key))
			}
		}
		return t.ptr()
	}
	var budgets []previewBudget
	add := func(m MacroState) {
		pb := previewBudget{Key: m.Key, Label: m.Label, Unit: m.Unit, Kind: m.Kind, Consumed: m.Consumed, Target: m.Target, Adds: sum(m.Key)}
		if pb.Adds != nil {
			a := round1(m.Consumed + *pb.Adds)
			pb.After = &a
			if m.Target != nil {
				l := round1(*m.Target - a)
				pb.LeftAfter = &l
			}
		}
		budgets = append(budgets, pb)
	}
	if len(snap.Budgets) > 0 {
		for _, b := range snap.Budgets {
			add(b.MacroState)
		}
	} else {
		for _, m := range snap.Macros {
			add(m)
		}
	}
	totals := map[string]*float64{}
	for _, k := range []string{"kcal", "protein_g", "carbs_g", "net_carbs_g", "fat_g", "sat_fat_g", "fiber_g", "caffeine_mg", "alcohol_g"} {
		totals[k] = sum(k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "written": false, "items": pis, "sum": totals, "budgets": budgets})
}
