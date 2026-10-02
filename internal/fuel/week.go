package fuel

import (
	"context"
	"math"
	"net/http"
	"time"
)

// The weekly view (spec section 19): the week budget Monday to Sunday and
// the seven-day performance. Deterministic code over the cached food rows,
// the targets file, the strength variables and the Strava files. No model
// call, no write, no clinician-owned value, no status and no score.

// WeekDay is one date of a period.
type WeekDay struct {
	Date      string `json:"date"`
	Weekday   string `json:"weekday"`
	State     string `json:"state"` // past | today | future
	DayType   string `json:"day_type"`
	DayClass  string `json:"day_class"`
	Estimated bool   `json:"estimated"`
	Complete  *bool  `json:"complete"`
}

// DayCell is one budget on one date.
type DayCell struct {
	Date        string   `json:"date"`
	Actual      *float64 `json:"actual"`
	UnknownRows int      `json:"unknown_rows"`
	Target      *float64 `json:"target"`
	Estimated   bool     `json:"estimated"`
	Result      string   `json:"result"` // met | missed | incomplete | open | over | none | future
}

// PeriodBudget is one budget over seven dates.
type PeriodBudget struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Unit        string    `json:"unit"`
	Kind        string    `json:"kind"`
	Info        string    `json:"info"`
	Days        []DayCell `json:"days"`
	DaysMet     *int      `json:"days_met"`
	DaysJudged  int       `json:"days_judged"`
	Consumed    float64   `json:"consumed"`
	UnknownRows int       `json:"unknown_rows"`
	Mean        *float64  `json:"mean"`
	MeanDays    int       `json:"mean_days"`
	MeanTarget  *float64  `json:"mean_target"`
}

// WeekBudget adds the week target, what is left and the pace value.
type WeekBudget struct {
	PeriodBudget
	Target          *float64 `json:"target"`
	TargetEstimated bool     `json:"target_estimated"`
	Remaining       *float64 `json:"remaining"`
	PaceNow         *float64 `json:"pace_now"`
}

// WeekPeriod is the calendar week of R.
type WeekPeriod struct {
	From              string       `json:"from"`
	To                string       `json:"to"`
	Days              []WeekDay    `json:"days"`
	IncompleteDays    int          `json:"incomplete_days"`
	ConsumedIsPartial bool         `json:"consumed_is_partial"`
	Budgets           []WeekBudget `json:"budgets"`
}

// Last7Period is the last seven dates of R.
type Last7Period struct {
	From           string         `json:"from"`
	To             string         `json:"to"`
	Days           []WeekDay      `json:"days"`
	IncompleteDays int            `json:"incomplete_days"`
	Budgets        []PeriodBudget `json:"budgets"`
}

// WeekAmount is a context amount (no judgement).
type WeekAmount struct {
	Unit  string    `json:"unit"`
	Week  float64   `json:"week"`
	Last7 []float64 `json:"last7"`
}

// WeekLever is a lever in the week context: covered days only.
type WeekLever struct {
	Key             string     `json:"key"`
	Label           string     `json:"label"`
	Unit            string     `json:"unit"`
	Week            float64    `json:"week"`
	WeekCoveredDays int        `json:"week_covered_days"`
	Last7           []*float64 `json:"last7"`
}

// WeekContext holds water, caffeine and the levers: sums only.
type WeekContext struct {
	FluidsML   WeekAmount  `json:"fluids_ml"`
	CaffeineMG WeekAmount  `json:"caffeine_mg"`
	Levers     []WeekLever `json:"levers"`
}

// StrengthDayWire is one date of a strength period.
type StrengthDayWire struct {
	Date    string   `json:"date"`
	Sets    *int     `json:"sets"`
	Reps    *float64 `json:"reps"`
	Strava  bool     `json:"strava"`
	Session bool     `json:"session"`
}

