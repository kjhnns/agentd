package fuel

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lv renders a model `levers` object; nil = null.
func lv(psyllium, betaGlucan, nuts, pulses, plantProtein, brew any) string {
	b, _ := json.Marshal(map[string]any{"psyllium_g": psyllium, "beta_glucan_g": betaGlucan, "nuts_g": nuts, "pulses_g": pulses, "plant_protein_g": plantProtein, "brew_method": brew})
	return string(b)
}

// lItem is a model item with levers. extra is raw JSON fields (",\"k\":v").
func lItem(name string, portion, kcal, protein, fibre float64, levers, extra string) string {
	return fmt.Sprintf(`{"item":%q,"kind":"food","staple_key":null,"portion_g":%g,"portion_basis":"stated","kcal":%g,"protein_g":%g,"carbs_g":10,"net_carbs_g":null,"fat_g":10,"sat_fat_g":1,"fiber_g":%g,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"levers":%s%s}`,
		name, portion, kcal, protein, fibre, levers, extra)
}

func logOf(items ...string) string {
	return `{"intent":"log","items":[` + strings.Join(items, ",") + `],"text":"","widgets":[]}`
}

func revisedOf(name string, portion, kcal, protein float64, levers string) string {
	return fmt.Sprintf(`"revised":{"item":%q,"portion_g":%g,"kcal":%g,"protein_g":%g,"carbs_g":10,"net_carbs_g":null,"fat_g":10,"sat_fat_g":1,"fiber_g":3,"food_class":"","levers":%s}`, name, portion, kcal, protein, levers)
}

func lever(s Snapshot, key string) LeverState {
	for _, l := range s.Levers {
		if l.Key == key {
			return l
		}
	}
	return LeverState{}
}

func (h *harness) say(text, model string) LogResponse {
	h.t.Helper()
	h.model.mu.Lock()
	h.model.fn = func(ModelInput) string { return model }
	h.model.mu.Unlock()
	h.clk.Add(time.Minute)
	recSeq++
	r := h.logText(fmt.Sprintf("73000000-%04d", recSeq), text)
	if r.Code != 200 {
		h.t.Fatalf("%q: %d %s", text, r.Code, r.Body)
	}
	return decode[LogResponse](h.t, r)
}

func (h *harness) itemNow(entryID string, i int) ItemState {
	h.t.Helper()
	return decode[entryWire](h.t, h.get("/fuel/entry/"+entryID)).Items[i]
}

// fixAt and mutateAt advance the clock first: a row merged on its 201 takes
// the service clock as its time, and the lever reducer reads rows in write
// order (a frozen test clock would put a later own row before an earlier
// row that came back from a server read).
func (h *harness) fixAt(cid, itemID string, body map[string]any) *httptest.ResponseRecorder {
	h.clk.Add(time.Second)
	return h.fix(cid, itemID, body)
}

func (h *harness) mutateAt(kind, cid, itemID string, f *float64) *httptest.ResponseRecorder {
	h.clk.Add(time.Second)
	return h.mutate(kind, cid, itemID, f)
}

// addAt stores a row of another writer with the TEST clock as its creation
// time (fakeVars.add uses the real clock, which is after every test time).
func (h *harness) addAt(varID, date string, data any) {
	b, _ := json.Marshal(data)
	h.vars.mu.Lock()
	defer h.vars.mu.Unlock()
	h.vars.n++
	h.vars.vals = append(h.vars.vals, fakeValue{ID: fmt.Sprintf("val-%d", h.vars.n), VariableID: varID, Data: string(b), RecordDate: date,
		CreatedAt: h.clk.Now().Add(time.Duration(h.vars.n) * time.Millisecond)})
}

func lastRow(h *harness) map[string]any {
	rows := h.vars.rows("var-food")
	return rows[len(rows)-1]
}

