package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func drink(name string, ml, kcal, caffeine, alcohol float64, needsFraction bool) string {
	cf, al := "null", "null"
	if caffeine > 0 {
		cf = fmt.Sprintf("%g", caffeine)
	}
	if alcohol > 0 {
		al = fmt.Sprintf("%g", alcohol)
	}
	return fmt.Sprintf(`{"intent":"log","items":[{"item":%q,"kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":%g,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":null,"needs_fraction":%v,"volume_ml":%g,"caffeine_mg":%s,"alcohol_g":%s}],"text":"","widgets":["fluids"]}`,
		name, kcal, needsFraction, ml, cf, al)
}

func intake(s Snapshot, key string) MacroState {
	for _, m := range s.Intake {
		if m.Key == key {
			return m
		}
	}
	return MacroState{}
}

func (h *harness) fix(cid, itemID string, body map[string]any) *httptest.ResponseRecorder {
	body["client_id"], body["item_id"] = cid, itemID
	b, _ := json.Marshal(body)
	return h.do("POST", "/fuel/fix", bytes.NewReader(b), "application/json")
}

func TestDrinkLogIntakeAndRow(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("Flat white", 200, 120, 130, 0, false) }
	resp := decode[LogResponse](t, h.logText("c0000001-0001", "flat white"))
	it := resp.Items[0]
	if it.Kind != "drink" || it.VolumeML.float() != 200 || it.CaffeineMG.float() != 130 || it.AlcoholG.OK {
		t.Fatalf("item %+v", it)
	}
	row := h.vars.rows("var-food")[0]
	if row["kind"] != "drink" || row["volume_ml"] != 200.0 || row["caffeine_mg"] != 130.0 {
		t.Fatalf("row %v", row)
	}
	if _, has := row["alcohol_g"]; has {
		t.Fatal("unknown alcohol_g must be omitted")
	}
	if fl := intake(resp.Snapshot, "fluids_ml"); fl.Consumed != 200 || *fl.Target != 2500 || fl.Kind != "floor" {
		t.Fatalf("fluids %+v", fl)
	}
	if c := intake(resp.Snapshot, "caffeine_mg"); c.Consumed != 130 || *c.Target != 400 || c.Status != "on_pace" {
		t.Fatalf("caffeine %+v", c)
	}
	var fluidsWidget bool
	for _, b := range resp.Blocks {
		fluidsWidget = fluidsWidget || b.Widget == "fluids"
	}
	if !fluidsWidget {
		t.Fatal("no fluids widget")
	}
	// Beer: alcohol today and this week.
	h.model.fn = func(ModelInput) string { return drink("Beer", 330, 140, 0, 13, false) }
	r2 := decode[LogResponse](t, h.logText("c0000001-0002", "a beer"))
	if a := intake(r2.Snapshot, "alcohol_g"); a.Consumed != 13 {
		t.Fatalf("alcohol today %+v", a)
	}
	if a := intake(r2.Snapshot, "alcohol_g_week"); a.Consumed != 13 || *a.Target != 30 {
		t.Fatalf("alcohol week %+v", a)
	}
	if fl := intake(r2.Snapshot, "fluids_ml"); fl.Consumed != 530 {
		t.Fatalf("fluids %+v", fl)
	}
}

func TestTargetsWithoutIntakeKeysStayValid(t *testing.T) {
	old := `{"protein_g":{"kind":"floor","value":160},"sat_fat_g":{"kind":"cap","value":20},"fiber_g":{"kind":"floor","value":35},"kcal":{"kind":"pace","rest":2300,"training":2600},"net_carbs_g":{"kind":"pace","rest":120,"training":null},"strength_per_week":3,"weight_band_kg":[78,82],"eating_window":{"start":"07:00","end":"20:30"},"tz":"Europe/Zurich"}`
	tg, err := ParseTargets([]byte(old))
	if err != nil || tg.WaterML != nil {
		t.Fatalf("%v %+v", err, tg.WaterML)
	}
	bad := strings.Replace(old, `"tz"`, `"water_ml":{"kind":"flor","value":1},"tz"`, 1)
	if _, err := ParseTargets([]byte(bad)); err == nil {
		t.Fatal("bad water_ml kind accepted")
	}
}