// StrengthPeriod is the strength sums of seven dates.
type StrengthPeriod struct {
	Sessions   int               `json:"sessions"`
	Sets       *int              `json:"sets"`
	Reps       *float64          `json:"reps"`
	ByVariable []StrengthVarWire `json:"by_variable"`
	Days       []StrengthDayWire `json:"days"`
}

// WeekStrength is the `strength` object of the week view (19.6).
type WeekStrength struct {
	Rule             string         `json:"rule"`
	SessionsTarget   int            `json:"sessions_target"`
	SessionMinSets   *int           `json:"session_min_sets"`
	Week             StrengthPeriod `json:"week"`
	Last7            StrengthPeriod `json:"last7"`
	MissingVariables []string       `json:"missing_variables"`
	Info             string         `json:"info"`
}

// WeekView is the body of GET /fuel/week.
type WeekView struct {
	Wire        int          `json:"wire"`
	AsOf        time.Time    `json:"as_of"`
	DataAsOf    time.Time    `json:"data_as_of"`
	Date        string       `json:"date"`
	Today       string       `json:"today"`
	Week        WeekPeriod   `json:"week"`
	Last7       Last7Period  `json:"last7"`
	Context     WeekContext  `json:"context"`
	Strength    WeekStrength `json:"strength"`
	CompleteDay struct {
		MinKcal *float64 `json:"min_kcal"`
	} `json:"complete_day"`
	Missing []string `json:"missing"`
}

// WeekCompactBudget is one budget of the snapshot's `week_budgets`.
type WeekCompactBudget struct {
	Key             string   `json:"key"`
	Unit            string   `json:"unit"`
	Kind            string   `json:"kind"`
	Target          *float64 `json:"target"`
	TargetEstimated bool     `json:"target_estimated"`
	Consumed        float64  `json:"consumed"`
	Remaining       *float64 `json:"remaining"`
	PaceNow         *float64 `json:"pace_now"`
	Last7DaysMet    *int     `json:"last7_days_met"`
	Last7DaysJudged int      `json:"last7_days_judged"`
}

// WeekCompact is the snapshot's `week_budgets` (19.7).
type WeekCompact struct {
	From              string              `json:"from"`
	To                string              `json:"to"`
	IncompleteDays    int                 `json:"incomplete_days"`
	ConsumedIsPartial bool                `json:"consumed_is_partial"`
	Budgets           []WeekCompactBudget `json:"budgets"`
}

// weekBudgetDef is one of the six week budgets, in wire order.
type weekBudgetDef struct {
	key, label, unit string
	cmp              string // floor | cap | energy | none: the comparison by KEY
}

var weekBudgetDefs = []weekBudgetDef{
	{"protein_g", "Protein", "g", "floor"},
	{"sat_fat_g", "Sat fat", "g", "cap"},
	{"fiber_g", "Fibre", "g", "floor"},
	{"kcal", "Energy", "kcal", "energy"},
	{"carbs_g", "Carbohydrate", "g", "floor"},
	{"alcohol_g", "Alcohol", "g", "none"},
}

const weekMaxBack = 28

// mondayOf is the Monday of the calendar week of a local date. Calendar
// steps on dates only: a day with 23 or 25 hours is one date like the others.
func mondayOf(date string) string {
	d, _ := time.Parse("2006-01-02", date)
	return dateAdd(date, -((int(d.Weekday()) + 6) % 7))
}

func weekdayOf(date string) string {
	d, _ := time.Parse("2006-01-02", date)
	return d.Weekday().String()[:3]
}

