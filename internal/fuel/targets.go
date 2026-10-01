package fuel

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"time"
)

// Target is one macro's target: a kind and either one value or a rest and a
// training value. A nil value means "no target" (the check is skipped).
type Target struct {
	Kind     string   `json:"kind"` // floor | cap | pace
	Value    *float64 `json:"value,omitempty"`
	Rest     *float64 `json:"rest,omitempty"`
	Training *float64 `json:"training,omitempty"`
	hasValue bool
	hasSplit bool
}

func (t *Target) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for k := range raw {
		switch k {
		case "kind", "value", "rest", "training":
		default:
			return fmt.Errorf("unknown key %q", k)
		}
	}
	type plain Target
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*t = Target(p)
	_, t.hasValue = raw["value"]
	_, hasRest := raw["rest"]
	_, hasTrain := raw["training"]
	t.hasSplit = hasRest && hasTrain
	if hasRest != hasTrain {
		return errors.New("rest and training come together")
	}
	return nil
}

// For returns the target for a day type (unknown uses rest, spec section 9).
func (t Target) For(dayType string) *float64 {
	if t.hasValue {
		return t.Value
	}
	if dayType == "training" {
		return t.Training
	}
	return t.Rest
}

// Targets is fuel-targets.json (spec section 14 [C17]).
type Targets struct {
	Protein  Target `json:"protein_g"`
	SatFat   Target `json:"sat_fat_g"`
	Fiber    Target `json:"fiber_g"`
	Kcal     Target `json:"kcal"`
	NetCarbs Target `json:"net_carbs_g"`
	// Optional (spec section 15); nil when absent, then the check is skipped.
	WaterML         *Target   `json:"water_ml,omitempty"`
	CaffeineMG      *Target   `json:"caffeine_mg,omitempty"`
	AlcoholGWeek    *Target   `json:"alcohol_g_week,omitempty"`
	StrengthPerWeek int       `json:"strength_per_week"`
	WeightBandKg    []float64 `json:"weight_band_kg"`
	EatingWindow    struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"eating_window"`
	TZ string `json:"tz"`

	loc        *time.Location
	startMin   int
	endMin     int
	isDefaults bool
}

// DefaultTargetsJSON is the built-in example (health-driver-tree-plan-2026-10:
// protein floor 160 g at about 80 kg, sat fat cap 20 g, fibre floor 35 g,
// energy pace, net carbs periodized with no fixed target on training days).
const DefaultTargetsJSON = `{
  "protein_g":   {"kind": "floor", "value": 160},
  "sat_fat_g":   {"kind": "cap",   "value": 20},
  "fiber_g":     {"kind": "floor", "value": 35},
  "kcal":        {"kind": "pace",  "rest": 2300, "training": 2600},
  "net_carbs_g": {"kind": "pace",  "rest": 120,  "training": null},
  "water_ml":    {"kind": "floor", "rest": 2500, "training": 3000},
  "caffeine_mg": {"kind": "cap",   "value": 400},
  "alcohol_g_week": {"kind": "cap", "value": 30},
  "strength_per_week": 3,
  "weight_band_kg": [78, 82],
  "eating_window": {"start": "07:00", "end": "20:30"},
  "tz": "Europe/Zurich"
}`

var requiredTargetKeys = []string{"protein_g", "sat_fat_g", "fiber_g", "kcal", "net_carbs_g", "strength_per_week", "weight_band_kg", "eating_window", "tz"}

// optionalTargetKeys may be absent (files from before section 15 stay valid).
var optionalTargetKeys = []string{"water_ml", "caffeine_mg", "alcohol_g_week"}

// ParseTargets validates a targets document: every key required, kinds valid.
func ParseTargets(b []byte) (*Targets, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("targets: %w", err)
	}
	for _, k := range requiredTargetKeys {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("targets: missing key %q", k)
		}
	}
	for k := range raw {
		found := false
		for _, r := range append(append([]string{}, requiredTargetKeys...), optionalTargetKeys...) {
			found = found || r == k
		}
		if !found {
			return nil, fmt.Errorf("targets: unknown key %q", k)
		}
	}
	var t Targets
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("targets: %w", err)
	}
	all := map[string]Target{"protein_g": t.Protein, "sat_fat_g": t.SatFat, "fiber_g": t.Fiber, "kcal": t.Kcal, "net_carbs_g": t.NetCarbs}
	for name, p := range map[string]*Target{"water_ml": t.WaterML, "caffeine_mg": t.CaffeineMG, "alcohol_g_week": t.AlcoholGWeek} {
		if p != nil {
			all[name] = *p
		}
	}
	for name, tg := range all {
		switch tg.Kind {
		case "floor", "cap", "pace":
		default:
			return nil, fmt.Errorf("targets: %s kind %q (floor|cap|pace)", name, tg.Kind)
		}
		if tg.hasValue == tg.hasSplit {
			return nil, fmt.Errorf("targets: %s needs value, or rest and training", name)
		}
		for _, v := range []*float64{tg.Value, tg.Rest, tg.Training} {
			if v != nil && *v < 0 {
				return nil, fmt.Errorf("targets: %s is negative", name)
			}
		}
	}
	if t.StrengthPerWeek < 0 || t.StrengthPerWeek > 14 {
		return nil, errors.New("targets: strength_per_week out of range")
	}
	if len(t.WeightBandKg) != 2 || t.WeightBandKg[0] > t.WeightBandKg[1] {
		return nil, errors.New("targets: weight_band_kg must be [low, high]")
	}
	loc, err := time.LoadLocation(t.TZ)
	if err != nil || t.TZ == "" {
		return nil, fmt.Errorf("targets: tz %q: %v", t.TZ, err)
	}
	t.loc = loc
	if t.startMin, err = parseHHMM(t.EatingWindow.Start); err != nil {
		return nil, fmt.Errorf("targets: eating_window.start: %w", err)
	}
	if t.endMin, err = parseHHMM(t.EatingWindow.End); err != nil {
		return nil, fmt.Errorf("targets: eating_window.end: %w", err)
	}
	if t.endMin <= t.startMin {
		return nil, errors.New("targets: eating_window end must be after start")
	}
	return &t, nil
}

var hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

func parseHHMM(s string) (int, error) {
	m := hhmmRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return int(m[1][0]-'0')*600 + int(m[1][1]-'0')*60 + int(m[2][0]-'0')*10 + int(m[2][1]-'0'), nil
}

// LoadTargets reads the targets file. Missing: the built-in defaults (the
// caller logs once). Invalid: an error, and the routes answer 503.
func LoadTargets(path string) (*Targets, bool, error) {
	b, err := os.ReadFile(expandHome(path))
	if errors.Is(err, os.ErrNotExist) {
		t, perr := ParseTargets([]byte(DefaultTargetsJSON))
		if perr != nil {
			panic(perr) // the built-in document is a constant
		}
		t.isDefaults = true
		return t, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	t, err := ParseTargets(b)
	return t, false, err
}

// Staple is one recurring food with label macros (spec section 8, [C17]).
type Staple struct {
	Key      string   `json:"key"`
	Aliases  []string `json:"aliases"`
	Per100g  Per100g  `json:"per_100g"`
	DefaultG float64  `json:"default_g"`
}

// Per100g are the label macros of a staple per 100 g. fiber_g may be null.
type Per100g struct {
	Kcal    *float64 `json:"kcal"`
	Protein *float64 `json:"protein_g"`
	Carbs   *float64 `json:"carbs_g"`
	Fat     *float64 `json:"fat_g"`
	SatFat  *float64 `json:"sat_fat_g"`
	Fiber   *float64 `json:"fiber_g"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// ParseStaples validates a staples document.
func ParseStaples(b []byte) ([]Staple, error) {
	var st []Staple
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("staples: %w", err)
	}
	seen := map[string]bool{}
	for i, s := range st {
		if !slugRe.MatchString(s.Key) {
			return nil, fmt.Errorf("staples[%d]: key %q is not a slug", i, s.Key)
		}
		if seen[s.Key] {
			return nil, fmt.Errorf("staples: duplicate key %q", s.Key)
		}
		seen[s.Key] = true
		if len(s.Aliases) == 0 {
			return nil, fmt.Errorf("staples[%s]: needs at least one alias", s.Key)
		}
		p := s.Per100g
		for name, v := range map[string]*float64{"kcal": p.Kcal, "protein_g": p.Protein, "carbs_g": p.Carbs, "fat_g": p.Fat, "sat_fat_g": p.SatFat} {
			if v == nil || *v < 0 {
				return nil, fmt.Errorf("staples[%s]: per_100g.%s must be a number >= 0", s.Key, name)
			}
		}
		if p.Fiber != nil && *p.Fiber < 0 {
			return nil, fmt.Errorf("staples[%s]: per_100g.fiber_g negative", s.Key)
		}
		if s.DefaultG <= 0 || s.DefaultG > 3000 {
			return nil, fmt.Errorf("staples[%s]: default_g must be in (0, 3000]", s.Key)
		}
	}
	return st, nil
}

// LoadStaples reads the staples file. Missing: none. Invalid: none plus the
// error (the caller logs it; staples are then ignored).
func LoadStaples(path string) ([]Staple, error) {
	b, err := os.ReadFile(expandHome(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseStaples(b)
}

// macros scales a staple's label values to grams.
func (s Staple) macros(grams float64) Macros {
	f := grams / 100
	sc := func(p *float64) tenth {
		if p == nil {
			return tenth{}
		}
		return known(toTenth(*p * f))
	}
	m := Macros{Kcal: sc(s.Per100g.Kcal), Protein: sc(s.Per100g.Protein), Carbs: sc(s.Per100g.Carbs),
		Fat: sc(s.Per100g.Fat), SatFat: sc(s.Per100g.SatFat), Fiber: sc(s.Per100g.Fiber)}
	// Net carbs from the unrounded scaled values, rounded once.
	if s.Per100g.Carbs != nil {
		net := *s.Per100g.Carbs * f
		if s.Per100g.Fiber != nil {
			net = math.Max(0, net-*s.Per100g.Fiber*f)
		}
		m.NetCarbs = known(toTenth(net))
	}
	return m
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}
