package fuel

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Section 19.9: the weekly view. Unless stated the file is schema 2 with
// protein 160 floor, sat fat 20 cap, fibre 35 floor, kcal 2600 / 3000,
// min_complete_kcal 1500, eating window 07:00 to 20:30, Europe/Zurich.

func weekDoc(kv ...any) string {
	return v2(append([]any{"energy.calibration", calibSettings("2026-08-01")}, kv...)...)
}

// at restarts the harness at a wall clock time in Zurich (the cache is
// hydrated for that "today").
func (h *harness) atZurich(t *testing.T, s string) {
	h.clk.Set(zurich(t, s))
	h.restart()
}

func states(days []WeekDay) string {
	var b strings.Builder
	for _, d := range days {
		b.WriteByte(d.State[0])
	}
	return b.String()
}

func TestW1WeekBoundary(t *testing.T) {
	h := newV7(t, weekDoc())
	h.food("2026-10-04", "chicken", 500, 50, 2, 0.0, nil)
	h.atZurich(t, "2026-10-04 23:30") // Sunday
	v := h.week(t, "")
	if v.Week.From != "2026-09-28" || v.Week.To != "2026-10-04" || states(v.Week.Days) != "ppppppt" || v.Today != "2026-10-04" {
		t.Fatalf("Sunday: %s to %s %s", v.Week.From, v.Week.To, states(v.Week.Days))
	}
	for _, b := range v.Week.Budgets {
		if b.TargetEstimated {
			t.Errorf("%s: target_estimated with no future date", b.Key)
		}
	}
	if wb(v, "protein_g").Consumed != 50 || v.Week.Days[0].Weekday != "Mon" || v.Week.Days[6].Weekday != "Sun" {
		t.Errorf("protein %g, weekdays %s %s", wb(v, "protein_g").Consumed, v.Week.Days[0].Weekday, v.Week.Days[6].Weekday)
	}
	// One hour later: Monday 00:30, a new week.
	h.clk.Add(time.Hour)
	v = h.week(t, "")
	p := wb(v, "protein_g")
	if v.Week.From != "2026-10-05" || v.Week.To != "2026-10-11" || states(v.Week.Days) != "tffffff" || p.Consumed != 0 || num(p.PaceNow) != "0" {
		t.Fatalf("Monday: %s to %s %s consumed %g pace %s", v.Week.From, v.Week.To, states(v.Week.Days), p.Consumed, num(p.PaceNow))
	}
	if !wb(v, "kcal").TargetEstimated || wb(v, "protein_g").TargetEstimated {
		t.Error("target_estimated: kcal true (class dependent, future dates), protein false")
	}
	// The row of Sunday is in last7, not in the new week.
	if pb(v, "protein_g").Consumed != 50 || cell(pb(v, "protein_g"), "2026-10-04").Actual == nil {
		t.Errorf("last7 protein %g", pb(v, "protein_g").Consumed)
	}
}

func TestW2DayInAnotherDayClass(t *testing.T) {
	h := newV7(t, weekDoc(carbsOn()...))
	fresh := h.clk.Now()
	h.strava("2026-09-28-run", "Run", 3600, 10000, "2026-09-28T08:00:00", fresh)
	h.strava("2026-09-30-ride", "Ride", 6000, 40000, "2026-09-30T08:00:00", fresh)
	v := h.week(t, "") // Thursday 2026-10-01 12:00
	want := []string{"400", "240", "480", "240", "240", "240", "240"}
	c := wb(v, "carbs_g")
	for i, cl := range c.Days {
		if num(cl.Target) != want[i] {
			t.Errorf("carbs %s: %s, want %s", cl.Date, num(cl.Target), want[i])
		}
	}
	if num(c.Target) != "2080" || !c.TargetEstimated || c.Kind != "floor" {
		t.Errorf("carbs week: %s estimated %v kind %s", num(c.Target), c.TargetEstimated, c.Kind)
	}
	for i, d := range v.Week.Days {
		if fut := i >= 4; d.Estimated != fut || (fut && (d.DayClass != "low" || d.DayType != "rest" || d.Complete != nil)) {
			t.Errorf("day %s: %+v", d.Date, d)
		}
	}
	if v.Week.Days[0].DayClass != "moderate" || v.Week.Days[2].DayClass != "long" || v.Week.Days[3].DayClass != "low" {
		t.Errorf("classes: %s %s %s", v.Week.Days[0].DayClass, v.Week.Days[2].DayClass, v.Week.Days[3].DayClass)
	}
	k := wb(v, "kcal")
	kw := []string{"3000", "2600", "3000", "2600", "2600", "2600", "2600"}
	for i, cl := range k.Days {
		if num(cl.Target) != kw[i] {
			t.Errorf("kcal %s: %s, want %s", cl.Date, num(cl.Target), kw[i])
		}
	}
	if num(k.Target) != "19000" || num(wb(v, "protein_g").Target) != "1120" || wb(v, "protein_g").TargetEstimated {
		t.Errorf("kcal %s protein %s", num(k.Target), num(wb(v, "protein_g").Target))
	}
	if cell(c.PeriodBudget, "2026-10-02").Estimated != true || cell(wb(v, "protein_g").PeriodBudget, "2026-10-02").Estimated {
		t.Error("cell.estimated: true for a class-dependent budget on a future date only")
	}
	// The newest Strava file older than 36 h: a past date is never stale.
	old := h.clk.Now().Add(-40 * time.Hour)
	h.strava("2026-09-28-run", "Run", 3600, 10000, "2026-09-28T08:00:00", old)
	h.strava("2026-09-30-ride", "Ride", 6000, 40000, "2026-09-30T08:00:00", old)
	v = h.week(t, "")
	c = wb(v, "carbs_g")
	if num(c.Days[0].Target) != "400" || num(c.Days[2].Target) != "480" || num(c.Days[3].Target) != "240" {
		t.Errorf("stale: %s %s %s", num(c.Days[0].Target), num(c.Days[2].Target), num(c.Days[3].Target))
	}
	if v.Week.Days[3].DayClass != "unknown" || v.Week.Days[3].DayType != "unknown" || v.Week.Days[0].DayClass != "moderate" || !has(v.Missing, "strava_stale") {
		t.Errorf("stale today: %+v missing %v", v.Week.Days[3], v.Missing)
	}
}

