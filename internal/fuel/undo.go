package fuel

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// isChatFix reports a chat entry that only changes earlier items: a
// correction ("correct") or a removal ("undo"). Both render from FixLines.
func isChatFix(e Entry) bool {
	return e.Intent == "correct" || e.Intent == "undo" || e.Intent == "move"
}

// amountMatches: an item matches a stated amount when its current or its
// originally logged amount is within 2 % (at least 1 unit) of it.
func amountMatches(want *float64, cur, orig *float64) bool {
	if want == nil {
		return true
	}
	tol := math.Max(1, *want*0.02)
	for _, v := range []*float64{cur, orig} {
		if v != nil && math.Abs(*v-*want) <= tol {
			return true
		}
	}
	return false
}

// describeDayItem is "water, 250 ml (09:12)".
func (s *Service) describeDayItem(d DayItem) string {
	amt := ""
	switch {
	case d.Kind == "drink" && d.VolumeML != nil:
		amt = ", " + fmtNum(*d.VolumeML) + " ml"
	case d.PortionG != nil:
		amt = ", " + fmtNum(*d.PortionG) + " g"
	case d.VolumeML != nil:
		amt = ", " + fmtNum(*d.VolumeML) + " ml"
	}
	return d.Item + amt + " (" + s.localHHMM(d.EatenAt) + ")"
}

// resolveUndoTarget finds the ONE active item of today a removal means (spec
// 15.4): by row key / item id, else by name (exact, then containment), then
// by the stated amount, then by "first" / "last" (earliest / newest eaten).
// More than one left = ambiguous: a question, no write.
func (s *Service) resolveUndoTarget(t ModelUndoTarget, items []DayItem) (DayItem, string) {
	ref := strings.TrimSpace(t.Ref)
	var cands []DayItem
	want := map[string]bool{}
	for _, n := range t.Names {
		want[normName(n)] = true
	}
	for _, d := range items {
		// An id match counts only if it is one of the named foods: names
		// constrain every path (an id the model got wrong must not remove
		// another food).
		if d.RowKey == ref && (len(want) == 0 || want[normName(d.Item)]) {
			cands = []DayItem{d}
			break
		}
	}
	if len(cands) == 0 && len(t.Names) > 0 {
		// The named food(s) first, exact (case-insensitive) names only: a
		// synonym counts only when the model listed it.
		for _, d := range items {
			if want[normName(d.Item)] {
				cands = append(cands, d)
			}
		}
		if len(cands) == 0 {
			return DayItem{}, "I could not find " + strings.Join(t.Names, " or ") + " in today's log to remove."
		}
	} else if len(cands) == 0 {
		if strings.EqualFold(ref, "last") {
			cands = append(cands, items...)
		} else {
			n := normName(ref)
			for _, d := range items {
				if normName(d.Item) == n {
					cands = append(cands, d)
				}
			}
			if len(cands) == 0 {
				for _, d := range items {
					in := normName(d.Item)
					if strings.Contains(in, n) || strings.Contains(n, in) {
						cands = append(cands, d)
					}
				}
			}
		}
	}
	var amt []DayItem
	for _, d := range cands {
		if amountMatches(t.VolumeML, d.VolumeML, d.origVolume) && amountMatches(t.PortionG, d.PortionG, d.origPortion) {
			amt = append(amt, d)
		}
	}
	cands = amt
	if len(cands) == 0 {
		return DayItem{}, "I could not find " + strings.TrimSpace(t.Ref) + " in today's log to remove."
	}
	// items are newest first; "first" = the earliest eaten.
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].EatenAt.After(cands[b].EatenAt) })
	if t.Which != nil {
		if *t.Which == "first" {
			return cands[len(cands)-1], ""
		}
		return cands[0], ""
	}
	if len(cands) == 1 {
		return cands[0], ""
	}
	var opts []string
	for i := len(cands) - 1; i >= 0 && len(opts) < 5; i-- {
		opts = append(opts, s.describeDayItem(cands[i]))
	}
	return DayItem{}, "Which one do you mean: " + strings.Join(opts, " or ") + "? Nothing was removed."
}

