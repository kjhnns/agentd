package fuel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The exact messages of the production failure of 2026-10-01 (spec 17).
const (
	msgBones   = "I measured 380g for one half of the chicken can you adjust. But incl bones etc"
	msgMeat    = "I ended up eating 265g of chicken (pure meat and skin)"
	msgBite    = "I ate one more bite of chicken"
	msgSalad   = "Plus salad with honey mustard sauce and beet roots and feta cheese and walnuts"
	chickenBad = `{"item":"whole roasted chicken (with skin and bones, on plate)","kind":"food","staple_key":null,"portion_g":1200,"portion_basis":"photo_estimate","kcal":1560,"protein_g":180,"carbs_g":0,"net_carbs_g":0,"fat_g":60,"sat_fat_g":18,"fiber_g":0,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"scale_g":%s,"food_class":"meat_fish"}`
)

func corr(body string) string {
	return `{"intent":"correct","items":[],"corrections":[` + body + `],"text":"","widgets":[]}`
}

func corrForm(ref, form string) string {
	c := map[string]any{"ref": ref, "portion_g": nil, "volume_ml": nil, "share": nil, "portion_g_delta": nil, "volume_ml_delta": nil, "count_delta": nil, "revised": nil}
	var kv map[string]any
	_ = json.Unmarshal([]byte("{"+form+"}"), &kv)
	for k, v := range kv {
		c[k] = v
	}
	b, _ := json.Marshal(c)
	return string(b)
}

const revisedChicken = `"revised":{"item":"roast chicken, meat and skin","portion_g":265,"kcal":633,"protein_g":72,"carbs_g":0,"net_carbs_g":0,"fat_g":37,"sat_fat_g":10,"fiber_g":0,"food_class":"meat_fish"}`

// script makes the fake model answer in order and records every input.
type script struct {
	mu  sync.Mutex
	ins []ModelInput
	out []string
}

func (s *script) fn(in ModelInput) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ins = append(s.ins, in)
	if len(s.out) == 0 {
		return `{"intent":"question","items":[],"text":"","widgets":[]}`
	}
	o := s.out[0]
	if len(s.out) > 1 {
		s.out = s.out[1:]
	}
	return o
}

func (h *harness) play(outs ...string) *script {
	s := &script{out: outs}
	h.model.mu.Lock()
	h.model.fn = s.fn
	h.model.mu.Unlock()
	return s
}

func (h *harness) tell(cid, text string) LogResponse {
	h.t.Helper()
	h.clk.Add(time.Minute)
	r := h.logText(cid, text)
	if r.Code != 200 {
		h.t.Fatalf("%q: %d %s", text, r.Code, r.Body)
	}
	return decode[LogResponse](h.t, r)
}

func (h *harness) dayOf() []DayItem {
	h.t.Helper()
	return decode[struct {
		Items []DayItem `json:"items"`
	}](h.t, h.do("GET", "/fuel/day", nil, "")).Items
}

func texts(r LogResponse) string {
	var out []string
	for _, b := range r.Blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, " | ")
}

// The whole sequence of the evening, with a model that now answers in the
// v6 forms: the result is what was really eaten.
func TestTheChickenEvening(t *testing.T) {
	h := newRecalHarness(t)
	sc := h.play(photoMeal(fmt.Sprintf(chickenBad, "382")))
	chicken := h.logPhoto("c6000001-0001", photoMeal(fmt.Sprintf(chickenBad, "382")), 1)
	// (1) The scale reads 382 g; the model guessed 1200 g: more than 2 x off,
	// so the scale value is used, the macros scale with it, and the reply
	// says so.
	it := chicken.Items[0]
	if *it.PortionG != 382 || it.PortionBasis != "scale" || it.Macros.Kcal.float() != 496.6 || it.Macros.Protein.float() != 57.3 {
		t.Fatalf("scale not used: %+v", it)
	}
	if !strings.Contains(texts(chicken), "Used the scale reading for whole roasted chicken (with skin and bones, on plate): 382 g, not the estimate of 1200 g.") {
		t.Fatalf("reply does not say so: %s", texts(chicken))
	}
	id := it.ItemID

	// (2) "380 g incl bones": a re-estimate of the edible part.
	sc = h.play(corr(corrForm(id, `"revised":{"item":"half roast chicken, meat and skin (380 g with bones)","portion_g":250,"kcal":598,"protein_g":68,"carbs_g":0,"net_carbs_g":0,"fat_g":35,"sat_fat_g":9.5,"fiber_g":0}`)))
	r2 := h.tell("c6000001-0002", msgBones)
	if r2.Intent != "correct" || r2.Items[0].Effective.Kcal.float() != 598 || r2.Items[0].Item != "half roast chicken, meat and skin (380 g with bones)" {
		t.Fatalf("after bones: %+v | %s", r2.Items[0], texts(r2))
	}
	// The correction turn saw the conversation and, in its second pass, the
	// stored photo of the entry it refers to.
	if len(sc.ins) != 2 || len(sc.ins[0].RefImages) != 0 || len(sc.ins[1].RefImages) != 1 {
		t.Fatalf("model calls %d, ref images %v", len(sc.ins), len(sc.ins[len(sc.ins)-1].RefImages))
	}
	hist := sc.ins[0].History
	if len(hist) < 2 || hist[0].Role != "user" || !strings.Contains(hist[0].Text, "[1 photo(s)]") || hist[1].Role != "fuel" || !strings.Contains(hist[1].Text, "Used the scale reading") {
		t.Fatalf("history %+v", hist)
	}

	// (3) "265 g pure meat and skin": NOT 265 g at the old per-gram values.
	h.play(corr(corrForm(id, revisedChicken)))
	r3 := h.tell("c6000001-0003", msgMeat)
	c := r3.Items[0]
	if c.Effective.Kcal.float() != 633 || c.Effective.Protein.float() != 72 || *c.PortionG != 265 || c.Item != "roast chicken, meat and skin" {
		t.Fatalf("after pure meat: %+v", c)
	}
	if !strings.Contains(texts(r3), "Re-estimated half roast chicken, meat and skin (380 g with bones) as roast chicken, meat and skin, 265 g: 633 kcal, 72 g protein.") {
		t.Fatalf("reply %s", texts(r3))
	}
	rows := 0
	for _, r := range h.vars.rows("var-food") {
		if r["reason"] == "revise" {
			rows++
			meta, _ := r["recalibrated"].(map[string]any)
			if r["source"] != "fuel" || r["corrects"] != id || meta["by"] != "user" || r["share_after"] != 1.0 {
				t.Fatalf("revise row %v", r)
			}
		}
	}
	if rows != 2 {
		t.Fatalf("revise rows %d", rows)
	}

	// (4) "one more bite": the model first answers with an absolute 120 g (a
	// reduction, the production bug). The guard asks again; the second
	// answer adds 15 g.
	sc = h.play(corr(corrForm(id, `"portion_g":120`)), corr(corrForm(id, `"portion_g":120`)), corr(corrForm(id, `"portion_g_delta":15`)))
	r4 := h.tell("c6000001-0004", msgBite)
	c = r4.Items[0]
	if *c.PortionG != 265 || c.Effective.Kcal.float() != 668.8 || c.Effective.Protein.float() != 76.1 {
		t.Fatalf("after one more bite: %+v (%s)", c, texts(r4))
	}
	if !strings.Contains(texts(r4), "Added 15 g to roast chicken, meat and skin: now 280 g.") {
		t.Fatalf("reply %s", texts(r4))
	}
	if n := len(sc.ins); n != 3 || !strings.Contains(sc.ins[2].Hint, "ADDITIVE") {
		t.Fatalf("calls %d", n)
	}
	d := h.dayOf()
	if len(d) != 1 || *d[0].PortionG != 280 || d[0].Item != "roast chicken, meat and skin" || d[0].Macros.Kcal.float() != 668.8 {
		t.Fatalf("day %+v", d)
	}
	// The day's totals are the truth, not 344 kcal / 40 g.
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if kcalOf(snap) != 668.8 || macro(snap, "protein_g").Consumed != 76.1 {
		t.Fatalf("snapshot %v kcal %v protein", kcalOf(snap), macro(snap, "protein_g").Consumed)
	}
	// LAST LOGGED ITEMS now tell the model the item's real state.
	sc = h.play()
	h.tell("c6000001-0005", "how is my protein?")
	li := sc.ins[0].LastItems[0]
	if li.Item != "roast chicken, meat and skin" || *li.PortionG != 280 || *li.Kcal != 668.8 || li.Photos != 1 || len(li.Changes) != 3 ||
		!strings.Contains(li.Logged, "whole roasted chicken") || !strings.Contains(li.Changes[2], "fix to 280 g") {
		t.Fatalf("last item %+v", li)
	}
}