func TestW3IncompleteDay(t *testing.T) {
	fix := func(h *harness) {
		h.food("2026-09-28", "monday", 2500, 150, 10, 30.0, nil)
		h.food("2026-09-29", "tuesday", 900, 170, 25, 5.0, nil)
		h.food("2026-09-30", "wednesday", 2500, 165, 18, 40.0, nil)
	}
	h := newV7(t, weekDoc())
	fix(h)
	h.restart()
	v := h.week(t, "")
	tue := v.Week.Days[1]
	if tue.Complete == nil || *tue.Complete || *v.Week.Days[0].Complete != true || v.Week.Days[3].Complete != nil {
		t.Fatalf("complete: %+v", v.Week.Days)
	}
	for _, b := range v.Week.Budgets {
		got := cell(b.PeriodBudget, "2026-09-29").Result
		want := "incomplete"
		if b.Key == "alcohol_g" || b.Key == "carbs_g" {
			want = "none" // no target
		}
		if got != want {
			t.Errorf("Tuesday %s: %s, want %s", b.Key, got, want)
		}
		for _, c := range b.Days {
			if c.Result == "missed" && c.Date == "2026-09-29" {
				t.Errorf("%s: missed on a day that is not complete", b.Key)
			}
			if c.Result == "missed" && c.Date == "2026-10-01" {
				t.Errorf("%s: missed for today", b.Key)
			}
		}
	}
	p := wb(v, "protein_g")
	if v.Week.IncompleteDays != 1 || !v.Week.ConsumedIsPartial || p.Consumed != 485 || p.MeanDays != 2 || num(p.Mean) != "157.5" || num(p.MeanTarget) != "160" {
		t.Errorf("week: incomplete %d partial %v protein %g mean %s over %d (target %s)", v.Week.IncompleteDays, v.Week.ConsumedIsPartial, p.Consumed, num(p.Mean), p.MeanDays, num(p.MeanTarget))
	}
	if cell(p.PeriodBudget, "2026-09-28").Result != "missed" || cell(p.PeriodBudget, "2026-09-30").Result != "met" || *p.DaysMet != 1 || p.DaysJudged != 2 {
		t.Errorf("protein cells: %+v", p.Days)
	}
	if num(v.CompleteDay.MinKcal) != "1500" || has(v.Missing, "complete_day_rule_unset") {
		t.Errorf("complete_day %s missing %v", num(v.CompleteDay.MinKcal), v.Missing)
	}

	// A kcal unknown row makes the day incomplete.
	h = newV7(t, weekDoc())
	fix(h)
	h.food("2026-09-30", "mystery", 0, 5, 1, 1.0, map[string]any{"kcal": nil})
	h.restart()
	if v := h.week(t, ""); *v.Week.Days[2].Complete || cell(wb(v, "protein_g").PeriodBudget, "2026-09-30").Result != "incomplete" {
		t.Errorf("kcal unknown row: %+v", v.Week.Days[2])
	}
	// A break date makes the day incomplete.
	cs := calibSettings("2026-08-01")
	cs["break_dates"] = []any{"2026-09-28"}
	h = newV7(t, v2("energy.calibration", cs))
	fix(h)
	h.restart()
	if v := h.week(t, ""); *v.Week.Days[0].Complete || v.Week.IncompleteDays != 2 {
		t.Errorf("break date: %+v", v.Week.Days[0])
	}
	// start_date is not used: a day before it with 2500 kcal is complete.
	h = newV7(t, v2("energy.calibration", calibSettings("2026-09-30")))
	fix(h)
	h.restart()
	if v := h.week(t, ""); !*v.Week.Days[0].Complete {
		t.Error("a day before start_date must be complete")
	}
	// No calibration settings: the rule cannot be evaluated.
	h = newV7(t, v2())
	fix(h)
	h.restart()
	v = h.week(t, "")
	if !has(v.Missing, "complete_day_rule_unset") || v.CompleteDay.MinKcal != nil || v.Week.IncompleteDays != 3 {
		t.Errorf("rule unset: missing %v min %v incomplete %d", v.Missing, v.CompleteDay.MinKcal, v.Week.IncompleteDays)
	}
	for _, b := range v.Week.Budgets[:4] {
		for _, c := range b.Days[:3] {
			if c.Result != "incomplete" {
				t.Errorf("rule unset %s %s: %s", b.Key, c.Date, c.Result)
			}
		}
		if b.Mean != nil || b.MeanDays != 0 {
			t.Errorf("rule unset %s: mean %s", b.Key, num(b.Mean))
		}
	}
}

