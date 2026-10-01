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

// FixTarget is the corrected amount of an item: exactly one of the share of
// the ORIGINAL row actually consumed, the grams eaten or the ml drunk.
type FixTarget struct {
	Share    *float64 `json:"share,omitempty"`
	PortionG *float64 `json:"portion_g,omitempty"`
	VolumeML *float64 `json:"volume_ml,omitempty"`
}

func (t FixTarget) count() int {
	n := 0
	for _, p := range []*float64{t.Share, t.PortionG, t.VolumeML} {
		if p != nil {
			n++
		}
	}
	return n
}

// absoluteCorrection computes the ONE correction row that makes the item
// count as the target amount (spec section 15): target = original row x
// share, delta = target - the item's current contribution in the
// authoritative rows. Because it is absolute against the original, a
// repeated request is a no-op (nil op). Null fields stay null.
func (s *Service) absoluteCorrection(it Item, rows []Value, tgt FixTarget, reason string, fraction *float64) (*Op, *apiError) {
	g := itemGroup(rows, it.ID)
	if g == nil || g.orig.Data == nil {
		return nil, errf(http.StatusNotFound, "not_found", false, "the original row of this item was deleted")
	}
	ic, origRow := g.c, g.orig
	if ic.undone {
		// Undone in the authoritative rows (also by the agentd food-log).
		return nil, errf(http.StatusConflict, "already_undone", false, "the item is already undone")
	}
	base := macrosFromData(origRow.Data)
	base.normalizeNetCarbs()
	var origPortion, origVolume *float64
	if p, ok := origRow.Data["portion_g"].(float64); ok && p > 0 {
		origPortion = &p
	}
	if v, ok := origRow.Data["volume_ml"].(float64); ok && v > 0 {
		origVolume = &v
	}
	var share float64
	switch {
	case tgt.Share != nil:
		share = *tgt.Share
	case tgt.PortionG != nil:
		if origPortion == nil {
			return nil, errf(http.StatusConflict, "portion_unknown", false, "the logged portion is unknown; give a share instead")
		}
		share = *tgt.PortionG / *origPortion
	case tgt.VolumeML != nil:
		if origVolume == nil {
			return nil, errf(http.StatusConflict, "volume_unknown", false, "the logged volume is unknown; give a share instead")
		}
		share = *tgt.VolumeML / *origVolume
	}
	if share <= 0 || share > 4 || math.IsNaN(share) {
		return nil, errf(http.StatusBadRequest, "bad_input", false, "the corrected amount must be more than 0 and at most 4 times the logged one")
	}
	target := base.Scale(share)
	var delta Macros
	any := false
	for i, f := range delta.fields() {
		k := AllKeys[i]
		tv := target.Get(k)
		if !tv.OK {
			continue // null stays null
		}
		*f = known(tv.V - ic.sum[k])
		if f.V != 0 {
			any = true
		}
	}
	if !any {
		// The rounded amounts may be unchanged while the portion or volume
		// is not (a zero-macro supplement by grams): then the row still
		// records the new amount, with a zero delta.
		curP, curV := amountsAfter(g, ic.effective(), base)
		differs := func(cur, orig *float64) bool {
			if orig == nil {
				return false
			}
			want := *orig * share
			return cur == nil || math.Abs(*cur-want) > 0.05
		}
		if !differs(curP, origPortion) && !differs(curV, origVolume) {
			return nil, nil // already counted at that amount
		}
	}
	op := s.newCorrectionOp(it, requiredKnown(delta), reason, fraction)
	op.Data["share_after"] = math.Round(share*1000) / 1000
	if origPortion != nil {
		op.Data["portion_g_after"] = round1(*origPortion * share)
	}
	if origVolume != nil {
		op.Data["volume_ml_after"] = round1(*origVolume * share)
	}
	return &op, nil
}