// T12 (levers, unit): the reducer and every operation.
func TestT12TaggedItemScalesWithCorrections(t *testing.T) {
	h := newV7(t, v2())
	resp := h.say("30 g walnuts", logOf(lItem("walnuts", 30, 196, 4.6, 2, lv(0, 0, 30, 0, 4.6, nil), "")))
	it := resp.Items[0]
	if num(it.Levers.Nuts) != "30" || num(it.Levers.PlantProtein) != "4.6" || num(it.Levers.Psyllium) != "0" || it.Levers.BrewMethod != nil || it.EntryID != resp.EntryID {
		t.Fatalf("item levers: %+v entry %q", it.Levers, it.EntryID)
	}
	row := lastRow(h)
	if row["nuts_g"] != 30.0 || row["plant_protein_g"] != 4.6 || row["psyllium_g"] != 0.0 {
		t.Fatalf("original row: %v", row)
	}
	if _, has := row["brew_method"]; has {
		t.Error("brew_method must not be written as null")
	}
	if l := lever(resp.Snapshot, "nuts_g"); l.Consumed != 30 || l.UntaggedRows != 0 || l.Label != "Nuts" || l.Unit != "g" {
		t.Errorf("snapshot lever: %+v", l)
	}
	// Halved: 15. The fix row carries the lever deltas.
	fx := decode[MutationResponse](t, h.fixAt(cid(), it.ItemID, map[string]any{"portion_g": 15}))
	if num(fx.Item.Levers.Nuts) != "15" || lever(fx.Snapshot, "nuts_g").Consumed != 15 {
		t.Errorf("after the fix: %s", num(fx.Item.Levers.Nuts))
	}
	if row := lastRow(h); row["nuts_g"] != -15.0 || row["plant_protein_g"] != -2.3 || row["psyllium_g"] != 0.0 {
		t.Errorf("fix row: %v", row)
	}
	// Undone: 0 (the undo row cancels the levers too).
	un := decode[MutationResponse](t, h.mutateAt("undo", cid(), it.ItemID, nil))
	if lever(un.Snapshot, "nuts_g").Consumed != 0 || lastRow(h)["nuts_g"] != -15.0 {
		t.Errorf("after undo: %g row %v", lever(un.Snapshot, "nuts_g").Consumed, lastRow(h)["nuts_g"])
	}
}

func TestT12UntaggedItemStaysUntaggedUntilARevise(t *testing.T) {
	h := newV7(t, v2())
	// A model item without levers (every amount null): untagged.
	resp := h.say("porridge", logOf(lItem("porridge", 300, 300, 10, 4, lv(nil, nil, nil, nil, nil, nil), "")))
	it := resp.Items[0]
	if it.Levers.Nuts != nil || lever(resp.Snapshot, "nuts_g").UntaggedRows != 1 {
		t.Fatalf("untagged: %+v %+v", it.Levers, lever(resp.Snapshot, "nuts_g"))
	}
	h.fixAt(cid(), it.ItemID, map[string]any{"portion_g": 150})
	row := lastRow(h)
	for _, k := range leverKeys {
		if _, has := row[k]; has {
			t.Errorf("a fix on an untagged item wrote %s", k)
		}
	}
	if st := h.itemNow(resp.EntryID, 0); st.Levers.Nuts != nil || st.Levers.BetaGlucan != nil {
		t.Errorf("still untagged: %+v", st.Levers)
	}
	// A revise makes it tagged with the revised amounts; a lever the answer
	// gives as null stays untagged.
	h.say("it was 80 g oats with 20 g walnuts", corr(corrForm(it.ItemID, revisedOf("oats with walnuts", 300, 420, 14, lv(0, 3.2, 20, 0, nil, nil)))))
	st := h.itemNow(resp.EntryID, 0)
	if num(st.Levers.Nuts) != "20" || num(st.Levers.BetaGlucan) != "3.2" || num(st.Levers.Psyllium) != "0" || st.Levers.PlantProtein != nil {
		t.Fatalf("after the revise: %+v", st.Levers)
	}
	row = lastRow(h)
	if row["reason"] != "revise" || row["nuts_g"] != 20.0 {
		t.Errorf("revise row: %v", row)
	}
	if _, has := row["plant_protein_g"]; has {
		t.Error("an untagged lever with a null answer must have no key")
	}
	meta := row["recalibrated"].(map[string]any)["to"].(map[string]any)["levers"].(map[string]any)
	if meta["nuts_g"] != 20.0 || meta["beta_glucan_g"] != 3.2 {
		t.Errorf("to.levers: %v", meta)
	}
	// A fix after a revise scales the revised amounts.
	h.fixAt(cid(), it.ItemID, map[string]any{"portion_g": 150})
	if st := h.itemNow(resp.EntryID, 0); num(st.Levers.Nuts) != "10" || num(st.Levers.BetaGlucan) != "1.6" {
		t.Errorf("fix after revise: %+v", st.Levers)
	}
	// A revert of a revise row is refused (section 16 reverts recalibrate rows only).
	b, _ := json.Marshal(map[string]string{"client_id": cid(), "item_id": it.ItemID})
	if r := h.do("POST", "/fuel/recalibration/revert", strings.NewReader(string(b)), "application/json"); r.Code != 409 || errCode(t, r) != "not_recalibrated" {
		t.Errorf("revert of a revise: %d %s", r.Code, r.Body)
	}
}

