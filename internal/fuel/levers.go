package fuel

import (
	"encoding/json"
	"log"
)

// Levers (spec 18.6): optional amounts of an item that the report names as
// levers. They are H values with optional reference doses: no status, no
// streak, no place in any score.

var leverKeys = []string{"psyllium_g", "beta_glucan_g", "nuts_g", "pulses_g", "plant_protein_g"}

var leverLabels = []string{"Psyllium", "Beta-glucan", "Nuts", "Pulses", "Plant protein"}

// leverAnchors are the 18.8 anchors, by lever.
var leverAnchors = []string{"viscous-fibre", "viscous-fibre", "nuts", "plant-protein", "plant-protein"}

// leverVals are the five amounts in leverKeys order; OK false = not known
// (a row without the key, an item that is untagged for the lever).
type leverVals [5]tenth

// Levers is the wire object of an item's levers (ItemState, /fuel/day) and
// the model's `levers` object: number or null each, and the brew method.
type Levers struct {
	Psyllium     *float64 `json:"psyllium_g"`
	BetaGlucan   *float64 `json:"beta_glucan_g"`
	Nuts         *float64 `json:"nuts_g"`
	Pulses       *float64 `json:"pulses_g"`
	PlantProtein *float64 `json:"plant_protein_g"`
	BrewMethod   *string  `json:"brew_method"`
}

func (l *Levers) ptrs() [5]**float64 {
	return [5]**float64{&l.Psyllium, &l.BetaGlucan, &l.Nuts, &l.Pulses, &l.PlantProtein}
}

// vals converts the wire object to tenths (nil receiver = nothing known).
func (l *Levers) vals() leverVals {
	var v leverVals
	if l == nil {
		return v
	}
	for i, p := range l.ptrs() {
		v[i] = tenthFromPtr(*p)
	}
	return v
}

func (l *Levers) brew() string {
	if l == nil || l.BrewMethod == nil {
		return ""
	}
	return *l.BrewMethod
}

// wireLevers renders amounts and a brew method ("" = null).
func wireLevers(v leverVals, brew string) Levers {
	var l Levers
	for i, p := range l.ptrs() {
		*p = v[i].ptr()
	}
	if brew != "" {
		l.BrewMethod = &brew
	}
	return l
}

func numFrom(x any) (float64, bool) {
	switch v := x.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// leversFromData reads the flat lever keys of a row.
func leversFromData(d map[string]any) leverVals {
	var v leverVals
	for i, k := range leverKeys {
		if f, ok := numFrom(d[k]); ok {
			v[i] = known(toTenth(f))
		}
	}
	return v
}

// putInto writes the known amounts as flat keys (a key is never written as
// null, spec 18.6).
func (v leverVals) putInto(d map[string]any) {
	for i, k := range leverKeys {
		if v[i].OK {
			d[k] = v[i].float()
		}
	}
}

func (v leverVals) scale(f float64) leverVals {
	for i := range v {
		v[i] = v[i].scale(f)
	}
	return v
}

func (v leverVals) any() bool {
	for _, x := range v {
		if x.OK {
			return true
		}
	}
	return false
}

// asMap renders the known amounts for `recalibrated.from/.to.levers`.
func (v leverVals) asMap() map[string]float64 {
	if !v.any() {
		return nil
	}
	m := map[string]float64{}
	for i, k := range leverKeys {
		if v[i].OK {
			m[k] = v[i].float()
		}
	}
	return m
}

func leverValsFromMap(m map[string]float64) leverVals {
	var v leverVals
	for i, k := range leverKeys {
		if f, ok := m[k]; ok {
			v[i] = known(toTenth(f))
		}
	}
	return v
}

// leverState is the reduced lever state of one item (contribution group).
type leverState struct {
	amount leverVals // OK = the item is TAGGED for the lever
	base   leverVals // what later shares scale (OK = known)
	brew   string    // "" = no brew method
}

func isCorrection(r Value) bool {
	c, _ := r.Data["corrects"].(string)
	return c != ""
}

// reduceLevers applies the reducer rule of spec 18.6 to a group. Rows are in
// write order. For a lever L the item is tagged when at least one row carries
// L and every correction row after the FIRST row with L also carries L; its
// amount is the sum, never below 0. A revise row whose recalibrated.to.levers
// holds L resets: the newest such row is the start (its `to` value plus the
// rows after it). So a correction by a writer without lever arithmetic (a v6
// binary, an old food-log) makes the item untagged, never wrong.
func reduceLevers(g *group) leverState {
	var st leverState
	if g == nil {
		return st
	}
	type rowL struct {
		vals   leverVals
		corr   bool
		revise leverVals // to.levers of a revise row
		isRev  bool
	}
	rows := make([]rowL, len(g.rows))
	for i, r := range g.rows {
		rows[i] = rowL{vals: leversFromData(r.Data), corr: isCorrection(r)}
		if reason, _ := r.Data["reason"].(string); reason == "revise" {
			if m, ok := recalOfData(r.Data); ok {
				rows[i].isRev = true
				rows[i].revise = leverValsFromMap(m.To.Levers)
			}
		}
		if b, _ := r.Data["brew_method"].(string); b != "" {
			st.brew = b // the newest row that carries the key wins
		}
	}
	for l := range leverKeys {
		start := -1
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].isRev && rows[i].revise[l].OK {
				start = i
				break
			}
		}
		var sum int64
		tagged := false
		from := 0
		if start >= 0 {
			sum, tagged, from = rows[start].revise[l].V, true, start+1
		}
		for i := from; i < len(rows); i++ {
			r := rows[i]
			switch {
			case r.vals[l].OK:
				sum += r.vals[l].V
				tagged = true
			case tagged && r.corr:
				// A correction without the key after the lever was known.
				tagged = false
				sum = 0
				// Nothing later can tag it again but a revise (handled by
				// `start`), so stop here.
				i = len(rows)
			}
		}
		if tagged {
			if sum < 0 {
				sum = 0
			}
			st.amount[l] = known(sum)
		}
	}
	// Base: recalibrated.to.levers of the newest recalibrate or revise row
	// that no revert names, else the original row's keys.
	if g.orig.Data != nil {
		st.base = leversFromData(g.orig.Data)
	}
	if m, ok := baseRow(g); ok {
		to := leverValsFromMap(m.To.Levers)
		for l := range leverKeys {
			if to[l].OK {
				st.base[l] = to[l]
			}
		}
	}
	return st
}