// describeFix is the code-generated lead of a fix reply.
func describeFix(it Item, op *Op) string {
	if op == nil {
		return it.Name + " was already counted at that amount."
	}
	v, hasV := op.Data["volume_ml_after"].(float64)
	p, hasP := op.Data["portion_g_after"].(float64)
	switch {
	case hasV && (it.KindOr() == "drink" || !hasP):
		return fmt.Sprintf("Corrected %s to %s ml.", it.Name, fmtNum(v))
	case hasP:
		return fmt.Sprintf("Corrected %s to %s g.", it.Name, fmtNum(p))
	}
	if sh, ok := op.Data["share_after"].(float64); ok {
		return fmt.Sprintf("Corrected %s to %s of what was logged.", it.Name, shareWords(sh))
	}
	return "Corrected " + it.Name + "."
}

func shareWords(f float64) string {
	switch f {
	case 0.25, 0.5, 0.75, 1:
		return fractionWords(f)
	}
	return fmt.Sprintf("%.0f %%", f*100)
}

// ---- chat corrections ----

// lastItems are the active items of the newest log entry with items (the
// model's reference list for "no, that was 100 g").
func (s *Service) lastItems() []LastItem {
	items := s.todaysActiveItems() // Fuel items, newest first
	newest, _ := s.newestLogEntry()
	type timed struct {
		li LastItem
		at time.Time
	}
	var all []timed
	fuel := map[string]bool{}
	for i := len(items) - 1; i >= 0; i-- { // oldest first
		it := items[i]
		fuel[it.ID] = true
		li := LastItem{ItemID: it.ID, Item: it.Name, Kind: it.KindOr(), PortionG: it.PortionG, NewestEntry: it.EntryID == newest.ID, At: s.localHHMM(it.EatenAt)}
		if it.Orig.VolumeML.OK && it.Orig.VolumeML.V > 0 {
			v := it.Orig.VolumeML.float()
			li.VolumeML = &v
		}
		if fitsUnit(it, unitGrams) {
			li.Units = append(li.Units, "g")
		}
		if fitsUnit(it, unitML) {
			li.Units = append(li.Units, "ml")
		}
		all = append(all, timed{li, it.EatenAt})
	}
	// Today's rows of OTHER writers (Telegram), so a removal can name them.
	s.stateMu.RLock()
	day := s.dayItems(s.today())
	s.stateMu.RUnlock()
	for i := len(day) - 1; i >= 0; i-- {
		d := day[i]
		if fuel[d.RowKey] || !isExternalKey(d.RowKey) {
			continue
		}
		all = append(all, timed{LastItem{ItemID: d.RowKey, Item: d.Item, Kind: d.Kind, PortionG: d.PortionG, VolumeML: d.VolumeML, At: s.localHHMM(d.EatenAt)}, d.EatenAt})
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].at.Before(all[b].at) })
	var out []LastItem
	for _, t := range all {
		out = append(out, t.li)
	}
	if len(out) > 20 {
		out = out[len(out)-20:] // the 20 newest, newest last
	}
	return out
}

// newestLogEntry is the newest entry with intent log (also one with no
// items, such as a no-food photo).
func (s *Service) newestLogEntry() (Entry, bool) {
	var newest *Entry
	for _, e := range s.journal.Entries() {
		e := e
		if e.Intent != "log" {
			continue
		}
		if newest == nil || e.CreatedAt.After(newest.CreatedAt) {
			newest = &e
		}
	}
	if newest == nil {
		return Entry{}, false
	}
	return *newest, true
}

// itemActive: not undone in the journal AND not undone in the cached
// authoritative rows (the agentd food-log can undo a Fuel item too).
func (s *Service) itemActive(it Item) bool {
	if s.viewItem(it).state.Undone {
		return false
	}
	rows, _, _ := s.cache.Rows(it.Date)
	if g := itemGroup(rows, it.ID); g != nil && g.c.undone {
		return false
	}
	return true
}

// Units of a chat correction (spec 15.2, v4.1).
const (
	unitGrams = "g"
	unitML    = "ml"
	unitShare = "share"
)

func correctionUnit(c ModelCorrection) string {
	switch {
	case c.PortionG != nil:
		return unitGrams
	case c.VolumeML != nil:
		return unitML
	}
	return unitShare
}

