package fuel

// T2 (spec 18.11): the v6 golden files. They were written by the v6 code
// (commit f4bb7e2) with FUEL_GOLDEN_UPDATE=1 and are never rewritten by a
// later version. Every later version must return every v6 key with the same
// value for the same fixture and clock; only NEW keys may differ.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// goldenTargetsV1 is the production schema 1 targets file of 2026-10-02.
const goldenTargetsV1 = `{
 "protein_g": {"kind": "floor", "value": 160},
 "sat_fat_g": {"kind": "cap", "value": 20},
 "fiber_g": {"kind": "floor", "value": 35},
 "kcal": {"kind": "pace", "rest": 2600, "training": 3000},
 "net_carbs_g": {"kind": "pace", "rest": 120, "training": null},
 "strength_per_week": 3,
 "weight_band_kg": [79, 81],
 "eating_window": {"start": "07:00", "end": "20:30"},
 "tz": "Europe/Zurich",
 "water_ml": {"kind": "floor", "rest": 2500, "training": 3000},
 "caffeine_mg": {"kind": "cap", "value": 400},
 "alcohol_g_week": {"kind": "cap", "value": 30}
}`

var goldenIDRe = regexp.MustCompile(`(it_|en_|op_)[0-9a-f]{20}|val-\d+`)

// goldenNorm replaces random ids and value ids by placeholders in the order
// of their first appearance, and zeroes the latency values.
type goldenNorm struct{ seen map[string]string }

func (g *goldenNorm) norm(b []byte) []byte {
	out := goldenIDRe.ReplaceAllFunc(b, func(m []byte) []byte {
		k := string(m)
		if v, ok := g.seen[k]; ok {
			return []byte(v)
		}
		prefix := "val"
		if !strings.HasPrefix(k, "val-") {
			prefix = k[:2]
		}
		v := fmt.Sprintf("%s#%d", prefix, len(g.seen)+1)
		g.seen[k] = v
		return []byte(v)
	})
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		return out
	}
	zeroLatency(v)
	nb, _ := json.MarshalIndent(v, "", " ")
	return append(nb, '\n')
}

func zeroLatency(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if k == "latency_ms" {
				if m, ok := c.(map[string]any); ok {
					for lk := range m {
						m[lk] = 0.0
					}
				}
				continue
			}
			zeroLatency(c)
		}
	case []any:
		for _, c := range x {
			zeroLatency(c)
		}
	}
}

// goldenSubset reports where want (a v6 golden document) is not contained in
// got: every key of an object in want must be in got with an equal value;
// arrays must have the same length. Extra keys in got are allowed.
func goldenSubset(path string, want, got any, diffs *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s: want an object, got %T", path, got))
			return
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, has := g[k]
			if !has {
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: the v6 key is missing", path, k))
				continue
			}
			goldenSubset(path+"."+k, w[k], gv, diffs)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			*diffs = append(*diffs, fmt.Sprintf("%s: want an array of %d, got %v", path, len(w), got))
			return
		}
		for i := range w {
			goldenSubset(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], diffs)
		}
	default:
		if !reflect.DeepEqual(want, got) {
			*diffs = append(*diffs, fmt.Sprintf("%s: v6 has %v, got %v", path, want, got))
		}
	}
}

