package fuel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Maintenance calibration (spec 18.5): an estimate of the maintenance energy
// from Joe's own data, M_cal = mean intake - rho x weight slope, over ONE
// contiguous interval of complete days. fueld never writes the targets file:
// adoption is a decision, recorded by POST /fuel/calibration/accept.

// CalibCandidate is the candidate of a result of state "candidate".
type CalibCandidate struct {
	ResultID        string    `json:"result_id"`
	Kcal            float64   `json:"kcal"`
	UncertaintyKcal float64   `json:"uncertainty_kcal"`
	MassKg          float64   `json:"mass_kg"`
	AvgRunKm        float64   `json:"avg_run_km_per_day"`
	MeanIntakeKcal  float64   `json:"mean_intake_kcal"`
	WeightSlope     float64   `json:"weight_slope_kg_per_week"`
	Interval        [2]string `json:"interval"`
}

// CalibGates are the six gates; all true = candidate.
type CalibGates struct {
	G1 bool `json:"G1"`
	G2 bool `json:"G2"`
	G3 bool `json:"G3"`
	G4 bool `json:"G4"`
	G5 bool `json:"G5"`
	G6 bool `json:"G6"`
}

// CalibResult is `energy.calibration` on the wire (18.5 step 9).
type CalibResult struct {
	State           string          `json:"state"` // off | collecting | blocked | candidate
	ForDate         string          `json:"for_date"`
	InputsAsOf      *time.Time      `json:"inputs_as_of"`
	Interval        *[2]string      `json:"interval"`
	Days            int             `json:"days"`
	DaysNeeded      *int            `json:"days_needed"`
	WeighDays       int             `json:"weigh_days"`
	RunKmPerWeek    *float64        `json:"run_km_per_week"`
	Gates           *CalibGates     `json:"gates"`
	BlockedBy       []string        `json:"blocked_by"`
	SettingsVersion *string         `json:"settings_version"`
	FoodRecordInfo  string          `json:"food_record_info"`
	Candidate       *CalibCandidate `json:"candidate"`
}

// calibRun is one stored run: the wire result plus the unrounded values
// that drift compares.
type calibRun struct {
	At          time.Time   `json:"at"`
	Result      CalibResult `json:"result"`
	MCal        float64     `json:"m_cal"`
	Uncertainty float64     `json:"uncertainty"`
}

// calibAccept is one accept line of calibration.jsonl.
type calibAccept struct {
	AcceptOp        string          `json:"accept_op"`
	Outcome         string          `json:"outcome"` // accepted | rejected
	Accepted        string          `json:"accepted,omitempty"`
	At              time.Time       `json:"at"`
	SettingsVersion string          `json:"settings_version,omitempty"`
	Status          int             `json:"status"`
	Response        json.RawMessage `json:"response"`
}

// calibDay is one day of the calibration inputs.
type calibDay struct {
	Date    string
	Kcal    float64
	Unknown int
	W       *float64 // mean weight of the local day
	RunKm   float64
}

// settingsVersion is the first 12 hex characters of SHA-256 over the
// canonical JSON of the calibration settings, the energy density and the
// run cost factor.
func settingsVersion(c *CalibrationCfg, density, runCost float64) string {
	b, _ := json.Marshal(struct {
		C *CalibrationCfg `json:"calibration"`
		D float64         `json:"energy_density_kcal_per_kg"`
		K float64         `json:"run_cost_kcal_per_kg_km"`
	}{c, density, runCost})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:12]
}

// calibOffReasons are the unmet preconditions (state "off").
func calibOffReasons(t *Targets) []string {
	if t.V2 == nil {
		return []string{"calibration settings not set"}
	}
	var out []string
	e := t.V2.Energy
	if e.Calibration == nil {
		out = append(out, "calibration settings not set")
	}
	if e.EnergyDensity == nil {
		out = append(out, "energy density not set")
	}
	if e.RunCost == nil {
		out = append(out, "run cost factor not set")
	}
	return out
}

// calibOff is the result of state "off" (or, with why, a placeholder while
// no run exists for the date).
func calibOff(t *Targets, date string, why string) CalibResult {
	r := CalibResult{State: "off", ForDate: date, BlockedBy: calibOffReasons(t), FoodRecordInfo: t.info("food-record")}
	if len(r.BlockedBy) == 0 {
		r.State = "blocked"
		r.BlockedBy = []string{why}
		sv := settingsVersion(t.V2.Energy.Calibration, *t.V2.Energy.EnergyDensity, *t.V2.Energy.RunCost)
		r.SettingsVersion = &sv
	}
	return r
}

