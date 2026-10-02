package fuel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Findings of the codex code review of v7 (round 1), each with its test.

// A calibration run that could not be stored is never accepted.
func TestCalibrationRunMustBeDurableBeforeAccept(t *testing.T) {
	h := newV7(t, calibDoc())
	calFixture(h, 14, 2800)
	h.restart()
	h.waitCalib()
	// The run file cannot be written any more.
	p := filepath.Join(h.opts.StateDir, "calibration.jsonl")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := h.vars.rows("var-food")
	h.vars.edit(rows[0]["_id"].(string), map[string]any{"kcal": 4200.0}, false) // a new candidate
	rec := h.post("/fuel/calibration/accept", map[string]any{"op_id": "82000000-0001"})
	if rec.Code != 500 || errCode(t, rec) != "internal" {
		t.Fatalf("accept of a run that is not durable: %d %s", rec.Code, rec.Body)
	}
	// Once the store works again the same op_id runs as new and accepts.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	rec = h.post("/fuel/calibration/accept", map[string]any{"op_id": "82000000-0001"})
	if rec.Code != 200 {
		t.Fatalf("accept after the store works: %d %s", rec.Code, rec.Body)
	}
	id := decode[acceptBody](t, rec).Accepted
	h.restart()
	c, ok := h.svc.calib.cands[id]
	if !ok || !h.svc.calib.accepted[id] || c.Kcal != 2900 {
		t.Errorf("the accepted candidate must be loadable after a restart: %v %+v", ok, c)
	}
}

// A void row that was deleted in Variables makes the record visible again
// after the next full read (rows are the truth).
func TestDeletedVoidRowMakesTheRecordVisibleAgain(t *testing.T) {
	h := newV7(t, v2(), recOpts)
	_, r, _ := h.record("blood_pressure", bp(120, 80), "2026-10-01T08:00:00+02:00")
	id := r.Record.RecordID
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": id}); rec.Code != 200 {
		t.Fatalf("void: %d", rec.Code)
	}
	rows := h.vars.rows("var-bp")
	h.vars.edit(rows[1]["_id"].(string), nil, true) // the void row is deleted outside
	h.clk.Add(2 * time.Minute)
	if err := h.svc.cache.RefreshBody(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 1 {
		t.Errorf("the record must be visible again: %d", len(l.Records))
	}
	// And a record whose own row was deleted outside is gone.
	h.vars.edit(rows[0]["_id"].(string), nil, true)
	h.clk.Add(2 * time.Minute)
	_ = h.svc.cache.RefreshBody(context.Background())
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 0 {
		t.Errorf("a record whose row was deleted must be gone: %d", len(l.Records))
	}
}

// POST /fuel/move moves all named items or none.
func TestMoveRouteIsAllOrNothing(t *testing.T) {
	h := newV7(t, v2())
	item := func(name, day string) string {
		h.clk.Add(time.Minute)
		b := map[string]any{"client_id": cid(), "items": []any{map[string]any{"item": name, "kcal": 100, "protein_g": 5, "carbs_g": 5, "fat_g": 5, "sat_fat_g": 1}}}
		if day != "" {
			b["day"] = day
		}
		return decode[LogResponse](t, h.post("/fuel/items", b)).Items[0].ItemID
	}
	a, b := item("apple", ""), item("bread", "yesterday")
	n := len(h.vars.rows("var-food"))
	// bread is already on the target day: the request names it, so nothing moves.
	rec := h.post("/fuel/move", map[string]any{"client_id": cid(), "day": "yesterday", "items": []any{a, b}})
	r := decode[LogResponse](t, rec)
	if rec.Code != 200 || !strings.Contains(r.Blocks[0].Text, "bread is already on Wed 30 Sep.") || !strings.Contains(r.Blocks[0].Text, "Nothing was moved.") {
		t.Fatalf("all or nothing: %d %q", rec.Code, r.Blocks[0].Text)
	}
	if got := len(h.vars.rows("var-food")); got != n {
		t.Errorf("a half move wrote %d rows", got-n)
	}
	if rec := h.post("/fuel/move", map[string]any{"client_id": cid(), "day": "yesterday", "items": []any{a}}); rec.Code != 200 || len(h.vars.rows("var-food")) != n+2 {
		t.Errorf("the single move: %d, %d rows", rec.Code, len(h.vars.rows("var-food"))-n)
	}
}

