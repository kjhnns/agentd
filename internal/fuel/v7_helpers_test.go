package fuel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- v7 test helpers (spec 18.11 and 19.9) ----

// v2Base is a schema 2 targets file with every decision null: the file that
// 18.2 step 5 builds from the production values (T3).
func v2Base() map[string]any {
	return map[string]any{
		"schema":            2,
		"protein_g":         map[string]any{"kind": "floor", "value": 160},
		"sat_fat_g":         map[string]any{"kind": "cap", "value": 20},
		"fiber_g":           map[string]any{"kind": "floor", "value": 35},
		"kcal":              map[string]any{"kind": "pace", "rest": 2600, "training": 3000},
		"net_carbs_g":       map[string]any{"kind": "pace", "rest": nil, "training": nil},
		"caffeine_mg":       map[string]any{"kind": "cap", "value": 400},
		"alcohol_g_week":    map[string]any{"kind": "cap", "value": 30},
		"strength_per_week": 3,
		"weight_band_kg":    []any{79, 81},
		"eating_window":     map[string]any{"start": "07:00", "end": "20:30"},
		"tz":                "Europe/Zurich",
		"reference_mass_kg": nil,
		"carbs_g": map[string]any{"kind": "floor", "unit": "total",
			"g_per_kg": map[string]any{"low": nil, "moderate": nil, "long": nil}, "class_minutes": nil},
		"energy": map[string]any{"maintenance": nil, "deficit_kcal": nil, "run_cost_kcal_per_kg_km": nil,
			"energy_density_kcal_per_kg": nil, "calibration": nil},
		"levers": map[string]any{
			"reference": map[string]any{"psyllium_g": nil, "beta_glucan_g": nil, "nuts_g": nil, "pulses_g": nil, "plant_protein_g": nil},
			"estimator": nil, "avg_min_covered_days": nil, "coffee_default_brew_method": nil},
		"strength": nil,
		"clinician": map[string]any{"bp": map[string]any{"home": nil, "office": nil, "ambulatory": nil},
			"symptom_review_rules": []any{}, "hard_sets": nil, "strength_test_protocol": nil},
		"info_url": "https://site.gojoe.run/brkGEwlVPGzT6U0s",
	}
}

// at sets a nested key ("energy.calibration") of a targets map.
func at(m map[string]any, path string, v any) {
	keys := splitDots(path)
	for _, k := range keys[:len(keys)-1] {
		m = m[k].(map[string]any)
	}
	m[keys[len(keys)-1]] = v
}