func TestAdditiveMessageNeverReduces(t *testing.T) {
	h := newHarness(t)
	h.play(oneItem("Chicken", 265, 633, 72, false))
	id := h.tell("c6000002-0001", "265 g chicken").Items[0].ItemID
	// The model insists on the reduction: nothing is written, the user is asked.
	sc := h.play(corr(corrForm(id, `"portion_g":120`)))
	rows := len(h.vars.rows("var-food"))
	r := h.tell("c6000002-0002", msgBite)
	if len(h.vars.rows("var-food")) != rows {
		t.Fatal("a reduction was written for an additive message")
	}
	if !strings.Contains(texts(r), "That sounds like you had MORE") || !strings.Contains(texts(r), "Nothing was changed.") || r.Intent != "correct" {
		t.Fatalf("reply %s", texts(r))
	}
	if len(sc.ins) != 2 {
		t.Fatalf("model calls %d (one re-ask)", len(sc.ins))
	}
	// The re-ask may also come back as a log of the added amount.
	h.play(corr(corrForm(id, `"share":0.5`)), oneItem("Chicken bite", 15, 36, 4, false))
	r = h.tell("c6000002-0003", "I had another piece of chicken")
	if r.Intent != "log" || len(r.Items) != 1 || r.Items[0].Item != "Chicken bite" {
		t.Fatalf("log after the re-ask: %+v", r)
	}
	// "a second one" by count.
	h.play(corr(corrForm(id, `"count_delta":1`)))
	r = h.tell("c6000002-0004", "I had a second one")
	if r.Items[0].Effective.Kcal.float() != 1266 || !strings.Contains(texts(r), "Added 1 x Chicken: now 2 x what was logged.") {
		t.Fatalf("count: %+v %s", r.Items[0], texts(r))
	}
	// A comparison ("more like") is not additive: a reduction goes through
	// with one model call.
	sc = h.play(corr(corrForm(id, `"portion_g":200`)))
	r = h.tell("c6000002-0005", "no, it was more like 200 g")
	if len(sc.ins) != 1 || *h.dayItem(id).PortionG != 200 {
		t.Fatalf("calls %d portion %v", len(sc.ins), *h.dayItem(id).PortionG)
	}
	// A negative delta without additive words is fine too.
	h.play(corr(corrForm(id, `"portion_g_delta":-50`)))
	r = h.tell("c6000002-0006", "I left about 50 g on the plate")
	if *h.dayItem(id).PortionG != 150 || !strings.Contains(texts(r), "Took 50 g off Chicken: now 150 g.") {
		t.Fatalf("negative delta: %v %s", *h.dayItem(id).PortionG, texts(r))
	}
}

func (h *harness) dayItem(id string) DayItem {
	for _, d := range h.dayOf() {
		if d.RowKey == id {
			return d
		}
	}
	h.t.Fatalf("no day item %s", id)
	return DayItem{}
}

func TestAdditiveWording(t *testing.T) {
	for s, want := range map[string]bool{
		msgBite:                               true,
		"another slice of pizza":              true,
		"I had a second one":                  true,
		"plus 50 g more":                      true,
		"50 g more rice":                      true,
		"had seconds":                         true,
		"two more glasses":                    true,
		"then had an extra spoon":             true,
		"no, it was more like 100 g":          false,
		"it was no more than half":            false,
		"that was 100 g, not 150 g":           false,
		"only half":                           false,
		"the second coffee was decaf":         false,
		msgMeat:                               false,
		msgBones:                              false,
		"more than I thought, 300 g in total": false,
		"no more slices; I only ate half":     false,
		"I did not have any more wine":        false,
		"five more grapes":                    true,
		"a couple more bites":                 true,
	} {
		if got := isAdditive(s); got != want {
			t.Errorf("%q: additive=%v", s, got)
		}
	}
}

