package fuel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// T22 (spec section 20): the write operations of the chat without a model call.
func TestT22ItemsRouteWritesWithoutTheModel(t *testing.T) {
	h := newV7(t, v2(), func(o *Options) {
		st := `[{"key":"skyr","aliases":["skyr"],"default_g":250,"per_100g":{"kcal":63,"protein_g":11,"carbs_g":4,"fat_g":0.2,"sat_fat_g":0.1,"fiber_g":0}}]`
		if err := writeFile(o.StaplesFile, st); err != nil {
			t.Fatal(err)
		}
	})
	h.model.fn = func(ModelInput) string { t.Error("the model was called"); return "{}" }
	body := map[string]any{"client_id": "90000000-0001", "note": "breakfast", "items": []any{
		map[string]any{"item": "almonds", "portion_g": 30, "kcal": 174, "protein_g": 6.3, "carbs_g": 6.5, "fat_g": 15, "sat_fat_g": 1.1, "fiber_g": 3.8,
			"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 30, "pulses_g": 0, "plant_protein_g": 6.3, "brew_method": nil}},
		map[string]any{"item": "espresso", "kind": "drink", "volume_ml": 30, "caffeine_mg": 65, "kcal": 1, "protein_g": 0.1, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0,
			"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 0, "plant_protein_g": 0, "brew_method": "espresso"}},
	}}
	rec := h.post("/fuel/items", body)
	if rec.Code != 200 {
		t.Fatalf("items: %d %s", rec.Code, rec.Body)
	}
	r := decode[LogResponse](t, rec)
	if r.Intent != "log" || r.Status != "done" || len(r.Items) != 2 || r.Items[0].PortionBasis != "stated" || num(r.Items[0].Levers.Nuts) != "30" ||
		r.Items[1].Kind != "drink" || *r.Items[1].Levers.BrewMethod != "espresso" || macro(r.Snapshot, "kcal").Consumed != 175 || r.Snapshot.Coffee.Today.Espresso != 1 {
		t.Fatalf("response: %+v", r.Items)
	}
	rows := h.vars.rows("var-food")
	if len(rows) == 2 && rows[0]["item"] != "almonds" {
		rows[0], rows[1] = rows[1], rows[0] // the two rows are posted at the same time
	}
	if len(rows) != 2 || rows[0]["source"] != "fuel" || rows[0]["nuts_g"] != 30.0 || rows[0]["entry_id"] != r.EntryID || rows[1]["brew_method"] != "espresso" || rows[0]["net_carbs_g"] != 2.7 {
		t.Fatalf("rows: %v", rows)
	}
	// Idempotent by client_id; another body = 409.
	if again := h.post("/fuel/items", body); again.Code != 200 || again.Body.String() != rec.Body.String() || len(h.vars.rows("var-food")) != 2 {
		t.Errorf("repeat: %d, %d rows", again.Code, len(h.vars.rows("var-food")))
	}
	body["note"] = "lunch"
	if c := h.post("/fuel/items", body); c.Code != 409 || errCode(t, c) != "idempotency_conflict" {
		t.Errorf("another body: %d %s", c.Code, c.Body)
	}
	// Refusals: nothing is written.
	item := func(kv ...any) map[string]any {
		m := map[string]any{"item": "x", "kcal": 100, "protein_g": 5, "carbs_g": 5, "fat_g": 5, "sat_fat_g": 1}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	for name, b := range map[string]map[string]any{
		"missing sat_fat_g": {"client_id": cid(), "items": []any{item("sat_fat_g", nil)}},
		"unknown key":       {"client_id": cid(), "items": []any{item("vitamin_c_mg", 5)}},
		"kcal 5000":         {"client_id": cid(), "items": []any{item("kcal", 5000)}},
		"no items":          {"client_id": cid(), "items": []any{}},
		"13 items":          {"client_id": cid(), "items": []any{item(), item(), item(), item(), item(), item(), item(), item(), item(), item(), item(), item(), item()}},
		"day too far back":  {"client_id": cid(), "day": dateAdd("2026-10-01", -40), "items": []any{item()}},
		"bad client id":     {"client_id": "x", "items": []any{item()}},
	} {
		if rec := h.post("/fuel/items", b); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if n := len(h.vars.rows("var-food")); n != 2 {
		t.Fatalf("a refused request wrote rows: %d", n)
	}
	// day "yesterday": the row is on yesterday's date and the reply names the day.
	h.clk.Add(time.Minute)
	y := h.post("/fuel/items", map[string]any{"client_id": cid(), "day": "yesterday", "time": "19:30", "items": []any{item("item", "lentil soup")}})
	yr := decode[LogResponse](t, y)
	if y.Code != 200 || yr.Snapshot.Date != "2026-09-30" || !strings.HasPrefix(yr.Blocks[0].Text, "Logged for Wed 30 Sep: lentil soup.") || lastRow(h)["_record_date"] != "2026-09-30" {
		t.Errorf("yesterday: %d %q %v", y.Code, yr.Blocks[0].Text, lastRow(h)["_record_date"])
	}
	// A staple key gives the label values.
	h.clk.Add(time.Minute)
	sk := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{item("item", "skyr", "staple_key", "skyr", "portion_g", 200)}}))
	if len(sk.Items) != 1 || sk.Items[0].Kcal.float() != 126 || sk.Items[0].Protein.float() != 22 {
		t.Errorf("staple: %+v", sk.Items)
	}
	// An implausible item is written with the check flag (no re-ask without a model).
	h.clk.Add(time.Minute)
	odd := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{
		item("item", "salad leaves", "portion_g", 80, "kcal", 90, "protein_g", 1, "carbs_g", 2, "fat_g", 0.2, "sat_fat_g", 0, "food_class", "leafy_vegetable")}}))
	if len(odd.Items) != 1 || odd.Items[0].Check == "" {
		t.Errorf("implausible item: %+v", odd.Items)
	}
	// Lever checks run: 500 g nuts in a 30 g portion is dropped.
	h.clk.Add(time.Minute)
	lvr := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{
		item("item", "nut mix", "portion_g", 30, "levers", map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 500, "pulses_g": 0, "plant_protein_g": 4, "brew_method": nil})}}))
	if len(lvr.Items) != 1 || lvr.Items[0].Levers.Nuts != nil || num(lvr.Items[0].Levers.PlantProtein) != "4" {
		t.Errorf("lever check: %+v", lvr.Items)
	}
	if h.model.calls != 0 {
		t.Errorf("%d model calls", h.model.calls)
	}
}