// fitsUnit: a grams correction needs an item with a portion in grams that
// is not a drink; a ml correction needs an item with a volume; a share fits
// anything.
func fitsUnit(it Item, unit string) bool {
	switch unit {
	case unitShareAnyName:
		return true
	case unitGrams:
		return it.PortionG != nil && *it.PortionG > 0 && it.KindOr() != "drink"
	case unitML:
		return it.Orig.VolumeML.OK && it.Orig.VolumeML.V > 0
	}
	return true
}

const lastSearchEntries = 8

// todaysActiveItems are the active items of today's newest log entries
// (at most lastSearchEntries entries), newest first.
func (s *Service) todaysActiveItems() []Item {
	today := s.today()
	entries := s.journal.Entries()
	sort.Slice(entries, func(a, b int) bool { return entries[a].CreatedAt.After(entries[b].CreatedAt) })
	var out []Item
	n := 0
	for _, e := range entries {
		if e.Intent != "log" || e.Date != today {
			continue
		}
		n++
		if n > lastSearchEntries {
			break
		}
		for i := len(e.ItemIDs) - 1; i >= 0; i-- {
			if it, ok := s.journal.Item(e.ItemIDs[i]); ok && s.itemActive(it) {
				out = append(out, it)
			}
		}
	}
	return out
}

// resolveRef finds the item a chat correction means, UNIT-AWARE (v4.1):
//   - "last" with grams: the newest active item of today's last entries that
//     has a portion in grams (not a drink); with ml: the newest that has a
//     volume; share only: the newest active item of the newest log entry.
//   - an item_id, else an item name among recent active items (exact
//     normalized match, then containment), newest first; a named item whose
//     unit does not fit falls back to the unit-aware "last".
func (s *Service) resolveRef(ref, unit string) (Item, bool) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, "last") {
		return s.resolveLast(unit)
	}
	if it, ok := s.journal.Item(ref); ok && s.itemActive(it) {
		// An item_id is how the model points at an item it was shown; for a
		// share-only ("only half") it must still be in the newest entry,
		// otherwise "only half" could reach past a newer, empty entry.
		if unit == unitShare {
			if e, ok := s.newestLogEntry(); !ok || e.ID != it.EntryID {
				return s.resolveLast(unit)
			}
		}
		if fitsUnit(it, unit) {
			return it, true
		}
		return s.resolveLast(unit)
	}
	entries := s.journal.Entries()
	sort.Slice(entries, func(a, b int) bool { return entries[a].CreatedAt.After(entries[b].CreatedAt) })
	var cands []Item
	for _, e := range entries {
		if e.Intent != "log" || len(e.ItemIDs) == 0 {
			continue
		}
		for i := len(e.ItemIDs) - 1; i >= 0; i-- {
			it, ok := s.journal.Item(e.ItemIDs[i])
			if ok && s.itemActive(it) {
				cands = append(cands, it)
			}
		}
		if len(cands) >= 50 {
			break
		}
	}
	n := normName(ref)
	match := func(exact bool) (Item, bool) {
		for _, it := range cands {
			in := normName(it.Name)
			if (exact && in == n) || (!exact && (strings.Contains(in, n) || strings.Contains(n, in))) {
				return it, true
			}
		}
		return Item{}, false
	}
	it, ok := match(true)
	if !ok {
		it, ok = match(false)
	}
	if !ok {
		return Item{}, false
	}
	if !fitsUnit(it, unit) {
		return s.resolveLast(unit)
	}
	return it, true
}

// refKnown reports whether a ref names an existing active item at all
// (ignoring units).
func (s *Service) refKnown(ref string) bool {
	_, ok := s.resolveRef(ref, unitShareAnyName)
	return ok
}

// unitShareAnyName resolves names and ids without a unit check.
const unitShareAnyName = "any"

func (s *Service) resolveLast(unit string) (Item, bool) {
	if unit == unitShare {
		// The newest active item of the newest log entry (an empty newest
		// entry, such as a no-food photo, resolves to nothing).
		e, ok := s.newestLogEntry()
		if !ok {
			return Item{}, false
		}
		for i := len(e.ItemIDs) - 1; i >= 0; i-- {
			if it, ok := s.journal.Item(e.ItemIDs[i]); ok && s.itemActive(it) {
				return it, true
			}
		}
		return Item{}, false
	}
	for _, it := range s.todaysActiveItems() {
		if fitsUnit(it, unit) {
			return it, true
		}
	}
	return Item{}, false
}

