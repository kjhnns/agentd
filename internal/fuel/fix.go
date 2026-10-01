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
	newest, ok := s.newestLogEntry()
	if !ok {
		return nil
	}
	var out []LastItem
	for _, id := range newest.ItemIDs {
		it, ok := s.journal.Item(id)
		if !ok || !s.itemActive(it) {
			continue
		}
		li := LastItem{ItemID: it.ID, Item: it.Name, Kind: it.KindOr(), PortionG: it.PortionG}
		if it.Orig.VolumeML.OK {
			v := it.Orig.VolumeML.float()
			li.VolumeML = &v
		}
		out = append(out, li)
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

// resolveRef finds the item a chat correction means: "last" = the newest
// active item of the newest log entry; an item_id; else an item name among
// the active items of recent log entries (exact normalized match first,
// then containment), newest first.
func (s *Service) resolveRef(ref string) (Item, bool) {
	ref = strings.TrimSpace(ref)
	if it, ok := s.journal.Item(ref); ok && s.itemActive(it) {
		return it, true
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
	if strings.EqualFold(ref, "last") {
		// Only the newest log entry (even an empty one): its newest active
		// item, else nothing.
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
	n := normName(ref)
	for _, it := range cands {
		if normName(it.Name) == n {
			return it, true
		}
	}
	for _, it := range cands {
		in := normName(it.Name)
		if strings.Contains(in, n) || strings.Contains(n, in) {
			return it, true
		}
	}
	return Item{}, false
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
		it, ok := s.resolveRef(c.Ref)
		if !ok {
			notes = append(notes, "I could not find "+c.Ref+" to correct.")
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