func TestFixRouteAbsoluteAndIdempotent(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, false) }
	w := decode[LogResponse](t, h.logText("c0000002-0001", "water")).Items[0]
	r := h.fix("c0000002-0002", w.ItemID, map[string]any{"volume_ml": 300})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	m := decode[MutationResponse](t, r)
	if m.Item.Effective.VolumeML.float() != 300 || intake(m.Snapshot, "fluids_ml").Consumed != 300 {
		t.Fatalf("after fix: %v %+v", m.Item.Effective.VolumeML.float(), intake(m.Snapshot, "fluids_ml"))
	}
	if !strings.HasPrefix(m.Blocks[0].Text, "Corrected Water to 300 ml.") {
		t.Fatalf("text %q", m.Blocks[0].Text)
	}
	rows := h.vars.rows("var-food")
	corr := rows[len(rows)-1]
	if corr["reason"] != "fix" || corr["volume_ml"] != -200.0 || corr["volume_ml_after"] != 300.0 || corr["share_after"] != 0.6 {
		t.Fatalf("correction row %v", corr)
	}
	// The same target again with a new client_id: absolute, so no new row.
	n := len(h.vars.rows("var-food"))
	if r := h.fix("c0000002-0003", w.ItemID, map[string]any{"volume_ml": 300}); r.Code != 200 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("repeat fix: %d rows %d->%d", r.Code, n, len(h.vars.rows("var-food")))
	}
	// Then 400 ml: the delta is +100 against the current 300.
	h.fix("c0000002-0004", w.ItemID, map[string]any{"volume_ml": 400})
	rows = h.vars.rows("var-food")
	if rows[len(rows)-1]["volume_ml"] != 100.0 {
		t.Fatalf("second fix delta %v", rows[len(rows)-1]["volume_ml"])
	}
	for _, bad := range []map[string]any{{}, {"volume_ml": 1, "share": 0.5}, {"share": 5}} {
		if r := h.fix("c0000002-0005", w.ItemID, bad); r.Code != 400 {
			t.Fatalf("%v: %d", bad, r.Code)
		}
	}
	// A food fix by grams, then undo cancels everything.
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	rice := decode[LogResponse](t, h.logText("c0000002-0010", "rice")).Items[0]
	fr := decode[MutationResponse](t, h.fix("c0000002-0011", rice.ItemID, map[string]any{"portion_g": 100}))
	if fr.Item.Effective.Kcal.float() != 130 {
		t.Fatalf("rice after fix %v", fr.Item.Effective.Kcal.float())
	}
	un := decode[MutationResponse](t, h.mutate("undo", "c0000002-0012", rice.ItemID, nil))
	if un.Item.Effective.Kcal.V != 0 {
		t.Fatal("undo after fix not zero")
	}
	if r := h.fix("c0000002-0013", rice.ItemID, map[string]any{"portion_g": 50}); r.Code != 409 {
		t.Fatalf("fix after undo %d", r.Code)
	}
}

