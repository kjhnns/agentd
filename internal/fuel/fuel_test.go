package fuel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/media"
)

// ---- loopback-only transport (spec 2: tests never reach a real host) ----

type loopbackOnly struct{ rt http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("test transport refuses host %q", r.URL.Host)
	}
	return l.rt.RoundTrip(r)
}

func loopbackClient() *http.Client {
	return &http.Client{Transport: loopbackOnly{http.DefaultTransport}, Timeout: 10 * time.Second}
}

func TestLoopbackTransportRefusesOtherHosts(t *testing.T) {
	_, err := loopbackClient().Get("https://app.logvariables.com/api/ext/variables")
	if err == nil || !strings.Contains(err.Error(), "refuses host") {
		t.Fatalf("want refusal, got %v", err)
	}
}

// ---- fake Variables (append-only, value ids, ?date=) ----

type fakeValue struct {
	ID, VariableID, Data, RecordDate string
	CreatedAt                        time.Time
}

type fakeVars struct {
	mu        sync.Mutex
	vals      []fakeValue
	n         int
	posts     int
	vars      []VarInfo
	onPost    func(n int, data map[string]any) (store bool, status int) // nil = store, 201
	failReads atomic.Bool
	now       func() time.Time
}

func newFakeVars() *fakeVars {
	return &fakeVars{vars: []VarInfo{
		{ID: "var-food", Name: "Fuel e2e food log", Type: "json"},
		{ID: "var-prod", Name: "Food log", Type: "json"},
		{ID: "var-body", Name: "Body composition", Type: "json"},
		{ID: "var-push", Name: "Push ups", Type: "numeric"},
	}}
}

func (f *fakeVars) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer vkey" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/variables":
		_ = json.NewEncoder(w).Encode(f.vars)
	case r.Method == "POST" && r.URL.Path == "/values":
		var body struct{ VariableID, Data, RecordDate string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		var data map[string]any
		if err := json.Unmarshal([]byte(body.Data), &data); err != nil {
			http.Error(w, "data is not a JSON object", 400)
			return
		}
		if _, ok := data["kcal"]; !ok && body.VariableID != "var-push" {
			http.Error(w, `data is missing required key "kcal"`, 400)
			return
		}
		f.mu.Lock()
		f.posts++
		n := f.posts
		hook := f.onPost
		f.mu.Unlock()
		store, status := true, 201
		if hook != nil {
			store, status = hook(n, data)
		}
		var id string
		if store {
			f.mu.Lock()
			f.n++
			id = fmt.Sprintf("val-%d", f.n)
			ts := time.Now().UTC()
			if f.now != nil {
				ts = f.now()
			}
			f.vals = append(f.vals, fakeValue{ID: id, VariableID: body.VariableID, Data: body.Data, RecordDate: body.RecordDate, CreatedAt: ts.Add(time.Duration(f.n) * time.Millisecond)})
			f.mu.Unlock()
		}
		if status >= 400 {
			http.Error(w, "injected failure", status)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == "GET" && r.URL.Path == "/values":
		if f.failReads.Load() {
			http.Error(w, "down", 503)
			return
		}
		date := r.URL.Query().Get("date")
		f.mu.Lock()
		out := []map[string]any{}
		for _, v := range f.vals {
			if date == "" || v.RecordDate == date {
				out = append(out, map[string]any{"id": v.ID, "variableId": v.VariableID, "data": v.Data, "recordDate": v.RecordDate, "createdAt": v.CreatedAt.Format(time.RFC3339Nano)})
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeVars) add(varID, date string, data any) {
	b, _ := json.Marshal(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.vals = append(f.vals, fakeValue{ID: fmt.Sprintf("val-%d", f.n), VariableID: varID, Data: string(b), RecordDate: date, CreatedAt: time.Now().UTC()})
}

// edit replaces fields of a stored value (an external edit), or deletes it.
func (f *fakeVars) edit(id string, patch map[string]any, del bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range f.vals {
		if v.ID != id {
			continue
		}
		if del {
			f.vals = append(f.vals[:i], f.vals[i+1:]...)
			return
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(v.Data), &m)
		for k, x := range patch {
			m[k] = x
		}
		b, _ := json.Marshal(m)
		f.vals[i].Data = string(b)
		return
	}
}

func (f *fakeVars) rows(varID string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, v := range f.vals {
		if v.VariableID == varID {
			var m map[string]any
			_ = json.Unmarshal([]byte(v.Data), &m)
			m["_record_date"] = v.RecordDate
			m["_id"] = v.ID
			out = append(out, m)
		}
	}
	return out
}

// ---- fake model and ASR ----

type fakeModel struct {
	mu    sync.Mutex
	fn    func(in ModelInput) string
	delay time.Duration
	calls int
}

func (m *fakeModel) Estimate(ctx context.Context, in ModelInput) (json.RawMessage, error) {
	m.mu.Lock()
	m.calls++
	fn, d := m.fn, m.delay
	m.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return json.RawMessage(fn(in)), nil
}

type fakeASR struct{ text string }

func (a fakeASR) Transcribe(ctx context.Context, path string) (media.Transcript, error) {
	return media.Transcript{Text: a.text}, nil
}

const skyrWalnuts = `{"intent":"log","items":[
 {"item":"skyr","staple_key":null,"portion_g":250,"portion_basis":"stated","kcal":157.5,"protein_g":27.5,"carbs_g":10,"net_carbs_g":null,"fat_g":0.5,"sat_fat_g":0.3,"fiber_g":0,"needs_fraction":false},
 {"item":"walnuts","staple_key":null,"portion_g":30,"portion_basis":"stated","kcal":196,"protein_g":4.6,"carbs_g":4.1,"net_carbs_g":2.1,"fat_g":19.6,"sat_fat_g":1.8,"fiber_g":2,"needs_fraction":false}],
 "text":"Good protein with 2 sources.","widgets":["macros_today"]}`

const pizzaPhoto = `{"intent":"log","items":[
 {"item":"pizza margherita","staple_key":null,"portion_g":null,"portion_basis":"photo_estimate","kcal":801,"protein_g":33.3,"carbs_g":99.9,"net_carbs_g":null,"fat_g":29.9,"sat_fat_g":13.3,"fiber_g":null,"needs_fraction":true}],
 "text":"A classic.","widgets":["macros_today"]}`

// ---- harness ----

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type harness struct {
	t     *testing.T
	svc   *Service
	vars  *fakeVars
	model *fakeModel
	clk   *clock
	h     http.Handler
	dir   string
	opts  Options
}

const testToken = "tok-0123456789abcdef"

func newHarness(t *testing.T, mut ...func(*Options)) *harness {
	t.Helper()
	fv := newFakeVars()
	srv := httptest.NewServer(fv)
	t.Cleanup(srv.Close)
	// 12:00 in Zurich on 2026-10-01.
	clk := &clock{t: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)}
	fv.now = clk.Now
	dir := t.TempDir()
	fm := &fakeModel{fn: func(ModelInput) string { return skyrWalnuts }}
	o := Options{
		Token:        testToken,
		Vars:         &VariablesHTTP{Base: srv.URL, Key: "vkey", Client: loopbackClient()},
		FoodVar:      "Fuel e2e food log",
		BodyVar:      "Body composition",
		Model:        fm,
		ASR:          fakeASR{text: "two eggs"},
		TargetsFile:  filepath.Join(dir, "targets.json"),
		StaplesFile:  filepath.Join(dir, "staples.json"),
		StateDir:     filepath.Join(dir, "state"),
		StravaDir:    filepath.Join(dir, "strava"),
		TestMode:     true,
		Now:          clk.Now,
		Budget:       3 * time.Second,
		ReadDeadline: 5 * time.Second,
		BusyWait:     200 * time.Millisecond,
	}
	for _, m := range mut {
		m(&o)
	}
	if o.LogBudget == 0 {
		o.LogBudget = o.Budget // the tests' budget is the log budget too
	}
	h := &harness{t: t, vars: fv, model: fm, clk: clk, dir: dir, opts: o}
	h.start()
	return h
}

func (h *harness) start() {
	h.t.Helper()
	svc, err := New(h.opts)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := svc.Resolve(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	svc.Start(context.Background())
	h.svc = svc
	h.h = svc.Handler()
	h.t.Cleanup(svc.Close)
}

// restart reopens the service on the same state dir (journal replay).
func (h *harness) restart() {
	h.svc.Close()
	h.start()
}

func (h *harness) do(method, path string, body io.Reader, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

func (h *harness) logText(clientID, text string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"client_id": clientID, "text": text})
	return h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json")
}

func (h *harness) mutate(kind, clientID, itemID string, f *float64) *httptest.ResponseRecorder {
	m := map[string]any{"client_id": clientID, "item_id": itemID}
	if f != nil {
		m["fraction"] = *f
	}
	b, _ := json.Marshal(m)
	return h.do("POST", "/fuel/"+kind, bytes.NewReader(b), "application/json")
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body.String(), err)
	}
	return v
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

func macro(s Snapshot, key string) MacroState {
	for _, m := range s.Macros {
		if m.Key == key {
			return m
		}
	}
	return MacroState{}
}

func f64(v float64) *float64 { return &v }

// ---- tests ----

func TestTextLogWritesRowsWithOpIDs(t *testing.T) {
	h := newHarness(t)
	rec := h.logText("11111111-aaaa", "250 g skyr and 30 g walnuts")
	if rec.Code != 200 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
	resp := decode[LogResponse](t, rec)
	if resp.Status != "done" || resp.Intent != "log" || len(resp.Items) != 2 {
		t.Fatalf("resp %+v", resp)
	}
	rows := h.vars.rows("var-food")
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	for _, r := range rows {
		if r["op_id"] == nil || r["source"] != "fuel" || r["entry_id"] != resp.EntryID || r["item_id"] == nil {
			t.Fatalf("row %v", r)
		}
		if r["_record_date"] != "2026-10-01" {
			t.Fatalf("record date %v", r["_record_date"])
		}
	}
	// net_carbs normalized on the original: skyr carbs 10 - fibre 0.
	for _, r := range rows {
		if r["item"] == "skyr" && r["net_carbs_g"] != 10.0 {
			t.Fatalf("net carbs %v", r["net_carbs_g"])
		}
	}
	if got := macro(resp.Snapshot, "protein_g").Consumed; got != 32.1 {
		t.Fatalf("protein %v", got)
	}
	if resp.Snapshot.Revision != 2 {
		t.Fatalf("revision %d", resp.Snapshot.Revision)
	}
	// Digits are stripped from the model text; the status sentence leads.
	if !strings.HasPrefix(resp.Blocks[0].Text, "Protein 32.1 of 160 g") {
		t.Fatalf("status block %q", resp.Blocks[0].Text)
	}
	if strings.ContainsAny(resp.Blocks[1].Text, "0123456789") {
		t.Fatalf("digits in model text %q", resp.Blocks[1].Text)
	}
	var sawWidget bool
	for _, b := range resp.Blocks {
		if b.Widget == "macros_today" {
			sawWidget = b.Date == "2026-10-01" && b.Revision != nil && *b.Revision == 2
		}
	}
	if !sawWidget {
		t.Fatal("macros_today widget missing date/revision")
	}
}

func TestIdempotentRepeatAnd409OnChangedPayload(t *testing.T) {
	h := newHarness(t)
	a := h.logText("22222222-bbbb", "250 g skyr and 30 g walnuts")
	b := h.logText("22222222-bbbb", "250 g skyr and 30 g walnuts")
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("codes %d %d", a.Code, b.Code)
	}
	ra, rb := decode[LogResponse](t, a), decode[LogResponse](t, b)
	if ra.EntryID != rb.EntryID {
		t.Fatal("repeat made a new entry")
	}
	if n := len(h.vars.rows("var-food")); n != 2 {
		t.Fatalf("rows %d after repeat", n)
	}
	if h.model.calls != 1 {
		t.Fatalf("model calls %d", h.model.calls)
	}
	c := h.logText("22222222-bbbb", "300 g skyr")
	if c.Code != 409 || errCode(t, c) != "idempotency_conflict" {
		t.Fatalf("changed payload: %d %s", c.Code, c.Body)
	}
}

func TestConcurrentDuplicatesWaitForTheFirst(t *testing.T) {
	h := newHarness(t)
	h.model.delay = 150 * time.Millisecond
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.logText("33333333-cccc", "250 g skyr and 30 g walnuts").Code
		}(i)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 200 {
		t.Fatalf("codes %v", codes)
	}
	if n := len(h.vars.rows("var-food")); n != 2 || h.model.calls != 1 {
		t.Fatalf("rows %d calls %d", n, h.model.calls)
	}
}

