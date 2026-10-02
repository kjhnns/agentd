package fuel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// T18 (cross-writer): one staples fixture, read by ~/clawd/scripts/food-log
// (tests/food-log, which also checks that the two copies of the fixture are
// equal) and by fueld. The rows in the fixture are the rows food-log writes.
func TestT18CrossWriterFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "t18_cross_writer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Date    string           `json:"date"`
		Staples json.RawMessage  `json:"staples"`
		Foods   []map[string]any `json:"foods"`
		Rows    []map[string]any `json:"rows"`
		Fix     struct {
			Row        int            `json:"row"`
			Correction map[string]any `json:"correction"`
		} `json:"fix"`
		Expect struct {
			Sums      map[string]float64 `json:"sums"`
			Walnuts   map[string]float64 `json:"walnuts"`
			DayLevers map[string]struct {
				Consumed float64 `json:"consumed"`
				Untagged int     `json:"untagged_rows"`
			} `json:"day_levers"`
		} `json:"expect_after_fix"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	staples, err := ParseStaples(fx.Staples)
	if err != nil {
		t.Fatalf("the fixture staples must parse in fueld: %v", err)
	}
	// The same staple gives the same row values in both writers.
	for i, st := range staples {
		row := fx.Rows[i]
		g := row["portion_g"].(float64)
		m := st.macros(g)
		for _, k := range []string{"kcal", "protein_g", "carbs_g", "net_carbs_g", "fat_g", "sat_fat_g", "fiber_g"} {
			if got := m.Get(k); !got.OK || got.float() != row[k].(float64) {
				t.Errorf("%s %s: fueld %v, food-log %v", st.Key, k, got.float(), row[k])
			}
		}
		for l, p := range st.leverAmounts(g) {
			want, has := row[leverKeys[l]]
			if (p != nil) != has || (p != nil && round1(*p) != want.(float64)) {
				t.Errorf("%s %s: fueld %v, food-log %v", st.Key, leverKeys[l], p, want)
			}
		}
	}
	// fueld reads the food-log rows (and the food-log fix of the tagged item)
	// to the same day sums and lever amounts.
	h := newV7(t, v2())
	var ids []string
	for i, row := range fx.Rows {
		d := map[string]any{"source": "agentd", "op_id": "op_t18_" + string(rune('0'+i)), "eaten_at": fx.Date + "T12:00:00+02:00"}
		for k, v := range row {
			d[k] = v
		}
		h.addAt("var-food", fx.Date, d)
		rows := h.vars.rows("var-food")
		ids = append(ids, rows[len(rows)-1]["_id"].(string))
	}
	corr := map[string]any{"corrects": ids[fx.Fix.Row], "op_id": "op_fix_" + ids[fx.Fix.Row] + "_1", "eaten_at": fx.Date + "T12:05:00+02:00"}
	for k, v := range fx.Fix.Correction {
		corr[k] = v
	}
	h.clk.Add(1e9)
	h.addAt("var-food", fx.Date, corr)
	h.restart()
	s := h.snap(t, "?date="+fx.Date)
	for k, want := range fx.Expect.Sums {
		got := 0.0
		if k == "carbs_g" {
			got = budget(s, k).Consumed
		} else if k == "fat_g" {
			continue // no snapshot entry carries total fat
		} else {
			got = macro(s, k).Consumed
		}
		if got != want {
			t.Errorf("day sum %s: fueld %g, food-log %g", k, got, want)
		}
	}
	for k, want := range fx.Expect.DayLevers {
		if l := lever(s, k); l.Consumed != want.Consumed || l.UntaggedRows != want.Untagged {
			t.Errorf("day lever %s: %+v, want %+v", k, l, want)
		}
	}
	for _, d := range h.day("?date=" + fx.Date).Items {
		if d["item"] != "walnuts" {
			continue
		}
		lv := d["levers"].(map[string]any)
		if d["portion_g"] != fx.Expect.Walnuts["portion_g"] || lv["nuts_g"] != fx.Expect.Walnuts["nuts_g"] || lv["plant_protein_g"] != fx.Expect.Walnuts["plant_protein_g"] ||
			d["macros"].(map[string]any)["kcal"] != fx.Expect.Walnuts["kcal"] {
			t.Errorf("walnuts in /fuel/day: %v", d)
		}
	}
	// A food-log energy fix (reason revise, source agentd) is the item's base
	// in fueld too: a later fix by portion scales the corrected values.
	h2 := newV7(t, v2())
	h2.addAt("var-food", "2026-10-01", map[string]any{"item": "green salad leaves", "kind": "food", "portion_g": 80.0, "portion_basis": "stated",
		"kcal": 90.0, "protein_g": 1.8, "carbs_g": 4.5, "net_carbs_g": 2.3, "fat_g": 7.2, "sat_fat_g": 0.9, "fiber_g": 2.2,
		"source": "agentd", "op_id": "op_salad", "eaten_at": "2026-10-01T11:00:00+02:00"})
	rows := h2.vars.rows("var-food")
	vid := rows[0]["_id"].(string)
	h2.clk.Add(1e9)
	h2.addAt("var-food", "2026-10-01", map[string]any{"item": "correction: green salad leaves", "source": "agentd", "reason": "revise", "corrects": vid,
		"kind": "food", "share_after": 1.0, "portion_g_after": 80.0, "op_id": "op_rev_" + vid + "_1",
		"kcal": -74.0, "protein_g": -1.5, "carbs_g": -3.7, "net_carbs_g": -1.9, "fat_g": -5.9, "sat_fat_g": -0.7, "fiber_g": -1.8,
		"recalibrated": map[string]any{"from": map[string]any{"kcal": 90, "protein_g": 1.8, "carbs_g": 4.5, "net_carbs_g": 2.3, "fat_g": 7.2, "sat_fat_g": 0.9, "fiber_g": 2.2, "portion_g": 80},
			"to":     map[string]any{"kcal": 16, "protein_g": 0.3, "carbs_g": 0.8, "net_carbs_g": 0.4, "fat_g": 1.3, "sat_fat_g": 0.2, "fiber_g": 0.4, "portion_g": 80},
			"reason": "energy corrected, portion kept", "by": "food-log"}})
	h2.restart()
	var salad map[string]any
	for _, d := range h2.day("").Items {
		salad = d
	}
	if salad == nil || salad["portion_g"] != 80.0 || salad["macros"].(map[string]any)["kcal"] != 16.0 {
		t.Fatalf("after the food-log energy fix: %v", salad)
	}
	h2.clk.Add(1e9)
	fx2 := decode[MutationResponse](t, h2.fix(cid(), "v:"+vid, map[string]any{"portion_g": 40}))
	if fx2.Item.Effective.Kcal.float() != 8 {
		t.Errorf("a portion fix after the energy fix: %g kcal, want 8 (half of the corrected 16)", fx2.Item.Effective.Kcal.float())
	}
}
