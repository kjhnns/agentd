package fuel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Activity energy (18.15, 2026-10-14, Joe: "the calories amount per day
// doesn't account for any activity, steps, or exercises I do"). With
// `energy.activity` set, the day's energy target counts the run by its
// distance, the other Strava sports, a strength session and the walking
// steps above the person's own average. Without the key the target is the
// 18.5 one, unchanged.

// ActivityCfg is `energy.activity`.
type ActivityCfg struct {
	WalkCost            float64 // kcal per kg per km of walking (net of rest)
	StepLengthM         float64
	RunCadenceSpm       float64 // steps per minute of a run without a cadence in Strava
	StrengthSessionKcal float64
	BaselineDays        int // trailing dates of the steps and sport averages
	MinBaselineDays     int // dates with steps data before steps count
}

func parseActivityCfg(raw json.RawMessage) (*ActivityCfg, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	keys := []string{"walk_cost_kcal_per_kg_km", "step_length_m", "run_cadence_spm", "strength_session_kcal", "baseline_days", "min_baseline_days"}
	m, err := obj("energy.activity", raw, keys)
	if err != nil {
		return nil, err
	}
	c := &ActivityCfg{}
	for _, f := range []struct {
		key    string
		v      *float64
		lo, hi float64
	}{{"walk_cost_kcal_per_kg_km", &c.WalkCost, 0.2, 1.0}, {"step_length_m", &c.StepLengthM, 0.4, 1.2},
		{"run_cadence_spm", &c.RunCadenceSpm, 120, 220}, {"strength_session_kcal", &c.StrengthSessionKcal, 0, 600}} {
		n, err := numOrNil("energy.activity."+f.key, m[f.key])
		if err != nil {
			return nil, err
		}
		if n == nil {
			return nil, fmt.Errorf("targets: energy.activity.%s must be a number", f.key)
		}
		if err := inRange("energy.activity."+f.key, n, f.lo, f.hi); err != nil {
			return nil, err
		}
		*f.v = *n
	}
	for _, f := range []struct {
		key    string
		v      *int
		lo, hi int
	}{{"baseline_days", &c.BaselineDays, 7, 60}, {"min_baseline_days", &c.MinBaselineDays, 3, 60}} {
		n, err := intOrNil("energy.activity."+f.key, m[f.key])
		if err != nil {
			return nil, err
		}
		if n == nil || *n < f.lo || *n > f.hi {
			return nil, fmt.Errorf("targets: energy.activity.%s must be an integer %d to %d", f.key, f.lo, f.hi)
		}
		*f.v = *n
	}
	if c.MinBaselineDays > c.BaselineDays {
		return nil, errors.New("targets: energy.activity.min_baseline_days must not exceed baseline_days")
	}
	return c, nil
}

// sportMET is the MET of a Strava sport type that is neither a run (by
// distance), nor walking (in the steps), nor strength (the session).
// Compendium of Physical Activities, moderate effort. Unknown sports: 4.
var sportMET = map[string]float64{
	"Ride": 7, "VirtualRide": 7, "GravelRide": 8, "MountainBikeRide": 8.5, "EBikeRide": 4,
	"Rowing": 7, "VirtualRow": 7, "Swim": 7, "Kayaking": 5, "Canoeing": 5, "StandUpPaddling": 6,
	"Elliptical": 5, "StairStepper": 9, "Crossfit": 8, "HighIntensityIntervalTraining": 8,
	"Yoga": 2.5, "Pilates": 3, "Tennis": 7, "Squash": 7, "Badminton": 5.5, "Soccer": 7,
	"NordicSki": 9, "BackcountrySki": 8, "AlpineSki": 5.5, "Snowboard": 5, "Surfing": 3, "RockClimbing": 7.5,
	"InlineSkate": 7.5, "IceSkate": 7, "Golf": 4.5, "Volleyball": 4, "Workout": 4,
}

// inSteps are the sports whose energy the step count already holds.
func inSteps(s string) bool { return s == "Walk" || s == "Hike" }

func isStrengthSport(s string) bool { return s == "WeightTraining" }

// otherSportKcal is the energy above rest of the date's other sports: the
// power meter's kilojoules for a ride (about 1 kcal per kJ of work at a
// 24 % efficiency), else the net MET cost of the moving time.
func otherSportKcal(acts []Activity, date string, mass float64) float64 {
	sum := 0.0
	for _, a := range acts {
		if a.LocalDate != date || isRun(a.SportType) || inSteps(a.SportType) || isStrengthSport(a.SportType) {
			continue
		}
		if strings.Contains(a.SportType, "Ride") && a.Kilojoules > 0 {
			sum += a.Kilojoules
			continue
		}
		met, ok := sportMET[a.SportType]
		if !ok {
			met = 4
		}
		sum += (met - 1) * 3.5 * mass / 200 * a.MovingS / 60
	}
	return sum
}

// runSteps estimates the steps of the date's runs, which the phone's step
// count also holds. Strava's running cadence is per leg on Garmin data.
func runSteps(acts []Activity, date string, defSpm float64) float64 {
	sum := 0.0
	for _, a := range acts {
		if a.LocalDate != date || !isRun(a.SportType) {
			continue
		}
		spm := defSpm
		if c := a.AvgCadence; c > 0 {
			spm = c
			if c < 120 {
				spm = 2 * c
			}
		}
		sum += spm * a.MovingS / 60
	}
	return sum
}

// activityInput is what the day plan needs beyond the Strava activities.
type activityInput struct {
	steps   func(date string) (int, bool)
	session func(date string) bool // a strength session (19.6)
}

// activityParts are the parts of the energy target from activity.
type activityParts struct {
	Run, Other, Strength, Steps float64
	StepCount                   *int
	StepBaseline                *float64
}

// walkSteps is the steps of a date without the run steps (none: false).
func walkSteps(ax *activityInput, acts []Activity, cfg *ActivityCfg, date string) (float64, bool) {
	if ax == nil || ax.steps == nil {
		return 0, false
	}
	n, ok := ax.steps(date)
	if !ok {
		return 0, false
	}
	return math.Max(0, float64(n)-runSteps(acts, date, cfg.RunCadenceSpm)), true
}

// baselines are the means over the BaselineDays dates before date: walking
// steps (over the dates with data, nil below MinBaselineDays), other sport
// kcal and strength kcal (over all dates).
func baselines(ax *activityInput, acts []Activity, cfg *ActivityCfg, date string, mass float64) (steps *float64, other, strength float64) {
	sum, n := 0.0, 0
	for i := 1; i <= cfg.BaselineDays; i++ {
		d := dateAdd(date, -i)
		if w, ok := walkSteps(ax, acts, cfg, d); ok {
			sum += w
			n++
		}
		other += otherSportKcal(acts, d, mass)
		if ax != nil && ax.session != nil && ax.session(d) {
			strength += cfg.StrengthSessionKcal
		}
	}
	other /= float64(cfg.BaselineDays)
	strength /= float64(cfg.BaselineDays)
	if n >= cfg.MinBaselineDays && n > 0 {
		steps = fptr(sum / float64(n))
	}
	return steps, other, strength
}

// activityEnergy computes the parts for a past date or today. relative:
// the formula state, whose maintenance already holds the average of the
// other sports, strength and walking, so only the difference to the
// trailing average is added (the run keeps its 18.5 rule, set by the
// caller). The steps part is relative in both states: the provisional
// rest value assumes ordinary walking. Today's steps are partial, so only
// steps above the average count until the date is over.
func activityEnergy(ax *activityInput, acts []Activity, cfg *ActivityCfg, date string, isToday, relative bool, mass float64) activityParts {
	var p activityParts
	bSteps, bOther, bStrength := baselines(ax, acts, cfg, date, mass)
	p.Other = otherSportKcal(acts, date, mass)
	if ax != nil && ax.session != nil && ax.session(date) {
		p.Strength = cfg.StrengthSessionKcal
	}
	if relative {
		p.Other -= bOther
		p.Strength -= bStrength
	}
	if n, ok := ax.stepsOf(date); ok {
		p.StepCount = &n
	}
	p.StepBaseline = bSteps
	if w, ok := walkSteps(ax, acts, cfg, date); ok && bSteps != nil {
		p.Steps = cfg.WalkCost * mass * cfg.StepLengthM / 1000 * (w - *bSteps)
		if isToday && p.Steps < 0 {
			p.Steps = 0
		}
	}
	return p
}

func (ax *activityInput) stepsOf(date string) (int, bool) {
	if ax == nil || ax.steps == nil {
		return 0, false
	}
	return ax.steps(date)
}

// ---- steps store: state_dir/steps.json, one count per local date ----

type stepsDay struct {
	Steps int       `json:"steps"`
	At    time.Time `json:"at"`
}

type stepsStore struct {
	mu   sync.Mutex
	path string
	days map[string]stepsDay
}

const stepsKeepDays = 90

func openStepsStore(stateDir string) (*stepsStore, error) {
	s := &stepsStore{path: filepath.Join(stateDir, "steps.json"), days: map[string]stepsDay{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.days); err != nil {
		return nil, fmt.Errorf("steps.json: %w", err)
	}
	return s, nil
}

// Snapshot returns a copy for one snapshot (no lock held while computing).
func (s *stepsStore) Snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.days))
	for d, v := range s.days {
		out[d] = v.Steps
	}
	return out
}

