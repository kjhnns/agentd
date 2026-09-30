package fuel

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Snapshot is the deterministic day state (spec section 9 and 14).
type Snapshot struct {
	Revision   int          `json:"revision"`
	AsOf       time.Time    `json:"as_of"`
	DataAsOf   time.Time    `json:"data_as_of"`
	Date       string       `json:"date"`
	DayType    string       `json:"day_type"`
	Macros     []MacroState `json:"macros"`
	NextAction *NextAction  `json:"next_action"`
	DayScore   DayScore     `json:"day_score"`
	Week       Week         `json:"week"`
	Weight     Weight       `json:"weight"`
	BodyFat    BodyFat      `json:"body_fat"`
	Streaks    Streaks      `json:"streaks"`
	Missing    []string     `json:"missing"`
}

type MacroState struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Unit          string   `json:"unit"`
	Kind          string   `json:"kind"`
	Consumed      float64  `json:"consumed"`
	UnknownRows   int      `json:"unknown_rows"`
	Target        *float64 `json:"target"`
	PaceTargetNow *float64 `json:"pace_target_now"`
	Status        string   `json:"status"`
}

type NextAction struct {
	Key   string  `json:"key"`
	Grams float64 `json:"grams"`
	By    string  `json:"by"`
	Text  string  `json:"text"`
}

type DayScore struct {
	Hit int `json:"hit"`
	Of  int `json:"of"`
}

type Week struct {
	StrengthSessions int        `json:"strength_sessions"`
	StrengthTarget   int        `json:"strength_target"`
	Runs             int        `json:"runs"`
	RunKm            float64    `json:"run_km"`
	StravaAsOf       *time.Time `json:"strava_as_of"`
}

type WeightPoint struct {
	Date string  `json:"date"`
	Kg   float64 `json:"kg"`
}

type Weight struct {
	Avg7Kg   *float64      `json:"avg7_kg"`
	Delta7Kg *float64      `json:"delta7_kg"`
	Points   []WeightPoint `json:"points"`
}

type FatPoint struct {
	Date string  `json:"date"`
	Pct  float64 `json:"pct"`
}

type BodyFat struct {
	LatestPct  *float64   `json:"latest_pct"`
	LatestDate *string    `json:"latest_date"`
	Points     []FatPoint `json:"points"`
}

type Streaks struct {
	Protein int `json:"protein_g"`
	SatFat  int `json:"sat_fat_g"`
	Fiber   int `json:"fiber_g"`
}

type macroDef struct {
	key, label, unit string
	target           func(*Targets) Target
}

var macroDefs = []macroDef{
	{"protein_g", "Protein", "g", func(t *Targets) Target { return t.Protein }},
	{"sat_fat_g", "Sat fat", "g", func(t *Targets) Target { return t.SatFat }},
	{"fiber_g", "Fibre", "g", func(t *Targets) Target { return t.Fiber }},
	{"kcal", "Energy", "kcal", func(t *Targets) Target { return t.Kcal }},
	{"net_carbs_g", "Net carbs", "g", func(t *Targets) Target { return t.NetCarbs }},
}

var checkpoints = []int{10 * 60, 13 * 60, 16*60 + 30, 20*60 + 30}

