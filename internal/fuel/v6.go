package fuel

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
)

// ---- conversation memory (spec 17 A) ----

// The whole conversation of today, bounded only against runaway input.
const (
	historyTurns     = 80
	historyTurnRunes = 600
	historyRunes     = 16000
)

func clipRunes(s string, max int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return string(r)
}

// history returns today's last feed turns (user texts, Fuel replies, coach
// lines), oldest first, bounded in count and size. It is DATA for the model.
func (s *Service) history(now time.Time, loc *time.Location) []HistoryTurn {
	return s.historyOf(now, loc, nil)
}

// historyOf is history without the feed lines skip names.
func (s *Service) historyOf(now time.Time, loc *time.Location, skip func(FeedItem) bool) []HistoryTurn {
	page, _ := s.feed.Page(0, 200)
	today := now.In(loc).Format("2006-01-02")
	var out []HistoryTurn
	for _, it := range page {
		if it.At.In(loc).Format("2006-01-02") != today {
			continue
		}
		if skip != nil && skip(it) {
			continue
		}
		text := ""
		if it.Text != nil {
			text = *it.Text
		}
		if text == "" {
			var parts []string
			for _, b := range it.Blocks {
				if b.Type == "text" && b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, " ")
		}
		if it.Role == "user" && len(it.PhotoIDs) > 0 {
			text = strings.TrimSpace(fmt.Sprintf("[%d photo(s)] %s", len(it.PhotoIDs), text))
		}
		if text = clipRunes(text, historyTurnRunes); text == "" {
			continue
		}
		out = append(out, HistoryTurn{At: it.At.In(loc).Format("15:04"), Role: it.Role, Text: text})
	}
	if len(out) > historyTurns {
		out = out[len(out)-historyTurns:]
	}
	total := 0
	for i := len(out) - 1; i >= 0; i-- {
		total += len([]rune(out[i].Text))
		if total > historyRunes {
			out = out[i+1:]
			break
		}
	}
	return out
}

// ---- scale readings (spec 17 D) ----

// applyScale enforces a readable scale: an item without a portion, or with
// an estimate of more than 2 x the scale reading, takes the scale value
// (macros scaled with it). An estimate BELOW the reading stands: it is the
// edible part, or the plate was not tared, and a misread display (a timer
// next to the weight) must never inflate a meal. It returns the notes for
// the reply.
func applyScale(out *ModelOutput) []string {
	var notes []string
	for i := range out.Items {
		it := &out.Items[i]
		if it.ScaleG == nil {
			continue
		}
		sg := round1(*it.ScaleG)
		switch {
		case it.PortionG == nil:
			it.PortionG, it.PortionBasis = &sg, "scale"
			notes = append(notes, fmt.Sprintf("Used the scale reading for %s: %s g.", it.Item, fmtNum(sg)))
		case *it.PortionG > 2*sg:
			f := sg / *it.PortionG
			old := *it.PortionG
			for _, p := range []*float64{it.Kcal, it.ProteinG, it.CarbsG, it.NetCarbsG, it.FatG, it.SatFatG, it.FiberG} {
				if p != nil {
					*p = *p * f
				}
			}
			// The known lever amounts scale by the same factor (spec 18.6).
			if it.Levers != nil {
				for _, p := range it.Levers.ptrs() {
					if *p != nil {
						v := **p * f
						*p = &v
					}
				}
			}
			it.PortionG, it.PortionBasis = &sg, "scale"
			notes = append(notes, fmt.Sprintf("Used the scale reading for %s: %s g, not the estimate of %s g.", it.Item, fmtNum(sg), fmtNum(round1(old))))
		}
	}
	return notes
}

// ---- plausibility (spec 17 E) ----

// densityBounds are kcal per gram by food class (min, max); 0 = no bound.
var densityBounds = map[string][2]float64{
	"leafy_vegetable": {0, 0.4},
	"vegetable":       {0, 1.2},
	"fruit":           {0, 1.8},
	"meat_fish":       {0.5, 5},
	"dairy":           {0.2, 5},
	"grain_starch":    {0.5, 5.5},
	"nuts_seeds":      {4, 7.5},
	"oil_fat":         {2.5, 9.2},
}

// implausible names what is wrong with an item's numbers ("" = fine). It
// runs only for items that declare a food_class (every real model answer;
// the strict schema requires it).
func implausible(it ModelItem) string {
	if it.FoodClass == "" {
		return ""
	}
	return implausibleAny(it)
}

