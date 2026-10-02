package fuel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Deterministic write routes for an agent (spec section 20). The Fuel chat
// is moving out of fueld; fueld stays the data layer. Every data operation
// the chat does is reachable here with NO model call: POST /fuel/items (log
// items with given values) and POST /fuel/move, next to the routes that were
// deterministic already (fix, undo, fraction, relog, day, week, snapshot).

// itemsBody is the body of POST /fuel/items.
type itemsBody struct {
	ClientID  string            `json:"client_id"`
	LocalTime string            `json:"local_time"`
	Day       *string           `json:"day"`  // today | yesterday | YYYY-MM-DD
	Time      *string           `json:"time"` // HH:MM
	Note      string            `json:"note"` // shown as the user line of the feed
	Items     []json.RawMessage `json:"items"`
	// New (a turn-bound request, spec 22.5): the caller confirms that an item
	// the duplicate guard named is MORE food, not the same food again.
	New bool `json:"new"`
}

// itemDefaults fills the keys a caller may leave out, so that the item
// passes the same validation as a model item.
func itemDefaults(raw json.RawMessage) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("each item must be an object")
	}
	def := func(k, v string) {
		if _, ok := m[k]; !ok {
			m[k] = json.RawMessage(v)
		}
	}
	basis := `"unspecified"`
	if p, ok := m["portion_g"]; ok && string(p) != "null" {
		basis = `"stated"`
	}
	if v, ok := m["volume_ml"]; ok && string(v) != "null" {
		basis = `"stated"`
	}
	def("staple_key", "null")
	def("portion_g", "null")
	def("portion_basis", basis)
	def("net_carbs_g", "null")
	def("fiber_g", "null")
	def("needs_fraction", "false")
	return json.Marshal(m)
}

