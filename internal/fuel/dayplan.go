package fuel

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// dayPlan is everything that depends on what kind of day a date is: the day
// type (section 9), the day class and the carbohydrate target (18.3) and the
// energy target (18.5). ONE function computes it for the snapshot and for
// the week view (section 19), so the two can never differ.
type dayPlan struct {
	Date  string
	State string // past | today | future

	DayType    string // rest | training | unknown
	DayClass   string // low | moderate | long | unknown
	Stale      bool   // "Strava stale" holds (the current date only)
	ClassUnset bool   // schema 2 with class_minutes null
	Minutes    float64
	RunKm      float64
	RunKnown   bool // false for a future date and while Strava is stale

	EnergyState   string // legacy | provisional | formula
	EnergyTarget  *float64
	RunAdjust     *float64
	EnergyBasis   *string
	CarbsTarget   *float64
	CarbsBasis    *string
	CarbsIsSchema bool // a schema 2 file (a carbohydrate budget exists)

	// Activity (18.15): set when energy.activity is set.
	Activity *activityParts

	// SatFatTarget is the saturated fat target of the date: the fixed cap,
	// or with the budget form a share of the day's energy target (18.14).
	SatFatTarget *float64
	SatFatBasis  *string
}

// stravaStale is the section 9 predicate on the files alone.
func stravaStale(now, stravaAt time.Time) bool {
	return stravaAt.IsZero() || now.Sub(stravaAt) > 36*time.Hour
}

func roundTo(f, step float64) float64 { return math.Round(f/step) * step }

// planDay computes the plan of a local date. today is the real current date.
func planDay(t *Targets, date, today string, acts []Activity, stravaAt, now time.Time, ax *activityInput) dayPlan {
	p := dayPlan{Date: date, State: "past", EnergyState: "legacy", CarbsIsSchema: t.V2 != nil}
	switch {
	case date == today:
		p.State = "today"
	case date > today:
		p.State = "future"
	}
	p.DayType = "rest"
	if p.State != "future" {
		p.DayType = dayTypeFor(date, acts)
		p.RunKnown = true
		for _, a := range acts {
			if a.LocalDate != date {
				continue
			}
			if isTrainingSport(a.SportType) {
				p.Minutes += a.MovingS / 60
			}
			if isRun(a.SportType) {
				p.RunKm += a.DistanceM / 1000
			}
		}
		// Strava stale: the current date only, and only without a training
		// activity of that date in the files (the v6 rule, exactly).
		if p.State == "today" && stravaStale(now, stravaAt) && p.DayType != "training" {
			p.DayType, p.Stale, p.RunKnown = "unknown", true, false
		}
	}

	// Day class and carbohydrate target (18.3).
	p.DayClass = "unknown"
	if v2 := t.V2; v2 != nil {
		gk := v2.Carbs.Low
		basis := "low day"
		switch cm := v2.Carbs.ClassMinutes; {
		case cm == nil:
			p.ClassUnset = true
			basis = "day class not set; the low value applies"
		case p.Stale:
			basis = "training data is stale; the low value applies"
		case p.State == "future":
			p.DayClass = "low"
			basis = "default for a day without a known activity"
		case p.Minutes < float64(cm.Moderate):
			p.DayClass = "low"
			if p.State == "today" {
				basis = "low day so far"
			}
		case p.Minutes < float64(cm.Long):
			p.DayClass, gk = "moderate", v2.Carbs.Moderate
			basis = "moderate day (" + strconv.Itoa(int(math.Round(p.Minutes))) + " min)"
		default:
			p.DayClass, gk = "long", v2.Carbs.Long
			basis = "long day (" + strconv.Itoa(int(math.Round(p.Minutes))) + " min)"
		}
		p.CarbsBasis = &basis
		if gk != nil && v2.ReferenceMassKg != nil {
			p.CarbsTarget = fptr(roundTo(*gk**v2.ReferenceMassKg, 5))
		}
	}

	// Energy target (18.5, with activity 18.15).
	legacy := t.Kcal.For(p.DayType)
	p.EnergyTarget = legacy
	if v2 := t.V2; v2 != nil {
		e := v2.Energy
		if e.Maintenance == nil || e.DeficitKcal == nil || e.RunCost == nil {
			p.EnergyState = "provisional"
			b := "Provisional. Maintenance is not calibrated yet."
			p.EnergyBasis = &b
			if e.Activity != nil && v2.ReferenceMassKg != nil && e.RunCost != nil {
				p.provisionalActivity(t, acts, ax, *v2.ReferenceMassKg)
			}
		} else {
			p.EnergyState = "formula"
			m := e.Maintenance
			adj := 0.0
			var b string
			if p.RunKnown {
				diff := p.RunKm - m.AvgRunKm
				adj = *e.RunCost * m.MassKg * diff
				sign, word := "+", "more"
				if adj < 0 {
					sign, word = "-", "less"
				}
				b = fmt.Sprintf("%s maintenance %s %s run (%s km %s than average) - %s deficit", fmtNum(roundTo(m.Kcal, 10)), sign,
					fmtNum(math.Abs(roundTo(adj, 10))), fmtNum(round1(math.Abs(diff))), word, fmtNum(*e.DeficitKcal))
			} else {
				b = fmt.Sprintf("%s maintenance - %s deficit, run distance unknown", fmtNum(roundTo(m.Kcal, 10)), fmtNum(*e.DeficitKcal))
			}
			p.EnergyBasis = &b
			p.RunAdjust = fptr(roundTo(adj, 10))
			target := m.Kcal + adj - *e.DeficitKcal
			if e.Activity != nil && p.State != "future" {
				a := activityEnergy(ax, acts, e.Activity, date, p.State == "today", true, m.MassKg)
				a.Run = adj
				p.Activity = &a
				target += a.Other + a.Strength + a.Steps
				b += activityBasis(a, true)
				p.EnergyBasis = &b
			}
			p.EnergyTarget = fptr(roundTo(target, 10))
		}
	}

	// Saturated fat (18.14).
	if t.SatFat.Kind == "budget" && t.SatFat.EnergyFrac != nil {
		if p.EnergyTarget != nil {
			p.SatFatTarget = fptr(math.Round(*t.SatFat.EnergyFrac * *p.EnergyTarget / 9))
			b := fmt.Sprintf("%s %% of %s kcal", fmtNum(round1(*t.SatFat.EnergyFrac*100)), fmtNum(*p.EnergyTarget))
			p.SatFatBasis = &b
		}
	} else {
		p.SatFatTarget = t.SatFat.For(p.DayType)
	}
	return p
}

