package fuel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Records (spec 18.4): blood pressure, symptoms and waist. They are stored
// and shown as records. The server and the app NEVER interpret them: no
// status, no target, no trend, no average, no judgement text. A clinician
// instruction is text, shown verbatim.
//
// Record operations live in their own store (records.jsonl), so the food
// journal and a v6 binary never see them.

const (
	recBP      = "blood_pressure"
	recSymptom = "symptom"
	recWaist   = "waist"
)

// Production variable names of the record types (refused in test mode).
const (
	prodBPVar      = "Blood pressure"
	prodSymptomVar = "Symptom log"
	prodBodyVar    = "Body composition"
)

// Record is the wire shape of one record.
type Record struct {
	RecordID   string         `json:"record_id"`
	Type       string         `json:"type"`
	MeasuredAt string         `json:"measured_at"`
	Data       map[string]any `json:"data"`
	ValueID    *string        `json:"value_id"`
	Source     string         `json:"source"`
	Pending    bool           `json:"pending"`

	date string    // local date of measured_at
	at   time.Time // measured_at as an instant
}

// recOp is one line of records.jsonl: one row write (a record or a void).
// The last line of an op_id wins.
type recOp struct {
	OpID        string         `json:"op_id"`
	RecordID    string         `json:"record_id"`
	Type        string         `json:"type"`
	Var         string         `json:"var"`  // bp | symptom | body
	Kind        string         `json:"kind"` // record | void
	Voids       string         `json:"voids,omitempty"`
	RecordDate  string         `json:"record_date"`
	Payload     map[string]any `json:"payload"`
	ClientID    string         `json:"client_id"`
	RequestHash string         `json:"request_hash"`
	State       string         `json:"state"`
	ValueID     string         `json:"value_id,omitempty"`
	Attempts    int            `json:"attempts"`
	At          time.Time      `json:"at"`
	LastTry     time.Time      `json:"last_try"`
}

// recordStore is the durable store of record operations.
type recordStore struct {
	mu       sync.Mutex
	path     string
	ops      map[string]*recOp // by op id
	order    []string
	byClient map[string]string // client id -> op id (the newest)
}

func openRecordStore(path string) (*recordStore, error) {
	rs := &recordStore{path: path, ops: map[string]*recOp{}, byClient: map[string]string{}}
	err := loadLines(path, false, func(b []byte) error {
		var op recOp
		if err := json.Unmarshal(b, &op); err != nil {
			return err
		}
		if op.OpID == "" {
			return errors.New("record op without an id")
		}
		rs.apply(op)
		return nil
	})
	return rs, err
}

func (rs *recordStore) apply(op recOp) {
	if _, ok := rs.ops[op.OpID]; !ok {
		rs.order = append(rs.order, op.OpID)
	}
	cp := op
	rs.ops[op.OpID] = &cp
	if op.ClientID != "" {
		rs.byClient[op.ClientID] = op.OpID
	}
}

// put appends the op's current state (fsynced) and applies it.
func (rs *recordStore) put(op recOp) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	f, err := openAppend(rs.path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	rs.apply(op)
	return nil
}

func (rs *recordStore) get(opID string) (recOp, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	op, ok := rs.ops[opID]
	if !ok {
		return recOp{}, false
	}
	return *op, true
}

func (rs *recordStore) client(clientID string) (recOp, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	id, ok := rs.byClient[clientID]
	if !ok {
		return recOp{}, false
	}
	return *rs.ops[id], true
}

func (rs *recordStore) all() []recOp {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]recOp, 0, len(rs.order))
	for _, id := range rs.order {
		out = append(out, *rs.ops[id])
	}
	return out
}

// ---- validation ----

func recVar(typ string) string {
	switch typ {
	case recBP:
		return "bp"
	case recSymptom:
		return "symptom"
	}
	return "body"
}

func intIn(v any, lo, hi float64) (float64, bool) {
	f, ok := numFrom(v)
	if !ok || f != math.Trunc(f) || f < lo || f > hi {
		return 0, false
	}
	return f, true
}