// whichItemText asks back when no item fits the correction (no write).
func whichItemText(unit string) string {
	switch unit {
	case unitGrams:
		return "Which item do you mean? I found no food in grams today; name it, for example \"the rice was 100 g\"."
	case unitML:
		return "Which drink do you mean? I found no drink with a volume today; name it, for example \"the water was 300 ml\"."
	}
	return "Which item do you mean? Name it, for example \"only half of the pasta\"."
}

// chatFix is one resolved correction of a chat message.
type chatFix struct {
	it   Item
	tgt  FixTarget
	op   *Op
	note string // why nothing was written for it
}

// planChatCorrections resolves the model's corrections and computes the
// rows under the item locks (taken in id order, released by the caller via
// unlock). Items that are pending or undone are skipped with a note.
func (s *Service) planChatCorrections(ctx context.Context, cs []ModelCorrection) ([]chatFix, func(), *apiError) {
	// Resolve against FRESH rows: the newest log entry's day (and today),
	// so an item undone elsewhere is not chosen.
	if e, ok := s.newestLogEntry(); ok {
		if !s.freshen(ctx, e.Date) {
			return nil, func() {}, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry")
		}
	}
	if !s.freshen(ctx, s.today()) {
		return nil, func() {}, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry")
	}
	var fixes []chatFix
	seen := map[string]bool{}
	var notes []string
	for _, c := range cs {
		unit := correctionUnit(c)
		it, ok := s.resolveRef(c.Ref, unit)
		if !ok {
			if strings.EqualFold(strings.TrimSpace(c.Ref), "last") || s.refKnown(c.Ref) {
				notes = append(notes, whichItemText(unit)) // no item fits the unit
			} else {
				notes = append(notes, "I could not find "+c.Ref+" to correct.")
			}
			continue
		}
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		fixes = append(fixes, chatFix{it: it, tgt: FixTarget{Share: c.Share, PortionG: c.PortionG, VolumeML: c.VolumeML}})
	}
	sort.Slice(fixes, func(a, b int) bool { return fixes[a].it.ID < fixes[b].it.ID })
	var locked []*itemMutex
	unlock := func() {
		for _, l := range locked {
			l.Unlock()
		}
		locked = nil
	}
	for i := range fixes {
		l := s.itemLock(fixes[i].it.ID)
		if !l.LockCtx(ctx) {
			unlock()
			return nil, func() {}, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry")
		}
		locked = append(locked, l)
	}
	days := map[string]bool{}
	for i := range fixes {
		f := &fixes[i]
		v := s.viewItem(f.it)
		switch {
		case v.pending || !v.origDone:
			f.note = f.it.Name + " is still being saved; try again in a moment."
			continue
		case v.failed:
			f.note = f.it.Name + " could not be saved, so there is nothing to correct."
			continue
		case v.state.Undone:
			f.note = f.it.Name + " was already removed."
			continue
		}
		if !days[f.it.Date] {
			if err := s.cache.RefreshDay(ctx, f.it.Date); err != nil {
				unlock()
				if ctx.Err() != nil {
					return nil, func() {}, errf(http.StatusGatewayTimeout, "timeout", true, "took too long; nothing was written, retry")
				}
				return nil, func() {}, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the current rows; nothing was written, retry")
			}
			days[f.it.Date] = true
		}
		rows, _, _ := s.cache.Rows(f.it.Date)
		op, ae := s.absoluteCorrection(f.it, rows, f.tgt, "fix", nil)
		if ae != nil {
			f.note = f.it.Name + ": " + ae.msg + "."
			continue
		}
		f.op = op
	}
	for _, n := range notes {
		fixes = append(fixes, chatFix{note: n})
	}
	return fixes, unlock, nil
}

// fixSummary is the code-generated text of a chat correction reply.
func fixSummary(fixes []chatFix, shownDate string) string {
	var parts []string
	for _, l := range fixLines(fixes, shownDate) {
		parts = append(parts, l.Text)
	}
	return strings.Join(parts, " ")
}

