package fuel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "That sounds like angina; see a cardiologist. Your pressure of 150 is high."

func clinicalOut(intent, items string) string {
	return `{"intent":"` + intent + `","items":[` + items + `],"corrections":[],"targets":[],"text":"` + secret + `","widgets":["macros_today","week"],"clinical_topic":true}`
}

func textBlocks(bs []Block) []string {
	var out []string
	for _, b := range bs {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// T11 (chat guard), with the fake model.
func TestT11ChatGuard(t *testing.T) {
	h := newV7(t, v2())
	posts := h.vars.posts
	// clinical_topic true and no items: no write, intent question, the fixed
	// line is the only text block, the model text is nowhere.
	for _, intent := range []string{"question", "log"} {
		h.model.fn = func(ModelInput) string { return clinicalOut(intent, "") }
		recSeq++
		rec := h.logText(cid(), "my chest hurt on the run today")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", intent, rec.Code, rec.Body)
		}
		r := decode[LogResponse](t, rec)
		tb := textBlocks(r.Blocks)
		if r.Intent != "question" || len(r.Items) != 0 || len(tb) != 1 || tb[0] != clinicalLine || len(r.Blocks) != 1 {
			t.Errorf("%s: intent %s blocks %+v", intent, r.Intent, r.Blocks)
		}
		if strings.Contains(rec.Body.String(), "angina") || strings.Contains(rec.Body.String(), "cardiologist") {
			t.Errorf("%s: the model text is in the response", intent)
		}
	}
	if h.vars.posts != posts {
		t.Error("a clinical question wrote to Variables")
	}
	// The feed stores the user turn as typed and the fixed reply; nothing else.
	fb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "feed.jsonl"))
	if !strings.Contains(string(fb), "my chest hurt on the run today") || !strings.Contains(string(fb), clinicalLine) || strings.Contains(string(fb), "angina") {
		t.Errorf("feed: %s", fb)
	}
	jb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "journal.jsonl"))
	if strings.Contains(string(jb), "angina") {
		t.Error("the model text is in the journal")
	}
	// clinical_topic true with one food item: the item is logged; the only
	// text block is the fixed line (no status line, spec 22.12).
	skyr := `{"item":"skyr","staple_key":null,"portion_g":200,"portion_basis":"stated","kcal":126,"protein_g":22,"carbs_g":8,"net_carbs_g":null,"fat_g":0.4,"sat_fat_g":0.2,"fiber_g":0,"needs_fraction":false}`
	h.model.fn = func(ModelInput) string { return clinicalOut("log", skyr) }
	rec := h.logText(cid(), "had 200 g skyr, and my knee hurts")
	r := decode[LogResponse](t, rec)
	tb := textBlocks(r.Blocks)
	if rec.Code != 200 || r.Intent != "log" || len(r.Items) != 1 || len(tb) != 1 || tb[0] != clinicalLine {
		t.Fatalf("log with a clinical topic: %d %+v", rec.Code, r.Blocks)
	}
	if strings.Contains(rec.Body.String(), "angina") || len(h.vars.rows("var-food")) != 1 {
		t.Errorf("the model text is shown, or the skyr was not logged (%d rows)", len(h.vars.rows("var-food")))
	}
	// The entry replays the same blocks (stored final answer and a rebuild).
	e := decode[entryWire](t, h.get("/fuel/entry/"+r.EntryID))
	_ = e
	feed := h.get("/fuel/feed").Body.String()
	if strings.Contains(feed, "angina") || !strings.Contains(feed, clinicalLine) {
		t.Errorf("feed route: %s", feed)
	}
	// No coach event carries the clinical words.
	cb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "coach-events.jsonl"))
	if strings.Contains(string(cb), "knee") || strings.Contains(string(cb), "angina") {
		t.Errorf("coach events: %s", cb)
	}
	// A correction with a clinical topic: processed, and the fixed line follows.
	h.model.fn = func(ModelInput) string {
		return `{"intent":"correct","items":[],"corrections":[` + corrForm("last", `"portion_g":100`) + `],"targets":[],"text":"` + secret + `","widgets":[],"clinical_topic":true}`
	}
	rc := decode[LogResponse](t, h.logText(cid(), "that was 100 g, and I feel dizzy"))
	tb = textBlocks(rc.Blocks)
	if rc.Intent != "correct" || len(tb) != 2 || tb[0] != "Corrected skyr to 100 g." || tb[1] != clinicalLine {
		t.Errorf("correction with a clinical topic: %+v", tb)
	}
	// After a restart the stored answers are the same (nothing is rebuilt
	// from the model text).
	h.restart()
	if got := decode[LogResponse](t, h.get("/fuel/entry/"+r.EntryID)); strings.Contains(string(mustMarshal(got)), "angina") {
		t.Error("the model text came back after a restart")
	}
	// A missing clinical_topic is invalid output (one retry, then 502).
	h.model.raw = true
	h.model.fn = func(ModelInput) string { return skyrWalnuts }
	calls := h.model.calls
	if rec := h.logText(cid(), "skyr and walnuts"); rec.Code != 502 || errCode(t, rec) != "model_invalid" || h.model.calls != calls+2 {
		t.Errorf("missing clinical_topic: %d %s (%d calls)", rec.Code, rec.Body, h.model.calls-calls)
	}
	for _, bad := range []string{`"clinical_topic":null`, `"clinical_topic":"no"`} {
		raw := strings.Replace(withClinical(skyrWalnuts), `"clinical_topic":false`, bad, 1)
		if _, err := validateOutput(json.RawMessage(raw)); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	// The model never sees a record or a clinician text: its snapshot is the
	// v6 view.
	h2 := newV7(t, v2("clinician.hard_sets", map[string]any{"text": "Not to failure.", "set_by": "Dr. X", "set_on": "2026-10-01"}), recOpts)
	h2.record("blood_pressure", bp(150, 95), "")
	var seen string
	h2.model.fn = func(in ModelInput) string {
		for _, p := range userContent(in) {
			if s, ok := p["text"].(string); ok {
				seen += s
			}
		}
		seen += systemFor(in)
		return `{"intent":"question","items":[],"text":"fine","widgets":[]}`
	}
	h2.logText(cid(), "how am I doing?")
	for _, bad := range []string{"systolic", "blood_pressure", "Not to failure", "records", "clinician\":"} {
		if strings.Contains(seen, bad) {
			t.Errorf("the model input holds %q", bad)
		}
	}
	if !strings.Contains(seen, `"macros"`) || !strings.Contains(seen, `"streaks"`) {
		t.Error("the model input lost the v6 snapshot")
	}
}