// cleanRecordData validates the data of a record against the bounds of 18.4
// (input validation only, never advice) and returns the type's keys.
func cleanRecordData(typ string, d map[string]any) (map[string]any, string) {
	allowed := map[string][]string{
		recBP:      {"systolic_mmhg", "diastolic_mmhg", "pulse_bpm", "context", "note"},
		recSymptom: {"category", "present", "note"},
		recWaist:   {"waist_cm", "note"},
	}[typ]
	if allowed == nil {
		return nil, "no such record type"
	}
	for k := range d {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return nil, "unknown data key " + k
		}
	}
	out := map[string]any{}
	note := ""
	if n, has := d["note"]; has && n != nil {
		s, ok := n.(string)
		if !ok || utf8.RuneCountInString(s) > 500 {
			return nil, "note must be text of at most 500 characters"
		}
		note = strings.TrimSpace(s)
	}
	out["note"] = note
	switch typ {
	case recBP:
		sys, ok1 := intIn(d["systolic_mmhg"], 60, 260)
		dia, ok2 := intIn(d["diastolic_mmhg"], 30, 160)
		if !ok1 || !ok2 || sys <= dia {
			return nil, "systolic_mmhg (60 to 260) and diastolic_mmhg (30 to 160) must be integers, systolic above diastolic"
		}
		out["systolic_mmhg"], out["diastolic_mmhg"] = sys, dia
		out["pulse_bpm"] = nil
		if p, has := d["pulse_bpm"]; has && p != nil {
			v, ok := intIn(p, 25, 220)
			if !ok {
				return nil, "pulse_bpm must be an integer 25 to 220 or null"
			}
			out["pulse_bpm"] = v
		}
		c, _ := d["context"].(string)
		if c != "home" && c != "office" && c != "ambulatory" {
			return nil, "context must be home, office or ambulatory"
		}
		out["context"] = c
	case recSymptom:
		c, _ := d["category"].(string)
		p, isBool := d["present"].(bool)
		if !isSymptomCategory(c) || !isBool {
			return nil, "category must be one of the list and present must be true or false"
		}
		if (c == "none") == p {
			return nil, `category "none" needs present false; every other category needs present true`
		}
		out["category"], out["present"] = c, p
	case recWaist:
		w, ok := numFrom(d["waist_cm"])
		if !ok || w < 40 || w > 150 {
			return nil, "waist_cm must be 40 to 150"
		}
		out["waist_cm"] = round1(w)
	}
	return out, ""
}

// recordFromRow reads a record row of any writer: it needs `type` and
// `measured_at` and must pass the bounds, else it is ignored.
func recordFromRow(v Value, wantTypes map[string]bool, loc *time.Location) (Record, bool) {
	d := v.Data
	typ, _ := d["type"].(string)
	if d == nil || !wantTypes[typ] {
		return Record{}, false
	}
	ms, _ := d["measured_at"].(string)
	at, err := time.Parse(time.RFC3339, ms)
	if err != nil {
		return Record{}, false
	}
	in := map[string]any{}
	for _, k := range []string{"systolic_mmhg", "diastolic_mmhg", "pulse_bpm", "context", "category", "present", "waist_cm", "note"} {
		if x, ok := d[k]; ok {
			in[k] = x
		}
	}
	clean, bad := cleanRecordData(typ, in)
	if bad != "" {
		return Record{}, false
	}
	id, _ := d["record_id"].(string)
	if id == "" {
		id = "v:" + v.ID
	}
	src, _ := d["source"].(string)
	if src == "" {
		src = "other"
	}
	vid := v.ID
	return Record{RecordID: id, Type: typ, MeasuredAt: ms, Data: clean, ValueID: &vid, Source: src, date: at.In(loc).Format("2006-01-02"), at: at}, true
}

// ---- the view ----

// recEntry is one record with the state of its own write and of its void.
type recEntry struct {
	rec    Record
	status string // done | pending_reconciliation | failed
	void   string // "" | done | pending_reconciliation | failed
}

func (e recEntry) visible() bool {
	return e.status != StatusFailed && (e.void == "" || e.void == StatusFailed)
}

// recordsView is every record as the routes and the snapshot see it, built
// from the cached rows (the truth) and the own operations.
type recordsView struct {
	entries map[string]*recEntry
	bpOK    bool // the variable resolved
	symOK   bool
	bpSet   bool // the config key is set
	symSet  bool
}

func opStatus(state string) string {
	switch state {
	case OpDone:
		return StatusDone
	case OpFailed:
		return StatusFailed
	}
	return StatusPending
}