// computeCalibration is the pure calibration of date D. days are the days
// from the first refreshed date to D-1, ascending and without gaps.
// refreshed is false when a read failed.
func computeCalibration(t *Targets, D string, days []calibDay, stravaFresh bool, refreshed bool, inputsAsOf time.Time) calibRun {
	run := calibRun{Result: CalibResult{State: "off", ForDate: D, BlockedBy: calibOffReasons(t), FoodRecordInfo: t.info("food-record")}}
	res := &run.Result
	if len(res.BlockedBy) > 0 {
		return run
	}
	e := t.V2.Energy
	c := e.Calibration
	rho, k := *e.EnergyDensity, *e.RunCost
	sv := settingsVersion(c, rho, k)
	res.SettingsVersion = &sv
	res.BlockedBy = []string{}
	if !inputsAsOf.IsZero() {
		ia := inputsAsOf.UTC()
		res.InputsAsOf = &ia
	}
	need := c.MinDays
	res.DaysNeeded = &need
	if c.StartDate > dateAdd(D, -1) {
		res.State = "collecting"
		return run
	}
	if !refreshed {
		res.State = "blocked"
		res.BlockedBy = []string{"could not refresh the inputs"}
		res.DaysNeeded = nil
		return run
	}
	complete := func(d calibDay) bool {
		return d.Kcal >= c.MinCompleteKcal && d.Unknown == 0 && !c.isBreak(d.Date) && d.Date >= c.StartDate
	}
	// The most recent run of consecutive complete days that ends on or
	// before D-1; its last max_days days are the interval.
	end := len(days) - 1
	for end >= 0 && !complete(days[end]) {
		end--
	}
	if end < 0 {
		res.State = "collecting"
		return run
	}
	start := end
	for start > 0 && complete(days[start-1]) {
		start--
	}
	if end-start+1 > c.MaxDays {
		start = end - c.MaxDays + 1
	}
	iv := days[start : end+1]
	n := len(iv)
	res.Interval = &[2]string{iv[0].Date, iv[n-1].Date}
	res.Days = n
	left := c.MinDays - n
	if left < 0 {
		left = 0
	}
	res.DaysNeeded = &left
	var sumI, sumR float64
	var xs, ws []float64
	for i, d := range iv {
		sumI += d.Kcal
		sumR += d.RunKm
		if d.W != nil {
			xs = append(xs, float64(i))
			ws = append(ws, *d.W)
		}
	}
	res.WeighDays = len(xs)
	avgKm := sumR / float64(n)
	res.RunKmPerWeek = fptr(round1(avgKm * 7))
	if n < c.MinDays {
		res.State = "collecting"
		return run
	}
	g := &CalibGates{G1: true}
	res.Gates = g
	// G2: the interval ends not more than max_age_days before D-1.
	age := daysBetween(iv[n-1].Date, dateAdd(D, -1))
	g.G2 = age <= c.MaxAgeDays
	if !g.G2 {
		res.BlockedBy = append(res.BlockedBy, fmt.Sprintf("The last complete day is %d days back; at most %d are allowed.", age, c.MaxAgeDays))
	}
	// G3: enough weigh days, also at both edges.
	first, last := 0, 0
	for _, x := range xs {
		if int(x) < c.EdgeDays {
			first++
		}
		if int(x) >= n-c.EdgeDays {
			last++
		}
	}
	g.G3 = len(xs) >= c.MinWeighDays && first >= c.MinEdgeWeighDays && last >= c.MinEdgeWeighDays
	switch {
	case len(xs) < c.MinWeighDays:
		res.BlockedBy = append(res.BlockedBy, fmt.Sprintf("Only %d weigh-ins in the %d days; %d are needed.", len(xs), n, c.MinWeighDays))
	case first < c.MinEdgeWeighDays:
		res.BlockedBy = append(res.BlockedBy, fmt.Sprintf("Only %d weigh-ins in the first %d days; %d are needed.", first, c.EdgeDays, c.MinEdgeWeighDays))
	case last < c.MinEdgeWeighDays:
		res.BlockedBy = append(res.BlockedBy, fmt.Sprintf("Only %d weigh-ins in the last %d days; %d are needed.", last, c.EdgeDays, c.MinEdgeWeighDays))
	}
	// G4: the Strava files are fresh.
	g.G4 = stravaFresh
	if !g.G4 {
		res.BlockedBy = append(res.BlockedBy, "The training data is older than 36 hours.")
	}
	// Maths (a gate that cannot be evaluated is false).
	ibar := sumI / float64(n)
	var mcal, unc, slope, mass float64
	fit := false
	if m := len(xs); m >= 2 {
		var sx, sw float64
		for i := range xs {
			sx += xs[i]
			sw += ws[i]
		}
		mx, mw := sx/float64(m), sw/float64(m)
		var sxx, sxy float64
		for i := range xs {
			sxx += (xs[i] - mx) * (xs[i] - mx)
			sxy += (xs[i] - mx) * (ws[i] - mw)
		}
		if sxx > 0 {
			slope, mass = sxy/sxx, mw
			mcal = ibar - rho*slope
			fit = true
			if m >= 3 {
				var ssr float64
				for i := range xs {
					r := ws[i] - (mw + slope*(xs[i]-mx))
					ssr += r * r
				}
				unc = rho * math.Sqrt((ssr/float64(m-2))/sxx)
				g.G5 = unc <= c.MaxUncertaintyKcal
			}
		}
	}
	if fit {
		run.MCal, run.Uncertainty = mcal, unc // unrounded; not on the wire
	}
	if !g.G5 {
		if fit && len(xs) >= 3 {
			res.BlockedBy = append(res.BlockedBy, fmt.Sprintf("The weight trend is too noisy: plus or minus %s kcal; at most %s are allowed.", fmtNum(roundTo(unc, 10)), fmtNum(c.MaxUncertaintyKcal)))
		} else {
			res.BlockedBy = append(res.BlockedBy, "Too few weigh-ins for a weight trend.")
		}
	}
	// G6: the candidate AS ROUNDED FOR THE WIRE would be a valid maintenance.
	cand := CalibCandidate{Kcal: roundTo(mcal, 10), UncertaintyKcal: roundTo(unc, 10), MassKg: round1(mass), AvgRunKm: round1(avgKm),
		MeanIntakeKcal: roundTo(ibar, 10), WeightSlope: round2(slope * 7), Interval: *res.Interval}
	if fit {
		deficit := 0.0
		if e.DeficitKcal != nil {
			deficit = *e.DeficitKcal
		}
		g.G6 = checkMaintenanceNumbers(cand.Kcal, cand.MassKg, cand.AvgRunKm) == nil && energyFloorOK(cand.Kcal, cand.MassKg, cand.AvgRunKm, k, deficit)
	}
	if !g.G6 && fit {
		res.BlockedBy = append(res.BlockedBy, "The estimate is outside the range of a valid maintenance value; check the food record.")
	}
	if !(g.G2 && g.G3 && g.G4 && g.G5 && g.G6) {
		res.State = "blocked"
		return run
	}
	// result_id: settings version, interval, and the I, W and R of its days.
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s", sv, iv[0].Date, iv[n-1].Date)
	for _, d := range iv {
		w := "-"
		if d.W != nil {
			w = fmt.Sprintf("%.4f", *d.W)
		}
		fmt.Fprintf(h, "|%s:%.1f:%s:%.3f", d.Date, d.Kcal, w, d.RunKm)
	}
	cand.ResultID = hex.EncodeToString(h.Sum(nil))[:16]
	res.State, res.Candidate = "candidate", &cand
	return run
}