func TestUncertainPostReconciledByOpID(t *testing.T) {
	h := newHarness(t)
	// The first POST is stored but answers 500: uncertain.
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) {
		if n == 1 {
			return true, 500
		}
		return true, 201
	}
	rec := h.logText("44444444-dddd", "250 g skyr and 30 g walnuts")
	if rec.Code != 202 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	resp := decode[LogResponse](t, rec)
	if resp.Status != "pending_reconciliation" {
		t.Fatal(resp.Status)
	}
	// Mutations on a pending item are refused.
	var pendingItem string
	for _, it := range resp.Items {
		if it.ValueID == nil {
			pendingItem = it.ItemID
		}
	}
	if r := h.mutate("undo", "55555555-eeee", pendingItem, nil); r.Code != 409 || errCode(t, r) != "pending" {
		t.Fatalf("undo on pending: %d %s", r.Code, r.Body)
	}
	// Before 60 s nothing happens; after, the re-read finds the op_id.
	h.svc.reconcileOnce(context.Background())
	if h.vars.posts != 2 {
		t.Fatalf("posts %d", h.vars.posts)
	}
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	if h.vars.posts != 2 {
		t.Fatalf("re-posted blindly: posts %d", h.vars.posts)
	}
	e := h.do("GET", "/fuel/entry/"+resp.EntryID, nil, "")
	er := decode[entryResponse](t, e)
	if e.Code != 200 || er.Status != "done" {
		t.Fatalf("entry %d %s", e.Code, e.Body)
	}
	if n := len(h.vars.rows("var-food")); n != 2 {
		t.Fatalf("rows %d", n)
	}
}

func TestUncertainPostNotStoredIsRepostedWithSameOpID(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) {
		if n == 1 {
			return false, 503
		}
		return true, 201
	}
	rec := h.logText("66666666-ffff", "250 g skyr and 30 g walnuts")
	if rec.Code != 202 {
		t.Fatalf("code %d", rec.Code)
	}
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	rows := h.vars.rows("var-food")
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	resp := decode[LogResponse](t, rec)
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if e.Status != "done" {
		t.Fatal(e.Status)
	}
	// The re-post carried the SAME op_id the journal holds.
	ops := h.svc.journal.Ops()
	seen := map[string]bool{}
	for _, op := range ops {
		seen[op.ID] = true
	}
	for _, r := range rows {
		if !seen[r["op_id"].(string)] {
			t.Fatalf("row op_id %v not journaled", r["op_id"])
		}
	}
}

func TestDuplicateOpIDCountedOnce(t *testing.T) {
	h := newHarness(t)
	rec := h.logText("77777777-aaaa", "250 g skyr and 30 g walnuts")
	resp := decode[LogResponse](t, rec)
	// A late duplicate of the skyr row lands (same op_id, new value id).
	rows := h.vars.rows("var-food")
	dup := map[string]any{}
	for k, v := range rows[0] {
		if !strings.HasPrefix(k, "_") {
			dup[k] = v
		}
	}
	h.vars.add("var-food", "2026-10-01", dup)
	h.clk.Add(2 * time.Minute)
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if got := macro(snap, "protein_g").Consumed; got != 32.1 {
		t.Fatalf("protein with duplicate %v (want 32.1)", got)
	}
	_ = resp
}

func TestHalveThenUndoEndsAtExactlyZero(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	resp := decode[LogResponse](t, h.logText("88888888-aaaa", "pizza"))
	it := resp.Items[0]
	if !it.NeedsFraction || len(it.Actions) != 2 {
		t.Fatalf("item %+v", it)
	}
	fr := h.mutate("fraction", "88888888-bbbb", it.ItemID, f64(0.5))
	if fr.Code != 200 {
		t.Fatalf("fraction %d %s", fr.Code, fr.Body)
	}
	fresp := decode[MutationResponse](t, fr)
	if fresp.Item.Effective.Kcal.float() != 400.5 || *fresp.Item.Fraction != 0.5 {
		t.Fatalf("after half: %+v", fresp.Item.Effective)
	}
	// Fraction only once.
	if r := h.mutate("fraction", "88888888-cccc", it.ItemID, f64(0.75)); r.Code != 409 || errCode(t, r) != "fraction_already_set" {
		t.Fatalf("second fraction %d %s", r.Code, r.Body)
	}
	un := h.mutate("undo", "88888888-dddd", it.ItemID, nil)
	if un.Code != 200 {
		t.Fatalf("undo %d %s", un.Code, un.Body)
	}
	uresp := decode[MutationResponse](t, un)
	for _, k := range MacroKeys {
		v := uresp.Item.Effective.Get(k)
		if !v.OK || v.V != 0 {
			t.Fatalf("effective %s = %+v after halve+undo", k, v)
		}
	}
	// The rows themselves sum to exactly 0 in every known field.
	sum := map[string]float64{}
	for _, r := range h.vars.rows("var-food") {
		for _, k := range []string{"kcal", "protein_g", "carbs_g", "net_carbs_g", "fat_g", "sat_fat_g"} {
			sum[k] += r[k].(float64)
		}
	}
	for k, v := range sum {
		if toTenth(v) != 0 {
			t.Fatalf("rows sum %s = %v", k, v)
		}
	}
	snap := uresp.Snapshot
	if macro(snap, "kcal").Consumed != 0 || macro(snap, "fiber_g").UnknownRows != 0 {
		t.Fatalf("snapshot after undo: %+v", snap.Macros)
	}
	// Undo twice and fraction after undo are refused.
	if r := h.mutate("undo", "88888888-eeee", it.ItemID, nil); r.Code != 409 || errCode(t, r) != "already_undone" {
		t.Fatalf("undo twice %d %s", r.Code, r.Body)
	}
	// An idempotent repeat of the undo returns the stored response.
	if r := h.mutate("undo", "88888888-dddd", it.ItemID, nil); r.Code != 200 {
		t.Fatalf("undo replay %d", r.Code)
	}
	if n := len(h.vars.rows("var-food")); n != 3 {
		t.Fatalf("rows %d", n)
	}
	// The correction rows carry the contract keys.
	for _, r := range h.vars.rows("var-food")[1:] {
		if r["corrects"] != it.ItemID || r["item_id"] == it.ItemID || !strings.HasPrefix(r["item"].(string), "correction: ") {
			t.Fatalf("correction row %v", r)
		}
		if _, has := r["fiber_g"]; !has || r["fiber_g"] != nil {
			t.Fatalf("fibre must be written as null: %v", r)
		}
		if _, has := r["portion_g"]; has {
			t.Fatalf("null portion_g must be omitted: %v", r)
		}
	}
}

func TestFractionRules(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("99999999-aaaa", "250 g skyr and 30 g walnuts"))
	if r := h.mutate("fraction", "99999999-bbbb", resp.Items[0].ItemID, f64(0.5)); r.Code != 409 || errCode(t, r) != "fraction_not_allowed" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if r := h.mutate("fraction", "99999999-cccc", resp.Items[0].ItemID, f64(0.3)); r.Code != 400 {
		t.Fatalf("bad fraction %d", r.Code)
	}
	if r := h.mutate("undo", "99999999-dddd", "it_nope", nil); r.Code != 404 {
		t.Fatalf("unknown item %d", r.Code)
	}
	// f = 1 writes no row but records the choice.
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("99999999-eeee", "pizza"))
	before := len(h.vars.rows("var-food"))
	r := h.mutate("fraction", "99999999-ffff", p.Items[0].ItemID, f64(1))
	if r.Code != 200 || len(h.vars.rows("var-food")) != before {
		t.Fatalf("f=1: %d rows %d->%d", r.Code, before, len(h.vars.rows("var-food")))
	}
	if m := decode[MutationResponse](t, r); m.Item.Fraction == nil || *m.Item.Fraction != 1 {
		t.Fatal("fraction not recorded")
	}
	// Undo after fraction then fraction after undo is refused.
	if r := h.mutate("undo", "99999999-gggg", p.Items[0].ItemID, nil); r.Code != 200 {
		t.Fatal(r.Code)
	}
}

func TestFractionAfterUndoRefused(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("aaaaaaaa-0001", "pizza"))
	if r := h.mutate("undo", "aaaaaaaa-0002", p.Items[0].ItemID, nil); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r := h.mutate("fraction", "aaaaaaaa-0003", p.Items[0].ItemID, f64(0.5)); r.Code != 409 || errCode(t, r) != "already_undone" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestNullFibreStaysUnknown(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	resp := decode[LogResponse](t, h.logText("bbbbbbbb-0001", "pizza"))
	m := macro(resp.Snapshot, "fiber_g")
	if m.UnknownRows != 1 || m.Consumed != 0 {
		t.Fatalf("fibre %+v", m)
	}
	if resp.Items[0].Fiber.OK || resp.Items[0].Effective.Fiber.OK {
		t.Fatal("fibre became known")
	}
	// Halving keeps it unknown.
	fr := decode[MutationResponse](t, h.mutate("fraction", "bbbbbbbb-0002", resp.Items[0].ItemID, f64(0.5)))
	if macro(fr.Snapshot, "fiber_g").UnknownRows != 1 {
		t.Fatal("halved null fibre not unknown")
	}
	// The added map carries null for fibre.
	for _, b := range resp.Blocks {
		if b.Widget == "macros_today" {
			added := b.Data.(map[string]any)["added"].(map[string]any)
			if added["fiber_g"] != nil {
				t.Fatal("added fibre must be null")
			}
		}
	}
}

func TestRecordDateStableAcrossMidnight(t *testing.T) {
	h := newHarness(t)
	// 23:50 in Zurich (CEST, UTC+2) on 2026-10-01.
	h.clk.Set(time.Date(2026, 10, 1, 21, 50, 0, 0, time.UTC))
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	resp := decode[LogResponse](t, h.logText("cccccccc-0001", "late pizza"))
	if resp.Snapshot.Date != "2026-10-01" {
		t.Fatal(resp.Snapshot.Date)
	}
	h.clk.Add(30 * time.Minute) // 00:20 on 2026-10-02
	r := h.mutate("fraction", "cccccccc-0002", resp.Items[0].ItemID, f64(0.5))
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	if d := decode[MutationResponse](t, r).Date; d != "2026-10-01" {
		t.Fatalf("mutation date %s", d)
	}
	for _, row := range h.vars.rows("var-food") {
		if row["_record_date"] != "2026-10-01" {
			t.Fatalf("row on %v", row["_record_date"])
		}
	}
	// local_time sets the day: a log with yesterday's 22:00 lands on the 1st.
	b, _ := json.Marshal(map[string]string{"client_id": "cccccccc-0003", "text": "x", "local_time": "2026-10-01T22:00:00+02:00"})
	rec := h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json")
	if decode[LogResponse](t, rec).Snapshot.Date != "2026-10-01" {
		t.Fatal("local_time date")
	}
	b, _ = json.Marshal(map[string]string{"client_id": "cccccccc-0004", "text": "x", "local_time": "2026-09-28T22:00:00+02:00"})
	if rec := h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"); rec.Code != 400 {
		t.Fatalf("48 h bound: %d", rec.Code)
	}
}