func TestW4CapAndFloor(t *testing.T) {
	for _, doc := range []string{
		weekDoc(carbsOn()...),
		// The comparison goes by the budget KEY, whatever kind the file gives.
		weekDoc(append(carbsOn(), "protein_g", map[string]any{"kind": "pace", "value": 160}, "fiber_g", map[string]any{"kind": "cap", "value": 35})...),
	} {
		h := newV7(t, doc)
		h.strava("x", "Walk", 60, 0, "2026-10-01T07:00:00", h.clk.Now()) // fresh files, no training
		c := func(g float64) map[string]any { return map[string]any{"carbs_g": g} }
		h.food("2026-09-27", "a", 2340, 160, 20.0, 35.0, c(240))
		h.food("2026-09-28", "b", 2339, 159.9, 20.1, 34.9, c(239))
		h.food("2026-09-29", "c", 2860, 200, 5, 40.0, c(300))
		h.food("2026-09-30", "d", 2861, 200, 5, 40.0, c(300))
		h.restart()
		v := h.week(t, "")
		res := func(key, date string) string { return cell(pb(v, key), date).Result }
		for _, w := range []struct{ key, date, want string }{
			{"protein_g", "2026-09-27", "met"}, {"protein_g", "2026-09-28", "missed"},
			{"sat_fat_g", "2026-09-27", "met"}, {"sat_fat_g", "2026-09-28", "missed"},
			{"fiber_g", "2026-09-27", "met"}, {"fiber_g", "2026-09-28", "missed"},
			{"kcal", "2026-09-27", "met"}, {"kcal", "2026-09-28", "missed"}, {"kcal", "2026-09-29", "met"}, {"kcal", "2026-09-30", "missed"},
			{"carbs_g", "2026-09-27", "met"}, {"carbs_g", "2026-09-28", "missed"},
			{"alcohol_g", "2026-09-27", "none"},
		} {
			if got := res(w.key, w.date); got != w.want {
				t.Errorf("%s %s: %s, want %s", w.key, w.date, got, w.want)
			}
		}
		k := pb(v, "kcal")
		met, judged := 0, 0
		for _, cl := range k.Days {
			if cl.Result == "met" {
				met++
			}
			if cl.Result == "met" || cl.Result == "missed" {
				judged++
			}
		}
		if *k.DaysMet != met || k.DaysJudged != judged || met != 2 || judged != 4 {
			t.Errorf("kcal days_met %d days_judged %d (cells %d %d)", *k.DaysMet, k.DaysJudged, met, judged)
		}
		if strings.Contains(doc, `"pace","value":160`) && (pb(v, "protein_g").Kind != "pace" || pb(v, "fiber_g").Kind != "cap") {
			t.Errorf("kind on the wire is the file's value: %s %s", pb(v, "protein_g").Kind, pb(v, "fiber_g").Kind)
		}
		// Today: no verdict, only what more food cannot change.
		step := func(item string, kcal, protein, sat float64) WeekView {
			h.food("2026-10-01", item, kcal, protein, sat, 0.0, nil)
			h.clk.Add(61 * time.Second)
			return h.week(t, "")
		}
		tr := func(v WeekView, key string) string { return cell(wb(v, key).PeriodBudget, "2026-10-01").Result }
		v = step("t1", 2000, 100, 15)
		if tr(v, "protein_g") != "open" || tr(v, "sat_fat_g") != "open" || tr(v, "kcal") != "open" || tr(v, "fiber_g") != "open" {
			t.Errorf("today 1: %s %s %s", tr(v, "protein_g"), tr(v, "sat_fat_g"), tr(v, "kcal"))
		}
		v = step("t2", 700, 60, 5) // 2700 kcal, protein 160, sat fat 20
		if tr(v, "protein_g") != "met" || tr(v, "sat_fat_g") != "open" || tr(v, "kcal") != "open" {
			t.Errorf("today 2: %s %s %s", tr(v, "protein_g"), tr(v, "sat_fat_g"), tr(v, "kcal"))
		}
		v = step("t3", 161, 0, 1) // 2861 kcal, sat fat 21
		if tr(v, "sat_fat_g") != "over" || tr(v, "kcal") != "over" {
			t.Errorf("today 3: %s %s", tr(v, "sat_fat_g"), tr(v, "kcal"))
		}
		s := wb(v, "sat_fat_g")
		for _, cl := range s.Days {
			if cl.Result == "over" || cl.Result == "open" {
				if cl.Date != "2026-10-01" {
					t.Errorf("%s on %s", cl.Result, cl.Date)
				}
			}
		}
		// Sat fat this week: 20.1 + 5 + 5 + 21 = 51.1 of 140: 88.9 left. Protein today counts as met.
		if num(s.Target) != "140" || num(s.Remaining) != "88.9" || *wb(v, "protein_g").DaysMet != 3 {
			t.Errorf("sat fat target %s remaining %s; protein days_met %d", num(s.Target), num(s.Remaining), *wb(v, "protein_g").DaysMet)
		}
		// Over the week cap: remaining is negative, not clamped.
		h.food("2026-10-01", "butter", 0, 0, 100, 0.0, nil)
		h.clk.Add(61 * time.Second)
		if r := wb(h.week(t, ""), "sat_fat_g").Remaining; num(r) != "-11.1" {
			t.Errorf("remaining over the cap: %s", num(r))
		}
	}
	// A carbohydrate target of null: "none".
	h := newV7(t, weekDoc())
	h.food("2026-09-30", "d", 2600, 200, 5, 40.0, map[string]any{"carbs_g": 300.0})
	h.restart()
	if got := cell(pb(h.week(t, ""), "carbs_g"), "2026-09-30"); got.Result != "none" || got.Target != nil || num(got.Actual) != "300" {
		t.Errorf("carbs without a target: %+v", got)
	}
}

