package fuel

import (
	"encoding/json"
	"math"
)

// Macro values are kept as integer TENTHS internally (g and kcal both round to
// 1 decimal, spec section 7), so sums, negations and "halve then undo" are
// exact: 25.3 - 12.7 - 12.6 is 0 in tenths, not 1e-15 in floats.
type tenth struct {
	V  int64
	OK bool // false = null (unknown)
}

func known(v int64) tenth { return tenth{V: v, OK: true} }

// toTenth rounds a float to tenths, half away from zero.
func toTenth(f float64) int64 {
	if f >= 0 {
		return int64(math.Floor(f*10 + 0.5 + 1e-9))
	}
	return -int64(math.Floor(-f*10 + 0.5 + 1e-9))
}

func tenthFromPtr(p *float64) tenth {
	if p == nil {
		return tenth{}
	}
	return known(toTenth(*p))
}

func (t tenth) float() float64 { return float64(t.V) / 10 }

// ptr returns the value as *float64 (nil for null).
func (t tenth) ptr() *float64 {
	if !t.OK {
		return nil
	}
	f := t.float()
	return &f
}

func (t tenth) neg() tenth { return tenth{V: -t.V, OK: t.OK} }

func (t tenth) add(o tenth) tenth {
	if !t.OK || !o.OK {
		return tenth{}
	}
	return known(t.V + o.V)
}

// scale multiplies by f and rounds once (half away from zero); null stays null.
func (t tenth) scale(f float64) tenth {
	if !t.OK {
		return t
	}
	x := float64(t.V) * f
	if x >= 0 {
		return known(int64(math.Floor(x + 0.5 + 1e-9)))
	}
	return known(-int64(math.Floor(-x + 0.5 + 1e-9)))
}

// MarshalJSON renders the value as a number with at most 1 decimal, or null.
func (t tenth) MarshalJSON() ([]byte, error) {
	if !t.OK {
		return []byte("null"), nil
	}
	return json.Marshal(t.float())
}

func (t *tenth) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = tenth{}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*t = known(toTenth(f))
	return nil
}

// MacroKeys is the fixed order of the macro fields a row carries.
var MacroKeys = []string{"kcal", "protein_g", "carbs_g", "net_carbs_g", "fat_g", "sat_fat_g", "fiber_g"}

// IntakeKeys are the additive non-macro amounts a row may carry (drinks and
// supplements, spec section 15): omitted when unknown, negated in
// corrections like macros.
var IntakeKeys = []string{"volume_ml", "caffeine_mg", "alcohol_g"}

// AllKeys is MacroKeys then IntakeKeys, the order of Macros.fields().
var AllKeys = append(append([]string{}, MacroKeys...), IntakeKeys...)

// Macros is one row's (or one contribution's) macro set.
type Macros struct {
	Kcal     tenth `json:"kcal"`
	Protein  tenth `json:"protein_g"`
	Carbs    tenth `json:"carbs_g"`
	NetCarbs tenth `json:"net_carbs_g"`
	Fat      tenth `json:"fat_g"`
	SatFat   tenth `json:"sat_fat_g"`
	Fiber    tenth `json:"fiber_g"`
	// Intake amounts (drinks, supplements); null when unknown.
	VolumeML   tenth `json:"volume_ml"`
	CaffeineMG tenth `json:"caffeine_mg"`
	AlcoholG   tenth `json:"alcohol_g"`
}

func (m *Macros) fields() []*tenth {
	return []*tenth{&m.Kcal, &m.Protein, &m.Carbs, &m.NetCarbs, &m.Fat, &m.SatFat, &m.Fiber, &m.VolumeML, &m.CaffeineMG, &m.AlcoholG}
}

// Get returns one macro by key.
func (m Macros) Get(key string) tenth {
	switch key {
	case "kcal":
		return m.Kcal
	case "protein_g":
		return m.Protein
	case "carbs_g":
		return m.Carbs
	case "net_carbs_g":
		return m.NetCarbs
	case "fat_g":
		return m.Fat
	case "sat_fat_g":
		return m.SatFat
	case "fiber_g":
		return m.Fiber
	case "volume_ml":
		return m.VolumeML
	case "caffeine_mg":
		return m.CaffeineMG
	case "alcohol_g":
		return m.AlcoholG
	}
	return tenth{}
}

func (m Macros) map2(o Macros, fn func(a, b tenth) tenth) Macros {
	out := Macros{}
	af, bf, of := m.fields(), o.fields(), out.fields()
	for i := range af {
		*of[i] = fn(*af[i], *bf[i])
	}
	return out
}

func (m Macros) map1(fn func(a tenth) tenth) Macros {
	out := Macros{}
	af, of := m.fields(), out.fields()
	for i := range af {
		*of[i] = fn(*af[i])
	}
	return out
}

// Add sums two macro sets; null in either stays null.
func (m Macros) Add(o Macros) Macros { return m.map2(o, func(a, b tenth) tenth { return a.add(b) }) }

// Neg negates every field; null stays null.
func (m Macros) Neg() Macros { return m.map1(func(a tenth) tenth { return a.neg() }) }

// Scale multiplies every field by f (rounded once); null stays null.
func (m Macros) Scale(f float64) Macros { return m.map1(func(a tenth) tenth { return a.scale(f) }) }

// zeroMacros is a known zero on every field (an undone item's contribution).
func zeroMacros() Macros { return Macros{}.map1(func(tenth) tenth { return known(0) }) }

// normalizeNetCarbs fills net_carbs_g on an ORIGINAL row (spec section 7):
// explicit, else carbs - fiber, else carbs. Never below 0.
func (m *Macros) normalizeNetCarbs() {
	if m.NetCarbs.OK {
		return
	}
	switch {
	case m.Carbs.OK && m.Fiber.OK:
		v := m.Carbs.V - m.Fiber.V
		if v < 0 {
			v = 0
		}
		m.NetCarbs = known(v)
	case m.Carbs.OK:
		m.NetCarbs = m.Carbs
	}
}

// rowData renders the macro fields of a row for Variables. The production
// Food log schema types net_carbs_g (and portion_g) as number only, so a null
// value there is OMITTED; only fiber_g may be written as null.
func (m Macros) putInto(d map[string]any) {
	for _, k := range AllKeys {
		v := m.Get(k)
		if !v.OK {
			if k == "fiber_g" {
				d[k] = nil
			}
			continue
		}
		d[k] = v.float()
	}
}

// macrosFromData reads the macro fields of a stored row. A missing key or a
// non-number is null.
func macrosFromData(d map[string]any) Macros {
	var m Macros
	get := func(k string) tenth {
		switch v := d[k].(type) {
		case float64:
			return known(toTenth(v))
		case json.Number:
			f, err := v.Float64()
			if err == nil {
				return known(toTenth(f))
			}
		}
		return tenth{}
	}
	m.Kcal, m.Protein, m.Carbs, m.NetCarbs = get("kcal"), get("protein_g"), get("carbs_g"), get("net_carbs_g")
	m.Fat, m.SatFat, m.Fiber = get("fat_g"), get("sat_fat_g"), get("fiber_g")
	m.VolumeML, m.CaffeineMG, m.AlcoholG = get("volume_ml"), get("caffeine_mg"), get("alcohol_g")
	return m
}
