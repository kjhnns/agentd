package fuel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type recentResp struct {
	Items []RecentItem `json:"items"`
}

func (h *harness) recent(q string) recentResp {
	h.t.Helper()
	return decode[recentResp](h.t, h.do("GET", "/fuel/recent"+q, nil, ""))
}

func oneItem(name string, portion, kcal, protein float64, needsFraction bool) string {
	return fmt.Sprintf(`{"intent":"log","items":[{"item":%q,"staple_key":null,"portion_g":%g,"portion_basis":"stated","kcal":%g,"protein_g":%g,"carbs_g":10,"net_carbs_g":null,"fat_g":2,"sat_fat_g":1,"fiber_g":null,"needs_fraction":%v}],"text":"","widgets":[]}`,
		name, portion, kcal, protein, needsFraction)
}

func TestRecentGroupingOrderUndoFractionLimit(t *testing.T) {
	h := newHarness(t)
	logAs := func(cid, model string) LogResponse {
		h.model.fn = func(ModelInput) string { return model }
		h.clk.Add(time.Minute)
		return decode[LogResponse](t, h.logText(cid, cid))
	}
	logAs("b0000001-0001", oneItem("Skyr", 250, 160, 27, false))
	logAs("b0000001-0002", oneItem("  skyr ", 252, 161, 27, false)) // same key: 250 and 252 round to 250
	walnut := logAs("b0000001-0003", oneItem("Walnuts", 30, 196, 5, false))
	pizza := logAs("b0000001-0004", oneItem("Pizza", 400, 800, 30, true))
	oats := logAs("b0000001-0005", oneItem("Oats", 60, 220, 8, false))
	h.mutate("undo", "b0000001-0006", walnut.Items[0].ItemID, nil)
	h.mutate("fraction", "b0000001-0007", pizza.Items[0].ItemID, f64(0.5))
	// An agentd food-log row and its undo (corrects = VALUE id).
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "Banana", "portion_g": 120.0, "kcal": 105.0, "protein_g": 1.3, "carbs_g": 27.0, "fat_g": 0.4, "sat_fat_g": 0.1, "fiber_g": 3.1, "source": "agentd", "op_id": "op_ag_1", "eaten_at": "2026-10-01T12:30:00+02:00"})
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "Apple", "portion_g": 150.0, "kcal": 80.0, "protein_g": 0.4, "carbs_g": 21.0, "fat_g": 0.2, "sat_fat_g": 0.0, "fiber_g": 3.6, "source": "agentd", "op_id": "op_ag_2", "eaten_at": "2026-10-01T12:31:00+02:00"})
	rows := h.vars.rows("var-food")
	appleID := rows[len(rows)-1]["_id"].(string)
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: Apple", "kcal": -80.0, "protein_g": -0.4, "carbs_g": -21.0, "fat_g": -0.2, "sat_fat_g": 0.0, "fiber_g": -3.6, "source": "agentd", "reason": "undo", "corrects": appleID, "op_id": "op_undo_" + appleID})
	h.clk.Add(2 * time.Minute)

	got := h.recent("")
	var names []string
	for _, it := range got.Items {
		names = append(names, it.Item)
	}
	// Newest first: banana 12:30 local (10:30Z) > oats > pizza > skyr. Walnuts
	// and the apple are undone.
	want := []string{"Banana", "Oats", "Pizza", "skyr"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("recent %v, want %v", names, want)
	}
	byName := map[string]RecentItem{}
	for _, it := range got.Items {
		byName[it.Item] = it
	}
	if byName["skyr"].Times != 2 || *byName["skyr"].PortionG != 252 {
		t.Fatalf("skyr %+v", byName["skyr"])
	}
	pz := byName["Pizza"]
	if *pz.PortionG != 200 || pz.Macros.Kcal.float() != 400 || pz.Source != "fuel" {
		t.Fatalf("pizza %+v kcal %v", pz, pz.Macros.Kcal.float())
	}
	if byName["Banana"].Source != "agentd" || byName["Banana"].Times != 1 {
		t.Fatalf("banana %+v", byName["Banana"])
	}
	if byName["Oats"].Key != recentKey("food", "oats", f64(60), nil) || oats.EntryID == "" {
		t.Fatal("key not the stable hash")
	}
	if l := h.recent("?limit=2"); len(l.Items) != 2 || l.Items[0].Item != "Banana" {
		t.Fatalf("limit: %+v", l.Items)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x"} {
		if r := h.do("GET", "/fuel/recent"+q, nil, ""); r.Code != 400 {
			t.Fatalf("%s: %d", q, r.Code)
		}
	}
}

