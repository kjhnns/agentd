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
	if st := statusSentence(h.snap(t, "")); !strings.Contains(st, "sat fat is over the budget") {
		t.Errorf("status line: %q", st)
	}
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

// The words of 18.14: a code line with the largest sources, never "eat less".
func TestT20SaturatedFatWords(t *testing.T) {
	sat := func(name string, g float64) string {
		return strings.Replace(lItem(name, 100, 300, 10, 1, lv(0, 0, 0, 0, 0, nil), ""), `"sat_fat_g":1`, `"sat_fat_g":`+num(&g), 1)
	}
	h := newV7(t, satDoc())
	h.strava("fresh", "Walk", 600, 0, "2026-10-01T07:00:00", h.clk.Now()) // a rest day: budget 20 g
	r := h.say("cottage cheese", logOf(sat("cottage cheese", 6.2)))
	if len(textBlocks(r.Blocks)) != 1 {
		t.Fatalf("on_pace: nothing is added: %v", textBlocks(r.Blocks))
	}
	h.say("eggs", logOf(sat("scrambled eggs", 5.1)))
	h.say("toast", logOf(sat("toast", 0.5)))
	r = h.say("butter and salami", logOf(sat("butter", 2.6), sat("salami", 2.6)))
	tb := textBlocks(r.Blocks)
	// 17 g of 20: near. Ties (2.6 g) go by row key.
	ids := []string{r.Items[0].ItemID, r.Items[1].ItemID}
	third := "butter"
	if ids[1] < ids[0] {
		third = "salami"
	}
	want := "Sat fat 17 of 20 g. Most of it: cottage cheese 6.2 g, scrambled eggs 5.1 g, " + third + " 2.6 g. A swap of the largest one helps most."
	if len(tb) != 2 || tb[1] != want {
		t.Fatalf("near:\n got %q\nwant %q", tb, want)
	}
	r = h.say("cheese", logOf(sat("cheddar", 6)))
	if tb = textBlocks(r.Blocks); len(tb) != 2 || !strings.HasPrefix(tb[1], "Sat fat 23 of 20 g, over the budget. Most of it: cottage cheese 6.2 g, cheddar 6 g, scrambled eggs 5.1 g.") {
		t.Fatalf("over: %q", tb)
	}
	if !strings.Contains(tb[0], "sat fat is over the budget") {
		t.Errorf("status line: %q", tb[0])
	}
	// A correction shows the line after the status line.
	c := h.say("the cheddar was only half", corr(corrForm(r.Items[0].ItemID, `"share":0.5`)))
	if tb = textBlocks(c.Blocks); len(tb) != 3 || !strings.HasPrefix(tb[2], "Sat fat 20 of 20 g. Most of it:") {
		t.Fatalf("after a correction: %q", tb)
	}
	feed := h.get("/fuel/feed").Body.String()
	if strings.Contains(strings.ToLower(feed), "eat less") || strings.Contains(strings.ToLower(feed), "eating less") {
		t.Error("a reply says to eat less")
	}
	// With the cap form nothing is added.
	h2 := newV7(t, weekDoc())
	h2.say("cheese", logOf(sat("cheddar", 19)))
	r2 := h2.say("more cheese", logOf(sat("brie", 5)))
	if tb := textBlocks(r2.Blocks); len(tb) != 1 || !strings.Contains(tb[0], "sat fat is over the cap") {
		t.Errorf("cap form: %q", tb)
	}
}
