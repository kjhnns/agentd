package fuel

import (
	"strings"
	"testing"
	"time"
)

// 18.15: the energy target counts the run by distance, other sports,
// strength sessions and steps against the person's own average.

func activityOn() map[string]any {
	return map[string]any{"walk_cost_kcal_per_kg_km": 0.5, "step_length_m": 0.75, "run_cadence_spm": 165,
		"strength_session_kcal": 200, "baseline_days": 28, "min_baseline_days": 7}
}

func provisionalActivityDoc() string {
	return v2("reference_mass_kg", 80, "energy.run_cost_kcal_per_kg_km", 1.0, "energy.activity", activityOn())
}

// stepsInput gives each date of the map its steps, and the strength dates.
func stepsInput(steps map[string]int, strength ...string) *activityInput {
	return &activityInput{
		steps: func(d string) (int, bool) { n, ok := steps[d]; return n, ok },
		session: func(d string) (bool, bool) {
			for _, s := range strength {
				if s == d {
					return true, true
				}
			}
			return false, true
		},
	}
}

// baselineSteps is 8000 steps on each of the 7 dates before 2026-10-01.
func baselineSteps() map[string]int {
	m := map[string]int{}
	for i := 1; i <= 7; i++ {
		m[dateAdd("2026-10-01", -i)] = 8000
	}
	return m
}

func TestActivityProvisionalTarget(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	tg := parseT(t, provisionalActivityDoc())
	run := func(km, minutes, cadence float64) Activity {
		return Activity{SportType: "Run", MovingS: minutes * 60, DistanceM: km * 1000, AvgCadence: cadence, LocalDate: "2026-10-01"}
	}
	for _, c := range []struct {
		name  string
		acts  []Activity
		steps map[string]int
		str   []string
		want  string
		basis string
	}{
		{"rest day, no steps", nil, nil, nil, "2600", "Provisional. 2600 rest"},
		{"5 km run", []Activity{run(5, 25, 0)}, nil, nil, "3000", "+ 400 run"},
		{"15 km run", []Activity{run(15, 75, 0)}, nil, nil, "3800", "+ 1200 run"},
		{"ride without power: MET 7 for 60 min", []Activity{{SportType: "Ride", MovingS: 3600, LocalDate: "2026-10-01"}}, nil, nil, "3100", "+ 500 other sport"},
		{"ride with power: kJ", []Activity{{SportType: "Ride", MovingS: 3600, Kilojoules: 700, LocalDate: "2026-10-01"}}, nil, nil, "3300", "+ 700 other sport"},
		{"a walk is in the steps, not a sport", []Activity{{SportType: "Walk", MovingS: 3600, DistanceM: 5000, LocalDate: "2026-10-01"}}, nil, nil, "2600", ""},
		{"strength session", nil, nil, []string{"2026-10-01"}, "2800", "+ 200 strength"},
		{"14000 steps against 8000", nil, merge(baselineSteps(), "2026-10-01", 14000), nil, "2780", "+ 180 steps (14000, average 8000)"},
		{"4000 steps on a past date lower it", nil, merge(baselineSteps(), "2026-10-01", 4000), nil, "2480", "- 120 steps"},
		// 25 min at cadence 83 (per leg) = 4150 run steps: 12150 steps are 8000 walking.
		{"run steps are not walking", []Activity{run(5, 25, 83)}, merge(baselineSteps(), "2026-10-01", 12150), nil, "3000", ""},
		{"too few dates for an average", nil, map[string]int{"2026-09-30": 8000, "2026-10-01": 20000}, nil, "2600", ""},
	} {
		p := planDay(tg, "2026-10-01", "2026-10-02", c.acts, fresh, now, stepsInput(c.steps, c.str...))
		if p.EnergyState != "provisional" || num(p.EnergyTarget) != c.want || p.EnergyBasis == nil || !strings.Contains(*p.EnergyBasis, c.basis) {
			t.Errorf("%s: %s %s %v, want %s %q", c.name, p.EnergyState, num(p.EnergyTarget), deref(p.EnergyBasis), c.want, c.basis)
		}
	}
	// Today: partial steps never lower the target; steps above the average raise it.
	low := planDay(tg, "2026-10-01", "2026-10-01", nil, fresh, now, stepsInput(merge(baselineSteps(), "2026-10-01", 3000)))
	high := planDay(tg, "2026-10-01", "2026-10-01", nil, fresh, now, stepsInput(merge(baselineSteps(), "2026-10-01", 10000)))
	if num(low.EnergyTarget) != "2600" || num(high.EnergyTarget) != "2660" {
		t.Errorf("today: %s %s, want 2600 2660", num(low.EnergyTarget), num(high.EnergyTarget))
	}
	// A future date keeps the rest value and has no activity.
	if p := planDay(tg, "2026-10-03", "2026-10-02", nil, fresh, now, stepsInput(baselineSteps())); num(p.EnergyTarget) != "2600" || p.Activity != nil {
		t.Errorf("future: %s %v", num(p.EnergyTarget), p.Activity)
	}
	// Without a run cost factor the activity is off: no default for it.
	noK := parseT(t, v2("reference_mass_kg", 80, "energy.activity", activityOn()))
	if p := planDay(noK, "2026-10-01", "2026-10-02", []Activity{run(15, 75, 0)}, fresh, now, nil); num(p.EnergyTarget) != "3000" || p.Activity != nil {
		t.Errorf("no run cost: %s", num(p.EnergyTarget))
	}
	// Without energy.activity the provisional switch stays (18.5).
	plain := parseT(t, v2("reference_mass_kg", 80, "energy.run_cost_kcal_per_kg_km", 1.0))
	if p := planDay(plain, "2026-10-01", "2026-10-02", []Activity{run(15, 75, 0)}, fresh, now, nil); num(p.EnergyTarget) != "3000" || p.Activity != nil {
		t.Errorf("no activity key: %s", num(p.EnergyTarget))
	}
}

