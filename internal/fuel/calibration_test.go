package fuel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func calibDoc(kv ...any) string {
	return v2(append([]any{"energy.calibration", calibSettings("2026-08-01"), "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0}, kv...)...)
}

// cdays builds n days ending on D-1 (ascending).
func cdays(D string, n int, kcal func(i int) float64, w func(i int) *float64, km func(i int) float64) []calibDay {
	var out []calibDay
	for i := 0; i < n; i++ {
		d := calibDay{Date: dateAdd(D, i-n), Kcal: kcal(i)}
		if w != nil {
			d.W = w(i)
		}
		if km != nil {
			d.RunKm = km(i)
		}
		out = append(out, d)
	}
	return out
}

func flat(v float64) func(int) float64 { return func(int) float64 { return v } }
func line(w0, slope float64) func(int) *float64 {
	return func(i int) *float64 { return fptr(w0 + slope*float64(i)) }
}

const calD = "2026-10-02"

// T6 (calibration maths).
func TestT6CalibrationMaths(t *testing.T) {
	tg := parseT(t, calibDoc())
	now := time.Now()
	// 14 complete days, mean intake 2900, weights on a straight line from
	// 80.00 kg falling 0.03 kg per day, rho 7700.
	kcal := func(i int) float64 { return 2900 + float64(i%2*200-100) } // mean 2900
	run := computeCalibration(tg, calD, cdays(calD, 14, kcal, line(80, -0.03), flat(6)), true, true, now)
	r := run.Result
	if r.State != "candidate" || r.Candidate == nil {
		t.Fatalf("state %s blocked by %v", r.State, r.BlockedBy)
	}
	if math.Abs(run.MCal-3131) > 1e-6 || r.Candidate.Kcal != 3130 || r.Candidate.UncertaintyKcal != 0 || math.Abs(run.Uncertainty) > 1e-6 {
		t.Errorf("M_cal %g, wire %g, uncertainty %g", run.MCal, r.Candidate.Kcal, r.Candidate.UncertaintyKcal)
	}
	// mass = the mean of the weights, avg km = sum / N.
	if r.Candidate.MassKg != 79.8 || r.Candidate.AvgRunKm != 6 || r.Candidate.MeanIntakeKcal != 2900 || r.Candidate.WeightSlope != -0.21 || *r.RunKmPerWeek != 42 {
		t.Errorf("candidate: %+v run km per week %g", r.Candidate, *r.RunKmPerWeek)
	}
	if r.Days != 14 || r.WeighDays != 14 || *r.DaysNeeded != 0 || r.Interval[0] != "2026-09-18" || r.Interval[1] != "2026-10-01" || len(r.Candidate.ResultID) != 16 || len(*r.SettingsVersion) != 12 {
		t.Errorf("result: %+v", r)
	}
	if g := r.Gates; g == nil || !(g.G1 && g.G2 && g.G3 && g.G4 && g.G5 && g.G6) || len(r.BlockedBy) != 0 {
		t.Errorf("gates %+v blocked %v", r.Gates, r.BlockedBy)
	}
	// A flat weight gives M_cal = the mean intake.
	run = computeCalibration(tg, calD, cdays(calD, 14, kcal, line(80, 0), flat(0)), true, true, now)
	if math.Abs(run.MCal-2900) > 1e-6 {
		t.Errorf("flat weight: %g", run.MCal)
	}
	// Noisy weights: uncertainty = rho x SE(s), computed by hand here.
	ws := []float64{80.0, 81.0, 79.2, 80.6, 79.4, 80.9, 79.1, 80.5, 79.5, 80.8, 79.0, 80.4, 79.3, 79.9}
	run = computeCalibration(tg, calD, cdays(calD, 14, kcal, func(i int) *float64 { return &ws[i] }, flat(3)), true, true, now)
	var sx, sw float64
	for i, w := range ws {
		sx += float64(i)
		sw += w
	}
	mx, mw := sx/14, sw/14
	var sxx, sxy float64
	for i, w := range ws {
		sxx += (float64(i) - mx) * (float64(i) - mx)
		sxy += (float64(i) - mx) * (w - mw)
	}
	s := sxy / sxx
	var ssr float64
	for i, w := range ws {
		e := w - (mw + s*(float64(i)-mx))
		ssr += e * e
	}
	wantUnc := 7700 * math.Sqrt((ssr/12)/sxx)
	if math.Abs(run.Uncertainty-wantUnc) > 1e-6 || math.Abs(run.MCal-(2900-7700*s)) > 1e-6 {
		t.Errorf("noisy: uncertainty %g want %g, M_cal %g want %g", run.Uncertainty, wantUnc, run.MCal, 2900-7700*s)
	}
	if wantUnc <= 200 {
		t.Fatalf("the fixture must be too noisy for the 200 kcal gate: %g", wantUnc)
	}
	if run.Result.State != "blocked" || run.Result.Gates.G5 || run.Result.Candidate != nil {
		t.Errorf("noisy weights must be blocked by G5: %+v", run.Result)
	}
	// Weights on some days only: x is the day index of the interval.
	sparse := func(i int) *float64 {
		if i%4 == 3 {
			return nil
		}
		return fptr(80 - 0.03*float64(i))
	}
	run = computeCalibration(tg, calD, cdays(calD, 14, kcal, sparse, flat(0)), true, true, now)
	if math.Abs(run.MCal-3131) > 1e-6 || run.Result.WeighDays != 11 {
		t.Errorf("sparse weights: %g, weigh days %d", run.MCal, run.Result.WeighDays)
	}
}

// T7 (calibration states and gates).
func TestT7CalibrationStatesAndGates(t *testing.T) {
	now := time.Now()
	ok := func(i int) float64 { return 2800 }
	w := line(80, 0)
	state := func(doc string, days []calibDay, fresh, refreshed bool) CalibResult {
		t.Helper()
		r := computeCalibration(parseT(t, doc), calD, days, fresh, refreshed, now).Result
		if r.State != "candidate" && r.Candidate != nil {
			t.Errorf("a candidate number is on the wire in state %s", r.State)
		}
		return r
	}
	// Off: the settings, the energy density or the run cost factor not set.
	if r := state(v2(), nil, true, true); r.State != "off" || !has(r.BlockedBy, "calibration settings not set") || r.Interval != nil || r.DaysNeeded != nil || r.Gates != nil || r.SettingsVersion != nil {
		t.Errorf("off: %+v", r)
	}
	if r := state(v2("energy.calibration", calibSettings("2026-08-01"), "energy.run_cost_kcal_per_kg_km", 1.0), nil, true, true); r.State != "off" || !has(r.BlockedBy, "energy density not set") {
		t.Errorf("rho null: %+v", r)
	}
	if r := state(v2("energy.calibration", calibSettings("2026-08-01"), "energy.energy_density_kcal_per_kg", 7700), nil, true, true); r.State != "off" || !has(r.BlockedBy, "run cost factor not set") {
		t.Errorf("k null: %+v", r)
	}
	// start_date tomorrow: collecting, days 0.
	if r := state(v2("energy.calibration", calibSettings("2026-10-03"), "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0), cdays(calD, 20, ok, w, nil), true, true); r.State != "collecting" || r.Days != 0 {
		t.Errorf("start tomorrow: %+v", r)
	}
	// 13 complete days: collecting, 1 day needed.
	if r := state(calibDoc(), cdays(calD, 13, ok, w, nil), true, true); r.State != "collecting" || r.Days != 13 || *r.DaysNeeded != 1 || r.Gates != nil || r.Interval == nil {
		t.Errorf("13 days: %+v", r)
	}
	// A 20-day stretch with one day under 1500 kcal on day 8: an interval of
	// 12 days (the day breaks the interval, it is not skipped).
	low := func(i int) float64 {
		if i == 7 {
			return 1400
		}
		return 2800
	}
	if r := state(calibDoc(), cdays(calD, 20, low, w, nil), true, true); r.State != "collecting" || r.Days != 12 || r.Interval[0] != "2026-09-20" {
		t.Errorf("a low day: %+v", r)
	}
	// The same with a kcal unknown row, and with a break date.
	unk := cdays(calD, 20, ok, w, nil)
	unk[7].Unknown = 1
	if r := state(calibDoc(), unk, true, true); r.Days != 12 {
		t.Errorf("an unknown row: %d days", r.Days)
	}
	cs := calibSettings("2026-08-01")
	cs["break_dates"] = []any{dateAdd(calD, -13)}
	if r := state(v2("energy.calibration", cs, "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0), cdays(calD, 20, ok, w, nil), true, true); r.Days != 12 {
		t.Errorf("a break date: %d days", r.Days)
	}
	// 25 complete days: the interval is the last 21.
	if r := state(calibDoc(), cdays(calD, 25, ok, w, nil), true, true); r.State != "candidate" || r.Days != 21 || r.Interval[0] != "2026-09-11" || r.Interval[1] != "2026-10-01" {
		t.Errorf("25 days: %+v", r)
	}
	// An interval that ended 9 days ago: blocked by G2.
	old := func(i int) float64 {
		if i >= 20 {
			return 0
		}
		return 2800
	}
	if r := state(calibDoc(), cdays(calD, 29, old, w, nil), true, true); r.State != "blocked" || r.Gates.G2 || !r.Gates.G1 || len(r.BlockedBy) == 0 {
		t.Errorf("old interval: %+v", r)
	}
	// 9 weigh days: blocked by G3; 2 weigh days in the last 5: blocked by G3.
	nine := func(i int) *float64 {
		if i < 9 {
			return fptr(80)
		}
		return nil
	}
	if r := state(calibDoc(), cdays(calD, 14, ok, nine, nil), true, true); r.State != "blocked" || r.Gates.G3 || !strings.Contains(r.BlockedBy[0], "Only 9 weigh-ins in the 14 days; 10 are needed.") {
		t.Errorf("9 weigh days: %+v", r)
	}
	edge := func(i int) *float64 {
		if i >= 9 && i < 12 {
			return nil
		}
		return fptr(80)
	}
	if r := state(calibDoc(), cdays(calD, 14, ok, edge, nil), true, true); r.State != "blocked" || r.Gates.G3 {
		t.Errorf("2 weigh days in the last 5: %+v", r)
	}
	// Stale Strava: blocked by G4.
	if r := state(calibDoc(), cdays(calD, 14, ok, w, nil), false, true); r.State != "blocked" || r.Gates.G4 || !r.Gates.G3 {
		t.Errorf("stale: %+v", r)
	}
	// A failed read: blocked, "could not refresh the inputs".
	if r := state(calibDoc(), cdays(calD, 14, ok, w, nil), true, false); r.State != "blocked" || !has(r.BlockedBy, "could not refresh the inputs") {
		t.Errorf("failed refresh: %+v", r)
	}
	// 14 complete days at 800 kcal with flat weights and min_complete_kcal 500: blocked by G6.
	cs = calibSettings("2026-08-01")
	cs["min_complete_kcal"] = 500
	lowDoc := v2("energy.calibration", cs, "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0)
	if r := state(lowDoc, cdays(calD, 14, flat(800), w, nil), true, true); r.State != "blocked" || r.Gates.G6 || !r.Gates.G5 {
		t.Errorf("800 kcal: %+v", r)
	}
	// A candidate of 2000 kcal with mass 80, 14 km per day and k 1.0: blocked by G6 (the cross-field rule).
	if r := state(calibDoc(), cdays(calD, 14, flat(2000), w, flat(14)), true, true); r.State != "blocked" || r.Gates.G6 {
		t.Errorf("cross-field: %+v", r)
	}
	// Unrounded 3000 / 79.99 / 18.75 km: 1500.19 unrounded, but 1496 as rounded: blocked by G6.
	if r := state(calibDoc(), cdays(calD, 14, flat(3000), line(79.99, 0), flat(18.75)), true, true); r.State != "blocked" || r.Gates.G6 {
		t.Errorf("rounded cross-field: %+v", r)
	}
	if r := state(calibDoc(), cdays(calD, 14, flat(3000), line(79.99, 0), flat(18.7)), true, true); r.State != "candidate" {
		t.Errorf("just inside the rule: %+v", r)
	}
}

// calFixture puts n complete days with flat weights into the fake Variables
// and fresh Strava files. Today is 2026-10-01 (the harness clock).
func calFixture(h *harness, n int, kcal float64) {
	for i := 1; i <= n; i++ {
		d := dateAdd("2026-10-01", -i)
		h.food(d, "day", kcal, 150, 10, 30.0, nil)
		h.vars.add("var-body", d, map[string]any{"weight_kg": 80.0, "method": "withings_scale", "measured_at": d + "T06:30:00+02:00"})
	}
	h.strava("fresh", "Walk", 600, 0, "2026-10-01T07:00:00", h.clk.Now())
}

// waitCalib waits until the calibration tick of the start is over, then
// runs the calibration once, so that today's result is of the current data
// (the tick of an earlier start may have stored a result of older data).
func (h *harness) waitCalib() {
	h.t.Helper()
	for i := 0; i < 2000 && !h.svc.calibBooted.Load(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.svc.calibBooted.Load() {
		h.t.Fatal("the calibration tick of the start did not finish")
	}
	h.svc.RunCalibration(context.Background())
}

func TestT7CalibrationRefreshAndStore(t *testing.T) {
	// min_days 28, max_days 28, max_age_days 14, D = 2026-10-02: a complete
	// interval 2026-08-25 to 2026-09-21 is read in full and passes G1 and G2.
	cs := calibSettings("2026-08-01")
	cs["min_days"], cs["max_days"], cs["max_age_days"], cs["min_weigh_days"] = 28, 28, 14, 10
	h := newV7(t, v2("energy.calibration", cs, "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0))
	for d := "2026-08-25"; d <= "2026-09-21"; d = dateAdd(d, 1) {
		h.food(d, "day", 2800, 150, 10, 30.0, nil)
		h.vars.add("var-body", d, map[string]any{"weight_kg": 80.0, "method": "withings_scale", "measured_at": d + "T06:30:00+02:00"})
	}
	h.clk.Set(zurich(t, "2026-10-02 12:00"))
	h.strava("fresh", "Walk", 600, 0, "2026-10-02T07:00:00", h.clk.Now())
	h.restart()
	h.waitCalib() // the run at startup is over: the count below is of one run
	reads := h.vars.reads.Load()
	res, ok := h.svc.RunCalibration(context.Background())
	if !ok || res.State != "candidate" || res.Interval[0] != "2026-08-25" || res.Interval[1] != "2026-09-21" || !res.Gates.G1 || !res.Gates.G2 || res.Days != 28 {
		t.Fatalf("28-day interval: %+v", res)
	}
	if got := h.vars.reads.Load() - reads; got != 42 {
		t.Errorf("the run made %d per-date reads, want 42 (every day from D - 42 to D - 1)", got)
	}
	// The snapshot shows the stored result; the candidate number is only there.
	s := h.snap(t, "")
	if s.Energy.Calibration.State != "candidate" || s.Energy.Calibration.Candidate.Kcal != 2800 || s.Energy.MaintenanceKcal != nil || s.Energy.State != "provisional" ||
		!strings.HasSuffix(s.Energy.Calibration.FoodRecordInfo, "#food-record") {
		t.Errorf("snapshot: %+v", s.Energy)
	}
	// An edit of a food row inside the interval by another writer changes the next run.
	rows := h.vars.rows("var-food")
	h.vars.edit(rows[3]["_id"].(string), map[string]any{"kcal": 5600.0}, false)
	res2, _ := h.svc.RunCalibration(context.Background())
	if res2.Candidate == nil || res2.Candidate.Kcal != 2900 || res2.Candidate.ResultID == res.Candidate.ResultID {
		t.Errorf("after an edit: %+v", res2.Candidate)
	}
	// One result per date in calibration.json, every run in calibration.jsonl.
	b, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "calibration.json"))
	var f struct {
		Results map[string]calibRun `json:"results"`
	}
	_ = json.Unmarshal(b, &f)
	if r := f.Results["2026-10-02"].Result; r.Candidate == nil || r.Candidate.Kcal != 2900 {
		t.Errorf("calibration.json: %s", b)
	}
	lb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "calibration.jsonl"))
	if n := bytes.Count(lb, []byte("\n")); n < 2 {
		t.Errorf("calibration.jsonl has %d lines", n)
	}
	// A failed Variables read: blocked, and POST /fuel/calibration/run answers 502.
	h.vars.failReads.Store(true)
	if rec := h.post("/fuel/calibration/run", nil); rec.Code != 502 {
		t.Errorf("run with failed reads: %d %s", rec.Code, rec.Body)
	}
	if s := h.snap(t, ""); s.Energy.Calibration.State != "blocked" || !has(s.Energy.Calibration.BlockedBy, "could not refresh the inputs") || s.Energy.Calibration.Candidate != nil {
		t.Errorf("after a failed refresh: %+v", s.Energy.Calibration)
	}
	h.vars.failReads.Store(false)
	if rec := h.post("/fuel/calibration/run", nil); rec.Code != 200 || decode[CalibResult](t, rec).State != "candidate" {
		t.Errorf("run: %d %s", rec.Code, rec.Body)
	}
	// Nothing of the calibration goes to Variables.
	if h.vars.posts != 0 {
		t.Errorf("the calibration posted %d values", h.vars.posts)
	}
}

// T8, drift: a sequence of candidate results against the adopted value.
func TestT8Drift(t *testing.T) {
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	tg := parseT(t, calibDoc("energy.maintenance", maint, "energy.deficit_kcal", 0))
	sv := settingsVersion(tg.V2.Energy.Calibration, 7700, 1.0)
	mk := func(spec func(i int) (state string, mcal, unc float64, version string)) *calibStore {
		cs, err := openCalibStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 7; i++ {
			st, m, u, v := spec(i)
			if st == "" {
				continue
			}
			ver := v
			r := calibRun{Result: CalibResult{State: st, ForDate: dateAdd("2026-10-01", -i), SettingsVersion: &ver}, MCal: m, Uncertainty: u}
			if err := cs.putRun(r); err != nil {
				t.Fatal(err)
			}
			// Two runs on one date count once (the later one replaces).
			if i == 2 {
				_ = cs.putRun(r)
			}
		}
		return cs
	}
	all := func(m, u float64) func(int) (string, float64, float64, string) {
		return func(int) (string, float64, float64, string) { return "candidate", m, u, sv }
	}
	if !mk(all(3200, 50)).drift(tg, "2026-10-01") {
		t.Error("7 dates, all 200 above with uncertainty 50: drift")
	}
	if mk(all(3200, 120)).drift(tg, "2026-10-01") {
		t.Error("uncertainty 120: 200 is not more than 2 x 120")
	}
	if mk(func(i int) (string, float64, float64, string) {
		if i == 6 {
			return "", 0, 0, ""
		}
		return "candidate", 3200, 50, sv
	}).drift(tg, "2026-10-01") {
		t.Error("6 dates: no drift")
	}
	if mk(func(i int) (string, float64, float64, string) {
		if i == 3 {
			return "blocked", 0, 0, sv
		}
		return "candidate", 3200, 50, sv
	}).drift(tg, "2026-10-01") {
		t.Error("a blocked date in between: no drift")
	}
	if mk(func(i int) (string, float64, float64, string) {
		if i%2 == 0 {
			return "candidate", 2800, 50, sv
		}
		return "candidate", 3200, 50, sv
	}).drift(tg, "2026-10-01") {
		t.Error("mixed signs: no drift")
	}
	if !mk(all(2800, 50)).drift(tg, "2026-10-01") {
		t.Error("7 dates, all 200 below: drift")
	}
	// A settings change on date 4: no drift until 7 dates with the new version.
	if mk(func(i int) (string, float64, float64, string) {
		if i >= 3 {
			return "candidate", 3200, 50, "oldversion00"
		}
		return "candidate", 3200, 50, sv
	}).drift(tg, "2026-10-01") {
		t.Error("a settings change: no drift")
	}
	// No adopted maintenance: false.
	if mk(all(3200, 50)).drift(parseT(t, calibDoc()), "2026-10-01") {
		t.Error("no maintenance: no drift")
	}
}

type acceptBody struct {
	Accepted    string          `json:"accepted"`
	Candidate   *CalibCandidate `json:"candidate"`
	Calibration CalibResult     `json:"calibration"`
	Error       struct {
		Code string `json:"code"`
	} `json:"error"`
}

func acceptLines(t *testing.T, h *harness) (accepted, rejected int) {
	b, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "calibration.jsonl"))
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(l, `"outcome":"accepted"`):
			accepted++
		case strings.Contains(l, `"outcome":"rejected"`):
			rejected++
		}
	}
	return
}

// T8, adoption: the accept route and the load check of the targets file.
func TestT8AcceptAndAdoption(t *testing.T) {
	h := newV7(t, calibDoc())
	calFixture(h, 14, 2800)
	h.restart()
	// A candidate state: one accepted line, the candidate comes back.
	rec := h.post("/fuel/calibration/accept", map[string]any{"op_id": "80000000-0001"})
	if rec.Code != 200 {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body)
	}
	a := decode[acceptBody](t, rec)
	if a.Candidate == nil || a.Accepted != a.Candidate.ResultID || a.Candidate.Kcal != 2800 || a.Candidate.MassKg != 80 {
		t.Fatalf("accept body: %s", rec.Body)
	}
	if acc, rej := acceptLines(t, h); acc != 1 || rej != 0 {
		t.Fatalf("lines: %d accepted, %d rejected", acc, rej)
	}
	A := *a.Candidate
	// An input of the interval changes: the next run gives B.
	rows := h.vars.rows("var-food")
	h.vars.edit(rows[0]["_id"].(string), map[string]any{"kcal": 4200.0}, false)
	resB, _ := h.svc.RunCalibration(context.Background())
	if resB.Candidate == nil || resB.Candidate.ResultID == A.ResultID || resB.Candidate.Kcal != 2900 {
		t.Fatalf("B: %+v", resB.Candidate)
	}
	// The same op_id again: the same answer, nothing appended, also when a
	// newer candidate B exists by then, and also after a restart.
	rec2 := h.post("/fuel/calibration/accept", map[string]any{"op_id": "80000000-0001"})
	if rec2.Code != 200 || rec2.Body.String() != rec.Body.String() {
		t.Errorf("replay: %d %s", rec2.Code, rec2.Body)
	}
	h.restart()
	rec3 := h.post("/fuel/calibration/accept", map[string]any{"op_id": "80000000-0001"})
	if rec3.Code != 200 || rec3.Body.String() != rec.Body.String() {
		t.Errorf("replay after a restart: %d %s", rec3.Code, rec3.Body)
	}
	if acc, rej := acceptLines(t, h); acc != 1 || rej != 0 {
		t.Errorf("after the replays: %d accepted, %d rejected", acc, rej)
	}
	// A new op_id accepts B (the refreshed run), not the older A.
	recB := h.post("/fuel/calibration/accept", map[string]any{"op_id": "80000000-0002"})
	if b := decode[acceptBody](t, recB); recB.Code != 200 || b.Accepted != resB.Candidate.ResultID {
		t.Errorf("accept B: %d %s", recB.Code, recB.Body)
	}

	// The targets file: a maintenance object with the result_id of the
	// accepted candidate and the same numbers loads.
	maintOf := func(c CalibCandidate, id any) map[string]any {
		return map[string]any{"kcal": c.Kcal, "mass_kg": c.MassKg, "avg_run_km_per_day": c.AvgRunKm, "window": []any{c.Interval[0], c.Interval[1]}, "adopted_on": "2026-10-01", "result_id": id}
	}
	h.setTargets(calibDoc("energy.maintenance", maintOf(A, A.ResultID), "energy.deficit_kcal", 0))
	s := h.snap(t, "")
	if s.Energy.State != "formula" || num(s.Energy.MaintenanceKcal) != "2800" || *s.Energy.MaintenanceSource != "calibration" {
		t.Errorf("adopted A: %+v", s.Energy)
	}
	// A reload and a restart on the same date and on a later date load A,
	// although the newest candidate is B.
	h.restart()
	if s := h.snap(t, ""); s.Energy.State != "formula" {
		t.Errorf("after a restart: %s", s.Energy.State)
	}
	h.clk.Add(48 * time.Hour)
	h.strava("fresh", "Walk", 600, 0, "2026-10-03T07:00:00", h.clk.Now())
	h.restart()
	if rec := h.get("/fuel/snapshot"); rec.Code != 200 || decode[Snapshot](t, rec).Energy.State != "formula" {
		t.Errorf("on a later date: %d", rec.Code)
	}
	// A settings change does the same.
	cs := calibSettings("2026-08-01")
	cs["drift_kcal"] = 100
	h.setTargets(v2("energy.calibration", cs, "energy.energy_density_kcal_per_kg", 7700, "energy.run_cost_kcal_per_kg_km", 1.0, "energy.maintenance", maintOf(A, A.ResultID), "energy.deficit_kcal", 0))
	if rec := h.get("/fuel/snapshot"); rec.Code != 200 {
		t.Errorf("after a settings change: %d %s", rec.Code, rec.Body)
	}
	// A changed kcal with that result_id = targets_invalid.
	bad := A
	bad.Kcal = 2810
	h.setTargets(calibDoc("energy.maintenance", maintOf(bad, A.ResultID), "energy.deficit_kcal", 0))
	if rec := h.get("/fuel/snapshot"); rec.Code != 503 || errCode(t, rec) != "targets_invalid" {
		t.Errorf("changed kcal: %d %s", rec.Code, rec.Body)
	}
	// An unknown result_id = targets_invalid.
	h.setTargets(calibDoc("energy.maintenance", maintOf(A, "0123456789abcdef"), "energy.deficit_kcal", 0))
	if rec := h.get("/fuel/snapshot"); rec.Code != 503 {
		t.Errorf("unknown result_id: %d", rec.Code)
	}
	// result_id null loads with maintenance_source "manual".
	h.setTargets(calibDoc("energy.maintenance", maintOf(A, nil), "energy.deficit_kcal", 0))
	if s := h.snap(t, ""); *s.Energy.MaintenanceSource != "manual" {
		t.Errorf("manual: %+v", s.Energy.MaintenanceSource)
	}
}

