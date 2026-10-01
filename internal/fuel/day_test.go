package fuel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const removeSentence = "I actually had two entries just now for water. The first with the 250 ml, can you remove that one?"

type dayResp struct {
	Date     string           `json:"date"`
	Items    []map[string]any `json:"items"`
	Snapshot Snapshot         `json:"snapshot"`
}

func (h *harness) day(q string) dayResp {
	h.t.Helper()
	return decode[dayResp](h.t, h.do("GET", "/fuel/day"+q, nil, ""))
}

func undoOut(ref, which, ml, g string) string {
	return fmt.Sprintf(`{"intent":"undo","items":[],"corrections":[],"targets":[{"ref":%q,"which":%s,"volume_ml":%s,"portion_g":%s}],"text":"","widgets":[]}`, ref, which, ml, g)
}

// twoWaters leaves today's log with exactly two waters: a 250 ml RELOG at
// 09:12 local and a 500 ml log at 09:15.
func twoWaters(t *testing.T, h *harness) (relog, big LogResponse) {
	t.Helper()
	h.clk.Set(time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC))
	h.model.fn = func(ModelInput) string { return drink("water", 250, 0, 0, 0, false) }
	tpl := decode[LogResponse](t, h.logText("g0000001-0001", "250 ml water"))
	var key string
	for _, r := range h.svc.recentAll() {
		if r.Item == "water" {
			key = r.Key
		}
	}
	h.clk.Set(time.Date(2026, 10, 1, 7, 12, 0, 0, time.UTC)) // 09:12 Zurich
	relog = decode[LogResponse](t, h.relog("g0000001-0002", key, nil))
	h.mutate("undo", "g0000001-0003", tpl.Items[0].ItemID, nil)
	h.clk.Set(time.Date(2026, 10, 1, 7, 15, 0, 0, time.UTC))
	h.model.fn = func(ModelInput) string { return drink("water", 500, 0, 0, 0, false) }
	big = decode[LogResponse](t, h.logText("g0000001-0004", "500 ml water"))
	h.clk.Set(time.Date(2026, 10, 1, 7, 17, 0, 0, time.UTC))
	return
}

func TestRemoveSentenceUndoesTheFirst250(t *testing.T) {
	h := newHarness(t)
	relog, big := twoWaters(t, h)
	var seen []LastItem
	h.model.fn = func(in ModelInput) string { seen = in.LastItems; return undoOut("water", `"first"`, "250", "null") }
	r := h.logText("g0000001-0005", removeSentence)
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if resp.Intent != "undo" || resp.Blocks[0].Text != "Removed water, 250 ml (09:12)." {
		t.Fatalf("intent %s text %q", resp.Intent, resp.Blocks[0].Text)
	}
	if len(resp.Items) != 1 || resp.Items[0].ItemID != relog.Items[0].ItemID || !resp.Items[0].Undone {
		t.Fatalf("items %+v", resp.Items)
	}
	rows := h.vars.rows("var-food")
	last := rows[len(rows)-1]
	if last["reason"] != "undo" || last["corrects"] != relog.Items[0].ItemID || last["volume_ml"] != -250.0 {
		t.Fatalf("undo row %v", last)
	}
	// Never a correction; the 500 ml stays; fluids = 500.
	for _, r := range rows {
		if r["reason"] == "fix" {
			t.Fatal("a removal wrote a fix")
		}
	}
	d := h.day("")
	if len(d.Items) != 1 || d.Items[0]["row_key"] != big.Items[0].ItemID || intake(d.Snapshot, "fluids_ml").Consumed != 500 {
		t.Fatalf("day %+v", d.Items)
	}
	var at bool
	for _, li := range seen {
		at = at || li.At == "09:12"
	}
	if !at {
		t.Fatalf("model did not see times: %+v", seen)
	}
}

func TestRemoveAmbiguousAsksAndWritesNothing(t *testing.T) {
	h := newHarness(t)
	twoWaters(t, h)
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return undoOut("water", "null", "null", "null") }
	resp := decode[LogResponse](t, h.logText("g0000002-0001", "remove the water"))
	if len(resp.Items) != 0 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("wrote on an ambiguous removal: %+v", resp.Items)
	}
	if !strings.HasPrefix(resp.Blocks[0].Text, "Which one do you mean: water, 250 ml (09:12) or water, 500 ml (09:15)?") {
		t.Fatalf("text %q", resp.Blocks[0].Text)
	}
	// "the 500 ml one" is unique: removed. "last" without amount = newest.
	h.model.fn = func(ModelInput) string { return undoOut("water", "null", "500", "null") }
	r2 := decode[LogResponse](t, h.logText("g0000002-0002", "remove the 500 ml water"))
	if r2.Blocks[0].Text != "Removed water, 500 ml (09:15)." {
		t.Fatalf("text %q", r2.Blocks[0].Text)
	}
	// Nothing matching: a note, no write.
	n = len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return undoOut("banana", "null", "null", "null") }
	r3 := decode[LogResponse](t, h.logText("g0000002-0003", "remove the banana"))
	if len(h.vars.rows("var-food")) != n || !strings.Contains(r3.Blocks[0].Text, "could not find banana") {
		t.Fatalf("text %q", r3.Blocks[0].Text)
	}
}

