package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var testTime = time.Date(2026, 11, 8, 12, 0, 0, 123456789, time.FixedZone("test", 3600))

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "fuel ?#' test.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func ownRow(id string) Row {
	return Row{ID: id, Source: SourceFueld, OpID: "op_" + id, RecordDate: "2026-11-08", CreatedAt: testTime,
		Data: json.RawMessage(`{"item":"coffee","kind":"drink","kcal":20,"protein_g":1,"carbs_g":2,"fat_g":0,"sat_fat_g":null,"fiber_g":0,"volume_ml":250,"caffeine_mg":110,"alcohol_g":0,"psyllium_g":0,"beta_glucan_g":null,"nuts_g":1,"pulses_g":2,"plant_protein_g":3,"brew_method":"filtered","future":{"x":true}}`)}
}

func importedRow(id, valueID string) Row {
	r := ownRow(id)
	r.Source, r.VariablesValueID, r.OpID = SourceVariablesImport, valueID, ""
	return r
}

func mustInsert(t *testing.T, s *Store, r Row) Row {
	t.Helper()
	out, err := s.InsertRowWithOutbox(context.Background(), r, "food-variable")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenMigrationsAndPragmas(t *testing.T) {
	s, path := openTest(t)
	for pragma, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000, "synchronous": 2, "user_version": len(migrations)} {
		var got int
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, %v; want %d", pragma, got, err, want)
		}
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q: %v", mode, err)
	}
	for _, table := range []string{"food_rows", "operations", "journal", "idem", "feed", "records", "export_outbox", "import_cursor"} {
		if n := count(t, s, table); n != 0 {
			t.Fatalf("%s initially has %d rows", table, n)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database mode %o", info.Mode().Perm())
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %o", info.Mode().Perm())
	}
	// Force database/sql to replace its connection. Connection-local pragmas
	// must be restored by the DSN, not merely set once at startup.
	s.db.SetMaxIdleConns(0)
	for _, pragma := range []string{"foreign_keys", "synchronous", "busy_timeout"} {
		var got int
		want := map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 5000}[pragma]
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("replacement connection %s=%d: %v", pragma, got, err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO export_outbox(row_id, target) VALUES ('missing', 'food')"); err == nil {
		t.Fatal("foreign key accepted orphan")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFailureIsAtomicAndFutureVersionRefused(t *testing.T) {
	s, path := openTest(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original := migrations
	migrations = append(append([]string(nil), migrations...), "CREATE TABLE partial_migration (id INTEGER); INSERT INTO absent_table VALUES (1);")
	defer func() { migrations = original }()
	if broken, err := Open(path); err == nil {
		broken.Close()
		t.Fatal("bad migration accepted")
	}
	migrations = original
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, partial int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'partial_migration'").Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) || partial != 0 {
		t.Fatalf("partial migration survived: version=%d table=%d", version, partial)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if future, err := Open(path); err == nil {
		future.Close()
		t.Fatal("future schema accepted")
	}
}

func TestRowRoundTripAndLocalOrdering(t *testing.T) {
	s, path := openTest(t)
	first := mustInsert(t, s, ownRow("first"))
	second := ownRow("second")
	second.RecordDate, second.CreatedAt = "2026-11-07", testTime.Add(-time.Hour)
	second.Data = json.RawMessage(`{"item":"correction: coffee","op_id":"op_second","corrects":"first","reason":"fix","share_after":0.5,"kcal":-10,"protein_g":0,"carbs_g":0,"fat_g":0,"sat_fat_g":null,"fiber_g":0,"volume_ml":-125,"moved_from":"older","moved_from_date":"2026-11-06"}`)
	second = mustInsert(t, s, second)
	third := mustInsert(t, s, ownRow("third")) // exact timestamp tie with first
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListRows(context.Background(), "2026-11-07", "2026-11-08")
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{first, second, third}
	if len(rows) != len(want) {
		t.Fatalf("rows=%d", len(rows))
	}
	for i, r := range rows {
		if r.ID != want[i].ID || r.Sequence != want[i].Sequence || !r.CreatedAt.Equal(want[i].CreatedAt) || !bytes.Equal(r.Data, want[i].Data) {
			t.Fatalf("row %d: %#v, want %#v", i, r, want[i])
		}
		if i > 0 && rows[i-1].Sequence >= r.Sequence {
			t.Fatal("sequence did not increase")
		}
	}
	rows, err = s.ListRows(context.Background(), "2026-11-07", "2026-11-07")
	if err != nil || len(rows) != 1 || rows[0].ID != "second" {
		t.Fatalf("inclusive date range: %v %v", rows, err)
	}
	rows, err = s.ListRows(context.Background(), "2026-01-01", "2026-01-01")
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty range: %v %v", rows, err)
	}
}

func TestRowAndOutboxRollbackOnFailure(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	mustInsert(t, s, ownRow("before"))
	// Fail the SECOND statement, after a valid food row has been inserted.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_export BEFORE INSERT ON export_outbox BEGIN SELECT RAISE(ABORT, 'injected outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRowWithOutbox(ctx, ownRow("rolled-back"), "food-variable"); err == nil {
		t.Fatal("injected failure not returned")
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if count(t, s, "food_rows") != 1 || count(t, s, "export_outbox") != 1 {
		t.Fatal("partial transaction survived reopen")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_export"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRowWithOutbox(ctx, ownRow("before"), "other-target"); err == nil {
		t.Fatal("duplicate ID accepted")
	}
	r := ownRow("other-id")
	r.OpID = "op_before"
	if _, err := s.InsertRowWithOutbox(ctx, r, "food-variable"); err == nil {
		t.Fatal("duplicate own operation accepted")
	}
	if count(t, s, "export_outbox") != 1 {
		t.Fatal("duplicate left obligation")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.InsertRowWithOutbox(cancelled, ownRow("cancelled"), "food-variable"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestImportIdempotenceUpdatesAndEchoes(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	r := importedRow("foreign", "value-1")
	first, err := s.UpsertImportedRow(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.ID = fmt.Sprintf("different-local-proposal-%d", i)
		got, err := s.UpsertImportedRow(ctx, r)
		if err != nil || got.ID != first.ID || got.Sequence != first.Sequence {
			t.Fatalf("repeat: %+v %v", got, err)
		}
	}
	r.RecordDate, r.CreatedAt = "2026-11-06", testTime.Add(-time.Hour)
	r.Data = json.RawMessage(`{"item":"edited foreign coffee","kcal":0,"fiber_g":null}`)
	got, err := s.UpsertImportedRow(ctx, r)
	if err != nil || got.ID != first.ID || got.Sequence != first.Sequence || !bytes.Equal(got.Data, r.Data) {
		t.Fatalf("update: %+v %v", got, err)
	}
	if count(t, s, "food_rows") != 1 || count(t, s, "export_outbox") != 0 {
		t.Fatal("import duplicated or created export")
	}
	rows, err := s.ListRows(ctx, "2026-11-06", "2026-11-06")
	if err != nil || len(rows) != 1 || !bytes.Equal(rows[0].Data, r.Data) {
		t.Fatalf("moved import: %v %v", rows, err)
	}
	// Distinct remote values with a shared op_id retain physical aliases.
	for _, id := range []string{"a", "b"} {
		r := importedRow(id, "remote-"+id)
		r.OpID = "foreign-duplicate-op"
		if _, err := s.UpsertImportedRow(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	local := mustInsert(t, s, ownRow("local"))
	echo := importedRow("echo", "value-local")
	echo.OpID, echo.CreatedAt, echo.Data = local.OpID, testTime.Add(time.Hour), json.RawMessage(`{"item":"remote altered payload"}`)
	got, err = s.UpsertImportedRow(ctx, echo)
	if err != nil || got.ID != local.ID || got.Source != SourceFueld || !got.CreatedAt.Equal(echo.CreatedAt) || !bytes.Equal(got.Data, local.Data) {
		t.Fatalf("op echo: %+v %v", got, err)
	}
	got, err = s.UpsertImportedRow(ctx, echo)
	if err != nil || got.VariablesValueID != echo.VariablesValueID {
		t.Fatalf("value echo: %+v %v", got, err)
	}
	echo.VariablesValueID = "duplicate-alias"
	alias, err := s.UpsertImportedRow(ctx, echo)
	if err != nil || alias.Source != SourceVariablesImport || alias.ID == local.ID {
		t.Fatalf("physical alias: %+v %v", alias, err)
	}
	again, err := s.UpsertImportedRow(ctx, echo)
	if err != nil || again.ID != alias.ID || again.Sequence != alias.Sequence {
		t.Fatalf("repeated alias: %+v %v", again, err)
	}
	echo.VariablesValueID, echo.OpID = "value-local", "different-op"
	if _, err := s.UpsertImportedRow(ctx, echo); !errors.Is(err, ErrConflict) {
		t.Fatalf("reassigned value identity: %v", err)
	}
	if count(t, s, "food_rows") != 5 || count(t, s, "export_outbox") != 1 {
		t.Fatal("echo duplicated or exported")
	}
}

func TestConcurrentImportsAndWrites(t *testing.T) {
	s, path := openTest(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := []*Store{s, other}[i%2]
			if i < 10 {
				_, err := store.UpsertImportedRow(context.Background(), importedRow(fmt.Sprint(i), "same-remote-value"))
				errCh <- err
			} else {
				_, err := store.InsertRowWithOutbox(context.Background(), ownRow(fmt.Sprint(i)), "food")
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, "food_rows") != 11 || count(t, s, "export_outbox") != 10 {
		t.Fatal("concurrent writes lost or duplicated")
	}
}

func TestExportAcknowledgementAtomicAndIdempotent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustInsert(t, s, ownRow("local"))
	if err := s.RecordExportAttempt(ctx, "local", "food-variable", "timeout"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingExports(ctx)
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError != "timeout" {
		t.Fatalf("pending: %v %v", pending, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_ack BEFORE UPDATE OF done_at ON export_outbox BEGIN SELECT RAISE(ABORT, 'injected ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExportDone(ctx, "local", "food-variable", "remote", testTime); err == nil {
		t.Fatal("ack failure not returned")
	}
	rows, err := s.ListRows(ctx, "2026-11-08", "2026-11-08")
	if err != nil || rows[0].VariablesValueID != "" {
		t.Fatalf("partial alias bind: %v %v", rows, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_ack"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{testTime, testTime.Add(time.Hour)} {
		if err := s.MarkExportDone(ctx, "local", "food-variable", "remote", at); err != nil {
			t.Fatal(err)
		}
	}
	pending, err = s.PendingExports(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("done still pending: %v %v", pending, err)
	}
	var done, diagnostic string
	if err := s.db.QueryRow("SELECT done_at, last_error FROM export_outbox").Scan(&done, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if done != stamp(testTime) || diagnostic != "" {
		t.Fatalf("ack changed: %q %q", done, diagnostic)
	}
	if err := s.MarkExportDone(ctx, "local", "food-variable", "other-remote", testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("alias reassigned: %v", err)
	}
	if err := s.MarkExportDone(ctx, "local", "missing-target", "remote", testTime); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing obligation: %v", err)
	}
	mustInsert(t, s, ownRow("second"))
	if err := s.MarkExportDone(ctx, "second", "food-variable", "remote", testTime); err == nil {
		t.Fatal("remote value bound to two rows")
	}
	if err := s.RecordExportAttempt(ctx, "local", "food-variable", "late"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed attempt: %v", err)
	}
}

func TestIdempotencyExpiryAndFinalResponse(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	r := IdemRecord{ClientID: "client", Hash: "hash", Kind: "log", At: testTime, ExpiresAt: testTime.Add(7 * 24 * time.Hour), EntryID: "entry", ItemIDs: []string{"a", "b"}}
	if _, err := s.GetIdem(ctx, r.ClientID, testTime); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.PutIdem(ctx, r, testTime); err != nil {
		t.Fatal(err)
	}
	conflict := r
	conflict.Hash = "other"
	if err := s.PutIdem(ctx, conflict, testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("hash conflict: %v", err)
	}
	r.Status, r.Response = 200, json.RawMessage(`{"status":"done","entry_id":"entry"}`)
	if err := s.PutIdem(ctx, r, testTime); err != nil {
		t.Fatal(err)
	}
	final := r
	r.Status, r.Response = 0, nil
	if err := s.PutIdem(ctx, r, testTime); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetIdem(ctx, r.ClientID, r.ExpiresAt.Add(-time.Nanosecond))
	if err != nil || got.Status != final.Status || !bytes.Equal(got.Response, final.Response) || !reflect.DeepEqual(got.ItemIDs, final.ItemIDs) {
		t.Fatalf("final lost: %+v %v", got, err)
	}
	if _, err := s.GetIdem(ctx, r.ClientID, r.ExpiresAt); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expiry boundary: %v", err)
	}
	conflict.At, conflict.ExpiresAt = r.ExpiresAt, r.ExpiresAt.Add(time.Hour)
	if err := s.PutIdem(ctx, conflict, conflict.At); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetIdem(ctx, r.ClientID, conflict.At)
	if err != nil || got.Hash != conflict.Hash {
		t.Fatalf("expired reuse: %+v %v", got, err)
	}
}

func TestInputValidation(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, change := range []func(*Row){
		func(r *Row) { r.ID = "" }, func(r *Row) { r.RecordDate = "2026-02-30" },
		func(r *Row) { r.CreatedAt = time.Time{} }, func(r *Row) { r.OpID = "" },
		func(r *Row) { r.Source = "wrong" }, func(r *Row) { r.Data = json.RawMessage(`[]`) },
		func(r *Row) { r.Data = json.RawMessage(`null`) }, func(r *Row) { r.Data = json.RawMessage(`{`) },
		func(r *Row) { r.Data = json.RawMessage(`{"op_id":"wrong"}`) },
	} {
		r := ownRow("bad")
		change(&r)
		if _, err := s.InsertRowWithOutbox(ctx, r, "food"); err == nil {
			t.Fatalf("invalid row accepted: %+v", r)
		}
	}
	if _, err := s.InsertRowWithOutbox(ctx, ownRow("bad"), ""); err == nil {
		t.Fatal("empty target accepted")
	}
	if _, err := s.UpsertImportedRow(ctx, importedRow("bad", "")); err == nil {
		t.Fatal("empty import identity accepted")
	}
	for _, dates := range [][2]string{{"invalid", "2026-11-08"}, {"2026-11-08", "2026-11-07"}} {
		if _, err := s.ListRows(ctx, dates[0], dates[1]); err == nil {
			t.Fatal("invalid range accepted")
		}
	}
	if err := s.PutIdem(ctx, IdemRecord{}, testTime); err == nil {
		t.Fatal("invalid idem accepted")
	}
	if err := s.MarkExportDone(ctx, "id", "food", "", testTime); err == nil {
		t.Fatal("empty acknowledgement accepted")
	}
	for _, path := range []string{"", ":memory:"} {
		if db, err := Open(path); err == nil {
			db.Close()
			t.Fatal("non-file database accepted")
		}
	}
}

// The child exits without defers/Close, as a process crash would. All temporary
// paths are supplied by the parent test's t.TempDir().
func TestCrashHelper(t *testing.T) {
	stage := os.Getenv("FUEL_SQLSTORE_CRASH_STAGE")
	if stage == "" {
		return
	}
	s, err := Open(os.Getenv("FUEL_SQLSTORE_CRASH_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA cache_size = 1"); err != nil {
		t.Fatal(err)
	}
	r := ownRow("crash-row")
	r.Data, err = json.Marshal(map[string]string{"item": "meal", "padding": strings.Repeat("x", 128*1024)})
	if err != nil {
		t.Fatal(err)
	}
	if stage == "committed" {
		if _, err := s.InsertRowWithOutbox(context.Background(), r, "food"); err != nil {
			t.Fatal(err)
		}
	} else {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := insertRow(context.Background(), tx, r); err != nil {
			t.Fatal(err)
		}
		if stage == "outbox" {
			if _, err := tx.Exec("INSERT INTO export_outbox(row_id,target) VALUES (?,?)", r.ID, "food"); err != nil {
				t.Fatal(err)
			}
		}
	}
	os.Exit(23)
}

func TestCrashRecovery(t *testing.T) {
	for _, stage := range []string{"row", "outbox", "committed"} {
		t.Run(stage, func(t *testing.T) {
			s, path := openTest(t)
			mustInsert(t, s, ownRow("baseline"))
			s.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestCrashHelper$")
			cmd.Env = append(os.Environ(), "FUEL_SQLSTORE_CRASH_STAGE="+stage, "FUEL_SQLSTORE_CRASH_PATH="+path)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child did not reach crash: %v %s", err, out)
			}
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := 1
			if stage == "committed" {
				want = 2
			}
			if count(t, s, "food_rows") != want || count(t, s, "export_outbox") != want {
				t.Fatalf("crash %s left partial/lost data", stage)
			}
			if err := s.IntegrityCheck(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