func TestChatCorrections(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[
 {"item":"Water","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":500,"caffeine_mg":null,"alcohol_g":null},
 {"item":"Chicken breast","kind":"food","staple_key":null,"portion_g":200,"portion_basis":"stated","kcal":330,"protein_g":62,"carbs_g":0,"net_carbs_g":null,"fat_g":7,"sat_fat_g":2,"fiber_g":0,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	logged := decode[LogResponse](t, h.logText("c0000003-0001", "500 ml water and 200 g chicken"))
	var seen []LastItem
	h.model.fn = func(in ModelInput) string {
		seen = in.LastItems
		return `{"intent":"correct","items":[],"corrections":[{"ref":"water","portion_g":null,"volume_ml":300,"share":null}],"text":"","widgets":[]}`
	}
	r := h.logText("c0000003-0002", "it was only 300 ml")
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if len(seen) != 2 || seen[0].Item != "Water" || *seen[0].VolumeML != 500 {
		t.Fatalf("model saw %+v", seen)
	}
	if resp.Intent != "correct" || len(resp.Items) != 1 || resp.Items[0].Effective.VolumeML.float() != 300 {
		t.Fatalf("correct resp %+v", resp)
	}
	if !strings.HasPrefix(resp.Blocks[0].Text, "Corrected Water to 300 ml.") || resp.Blocks[2].Widget != "macros_today" {
		t.Fatalf("blocks %+v", resp.Blocks)
	}
	if intake(resp.Snapshot, "fluids_ml").Consumed != 300 {
		t.Fatal("fluids not corrected")
	}
	// "no, that was 100 g" with ref "last" -> the newest active item: chicken.
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":100,"volume_ml":null,"share":null}],"text":"","widgets":[]}`
	}
	c2 := decode[LogResponse](t, h.logText("c0000003-0003", "no, that was 100 g"))
	if c2.Items[0].ItemID != logged.Items[1].ItemID || c2.Items[0].Effective.Protein.float() != 31 {
		t.Fatalf("last ref %+v", c2.Items)
	}
	// Retry of the same message: idempotent (stored), no extra rows.
	n := len(h.vars.rows("var-food"))
	h.logText("c0000003-0003", "no, that was 100 g")
	if len(h.vars.rows("var-food")) != n {
		t.Fatal("retried correction wrote again")
	}
	// An unresolvable ref writes nothing and says so.
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"pancakes","portion_g":50,"volume_ml":null,"share":null}],"text":"","widgets":[]}`
	}
	c3 := decode[LogResponse](t, h.logText("c0000003-0004", "the pancakes were 50 g"))
	if len(c3.Items) != 0 || !strings.Contains(c3.Blocks[0].Text, "could not find pancakes") || len(h.vars.rows("var-food")) != n {
		t.Fatalf("unresolved %+v", c3.Blocks)
	}
	// The entry endpoint renders the same correction text.
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if !strings.HasPrefix(e.Blocks[0].Text, "Corrected Water to 300 ml.") {
		t.Fatalf("entry %q", e.Blocks[0].Text)
	}
	// Invalid correction output is retried and then 502.
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":100,"volume_ml":100,"share":null}],"text":"","widgets":[]}`
	}
	if r := h.logText("c0000003-0005", "x"); r.Code != 502 {
		t.Fatalf("two amounts: %d", r.Code)
	}
}

func TestAgentdRowsAndCorrections(t *testing.T) {
	h := newHarness(t)
	add := func(d map[string]any) string {
		h.vars.add("var-food", "2026-10-01", d)
		rows := h.vars.rows("var-food")
		return rows[len(rows)-1]["_id"].(string)
	}
	coffee := map[string]any{"item": "Coffee", "kind": "drink", "volume_ml": 200.0, "caffeine_mg": 95.0, "kcal": 2.0, "protein_g": 0.3, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "source": "agentd", "op_id": "op_ag_c1", "eaten_at": "2026-10-01T08:00:00+02:00"}
	id1 := add(coffee)
	id2 := add(coffee) // a duplicate copy of the same op_id
	// A fix that names the DUPLICATE copy: 200 -> 100 ml.
	add(map[string]any{"item": "correction: Coffee", "kcal": -1.0, "protein_g": -0.2, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "volume_ml": -100.0, "caffeine_mg": -47.5, "source": "agentd", "reason": "fix", "corrects": id2, "share_after": 0.5, "volume_ml_after": 100.0, "op_id": "op_fix_" + id2})
	_ = id1
	h.clk.Add(2 * time.Minute)
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if fl := intake(snap, "fluids_ml"); fl.Consumed != 100 {
		t.Fatalf("fluids %v (duplicate counted or correction lost)", fl.Consumed)
	}
	if c := intake(snap, "caffeine_mg"); c.Consumed != 47.5 {
		t.Fatalf("caffeine %v", c.Consumed)
	}
	rec := h.recent("")
	if len(rec.Items) != 1 || rec.Items[0].Kind != "drink" || *rec.Items[0].VolumeML != 100 || rec.Items[0].Source != "agentd" || rec.Items[0].PortionG != nil {
		t.Fatalf("recent %+v", rec.Items)
	}
}