// ---- store ----

// calibStore holds calibration.json (the result per local date, the last 60
// dates) and calibration.jsonl (every run and every accept line).
type calibStore struct {
	mu       sync.Mutex
	dir      string
	results  map[string]calibRun
	cands    map[string]CalibCandidate // every candidate ever stored, by result id
	accepted map[string]bool
	accepts  map[string]calibAccept // by accept op
}

func openCalibStore(dir string) (*calibStore, error) {
	cs := &calibStore{dir: dir, results: map[string]calibRun{}, cands: map[string]CalibCandidate{}, accepted: map[string]bool{}, accepts: map[string]calibAccept{}}
	if b, err := os.ReadFile(filepath.Join(dir, "calibration.json")); err == nil {
		var f struct {
			Results map[string]calibRun `json:"results"`
		}
		if json.Unmarshal(b, &f) == nil && f.Results != nil {
			cs.results = f.Results
		}
	}
	err := loadLines(filepath.Join(dir, "calibration.jsonl"), false, func(b []byte) error {
		var probe struct {
			AcceptOp string `json:"accept_op"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			return err
		}
		if probe.AcceptOp != "" {
			var a calibAccept
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			cs.accepts[a.AcceptOp] = a
			if a.Outcome == "accepted" && a.Accepted != "" {
				cs.accepted[a.Accepted] = true
			}
			return nil
		}
		var r calibRun
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if c := r.Result.Candidate; c != nil {
			cs.cands[c.ResultID] = *c
		}
		return nil
	})
	return cs, err
}

func (cs *calibStore) appendLine(v any) error {
	f, err := openAppend(filepath.Join(cs.dir, "calibration.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// putRun appends the run (durable first), then replaces the result of its
// date. Nothing is published in memory when the line could not be written:
// a candidate that is not durable must never be accepted or shown.
func (cs *calibStore) putRun(r calibRun) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err := cs.appendLine(r); err != nil {
		return err
	}
	cs.results[r.Result.ForDate] = r
	if c := r.Result.Candidate; c != nil {
		cs.cands[c.ResultID] = *c
	}
	var dates []string
	for d := range cs.results {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for len(dates) > 60 {
		delete(cs.results, dates[0])
		dates = dates[1:]
	}
	b, err := json.Marshal(map[string]any{"results": cs.results})
	if err != nil {
		return err
	}
	tmp := filepath.Join(cs.dir, "calibration.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// calibration.json is the per-date view; calibration.jsonl (written
	// above) is the durable record that the load check and a restart read.
	return os.Rename(tmp, filepath.Join(cs.dir, "calibration.json"))
}

// hasCandidate reports whether the candidate is stored (durably appended).
func (cs *calibStore) hasCandidate(c *CalibCandidate) bool {
	if c == nil {
		return false
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	got, ok := cs.cands[c.ResultID]
	return ok && got == *c
}

func (cs *calibStore) result(date string) (calibRun, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	r, ok := cs.results[date]
	return r, ok
}

func (cs *calibStore) accept(op string) (calibAccept, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	a, ok := cs.accepts[op]
	return a, ok
}

func (cs *calibStore) putAccept(a calibAccept) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err := cs.appendLine(a); err != nil {
		return err
	}
	cs.accepts[a.AcceptOp] = a
	if a.Outcome == "accepted" && a.Accepted != "" {
		cs.accepted[a.Accepted] = true
	}
	return nil
}

// checkAdoption is the load check of 18.2 for a maintenance object with a
// result_id: a stored candidate with that id and the same numbers, and an
// accepted line for it. It never looks at the date, at newer results or at
// the current settings version.
func (cs *calibStore) checkAdoption(m *Maintenance) error {
	if m == nil || m.ResultID == nil {
		return nil
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.cands[*m.ResultID]
	if !ok {
		return errors.New("targets: energy.maintenance.result_id names no stored calibration result")
	}
	if c.Kcal != roundTo(m.Kcal, 10) || c.MassKg != round1(m.MassKg) || c.AvgRunKm != round1(m.AvgRunKm) || c.Interval != m.Window ||
		m.Kcal != roundTo(m.Kcal, 10) || m.MassKg != round1(m.MassKg) || m.AvgRunKm != round1(m.AvgRunKm) {
		return errors.New("targets: energy.maintenance does not match the calibration result that result_id names")
	}
	if !cs.accepted[*m.ResultID] {
		return errors.New("targets: energy.maintenance.result_id was never accepted (POST /fuel/calibration/accept)")
	}
	return nil
}

// drift (18.5 step 11): each of the last drift_days local dates up to D has
// a candidate result under the CURRENT settings whose unrounded estimate is
// off the adopted value by more than max(drift_kcal, factor x uncertainty),
// all in the same direction.
func (cs *calibStore) drift(t *Targets, D string) bool {
	if t.V2 == nil {
		return false
	}
	e := t.V2.Energy
	if e.Maintenance == nil || e.Calibration == nil || e.EnergyDensity == nil || e.RunCost == nil {
		return false
	}
	c := e.Calibration
	sv := settingsVersion(c, *e.EnergyDensity, *e.RunCost)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	sign := 0
	for i := 0; i < c.DriftDays; i++ {
		r, ok := cs.results[dateAdd(D, -i)]
		if !ok || r.Result.State != "candidate" || r.Result.SettingsVersion == nil || *r.Result.SettingsVersion != sv {
			return false
		}
		diff := r.MCal - e.Maintenance.Kcal
		if math.Abs(diff) <= math.Max(c.DriftKcal, c.DriftUncertaintyFactor*r.Uncertainty) {
			return false
		}
		s := 1
		if diff < 0 {
			s = -1
		}
		if sign != 0 && s != sign {
			return false
		}
		sign = s
	}
	return true
}

// ---- service ----

// RunCalibration refreshes every input and runs the calibration of today.
// The run replaces the stored result of the date. ok is false when a read
// failed (the result is then state "blocked").
func (s *Service) RunCalibration(ctx context.Context) (CalibResult, bool) {
	s.calibMu.Lock()
	defer s.calibMu.Unlock()
	return s.runCalibrationLocked(ctx)
}

func (s *Service) runCalibrationLocked(ctx context.Context) (CalibResult, bool) {
	t, err := s.loadTargets()
	if err != nil || t == nil {
		return CalibResult{State: "off", BlockedBy: []string{"targets invalid"}}, false
	}
	D := s.today()
	s.calibLast = s.o.Now()
	s.calibDirty.Store(false)
	if len(calibOffReasons(t)) > 0 {
		run := computeCalibration(t, D, nil, false, true, time.Time{})
		run.At = s.o.Now()
		return run.Result, true
	}
	c := t.V2.Energy.Calibration
	from := dateAdd(D, -(c.MaxDays + c.MaxAgeDays))
	if c.StartDate > from {
		from = c.StartDate
	}
	last := dateAdd(D, -1)
	refreshed := true
	var asOf time.Time
	var days []calibDay
	if from <= last {
		// Re-read every Food log day of the range (made for the calibration,
		// independent of the cache window), Body composition and the Strava
		// files.
		var dates []string
		for d := from; d <= last; d = dateAdd(d, 1) {
			dates = append(dates, d)
		}
		sem := make(chan struct{}, 6)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, d := range dates {
			wg.Add(1)
			sem <- struct{}{}
			go func(d string) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := s.cache.RefreshDay(ctx, d); err != nil {
					mu.Lock()
					refreshed = false
					mu.Unlock()
				}
			}(d)
		}
		wg.Wait()
		if err := s.cache.RefreshBody(ctx); err != nil {
			refreshed = false
		}
		s.adoptRecordRows()
		acts, _ := s.strava.Load()
		body, bodyAt := s.cache.Body()
		asOf = bodyAt
		wSum, wN := map[string]float64{}, map[string]int{}
		for _, v := range body {
			if kg, ok := v.Data["weight_kg"].(float64); ok && kg > 0 {
				d := bodyLocalDate(v, t.loc)
				wSum[d] += kg
				wN[d]++
			}
		}
		km := map[string]float64{}
		for _, a := range acts {
			if isRun(a.SportType) {
				km[a.LocalDate] += a.DistanceM / 1000
			}
		}
		for _, d := range dates {
			rows, fetched, ok := s.cache.Rows(d)
			if !ok {
				refreshed = false
			}
			if !fetched.IsZero() && (asOf.IsZero() || fetched.Before(asOf)) {
				asOf = fetched
			}
			tot := totalsFromRows(rows)
			cd := calibDay{Date: d, Kcal: float64(tot.Sum["kcal"]) / 10, Unknown: tot.Unknown["kcal"], RunKm: km[d]}
			if wN[d] > 0 {
				cd.W = fptr(wSum[d] / float64(wN[d]))
			}
			days = append(days, cd)
		}
	}
	_, stravaAt := s.strava.Load()
	run := computeCalibration(t, D, days, !stravaStale(s.o.Now(), stravaAt), refreshed, asOf)
	run.At = s.o.Now()
	if ctx.Err() != nil {
		// The service is stopping (or the request budget ran out): the reads
		// were cut, so this is no result. Nothing is stored.
		return run.Result, false
	}
	if err := s.calib.putRun(run); err != nil {
		log.Printf("fuel: calibration: could not store the result: %v", err)
		s.calibStoreErr = err
	} else {
		s.calibStoreErr = nil
	}
	log.Printf("fuel: calibration for %s: state=%s days=%d", D, run.Result.State, run.Result.Days)
	return run.Result, refreshed
}

// calibTick runs the calibration when it is due: at startup when today has
// no result, once per local date from 04:00, and (at most every 10 min)
// after the targets file reloaded or a food write for a recent day was seen.
func (s *Service) calibTick(ctx context.Context, startup bool) {
	t, err := s.loadTargets()
	if err != nil || t == nil || len(calibOffReasons(t)) > 0 {
		return
	}
	now := s.o.Now()
	_, has := s.calib.result(s.today())
	due := !has && (startup || now.In(t.loc).Hour() >= 4)
	s.calibMu.Lock()
	dirty := s.calibDirty.Load() && now.Sub(s.calibLast) >= 10*time.Minute
	s.calibMu.Unlock()
	if due || dirty {
		s.RunCalibration(ctx)
	}
}

// calibFor is the calibration object of a snapshot.
func (s *Service) calibFor(t *Targets, today string) (CalibResult, bool) {
	if len(calibOffReasons(t)) > 0 {
		return calibOff(t, today, ""), false
	}
	sv := settingsVersion(t.V2.Energy.Calibration, *t.V2.Energy.EnergyDensity, *t.V2.Energy.RunCost)
	if r, ok := s.calib.result(today); ok && r.Result.SettingsVersion != nil && *r.Result.SettingsVersion == sv {
		res := r.Result
		res.FoodRecordInfo = t.info("food-record")
		return res, s.calib.drift(t, today)
	}
	return calibOff(t, today, "no calibration run for this date yet"), false
}

func (s *Service) handleCalibrationRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*s.o.Budget)
	defer cancel()
	res, ok := s.RunCalibration(ctx)
	if !ok {
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", true, "could not refresh the inputs; retry"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleCalibrationAccept records an acceptance (18.5 step 10): a fresh run
// under one lock; a candidate is accepted, anything else is refused; the
// line with the full answer is durable BEFORE the answer is sent, and an
// op_id that has a line returns that answer, also after a restart.
func (s *Service) handleCalibrationAccept(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpID string `json:"op_id"`
	}
	if !s.readJSONBody(w, r, &body) {
		return
	}
	if !clientIDRe.MatchString(body.OpID) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "op_id (a uuid) is required"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*s.o.Budget)
	defer cancel()
	s.calibMu.Lock()
	defer s.calibMu.Unlock()
	send := func(a calibAccept) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.Status)
		_, _ = w.Write(append(append([]byte(nil), a.Response...), '\n'))
	}
	if a, ok := s.calib.accept(body.OpID); ok {
		send(a)
		return
	}
	res, _ := s.runCalibrationLocked(ctx)
	if res.State == "candidate" && (s.calibStoreErr != nil || !s.calib.hasCandidate(res.Candidate)) {
		// The run is not durable: an acceptance of it could not be loaded
		// after a restart. Nothing is accepted and no line is written.
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not store the calibration result; nothing was accepted"))
		return
	}
	a := calibAccept{AcceptOp: body.OpID, At: s.o.Now()}
	if res.SettingsVersion != nil {
		a.SettingsVersion = *res.SettingsVersion
	}
	if res.State == "candidate" && res.Candidate != nil {
		a.Outcome, a.Accepted, a.Status = "accepted", res.Candidate.ResultID, http.StatusOK
		a.Response, _ = json.Marshal(map[string]any{"accepted": res.Candidate.ResultID, "candidate": res.Candidate, "calibration": res})
	} else {
		a.Outcome, a.Status = "rejected", http.StatusConflict
		a.Response, _ = json.Marshal(map[string]any{
			"error":       map[string]any{"code": "not_a_candidate", "message": "the calibration has no candidate now", "retryable": false},
			"calibration": res})
	}
	if err := s.calib.putAccept(a); err != nil {
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not record the answer; nothing was accepted"))
		return
	}
	log.Printf("fuel: calibration accept %s: %s", body.OpID, a.Outcome)
	send(a)
}
