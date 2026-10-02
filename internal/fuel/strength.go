package fuel

import (
	"math"
	"strconv"
	"strings"
)

// Strength (spec 18.6, D12 as Joe decided it on 2026-10-02): each value of a
// strength variable is ONE SET and its number is the reps of that set. A
// local date with at least session_min_sets sets across the variables, or
// with a Strava strength activity, is a session. Read only: fueld never
// writes a strength variable. Hard sets stay clinician-owned: no label, no
// count, no target.

// numVal is one cached non-json value of a date.
type numVal struct {
	VarID string
	ID    string
	Raw   string
}

// strengthVar is a set variable resolved to its id.
type strengthVar struct {
	Name string
	ID   string
}

// strengthInput is what the strength rules read.
type strengthInput struct {
	cfg     *StrengthCfg  // nil = rule "legacy"
	vars    []strengthVar // resolved set variables, in file order
	missing []string      // names that did not resolve
	nums    func(date string) []numVal
	legacy  func(date string) bool // the v6 predicate (Push ups / Pull ups)
	acts    []Activity
}

// strengthDay is the strength state of one date.
type strengthDay struct {
	Sets    int
	Reps    float64
	ByVar   []strengthVarSum // by resolved variable, file order
	Strava  bool
	Session bool
}

type strengthVarSum struct {
	Sets int
	Reps float64
}

// setReps parses the stored text of a value: a set is a number above 0.
func setReps(raw string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, false
	}
	return f, true
}

func (si strengthInput) day(date string) strengthDay {
	var d strengthDay
	if si.cfg == nil {
		d.Session = si.legacy != nil && si.legacy(date)
		return d
	}
	d.ByVar = make([]strengthVarSum, len(si.vars))
	idx := map[string]int{}
	for i, v := range si.vars {
		idx[v.ID] = i
	}
	if si.nums != nil {
		seen := map[string]bool{}
		for _, n := range si.nums(date) {
			i, ok := idx[n.VarID]
			if !ok || (n.ID != "" && seen[n.ID]) {
				continue
			}
			seen[n.ID] = true
			if reps, ok := setReps(n.Raw); ok {
				d.Sets++
				d.Reps += reps
				d.ByVar[i].Sets++
				d.ByVar[i].Reps += reps
			}
		}
	}
	for _, a := range si.acts {
		if a.LocalDate != date {
			continue
		}
		for _, st := range si.cfg.StravaSportTypes {
			if a.SportType == st {
				d.Strava = true
			}
		}
	}
	d.Session = d.Sets >= si.cfg.SessionMinSets || d.Strava
	return d
}

// StrengthVarWire is one variable's sets and reps.
type StrengthVarWire struct {
	Name string  `json:"name"`
	Sets int     `json:"sets"`
	Reps float64 `json:"reps"`
}

// StrengthWeek is the sets and reps of a period (context, no target).
type StrengthWeek struct {
	Sets       int               `json:"sets"`
	Reps       float64           `json:"reps"`
	ByVariable []StrengthVarWire `json:"by_variable"`
}

// StrengthState is the snapshot's `strength` object (spec 18.7).
type StrengthState struct {
	Rule             string        `json:"rule"` // sets | legacy
	Sessions         int           `json:"sessions"`
	SessionsTarget   int           `json:"sessions_target"`
	SessionMinSets   *int          `json:"session_min_sets"`
	Week             *StrengthWeek `json:"week"`
	MissingVariables []string      `json:"missing_variables"`
	Clinician        *Instruction  `json:"clinician"`
	Info             string        `json:"info"`
}

// sum adds the days of a period (the dates given, in order).
func (si strengthInput) sum(dates []string) (sessions int, week *StrengthWeek, days []strengthDay) {
	if si.cfg != nil {
		week = &StrengthWeek{ByVariable: make([]StrengthVarWire, len(si.vars))}
		for i, v := range si.vars {
			week.ByVariable[i].Name = v.Name
		}
	}
	for _, d := range dates {
		sd := si.day(d)
		days = append(days, sd)
		if sd.Session {
			sessions++
		}
		if week != nil {
			week.Sets += sd.Sets
			week.Reps += sd.Reps
			for i := range sd.ByVar {
				week.ByVariable[i].Sets += sd.ByVar[i].Sets
				week.ByVariable[i].Reps += sd.ByVar[i].Reps
			}
		}
	}
	return sessions, week, days
}

func (si strengthInput) rule() string {
	if si.cfg == nil {
		return "legacy"
	}
	return "sets"
}

func (si strengthInput) missingNames() []string {
	if si.missing == nil {
		return []string{}
	}
	return si.missing
}

// resolveStrengthVars maps the configured names to ids with the kept
// variable list: a name resolves when exactly one variable has that exact
// name and its type is numeric.
func resolveStrengthVars(cfg *StrengthCfg, list []VarInfo) (vars []strengthVar, missing []string) {
	if cfg == nil {
		return nil, nil
	}
	for _, name := range cfg.SetVariables {
		var hits []VarInfo
		for _, v := range list {
			if v.Name == name {
				hits = append(hits, v)
			}
		}
		if len(hits) == 1 && hits[0].Type == "numeric" {
			vars = append(vars, strengthVar{Name: name, ID: hits[0].ID})
		} else {
			missing = append(missing, name)
		}
	}
	return vars, missing
}