func TestRecentPhotoID(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Pizza", 400, 800, 30, false) }
	body, ct := multipartBody(t, map[string]string{"client_id": "b0000002-0001", "text": "pizza"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(100, 100)}})
	p := decode[LogResponse](t, h.do("POST", "/fuel/log", body, ct))
	got := h.recent("")
	if len(got.Items) != 1 || got.Items[0].PhotoID == nil || *got.Items[0].PhotoID != p.PhotoIDs[0] {
		t.Fatalf("photo id %+v", got.Items)
	}
	_ = os.Remove(filepath.Join(h.opts.StateDir, "photos", p.PhotoIDs[0]+".jpg"))
	if got := h.recent(""); got.Items[0].PhotoID != nil {
		t.Fatal("pruned photo still referenced")
	}
}

func (h *harness) relog(cid, key string, scale *float64) *httptest.ResponseRecorder {
	m := map[string]any{"client_id": cid, "key": key}
	if scale != nil {
		m["scale"] = *scale
	}
	b, _ := json.Marshal(m)
	return h.do("POST", "/fuel/relog", bytes.NewReader(b), "application/json")
}

func TestRelogMathIdempotencyAndShape(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Skyr", 250, 157.5, 27.5, false) }
	h.logText("b0000003-0001", "skyr")
	key := h.recent("").Items[0].Key
	calls := h.model.calls

	r := h.relog("b0000003-0002", key, f64(0.5))
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	it := resp.Items[0]
	if resp.Intent != "log" || len(resp.Items) != 1 || it.Kcal.float() != 78.8 || *it.PortionG != 125 || it.PortionBasis != "repeat" {
		t.Fatalf("relog %+v kcal %v", it, it.Kcal.float())
	}
	if h.model.calls != calls {
		t.Fatal("relog called the model")
	}
	if resp.LatencyMs == nil || resp.Snapshot.Revision != 2 || len(resp.Blocks) != 1 || resp.Blocks[0].Widget != "macros_today" {
		t.Fatalf("shape %+v", resp)
	}
	var added map[string]any
	for _, b := range resp.Blocks {
		if b.Widget == "macros_today" {
			added = b.Data.(map[string]any)["added"].(map[string]any)
		}
	}
	if added["kcal"] != 78.8 {
		t.Fatalf("added %v", added)
	}
	rows := h.vars.rows("var-food")
	last := rows[len(rows)-1]
	if len(rows) != 2 || last["portion_basis"] != "repeat" || last["source"] != "fuel" || last["op_id"] == nil {
		t.Fatalf("row %v", last)
	}
	// Idempotent: same client_id, same answer, no second row.
	r2 := decode[LogResponse](t, h.relog("b0000003-0002", key, f64(0.5)))
	if r2.EntryID != resp.EntryID || len(h.vars.rows("var-food")) != 2 {
		t.Fatal("relog repeat not idempotent")
	}
	if r := h.relog("b0000003-0002", key, f64(1)); r.Code != 409 {
		t.Fatalf("changed scale with same client_id: %d", r.Code)
	}
	if r := h.relog("b0000003-0003", "nope", nil); r.Code != 404 {
		t.Fatalf("unknown key %d", r.Code)
	}
	if r := h.relog("b0000003-0004", key, f64(3)); r.Code != 400 {
		t.Fatalf("bad scale %d", r.Code)
	}
	// scale defaults to 1, and the feed records "again: <item>".
	r3 := decode[LogResponse](t, h.relog("b0000003-0005", key, nil))
	if r3.Items[0].Kcal.float() != 157.5 {
		t.Fatalf("default scale kcal %v", r3.Items[0].Kcal.float())
	}
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	found := false
	for _, f := range feed.Items {
		found = found || (f.Text != nil && *f.Text == "again: Skyr")
	}
	if !found {
		t.Fatal("feed lacks the again line")
	}
	// The recent list now counts the relogs: 250 g twice (log + scale 1),
	// 125 g once.
	rec := h.recent("")
	if len(rec.Items) != 2 || rec.Items[0].Times != 2 || *rec.Items[1].PortionG != 125 {
		t.Fatalf("recent after relogs %+v", rec.Items)
	}
}