func TestT8AcceptRejectedAndConcurrent(t *testing.T) {
	h := newV7(t, calibDoc())
	calFixture(h, 13, 2800) // collecting: one day short
	h.restart()
	rec := h.post("/fuel/calibration/accept", map[string]any{"op_id": "81000000-0001"})
	a := decode[acceptBody](t, rec)
	if rec.Code != 409 || a.Error.Code != "not_a_candidate" || a.Calibration.State != "collecting" || a.Candidate != nil {
		t.Fatalf("accept while collecting: %d %s", rec.Code, rec.Body)
	}
	if acc, rej := acceptLines(t, h); acc != 0 || rej != 1 {
		t.Fatalf("lines: %d accepted, %d rejected", acc, rej)
	}
	// The state becomes candidate (a 14th day); the rejected op_id still
	// returns the stored 409, also after a restart.
	d := dateAdd("2026-10-01", -14)
	h.food(d, "day", 2800, 150, 10, 30.0, nil)
	h.vars.add("var-body", d, map[string]any{"weight_kg": 80.0, "method": "withings_scale", "measured_at": d + "T06:30:00+02:00"})
	h.restart()
	res, _ := h.svc.RunCalibration(context.Background())
	if res.State != "candidate" {
		t.Fatalf("now a candidate: %+v", res)
	}
	if rec2 := h.post("/fuel/calibration/accept", map[string]any{"op_id": "81000000-0001"}); rec2.Code != 409 || rec2.Body.String() != rec.Body.String() {
		t.Errorf("a rejected op_id replayed: %d %s", rec2.Code, rec2.Body)
	}
	// A candidate that was stored but never accepted does not load: a
	// rejected line alone does not make a result_id loadable.
	c := *res.Candidate
	m := map[string]any{"kcal": c.Kcal, "mass_kg": c.MassKg, "avg_run_km_per_day": c.AvgRunKm, "window": []any{c.Interval[0], c.Interval[1]}, "adopted_on": "2026-10-01", "result_id": c.ResultID}
	h.setTargets(calibDoc("energy.maintenance", m, "energy.deficit_kcal", 0))
	if rec := h.get("/fuel/snapshot"); rec.Code != 503 {
		t.Errorf("a candidate without an accepted line loaded: %d", rec.Code)
	}
	h.setTargets(calibDoc())
	// Two concurrent requests with one op_id: one line, two equal answers.
	var wg sync.WaitGroup
	out := make([]string, 2)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := h.post("/fuel/calibration/accept", map[string]any{"op_id": "81000000-0002"})
			out[i] = fmt.Sprintf("%d %s", r.Code, r.Body)
		}(i)
	}
	wg.Wait()
	if out[0] != out[1] || !strings.HasPrefix(out[0], "200 ") {
		t.Errorf("concurrent accepts differ:\n%s\n%s", out[0], out[1])
	}
	if acc, rej := acceptLines(t, h); acc != 1 || rej != 1 {
		t.Errorf("lines: %d accepted, %d rejected", acc, rej)
	}
	// An input changed since the last run so that the refreshed run is no
	// candidate: 409, one more rejected line, no accepted line.
	rows := h.vars.rows("var-food")
	h.vars.edit(rows[2]["_id"].(string), map[string]any{"kcal": 100.0}, false)
	if rec := h.post("/fuel/calibration/accept", map[string]any{"op_id": "81000000-0003"}); rec.Code != 409 {
		t.Errorf("accept after the input changed: %d %s", rec.Code, rec.Body)
	}
	if acc, rej := acceptLines(t, h); acc != 1 || rej != 2 {
		t.Errorf("lines: %d accepted, %d rejected", acc, rej)
	}
	if rec := h.post("/fuel/calibration/accept", map[string]any{"op_id": "x"}); rec.Code != 400 {
		t.Errorf("a bad op_id: %d", rec.Code)
	}
}