// A refresh that brings other rows for a past day, or other weigh-ins, makes
// the calibration run again.
func TestChangedInputsMarkTheCalibrationDirty(t *testing.T) {
	h := newV7(t, calibDoc())
	calFixture(h, 3, 2800)
	h.restart()
	h.waitCalib()
	h.svc.calibDirty.Store(false)
	ctx := context.Background()
	// The same rows again: nothing changed.
	_ = h.svc.cache.RefreshDay(ctx, "2026-09-30")
	_ = h.svc.cache.RefreshBody(ctx)
	if h.svc.calibDirty.Load() {
		t.Fatal("an unchanged refresh marked the calibration dirty")
	}
	rows := h.vars.rows("var-food")
	h.vars.edit(rows[0]["_id"].(string), map[string]any{"kcal": 1234.0}, false) // another writer corrects yesterday
	_ = h.svc.cache.RefreshDay(ctx, "2026-09-30")
	if !h.svc.calibDirty.Load() {
		t.Error("a changed past day did not mark the calibration dirty")
	}
	h.svc.calibDirty.Store(false)
	h.vars.add("var-body", "2026-10-01", map[string]any{"weight_kg": 79.5, "method": "withings_scale", "measured_at": "2026-10-01T06:30:00+02:00"})
	_ = h.svc.cache.RefreshBody(ctx)
	if !h.svc.calibDirty.Load() {
		t.Error("a new weigh-in did not mark the calibration dirty")
	}
}

// The deterministic routes check plausibility also without a food class.
func TestDeterministicRoutesCheckWithoutAFoodClass(t *testing.T) {
	h := newV7(t, v2())
	odd := map[string]any{"item": "mystery", "portion_g": 100, "kcal": 3000, "protein_g": 0, "carbs_g": 0, "fat_g": 0, "sat_fat_g": 0}
	r := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{odd}}))
	if len(r.Items) != 1 || r.Items[0].Check == "" {
		t.Fatalf("an impossible item without a food class must carry check: %+v", r.Items)
	}
	fine := decode[LogResponse](t, h.post("/fuel/items", map[string]any{"client_id": cid(), "items": []any{
		map[string]any{"item": "rice", "portion_g": 100, "kcal": 130, "protein_g": 2.7, "carbs_g": 28, "fat_g": 0.3, "sat_fat_g": 0.1}}}))
	rows := len(h.vars.rows("var-food"))
	h.clk.Add(time.Second)
	rev := map[string]any{"item": "rice", "portion_g": 100, "kcal": 3000, "protein_g": 0, "carbs_g": 0, "net_carbs_g": nil, "fat_g": 0, "sat_fat_g": 0, "fiber_g": 0, "food_class": "", "levers": nil}
	if rec := h.fix(cid(), fine.Items[0].ItemID, map[string]any{"revised": rev}); rec.Code != 400 || len(h.vars.rows("var-food")) != rows {
		t.Errorf("an impossible revised without a food class: %d %s", rec.Code, rec.Body)
	}
}

// Required numbers of a schema 2 file must not be null; a window has exactly two dates.
func TestSchemaTwoRequiredNumbersAreNotNull(t *testing.T) {
	with := func(m map[string]any, k string, v any) map[string]any {
		c := map[string]any{}
		for kk, vv := range m {
			c[kk] = vv
		}
		c[k] = v
		return c
	}
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	if _, err := ParseTargets([]byte(v2("energy.maintenance", maint))); err != nil {
		t.Fatalf("the base maintenance must parse (result_id may be null): %v", err)
	}
	for name, kv := range map[string][]any{
		"min_edge_weigh_days null":      {"energy.calibration", with(calibSettings("2026-09-01"), "min_edge_weigh_days", nil)},
		"max_age_days null":             {"energy.calibration", with(calibSettings("2026-09-01"), "max_age_days", nil)},
		"drift_uncertainty_factor null": {"energy.calibration", with(calibSettings("2026-09-01"), "drift_uncertainty_factor", nil)},
		"break_dates null":              {"energy.calibration", with(calibSettings("2026-09-01"), "break_dates", nil)},
		"avg_run_km_per_day null":       {"energy.maintenance", with(maint, "avg_run_km_per_day", nil)},
		"window with three dates":       {"energy.maintenance", with(maint, "window", []any{"2026-09-01", "2026-09-14", "x"})},
		"window with one date":          {"energy.maintenance", with(maint, "window", []any{"2026-09-01"})},
		"session_min_sets null":         {"strength", map[string]any{"set_variables": []any{}, "session_min_sets": nil, "strava_sport_types": []any{}}},
		"instruction set_on null":       {"clinician.hard_sets", map[string]any{"text": "x", "set_by": "y", "set_on": nil}},
		"estimator factor null":         {"levers.estimator", map[string]any{"beta_glucan_g_per_100": map[string]any{"rolled_oats": 4, "oat_bran": 7, "barley": 4, "oat_drink": 0.4}, "pulses_dry_to_cooked": nil}},
	} {
		if _, err := ParseTargets([]byte(v2(kv...))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