func TestRecentPendingFractionStaysConsistent(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Pizza", 400, 800, 30, true) }
	p := decode[LogResponse](t, h.logText("b0000004-0001", "pizza"))
	h.vars.onPost = func(int, map[string]any) (bool, int) { return false, 500 } // the half stays pending
	h.mutate("fraction", "b0000004-0002", p.Items[0].ItemID, f64(0.5))
	it := h.recent("").Items[0]
	if *it.PortionG != 400 || it.Macros.Kcal.float() != 800 {
		t.Fatalf("pending fraction: portion %v kcal %v", *it.PortionG, it.Macros.Kcal.float())
	}
}

func TestRecentKeepsZeroKcalItems(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[{"item":"Sparkling water","staple_key":null,"portion_g":500,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":0,"needs_fraction":false}],"text":"","widgets":[]}`
	}
	h.logText("b0000005-0001", "water")
	got := h.recent("")
	if len(got.Items) != 1 || got.Items[0].Item != "Sparkling water" || *got.Items[0].PortionG != 500 {
		t.Fatalf("%+v", got.Items)
	}
	if r := h.relog("b0000005-0002", got.Items[0].Key, nil); r.Code != 200 {
		t.Fatalf("relog zero kcal: %d %s", r.Code, r.Body)
	}
}

func TestRecentAndRelogFailWhenTheWindowCannotBeRead(t *testing.T) {
	h := newHarness(t)
	h.logText("b0000006-0001", "250 g skyr and 30 g walnuts")
	key := h.recent("").Items[0].Key
	h.vars.failReads.Store(true)
	h.restart()
	if r := h.do("GET", "/fuel/recent", nil, ""); r.Code != 502 || errCode(t, r) != "upstream_failed" {
		t.Fatalf("recent %d %s", r.Code, r.Body)
	}
	if r := h.relog("b0000006-0002", key, nil); r.Code != 502 {
		t.Fatalf("relog %d %s", r.Code, r.Body)
	}
}

func TestRelogRefusesUnknownRequiredMacros(t *testing.T) {
	h := newHarness(t)
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "Mystery stew", "portion_g": 300.0, "kcal": 400.0, "carbs_g": 30.0, "fat_g": 10.0, "sat_fat_g": 3.0, "fiber_g": nil, "source": "agentd", "op_id": "op_ag_x", "eaten_at": "2026-10-01T11:00:00+02:00"})
	h.clk.Add(2 * time.Minute)
	got := h.recent("")
	if len(got.Items) != 1 || got.Items[0].Macros.Protein.OK {
		t.Fatalf("%+v", got.Items)
	}
	if r := h.relog("b0000007-0001", got.Items[0].Key, nil); r.Code != 400 || h.vars.posts != 0 {
		t.Fatalf("relog with unknown protein: %d posts %d", r.Code, h.vars.posts)
	}
}

func TestRelogSharesTheLogSlots(t *testing.T) {
	h := newHarness(t)
	h.logText("b0000008-0001", "250 g skyr and 30 g walnuts")
	key := h.recent("").Items[0].Key
	h.svc.slots <- struct{}{}
	h.svc.slots <- struct{}{}
	r := h.relog("b0000008-0002", key, nil)
	<-h.svc.slots
	<-h.svc.slots
	if r.Code != 503 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", r.Code)
	}
}