// handleItems serves POST /fuel/items: one entry of intent "log" with the
// given items. The same rules as a chat log apply after the model step
// (staples, net carbs, lever checks, plausibility flags, journal, rows,
// idempotency by client_id); the answer has the shape of POST /fuel/log.
func (s *Service) handleItems(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if e := s.acquireSlot(r.Context()); e != nil {
		writeErr(w, e)
		return
	}
	defer func() { <-s.slots }()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.o.ReadDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body itemsBody
	err := decodeStrict(r.Body, &body)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		if maxBytesErr(err) {
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", false, "body too large"))
			return
		}
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "bad JSON body"))
		return
	}
	if !clientIDRe.MatchString(body.ClientID) || len(body.Items) == 0 {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id and at least one item are required"))
		return
	}
	// The items go through the validation of a model answer.
	items := make([]json.RawMessage, 0, len(body.Items))
	for _, raw := range body.Items {
		it, err := itemDefaults(raw)
		if err != nil {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%v", err))
			return
		}
		items = append(items, it)
	}
	synth, _ := json.Marshal(map[string]any{"intent": "log", "items": items, "text": "", "widgets": []string{}, "clinical_topic": false,
		"day": body.Day, "time": body.Time})
	out, verr := validateOutput(synth)
	if verr != nil {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%v", verr))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	now := s.o.Now()
	hh := sha256.Sum256(append([]byte("items\x00"+strings.TrimSpace(body.LocalTime)+"\x00"+body.Note+"\x00"), synth...))
	hash := hex.EncodeToString(hh[:])
	turn := turnOf(r)
	if turn != nil {
		// A turn-bound write (spec 22.5): idempotent inside the turn.
		if turn.replay(w, body.ClientID) {
			return
		}
	} else {
		claim, ce := s.claim(ctx, body.ClientID, "log", hash)
		if ce != nil {
			writeErr(w, ce)
			return
		}
		if claim.replay != nil {
			s.replayLog(ctx, w, *claim.replay)
			return
		}
		defer claim.release()
	}

	targets, terr := s.loadTargets()
	if terr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	eatenAt := now
	if turn != nil {
		eatenAt = turn.at // the receipt time of the user's message, not the clock
	}
	if body.LocalTime != "" {
		t, err := time.Parse(time.RFC3339, body.LocalTime)
		if err != nil || now.Sub(t) > 48*time.Hour || t.Sub(now) > 5*time.Minute {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "local_time must be RFC3339, at most 48 h in the past and 5 min in the future"))
			return
		}
		eatenAt = t
	}
	date := eatenAt.In(targets.loc).Format("2006-01-02")
	requestDate := date
	if out.Day != nil || out.Time != nil {
		d, at, refuse := resolveDay(out.Day, out.Time, now, targets.loc, date, eatenAt)
		if refuse != "" {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%s", refuse))
			return
		}
		date, eatenAt = d, at
	}
	if turn != nil {
		s.turnItems(ctx, w, turn, body, out, date, eatenAt, now)
		return
	}
	if e := s.rateCheck(now); e != nil {
		writeErr(w, e)
		return
	}
	s.freshen(ctx, date)

	// Plausibility: no re-ask here (there is no model); what looks wrong
	// gets the visible check flag (spec 17 E).
	checks := s.problemsWith(out, implausibleAny) // also without a food class
	userText := strings.TrimSpace(body.Note)
	if userText == "" {
		var names []string
		for _, it := range out.Items {
			names = append(names, strings.TrimSpace(it.Item))
		}
		userText = "log: " + joinAnd(names)
	}
	entry := Entry{ID: newID("en_"), ClientID: body.ClientID, Date: date, EatenAt: eatenAt, CreatedAt: now,
		Intent: "log", PhotoIDs: []string{}, ReqHash: hash, UserText: clipRunes(userText, 400)}
	if date != now.In(targets.loc).Format("2006-01-02") || date != requestDate {
		entry.DayLabel = dayLabel(date)
	}
	its := s.buildItems(out, entry)
	var ops []Op
	for i := range its {
		if why := checks[i]; why != "" {
			its[i].Check = why
			entry.Checks = append(entry.Checks, "Check this: "+its[i].Name+": "+why+".")
		}
		entry.ItemIDs = append(entry.ItemIDs, its[i].ID)
		opID := newID("op_")
		ops = append(ops, Op{ID: opID, Kind: "original", EntryID: entry.ID, ItemID: its[i].ID, RowItemID: its[i].ID, Date: date,
			Data: originalRowData(its[i], opID, ""), Macros: its[i].Orig, CreatedAt: now, State: OpPending, Attempts: 1, LastTry: s.o.Now()})
	}
	tWrite := time.Now()
	if err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: its, Ops: ops}); err != nil {
		if err == ErrDeadline {
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		} else {
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		}
		return
	}
	if err := s.idem.Put(idemRec{ClientID: body.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs}); err != nil {
		log.Printf("fuel: idem reservation for %s failed (the journal identity still holds)", entry.ID)
	}
	s.runOps(ctx, ops)
	writeD := time.Since(tWrite)

	ready := s.renderReady(ctx, date)
	resp, status := s.buildLogResponse(entry)
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	resp.LatencyMs = map[string]int{"upload": 0, "asr": 0, "model": 0, "write": ms(writeD), "total": ms(time.Since(t0))}
	if ready {
		s.appendEntryFeed(entry, resp.Blocks)
	}
	code := http.StatusOK
	switch status {
	case StatusPending:
		code = http.StatusAccepted
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected part of the entry; nothing more is written and the saved part is being cancelled"))
		return
	}
	b, _ := jsonMarshal(resp)
	if status == StatusDone && ready {
		s.coachEvent(entry, resp.Items, resp.Snapshot)
		_ = s.idem.Put(idemRec{ClientID: body.ClientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: b})
	}
	s.logLine("items", entry.ID, len(its), resp.LatencyMs, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

// handleMove serves POST /fuel/move: the named items go to another day (the
// rules of section 15.6: a new row on the target day plus the undo of the
// old row, one journal transaction, never on both days). No model call.
func (s *Service) handleMove(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var body struct {
		ClientID string   `json:"client_id"`
		Day      string   `json:"day"` // today | yesterday | YYYY-MM-DD
		ItemID   string   `json:"item_id"`
		RowKey   string   `json:"row_key"`
		Items    []string `json:"items"` // item ids or row keys
	}
	if !s.readJSONBody(w, r, &body) {
		return
	}
	ids := append([]string{}, body.Items...)
	for _, id := range []string{body.ItemID, body.RowKey} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if !clientIDRe.MatchString(body.ClientID) || len(ids) == 0 || len(ids) > 12 ||
		(body.Day != "today" && body.Day != "yesterday" && !isoDateRe.MatchString(body.Day)) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id, day (today, yesterday or YYYY-MM-DD) and 1 to 12 items are required"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	now := s.o.Now()
	hash := hashOf("move", body.Day, ids)
	turn := turnOf(r)
	if turn != nil {
		if turn.replay(w, body.ClientID) {
			return
		}
		for _, id := range ids {
			if e := turn.changed(id, "move"); e != nil {
				writeErr(w, e)
				return
			}
		}
	} else {
		claim, ce := s.claim(ctx, body.ClientID, "log", hash)
		if ce != nil {
			writeErr(w, ce)
			return
		}
		if claim.replay != nil {
			s.replayLog(ctx, w, *claim.replay)
			return
		}
		defer claim.release()
	}

	// Find each item on its own day (fresh rows of that day).
	var found []DayItem
	var notes []string
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		date := ""
		if isExternalKey(id) {
			if d, ok := s.findRow(id); ok {
				date = d
			} else if s.loadWindow(ctx) {
				date, _ = s.findRow(id)
			}
		} else if it, ok := s.journal.Item(id); ok {
			date = it.Date
		}
		var hit *DayItem
		if date != "" && s.freshen(ctx, date) {
			s.stateMu.RLock()
			for _, d := range s.dayItems(date) {
				if d.RowKey == id {
					d := d
					hit = &d
				}
			}
			s.stateMu.RUnlock()
		}
		if hit == nil {
			notes = append(notes, "I cannot find the item "+id+" in the log. Nothing was moved.")
			continue
		}
		found = append(found, *hit)
	}
	day := body.Day
	out := &ModelOutput{Intent: "move", RawIntent: "move", Day: &day}
	s.finishMove(ctx, w, out, body.ClientID, hash, "move to "+body.Day, now, t0, map[string]int{"upload": 0, "asr": 0, "model": 0}, &movePreset{found: found, notes: notes, turn: turn})
}