// leverScaleDeltas are the lever keys of a size correction (fix, fraction,
// portion or volume delta, count delta): for each lever the item is tagged
// for, base x share minus the current amount (a known 0 when unchanged); no
// key for an untagged lever.
func leverScaleDeltas(st leverState, share float64) leverVals {
	var d leverVals
	for l := range leverKeys {
		if !st.amount[l].OK {
			continue
		}
		d[l] = known(0)
		if st.base[l].OK {
			d[l] = known(st.base[l].scale(share).V - st.amount[l].V)
		}
	}
	return d
}

// leverCancel brings every tagged lever to 0 (undo, compensation, the undo
// half of a move).
func leverCancel(st leverState) leverVals {
	var d leverVals
	for l := range leverKeys {
		if st.amount[l].OK {
			d[l] = known(-st.amount[l].V)
		}
	}
	return d
}

// leverKeep is a known 0 for every tagged lever (the row changes no lever
// but keeps the tags).
func leverKeep(st leverState) leverVals {
	var d leverVals
	for l := range leverKeys {
		if st.amount[l].OK {
			d[l] = known(0)
		}
	}
	return d
}

// checkLevers applies the code checks of spec 18.6 to a new item: a value
// that cannot be true is set to null (and logged with the item id).
func checkLevers(itemID string, v leverVals, brew string, kind string, portion *float64, m Macros, est *EstimatorCfg) (leverVals, string) {
	drop := func(l int, why string) {
		if v[l].OK {
			log.Printf("fuel: item %s: %s dropped (%s)", itemID, leverKeys[l], why)
			v[l] = tenth{}
		}
	}
	for l := range v {
		if v[l].OK && v[l].V < 0 {
			drop(l, "negative")
		}
	}
	if portion != nil && *portion > 0 {
		max := toTenth(*portion * 1.05)
		for _, l := range []int{0, 2} { // psyllium, nuts
			if v[l].OK && v[l].V > max {
				drop(l, "more than the portion")
			}
		}
		pmax := *portion * 1.05
		if est != nil && est.PulsesDryToCooked*1.1 > 1.05 {
			pmax = *portion * est.PulsesDryToCooked * 1.1
		}
		if v[3].OK && v[3].V > toTenth(pmax) {
			drop(3, "more than the portion allows")
		}
	}
	if m.Fiber.OK && v[1].OK && v[1].V > m.Fiber.V+5 {
		drop(1, "more than the fibre")
	}
	if m.Protein.OK && v[4].OK && v[4].V > m.Protein.V+5 {
		drop(4, "more than the protein")
	}
	if brew != "" && (kind != "drink" || !isBrewMethod(brew)) {
		log.Printf("fuel: item %s: brew_method dropped (not a drink, or unknown method)", itemID)
		brew = ""
	}
	return v, brew
}

// ---- day state ----

// dayLeverState is the lever state of one day (spec 18.6 "Day state").
type dayLeverState struct {
	consumed [5]int64 // tenths, over the active tagged items
	untagged [5]int   // active items that are untagged for the lever
	active   int
	coffee   map[string]int // brew method -> active drink items
}

func (d dayLeverState) covered(l int) bool { return d.active > 0 && d.untagged[l] == 0 }

// dayLevers reduces one day's rows.
func dayLevers(rows []Value) dayLeverState {
	st := dayLeverState{coffee: map[string]int{}}
	for _, g := range groupRows(rows) {
		if g.orig.Data == nil || g.c.undone {
			continue
		}
		st.active++
		ls := reduceLevers(g)
		for l := range leverKeys {
			if ls.amount[l].OK {
				st.consumed[l] += ls.amount[l].V
			} else {
				st.untagged[l]++
			}
		}
		if kind, _ := g.orig.Data["kind"].(string); kind == "drink" && ls.brew != "" && isBrewMethod(ls.brew) {
			st.coffee[ls.brew]++
		}
	}
	return st
}
