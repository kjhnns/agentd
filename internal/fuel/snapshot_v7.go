package fuel

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// wireVersion is the `wire` value of the snapshot and of the week view.
const wireVersion = 7

// The v7 additions to the snapshot (spec 18.7 and 19.7). Every v6 key keeps
// its value; these are NEW keys only.

// Budget is a MacroState with the v7 marks.
type Budget struct {
	MacroState
	Provisional bool    `json:"provisional"`
	Basis       *string `json:"basis"`
	Info        string  `json:"info"`
}

// LeverState is one lever of the day (no status, no score).
type LeverState struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Unit         string   `json:"unit"`
	Consumed     float64  `json:"consumed"`
	UntaggedRows int      `json:"untagged_rows"`
	Avg7         *float64 `json:"avg7"`
	CoveredDays7 int      `json:"covered_days_7"`
	Reference    *float64 `json:"reference"`
	Info         string   `json:"info"`
}

// CoffeeCounts are active coffee drinks by brew method.
type CoffeeCounts struct {
	Filtered   int `json:"filtered"`
	Unfiltered int `json:"unfiltered"`
	Espresso   int `json:"espresso"`
	Instant    int `json:"instant"`
	Unknown    int `json:"unknown"`
}

func coffeeCounts(m map[string]int) CoffeeCounts {
	return CoffeeCounts{Filtered: m["filtered"], Unfiltered: m["unfiltered"], Espresso: m["espresso"], Instant: m["instant"], Unknown: m["unknown"]}
}

// Coffee is the snapshot's `coffee` object.
type Coffee struct {
	Today         CoffeeCounts `json:"today"`
	Last7         CoffeeCounts `json:"last7"`
	DefaultMethod *string      `json:"default_method"`
	Info          string       `json:"info"`
}

// EnergyState is the snapshot's `energy` object (18.5).
type EnergyState struct {
	State             string      `json:"state"` // legacy | provisional | formula
	Target            *float64    `json:"target"`
	MaintenanceKcal   *float64    `json:"maintenance_kcal"`
	RunKm             *float64    `json:"run_km"`
	RunAdjustKcal     *float64    `json:"run_adjust_kcal"`
	DeficitKcal       *float64    `json:"deficit_kcal"`
	MaintenanceSource *string     `json:"maintenance_source"`
	Drift             bool        `json:"drift"`
	Calibration       CalibResult `json:"calibration"`
	Info              string      `json:"info"`
	CalibrationInfo   string      `json:"calibration_info"`
}

// SnapContext holds values that are context, never a target.
type SnapContext struct {
	FluidsML struct {
		Consumed float64 `json:"consumed"`
		Info     string  `json:"info"`
	} `json:"fluids_ml"`
	AlcoholG struct {
		Consumed float64 `json:"consumed"`
	} `json:"alcohol_g"`
	BodyFat struct {
		LatestPct  *float64 `json:"latest_pct"`
		LatestDate *string  `json:"latest_date"`
		Note       string   `json:"note"`
		Info       string   `json:"info"`
	} `json:"body_fat"`
}

// Progress holds the progress markers (H values).
type Progress struct {
	Weight struct {
		Avg7Kg   *float64  `json:"avg7_kg"`
		Delta7Kg *float64  `json:"delta7_kg"`
		BandKg   []float64 `json:"band_kg"`
		Info     string    `json:"info"`
	} `json:"weight"`
	Waist struct {
		LatestCm     *float64 `json:"latest_cm"`
		LatestDate   *string  `json:"latest_date"`
		PreviousCm   *float64 `json:"previous_cm"`
		PreviousDate *string  `json:"previous_date"`
		Info         string   `json:"info"`
	} `json:"waist"`
	StrengthTest struct {
		Protocol *Instruction `json:"protocol"`
		Info     string       `json:"info"`
	} `json:"strength_test"`
}

