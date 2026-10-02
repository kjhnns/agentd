package fuel

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// dayLabel is "Wed 30 Sep".
func dayLabel(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.Format("Mon 2 Jan")
}

// resolveDay turns the model's day / time into a record date and an eaten
// instant in the targets tz (spec 15.6). day nil or "today" keeps the
// request's own date and time (a stated time today replaces the clock time
// unless it is in the future). A past day gets the stated time, else 12:00.
// Only today and the 34 days before it are accepted; refusal is a text for
// the user ("" = accepted).
func resolveDay(day, clock *string, now time.Time, loc *time.Location, baseDate string, baseAt time.Time) (string, time.Time, string) {
	today := now.In(loc).Format("2006-01-02")
	date := baseDate
	stated := false
	if day != nil {
		switch *day {
		case "today":
			date = today
		case "yesterday":
			date, stated = dateAdd(today, -1), true
		default:
			date, stated = *day, *day != today
			if _, err := time.ParseInLocation("2006-01-02", date, loc); err != nil {
				return "", time.Time{}, "I could not read the day " + *day + ". Nothing was logged."
			}
		}
	}
	switch {
	case date > today:
		return "", time.Time{}, "I cannot log for a day in the future (" + dayLabel(date) + "). Nothing was logged."
	case date < dateAdd(today, -34):
		return "", time.Time{}, "I can only log for today and the 34 days before it; " + dayLabel(date) + " is too far back. Nothing was logged."
	}
	// The fallback follows the RESOLVED day: the request's own instant when
	// it is on that day, else the request time for today, noon for a past day.
	at := baseAt
	if baseAt.In(loc).Format("2006-01-02") != date || stated {
		if date == today {
			at = now
		} else {
			at = localClock(date, 12*60, loc)
		}
	}
	if clock != nil {
		m, _ := parseHHMM(*clock)
		if t := localClock(date, m, loc); !t.After(now) { // a future time is ignored
			at = t
		}
	}
	return date, at, ""
}

// localClock is the instant of a wall-clock minute on a local date, built
// with time.Date (DST days have 23 or 25 hours; a nonexistent time moves
// forward, a repeated one takes the first).
func localClock(date string, minute int, loc *time.Location) time.Time {
	d, _ := time.Parse("2006-01-02", date)
	return time.Date(d.Year(), d.Month(), d.Day(), minute/60, minute%60, 0, 0, loc)
}

// ---- move ----

// moveTargets resolves what a "move" means: no targets = every active item
// of the newest log entry; a target is looked up among the newest entry's
// active items first (ALL of them that match its names / ref, or the one
// `which` picks), else like a removal among today's items (ambiguous =
// a question). It returns day items (current amounts) or notes.
func (s *Service) moveTargets(out *ModelOutput) ([]DayItem, []string) {
	today := s.today()
	newest, hasNewest := s.newestLogEntry()
	var newestItems []DayItem
	if hasNewest {
		inEntry := map[string]bool{}
		for _, id := range newest.ItemIDs {
			inEntry[id] = true
		}
		s.stateMu.RLock()
		byKey := map[string]DayItem{}
		for _, d := range s.dayItems(newest.Date) {
			if inEntry[d.RowKey] {
				byKey[d.RowKey] = d
			}
		}
		s.stateMu.RUnlock()
		for _, id := range newest.ItemIDs { // the entry's own order
			if d, ok := byKey[id]; ok {
				newestItems = append(newestItems, d)
			}
		}
	}
	if len(out.Targets) == 0 {
		if len(newestItems) == 0 {
			return nil, []string{"I found nothing in the last entry to move."}
		}
		return newestItems, nil
	}
	s.stateMu.RLock()
	todays := s.dayItems(today)
	s.stateMu.RUnlock()
	var got []DayItem
	var notes []string
	seen := map[string]bool{}
	add := func(d DayItem) {
		if !seen[d.RowKey] {
			seen[d.RowKey] = true
			got = append(got, d)
		}
	}
	for _, t := range out.Targets {
		// In the newest entry every match moves (no ambiguity question).
		var hits []DayItem
		want := map[string]bool{}
		for _, n := range t.Names {
			want[normName(n)] = true
		}
		ref := strings.TrimSpace(t.Ref)
		// An id narrows to one item, but only if it is one of the named
		// foods; names (when given) are the constraint.
		for _, d := range newestItems {
			if d.RowKey == ref && (len(want) == 0 || want[normName(d.Item)]) {
				hits = []DayItem{d}
				break
			}
		}
		if len(hits) == 0 {
			for _, d := range newestItems {
				byName := len(want) > 0 && want[normName(d.Item)]
				// An unnamed "last" means the newest entry itself (all of its
				// items, narrowed by amount and which), never an older one.
				byRefName := len(want) == 0 && (strings.EqualFold(ref, "last") || normName(d.Item) == normName(ref))
				if byName || byRefName {
					hits = append(hits, d)
				}
			}
		}
		var fit []DayItem
		for _, d := range hits {
			if amountMatches(t.VolumeML, d.VolumeML, d.origVolume) && amountMatches(t.PortionG, d.PortionG, d.origPortion) {
				fit = append(fit, d)
			}
		}
		hits = fit
		if len(hits) > 0 {
			if t.Which != nil {
				sort.SliceStable(hits, func(a, b int) bool { return hits[a].EatenAt.Before(hits[b].EatenAt) })
				if *t.Which == "first" {
					hits = hits[:1]
				} else {
					hits = hits[len(hits)-1:]
				}
			}
			for _, d := range hits {
				add(d)
			}
			continue
		}
		if len(want) == 0 && strings.EqualFold(ref, "last") {
			// An unnamed "last" never reaches older items.
			notes = append(notes, "I found nothing in the last entry to move. Nothing was moved.")
			continue
		}
		d, note := s.resolveUndoTarget(t, todays)
		if note != "" {
			notes = append(notes, strings.Replace(strings.Replace(note, "to remove", "to move", 1), "Nothing was removed.", "Nothing was moved.", 1))
			continue
		}
		add(d)
	}
	return got, notes
}