func TestUndoIntentValidation(t *testing.T) {
	bad := []string{
		`{"intent":"undo","items":[],"corrections":[],"targets":[],"text":"","widgets":[]}`,
		`{"intent":"undo","items":[],"corrections":[],"targets":[{"ref":"x","which":"second","volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`,
		`{"intent":"correct","items":[],"corrections":[{"ref":"last","portion_g":1,"volume_ml":null,"share":null}],"targets":[{"ref":"x","which":null,"volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`,
	}
	for i, b := range bad {
		if _, err := validateOutput(json.RawMessage(b)); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if !strings.Contains(systemPrompt, "A removal is NEVER a correction") {
		t.Fatal("prompt lacks the removal rule")
	}
}

func agentdRow(h *harness, item string, ml, kcal float64, at string, extra map[string]any) string {
	d := map[string]any{"item": item, "kind": "drink", "volume_ml": ml, "kcal": kcal, "protein_g": 0.0, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "source": "agentd", "op_id": "op_ag_" + item + at, "eaten_at": at}
	for k, v := range extra {
		d[k] = v
	}
	h.vars.add("var-food", "2026-10-01", d)
	rows := h.vars.rows("var-food")
	return rows[len(rows)-1]["_id"].(string)
}

func TestDayListsAllSourcesNewestFirst(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oneItem("Rice", 200, 260, 5, false) }
	rice := decode[LogResponse](t, h.logText("g0000003-0001", "rice")).Items[0] // 12:00 local
	h.model.fn = func(ModelInput) string { return oneItem("Banana", 120, 105, 1, false) }
	ban := decode[LogResponse](t, h.logText("g0000003-0002", "banana")).Items[0]
	h.mutate("undo", "g0000003-0003", ban.ItemID, nil)
	h.fix("g0000003-0004", rice.ItemID, map[string]any{"portion_g": 100})
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", map[string]any{"caffeine_mg": 110.0})
	h.clk.Add(2 * time.Minute)
	d := h.day("")
	if d.Date != "2026-10-01" || len(d.Items) != 2 {
		t.Fatalf("day %+v", d.Items)
	}
	r, c := d.Items[0], d.Items[1]
	if r["row_key"] != rice.ItemID || r["portion_g"] != 100.0 || r["source"] != "fuel" || r["recent_key"] != recentKey("food", "Rice", f64(100), nil) {
		t.Fatalf("rice %v", r)
	}
	if c["row_key"] != "v:"+cof || c["source"] != "agentd" || c["caffeine_mg"] != 110.0 || c["volume_ml"] != 300.0 || c["kind"] != "drink" {
		t.Fatalf("coffee %v", c)
	}
	acts, _ := c["actions"].([]any)
	if len(acts) != 3 || c["eaten_at"] != "2026-10-01T06:47:13Z" {
		t.Fatalf("coffee actions/time %v %v", acts, c["eaten_at"])
	}
	if r := h.do("GET", "/fuel/day?date=2026-08-01", nil, ""); r.Code != 400 {
		t.Fatal(r.Code)
	}
}