// buildRecordsView reduces rows and ops. rows holds the cached rows of the
// record variables by var ("bp", "symptom", "body").
func buildRecordsView(rows map[string][]Value, ops []recOp, loc *time.Location, readAt time.Time) *recordsView {
	v := &recordsView{entries: map[string]*recEntry{}}
	// Rows are the truth. A DONE operation whose row is not in the cached
	// rows counts only while its write is newer than the last full read (the
	// read may not hold it yet). After a read that came later, a missing row
	// was deleted outside: the record, or the void, is gone with it.
	gone := func(op recOp) bool {
		return op.State == OpDone && !readAt.IsZero() && op.LastTry.Before(readAt.Add(-60*time.Second))
	}
	types := map[string]map[string]bool{"bp": {recBP: true}, "symptom": {recSymptom: true}, "body": {recWaist: true}}
	rowOp := map[string]string{} // op id -> value id of its row
	voidRow := map[string]bool{} // record id -> a void row exists
	for vr, rs := range rows {
		for _, r := range dedupeRows(rs) {
			if op, _ := r.Data["op_id"].(string); op != "" {
				rowOp[op] = r.ID
			}
			if target, _ := r.Data["voids"].(string); target != "" {
				if typ, _ := r.Data["type"].(string); types[vr][typ] {
					voidRow[target] = true
				}
				continue
			}
			if rec, ok := recordFromRow(r, types[vr], loc); ok {
				v.entries[rec.RecordID] = &recEntry{rec: rec, status: StatusDone}
			}
		}
	}
	for _, op := range ops {
		if op.Kind != "record" {
			continue
		}
		if _, found := rowOp[op.OpID]; found {
			continue // the row is the truth, whatever the op state says
		}
		if gone(op) {
			continue
		}
		rec, ok := recordFromPayload(op, loc)
		if !ok {
			continue
		}
		st := opStatus(op.State)
		if op.State == OpDone {
			// Done and not yet in the cached rows (the write is newer than
			// the last full read): shown from the op.
			vid := op.ValueID
			rec.ValueID = &vid
		} else {
			rec.Pending = st == StatusPending
		}
		v.entries[rec.RecordID] = &recEntry{rec: rec, status: st}
	}
	for id := range voidRow {
		if e := v.entries[id]; e != nil {
			e.void = StatusDone
		}
	}
	for _, op := range ops {
		if op.Kind != "void" {
			continue
		}
		e := v.entries[op.Voids]
		if e == nil || e.void == StatusDone {
			continue
		}
		if _, found := rowOp[op.OpID]; found {
			e.void = StatusDone
			continue
		}
		if gone(op) {
			continue // the void row was deleted outside: the record is visible again
		}
		e.void = opStatus(op.State)
	}
	return v
}

func recordFromPayload(op recOp, loc *time.Location) (Record, bool) {
	ms, _ := op.Payload["measured_at"].(string)
	at, err := time.Parse(time.RFC3339, ms)
	if err != nil {
		return Record{}, false
	}
	in := map[string]any{}
	for _, k := range []string{"systolic_mmhg", "diastolic_mmhg", "pulse_bpm", "context", "category", "present", "waist_cm", "note"} {
		if x, ok := op.Payload[k]; ok {
			in[k] = x
		}
	}
	clean, bad := cleanRecordData(op.Type, in)
	if bad != "" {
		return Record{}, false
	}
	return Record{RecordID: op.RecordID, Type: op.Type, MeasuredAt: ms, Data: clean, Source: "fuel", date: at.In(loc).Format("2006-01-02"), at: at}, true
}

// list returns the visible records of a type, newest first.
func (v *recordsView) list(typ, from, to string) []Record {
	out := []Record{}
	for _, e := range v.entries {
		if e.rec.Type != typ || !e.visible() || (from != "" && e.rec.date < from) || (to != "" && e.rec.date > to) {
			continue
		}
		out = append(out, e.rec)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].at.Equal(out[b].at) {
			return out[a].at.After(out[b].at)
		}
		return out[a].RecordID > out[b].RecordID
	})
	return out
}