// implausibleAny runs the checks that need no food class on EVERY item (the
// deterministic routes of section 20: a caller may leave the class out), and
// the density bounds when a class is given.
func implausibleAny(it ModelItem) string {
	v := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	kcal := v(it.Kcal)
	calc := 4*v(it.ProteinG) + 4*v(it.CarbsG) + 9*v(it.FatG) + 7*v(it.AlcoholG)
	if d := math.Abs(kcal - calc); d > 40 && d > 0.25*math.Max(kcal, calc) {
		return fmt.Sprintf("%s kcal do not match the macros (protein, carbs and fat give about %s kcal)", fmtNum(round1(kcal)), fmtNum(math.Round(calc)))
	}
	if v(it.SatFatG) > v(it.FatG)+0.5 {
		return "saturated fat is more than the total fat"
	}
	if it.PortionG == nil || *it.PortionG <= 0 {
		return ""
	}
	g := *it.PortionG
	if v(it.ProteinG)+v(it.CarbsG)+v(it.FatG) > g*1.05+1 {
		return fmt.Sprintf("the macros weigh more than the %s g portion", fmtNum(round1(g)))
	}
	d := kcal / g
	if d > 9.2 {
		return fmt.Sprintf("%s kcal for %s g is more than pure fat", fmtNum(round1(kcal)), fmtNum(round1(g)))
	}
	if b, ok := densityBounds[it.FoodClass]; ok && kcal > 15 && it.Kind != "supplement" && it.Kind != "drink" {
		what := strings.ReplaceAll(it.FoodClass, "_", " ")
		if b[1] > 0 && d > b[1] {
			return fmt.Sprintf("%s kcal for %s g is too high for %s", fmtNum(round1(kcal)), fmtNum(round1(g)), what)
		}
		if d < b[0] {
			return fmt.Sprintf("%s kcal for %s g is too low for %s", fmtNum(round1(kcal)), fmtNum(round1(g)), what)
		}
	}
	return ""
}

// problemsOf lists the implausible items. An item with a KNOWN staple key is
// exempt: the server replaces its macros by the label values.
func (s *Service) problemsOf(out *ModelOutput) map[int]string {
	return s.problemsWith(out, implausible)
}

// problemsWith is problemsOf with the given check.
func (s *Service) problemsWith(out *ModelOutput, check func(ModelItem) string) map[int]string {
	p := map[int]string{}
	for i, it := range out.Items {
		if it.StapleKey != nil && s.stapleKnown(*it.StapleKey) {
			continue
		}
		if why := check(it); why != "" {
			p[i] = why
		}
	}
	return p
}

// plausible checks a log's items; implausible ones trigger ONE re-ask. What
// is still implausible afterwards is returned (item index -> reason) and is
// logged with a visible flag, never silently. It also applies scale
// readings; the notes are returned.
func (s *Service) plausible(call modelCall, mi ModelInput, out *ModelOutput) (*ModelOutput, map[int]string, []string) {
	notes := applyScale(out)
	probs := s.problemsOf(out)
	if len(probs) == 0 {
		return out, probs, notes
	}
	var lines []string
	for i := range out.Items {
		if why, ok := probs[i]; ok {
			lines = append(lines, fmt.Sprintf("item %d: %s", i+1, why))
		}
	}
	log.Printf("fuel: %d implausible item(s) in the model answer; asking once more", len(probs))
	retry := mi
	retry.Hint = "Your previous answer had implausible values (" + strings.Join(lines, "; ") + "). Estimate again: same foods, each as its own item, with a realistic portion, kcal that match the macros and a kcal per gram that fits the food_class."
	out2, e := call(retry)
	if e != nil || out2.Intent != "log" || len(out2.Items) == 0 {
		return out, probs, notes
	}
	notes2 := applyScale(out2)
	p2 := s.problemsOf(out2)
	if len(p2) > len(probs) {
		return out, probs, notes // the second answer is worse: keep the first
	}
	return out2, p2, notes2
}

// revisedIssue checks a re-estimate against the item it replaces: the item
// keeps its kind, its alcohol and (when revised.portion_g is null) its
// current portion, so all three count ("" = plausible).
func revisedIssue(it Item, g *group, r ModelRevised) string {
	return revisedIssueWith(it, g, r, implausible)
}

