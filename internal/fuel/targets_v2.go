package fuel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Schema 2 of fuel-targets.json (spec 18.2). Every schema 1 key keeps its
// shape; the keys below are new. A nil pointer is "not set": the server never
// puts a default in its place.

// Instruction is a clinician instruction, stored and shown verbatim.
type Instruction struct {
	Text  string `json:"text"`
	SetBy string `json:"set_by"`
	SetOn string `json:"set_on"`
}

// SymptomRule is one clinician review rule for a symptom category.
type SymptomRule struct {
	Category string `json:"category"`
	Instruction
}

// CarbsCfg is `carbs_g`: total carbohydrate by day class.
type CarbsCfg struct {
	Kind         string
	Unit         string
	Low          *float64
	Moderate     *float64
	Long         *float64
	ClassMinutes *ClassMinutes
}

// ClassMinutes are the day class limits (minutes of training).
type ClassMinutes struct {
	Moderate int `json:"moderate"`
	Long     int `json:"long"`
}

// Maintenance is the adopted (or hand-set) maintenance energy.
type Maintenance struct {
	Kcal      float64   `json:"kcal"`
	MassKg    float64   `json:"mass_kg"`
	AvgRunKm  float64   `json:"avg_run_km_per_day"`
	Window    [2]string `json:"window"`
	AdoptedOn string    `json:"adopted_on"`
	ResultID  *string   `json:"result_id"`
}

// CalibrationCfg are the calibration settings (every key required).
type CalibrationCfg struct {
	StartDate              string   `json:"start_date"`
	MinDays                int      `json:"min_days"`
	MaxDays                int      `json:"max_days"`
	MinCompleteKcal        float64  `json:"min_complete_kcal"`
	MinWeighDays           int      `json:"min_weigh_days"`
	EdgeDays               int      `json:"edge_days"`
	MinEdgeWeighDays       int      `json:"min_edge_weigh_days"`
	MaxUncertaintyKcal     float64  `json:"max_uncertainty_kcal"`
	MaxAgeDays             int      `json:"max_age_days"`
	DriftKcal              float64  `json:"drift_kcal"`
	DriftUncertaintyFactor float64  `json:"drift_uncertainty_factor"`
	DriftDays              int      `json:"drift_days"`
	BreakDates             []string `json:"break_dates"`
}

func (c *CalibrationCfg) isBreak(date string) bool {
	for _, d := range c.BreakDates {
		if d == date {
			return true
		}
	}
	return false
}

// EnergyCfg is `energy`.
type EnergyCfg struct {
	Maintenance   *Maintenance
	DeficitKcal   *float64
	RunCost       *float64 // kcal per kg per km
	EnergyDensity *float64 // kcal per kg of body mass change
	Calibration   *CalibrationCfg
}

// EstimatorCfg are the lever estimator factors.
type EstimatorCfg struct {
	BetaGlucanPer100 struct {
		RolledOats float64 `json:"rolled_oats"`
		OatBran    float64 `json:"oat_bran"`
		Barley     float64 `json:"barley"`
		OatDrink   float64 `json:"oat_drink"`
	} `json:"beta_glucan_g_per_100"`
	PulsesDryToCooked float64 `json:"pulses_dry_to_cooked"`
}

// LeversCfg is `levers`.
type LeversCfg struct {
	Reference         [5]*float64 // by leverKeys
	Estimator         *EstimatorCfg
	AvgMinCoveredDays *int
	CoffeeDefaultBrew *string
}

// StrengthCfg is `strength` (D12).
type StrengthCfg struct {
	SetVariables     []string `json:"set_variables"`
	SessionMinSets   int      `json:"session_min_sets"`
	StravaSportTypes []string `json:"strava_sport_types"`
}

// ClinicianCfg is `clinician`: text only, never a number the app compares.
type ClinicianCfg struct {
	BPHome, BPOffice, BPAmbulatory *Instruction
	SymptomRules                   []SymptomRule
	HardSets                       *Instruction
	StrengthTestProtocol           *Instruction
}