func TestDeltaAndRevisedValidation(t *testing.T) {
	for _, bad := range []string{
		corr(corrForm("last", `"portion_g":100,"portion_g_delta":10`)),
		corr(corrForm("last", `"portion_g_delta":0`)),
		corr(corrForm("last", `"count_delta":11`)),
		corr(corrForm("last", ``)),
		corr(corrForm("last", `"revised":{"item":"","portion_g":1,"kcal":1,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null}`)),
		corr(corrForm("last", `"revised":{"item":"x","portion_g":1,"kcal":9000,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null}`)),
		corr(corrForm("last", `"revised":{"item":"x","portion_g":1,"kcal":100,"protein_g":null,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null}`)),
		corr(corrForm("last", `"share":0.5,`+revisedChicken)),
	} {
		if _, err := validateOutput(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := validateOutput(json.RawMessage(corr(corrForm("last", revisedChicken)))); err != nil {
		t.Fatal(err)
	}
}

func TestReviseBaseRules(t *testing.T) {
	h := newHarness(t)
	h.play(fmt.Sprintf(`{"intent":"log","items":[%s],"text":"","widgets":[]}`, fmt.Sprintf(chickenBad, "null")))
	id := h.tell("c6000003-0001", "a roast chicken").Items[0].ItemID
	h.play(corr(corrForm(id, revisedChicken)))
	h.tell("c6000003-0002", msgMeat)
	// A later size correction scales the REVISED values.
	h.clk.Add(time.Minute)
	fx := decode[MutationResponse](t, h.fix("c6000003-0003", id, map[string]any{"portion_g": 132.5}))
	if fx.Item.Effective.Kcal.float() != 316.5 || fx.Item.Macros.Kcal.float() != 633 || fx.Item.Item != "roast chicken, meat and skin" {
		t.Fatalf("fix after revise %+v", fx.Item)
	}
	// Recent and relog use the revised item.
	rec := h.recent("")
	if len(rec.Items) != 1 || rec.Items[0].Item != "roast chicken, meat and skin" || *rec.Items[0].PortionG != 132.5 || rec.Items[0].Macros.Kcal.float() != 316.5 {
		t.Fatalf("recent %+v", rec.Items)
	}
	// A second re-estimate replaces the first as the base (the newest wins).
	h.play(corr(corrForm(id, `"revised":{"item":"chicken breast, no skin","portion_g":200,"kcal":330,"protein_g":62,"carbs_g":0,"net_carbs_g":0,"fat_g":7,"sat_fat_g":2,"fiber_g":0}`)))
	r := h.tell("c6000003-0004", "actually it was 200 g of breast without skin")
	if r.Items[0].Effective.Kcal.float() != 330 || r.Items[0].Item != "chicken breast, no skin" || *r.Items[0].PortionG != 200 {
		t.Fatalf("second revise %+v", r.Items[0])
	}
	h.clk.Add(time.Minute)
	fx = decode[MutationResponse](t, h.fix("c6000003-0005", id, map[string]any{"share": 0.5}))
	if fx.Item.Effective.Kcal.float() != 165 {
		t.Fatalf("share of the second base %v", fx.Item.Effective.Kcal.float())
	}
	// The same re-estimate again writes nothing.
	n := len(h.vars.rows("var-food"))
	h.clk.Add(time.Minute)
	h.fix("c6000003-0006", id, map[string]any{"share": 1})
	h.play(corr(corrForm(id, `"revised":{"item":"chicken breast, no skin","portion_g":200,"kcal":330,"protein_g":62,"carbs_g":0,"net_carbs_g":0,"fat_g":7,"sat_fat_g":2,"fiber_g":0}`)))
	r = h.tell("c6000003-0007", "it was 200 g of breast without skin")
	if len(h.vars.rows("var-food")) != n+1 || !strings.Contains(texts(r), "already counted like that") {
		t.Fatalf("rows %d -> %d: %s", n, len(h.vars.rows("var-food")), texts(r))
	}
	// Undo ends at zero; survives a restart.
	h.clk.Add(time.Minute)
	un := decode[MutationResponse](t, h.mutate("undo", "c6000003-0008", id, nil))
	if kcalOf(un.Snapshot) != 0 {
		t.Fatalf("after undo %v", kcalOf(un.Snapshot))
	}
	h.restart()
	if len(h.dayOf()) != 0 {
		t.Fatal("undone item is back after a restart")
	}
}

func TestReviseAfterRecalibrateBlocksRevert(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("c6000004-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	id := resp.Items[0].ItemID
	h.recalHarnessSay(id)
	h.play(corr(corrForm(id, `"revised":{"item":"pasta with pesto","portion_g":250,"kcal":520,"protein_g":14,"carbs_g":60,"net_carbs_g":56,"fat_g":24,"sat_fat_g":4,"fiber_g":4}`)))
	r := h.tell("c6000004-0002", "that was pasta with pesto, 250 g cooked")
	if r.Items[0].Effective.Kcal.float() != 520 || r.Items[0].Recalibration.CanRevert {
		t.Fatalf("%+v %+v", r.Items[0], r.Items[0].Recalibration)
	}
	if rr := h.revert("c6000004-0003", id); rr.Code != 409 || errCode(t, rr) != "changed_after" {
		t.Fatalf("revert after a re-estimate: %d %s", rr.Code, rr.Body)
	}
}

func (h *recalHarness) recalHarnessSay(id string) {
	h.say2(answerOf(opinion(id, "pasta", 300, 480, 16, 2.4, "high", "r")))
}

func (h *recalHarness) say2(answer string) {
	h.agent.mu.Lock()
	h.agent.answer = func(string) string { return answer }
	h.agent.mu.Unlock()
	h.run()
}

func leaves(kcal, fat float64) string {
	return fmt.Sprintf(`{"item":"green salad leaves","kind":"food","staple_key":null,"portion_g":80,"portion_basis":"photo_estimate","kcal":%g,"protein_g":1,"carbs_g":2,"net_carbs_g":1,"fat_g":%g,"sat_fat_g":0,"fiber_g":1,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"scale_g":null,"food_class":"leafy_vegetable"}`, kcal, fat)
}

func TestImplausibleItemIsReaskedThenFlagged(t *testing.T) {
	h := newHarness(t)
	// "green salad leaves 80 g = 90 kcal" (production): one re-ask fixes it.
	sc := h.play(photoMeal(leaves(90, 7)), photoMeal(leaves(14, 0.2)))
	r := h.tell("c6000005-0001", msgSalad)
	if len(sc.ins) != 2 || !strings.Contains(sc.ins[1].Hint, "item 1: 90 kcal for 80 g is too high for leafy vegetable") {
		t.Fatalf("calls %d hint %q", len(sc.ins), sc.ins[len(sc.ins)-1].Hint)
	}
	if strings.Contains(sc.ins[1].Hint, "green salad") {
		t.Fatal("the hint (system message) must not carry model-made names")
	}
	if r.Items[0].Effective.Kcal.float() != 14 || r.Items[0].Check != "" || strings.Contains(texts(r), "Check this") {
		t.Fatalf("%+v %s", r.Items[0], texts(r))
	}
	// Still implausible after the re-ask: logged WITH a visible flag.
	sc = h.play(photoMeal(leaves(90, 7)))
	r = h.tell("c6000005-0002", "salad")
	if len(sc.ins) != 2 || r.Items[0].Check == "" || !strings.Contains(texts(r), "Check this: green salad leaves: 90 kcal for 80 g is too high for leafy vegetable.") {
		t.Fatalf("calls %d item %+v text %s", len(sc.ins), r.Items[0], texts(r))
	}
	found := false
	for _, row := range h.vars.rows("var-food") {
		if n, _ := row["note"].(string); strings.HasPrefix(n, "check: 90 kcal for 80 g") {
			found = true
		}
	}
	if !found {
		t.Fatal("the row carries no check note")
	}
	// The flag survives a restart and shows on GET /fuel/entry.
	h.restart()
	e := decode[entryWire](t, h.do("GET", "/fuel/entry/"+r.EntryID, nil, ""))
	if e.Items[0].Check == "" {
		t.Fatal("flag lost")
	}
}

func TestImplausibleRules(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	mk := func(class string, g, kcal, p, c, fat, sat float64) ModelItem {
		return ModelItem{Item: "x", Kind: "food", FoodClass: class, PortionG: f(g), Kcal: f(kcal), ProteinG: f(p), CarbsG: f(c), FatG: f(fat), SatFatG: f(sat)}
	}
	cases := []struct {
		it   ModelItem
		want string
	}{
		{mk("leafy_vegetable", 80, 90, 3, 6, 7, 1.5), "too high for leafy vegetable"}, // production
		{mk("leafy_vegetable", 80, 14, 1, 2, 0.2, 0), ""},
		{mk("meat_fish", 265, 633, 72, 0, 37, 10), ""},
		{mk("meat_fish", 1200, 1560, 180, 0, 60, 18), ""},
		{mk("meat_fish", 100, 1560, 180, 0, 60, 18), "macros weigh more"},
		{mk("other", 100, 950, 0, 0, 105, 10), "more than pure fat"},
		{mk("other", 100, 600, 10, 10, 10, 2), "do not match the macros"},
		{mk("nuts_seeds", 30, 196, 4.6, 4.1, 19.6, 1.8), ""},
		{mk("nuts_seeds", 100, 200, 10, 10, 13, 2), "too low for nuts seeds"},
		{mk("oil_fat", 10, 88, 0, 0, 10, 1.4), ""},
		{mk("dairy", 30, 90, 5, 1, 8, 9), "saturated fat is more than the total fat"},
		{mk("vegetable", 60, 40, 1, 8, 0, 0), ""},
		{mk("", 80, 90, 3, 6, 7, 1.5), ""}, // no class declared: not checked
		{mk("meat_fish", 100, 550, 30, 0, 47, 15), "too high for meat fish"},
		{mk("dairy", 30, 90, 5, 1, 8, 8.4), ""}, // within the 0.5 g tolerance
		{mk("dairy", 30, 90, 5, 1, 8, 8.6), "saturated fat is more than the total fat"},
	}
	for i, c := range cases {
		got := implausible(c.it)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
	// A drink with alcohol: 7 kcal per gram count.
	wine := ModelItem{Item: "wine", Kind: "drink", FoodClass: "drink", Kcal: f(125), ProteinG: f(0), CarbsG: f(4), FatG: f(0), SatFatG: f(0), AlcoholG: f(14)}
	if got := implausible(wine); got != "" {
		t.Errorf("wine: %s", got)
	}
}

func TestScaleRule(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	mk := func(portion *float64, scale float64) *ModelOutput {
		return &ModelOutput{Items: []ModelItem{{Item: "chicken", PortionG: portion, PortionBasis: "photo_estimate", Kcal: f(1000), ProteinG: f(100), CarbsG: f(0), FatG: f(60), SatFatG: f(18), ScaleG: f(scale)}}}
	}
	// Within 2 x (the edible part of a bone-in reading): the estimate stands.
	o := mk(f(250), 382)
	if n := applyScale(o); len(n) != 0 || *o.Items[0].PortionG != 250 || *o.Items[0].Kcal != 1000 {
		t.Fatalf("%v %+v", n, o.Items[0])
	}
	// Exactly 2 x is not "more than 2 x".
	o = mk(f(764), 382)
	if n := applyScale(o); len(n) != 0 {
		t.Fatalf("%v", n)
	}
	o = mk(f(1200), 382)
	if n := applyScale(o); len(n) != 1 || *o.Items[0].PortionG != 382 || round1(*o.Items[0].Kcal) != 318.3 || o.Items[0].PortionBasis != "scale" {
		t.Fatalf("%v %+v", n, o.Items[0])
	}
	// An estimate far BELOW the reading stands (edible part, an untared
	// plate, or a misread display such as "0000 382.0" read as 1382).
	o = mk(f(550), 1382)
	if n := applyScale(o); len(n) != 0 || *o.Items[0].PortionG != 550 || *o.Items[0].Kcal != 1000 {
		t.Fatalf("%v %+v", n, o.Items[0])
	}
	o = mk(nil, 382)
	if n := applyScale(o); len(n) != 1 || *o.Items[0].PortionG != 382 || *o.Items[0].Kcal != 1000 {
		t.Fatalf("%v %+v", n, o.Items[0])
	}
	if !strings.Contains(systemPrompt, "weighing scale with a readable display") || !strings.Contains(systemPrompt, "Never estimate a weight far above a scale reading") {
		t.Fatal("the prompt does not tell the model to read scales")
	}
}

func TestSecondOpinionWithScaleEvidenceIsApplied(t *testing.T) {
	h := newRecalHarness(t)
	resp := h.logPhoto("c6000006-0001", photoMeal(fmt.Sprintf(chickenBad, "null"), fmt.Sprintf(strings.Replace(chickenBad, "whole roasted chicken (with skin and bones, on plate)", "second bird", 1), "null")), 1)
	a, b := resp.Items[0].ItemID, resp.Items[1].ItemID
	op := func(id, evidence string) string {
		return strings.Replace(opinion(id, "chicken", 190, 450, 52, 7, "medium", "The scale reads 382.0 g, about 190 g edible."), `"evidence":"visual"`, `"evidence":"`+evidence+`"`, 1)
	}
	// 1560 -> 450 kcal is beyond the 3 x bound... for a visual guess only.
	h.say2(answerOf(op(a, "scale"), op(b, "visual")))
	e := h.entry(resp.EntryID)
	if e.Items[0].Recalibration.State != "applied" || e.Items[0].Effective.Kcal.float() != 450 {
		t.Fatalf("scale evidence not applied: %+v", e.Items[0].Recalibration)
	}
	if e.Items[1].Recalibration.State != "suggested" || !strings.Contains(e.Items[1].Recalibration.Summary, "Too far from the first estimate") {
		t.Fatalf("visual evidence beyond 3 x: %+v", e.Items[1].Recalibration)
	}
	for _, r := range h.corrRows("recalibrate") {
		if meta, _ := r["recalibrated"].(map[string]any); meta["evidence"] != "scale" {
			t.Fatalf("meta %v", meta)
		}
	}
	// Low confidence still blocks, also with scale evidence; an unknown
	// evidence value is an invalid answer.
	resp = h.logPhoto("c6000006-0002", photoMeal(fmt.Sprintf(chickenBad, "null")), 1)
	h.say2(answerOf(strings.Replace(strings.Replace(opinion(resp.Items[0].ItemID, "chicken", 250, 598, 68, 9.5, "low", "r"), `"evidence":"visual"`, `"evidence":"scale"`, 1), "", "", 0)))
	if s := h.entry(resp.EntryID).Items[0].Recalibration.State; s != "suggested" {
		t.Fatalf("low confidence: %s", s)
	}
	resp = h.logPhoto("c6000006-0003", photoMeal(fmt.Sprintf(chickenBad, "null")), 1)
	h.say2(answerOf(strings.Replace(opinion(resp.Items[0].ItemID, "chicken", 250, 598, 68, 9.5, "high", "r"), `"evidence":"visual"`, `"evidence":"trust me"`, 1)))
	if s := h.entry(resp.EntryID).Recalibration.State; s != "failed" {
		t.Fatalf("bad evidence: %s", s)
	}
	if !strings.Contains(recalBrief, `"evidence":"scale"|"label"|"visual"`) || !strings.Contains(recalBrief, "never \"low\"") {
		t.Fatal("brief")
	}
}

func TestChatModelAnswersNonLogIntents(t *testing.T) {
	chat := &fakeModel{}
	h := newHarness(t, func(o *Options) { o.ChatModel = chat })
	chat.fn = func(ModelInput) string { t.Error("the chat model was asked for a plain log"); return skyrWalnuts }
	h.play(skyrWalnuts)
	r := h.tell("c6000007-0001", "250 g skyr and 30 g walnuts")
	skyr := r.Items[0].ItemID
	// The fast model says "correct, 100 g"; the chat model's answer counts.
	h.play(corr(corrForm(skyr, `"portion_g":100`)))
	chat.mu.Lock()
	chat.calls = 0
	chat.fn = func(ModelInput) string { return corr(corrForm(skyr, `"portion_g":125`)) }
	chat.mu.Unlock()
	h.tell("c6000007-0002", "the skyr was only half the tub")
	if p := *h.dayItem(skyr).PortionG; p != 125 || chat.calls != 1 {
		t.Fatalf("portion %v, chat calls %d", p, chat.calls)
	}
	// The chat model fails: the fast answer stands.
	chat.mu.Lock()
	chat.fn = func(ModelInput) string { return "not json" }
	chat.mu.Unlock()
	h.play(corr(corrForm(skyr, `"portion_g":100`)))
	h.tell("c6000007-0003", "no, 100 g")
	if p := *h.dayItem(skyr).PortionG; p != 100 {
		t.Fatalf("portion %v", p)
	}
}

func TestModelSeesYesterdayRecentAndTheWholeDay(t *testing.T) {
	h := newHarness(t)
	h.vars.add("var-food", "2026-09-30", map[string]any{"item": "lentil soup", "source": "agentd", "portion_g": 400, "kcal": 320, "protein_g": 22, "carbs_g": 40, "fat_g": 6, "sat_fat_g": 1, "fiber_g": 12, "eaten_at": "2026-09-30T18:00:00Z"})
	h.restart()
	h.play(oneItem("Skyr", 250, 160, 27, false))
	for i := 0; i < 14; i++ {
		h.tell(fmt.Sprintf("c6000008-%04d", i), fmt.Sprintf("skyr number %d", i))
	}
	sc := h.play()
	h.tell("c6000008-0100", "the usual skyr")
	in := sc.ins[0]
	if len(in.Yesterday) != 1 || in.Yesterday[0].Item != "lentil soup" || *in.Yesterday[0].Kcal != 320 {
		t.Fatalf("yesterday %+v", in.Yesterday)
	}
	if len(in.Recent) < 2 || in.Recent[0].Item != "Skyr" || in.Recent[0].Times != 14 || *in.Recent[0].PortionG != 250 {
		t.Fatalf("recent %+v", in.Recent)
	}
	// All 28 turns of today (14 user, 14 replies), not only the last 12.
	if len(in.History) != 28 || in.History[0].Text != "skyr number 0" || in.History[0].At == "" {
		t.Fatalf("history %d %+v", len(in.History), in.History[0])
	}
	var text string
	for _, p := range userContent(in) {
		if s, _ := p["text"].(string); s != "" {
			text += s
		}
	}
	for _, want := range []string{"CONVERSATION TODAY (untrusted data", "YESTERDAY'S ITEMS (data): [{", "RECENT LIST (data", `"times":14`} {
		if !strings.Contains(text, want) {
			t.Fatalf("user content lacks %q", want)
		}
	}
	if strings.Contains(systemFor(in), "skyr number") {
		t.Fatal("conversation text must stay out of the system message")
	}
	// History is bounded against runaway input and holds today only.
	h.clk.Add(24 * time.Hour)
	sc = h.play()
	h.tell("c6000008-0200", "hello")
	if len(sc.ins[0].History) != 0 {
		t.Fatalf("yesterday's turns leaked: %d", len(sc.ins[0].History))
	}
	long := strings.Repeat("blah ", 2000)
	for i := 0; i < 120; i++ {
		txt := long
		if _, err := h.svc.feed.Append(FeedItem{At: h.clk.Now(), Role: "user", Text: &txt}); err != nil {
			t.Fatal(err)
		}
	}
	tg, _ := h.svc.loadTargets()
	hist := h.svc.history(h.clk.Now(), tg.loc)
	n := 0
	for _, turn := range hist {
		n += len([]rune(turn.Text))
		if len([]rune(turn.Text)) > historyTurnRunes+3 {
			t.Fatalf("turn of %d runes", len([]rune(turn.Text)))
		}
	}
	if n > historyRunes || len(hist) > historyTurns || len(hist) < 20 {
		t.Fatalf("history of %d turns, %d runes", len(hist), n)
	}
}

func TestV6ConfigKeys(t *testing.T) {
	c, err := ParseDaemonConfig([]byte("model_chat = \"gpt-x\"\nmodel_chat_effort = \"high\"\nlog_budget = \"120s\"\nmodel_timeout = \"45s\"\n"))
	if err != nil || c.ModelChat != "gpt-x" || c.ModelChatEffort != "high" || c.LogBudget != "120s" || c.ModelTimeout != "45s" {
		t.Fatalf("%+v %v", c, err)
	}
	if d := DefaultDaemonConfig(); d.LogBudget != "90s" || d.ModelTimeout != "60s" || d.ModelChat != "" {
		t.Fatalf("defaults %+v", d)
	}
	if _, err := ParseDaemonConfig([]byte("log_budget = \"fast\"\n")); err == nil {
		t.Fatal("accepted a bad duration")
	}
}

func TestImplausibleReEstimateIsReaskedThenRefused(t *testing.T) {
	h := newHarness(t)
	h.play(oneItem("Chicken", 380, 590, 59, false))
	id := h.tell("c6000009-0001", "380 g chicken with bones").Items[0].ItemID
	// 265 g with 2250 kcal (seen in the evaluation): 8.5 kcal per gram of meat.
	bad := corr(corrForm(id, `"revised":{"item":"roast chicken, meat and skin","portion_g":265,"kcal":2250,"protein_g":66,"carbs_g":0,"net_carbs_g":0,"fat_g":180,"sat_fat_g":60,"fiber_g":0,"food_class":"meat_fish"}`))
	sc := h.play(bad, corr(corrForm(id, revisedChicken)))
	r := h.tell("c6000009-0002", msgMeat)
	if len(sc.ins) != 2 || !strings.Contains(sc.ins[1].Hint, "correction 1: 2250 kcal for 265 g is too high for meat fish") || r.Items[0].Effective.Kcal.float() != 633 {
		t.Fatalf("calls %d hint %q item %+v", len(sc.ins), sc.ins[len(sc.ins)-1].Hint, r.Items[0])
	}
	// Still implausible: nothing is written, the reply says so.
	n := len(h.vars.rows("var-food"))
	h.play(bad)
	r = h.tell("c6000009-0003", "it was 265 g of pure meat")
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "I could not re-estimate roast chicken, meat and skin reliably") || !strings.Contains(texts(r), "Nothing was changed for it") {
		t.Fatalf("rows %d -> %d: %s", n, len(h.vars.rows("var-food")), texts(r))
	}
}

func TestReviseKeepsTheCurrentIntakeAsBase(t *testing.T) {
	h := newHarness(t)
	h.play(`{"intent":"log","items":[` + capItem("cappuccino", 200, 90, 80) + `],"text":"","widgets":[]}`)
	id := h.tell("c6000010-0001", "a cappuccino").Items[0].ItemID
	h.clk.Add(time.Minute)
	h.fix("c6000010-0002", id, map[string]any{"share": 0.5}) // 100 ml, 40 mg
	h.play(corr(corrForm(id, `"revised":{"item":"oat cappuccino","portion_g":null,"kcal":60,"protein_g":1,"carbs_g":8,"net_carbs_g":7,"fat_g":2.5,"sat_fat_g":0.3,"fiber_g":1,"food_class":"drink"}`)))
	h.tell("c6000010-0003", "that was with oat milk")
	d := h.dayItem(id)
	if d.Item != "oat cappuccino" || *d.VolumeML != 100 || *d.CaffeineMG != 40 || d.Macros.Kcal.float() != 60 {
		t.Fatalf("after revise: %+v", d)
	}
	rec := h.recent("")
	if *rec.Items[0].VolumeML != 100 || *rec.Items[0].CaffeineMG != 40 {
		t.Fatalf("recent %+v", rec.Items[0])
	}
	// Half of the revised drink is half of 100 ml / 40 mg, not of 200 / 80.
	h.clk.Add(time.Minute)
	fx := decode[MutationResponse](t, h.fix("c6000010-0004", id, map[string]any{"share": 0.5}))
	if fx.Item.Effective.VolumeML.float() != 50 || fx.Item.Effective.CaffeineMG.float() != 20 || fx.Item.Effective.Kcal.float() != 30 {
		t.Fatalf("share after revise %+v", fx.Item.Effective)
	}
}

func capItem(name string, ml, kcal, caffeine float64) string {
	return fmt.Sprintf(`{"item":%q,"kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":%g,"protein_g":5,"carbs_g":8,"net_carbs_g":8,"fat_g":4,"sat_fat_g":2.5,"fiber_g":0,"needs_fraction":false,"volume_ml":%g,"caffeine_mg":%g,"alcohol_g":null}`, name, kcal, ml, caffeine)
}

func TestCountDeltaUsesTheRecordedAmount(t *testing.T) {
	h := newHarness(t)
	// A zero-macro supplement: the share cannot be read off the macros.
	h.play(`{"intent":"log","items":[{"item":"creatine","kind":"supplement","staple_key":null,"portion_g":10,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":0,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`)
	id := h.tell("c6000011-0001", "10 g creatine").Items[0].ItemID
	h.clk.Add(time.Minute)
	h.fix("c6000011-0002", id, map[string]any{"portion_g": 5})
	h.play(corr(corrForm(id, `"count_delta":1`)))
	h.tell("c6000011-0003", "I took a second one")
	if p := *h.dayItem(id).PortionG; p != 15 {
		t.Fatalf("5 g + one more 10 g serving = %v g", p)
	}
}

func TestReviseAndCountResolveAmongTodaysItemsOnly(t *testing.T) {
	h := newHarness(t)
	h.play(`{"intent":"log","items":[` + capItem("cappuccino", 200, 90, 80) + `],"text":"","widgets":[]}`)
	today := h.tell("c6000012-0001", "a cappuccino").Items[0].ItemID
	// Yesterday's cappuccino is backfilled AFTER today's (a newer entry).
	h.play(`{"intent":"log","day":"yesterday","time":null,"items":[` + capItem("cappuccino", 200, 90, 80) + `],"text":"","widgets":[]}`)
	h.tell("c6000012-0002", "yesterday I had a cappuccino")
	h.play(corr(corrForm("cappuccino", `"count_delta":1`)))
	h.tell("c6000012-0003", "I had a second cappuccino")
	if v := *h.dayItem(today).VolumeML; v != 400 {
		t.Fatalf("today's cappuccino %v ml", v)
	}
	y := decode[struct {
		Items []DayItem `json:"items"`
	}](t, h.do("GET", "/fuel/day?date=2026-09-30", nil, "")).Items
	if len(y) != 1 || *y[0].VolumeML != 200 {
		t.Fatalf("yesterday's item was changed: %+v", y)
	}
}

func TestStaplesScaleAndUnknownStapleKeys(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	key := "skyr"
	o := &ModelOutput{Items: []ModelItem{{Item: "skyr", StapleKey: &key, PortionBasis: "photo_estimate", Kcal: f(100), ProteinG: f(10), CarbsG: f(4), FatG: f(0), SatFatG: f(0), ScaleG: f(100)}}}
	if n := applyScale(o); len(n) != 1 || *o.Items[0].PortionG != 100 {
		t.Fatalf("a staple on a scale: %v %+v", n, o.Items[0])
	}
	h := newHarness(t)
	// A hallucinated staple key gets no label values, so it is checked.
	bad := strings.Replace(leaves(90, 7), `"staple_key":null`, `"staple_key":"salad-mix"`, 1)
	sc := h.play(photoMeal(bad))
	r := h.tell("c6000013-0001", "salad")
	if len(sc.ins) != 2 || r.Items[0].Check == "" {
		t.Fatalf("calls %d check %q", len(sc.ins), r.Items[0].Check)
	}
}

func TestAtMostFourModelCallsPerTurn(t *testing.T) {
	chat := &fakeModel{}
	h := newRecalHarness(t, func(o *Options) { o.ChatModel = chat; o.Recal.Enabled = false })
	resp := h.logPhoto("c6000014-0001", photoMeal(photoItem("pasta", 200, 300, 10, 2, false)), 1)
	id := resp.Items[0].ItemID
	// Every stage wants one more call: fast (1), chat (2), stored photo (3),
	// implausible re-estimate (4); the additive re-ask would be the fifth.
	bad := corr(corrForm(id, `"revised":{"item":"pasta","portion_g":100,"kcal":50,"protein_g":40,"carbs_g":80,"net_carbs_g":80,"fat_g":20,"sat_fat_g":2,"fiber_g":0,"food_class":"grain_starch"}`))
	h.play(bad)
	chat.fn = func(ModelInput) string { return bad }
	n := len(h.vars.rows("var-food"))
	r := h.tell("c6000014-0002", "one more bite of pasta")
	h.model.mu.Lock()
	fast := h.model.calls
	h.model.mu.Unlock()
	chat.mu.Lock()
	total := fast - 1 + chat.calls // minus the photo log's own call
	chat.mu.Unlock()
	if total != maxModelCalls {
		t.Fatalf("model calls of the turn: %d", total)
	}
	// The answer never became acceptable: nothing is written.
	if len(h.vars.rows("var-food")) != n || r.Intent != "correct" {
		t.Fatalf("rows %d -> %d: %s", n, len(h.vars.rows("var-food")), texts(r))
	}
}

func TestAdditiveGuardHoldsUnderTheLockForAReEstimate(t *testing.T) {
	h := newHarness(t)
	h.play(oneItem("Chicken", 265, 633, 72, false))
	id := h.tell("c6000015-0001", "265 g chicken").Items[0].ItemID
	// An implausible upward re-estimate, then (after the plausibility
	// re-ask) a plausible DOWNWARD one: the additive guard must still hold.
	up := corr(corrForm(id, `"revised":{"item":"chicken","portion_g":280,"kcal":2500,"protein_g":76,"carbs_g":0,"net_carbs_g":0,"fat_g":200,"sat_fat_g":50,"fiber_g":0,"food_class":"meat_fish"}`))
	down := corr(corrForm(id, `"revised":{"item":"chicken","portion_g":120,"kcal":287,"protein_g":33,"carbs_g":0,"net_carbs_g":0,"fat_g":17,"sat_fat_g":4.5,"fiber_g":0,"food_class":"meat_fish"}`))
	h.play(up, down)
	n := len(h.vars.rows("var-food"))
	r := h.tell("c6000015-0002", msgBite)
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "That sounds like you had MORE") {
		t.Fatalf("rows %d -> %d: %s", n, len(h.vars.rows("var-food")), texts(r))
	}
}

func TestPlausibilityOfDrinksAndRevisedDrinks(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	// A drink with a stated weight obeys the universal limits.
	d := ModelItem{Item: "shake", Kind: "drink", FoodClass: "drink", PortionG: f(100), Kcal: f(950), ProteinG: f(0), CarbsG: f(0), FatG: f(105), SatFatG: f(10)}
	if got := implausible(d); !strings.Contains(got, "more than pure fat") {
		t.Fatalf("drink: %q", got)
	}
	h := newHarness(t)
	h.play(`{"intent":"log","items":[{"item":"red wine","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":125,"protein_g":0,"carbs_g":4,"net_carbs_g":4,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":150,"caffeine_mg":null,"alcohol_g":15}],"text":"","widgets":[]}`)
	id := h.tell("c6000016-0001", "a glass of red wine").Items[0].ItemID
	// 120 kcal with 3 g carbs is right for a wine that keeps its 15 g alcohol.
	sc := h.play(corr(corrForm(id, `"revised":{"item":"dry white wine","portion_g":null,"kcal":120,"protein_g":0,"carbs_g":3,"net_carbs_g":3,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"food_class":"drink"}`)))
	r := h.tell("c6000016-0002", "it was a dry white wine")
	if len(sc.ins) != 1 || r.Items[0].Effective.Kcal.float() != 120 || r.Items[0].Effective.AlcoholG.float() != 15 {
		t.Fatalf("calls %d item %+v (%s)", len(sc.ins), r.Items[0].Effective, texts(r))
	}
	// 12 kcal for a drink that keeps 15 g alcohol is refused.
	n := len(h.vars.rows("var-food"))
	h.play(corr(corrForm(id, `"revised":{"item":"wine","portion_g":null,"kcal":12,"protein_g":0,"carbs_g":3,"net_carbs_g":3,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"food_class":"drink"}`)))
	r = h.tell("c6000016-0003", "it was a very light wine")
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "I could not re-estimate wine reliably") {
		t.Fatalf("%s", texts(r))
	}
}

func TestAdditiveGuardSeesAmountOnlyReductions(t *testing.T) {
	h := newHarness(t)
	h.play(`{"intent":"log","items":[{"item":"creatine","kind":"supplement","staple_key":null,"portion_g":15,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":0,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`)
	id := h.tell("c6000017-0001", "15 g creatine").Items[0].ItemID
	rows, _, _ := h.svc.cache.Rows("2026-10-01")
	g := itemGroup(rows, id)
	it, _ := h.svc.journal.Item(id)
	ten := 10.0
	op, ae := h.svc.absoluteCorrection(it, rows, FixTarget{PortionG: &ten}, "fix", nil)
	if ae != nil || op == nil || opReduces(op) || !amountReduced(g, op) {
		t.Fatalf("a 15 g -> 10 g change of a zero-macro item must count as a reduction: %v %v", ae, op)
	}
	n := len(h.vars.rows("var-food"))
	h.play(corr(corrForm(id, `"portion_g":10`)))
	r := h.tell("c6000017-0002", "I took another scoop of creatine")
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "That sounds like you had MORE") {
		t.Fatalf("%s", texts(r))
	}
}