func TestT12ReviseKeepsATaggedLeverAndChangesTheBrewMethod(t *testing.T) {
	h := newV7(t, v2())
	resp := h.say("muesli", logOf(lItem("muesli", 100, 400, 10, 8, lv(0, 2, 30, 0, 9, nil), "")))
	it := resp.Items[0]
	// nuts null in the answer: the 30 g stay.
	h.say("it was 120 g", corr(corrForm(it.ItemID, revisedOf("muesli", 120, 480, 12, lv(nil, 2.4, nil, nil, nil, nil)))))
	st := h.itemNow(resp.EntryID, 0)
	if num(st.Levers.Nuts) != "30" || num(st.Levers.BetaGlucan) != "2.4" || num(st.Levers.PlantProtein) != "9" {
		t.Fatalf("retained: %+v", st.Levers)
	}
	if row := lastRow(h); row["nuts_g"] != 0.0 || row["recalibrated"].(map[string]any)["to"].(map[string]any)["levers"].(map[string]any)["nuts_g"] != 30.0 {
		t.Errorf("revise row: %v", row)
	}
	// Coffee: unknown, then a revise that only changes the brew method.
	coffee := `{"item":"coffee","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":2,"protein_g":0.2,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":200,"caffeine_mg":95,"alcohol_g":null,"levers":` + lv(0, 0, 0, 0, 0, "unknown") + `}`
	c := h.say("a coffee", logOf(coffee))
	if b := c.Items[0].Levers.BrewMethod; b == nil || *b != "unknown" || c.Snapshot.Coffee.Today.Unknown != 1 || c.Snapshot.Coffee.Last7.Unknown != 1 {
		t.Fatalf("coffee: %+v %+v", c.Items[0].Levers, c.Snapshot.Coffee)
	}
	rev := `"revised":{"item":"coffee","portion_g":null,"kcal":2,"protein_g":0.2,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"food_class":"drink","levers":` + lv(0, 0, 0, 0, 0, "unfiltered") + `}`
	r := h.say("that was French press", corr(corrForm(c.Items[0].ItemID, rev)))
	st = h.itemNow(c.EntryID, 0)
	if st.Levers.BrewMethod == nil || *st.Levers.BrewMethod != "unfiltered" || r.Snapshot.Coffee.Today.Unfiltered != 1 || r.Snapshot.Coffee.Today.Unknown != 0 {
		t.Errorf("after the revise: %+v %+v", st.Levers, r.Snapshot.Coffee)
	}
	if row := lastRow(h); row["reason"] != "revise" || row["brew_method"] != "unfiltered" {
		t.Errorf("brew revise row: %v", row)
	}
	// The default method of the file replaces "unknown" (the user's words win).
	h2 := newV7(t, v2("levers.coffee_default_brew_method", "filtered"))
	c2 := h2.say("a coffee", logOf(coffee))
	if b := c2.Items[0].Levers.BrewMethod; b == nil || *b != "filtered" || c2.Snapshot.Coffee.DefaultMethod == nil {
		t.Errorf("default method: %+v", c2.Items[0].Levers)
	}
	c3 := h2.say("an espresso", logOf(strings.Replace(coffee, `"unknown"`, `"espresso"`, 1)))
	if b := c3.Items[0].Levers.BrewMethod; b == nil || *b != "espresso" {
		t.Errorf("stated method: %+v", c3.Items[0].Levers)
	}
}