func TestAuthCheckedBeforeBodyRead(t *testing.T) {
	h := newHarness(t)
	var read atomic.Bool
	body := readerFunc(func(p []byte) (int, error) { read.Store(true); return 0, io.EOF })
	for _, tok := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest("POST", "/fuel/log", body)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
		if rec.Code != 401 || errCode(t, rec) != "unauthorized" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("code %d %s", rec.Code, rec.Body)
		}
	}
	if read.Load() {
		t.Fatal("body was read before auth")
	}
	// The brake: 429 with Retry-After after 30 failures a minute.
	var last *httptest.ResponseRecorder
	for i := 0; i < 31; i++ {
		req := httptest.NewRequest("GET", "/fuel/snapshot", nil)
		last = httptest.NewRecorder()
		h.h.ServeHTTP(last, req)
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" {
		t.Fatalf("brake: %d", last.Code)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func multipartBody(t *testing.T, fields map[string]string, files []filePart) (io.Reader, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for _, f := range files {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.field, f.name))
		hdr.Set("Content-Type", f.ct)
		w, _ := mw.CreatePart(hdr)
		_, _ = w.Write(f.data)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

type filePart struct {
	field, name, ct string
	data            []byte
}

func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 80, 255})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func TestPhotoLogAndMediaRejections(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(in ModelInput) string {
		if len(in.Images) != 1 {
			t.Errorf("model got %d images", len(in.Images))
		}
		return pizzaPhoto
	}
	body, ct := multipartBody(t, map[string]string{"client_id": "dddddddd-0001"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(2000, 1000)}})
	rec := h.do("POST", "/fuel/log", body, ct)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decode[LogResponse](t, rec)
	if len(resp.PhotoIDs) != 1 {
		t.Fatal("no photo id")
	}
	rows := h.vars.rows("var-food")
	if rows[0]["photo_ref"] != resp.PhotoIDs[0] {
		t.Fatalf("photo_ref %v", rows[0]["photo_ref"])
	}
	ph := h.do("GET", "/fuel/photo/"+resp.PhotoIDs[0], nil, "")
	if ph.Code != 200 || ph.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("photo %d", ph.Code)
	}
	if st, err := os.Stat(filepath.Join(h.opts.StateDir, "photos", resp.PhotoIDs[0]+".jpg")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("photo file mode %v %v", st, err)
	}
	if r := h.do("GET", "/fuel/photo/..%2Fjournal.jsonl", nil, ""); r.Code != 404 {
		t.Fatalf("photo traversal %d", r.Code)
	}

	// HEIC (ftypheic) and GIF: 415 by content, whatever the header says.
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
	for i, data := range [][]byte{heic, []byte("GIF89a....")} {
		body, ct := multipartBody(t, map[string]string{"client_id": fmt.Sprintf("dddddddd-01%02d", i)}, []filePart{{"image", "x.jpg", "image/jpeg", data}})
		if r := h.do("POST", "/fuel/log", body, ct); r.Code != 415 || errCode(t, r) != "unsupported_media" {
			t.Fatalf("image %d: %d %s", i, r.Code, r.Body)
		}
	}
	// Audio that is not MP4/WAV: 415.
	body, ct = multipartBody(t, map[string]string{"client_id": "dddddddd-0200"}, []filePart{{"audio", "a.ogg", "audio/mp4", []byte("OggS\x00\x02 not an mp4 container")}})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 415 {
		t.Fatalf("audio: %d %s", r.Code, r.Body)
	}
	// Oversize image part: 413.
	big := append(testJPEG(10, 10), make([]byte, maxImage)...)
	body, ct = multipartBody(t, map[string]string{"client_id": "dddddddd-0300"}, []filePart{{"image", "big.jpg", "image/jpeg", big}})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 413 || errCode(t, r) != "too_large" {
		t.Fatalf("oversize image: %d %s", r.Code, r.Body)
	}
	// Whole body over 24 MB: 413.
	parts := []filePart{}
	for i := 0; i < 4; i++ {
		parts = append(parts, filePart{"image", "p.jpg", "image/jpeg", make([]byte, 7<<20)})
	}
	body, ct = multipartBody(t, map[string]string{"client_id": "dddddddd-0400"}, parts)
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 413 {
		t.Fatalf("oversize body: %d", r.Code)
	}
	// Text over 2000 characters: 413.
	if r := h.logText("dddddddd-0500", strings.Repeat("a", 2001)); r.Code != 413 {
		t.Fatalf("long text: %d", r.Code)
	}
	// Nothing to log: 400 empty_input.
	if r := h.logText("dddddddd-0600", "  "); r.Code != 400 || errCode(t, r) != "empty_input" {
		t.Fatalf("empty: %d %s", r.Code, r.Body)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVoiceLog(t *testing.T) {
	h := newHarness(t)
	var got string
	h.model.fn = func(in ModelInput) string { got = in.Text; return skyrWalnuts }
	body, ct := multipartBody(t, map[string]string{"client_id": "eeeeeeee-0001", "text": "with honey"}, []filePart{{"audio", "a.m4a", "audio/m4a", fixture(t, "tone.m4a")}})
	rec := h.do("POST", "/fuel/log", body, ct)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decode[LogResponse](t, rec)
	if resp.Transcript == nil || *resp.Transcript != "two eggs" || got != "two eggs\nwith honey" {
		t.Fatalf("transcript %v model text %q", resp.Transcript, got)
	}
	// Too long a clip: 400.
	body, ct = multipartBody(t, map[string]string{"client_id": "eeeeeeee-0002"}, []filePart{{"audio", "a.m4a", "audio/m4a", fixture(t, "long.m4a")}})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 400 {
		t.Fatalf("long audio %d %s", r.Code, r.Body)
	}
	// Video-only MP4, ALAC in MP4 and a truncated file: 415.
	for i, name := range []string{"video.mp4", "alac.m4a", "truncated.m4a"} {
		body, ct = multipartBody(t, map[string]string{"client_id": fmt.Sprintf("eeeeeeee-01%02d", i)}, []filePart{{"audio", "a.m4a", "audio/m4a", fixture(t, name)}})
		if r := h.do("POST", "/fuel/log", body, ct); r.Code != 415 {
			t.Fatalf("%s: %d %s", name, r.Code, r.Body)
		}
	}
}

func TestQuestionWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"question","items":[],"text":"Protein is lagging; add a dairy snack.","widgets":["macros_today","next_action"]}`
	}
	rec := h.logText("ffffffff-0001", "how is my day?")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decode[LogResponse](t, rec)
	if resp.Intent != "question" || len(resp.Items) != 0 || h.vars.posts != 0 {
		t.Fatalf("intent %s items %d posts %d", resp.Intent, len(resp.Items), h.vars.posts)
	}
	if resp.Snapshot.Revision != 0 {
		t.Fatal("revision bumped by a question")
	}
	// Question with items is invalid if intent says question but items exist:
	// the server treats items as a log (spec 14). Log with zero items is invalid.
	h.model.fn = func(ModelInput) string { return `{"intent":"log","items":[],"text":"","widgets":[]}` }
	if r := h.logText("ffffffff-0002", "hmm"); r.Code != 502 || errCode(t, r) != "model_invalid" {
		t.Fatalf("log with no items: %d %s", r.Code, r.Body)
	}
	if h.model.calls != 3 {
		t.Fatalf("one retry expected, calls %d", h.model.calls)
	}
}

func TestTimeoutBeforeWritesIs504AndRetryRunsAgain(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Budget = 300 * time.Millisecond })
	h.model.delay = 500 * time.Millisecond
	rec := h.logText("12121212-0001", "250 g skyr and 30 g walnuts")
	if rec.Code != 504 || errCode(t, rec) != "timeout" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(h.svc.journal.Ops()) != 0 || len(h.svc.journal.Entries()) != 0 || h.vars.posts != 0 {
		t.Fatal("something was journaled or written")
	}
	if _, ok := h.svc.idem.Get("12121212-0001"); ok {
		t.Fatal("idem reserved on 504")
	}
	h.model.delay = 0
	if r := h.logText("12121212-0001", "250 g skyr and 30 g walnuts"); r.Code != 200 {
		t.Fatalf("retry %d %s", r.Code, r.Body)
	}
}

func TestDefinitiveRejectionFailsAndCompensates(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 400
		}
		return true, 201
	}
	rec := h.logText("13131313-0001", "250 g skyr and 30 g walnuts")
	if rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h.svc.reconcileOnce(context.Background())
	rows := h.vars.rows("var-food")
	if len(rows) != 2 || rows[1]["reason"] != "compensation" {
		t.Fatalf("rows %v", rows)
	}
	h.clk.Add(2 * time.Minute)
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if macro(snap, "protein_g").Consumed != 0 {
		t.Fatalf("protein %v after compensation", macro(snap, "protein_g").Consumed)
	}
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	found := false
	for _, it := range feed.Items {
		found = found || (it.Text != nil && *it.Text == "could not save walnuts")
	}
	if !found {
		t.Fatal("feed lacks the could-not-save line")
	}
}

func TestOpFailsAfter24h(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500
		}
		return true, 201
	}
	resp := decode[LogResponse](t, h.logText("14141414-0001", "250 g skyr and 30 g walnuts"))
	for i := 0; i < 3; i++ {
		h.clk.Add(61 * time.Second)
		h.svc.reconcileOnce(context.Background())
	}
	// Three attempts in total, 60 s apart; then it waits.
	if h.vars.posts != 4 {
		t.Fatalf("posts %d (want 1 skyr + 3 walnut attempts)", h.vars.posts)
	}
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	h.svc.reconcileOnce(context.Background())
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if e.Status != "failed" {
		t.Fatalf("status %s", e.Status)
	}
	var comp int
	for _, r := range h.vars.rows("var-food") {
		if r["reason"] == "compensation" {
			comp++
		}
	}
	if comp != 1 {
		t.Fatalf("compensations %d", comp)
	}
}

func TestJournalReplayResumesAndRevisionSurvives(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) {
		if n == 1 {
			return true, 500
		}
		return true, 201
	}
	resp := decode[LogResponse](t, h.logText("15151515-0001", "250 g skyr and 30 g walnuts"))
	h.restart()
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if e.Status != "done" || e.Snapshot.Revision != 2 {
		t.Fatalf("status %s revision %d", e.Status, e.Snapshot.Revision)
	}
	// Idempotency survives the restart.
	if r := h.logText("15151515-0001", "250 g skyr and 30 g walnuts"); r.Code != 200 && r.Code != 202 {
		t.Fatalf("replay %d", r.Code)
	}
	if h.model.calls != 1 {
		t.Fatalf("model calls %d", h.model.calls)
	}
}

func TestFeedShowsCurrentItemState(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("16161616-0001", "250 g skyr and 30 g walnuts"))
	h.mutate("undo", "16161616-0002", resp.Items[0].ItemID, nil)
	feed := decode[struct {
		Items      []feedOut `json:"items"`
		NextBefore *string   `json:"next_before"`
	}](t, h.do("GET", "/fuel/feed?limit=2", nil, ""))
	if len(feed.Items) != 2 || feed.NextBefore == nil {
		t.Fatalf("page %d next %v", len(feed.Items), feed.NextBefore)
	}
	all := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	var reply *feedOut
	for i := range all.Items {
		if len(all.Items[i].Items) > 0 {
			reply = &all.Items[i]
		}
	}
	if reply == nil || !reply.Items[0].Undone {
		t.Fatal("feed item state is not current")
	}
	older := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed?before="+*feed.NextBefore, nil, ""))
	if len(older.Items) != 1 || older.Items[0].Role != "user" {
		t.Fatalf("older page %+v", older.Items)
	}
}

func TestSnapshotDateBoundsAndTargets(t *testing.T) {
	h := newHarness(t)
	if r := h.do("GET", "/fuel/snapshot?date=2026-08-01", nil, ""); r.Code != 400 {
		t.Fatalf("old date %d", r.Code)
	}
	if r := h.do("GET", "/fuel/snapshot?date=2026-10-02", nil, ""); r.Code != 400 {
		t.Fatalf("future date %d", r.Code)
	}
	if r := h.do("GET", "/fuel/snapshot?date=2026-08-28", nil, ""); r.Code != 200 {
		t.Fatalf("34 days back %d", r.Code)
	}
	// An invalid targets file: 503 targets_invalid until fixed.
	_ = os.WriteFile(h.opts.TargetsFile, []byte(`{"protein_g":{"kind":"floor","value":160}}`), 0o600)
	r := h.do("GET", "/fuel/snapshot", nil, "")
	if r.Code != 503 || errCode(t, r) != "targets_invalid" {
		t.Fatalf("invalid targets %d %s", r.Code, r.Body)
	}
	_ = os.WriteFile(h.opts.TargetsFile, []byte(DefaultTargetsJSON), 0o600)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(h.opts.TargetsFile, future, future)
	if r := h.do("GET", "/fuel/snapshot", nil, ""); r.Code != 200 {
		t.Fatalf("fixed targets %d %s", r.Code, r.Body)
	}
}

func TestStaplesOverrideMacros(t *testing.T) {
	h := newHarness(t, func(o *Options) {})
	_ = os.WriteFile(h.opts.StaplesFile, []byte(`[{"key":"skyr","aliases":["skyr"],"per_100g":{"kcal":63,"protein_g":11,"carbs_g":4,"fat_g":0.2,"sat_fat_g":0.1,"fiber_g":0},"default_g":250}]`), 0o600)
	h.restart()
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[{"item":"skyr","staple_key":"skyr","portion_g":null,"portion_basis":"label","kcal":1,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null,"needs_fraction":false}],"text":"","widgets":[]}`
	}
	resp := decode[LogResponse](t, h.logText("17171717-0001", "skyr"))
	it := resp.Items[0]
	if it.Protein.float() != 27.5 || *it.PortionG != 250 || it.Kcal.float() != 157.5 {
		t.Fatalf("staple not applied: %+v", it)
	}
}

func TestTestModeRefusesProductionFoodLog(t *testing.T) {
	o := Options{Token: "t", Vars: &VariablesHTTP{}, Model: &fakeModel{}, FoodVar: "Food log", BodyVar: "Body composition", StateDir: t.TempDir(), TestMode: true}
	if err := o.Validate(); err == nil {
		t.Fatal("test_mode accepted Food log")
	}
	o.Token = ""
	o.TestMode = false
	if err := o.Validate(); err == nil {
		t.Fatal("empty token accepted")
	}
}

func TestResolveVarsDefinitiveErrors(t *testing.T) {
	fv := newFakeVars()
	fv.vars = append(fv.vars, VarInfo{ID: "dup", Name: "Body composition", Type: "json"}, VarInfo{ID: "n", Name: "Numeric", Type: "numeric"})
	srv := httptest.NewServer(fv)
	defer srv.Close()
	v := &VariablesHTTP{Base: srv.URL, Key: "vkey", Client: loopbackClient()}
	for _, c := range []struct{ food, body string }{{"Fuel e2e food log", "Body composition"}, {"Missing", "x"}, {"Numeric", "x"}} {
		_, err := ResolveVars(context.Background(), v, c.food, c.body)
		if err == nil || !errors.Is(err, errConfig) {
			t.Fatalf("%v: want config error, got %v", c, err)
		}
	}
}

func TestRateLimitAndBusy(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 30; i++ {
		if r := h.logText(fmt.Sprintf("18181818-%04d", i), "skyr"); r.Code != 200 {
			t.Fatalf("log %d: %d", i, r.Code)
		}
	}
	r := h.logText("18181818-9999", "skyr")
	if r.Code != 429 || r.Header().Get("Retry-After") == "" {
		t.Fatalf("31st log: %d", r.Code)
	}
}

func TestConcurrencySlotsAnswer503(t *testing.T) {
	h := newHarness(t)
	h.model.delay = 600 * time.Millisecond
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.logText(fmt.Sprintf("19191919-%04d", i), "skyr").Code
		}(i)
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	n503 := 0
	for _, c := range codes {
		if c == 503 {
			n503++
		}
	}
	if n503 != 1 {
		t.Fatalf("codes %v", codes)
	}
}