// fill writes the record parts of a snapshot: views only.
func (v *recordsView) fill(s *Snapshot, date string, t *Targets, cl ClinicianCfg) {
	if v.bpOK {
		bp := &BPSnap{}
		recs := v.list(recBP, "", date)
		if len(recs) > 0 {
			r := recs[0]
			bp.Latest = &r
		}
		from := dateAdd(date, -6)
		for _, r := range recs {
			if r.date >= from {
				bp.Count7d++
			}
		}
		bp.Clinician.Home, bp.Clinician.Office, bp.Clinician.Ambulatory = cl.BPHome, cl.BPOffice, cl.BPAmbulatory
		bp.Info.Home, bp.Info.Office, bp.Info.Ambulatory = t.info("bp-home"), t.info("bp-office"), t.info("bp-ambulatory")
		s.Records.BloodPressure = bp
	} else if v.bpSet {
		s.Missing = append(s.Missing, recBP)
	}
	if v.symOK {
		sy := &SymptomSnap{Info: t.info("symptoms")}
		recs := v.list(recSymptom, "", date)
		if len(recs) > 0 {
			d := recs[0].date
			sy.LatestDate = &d
		}
		mon := mondayOf(date)
		sun := dateAdd(mon, 6)
		for _, r := range v.list(recSymptom, mon, sun) {
			_ = r
			sy.EntryThisWeek = true
		}
		s.Records.Symptom = sy
	} else if v.symSet {
		s.Missing = append(s.Missing, recSymptom)
	}
	w := v.list(recWaist, "", date)
	if len(w) > 0 {
		cm, _ := numFrom(w[0].Data["waist_cm"])
		d := w[0].date
		s.Progress.Waist.LatestCm, s.Progress.Waist.LatestDate = &cm, &d
	}
	if len(w) > 1 {
		cm, _ := numFrom(w[1].Data["waist_cm"])
		d := w[1].date
		s.Progress.Waist.PreviousCm, s.Progress.Waist.PreviousDate = &cm, &d
	}
}

// ---- service ----

// recordsViewNow builds the view from the cache and the store.
func (s *Service) recordsViewNow() *recordsView {
	t, _ := s.loadTargets()
	loc := time.UTC
	if t != nil {
		loc = t.loc
	}
	rows := s.cache.RecordRows()
	v := buildRecordsView(rows, s.records.all(), loc, s.cache.RecordsReadAt())
	s.mu.Lock()
	v.bpOK, v.symOK = s.ids.BP != "", s.ids.Symptom != ""
	s.mu.Unlock()
	v.bpSet, v.symSet = s.o.BPVar != "", s.o.SymptomVar != ""
	return v
}

func (s *Service) recVarID(vr string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch vr {
	case "bp":
		return s.ids.BP
	case "symptom":
		return s.ids.Symptom
	}
	return s.ids.Body
}

func hashOf(parts ...any) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// postRecordOp performs one POST attempt of a record operation and stores
// the outcome: done on 201, failed on a definitive rejection of the FIRST
// attempt, uncertain otherwise (never blindly re-posted: the reconciler
// reads first).
func (s *Service) postRecordOp(ctx context.Context, op recOp) recOp {
	op.Attempts++
	op.LastTry = s.o.Now()
	if err := s.records.put(op); err != nil {
		log.Printf("fuel: record op %s: could not journal the attempt; not posting", op.OpID)
		return op
	}
	pctx, cancel := context.WithTimeout(ctx, s.o.VarTimeout)
	vid, err := s.o.Vars.Post(pctx, s.recVarID(op.Var), op.Payload, op.RecordDate)
	cancel()
	var pe *PostError
	switch {
	case err == nil:
		op.State, op.ValueID = OpDone, vid
		s.cache.mergeRecord(op.Var, Value{ID: vid, VariableID: s.recVarID(op.Var), RecordDate: op.RecordDate, CreatedAt: s.o.Now(), Data: op.Payload})
	case errors.As(err, &pe) && op.Attempts == 1:
		log.Printf("fuel: record op %s rejected by Variables (HTTP %d)", op.OpID, pe.Status)
		op.State = OpFailed
	default:
		log.Printf("fuel: record op %s uncertain after attempt %d (%s)", op.OpID, op.Attempts, errClass(err))
		op.State = OpUncertain
	}
	op.LastTry = s.o.Now()
	if err := s.records.put(op); err != nil {
		log.Printf("fuel: record op %s: could not journal the outcome", op.OpID)
	}
	return op
}