// A correction by a writer without lever arithmetic (a v6 binary, an old
// food-log) makes the item untagged, never wrong; a revise sets it again.
func TestT12KeylessCorrectionUntagsAndReviseResets(t *testing.T) {
	h := newV7(t, v2())
	resp := h.say("granola", logOf(lItem("granola", 100, 450, 10, 6, lv(0, 1, 30, 0, 8, nil), "")))
	it := resp.Items[0]
	keyless := func(op string) {
		h.clk.Add(time.Second)
		h.addAt("var-food", "2026-10-01", map[string]any{"item": "correction: granola", "source": "fuel", "corrects": it.ItemID, "item_id": "it_" + op,
			"op_id": op, "reason": "fix", "kcal": -45.0, "protein_g": -1.0, "carbs_g": -1.0, "fat_g": -1.0, "sat_fat_g": -0.1, "fiber_g": -0.6,
			"share_after": 0.9, "portion_g_after": 90.0, "eaten_at": "2026-10-01T12:30:00+02:00"})
		h.clk.Add(61 * time.Second)
	}
	keyless("op_v6_fix_1")
	st := h.itemNow(resp.EntryID, 0)
	if st.Levers.Nuts != nil || st.Levers.PlantProtein != nil || st.Effective.Kcal.float() != 405 {
		t.Fatalf("after a keyless correction: %+v kcal %g", st.Levers, st.Effective.Kcal.float())
	}
	if l := lever(h.snap(t, ""), "nuts_g"); l.Consumed != 0 || l.UntaggedRows != 1 {
		t.Errorf("day: %+v", l)
	}
	// A revise to 15 g nuts: tagged with 15 (not 45).
	h.say("only 15 g nuts in it", corr(corrForm(it.ItemID, revisedOf("granola", 90, 400, 9, lv(0, 1, 15, 0, 7, nil)))))
	if st := h.itemNow(resp.EntryID, 0); num(st.Levers.Nuts) != "15" {
		t.Fatalf("after the revise: %s", num(st.Levers.Nuts))
	}
	h.restart()
	if st := h.itemNow(resp.EntryID, 0); num(st.Levers.Nuts) != "15" {
		t.Fatalf("after a restart: %s", num(st.Levers.Nuts))
	}
	h.fixAt(cid(), it.ItemID, map[string]any{"share": 0.5})
	if st := h.itemNow(resp.EntryID, 0); num(st.Levers.Nuts) != "7.5" {
		t.Fatalf("after a fix to half: %s", num(st.Levers.Nuts))
	}
	// A relog copies 7.5 (times the scale).
	var key string
	for _, r := range h.svc.recentAll() {
		if r.Item == "granola" {
			key = r.Key
		}
	}
	h.clk.Add(time.Minute)
	rl := decode[LogResponse](t, h.relog(cid(), key, nil))
	if num(rl.Items[0].Levers.Nuts) != "7.5" || lastRow(h)["nuts_g"] != 7.5 {
		t.Errorf("relog: %s row %v", num(rl.Items[0].Levers.Nuts), lastRow(h)["nuts_g"])
	}
	half := 0.5
	h.clk.Add(time.Minute)
	rl2 := decode[LogResponse](t, h.relog(cid(), key, &half))
	if num(rl2.Items[0].Levers.Nuts) != "3.8" { // 7.5 x 0.5, rounded half away from zero
		t.Errorf("relog x 0.5: %s", num(rl2.Items[0].Levers.Nuts))
	}
	// A second keyless correction after the revise: untagged again.
	keyless("op_v6_fix_2")
	if st := h.itemNow(resp.EntryID, 0); st.Levers.Nuts != nil {
		t.Errorf("after a second keyless correction: %s", num(st.Levers.Nuts))
	}
}