// T2, second part: with a schema 2 file with an active carbohydrate floor and
// energy state formula, the v6 surfaces keep their v6 shape.
func TestT2LegacyShapeUnderSchemaTwo(t *testing.T) {
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	doc := v2(append(carbsOn(), "energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0)...)
	_, order, docs := goldenFixture(t, doc)
	v6Widgets := map[string]bool{}
	for _, w := range widgetNames {
		v6Widgets[w] = true
	}
	for _, name := range order {
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if na, ok := x["next_action"].(map[string]any); ok && na["key"] == "carbs_g" {
					t.Errorf("%s: next_action.key is carbs_g", name)
				}
				if ms, ok := x["macros"].([]any); ok {
					if _, isSnap := x["day_type"]; isSnap {
						var keys []string
						for _, m := range ms {
							keys = append(keys, m.(map[string]any)["key"].(string))
						}
						if strings.Join(keys, ",") != "protein_g,sat_fat_g,fiber_g,kcal,net_carbs_g" {
							t.Errorf("%s: macros keys %v", name, keys)
						}
					}
				}
				if x["type"] == "widget" && !v6Widgets[x["widget"].(string)] {
					t.Errorf("%s: widget %v is not a v6 widget", name, x["widget"])
				}
				for _, c := range x {
					walk(c)
				}
			case []any:
				for _, c := range x {
					walk(c)
				}
			}
		}
		var v any
		if err := json.Unmarshal(docs[name], &v); err != nil {
			t.Fatal(err)
		}
		walk(v)
	}
	var s Snapshot
	_ = json.Unmarshal(docs["snapshot"], &s)
	if num(macro(s, "net_carbs_g").Target) != "null" || s.DayScore.Of != 4 || s.Energy.State != "formula" || num(macro(s, "kcal").Target) != "3320" {
		t.Errorf("schema 2 snapshot: net carbs %s, of %d, energy %s %s", num(macro(s, "net_carbs_g").Target), s.DayScore.Of, s.Energy.State, num(macro(s, "kcal").Target))
	}
	for _, m := range s.Intake {
		if m.Key == "fluids_ml" && m.Target != nil {
			t.Error("fluids target must be null under schema 2")
		}
	}
}