func splitDots(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// v2 builds a schema 2 file with changes: v2("reference_mass_kg", 80, ...).
func v2(kv ...any) string {
	m := v2Base()
	for i := 0; i+1 < len(kv); i += 2 {
		at(m, kv[i].(string), kv[i+1])
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// calibSettings are the T7 settings.
func calibSettings(start string) map[string]any {
	return map[string]any{"start_date": start, "min_days": 14, "max_days": 21, "min_complete_kcal": 1500, "min_weigh_days": 10,
		"edge_days": 5, "min_edge_weigh_days": 3, "max_uncertainty_kcal": 200, "max_age_days": 7, "drift_kcal": 150,
		"drift_uncertainty_factor": 2, "drift_days": 7, "break_dates": []any{}}
}

// carbsOn are the T5 carbohydrate settings (80 kg, 3 / 5 / 6 g/kg, 45 / 90 min).
func carbsOn() []any {
	return []any{"reference_mass_kg", 80, "carbs_g.g_per_kg", map[string]any{"low": 3, "moderate": 5, "long": 6},
		"carbs_g.class_minutes", map[string]any{"moderate": 45, "long": 90}}
}

// parseT parses a targets document or fails the test.
func parseT(t *testing.T, doc string) *Targets {
	t.Helper()
	tg, err := ParseTargets([]byte(doc))
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	return tg
}

// newV7 starts a harness on a targets document (written before the start).
func newV7(t *testing.T, targets string, mut ...func(*Options)) *harness {
	t.Helper()
	all := append([]func(*Options){func(o *Options) {
		if targets != "" {
			if err := os.WriteFile(o.TargetsFile, []byte(targets), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}}, mut...)
	return newHarness(t, all...)
}

// setTargets replaces the targets file (with a new modification time, so
// the service reloads it).
func (h *harness) setTargets(doc string) {
	h.t.Helper()
	if err := os.WriteFile(h.opts.TargetsFile, []byte(doc), 0o600); err != nil {
		h.t.Fatal(err)
	}
	mt := time.Now().Add(time.Duration(time.Now().UnixNano()%1000+1) * time.Second)
	if err := os.Chtimes(h.opts.TargetsFile, mt, mt); err != nil {
		h.t.Fatal(err)
	}
}

// strava writes one activity file; mtime = when the file was synced.
func (h *harness) strava(name, sport string, movingS, distM float64, localStart string, mtime time.Time) {
	h.t.Helper()
	if err := os.MkdirAll(h.opts.StravaDir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	p := filepath.Join(h.opts.StravaDir, name+".md")
	body := fmt.Sprintf("---\ntitle: x\n---\n```json\n{\"sport_type\":%q,\"moving_time\":%g,\"distance\":%g,\"start_date_local\":\"%sZ\"}\n```\n", sport, movingS, distM, localStart)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		h.t.Fatal(err)
	}
}

var foodSeq int

// food adds one food row of another writer (source agentd) to a date.
func (h *harness) food(date, item string, kcal, protein, satFat float64, fibre any, extra map[string]any) string {
	foodSeq++
	d := map[string]any{"item": item, "kind": "food", "kcal": kcal, "protein_g": protein, "carbs_g": 0.0, "fat_g": satFat, "sat_fat_g": satFat,
		"fiber_g": fibre, "source": "agentd", "op_id": fmt.Sprintf("op_fx_%d", foodSeq), "eaten_at": date + "T12:00:00+02:00"}
	for k, v := range extra {
		if v == nil {
			delete(d, k)
			continue
		}
		d[k] = v
	}
	h.vars.add("var-food", date, d)
	rows := h.vars.rows("var-food")
	return rows[len(rows)-1]["_id"].(string)
}

func (h *harness) get(path string) *httptest.ResponseRecorder { return h.do("GET", path, nil, "") }

func (h *harness) post(path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	return h.do("POST", path, bytes.NewReader(b), "application/json")
}

func (h *harness) week(t *testing.T, q string) WeekView {
	t.Helper()
	rec := h.get("/fuel/week" + q)
	if rec.Code != 200 {
		t.Fatalf("week%s: %d %s", q, rec.Code, rec.Body)
	}
	return decode[WeekView](t, rec)
}

func (h *harness) snap(t *testing.T, q string) Snapshot {
	t.Helper()
	rec := h.get("/fuel/snapshot" + q)
	if rec.Code != 200 {
		t.Fatalf("snapshot%s: %d %s", q, rec.Code, rec.Body)
	}
	return decode[Snapshot](t, rec)
}

func wb(v WeekView, key string) WeekBudget {
	for _, b := range v.Week.Budgets {
		if b.Key == key {
			return b
		}
	}
	return WeekBudget{}
}

func pb(v WeekView, key string) PeriodBudget {
	for _, b := range v.Last7.Budgets {
		if b.Key == key {
			return b
		}
	}
	return PeriodBudget{}
}

func budget(s Snapshot, key string) Budget {
	for _, b := range s.Budgets {
		if b.Key == key {
			return b
		}
	}
	return Budget{}
}

func cell(b PeriodBudget, date string) DayCell {
	for _, c := range b.Days {
		if c.Date == date {
			return c
		}
	}
	return DayCell{}
}

func num(p *float64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%g", *p)
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// keysOf collects every object key of a JSON document (for "no key named").
func keysOf(b []byte) map[string]bool {
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				out[k] = true
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	var v any
	_ = json.Unmarshal(b, &v)
	walk(v)
	return out
}

// zurich is a wall clock time in Europe/Zurich as an instant.
func zurich(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm.UTC()
}