func TestT12ScaleOverrideAndCodeChecks(t *testing.T) {
	h := newV7(t, v2())
	// A 400 g mixed dish with 30 g nuts, replaced by a scale reading of 100 g.
	dish := strings.Replace(lItem("nut curry", 400, 800, 20, 8, lv(0, 0, 30, 100, 12, nil), `,"scale_g":100,"food_class":"mixed_dish"`), `"portion_basis":"stated"`, `"portion_basis":"photo_estimate"`, 1)
	resp := h.say("curry on the scale", logOf(dish))
	it := resp.Items[0]
	if *it.PortionG != 100 || num(it.Levers.Nuts) != "7.5" || num(it.Levers.Pulses) != "25" || lastRow(h)["nuts_g"] != 7.5 || lever(resp.Snapshot, "nuts_g").Consumed != 7.5 {
		t.Fatalf("scale override: portion %g levers %+v row %v", *it.PortionG, it.Levers, lastRow(h)["nuts_g"])
	}
	// Each code check nulls an impossible value.
	p := 40.0
	m := Macros{Protein: known(100), Fiber: known(30)} // 10 g protein, 3 g fibre
	in := leverVals{known(500), known(50), known(450), known(430), known(160)}
	out, brew := checkLevers("it_x", in, "filtered", "food", &p, m, nil)
	for l := range out {
		if out[l].OK {
			t.Errorf("%s = %g must be dropped", leverKeys[l], out[l].float())
		}
	}
	if brew != "" {
		t.Error("a brew method on a food must be dropped")
	}
	ok := leverVals{known(400), known(35), known(420), known(420), known(105)}
	if out, brew := checkLevers("it_x", ok, "espresso", "drink", &p, m, nil); out != ok || brew != "espresso" {
		t.Errorf("values at the bounds must stay: %+v %q", out, brew)
	}
	// With the estimator a dry portion gives a larger cooked-equivalent amount.
	est := &EstimatorCfg{PulsesDryToCooked: 2.5}
	dry := 80.0
	if out, _ := checkLevers("it_x", leverVals{{}, {}, {}, known(2000), {}}, "", "food", &dry, Macros{}, est); !out[3].OK {
		t.Error("200 g cooked from 80 g dry must pass with the factor 2.5")
	}
	if out, _ := checkLevers("it_x", leverVals{{}, {}, {}, known(2000), {}}, "", "food", &dry, Macros{}, nil); out[3].OK {
		t.Error("200 g pulses in an 80 g portion must be dropped without the estimator")
	}
	// The model answer is bounded too.
	if _, err := validateOutput(json.RawMessage(withClinical(logOf(lItem("x", 30, 100, 3, 1, lv(-1, 0, 0, 0, 0, nil), ""))))); err == nil {
		t.Error("a negative lever amount was accepted")
	}
	if _, err := validateOutput(json.RawMessage(withClinical(logOf(lItem("x", 30, 100, 3, 1, lv(0, 0, 0, 0, 0, "cold brew"), ""))))); err == nil {
		t.Error("an unknown brew method was accepted")
	}
}

// Staples: lever values per 100 g replace the model's.
func TestT12StapleLevers(t *testing.T) {
	h := newV7(t, v2(), func(o *Options) {
		st := `[{"key":"walnuts","aliases":["walnuts"],"default_g":30,"per_100g":{"kcal":654,"protein_g":15.2,"carbs_g":13.7,"fat_g":65.2,"sat_fat_g":6.1,"fiber_g":6.7,"nuts_g":100,"plant_protein_g":15.2}},
		 {"key":"skyr","aliases":["skyr"],"default_g":250,"per_100g":{"kcal":63,"protein_g":11,"carbs_g":4,"fat_g":0.2,"sat_fat_g":0.1,"fiber_g":0}}]`
		if err := writeFile(o.StaplesFile, st); err != nil {
			t.Fatal(err)
		}
	})
	w := strings.Replace(lItem("walnuts", 40, 1, 1, 1, lv(0, 0, 5, 0, 1, nil), ""), `"staple_key":null`, `"staple_key":"walnuts"`, 1)
	s := strings.Replace(lItem("skyr", 200, 1, 1, 1, lv(0, 0, 0, 0, 0, nil), ""), `"staple_key":null`, `"staple_key":"skyr"`, 1)
	resp := h.say("40 g walnuts and 200 g skyr", logOf(w, s))
	if it := resp.Items[0]; num(it.Levers.Nuts) != "40" || num(it.Levers.PlantProtein) != "6.1" || num(it.Levers.Psyllium) != "0" {
		t.Errorf("staple levers replace the model's: %+v", it.Levers)
	}
	if it := resp.Items[1]; num(it.Levers.Nuts) != "0" || num(it.Levers.PlantProtein) != "0" {
		t.Errorf("a staple without lever values keeps the model's: %+v", it.Levers)
	}
}