func TestUndoAndFixByExternalRowKey(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", map[string]any{"caffeine_mg": 110.0})
	h.clk.Add(2 * time.Minute)
	key := "v:" + cof
	fixB, _ := json.Marshal(map[string]any{"client_id": "g0000004-0001", "row_key": key, "volume_ml": 200})
	fr := h.do("POST", "/fuel/fix", bytes.NewReader(fixB), "application/json")
	if fr.Code != 200 {
		t.Fatalf("fix %d %s", fr.Code, fr.Body)
	}
	rows := h.vars.rows("var-food")
	fix := rows[len(rows)-1]
	if fix["corrects"] != cof || fix["reason"] != "fix" || fix["volume_ml"] != -100.0 || fix["volume_ml_after"] != 200.0 || !strings.HasPrefix(fix["op_id"].(string), "op_fix_"+cof) {
		t.Fatalf("fix row %v", fix)
	}
	if _, has := fix["entry_id"]; has {
		t.Fatal("external correction carries a Fuel entry_id")
	}
	d := h.day("")
	if d.Items[0]["volume_ml"] != 200.0 || d.Items[0]["caffeine_mg"] != 73.3 {
		t.Fatalf("after fix %v", d.Items[0])
	}
	undoB, _ := json.Marshal(map[string]any{"client_id": "g0000004-0002", "row_key": key})
	ur := h.do("POST", "/fuel/undo", bytes.NewReader(undoB), "application/json")
	if ur.Code != 200 {
		t.Fatalf("undo %d %s", ur.Code, ur.Body)
	}
	rows = h.vars.rows("var-food")
	un := rows[len(rows)-1]
	if un["op_id"] != "op_undo_"+cof || un["corrects"] != cof || un["volume_ml"] != -200.0 || un["caffeine_mg"] != -73.3 {
		t.Fatalf("undo row %v", un)
	}
	n := len(rows)
	// Retry with the same client_id: no new row. A new client_id: 409.
	if r := h.do("POST", "/fuel/undo", bytes.NewReader(undoB), "application/json"); r.Code != 200 || len(h.vars.rows("var-food")) != n {
		t.Fatalf("retry %d", r.Code)
	}
	b2, _ := json.Marshal(map[string]any{"client_id": "g0000004-0003", "row_key": key})
	if r := h.do("POST", "/fuel/undo", bytes.NewReader(b2), "application/json"); r.Code != 409 {
		t.Fatalf("second undo %d", r.Code)
	}
	if len(h.day("").Items) != 0 {
		t.Fatal("undone row still listed")
	}
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if intake(snap, "fluids_ml").Consumed != 0 || intake(snap, "caffeine_mg").Consumed != 0 {
		t.Fatalf("intake %+v", snap.Intake)
	}
	// A food-log undo of the same row (it also negates the CURRENT
	// contribution and uses the same deterministic op_id) counts once.
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: filter coffee", "kcal": -1.3, "protein_g": 0.0, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0, "volume_ml": -200.0, "caffeine_mg": -73.3, "source": "agentd", "reason": "undo", "corrects": cof, "op_id": "op_undo_" + cof})
	h.clk.Add(2 * time.Minute)
	snap = decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if intake(snap, "fluids_ml").Consumed != 0 {
		t.Fatalf("duplicate undo counted twice: %v", intake(snap, "fluids_ml").Consumed)
	}
	for _, bad := range []map[string]any{{"client_id": "g0000004-0004", "row_key": "v:nope"}, {"client_id": "g0000004-0005", "row_key": key, "item_id": "x"}} {
		b, _ := json.Marshal(bad)
		if r := h.do("POST", "/fuel/undo", bytes.NewReader(b), "application/json"); r.Code != 404 && r.Code != 400 {
			t.Fatalf("%v: %d", bad, r.Code)
		}
	}
}

func TestChatRemovesATelegramRow(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", nil)
	h.clk.Add(2 * time.Minute)
	h.model.fn = func(ModelInput) string { return undoOut("filter coffee", "null", "null", "null") }
	r := decode[LogResponse](t, h.logText("g0000005-0001", "delete the coffee"))
	if r.Blocks[0].Text != "Removed filter coffee, 300 ml (08:47)." {
		t.Fatalf("%q", r.Blocks[0].Text)
	}
	rows := h.vars.rows("var-food")
	if rows[len(rows)-1]["op_id"] != "op_undo_"+cof {
		t.Fatalf("%v", rows[len(rows)-1])
	}
	h.restart()
	if rep := decode[LogResponse](t, h.logText("g0000005-0001", "delete the coffee")); rep.Blocks[0].Text != r.Blocks[0].Text {
		t.Fatal("replay differs")
	}
}

func TestExternalUndoKeepsItsWireOpIDAfterARejection(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", nil)
	h.clk.Add(2 * time.Minute)
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, d map[string]any) (bool, int) { return false, 400 }
	h.vars.mu.Unlock()
	b1, _ := json.Marshal(map[string]any{"client_id": "g1000001-0001", "row_key": "v:" + cof})
	if r := h.do("POST", "/fuel/undo", bytes.NewReader(b1), "application/json"); r.Code != 502 {
		t.Fatalf("first %d", r.Code)
	}
	h.vars.mu.Lock()
	h.vars.onPost = nil
	h.vars.mu.Unlock()
	b2, _ := json.Marshal(map[string]any{"client_id": "g1000001-0002", "row_key": "v:" + cof})
	if r := h.do("POST", "/fuel/undo", bytes.NewReader(b2), "application/json"); r.Code != 200 {
		t.Fatalf("second %d %s", r.Code, r.Body)
	}
	rows := h.vars.rows("var-food")
	if rows[len(rows)-1]["op_id"] != "op_undo_"+cof {
		t.Fatalf("wire op_id changed: %v", rows[len(rows)-1]["op_id"])
	}
}

func TestRemovalSeesARowAddedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return drink("water", 500, 0, 0, 0, false) }
	h.logText("g1000002-0001", "water")
	// The food-log adds another water; the cache has not seen it.
	agentdRow(h, "water", 250, 0, "2026-10-01T12:01:00+02:00", nil)
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string { return undoOut("water", "null", "null", "null") }
	r := decode[LogResponse](t, h.logText("g1000002-0002", "remove the water"))
	if len(h.vars.rows("var-food")) != n || !strings.HasPrefix(r.Blocks[0].Text, "Which one do you mean") {
		t.Fatalf("text %q", r.Blocks[0].Text)
	}
}

func TestMixedRemovalWritesNothingWhenOneTargetIsAmbiguous(t *testing.T) {
	h := newHarness(t)
	twoWaters(t, h)
	h.model.fn = func(ModelInput) string { return oneItem("banana", 120, 105, 1, false) }
	h.logText("g1000003-0001", "banana")
	n := len(h.vars.rows("var-food"))
	h.model.fn = func(ModelInput) string {
		return `{"intent":"undo","items":[],"corrections":[],"targets":[{"ref":"banana","which":null,"volume_ml":null,"portion_g":null},{"ref":"water","which":null,"volume_ml":null,"portion_g":null}],"text":"","widgets":[]}`
	}
	r := decode[LogResponse](t, h.logText("g1000003-0002", "remove the banana and the water"))
	if len(h.vars.rows("var-food")) != n || len(r.Items) != 0 {
		t.Fatalf("partial removal written: %+v", r.Items)
	}
	if !strings.Contains(r.Blocks[0].Text, "Which one do you mean") || !strings.Contains(r.Blocks[0].Text, "Nothing was removed.") || strings.Contains(r.Blocks[0].Text, "Removed banana") {
		t.Fatalf("text %q", r.Blocks[0].Text)
	}
}

func TestExternalNoOpFixReplaysAfterRestart(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", nil)
	h.clk.Add(2 * time.Minute)
	b, _ := json.Marshal(map[string]any{"client_id": "g1000004-0001", "row_key": "v:" + cof, "volume_ml": 300})
	if r := h.do("POST", "/fuel/fix", bytes.NewReader(b), "application/json"); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	h.svc.Close()
	_ = removeFile(h, "idem.jsonl")
	h.start()
	if r := h.do("POST", "/fuel/fix", bytes.NewReader(b), "application/json"); r.Code != 200 {
		t.Fatalf("replay after restart %d %s", r.Code, r.Body)
	}
}

func removeFile(h *harness, name string) error {
	return os.Remove(h.opts.StateDir + "/" + name)
}

func TestExternalFixSequenceBackToAnEarlierAmount(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T08:47:13+02:00", nil)
	h.clk.Add(2 * time.Minute)
	for i, ml := range []float64{200, 100, 200} {
		b, _ := json.Marshal(map[string]any{"client_id": fmt.Sprintf("g1000005-%04d", i), "row_key": "v:" + cof, "volume_ml": ml})
		if r := h.do("POST", "/fuel/fix", bytes.NewReader(b), "application/json"); r.Code != 200 {
			t.Fatalf("fix %v: %d %s", ml, r.Code, r.Body)
		}
	}
	h.clk.Add(2 * time.Minute)
	if got := h.day("").Items[0]["volume_ml"]; got != 200.0 {
		t.Fatalf("volume %v after 200 -> 100 -> 200", got)
	}
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if intake(snap, "fluids_ml").Consumed != 200 {
		t.Fatalf("fluids %v", intake(snap, "fluids_ml").Consumed)
	}
}

func TestRowKeyOfARowWrittenSecondsAgo(t *testing.T) {
	h := newHarness(t)
	cof := agentdRow(h, "filter coffee", 300, 2, "2026-10-01T11:59:00+02:00", nil) // no clock advance: cache is fresh
	b, _ := json.Marshal(map[string]any{"client_id": "g1000006-0001", "row_key": "v:" + cof})
	if r := h.do("POST", "/fuel/undo", bytes.NewReader(b), "application/json"); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}