func cellOf(w WeekBudget, date string) DayCell { return cell(w.PeriodBudget, date) }

func TestW5DaylightSaving(t *testing.T) {
	h := newV7(t, weekDoc())
	h.food("2026-10-25", "sunday roast", 800, 60, 5, 5.0, nil)
	for _, c := range []struct{ utc, today, from, to string }{
		{"2026-10-25T00:30:00Z", "2026-10-25", "2026-10-19", "2026-10-25"}, // 02:30 CEST
		{"2026-10-25T01:30:00Z", "2026-10-25", "2026-10-19", "2026-10-25"}, // 02:30 CET, after the clock went back
		{"2026-10-25T22:59:00Z", "2026-10-25", "2026-10-19", "2026-10-25"}, // 23:59 CET
		{"2026-10-25T23:00:00Z", "2026-10-26", "2026-10-26", "2026-11-01"},
		{"2026-03-29T00:30:00Z", "2026-03-29", "2026-03-23", "2026-03-29"}, // 01:30 CET
		{"2026-03-29T01:30:00Z", "2026-03-29", "2026-03-23", "2026-03-29"}, // 03:30 CEST (the 23-hour day)
	} {
		tm, _ := time.Parse(time.RFC3339, c.utc)
		h.clk.Set(tm)
		h.restart()
		v := h.week(t, "")
		if v.Today != c.today || v.Week.From != c.from || v.Week.To != c.to {
			t.Errorf("%s: today %s week %s to %s", c.utc, v.Today, v.Week.From, v.Week.To)
		}
		seen := map[string]bool{}
		for i, d := range v.Last7.Days {
			if seen[d.Date] || d.Date != dateAdd(c.today, i-6) {
				t.Errorf("%s: last7[%d] = %s", c.utc, i, d.Date)
			}
			seen[d.Date] = true
		}
		if len(v.Week.Days) != 7 || len(v.Last7.Days) != 7 {
			t.Errorf("%s: %d and %d days", c.utc, len(v.Week.Days), len(v.Last7.Days))
		}
		if strings.HasPrefix(c.utc, "2026-10-25") {
			if got := pb(v, "protein_g").Consumed; got != 60 {
				t.Errorf("%s: the row of 2026-10-25 counts %g in last7, want 60 (once)", c.utc, got)
			}
			wantWeek := 60.0
			if c.today == "2026-10-26" {
				wantWeek = 0
			}
			if got := wb(v, "protein_g").Consumed; got != wantWeek {
				t.Errorf("%s: week protein %g, want %g", c.utc, got, wantWeek)
			}
		}
	}
	// 13:45 CET on the 25-hour day: elapsed 0.5, six day targets plus half.
	tm, _ := time.Parse(time.RFC3339, "2026-10-25T12:45:00Z")
	h.clk.Set(tm)
	h.restart()
	if p := wb(h.week(t, ""), "protein_g").PaceNow; num(p) != "1040" {
		t.Errorf("pace on the DST day: %s, want 1040", num(p))
	}
}