// revisedIssueWith is revisedIssue with the given check.
func revisedIssueWith(it Item, g *group, r ModelRevised, check func(ModelItem) string) string {
	as := r.asItem()
	as.Kind = it.KindOr()
	if g != nil && g.orig.Data != nil {
		eff := g.c.effective()
		as.AlcoholG = eff.AlcoholG.ptr()
		if as.PortionG == nil {
			base, _, _ := groupBase(g)
			as.PortionG, _ = amountsAfter(g, eff, base)
		}
	}
	return check(as)
}

// revisedProblems lists the implausible re-estimates of a correction answer
// (correction index -> reason), against the cached rows. The write path
// checks again under the item lock on fresh rows.
func (s *Service) revisedProblems(out *ModelOutput) map[int]string {
	p := map[int]string{}
	for i, c := range out.Corrections {
		if c.Revised == nil {
			continue
		}
		why := implausible(c.Revised.asItem())
		if it, ok := s.resolveRef(c.Ref, correctionUnit(c)); ok {
			rows, _, _ := s.cache.Rows(it.Date)
			why = revisedIssue(it, itemGroup(rows, it.ID), *c.Revised)
		}
		if why != "" {
			p[i] = why
		}
	}
	return p
}

// plausibleRevised re-asks ONCE when a re-estimate is implausible. The
// caller refuses what is still implausible in the FINAL answer of the turn
// (revisedProblems): an existing item is never replaced by numbers that do
// not add up.
func (s *Service) plausibleRevised(call modelCall, mi ModelInput, out *ModelOutput) *ModelOutput {
	probs := s.revisedProblems(out)
	if len(probs) == 0 {
		return out
	}
	var lines []string
	for i := range out.Corrections {
		if why, ok := probs[i]; ok {
			lines = append(lines, fmt.Sprintf("correction %d: %s", i+1, why))
		}
	}
	log.Printf("fuel: %d implausible re-estimate(s) in the model answer; asking once more", len(probs))
	retry := mi
	retry.Hint = "Your previous answer had an implausible revised estimate (" + strings.Join(lines, "; ") + "). Answer again: revised must hold the macros of the NEW portion only, with kcal that match the macros and a kcal per gram that fits the food_class."
	out2, e := call(retry)
	if e != nil || out2.Intent != "correct" {
		return out
	}
	return out2
}

// modelCall is one model call of a turn (counted against the turn's limit).
type modelCall func(ModelInput) (*ModelOutput, *apiError)

// maxModelCalls bounds the model calls of one turn.
const maxModelCalls = 4

func (s *Service) stapleKnown(key string) bool {
	for _, st := range s.staples {
		if st.Key == key {
			return true
		}
	}
	return false
}

// ---- additive follow-ups (spec 17 C) ----

// additiveRe matches wording that ADDS to what was logged.
var additiveRe = regexp.MustCompile(`(?i)\b((one|two|three|four|five|six|seven|eight|nine|ten|a|an|some|few|couple|\d+)\s+more\b|\d+\s*(g|grams?|ml|gramm)\s+more\b|more\s+(bites?|slices?|pieces?|spoons?|spoonfuls?|glass(es)?|cups?|sips?|helpings?|servings?)\b|another\b|extra\b|additional(ly)?\b|a second (one|helping|serving|portion|slice|glass|piece)\b|second helping\b|seconds\b|plus\s+\d|(also|then) (had|ate|drank)\b|on top of that\b)`)

// notAdditiveRe removes comparisons and negations before the match: "no
// more slices", "not any more", "more like 100 g", "more than I thought".
var notAdditiveRe = regexp.MustCompile(`(?i)\b(no|not|not any|any|never|nothing|without)\s+more\b(\s+\w+)?|\bmore\s+(like|than|or less)\b`)

// isAdditive reports whether the user's words say the amount went UP.
func isAdditive(text string) bool {
	return additiveRe.MatchString(notAdditiveRe.ReplaceAllString(text, " "))
}

const additiveAsk = "That sounds like you had MORE, but I read it as a smaller amount. How much more was it, or what is the new total? Nothing was changed."