// weekDates are the dates the view of R needs loaded: the week dates that
// are not future and the last seven dates.
func weekDates(r, today string) []string {
	seen := map[string]bool{}
	var out []string
	mon := mondayOf(r)
	for i := 0; i < 7; i++ {
		if d := dateAdd(mon, i); d <= today && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for i := 6; i >= 0; i-- {
		if d := dateAdd(r, -i); !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// kindOf is the `kind` of a week budget on the wire: the file's kind for
// the four macros, floor for carbs_g, cap for alcohol_g.
func (in *snapInput) weekKind(key string) string {
	t := in.targets
	switch key {
	case "protein_g":
		return t.Protein.Kind
	case "sat_fat_g":
		return t.SatFat.Kind // "budget" with the budget form (18.14)
	case "fiber_g":
		return t.Fiber.Kind
	case "kcal":
		return t.Kcal.Kind
	case "carbs_g":
		return "floor"
	}
	return "cap"
}

// classDependent: the daily target can differ between dates.
func (in *snapInput) classDependent(key string) bool {
	t := in.targets
	switch key {
	case "kcal", "carbs_g":
		return true
	case "protein_g":
		return t.Protein.hasSplit
	case "sat_fat_g":
		return t.SatFat.hasSplit || t.SatFat.Kind == "budget" // a share of the day's energy
	case "fiber_g":
		return t.Fiber.hasSplit
	}
	return false
}

// dayTarget is T(b, d).
func (in *snapInput) dayTarget(key string, dd *dayData) *float64 {
	t := in.targets
	switch key {
	case "protein_g":
		return t.Protein.For(dd.plan.DayType)
	case "sat_fat_g":
		return dd.plan.SatFatTarget
	case "fiber_g":
		return t.Fiber.For(dd.plan.DayType)
	case "kcal":
		return dd.plan.EnergyTarget
	case "carbs_g":
		return dd.plan.CarbsTarget
	}
	return nil // alcohol_g: the target is for the week as a whole
}

// dayActual is A(b, d) and its unknown rows.
func dayActual(key string, dd *dayData) (float64, int) {
	if key == "alcohol_g" {
		return round1(float64(dd.alcohol) / 10), 0
	}
	return round1(float64(dd.tot.Sum[key]) / 10), dd.tot.Unknown[key]
}

// complete is the complete-day rule (19.1); nil for today and the future.
func (in *snapInput) complete(dd *dayData) *bool {
	if dd.plan.State != "past" {
		return nil
	}
	ok := false
	if c := in.calibCfg(); c != nil {
		ok = dd.loaded && float64(dd.tot.Sum["kcal"])/10 >= c.MinCompleteKcal && dd.tot.Unknown["kcal"] == 0 && !c.isBreak(dd.plan.Date)
	}
	return &ok
}

func (in *snapInput) calibCfg() *CalibrationCfg {
	if in.targets.V2 == nil {
		return nil
	}
	return in.targets.V2.Energy.Calibration
}

// cellResult applies the rules of 19.1 in their order.
func cellResult(cmp string, dd *dayData, complete *bool, a float64, unknown int, t *float64) string {
	switch {
	case dd.plan.State == "future":
		return "future"
	case t == nil || cmp == "none":
		return "none"
	case dd.plan.State == "today":
		// The day is not over: only what more food cannot change.
		switch cmp {
		case "floor":
			if a >= *t {
				return "met"
			}
		case "cap":
			if a > *t {
				return "over"
			}
		case "energy":
			if a > 1.1**t {
				return "over"
			}
		}
		return "open"
	case complete == nil || !*complete:
		return "incomplete"
	}
	switch cmp {
	case "floor":
		if a >= *t {
			return "met"
		}
		if unknown > 0 {
			return "incomplete" // the known sum is a lower bound
		}
		return "missed"
	case "cap":
		if a > *t {
			return "missed"
		}
		if unknown > 0 {
			return "incomplete"
		}
		return "met"
	}
	if unknown > 0 {
		return "incomplete"
	}
	if math.Abs(a-*t) <= 0.1**t {
		return "met"
	}
	return "missed"
}

func (in *snapInput) weekDay(date string) (WeekDay, *dayData) {
	dd := in.day(date)
	return WeekDay{Date: date, Weekday: weekdayOf(date), State: dd.plan.State, DayType: dd.plan.DayType, DayClass: dd.plan.DayClass,
		Estimated: dd.plan.State == "future", Complete: in.complete(dd)}, dd
}

// periodBudget computes one budget over the given dates.
func (in *snapInput) periodBudget(def weekBudgetDef, dates []string) PeriodBudget {
	pb := PeriodBudget{Key: def.key, Label: def.label, Unit: def.unit, Kind: in.weekKind(def.key), Info: in.targets.info(budgetAnchor(def.key))}
	met := 0
	var sum, meanSum, meanT float64
	meanTOK := true
	for _, date := range dates {
		dd := in.day(date)
		t := in.dayTarget(def.key, dd)
		cell := DayCell{Date: date, Target: t, Estimated: dd.plan.State == "future" && in.classDependent(def.key)}
		complete := in.complete(dd)
		var a float64
		if dd.plan.State != "future" {
			var unk int
			a, unk = dayActual(def.key, dd)
			cell.Actual, cell.UnknownRows = fptr(a), unk
			sum += a
			pb.UnknownRows += unk
		}
		cell.Result = cellResult(def.cmp, dd, complete, a, cell.UnknownRows, t)
		switch cell.Result {
		case "met":
			met++
			pb.DaysJudged++
		case "missed":
			pb.DaysJudged++
		}
		// Mean dates: past, complete, and no unknown amount of this budget.
		if complete != nil && *complete && cell.UnknownRows == 0 {
			pb.MeanDays++
			meanSum += a
			if t == nil {
				meanTOK = false
			} else {
				meanT += *t
			}
		}
		pb.Days = append(pb.Days, cell)
	}
	pb.Consumed = round1(sum)
	if def.cmp != "none" {
		pb.DaysMet = &met
	} else {
		pb.DaysJudged = 0
	}
	if pb.MeanDays > 0 {
		pb.Mean = fptr(round1(meanSum / float64(pb.MeanDays)))
		if meanTOK {
			pb.MeanTarget = fptr(round1(meanT / float64(pb.MeanDays)))
		}
	}
	return pb
}

// weekView computes the whole view of the reference date in.date.
func (in *snapInput) weekView() WeekView {
	t := in.targets
	r, today := in.date, in.today()
	v := WeekView{Wire: wireVersion, AsOf: in.now.UTC(), Date: r, Today: today, Missing: []string{}}
	mon := mondayOf(r)
	var wk, l7 []string
	for i := 0; i < 7; i++ {
		wk = append(wk, dateAdd(mon, i))
		l7 = append(l7, dateAdd(r, i-6))
	}
	nowL := in.now.In(t.loc)
	elapsed := elapsedFrac(t, float64(nowL.Hour()*60+nowL.Minute())+float64(nowL.Second())/60)

	// Week.
	v.Week.From, v.Week.To = wk[0], wk[6]
	future, past, todayIn := 0, 0, false
	for _, d := range wk {
		wd, dd := in.weekDay(d)
		v.Week.Days = append(v.Week.Days, wd)
		switch dd.plan.State {
		case "future":
			future++
		case "today":
			todayIn = true
		default:
			past++
			if wd.Complete != nil && !*wd.Complete {
				v.Week.IncompleteDays++
			}
		}
	}
	v.Week.ConsumedIsPartial = v.Week.IncompleteDays > 0
	for _, def := range weekBudgetDefs {
		wb := WeekBudget{PeriodBudget: in.periodBudget(def, wk)}
		if def.key == "alcohol_g" {
			// The week cap, as the snapshot of R selects it. Not a sum.
			if t.AlcoholGWeek != nil {
				wb.Target = t.AlcoholGWeek.For(in.day(r).plan.DayType)
			}
			if wb.Target != nil {
				share := float64(past)
				if todayIn {
					share += elapsed
				}
				wb.PaceNow = fptr(round1(*wb.Target * share / 7))
			}
		} else {
			var sum, pace float64
			sumOK, paceOK := true, true
			for _, d := range wk {
				dd := in.day(d)
				tg := in.dayTarget(def.key, dd)
				if tg == nil {
					sumOK = false
					if dd.plan.State != "future" {
						paceOK = false
					}
					continue
				}
				sum += *tg
				switch dd.plan.State {
				case "past":
					pace += *tg
				case "today":
					pace += *tg * elapsed
				}
			}
			if sumOK {
				wb.Target = fptr(round1(sum))
			}
			if paceOK {
				wb.PaceNow = fptr(round1(pace))
			}
			wb.TargetEstimated = future > 0 && in.classDependent(def.key)
		}
		if wb.Target != nil {
			wb.Remaining = fptr(round1(*wb.Target - wb.Consumed))
		}
		v.Week.Budgets = append(v.Week.Budgets, wb)
	}

	// Last seven dates.
	v.Last7.From, v.Last7.To = l7[0], l7[6]
	for _, d := range l7 {
		wd, _ := in.weekDay(d)
		v.Last7.Days = append(v.Last7.Days, wd)
		if wd.Complete != nil && !*wd.Complete {
			v.Last7.IncompleteDays++
		}
	}
	for _, def := range weekBudgetDefs {
		v.Last7.Budgets = append(v.Last7.Budgets, in.periodBudget(def, l7))
	}

	// Context: sums only.
	v.Context.FluidsML = WeekAmount{Unit: "ml", Last7: []float64{}}
	v.Context.CaffeineMG = WeekAmount{Unit: "mg", Last7: []float64{}}
	var fl, caf int64
	for _, d := range wk {
		if dd := in.day(d); dd.plan.State != "future" {
			fl += dd.fluids
			caf += dd.caffeine
		}
	}
	v.Context.FluidsML.Week, v.Context.CaffeineMG.Week = round1(float64(fl)/10), round1(float64(caf)/10)
	for _, d := range l7 {
		dd := in.day(d)
		v.Context.FluidsML.Last7 = append(v.Context.FluidsML.Last7, round1(float64(dd.fluids)/10))
		v.Context.CaffeineMG.Last7 = append(v.Context.CaffeineMG.Last7, round1(float64(dd.caffeine)/10))
	}
	for l, key := range leverKeys {
		wl := WeekLever{Key: key, Label: leverLabels[l], Unit: "g", Last7: []*float64{}}
		var sum int64
		for _, d := range wk {
			if dd := in.day(d); dd.plan.State != "future" && dd.lev.covered(l) {
				sum += dd.lev.consumed[l]
				wl.WeekCoveredDays++
			}
		}
		wl.Week = round1(float64(sum) / 10)
		for _, d := range l7 {
			var p *float64
			if dd := in.day(d); dd.lev.covered(l) {
				p = fptr(round1(float64(dd.lev.consumed[l]) / 10))
			}
			wl.Last7 = append(wl.Last7, p)
		}
		v.Context.Levers = append(v.Context.Levers, wl)
	}

	// Strength.
	si := in.si
	v.Strength = WeekStrength{Rule: si.rule(), SessionsTarget: t.StrengthPerWeek, MissingVariables: si.missingNames(), Info: t.info("strength")}
	if si.cfg != nil {
		n := si.cfg.SessionMinSets
		v.Strength.SessionMinSets = &n
	}
	v.Strength.Week = in.strengthPeriod(wk, today)
	v.Strength.Last7 = in.strengthPeriod(l7, today)

	if c := in.calibCfg(); c != nil {
		v.CompleteDay.MinKcal = fptr(c.MinCompleteKcal)
	} else {
		v.Missing = append(v.Missing, "complete_day_rule_unset")
	}
	if td := in.day(today); td.plan.Stale {
		v.Missing = append(v.Missing, "strava_stale")
	}
	if t.V2 != nil && t.V2.Carbs.ClassMinutes == nil {
		v.Missing = append(v.Missing, "day_class_unset")
	}
	// data_as_of: the oldest read time of the days used.
	for _, d := range weekDates(r, today) {
		if in.fetched == nil {
			break
		}
		if f := in.fetched(d); !f.IsZero() && (v.DataAsOf.IsZero() || f.Before(v.DataAsOf)) {
			v.DataAsOf = f.UTC()
		}
	}
	return v
}

// strengthPeriod sums the dates that are past or today; a future date has
// sets 0, reps 0, strava false and session false.
func (in *snapInput) strengthPeriod(dates []string, today string) StrengthPeriod {
	si := in.si
	var upTo []string
	for _, d := range dates {
		if d <= today {
			upTo = append(upTo, d)
		}
	}
	sessions, week, days := si.sum(upTo)
	p := StrengthPeriod{Sessions: sessions}
	if week != nil {
		p.Sets, p.Reps, p.ByVariable = &week.Sets, &week.Reps, week.ByVariable
	}
	for i, d := range dates {
		w := StrengthDayWire{Date: d}
		if si.cfg != nil {
			w.Sets, w.Reps = new(int), new(float64)
		}
		if i < len(days) && d <= today {
			sd := days[i]
			w.Strava, w.Session = sd.Strava, sd.Session
			if si.cfg != nil {
				*w.Sets, *w.Reps = sd.Sets, sd.Reps
			}
		}
		p.Days = append(p.Days, w)
	}
	return p
}

// weekCompact is the snapshot's `week_budgets`: nil when the route would
// refuse the date or a needed date was never read (no made-up sums).
func (in *snapInput) weekCompact() *WeekCompact {
	r, today := in.date, in.today()
	if r > today || r < dateAdd(today, -weekMaxBack) {
		return nil
	}
	for _, d := range weekDates(r, today) {
		if _, ok := in.rowsFor(d); !ok {
			return nil
		}
	}
	v := in.weekView()
	c := &WeekCompact{From: v.Week.From, To: v.Week.To, IncompleteDays: v.Week.IncompleteDays, ConsumedIsPartial: v.Week.ConsumedIsPartial}
	for i, b := range v.Week.Budgets {
		l := v.Last7.Budgets[i]
		c.Budgets = append(c.Budgets, WeekCompactBudget{Key: b.Key, Unit: b.Unit, Kind: b.Kind, Target: b.Target, TargetEstimated: b.TargetEstimated,
			Consumed: b.Consumed, Remaining: b.Remaining, PaceNow: b.PaceNow, Last7DaysMet: l.DaysMet, Last7DaysJudged: l.DaysJudged})
	}
	return c
}

// handleWeek serves GET /fuel/week (19.4).
func (s *Service) handleWeek(w http.ResponseWriter, r *http.Request) {
	today := s.today()
	date := r.URL.Query().Get("date")
	if date == "" {
		date = today
	}
	if !validDate(date) || date > today || date < dateAdd(today, -weekMaxBack) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "date must be today or one of the 28 days before it"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.Budget)
	defer cancel()
	// Today fresh, every needed date loaded; a failed read is a 502.
	ok := s.freshen(ctx, today)
	for _, d := range weekDates(date, today) {
		if !ok {
			break
		}
		if !s.cache.Loaded(d) {
			ok = s.cache.RefreshDay(ctx, d) == nil
		}
	}
	if !ok {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the food log; retry"))
		return
	}
	s.stateMu.RLock()
	in, err := s.snapInputFor(date)
	s.stateMu.RUnlock()
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid"))
		return
	}
	for _, d := range weekDates(date, today) {
		if _, loaded := in.rowsFor(d); !loaded {
			writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not read the food log; retry"))
			return
		}
	}
	writeJSON(w, http.StatusOK, in.weekView())
}