func TestT12MoveAndSequences(t *testing.T) {
	h := newV7(t, v2())
	// (revise, fix, fraction, relog, move), amounts computed by hand.
	item := strings.Replace(lItem("nut bowl", 100, 500, 12, 6, lv(0, 0, 30, 0, 10, nil), ""), `"needs_fraction":false`, `"needs_fraction":true`, 1)
	resp := h.say("nut bowl", logOf(item))
	it := resp.Items[0]
	h.say("it was 200 g with 40 g nuts", corr(corrForm(it.ItemID, revisedOf("nut bowl", 200, 900, 20, lv(0, 0, 40, 0, 16, nil))))) // 40
	h.fixAt(cid(), it.ItemID, map[string]any{"portion_g": 100})                                                                    // 20
	q := 0.25
	if r := h.mutateAt("fraction", cid(), it.ItemID, &q); r.Code != 200 { // 40 x 0.25 = 10
		t.Fatalf("fraction: %d %s", r.Code, r.Body)
	}
	st := h.itemNow(resp.EntryID, 0)
	if num(st.Levers.Nuts) != "10" || num(st.Levers.PlantProtein) != "4" {
		t.Fatalf("after revise, fix, fraction: %+v", st.Levers)
	}
	var key string
	for _, r := range h.svc.recentAll() {
		if r.Item == "nut bowl" {
			key = r.Key
		}
	}
	half := 0.5
	h.clk.Add(time.Minute)
	rl := decode[LogResponse](t, h.relog(cid(), key, &half)) // a new item: 5
	if num(rl.Items[0].Levers.Nuts) != "5" {
		t.Fatalf("relog x 0.5: %s", num(rl.Items[0].Levers.Nuts))
	}
	// Move the relogged item (the newest entry) to yesterday: the new row
	// carries the current amounts, the source is cancelled with its levers.
	mv := h.say("that last one was for yesterday", moveOut(`"yesterday"`))
	if mv.Intent != "move" {
		t.Fatalf("move: %+v", mv)
	}
	today, yday := h.snap(t, ""), h.snap(t, "?date=2026-09-30")
	if lever(today, "nuts_g").Consumed != 10 || lever(yday, "nuts_g").Consumed != 5 || lever(yday, "plant_protein_g").Consumed != 2 || lever(today, "nuts_g").UntaggedRows != 0 {
		t.Errorf("after the move: today %g, yesterday %g", lever(today, "nuts_g").Consumed, lever(yday, "nuts_g").Consumed)
	}
	var moved map[string]any
	for _, r := range h.vars.rows("var-food") {
		if r["moved_from"] != nil {
			moved = r
		}
	}
	if moved == nil || moved["nuts_g"] != 5.0 || moved["_record_date"] != "2026-09-30" {
		t.Errorf("moved row: %v", moved)
	}
}

// The second opinion scales a tagged lever with the portion; its revert
// returns the amount.
func TestT12SecondOpinionScalesLeversAndRevertReturns(t *testing.T) {
	h := newRecalHarness(t, func(o *Options) {
		if err := writeFile(o.TargetsFile, v2()); err != nil {
			t.Fatal(err)
		}
	})
	item := `{"item":"pasta with walnuts","kind":"food","staple_key":null,"portion_g":200,"portion_basis":"photo_estimate","kcal":300,"protein_g":10,"carbs_g":40,"net_carbs_g":null,"fat_g":10,"sat_fat_g":2,"fiber_g":3,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"levers":` + lv(0, 0, 30, 0, 8, nil) + `}`
	resp := h.logPhoto("r1200000-0001", photoMeal(item), 1)
	id := resp.Items[0].ItemID
	h.say(answerOf(opinion(id, "pasta with walnuts", 300, 480, 16, 2.4, "high", "the plate is full")))
	h.run()
	st := h.entry(resp.EntryID).Items[0]
	if num(st.Levers.Nuts) != "45" || num(st.Levers.PlantProtein) != "12" {
		t.Fatalf("after the second opinion: %+v", st.Levers)
	}
	rows := h.corrRows("recalibrate")
	if len(rows) != 1 || rows[0]["nuts_g"] != 15.0 {
		t.Fatalf("recalibrate row: %v", rows)
	}
	meta := rows[0]["recalibrated"].(map[string]any)
	if meta["from"].(map[string]any)["levers"].(map[string]any)["nuts_g"] != 30.0 || meta["to"].(map[string]any)["levers"].(map[string]any)["nuts_g"] != 45.0 {
		t.Errorf("from / to levers: %v", meta)
	}
	// A fix after the second opinion scales the recalibrated base.
	// (Not done here: it would block the revert.) Revert returns 30.
	if r := h.revert("r1200000-0002", id); r.Code != 200 {
		t.Fatalf("revert: %d %s", r.Code, r.Body)
	}
	if st := h.entry(resp.EntryID).Items[0]; num(st.Levers.Nuts) != "30" || num(st.Levers.PlantProtein) != "8" {
		t.Errorf("after the revert: %+v", st.Levers)
	}
	if rows := h.corrRows("recalibrate_revert"); len(rows) != 1 || rows[0]["nuts_g"] != -15.0 {
		t.Errorf("revert row: %v", rows)
	}
	// After the revert the original is the base again.
	h.fixAt("r1200000-0003", id, map[string]any{"portion_g": 100})
	if st := h.entry(resp.EntryID).Items[0]; num(st.Levers.Nuts) != "15" {
		t.Errorf("fix after the revert: %s", num(st.Levers.Nuts))
	}
}

