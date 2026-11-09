package fuel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kjhnns/agentd/internal/fuel/sqlstore"
)

// No sockets: delayed visibility and lost replies are controlled independently.
type localExportRemote struct {
	values  []Value
	posts   []string
	reads   int
	postErr error
	readErr error
	now     time.Time
	path    string
}

func (r *localExportRemote) ListVariables(context.Context) ([]VarInfo, error) {
	r.reads++
	return []VarInfo{{ID: "food", Name: "Food", Type: "json"}, {ID: "body", Name: "Body", Type: "json"}, {ID: "push", Name: "Push ups", Type: "numeric"}}, r.readErr
}
func (r *localExportRemote) All(context.Context) ([]Value, error) {
	r.reads++
	return r.values, r.readErr
}
func (r *localExportRemote) ByDate(ctx context.Context, date string) ([]Value, error) {
	vals, err := r.All(ctx)
	var out []Value
	for _, v := range vals {
		if v.RecordDate == date {
			out = append(out, v)
		}
	}
	return out, err
}
func (r *localExportRemote) Post(_ context.Context, variable string, data map[string]any, date string) (string, error) {
	r.posts = append(r.posts, data["op_id"].(string))
	if r.postErr != nil {
		return "", r.postErr
	}
	id := fmt.Sprintf("remote-%d", len(r.posts))
	r.values = append(r.values, Value{ID: id, VariableID: variable, RecordDate: date, CreatedAt: r.now.Add(time.Duration(len(r.posts)) * time.Second), Data: data})
	return id, nil
}
func localExportFixture(t *testing.T) (*localStore, *localExportRemote) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sqlstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := &localExportRemote{path: path, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	return &localStore{db: db, remote: r, food: "Food", export: true, defaults: []VarInfo{{Name: "Food"}, {Name: "Body"}}}, r
}
func localExportValue(id, raw string, now time.Time) sqlstore.LocalValue {
	return sqlstore.LocalValue{ID: id, OpID: id, Variable: "Food", Date: "2026-10-01", At: now, Own: true, Food: true, Raw: raw}
}
func runLocalExport(t *testing.T, l *localStore, now time.Time) {
	t.Helper()
	if err := l.exportOnce(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}
func localExportStatus(t *testing.T, l *localStore, now time.Time) sqlstore.LocalStatus {
	t.Helper()
	st, err := l.db.LocalStatus(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
func TestLocalExportPayloadConflicts(t *testing.T) {
	for _, field := range []string{"item", "kcal", "protein_g", "portion_g_after", "corrects", "reason", "extra_macro", "date", "target"} {
		t.Run(field, func(t *testing.T) {
			l, r := localExportFixture(t)
			v := localExportValue("shared", `{"op_id":"shared","item":"correction: apple","item_id":"correction","corrects":"apple","kcal":-50,"protein_g":-1,"portion_g_after":50,"reason":"portion"}`, r.now)
			if err := l.db.CommitLocal(context.Background(), nil, []sqlstore.LocalValue{v}, true); err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(v.Raw), &data); err != nil {
				t.Fatal(err)
			}
			remote := Value{ID: "foreign", VariableID: "food", RecordDate: v.Date, CreatedAt: r.now, Data: data}
			switch field {
			case "date":
				remote.RecordDate = "2026-10-02"
			case "target":
				remote.VariableID = "body"
			case "extra_macro":
				data["fat_g"] = float64(7)
			case "kcal", "protein_g", "portion_g_after":
				data[field] = float64(3)
			default:
				data[field] = "different"
			}
			// An exact match first must not hide a conflicting duplicate.
			var exact map[string]any
			_ = json.Unmarshal([]byte(v.Raw), &exact)
			r.values = []Value{{ID: "exact", VariableID: "food", RecordDate: v.Date, CreatedAt: r.now, Data: exact}, remote}
			runLocalExport(t, l, r.now)
			st := localExportStatus(t, l, r.now)
			if st.Pending != 1 || st.Conflicts != 1 || !reflect.DeepEqual(st.ConflictIDs, []string{v.ID}) || len(r.posts) != 0 {
				t.Fatalf("conflict silently acknowledged: %+v posts %v", st, r.posts)
			}
			runLocalExport(t, l, r.now.Add(time.Hour))
			if len(r.posts) != 0 {
				t.Fatal("conflict reposted")
			}
		})
	}
}
func TestLocalExportMatchingPayloadAcknowledges(t *testing.T) {
	l, r := localExportFixture(t)
	v := localExportValue("shared", `{"op_id":"shared","item":"apple","kcal":100,"fiber_g":null}`, r.now)
	if err := l.db.CommitLocal(context.Background(), nil, []sqlstore.LocalValue{v}, true); err != nil {
		t.Fatal(err)
	}
	r.values = []Value{{ID: "foreign", VariableID: "food", RecordDate: v.Date, CreatedAt: r.now, Raw: `{"title":"Remote title","fiber_g":null,"kcal":100.0,"item":"apple","op_id":"shared"}`}}
	runLocalExport(t, l, r.now)
	if st := localExportStatus(t, l, r.now); st.Pending != 0 || len(r.posts) != 0 {
		t.Fatalf("match not acknowledged: %+v", st)
	}
}
func TestLocalExportUncertainSettleAndRestart(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(fmt.Sprint(visible), func(t *testing.T) {
			l, r := localExportFixture(t)
			v := localExportValue("lost", `{"op_id":"lost","item":"apple","item_id":"apple","kcal":100}`, r.now)
			if err := l.db.CommitLocal(context.Background(), nil, []sqlstore.LocalValue{v}, true); err != nil {
				t.Fatal(err)
			}
			r.postErr = errors.New("reply lost")
			runLocalExport(t, l, r.now)
			if st := localExportStatus(t, l, r.now); st.Uncertain != 1 || st.Pending != 1 {
				t.Fatalf("not uncertain: %+v", st)
			}
			// Reopen the actual database, not merely a new exporter object.
			path := r.path
			l.db.Close()
			var err error
			l.db, err = sqlstore.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { l.db.Close() })
			r.postErr = nil
			for _, elapsed := range []time.Duration{time.Minute, 3 * time.Minute} {
				runLocalExport(t, l, r.now.Add(elapsed))
				if len(r.posts) != 1 {
					t.Fatalf("reposted inside settle window at %s", elapsed)
				}
			}
			if visible {
				var data map[string]any
				_ = json.Unmarshal([]byte(v.Raw), &data)
				r.values = []Value{{ID: "late", VariableID: "food", RecordDate: v.Date, CreatedAt: r.now, Data: data}}
			}
			runLocalExport(t, l, r.now.Add(7*time.Minute))
			if len(r.posts) != 1 {
				t.Fatal("reposted inside settle window")
			}
			if visible && localExportStatus(t, l, r.now).Pending != 0 {
				t.Fatal("visible row not reconciled during settle window")
			}
			runLocalExport(t, l, r.now.Add(9*time.Minute))
			if len(r.posts) != 1 {
				t.Fatal("reposted inside settle window")
			}
			runLocalExport(t, l, r.now.Add(15*time.Minute))
			want := 2
			if visible {
				want = 1
			}
			if st := localExportStatus(t, l, r.now); st.Pending != 0 || len(r.posts) != want {
				t.Fatalf("settle result %+v posts %v", st, r.posts)
			}
		})
	}
}
func TestLocalExportUncertainRequiresTwoSuccessfulEmptyReads(t *testing.T) {
	l, r := localExportFixture(t)
	v := localExportValue("crash", `{"op_id":"crash","item":"apple","kcal":100}`, r.now)
	if err := l.db.CommitLocal(context.Background(), nil, []sqlstore.LocalValue{v}, true); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after persisting uncertainty but before receiving a reply.
	if err := l.db.UncertainLocalExport(context.Background(), v.ID, r.now); err != nil {
		t.Fatal(err)
	}
	r.readErr = errors.New("read failed")
	runLocalExport(t, l, r.now.Add(20*time.Minute))
	r.readErr = nil
	runLocalExport(t, l, r.now.Add(30*time.Minute))
	if len(r.posts) != 0 {
		t.Fatal("one empty read allowed retry")
	}
	runLocalExport(t, l, r.now.Add(40*time.Minute))
	if len(r.posts) != 1 {
		t.Fatal("second settled empty read did not allow retry")
	}
}
func TestLocalExporterDisabledSleepsWithPendingUncertainWrites(t *testing.T) {
	l, r := localExportFixture(t)
	v := localExportValue("pending", `{"op_id":"pending","item":"apple","kcal":100}`, r.now)
	if err := l.db.CommitLocal(context.Background(), nil, []sqlstore.LocalValue{v}, true); err != nil {
		t.Fatal(err)
	}
	if err := l.db.UncertainLocalExport(context.Background(), v.ID, r.now); err != nil {
		t.Fatal(err)
	}
	l.export = false
	for _, elapsed := range []time.Duration{0, time.Minute, time.Hour} {
		runLocalExport(t, l, r.now.Add(elapsed))
	}
	if r.reads != 0 || len(r.posts) != 0 {
		t.Fatalf("disabled exporter contacted remote: %d reads, %v posts", r.reads, r.posts)
	}
	if st := localExportStatus(t, l, r.now); st.Pending != 1 || st.Uncertain != 1 {
		t.Fatalf("disabled exporter reconciled: %+v", st)
	}
	var data map[string]any
	_ = json.Unmarshal([]byte(v.Raw), &data)
	r.values = []Value{{ID: "visible", VariableID: "food", RecordDate: v.Date, CreatedAt: r.now, Data: data}}
	if err := l.importOnce(context.Background(), "", r.now); err != nil {
		t.Fatal(err)
	}
	if st := localExportStatus(t, l, r.now); st.Pending != 1 || st.Uncertain != 1 {
		t.Fatalf("import reconciled disabled exports: %+v", st)
	}

}

