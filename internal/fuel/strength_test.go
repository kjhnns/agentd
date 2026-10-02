package fuel

import (
	"strings"
	"testing"
)

// The T17 fixture (spec 18.11): the week of Monday 2026-09-28, the clock on
// Thursday 2026-10-01 12:00 Europe/Zurich.
func strengthDoc(kv ...any) string {
	base := []any{"energy.calibration", calibSettings("2026-08-01"),
		"strength", map[string]any{"set_variables": []any{"Fuel e2e push ups", "Fuel e2e pull ups"}, "session_min_sets": 3, "strava_sport_types": []any{"WeightTraining"}}}
	return v2(append(base, kv...)...)
}

func strengthFixture(h *harness) {
	push := func(d string, reps ...any) {
		for _, r := range reps {
			h.vars.add("var-epush", d, r)
		}
	}
	pull := func(d string, reps ...any) {
		for _, r := range reps {
			h.vars.add("var-epull", d, r)
		}
	}
	push("2026-09-28", 20, 15)
	pull("2026-09-28", 8)
	push("2026-09-29", 12, 0, "abc") // 0 and a text are not sets
	pull("2026-09-29", 6)
	h.vars.add("var-cold", "2026-09-29", 1) // not a strength variable
	push("2026-09-30", 20, 18, 15)
	pull("2026-09-30", 8, 7)
	// Three squat sets on Tuesday: counted only once the variable is listed.
	for _, r := range []any{10, 10, 10} {
		h.vars.add("var-esquat", "2026-09-29", r)
	}
	// Saturday of the week before: 4 sets (for last7).
	push("2026-09-26", 10, 10, 10, 10)
	now := h.clk.Now()
	h.strava("2026-09-30-wt", "WeightTraining", 1800, 0, "2026-09-30T18:00:00", now)
	h.strava("2026-10-01-wt", "WeightTraining", 1200, 0, "2026-10-01T07:00:00", now)
	h.strava("2026-09-29-run", "Run", 1800, 5000, "2026-09-29T07:00:00", now) // a Run makes no session
}

func TestT17StrengthSetsAndSessions(t *testing.T) {
	h := newV7(t, strengthDoc())
	strengthFixture(h)
	h.restart()
	rec := h.get("/fuel/snapshot")
	s := decode[Snapshot](t, rec)
	st := s.Strength
	if st.Rule != "sets" || st.Sessions != 3 || s.Week.StrengthSessions != 3 || st.SessionsTarget != 3 || st.SessionMinSets == nil || *st.SessionMinSets != 3 {
		t.Fatalf("strength: %+v, week.strength_sessions %d", st, s.Week.StrengthSessions)
	}
	if st.Week == nil || st.Week.Sets != 10 || st.Week.Reps != 129 || len(st.Week.ByVariable) != 2 {
		t.Fatalf("week: %+v", st.Week)
	}
	if p := st.Week.ByVariable[0]; p.Name != "Fuel e2e push ups" || p.Sets != 6 || p.Reps != 100 {
		t.Errorf("push: %+v", p)
	}
	if p := st.Week.ByVariable[1]; p.Name != "Fuel e2e pull ups" || p.Sets != 4 || p.Reps != 29 {
		t.Errorf("pull: %+v", p)
	}
	if len(st.MissingVariables) != 0 || st.Clinician != nil || s.Progress.StrengthTest.Protocol != nil {
		t.Errorf("missing %v clinician %v", st.MissingVariables, st.Clinician)
	}
	keys := keysOf(rec.Body.Bytes())
	for _, bad := range []string{"hard_sets_week", "hard", "by_group", "hard_sets"} {
		if keys[bad] {
			t.Errorf("a response key is named %q", bad)
		}
	}
	posts := h.vars.posts

	// A name that does not resolve: listed, the numbers stay.
	h.setTargets(strengthDoc("strength", map[string]any{"set_variables": []any{"Fuel e2e push ups", "Fuel e2e pull ups", "No such variable"}, "session_min_sets": 3, "strava_sport_types": []any{"WeightTraining"}}))
	s = h.snap(t, "")
	if !has(s.Strength.MissingVariables, "No such variable") || s.Strength.Sessions != 3 || s.Strength.Week.Sets != 10 {
		t.Errorf("unresolved name: %+v", s.Strength)
	}
	// A variable added to the list AFTER startup by a reload: its cached
	// values count at once, with no new per-date read.
	reads := h.vars.reads.Load()
	h.setTargets(strengthDoc("strength", map[string]any{"set_variables": []any{"Fuel e2e push ups", "Fuel e2e pull ups", "Fuel e2e squats"}, "session_min_sets": 3, "strava_sport_types": []any{"WeightTraining"}}))
	s = h.snap(t, "")
	if s.Strength.Sessions != 4 || s.Strength.Week.Sets != 13 || len(s.Strength.Week.ByVariable) != 3 || s.Strength.Week.ByVariable[2].Sets != 3 {
		t.Errorf("after the reload: %+v", s.Strength)
	}
	if got := h.vars.reads.Load(); got != reads {
		t.Errorf("the reload made %d per-date reads", got-reads)
	}
	// session_min_sets 1: the Tuesday counts.
	h.setTargets(strengthDoc("strength", map[string]any{"set_variables": []any{"Fuel e2e push ups", "Fuel e2e pull ups"}, "session_min_sets": 1, "strava_sport_types": []any{"WeightTraining"}}))
	if s = h.snap(t, ""); s.Strength.Sessions != 4 {
		t.Errorf("min sets 1: %d sessions", s.Strength.Sessions)
	}
	// An empty Strava type list: Thursday (only an activity) is no session.
	h.setTargets(strengthDoc("strength", map[string]any{"set_variables": []any{"Fuel e2e push ups", "Fuel e2e pull ups"}, "session_min_sets": 3, "strava_sport_types": []any{}}))
	if s = h.snap(t, ""); s.Strength.Sessions != 2 {
		t.Errorf("no strava types: %d sessions", s.Strength.Sessions)
	}
	// Clinician texts come back verbatim; the protocol is the whole object.
	hard := map[string]any{"text": "Two hard sets per exercise. Not to failure.", "set_by": "Dr. X", "set_on": "2026-10-01"}
	proto := map[string]any{"text": "5 submaximal squats at 60 kg, rated by effort.", "set_by": "Dr. Y", "set_on": "2026-09-20"}
	h.setTargets(strengthDoc("clinician.hard_sets", hard, "clinician.strength_test_protocol", proto))
	s = h.snap(t, "")
	if c := s.Strength.Clinician; c == nil || c.Text != hard["text"] || c.SetBy != "Dr. X" || c.SetOn != "2026-10-01" {
		t.Errorf("hard sets instruction: %+v", c)
	}
	if p := s.Progress.StrengthTest.Protocol; p == nil || p.Text != proto["text"] || p.SetBy != "Dr. Y" || p.SetOn != "2026-09-20" {
		t.Errorf("protocol: %+v", p)
	}
	if h.vars.posts != posts {
		t.Error("fueld posted to Variables")
	}
	for id, n := range h.vars.postsTo {
		if strings.HasPrefix(id, "var-e") && n > 0 {
			t.Errorf("fueld posted %d values to the strength variable %s", n, id)
		}
	}
}