// BPSnap is `records.blood_pressure`: a record view, never a judgement.
type BPSnap struct {
	Latest    *Record `json:"latest"`
	Count7d   int     `json:"count_7d"`
	Clinician struct {
		Home       *Instruction `json:"home"`
		Office     *Instruction `json:"office"`
		Ambulatory *Instruction `json:"ambulatory"`
	} `json:"clinician"`
	Info struct {
		Home       string `json:"home"`
		Office     string `json:"office"`
		Ambulatory string `json:"ambulatory"`
	} `json:"info"`
}

// SymptomSnap is `records.symptom`.
type SymptomSnap struct {
	LatestDate    *string `json:"latest_date"`
	EntryThisWeek bool    `json:"entry_this_week"`
	Info          string  `json:"info"`
}

// SnapRecords is the snapshot's `records` object; a part is null when its
// variable is missing.
type SnapRecords struct {
	BloodPressure *BPSnap      `json:"blood_pressure"`
	Symptom       *SymptomSnap `json:"symptom"`
}

// SnapInfo holds the base link of the explainer page.
type SnapInfo struct {
	Base string `json:"base"`
}

// dayData is one date as the snapshot and the week view see it.
type dayData struct {
	plan     dayPlan
	tot      DayTotals
	fluids   int64
	caffeine int64
	alcohol  int64
	lev      dayLeverState
	loaded   bool
}

func (in *snapInput) today() string { return in.now.In(in.targets.loc).Format("2006-01-02") }

// day computes (once) the data of a date from the one view of the cache.
func (in *snapInput) day(date string) *dayData {
	if dd := in.days[date]; dd != nil {
		return dd
	}
	rows, ok := in.rowsFor(date)
	dd := &dayData{plan: planDay(in.targets, date, in.today(), in.acts, in.stravaAt, in.now), tot: totalsFromRows(rows), lev: dayLevers(rows), loaded: ok}
	dd.fluids, dd.caffeine, dd.alcohol = intakeSums(rows)
	in.days[date] = dd
	return dd
}

// budgetChecks are the budget_score checks, by KEY (18.7); a nil entry is a
// skipped check (target null).
func budgetChecks(t *Targets, plan dayPlan, tot DayTotals) map[string]*bool {
	out := dayChecks(t, plan.DayType, tot, plan.EnergyTarget)
	delete(out, "net_carbs_g")
	if tg := plan.CarbsTarget; tg != nil {
		v := float64(tot.Sum["carbs_g"])/10 >= *tg
		out["carbs_g"] = &v
	}
	return out
}

// nextActionOf is the section 9 rule over the given states: among those
// that are behind, the one with the largest gap at the next checkpoint.
func nextActionOf(t *Targets, states []MacroState, minute float64, use func(MacroState) bool) *NextAction {
	next := -1
	for _, cp := range checkpoints {
		if float64(cp) > minute {
			next = cp
			break
		}
	}
	if next < 0 {
		return nil
	}
	best, bestGap := -1, 0.0
	var bestNeed float64
	for i, ms := range states {
		if !use(ms) || ms.Status != "behind" || ms.Target == nil || *ms.Target <= 0 {
			continue
		}
		paceAt := *ms.Target * elapsedFrac(t, float64(next))
		need := paceAt - ms.Consumed
		gap := need / *ms.Target
		if need > 0 && (best < 0 || gap > bestGap) {
			best, bestGap, bestNeed = i, gap, need
		}
	}
	if best < 0 {
		return nil
	}
	ms := states[best]
	g := math.Ceil(bestNeed - 1e-9)
	by := fmt.Sprintf("%02d:%02d", next/60, next%60)
	return &NextAction{Key: ms.Key, Grams: g, By: by, Text: fmt.Sprintf("Eat %.0f g %s by %s", g, strings.ToLower(ms.Label), by)}
}