func TestActivityFormulaIsRelative(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	tg := parseT(t, v2("energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0, "energy.activity", activityOn()))
	ride := func(d string) Activity { return Activity{SportType: "Ride", MovingS: 3600, LocalDate: d} }
	// No run (-480), a ride with none in the 28 days before (+500, rounded), 14000 steps (+180).
	p := planDay(tg, "2026-10-01", "2026-10-02", []Activity{ride("2026-10-01")}, fresh, now, stepsInput(merge(baselineSteps(), "2026-10-01", 14000)))
	if p.EnergyState != "formula" || num(p.EnergyTarget) != "3200" || !strings.Contains(deref(p.EnergyBasis), "+ 500 other sport") {
		t.Errorf("formula: %s %s %q", p.EnergyState, num(p.EnergyTarget), deref(p.EnergyBasis))
	}
	// A ride on every date: maintenance already holds it, so it adds nothing.
	var every []Activity
	for i := 0; i <= 28; i++ {
		every = append(every, ride(dateAdd("2026-10-01", -i)))
	}
	if p := planDay(tg, "2026-10-01", "2026-10-02", every, fresh, now, nil); num(p.EnergyTarget) != "2520" {
		t.Errorf("daily ride: %s, want 2520", num(p.EnergyTarget))
	}
}

// The strength average counts only the dates whose data were read.
func TestActivityStrengthAverageOverKnownDates(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 0, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	tg := parseT(t, v2("energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0, "energy.activity", activityOn()))
	// A session on every one of the 7 known dates before; the other 21 are unknown.
	ax := &activityInput{session: func(d string) (bool, bool) {
		if d >= "2026-09-24" {
			return true, true
		}
		return false, false
	}}
	// Average 200 per day, a session today: +0. Unknown dates counted as none would give +150.
	if p := planDay(tg, "2026-10-01", "2026-10-02", nil, now.Add(-time.Hour), now, ax); num(p.EnergyTarget) != "3000" {
		t.Errorf("strength average: %s, want 3000", num(p.EnergyTarget))
	}
}

func TestActivityConfigIsChecked(t *testing.T) {
	for _, bad := range []map[string]any{
		{"walk_cost_kcal_per_kg_km": 2},
		{"baseline_days": 3},
		{"min_baseline_days": 40, "baseline_days": 28},
		{"step_length_m": nil},
		{"extra": 1},
	} {
		a := activityOn()
		for k, v := range bad {
			a[k] = v
		}
		if _, err := ParseTargets([]byte(v2("energy.activity", a))); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if tg := parseT(t, v2("energy.activity", nil)); tg.V2.Energy.Activity != nil {
		t.Error("null activity must be off")
	}
}

func TestStepsRouteFeedsTheSnapshot(t *testing.T) {
	h := newV7(t, provisionalActivityDoc())
	days := []map[string]any{}
	for i := 1; i <= 7; i++ {
		days = append(days, map[string]any{"date": dateAdd("2026-10-01", -i), "steps": 8000})
	}
	days = append(days, map[string]any{"date": "2026-10-01", "steps": 12000})
	if rec := h.post("/fuel/steps", map[string]any{"days": days}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stored":8`) {
		t.Fatalf("steps: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []map[string]any{
		{"days": []any{map[string]any{"date": "2026-10-03", "steps": 1}}},
		{"days": []any{map[string]any{"date": "2026-10-01", "steps": -1}}},
		{"days": []any{map[string]any{"date": "2026-10-01"}}},
		{"days": []any{}},
	} {
		if rec := h.post("/fuel/steps", bad); rec.Code != 400 {
			t.Errorf("%v: %d, want 400", bad, rec.Code)
		}
	}
	s := h.snap(t, "")
	a := s.Energy.Activity
	// 4000 steps over 8000 at 0.03 kcal each: +120.
	if a == nil || a.Steps == nil || *a.Steps != 12000 || a.StepsAverage == nil || *a.StepsAverage != 8000 || a.StepsKcal != 120 || num(s.Energy.Target) != "2720" {
		t.Fatalf("snapshot energy: %+v target %s", a, num(s.Energy.Target))
	}
	// A count sent again replaces the first one, also after a restart.
	h.post("/fuel/steps", map[string]any{"days": []any{map[string]any{"date": "2026-10-01", "steps": 14000}}})
	h.restart()
	if s := h.snap(t, ""); s.Energy.Activity == nil || *s.Energy.Activity.Steps != 14000 || num(s.Energy.Target) != "2780" {
		t.Errorf("after restart: %+v %s", s.Energy.Activity, num(s.Energy.Target))
	}
}

func merge(m map[string]int, date string, n int) map[string]int {
	out := map[string]int{date: n}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
