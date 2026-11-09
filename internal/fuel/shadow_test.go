package fuel

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

func newShadowHarness(t *testing.T, opts ...func(*Options)) *harness {
	return newHarnessMode(t, true, append([]func(*Options){func(o *Options) { o.StoreMode = "shadow" }}, opts...)...)
}
func shadowDrain(t *testing.T, sh *shadowStore) {
	t.Helper()
	done := make(chan struct{})
	sh.enqueue(shadowJob{barrier: done})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shadow drain timed out")
	}
}
func shadowStats(t *testing.T, h *harness) shadowStatus {
	t.Helper()
	shadowDrain(t, h.svc.shadow)
	return decode[shadowStatus](t, h.get("/fuel/admin/store-status"))
}
func shadowParity(t *testing.T, h *harness, dates ...string) {
	t.Helper()
	shadowDrain(t, h.svc.shadow)
	rows, err := h.svc.shadow.db.ListRows(context.Background(), "2000-01-01", "2099-12-31")
	if err != nil {
		t.Fatal(err)
	}
	local := newCache(nil, VarIDs{}, nil, h.clk.Now)
	for _, date := range dates {
		local.days[date] = &dayCache{ok: true}
	}
	for _, r := range rows {
		if r.VariablesValueID == "" {
			continue
		} // pending writes cannot count as consumed
		var data map[string]any
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		if local.days[r.RecordDate] == nil {
			local.days[r.RecordDate] = &dayCache{ok: true}
		}
		local.days[r.RecordDate].rows = append(local.days[r.RecordDate].rows, Value{ID: r.VariablesValueID, RecordDate: r.RecordDate, CreatedAt: r.CreatedAt, Data: data})
	}
	for _, date := range dates {
		want, _, _ := h.svc.cache.Rows(date)
		got, _, _ := local.Rows(date)
		if !reflect.DeepEqual(totalsFromRows(got), totalsFromRows(want)) {
			t.Fatalf("%s shadow %+v cache %+v", date, totalsFromRows(got), totalsFromRows(want))
		}
	}
	if st := shadowStats(t, h); st.ErrorCount != 0 {
		t.Fatalf("shadow errors: %+v", st)
	}
}
func TestShadowScriptedDay(t *testing.T) {
	h := newShadowHarness(t)
	logged := decode[LogResponse](t, h.logText("shadow01-log1", "skyr and walnuts"))
	if len(logged.Items) != 2 {
		t.Fatalf("log: %+v", logged)
	}
	shadowParity(t, h, "2026-10-01")
	check := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	h.clk.Add(time.Minute)
	check(h.post("/fuel/fix", map[string]any{"client_id": "shadow01-fix1", "item_id": logged.Items[0].ItemID, "share": 0.5}))
	shadowParity(t, h, "2026-10-01")
	h.clk.Add(time.Minute)
	check(h.mutate("undo", "shadow01-undo", logged.Items[1].ItemID, nil))
	shadowParity(t, h, "2026-10-01")
	h.clk.Add(time.Minute)
	check(h.post("/fuel/move", map[string]any{"client_id": "shadow01-move", "item_id": logged.Items[0].ItemID, "day": "yesterday"}))
	shadowParity(t, h, "2026-10-01", "2026-09-30")
	h.clk.Add(time.Minute)
	recent := h.recent("")
	if len(recent.Items) == 0 {
		t.Fatal("no repeat candidate")
	}
	check(h.relog("shadow01-relog", recent.Items[0].Key, f64(1.5)))
	check(h.post("/fuel/items", map[string]any{"client_id": "shadow01-items", "items": []any{map[string]any{"item": "toast", "kcal": 100, "protein_g": 3, "carbs_g": 20, "fat_g": 1, "sat_fat_g": 0.2}}}))
	shadowParity(t, h, "2026-10-01", "2026-09-30")
	st := shadowStats(t, h)
	if !st.Available || st.LastWrite == nil || st.LastBackup != nil || st.Counts["pending"] != 0 || st.Counts["food_rows"] < 8 || st.Counts["operations"] != int64(len(h.svc.journal.Ops())) || st.Counts["feed"] != int64(len(h.svc.feed.items)) || st.Counts["idem"] != int64(len(h.svc.idem.recs)) {
		t.Fatalf("status: %+v", st)
	}
	h.restart()
	shadowParity(t, h, "2026-10-01", "2026-09-30")
	after := shadowStats(t, h)
	if st.Counts["food_rows"] != after.Counts["food_rows"] || st.Counts["journal"] != after.Counts["journal"] || st.Counts["feed"] != after.Counts["feed"] {
		t.Fatalf("replay duplicated rows: before %+v after %+v", st, after)
	}
}
func TestShadowForeignRefreshAndFullHistory(t *testing.T) {
	h := newShadowHarness(t)
	data := map[string]any{"item": "script apple", "kcal": 90, "protein_g": nil, "carbs_g": 20, "fat_g": 0, "future": map[string]any{"keep": true}, "source": "agentd"}
	h.vars.add("var-food", "2020-01-02", data)
	h.vars.add("var-food", "2026-10-01", data)
	if err := h.svc.cache.RefreshBody(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.cache.RefreshDay(context.Background(), "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	shadowParity(t, h, "2026-10-01")
	rows, err := h.svc.shadow.db.ListRows(context.Background(), "2000-01-01", "2099-12-31")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Source != sqlstore.SourceVariablesImport {
		t.Fatalf("full history: %+v", rows)
	}
	current := rows[1]
	h.vars.edit(current.VariablesValueID, map[string]any{"kcal": 110}, false)
	if err := h.svc.cache.RefreshDay(context.Background(), "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	shadowParity(t, h, "2026-10-01")
	updated, _ := h.svc.shadow.db.ListRows(context.Background(), "2026-10-01", "2026-10-01")
	if len(updated) != 1 || updated[0].ID != current.ID || updated[0].Sequence != current.Sequence {
		t.Fatalf("import lost identity: %+v", updated)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(updated[0].Data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["protein_g"]) != "null" || string(got["fat_g"]) != "0" || got["fiber_g"] != nil || got["future"] == nil {
		t.Fatalf("payload changed: %s", updated[0].Data)
	}
}
func TestShadowUncertainAndRejectedWrites(t *testing.T) {
	h := newShadowHarness(t)
	h.play(oneItem("skyr", 250, 160, 27, false))
	h.vars.onPost = func(int, map[string]any) (bool, int) { return true, 503 }
	r := h.logText("shadow02-lost", "skyr")
	if r.Code != 202 {
		t.Fatalf("uncertain %d %s", r.Code, r.Body)
	}
	if st := shadowStats(t, h); st.Counts["food_rows"] != 1 || st.Counts["pending"] != 1 {
		t.Fatalf("pending: %+v", st)
	}
	if err := h.svc.cache.RefreshDay(context.Background(), "2026-10-01"); err != nil {
		t.Fatal(err)
	}
	if st := shadowStats(t, h); st.Counts["pending"] != 0 || st.Counts["food_rows"] != 1 {
		t.Fatalf("echo not acknowledged: %+v", st)
	}
	h.clk.Add(2 * time.Minute)
	h.svc.reconcileOnce(context.Background())
	shadowParity(t, h, "2026-10-01")
	if h.vars.posts != 1 {
		t.Fatalf("shadow caused extra posts: %d", h.vars.posts)
	}
	h.vars.onPost = func(int, map[string]any) (bool, int) { return false, 400 }
	h.logText("shadow02-reject", "skyr")
	if st := shadowStats(t, h); st.Counts["food_rows"] != 1 {
		t.Fatalf("rejected food copied: %+v", st)
	}
}
func TestShadowRecordsAndVoids(t *testing.T) {
	h := newShadowHarness(t, recOpts)
	code, r, raw := h.record("blood_pressure", bp(128, 82), "")
	if code != 200 || r.Record == nil {
		t.Fatalf("record %d %s", code, raw)
	}
	v := h.post("/fuel/record/void", map[string]any{"client_id": "shadow03-void", "record_id": r.Record.RecordID})
	if v.Code != 200 {
		t.Fatalf("void %d %s", v.Code, v.Body)
	}
	if st := shadowStats(t, h); st.Counts["records"] != 2 || st.ErrorCount != 0 {
		t.Fatalf("records: %+v", st)
	}
	h.restart()
	if st := shadowStats(t, h); st.Counts["records"] != 2 || st.ErrorCount != 0 {
		t.Fatalf("replay records: %+v", st)
	}
}
func TestShadowUnavailableAndBearerStatus(t *testing.T) {
	h := newShadowHarness(t, func(o *Options) { o.StorePath = o.StateDir }) // directory cannot be a database
	if r := h.logText("shadow04-log1", "skyr and walnuts"); r.Code != 200 {
		t.Fatalf("shadow failed request %d %s", r.Code, r.Body)
	}
	st := shadowStats(t, h)
	if st.Available || st.ErrorCount == 0 {
		t.Fatalf("missing failure signal: %+v", st)
	}
	for _, auth := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest("GET", "/fuel/admin/store-status", nil)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("status without bearer: %d", w.Code)
		}
	}
	// Operational status remains available even when normal routes are not ready.
	h.svc.mu.Lock()
	h.svc.ready = false
	h.svc.mu.Unlock()
	if r := h.get("/fuel/admin/store-status"); r.Code != 200 {
		t.Fatal(r.Code)
	}
}
func TestShadowSQLiteLockDoesNotDelayRequest(t *testing.T) {
	h := newShadowHarness(t)
	shadowDrain(t, h.svc.shadow)
	db, err := sql.Open("sqlite", filepath.Join(h.opts.StateDir, "fuel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO store_metadata(key,value) VALUES('test_lock','held')"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	r := h.logText("shadow05-lock", "skyr and walnuts")
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SQLite lock delayed legacy request: %s", elapsed)
	}
	tx.Rollback()
	shadowParity(t, h, "2026-10-01")
}
func TestShadowQueueOverflowIsBounded(t *testing.T) {
	sh := &shadowStore{queue: make(chan shadowJob, 1)}
	sh.enqueue(shadowJob{data: []byte("first")})
	started := time.Now()
	for i := 0; i < 1000; i++ {
		sh.enqueue(shadowJob{data: []byte("next")})
	}
	if sh.errors.Load() != 1000 || sh.bytes.Load() != 5 || len(sh.queue) != 1 || time.Since(started) > time.Second {
		t.Fatalf("queue errors=%d bytes=%d", sh.errors.Load(), sh.bytes.Load())
	}
}
func TestShadowOffAndConfig(t *testing.T) {
	c, err := ParseDaemonConfig([]byte("state_dir = \"/tmp/fuel-state\"\n"))
	if err != nil || c.StoreMode != "off" || c.StorePath != "/tmp/fuel-state/fuel.sqlite" {
		t.Fatalf("defaults %+v %v", c, err)
	}
	c, err = ParseDaemonConfig([]byte("store_mode = \"shadow\"\nstore_path = \"/tmp/other.sqlite\"\n"))
	if err != nil || c.StoreMode != "shadow" || c.StorePath != "/tmp/other.sqlite" {
		t.Fatalf("config %+v %v", c, err)
	}
	for _, bad := range []string{"store_mode = \"hybrid\"", "store_mode = \"\"", "store_modes = \"shadow\""} {
		if _, err := ParseDaemonConfig([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	h := newHarnessMode(t, true)
	if h.svc.shadow != nil {
		t.Fatal("shadow enabled by default")
	}
	if _, err := os.Stat(filepath.Join(h.opts.StateDir, "fuel.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("off created store: %v", err)
	}
	if st := decode[shadowStatus](t, h.get("/fuel/admin/store-status")); st.Mode != "off" || st.Available {
		t.Fatalf("off: %+v", st)
	}
}

func TestShadowEnableBackfillsInLegacyOrder(t *testing.T) {
	h := newHarnessMode(t, true)
	logged := h.logText("shadow06-oldlog", "skyr and walnuts")
	if logged.Code != 200 {
		t.Fatalf("legacy log %d %s", logged.Code, logged.Body)
	}
	// Insert a foreign historical row after the own rows, with an older timestamp.
	h.vars.add("var-food", "2020-01-02", map[string]any{"item": "old script meal", "kcal": 90})
	h.vars.mu.Lock()
	h.vars.vals[len(h.vars.vals)-1].CreatedAt = h.clk.Now().Add(-24 * time.Hour)
	h.vars.mu.Unlock()
	h.opts.StoreMode = "shadow"
	h.restart()
	shadowDrain(t, h.svc.shadow)
	rows, err := h.svc.shadow.db.ListRows(context.Background(), "2000-01-01", "2099-12-31")
	if err != nil || len(rows) != 3 {
		t.Fatalf("backfill %+v %v", rows, err)
	}
	if rows[0].Source != sqlstore.SourceVariablesImport || rows[0].RecordDate != "2020-01-02" || rows[1].Source != sqlstore.SourceFueld || rows[2].Source != sqlstore.SourceFueld {
		t.Fatalf("initial reducer order: %+v", rows)
	}
	shadowParity(t, h, "2026-10-01")
}

func TestShadowFailedUncertainCleanupAndRestart(t *testing.T) {
	for _, replayOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "replay"}[replayOnly], func(t *testing.T) {
			h := newShadowHarness(t)
			h.play(oneItem("skyr", 250, 160, 27, false))
			h.vars.onPost = func(int, map[string]any) (bool, int) { return false, 503 }
			if r := h.logText("shadow-failed-uncertain", "skyr"); r.Code != 202 {
				t.Fatalf("uncertain: %d %s", r.Code, r.Body)
			}
			if st := shadowStats(t, h); st.Counts["food_rows"] != 1 || st.Counts["pending"] != 1 {
				t.Fatalf("missing uncertain row: %+v", st)
			}
			op := h.svc.journal.Ops()[0]
			if replayOnly {
				// Simulate a crash after the legacy failure but before shadow consumes it.
				if err := appendJSONLine(filepath.Join(h.opts.StateDir, "journal.jsonl"), journalRec{T: "state", OpID: op.ID, State: OpFailed, At: h.clk.Now()}); err != nil {
					t.Fatal(err)
				}
			} else {
				h.clk.Add(h.opts.FailAfter + 24*time.Hour)
				h.svc.reconcileOnce(context.Background())
			}
			check := func() {
				t.Helper()
				st := shadowStats(t, h)
				if st.ErrorCount != 0 || st.Counts["food_rows"] != 0 || st.Counts["export_outbox"] != 0 || st.Counts["pending"] != 0 || st.Counts["operations"] != 1 {
					t.Fatalf("failed ghost survived: %+v", st)
				}
				rows, err := h.svc.shadow.db.ListRows(context.Background(), "0001-01-01", "9999-12-31")
				if err != nil || len(rows) != 0 {
					t.Fatalf("failed rows available for comparison: %+v %v", rows, err)
				}
				pending, err := h.svc.shadow.db.PendingExports(context.Background())
				if err != nil || len(pending) != 0 {
					t.Fatalf("failed exports: %+v %v", pending, err)
				}
			}
			if !replayOnly {
				check()
			}
			h.restart()
			check()
			h.restart()
			check()
		})
	}
}

func TestShadowPhysicalDuplicateCorrectionAlias(t *testing.T) {
	h := newShadowHarness(t)
	h.play(oneItem("skyr", 250, 160, 27, false))
	if r := h.logText("shadow-duplicate-alias", "skyr"); r.Code != 200 {
		t.Fatalf("log: %d %s", r.Code, r.Body)
	}
	op := h.svc.journal.Ops()[0]
	h.clk.Add(time.Minute)
	h.vars.add("var-food", op.Date, op.Data)
	h.vars.mu.Lock()
	alias := h.vars.vals[len(h.vars.vals)-1].ID
	h.vars.mu.Unlock()
	h.clk.Add(time.Minute)
	h.vars.add("var-food", op.Date, map[string]any{"corrects": alias, "reason": "undo", "kcal": -160, "protein_g": -27})
	for i := 0; i < 2; i++ {
		if err := h.svc.cache.RefreshDay(context.Background(), op.Date); err != nil {
			t.Fatal(err)
		}
		shadowParity(t, h, op.Date)
		if st := shadowStats(t, h); st.Counts["food_rows"] != 3 || st.Counts["imported"] != 2 || st.Counts["export_outbox"] != 1 || st.Counts["pending"] != 0 {
			t.Fatalf("physical aliases lost: %+v", st)
		}
		h.restart()
	}
	shadowParity(t, h, op.Date)
}

func TestShadowEchoTimestampOrdersLatestRevision(t *testing.T) {
	for _, kind := range []string{"import", "import_full"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			db, err := sqlstore.Open(filepath.Join(t.TempDir(), "fuel.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sh := &shadowStore{db: db, journal: newJournalState()}
			sh.target.Store("food")
			original := historyValue("original", "2020-01-01", `{"item":"rice","kcal":100}`)
			older := historyValue("older", original.RecordDate, `{"corrects":"original","source":"agentd","reason":"revise","kcal":10,"recalibrated":{"item":"older revision","to":{"kcal":110}}}`)
			older.CreatedAt = original.CreatedAt.Add(2 * time.Minute)
			echo := historyValue("echo", original.RecordDate, `{"op_id":"own","corrects":"original","source":"fuel","reason":"revise","kcal":20,"recalibrated":{"item":"latest revision","to":{"kcal":130}}}`)
			echo.CreatedAt = original.CreatedAt.Add(3 * time.Minute)
			op := Op{ID: "own", EntryID: "entry", ItemID: "item", Date: original.RecordDate, State: OpDone, ValueID: echo.ID, CreatedAt: original.CreatedAt.Add(time.Minute), DoneAt: original.CreatedAt.Add(time.Minute), Data: echo.Data}
			sh.journal.apply(journalRec{T: "txn", Ops: []Op{op}})
			if err := sh.writeFood(ctx, op); err != nil {
				t.Fatal(err)
			}
			values := []Value{echo, original, older}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			if err := sh.write(ctx, shadowJob{kind: kind, data: data}); err != nil {
				t.Fatal(err)
			}
			// A subsequent journal replay carries the earlier local timestamp.
			if err := sh.writeFood(ctx, op); err != nil {
				t.Fatal(err)
			}
			rows, err := db.ListRows(ctx, original.RecordDate, original.RecordDate)
			if err != nil || len(rows) != 3 || sh.errors.Load() != 0 {
				t.Fatalf("import: %+v %v errors=%d", rows, err, sh.errors.Load())
			}
			var local []Value
			for _, r := range rows {
				var data map[string]any
				if err := json.Unmarshal(r.Data, &data); err != nil {
					t.Fatal(err)
				}
				local = append(local, Value{ID: r.VariablesValueID, CreatedAt: r.CreatedAt, Data: data})
			}
			groups := groupRows(local)
			if len(groups) != 1 || groups[0].last.ID != echo.ID || baseName(groups[0]) != "latest revision" || baseName(groups[0]) != baseName(groupRows(values)[0]) {
				t.Fatalf("remote correction order lost: %+v", groups)
			}
		})
	}
}
