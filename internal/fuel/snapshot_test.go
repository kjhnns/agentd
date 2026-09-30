package fuel

import (
	"encoding/json"
	"testing"
	"time"
)

func row(id string, data map[string]any) Value {
	b, _ := json.Marshal(data)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return Value{ID: id, Data: m, CreatedAt: time.Unix(int64(len(id)), 0)}
}

func defaults(t *testing.T) *Targets {
	tg, err := ParseTargets([]byte(DefaultTargetsJSON))
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestStatusBands(t *testing.T) {
	tg := 160.0
	pace := 80.0
	cases := []struct {
		kind     string
		consumed float64
		want     string
	}{
		{"floor", 160, "met"}, {"floor", 96, "ahead"}, {"floor", 95, "on_pace"},
		{"floor", 64, "on_pace"}, {"floor", 63.9, "behind"},
		{"pace", 170, "ahead"}, {"cap", 161, "over"}, {"cap", 100, "on_pace"},
	}
	for _, c := range cases {
		p := &pace
		if c.kind == "cap" {
			p = nil
		}
		if got := statusFor(c.kind, c.consumed, &tg, p); got != c.want {
			t.Errorf("%s %v: %s want %s", c.kind, c.consumed, got, c.want)
		}
	}
	if statusFor("floor", 5, nil, nil) != "on_pace" {
		t.Error("null target")
	}
}

func TestSnapshotFormulas(t *testing.T) {
	tg := defaults(t)
	// 12:00 Zurich: elapsed = (720-420)/(1230-420) = 0.37037.
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	rows := []Value{
		row("a", map[string]any{"item": "x", "kcal": 500, "protein_g": 20, "carbs_g": 50, "fat_g": 10, "sat_fat_g": 5, "fiber_g": 4}),
		row("b", map[string]any{"item": "y", "kcal": 100, "protein_g": 5, "carbs_g": 10, "net_carbs_g": 9, "fat_g": 1, "sat_fat_g": 1, "fiber_g": nil}),
	}
	body := []Value{}
	for i := 0; i < 14; i++ {
		d := dateAdd("2026-10-01", -i)
		body = append(body, row("w"+d, map[string]any{"method": "withings_scale", "measured_at": d + "T07:00:00+02:00", "weight_kg": 80.0 - float64(i)*0.1}))
	}
	body = append(body, row("bf", map[string]any{"method": "withings_bia", "measured_at": "2026-09-28T23:30:00+02:00", "weight_kg": 80.3, "fat_pct": 14.59}))
	acts := []Activity{{SportType: "Run", MovingS: 50 * 60, DistanceM: 10000, LocalDate: "2026-09-29"}, {SportType: "Run", MovingS: 20 * 60, DistanceM: 5300, LocalDate: "2026-09-30"}}
	s := computeSnapshot(snapInput{
		date: "2026-10-01", now: now, targets: tg,
		rowsFor: func(d string) ([]Value, bool) {
			if d == "2026-10-01" {
				return rows, true
			}
			return nil, true
		},
		strength: func(d string) bool { return d == "2026-09-29" || d == "2026-09-30" },
		dataAsOf: now, revision: 7, body: body, acts: acts, stravaAt: now.Add(-time.Hour),
	})
	if s.DayType != "rest" || s.Revision != 7 {
		t.Fatalf("day type %s rev %d", s.DayType, s.Revision)
	}
	p := macro(s, "protein_g")
	if p.Consumed != 25 || *p.PaceTargetNow != 59.3 || p.Status != "behind" {
		t.Fatalf("protein %+v pace %v", p, *p.PaceTargetNow)
	}
	nc := macro(s, "net_carbs_g")
	if nc.Consumed != 55 { // 50-4 normalized + 9 explicit
		t.Fatalf("net carbs %v", nc.Consumed)
	}
	if f := macro(s, "fiber_g"); f.UnknownRows != 1 || f.Consumed != 4 {
		t.Fatalf("fibre %+v", f)
	}
	if sf := macro(s, "sat_fat_g"); sf.PaceTargetNow != nil || sf.Status != "on_pace" {
		t.Fatalf("sat fat %+v", sf)
	}
	// Next checkpoint 13:00: elapsed (780-420)/810 = 0.4444; protein pace 71.1,
	// need 46.1 -> 47 g; fibre pace 15.6 need 11.6 (0.33 of target) vs protein 0.29.
	if s.NextAction == nil || s.NextAction.By != "13:00" {
		t.Fatalf("next action %+v", s.NextAction)
	}
	if s.NextAction.Key != "fiber_g" || s.NextAction.Grams != 12 {
		t.Fatalf("next action %+v", s.NextAction)
	}
	// Day score: rest day, 5 checks, sat fat and net carbs pass.
	if s.DayScore.Of != 5 || s.DayScore.Hit != 2 {
		t.Fatalf("day score %+v", s.DayScore)
	}
	if s.Week.StrengthSessions != 2 || s.Week.Runs != 2 || s.Week.RunKm != 15.3 || s.Week.StrengthTarget != 3 {
		t.Fatalf("week %+v", s.Week)
	}
	// avg7(D) = mean of 80.0..79.4 = 79.7; avg7(D-7) = 79.0..78.4 mean 78.97 (bia day adds 80.3 on 09-28)
	if s.Weight.Avg7Kg == nil || *s.Weight.Avg7Kg != 79.74 {
		t.Fatalf("avg7 %v", *s.Weight.Avg7Kg)
	}
	if s.Weight.Delta7Kg == nil || *s.Weight.Delta7Kg != 0.74 {
		t.Fatalf("delta7 %v", *s.Weight.Delta7Kg)
	}
	if s.BodyFat.LatestPct == nil || *s.BodyFat.LatestPct != 14.59 || *s.BodyFat.LatestDate != "2026-09-28" {
		t.Fatalf("body fat %+v", s.BodyFat)
	}
	// Past date: evaluated at its end, no next action.
	past := computeSnapshot(snapInput{date: "2026-09-30", now: now, targets: tg,
		rowsFor: func(string) ([]Value, bool) { return rows, true }, stravaAt: now})
	if past.NextAction != nil || *macro(past, "protein_g").PaceTargetNow != 160 {
		t.Fatal("past date not evaluated at its end")
	}
}

func TestStreaksAndStaleStrava(t *testing.T) {
	tg := defaults(t)
	now := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	good := []Value{row("g", map[string]any{"item": "x", "kcal": 2300, "protein_g": 170, "carbs_g": 100, "fat_g": 60, "sat_fat_g": 15, "fiber_g": 40})}
	s := computeSnapshot(snapInput{date: "2026-10-01", now: now, targets: tg,
		rowsFor: func(d string) ([]Value, bool) {
			if d >= "2026-09-28" {
				return good, true
			}
			return nil, true
		}})
	if s.Streaks.Protein != 4 || s.Streaks.SatFat != 4 || s.Streaks.Fiber != 4 {
		t.Fatalf("streaks %+v", s.Streaks)
	}
	if s.DayType != "unknown" || len(s.Missing) == 0 || s.Missing[0] != "strava_stale" {
		t.Fatalf("day type %s missing %v", s.DayType, s.Missing)
	}
}

func TestTargetsValidation(t *testing.T) {
	bad := []string{
		`{}`,
		`{"protein_g":{"kind":"floor","value":160},"sat_fat_g":{"kind":"cap","value":20},"fiber_g":{"kind":"floor","value":35},"kcal":{"kind":"pace","rest":2300},"net_carbs_g":{"kind":"pace","value":120},"strength_per_week":3,"weight_band_kg":[78,82],"eating_window":{"start":"07:00","end":"20:30"},"tz":"Europe/Zurich"}`,
		`{"protein_g":{"kind":"flor","value":160},"sat_fat_g":{"kind":"cap","value":20},"fiber_g":{"kind":"floor","value":35},"kcal":{"kind":"pace","value":2300},"net_carbs_g":{"kind":"pace","value":120},"strength_per_week":3,"weight_band_kg":[78,82],"eating_window":{"start":"07:00","end":"20:30"},"tz":"Europe/Zurich"}`,
	}
	for i, b := range bad {
		if _, err := ParseTargets([]byte(b)); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if _, err := ParseStaples([]byte(`[{"key":"Skyr!","aliases":["s"],"per_100g":{"kcal":1,"protein_g":1,"carbs_g":1,"fat_g":1,"sat_fat_g":1,"fiber_g":null},"default_g":1}]`)); err == nil {
		t.Error("bad slug accepted")
	}
}

func TestRoundingHalfAwayFromZero(t *testing.T) {
	if toTenth(12.65) != 127 || toTenth(-12.65) != -127 || toTenth(0.05) != 1 || toTenth(-0.05) != -1 {
		t.Fatal("rounding")
	}
	m := Macros{Kcal: known(253)}
	if m.Scale(-0.5).Kcal.V != -127 {
		t.Fatal("scale rounding")
	}
}

func TestStravaParse(t *testing.T) {
	a := parseStrava([]byte("---\ntype: x\n---\n```json\n{\"sport_type\":\"Run\",\"moving_time\":1570,\"distance\":5297.6,\"start_date_local\":\"2026-09-30T15:47:25Z\"}\n```\n"))
	if a == nil || a.LocalDate != "2026-09-30" || a.SportType != "Run" || a.MovingS != 1570 {
		t.Fatalf("%+v", a)
	}
}