// V2 holds the schema 2 keys of a targets file.
type V2 struct {
	ReferenceMassKg *float64
	Carbs           CarbsCfg
	Energy          EnergyCfg
	Levers          LeversCfg
	Strength        *StrengthCfg
	Clinician       ClinicianCfg
	InfoURL         string
}

var symptomCategories = []string{"chest_pain", "back_or_neck_pain", "breathlessness", "palpitations", "dizziness_or_syncope",
	"performance_decline", "low_libido", "mood", "bone_stress_injury", "injury_other", "other", "none"}

func isSymptomCategory(c string) bool {
	for _, x := range symptomCategories {
		if x == c {
			return true
		}
	}
	return false
}

var brewMethods = []string{"filtered", "unfiltered", "espresso", "instant", "unknown"}

func isBrewMethod(m string) bool {
	for _, x := range brewMethods {
		if x == m {
			return true
		}
	}
	return false
}

var schema2Keys = []string{"reference_mass_kg", "carbs_g", "energy", "levers", "strength", "clinician"}

// obj decodes a JSON object and checks its keys: every key of required must
// be present, every present key must be in required or optional.
func obj(what string, raw json.RawMessage, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("targets: %s must be an object", what)
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("targets: %s: missing key %q", what, k)
		}
	}
	for k := range m {
		ok := false
		for _, r := range required {
			ok = ok || r == k
		}
		for _, r := range optional {
			ok = ok || r == k
		}
		if !ok {
			return nil, fmt.Errorf("targets: %s: unknown key %q", what, k)
		}
	}
	return m, nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// numOrNil decodes a finite number or null.
func numOrNil(what string, raw json.RawMessage) (*float64, error) {
	if isNull(raw) {
		return nil, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("targets: %s must be a number or null", what)
	}
	return &f, nil
}

func intOrNil(what string, raw json.RawMessage) (*int, error) {
	f, err := numOrNil(what, raw)
	if err != nil || f == nil {
		return nil, err
	}
	if *f != math.Trunc(*f) {
		return nil, fmt.Errorf("targets: %s must be an integer", what)
	}
	i := int(*f)
	return &i, nil
}

func inRange(what string, v *float64, lo, hi float64) error {
	if v != nil && (*v < lo || *v > hi) {
		return fmt.Errorf("targets: %s must be %g to %g", what, lo, hi)
	}
	return nil
}

func validDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// strictInto decodes raw into v with unknown keys refused, after checking
// that every key of required is present.
func strictInto(what string, raw json.RawMessage, required []string, v any) error {
	if _, err := obj(what, raw, required); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("targets: %s: %v", what, err)
	}
	return nil
}

func parseInstruction(what string, raw json.RawMessage) (*Instruction, error) {
	if isNull(raw) {
		return nil, nil
	}
	var in Instruction
	if err := strictInto(what, raw, []string{"text", "set_by", "set_on"}, &in); err != nil {
		return nil, err
	}
	if err := checkInstruction(what, in); err != nil {
		return nil, err
	}
	return &in, nil
}

func checkInstruction(what string, in Instruction) error {
	if strings.TrimSpace(in.Text) == "" || utf8.RuneCountInString(in.Text) > 600 {
		return fmt.Errorf("targets: %s.text must be 1 to 600 characters", what)
	}
	for _, r := range in.Text {
		if r == '\n' || r == '\r' || !unicode.IsPrint(r) {
			return fmt.Errorf("targets: %s.text must be one printable paragraph", what)
		}
	}
	if strings.TrimSpace(in.SetBy) == "" || utf8.RuneCountInString(in.SetBy) > 80 {
		return fmt.Errorf("targets: %s.set_by must be 1 to 80 characters", what)
	}
	if !validDate(in.SetOn) {
		return fmt.Errorf("targets: %s.set_on must be a date", what)
	}
	return nil
}