const additiveHint = `The user's words are ADDITIVE ("one more", "another", "a second one", "plus ..."): the consumed amount went UP. Your previous answer made it SMALLER, which is wrong. Answer again with a correction that uses portion_g_delta, volume_ml_delta or count_delta (positive: only the amount added), or with a log of a new item for the added amount. Never an absolute portion_g, volume_ml or share here.`

// reduces reports whether a correction would make its item smaller than it
// is now (dry run, no locks; the write path computes again under the lock).
func (s *Service) reduces(c ModelCorrection) bool {
	if (c.PortionGDelta != nil && *c.PortionGDelta < 0) || (c.VolumeMLDelta != nil && *c.VolumeMLDelta < 0) || (c.CountDelta != nil && *c.CountDelta < 0) {
		return true
	}
	if c.PortionGDelta != nil || c.VolumeMLDelta != nil || c.CountDelta != nil {
		return false
	}
	it, ok := s.resolveRef(c.Ref, correctionUnit(c))
	if !ok {
		return false
	}
	rows, _, _ := s.cache.Rows(it.Date)
	g := itemGroup(rows, it.ID)
	if g == nil || g.orig.Data == nil {
		return false
	}
	eff := g.c.effective()
	base, bp, bv := groupBase(g)
	curP, curV := amountsAfter(g, eff, base)
	switch {
	case c.Revised != nil:
		return eff.Kcal.OK && toTenth(*c.Revised.Kcal) < eff.Kcal.V
	case c.PortionG != nil:
		return curP != nil && *c.PortionG < *curP-0.05
	case c.VolumeML != nil:
		return curV != nil && *c.VolumeML < *curV-0.05
	case c.Share != nil:
		_, _ = bp, bv
		return shareOf(g, eff, base) > *c.Share+0.0005
	}
	return false
}

func (s *Service) anyReduces(out *ModelOutput) bool {
	for _, c := range out.Corrections {
		if s.reduces(c) {
			return true
		}
	}
	return false
}

// ---- the base of an item (shared by fix, delta and revise) ----

// groupBase returns what later shares scale: the original row's macros,
// portion and volume, or the revised / recalibrated values while one applies.
func groupBase(g *group) (base Macros, portion, volume *float64) {
	base = macrosFromData(g.orig.Data)
	base.normalizeNetCarbs()
	if p, ok := g.orig.Data["portion_g"].(float64); ok && p > 0 {
		portion = &p
	}
	if v, ok := g.orig.Data["volume_ml"].(float64); ok && v > 0 {
		volume = &v
	}
	if bm, bp, ok := recalBase(g); ok {
		base = withMacroBase(base, bm)
		if bp != nil {
			portion = bp
		}
	}
	// A re-estimate freezes the intake amounts the item had then: they are
	// the base of later shares too (a halved drink stays halved).
	if m, ok := baseRow(g); ok && m.Intake != nil {
		base.VolumeML, base.CaffeineMG, base.AlcoholG = tenthFromPtr(m.Intake.VolumeML), tenthFromPtr(m.Intake.CaffeineMG), tenthFromPtr(m.Intake.AlcoholG)
		volume = nil
		if v := m.Intake.VolumeML; v != nil && *v > 0 {
			volume = v
		}
	}
	return base, portion, volume
}

// shareOf is the item's current share of its base (1 = as logged): from the
// recorded amounts (current portion or volume against the base), else from
// the macros.
func shareOf(g *group, eff, base Macros) float64 {
	_, bp, bv := groupBase(g)
	curP, curV := amountsAfter(g, eff, base)
	switch {
	case bp != nil && *bp > 0 && curP != nil:
		return *curP / *bp
	case bv != nil && *bv > 0 && curV != nil:
		return *curV / *bv
	}
	for _, k := range []string{"kcal", "protein_g", "carbs_g", "fat_g", "volume_ml"} {
		b, e := base.Get(k), eff.Get(k)
		if b.OK && b.V > 0 && e.OK {
			return float64(e.V) / float64(b.V)
		}
	}
	return 1
}