// goldenFixture builds the fixed state and returns the documents in a fixed
// order: name -> raw response body.
func goldenFixture(t *testing.T, targets string) (*harness, []string, map[string][]byte) {
	t.Helper()
	h := newHarness(t, func(o *Options) {
		if err := os.WriteFile(o.TargetsFile, []byte(targets), 0o600); err != nil {
			t.Fatal(err)
		}
		// A run of 50 min and 10 km at 08:00 local on 2026-10-01.
		if err := os.MkdirAll(o.StravaDir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(o.StravaDir, "2026-10-01-run.md")
		body := "---\ntitle: run\n---\n```json\n{\"sport_type\":\"Run\",\"moving_time\":3000,\"distance\":10000,\"start_date_local\":\"2026-10-01T08:00:00Z\"}\n```\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	})
	// Body composition: five weigh-ins and one body-fat value.
	for i, kg := range []float64{80.4, 80.1, 80.3, 79.9, 80.0} {
		d := dateAdd("2026-10-01", -i)
		row := map[string]any{"weight_kg": kg, "method": "withings_scale", "measured_at": d + "T06:30:00+02:00"}
		if i == 1 {
			row["method"], row["fat_pct"] = "withings_bia", 17.5
		}
		h.vars.add("var-body", d, row)
	}
	h.vars.add("var-push", "2026-10-01", 20)
	// Rows of the Telegram writer: a coffee today (fixed from 300 to 200 ml)
	// and a meal yesterday.
	coffee := agentdRow(h, "coffee", 300, 6, "2026-10-01T08:30:00+02:00", map[string]any{"caffeine_mg": 120.0})
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "correction: coffee", "source": "agentd", "reason": "fix", "corrects": coffee,
		"op_id": "op_fix_" + coffee + "_1", "kind": "drink", "share_after": 0.666667, "volume_ml_after": 200.0, "volume_ml": -100.0,
		"caffeine_mg": -40.0, "kcal": -2.0, "protein_g": 0.0, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "eaten_at": "2026-10-01T08:40:00+02:00"})
	h.vars.add("var-food", "2026-09-30", map[string]any{"item": "lentil curry", "kind": "food", "portion_g": 400.0, "portion_basis": "stated",
		"kcal": 520.0, "protein_g": 28.0, "carbs_g": 70.0, "net_carbs_g": 54.0, "fat_g": 12.0, "sat_fat_g": 3.0, "fiber_g": 16.0,
		"source": "agentd", "op_id": "op_ag_curry", "eaten_at": "2026-09-30T19:00:00+02:00"})
	h.restart() // hydrate with the rows above

	docs := map[string][]byte{}
	var order []string
	keep := func(name string, b []byte) {
		order = append(order, name)
		docs[name] = b
	}
	// A two-item log for YESTERDAY (items of one entry share their time, and
	// /fuel/day breaks that tie by the random item id, so they stay off today).
	h.model.fn = func(ModelInput) string { return skyrWalnuts }
	lb, _ := json.Marshal(map[string]string{"client_id": "90000001-0001", "text": "250 g skyr and 30 g walnuts", "local_time": "2026-09-30T13:00:00+02:00"})
	logRec := h.do("POST", "/fuel/log", bytes.NewReader(lb), "application/json")
	if logRec.Code != 200 {
		t.Fatalf("log: %d %s", logRec.Code, logRec.Body)
	}
	keep("log", logRec.Body.Bytes())
	lr := decode[LogResponse](t, logRec)
	h.clk.Add(5 * time.Minute)
	h.model.fn = func(ModelInput) string { return oneItem("walnuts", 30, 196, 4.6, false) }
	wr := h.logText("90000001-0005", "30 g walnuts")
	if wr.Code != 200 {
		t.Fatalf("walnuts: %d %s", wr.Code, wr.Body)
	}
	walnuts := decode[LogResponse](t, wr)
	h.clk.Add(5 * time.Minute)
	h.model.fn = func(ModelInput) string { return drink("water", 250, 0, 0, 0, false) }
	if r := h.logText("90000001-0002", "a glass of water"); r.Code != 200 {
		t.Fatalf("water: %d %s", r.Code, r.Body)
	}
	h.clk.Add(5 * time.Minute)
	b, _ := json.Marshal(map[string]any{"client_id": "90000001-0003", "item_id": walnuts.Items[0].ItemID, "portion_g": 15})
	fix := h.do("POST", "/fuel/fix", bytes.NewReader(b), "application/json")
	if fix.Code != 200 {
		t.Fatalf("fix: %d %s", fix.Code, fix.Body)
	}
	keep("fix", fix.Body.Bytes())
	h.clk.Add(5 * time.Minute)
	var key string
	for _, r := range h.svc.recentAll() {
		if r.Item == "skyr" {
			key = r.Key
		}
	}
	half := 0.5
	relog := h.relog("90000001-0004", key, &half)
	if relog.Code != 200 {
		t.Fatalf("relog: %d %s", relog.Code, relog.Body)
	}
	keep("relog", relog.Body.Bytes())
	h.clk.Add(5 * time.Minute)
	for _, g := range []struct{ name, path string }{
		{"snapshot", "/fuel/snapshot"},
		{"snapshot_yesterday", "/fuel/snapshot?date=2026-09-30"},
		{"day", "/fuel/day"},
		{"entry", "/fuel/entry/" + lr.EntryID},
		{"feed", "/fuel/feed"},
		{"recent", "/fuel/recent"},
	} {
		rec := h.do("GET", g.path, nil, "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", g.name, rec.Code, rec.Body)
		}
		keep(g.name, rec.Body.Bytes())
	}
	return h, order, docs
}

// pruneToGolden rewrites a response with ONLY the keys that the v6 golden
// document has, in the order of the response. Random ids are numbered by
// their first appearance, so a NEW key that carries an id (v7: entry_id on
// an item) must not move the numbers of the v6 keys.
func pruneToGolden(raw []byte, want any) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	pruneValue(dec, want, &out)
	return out.Bytes()
}

func pruneValue(dec *json.Decoder, want any, out *bytes.Buffer) {
	tok, err := dec.Token()
	if err != nil {
		return
	}
	d, isDelim := tok.(json.Delim)
	switch {
	case isDelim && d == '{':
		wm, isObj := want.(map[string]any)
		out.WriteByte('{')
		first := true
		for dec.More() {
			kt, _ := dec.Token()
			key, _ := kt.(string)
			wv, keep := wm[key]
			if !isObj {
				keep = true // the golden has no object here: the compare reports it
			}
			if !keep {
				var skip json.RawMessage
				_ = dec.Decode(&skip)
				continue
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			kb, _ := json.Marshal(key)
			out.Write(kb)
			out.WriteByte(':')
			pruneValue(dec, wv, out)
		}
		_, _ = dec.Token()
		out.WriteByte('}')
	case isDelim && d == '[':
		wa, _ := want.([]any)
		out.WriteByte('[')
		for i := 0; dec.More(); i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			var wv any
			if i < len(wa) {
				wv = wa[i]
			}
			pruneValue(dec, wv, out)
		}
		_, _ = dec.Token()
		out.WriteByte(']')
	default:
		b, _ := json.Marshal(tok)
		out.Write(b)
	}
}

func TestGoldenV6(t *testing.T) {
	_, order, docs := goldenFixture(t, goldenTargetsV1)
	n := &goldenNorm{seen: map[string]string{}}
	for _, name := range order {
		path := filepath.Join("testdata", "golden_v6", name+".json")
		raw := docs[name]
		if os.Getenv("FUEL_GOLDEN_UPDATE") != "1" {
			if wantB, err := os.ReadFile(path); err == nil {
				var want any
				if json.Unmarshal(wantB, &want) == nil {
					raw = pruneToGolden(raw, want)
				}
			}
		}
		got := n.norm(raw)
		if os.Getenv("FUEL_GOLDEN_UPDATE") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		wantB, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		var want, have any
		if err := json.Unmarshal(wantB, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(got, &have); err != nil {
			t.Fatal(err)
		}
		var diffs []string
		goldenSubset(name, want, have, &diffs)
		for _, d := range diffs {
			t.Error(d)
		}
	}
}