// reconcileRecords finishes the record operations that are not terminal:
// re-read the record date and look for the op_id before any re-post (the
// same op_id, at most 3 attempts 60 s apart); not done after 24 h = failed.
func (s *Service) reconcileRecords(ctx context.Context) {
	now := s.o.Now()
	for _, op := range s.records.all() {
		if terminal(op.State) || now.Sub(op.LastTry) < s.o.RecheckAfter {
			continue
		}
		if s.recVarID(op.Var) == "" {
			continue // the variable is gone: nothing can be posted or found
		}
		s.recMu.Lock()
		cur, ok := s.records.get(op.OpID)
		if !ok || terminal(cur.State) {
			s.recMu.Unlock()
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, s.o.VarTimeout)
		vals, err := s.o.Vars.ByDate(rctx, cur.RecordDate)
		cancel()
		if err != nil {
			if now.Sub(cur.At) >= s.o.FailAfter {
				cur.State, cur.LastTry = OpFailed, now
				_ = s.records.put(cur)
			}
			s.recMu.Unlock()
			continue
		}
		found := false
		for _, v := range vals {
			if id, _ := v.Data["op_id"].(string); v.VariableID == s.recVarID(cur.Var) && id == cur.OpID {
				cur.State, cur.ValueID, cur.LastTry = OpDone, v.ID, now
				_ = s.records.put(cur)
				s.cache.mergeRecord(cur.Var, v)
				log.Printf("fuel: record op %s reconciled: found as value %s", cur.OpID, v.ID)
				found = true
				break
			}
		}
		switch {
		case found:
		case now.Sub(cur.At) >= s.o.FailAfter:
			cur.State, cur.LastTry = OpFailed, now
			_ = s.records.put(cur)
		case cur.Attempts >= 3:
			cur.LastTry = now // wait for a late write to show up, until FailAfter
			_ = s.records.put(cur)
		default:
			s.postRecordOp(context.WithoutCancel(ctx), cur)
		}
		s.recMu.Unlock()
	}
}

// adoptRecordRows makes the rows the truth: an operation that is not done
// (also one in state failed) whose row is in the cached rows becomes done
// with the value id of that row.
func (s *Service) adoptRecordRows() {
	rows := s.cache.RecordRows()
	byOp := map[string]Value{}
	for _, rs := range rows {
		for _, r := range rs {
			if id, _ := r.Data["op_id"].(string); id != "" {
				if _, dup := byOp[id]; !dup {
					byOp[id] = r
				}
			}
		}
	}
	for _, op := range s.records.all() {
		if op.State == OpDone {
			continue
		}
		if r, ok := byOp[op.OpID]; ok {
			op.State, op.ValueID, op.LastTry = OpDone, r.ID, s.o.Now()
			if err := s.records.put(op); err == nil {
				log.Printf("fuel: record op %s: its row was found (value %s); done", op.OpID, r.ID)
			}
		}
	}
}

type recordResponse struct {
	Status   string        `json:"status"`
	Record   *Record       `json:"record,omitempty"`
	RecordID string        `json:"record_id,omitempty"`
	Review   **Instruction `json:"review,omitempty"`
	Snapshot *Snapshot     `json:"snapshot,omitempty"`
}

// recordAnswer writes the answer of a record or void operation by its
// current state.
func (s *Service) recordAnswer(w http.ResponseWriter, op recOp) {
	switch opStatus(op.State) {
	case StatusFailed:
		writeErr(w, errf(http.StatusBadGateway, "upstream_failed", false, "Variables rejected the record; nothing was stored"))
		return
	case StatusPending:
		writeJSON(w, http.StatusAccepted, recordResponse{Status: StatusPending, RecordID: recordIDOf(op)})
		return
	}
	snap, err := s.snapshotFor(s.today())
	if err != nil {
		writeErr(w, errf(http.StatusServiceUnavailable, "targets_invalid", false, "targets invalid; the record itself was saved"))
		return
	}
	resp := recordResponse{Status: StatusDone, Snapshot: &snap}
	if op.Kind == "void" {
		resp.RecordID = op.Voids
		writeJSON(w, http.StatusOK, resp)
		return
	}
	v := s.recordsViewNow()
	if e := v.entries[op.RecordID]; e != nil {
		r := e.rec
		resp.Record = &r
	}
	if op.Type == recSymptom {
		var review *Instruction
		if t, _ := s.loadTargets(); t != nil && t.V2 != nil {
			cat, _ := op.Payload["category"].(string)
			for _, r := range t.V2.Clinician.SymptomRules {
				if r.Category == cat {
					in := r.Instruction
					review = &in
				}
			}
		}
		resp.Review = &review
	}
	writeJSON(w, http.StatusOK, resp)
}