// formTarget turns a correction form into the absolute target of the item
// (deltas are added to the CURRENT amount). note is set when it cannot.
func formTarget(it Item, g *group, c ModelCorrection) (FixTarget, string, string) {
	eff := g.c.effective()
	base, _, _ := groupBase(g)
	curP, curV := amountsAfter(g, eff, base)
	switch {
	case c.PortionGDelta != nil:
		if curP == nil {
			return FixTarget{}, "", it.Name + " has no logged weight; tell me the total amount instead."
		}
		t := round1(*curP + *c.PortionGDelta)
		if t <= 0 {
			return FixTarget{}, "", "That would leave nothing of " + it.Name + "; remove it instead."
		}
		return FixTarget{PortionG: &t}, deltaLead(it, *c.PortionGDelta, t, "g"), ""
	case c.VolumeMLDelta != nil:
		if curV == nil {
			return FixTarget{}, "", it.Name + " has no logged volume; tell me the total amount instead."
		}
		t := round1(*curV + *c.VolumeMLDelta)
		if t <= 0 {
			return FixTarget{}, "", "That would leave nothing of " + it.Name + "; remove it instead."
		}
		return FixTarget{VolumeML: &t}, deltaLead(it, *c.VolumeMLDelta, t, "ml"), ""
	case c.CountDelta != nil:
		t := math.Round((shareOf(g, eff, base)+*c.CountDelta)*1000) / 1000
		if t <= 0 {
			return FixTarget{}, "", "That would leave nothing of " + it.Name + "; remove it instead."
		}
		verb := "Added"
		n := *c.CountDelta
		if n < 0 {
			verb, n = "Took off", -n
		}
		return FixTarget{Share: &t}, fmt.Sprintf("%s %s x %s: now %s x what was logged.", verb, fmtNum(n), it.Name, fmtNum(t)), ""
	}
	return FixTarget{Share: c.Share, PortionG: c.PortionG, VolumeML: c.VolumeML}, "", ""
}

func deltaLead(it Item, d, total float64, unit string) string {
	if d < 0 {
		return fmt.Sprintf("Took %s %s off %s: now %s %s.", fmtNum(round1(-d)), unit, it.Name, fmtNum(total), unit)
	}
	return fmt.Sprintf("Added %s %s to %s: now %s %s.", fmtNum(round1(d)), unit, it.Name, fmtNum(total), unit)
}

// ---- re-estimate (spec 17 B) ----