func TestTornJournalTailIsCutAndSurvivesTwoRestarts(t *testing.T) {
	h := newHarness(t)
	decode[LogResponse](t, h.logText("20202020-0001", "250 g skyr and 30 g walnuts"))
	path := filepath.Join(h.opts.StateDir, "journal.jsonl")
	h.svc.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"t":"state","op_id":"x","sta`) // a crash mid-append
	f.Close()
	h.start()
	decode[LogResponse](t, h.logText("20202020-0002", "250 g skyr and 30 g walnuts"))
	h.restart()
	if n := len(h.svc.journal.Entries()); n != 2 {
		t.Fatalf("entries after two restarts: %d", n)
	}
	// Interior corruption is refused, not skipped.
	h.svc.Close()
	b, _ := os.ReadFile(path)
	_ = os.WriteFile(path, append([]byte("{garbage}\n"), b...), 0o600)
	if _, err := New(h.opts); err == nil {
		t.Fatal("interior corruption accepted")
	}
	_ = os.WriteFile(path, b, 0o600)
	h.start()
}

func TestFailedOpDerivesEntryFailureOnReplay(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("21212121-0001", "250 g skyr and 30 g walnuts"))
	// Simulate: an op was failed, and the process died before anything else.
	var opID string
	for _, op := range h.svc.journal.Ops() {
		if op.ItemID == resp.Items[1].ItemID {
			opID = op.ID
		}
	}
	h.svc.Close()
	// Append a failed state for a done op is ignored (terminal); use a fresh
	// pending op instead via a txn, then its failure.
	path := filepath.Join(h.opts.StateDir, "journal.jsonl")
	it := resp.Items[1]
	extra := Op{ID: "op_extra", Kind: "original", EntryID: resp.EntryID, ItemID: it.ItemID, RowItemID: "it_x", Reason: "undo", Date: "2026-10-01", State: OpPending, CreatedAt: h.clk.Now()}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	b1, _ := json.Marshal(journalRec{T: "txn", Ops: []Op{extra}, At: h.clk.Now()})
	b2, _ := json.Marshal(journalRec{T: "state", OpID: "op_extra", State: OpFailed, At: h.clk.Now()})
	_, _ = f.Write(append(append(b1, '\n'), append(b2, '\n')...))
	f.Close()
	h.start()
	e, _ := h.svc.journal.Entry(resp.EntryID)
	if !e.Failed || !h.svc.feed.HasNotice("op_extra") {
		t.Fatalf("failed=%v notice=%v", e.Failed, h.svc.feed.HasNotice("op_extra"))
	}
	_ = opID
}

func TestAttemptSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	// Every POST hangs past the 5 s Variables timeout? Use a 500 instead and
	// check that attempts are journaled before each post.
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) { return false, 500 }
	decode[LogResponse](t, h.logText("22222222-0001", "skyr"))
	for i := 0; i < 2; i++ {
		h.clk.Add(61 * time.Second)
		h.svc.reconcileOnce(context.Background())
	}
	h.restart()
	for _, op := range h.svc.journal.Ops() {
		if op.Attempts != 3 {
			t.Fatalf("attempts %d after restart", op.Attempts)
		}
	}
	h.clk.Add(61 * time.Second)
	posts := h.vars.posts
	h.svc.reconcileOnce(context.Background())
	if h.vars.posts != posts {
		t.Fatal("a fourth attempt was posted")
	}
}

func TestReplayIgnoresTheTimeWindow(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(map[string]string{"client_id": "23232323-0001", "text": "skyr", "local_time": "2026-10-01T11:00:00+02:00"})
	if r := h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"); r.Code != 200 {
		t.Fatal(r.Code)
	}
	h.clk.Add(72 * time.Hour)
	if r := h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"); r.Code != 200 {
		t.Fatalf("replay after 3 days: %d %s", r.Code, r.Body)
	}
}

func TestJSONTrailingDataRejected(t *testing.T) {
	h := newHarness(t)
	body := `{"client_id":"24242424-0001","text":"skyr"}{"x":1}`
	if r := h.do("POST", "/fuel/log", strings.NewReader(body), "application/json"); r.Code != 400 {
		t.Fatalf("trailing log: %d", r.Code)
	}
	body = `{"client_id":"24242424-0002","item_id":"it_x"} trailing`
	if r := h.do("POST", "/fuel/undo", strings.NewReader(body), "application/json"); r.Code != 400 {
		t.Fatalf("trailing undo: %d", r.Code)
	}
	if r := h.do("POST", "/fuel/log", strings.NewReader(`{"client_id":"24242424-0003","text":"`+strings.Repeat("a", 70<<10)+`"}`), "application/json"); r.Code != 413 {
		t.Fatalf("oversize JSON: %d", r.Code)
	}
}

func TestLogsCarryNoUpstreamText(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"SECRET-MARKER","items":[],"text":"","widgets":["SECRET-MARKER"]}`
	}
	h.logText("25252525-0001", "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SECRET-MARKER upstream body", 500)
	}))
	defer srv.Close()
	v := &VariablesHTTP{Base: srv.URL, Key: "k", Client: loopbackClient()}
	_, err := v.Post(context.Background(), "x", map[string]any{"a": 1}, "2026-10-01")
	log.Printf("fuel: %s", errClass(err))
	o := &OpenAI{Key: "k", Model: "m", BaseURL: srv.URL, Client: loopbackClient()}
	_, err = o.Estimate(context.Background(), ModelInput{})
	log.Printf("fuel: %v", err)
	if strings.Contains(buf.String(), "SECRET-MARKER") {
		t.Fatalf("log leaked upstream text:\n%s", buf.String())
	}
}

func TestNetCarbsFromUnroundedValues(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		return `{"intent":"log","items":[{"item":"x","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":1,"protein_g":1,"carbs_g":10.04,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":0.05,"needs_fraction":false}],"text":"","widgets":[]}`
	}
	resp := decode[LogResponse](t, h.logText("26262626-0001", "x"))
	if got := resp.Items[0].NetCarbs.float(); got != 10 {
		t.Fatalf("net carbs %v, want 10 (9.99 rounded once)", got)
	}
}

func TestClaimRecheckUnderLock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, e := h.svc.claim(ctx, "27272727-0001", "log", "h")
	if e != nil || a.release == nil {
		t.Fatal("first claim")
	}
	_ = h.svc.idem.Put(idemRec{ClientID: "27272727-0001", Hash: "h", Kind: "log", At: h.clk.Now(), Status: 200, Response: []byte("{}")})
	a.release()
	b, e := h.svc.claim(ctx, "27272727-0001", "log", "h")
	if e != nil || b.replay == nil {
		t.Fatal("second claim did not replay the stored record")
	}
}

func TestJournalWriteFailureStopsWrites(t *testing.T) {
	h := newHarness(t)
	h.svc.journal.f.Close() // every write now fails, and cannot be cut back
	if r := h.logText("30303030-0001", "skyr"); r.Code != 500 {
		t.Fatalf("first: %d %s", r.Code, r.Body)
	}
	if h.svc.journal.broken == nil {
		t.Fatal("journal not marked broken")
	}
	if r := h.logText("30303030-0002", "skyr"); r.Code < 500 || h.vars.posts != 0 {
		t.Fatalf("second: %d posts %d", r.Code, h.vars.posts)
	}
}

func TestIdempotencyHoldsWhenIdemFileFails(t *testing.T) {
	h := newHarness(t)
	h.svc.idem.f.Close() // idem.jsonl unwritable
	if r := h.logText("31313131-0001", "250 g skyr and 30 g walnuts"); r.Code != 200 {
		t.Fatalf("%d", r.Code)
	}
	r := h.logText("31313131-0001", "250 g skyr and 30 g walnuts")
	if r.Code != 200 || h.model.calls != 1 || len(h.vars.rows("var-food")) != 2 {
		t.Fatalf("repeat: %d calls %d rows %d", r.Code, h.model.calls, len(h.vars.rows("var-food")))
	}
	resp := decode[LogResponse](t, r)
	if resp.Intent != "log" || resp.LatencyMs == nil || len(resp.Items) != 2 {
		t.Fatalf("replay schema %+v", resp)
	}
	if r := h.logText("31313131-0001", "other"); r.Code != 409 {
		t.Fatalf("changed payload %d", r.Code)
	}
}

func TestReplayAfterReconciliationKeepsSchemas(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("32323232-0001", "pizza"))
	n := 0
	h.vars.onPost = func(int, map[string]any) (bool, int) { n++; return true, 500 }
	r := h.mutate("fraction", "32323232-0002", p.Items[0].ItemID, f64(0.5))
	if r.Code != 202 {
		t.Fatalf("fraction %d %s", r.Code, r.Body)
	}
	h.vars.onPost = nil
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	for _, restart := range []bool{false, true} {
		if restart {
			h.restart()
		}
		r = h.mutate("fraction", "32323232-0002", p.Items[0].ItemID, f64(0.5))
		m := decode[map[string]any](t, r)
		if r.Code != 200 || m["item"] == nil || m["date"] != "2026-10-01" || m["status"] != "done" {
			t.Fatalf("restart=%v replay %d %v", restart, r.Code, m)
		}
	}
	// The log itself replays in the log schema after a restart too.
	r = h.logText("32323232-0001", "pizza")
	m := decode[map[string]any](t, r)
	for _, k := range []string{"intent", "transcript", "photo_ids", "latency_ms", "items", "snapshot"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("log replay lacks %s: %v", k, m)
		}
	}
}

func TestDeadlineCheckedUnderTheJournalLock(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Budget = 300 * time.Millisecond })
	h.svc.journal.mu.Lock()
	go func() { time.Sleep(500 * time.Millisecond); h.svc.journal.mu.Unlock() }()
	r := h.logText("33333333-0001", "skyr")
	if r.Code != 504 || h.vars.posts != 0 || len(h.svc.journal.Entries()) != 0 {
		t.Fatalf("%d posts %d entries %d", r.Code, h.vars.posts, len(h.svc.journal.Entries()))
	}
}

func TestFeedRebuiltAfterCrashBeforeFeedAppend(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("34343434-0001", "250 g skyr and 30 g walnuts"))
	h.mutate("undo", "34343434-0002", resp.Items[0].ItemID, nil)
	h.svc.Close()
	_ = os.Remove(filepath.Join(h.opts.StateDir, "feed.jsonl"))
	h.start()
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	if len(feed.Items) != 3 {
		t.Fatalf("rebuilt feed has %d lines", len(feed.Items))
	}
	h.restart()
	feed = decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	if len(feed.Items) != 3 {
		t.Fatalf("rebuild not idempotent: %d lines", len(feed.Items))
	}
}

func TestFailAfter24hEvenWhenReadsFail(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500
		}
		return true, 201
	}
	resp := decode[LogResponse](t, h.logText("35353535-0001", "250 g skyr and 30 g walnuts"))
	h.vars.failReads.Store(true)
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	h.vars.failReads.Store(false)
	h.svc.reconcileOnce(context.Background())
	e, _ := h.svc.journal.Entry(resp.EntryID)
	if !e.Failed {
		t.Fatal("entry not failed")
	}
	var comp int
	for _, r := range h.vars.rows("var-food") {
		if r["reason"] == "compensation" {
			comp++
		}
	}
	if comp != 1 {
		t.Fatalf("compensations %d", comp)
	}
}

func TestLateRowOfFailedOpIsCompensated(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500
		}
		return true, 201
	}
	decode[LogResponse](t, h.logText("36363636-0001", "250 g skyr and 30 g walnuts"))
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	// The walnut row turns up late with its op_id.
	var walnutOp Op
	for _, op := range h.svc.journal.Ops() {
		if op.Data["item"] == "walnuts" {
			walnutOp = op
		}
	}
	h.vars.add("var-food", walnutOp.Date, walnutOp.Data)
	_ = h.svc.cache.RefreshDay(context.Background(), walnutOp.Date)
	h.svc.reconcileOnce(context.Background())
	sum := 0.0
	for _, r := range h.vars.rows("var-food") {
		sum += r["protein_g"].(float64)
	}
	if toTenth(sum) != 0 {
		t.Fatalf("protein rows sum %v, want 0", sum)
	}
}