// Put stores the counts (a later count of a date replaces the earlier one)
// and drops dates older than stepsKeepDays before today. The file is
// written and synced before Put returns.
func (s *stepsStore) Put(days map[string]int, now time.Time, today string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]stepsDay, len(s.days)+len(days))
	for d, v := range s.days {
		next[d] = v
	}
	for d, n := range days {
		next[d] = stepsDay{Steps: n, At: now.UTC()}
	}
	cut := dateAdd(today, -stepsKeepDays)
	for d := range next {
		if d < cut {
			delete(next, d)
		}
	}
	b, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.days = next
	return nil
}

// handleSteps is POST /fuel/steps: {"days":[{"date":"YYYY-MM-DD","steps":int}]}
// (at most 60 dates, none after today in targets.tz). The phone sends the
// step count of Apple Health per local date; a date sent again replaces
// its count. Answer: {"stored":n,"dates":[...]}.
func (s *Service) handleSteps(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Days []struct {
			Date  string `json:"date"`
			Steps *int   `json:"steps"`
		} `json:"days"`
	}
	if !s.readJSONBody(w, r, &body) {
		return
	}
	if len(body.Days) == 0 || len(body.Days) > 60 {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "days: 1 to 60 dates"))
		return
	}
	t, err := s.loadTargets()
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "the targets file is invalid; fix it on the server"))
		return
	}
	now := s.o.Now()
	today := now.In(t.loc).Format("2006-01-02")
	days := map[string]int{}
	for _, d := range body.Days {
		if !validDate(d.Date) || d.Date > today || d.Date < dateAdd(today, -stepsKeepDays) {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "date %q: a date of the last %d days, not after today", d.Date, stepsKeepDays))
			return
		}
		if d.Steps == nil || *d.Steps < 0 || *d.Steps > 150000 {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "steps of %s: an integer 0 to 150000", d.Date))
			return
		}
		days[d.Date] = *d.Steps
	}
	if err := s.steps.Put(days, now, today); err != nil {
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not store the steps"))
		return
	}
	dates := make([]string, 0, len(days))
	for d := range days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	writeJSON(w, http.StatusOK, map[string]any{"stored": len(dates), "dates": dates})
}