func recordIDOf(op recOp) string {
	if op.Kind == "void" {
		return op.Voids
	}
	return op.RecordID
}

func (s *Service) recordIdem(w http.ResponseWriter, clientID, hash string) bool {
	prev, ok := s.records.client(clientID)
	if !ok || s.o.Now().Sub(prev.At) > idemRetention {
		return false
	}
	if prev.RequestHash != hash {
		writeErr(w, errf(http.StatusConflict, "idempotency_conflict", false, "client_id was used for a different request"))
		return true
	}
	s.recordAnswer(w, prev)
	return true
}

func (s *Service) readJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.o.ReadDeadline))
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	err := decodeStrict(r.Body, v)
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		if maxBytesErr(err) {
			writeErr(w, errf(http.StatusRequestEntityTooLarge, "too_large", false, "body too large"))
		} else {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "bad JSON body"))
		}
		return false
	}
	return true
}

// handleRecord serves POST /fuel/record. No model call.
func (s *Service) handleRecord(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID   string         `json:"client_id"`
		Type       string         `json:"type"`
		MeasuredAt string         `json:"measured_at"`
		Data       map[string]any `json:"data"`
	}
	if !s.readJSONBody(w, r, &body) {
		return
	}
	if !clientIDRe.MatchString(body.ClientID) {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id (a uuid) is required"))
		return
	}
	clean, bad := cleanRecordData(body.Type, body.Data)
	if bad != "" {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "%s", bad))
		return
	}
	t, _ := s.loadTargets()
	now := s.o.Now()
	at := now
	if body.MeasuredAt != "" {
		p, err := time.Parse(time.RFC3339, body.MeasuredAt)
		if err != nil || p.Sub(now) > time.Minute || now.Sub(p) > 34*24*time.Hour {
			writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "measured_at must be RFC3339, not in the future and not older than 34 days"))
			return
		}
		at = p
	}
	vr := recVar(body.Type)
	if s.recVarID(vr) == "" {
		writeErr(w, errf(http.StatusServiceUnavailable, "variable_missing", false, "the variable for this record type is not set up"))
		return
	}
	if body.Type == recWaist && s.o.TestMode && strings.EqualFold(strings.TrimSpace(s.o.BodyVar), prodBodyVar) {
		writeErr(w, errf(http.StatusConflict, "test_mode_read_only", false, "the test instance reads Body composition and never writes it"))
		return
	}
	hash := hashOf("record", body.Type, clean, body.MeasuredAt)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if s.recordIdem(w, body.ClientID, hash) {
		return
	}
	measured := at.In(t.loc).Format(time.RFC3339)
	op := recOp{OpID: newID("op_"), RecordID: newID("rc_"), Type: body.Type, Var: vr, Kind: "record",
		RecordDate: at.In(t.loc).Format("2006-01-02"), ClientID: body.ClientID, RequestHash: hash, State: OpPending, At: now}
	p := map[string]any{"source": "fuel", "op_id": op.OpID, "record_id": op.RecordID, "type": body.Type, "measured_at": measured}
	for k, v := range clean {
		if v != nil {
			p[k] = v // a null optional key is omitted from the row
		}
	}
	if body.Type == recWaist {
		p["method"] = "tape" // no weight_kg key: the weight readers skip the row
	}
	op.Payload = p
	if err := s.records.put(op); err != nil {
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		return
	}
	op = s.postRecordOp(ctx, op)
	log.Printf("fuel: record type=%s status=%s", body.Type, opStatus(op.State))
	s.recordAnswer(w, op)
}

