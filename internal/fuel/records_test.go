package fuel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func recOpts(o *Options) {
	o.BPVar, o.SymptomVar, o.BodyVar = "Fuel e2e blood pressure", "Fuel e2e symptom log", "Fuel e2e body"
}

type recResp struct {
	Status   string          `json:"status"`
	Record   *Record         `json:"record"`
	RecordID string          `json:"record_id"`
	Review   json.RawMessage `json:"review"`
	Snapshot *Snapshot       `json:"snapshot"`
}

type recGet struct {
	Status string `json:"status"`
	Void   *struct {
		Status string `json:"status"`
	} `json:"void"`
	Record Record `json:"record"`
}

type recList struct {
	Type      string          `json:"type"`
	Records   []Record        `json:"records"`
	Clinician json.RawMessage `json:"clinician"`
	Info      string          `json:"info"`
}

var recSeq int

func cid() string {
	recSeq++
	return fmt.Sprintf("70000000-%04d", recSeq)
}

func (h *harness) record(typ string, data map[string]any, measuredAt string) (int, recResp, string) {
	body := map[string]any{"client_id": cid(), "type": typ, "data": data}
	if measuredAt != "" {
		body["measured_at"] = measuredAt
	}
	rec := h.post("/fuel/record", body)
	var r recResp
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r, rec.Body.String()
}

func bp(sys, dia int) map[string]any {
	return map[string]any{"systolic_mmhg": sys, "diastolic_mmhg": dia, "pulse_bpm": 58, "context": "home", "note": "after rest"}
}