// reviseOp builds the ONE row of a re-estimate: reason "revise", the deltas
// from the item's current contribution to the revised values, and the
// revised values as the item's new BASE (the recalibrate row shape and base
// rules). nil op = nothing to change.
func (s *Service) reviseOp(it Item, g *group, r ModelRevised) (*Op, string) {
	cur := g.c.effective()
	// net_carbs_g from the UNROUNDED carbs and fibre, every field rounded once.
	net := r.NetCarbsG
	if net == nil && r.CarbsG != nil {
		n := *r.CarbsG
		if r.FiberG != nil {
			n = math.Max(0, n-*r.FiberG)
		}
		net = &n
	}
	want := Macros{Kcal: tenthFromPtr(r.Kcal), Protein: tenthFromPtr(r.ProteinG), Carbs: tenthFromPtr(r.CarbsG),
		NetCarbs: tenthFromPtr(net), Fat: tenthFromPtr(r.FatG), SatFat: tenthFromPtr(r.SatFatG), Fiber: tenthFromPtr(r.FiberG)}
	after := cur
	var delta Macros
	af, df := after.fields(), delta.fields()
	changed := false
	for i, k := range AllKeys {
		cv := cur.Get(k)
		if !cv.OK {
			continue // a null stays null
		}
		*df[i] = known(0)
		if wv := want.Get(k); i < len(MacroKeys) && wv.OK {
			*af[i] = wv
			*df[i] = known(wv.V - cv.V)
			changed = changed || wv.V != cv.V
		}
	}
	base, _, _ := groupBase(g)
	curP, curV := amountsAfter(g, cur, base)
	portion := curP
	if r.PortionG != nil {
		p := round1(*r.PortionG)
		portion = &p
	}
	name := cleanText(r.Item, 120)
	if name == "" {
		name = it.Name
	}
	samePortion := (portion == nil && curP == nil) || (portion != nil && curP != nil && math.Abs(*portion-*curP) < 0.05)
	// Levers (spec 18.6): for each lever the answer knows, new minus current
	// (current = 0 when untagged), which makes the item tagged. For a lever
	// the answer gives as null: a tagged lever is retained (delta 0), an
	// untagged one stays untagged (no key).
	ls := reduceLevers(g)
	want2 := r.Levers.vals()
	kind := it.KindOr()
	var est *EstimatorCfg
	if t, _ := s.loadTargets(); t != nil && t.V2 != nil {
		est = t.V2.Levers.Estimator
	}
	newBrew := r.Levers.brew()
	want2, newBrew = checkLevers(it.ID, want2, newBrew, kind, portion, after, est)
	var levDelta, levAfter leverVals
	levChanged := false
	for l := range leverKeys {
		switch {
		case want2[l].OK:
			curL := int64(0)
			if ls.amount[l].OK {
				curL = ls.amount[l].V
			}
			levDelta[l], levAfter[l] = known(want2[l].V-curL), want2[l]
			levChanged = levChanged || !ls.amount[l].OK || want2[l].V != curL
		case ls.amount[l].OK:
			levDelta[l], levAfter[l] = known(0), ls.amount[l]
		}
	}
	brewChanged := newBrew != "" && newBrew != ls.brew
	if !changed && samePortion && name == it.Name && !levChanged && !brewChanged {
		return nil, ""
	}
	from, to := valuesOf(cur, curP), valuesOf(after, portion)
	from.Levers, to.Levers = ls.amount.asMap(), levAfter.asMap()
	if brewChanged {
		to.BrewMethod = newBrew
	}
	op := s.newCorrectionOp(it, requiredKnown(delta), "revise", nil)
	levDelta.putInto(op.Data)
	if brewChanged {
		op.Data["brew_method"] = newBrew
	}
	meta := map[string]any{"from": from, "to": to, "reason": "restated by the user", "by": "user", "item": name}
	if cur.VolumeML.OK || cur.CaffeineMG.OK || cur.AlcoholG.OK {
		meta["intake"] = recalIntake{VolumeML: cur.VolumeML.ptr(), CaffeineMG: cur.CaffeineMG.ptr(), AlcoholG: cur.AlcoholG.ptr()}
	}
	op.Data["recalibrated"] = meta
	op.Data["share_after"] = 1.0
	if portion != nil {
		op.Data["portion_g_after"] = *portion
	}
	if curV != nil {
		op.Data["volume_ml_after"] = *curV
	}
	lead := "Re-estimated " + it.Name
	if name != it.Name {
		lead += " as " + name
	}
	if portion != nil {
		lead += ", " + fmtNum(*portion) + " g"
	}
	lead += fmt.Sprintf(": %s kcal, %s g protein.", fmtNum(after.Kcal.float()), fmtNum(after.Protein.float()))
	return &op, lead
}

// shown overlays an item with its current base name and portion (a revised
// or recalibrated item is found and described by what it now is).
func (s *Service) shown(it Item) Item {
	rows, _, _ := s.cache.Rows(it.Date)
	if g := itemGroup(rows, it.ID); g != nil {
		if n := baseName(g); n != "" {
			it.Name = n
		}
		if _, bp, ok := recalBase(g); ok && bp != nil {
			it.PortionG = bp
		}
	}
	return it
}

// ---- generous context (spec 17 A) ----

// CtxItem is one item of the model's background lists (yesterday, recent).
type CtxItem struct {
	Item     string   `json:"item"`
	PortionG *float64 `json:"portion_g,omitempty"`
	VolumeML *float64 `json:"volume_ml,omitempty"`
	Kcal     *float64 `json:"kcal"`
	ProteinG *float64 `json:"protein_g"`
	At       string   `json:"at,omitempty"`    // yesterday: local HH:MM
	Times    int      `json:"times,omitempty"` // recent: how often in the window
	Last     string   `json:"last,omitempty"`  // recent: last date
}

// yesterdayItems lists yesterday's active items (all writers), oldest first.
func (s *Service) yesterdayItems(now time.Time, loc *time.Location) []CtxItem {
	y := now.In(loc).AddDate(0, 0, -1).Format("2006-01-02")
	s.stateMu.RLock()
	day := s.dayItems(y)
	s.stateMu.RUnlock()
	var out []CtxItem
	for i := len(day) - 1; i >= 0 && len(out) < 40; i-- {
		d := day[i]
		out = append(out, CtxItem{Item: clipRunes(d.Item, 80), PortionG: d.PortionG, VolumeML: d.VolumeML,
			Kcal: d.Macros.Kcal.ptr(), ProteinG: d.Macros.Protein.ptr(), At: d.EatenAt.In(loc).Format("15:04")})
	}
	return out
}