func TestMalformedAudioContainers(t *testing.T) {
	good := fixture(t, "tone.m4a")
	if _, _, err := sniffAudio(good); err != nil {
		t.Fatal("real AAC rejected")
	}
	noEsds := bytes.Replace(good, []byte("esds"), []byte("xsds"), 1)
	badOff := append([]byte(nil), good...)
	if i := bytes.Index(badOff, []byte("stco")); i > 0 {
		binary.BigEndian.PutUint32(badOff[i+12:], 0x7fffffff)
	}
	notSoun := bytes.Replace(good, []byte("soun"), []byte("vide"), 1)
	for name, b := range map[string][]byte{"no esds": noEsds, "offset outside mdat": badOff, "no sound track": notSoun} {
		if _, _, err := sniffAudio(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// WAV: 1 s of 8 kHz mono 16-bit, then a truncated copy.
	wav := func(dataLen, have int) []byte {
		var b bytes.Buffer
		b.WriteString("RIFF")
		_ = binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
		b.WriteString("WAVEfmt ")
		for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(16000), uint16(2), uint16(16)} {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
		b.WriteString("data")
		_ = binary.Write(&b, binary.LittleEndian, uint32(dataLen))
		b.Write(make([]byte, have))
		return b.Bytes()
	}
	if _, d, err := sniffAudio(wav(16000, 16000)); err != nil || d != 1 {
		t.Fatalf("good wav: %v %v", d, err)
	}
	if _, _, err := sniffAudio(wav(16000, 4000)); err == nil {
		t.Fatal("truncated wav accepted")
	}
}

func TestModelOutputStrictness(t *testing.T) {
	cases := map[string]string{
		"trailing":            skyrWalnuts + `{"x":1}`,
		"missing fraction":    `{"intent":"log","items":[{"item":"x","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":1,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null}],"text":"","widgets":[]}`,
		"null needs_fraction": `{"intent":"log","items":[{"item":"x","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":1,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null,"needs_fraction":null}],"text":"","widgets":[]}`,
		"missing widgets":     `{"intent":"question","items":[],"text":""}`,
		"null kcal":           `{"intent":"log","items":[{"item":"x","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":null,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null,"needs_fraction":false}],"text":"","widgets":[]}`,
		"kcal out of bounds":  `{"intent":"log","items":[{"item":"x","staple_key":null,"portion_g":null,"portion_basis":"unspecified","kcal":3001,"protein_g":1,"carbs_g":1,"net_carbs_g":null,"fat_g":1,"sat_fat_g":1,"fiber_g":null,"needs_fraction":false}],"text":"","widgets":[]}`,
	}
	for name, raw := range cases {
		if _, err := validateOutput(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := validateOutput(json.RawMessage(skyrWalnuts)); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotencyExpiresAfter7Days(t *testing.T) {
	h := newHarness(t)
	h.logText("37373737-0001", "skyr")
	if _, ok := h.svc.lookupIdem("37373737-0001"); !ok {
		t.Fatal("fresh record missing")
	}
	h.clk.Add(8 * 24 * time.Hour)
	if _, ok := h.svc.lookupIdem("37373737-0001"); ok {
		t.Fatal("record older than 7 days still used")
	}
	h.restart()
	if _, ok := h.svc.lookupIdem("37373737-0001"); ok {
		t.Fatal("record older than 7 days back after a restart")
	}
}

func TestUndoUsesAuthoritativeRows(t *testing.T) {
	for _, del := range []bool{false, true} {
		h := newHarness(t)
		resp := decode[LogResponse](t, h.logText("40404040-0001", "250 g skyr and 30 g walnuts"))
		skyr := resp.Items[0]
		h.vars.edit(*skyr.ValueID, map[string]any{"kcal": 300.0, "protein_g": 40.0}, del)
		// Own rows younger than 60 s are re-added on refresh (eventual
		// consistency, spec 14 [C8]); after that the server's view wins.
		h.clk.Add(61 * time.Second)
		r := h.mutate("undo", "40404040-0002", skyr.ItemID, nil)
		if r.Code != 200 {
			t.Fatalf("del=%v undo %d %s", del, r.Code, r.Body)
		}
		var corr map[string]any
		for _, row := range h.vars.rows("var-food") {
			if row["corrects"] == skyr.ItemID {
				corr = row
			}
		}
		want := -300.0
		if del {
			want = 0
		}
		if corr["kcal"] != want {
			t.Fatalf("del=%v undo row kcal %v want %v", del, corr["kcal"], want)
		}
		m := decode[MutationResponse](t, r)
		if got := macro(m.Snapshot, "kcal").Consumed; got != 196 {
			t.Fatalf("del=%v kcal after undo %v (walnuts only)", del, got)
		}
	}
}

func TestFailedCompensationIsRetried(t *testing.T) {
	h := newHarness(t)
	compTries := 0
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 400
		}
		if data["reason"] == "compensation" {
			compTries++
			if compTries == 1 {
				return false, 400
			}
		}
		return true, 201
	}
	h.logText("41414141-0001", "250 g skyr and 30 g walnuts")
	h.svc.reconcileOnce(context.Background()) // compensation written, rejected
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background()) // re-read, re-posted with the same op_id
	sum := 0.0
	comps := 0
	for _, r := range h.vars.rows("var-food") {
		sum += r["kcal"].(float64)
		if r["reason"] == "compensation" {
			comps++
		}
	}
	var compOps int
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "compensation" {
			compOps++
		}
	}
	if compTries != 2 || toTenth(sum) != 0 || comps != 1 || compOps != 1 {
		t.Fatalf("tries %d sum %v rows %d ops %d", compTries, sum, comps, compOps)
	}
}

func TestLateRowOutsideRefreshWindowIsCompensated(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500
		}
		return true, 201
	}
	h.logText("42424242-0001", "250 g skyr and 30 g walnuts")
	h.clk.Add(72 * time.Hour) // the entry's date left today/yesterday
	h.svc.reconcileOnce(context.Background())
	var walnutOp Op
	for _, op := range h.svc.journal.Ops() {
		if op.Data["item"] == "walnuts" {
			walnutOp = op
		}
	}
	if walnutOp.State != OpFailed {
		t.Fatalf("walnut op %s", walnutOp.State)
	}
	h.vars.add("var-food", walnutOp.Date, walnutOp.Data)
	h.clk.Add(11 * time.Minute)
	h.svc.reconcileOnce(context.Background())
	sum := 0.0
	for _, r := range h.vars.rows("var-food") {
		sum += r["kcal"].(float64)
	}
	if toTenth(sum) != 0 {
		t.Fatalf("kcal rows sum %v, want 0", sum)
	}
}

func TestFsyncFailureOnDoneKeepsRevisionAndRowsTogether(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	h.svc.journal.syncFn = func(f *os.File) error {
		if calls.Add(1) >= 2 { // the txn syncs, the first done record fails
			return errors.New("injected fsync failure")
		}
		return f.Sync()
	}
	r := h.logText("43434343-0001", "250 g skyr and 30 g walnuts")
	resp := decode[LogResponse](t, r)
	done := 0
	for _, it := range resp.Items {
		if it.ValueID != nil {
			done++
		}
	}
	rows, _, _ := h.svc.cache.Rows("2026-10-01")
	if resp.Snapshot.Revision != len(rows) {
		t.Fatalf("revision %d but %d rows in the cache", resp.Snapshot.Revision, len(rows))
	}
	if h.svc.journal.broken == nil {
		t.Fatal("journal not marked broken after the fsync failure")
	}
	if r := h.logText("43434343-0002", "skyr"); r.Code < 500 {
		t.Fatalf("writes continued after an fsync failure: %d", r.Code)
	}
	_ = done
}

func TestFeedReplyLineRecoveredAlone(t *testing.T) {
	h := newHarness(t)
	decode[LogResponse](t, h.logText("44444444-0101", "250 g skyr and 30 g walnuts"))
	h.svc.Close()
	path := filepath.Join(h.opts.StateDir, "feed.jsonl")
	b, _ := os.ReadFile(path)
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(line, `"key":"r:`) {
			keep = append(keep, line)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
	h.start()
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	if len(feed.Items) != 2 || feed.Items[1].Role != "fuel" || len(feed.Items[1].Items) != 2 {
		t.Fatalf("feed %+v", feed.Items)
	}
}

func TestFeedSyncFailureStopsAppendsWithoutSeqReuse(t *testing.T) {
	dir := t.TempDir()
	fd, err := OpenFeed(filepath.Join(dir, "feed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fd.syncFn = func(*os.File) error { return errors.New("injected") }
	a, err := fd.Append(FeedItem{Role: "user"})
	if err == nil || a.Seq != 1 {
		t.Fatalf("first append: %v seq %d", err, a.Seq)
	}
	if _, err := fd.Append(FeedItem{Role: "user"}); err == nil {
		t.Fatal("append after a failed fsync accepted")
	}
	fd.Close()
	fd2, _ := OpenFeed(filepath.Join(dir, "feed.jsonl"))
	if items, _ := fd2.Page(0, 10); len(items) != 1 {
		t.Fatalf("lines %d", len(items))
	}
}

func TestExpiredClientIDReusedForFractionOne(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p1 := decode[LogResponse](t, h.logText("45454545-0001", "pizza"))
	if r := h.mutate("fraction", "45454545-0009", p1.Items[0].ItemID, f64(1)); r.Code != 200 {
		t.Fatal(r.Code)
	}
	h.clk.Add(8 * 24 * time.Hour)
	b, _ := json.Marshal(map[string]string{"client_id": "45454545-0002", "text": "pizza"})
	p2 := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"))
	h.svc.idem.f.Close() // idem storage unavailable from here
	r := h.mutate("fraction", "45454545-0009", p2.Items[0].ItemID, f64(1))
	if r.Code != 200 {
		t.Fatalf("reuse after expiry: %d %s", r.Code, r.Body)
	}
	id, _ := h.svc.journal.Ident("45454545-0009")
	if id.ItemID != p2.Items[0].ItemID {
		t.Fatalf("identity still points at %s", id.ItemID)
	}
	rep := decode[MutationResponse](t, h.mutate("fraction", "45454545-0009", p2.Items[0].ItemID, f64(1)))
	if rep.Item.ItemID != p2.Items[0].ItemID {
		t.Fatal("replay answered for the old item")
	}
}

func TestForgedAudioMetadataRejected(t *testing.T) {
	good := fixture(t, "tone.m4a")
	patch := func(box string, off int, v uint32) []byte {
		b := append([]byte(nil), good...)
		i := bytes.Index(b, []byte(box))
		binary.BigEndian.PutUint32(b[i+off:], v)
		return b
	}
	// mdhd duration (version 0: +4 type, +4 v/f, +4 ctime, +4 mtime, +4 scale).
	forgedDur := patch("mdhd", 20, 1)
	// stsz uniform sample size forged huge.
	forgedSize := patch("stsz", 8, 60000)
	for name, b := range map[string][]byte{"mdhd duration": forgedDur, "stsz size": forgedSize} {
		if _, _, err := sniffAudio(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	wav := func(format, bits, align uint16, ext bool, sub uint16) []byte {
		var b bytes.Buffer
		fmtLen := uint32(16)
		if ext {
			fmtLen = 40
		}
		b.WriteString("RIFF")
		_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+fmtLen+8+1600))
		b.WriteString("WAVEfmt ")
		for _, v := range []any{fmtLen, format, uint16(1), uint32(8000), uint32(8000) * uint32(align), align, bits} {
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
		if ext {
			for _, v := range []any{uint16(22), bits, uint32(0), sub} {
				_ = binary.Write(&b, binary.LittleEndian, v)
			}
			b.Write(make([]byte, 14))
		}
		b.WriteString("data")
		_ = binary.Write(&b, binary.LittleEndian, uint32(1600))
		b.Write(make([]byte, 1600))
		return b.Bytes()
	}
	if _, _, err := sniffAudio(wav(1, 16, 2, false, 0)); err != nil {
		t.Fatal("good wav rejected")
	}
	if _, _, err := sniffAudio(wav(0xFFFE, 16, 2, true, 1)); err != nil {
		t.Fatal("good extensible wav rejected")
	}
	for name, b := range map[string][]byte{
		"bit depth vs align": wav(1, 16, 4, false, 0),
		"odd bit depth":      wav(1, 12, 2, false, 0),
		"extensible subtype": wav(0xFFFE, 16, 2, true, 2),
	} {
		if _, _, err := sniffAudio(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUnknownIntentWithItemsRejected(t *testing.T) {
	raw := strings.Replace(skyrWalnuts, `"intent":"log"`, `"intent":"unexpected"`, 1)
	if _, err := validateOutput(json.RawMessage(raw)); err == nil {
		t.Fatal("unknown intent with items accepted")
	}
}

func TestCompensationSurvivesRejectionsAndRestart(t *testing.T) {
	h := newHarness(t)
	reject := true
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 400
		}
		if data["reason"] == "compensation" && reject {
			return false, 400
		}
		return true, 201
	}
	h.logText("46464646-0001", "250 g skyr and 30 g walnuts")
	for i := 0; i < 6; i++ {
		h.clk.Add(2 * time.Hour)
		h.svc.reconcileOnce(context.Background())
	}
	reject = false
	h.restart()
	h.clk.Add(2 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	sum := 0.0
	for _, r := range h.vars.rows("var-food") {
		sum += r["kcal"].(float64)
	}
	if toTenth(sum) != 0 {
		t.Fatalf("kcal sum %v after recovery", sum)
	}
}

func TestLateCompensationRowCountedOnce(t *testing.T) {
	h := newHarness(t)
	// The compensation POST is stored but answers 500 (uncertain) twice.
	n := 0
	h.vars.onPost = func(_ int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 400
		}
		if data["reason"] == "compensation" {
			n++
			if n <= 2 {
				return true, 500
			}
		}
		return true, 201
	}
	h.logText("47474747-0001", "250 g skyr and 30 g walnuts")
	for i := 0; i < 4; i++ {
		h.clk.Add(61 * time.Second)
		h.svc.reconcileOnce(context.Background())
	}
	h.clk.Add(2 * time.Minute)
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot", nil, ""))
	if got := macro(snap, "kcal").Consumed; got != 0 {
		t.Fatalf("kcal %v: a compensation counted twice or not at all", got)
	}
}

func TestCompensationFollowsExternalEdits(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500 // uncertain: the entry waits
		}
		return true, 201
	}
	resp := decode[LogResponse](t, h.logText("48484848-0001", "250 g skyr and 30 g walnuts"))
	h.vars.edit(*resp.Items[0].ValueID, map[string]any{"kcal": 400.0}, false)
	h.clk.Add(25 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	h.svc.reconcileOnce(context.Background())
	sum := 0.0
	for _, r := range h.vars.rows("var-food") {
		sum += r["kcal"].(float64)
	}
	if toTenth(sum) != 0 {
		t.Fatalf("kcal sum %v after compensating an edited row", sum)
	}
}

func TestFractionAfterExternalDeletionWritesNoRow(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("50505050-0001", "pizza"))
	h.vars.edit(*p.Items[0].ValueID, nil, true)
	h.clk.Add(61 * time.Second)
	r := h.mutate("fraction", "50505050-0002", p.Items[0].ItemID, f64(0.5))
	if r.Code != 200 || len(h.vars.rows("var-food")) != 0 {
		t.Fatalf("%d rows %d", r.Code, len(h.vars.rows("var-food")))
	}
}

func TestEffectiveAgreesWithSnapshotAfterEdit(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("51515151-0001", "pizza"))
	h.vars.edit(*p.Items[0].ValueID, map[string]any{"kcal": 1000.0}, false)
	h.clk.Add(61 * time.Second)
	m := decode[MutationResponse](t, h.mutate("fraction", "51515151-0002", p.Items[0].ItemID, f64(0.5)))
	if m.Item.Effective.Kcal.float() != 500 || macro(m.Snapshot, "kcal").Consumed != 500 {
		t.Fatalf("effective %v snapshot %v", m.Item.Effective.Kcal.float(), macro(m.Snapshot, "kcal").Consumed)
	}
}

func TestForgedTimescaleRejected(t *testing.T) {
	b := append([]byte(nil), fixture(t, "long.m4a")...)
	i := bytes.Index(b, []byte("mdhd"))
	scale := binary.BigEndian.Uint32(b[i+16:])
	binary.BigEndian.PutUint32(b[i+16:], scale*2) // halves the claimed duration
	if _, _, err := sniffAudio(b); err == nil {
		t.Fatal("forged timescale accepted")
	}
}

func TestCoachEventRecoveredAfterCrash(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("52525252-0001", "250 g skyr and 30 g walnuts"))
	h.svc.Close()
	path := filepath.Join(h.opts.StateDir, "coach-events.jsonl")
	_ = os.Remove(path)
	h.start()
	h.restart()
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), resp.EntryID); n != 1 {
		t.Fatalf("coach events for the entry: %d", n)
	}
}

