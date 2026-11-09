package fuel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

func newLocalHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	h := newHarnessMode(t, true, append([]func(*Options){func(o *Options) { o.StoreMode = "local" }}, opts...)...)
	h.svc.Stop() // deterministic: background passes are explicitly driven by tests
	return h
}
func localStats(t *testing.T, h *harness) sqlstore.LocalStatus {
	t.Helper()
	return decode[sqlstore.LocalStatus](t, h.get("/fuel/admin/store-status"))
}
func localCheck(t *testing.T, r *httptest.ResponseRecorder) {
	t.Helper()
	if r.Code != http.StatusOK {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}
func TestLocalOutageScriptedDayRestartAndCatchup(t *testing.T) {
	h := newLocalHarness(t)
	h.vars.failReads.Store(true)
	loggedRaw := h.logText("local001-log1", "skyr and walnuts")
	localCheck(t, loggedRaw)
	logged := decode[LogResponse](t, loggedRaw)
	if len(logged.Items) != 2 || logged.Status != StatusDone {
		t.Fatalf("log %+v", logged)
	}
	h.clk.Add(time.Minute)
	localCheck(t, h.fix("local001-fix1", logged.Items[0].ItemID, map[string]any{"share": 0.5}))
	h.clk.Add(time.Minute)
	localCheck(t, h.mutate("undo", "local001-undo", logged.Items[1].ItemID, nil))
	h.clk.Add(time.Minute)
	localCheck(t, h.post("/fuel/move", map[string]any{"client_id": "local001-move", "item_id": logged.Items[0].ItemID, "day": "yesterday"}))
	if len(h.day("").Items) != 0 || len(h.day("?date=2026-09-30").Items) != 1 {
		t.Fatalf("move: today %+v yesterday %+v", h.day(""), h.day("?date=2026-09-30"))
	}
	before := h.snap(t, "?date=2026-09-30")
	st := localStats(t, h)
	if st.Pending != 6 {
		t.Fatalf("pending %+v", st)
	}
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	st = localStats(t, h)
	if st.Pending != 6 || st.ErrorCount != 6 || st.LastError == "" {
		t.Fatalf("outage %+v", st)
	}
	if len(h.vars.rows("var-food")) != 0 {
		t.Fatal("request contacted Variables")
	}
	// Delete every legacy replay file: SQLite alone must preserve the request.
	for _, name := range []string{"journal", "idem", "feed", "records"} {
		_ = os.Remove(filepath.Join(h.opts.StateDir, name+".jsonl"))
	}
	h.restart()
	h.svc.Stop()
	if st := localStats(t, h); st.Pending != 6 {
		t.Fatalf("restart %+v", st)
	}
	after := h.snap(t, "?date=2026-09-30")
	if !reflect.DeepEqual(before.Macros, after.Macros) || before.Revision != after.Revision {
		t.Fatalf("restart totals changed: %+v %+v", before.Macros, after.Macros)
	}
	localCheck(t, h.logText("local001-log1", "skyr and walnuts"))
	if localStats(t, h).Pending != 6 {
		t.Fatal("retry wrote twice")
	}
	h.vars.failReads.Store(false)
	h.clk.Add(time.Hour)
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if st := localStats(t, h); st.Pending != 0 {
		t.Fatalf("catchup %+v", st)
	}
	if len(h.vars.rows("var-food")) != 6 {
		t.Fatalf("exported %d rows", len(h.vars.rows("var-food")))
	}
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	final := h.snap(t, "?date=2026-09-30")
	if !reflect.DeepEqual(before.Macros, final.Macros) {
		t.Fatal("echo changed totals")
	}
	if err := ExportLocalLegacy(context.Background(), h.svc.o.StorePath, filepath.Join(h.dir, "rollback")); err != nil {
		t.Fatal(err)
	}
}
func TestLocalExportDisabledRecordsAndRelog(t *testing.T) {
	no := false
	h := newLocalHarness(t, recOpts, func(o *Options) { o.ExportVariables = &no })
	h.vars.failReads.Store(true)
	r := h.logText("local002-log1", "skyr and walnuts")
	localCheck(t, r)
	recent := h.recent("")
	if len(recent.Items) == 0 {
		t.Fatal("no recent")
	}
	localCheck(t, h.post("/fuel/relog", map[string]any{"client_id": "local002-redo", "key": recent.Items[0].Key}))
	code, record, raw := h.record("blood_pressure", bp(120, 80), "")
	if code != 200 || record.Status != StatusDone {
		t.Fatalf("record %d %s", code, raw)
	}
	localCheck(t, h.post("/fuel/record/void", map[string]any{"client_id": "local002-void", "record_id": record.Record.RecordID}))
	if got := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(got.Records) != 0 {
		t.Fatalf("void %+v", got)
	}
	if st := localStats(t, h); st.Pending != 0 {
		t.Fatalf("H4 %+v", st)
	}
	pending, err := h.svc.local.db.PendingExports(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("H4 outbox %+v %v", pending, err)
	}
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if len(h.vars.rows("var-food"))+len(h.vars.rows("var-bp")) != 0 {
		t.Fatal("H4 exported")
	}
	h.restart()
	h.svc.Stop()
	if got := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(got.Records) != 0 {
		t.Fatal("restart lost void")
	}
	if err := ExportLocalLegacy(context.Background(), h.svc.o.StorePath, filepath.Join(h.dir, "rollback")); err == nil {
		t.Fatal("rollback allowed unexported H4 values")
	}
}
func TestLocalForeignImportEditsDeletesAndExternalInputs(t *testing.T) {
	h := newLocalHarness(t)
	food := map[string]any{"op_id": "foreign-op", "item": "foreign apple", "kcal": 100, "protein_g": 1, "source": "agentd"}
	h.addAt("var-food", "2026-10-01", food)
	h.addAt("var-body", "2026-10-01", map[string]any{"weight_kg": 80, "fat_pct": 20, "method": "scale"})
	h.vars.mu.Lock()
	h.vars.vals = append(h.vars.vals, fakeValue{ID: "external-sets", VariableID: "var-push", Data: "12", RecordDate: "2026-10-01", CreatedAt: h.clk.Now()})
	h.vars.mu.Unlock()
	for i := 0; i < 2; i++ {
		if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	_ = h.svc.cache.RefreshBody(context.Background())
	if len(h.day("").Items) != 1 {
		t.Fatal("foreign import missing or doubled")
	}
	nums := h.svc.cache.numsLocked("2026-10-01")
	if len(nums) != 1 || nums[0].Raw != "12" {
		t.Fatalf("numeric input %+v", nums)
	}
	body, err := h.svc.local.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range body {
		if v.VariableID == h.opts.BodyVar && v.Data["weight_kg"] == float64(80) {
			found = true
		}
	}
	if !found {
		t.Fatal("body import missing")
	}
	h.vars.mu.Lock()
	for i := range h.vars.vals {
		if h.vars.vals[i].VariableID == "var-food" {
			food["kcal"] = 200
			b, _ := json.Marshal(food)
			h.vars.vals[i].Data = string(b)
		}
	}
	h.vars.mu.Unlock()
	if err := h.svc.local.importOnce(context.Background(), "2026-10-01", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	vals, _ := h.svc.local.ByDate(context.Background(), "2026-10-01")
	for _, v := range vals {
		if v.VariableID == h.opts.FoodVar && v.Data["kcal"] != float64(200) {
			t.Fatal("foreign edit missing")
		}
	}
	h.vars.failReads.Store(true)
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err == nil {
		t.Fatal("expected outage")
	}
	if len(h.day("").Items) != 1 {
		t.Fatal("failed scan deleted food")
	}
	h.vars.failReads.Store(false)
	h.vars.mu.Lock()
	h.vars.vals = nil
	h.vars.mu.Unlock()
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if len(h.day("").Items) != 0 {
		t.Fatal("successful scan did not reconcile deletion")
	}
}
func checkLocalReadIsolation(t *testing.T, h *harness, count *atomic.Int64) {
	t.Helper()
	localCheck(t, h.logText("local003-log1", "skyr and walnuts"))
	count.Store(0)
	for _, path := range []string{"/fuel/snapshot", "/fuel/day", "/fuel/week", "/fuel/recent", "/fuel/feed", "/fuel/records?type=blood_pressure", "/fuel/gauges.png"} {
		localCheck(t, h.get(path))
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("read paths made %d Variables requests", n)
	}
}
func TestLocalReadsNeverCallVariables(t *testing.T) {
	var count atomic.Int64
	h := newLocalHarness(t, recOpts)
	remote := h.svc.local.remote.(*VariablesHTTP)
	old := remote.Client.Transport
	remote.Client = &http.Client{Transport: countReads{next: old, n: &count}}
	checkLocalReadIsolation(t, h, &count)
}

type countReads struct {
	next http.RoundTripper
	n    *atomic.Int64
}

func (c countReads) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == "GET" {
		c.n.Add(1)
	}
	return c.next.RoundTrip(r)
}

// The box runs the same isolation assertion against a real httptest server.
func TestLocalReadsNeverCallVariablesTCP(t *testing.T) {
	var count atomic.Int64
	h := newLocalHarness(t, recOpts)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			count.Add(1)
		}
		h.vars.ServeHTTP(w, r)
	}))
	defer srv.Close()
	h.svc.local.remote = &VariablesHTTP{Base: srv.URL, Client: srv.Client()}
	checkLocalReadIsolation(t, h, &count)
}
func TestLocalLostExportReplyReconcilesAndBacksOff(t *testing.T) {
	h := newLocalHarness(t)
	localCheck(t, h.logText("local004-log1", "skyr and walnuts"))
	h.vars.onPost = func(int, map[string]any) (bool, int) { return true, 500 }
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if st := localStats(t, h); st.Pending != 2 || st.ErrorCount != 2 {
		t.Fatalf("lost reply %+v", st)
	}
	h.vars.mu.Lock()
	posts := h.vars.posts
	h.vars.mu.Unlock()
	_ = h.svc.local.exportOnce(context.Background(), h.clk.Now())
	h.vars.mu.Lock()
	after := h.vars.posts
	h.vars.mu.Unlock()
	if after != posts {
		t.Fatal("backoff ignored")
	}
	h.restart()
	h.svc.Stop()
	h.clk.Add(time.Minute)
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if localStats(t, h).Pending != 0 || len(h.vars.rows("var-food")) != 2 {
		t.Fatal("reconciliation duplicated or lost rows")
	}
}