func TestRecentFractionFromMetadataNotRoundedMacros(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[{"item":"Lemonade","kind":"drink","staple_key":null,"portion_g":500,"portion_basis":"photo_estimate","kcal":1,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":null,"needs_fraction":true,"volume_ml":500,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	p := decode[LogResponse](t, h.logText("c0000004-0001", "lemonade"))
	h.mutate("fraction", "c0000004-0002", p.Items[0].ItemID, f64(0.75))
	it := h.recent("").Items[0]
	if *it.PortionG != 375 || *it.VolumeML != 375 {
		t.Fatalf("portion %v volume %v, want 375", *it.PortionG, *it.VolumeML)
	}
	// Zero-macro water halved: 250 ml.
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, true) }
	w := decode[LogResponse](t, h.logText("c0000004-0003", "water"))
	h.mutate("fraction", "c0000004-0004", w.Items[0].ItemID, f64(0.5))
	for _, r := range h.recent("").Items {
		if r.Item == "Water" && *r.VolumeML != 250 {
			t.Fatalf("water %v", *r.VolumeML)
		}
	}
}

func TestRecentRefreshesStaleOlderDays(t *testing.T) {
	h := newHarness(t)
	h.vars.add("var-food", "2026-09-28", map[string]any{"item": "Toast", "portion_g": 40.0, "kcal": 100.0, "protein_g": 3.0, "carbs_g": 18.0, "fat_g": 1.0, "sat_fat_g": 0.2, "fiber_g": 1.0, "source": "agentd", "op_id": "op_ag_t", "eaten_at": "2026-09-28T08:00:00+02:00"})
	if len(h.recent("").Items) != 0 {
		t.Fatal("hydrated before the row existed; expected empty")
	}
	h.clk.Add(61 * time.Minute)
	if got := h.recent("").Items; len(got) != 1 || got[0].Item != "Toast" {
		t.Fatalf("stale older day not refreshed: %+v", got)
	}
	h.clk.Add(61 * time.Minute)
	h.vars.failReads.Store(true)
	if r := h.do("GET", "/fuel/recent", nil, ""); r.Code != 502 {
		t.Fatalf("failed refresh: %d", r.Code)
	}
}

func TestCorrectFailsOnItsOwnRejectedOp(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, false) }
	h.logText("d0000001-0001", "water")
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "fix" {
			return false, 400
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":null,"volume_ml":300,"share":null}],"text":"","widgets":[]}`
	}
	if r := h.logText("d0000001-0002", "it was only 300 ml"); r.Code != 502 {
		t.Fatalf("rejected correction answered %d", r.Code)
	}
	h.restart()
	if r := h.logText("d0000001-0002", "it was only 300 ml"); r.Code != 502 {
		t.Fatalf("replay after restart answered %d", r.Code)
	}
}