// parseV2 validates the schema 2 keys (spec 18.2).
func parseV2(raw map[string]json.RawMessage) (*V2, error) {
	for _, k := range schema2Keys {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("targets: missing key %q", k)
		}
	}
	v := &V2{}
	var err error
	if v.ReferenceMassKg, err = numOrNil("reference_mass_kg", raw["reference_mass_kg"]); err != nil {
		return nil, err
	}
	if err = inRange("reference_mass_kg", v.ReferenceMassKg, 40, 150); err != nil {
		return nil, err
	}
	if s, ok := raw["info_url"]; ok {
		if json.Unmarshal(s, &v.InfoURL) != nil || (v.InfoURL != "" && !strings.HasPrefix(v.InfoURL, "https://")) || len(v.InfoURL) > 300 || strings.ContainsAny(v.InfoURL, "# \t\n") {
			return nil, errors.New("targets: info_url must be an https URL without a fragment")
		}
	}

	// carbs_g
	cm, err := obj("carbs_g", raw["carbs_g"], []string{"kind", "unit", "g_per_kg", "class_minutes"})
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(cm["kind"], &v.Carbs.Kind) != nil || v.Carbs.Kind != "floor" {
		return nil, errors.New(`targets: carbs_g.kind must be "floor"`)
	}
	if json.Unmarshal(cm["unit"], &v.Carbs.Unit) != nil || v.Carbs.Unit != "total" {
		return nil, errors.New(`targets: carbs_g.unit must be "total"`)
	}
	gk, err := obj("carbs_g.g_per_kg", cm["g_per_kg"], []string{"low", "moderate", "long"})
	if err != nil {
		return nil, err
	}
	for _, x := range []struct {
		k string
		p **float64
	}{{"low", &v.Carbs.Low}, {"moderate", &v.Carbs.Moderate}, {"long", &v.Carbs.Long}} {
		if *x.p, err = numOrNil("carbs_g.g_per_kg."+x.k, gk[x.k]); err != nil {
			return nil, err
		}
		if *x.p != nil && (**x.p <= 0 || **x.p > 12) {
			return nil, fmt.Errorf("targets: carbs_g.g_per_kg.%s must be above 0 and at most 12", x.k)
		}
	}
	set := []*float64{v.Carbs.Low, v.Carbs.Moderate, v.Carbs.Long}
	for i := 0; i < len(set); i++ {
		for j := i + 1; j < len(set); j++ {
			if set[i] != nil && set[j] != nil && *set[i] > *set[j] {
				return nil, errors.New("targets: carbs_g.g_per_kg must obey low <= moderate <= long")
			}
		}
	}
	if !isNull(cm["class_minutes"]) {
		km, err := obj("carbs_g.class_minutes", cm["class_minutes"], []string{"moderate", "long"})
		if err != nil {
			return nil, err
		}
		mo, e1 := intOrNil("carbs_g.class_minutes.moderate", km["moderate"])
		lo, e2 := intOrNil("carbs_g.class_minutes.long", km["long"])
		if e1 != nil || e2 != nil || mo == nil || lo == nil || *mo <= 0 || *mo >= *lo || *lo > 600 {
			return nil, errors.New("targets: carbs_g.class_minutes needs integers with 0 < moderate < long <= 600")
		}
		v.Carbs.ClassMinutes = &ClassMinutes{Moderate: *mo, Long: *lo}
	} else if v.Carbs.Moderate != nil || v.Carbs.Long != nil {
		return nil, errors.New("targets: carbs_g.g_per_kg.moderate or long is set while class_minutes is null")
	}

	// energy
	em, err := obj("energy", raw["energy"], []string{"maintenance", "deficit_kcal", "run_cost_kcal_per_kg_km", "energy_density_kcal_per_kg", "calibration"})
	if err != nil {
		return nil, err
	}
	e := &v.Energy
	if e.DeficitKcal, err = numOrNil("energy.deficit_kcal", em["deficit_kcal"]); err != nil {
		return nil, err
	}
	if e.RunCost, err = numOrNil("energy.run_cost_kcal_per_kg_km", em["run_cost_kcal_per_kg_km"]); err != nil {
		return nil, err
	}
	if e.EnergyDensity, err = numOrNil("energy.energy_density_kcal_per_kg", em["energy_density_kcal_per_kg"]); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		what   string
		v      *float64
		lo, hi float64
	}{{"energy.deficit_kcal", e.DeficitKcal, 0, 500}, {"energy.run_cost_kcal_per_kg_km", e.RunCost, 0.5, 1.5}, {"energy.energy_density_kcal_per_kg", e.EnergyDensity, 5000, 9500}} {
		if err := inRange(c.what, c.v, c.lo, c.hi); err != nil {
			return nil, err
		}
	}
	if !isNull(em["maintenance"]) {
		var m Maintenance
		if err := strictInto("energy.maintenance", em["maintenance"], []string{"kcal", "mass_kg", "avg_run_km_per_day", "window", "adopted_on", "result_id"}, &m); err != nil {
			return nil, err
		}
		if err := checkMaintenanceNumbers(m.Kcal, m.MassKg, m.AvgRunKm); err != nil {
			return nil, err
		}
		if !validDate(m.Window[0]) || !validDate(m.Window[1]) || m.Window[0] > m.Window[1] {
			return nil, errors.New("targets: energy.maintenance.window needs two dates, from <= to")
		}
		if n := daysBetween(m.Window[0], m.Window[1]) + 1; n < 14 || n > 28 {
			return nil, errors.New("targets: energy.maintenance.window must be 14 to 28 days long")
		}
		if !validDate(m.AdoptedOn) || m.AdoptedOn < m.Window[1] {
			return nil, errors.New("targets: energy.maintenance.adopted_on must be a date not before the window end")
		}
		if m.ResultID != nil && *m.ResultID == "" {
			return nil, errors.New("targets: energy.maintenance.result_id must be null or an id")
		}
		e.Maintenance = &m
	}
	if e.Maintenance != nil && e.DeficitKcal != nil && e.RunCost != nil {
		if !energyFloorOK(e.Maintenance.Kcal, e.Maintenance.MassKg, e.Maintenance.AvgRunKm, *e.RunCost, *e.DeficitKcal) {
			return nil, errors.New("targets: energy: a day without a run would get less than half of the maintenance energy")
		}
	}
	if !isNull(em["calibration"]) {
		var c CalibrationCfg
		keys := []string{"start_date", "min_days", "max_days", "min_complete_kcal", "min_weigh_days", "edge_days", "min_edge_weigh_days",
			"max_uncertainty_kcal", "max_age_days", "drift_kcal", "drift_uncertainty_factor", "drift_days", "break_dates"}
		if err := strictInto("energy.calibration", em["calibration"], keys, &c); err != nil {
			return nil, err
		}
		switch {
		case !validDate(c.StartDate):
			return nil, errors.New("targets: energy.calibration.start_date must be a date")
		case c.MinDays < 14 || c.MinDays > c.MaxDays || c.MaxDays > 28:
			return nil, errors.New("targets: energy.calibration needs 14 <= min_days <= max_days <= 28")
		case c.MinCompleteKcal < 500 || c.MinCompleteKcal > 3000:
			return nil, errors.New("targets: energy.calibration.min_complete_kcal must be 500 to 3000")
		case c.MinWeighDays < 3 || c.MinWeighDays > c.MinDays:
			return nil, errors.New("targets: energy.calibration needs 3 <= min_weigh_days <= min_days")
		case c.EdgeDays < 1 || c.EdgeDays > 7:
			return nil, errors.New("targets: energy.calibration.edge_days must be 1 to 7")
		case c.MinEdgeWeighDays < 0 || c.MinEdgeWeighDays > c.EdgeDays:
			return nil, errors.New("targets: energy.calibration needs 0 <= min_edge_weigh_days <= edge_days")
		case c.MaxUncertaintyKcal < 20 || c.MaxUncertaintyKcal > 1000:
			return nil, errors.New("targets: energy.calibration.max_uncertainty_kcal must be 20 to 1000")
		case c.MaxAgeDays < 0 || c.MaxAgeDays > 14:
			return nil, errors.New("targets: energy.calibration.max_age_days must be 0 to 14")
		case c.DriftKcal < 20 || c.DriftKcal > 1000:
			return nil, errors.New("targets: energy.calibration.drift_kcal must be 20 to 1000")
		case c.DriftUncertaintyFactor < 0 || c.DriftUncertaintyFactor > 5:
			return nil, errors.New("targets: energy.calibration.drift_uncertainty_factor must be 0 to 5")
		case c.DriftDays < 2 || c.DriftDays > 30:
			return nil, errors.New("targets: energy.calibration.drift_days must be 2 to 30")
		case c.BreakDates == nil || len(c.BreakDates) > 100:
			return nil, errors.New("targets: energy.calibration.break_dates must be a list of at most 100 dates")
		}
		for _, d := range c.BreakDates {
			if !validDate(d) {
				return nil, errors.New("targets: energy.calibration.break_dates holds an invalid date")
			}
		}
		for _, f := range []float64{c.MinCompleteKcal, c.MaxUncertaintyKcal, c.DriftKcal, c.DriftUncertaintyFactor} {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, errors.New("targets: energy.calibration holds a number that is not finite")
			}
		}
		e.Calibration = &c
	}

	// levers
	lm, err := obj("levers", raw["levers"], []string{"reference", "estimator", "avg_min_covered_days", "coffee_default_brew_method"})
	if err != nil {
		return nil, err
	}
	rm, err := obj("levers.reference", lm["reference"], leverKeys)
	if err != nil {
		return nil, err
	}
	for i, k := range leverKeys {
		if v.Levers.Reference[i], err = numOrNil("levers.reference."+k, rm[k]); err != nil {
			return nil, err
		}
		if p := v.Levers.Reference[i]; p != nil && (*p <= 0 || *p > 500) {
			return nil, fmt.Errorf("targets: levers.reference.%s must be above 0 and at most 500", k)
		}
	}
	if !isNull(lm["estimator"]) {
		var est EstimatorCfg
		if err := strictInto("levers.estimator", lm["estimator"], []string{"beta_glucan_g_per_100", "pulses_dry_to_cooked"}, &est); err != nil {
			return nil, err
		}
		if _, err := obj("levers.estimator.beta_glucan_g_per_100", mustRaw(lm["estimator"], "beta_glucan_g_per_100"), []string{"rolled_oats", "oat_bran", "barley", "oat_drink"}); err != nil {
			return nil, err
		}
		b := est.BetaGlucanPer100
		for _, f := range []float64{b.RolledOats, b.OatBran, b.Barley, b.OatDrink} {
			if !(f > 0) || f > 20 {
				return nil, errors.New("targets: levers.estimator.beta_glucan_g_per_100 factors must be above 0 and at most 20")
			}
		}
		if !(est.PulsesDryToCooked >= 1) || est.PulsesDryToCooked > 4 {
			return nil, errors.New("targets: levers.estimator.pulses_dry_to_cooked must be 1 to 4")
		}
		v.Levers.Estimator = &est
	}
	if v.Levers.AvgMinCoveredDays, err = intOrNil("levers.avg_min_covered_days", lm["avg_min_covered_days"]); err != nil {
		return nil, err
	}
	if p := v.Levers.AvgMinCoveredDays; p != nil && (*p < 1 || *p > 7) {
		return nil, errors.New("targets: levers.avg_min_covered_days must be 1 to 7")
	}
	if !isNull(lm["coffee_default_brew_method"]) {
		var m string
		if json.Unmarshal(lm["coffee_default_brew_method"], &m) != nil || !isBrewMethod(m) || m == "unknown" {
			return nil, errors.New("targets: levers.coffee_default_brew_method must be filtered, unfiltered, espresso, instant or null")
		}
		v.Levers.CoffeeDefaultBrew = &m
	}

	// strength
	if !isNull(raw["strength"]) {
		var st StrengthCfg
		if err := strictInto("strength", raw["strength"], []string{"set_variables", "session_min_sets", "strava_sport_types"}, &st); err != nil {
			return nil, err
		}
		if st.SetVariables == nil || st.StravaSportTypes == nil {
			return nil, errors.New("targets: strength lists must be arrays")
		}
		if err := distinctNames("strength.set_variables", st.SetVariables, 20, 80); err != nil {
			return nil, err
		}
		if err := distinctNames("strength.strava_sport_types", st.StravaSportTypes, 10, 40); err != nil {
			return nil, err
		}
		if st.SessionMinSets < 1 || st.SessionMinSets > 50 {
			return nil, errors.New("targets: strength.session_min_sets must be 1 to 50")
		}
		v.Strength = &st
	}

	// clinician
	clm, err := obj("clinician", raw["clinician"], []string{"bp", "symptom_review_rules", "hard_sets", "strength_test_protocol"})
	if err != nil {
		return nil, err
	}
	bpm, err := obj("clinician.bp", clm["bp"], []string{"home", "office", "ambulatory"})
	if err != nil {
		return nil, err
	}
	c := &v.Clinician
	for _, x := range []struct {
		k string
		p **Instruction
	}{{"home", &c.BPHome}, {"office", &c.BPOffice}, {"ambulatory", &c.BPAmbulatory}} {
		if *x.p, err = parseInstruction("clinician.bp."+x.k, bpm[x.k]); err != nil {
			return nil, err
		}
	}
	if c.HardSets, err = parseInstruction("clinician.hard_sets", clm["hard_sets"]); err != nil {
		return nil, err
	}
	if c.StrengthTestProtocol, err = parseInstruction("clinician.strength_test_protocol", clm["strength_test_protocol"]); err != nil {
		return nil, err
	}
	var rules []json.RawMessage
	if json.Unmarshal(clm["symptom_review_rules"], &rules) != nil || rules == nil {
		return nil, errors.New("targets: clinician.symptom_review_rules must be an array")
	}
	seen := map[string]bool{}
	for i, rr := range rules {
		what := fmt.Sprintf("clinician.symptom_review_rules[%d]", i)
		var r SymptomRule
		if err := strictInto(what, rr, []string{"category", "text", "set_by", "set_on"}, &r); err != nil {
			return nil, err
		}
		if !isSymptomCategory(r.Category) || seen[r.Category] {
			return nil, fmt.Errorf("targets: %s: unknown or repeated category", what)
		}
		seen[r.Category] = true
		if err := checkInstruction(what, r.Instruction); err != nil {
			return nil, err
		}
		c.SymptomRules = append(c.SymptomRules, r)
	}
	return v, nil
}