func TestT17LegacyStrengthRule(t *testing.T) {
	for _, doc := range []string{goldenTargetsV1, v2()} {
		h := newV7(t, doc)
		h.vars.add("var-push", "2026-09-28", 20)
		h.vars.add("var-push", "2026-09-29", 0)   // the v6 predicate: "0" is no session
		h.vars.add("var-epush", "2026-09-30", 20) // not Push ups / Pull ups
		h.strava("wt", "WeightTraining", 1800, 0, "2026-10-01T07:00:00", h.clk.Now())
		h.restart()
		s := h.snap(t, "")
		if s.Strength.Rule != "legacy" || s.Strength.Week != nil || s.Strength.SessionMinSets != nil || s.Strength.Sessions != 1 || s.Week.StrengthSessions != 1 {
			t.Errorf("legacy: %+v week %d", s.Strength, s.Week.StrengthSessions)
		}
		v := h.week(t, "")
		w := v.Strength.Week
		if v.Strength.Rule != "legacy" || w.Sets != nil || w.Reps != nil || w.ByVariable != nil || w.Sessions != 1 || !w.Days[0].Session || w.Days[1].Session || w.Days[3].Strava || w.Days[0].Sets != nil {
			t.Errorf("legacy week: %+v", w)
		}
	}
}

func TestW15StrengthInTheWeekView(t *testing.T) {
	h := newV7(t, strengthDoc("clinician.hard_sets", map[string]any{"text": "Not to failure.", "set_by": "Dr. X", "set_on": "2026-10-01"}))
	strengthFixture(h)
	h.restart()
	v := h.week(t, "")
	st := v.Strength
	if st.Rule != "sets" || st.SessionsTarget != 3 || st.Week.Sessions != 3 || *st.Week.Sets != 10 || *st.Week.Reps != 129 {
		t.Fatalf("strength: %+v", st)
	}
	want := []bool{true, false, true, true, false, false, false}
	for i, d := range st.Week.Days {
		if d.Session != want[i] {
			t.Errorf("%s session %v", d.Date, d.Session)
		}
		if i >= 4 && (*d.Sets != 0 || *d.Reps != 0 || d.Strava) {
			t.Errorf("future %s: %+v", d.Date, d)
		}
	}
	if d := st.Week.Days[2]; !d.Strava || *d.Sets != 5 || *d.Reps != 68 {
		t.Errorf("Wednesday: %+v", d)
	}
	if d := st.Week.Days[3]; !d.Strava || *d.Sets != 0 || !d.Session {
		t.Errorf("Thursday: %+v", d)
	}
	if b := st.Week.ByVariable; len(b) != 2 || b[0].Sets != 6 || b[0].Reps != 100 || b[1].Sets != 4 || b[1].Reps != 29 {
		t.Errorf("by_variable: %+v", b)
	}
	// last7 = 2026-09-25 to 2026-10-01: the same dates plus Saturday (4 sets).
	if l := st.Last7; l.Sessions != 4 || *l.Sets != 14 || l.Days[1].Date != "2026-09-26" || !l.Days[1].Session || *l.Days[1].Sets != 4 {
		t.Errorf("last7: %+v", l)
	}
	keys := keysOf(mustMarshal(st))
	for _, bad := range []string{"status", "score", "hard", "clinician", "hard_sets"} {
		if keys[bad] {
			t.Errorf("strength has a key named %q", bad)
		}
	}
	if s := h.snap(t, ""); s.Strength.Sessions != st.Week.Sessions {
		t.Errorf("snapshot %d, week %d", s.Strength.Sessions, st.Week.Sessions)
	}
}