func TestNoOpFixReplaysAfterLaterChanges(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, false) }
	w := decode[LogResponse](t, h.logText("d0000002-0001", "water")).Items[0]
	h.fix("d0000002-0002", w.ItemID, map[string]any{"volume_ml": 500}) // a no-op
	h.fix("d0000002-0003", w.ItemID, map[string]any{"volume_ml": 400})
	n := len(h.vars.rows("var-food"))
	h.restart()
	if r := h.fix("d0000002-0002", w.ItemID, map[string]any{"volume_ml": 500}); r.Code != 200 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("no-op retry wrote: %d rows %d->%d", r.Code, n, len(h.vars.rows("var-food")))
	}
	if r := h.fix("d0000002-0002", w.ItemID, map[string]any{"volume_ml": 100}); r.Code != 409 {
		t.Fatalf("reused client_id with a new amount: %d", r.Code)
	}
}

func TestFixRefusesAnExternallyUndoneItem(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	rice := decode[LogResponse](t, h.logText("d0000003-0001", "rice")).Items[0]
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: Rice", "kcal": -260.0, "protein_g": -5.0, "carbs_g": -10.0, "net_carbs_g": -10.0, "fat_g": -2.0, "sat_fat_g": -1.0, "fiber_g": nil, "source": "agentd", "reason": "undo", "corrects": *rice.ValueID, "op_id": "op_undo_x"})
	h.clk.Add(61 * time.Second)
	if r := h.fix("d0000003-0002", rice.ItemID, map[string]any{"portion_g": 100}); r.Code != 409 || errCode(t, r) != "already_undone" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestLastMeansTheNewestEntryOnly(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	h.logText("d0000004-0001", "rice")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, false) }
	w := decode[LogResponse](t, h.logText("d0000004-0002", "water")).Items[0]
	h.mutate("undo", "d0000004-0003", w.ItemID, nil)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":null,"volume_ml":null,"share":0.5}],"text":"","widgets":[]}`
	}
	n := len(h.vars.rows("var-food"))
	r := decode[LogResponse](t, h.logText("d0000004-0004", "only half"))
	if len(r.Items) != 0 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("'last' reached an older entry: %+v", r.Items)
	}
}

func TestZeroMacroSupplementPortionFix(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[{"item":"Creatine","kind":"supplement","staple_key":null,"portion_g":5,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	c := decode[LogResponse](t, h.logText("d0000005-0001", "5 g creatine")).Items[0]
	if c.Kind != "supplement" {
		t.Fatal(c.Kind)
	}
	h.fix("d0000005-0002", c.ItemID, map[string]any{"portion_g": 3})
	got := h.recent("").Items[0]
	if got.Kind != "supplement" || *got.PortionG != 3 {
		t.Fatalf("recent %+v", got)
	}
}

func TestWindowRefreshRotates(t *testing.T) {
	h := newHarness(t)
	h.clk.Add(31 * time.Minute)
	for i := 0; i < 6; i++ {
		h.svc.refreshWindowSlice(context.Background(), 6)
		h.clk.Add(time.Minute)
	}
	for i := 2; i < recentDays; i++ {
		d := dateAdd("2026-10-01", -i)
		if h.svc.cache.Age(d) > 30*time.Minute {
			t.Fatalf("day %s never refreshed (age %v)", d, h.svc.cache.Age(d))
		}
	}
}

func TestLastAfterANoFoodPhotoIsUnresolved(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	h.logText("e0000001-0001", "rice")
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return `{"intent":"question","items":[],"text":"","widgets":[]}` }
	photoOnlyLog(t, h, "e0000001-0002")
	h.model.fn = func(in ModelInput) string {
		if len(in.LastItems) != 0 {
			t.Errorf("last items %+v", in.LastItems)
		}
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":null,"volume_ml":null,"share":0.5}],"text":"","widgets":[]}`
	}
	n := len(h.vars.rows("var-food"))
	if r := decode[LogResponse](t, h.logText("e0000001-0003", "only half")); len(r.Items) != 0 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("halved an older meal: %+v", r.Items)
	}
}