// recentItems lists the Recent list (what the user logs again and again), so
// "the usual skyr" resolves to its usual portion and values.
func (s *Service) recentItems(loc *time.Location) []CtxItem {
	var out []CtxItem
	for _, r := range s.recentAll() {
		if len(out) == 30 {
			break
		}
		out = append(out, CtxItem{Item: clipRunes(r.Item, 80), PortionG: r.PortionG, VolumeML: r.VolumeML,
			Kcal: r.Macros.Kcal.ptr(), ProteinG: r.Macros.Protein.ptr(), Times: r.Times, Last: r.LastEatenAt.In(loc).Format("2006-01-02")})
	}
	return out
}

// itemChanges describes the corrections of an item, oldest first ("19:49
// fix to 380 g"), so the model sees how an amount came to be.
func itemChanges(g *group, loc *time.Location) []string {
	var out []string
	for _, r := range g.rows {
		if _, isCorr := r.Data["corrects"]; !isCorr {
			continue
		}
		reason, _ := r.Data["reason"].(string)
		line := reason
		if p, ok := r.Data["portion_g_after"].(float64); ok {
			line += " to " + fmtNum(p) + " g"
		} else if v, ok := r.Data["volume_ml_after"].(float64); ok {
			line += " to " + fmtNum(v) + " ml"
		} else if sh, ok := r.Data["share_after"].(float64); ok {
			line += " to " + fmtNum(sh) + " x"
		}
		if !r.CreatedAt.IsZero() {
			line = r.CreatedAt.In(loc).Format("15:04") + " " + line
		}
		out = append(out, line)
		if len(out) == 8 {
			break
		}
	}
	return out
}

// refPhotos returns the stored photos (at most 3, model-sized JPEG) of the
// entries a correction refers to, so the model can judge the restated
// amount against what was photographed.
func (s *Service) refPhotos(out *ModelOutput) [][]byte {
	var imgs [][]byte
	seen := map[string]bool{}
	for _, c := range out.Corrections {
		it, ok := s.resolveRef(c.Ref, correctionUnit(c))
		if !ok {
			continue
		}
		e, ok := s.journal.Entry(it.EntryID)
		if !ok {
			continue
		}
		for _, id := range e.PhotoIDs {
			if seen[id] || len(imgs) == 3 {
				continue
			}
			seen[id] = true
			if b := s.photoForModel(id); b != nil {
				imgs = append(imgs, b)
			}
		}
	}
	return imgs
}

// describeState fills the current state of an item for the model: what it
// counts as now, how it was logged, its corrections, its photos.
func (s *Service) describeState(li *LastItem, it Item) {
	loc := time.UTC
	if tg, err := s.loadTargets(); err == nil && tg != nil {
		loc = tg.loc
	}
	if e, ok := s.journal.Entry(it.EntryID); ok {
		li.Photos = len(e.PhotoIDs)
	}
	li.Basis = it.Basis
	rows, _, _ := s.cache.Rows(it.Date)
	g := itemGroup(rows, it.ID)
	if g == nil || g.orig.Data == nil {
		return
	}
	eff := g.c.effective()
	li.Kcal, li.ProteinG = eff.Kcal.ptr(), eff.Protein.ptr()
	base, _, _ := groupBase(g)
	if p, _ := amountsAfter(g, eff, base); p != nil {
		li.PortionG = p
	}
	li.Changes = itemChanges(g, loc)
	if len(li.Changes) > 0 {
		orig := macrosFromData(g.orig.Data)
		name, _ := g.orig.Data["item"].(string)
		li.Logged = clipRunes(name, 80)
		if p, ok := g.orig.Data["portion_g"].(float64); ok {
			li.Logged += ", " + fmtNum(p) + " g"
		}
		if orig.Kcal.OK {
			li.Logged += ", " + fmtNum(orig.Kcal.float()) + " kcal"
		}
	}
}

// photoForModel loads a stored photo scaled for the model (nil if gone).
func (s *Service) photoForModel(id string) []byte {
	p := s.photos.path(id)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	if img, _, derr := image.Decode(bytes.NewReader(b)); derr == nil {
		var out bytes.Buffer
		if jpeg.Encode(&out, scaleToLongEdge(img, modelLongEdge), &jpeg.Options{Quality: 85}) == nil {
			return out.Bytes()
		}
	}
	return b
}