func TestW6Pace(t *testing.T) {
	h := newV7(t, weekDoc())
	h.atZurich(t, "2026-09-30 13:45") // Wednesday
	v := h.week(t, "")
	if p := wb(v, "protein_g"); num(p.PaceNow) != "400" || num(p.Target) != "1120" {
		t.Errorf("protein pace %s target %s", num(p.PaceNow), num(p.Target))
	}
	if num(wb(v, "sat_fat_g").PaceNow) != "50" || num(wb(v, "alcohol_g").PaceNow) != "10.7" {
		t.Errorf("sat fat pace %s alcohol pace %s", num(wb(v, "sat_fat_g").PaceNow), num(wb(v, "alcohol_g").PaceNow))
	}
	if wb(v, "carbs_g").PaceNow != nil || wb(v, "carbs_g").Target != nil {
		t.Error("a budget without targets has no pace and no target")
	}
	// Before 07:00 on Monday: 0 for all.
	h.atZurich(t, "2026-09-28 06:30")
	v = h.week(t, "")
	for _, k := range []string{"protein_g", "sat_fat_g", "fiber_g", "kcal", "alcohol_g"} {
		if num(wb(v, k).PaceNow) != "0" {
			t.Errorf("Monday 06:30 %s pace %s", k, num(wb(v, k).PaceNow))
		}
	}
	// A week that is fully past: pace = target, also for alcohol.
	h.atZurich(t, "2026-10-07 12:00")
	v = h.week(t, "?date=2026-09-30")
	for _, k := range []string{"protein_g", "sat_fat_g", "fiber_g", "kcal", "alcohol_g"} {
		b := wb(v, k)
		if b.Target == nil || num(b.PaceNow) != num(b.Target) || b.TargetEstimated {
			t.Errorf("past week %s: pace %s target %s", k, num(b.PaceNow), num(b.Target))
		}
	}
	if states(v.Week.Days) != "ppppppp" {
		t.Errorf("past week states %s", states(v.Week.Days))
	}
}