// GET /fuel/frequent: variants of one food and of one combination take ONE
// slot (Joe, 2026-10-04), the slot shows the most frequent form, and the
// items of a combination are the items of a meal.
func TestFrequentMergesVariantsAndKeepsCombinations(t *testing.T) {
	names := func(m FrequentMeal) string {
		var n []string
		for _, it := range m.Items {
			n = append(n, it.Item)
		}
		sort.Strings(n)
		return strings.Join(n, "+")
	}
	at := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	var items []RecentItem
	var meals []mealOcc
	add := func(mins int, foods ...string) {
		m := mealOcc{at: at.Add(time.Duration(mins) * time.Minute)}
		for _, f := range foods {
			k := recentKey("food", f, nil, nil)
			m.keys = append(m.keys, k)
			m.names = append(m.names, f)
			found := false
			for _, it := range items {
				found = found || it.Key == k
			}
			if !found {
				items = append(items, RecentItem{Key: k, Item: f})
			}
		}
		meals = append(meals, m)
	}
	// Water with lime: the combination 3 times (three names of the juice are
	// ONE form), sparkling water 3 times, the can once = 7 logs, one slot.
	// Equal counts: the form logged last wins the slot.
	add(1, "water", "lime juice, half a lime squeezed")
	add(2, "water", "lime juice, freshly squeezed")
	add(3, "sparkling water")
	add(4, "sparkling water")
	add(5, "sparkling water")
	add(6, "water", "Lime juice, fresh")
	add(7, "lime sparkling water (can)")
	// Coffee in two forms: 2 + 1.
	add(8, "Bialetti moka coffee, black")
	add(9, "Bialetti moka coffee, black")
	add(10, "filter coffee, black")
	// Two other foods, once each; juice alone is no family word.
	add(11, "orange juice")
	add(12, "Migros cottage cheese nature")

	got := frequentMeals(items, meals, 5)
	var lines []string
	for _, m := range got {
		lines = append(lines, fmt.Sprintf("%s x%d", names(m), m.Times))
	}
	want := []string{"Lime juice, fresh+water x7", "Bialetti moka coffee, black x3", "Migros cottage cheese nature x1", "orange juice x1"}
	if strings.Join(lines, "; ") != strings.Join(want, "; ") {
		t.Fatalf("frequent:\n got %v\nwant %v", lines, want)
	}
	// With the combination as the most frequent form, the slot holds both items.
	add(13, "water", "lime juice, half a lime squeezed")
	add(14, "water", "lime juice, half a lime squeezed")
	got = frequentMeals(items, meals, 5)
	if n := names(got[0]); n != "lime juice, half a lime squeezed+water" || got[0].Times != 9 || len(got[0].Items) != 2 {
		t.Fatalf("first slot %q x%d", n, got[0].Times)
	}
	if got[0].Key == "" || got[0].Key == got[1].Key {
		t.Fatalf("keys %q %q", got[0].Key, got[1].Key)
	}
	if len(frequentMeals(items, meals, 2)) != 2 {
		t.Fatal("limit")
	}
}

func TestFrequentRouteGroupsARepeatedMeal(t *testing.T) {
	h := newHarness(t)
	two := `{"intent":"log","items":[{"item":"Water","staple_key":null,"portion_g":500,"portion_basis":"stated","kcal":0,"protein_g":0,"carbs_g":0,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":null,"needs_fraction":false},{"item":"Lime juice, fresh","staple_key":null,"portion_g":20,"portion_basis":"stated","kcal":5,"protein_g":0,"carbs_g":1,"net_carbs_g":null,"fat_g":0,"sat_fat_g":0,"fiber_g":null,"needs_fraction":false}],"text":"","widgets":[]}`
	h.model.fn = func(ModelInput) string { return two }
	h.clk.Add(time.Minute)
	decode[LogResponse](t, h.logText("c0000001-0001", "water with lime"))
	h.model.fn = func(ModelInput) string { return oneItem("Skyr", 250, 160, 27, false) }
	h.clk.Add(time.Minute)
	decode[LogResponse](t, h.logText("c0000001-0002", "skyr"))
	// "Repeat meal": one re-log per item, both with ONE local_time.
	h.clk.Add(time.Minute)
	stamp := h.clk.Now().Format(time.RFC3339)
	for i, it := range h.recent("").Items {
		if it.Item == "Skyr" {
			continue
		}
		body := fmt.Sprintf(`{"client_id":"c0000001-01%02d","key":%q,"local_time":%q}`, i, it.Key, stamp)
		if rec := h.do("POST", "/fuel/relog", strings.NewReader(body), "application/json"); rec.Code != 200 {
			t.Fatalf("relog %s: %d %s", it.Item, rec.Code, rec.Body.String())
		}
	}
	h.clk.Add(2 * time.Minute)
	type resp struct {
		Items []FrequentMeal `json:"items"`
	}
	got := decode[resp](t, h.do("GET", "/fuel/frequent", nil, ""))
	if len(got.Items) != 2 || got.Items[0].Times != 2 || len(got.Items[0].Items) != 2 || got.Items[1].Items[0].Item != "Skyr" {
		t.Fatalf("frequent %+v", got.Items)
	}
	if rec := h.do("GET", "/fuel/frequent?limit=11", nil, ""); rec.Code != 400 {
		t.Fatalf("limit 11: %d", rec.Code)
	}
}