func TestFractionRefusesAnExternallyUndoneItem(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("e0000002-0001", "pizza")).Items[0]
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: pizza", "kcal": -801.0, "protein_g": -33.3, "carbs_g": -99.9, "net_carbs_g": -99.9, "fat_g": -29.9, "sat_fat_g": -13.3, "fiber_g": nil, "source": "agentd", "reason": "undo", "corrects": *p.ValueID, "op_id": "op_undo_p"})
	h.clk.Add(61 * time.Second)
	if r := h.mutate("fraction", "e0000002-0002", p.ItemID, f64(0.5)); r.Code != 409 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if _, ok := h.svc.journal.Fraction(p.ItemID); ok {
		t.Fatal("fraction choice journaled for an undone item")
	}
}

func TestLastSkipsAnExternallyUndoneItem(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[
 {"item":"Rice","kind":"food","staple_key":null,"portion_g":200,"portion_basis":"stated","kcal":260,"protein_g":5,"carbs_g":56,"net_carbs_g":null,"fat_g":1,"sat_fat_g":0.2,"fiber_g":1,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null},
 {"item":"Juice","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":110,"protein_g":1,"carbs_g":26,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":250,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	l := decode[LogResponse](t, h.logText("e0000003-0001", "rice and juice"))
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: Juice", "kcal": -110.0, "protein_g": -1.0, "carbs_g": -26.0, "net_carbs_g": -26.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "volume_ml": -250.0, "source": "agentd", "reason": "undo", "corrects": *l.Items[1].ValueID, "op_id": "op_undo_j"})
	h.clk.Add(2 * time.Minute) // no manual refresh: the correction path re-reads
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":null,"volume_ml":null,"share":0.5}],"text":"","widgets":[]}`
	}
	r := decode[LogResponse](t, h.logText("e0000003-0002", "only half"))
	if len(r.Items) != 1 || r.Items[0].Item != "Rice" {
		t.Fatalf("corrected %+v", r.Items)
	}
}

func TestCorrectionAcrossTwoDays(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	b, _ := json.Marshal(map[string]string{"client_id": "e0000004-0001", "text": "rice", "local_time": "2026-09-30T19:00:00+02:00"})
	rice := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json")).Items[0]
	h.clk.Add(time.Minute)
	h.model.fn = func(ModelInput) string { return oneItem("Chicken", 200, 330, 62, false) }
	chicken := decode[LogResponse](t, h.logText("e0000004-0002", "chicken")).Items[0]
	h.model.fn = func(ModelInput) string {
		return fmt.Sprintf(`{"intent":"correct","items":[],"corrections":[{"ref":%q,"portion_g":100,"volume_ml":null,"share":null},{"ref":%q,"portion_g":100,"volume_ml":null,"share":null}],"text":"","widgets":[]}`, chicken.ItemID, rice.ItemID)
	}
	r := decode[LogResponse](t, h.logText("e0000004-0003", "both were 100 g"))
	var added map[string]any
	for _, bl := range r.Blocks {
		if bl.Widget == "macros_today" {
			added = bl.Data.(map[string]any)["added"].(map[string]any)
			if bl.Date != r.Snapshot.Date {
				t.Fatal("widget date mismatch")
			}
		}
	}
	// The snapshot day's delta only: one of -165 (chicken) or -130 (rice).
	k := added["kcal"].(float64)
	want := -165.0
	if r.Snapshot.Date == "2026-09-30" {
		want = -130
	}
	if k != want || !strings.Contains(r.Blocks[0].Text, "logged on") {
		t.Fatalf("added kcal %v (date %s) text %q", k, r.Snapshot.Date, r.Blocks[0].Text)
	}
}

func TestRejectedFractionDoesNotBlockANewOne(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("f0000001-0001", "pizza")).Items[0]
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "fraction" {
			return false, 400
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	if r := h.mutate("fraction", "f0000001-0002", p.ItemID, f64(0.5)); r.Code != 502 {
		t.Fatalf("%d", r.Code)
	}
	h.vars.mu.Lock()
	h.vars.onPost = nil
	h.vars.mu.Unlock()
	h.restart()
	if r := h.mutate("fraction", "f0000001-0003", p.ItemID, f64(0.5)); r.Code != 200 {
		t.Fatalf("new fraction after a rejected one: %d %s", r.Code, r.Body)
	}
}

func TestRejectedChatCorrectionFeedAndEntrySayItFailed(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("Water", 500, 0, 0, 0, false) }
	h.logText("f0000002-0001", "water")
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "fix" {
			return false, 400
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":null,"volume_ml":300,"share":null}],"text":"","widgets":[]}`
	}
	h.logText("f0000002-0002", "it was only 300 ml")
	h.restart()
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	for _, it := range feed.Items {
		for _, b := range it.Blocks {
			if strings.Contains(b.Text, "Corrected Water") {
				t.Fatalf("feed claims success: %q", b.Text)
			}
		}
	}
	var entryID string
	for _, e := range h.svc.journal.Entries() {
		if e.Intent == "correct" {
			entryID = e.ID
		}
	}
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+entryID, nil, ""))
	if !strings.HasPrefix(e.Blocks[0].Text, "Could not save the correction of Water") {
		t.Fatalf("entry text %q", e.Blocks[0].Text)
	}
}