func TestLocalConfig(t *testing.T) {
	for _, text := range []string{`store_mode = "local"`, "store_mode = \"local\"\nexport_variables = true"} {
		c, err := ParseDaemonConfig([]byte(text))
		if err != nil || c.StoreMode != "local" || !c.ExportVariables {
			t.Fatalf("config %+v %v", c, err)
		}
	}
	c, err := ParseDaemonConfig([]byte("store_mode = \"local\"\nexport_variables = false"))
	if err != nil || c.ExportVariables {
		t.Fatalf("H4 %+v %v", c, err)
	}
	if _, err := ParseDaemonConfig([]byte(`export_variables = "false"`)); err == nil {
		t.Fatal("accepted string boolean")
	}
}
func TestLocalStartupDoesNotReadVariables(t *testing.T) {
	h := newLocalHarness(t)
	var count atomic.Int64
	remote := h.opts.Vars.(*VariablesHTTP)
	remote.Client = &http.Client{Transport: countReads{next: remote.Client.Transport, n: &count}}
	h.svc.Close()
	s, err := New(h.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.hydrate(context.Background())
	if count.Load() != 0 {
		t.Fatalf("startup made %d remote reads", count.Load())
	}
}
func TestLocalRecordExportAndRollbackGate(t *testing.T) {
	h := newLocalHarness(t, recOpts)
	code, r, raw := h.record("blood_pressure", bp(120, 80), "")
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	localCheck(t, h.post("/fuel/record/void", map[string]any{"client_id": "local005-void", "record_id": r.Record.RecordID}))
	if st := localStats(t, h); st.Pending != 2 {
		t.Fatalf("records not queued %+v", st)
	}
	if err := ExportLocalLegacy(context.Background(), h.svc.o.StorePath, filepath.Join(h.dir, "rollback")); err == nil {
		t.Fatal("rollback accepted pending outbox")
	}
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if st := localStats(t, h); st.Pending != 0 || len(h.vars.rows("var-bp")) != 2 {
		t.Fatalf("record export %+v", st)
	}
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if got := decode[recList](t, h.get("/fuel/records?type=blood_pressure")); len(got.Records) != 0 {
		t.Fatal("echo resurrected void")
	}
}
func TestLocalForeignDuplicateAndCorrectionAliases(t *testing.T) {
	h := newLocalHarness(t)
	original := map[string]any{"op_id": "foreign-double", "item": "apple", "kcal": 100, "portion_g": 100}
	h.addAt("var-food", "2026-10-01", original)
	h.addAt("var-food", "2026-10-01", original)
	rows := h.vars.rows("var-food")
	alias := rows[1]["_id"].(string)
	h.addAt("var-food", "2026-10-01", map[string]any{"op_id": "foreign-fix", "corrects": alias, "kcal": -50})
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	vals, _ := h.svc.local.ByDate(context.Background(), "2026-10-01")
	if got := totalsFromRows(vals); got.Sum["kcal"] != 500 {
		t.Fatalf("foreign duplicate totals %+v", got)
	}
	// Exported Fuel aliases must also join external corrections to their original.
	localCheck(t, h.logText("local006-log1", "skyr and walnuts"))
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	rows = h.vars.rows("var-food")
	own := rows[len(rows)-1]
	h.addAt("var-food", "2026-10-01", map[string]any{"op_id": "foreign-own-fix", "corrects": own["_id"], "kcal": -10})
	if err := h.svc.local.importOnce(context.Background(), "", h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	vals, _ = h.svc.local.ByDate(context.Background(), "2026-10-01")
	groups := groupRows(vals)
	for _, g := range groups {
		if g.orig.Data == nil {
			t.Fatal("correction lost Variables alias")
		}
	}
}
func TestLocalRollbackToShadowPreservesRevisionsAndRetries(t *testing.T) {
	h := newLocalHarness(t)
	original := h.logText("local007-log1", "skyr and walnuts")
	localCheck(t, original)
	logged := decode[LogResponse](t, original)
	localCheck(t, h.fix("local007-fix1", logged.Items[0].ItemID, map[string]any{"share": 0.5}))
	before := h.snap(t, "")
	if err := h.svc.local.exportOnce(context.Background(), h.clk.Now()); err != nil {
		t.Fatal(err)
	}
	path := h.svc.o.StorePath
	h.svc.Close()
	if err := ExportLocalLegacy(context.Background(), path, h.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	h.opts.StoreMode = "shadow"
	h.start()
	h.svc.Stop()
	after := h.snap(t, "")
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Macros, after.Macros) {
		t.Fatalf("rollback differs %d %d", before.Revision, after.Revision)
	}
	localCheck(t, h.logText("local007-log1", "skyr and walnuts"))
	if len(h.vars.rows("var-food")) != 3 {
		t.Fatal("rollback retry duplicated writes")
	}
}

func TestLocalFuelOpTurnWritesDuringOutage(t *testing.T) {
	h := newLocalHarness(t, func(o *Options) { o.Chat.OpToken = opTok })
	h.vars.failReads.Store(true)
	entry := Entry{ID: "en_local_agent", ClientID: "local008-turn", Date: "2026-10-01", EatenAt: h.clk.Now(), CreatedAt: h.clk.Now(), Intent: "log", Chat: chatAgent, Agent: agentPending}
	if err := h.svc.journal.Append(journalRec{T: "txn", Entry: &entry}); err != nil {
		t.Fatal(err)
	}
	turn := h.svc.turnOpen(entry, h.clk.Now())
	defer h.svc.turnClose(turn)
	body := map[string]any{"client_id": "local008-item", "items": []any{food("almonds", 30, 174, 6.3, 1.1)}}
	localCheck(t, h.op("POST", "/fuel/items", turn.cap, body))
	localCheck(t, h.op("POST", "/fuel/items", turn.cap, body))
	e, _ := h.svc.journal.Entry(entry.ID)
	if len(e.ItemIDs) != 1 {
		t.Fatalf("turn entry %+v", e)
	}
	localCheck(t, h.op("POST", "/fuel/fix", turn.cap, map[string]any{"client_id": "local008-fix1", "item_id": e.ItemIDs[0], "share": 0.5}))
	if st := localStats(t, h); st.Pending != 2 {
		t.Fatalf("turn outbox %+v", st)
	}
	if len(h.vars.rows("var-food")) != 0 {
		t.Fatal("turn wrote remotely")
	}
	e, _ = h.svc.journal.Entry(entry.ID)
	e.Agent = agentDone
	if err := h.svc.journal.Append(journalRec{T: "txn", Entry: &e}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.svc.Stop()
	if st := localStats(t, h); st.Pending != 2 {
		t.Fatalf("turn lost after restart %+v", st)
	}
	if len(h.day("").Items) != 1 {
		t.Fatal("turn food lost")
	}
}
func TestLocalBlockedExporterDoesNotBlockWrites(t *testing.T) {
	h := newLocalHarness(t)
	localCheck(t, h.logText("local009-log1", "skyr and walnuts"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	h.vars.onPost = func(int, map[string]any) (bool, int) {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return true, 201
	}
	done := make(chan error, 1)
	go func() { done <- h.svc.local.exportOnce(context.Background(), h.clk.Now()) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("exporter did not start")
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- h.post("/fuel/items", map[string]any{"client_id": "local009-item", "items": []any{food("almonds", 30, 174, 6.3, 1.1)}})
	}()
	select {
	case r := <-response:
		localCheck(t, r)
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatal("Variables blocked a local write")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := localStats(t, h); st.Pending != 1 {
		t.Fatalf("concurrent write missing %+v", st)
	}
}

func TestLocalPublishesRowsWithCommittedRevision(t *testing.T) {
	h := newLocalHarness(t)
	now := h.clk.Now()
	op := Op{ID: "op_publish", Kind: "original", EntryID: "en_publish", ItemID: "it_publish", Date: "2026-10-01", CreatedAt: now, State: OpPending, Data: map[string]any{"op_id": "op_publish", "item_id": "it_publish", "item": "apple", "kcal": 100}}
	h.svc.stateMu.Lock()
	err := h.svc.journal.Append(journalRec{T: "txn", At: now, Ops: []Op{op}})
	h.svc.stateMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// No postOp or read refresh has run. The committed revision and its rows
	// must already be visible together to a renderer holding stateMu.
	h.svc.stateMu.RLock()
	rows, _, _ := h.svc.cache.Rows(op.Date)
	revision := h.svc.journal.Revision(op.Date)
	h.svc.stateMu.RUnlock()
	if revision != 1 || totalsFromRows(rows).Sum["kcal"] != 1000 {
		t.Fatalf("revision %d rows %+v", revision, rows)
	}
}

func TestLocalPromotesShadowWithoutVariables(t *testing.T) {
	h := newShadowHarness(t, recOpts)
	localCheck(t, h.logText("local010-log1", "skyr and walnuts"))
	code, r, raw := h.record("blood_pressure", bp(120, 80), "")
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	before := h.snap(t, "")
	shadowDrain(t, h.svc.shadow)
	h.svc.Close()
	h.vars.failReads.Store(true)
	h.opts.StoreMode = "local"
	h.start()
	h.svc.Stop()
	after := h.snap(t, "")
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Macros, after.Macros) {
		t.Fatal("promotion changed totals or revision")
	}
	got := decode[recGet](t, h.get("/fuel/record/"+r.Record.RecordID))
	if got.Status != StatusDone {
		t.Fatalf("promoted record %+v", got)
	}
	localCheck(t, h.logText("local010-log1", "skyr and walnuts"))
	if st := localStats(t, h); st.Pending != 0 {
		t.Fatalf("promotion re-exported history %+v", st)
	}
	localCheck(t, h.logText("local010-log2", "skyr and walnuts"))
	if st := localStats(t, h); st.Pending != 2 {
		t.Fatalf("new local writes missing %+v", st)
	}
}
func TestLocalBackgroundExporterDrains(t *testing.T) {
	h := newHarnessMode(t, true, func(o *Options) { o.StoreMode = "local" })
	localCheck(t, h.logText("local011-log1", "skyr and walnuts"))
	deadline := time.Now().Add(5 * time.Second)
	for localStats(t, h).Pending != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background exporter did not drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.vars.rows("var-food")) != 2 {
		t.Fatal("background exporter lost rows")
	}
}