func round1(f float64) float64 {
	if f >= 0 {
		return math.Floor(f*10+0.5+1e-9) / 10
	}
	return -math.Floor(-f*10+0.5+1e-9) / 10
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func fptr(f float64) *float64 { return &f }

// snapInput is everything a snapshot is computed from (pure function inputs,
// so the formulas are unit-testable without a server).
type snapInput struct {
	date     string
	now      time.Time
	targets  *Targets
	rowsFor  func(date string) ([]Value, bool)
	strength func(date string) bool
	dataAsOf time.Time
	revision int
	body     []Value
	acts     []Activity
	stravaAt time.Time
}

func dateAdd(date string, days int) string {
	t, _ := time.Parse("2006-01-02", date)
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// elapsedFrac is the share of the eating window passed at local minute m.
func elapsedFrac(t *Targets, minute float64) float64 {
	e := (minute - float64(t.startMin)) / float64(t.endMin-t.startMin)
	return math.Max(0, math.Min(1, e))
}

func statusFor(kind string, consumed float64, target, pace *float64) string {
	if target == nil {
		return "on_pace"
	}
	tg := *target
	switch kind {
	case "cap":
		if consumed > tg {
			return "over"
		}
		return "on_pace"
	case "floor":
		if consumed >= tg {
			return "met"
		}
	}
	p := 0.0
	if pace != nil {
		p = *pace
	}
	switch {
	case consumed >= p+0.1*tg:
		return "ahead"
	case consumed < p-0.1*tg:
		return "behind"
	}
	return "on_pace"
}

// dayTypeFor: training if a Run/Ride/Swim of 45 min or more started that
// local date. Unknown (today only) when the newest Strava file is over 36 h old.
func dayTypeFor(date string, acts []Activity) string {
	for _, a := range acts {
		if a.LocalDate == date && isTrainingSport(a.SportType) && a.MovingS >= 45*60 {
			return "training"
		}
	}
	return "rest"
}

// dayChecks evaluates the day-score checks for totals; nil entries are
// skipped checks (target null, or net carbs on a training day).
func dayChecks(t *Targets, dayType string, tot DayTotals) map[string]*bool {
	c := func(key string) float64 { return float64(tot.Sum[key]) / 10 }
	out := map[string]*bool{}
	b := func(v bool) *bool { return &v }
	if tg := t.Protein.For(dayType); tg != nil {
		out["protein_g"] = b(c("protein_g") >= *tg)
	}
	if tg := t.Fiber.For(dayType); tg != nil {
		out["fiber_g"] = b(c("fiber_g") >= *tg)
	}
	if tg := t.SatFat.For(dayType); tg != nil {
		out["sat_fat_g"] = b(c("sat_fat_g") <= *tg)
	}
	if tg := t.Kcal.For(dayType); tg != nil {
		out["kcal"] = b(math.Abs(c("kcal")-*tg) <= 0.1**tg)
	}
	if dayType != "training" {
		if tg := t.NetCarbs.For(dayType); tg != nil {
			out["net_carbs_g"] = b(c("net_carbs_g") <= *tg)
		}
	}
	return out
}

func computeSnapshot(in snapInput) Snapshot {
	t := in.targets
	loc := t.loc
	nowL := in.now.In(loc)
	today := nowL.Format("2006-01-02")
	isToday := in.date == today
	minute := float64(nowL.Hour()*60+nowL.Minute()) + float64(nowL.Second())/60
	if !isToday {
		minute = float64(24 * 60) // a past date is evaluated at its end
	}
	s := Snapshot{Revision: in.revision, AsOf: in.now.UTC(), DataAsOf: in.dataAsOf.UTC(), Date: in.date, Missing: []string{}}

	// Day type.
	s.DayType = dayTypeFor(in.date, in.acts)
	if isToday && (in.stravaAt.IsZero() || in.now.Sub(in.stravaAt) > 36*time.Hour) && s.DayType != "training" {
		s.DayType = "unknown"
		s.Missing = append(s.Missing, "strava_stale")
	}
	targetDay := s.DayType

	rows, _ := in.rowsFor(in.date)
	tot := totalsFromRows(rows)
	el := elapsedFrac(t, minute)

	for _, d := range macroDefs {
		tg := d.target(t)
		target := tg.For(targetDay)
		consumed := round1(float64(tot.Sum[d.key]) / 10)
		ms := MacroState{Key: d.key, Label: d.label, Unit: d.unit, Kind: tg.Kind, Consumed: consumed, UnknownRows: tot.Unknown[d.key], Target: target}
		if target != nil && tg.Kind != "cap" {
			ms.PaceTargetNow = fptr(round1(*target * el))
		}
		ms.Status = statusFor(tg.Kind, consumed, target, ms.PaceTargetNow)
		s.Macros = append(s.Macros, ms)
	}

	// Next action: among floors that are behind, the one with the largest
	// (pace at the next checkpoint - consumed) relative to target.
	if isToday {
		next := -1
		for _, cp := range checkpoints {
			if float64(cp) > minute {
				next = cp
				break
			}
		}
		if next >= 0 {
			best, bestGap := -1, 0.0
			var bestNeed float64
			for i, ms := range s.Macros {
				if ms.Kind != "floor" || ms.Status != "behind" || ms.Target == nil || *ms.Target <= 0 {
					continue
				}
				paceAt := *ms.Target * elapsedFrac(t, float64(next))
				need := paceAt - ms.Consumed
				gap := need / *ms.Target
				if need > 0 && (best < 0 || gap > bestGap) {
					best, bestGap, bestNeed = i, gap, need
				}
			}
			if best >= 0 {
				ms := s.Macros[best]
				g := math.Ceil(bestNeed - 1e-9)
				by := fmt.Sprintf("%02d:%02d", next/60, next%60)
				s.NextAction = &NextAction{Key: ms.Key, Grams: g, By: by,
					Text: fmt.Sprintf("Eat %.0f g %s by %s", g, strings.ToLower(ms.Label), by)}
			}
		}
	}

	// Day score.
	checks := dayChecks(t, targetDay, tot)
	for _, v := range checks {
		s.DayScore.Of++
		if *v {
			s.DayScore.Hit++
		}
	}

	// Streaks: consecutive past days (yesterday backward, at most 30) where the
	// check passed; the day itself adds 1 only once passed. A day with no rows
	// did not pass (an empty log is not a kept cap).
	passed := func(date, key string) bool {
		r, ok := in.rowsFor(date)
		if !ok || len(dedupeRows(r)) == 0 {
			return false
		}
		dt := dayTypeFor(date, in.acts)
		ch := dayChecks(t, dt, totalsFromRows(r))[key]
		return ch != nil && *ch
	}
	streak := func(key string) int {
		n := 0
		for i := 1; i <= 30; i++ {
			if !passed(dateAdd(in.date, -i), key) {
				break
			}
			n++
		}
		if ch := checks[key]; ch != nil && *ch && len(dedupeRows(rows)) > 0 {
			n++
		}
		return n
	}
	s.Streaks = Streaks{Protein: streak("protein_g"), SatFat: streak("sat_fat_g"), Fiber: streak("fiber_g")}

	// Week (ISO, Monday to Sunday, in targets.tz), up to the date.
	d0, _ := time.ParseInLocation("2006-01-02", in.date, loc)
	wd := (int(d0.Weekday()) + 6) % 7 // Monday = 0
	monday := d0.AddDate(0, 0, -wd).Format("2006-01-02")
	sunday := d0.AddDate(0, 0, 6-wd).Format("2006-01-02")
	s.Week.StrengthTarget = t.StrengthPerWeek
	for i := 0; i <= wd; i++ {
		if in.strength != nil && in.strength(dateAdd(monday, i)) {
			s.Week.StrengthSessions++
		}
	}
	var km float64
	for _, a := range in.acts {
		if isRun(a.SportType) && a.LocalDate >= monday && a.LocalDate <= sunday && a.LocalDate <= in.date {
			s.Week.Runs++
			km += a.DistanceM
		}
	}
	s.Week.RunKm = round1(km / 1000)
	if !in.stravaAt.IsZero() {
		st := in.stravaAt.UTC()
		s.Week.StravaAsOf = &st
	}

	s.Weight, s.BodyFat = bodyTrends(in.body, in.date, loc)
	if s.Weight.Avg7Kg == nil {
		s.Missing = append(s.Missing, "weight")
	}
	if s.BodyFat.LatestPct == nil {
		s.Missing = append(s.Missing, "body_fat")
	}
	if in.dataAsOf.IsZero() {
		s.Missing = append(s.Missing, "food_log")
	}
	return s
}

// bodyLocalDate is the local date of a Body composition row (measured_at in
// the targets tz, else the recordDate).
func bodyLocalDate(v Value, loc *time.Location) string {
	if s, _ := v.Data["measured_at"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.In(loc).Format("2006-01-02")
		}
	}
	return v.RecordDate
}

func bodyTrends(body []Value, date string, loc *time.Location) (Weight, BodyFat) {
	wSum, wN := map[string]float64{}, map[string]int{}
	fSum, fN := map[string]float64{}, map[string]int{}
	for _, v := range body {
		d := bodyLocalDate(v, loc)
		if d == "" || d > date {
			continue
		}
		if kg, ok := v.Data["weight_kg"].(float64); ok && kg > 0 {
			wSum[d] += kg
			wN[d]++
		}
		method, _ := v.Data["method"].(string)
		if method == "withings_bia" || method == "dexa" {
			if pct, ok := v.Data["fat_pct"].(float64); ok && pct > 0 {
				fSum[d] += pct
				fN[d]++
			}
		}
	}
	mean := func(d string) (float64, bool) {
		if wN[d] == 0 {
			return 0, false
		}
		return wSum[d] / float64(wN[d]), true
	}
	avg7 := func(end string) *float64 {
		var sum float64
		n := 0
		for i := 0; i < 7; i++ {
			if m, ok := mean(dateAdd(end, -i)); ok {
				sum += m
				n++
			}
		}
		if n < 3 {
			return nil
		}
		return fptr(round2(sum / float64(n)))
	}
	w := Weight{Points: []WeightPoint{}}
	w.Avg7Kg = avg7(date)
	if prev := avg7(dateAdd(date, -7)); prev != nil && w.Avg7Kg != nil {
		w.Delta7Kg = fptr(round2(*w.Avg7Kg - *prev))
	}
	for i := 29; i >= 0; i-- {
		d := dateAdd(date, -i)
		if m, ok := mean(d); ok {
			w.Points = append(w.Points, WeightPoint{Date: d, Kg: round2(m)})
		}
	}
	bf := BodyFat{Points: []FatPoint{}}
	var dates []string
	for d := range fN {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	from := dateAdd(date, -89)
	for _, d := range dates {
		if d >= from {
			bf.Points = append(bf.Points, FatPoint{Date: d, Pct: round2(fSum[d] / float64(fN[d]))})
		}
	}
	if n := len(dates); n > 0 {
		d := dates[n-1]
		bf.LatestPct = fptr(round2(fSum[d] / float64(fN[d])))
		bf.LatestDate = &d
	}
	return w, bf
}

// statusSentence is the code-generated first text block (spec 14 [C16]),
// e.g. "Protein 137 of 160 g, fibre is the gap."
func statusSentence(s Snapshot) string {
	var protein *MacroState
	for i := range s.Macros {
		if s.Macros[i].Key == "protein_g" {
			protein = &s.Macros[i]
		}
	}
	var parts []string
	if protein != nil {
		if protein.Target != nil {
			parts = append(parts, fmt.Sprintf("Protein %s of %s g", fmtNum(protein.Consumed), fmtNum(*protein.Target)))
		} else {
			parts = append(parts, fmt.Sprintf("Protein %s g", fmtNum(protein.Consumed)))
		}
	}
	// The gap: an over cap first, else the floor furthest behind its pace.
	gap := ""
	worst := 0.0
	for _, m := range s.Macros {
		if m.Kind == "cap" && m.Status == "over" {
			gap = strings.ToLower(m.Label) + " is over the cap"
			break
		}
		if m.Kind == "floor" && m.Status == "behind" && m.Target != nil && m.PaceTargetNow != nil && *m.Target > 0 {
			d := (*m.PaceTargetNow - m.Consumed) / *m.Target
			if d > worst {
				worst, gap = d, strings.ToLower(m.Label)+" is the gap"
			}
		}
	}
	if gap == "" {
		gap = "on track"
	}
	parts = append(parts, gap)
	out := strings.Join(parts, ", ")
	if out != "" {
		out = strings.ToUpper(out[:1]) + out[1:] + "."
	}
	return out
}

func fmtNum(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%.1f", f)
}