const oatsFibre10 = `{"intent":"log","items":[{"item":"oats","staple_key":null,"portion_g":null,"portion_basis":"photo_estimate","kcal":300,"protein_g":10,"carbs_g":50,"net_carbs_g":null,"fat_g":5,"sat_fat_g":1,"fiber_g":10,"needs_fraction":true}],"text":"","widgets":[]}`

func TestNullEditThenUndoAgreesEverywhere(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return oatsFibre10 }
	p := decode[LogResponse](t, h.logText("60606060-0001", "oats"))
	id := p.Items[0].ItemID
	h.mutate("fraction", "60606060-0002", id, f64(0.5)) // fibre -5
	h.vars.edit(*p.Items[0].ValueID, map[string]any{"fiber_g": nil}, false)
	h.clk.Add(61 * time.Second)
	m := decode[MutationResponse](t, h.mutate("undo", "60606060-0003", id, nil))
	fib := macro(m.Snapshot, "fiber_g")
	if fib.Consumed != 0 || fib.UnknownRows != 0 || !m.Item.Effective.Fiber.OK || m.Item.Effective.Fiber.V != 0 {
		t.Fatalf("snapshot fibre %+v effective %+v", fib, m.Item.Effective.Fiber)
	}
}

func TestFailedUndoPostKeepsFibreUnknown(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("61616161-0001", "pizza"))
	h.vars.onPost = func(int, map[string]any) (bool, int) { return false, 500 }
	r := h.mutate("undo", "61616161-0002", p.Items[0].ItemID, nil)
	if r.Code != 202 {
		t.Fatalf("undo %d", r.Code)
	}
	m := decode[MutationResponse](t, r)
	if m.Item.Effective.Fiber.OK || macro(m.Snapshot, "fiber_g").UnknownRows != 1 {
		t.Fatalf("effective fibre %+v, unknown rows %d", m.Item.Effective.Fiber, macro(m.Snapshot, "fiber_g").UnknownRows)
	}
}

func TestCoachEventWaitsForDoneAndCarriesMacros(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) {
		if n == 1 {
			return true, 500
		}
		return true, 201
	}
	resp := decode[LogResponse](t, h.logText("62626262-0001", "250 g skyr and 30 g walnuts"))
	path := filepath.Join(h.opts.StateDir, "coach-events.jsonl")
	if b, _ := os.ReadFile(path); strings.Contains(string(b), resp.EntryID) {
		t.Fatal("coach event written while pending")
	}
	h.restart()
	h.clk.Add(61 * time.Second)
	h.svc.reconcileOnce(context.Background())
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), resp.EntryID) || !strings.Contains(string(b), `"protein_g":27.5`) {
		t.Fatalf("coach events: %s", b)
	}
}

func TestStscOverrunRejected(t *testing.T) {
	b := append([]byte(nil), fixture(t, "tone.m4a")...)
	i := bytes.Index(b, []byte("stsc"))
	// First run's samples_per_chunk: +4 type, +4 v/f, +4 count, +4 first.
	binary.BigEndian.PutUint32(b[i+16:], 100000)
	start := time.Now()
	if _, _, err := sniffAudio(b); err == nil {
		t.Fatal("inflated samples_per_chunk accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("validation too slow")
	}
}

func TestFeedRefreshesOlderDays(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(map[string]string{"client_id": "63636363-0001", "text": "skyr", "local_time": "2026-09-30T09:00:00+02:00"})
	p := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"))
	h.clk.Add(40 * time.Hour) // the entry's day is now older than yesterday
	h.vars.edit(*p.Items[0].ValueID, nil, true)
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	for _, it := range feed.Items {
		for _, st := range it.Items {
			if st.ItemID == p.Items[0].ItemID && st.Effective.Kcal.V != 0 {
				t.Fatalf("deleted item still shows %v kcal", st.Effective.Kcal.float())
			}
		}
	}
}

func TestModelSeesRefreshedSnapshot(t *testing.T) {
	h := newHarness(t)
	h.clk.Add(2 * time.Minute) // the cache is stale
	h.vars.add("var-food", "2026-10-01", map[string]any{"item": "outside", "kcal": 500.0, "protein_g": 50.0, "carbs_g": 0.0, "fat_g": 0.0, "sat_fat_g": 0.0, "fiber_g": 0.0})
	var seen float64
	h.model.fn = func(in ModelInput) string {
		seen = macro(*in.Snapshot, "protein_g").Consumed
		return `{"intent":"question","items":[],"text":"ok","widgets":[]}`
	}
	h.logText("64646464-0001", "how is my day?")
	if seen != 50 {
		t.Fatalf("model saw protein %v", seen)
	}
}

func TestEffectiveMatchesSnapshotWhenReadsFail(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.vars.onPost = func(int, map[string]any) (bool, int) { return false, 500 }
	h.clk.Add(2 * time.Minute)
	resp := decode[LogResponse](t, h.logText("70707070-0001", "250 g skyr and 30 g walnuts"))
	for _, it := range resp.Items {
		if it.Effective.Kcal.V != 0 {
			t.Fatalf("uncommitted item shows %v kcal", it.Effective.Kcal.float())
		}
	}
	if macro(resp.Snapshot, "kcal").Consumed != 0 {
		t.Fatal("snapshot counts uncommitted rows")
	}
}

func TestFeedRefreshesEveryStaleDay(t *testing.T) {
	h := newHarness(t)
	var first LogResponse
	for i := 0; i < 7; i++ {
		lt := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, -i).Format(time.RFC3339)
		// Walk the clock so each local_time is within 48 h.
		h.clk.Set(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, -i))
		b, _ := json.Marshal(map[string]string{"client_id": fmt.Sprintf("71717171-%04d", i), "text": "skyr", "local_time": lt})
		r := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"))
		if i == 6 {
			first = r
		}
	}
	h.clk.Set(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	h.vars.edit(*first.Items[0].ValueID, nil, true) // the oldest day
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed?limit=100", nil, ""))
	for _, it := range feed.Items {
		for _, st := range it.Items {
			if st.ItemID == first.Items[0].ItemID && st.Effective.Kcal.V != 0 {
				t.Fatal("the oldest day was not refreshed")
			}
		}
	}
}

func TestEntryPollIsConsistent(t *testing.T) {
	h := newHarness(t)
	h.vars.onPost = func(int, map[string]any) (bool, int) { return true, 500 }
	resp := decode[LogResponse](t, h.logText("72727272-0001", "250 g skyr and 30 g walnuts"))
	h.vars.onPost = nil
	h.clk.Add(61 * time.Second)
	done := make(chan struct{})
	go func() { h.svc.reconcileOnce(context.Background()); close(done) }()
	for {
		e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
		if e.Status == "done" && macro(e.Snapshot, "protein_g").Consumed != 32.1 {
			t.Fatalf("done with protein %v", macro(e.Snapshot, "protein_g").Consumed)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestMalformedStscBoundariesNoPanic(t *testing.T) {
	good := fixture(t, "tone.m4a")
	i := bytes.Index(good, []byte("stsc"))
	n := int(binary.BigEndian.Uint32(good[i+8:]))
	for _, v := range []uint32{0, 2, 3, 1 << 30} {
		b := append([]byte(nil), good...)
		binary.BigEndian.PutUint32(b[i+12:], v) // the first run's first_chunk
		if v == 1 {
			continue
		}
		if _, _, err := sniffAudio(b); err == nil && v != 1 {
			t.Errorf("first_chunk %d accepted", v)
		}
	}
	_ = n
	b := append([]byte(nil), good...)
	j := bytes.Index(b, []byte("stsz"))
	binary.BigEndian.PutUint32(b[j+12:], 20001) // sample count over the cap
	if _, _, err := sniffAudio(b); err == nil {
		t.Error("oversized sample table accepted")
	}
}

type slowASR struct{ d time.Duration }

func (a slowASR) Transcribe(ctx context.Context, path string) (media.Transcript, error) {
	select {
	case <-time.After(a.d):
		return media.Transcript{Text: "eggs"}, nil
	case <-ctx.Done():
		return media.Transcript{}, ctx.Err()
	}
}

func TestBudgetExpiryInASRIs504(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Budget = 200 * time.Millisecond; o.ASR = slowASR{time.Second} })
	body, ct := multipartBody(t, map[string]string{"client_id": "80808080-0001"}, []filePart{{"audio", "a.m4a", "audio/m4a", fixture(t, "tone.m4a")}})
	if r := h.do("POST", "/fuel/log", body, ct); r.Code != 504 || len(h.svc.journal.Entries()) != 0 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestBudgetExpiryInMutationReadIs504(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("81818181-0001", "250 g skyr and 30 g walnuts"))
	h.svc.o.Budget = 100 * time.Millisecond
	h.svc.cache.readTO = time.Second
	hold := make(chan struct{})
	h.vars.mu.Lock() // every read now blocks until released
	go func() { <-hold; h.vars.mu.Unlock() }()
	r := h.mutate("undo", "81818181-0002", resp.Items[0].ItemID, nil)
	close(hold)
	if r.Code != 504 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	n := 0
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "undo" {
			n++
		}
	}
	if n != 0 {
		t.Fatal("undo journaled after a 504")
	}
}

func TestFeedOlderThan35DaysAfterRestart(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("82828282-0001", "250 g skyr and 30 g walnuts"))
	h.clk.Add(36 * 24 * time.Hour)
	h.restart()
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	for _, it := range feed.Items {
		for _, st := range it.Items {
			if st.ItemID == resp.Items[0].ItemID && st.Effective.Kcal.float() != 157.5 {
				t.Fatalf("old item shows %v kcal", st.Effective.Kcal.float())
			}
		}
	}
}

func TestModelRequestCarriesTheWholeSnapshot(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()
	kg := 79.9
	o := &OpenAI{Key: "k", Model: "m", BaseURL: srv.URL, Client: loopbackClient()}
	_, _ = o.Estimate(context.Background(), ModelInput{Text: "how is my weight?", Snapshot: &Snapshot{Date: "2026-10-01", Weight: Weight{Avg7Kg: &kg}, Streaks: Streaks{Protein: 4}}})
	for _, want := range []string{"avg7_kg", "79.9", "streaks", "body_fat", "next_action", "day_score"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("model request lacks %s", want)
		}
	}
}

func TestRenderNeverMixesTwoViews(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	resp := decode[LogResponse](t, h.logText("83838383-0001", "pizza"))
	vid := *resp.Items[0].ValueID
	stop := make(chan struct{})
	go func() {
		kcal := 801.0
		for {
			select {
			case <-stop:
				return
			default:
			}
			kcal = 1602 - kcal + 1 // flips between two values
			h.vars.edit(vid, map[string]any{"kcal": kcal}, false)
			_ = h.svc.cache.RefreshDay(context.Background(), "2026-10-01")
		}
	}()
	for i := 0; i < 200; i++ {
		e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
		if e.Items[0].Effective.Kcal.float() != macro(e.Snapshot, "kcal").Consumed {
			close(stop)
			t.Fatalf("item %v vs snapshot %v", e.Items[0].Effective.Kcal.float(), macro(e.Snapshot, "kcal").Consumed)
		}
	}
	close(stop)
}