func TestLocalExportOrderingAcrossRetryAndRollback(t *testing.T) {
	h := newLocalHarness(t)
	logged := decode[LogResponse](t, h.logText("order001-log1", "skyr and walnuts"))
	first, other := logged.Items[0].ItemID, logged.Items[1].ItemID
	localCheck(t, h.fix("order001-fix1", first, map[string]any{"share": 0.5}))
	localCheck(t, h.fix("order001-fix2", first, map[string]any{"share": 0.75}))
	localCheck(t, h.post("/fuel/move", map[string]any{"client_id": "order001-move", "item_id": first, "day": "yesterday"}))
	moved := h.day("?date=2026-09-30").Items[0]["row_key"].(string)
	localCheck(t, h.fix("order001-fix3", moved, map[string]any{"share": 0.5}))
	localCheck(t, h.fix("order001-other", other, map[string]any{"share": 0.5}))
	before, beforeDay := h.snap(t, "?date=2026-09-30"), h.day("?date=2026-09-30")
	entries, err := h.svc.local.db.LocalExports(context.Background(), h.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	failed := entries[2].OpID
	h.vars.onPost = func(_ int, data map[string]any) (bool, int) {
		if data["op_id"] == failed {
			return false, 400
		}
		return true, 201
	}
	runLocalExport(t, h.svc.local, h.clk.Now())
	if st := localStats(t, h); st.Pending != len(entries)-3 {
		t.Fatalf("later operations overtook failure: %+v", st)
	}
	rows := h.vars.rows("var-food")
	if len(rows) != 3 {
		t.Fatalf("independent item blocked: %d rows", len(rows))
	}
	// Backoff and a process restart must retain the dependency barrier.
	h.restart()
	h.svc.Stop()
	runLocalExport(t, h.svc.local, h.clk.Now())
	if len(h.vars.rows("var-food")) != 3 {
		t.Fatal("backoff predecessor was skipped")
	}
	h.vars.onPost = nil
	h.clk.Add(time.Minute)
	runLocalExport(t, h.svc.local, h.clk.Now())
	if st := localStats(t, h); st.Pending != 0 {
		t.Fatalf("retry failed to drain: %+v", st)
	}
	remote, err := h.svc.local.remote.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, v := range remote {
		actual = append(actual, v.Data["op_id"].(string))
	}
	want := []string{entries[0].OpID, entries[1].OpID, entries[len(entries)-1].OpID}
	for _, e := range entries[2 : len(entries)-1] {
		want = append(want, e.OpID)
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("export order %v, want %v", actual, want)
	}
	path := h.svc.o.StorePath
	h.svc.Close()
	if err := ExportLocalLegacy(context.Background(), path, h.opts.StateDir); err != nil {
		t.Fatal(err)
	}
	h.opts.StoreMode = "shadow"
	h.start()
	h.svc.Stop()
	after, afterDay := h.snap(t, "?date=2026-09-30"), h.day("?date=2026-09-30")
	if before.Revision != after.Revision || !reflect.DeepEqual(before.Macros, after.Macros) || !reflect.DeepEqual(beforeDay.Items[0]["portion_g"], afterDay.Items[0]["portion_g"]) {
		t.Fatalf("rollback changed state: before %+v %+v, after %+v %+v", before.Macros, beforeDay.Items, after.Macros, afterDay.Items)
	}
}

func TestLocalImportedChronologyMatchesLegacyReducers(t *testing.T) {
	l, r := localExportFixture(t)
	for i, raw := range []string{
		`{"op_id":"original","item":"apple","item_id":"apple","kcal":100,"portion_g":100,"nuts_g":10,"brew_method":"filtered"}`,
		`{"op_id":"fix1","item":"correction: apple","corrects":"apple","kcal":-50,"share_after":0.5,"portion_g_after":50,"nuts_g":-5,"brew_method":"unfiltered"}`,
		`{"op_id":"fix2","item":"correction: apple","corrects":"apple","kcal":25,"share_after":0.75,"portion_g_after":75,"nuts_g":2.5,"brew_method":"filtered"}`,
		`{"op_id":"revise1","item":"correction: apple","corrects":"apple","source":"agentd","reason":"revise","kcal":20,"portion_g_after":80,"nuts_g":2.5,"recalibrated":{"item":"first revision","to":{"portion_g":80,"kcal":95,"levers":{"nuts_g":10}}}}`,
		`{"op_id":"revise2","item":"correction: apple","corrects":"apple","source":"agentd","reason":"revise","kcal":-5,"portion_g_after":70,"nuts_g":-2,"recalibrated":{"item":"last revision","to":{"portion_g":70,"kcal":90,"levers":{"nuts_g":8}}}}`,
	} {
		var data map[string]any
		_ = json.Unmarshal([]byte(raw), &data)
		r.values = append(r.values, Value{ID: fmt.Sprintf("row-%d", i), VariableID: "food", RecordDate: "2026-10-01", CreatedAt: r.now.Add(time.Duration(i) * time.Second), Raw: raw, Data: data})
	}
	legacy := groupRows(r.values)[0]
	wantPortion, _ := amountsAfter(legacy, legacy.c.effective(), macrosFromData(legacy.orig.Data))
	wantLevers := reduceLevers(legacy)
	for i, j := 0, len(r.values)-1; i < j; i, j = i+1, j-1 {
		r.values[i], r.values[j] = r.values[j], r.values[i]
	}
	if err := l.importOnce(context.Background(), "", r.now); err != nil {
		t.Fatal(err)
	}
	vals, err := l.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := groupRows(vals)[0]
	portion, _ := amountsAfter(g, g.c.effective(), macrosFromData(g.orig.Data))
	if !reflect.DeepEqual(portion, wantPortion) || !reflect.DeepEqual(reduceLevers(g), wantLevers) || g.last.ID != legacy.last.ID || baseName(g) != "last revision" {
		t.Fatalf("reversed import changed portion, levers or latest correction: %+v", g)
	}
}

func TestLocalRecordVoidWaitsForRecordExport(t *testing.T) {
	l, r := localExportFixture(t)
	var values []sqlstore.LocalValue
	for i, raw := range []string{
		`{"op_id":"record","record_id":"record-id","weight_kg":80}`,
		`{"op_id":"void","record_id":"void-id","voids":"record-id"}`,
		`{"op_id":"other","record_id":"other-id","weight_kg":79}`,
	} {
		id := []string{"record", "void", "other"}[i]
		v := localExportValue(id, raw, r.now)
		v.Variable, v.Food = "Body", false
		values = append(values, v)
	}
	if err := l.db.CommitLocal(context.Background(), nil, values, true); err != nil {
		t.Fatal(err)
	}
	r.postErr = &PostError{Status: 400}
	runLocalExport(t, l, r.now)
	if !reflect.DeepEqual(r.posts, []string{"record", "other"}) {
		t.Fatalf("void overtook record or blocked other record: %v", r.posts)
	}
	r.postErr = nil
	runLocalExport(t, l, r.now.Add(time.Minute))
	if !reflect.DeepEqual(r.posts, []string{"record", "other", "record", "void", "other"}) {
		t.Fatalf("wrong retry order: %v", r.posts)
	}
}

func TestLocalStoreStatusExposesConflictAndBlocksRollback(t *testing.T) {
	h := newLocalHarness(t)
	localCheck(t, h.logText("conflict001-log1", "skyr and walnuts"))
	entries, err := h.svc.local.db.LocalExports(context.Background(), h.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(entries[0].Raw), &data); err != nil {
		t.Fatal(err)
	}
	data["kcal"] = float64(1)
	h.addAt("var-food", entries[0].Date, data)
	runLocalExport(t, h.svc.local, h.clk.Now())
	st := localStats(t, h)
	if st.Pending != 1 || st.Conflicts != 1 || !reflect.DeepEqual(st.ConflictIDs, []string{entries[0].ID}) {
		t.Fatalf("store-status hides conflict: %+v", st)
	}
	if err := ExportLocalLegacy(context.Background(), h.svc.o.StorePath, filepath.Join(h.dir, "rollback")); err == nil {
		t.Fatal("rollback accepted conflicting export")
	}
}