// T9: each record type round-trips; void, idempotency, reconciliation.
func TestT9RecordsRoundTrip(t *testing.T) {
	rule := map[string]any{"category": "chest_pain", "text": "Call the practice the same day.", "set_by": "Dr. X", "set_on": "2026-10-01"}
	h := newV7(t, v2("clinician.symptom_review_rules", []any{rule}), recOpts)
	// Blood pressure.
	code, r, raw := h.record("blood_pressure", bp(128, 82), "2026-10-01T08:30:00+02:00")
	if code != 200 || r.Status != "done" || r.Record == nil || r.Snapshot == nil || r.Review != nil {
		t.Fatalf("bp: %d %s", code, raw)
	}
	id := r.Record.RecordID
	if !strings.HasPrefix(id, "rc_") || r.Record.Type != "blood_pressure" || r.Record.MeasuredAt != "2026-10-01T08:30:00+02:00" || r.Record.Pending || r.Record.ValueID == nil || r.Record.Source != "fuel" {
		t.Errorf("record: %+v", r.Record)
	}
	if d := r.Record.Data; d["systolic_mmhg"] != 128.0 || d["diastolic_mmhg"] != 82.0 || d["pulse_bpm"] != 58.0 || d["context"] != "home" || d["note"] != "after rest" {
		t.Errorf("data: %v", d)
	}
	rows := h.vars.rows("var-bp")
	if len(rows) != 1 || rows[0]["_record_date"] != "2026-10-01" || rows[0]["source"] != "fuel" || rows[0]["type"] != "blood_pressure" || rows[0]["record_id"] != id || rows[0]["op_id"] == "" {
		t.Fatalf("row: %v", rows)
	}
	if s := r.Snapshot.Records.BloodPressure; s == nil || s.Latest == nil || s.Latest.RecordID != id || s.Count7d != 1 {
		t.Errorf("snapshot part: %+v", r.Snapshot.Records.BloodPressure)
	}
	g := decode[recGet](t, h.get("/fuel/record/"+id))
	if g.Status != "done" || g.Void != nil || g.Record.RecordID != id {
		t.Errorf("get: %+v", g)
	}
	l := decode[recList](t, h.get("/fuel/records?type=blood_pressure"))
	if len(l.Records) != 1 || l.Records[0].RecordID != id || l.Type != "blood_pressure" || !strings.HasSuffix(l.Info, "#bp-home") {
		t.Errorf("list: %+v", l)
	}
	// Symptom with a review rule; "none" needs present false.
	code, r, raw = h.record("symptom", map[string]any{"category": "chest_pain", "present": true, "note": "on the hill"}, "")
	if code != 200 || !strings.Contains(string(r.Review), "Call the practice the same day.") {
		t.Fatalf("symptom: %d %s", code, raw)
	}
	var review Instruction
	_ = json.Unmarshal(r.Review, &review)
	if review.Text != rule["text"] || review.SetBy != "Dr. X" {
		t.Errorf("review must be the file text, byte-equal: %+v", review)
	}
	code, r, raw = h.record("symptom", map[string]any{"category": "none", "present": false}, "")
	if code != 200 || string(r.Review) != "null" {
		t.Fatalf("symptom none: %d %s", code, raw)
	}
	if s := h.snap(t, "").Records.Symptom; s == nil || !s.EntryThisWeek || s.LatestDate == nil || *s.LatestDate != "2026-10-01" {
		t.Errorf("symptom snapshot: %+v", s)
	}
	// Waist goes into the body variable with method tape and no weight_kg.
	code, r, raw = h.record("waist", map[string]any{"waist_cm": 84.5}, "2026-09-20T07:00:00+02:00")
	if code != 200 {
		t.Fatalf("waist: %d %s", code, raw)
	}
	h.record("waist", map[string]any{"waist_cm": 84.0}, "2026-10-01T07:00:00+02:00")
	wrows := h.vars.rows("var-body2")
	if len(wrows) != 2 || wrows[0]["method"] != "tape" || wrows[0]["waist_cm"] != 84.5 {
		t.Fatalf("waist rows: %v", wrows)
	}
	if _, has := wrows[0]["weight_kg"]; has {
		t.Error("a waist row must have no weight_kg key")
	}
	w := h.snap(t, "").Progress.Waist
	if num(w.LatestCm) != "84" || *w.LatestDate != "2026-10-01" || num(w.PreviousCm) != "84.5" || *w.PreviousDate != "2026-09-20" {
		t.Errorf("waist progress: %+v", w)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=waist&from=2026-09-01&to=2026-09-30")); len(l.Records) != 1 || string(l.Clinician) != "null" {
		t.Errorf("waist list by date: %+v", l)
	}

	// Void: hidden at once, for every date filter.
	rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": id})
	if rec.Code != 200 {
		t.Fatalf("void: %d %s", rec.Code, rec.Body)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure&from=2026-09-01&to=2026-10-01")); len(l.Records) != 0 {
		t.Errorf("voided record still listed: %+v", l.Records)
	}
	if s := h.snap(t, "").Records.BloodPressure; s.Latest != nil || s.Count7d != 0 {
		t.Errorf("voided record in the snapshot: %+v", s)
	}
	if g := decode[recGet](t, h.get("/fuel/record/"+id)); g.Status != "done" || g.Void == nil || g.Void.Status != "done" {
		t.Errorf("get after void: %+v", g)
	}
	vrows := h.vars.rows("var-bp")
	if len(vrows) != 2 || vrows[1]["voids"] != id || vrows[1]["op_id"] != "op_void_"+id || vrows[1]["_record_date"] != "2026-10-01" || vrows[1]["measured_at"] != "2026-10-01T08:30:00+02:00" {
		t.Errorf("void row: %v", vrows)
	}
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": id}); rec.Code != 409 || errCode(t, rec) != "already_voided" {
		t.Errorf("second void: %d %s", rec.Code, rec.Body)
	}
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": "rc_nope"}); rec.Code != 404 {
		t.Errorf("void of an unknown record: %d", rec.Code)
	}
	if rec := h.get("/fuel/record/rc_nope"); rec.Code != 404 {
		t.Errorf("get of an unknown record: %d", rec.Code)
	}
	// After a restart the voided record stays hidden (rows are the truth).
	h.restart()
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 0 {
		t.Errorf("after restart: %+v", l.Records)
	}
	// No record operation is in the food journal, and none of the food files
	// got a new line shape (a v6 binary replays this state directory).
	jb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "journal.jsonl"))
	if strings.Contains(string(jb), "blood_pressure") || strings.Contains(string(jb), "rc_") {
		t.Error("a record operation is in journal.jsonl")
	}
	fb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "feed.jsonl"))
	if len(fb) != 0 {
		t.Errorf("a record wrote a feed line: %s", fb)
	}
	if _, err := os.Stat(filepath.Join(h.opts.StateDir, "coach-events.jsonl")); err == nil {
		t.Error("a record wrote a coach event")
	}
}