// handleRecordVoid serves POST /fuel/record/void: a void is a new row; a
// record is never edited.
func (s *Service) handleRecordVoid(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"client_id"`
		RecordID string `json:"record_id"`
	}
	if !s.readJSONBody(w, r, &body) {
		return
	}
	if !clientIDRe.MatchString(body.ClientID) || body.RecordID == "" || len(body.RecordID) > 100 {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "client_id and record_id are required"))
		return
	}
	hash := hashOf("void", body.RecordID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.o.Budget)
	defer cancel()
	s.recMu.Lock() // one lock: a concurrent second void waits for the first
	defer s.recMu.Unlock()
	if s.recordIdem(w, body.ClientID, hash) {
		return
	}
	e := s.recordsViewNow().entries[body.RecordID]
	switch {
	case e == nil:
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "unknown record"))
		return
	case e.status != StatusDone:
		writeErr(w, errf(http.StatusConflict, "pending", true, "the record is not saved yet"))
		return
	case e.void == StatusDone || e.void == StatusPending:
		writeErr(w, errf(http.StatusConflict, "already_voided", false, "the record is already voided"))
		return
	}
	vr := recVar(e.rec.Type)
	if s.recVarID(vr) == "" {
		writeErr(w, errf(http.StatusServiceUnavailable, "variable_missing", false, "the variable for this record type is not set up"))
		return
	}
	if e.rec.Type == recWaist && s.o.TestMode && strings.EqualFold(strings.TrimSpace(s.o.BodyVar), prodBodyVar) {
		writeErr(w, errf(http.StatusConflict, "test_mode_read_only", false, "the test instance reads Body composition and never writes it"))
		return
	}
	now := s.o.Now()
	// A deterministic op_id: a new void after a failed one writes a row that
	// counts once with the earlier attempt.
	op := recOp{OpID: "op_void_" + body.RecordID, RecordID: newID("rc_"), Type: e.rec.Type, Var: vr, Kind: "void", Voids: body.RecordID,
		RecordDate: e.rec.date, ClientID: body.ClientID, RequestHash: hash, State: OpPending, At: now}
	op.Payload = map[string]any{"type": e.rec.Type, "voids": body.RecordID, "source": "fuel", "op_id": op.OpID, "record_id": op.RecordID,
		"measured_at": e.rec.MeasuredAt, "voided_at": now.Format(time.RFC3339)}
	if e.rec.Type == recWaist {
		// The Body composition variable has a schema that requires `method`
		// (and measured_at) on every row, a void row included.
		op.Payload["method"] = "tape"
	}
	if err := s.records.put(op); err != nil {
		writeErr(w, errf(http.StatusInternalServerError, "internal", true, "could not persist the request; nothing was written"))
		return
	}
	op = s.postRecordOp(ctx, op)
	log.Printf("fuel: record void type=%s status=%s", e.rec.Type, opStatus(op.State))
	s.recordAnswer(w, op)
}

// handleRecordGet serves GET /fuel/record/{record_id}.
func (s *Service) handleRecordGet(w http.ResponseWriter, r *http.Request) {
	e := s.recordsViewNow().entries[r.PathValue("id")]
	if e == nil {
		writeErr(w, errf(http.StatusNotFound, "not_found", false, "unknown record"))
		return
	}
	var void any
	if e.void != "" {
		void = map[string]string{"status": e.void}
	}
	code := http.StatusOK
	writeJSON(w, code, map[string]any{"status": e.status, "void": void, "record": e.rec})
}

// handleRecords serves GET /fuel/records?type=&from=&to=.
func (s *Service) handleRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ := q.Get("type")
	if typ != recBP && typ != recSymptom && typ != recWaist {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "type must be blood_pressure, symptom or waist"))
		return
	}
	today := s.today()
	from, to := q.Get("from"), q.Get("to")
	if to == "" {
		to = today
	}
	if from == "" {
		from = dateAdd(to, -89)
	}
	if !validDate(from) || !validDate(to) || from > to || daysBetween(from, to) > 179 {
		writeErr(w, errf(http.StatusBadRequest, "bad_input", false, "from and to must be dates, from <= to, at most 180 days"))
		return
	}
	if s.recVarID(recVar(typ)) == "" {
		writeErr(w, errf(http.StatusServiceUnavailable, "variable_missing", false, "the variable for this record type is not set up"))
		return
	}
	t, _ := s.loadTargets()
	var cl ClinicianCfg
	if t.V2 != nil {
		cl = t.V2.Clinician
	}
	var clinician any
	info := ""
	switch typ {
	case recBP:
		clinician = map[string]*Instruction{"home": cl.BPHome, "office": cl.BPOffice, "ambulatory": cl.BPAmbulatory}
		info = t.info("bp-home")
	case recSymptom:
		rules := cl.SymptomRules
		if rules == nil {
			rules = []SymptomRule{}
		}
		clinician = map[string]any{"rules": rules}
		info = t.info("symptoms")
	default:
		info = t.info("waist")
	}
	writeJSON(w, http.StatusOK, map[string]any{"type": typ, "records": s.recordsViewNow().list(typ, from, to), "clinician": clinician, "info": info})
}
