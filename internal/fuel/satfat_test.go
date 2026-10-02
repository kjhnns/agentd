package fuel

import (
	"strings"
	"testing"
	"time"
)

func satDoc(kv ...any) string {
	return weekDoc(append([]any{"sat_fat_g", map[string]any{"kind": "budget", "energy_frac": 0.07}}, kv...)...)
}

// T20 (saturated fat budget, spec 18.14).
func TestT20SaturatedFatBudget(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	tg := parseT(t, satDoc())
	// 7 % of 2600 / 9 = 20.2 = 20 g; of 3000 = 23.3 = 23 g.
	if p := planDay(tg, "2026-10-01", "2026-10-01", nil, fresh, now); num(p.SatFatTarget) != "20" || *p.SatFatBasis != "7 % of 2600 kcal" {
		t.Errorf("rest day: %s %v", num(p.SatFatTarget), p.SatFatBasis)
	}
	run := []Activity{{SportType: "Run", MovingS: 3000, DistanceM: 20000, LocalDate: "2026-10-01"}}
	if p := planDay(tg, "2026-10-01", "2026-10-01", run, fresh, now); num(p.SatFatTarget) != "23" {
		t.Errorf("training day: %s", num(p.SatFatTarget))
	}
	// State formula with a 4120 kcal day = 32 g.
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	f := parseT(t, satDoc("energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0))
	if p := planDay(f, "2026-10-01", "2026-10-01", run, fresh, now); num(p.EnergyTarget) != "4120" || num(p.SatFatTarget) != "32" {
		t.Errorf("formula: %s %s", num(p.EnergyTarget), num(p.SatFatTarget))
	}
	// Invalid forms.
	for name, doc := range map[string]string{
		"energy_frac 0.3":            v2("sat_fat_g", map[string]any{"kind": "budget", "energy_frac": 0.3}),
		"value next to energy_frac":  v2("sat_fat_g", map[string]any{"kind": "budget", "energy_frac": 0.07, "value": 20}),
		"budget without energy_frac": v2("sat_fat_g", map[string]any{"kind": "budget"}),
		"cap with energy_frac":       v2("sat_fat_g", map[string]any{"kind": "cap", "energy_frac": 0.07}),
		"budget on protein":          v2("protein_g", map[string]any{"kind": "budget", "energy_frac": 0.07}),
		"budget in a schema 1 file":  strings.Replace(goldenTargetsV1, `"sat_fat_g": {"kind": "cap", "value": 20}`, `"sat_fat_g": {"kind": "budget", "energy_frac": 0.07}`, 1),
	} {
		if _, err := ParseTargets([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The cap forms keep their behaviour in both schemas.
	for _, doc := range []string{goldenTargetsV1, v2()} {
		c := parseT(t, doc)
		for _, acts := range [][]Activity{nil, run} {
			if p := planDay(c, "2026-10-01", "2026-10-01", acts, fresh, now); num(p.SatFatTarget) != "20" {
				t.Errorf("fixed cap: %s", num(p.SatFatTarget))
			}
		}
	}
	split := parseT(t, v2("sat_fat_g", map[string]any{"kind": "cap", "rest": 18, "training": 24}))
	if a, b := planDay(split, "2026-10-01", "2026-10-01", nil, fresh, now), planDay(split, "2026-10-01", "2026-10-01", run, fresh, now); num(a.SatFatTarget) != "18" || num(b.SatFatTarget) != "24" {
		t.Errorf("rest / training cap: %s %s", num(a.SatFatTarget), num(b.SatFatTarget))
	}
	if s := newV7(t, v2()).snap(t, ""); budget(s, "sat_fat_g").Kind != "cap" || budget(s, "sat_fat_g").Basis != nil {
		t.Errorf("cap form in budgets: %+v", budget(s, "sat_fat_g"))
	}

	// The wire, on a training day (budget 23 g).
	h := newV7(t, satDoc())
	h.strava("2026-10-01-run", "Run", 3000, 10000, "2026-10-01T08:00:00", h.clk.Now())
	// Yesterday (a rest day, budget 20): 20 g = the streak day passes on ITS budget.
	h.food("2026-09-30", "y", 2600, 170, 20, 40.0, nil)
	h.restart()
	step := func(g float64, wantLegacy, wantBudget string, met bool) {
		t.Helper()
		h.food("2026-10-01", "fat", 100, 5, g, 1.0, nil)
		h.clk.Add(61 * time.Second)
		s := h.snap(t, "")
		m, b := macro(s, "sat_fat_g"), budget(s, "sat_fat_g")
		if m.Kind != "cap" || num(m.Target) != "23" || m.Status != wantLegacy || m.PaceTargetNow != nil {
			t.Errorf("macros at %g: %+v", m.Consumed, m)
		}
		if b.Kind != "budget" || num(b.Target) != "23" || b.Status != wantBudget || b.Basis == nil || *b.Basis != "7 % of 3000 kcal" {
			t.Errorf("budgets at %g: %+v", b.Consumed, b)
		}
		// Protein, fibre and kcal are not met in this fixture; sat fat is the only check that can pass.
		if (s.BudgetScore.Hit == 1) != met || (s.DayScore.Hit == 1) != met {
			t.Errorf("at %g: budget_score %+v day_score %+v, met %v", b.Consumed, s.BudgetScore, s.DayScore, met)
		}
		if wantStreak := map[bool]int{true: 2, false: 1}[met]; s.Streaks.SatFat != wantStreak {
			t.Errorf("at %g: sat fat streak %d, want %d", b.Consumed, s.Streaks.SatFat, wantStreak)
		}
	}
	step(18.3, "on_pace", "on_pace", true)
	step(0.1, "on_pace", "near", true) // 18.4 = 80 % of 23
	step(4.6, "on_pace", "near", true) // 23.0: at the budget is met
	step(0.1, "over", "over", false)   // 23.1
	// The week: one training day (23) and six rest days (20) = 143.
	v := h.week(t, "")
	w := wb(v, "sat_fat_g")
	if num(w.Target) != "143" || w.Kind != "budget" || !w.TargetEstimated || num(cellOf(w, "2026-10-01").Target) != "23" || num(cellOf(w, "2026-09-30").Target) != "20" {
		t.Errorf("week: %+v", w)
	}
	if cellOf(w, "2026-09-30").Result != "met" || cellOf(w, "2026-10-01").Result != "over" {
		t.Errorf("week cells: %s %s", cellOf(w, "2026-09-30").Result, cellOf(w, "2026-10-01").Result)
	}
	if s := h.snap(t, ""); s.WeekBudgets == nil || s.WeekBudgets.Budgets[1].Kind != "budget" || num(s.WeekBudgets.Budgets[1].Target) != "143" {
		t.Errorf("week_budgets: %+v", s.WeekBudgets)
	}
}

// Spec 22.12 (Joe, 2026-10-02): a chat answer has NO status line and NO
// saturated fat line by code, in any budget state. The budget state stays on
// the wire (the snapshot) for the card, the dashboard and Today.
func TestT20NoStatusAndNoSatFatLineInReplies(t *testing.T) {
	sat := func(name string, g float64) string {
		return strings.Replace(lItem(name, 100, 300, 10, 1, lv(0, 0, 0, 0, 0, nil), ""), `"sat_fat_g":1`, `"sat_fat_g":`+num(&g), 1)
	}
	h := newV7(t, satDoc())
	h.strava("fresh", "Walk", 600, 0, "2026-10-01T07:00:00", h.clk.Now()) // a rest day: budget 20 g
	clean := func(what string, blocks []Block) {
		t.Helper()
		for _, tb := range textBlocks(blocks) {
			if strings.HasPrefix(tb, "Protein ") || strings.Contains(tb, "Sat fat") || strings.Contains(tb, "over the budget") || strings.Contains(tb, "Most of it") {
				t.Errorf("%s: a status or saturated fat line is in the reply: %q", what, tb)
			}
		}
	}
	r := h.say("cottage cheese", logOf(sat("cottage cheese", 6.2)))
	clean("on_pace", r.Blocks)
	h.say("eggs", logOf(sat("scrambled eggs", 5.1)))
	r = h.say("butter and salami", logOf(sat("butter", 3.1), sat("salami", 2.6)))
	clean("near", r.Blocks) // 17 g of 20
	if b := budget(r.Snapshot, "sat_fat_g"); b.Status != "near" {
		t.Errorf("the snapshot keeps the state: %+v", b)
	}
	r = h.say("cheese", logOf(sat("cheddar", 6)))
	clean("over", r.Blocks)
	if b := budget(r.Snapshot, "sat_fat_g"); b.Status != "over" {
		t.Errorf("the snapshot keeps the state: %+v", b)
	}
	// The reply of a log is the model's sentence and the widget, nothing else.
	if tb := textBlocks(r.Blocks); len(tb) > 1 || r.Blocks[len(r.Blocks)-1].Widget != "macros_today" || len(r.Blocks) != len(tb)+1 {
		t.Errorf("log reply blocks: %+v", r.Blocks)
	}
	// A correction: the code summary and the widget.
	c := h.say("the cheddar was only half", corr(corrForm(r.Items[0].ItemID, `"share":0.5`)))
	clean("correction", c.Blocks)
	if tb := textBlocks(c.Blocks); len(tb) != 1 || len(c.Blocks) != 2 {
		t.Errorf("correction blocks: %+v", c.Blocks)
	}
	// A drink and a question: no remark by code.
	q := h.say("how am I doing", `{"intent":"question","items":[],"text":"You are behind on fibre.","widgets":[],"clinical_topic":false}`)
	clean("question", q.Blocks)
	// The button replies carry no status line either.
	u := h.post("/fuel/undo", map[string]any{"client_id": "c0000000-undo-0001", "item_id": r.Items[0].ItemID})
	if !strings.Contains(u.Body.String(), `"text":"Removed cheddar."`) {
		t.Errorf("undo reply: %s", u.Body.String())
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Contains(feed, "Protein 1") || strings.Contains(feed, "Sat fat ") || strings.Contains(feed, "over the budget") {
		t.Error("a feed line carries a status or saturated fat line")
	}
}
