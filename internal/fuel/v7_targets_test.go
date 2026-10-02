package fuel

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// T3 (migration): the schema 2 file built from the production values with
// every decision null.
func TestT3MigrationFileWithEveryDecisionNull(t *testing.T) {
	h := newV7(t, v2())
	h.strava("2026-10-01-walk", "Walk", 600, 0, "2026-10-01T07:00:00", h.clk.Now()) // fresh Strava, no training
	s := h.snap(t, "")
	for _, c := range []struct {
		key    string
		kind   string
		target string
	}{{"protein_g", "floor", "160"}, {"sat_fat_g", "cap", "20"}, {"fiber_g", "floor", "35"}, {"kcal", "pace", "2600"},
		{"carbs_g", "floor", "null"}, {"caffeine_mg", "cap", "400"}, {"alcohol_g_week", "cap", "30"}} {
		b := budget(s, c.key)
		if b.Kind != c.kind || num(b.Target) != c.target {
			t.Errorf("budget %s: kind %s target %s, want %s %s", c.key, b.Kind, num(b.Target), c.kind, c.target)
		}
	}
	if len(s.Budgets) != 7 {
		t.Fatalf("budgets: %d, want 7", len(s.Budgets))
	}
	if !budget(s, "kcal").Provisional || budget(s, "protein_g").Provisional {
		t.Error("only kcal is provisional")
	}
	if num(macro(s, "net_carbs_g").Target) != "null" {
		t.Error("net carbs target must be null under schema 2")
	}
	for _, m := range s.Intake {
		if m.Key == "fluids_ml" && m.Target != nil {
			t.Error("fluids target must be null")
		}
	}
	if s.Energy.State != "provisional" || s.Energy.Calibration.State != "off" || !has(s.Energy.Calibration.BlockedBy, "calibration settings not set") {
		t.Errorf("energy: %+v", s.Energy)
	}
	if s.DayClass != "unknown" || !has(s.Missing, "day_class_unset") {
		t.Errorf("day class %q missing %v", s.DayClass, s.Missing)
	}
	if len(s.Levers) != 5 {
		t.Fatalf("levers: %d", len(s.Levers))
	}
	for _, l := range s.Levers {
		if l.Reference != nil || l.Avg7 != nil {
			t.Errorf("lever %s: reference and avg7 must be null", l.Key)
		}
	}
	if s.Wire != 7 || s.Strength.Rule != "legacy" || s.Strength.Week != nil {
		t.Errorf("wire %d strength %+v", s.Wire, s.Strength)
	}
	// The 3000 kcal training value on a training day (the v6 rule).
	h.strava("2026-10-01-run", "Run", 3000, 10000, "2026-10-01T08:00:00", h.clk.Now())
	if s2 := h.snap(t, ""); num(budget(s2, "kcal").Target) != "3000" || s2.DayType != "training" {
		t.Errorf("training day: %s %s", num(budget(s2, "kcal").Target), s2.DayType)
	}
}