// T13 (lever coverage).
func TestT13LeverCoverage(t *testing.T) {
	tagged := func(nuts float64) map[string]any {
		return map[string]any{"psyllium_g": 0.0, "beta_glucan_g": 0.0, "nuts_g": nuts, "pulses_g": 0.0, "plant_protein_g": 0.0}
	}
	fix := func(h *harness, withKeys bool) {
		ex := func(n float64) map[string]any {
			if !withKeys {
				return nil
			}
			return tagged(n)
		}
		h.food("2026-09-27", "a", 500, 60, 5, 10.0, ex(10))
		h.food("2026-09-28", "b", 500, 60, 5, 10.0, ex(20))
		h.food("2026-09-29", "c", 500, 60, 5, 10.0, ex(30))
		// 2026-09-30: one tagged and one untagged item: not covered.
		h.food("2026-09-30", "d", 500, 60, 5, 10.0, ex(40))
		h.food("2026-09-30", "plain", 100, 5, 1, 1.0, nil)
		h.food("2026-10-01", "e", 800, 80, 5, 10.0, ex(0))
		h.restart()
	}
	h := newV7(t, v2("levers.avg_min_covered_days", 3, "levers.reference.nuts_g", 30))
	fix(h, true)
	y := h.snap(t, "?date=2026-09-30")
	if l := lever(y, "nuts_g"); l.UntaggedRows != 1 || l.Consumed != 40 || l.CoveredDays7 != 3 || num(l.Avg7) != "20" || num(l.Reference) != "30" {
		t.Errorf("2026-09-30: %+v", l)
	}
	// Today is covered with consumed 0: the mean is over 4 covered days; the
	// uncovered 2026-09-30 does not lower it.
	s := h.snap(t, "")
	if l := lever(s, "nuts_g"); l.CoveredDays7 != 4 || num(l.Avg7) != "15" || l.Consumed != 0 {
		t.Errorf("today: %+v", l)
	}
	if l := lever(s, "psyllium_g"); l.Reference != nil || num(l.Avg7) != "0" {
		t.Errorf("psyllium: %+v", l)
	}
	// Two covered days only: null.
	if l := lever(h.snap(t, "?date=2026-09-28"), "nuts_g"); l.Avg7 != nil || l.CoveredDays7 != 2 {
		t.Errorf("two covered days: %+v", l)
	}
	// The setting null: no mean.
	h2 := newV7(t, v2())
	fix(h2, true)
	if l := lever(h2.snap(t, ""), "nuts_g"); l.Avg7 != nil || l.CoveredDays7 != 4 || l.Reference != nil {
		t.Errorf("setting null: %+v", l)
	}
	// A lever never changes a score, a streak or a next action.
	h3 := newV7(t, v2("levers.avg_min_covered_days", 3, "levers.reference.nuts_g", 30))
	fix(h3, false)
	s3 := h3.snap(t, "")
	if s.DayScore != s3.DayScore || s.BudgetScore != s3.BudgetScore || s.Streaks != s3.Streaks ||
		fmt.Sprint(s.NextAction) != fmt.Sprint(s3.NextAction) || fmt.Sprint(s.BudgetNextAction) != fmt.Sprint(s3.BudgetNextAction) {
		t.Errorf("levers changed a score: %+v %+v vs %+v %+v", s.DayScore, s.BudgetScore, s3.DayScore, s3.BudgetScore)
	}
	raw := h.get("/fuel/snapshot").Body.Bytes()
	var doc struct {
		Levers []map[string]any `json:"levers"`
	}
	_ = json.Unmarshal(raw, &doc)
	for _, l := range doc.Levers {
		for _, bad := range []string{"status", "target", "pace_target_now", "score", "streak"} {
			if _, has := l[bad]; has {
				t.Errorf("lever %v has a key named %q", l["key"], bad)
			}
		}
	}
}