func TestW7Energy(t *testing.T) {
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	h := newV7(t, weekDoc("energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0))
	h.strava("2026-09-28-run", "Run", 6000, 20000, "2026-09-28T08:00:00", h.clk.Now())
	v := h.week(t, "")
	k := wb(v, "kcal")
	if num(k.Days[0].Target) != "4120" || num(k.Days[1].Target) != "2520" || num(k.Days[4].Target) != "3000" || !k.Days[4].Estimated || k.Days[4].Result != "future" || k.Days[4].Actual != nil {
		t.Errorf("formula: %s %s %+v", num(k.Days[0].Target), num(k.Days[1].Target), k.Days[4])
	}
	// 4120 + 2520 + 2520 + 2520 (today, no run yet) + 3 x 3000.
	if num(k.Target) != "20680" || !k.TargetEstimated {
		t.Errorf("formula week target %s", num(k.Target))
	}
	// Legacy (schema 1 file): past days by day type, future days the rest value.
	h = newV7(t, goldenTargetsV1)
	h.strava("2026-09-28-run", "Run", 6000, 20000, "2026-09-28T08:00:00", h.clk.Now())
	v = h.week(t, "")
	k = wb(v, "kcal")
	if num(k.Days[0].Target) != "3000" || num(k.Days[1].Target) != "2600" || num(k.Days[5].Target) != "2600" || num(k.Target) != "18600" {
		t.Errorf("legacy: %s %s %s week %s", num(k.Days[0].Target), num(k.Days[1].Target), num(k.Days[5].Target), num(k.Target))
	}
	c := wb(v, "carbs_g")
	for i, cl := range c.Days {
		want := "none"
		if i >= 4 {
			want = "future"
		}
		if cl.Target != nil || cl.Result != want {
			t.Errorf("legacy carbs %s: %s %s", cl.Date, num(cl.Target), cl.Result)
		}
	}
	if c.Target != nil || v.Week.Days[0].DayClass != "unknown" || has(v.Missing, "day_class_unset") || !has(v.Missing, "complete_day_rule_unset") {
		t.Errorf("legacy: carbs target %s class %s missing %v", num(c.Target), v.Week.Days[0].DayClass, v.Missing)
	}
}

func TestW8Alcohol(t *testing.T) {
	wine := func(g float64) map[string]any { return map[string]any{"kind": "drink", "alcohol_g": g, "volume_ml": 150.0} }
	fix := func(h *harness) {
		h.food("2026-09-28", "wine", 100, 0, 0, 0.0, wine(12))
		h.food("2026-09-30", "beer", 150, 1, 0, 0.0, wine(10))
		h.strava("x", "Walk", 60, 0, "2026-10-01T07:00:00", h.clk.Now())
		h.restart()
	}
	h := newV7(t, weekDoc())
	fix(h)
	v := h.week(t, "")
	a := wb(v, "alcohol_g")
	if a.Consumed != 22 || num(a.Target) != "30" || num(a.Remaining) != "8" || a.DaysMet != nil || a.DaysJudged != 0 || a.Kind != "cap" || a.TargetEstimated {
		t.Errorf("alcohol: %+v", a)
	}
	for i, c := range a.Days {
		want := "none"
		if i >= 4 {
			want = "future"
		}
		if c.Target != nil || c.Result != want {
			t.Errorf("alcohol cell %s: %s %s", c.Date, num(c.Target), c.Result)
		}
	}
	s := h.snap(t, "")
	for _, m := range s.Intake {
		if m.Key == "alcohol_g_week" && m.Consumed != a.Consumed {
			t.Errorf("R = today: week %g, intake %g", a.Consumed, m.Consumed)
		}
	}
	// R = Tuesday: the week sum runs to the real today; the snapshot of
	// Tuesday keeps the section 15 rule (Monday to Tuesday).
	if got := wb(h.week(t, "?date=2026-09-29"), "alcohol_g").Consumed; got != 22 {
		t.Errorf("R = Tuesday: week alcohol %g, want 22", got)
	}
	for _, m := range h.snap(t, "?date=2026-09-29").Intake {
		if m.Key == "alcohol_g_week" && m.Consumed != 12 {
			t.Errorf("snapshot of Tuesday: intake %g, want 12", m.Consumed)
		}
	}
	// Without the key: no target, nothing left, no pace.
	m := v2Base()
	delete(m, "alcohol_g_week")
	at(m, "energy.calibration", calibSettings("2026-08-01"))
	h = newV7(t, mustJSON(m))
	fix(h)
	a = wb(h.week(t, ""), "alcohol_g")
	if a.Target != nil || a.Remaining != nil || a.PaceNow != nil || a.Consumed != 22 {
		t.Errorf("no cap: %+v", a)
	}
	// rest 30 / training null: the snapshot's selection for R.
	h = newV7(t, weekDoc("alcohol_g_week", map[string]any{"kind": "cap", "rest": 30, "training": nil}))
	fix(h)
	if a = wb(h.week(t, ""), "alcohol_g"); num(a.Target) != "30" {
		t.Errorf("rest day: %s", num(a.Target))
	}
	h.strava("2026-10-01-run", "Run", 3000, 10000, "2026-10-01T08:00:00", h.clk.Now())
	if a = wb(h.week(t, ""), "alcohol_g"); a.Target != nil || a.PaceNow != nil {
		t.Errorf("training day: %s", num(a.Target))
	}
}

func mustJSON(v any) string {
	b, err := jsonMarshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestW9OneTruth(t *testing.T) {
	h := newV7(t, weekDoc(carbsOn()...))
	h.food("2026-09-28", "a", 2500, 150, 10, 30.0, map[string]any{"carbs_g": 250.0})
	h.food("2026-09-30", "b", 2700, 170, 25, 36.0, map[string]any{"carbs_g": 410.0})
	h.food("2026-10-01", "c", 900, 60, 8, 12.0, map[string]any{"carbs_g": 90.0, "alcohol_g": 5.0})
	h.restart()
	check := func(label, q string) {
		t.Helper()
		// Fix the clock: the two requests are at the same instant.
		v := h.week(t, q)
		s := h.snap(t, q)
		if s.WeekBudgets == nil {
			t.Fatalf("%s: week_budgets is null", label)
		}
		c := s.WeekBudgets
		if c.From != v.Week.From || c.To != v.Week.To || c.IncompleteDays != v.Week.IncompleteDays || c.ConsumedIsPartial != v.Week.ConsumedIsPartial || len(c.Budgets) != 6 {
			t.Errorf("%s: compact head %+v", label, c)
		}
		for i, b := range v.Week.Budgets {
			cb, l7 := c.Budgets[i], v.Last7.Budgets[i]
			got := fmt.Sprintf("%s %s %s %s %v %g %s %s", cb.Key, cb.Unit, cb.Kind, num(cb.Target), cb.TargetEstimated, cb.Consumed, num(cb.Remaining), num(cb.PaceNow))
			want := fmt.Sprintf("%s %s %s %s %v %g %s %s", b.Key, b.Unit, b.Kind, num(b.Target), b.TargetEstimated, b.Consumed, num(b.Remaining), num(b.PaceNow))
			if got != want || cb.Last7DaysJudged != l7.DaysJudged || (cb.Last7DaysMet == nil) != (l7.DaysMet == nil) || (l7.DaysMet != nil && *cb.Last7DaysMet != *l7.DaysMet) {
				t.Errorf("%s %s: compact %s, week %s", label, b.Key, got, want)
			}
		}
		if q != "" {
			return
		}
		// T(b, today) and A(b, today) equal the snapshot budgets.
		for _, b := range v.Week.Budgets {
			cl := cellOf(b, v.Today)
			if b.Key == "alcohol_g" {
				for _, m := range s.Intake {
					if m.Key == "alcohol_g" && m.Consumed != *cl.Actual {
						t.Errorf("%s alcohol today: %g vs %g", label, *cl.Actual, m.Consumed)
					}
				}
				continue
			}
			sb := budget(s, b.Key)
			if num(cl.Target) != num(sb.Target) || *cl.Actual != sb.Consumed || cl.UnknownRows != sb.UnknownRows {
				t.Errorf("%s %s today: week %s / %g, snapshot %s / %g", label, b.Key, num(cl.Target), *cl.Actual, num(sb.Target), sb.Consumed)
			}
		}
	}
	check("stale", "") // no Strava file: today is stale
	h.strava("2026-10-01-run", "Run", 3600, 10000, "2026-10-01T08:00:00", h.clk.Now())
	check("moderate day", "")
	if s := h.snap(t, ""); s.DayClass != "moderate" || num(budget(s, "carbs_g").Target) != "400" {
		t.Errorf("moderate day: %s %s", s.DayClass, num(budget(s, "carbs_g").Target))
	}
	check("past date", "?date=2026-09-29")
	// A needed date that was never read, and the reads fail: null, and 502.
	h.svc.cache.mu.Lock()
	delete(h.svc.cache.days, "2026-09-29")
	h.svc.cache.mu.Unlock()
	h.vars.failReads.Store(true)
	h.clk.Add(61 * time.Second)
	rec := h.get("/fuel/snapshot")
	if rec.Code != 200 {
		t.Fatalf("snapshot with a failed read: %d", rec.Code)
	}
	if s := decode[Snapshot](t, rec); s.WeekBudgets != nil || len(s.Budgets) != 7 {
		t.Errorf("week_budgets must be null when a needed date is not loaded")
	}
	if rec := h.get("/fuel/week"); rec.Code != 502 || errCode(t, rec) != "upstream_failed" {
		t.Errorf("week with a failed read: %d %s", rec.Code, rec.Body)
	}
	h.vars.failReads.Store(false)
	// A snapshot date 30 days back: 200, week_budgets null.
	if s := h.snap(t, "?date="+dateAdd("2026-10-01", -30)); s.WeekBudgets != nil {
		t.Error("week_budgets must be null more than 28 days back")
	}
	if s := h.snap(t, "?date="+dateAdd("2026-10-01", -28)); s.WeekBudgets == nil {
		t.Error("week_budgets must be set 28 days back")
	}
}

func TestW10AdditiveAndClean(t *testing.T) {
	h := newV7(t, weekDoc(carbsOn()...), func(o *Options) { o.BPVar, o.SymptomVar = "Fuel e2e blood pressure", "Fuel e2e symptom log" })
	h.setTargets(weekDoc(append(carbsOn(), "clinician.hard_sets", map[string]any{"text": "Not to failure.", "set_by": "Dr. X", "set_on": "2026-10-01"})...))
	calls := h.model.calls
	posts := h.vars.posts
	rec := h.get("/fuel/week")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("week: %d cache-control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	keys := keysOf(rec.Body.Bytes())
	for _, bad := range []string{"status", "score", "clinician", "records", "blood_pressure", "symptom", "hard_sets", "hard", "pace_target_now"} {
		if keys[bad] {
			t.Errorf("the week view has a key named %q", bad)
		}
	}
	if strings.Contains(rec.Body.String(), "Not to failure") {
		t.Error("a clinician text is in the week view")
	}
	if h.model.calls != calls || h.vars.posts != posts {
		t.Error("the route called the model or wrote to Variables")
	}
	req := httptest.NewRequest("GET", "/fuel/week", nil)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("without the token: %d", w.Code)
	}
}

func TestW11UnknownRows(t *testing.T) {
	h := newV7(t, weekDoc())
	h.food("2026-09-28", "known", 2500, 170, 5, 20.0, nil)
	h.food("2026-09-28", "fibre unknown", 100, 5, 1, nil, map[string]any{"fiber_g": nil})
	h.food("2026-09-29", "known", 2500, 170, 5, 36.0, nil)
	h.food("2026-09-29", "fibre unknown", 100, 5, 1, nil, map[string]any{"fiber_g": nil})
	// A row of another writer without sat_fat_g.
	h.food("2026-09-30", "known", 2500, 170, 21, 40.0, nil)
	h.food("2026-09-30", "no sat fat", 100, 5, 0, 1.0, map[string]any{"sat_fat_g": nil})
	h.food("2026-09-27", "known", 2500, 170, 10, 40.0, nil)
	h.food("2026-09-27", "no sat fat", 100, 5, 0, 1.0, map[string]any{"sat_fat_g": nil})
	h.restart()
	v := h.week(t, "")
	f := pb(v, "fiber_g")
	if c := cell(f, "2026-09-28"); c.Result != "incomplete" || c.UnknownRows != 1 || num(c.Actual) != "20" {
		t.Errorf("fibre known sum 20: %+v", c)
	}
	if cell(f, "2026-09-29").Result != "met" {
		t.Errorf("fibre known sum 36: %s", cell(f, "2026-09-29").Result)
	}
	for _, k := range []string{"protein_g", "sat_fat_g", "kcal"} {
		if r := cell(pb(v, k), "2026-09-28").Result; r == "incomplete" {
			t.Errorf("%s on 2026-09-28 must be judged", k)
		}
	}
	p := pb(v, "protein_g")
	if p.MeanDays != 4 || f.MeanDays != 2 {
		t.Errorf("mean days: protein %d, fibre %d (the days with unknown fibre are no mean dates of fibre)", p.MeanDays, f.MeanDays)
	}
	s := pb(v, "sat_fat_g")
	if cell(s, "2026-09-30").Result != "missed" || cell(s, "2026-09-27").Result != "incomplete" || s.MeanDays != 2 {
		t.Errorf("sat fat: %s %s mean days %d", cell(s, "2026-09-30").Result, cell(s, "2026-09-27").Result, s.MeanDays)
	}
}

func TestW12Errors(t *testing.T) {
	h := newV7(t, weekDoc())
	for _, q := range []string{"?date=" + dateAdd("2026-10-01", -29), "?date=2026-10-02", "?date=yesterday", "?date=2026-9-1"} {
		if rec := h.get("/fuel/week" + q); rec.Code != 400 || errCode(t, rec) != "bad_input" {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	if rec := h.get("/fuel/week?date=" + dateAdd("2026-10-01", -28)); rec.Code != 200 {
		t.Errorf("28 days back: %d %s", rec.Code, rec.Body)
	}
	h.vars.failReads.Store(true)
	h.clk.Add(61 * time.Second)
	if rec := h.get("/fuel/week"); rec.Code != 502 {
		t.Errorf("failing read: %d", rec.Code)
	}
	h.vars.failReads.Store(false)
	h.setTargets(`{"schema": 3}`)
	if rec := h.get("/fuel/week"); rec.Code != 503 || errCode(t, rec) != "targets_invalid" {
		t.Errorf("invalid targets: %d %s", rec.Code, rec.Body)
	}
}

func TestW13Context(t *testing.T) {
	h := newV7(t, weekDoc())
	water := map[string]any{"kind": "drink", "volume_ml": 500.0}
	h.food("2026-09-28", "water", 0, 0, 0, 0.0, water)
	h.food("2026-09-28", "coffee", 5, 0, 0, 0.0, map[string]any{"kind": "drink", "volume_ml": 200.0, "caffeine_mg": 95.0})
	// The water of Wednesday is tagged (every lever 0), so Wednesday is covered.
	h.food("2026-09-30", "water", 0, 0, 0, 0.0, map[string]any{"kind": "drink", "volume_ml": 500.0,
		"psyllium_g": 0.0, "beta_glucan_g": 0.0, "nuts_g": 0.0, "pulses_g": 0.0, "plant_protein_g": 0.0})
	// Levers: Monday has one tagged item (30 g nuts); Tuesday has a tagged
	// and an untagged item; Wednesday has one tagged item (15 g).
	all := func(nuts float64) map[string]any {
		return map[string]any{"psyllium_g": 0.0, "beta_glucan_g": 0.0, "nuts_g": nuts, "pulses_g": 0.0, "plant_protein_g": 0.0}
	}
	h.food("2026-09-28", "walnuts", 200, 5, 2, 2.0, all(30))
	h.food("2026-09-29", "almonds", 180, 6, 1, 3.0, all(30))
	h.food("2026-09-29", "untagged bar", 200, 5, 2, 2.0, nil)
	h.food("2026-09-30", "cashews", 90, 3, 1, 1.0, all(15))
	h.restart()
	rec := h.get("/fuel/week")
	v := decode[WeekView](t, rec)
	c := v.Context
	if c.FluidsML.Week != 1200 || c.CaffeineMG.Week != 95 || c.FluidsML.Unit != "ml" || len(c.FluidsML.Last7) != 7 || c.FluidsML.Last7[3] != 700 || c.CaffeineMG.Last7[3] != 95 {
		t.Errorf("fluids %+v caffeine %+v", c.FluidsML, c.CaffeineMG)
	}
	var nuts WeekLever
	for _, l := range c.Levers {
		if l.Key == "nuts_g" {
			nuts = l
		}
	}
	// last7 = 2026-09-25 .. 2026-10-01. Monday is NOT covered: the water and
	// coffee rows of other writers carry no lever keys.
	if len(c.Levers) != 5 || nuts.Last7[3] != nil || nuts.Last7[4] != nil || num(nuts.Last7[5]) != "15" || nuts.Week != 15 || nuts.WeekCoveredDays != 1 {
		t.Errorf("nuts: %+v", nuts)
	}
	if nuts.Last7[0] != nil {
		t.Errorf("a day without items is not covered: %v", nuts.Last7[0])
	}
	keys := keysOf(mustMarshal(c))
	for _, bad := range []string{"target", "result", "status", "reference", "mean", "days_met"} {
		if keys[bad] {
			t.Errorf("context has a key named %q", bad)
		}
	}
}

func mustMarshal(v any) []byte {
	b, _ := jsonMarshal(v)
	return b
}

func TestW14PastReferenceDate(t *testing.T) {
	h := newV7(t, weekDoc())
	h.food("2026-10-01", "today", 3000, 170, 25, 10.0, nil)
	h.restart()
	v := h.week(t, "?date=2026-09-30")
	if v.Date != "2026-09-30" || v.Today != "2026-10-01" || v.Last7.From != "2026-09-24" || v.Last7.To != "2026-09-30" {
		t.Fatalf("dates: %s %s %s %s", v.Date, v.Today, v.Last7.From, v.Last7.To)
	}
	for _, b := range v.Last7.Budgets {
		for _, c := range b.Days {
			if c.Result == "open" || c.Result == "over" || c.Result == "future" {
				t.Errorf("last7 %s %s: %s", b.Key, c.Date, c.Result)
			}
		}
	}
	if v.Week.From != "2026-09-28" || states(v.Week.Days) != "ppptfff" {
		t.Errorf("week %s states %s", v.Week.From, states(v.Week.Days))
	}
	if r := cellOf(wb(v, "sat_fat_g"), "2026-10-01").Result; r != "over" {
		t.Errorf("today's cell in the week of a past R: %s", r)
	}
	if r := cellOf(wb(v, "protein_g"), "2026-10-01").Result; r != "met" {
		t.Errorf("today's protein cell: %s", r)
	}
}