func TestRevisedWithoutPortionIsCheckedAgainstTheCurrentPortion(t *testing.T) {
	h := newHarness(t)
	h.play(oneItem("Chicken", 100, 165, 31, false))
	id := h.tell("c6000018-0001", "100 g chicken").Items[0].ItemID
	n := len(h.vars.rows("var-food"))
	// 600 kcal on the retained 100 g would be 6 kcal per gram of meat.
	h.play(corr(corrForm(id, `"revised":{"item":"fried chicken","portion_g":null,"kcal":600,"protein_g":60,"carbs_g":0,"net_carbs_g":0,"fat_g":40,"sat_fat_g":10,"fiber_g":0,"food_class":"meat_fish"}`)))
	r := h.tell("c6000018-0002", "it was fried chicken")
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "I could not re-estimate fried chicken reliably") {
		t.Fatalf("%s", texts(r))
	}
}

func TestRevisedIsRecheckedOnFreshRowsUnderTheLock(t *testing.T) {
	h := newHarness(t)
	h.play(oneItem("Chicken", 400, 660, 124, false))
	id := h.tell("c6000019-0001", "400 g chicken").Items[0].ItemID
	// Another writer shrinks the item to 100 g; the service has not read it.
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: Chicken", "source": "agentd", "corrects": id, "reason": "fix",
		"op_id": "ext-7", "kcal": -495, "protein_g": -93, "carbs_g": -7.5, "fat_g": -1.5, "sat_fat_g": -0.8, "fiber_g": nil, "share_after": 0.25, "portion_g_after": 100})
	n := len(h.vars.rows("var-food"))
	// 1200 kcal is plausible on the cached 400 g, not on the fresh 100 g.
	h.play(corr(corrForm(id, `"revised":{"item":"fried chicken","portion_g":null,"kcal":1200,"protein_g":110,"carbs_g":20,"net_carbs_g":20,"fat_g":75,"sat_fat_g":18,"fiber_g":0,"food_class":"meat_fish"}`)))
	r := h.tell("c6000019-0002", "it was fried chicken")
	if len(h.vars.rows("var-food")) != n || !strings.Contains(texts(r), "I could not re-estimate fried chicken reliably") {
		t.Fatalf("rows %d -> %d: %s", n, len(h.vars.rows("var-food")), texts(r))
	}
}