func TestT9RecordBoundsAndTypes(t *testing.T) {
	h := newV7(t, v2(), recOpts)
	bad := []struct {
		typ  string
		data map[string]any
		at   string
	}{
		{"blood_pressure", map[string]any{"systolic_mmhg": 59, "diastolic_mmhg": 40, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 261, "diastolic_mmhg": 80, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 29, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 161, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 90, "diastolic_mmhg": 90, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120.5, "diastolic_mmhg": 80, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "pulse_bpm": 24, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "pulse_bpm": 221, "context": "home"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "context": "gym"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "context": "home", "status": "ok"}, ""},
		{"blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "context": "home", "note": strings.Repeat("x", 501)}, ""},
		{"blood_pressure", bp(120, 80), "2026-10-01T13:00:00+02:00"}, // in the future
		{"blood_pressure", bp(120, 80), "2026-08-01T08:00:00+02:00"}, // older than 34 days
		{"blood_pressure", bp(120, 80), "yesterday"},
		{"symptom", map[string]any{"category": "none", "present": true}, ""},
		{"symptom", map[string]any{"category": "mood", "present": false}, ""},
		{"symptom", map[string]any{"category": "headache", "present": true}, ""},
		{"waist", map[string]any{"waist_cm": 39}, ""},
		{"waist", map[string]any{"waist_cm": 151}, ""},
		{"strength_set", map[string]any{"exercise": "push up", "sets": 3}, ""},
		{"strength_test", map[string]any{"reps": 10}, ""},
	}
	for i, b := range bad {
		code, _, raw := h.record(b.typ, b.data, b.at)
		if code != 400 || !strings.Contains(raw, "bad_input") {
			t.Errorf("bad %d (%s): %d %s", i, b.typ, code, raw)
		}
	}
	if h.vars.posts != 0 {
		t.Errorf("a refused record was posted (%d posts)", h.vars.posts)
	}
	if code, _, raw := h.record("blood_pressure", map[string]any{"systolic_mmhg": 120, "diastolic_mmhg": 80, "context": "office"}, ""); code != 200 {
		t.Errorf("pulse is optional: %d %s", code, raw)
	}
	if rows := h.vars.rows("var-bp"); len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	} else if _, has := rows[0]["pulse_bpm"]; has {
		t.Error("a null pulse must be omitted from the row")
	}
	for _, q := range []string{"?type=strength_set", "?type=blood_pressure&from=2026-01-01&to=2026-10-01", "?type=blood_pressure&from=x"} {
		if rec := h.get("/fuel/records" + q); rec.Code != 400 {
			t.Errorf("records%s: %d", q, rec.Code)
		}
	}
}