func TestRejectedCompensationIsReportedAndRetried(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["item"] == "walnuts" || d["reason"] == "compensation" {
			return false, 400
		}
		return true, 201
	}
	h.logText("f0000003-0001", "250 g skyr and 30 g walnuts")
	h.svc.reconcileOnce(context.Background())
	var comp Op
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "compensation" {
			comp = op
		}
	}
	if comp.ID == "" || terminal(comp.State) || !h.svc.feed.HasNotice(comp.ID) {
		t.Fatalf("compensation %+v notice %v", comp.State, h.svc.feed.HasNotice(comp.ID))
	}
}

func TestPendingCorrectionThatFailsUpdatesTheFeed(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[
 {"item":"Water","kind":"drink","staple_key":null,"portion_g":null,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false,"volume_ml":500,"caffeine_mg":null,"alcohol_g":null},
 {"item":"Rice","kind":"food","staple_key":null,"portion_g":200,"portion_basis":"stated","kcal":260,"protein_g":5,"carbs_g":56,"net_carbs_g":null,"fat_g":1,"sat_fat_g":0.2,"fiber_g":1,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null}],"text":"","widgets":[]}`
	}
	h.logText("f0000004-0001", "water and rice")
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) {
		if d["reason"] == "fix" && d["item"] == "correction: Water" {
			return false, 500 // uncertain: pending, later failed
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[{"ref":"Water","portion_g":null,"volume_ml":300,"share":null},{"ref":"Rice","portion_g":100,"volume_ml":null,"share":null},{"ref":"pancakes","portion_g":50,"volume_ml":null,"share":null}],"text":"","widgets":[]}`
	}
	r := decode[LogResponse](t, h.logText("f0000004-0002", "300 ml water, 100 g rice, 50 g pancakes"))
	txt := r.Blocks[0].Text
	if !strings.Contains(txt, "Water to 300 ml (still saving)") || !strings.Contains(txt, "Corrected Rice to 100 g.") || !strings.Contains(txt, "could not find pancakes") {
		t.Fatalf("mixed summary %q", txt)
	}
	h.clk.Add(25 * time.Hour)
	for i := 0; i < 4; i++ {
		h.svc.reconcileOnce(context.Background())
		h.clk.Add(61 * time.Second)
	}
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed?limit=100", nil, ""))
	found := false
	for _, it := range feed.Items {
		if len(it.Blocks) > 0 && strings.Contains(it.Blocks[0].Text, "Could not save the correction of Water") {
			found = strings.Contains(it.Blocks[0].Text, "Corrected Rice to 100 g.")
		}
	}
	if !found {
		t.Fatal("feed reply not re-rendered after the pending correction failed")
	}
}