func TestLockedAdditiveGuardRejectsTheWholeTurn(t *testing.T) {
	h := newHarness(t)
	h.play(`{"intent":"log","items":[` + photoItem("chicken", 100, 165, 31, 1, false) + `,` + photoItem("rice", 200, 260, 5, 0.2, false) + `],"text":"","widgets":[]}`)
	r := h.tell("c6000020-0001", "chicken and rice")
	chicken, rice := r.Items[0].ItemID, r.Items[1].ItemID
	rows, _, _ := h.svc.cache.Rows("2026-10-01")
	_ = rows
	// The plan: chicken -> 120 g, rice -> 220 g. Chicken is 150 g by now.
	h.clk.Add(time.Minute)
	h.fix("c6000020-0002", chicken, map[string]any{"portion_g": 150})
	n := len(h.vars.rows("var-food"))
	fixes, unlock, ae := h.svc.planChatCorrections(context.Background(), []ModelCorrection{
		{Ref: chicken, PortionG: f64(120)}, {Ref: rice, PortionG: f64(220)}}, nil, true)
	unlock()
	if ae != nil || len(fixes) != 1 || fixes[0].op != nil || fixes[0].note != additiveAsk || fixes[0].it.ID != "" {
		t.Fatalf("the whole plan must be dropped: %+v", fixes)
	}
	// Through the route: no row, no items, only the question.
	h.play(corr(corrForm(chicken, `"portion_g_delta":20`)+","+corrForm(rice, `"portion_g":150`)), corr(corrForm(chicken, `"portion_g_delta":20`)+","+corrForm(rice, `"portion_g":150`)))
	resp := h.tell("c6000020-0003", "I had some more chicken and rice")
	_ = resp
	h.play(corr(corrForm(chicken, `"portion_g_delta":20`) + "," + corrForm(rice, `"portion_g":150`)))
	resp = h.tell("c6000020-0004", "another bite of each")
	if len(h.vars.rows("var-food")) != n || len(resp.Items) != 0 || !strings.Contains(texts(resp), "Nothing was changed.") {
		t.Fatalf("rows %d -> %d items %d: %s", n, len(h.vars.rows("var-food")), len(resp.Items), texts(resp))
	}
}