// finishMove moves items to another day (spec 15.6): ONE journal txn holds,
// per item, the new row on the target day and the undo of the old row. The
// new row is written first; the undo is posted only once the new row is
// done (PairOp), so a crash or a rejection can leave the item on its old
// day or on the new one, never on both and never on neither.
func (s *Service) finishMove(ctx context.Context, w http.ResponseWriter, out *ModelOutput, clientID, hash, userText string, now, t0 time.Time, lat map[string]int, preset *movePreset) {
	targets, terr := s.loadTargets()
	if terr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	today := s.today()
	answer := func(date string, lines []FixLine, stand []Item, items []Item, ops []Op, release func()) {
		if preset != nil && preset.turn != nil {
			s.turnMove(ctx, w, preset.turn, clientID, hash, date, lines, stand, items, ops, release)
			return
		}
		entry := Entry{ID: newID("en_"), ClientID: clientID, Date: date, EatenAt: now, CreatedAt: now,
			Intent: "move", PhotoIDs: []string{}, ReqHash: hash, UserText: userText, FixLines: lines, FixText: joinLines(lines), MoveTo: date, Clinical: out.clinical()}
		for i := range items {
			items[i].EntryID = entry.ID
			entry.ItemIDs = append(entry.ItemIDs, items[i].ID)
		}
		for i := range ops {
			if ops[i].Kind == "original" {
				ops[i].EntryID = entry.ID
				ops[i].Data["entry_id"] = entry.ID
			}
			entry.FixOps = append(entry.FixOps, ops[i].ID)
		}
		s.finishMoveEntry(ctx, w, entry, append(stand, items...), ops, clientID, hash, now, t0, lat, release)
	}
	ref, refDay := now, today
	if preset != nil && preset.turn != nil {
		// A turn-bound move: "yesterday" is the day before the user's message.
		ref, refDay = preset.turn.at, preset.turn.date
	}
	date, _, refuse := resolveDay(out.Day, nil, ref, targets.loc, refDay, ref)
	if refuse != "" {
		answer(today, []FixLine{{Text: strings.Replace(strings.Replace(refuse, "log for", "move to", 1), "Nothing was logged.", "Nothing was moved.", 1)}}, nil, nil, nil, func() {})
		return
	}
	if err := s.cache.RefreshDay(ctx, today); err != nil {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
		return
	}
	if e, ok := s.newestLogEntry(); ok && e.Date != today {
		if !s.freshen(ctx, e.Date) {
			writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
			return
		}
	}
	var found []DayItem
	var notes []string
	if preset != nil {
		found, notes = preset.found, preset.notes // POST /fuel/move names the items
	} else {
		found, notes = s.moveTargets(out)
	}
	if len(notes) > 0 || len(found) == 0 {
		var lines []FixLine
		for _, n := range notes {
			lines = append(lines, FixLine{Text: n})
		}
		if len(lines) == 0 {
			lines = []FixLine{{Text: "I found nothing to move."}}
		}
		answer(today, lines, nil, nil, nil, func() {})
		return
	}
	// Lock in key order (the reply keeps the entry's order), re-read the
	// source days, re-check each item.
	byKey := append([]DayItem(nil), found...)
	sort.SliceStable(byKey, func(a, b int) bool { return byKey[a].RowKey < byKey[b].RowKey })
	var locks []*itemMutex
	release := func() {
		for _, l := range locks {
			l.Unlock()
		}
		locks = nil
	}
	for _, d := range byKey {
		l := s.itemLock(d.RowKey)
		if !l.LockCtx(ctx) {
			release()
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
			return
		}
		locks = append(locks, l)
	}
	var lines []FixLine
	var stand, items []Item
	var ops []Op
	read := map[string]bool{}
	skipped := 0 // items that cannot be moved (checked under their locks)
	for _, d := range found {
		srcDate := d.EatenAt.In(targets.loc).Format("2006-01-02")
		var old Item
		ext := isExternalKey(d.RowKey)
		if !ext {
			it, ok := s.journal.Item(d.RowKey)
			if !ok {
				lines = append(lines, FixLine{Text: "I cannot move " + d.Item + " from here."})
				skipped++
				continue
			}
			old, srcDate = it, it.Date
		} else if dt, ok := s.findRow(d.RowKey); ok {
			srcDate = dt
		}
		if !read[srcDate] {
			if err := s.cache.RefreshDay(ctx, srcDate); err != nil {
				release()
				writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
				return
			}
			read[srcDate] = true
		}
		rows, _, _ := s.cache.Rows(srcDate)
		if ext {
			var ok bool
			if old, ok = externalItem(d.RowKey, srcDate, rows); !ok {
				lines = append(lines, FixLine{Text: d.Item + " is no longer in the log."})
				skipped++
				continue
			}
		}
		g := itemGroup(rows, old.ID)
		v := s.viewItem(old)
		switch {
		case srcDate == date:
			lines = append(lines, FixLine{ItemID: old.ID, Text: d.Item + " is already on " + dayLabel(date) + "."})
			skipped++
			continue
		case v.pending:
			lines = append(lines, FixLine{ItemID: old.ID, Text: d.Item + " is still being saved; try again in a moment."})
			skipped++
			continue
		case g == nil || g.orig.Data == nil || g.c.undone || v.state.Undone:
			lines = append(lines, FixLine{ItemID: old.ID, Text: d.Item + " was already removed."})
			skipped++
			continue
		}
		// The new row: the item's CURRENT amounts on the target day, at the
		// same clock time, with the same photos.
		eff := g.c.effective()
		portion, _ := amountsAfter(g, eff, func() Macros { m := macrosFromData(g.orig.Data); m.normalizeNetCarbs(); return m }())
		clock := old.EatenAt.In(targets.loc)
		at := localClock(date, clock.Hour()*60+clock.Minute(), targets.loc)
		basis, _ := g.orig.Data["portion_basis"].(string)
		if basis == "" {
			basis = "unspecified"
		}
		kind, _ := g.orig.Data["kind"].(string)
		ni := Item{ID: newID("it_"), Date: date, Name: d.Item, PortionG: portion, Basis: basis, Kind: kind,
			Orig: requiredKnown(eff), EatenAt: at}
		// The new row carries the item's CURRENT lever amounts (tagged levers
		// only) and its brew method (spec 18.6).
		ls := reduceLevers(g)
		ni.levers, ni.brew = ls.amount, ls.brew
		nopID := newID("op_")
		photoRef, _ := g.orig.Data["photo_ref"].(string)
		data := originalRowData(ni, nopID, photoRef)
		data["moved_from"] = strings.TrimPrefix(old.ID, "v:")
		data["moved_from_date"] = srcDate
		nop := Op{ID: nopID, Kind: "original", ItemID: ni.ID, RowItemID: ni.ID, Date: date, Data: data,
			Macros: ni.Orig, CreatedAt: now, State: OpPending, Attempts: 1, LastTry: now}
		uop := s.newCorrectionOp(old, requiredKnown(g.c.cancel()), "undo", nil)
		leverCancel(ls).putInto(uop.Data)
		if ext {
			s.deterministicOp(&uop, "op_undo_"+strings.TrimPrefix(old.ID, "v:"))
			stand = append(stand, old)
		}
		uop.Data["moved_to"] = date
		uop.PairOp = nop.ID                       // posted only after the new row is done
		uop.CreatedAt = now.Add(time.Millisecond) // reconciled after its pair
		items = append(items, ni)
		ops = append(ops, nop, uop)
		lines = append(lines, FixLine{OpID: uop.ID, ItemID: old.ID, Text: d.Item})
	}
	if preset != nil && skipped > 0 {
		// POST /fuel/move names its items: all of them move, or none (an item
		// that another request removed meanwhile must not leave a half move).
		release()
		var notes []FixLine
		for _, l := range lines {
			if l.OpID == "" {
				notes = append(notes, FixLine{Text: l.Text})
			}
		}
		notes = append(notes, FixLine{Text: "Nothing was moved."})
		answer(today, notes, nil, nil, nil, func() {})
		return
	}
	answer(date, lines, stand, items, ops, release)
}