// finishUndo resolves and writes a chat removal (intent "undo") and answers
// in the POST /fuel/log shape like a correction.
func (s *Service) finishUndo(ctx context.Context, w http.ResponseWriter, out *ModelOutput, clientID, hash, userText string, now, t0 time.Time, lat map[string]int) {
	today := s.today()
	// Resolve against a FRESH read of today (not the cache): an item another
	// writer just added must make "the water" ambiguous.
	if err := s.cache.RefreshDay(ctx, today); err != nil {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
		return
	}
	type target struct {
		d    DayItem
		it   Item
		ext  bool
		note string
	}
	resolve := func() ([]target, bool) {
		s.stateMu.RLock()
		items := s.dayItems(today)
		s.stateMu.RUnlock()
		var ts []target
		seen := map[string]bool{}
		clean := true
		for _, t := range out.Targets {
			d, note := s.resolveUndoTarget(t, items)
			if note != "" {
				ts = append(ts, target{note: note})
				clean = false
				continue
			}
			if seen[d.RowKey] {
				continue
			}
			seen[d.RowKey] = true
			tg := target{d: d, ext: isExternalKey(d.RowKey)}
			if tg.ext {
				rows, _, _ := s.cache.Rows(today)
				tg.it, _ = externalItem(d.RowKey, today, rows)
			} else if it, ok := s.journal.Item(d.RowKey); ok {
				tg.it = it
			} else {
				tg.note = "I cannot remove " + d.Item + " from here."
				clean = false
			}
			ts = append(ts, tg)
		}
		return ts, clean
	}
	// All targets resolve, or nothing is written: an ambiguous or unknown
	// target makes the whole message a question (spec 15.4).
	answerOnly := func(lines []FixLine) {
		entry := Entry{ID: newID("en_"), ClientID: clientID, Date: today, EatenAt: now, CreatedAt: now,
			Intent: "undo", PhotoIDs: []string{}, ReqHash: hash, UserText: userText, FixLines: lines, FixText: joinLines(lines)}
		s.finishUndoEntry(ctx, w, entry, nil, nil, clientID, hash, now, t0, lat, func() {})
	}
	targets, clean := resolve()
	if !clean {
		var lines []FixLine
		for _, tg := range targets {
			if tg.note != "" {
				lines = append(lines, FixLine{Text: tg.note})
			}
		}
		if len(out.Targets) > 1 {
			lines = append(lines, FixLine{Text: "Nothing was removed."})
		}
		answerOnly(lines)
		return
	}
	// Lock in id order, re-read, then build one undo op per target.
	sort.SliceStable(targets, func(a, b int) bool { return targets[a].it.ID < targets[b].it.ID })
	var locks []*itemMutex
	defer func() {
		for _, l := range locks {
			l.Unlock()
		}
	}()
	for _, tg := range targets {
		if tg.it.ID == "" || tg.note != "" {
			continue
		}
		l := s.itemLock(tg.it.ID)
		if !l.LockCtx(ctx) {
			writeErr(w, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry"))
			return
		}
		locks = append(locks, l)
	}
	if err := s.cache.RefreshDay(ctx, today); err != nil {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry"))
		return
	}
	// Re-resolve under the locks on the re-read rows: the same items, or
	// nothing is written.
	again, clean2 := resolve()
	want := map[string]bool{}
	for _, t := range targets {
		want[t.d.RowKey] = true
	}
	same := clean2 && len(again) == len(targets)
	for _, a := range again {
		same = same && want[a.d.RowKey]
	}
	if !same {
		for _, l := range locks {
			l.Unlock()
		}
		locks = nil
		answerOnly([]FixLine{{Text: "Your log changed while I looked; please say again which item to remove. Nothing was removed."}})
		return
	}
	for i := range targets {
		for _, a := range again {
			if a.d.RowKey == targets[i].d.RowKey {
				targets[i].d, targets[i].it = a.d, a.it // current amounts for the reply
			}
		}
	}
	rows, _, _ := s.cache.Rows(today)
	entry := Entry{ID: newID("en_"), ClientID: clientID, Date: today, EatenAt: now, CreatedAt: now,
		Intent: "undo", PhotoIDs: []string{}, ReqHash: hash, UserText: userText}
	var ops []Op
	var stand []Item
	for _, tg := range targets {
		if tg.note != "" || tg.it.ID == "" {
			entry.FixLines = append(entry.FixLines, FixLine{Text: tg.note})
			continue
		}
		v := s.viewItem(tg.it)
		g := itemGroup(rows, tg.it.ID)
		switch {
		case v.pending:
			entry.FixLines = append(entry.FixLines, FixLine{ItemID: tg.it.ID, Text: tg.d.Item + " is still being saved; try again in a moment."})
			continue
		case g == nil || g.c.undone || v.state.Undone:
			entry.FixLines = append(entry.FixLines, FixLine{ItemID: tg.it.ID, Text: tg.d.Item + " was already removed."})
			continue
		}
		op := s.newCorrectionOp(tg.it, requiredKnown(g.c.cancel()), "undo", nil)
		if tg.ext {
			s.deterministicOp(&op, "op_undo_"+strings.TrimPrefix(tg.it.ID, "v:"))
			stand = append(stand, tg.it)
		}
		op.Attempts, op.LastTry = 1, now
		ops = append(ops, op)
		entry.ItemIDs = append(entry.ItemIDs, tg.it.ID)
		entry.FixOps = append(entry.FixOps, op.ID)
		entry.FixLines = append(entry.FixLines, FixLine{OpID: op.ID, ItemID: tg.it.ID, Text: "Removed " + s.describeDayItem(tg.d) + "."})
	}
	entry.FixText = joinLines(entry.FixLines)
	held := locks
	locks = nil // released by finishUndoEntry right after the journal write
	s.finishUndoEntry(ctx, w, entry, stand, ops, clientID, hash, now, t0, lat, func() {
		for _, l := range held {
			l.Unlock()
		}
	})
}

// finishUndoEntry journals a chat-removal entry (with its ops, if any),
// writes the ops and answers in the POST /fuel/log shape.
func (s *Service) finishUndoEntry(ctx context.Context, w http.ResponseWriter, entry Entry, stand []Item, ops []Op, clientID, hash string, now, t0 time.Time, lat map[string]int, release func()) {
	tWrite := time.Now()
	s.stateMu.Lock()
	err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Items: stand, Ops: ops})
	s.stateMu.Unlock()
	release() // the item locks are held until the ops are journaled
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
		s.runOps(ctx, ops)
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
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the removal"))
		return
	}
	b, _ := jsonMarshal(resp)
	if status == StatusDone && ready {
		_ = s.idem.Put(idemRec{ClientID: clientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: b})
	}
	s.logLine("undo", entry.ID, len(ops), lat, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func joinLines(ls []FixLine) string {
	var parts []string
	for _, l := range ls {
		parts = append(parts, l.Text)
	}
	return strings.Join(parts, " ")
}

var _ = fmt.Sprintf