func TestT9IdempotencyAndUncertainWrites(t *testing.T) {
	h := newV7(t, v2(), recOpts, func(o *Options) { o.RecheckAfter = time.Second })
	// An idempotent replay by client_id writes one row; a changed payload = 409.
	body := map[string]any{"client_id": "71000000-0001", "type": "blood_pressure", "data": bp(120, 80), "measured_at": "2026-10-01T08:00:00+02:00"}
	a := h.post("/fuel/record", body)
	b := h.post("/fuel/record", body)
	if a.Code != 200 || b.Code != 200 || len(h.vars.rows("var-bp")) != 1 {
		t.Fatalf("replay: %d %d rows %d", a.Code, b.Code, len(h.vars.rows("var-bp")))
	}
	if decode[recResp](t, a).Record.RecordID != decode[recResp](t, b).Record.RecordID {
		t.Error("the replay names another record")
	}
	body["data"] = bp(121, 80)
	if c := h.post("/fuel/record", body); c.Code != 409 || errCode(t, c) != "idempotency_conflict" {
		t.Errorf("changed payload: %d %s", c.Code, c.Body)
	}
	// A first-attempt 400 from Variables = 502 and no record.
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { return false, 400 }
	code, _, raw := h.record("blood_pressure", bp(130, 85), "")
	if code != 502 || !strings.Contains(raw, "upstream_failed") {
		t.Errorf("rejected: %d %s", code, raw)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 1 {
		t.Errorf("a rejected record is listed: %d", len(l.Records))
	}
	// An uncertain POST (the row arrived, the answer did not): 202, then
	// found by op_id and NOT posted twice.
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { return true, 500 }
	code, r, raw := h.record("blood_pressure", bp(140, 90), "")
	if code != 202 || r.Status != "pending_reconciliation" || r.RecordID == "" {
		t.Fatalf("uncertain: %d %s", code, raw)
	}
	h.vars.onPost = nil
	pid := r.RecordID
	if g := decode[recGet](t, h.get("/fuel/record/"+pid)); g.Status != "pending_reconciliation" || !g.Record.Pending {
		t.Errorf("pending get: %+v", g)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 2 || !l.Records[0].Pending {
		t.Errorf("a pending record is listed with pending true: %+v", l.Records)
	}
	// A void of a pending record = 409 pending.
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": pid}); rec.Code != 409 || errCode(t, rec) != "pending" {
		t.Errorf("void of a pending record: %d %s", rec.Code, rec.Body)
	}
	posts := h.vars.posts
	h.clk.Add(2 * time.Second)
	h.svc.reconcileOnce(context.Background())
	if g := decode[recGet](t, h.get("/fuel/record/"+pid)); g.Status != "done" || g.Record.Pending || g.Record.ValueID == nil {
		t.Errorf("after reconcile: %+v", g)
	}
	if h.vars.posts != posts || len(h.vars.rows("var-bp")) != 2 {
		t.Errorf("the uncertain POST was posted again (%d posts, %d rows)", h.vars.posts-posts, len(h.vars.rows("var-bp")))
	}
	// An uncertain POST that did NOT arrive is re-posted with the same op_id.
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { return false, 500 }
	_, r, _ = h.record("symptom", map[string]any{"category": "mood", "present": true}, "")
	h.vars.onPost = nil
	h.clk.Add(2 * time.Second)
	h.svc.reconcileOnce(context.Background())
	if g := decode[recGet](t, h.get("/fuel/record/"+r.RecordID)); g.Status != "done" || len(h.vars.rows("var-sym")) != 1 {
		t.Errorf("re-post: %+v rows %d", g, len(h.vars.rows("var-sym")))
	}
}

// A write that timed out, was set to failed, and whose row then appears in a
// refresh is done with that value id; the same for a void row; also after a
// restart on the same state directory.
func TestT9RowsAreTheTruth(t *testing.T) {
	h := newV7(t, v2(), recOpts, func(o *Options) { o.RecheckAfter = time.Second; o.FailAfter = time.Hour })
	var held map[string]any
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { held = d; return false, 500 }
	_, r, _ := h.record("blood_pressure", bp(118, 76), "2026-10-01T09:00:00+02:00")
	id := r.RecordID
	// Reads fail while the op is retried, until it is failed after FailAfter.
	h.vars.failReads.Store(true)
	h.clk.Add(2 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	h.vars.failReads.Store(false)
	h.vars.onPost = nil
	if g := decode[recGet](t, h.get("/fuel/record/"+id)); g.Status != "failed" {
		t.Fatalf("the op must be failed: %+v", g)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 0 {
		t.Errorf("a failed record is listed")
	}
	// The row arrives late (the timed-out POST did land).
	h.vars.add("var-bp", "2026-10-01", held)
	h.restart() // a v7 start re-reads the record variables
	g := decode[recGet](t, h.get("/fuel/record/"+id))
	if g.Status != "done" || g.Record.ValueID == nil {
		t.Fatalf("the late row must make the record done: %+v", g)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 1 {
		t.Errorf("the record must be listed: %d", len(l.Records))
	}
	// A failed void makes the record visible again; GET shows void failed.
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { held = d; return false, 400 }
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": id}); rec.Code != 502 {
		t.Fatalf("rejected void: %d %s", rec.Code, rec.Body)
	}
	h.vars.onPost = nil
	if g := decode[recGet](t, h.get("/fuel/record/"+id)); g.Status != "done" || g.Void == nil || g.Void.Status != "failed" {
		t.Errorf("after a failed void: %+v", g)
	}
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 1 {
		t.Error("a failed void must leave the record visible")
	}
	// The void row is found later: hidden again.
	h.vars.add("var-bp", "2026-10-01", held)
	h.restart()
	if l := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(l.Records) != 0 {
		t.Error("a void row that is found must hide the record")
	}
	// A new void after a failed one is allowed and has the same op_id.
	_, r2, _ := h.record("blood_pressure", bp(119, 77), "")
	h.vars.onPost = func(n int, d map[string]any) (bool, int) { return false, 400 }
	h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": r2.Record.RecordID})
	h.vars.onPost = nil
	if rec := h.post("/fuel/record/void", map[string]any{"client_id": cid(), "record_id": r2.Record.RecordID}); rec.Code != 200 {
		t.Errorf("a new void after a failed one: %d %s", rec.Code, rec.Body)
	}
}

func TestT9MissingVariableAndTestMode(t *testing.T) {
	// Only the symptom variable is configured; the blood-pressure name does
	// not resolve.
	h := newV7(t, v2(), func(o *Options) { o.SymptomVar, o.BPVar = "Fuel e2e symptom log", "No such variable" })
	if code, _, raw := h.record("blood_pressure", bp(120, 80), ""); code != 503 || !strings.Contains(raw, "variable_missing") {
		t.Errorf("missing variable: %d %s", code, raw)
	}
	if rec := h.get("/fuel/records?type=blood_pressure"); rec.Code != 503 {
		t.Errorf("records of a missing variable: %d", rec.Code)
	}
	if code, _, raw := h.record("symptom", map[string]any{"category": "none", "present": false}, ""); code != 200 {
		t.Errorf("the other type must work: %d %s", code, raw)
	}
	s := h.snap(t, "")
	if s.Records.BloodPressure != nil || s.Records.Symptom == nil || !has(s.Missing, "blood_pressure") || has(s.Missing, "symptom") {
		t.Errorf("snapshot: bp %v symptom %v missing %v", s.Records.BloodPressure, s.Records.Symptom, s.Missing)
	}
	// The waist path on the real Body composition name is read-only in test mode.
	if code, _, raw := h.record("waist", map[string]any{"waist_cm": 84}, ""); code != 409 || !strings.Contains(raw, "test_mode_read_only") {
		t.Errorf("waist in test mode: %d %s", code, raw)
	}
	// With no record key set, `missing` is the v6 list and both parts are null.
	h2 := newV7(t, v2())
	if s := h2.snap(t, ""); s.Records.BloodPressure != nil || s.Records.Symptom != nil || has(s.Missing, "blood_pressure") || has(s.Missing, "symptom") {
		t.Errorf("no keys: %+v missing %v", s.Records, s.Missing)
	}
	// test_mode refuses the production variable names.
	for _, mut := range []func(*Options){
		func(o *Options) { o.BPVar = "Blood pressure" },
		func(o *Options) { o.SymptomVar = " symptom log " },
	} {
		o := h.opts
		o.StateDir = t.TempDir()
		mut(&o)
		if _, err := New(o); err == nil || !strings.Contains(err.Error(), "test_mode refuses") {
			t.Errorf("test mode accepted a production record variable: %v", err)
		}
	}
}

// A waist row leaves the weight average and the body fat unchanged.
func TestT9WaistRowDoesNotMoveWeightOrBodyFat(t *testing.T) {
	h := newV7(t, v2(), recOpts)
	for i, kg := range []float64{80.4, 80.1, 80.3, 79.9, 80.0} {
		d := dateAdd("2026-10-01", -i)
		row := map[string]any{"weight_kg": kg, "method": "withings_scale", "measured_at": d + "T06:30:00+02:00"}
		if i == 1 {
			row["method"], row["fat_pct"] = "withings_bia", 17.5
		}
		h.vars.add("var-body2", d, row)
	}
	h.restart()
	before := h.snap(t, "")
	if code, _, raw := h.record("waist", map[string]any{"waist_cm": 84.2, "note": "morning"}, ""); code != 200 {
		t.Fatalf("waist: %d %s", code, raw)
	}
	h.restart()
	after := h.snap(t, "")
	if num(before.Weight.Avg7Kg) != num(after.Weight.Avg7Kg) || num(before.BodyFat.LatestPct) != num(after.BodyFat.LatestPct) ||
		len(before.Weight.Points) != len(after.Weight.Points) || num(after.Weight.Avg7Kg) == "null" {
		t.Errorf("weight %s to %s, body fat %s to %s", num(before.Weight.Avg7Kg), num(after.Weight.Avg7Kg), num(before.BodyFat.LatestPct), num(after.BodyFat.LatestPct))
	}
	if num(after.Progress.Waist.LatestCm) != "84.2" || num(after.Progress.Weight.Avg7Kg) != num(after.Weight.Avg7Kg) || len(after.Progress.Weight.BandKg) != 2 {
		t.Errorf("progress: %+v", after.Progress)
	}
}

var maskNums = regexp.MustCompile(`"(systolic_mmhg|diastolic_mmhg)":\d+`)
var maskIDs = regexp.MustCompile(`"(rc_|op_|val-)[0-9a-f-]+"|"(as_of|data_as_of)":"[^"]+"`)

// T10: no interpretation, ever.
func TestT10NoInterpretation(t *testing.T) {
	rule := map[string]any{"category": "palpitations", "text": "Note the time and the pulse. Tell me at the next visit.", "set_by": "Dr. X", "set_on": "2026-10-01"}
	bpInst := map[string]any{"text": "Measure seated, twice, in the morning.", "set_by": "Dr. X", "set_on": "2026-10-01"}
	run := func(sys, dia int) string {
		h := newV7(t, v2("clinician.symptom_review_rules", []any{rule}, "clinician.bp.home", bpInst), recOpts)
		body := map[string]any{"client_id": "72000000-0001", "type": "blood_pressure", "data": bp(sys, dia), "measured_at": "2026-10-01T08:00:00+02:00"}
		rec := h.post("/fuel/record", body)
		if rec.Code != 200 {
			t.Fatalf("bp %d/%d: %d %s", sys, dia, rec.Code, rec.Body)
		}
		// No coach event, no feed line, no text block.
		if fb, _ := os.ReadFile(filepath.Join(h.opts.StateDir, "feed.jsonl")); len(fb) != 0 {
			t.Errorf("feed line: %s", fb)
		}
		if _, err := os.Stat(filepath.Join(h.opts.StateDir, "coach-events.jsonl")); err == nil {
			t.Error("coach event")
		}
		if keysOf(rec.Body.Bytes())["blocks"] || keysOf(rec.Body.Bytes())["text"] && !strings.Contains(rec.Body.String(), `"text":"Measure seated`) {
			t.Error("a record answer has a text block")
		}
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		var snap map[string]json.RawMessage
		_ = json.Unmarshal(doc["snapshot"], &snap)
		var recs map[string]json.RawMessage
		_ = json.Unmarshal(snap["records"], &recs)
		for name, part := range map[string]json.RawMessage{"record": doc["record"], "records.blood_pressure": recs["blood_pressure"], "records.symptom": recs["symptom"]} {
			keys := keysOf(part)
			for _, bad := range []string{"status", "target", "pace_target_now", "trend", "delta", "average", "score"} {
				if keys[bad] {
					t.Errorf("%s has a key named %q", name, bad)
				}
			}
		}
		if !strings.Contains(string(recs["blood_pressure"]), "Measure seated, twice, in the morning.") {
			t.Error("the clinician instruction must be shown verbatim")
		}
		out := maskNums.ReplaceAllString(rec.Body.String(), `"$1":0`)
		return maskIDs.ReplaceAllString(out, `"x"`)
	}
	if a, b := run(180, 110), run(100, 60); a != b {
		t.Errorf("the answers differ beyond the two pressure numbers:\n%s\n%s", a, b)
	}
	// The symptom review text is byte-equal to the file text and has no judgement key.
	h := newV7(t, v2("clinician.symptom_review_rules", []any{rule}), recOpts)
	code, r, raw := h.record("symptom", map[string]any{"category": "palpitations", "present": true}, "")
	if code != 200 {
		t.Fatalf("symptom: %d %s", code, raw)
	}
	var review map[string]any
	_ = json.Unmarshal(r.Review, &review)
	if review["text"] != rule["text"] || len(review) != 3 {
		t.Errorf("review: %v", review)
	}
	l := h.get("/fuel/records?type=symptom")
	if !strings.Contains(l.Body.String(), `"rules":[{"category":"palpitations"`) {
		t.Errorf("records clinician: %s", l.Body)
	}
	for _, bad := range []string{"status", "target", "trend", "delta", "average", "score"} {
		if keysOf(l.Body.Bytes())[bad] {
			t.Errorf("GET /fuel/records has a key named %q", bad)
		}
	}
}