// provisionalActivity replaces the provisional rest / training switch by
// the rest value plus the day's activity (18.15): the run by its distance
// (k x reference mass x km), the other sports and a strength session in
// full (the rest value holds none of them), the steps against the average.
// A future date and a stale today keep the rest value.
func (p *dayPlan) provisionalActivity(t *Targets, acts []Activity, ax *activityInput, mass float64) {
	rest := t.Kcal.For("rest")
	if rest == nil || p.State == "future" {
		return
	}
	cfg := t.V2.Energy.Activity
	k := *t.V2.Energy.RunCost
	a := activityEnergy(ax, acts, cfg, p.Date, p.State == "today", false, mass)
	if p.RunKnown {
		a.Run = k * mass * p.RunKm
	}
	p.Activity = &a
	p.EnergyTarget = fptr(roundTo(*rest+a.Run+a.Other+a.Strength+a.Steps, 10))
	b := "Provisional. " + fmtNum(*rest) + " rest" + activityBasis(a, false)
	if !p.RunKnown {
		b += ", run distance unknown"
	}
	p.EnergyBasis = &b
}

// activityBasis names the non-zero activity parts: " + 400 run (5 km) + 120 steps".
func activityBasis(a activityParts, relative bool) string {
	var out string
	part := func(v float64, what string) {
		r := roundTo(v, 10)
		if r == 0 {
			return
		}
		sign := "+"
		if r < 0 {
			sign = "-"
		}
		out += fmt.Sprintf(" %s %s %s", sign, fmtNum(math.Abs(r)), what)
	}
	if !relative {
		part(a.Run, "run")
	}
	part(a.Other, "other sport")
	part(a.Strength, "strength")
	if a.StepCount != nil && a.StepBaseline != nil {
		part(a.Steps, fmt.Sprintf("steps (%s, average %s)", fmtNum(float64(*a.StepCount)), fmtNum(roundTo(*a.StepBaseline, 100))))
	}
	return out
}

// info builds an 18.8 link ("" when info_url is absent).
func (t *Targets) info(anchor string) string {
	if t.V2 == nil || t.V2.InfoURL == "" {
		return ""
	}
	return t.V2.InfoURL + "#" + anchor
}

// infoAnchors is the constant table of 18.8 (the server sends these only).
var infoAnchors = []string{"protein", "saturated-fat", "fibre", "energy", "maintenance-energy", "carbohydrate", "viscous-fibre", "nuts",
	"plant-protein", "coffee-brewing", "caffeine", "alcohol", "fluids", "not-tracked", "body-weight", "waist", "strength-test", "strength",
	"bp-home", "bp-office", "bp-ambulatory", "symptoms", "food-record"}

// budgetAnchor maps a budget key to its 18.8 anchor.
func budgetAnchor(key string) string {
	switch key {
	case "protein_g":
		return "protein"
	case "sat_fat_g":
		return "saturated-fat"
	case "fiber_g":
		return "fibre"
	case "kcal":
		return "energy"
	case "carbs_g":
		return "carbohydrate"
	case "caffeine_mg":
		return "caffeine"
	case "alcohol_g_week", "alcohol_g":
		return "alcohol"
	}
	return ""
}