// T4 (parser): unknown keys, bounds and required keys of schema 2.
func TestT4SchemaTwoParser(t *testing.T) {
	if _, err := ParseTargets([]byte(v2())); err != nil {
		t.Fatalf("the base file must parse: %v", err)
	}
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	with := func(m map[string]any, k string, v any) map[string]any {
		c := map[string]any{}
		for kk, vv := range m {
			c[kk] = vv
		}
		if v == nil && k != "" {
			delete(c, k)
		} else if k != "" {
			c[k] = v
		}
		return c
	}
	strength := map[string]any{"set_variables": []any{"Push ups"}, "session_min_sets": 3, "strava_sport_types": []any{"WeightTraining"}}
	inst := map[string]any{"text": "As agreed.", "set_by": "Dr. X", "set_on": "2026-10-01"}
	bad := map[string][]any{
		"schema 3":                    {"schema", 3},
		"unknown top key":             {"extra", 1},
		"unknown carbs key":           {"carbs_g", map[string]any{"kind": "floor", "unit": "total", "g_per_kg": map[string]any{"low": nil, "moderate": nil, "long": nil}, "class_minutes": nil, "x": 1}},
		"unknown g_per_kg key":        {"carbs_g.g_per_kg", map[string]any{"low": nil, "moderate": nil, "long": nil, "x": 1}},
		"unknown energy key":          {"energy.x", 1},
		"unknown levers key":          {"levers.x", 1},
		"unknown reference key":       {"levers.reference.x", 1},
		"unknown clinician key":       {"clinician.x", 1},
		"unknown bp key":              {"clinician.bp.x", nil},
		"unknown strength key":        {"strength", with(strength, "x", 1)},
		"strength missing key":        {"strength", with(strength, "strava_sport_types", nil)},
		"strength min sets 0":         {"strength", with(strength, "session_min_sets", 0)},
		"strength name twice":         {"strength", with(strength, "set_variables", []any{"Push ups", "Push ups"})},
		"mass out of range":           {"reference_mass_kg", 30},
		"g_per_kg order":              {"carbs_g.g_per_kg", map[string]any{"low": 5, "moderate": 3, "long": nil}, "carbs_g.class_minutes", map[string]any{"moderate": 45, "long": 90}},
		"g_per_kg above 12":           {"carbs_g.g_per_kg", map[string]any{"low": 13, "moderate": nil, "long": nil}},
		"moderate without minutes":    {"carbs_g.g_per_kg", map[string]any{"low": 3, "moderate": 5, "long": nil}},
		"class minutes order":         {"carbs_g.class_minutes", map[string]any{"moderate": 90, "long": 45}},
		"class minutes above 600":     {"carbs_g.class_minutes", map[string]any{"moderate": 45, "long": 700}},
		"deficit above 500":           {"energy.deficit_kcal", 600},
		"run cost out of range":       {"energy.run_cost_kcal_per_kg_km", 2},
		"density out of range":        {"energy.energy_density_kcal_per_kg", 4000},
		"maintenance missing key":     {"energy.maintenance", with(maint, "adopted_on", nil)},
		"maintenance unknown key":     {"energy.maintenance", with(maint, "x", 1)},
		"maintenance kcal low":        {"energy.maintenance", with(maint, "kcal", 1000)},
		"maintenance window short":    {"energy.maintenance", with(maint, "window", []any{"2026-09-01", "2026-09-05"})},
		"maintenance adopted early":   {"energy.maintenance", with(maint, "adopted_on", "2026-09-10")},
		"cross-field energy rule":     {"energy.maintenance", with(with(maint, "kcal", 2000), "avg_run_km_per_day", 14), "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0},
		"calibration missing key":     {"energy.calibration", with(calibSettings("2026-09-01"), "drift_days", nil)},
		"calibration min days 10":     {"energy.calibration", with(calibSettings("2026-09-01"), "min_days", 10)},
		"calibration bad break date":  {"energy.calibration", with(calibSettings("2026-09-01"), "break_dates", []any{"2026-13-01"})},
		"lever reference 0":           {"levers.reference.nuts_g", 0},
		"avg min covered days 8":      {"levers.avg_min_covered_days", 8},
		"estimator missing key":       {"levers.estimator", map[string]any{"beta_glucan_g_per_100": map[string]any{"rolled_oats": 4, "oat_bran": 7, "barley": 4, "oat_drink": 0.4}}},
		"estimator factor above 20":   {"levers.estimator", map[string]any{"beta_glucan_g_per_100": map[string]any{"rolled_oats": 40, "oat_bran": 7, "barley": 4, "oat_drink": 0.4}, "pulses_dry_to_cooked": 2.5}},
		"estimator dry to cooked 5":   {"levers.estimator", map[string]any{"beta_glucan_g_per_100": map[string]any{"rolled_oats": 4, "oat_bran": 7, "barley": 4, "oat_drink": 0.4}, "pulses_dry_to_cooked": 5}},
		"brew method":                 {"levers.coffee_default_brew_method", "cold"},
		"instruction without text":    {"clinician.hard_sets", with(inst, "text", "")},
		"instruction unknown key":     {"clinician.hard_sets", with(inst, "x", 1)},
		"instruction two paragraphs":  {"clinician.hard_sets", with(inst, "text", "a\nb")},
		"symptom rule bad category":   {"clinician.symptom_review_rules", []any{with(inst, "category", "headache")}},
		"symptom rule category twice": {"clinician.symptom_review_rules", []any{with(inst, "category", "mood"), with(inst, "category", "mood")}},
	}
	for name, kv := range bad {
		if _, err := ParseTargets([]byte(v2(kv...))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A schema 2 key is required.
	m := v2Base()
	delete(m, "levers")
	b, _ := json.Marshal(m)
	if _, err := ParseTargets(b); err == nil {
		t.Error("a missing schema 2 key was accepted")
	}
	// No `schema` = the v6 parse: a schema 2 key is then an unknown key.
	m = v2Base()
	delete(m, "schema")
	b, _ = json.Marshal(m)
	if _, err := ParseTargets(b); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("schema 1 with schema 2 keys: %v", err)
	}
	if tg := parseT(t, goldenTargetsV1); tg.V2 != nil || tg.WaterML == nil {
		t.Error("a file without schema is schema 1, water target kept")
	}
	// Valid forms.
	good := [][]any{
		{"energy.maintenance", maint, "energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0},
		{"strength", strength},
		{"clinician.hard_sets", inst, "clinician.symptom_review_rules", []any{with(inst, "category", "mood")}},
		{"energy.calibration", calibSettings("2026-09-01")},
	}
	for i, kv := range good {
		if _, err := ParseTargets([]byte(v2(kv...))); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	// water_ml in a schema 2 file is ignored with a log line.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	tg, err := ParseTargets([]byte(v2("water_ml", map[string]any{"kind": "floor", "rest": 2500, "training": 3000})))
	log.SetOutput(os.Stderr)
	if err != nil || tg.WaterML != nil || !strings.Contains(buf.String(), "water_ml is ignored") {
		t.Errorf("water_ml: err %v, target %v, log %q", err, tg != nil && tg.WaterML != nil, buf.String())
	}
	// An invalid file answers 503 targets_invalid.
	h := newV7(t, v2("schema", 3))
	if rec := h.get("/fuel/snapshot"); rec.Code != 503 || errCode(t, rec) != "targets_invalid" {
		t.Errorf("invalid file: %d %s", rec.Code, rec.Body)
	}
}

// T5 (carbohydrate): the target by day class, and the staple carbs basis.
func TestT5CarbohydrateByDayClass(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	tg := parseT(t, v2(carbsOn()...))
	act := func(min float64) []Activity {
		if min == 0 {
			return nil
		}
		return []Activity{{SportType: "Run", MovingS: min * 60, DistanceM: 1000, LocalDate: "2026-10-01"}}
	}
	for _, c := range []struct {
		min    float64
		target string
		class  string
	}{{0, "240", "low"}, {44, "240", "low"}, {45, "400", "moderate"}, {89, "400", "moderate"}, {90, "480", "long"}} {
		p := planDay(tg, "2026-10-01", "2026-10-01", act(c.min), fresh, now)
		if num(p.CarbsTarget) != c.target || p.DayClass != c.class {
			t.Errorf("%g min: %s %s, want %s %s", c.min, num(p.CarbsTarget), p.DayClass, c.target, c.class)
		}
	}
	// Two activities add up: 30 + 30 min = moderate.
	two := []Activity{{SportType: "Run", MovingS: 1800, LocalDate: "2026-10-01"}, {SportType: "Swim", MovingS: 1800, LocalDate: "2026-10-01"}}
	if p := planDay(tg, "2026-10-01", "2026-10-01", two, fresh, now); p.DayClass != "moderate" {
		t.Errorf("two activities: %s", p.DayClass)
	}
	// Stale Strava: unknown, the low value, strava_stale.
	stale := now.Add(-40 * time.Hour)
	if p := planDay(tg, "2026-10-01", "2026-10-01", nil, stale, now); num(p.CarbsTarget) != "240" || p.DayClass != "unknown" || !p.Stale {
		t.Errorf("stale: %+v", p)
	}
	// A past date is never stale.
	old := []Activity{{SportType: "Ride", MovingS: 6000, LocalDate: "2026-09-29"}}
	if p := planDay(tg, "2026-09-29", "2026-10-01", old, stale, now); p.DayClass != "long" || p.Stale {
		t.Errorf("past date under stale files: %+v", p)
	}
	// class_minutes null with low 3: 240 on every day, unknown.
	unset := parseT(t, v2("reference_mass_kg", 80, "carbs_g.g_per_kg", map[string]any{"low": 3, "moderate": nil, "long": nil}))
	if p := planDay(unset, "2026-10-01", "2026-10-01", act(120), fresh, now); num(p.CarbsTarget) != "240" || p.DayClass != "unknown" || !p.ClassUnset {
		t.Errorf("class unset: %+v", p)
	}
	// low null, or the reference mass null: the target is null.
	for _, doc := range []string{v2("reference_mass_kg", 80), v2("carbs_g.g_per_kg", map[string]any{"low": 3, "moderate": nil, "long": nil})} {
		if p := planDay(parseT(t, doc), "2026-10-01", "2026-10-01", nil, fresh, now); p.CarbsTarget != nil {
			t.Errorf("target must be null: %s", num(p.CarbsTarget))
		}
	}
	// In the snapshot: the stale day has strava_stale; a null target is a skipped check.
	h := newV7(t, v2(carbsOn()...))
	s := h.snap(t, "")
	if !has(s.Missing, "strava_stale") || s.DayClass != "unknown" || num(budget(s, "carbs_g").Target) != "240" || s.BudgetScore.Of != 5 {
		t.Errorf("stale snapshot: %v %s %s of %d", s.Missing, s.DayClass, num(budget(s, "carbs_g").Target), s.BudgetScore.Of)
	}
	h2 := newV7(t, v2())
	if s := h2.snap(t, ""); s.BudgetScore.Of != 4 || budget(s, "carbs_g").Status != "on_pace" {
		t.Errorf("null carb target: of %d status %s", s.BudgetScore.Of, budget(s, "carbs_g").Status)
	}
	// Staples: carbs_basis "available" (60 g carbohydrate, 10 g fibre per 100 g).
	st := func(basis string) Staple {
		ss, err := ParseStaples([]byte(`[{"key":"bread","aliases":["bread"],"default_g":100,"carbs_basis":"` + basis + `","per_100g":{"kcal":250,"protein_g":8,"carbs_g":60,"fat_g":2,"sat_fat_g":0.5,"fiber_g":10}}]`))
		if err != nil {
			t.Fatal(err)
		}
		return ss[0]
	}
	if m := st("available").macros(100); m.Carbs.float() != 70 || m.NetCarbs.float() != 60 {
		t.Errorf("available: carbs %g net %g", m.Carbs.float(), m.NetCarbs.float())
	}
	if m := st("total").macros(100); m.Carbs.float() != 60 || m.NetCarbs.float() != 50 {
		t.Errorf("total: carbs %g net %g", m.Carbs.float(), m.NetCarbs.float())
	}
	if _, err := ParseStaples([]byte(`[{"key":"b","aliases":["b"],"default_g":100,"carbs_basis":"net","per_100g":{"kcal":1,"protein_g":1,"carbs_g":1,"fat_g":1,"sat_fat_g":1,"fiber_g":1}}]`)); err == nil {
		t.Error("an unknown carbs_basis was accepted")
	}
}

// T8 (energy formula): the target by run distance.
func TestT8EnergyFormula(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	maint := map[string]any{"kcal": 3000, "mass_kg": 80, "avg_run_km_per_day": 6, "window": []any{"2026-09-01", "2026-09-14"}, "adopted_on": "2026-09-15", "result_id": nil}
	doc := func(deficit any) string {
		return v2("energy.maintenance", maint, "energy.deficit_kcal", deficit, "energy.run_cost_kcal_per_kg_km", 1.0)
	}
	run := func(date string, km float64) Activity {
		return Activity{SportType: "Run", MovingS: 600, DistanceM: km * 1000, LocalDate: date}
	}
	tg := parseT(t, doc(0))
	for _, c := range []struct {
		km   float64
		want string
	}{{0, "2520"}, {6, "3000"}, {20, "4120"}} {
		var acts []Activity
		if c.km > 0 {
			acts = []Activity{run("2026-10-01", c.km)}
		}
		p := planDay(tg, "2026-10-01", "2026-10-01", acts, fresh, now)
		if p.EnergyState != "formula" || num(p.EnergyTarget) != c.want {
			t.Errorf("%g km: %s %s, want %s", c.km, p.EnergyState, num(p.EnergyTarget), c.want)
		}
	}
	// A 7-day fixture with 42 km sums to 21000.
	var acts []Activity
	kms := []float64{10, 0, 8, 0, 12, 0, 12}
	sum := 0.0
	for i, km := range kms {
		d := dateAdd("2026-09-21", i)
		if km > 0 {
			acts = append(acts, run(d, km))
		}
		sum += *planDay(tg, d, "2026-10-01", acts, fresh, now).EnergyTarget
	}
	if sum != 21000 {
		t.Errorf("week sum %g, want 21000", sum)
	}
	// A deficit of 300 lowers each by 300.
	if p := planDay(parseT(t, doc(300)), "2026-10-01", "2026-10-01", nil, fresh, now); num(p.EnergyTarget) != "2220" {
		t.Errorf("deficit: %s", num(p.EnergyTarget))
	}
	// Any of the three inputs null: provisional, the v6 rest / training value.
	for _, d := range []string{doc(nil), v2("energy.maintenance", maint, "energy.deficit_kcal", 0), v2("energy.deficit_kcal", 0, "energy.run_cost_kcal_per_kg_km", 1.0)} {
		p := planDay(parseT(t, d), "2026-10-01", "2026-10-01", []Activity{{SportType: "Run", MovingS: 3000, DistanceM: 10000, LocalDate: "2026-10-01"}}, fresh, now)
		if p.EnergyState != "provisional" || num(p.EnergyTarget) != "3000" {
			t.Errorf("provisional: %s %s", p.EnergyState, num(p.EnergyTarget))
		}
	}
	// Stale Strava: the average distance (adjustment 0) and the basis says so.
	p := planDay(tg, "2026-10-01", "2026-10-01", nil, now.Add(-40*time.Hour), now)
	if num(p.EnergyTarget) != "3000" || p.EnergyBasis == nil || !strings.Contains(*p.EnergyBasis, "run distance unknown") {
		t.Errorf("stale: %s %v", num(p.EnergyTarget), p.EnergyBasis)
	}
	// The snapshot: macros, budgets and energy carry the same target; the
	// basis names the parts.
	h := newV7(t, doc(0))
	h.strava("2026-10-01-run", "Run", 1200, 20000, "2026-10-01T08:00:00", h.clk.Now())
	s := h.snap(t, "")
	b := budget(s, "kcal")
	if num(b.Target) != "4120" || num(macro(s, "kcal").Target) != "4120" || num(s.Energy.Target) != "4120" || b.Provisional {
		t.Errorf("snapshot kcal: %s %s %s", num(b.Target), num(macro(s, "kcal").Target), num(s.Energy.Target))
	}
	if b.Basis == nil || *b.Basis != "3000 maintenance + 1120 run (14 km more than average) - 0 deficit" {
		t.Errorf("basis: %v", b.Basis)
	}
	if s.Energy.State != "formula" || num(s.Energy.MaintenanceKcal) != "3000" || num(s.Energy.RunAdjustKcal) != "1120" || num(s.Energy.RunKm) != "20" ||
		s.Energy.MaintenanceSource == nil || *s.Energy.MaintenanceSource != "manual" {
		t.Errorf("energy: %+v", s.Energy)
	}
}

// T16 (info links): every info value is info_url + "#" + an anchor of 18.8,
// and every anchor is an id of the published page.
func TestT16InfoLinks(t *testing.T) {
	const base = "https://site.gojoe.run/brkGEwlVPGzT6U0s"
	h := newV7(t, v2(), func(o *Options) { o.BPVar, o.SymptomVar = "Fuel e2e blood pressure", "Fuel e2e symptom log" })
	anchors := map[string]bool{}
	for _, a := range infoAnchors {
		anchors[a] = true
	}
	seen := map[string]bool{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				if s, ok := c.(string); ok && (k == "info" || strings.HasSuffix(k, "_info") || strings.HasSuffix(path, ".info")) {
					if k == "base" {
						if s != base {
							t.Errorf("%s.base = %q", path, s)
						}
						continue
					}
					if !strings.HasPrefix(s, base+"#") || !anchors[strings.TrimPrefix(s, base+"#")] {
						t.Errorf("%s.%s = %q is not a link of the 18.8 table", path, k, s)
					}
					seen[strings.TrimPrefix(s, base+"#")] = true
					continue
				}
				walk(path+"."+k, c)
			}
		case []any:
			for _, c := range x {
				walk(path, c)
			}
		}
	}
	docs := [][]byte{h.get("/fuel/snapshot").Body.Bytes(), h.get("/fuel/week").Body.Bytes()}
	for _, typ := range []string{"blood_pressure", "symptom", "waist"} {
		rec := h.get("/fuel/records?type=" + typ)
		if rec.Code != 200 {
			t.Fatalf("records %s: %d %s", typ, rec.Code, rec.Body)
		}
		docs = append(docs, rec.Body.Bytes())
	}
	for _, d := range docs {
		var v any
		if err := json.Unmarshal(d, &v); err != nil {
			t.Fatal(err)
		}
		walk("", v)
	}
	for _, a := range infoAnchors {
		if !seen[a] {
			t.Errorf("anchor %s of the table is never sent", a)
		}
	}
	// Without info_url every info value is the empty string.
	m := v2Base()
	delete(m, "info_url")
	b, _ := json.Marshal(m)
	h2 := newV7(t, string(b))
	if s := h2.snap(t, ""); budget(s, "protein_g").Info != "" || s.Info.Base != "" {
		t.Errorf("no info_url: %q %q", budget(s, "protein_g").Info, s.Info.Base)
	}
	// Every anchor is an id of the published page (when the page source is on this host).
	home, _ := os.UserHomeDir()
	page, err := os.ReadFile(home + "/clawd/state/site-pages/fuel-framework.html")
	if err != nil {
		t.Logf("page source not on this host: %v (anchor check skipped)", err)
		return
	}
	for _, a := range infoAnchors {
		if !regexp.MustCompile(`id=["']` + regexp.QuoteMeta(a) + `["']`).Match(page) {
			t.Errorf("anchor %s is not an id of the page", a)
		}
	}
}