func TestOldEntryFailedCorrectionIsCompensatedAfterRestart(t *testing.T) {
	// A rejected CORRECTION fails only itself (the meal stays counted); a
	// failed ORIGINAL on an entry older than 35 days is still compensated.
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("90909090-0001", "pizza"))
	h.clk.Add(36 * 24 * time.Hour)
	h.vars.mu.Lock()
	h.vars.onPost = func(_ int, data map[string]any) (bool, int) {
		if data["reason"] == "fraction" {
			return false, 400
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	if r := h.mutate("fraction", "90909090-0002", p.Items[0].ItemID, f64(0.5)); r.Code != 502 {
		t.Fatalf("fraction %d", r.Code)
	}
	h.restart()
	h.clk.Add(7 * time.Hour)
	h.svc.reconcileOnce(context.Background())
	sum := 0.0
	for _, r := range h.vars.rows("var-food") {
		sum += r["kcal"].(float64)
	}
	if e, _ := h.svc.journal.Entry(p.EntryID); e.Failed || sum != 801 {
		t.Fatalf("failed=%v kcal %v: a failed correction cancelled the meal", e.Failed, sum)
	}

	h2 := newHarness(t)
	h2.vars.onPost = func(n int, data map[string]any) (bool, int) {
		if data["item"] == "walnuts" {
			return false, 500
		}
		return true, 201
	}
	h2.logText("90909090-0101", "250 g skyr and 30 g walnuts")
	h2.clk.Add(36 * 24 * time.Hour)
	h2.restart()
	h2.clk.Add(7 * time.Hour)
	h2.svc.reconcileOnce(context.Background())
	h2.svc.reconcileOnce(context.Background())
	sum = 0
	for _, r := range h2.vars.rows("var-food") {
		sum += r["kcal"].(float64)
	}
	if toTenth(sum) != 0 {
		t.Fatalf("kcal rows sum %v on a failed 36-day-old entry", sum)
	}
}

func TestHistoricalSnapshotLoadsDaysBeforeHydration(t *testing.T) {
	h := newHarness(t)
	good := map[string]any{"item": "x", "kcal": 2300.0, "protein_g": 170.0, "carbs_g": 100.0, "fat_g": 60.0, "sat_fat_g": 15.0, "fiber_g": 40.0}
	// Rows on 2026-08-26 .. 2026-08-28: before today-34 (2026-08-28 is
	// today-34; the two days before it are outside the hydration).
	for _, d := range []string{"2026-08-26", "2026-08-27", "2026-08-28"} {
		h.vars.add("var-food", d, good)
	}
	h.restart()
	snap := decode[Snapshot](t, h.do("GET", "/fuel/snapshot?date=2026-08-28", nil, ""))
	if snap.Streaks.Protein != 3 {
		t.Fatalf("protein streak %d, want 3 (days before the hydration window)", snap.Streaks.Protein)
	}
}

func TestFeedCardsAreConsistentDuringUndo(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("91919191-0001", "250 g skyr and 30 g walnuts"))
	done := make(chan struct{})
	go func() { h.mutate("undo", "91919191-0002", resp.Items[0].ItemID, nil); close(done) }()
	for {
		feed := decode[struct {
			Items []feedOut `json:"items"`
		}](t, h.do("GET", "/fuel/feed", nil, ""))
		for _, it := range feed.Items {
			for _, st := range it.Items {
				if st.ItemID != resp.Items[0].ItemID {
					continue
				}
				canUndo := len(st.Actions) > 0 && st.Actions[0] == "undo"
				if canUndo && st.Effective.Kcal.V == 0 {
					t.Fatal("card offers undo while showing the undone contribution")
				}
			}
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func TestRecoveredOldMutationLineHasRealTotals(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("93939393-0001", "250 g skyr and 30 g walnuts"))
	h.clk.Add(40 * 24 * time.Hour)
	h.mutate("undo", "93939393-0002", resp.Items[1].ItemID, nil)
	h.svc.Close()
	path := filepath.Join(h.opts.StateDir, "feed.jsonl")
	b, _ := os.ReadFile(path)
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(line, `"key":"93939393-0002"`) {
			keep = append(keep, line)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
	h.start()
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	last := feed.Items[len(feed.Items)-1]
	var kcal float64 = -1
	for _, bl := range last.Blocks {
		if bl.Widget == "macros_today" {
			for _, m := range bl.Data.(map[string]any)["macros"].([]any) {
				mm := m.(map[string]any)
				if mm["key"] == "kcal" {
					kcal = mm["consumed"].(float64)
				}
			}
		}
	}
	if kcal != 157.5 {
		t.Fatalf("recovered widget kcal %v, want 157.5 (skyr only)", kcal)
	}
}

type blockingWriter struct {
	h       http.Header
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Header() http.Header { return b.h }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return len(p), nil
}

func TestFeedWriteDoesNotHoldTheRenderLock(t *testing.T) {
	h := newHarness(t)
	h.logText("94949494-0001", "250 g skyr and 30 g walnuts")
	bw := &blockingWriter{h: http.Header{}, release: make(chan struct{}), started: make(chan struct{})}
	req := httptest.NewRequest("GET", "/fuel/feed", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	go h.h.ServeHTTP(bw, req)
	<-bw.started
	got := make(chan struct{})
	go func() { h.svc.stateMu.Lock(); h.svc.stateMu.Unlock(); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the render lock is held across the network write")
	}
	close(bw.release)
}

func TestNothingPersistedFromIncompleteHistory(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart() // hydration and the body read fail
	resp := decode[LogResponse](t, h.logText("95959595-0001", "250 g skyr and 30 g walnuts"))
	if h.svc.feed.HasKey("r:"+resp.EntryID) || h.svc.coachSeen[resp.EntryID] {
		t.Fatal("persisted a render built from unread days")
	}
	h.vars.failReads.Store(false)
	h.clk.Add(time.Minute)
	h.svc.reconcileOnce(context.Background())
	if !h.svc.feed.HasKey("r:"+resp.EntryID) || !h.svc.coachSeen[resp.EntryID] {
		t.Fatal("recovery did not render the entry once reads worked")
	}
	if rec, ok := h.svc.idem.Get("95959595-0001"); !ok || len(rec.Response) == 0 {
		t.Fatal("recovery did not store the final response")
	}
	// The stored final response is stable across a later mutation and a restart.
	h.mutate("undo", "95959595-0002", resp.Items[0].ItemID, nil)
	h.restart()
	rep := decode[LogResponse](t, h.logText("95959595-0001", "250 g skyr and 30 g walnuts"))
	if rep.Items[0].Undone {
		t.Fatal("replay is not the stored final response")
	}
}

func TestInvalidTargetsDuringProcessing(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string {
		_ = os.WriteFile(h.opts.TargetsFile, []byte(`{"bad":1}`), 0o600)
		future := time.Now().Add(3 * time.Second)
		_ = os.Chtimes(h.opts.TargetsFile, future, future)
		return skyrWalnuts
	}
	r := h.logText("96969696-0001", "250 g skyr and 30 g walnuts")
	if r.Code != 503 || errCode(t, r) != "targets_invalid" {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if rec, ok := h.svc.idem.Get("96969696-0001"); ok && len(rec.Response) > 0 {
		t.Fatal("stored a final response rendered without targets")
	}
	if h.svc.feed.HasKey("r:" + h.svc.journal.Entries()[0].ID) {
		t.Fatal("persisted a feed line rendered without targets")
	}
}

func TestReplayFreezesTheFirstCompleteRender(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart()
	resp := decode[LogResponse](t, h.logText("97979797-0001", "250 g skyr and 30 g walnuts"))
	h.vars.failReads.Store(false)
	first := decode[LogResponse](t, h.logText("97979797-0001", "250 g skyr and 30 g walnuts"))
	h.mutate("undo", "97979797-0002", resp.Items[0].ItemID, nil)
	second := decode[LogResponse](t, h.logText("97979797-0001", "250 g skyr and 30 g walnuts"))
	if first.Items[0].Undone || second.Items[0].Undone {
		t.Fatal("replays changed after the first complete render")
	}
}

func TestDuplicateWaitsDuringVariablesPost(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	var blocked atomic.Bool
	h.vars.mu.Lock()
	h.vars.onPost = func(n int, _ map[string]any) (bool, int) {
		if blocked.CompareAndSwap(false, true) {
			<-release
		}
		return true, 201
	}
	h.vars.mu.Unlock()
	first := make(chan int)
	go func() { first <- h.logText("98989898-0001", "250 g skyr and 30 g walnuts").Code }()
	for !blocked.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	second := make(chan *httptest.ResponseRecorder)
	go func() { second <- h.logText("98989898-0001", "250 g skyr and 30 g walnuts") }()
	select {
	case r := <-second:
		t.Fatalf("duplicate answered (%d) while the first was still posting", r.Code)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if c := <-first; c != 200 {
		t.Fatalf("first %d", c)
	}
	if r := <-second; r.Code != 200 {
		t.Fatalf("second %d", r.Code)
	}
}

func TestMutationReplayReportsInvalidTargets(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p := decode[LogResponse](t, h.logText("99999999-1001", "pizza"))
	h.vars.onPost = func(int, map[string]any) (bool, int) { return true, 500 }
	h.mutate("fraction", "99999999-1002", p.Items[0].ItemID, f64(0.5)) // 202, no final response
	h.svc.mu.Lock()
	h.svc.mu.Unlock()
	h.svc.targetsMu.Lock()
	h.svc.targets, h.svc.targetsErr = nil, errors.New("broken")
	h.svc.targetsSeen, h.svc.targetsMod = true, time.Time{}
	h.svc.targetsMu.Unlock()
	_ = os.WriteFile(h.opts.TargetsFile, []byte(`{"bad":1}`), 0o600)
	future := time.Now().Add(5 * time.Second)
	_ = os.Chtimes(h.opts.TargetsFile, future, future)
	r := h.svc.rebuildMutationForTest("99999999-1002")
	if r.renderErr == nil {
		t.Fatal("render error hidden")
	}
}

func (s *Service) rebuildMutationForTest(clientID string) MutationResponse {
	r, _ := s.rebuildMutation(clientID)
	return r
}

func TestReplayRefreshesAStaleDay(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart()
	resp := decode[LogResponse](t, h.logText("12345678-0001", "250 g skyr and 30 g walnuts"))
	h.vars.failReads.Store(false)
	_ = h.svc.cache.RefreshDay(context.Background(), "2026-10-01")
	h.vars.edit(*resp.Items[0].ValueID, map[string]any{"kcal": 500.0}, false)
	h.clk.Add(61 * time.Second)
	rep := decode[LogResponse](t, h.logText("12345678-0001", "250 g skyr and 30 g walnuts"))
	if rep.Items[0].Effective.Kcal.float() != 500 {
		t.Fatalf("replay froze a stale day: %v", rep.Items[0].Effective.Kcal.float())
	}
}

func TestReplayRespectsTheBudget(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart()
	h.logText("12345678-0101", "250 g skyr and 30 g walnuts")
	h.vars.failReads.Store(false)
	h.svc.o.Budget, h.svc.o.LogBudget = 300*time.Millisecond, 300*time.Millisecond
	h.clk.Add(61 * time.Second)
	h.vars.mu.Lock() // reads hang
	start := time.Now()
	done := make(chan int)
	go func() { done <- h.logText("12345678-0101", "250 g skyr and 30 g walnuts").Code }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		h.vars.mu.Unlock()
		t.Fatal("replay ignored the request budget")
	}
	h.vars.mu.Unlock()
	if time.Since(start) > 2*time.Second {
		t.Fatal("replay too slow")
	}
}

func TestRefreshWaitHonoursTheContext(t *testing.T) {
	h := newHarness(t)
	slot := h.svc.cache.daySlot("2026-10-01")
	slot <- struct{}{} // another refresh holds the day
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.svc.cache.RefreshDay(ctx, "2026-10-01"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	<-slot
}

func TestCloseDuringStartDoesNotHang(t *testing.T) {
	h := newHarness(t)
	h.svc.Close()
	svc, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	go svc.Start(context.Background())
	done := make(chan struct{})
	go func() { svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung during Start")
	}
	h.start()
}

func TestReusedClientIDGetsItsOwnFeedLine(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return pizzaPhoto }
	p1 := decode[LogResponse](t, h.logText("13131313-1001", "pizza"))
	h.mutate("fraction", "13131313-1009", p1.Items[0].ItemID, f64(1))
	h.clk.Add(8 * 24 * time.Hour)
	b, _ := json.Marshal(map[string]string{"client_id": "13131313-1002", "text": "pizza"})
	p2 := decode[LogResponse](t, h.do("POST", "/fuel/log", bytes.NewReader(b), "application/json"))
	before := len(decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed?limit=100", nil, "")).Items)
	h.mutate("fraction", "13131313-1009", p2.Items[0].ItemID, f64(1))
	after := len(decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed?limit=100", nil, "")).Items)
	if after != before+1 {
		t.Fatalf("feed lines %d -> %d", before, after)
	}
}

func TestReplayDoesNotFreezeWhenRefreshFails(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart()
	h.logText("14141414-1001", "250 g skyr and 30 g walnuts")
	h.vars.failReads.Store(false)
	_ = h.svc.ensureHistory(context.Background(), "2026-10-01")
	h.clk.Add(61 * time.Second)
	h.vars.failReads.Store(true)
	h.logText("14141414-1001", "250 g skyr and 30 g walnuts")
	if rec, ok := h.svc.idem.Get("14141414-1001"); ok && len(rec.Response) > 0 {
		t.Fatal("froze a response while the day could not be refreshed")
	}
}

func TestItemLockWaitHonoursTheBudget(t *testing.T) {
	h := newHarness(t)
	resp := decode[LogResponse](t, h.logText("15151515-2001", "250 g skyr and 30 g walnuts"))
	h.svc.o.Budget = 200 * time.Millisecond
	l := h.svc.itemLock(resp.Items[0].ItemID)
	l.Lock()
	defer l.Unlock()
	start := time.Now()
	r := h.mutate("undo", "15151515-2002", resp.Items[0].ItemID, nil)
	if r.Code != 504 || time.Since(start) > 2*time.Second {
		t.Fatalf("%d after %v", r.Code, time.Since(start))
	}
	for _, op := range h.svc.journal.Ops() {
		if op.Reason == "undo" {
			t.Fatal("undo journaled after a 504")
		}
	}
}

func TestNewLogNotPersistedWhenTheDayCannotBeRefreshed(t *testing.T) {
	h := newHarness(t)
	h.clk.Add(2 * time.Minute) // the hydrated day is now stale
	h.vars.failReads.Store(true)
	resp := decode[LogResponse](t, h.logText("16161616-2001", "250 g skyr and 30 g walnuts"))
	if h.svc.feed.HasKey("r:" + resp.EntryID) {
		t.Fatal("persisted a feed widget from a stale day")
	}
	if rec, ok := h.svc.idem.Get("16161616-2001"); ok && len(rec.Response) > 0 {
		t.Fatal("persisted a final response from a stale day")
	}
	h.vars.failReads.Store(false)
	h.clk.Add(time.Minute)
	h.svc.reconcileOnce(context.Background())
	if !h.svc.feed.HasKey("r:" + resp.EntryID) {
		t.Fatal("recovery did not render once reads worked")
	}
}

func TestStopReachesRecoveryReads(t *testing.T) {
	h := newHarness(t)
	h.vars.failReads.Store(true)
	h.restart()
	h.logText("17171717-2001", "250 g skyr and 30 g walnuts")
	h.vars.failReads.Store(false)
	h.clk.Add(time.Minute)
	h.vars.mu.Lock() // every read now hangs
	recovered := make(chan struct{})
	go func() { h.svc.reconcileOnce(h.svc.lifecycleCtxForTest()); close(recovered) }()
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() { h.svc.Stop(); close(done) }()
	for _, ch := range []chan struct{}{done, recovered} {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			h.vars.mu.Unlock()
			t.Fatal("cancellation did not reach the hanging recovery read")
		}
	}
	h.vars.mu.Unlock()
}

func (s *Service) lifecycleCtxForTest() context.Context {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()
	return s.lctx
}

func photoOnlyLog(t *testing.T, h *harness, clientID string) *httptest.ResponseRecorder {
	body, ct := multipartBody(t, map[string]string{"client_id": clientID}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(400, 300)}})
	return h.do("POST", "/fuel/log", body, ct)
}

func TestPhotoOnlyQuestionIsRetriedAsLog(t *testing.T) {
	h := newHarness(t)
	var inputs []ModelInput
	h.model.fn = func(in ModelInput) string {
		inputs = append(inputs, in)
		if !in.ListFoods {
			return `{"intent":"question","items":[],"text":"What is this?","widgets":[]}`
		}
		return pizzaPhoto
	}
	r := photoOnlyLog(t, h, "a1a1a1a1-0001")
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if resp.Intent != "log" || len(resp.Items) != 1 || len(h.vars.rows("var-food")) != 1 {
		t.Fatalf("intent %s items %d rows %d", resp.Intent, len(resp.Items), len(h.vars.rows("var-food")))
	}
	if len(inputs) != 2 || !inputs[0].PhotoOnly || inputs[0].ListFoods || !inputs[1].ListFoods {
		t.Fatalf("model calls %+v", len(inputs))
	}
	// With a caption the model's question stands (no retry).
	h.model.fn = func(in ModelInput) string {
		if in.PhotoOnly {
			t.Error("captioned photo marked photo-only")
		}
		return `{"intent":"question","items":[],"text":"Looks tasty.","widgets":[]}`
	}
	body, ct := multipartBody(t, map[string]string{"client_id": "a1a1a1a1-0002", "text": "is this healthy?"}, []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(400, 300)}})
	if q := decode[LogResponse](t, h.do("POST", "/fuel/log", body, ct)); q.Intent != "question" {
		t.Fatalf("captioned question became %s", q.Intent)
	}
}

func TestPhotoOnlyNoFoodAnswer(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.model.fn = func(in ModelInput) string {
		calls++
		return `{"intent":"question","items":[],"text":"A picture of a wall.","widgets":[]}`
	}
	r := photoOnlyLog(t, h, "a2a2a2a2-0001")
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	resp := decode[LogResponse](t, r)
	if resp.Intent != "log" || len(resp.Items) != 0 || calls != 2 {
		t.Fatalf("intent %s items %d calls %d", resp.Intent, len(resp.Items), calls)
	}
	if resp.Blocks[0].Type != "text" || resp.Blocks[0].Text != noFoodText {
		t.Fatalf("first block %+v", resp.Blocks[0])
	}
	if h.vars.posts != 0 || resp.Snapshot.Revision != 0 {
		t.Fatalf("posts %d revision %d", h.vars.posts, resp.Snapshot.Revision)
	}
	if !h.svc.feed.HasKey("r:"+resp.EntryID) || !h.svc.feed.HasKey("u:"+resp.EntryID) {
		t.Fatal("feed items not recorded")
	}
	if h.svc.coachSeen[resp.EntryID] {
		t.Fatal("coach event for a log with no items")
	}
	// Idempotent repeat answers the same, without a model call.
	if r2 := decode[LogResponse](t, photoOnlyLog(t, h, "a2a2a2a2-0001")); r2.EntryID != resp.EntryID || calls != 2 {
		t.Fatal("repeat not idempotent")
	}
}

func TestPhotoOnlyEmptyLogOutputsTakeTheSamePaths(t *testing.T) {
	emptyLog := `{"intent":"log","items":[],"text":"","widgets":[]}`
	emptyQ := `{"intent":"question","items":[],"text":"?","widgets":[]}`
	for i, seq := range [][2]string{{emptyLog, pizzaPhoto}, {emptyQ, emptyLog}, {emptyLog, emptyLog}, {emptyLog, emptyQ}} {
		h := newHarness(t)
		n := 0
		seq := seq
		h.model.fn = func(ModelInput) string { n++; return seq[(n-1)%2] }
		r := photoOnlyLog(t, h, fmt.Sprintf("a3a3a3a3-%04d", i))
		if r.Code != 200 {
			t.Fatalf("seq %d: %d %s", i, r.Code, r.Body)
		}
		resp := decode[LogResponse](t, r)
		wantItems := 0
		if i == 0 {
			wantItems = 1
		}
		if resp.Intent != "log" || len(resp.Items) != wantItems || n != 2 {
			t.Fatalf("seq %d: intent %s items %d calls %d", i, resp.Intent, len(resp.Items), n)
		}
		if wantItems == 0 && resp.Blocks[0].Text != noFoodText {
			t.Fatalf("seq %d: %q", i, resp.Blocks[0].Text)
		}
	}
	// A non-photo empty log is still invalid output.
	if _, err := validateOutput(json.RawMessage(emptyLog)); err == nil {
		t.Fatal("empty log accepted outside the photo-only path")
	}
}

func TestNoFoodEntrySurvivesRecovery(t *testing.T) {
	h := newHarness(t)
	h.model.fn = func(ModelInput) string { return `{"intent":"question","items":[],"text":"?","widgets":[]}` }
	resp := decode[LogResponse](t, photoOnlyLog(t, h, "a4a4a4a4-0001"))
	e := decode[entryResponse](t, h.do("GET", "/fuel/entry/"+resp.EntryID, nil, ""))
	if e.Blocks[0].Text != noFoodText {
		t.Fatalf("entry fetch %q", e.Blocks[0].Text)
	}
	// Crash after the journal txn: lose the feed and the final response.
	h.svc.Close()
	_ = os.Remove(filepath.Join(h.opts.StateDir, "feed.jsonl"))
	_ = os.Remove(filepath.Join(h.opts.StateDir, "idem.jsonl"))
	h.start()
	for i := 0; i < 2; i++ {
		h.clk.Add(time.Minute)
		h.svc.reconcileOnce(context.Background())
	}
	if !h.svc.feed.HasKey("u:"+resp.EntryID) || !h.svc.feed.HasKey("r:"+resp.EntryID) {
		t.Fatal("feed not rebuilt")
	}
	feed := decode[struct {
		Items []feedOut `json:"items"`
	}](t, h.do("GET", "/fuel/feed", nil, ""))
	if len(feed.Items) != 2 || feed.Items[1].Blocks[0].Text != noFoodText {
		t.Fatalf("feed %+v", feed.Items)
	}
	rec, ok := h.svc.idem.Get("a4a4a4a4-0001")
	if !ok || !strings.Contains(string(rec.Response), "could not see any food") {
		t.Fatal("final response not recovered with the no-food text")
	}
	if h.vars.posts != 0 || h.svc.coachSeen[resp.EntryID] {
		t.Fatalf("posts %d coach %v", h.vars.posts, h.svc.coachSeen[resp.EntryID])
	}
}

func TestPhotoOnlyQuestionWithItemsIsRetriedAndKeepsFoods(t *testing.T) {
	qWithItems := strings.Replace(pizzaPhoto, `"intent":"log"`, `"intent":"question"`, 1)
	oatsQ := strings.Replace(oatsFibre10, `"intent":"log"`, `"intent":"question"`, 1)
	for i, second := range []string{`{"intent":"log","items":[],"text":"","widgets":[]}`, oatsFibre10, oatsQ} {
		h := newHarness(t)
		n := 0
		second := second
		h.model.fn = func(in ModelInput) string {
			n++
			if n == 1 {
				return qWithItems
			}
			return second
		}
		r := photoOnlyLog(t, h, fmt.Sprintf("a5a5a5a5-%04d", i))
		resp := decode[LogResponse](t, r)
		if r.Code != 200 || n != 2 || resp.Intent != "log" || len(resp.Items) != 1 {
			t.Fatalf("case %d: code %d calls %d intent %s items %d", i, r.Code, n, resp.Intent, len(resp.Items))
		}
		want := []string{"pizza margherita", "oats", "oats"}[i]
		rows := h.vars.rows("var-food")
		if resp.Items[0].Item != want || len(rows) != 1 || rows[0]["item"] != want {
			t.Fatalf("case %d: logged %q rows %d, want %q", i, resp.Items[0].Item, len(rows), want)
		}
		photoOnlyLog(t, h, fmt.Sprintf("a5a5a5a5-%04d", i)) // repeat: idempotent
		if n != 2 || len(h.vars.rows("var-food")) != 1 {
			t.Fatalf("case %d: repeat made calls %d rows %d", i, n, len(h.vars.rows("var-food")))
		}
	}
}

func TestRetryInstructionIsInTheSystemMessage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`))
	}))
	defer srv.Close()
	o := &OpenAI{Key: "k", Model: "m", BaseURL: srv.URL, Client: loopbackClient()}
	_, _ = o.Estimate(context.Background(), ModelInput{Images: [][]byte{{0xff, 0xd8}}, PhotoOnly: true, ListFoods: true})
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	user, _ := json.Marshal(msgs[1])
	if !strings.Contains(sys, "Retry for this request") || !strings.Contains(sys, "NO caption") {
		t.Fatal("retry instructions not in the system message")
	}
	if strings.Contains(string(user), "Retry for this request") || !strings.Contains(string(user), "image_url") {
		t.Fatal("user message carries the instruction or lacks the image")
	}
}

func TestPhotoOnlyClassificationBoundaries(t *testing.T) {
	for i, c := range []struct {
		asr, text string
		audio     bool
		photoOnly bool
	}{
		{"", "", true, true},
		{"   ", "", true, true},
		{"two eggs", "", true, false},
		{"", "   ", false, true},
		{"", "lunch", false, false},
	} {
		c := c
		h := newHarness(t, func(o *Options) { o.ASR = fakeASR{text: c.asr} })
		var saw *bool
		h.model.fn = func(in ModelInput) string {
			if saw == nil {
				v := in.PhotoOnly
				saw = &v
			}
			return pizzaPhoto
		}
		fields := map[string]string{"client_id": fmt.Sprintf("a6a6a6a6-%04d", i)}
		if c.text != "" {
			fields["text"] = c.text
		}
		files := []filePart{{"image", "a.jpg", "image/jpeg", testJPEG(200, 200)}}
		if c.audio {
			files = append(files, filePart{"audio", "a.m4a", "audio/m4a", fixture(t, "tone.m4a")})
		}
		body, ct := multipartBody(t, fields, files)
		if r := h.do("POST", "/fuel/log", body, ct); r.Code != 200 {
			t.Fatalf("case %d: %d %s", i, r.Code, r.Body)
		}
		if saw == nil || *saw != c.photoOnly {
			t.Fatalf("case %d: photoOnly %v, want %v", i, saw, c.photoOnly)
		}
	}
}