// movePreset names the items of a move directly (POST /fuel/move); nil =
// the chat path resolves them from the model's targets.
type movePreset struct {
	found []DayItem
	notes []string
	turn  *chatTurn // set for a turn-bound move (spec 22.5)
}

// turnItems is POST /fuel/items inside a turn (spec 22.5): the items become
// items of the turn's entry, in one journal line with their rows.
func (s *Service) turnItems(ctx context.Context, w http.ResponseWriter, t *chatTurn, body itemsBody, out *ModelOutput, date string, eatenAt, now time.Time) {
	entry, ae := s.turnEntry(t)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	if t.newItems+len(out.Items) > 12 {
		writeErr(w, errf(http.StatusConflict, "turn_items_full", false, "a turn takes at most 12 new items"))
		return
	}
	s.freshen(ctx, date)
	seen := map[string]bool{}
	for _, it := range out.Items {
		n := normName(it.Item)
		if t.names[n] || seen[n] {
			writeErr(w, errf(http.StatusConflict, "turn_item_exists", false, "%q was already written in this turn (or is twice in this call). To change it use fix with revised values; nothing was written", strings.TrimSpace(it.Item)))
			return
		}
		seen[n] = true
		if body.New {
			continue
		}
		if d, dup := s.likelyDuplicate(t, date, it.Item); dup {
			writeErr(w, errf(http.StatusConflict, "likely_duplicate", false,
				"%q looks like %s (id %s), logged a few minutes ago. If it is the SAME food, use fix with revised values on that id. If the user's words say it is MORE food, repeat this call with --new. Nothing was written",
				strings.TrimSpace(it.Item), s.describeDayItem(d), d.RowKey))
			return
		}
	}
	checks := s.problemsWith(out, implausibleAny)
	its := s.buildItems(out, Entry{ID: entry.ID, Date: date, EatenAt: eatenAt})
	photoRef := strings.Join(entry.PhotoIDs, ",")
	var ops []Op
	var opIDs, itemIDs, lines []string
	first := len(entry.ItemIDs) == 0 && len(entry.FixOps) == 0 && len(entry.FixLines) == 0
	for i := range its {
		if why := checks[i]; why != "" {
			its[i].Check = why
			entry.Checks = append(entry.Checks, "Check this: "+its[i].Name+": "+why+".")
			lines = append(lines, "check flag on "+its[i].Name+": "+why)
		}
		entry.ItemIDs = append(entry.ItemIDs, its[i].ID)
		itemIDs = append(itemIDs, its[i].ID)
		opID := newID("op_")
		opIDs = append(opIDs, opID)
		ops = append(ops, Op{ID: opID, Kind: "original", EntryID: entry.ID, ItemID: its[i].ID, RowItemID: its[i].ID, Date: date,
			Data: originalRowData(its[i], opID, photoRef), Macros: its[i].Orig, CreatedAt: now, State: OpPending, Attempts: 1, LastTry: s.o.Now()})
	}
	entry.Intent = "log"
	if first && date != entry.Date {
		// The reply shows the day of the food (the v4.4 rule).
		entry.Date, entry.DayLabel = date, dayLabel(date)
	}
	t.admit(body.ClientID)
	s.stateMu.Lock()
	err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: its, Ops: ops})
	s.stateMu.Unlock()
	if err != nil {
		t.unadmit(body.ClientID)
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		return
	}
	for _, it := range out.Items {
		t.names[normName(it.Item)] = true
	}
	t.newItems += len(its)
	s.runOps(ctx, ops)
	log.Printf("fuel: turn items entry=%s items=%d", entry.ID, len(its))
	s.turnAnswer(ctx, w, t, date, opIDs, itemIDs, lines)
}