// finishMoveEntry journals the move (one txn), runs it and answers in the
// POST /fuel/log shape.
func (s *Service) finishMoveEntry(ctx context.Context, w http.ResponseWriter, entry Entry, items []Item, ops []Op, clientID, hash string, now, t0 time.Time, lat map[string]int, release func()) {
	tWrite := time.Now()
	s.stateMu.Lock()
	err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: items, Ops: ops})
	s.stateMu.Unlock()
	release()
	if err != nil {
		if err == ErrDeadline {
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
		} else {
			writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		}
		return
	}
	_ = s.idem.Put(idemRec{ClientID: clientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs})
	if len(ops) > 0 {
		s.runMoveOps(ctx, ops)
	}
	lat["write"] = ms(time.Since(tWrite))
	ready := s.renderReady(ctx, entry.Date)
	resp, status := s.buildLogResponse(entry)
	if resp.renderErr != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the request itself was saved, retry to see it"))
		return
	}
	lat["total"] = ms(time.Since(t0))
	resp.LatencyMs = lat
	if ready {
		s.appendEntryFeed(entry, resp.Blocks)
	}
	code := http.StatusOK
	switch status {
	case StatusPending:
		code = http.StatusAccepted
	}
	b, _ := jsonMarshal(resp)
	if status == StatusDone && ready {
		_ = s.idem.Put(idemRec{ClientID: clientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: b})
	}
	s.logLine("move", entry.ID, len(ops)/2, lat, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

// runMoveOps writes the pairs in order (new row, then the undo of the old
// one) and waits for them or the budget. Detached like runOps.
func (s *Service) runMoveOps(ctx context.Context, ops []Op) {
	done := make(chan struct{})
	s.writers.Add(1)
	go func() {
		defer s.writers.Done()
		defer close(done)
		for _, o := range ops {
			s.postMoveOp(o.ID)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// postMoveOp posts one op of a move under its item lock (the lock the
// reconciler takes), on the op's CURRENT journal state: only an op nobody
// has attempted beyond what the txn recorded is posted here; anything else
// belongs to the reconciler.
func (s *Service) postMoveOp(opID string) {
	first, ok := s.journal.Op(opID)
	if !ok {
		return
	}
	l := s.itemLock(first.ItemID)
	l.Lock()
	defer l.Unlock()
	op, ok := s.journal.Op(opID)
	if !ok || op.State != OpPending {
		return // done, failed, or already attempted by the reconciler
	}
	if op.PairOp != "" {
		if op.Attempts != 0 || !s.pairReady(op) {
			return
		}
		s.postOp(context.Background(), op, false) // journals attempt 1
		return
	}
	if op.Attempts != 1 {
		return
	}
	s.postOp(context.Background(), op, true) // attempt 1 is in the txn
}

// isMoveRow reports the new (destination) row op of a move.
func isMoveRow(op Op) bool {
	_, ok := op.Data["moved_from"]
	return ok && op.Kind == "original"
}

// pairReady reports whether a paired undo may be posted: its new row is
// done. A failed pair fails the undo without posting it (the item stays on
// its old day).
func (s *Service) pairReady(op Op) bool {
	pair, ok := s.journal.Op(op.PairOp)
	if !ok {
		return true
	}
	switch pair.State {
	case OpDone:
		return true
	case OpFailed:
		s.failOp(op, s.o.Now())
	}
	return false
}

// moveSummary renders a move reply from the CURRENT state of its undo ops:
// "Moved a, b and c to Wed 30 Sep." plus what is still moving or failed.
func (s *Service) moveSummary(e Entry) string {
	var moved, moving, failed, notes []string
	for _, l := range e.FixLines {
		if l.OpID == "" {
			notes = append(notes, l.Text)
			continue
		}
		op, ok := s.journal.Op(l.OpID)
		switch {
		case ok && op.State == OpDone:
			moved = append(moved, l.Text)
		case ok && op.State == OpFailed:
			failed = append(failed, l.Text)
		default:
			moving = append(moving, l.Text)
		}
	}
	var parts []string
	if len(moved) > 0 {
		parts = append(parts, "Moved "+joinAnd(moved)+" to "+dayLabel(e.MoveTo)+".")
	}
	if len(moving) > 0 {
		parts = append(parts, "Still moving "+joinAnd(moving)+" to "+dayLabel(e.MoveTo)+".")
	}
	if len(failed) > 0 {
		parts = append(parts, "Could not move "+joinAnd(failed)+"; it stays where it was.")
	}
	return strings.Join(append(parts, notes...), " ")
}

// joinAnd is "a, b and c".
func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// turnMove is POST /fuel/move inside a turn (spec 22.5): the new rows and the
// undo ops belong to the entry of the turn. A request that moves nothing
// writes nothing.
func (s *Service) turnMove(ctx context.Context, w http.ResponseWriter, t *chatTurn, clientID, hash, date string, lines []FixLine, stand, items []Item, ops []Op, release func()) {
	entry, ae := s.turnEntry(t)
	if ae != nil {
		release()
		writeErr(w, ae)
		return
	}
	if len(ops) == 0 {
		release()
		var texts []string
		for _, l := range lines {
			texts = append(texts, l.Text)
		}
		writeErr(w, errf(http.StatusConflict, "not_applicable", false, "%s", strings.Join(append(texts, "Nothing was moved."), " ")))
		return
	}
	var opIDs, itemIDs, texts []string
	for i := range items {
		items[i].EntryID = entry.ID
		entry.MovedIDs = append(entry.MovedIDs, items[i].ID)
		itemIDs = append(itemIDs, items[i].ID)
	}
	for i := range ops {
		if ops[i].Kind == "original" {
			ops[i].EntryID = entry.ID
			ops[i].Data["entry_id"] = entry.ID
		}
		entry.FixOps = append(entry.FixOps, ops[i].ID)
		opIDs = append(opIDs, ops[i].ID)
	}
	for _, l := range lines {
		if l.OpID != "" {
			l.Move = date
			texts = append(texts, "Moved "+l.Text+" to "+dayLabel(date)+".")
		} else {
			texts = append(texts, l.Text)
		}
		entry.FixLines = append(entry.FixLines, l)
	}
	entry.MoveTo = date
	entry.FixText = joinLines(entry.FixLines)
	if entry.Intent == "question" {
		entry.Intent = "move"
	}
	t.admit(clientID, hash)
	s.stateMu.Lock()
	err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: append(stand, items...), Ops: ops})
	s.stateMu.Unlock()
	release()
	if err != nil {
		turnPersistErr(w, t, clientID, err)
		return
	}
	// The pairs are (new row, undo of the old row): the old item took its
	// move, and the new row carries the old item's history of this turn.
	for i := 0; i+1 < len(ops); i += 2 {
		t.mark(ops[i+1].ItemID, "move")
		t.inherit(ops[i+1].ItemID, ops[i].ItemID)
	}
	s.runMoveOps(ctx, ops)
	log.Printf("fuel: turn move entry=%s items=%d", entry.ID, len(ops)/2)
	s.turnAnswer(ctx, w, t, t.date, opIDs, itemIDs, texts)
}