func TestT22MoveRouteAndTheOtherRoutesMakeNoModelCall(t *testing.T) {
	h := newV7(t, v2())
	beer := map[string]any{"item": "beer", "kind": "drink", "volume_ml": 330, "alcohol_g": 13, "kcal": 140, "protein_g": 1, "carbs_g": 12, "fat_g": 0, "sat_fat_g": 0}
	r := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{beer}}))
	tg := agentdRow(h, "wine", 150, 120, "2026-10-01T11:00:00+02:00", map[string]any{"alcohol_g": 14.0})
	h.clk.Add(61 * time.Second)
	h.model.fn = func(ModelInput) string { t.Error("the model was called"); return "{}" }
	before := h.snap(t, "")
	var weekAlc float64
	for _, m := range before.Intake {
		if m.Key == "alcohol_g_week" {
			weekAlc = m.Consumed
		}
	}
	if weekAlc != 27 {
		t.Fatalf("alcohol this week before: %g", weekAlc)
	}
	rec := h.post("/fuel/move", map[string]any{"client_id": "91000000-0001", "day": "yesterday", "items": []any{r.Items[0].ItemID, "v:" + tg}})
	if rec.Code != 200 {
		t.Fatalf("move: %d %s", rec.Code, rec.Body)
	}
	mv := decode[LogResponse](t, rec)
	if mv.Intent != "move" || mv.Snapshot.Date != "2026-09-30" || !strings.HasPrefix(mv.Blocks[0].Text, "Moved beer and wine to Wed 30 Sep.") {
		t.Fatalf("move reply: %+v", mv.Blocks[0])
	}
	today, yday := h.snap(t, ""), h.snap(t, "?date=2026-09-30")
	alc := func(s Snapshot, key string) float64 {
		for _, m := range s.Intake {
			if m.Key == key {
				return m.Consumed
			}
		}
		return -1
	}
	if alc(today, "alcohol_g") != 0 || alc(yday, "alcohol_g") != 27 || alc(today, "alcohol_g_week") != 27 {
		t.Errorf("after the move: today %g, yesterday %g, week %g", alc(today, "alcohol_g"), alc(yday, "alcohol_g"), alc(today, "alcohol_g_week"))
	}
	moved, undos := 0, 0
	for _, row := range h.vars.rows("var-food") {
		if row["moved_from"] != nil && row["_record_date"] == "2026-09-30" {
			moved++
		}
		if row["reason"] == "undo" && row["moved_to"] == "2026-09-30" {
			undos++
		}
	}
	if moved != 2 || undos != 2 {
		t.Errorf("rows: %d moved, %d undo", moved, undos)
	}
	// A repeat is idempotent.
	n := len(h.vars.rows("var-food"))
	if again := h.post("/fuel/move", map[string]any{"client_id": "91000000-0001", "day": "yesterday", "items": []any{r.Items[0].ItemID, "v:" + tg}}); again.Code != 200 || len(h.vars.rows("var-food")) != n {
		t.Errorf("repeat: %d, %d rows", again.Code, len(h.vars.rows("var-food"))-n)
	}
	// An unknown id moves nothing.
	u := h.post("/fuel/move", map[string]any{"client_id": cid(), "day": "yesterday", "item_id": "it_nope"})
	if ur := decode[LogResponse](t, u); u.Code != 200 || !strings.Contains(ur.Blocks[0].Text, "I cannot find the item it_nope") || len(h.vars.rows("var-food")) != n {
		t.Errorf("unknown id: %d %s", u.Code, u.Body)
	}
	for _, b := range []map[string]any{
		{"client_id": cid(), "day": "tomorrow", "item_id": "x"},
		{"client_id": cid(), "day": "yesterday"},
	} {
		if rec := h.post("/fuel/move", b); rec.Code != 400 {
			t.Errorf("bad move: %d %s", rec.Code, rec.Body)
		}
	}
	// Fix, undo, relog, day, week and snapshot make no model call.
	h.clk.Add(time.Minute)
	w := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{
		map[string]any{"item": "water", "kind": "drink", "volume_ml": 500, "kcal": 0, "protein_g": 0, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0}}}))
	id := w.Items[0].ItemID
	h.clk.Add(time.Second)
	if rec := h.fix(cid(), id, map[string]any{"volume_ml": 300}); rec.Code != 200 {
		t.Errorf("fix: %d %s", rec.Code, rec.Body)
	}
	var key string
	for _, it := range h.svc.recentAll() {
		if it.Item == "water" {
			key = it.Key
		}
	}
	h.clk.Add(time.Minute)
	if rec := h.relog(cid(), key, nil); rec.Code != 200 {
		t.Errorf("relog: %d %s", rec.Code, rec.Body)
	}
	h.clk.Add(time.Second)
	if rec := h.mutate("undo", cid(), id, nil); rec.Code != 200 {
		t.Errorf("undo: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/fuel/day", "/fuel/week", "/fuel/snapshot", "/fuel/recent"} {
		if rec := h.get(p); rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if h.model.calls != 0 {
		t.Errorf("%d model calls", h.model.calls)
	}
}

// The v7 forms of POST /fuel/fix: the chat's corrections with given values.
func TestT22FixFormsWithoutTheModel(t *testing.T) {
	h := newV7(t, v2(), func(o *Options) { o.RecheckAfter = time.Second })
	h.model.fn = func(ModelInput) string { t.Error("the model was called"); return "{}" }
	add := func(item map[string]any) ItemState {
		t.Helper()
		h.clk.Add(time.Minute)
		rec := h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{item}})
		if rec.Code != 200 {
			t.Fatalf("items: %d %s", rec.Code, rec.Body)
		}
		return decode[LogResponse](t, rec).Items[0]
	}
	fixIt := func(id string, body map[string]any) (int, MutationResponse, string) {
		h.clk.Add(time.Second)
		rec := h.fix(cid(), id, body)
		var m MutationResponse
		if rec.Code == 200 {
			m = decode[MutationResponse](t, rec)
		}
		return rec.Code, m, rec.Body.String()
	}
	rice := add(map[string]any{"item": "rice", "portion_g": 100, "kcal": 130, "protein_g": 2.7, "carbs_g": 28, "fat_g": 0.3, "sat_fat_g": 0.1, "fiber_g": 0.4})
	// portion_g_delta 50 on a 100 g item gives 150 g and scaled macros.
	code, m, raw := fixIt(rice.ItemID, map[string]any{"portion_g_delta": 50})
	if code != 200 || m.Item.Effective.Kcal.float() != 195 || lastRow(h)["portion_g_after"] != 150.0 || lastRow(h)["reason"] != "fix" {
		t.Fatalf("portion_g_delta: %d %s", code, raw)
	}
	// count_delta 1 doubles a serving.
	bar := add(map[string]any{"item": "protein bar", "portion_g": 60, "kcal": 200, "protein_g": 20, "carbs_g": 20, "fat_g": 6, "sat_fat_g": 3})
	if code, m, raw = fixIt(bar.ItemID, map[string]any{"count_delta": 1}); code != 200 || m.Item.Effective.Kcal.float() != 400 {
		t.Fatalf("count_delta: %d %s", code, raw)
	}
	// A delta on an item without that amount = 409.
	if code, _, raw = fixIt(bar.ItemID, map[string]any{"volume_ml_delta": 100}); code != 409 || !strings.Contains(raw, "not_applicable") {
		t.Errorf("volume delta without a volume: %d %s", code, raw)
	}
	// revised: renames, sets the macros and the levers, one row of reason "revise".
	rows := len(h.vars.rows("var-food"))
	rev := map[string]any{"item": "rice with lentils", "portion_g": 200, "kcal": 290, "protein_g": 12, "carbs_g": 50, "net_carbs_g": nil, "fat_g": 2, "sat_fat_g": 0.3, "fiber_g": 6, "food_class": "mixed_dish",
		"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 80, "plant_protein_g": 12, "brew_method": nil}}
	h.clk.Add(time.Second)
	body := map[string]any{"client_id": "92000000-0001", "item_id": rice.ItemID, "revised": rev}
	rec := h.post("/fuel/fix", body)
	if rec.Code != 200 {
		t.Fatalf("revised: %d %s", rec.Code, rec.Body)
	}
	m = decode[MutationResponse](t, rec)
	if m.Item.Item != "rice with lentils" || m.Item.Effective.Kcal.float() != 290 || *m.Item.PortionG != 200 || num(m.Item.Levers.Pulses) != "80" ||
		!strings.HasPrefix(m.Blocks[0].Text, "Re-estimated rice as rice with lentils, 200 g: 290 kcal, 12 g protein.") {
		t.Fatalf("revised item: %+v %q", m.Item, m.Blocks[0].Text)
	}
	if n := len(h.vars.rows("var-food")); n != rows+1 || lastRow(h)["reason"] != "revise" || lastRow(h)["pulses_g"] != 80.0 {
		t.Fatalf("revise row: %d rows, %v", n-rows, lastRow(h))
	}
	// Idempotent, also after a restart.
	if again := h.post("/fuel/fix", body); again.Code != 200 || again.Body.String() != rec.Body.String() {
		t.Errorf("repeat: %d", again.Code)
	}
	h.restart()
	h.model.fn = func(ModelInput) string { t.Error("the model was called"); return "{}" }
	if again := h.post("/fuel/fix", body); again.Code != 200 || len(h.vars.rows("var-food")) != rows+1 {
		t.Errorf("repeat after a restart: %d, %d rows", again.Code, len(h.vars.rows("var-food"))-rows)
	}
	body["revised"].(map[string]any)["kcal"] = 300
	if c := h.post("/fuel/fix", body); c.Code != 409 {
		t.Errorf("another body: %d %s", c.Code, c.Body)
	}
	// A revised with only another brew method is written.
	coffee := add(map[string]any{"item": "coffee", "kind": "drink", "volume_ml": 200, "caffeine_mg": 95, "kcal": 2, "protein_g": 0.2, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0, "fiber_g": 0,
		"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 0, "plant_protein_g": 0, "brew_method": "unknown"}})
	brew := map[string]any{"item": "coffee", "portion_g": nil, "kcal": 2, "protein_g": 0.2, "carbs_g": 0, "net_carbs_g": nil, "fat_g": 0, "sat_fat_g": 0, "fiber_g": 0, "food_class": "drink",
		"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 0, "plant_protein_g": 0, "brew_method": "filtered"}}
	if code, m, raw = fixIt(coffee.ItemID, map[string]any{"revised": brew}); code != 200 || m.Item.Levers.BrewMethod == nil || *m.Item.Levers.BrewMethod != "filtered" || m.Item.Effective.VolumeML.float() != 200 {
		t.Errorf("brew revise: %d %s", code, raw)
	}
	// Refusals write nothing.
	rows = len(h.vars.rows("var-food"))
	bad := map[string]any{"item": "leaves", "portion_g": 50, "kcal": 900, "protein_g": 1, "carbs_g": 2, "net_carbs_g": nil, "fat_g": 0.2, "sat_fat_g": 0, "fiber_g": 1, "food_class": "leafy_vegetable",
		"levers": map[string]any{"psyllium_g": 0, "beta_glucan_g": 0, "nuts_g": 0, "pulses_g": 0, "plant_protein_g": 0, "brew_method": nil}}
	for name, b := range map[string]map[string]any{
		"implausible revised": {"revised": bad},
		"two forms":           {"portion_g": 100, "portion_g_delta": 10},
		"delta 0":             {"portion_g_delta": 0},
		"revised kcal 9000":   {"revised": map[string]any{"item": "x", "portion_g": 100, "kcal": 9000, "protein_g": 1, "carbs_g": 1, "net_carbs_g": nil, "fat_g": 1, "sat_fat_g": 1, "fiber_g": 1, "food_class": "other", "levers": nil}},
	} {
		if code, _, raw := fixIt(bar.ItemID, b); code != 400 {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	tg := agentdRow(h, "tea", 300, 2, "2026-10-01T11:00:00+02:00", nil)
	h.clk.Add(61 * time.Second)
	if code, _, raw := fixIt("v:"+tg, map[string]any{"revised": brew}); code != 400 {
		t.Errorf("revised on a row of another writer: %d %s", code, raw)
	}
	if code, m, raw := fixIt("v:"+tg, map[string]any{"volume_ml_delta": 100}); code != 200 || m.Item.Effective.VolumeML.float() != 400 {
		t.Errorf("delta on a row of another writer: %d %s", code, raw)
	}
	if n := len(h.vars.rows("var-food")); n != rows+2 { // the tea row and its fix
		t.Errorf("rows after the refusals: %d", n-rows)
	}
	// A first answer of 202: a retry returns the current state (done).
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { return true, 500 }
	h.clk.Add(time.Second)
	pb := map[string]any{"client_id": "92000000-0002", "item_id": bar.ItemID, "portion_g_delta": 30}
	first := h.post("/fuel/fix", pb)
	h.vars.onPost = nil
	if first.Code != 202 {
		t.Fatalf("uncertain fix: %d %s", first.Code, first.Body)
	}
	h.clk.Add(2 * time.Second)
	h.svc.reconcileOnce(context.Background())
	if retry := h.post("/fuel/fix", pb); retry.Code != 200 || decode[MutationResponse](t, retry).Status != "done" {
		t.Errorf("retry after reconciliation: %d %s", retry.Code, retry.Body)
	}
	if h.model.calls != 0 {
		t.Errorf("%d model calls", h.model.calls)
	}
}