// fixLines are the reply lines of a chat correction, one per correction.
func fixLines(fixes []chatFix, shownDate string) []FixLine {
	var out []FixLine
	for _, f := range fixes {
		if f.note != "" {
			out = append(out, FixLine{ItemID: f.it.ID, Text: f.note})
			continue
		}
		d := describeFix(f.it, f.op)
		if f.it.Date != shownDate {
			d = strings.TrimSuffix(d, ".") + " (logged on " + f.it.Date + ")."
		}
		l := FixLine{ItemID: f.it.ID, Text: d}
		if f.op != nil {
			l.OpID = f.op.ID
		}
		out = append(out, l)
	}
	return out
}

// correctSummary renders each line by its op's CURRENT state.
func (s *Service) correctSummary(e Entry) string {
	if len(e.FixLines) == 0 {
		return e.FixText
	}
	var parts []string
	for _, l := range e.FixLines {
		text := l.Text
		if l.OpID != "" {
			if op, ok := s.journal.Op(l.OpID); ok {
				it, _ := s.journal.Item(op.ItemID)
				switch {
				case op.State == OpFailed && op.Reason == "undo":
					text = "Could not remove " + it.Name + "; it is still logged."
				case op.State == OpFailed:
					text = "Could not save the correction of " + it.Name + "; nothing changed for it."
				case !terminal(op.State):
					text = strings.TrimSuffix(text, ".") + " (still saving)."
				}
			}
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

// finishCorrect journals and writes a chat correction (intent "correct")
// and answers in the POST /fuel/log shape: the code-generated summary with
// the status line, macros_today with what changed, the corrected items.
func (s *Service) finishCorrect(ctx context.Context, w http.ResponseWriter, out *ModelOutput, clientID, hash, userText, date string, now time.Time, t0 time.Time, lat map[string]int) {
	fixes, unlock, ae := s.planChatCorrections(ctx, out.Corrections)
	if ae != nil {
		writeErr(w, ae)
		return
	}
	entry := Entry{ID: newID("en_"), ClientID: clientID, Date: date, EatenAt: now, CreatedAt: now,
		Intent: "correct", PhotoIDs: []string{}, ReqHash: hash, UserText: userText}
	var ops []Op
	for _, f := range fixes {
		if f.it.ID == "" {
			continue
		}
		if len(entry.ItemIDs) == 0 {
			entry.Date = f.it.Date // the reply shows the corrected day
		}
		entry.ItemIDs = append(entry.ItemIDs, f.it.ID)
		if f.op != nil {
			op := *f.op
			op.Attempts, op.LastTry = 1, now
			ops = append(ops, op)
			entry.FixOps = append(entry.FixOps, op.ID)
		}
	}
	entry.FixText = fixSummary(fixes, entry.Date)
	entry.FixLines = fixLines(fixes, entry.Date)
	tWrite := time.Now()
	s.stateMu.Lock()
	err := s.journal.AppendCtx(ctx, journalRec{T: "txn", Entry: &entry, Ops: ops})
	s.stateMu.Unlock()
	unlock()
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
	lat["asr"], lat["total"] = lat["asr"], ms(time.Since(t0))
	resp.LatencyMs = lat
	if ready {
		s.appendEntryFeed(entry, resp.Blocks)
	}
	code := http.StatusOK
	switch status {
	case StatusPending:
		code = http.StatusAccepted
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the correction"))
		return
	}
	b, _ := jsonMarshal(resp)
	if status == StatusDone && ready {
		_ = s.idem.Put(idemRec{ClientID: clientID, Hash: hash, Kind: "log", At: now, EntryID: entry.ID, ItemIDs: entry.ItemIDs, Status: code, Response: b})
	}
	s.logLine("correct", entry.ID, len(ops), lat, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

// localHHMM renders an instant as HH:MM in the targets tz.
func (s *Service) localHHMM(t time.Time) string {
	loc := time.UTC
	if tg, err := s.loadTargets(); err == nil && tg != nil {
		loc = tg.loc
	}
	return t.In(loc).Format("15:04")
}