// T15 (/fuel/day): the build 7 gap.
func TestT15DayItems(t *testing.T) {
	h := newRecalHarness(t, func(o *Options) {
		if err := writeFile(o.TargetsFile, v2()); err != nil {
			t.Fatal(err)
		}
	})
	// A photo item with a check flag (implausible numbers) and levers.
	item := `{"item":"pasta","kind":"food","staple_key":null,"portion_g":200,"portion_basis":"photo_estimate","kcal":300,"protein_g":10,"carbs_g":40,"net_carbs_g":null,"fat_g":10,"sat_fat_g":2,"fiber_g":3,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"levers":` + lv(0, 0, 20, 0, 5, nil) + `}`
	odd := `{"item":"leaves","kind":"food","staple_key":null,"portion_g":50,"portion_basis":"photo_estimate","kcal":200,"protein_g":1,"carbs_g":2,"net_carbs_g":null,"fat_g":0.2,"sat_fat_g":0,"fiber_g":1,"needs_fraction":false,"volume_ml":null,"caffeine_mg":null,"alcohol_g":null,"food_class":"leafy_vegetable","scale_g":null,"levers":` + lv(0, 0, 0, 0, 1, nil) + `}`
	resp := h.logPhoto("r1500000-0001", photoMeal(item, odd), 1)
	agentd := agentdRow(h.harness, "coffee", 300, 6, "2026-10-01T08:30:00+02:00", map[string]any{"caffeine_mg": 120.0})
	h.clk.Add(61 * 1e9)
	day := h.dayTyped()
	byName := map[string]DayItem{}
	for _, d := range day {
		byName[d.Item] = d
	}
	for i, st := range resp.Items {
		d := byName[st.Item]
		if d.ItemID == nil || *d.ItemID != st.ItemID || d.EntryID == nil || *d.EntryID != resp.EntryID || d.PortionBasis == nil || *d.PortionBasis != st.PortionBasis ||
			d.Check != st.Check || string(mustMarshal(d.Levers)) != string(mustMarshal(st.Levers)) {
			t.Errorf("item %d: day %+v, item state %+v", i, d, st)
		}
	}
	if byName["leaves"].Check == "" || byName["pasta"].Check != "" || num(byName["pasta"].Levers.Nuts) != "20" {
		t.Errorf("check and levers: %q %q %s", byName["leaves"].Check, byName["pasta"].Check, num(byName["pasta"].Levers.Nuts))
	}
	raw := h.get("/fuel/day").Body.String()
	if strings.Count(raw, `"check"`) != 1 {
		t.Errorf("the check key must be absent when the item is fine: %d", strings.Count(raw, `"check"`))
	}
	// A food-log row: item_id null and entry_id null, levers untagged.
	c := byName["coffee"]
	if c.RowKey != "v:"+agentd || c.ItemID != nil || c.EntryID != nil || c.PortionBasis != nil || c.Levers.Nuts != nil {
		t.Errorf("row of another writer: %+v", c)
	}
	if !strings.Contains(raw, `"item_id":null`) || !strings.Contains(raw, `"entry_id":null`) || !strings.Contains(raw, `"portion_basis":null`) {
		t.Error("the keys must be present with null for a row of another writer")
	}
	// A recalibrated item can be reverted with the ids taken from /fuel/day alone.
	pasta := byName["pasta"]
	h.say(answerOf(opinion(*pasta.ItemID, "pasta", 300, 480, 16, 2.4, "high", "r"), opinion(resp.Items[1].ItemID, "leaves", 50, 200, 1, 0, "high", "ok")))
	h.run()
	for _, d := range h.dayTyped() {
		if d.Item == "pasta" {
			pasta = d
		}
	}
	if pasta.Recalibration == nil || !pasta.Recalibration.CanRevert || *pasta.PortionBasis != "photo_estimate" {
		t.Fatalf("recalibrated day item: %+v", pasta.Recalibration)
	}
	if r := h.revert("r1500000-0002", *pasta.ItemID); r.Code != 200 {
		t.Fatalf("revert by the day ids: %d %s", r.Code, r.Body)
	}
	if e := h.entry(*pasta.EntryID); len(e.Items) != 2 || e.Items[0].Recalibration.State != "reverted" {
		t.Errorf("entry by the day entry_id: %+v", e.Items)
	}
	// After a fix the day amounts equal ItemState.effective; the basis stays.
	h.clk.Add(1e9)
	fx := decode[MutationResponse](t, h.fix("r1500000-0003", *pasta.ItemID, map[string]any{"portion_g": 100}))
	for _, d := range h.dayTyped() {
		if d.Item == "pasta" && (d.Macros != fx.Item.Effective || *d.PortionG != 100 || *d.PortionBasis != "photo_estimate" || num(d.Levers.Nuts) != "10") {
			t.Errorf("after a fix: day %+v, effective %+v", d, fx.Item.Effective)
		}
	}
}