func mustRaw(raw json.RawMessage, key string) json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	return m[key]
}

func distinctNames(what string, xs []string, max, maxLen int) error {
	if len(xs) > max {
		return fmt.Errorf("targets: %s holds more than %d names", what, max)
	}
	seen := map[string]bool{}
	for _, x := range xs {
		if strings.TrimSpace(x) == "" || x != strings.TrimSpace(x) || utf8.RuneCountInString(x) > maxLen || seen[x] {
			return fmt.Errorf("targets: %s needs different non-empty names of at most %d characters", what, maxLen)
		}
		seen[x] = true
	}
	return nil
}

// checkMaintenanceNumbers are the bounds of a maintenance object (also gate
// G6 of the calibration).
func checkMaintenanceNumbers(kcal, mass, avgKm float64) error {
	for _, f := range []float64{kcal, mass, avgKm} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("targets: energy.maintenance holds a number that is not finite")
		}
	}
	switch {
	case kcal < 1500 || kcal > 6000:
		return errors.New("targets: energy.maintenance.kcal must be 1500 to 6000")
	case mass < 40 || mass > 150:
		return errors.New("targets: energy.maintenance.mass_kg must be 40 to 150")
	case avgKm < 0 || avgKm > 60:
		return errors.New("targets: energy.maintenance.avg_run_km_per_day must be 0 to 60")
	}
	return nil
}

// energyFloorOK is the cross-field rule: the lowest value the formula can
// give (a day without a run) is at least half of the maintenance energy.
func energyFloorOK(kcal, mass, avgKm, k, deficit float64) bool {
	return kcal-k*mass*avgKm-deficit >= 0.5*kcal-1e-9
}

// daysBetween is the number of calendar days from a to b (dates).
func daysBetween(a, b string) int {
	ta, _ := time.Parse("2006-01-02", a)
	tb, _ := time.Parse("2006-01-02", b)
	return int(math.Round(tb.Sub(ta).Hours() / 24))
}

// logOnce guards the "water_ml ignored" line (one per load).
func logWaterIgnored() {
	log.Printf("fuel: targets: water_ml is ignored in a schema 2 file (water is context, not a target)")
}