// fillV7 adds the v7 keys to a computed v6 snapshot.
func (in *snapInput) fillV7(s *Snapshot, isToday bool, minute, el float64) {
	t := in.targets
	dd := in.day(in.date)
	plan := dd.plan
	s.Wire = wireVersion
	s.DayClass = plan.DayClass
	if plan.ClassUnset {
		s.Missing = append(s.Missing, "day_class_unset")
	}

	// Budgets: protein_g, sat_fat_g, fiber_g, kcal, carbs_g, caffeine_mg,
	// alcohol_g_week.
	for _, m := range s.Macros {
		if m.Key == "net_carbs_g" {
			continue
		}
		b := Budget{MacroState: m, Info: t.info(budgetAnchor(m.Key))}
		if m.Key == "kcal" {
			b.Provisional = plan.EnergyState == "provisional"
			b.Basis = plan.EnergyBasis
		}
		s.Budgets = append(s.Budgets, b)
	}
	carbs := MacroState{Key: "carbs_g", Label: "Carbohydrate", Unit: "g", Kind: "floor",
		Consumed: round1(float64(dd.tot.Sum["carbs_g"]) / 10), UnknownRows: dd.tot.Unknown["carbs_g"], Target: plan.CarbsTarget}
	if carbs.Target != nil {
		carbs.PaceTargetNow = fptr(round1(*carbs.Target * el))
	}
	carbs.Status = statusFor("floor", carbs.Consumed, carbs.Target, carbs.PaceTargetNow)
	s.Budgets = append(s.Budgets, Budget{MacroState: carbs, Basis: plan.CarbsBasis, Info: t.info("carbohydrate")})
	for _, m := range s.Intake {
		if m.Key == "caffeine_mg" || m.Key == "alcohol_g_week" {
			s.Budgets = append(s.Budgets, Budget{MacroState: m, Info: t.info(budgetAnchor(m.Key))})
		}
	}
	for _, v := range budgetChecks(t, plan, dd.tot) {
		s.BudgetScore.Of++
		if *v {
			s.BudgetScore.Hit++
		}
	}
	if isToday {
		var st []MacroState
		for _, b := range s.Budgets {
			st = append(st, b.MacroState)
		}
		s.BudgetNextAction = nextActionOf(t, st, minute, func(m MacroState) bool {
			return m.Key == "protein_g" || m.Key == "fiber_g" || m.Key == "carbs_g"
		})
	}

	// Levers and coffee: the day and the 7 local days ending on it.
	var lcfg LeversCfg
	if t.V2 != nil {
		lcfg = t.V2.Levers
	}
	last7 := map[string]int{}
	for i := 0; i < 7; i++ {
		for k, n := range in.day(dateAdd(in.date, -i)).lev.coffee {
			last7[k] += n
		}
	}
	for l, key := range leverKeys {
		ls := LeverState{Key: key, Label: leverLabels[l], Unit: "g", Consumed: round1(float64(dd.lev.consumed[l]) / 10),
			UntaggedRows: dd.lev.untagged[l], Reference: lcfg.Reference[l], Info: t.info(leverAnchors[l])}
		var sum int64
		for i := 0; i < 7; i++ {
			if d := in.day(dateAdd(in.date, -i)); d.lev.covered(l) {
				ls.CoveredDays7++
				sum += d.lev.consumed[l]
			}
		}
		if n := lcfg.AvgMinCoveredDays; n != nil && ls.CoveredDays7 >= *n && ls.CoveredDays7 > 0 {
			ls.Avg7 = fptr(round1(float64(sum) / 10 / float64(ls.CoveredDays7)))
		}
		s.Levers = append(s.Levers, ls)
	}
	s.Coffee = Coffee{Today: coffeeCounts(dd.lev.coffee), Last7: coffeeCounts(last7), DefaultMethod: lcfg.CoffeeDefaultBrew, Info: t.info("coffee-brewing")}

	// Energy.
	s.Energy = EnergyState{State: plan.EnergyState, Target: plan.EnergyTarget, Info: t.info("energy"), CalibrationInfo: t.info("maintenance-energy"),
		Calibration: in.calib, Drift: in.drift}
	if plan.RunKnown {
		s.Energy.RunKm = fptr(round1(plan.RunKm))
	}
	if t.V2 != nil {
		if m := t.V2.Energy.Maintenance; m != nil {
			src := "manual"
			if m.ResultID != nil {
				src = "calibration"
			}
			s.Energy.MaintenanceSource = &src
			if plan.EnergyState == "formula" {
				s.Energy.MaintenanceKcal = fptr(m.Kcal)
			}
		}
		if plan.EnergyState == "formula" {
			s.Energy.RunAdjustKcal = plan.RunAdjust
			s.Energy.DeficitKcal = t.V2.Energy.DeficitKcal
		}
	}
	if s.Energy.Calibration.State == "" {
		s.Energy.Calibration = calibOff(t, in.today(), "no calibration result yet")
	}

	// Context and progress.
	s.Context.FluidsML.Consumed, s.Context.FluidsML.Info = round1(float64(dd.fluids)/10), t.info("fluids")
	s.Context.AlcoholG.Consumed = round1(float64(dd.alcohol) / 10)
	s.Context.BodyFat.LatestPct, s.Context.BodyFat.LatestDate = s.BodyFat.LatestPct, s.BodyFat.LatestDate
	s.Context.BodyFat.Note, s.Context.BodyFat.Info = "scale estimate, not a progress marker", t.info("not-tracked")
	s.Progress.Weight.Avg7Kg, s.Progress.Weight.Delta7Kg = s.Weight.Avg7Kg, s.Weight.Delta7Kg
	s.Progress.Weight.BandKg, s.Progress.Weight.Info = t.WeightBandKg, t.info("body-weight")
	s.Progress.Waist.Info = t.info("waist")
	s.Progress.StrengthTest.Info = t.info("strength-test")
	var cl ClinicianCfg
	if t.V2 != nil {
		cl = t.V2.Clinician
	}
	s.Progress.StrengthTest.Protocol = cl.StrengthTestProtocol

	// Records (views only: no status, no trend, no judgement).
	if in.recs != nil {
		in.recs.fill(s, in.date, t, cl)
	}

	// Strength.
	si := in.si
	s.Strength = StrengthState{Rule: si.rule(), Sessions: s.Week.StrengthSessions, SessionsTarget: t.StrengthPerWeek,
		MissingVariables: si.missingNames(), Clinician: cl.HardSets, Info: t.info("strength")}
	if si.cfg != nil {
		n := si.cfg.SessionMinSets
		s.Strength.SessionMinSets = &n
		mon := mondayOf(in.date)
		var dates []string
		for d := mon; d <= in.date; d = dateAdd(d, 1) {
			dates = append(dates, d)
		}
		_, s.Strength.Week, _ = si.sum(dates)
	}
	if t.V2 != nil {
		s.Info.Base = t.V2.InfoURL
	}
	s.WeekBudgets = in.weekCompact()
}

// legacyJSON renders only the v6 keys of a snapshot: what the model sees.
// No record, no clinician text and no v7 key ever reaches the model.
func (s Snapshot) legacyJSON() []byte {
	b, _ := json.Marshal(struct {
		Revision   int          `json:"revision"`
		AsOf       time.Time    `json:"as_of"`
		DataAsOf   time.Time    `json:"data_as_of"`
		Date       string       `json:"date"`
		DayType    string       `json:"day_type"`
		Macros     []MacroState `json:"macros"`
		Intake     []MacroState `json:"intake"`
		NextAction *NextAction  `json:"next_action"`
		DayScore   DayScore     `json:"day_score"`
		Week       Week         `json:"week"`
		Weight     Weight       `json:"weight"`
		BodyFat    BodyFat      `json:"body_fat"`
		Streaks    Streaks      `json:"streaks"`
		Missing    []string     `json:"missing"`
	}{s.Revision, s.AsOf, s.DataAsOf, s.Date, s.DayType, s.Macros, s.Intake, s.NextAction, s.DayScore, s.Week, s.Weight, s.BodyFat, s.Streaks, s.Missing})
	return b
}
