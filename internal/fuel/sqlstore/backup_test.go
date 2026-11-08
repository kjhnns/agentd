package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRestoreRoundTrip(t *testing.T) {
	s, source := openTest(t)
	ctx := context.Background()
	// Keep committed pages in the live WAL. A plain copy of source is not
	// sufficient to restore the rows written below.
	if _, err := s.db.Exec("PRAGMA wal_autocheckpoint = 0"); err != nil {
		t.Fatal(err)
	}
	want := mustInsert(t, s, ownRow("meal"))
	if err := s.MarkExportDone(ctx, want.ID, "food-variable", "value-meal", testTime); err != nil {
		t.Fatal(err)
	}
	want.VariablesValueID = "value-meal"
	imported, err := s.UpsertImportedRow(ctx, importedRow("foreign", "value-foreign"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutIdem(ctx, IdemRecord{ClientID: "client", Hash: "hash", Kind: "log", At: testTime, ExpiresAt: testTime.Add(time.Hour), Status: 200, Response: json.RawMessage(`{"done":true}`)}, testTime); err != nil {
		t.Fatal(err)
	}
	// These envelopes exercise the complete legacy shapes reserved by the H1
	// schema, not only food rows. Runtime CRUD for them belongs to integration.
	fixtures := map[string]string{
		"operations":    `INSERT INTO operations VALUES ('op_meal', 'entry', 'item', '2026-11-08', 'done', '2026-11-08T11:00:00Z', '{"op_id":"op_meal","kind":"original","data":{"fiber_g":0},"attempts":1}')`,
		"journal":       `INSERT INTO journal(kind,at,entry_id,op_id,payload) VALUES ('txn','2026-11-08T11:00:00Z','entry','op_meal','{"t":"txn","entry":{"id":"entry","photo_ids":["photo"]},"items":[],"ops":[],"fraction":{"f":0.5},"ident":{"client_id":"client"}}')`,
		"feed":          `INSERT INTO feed(id,entry_id,at,payload) VALUES ('line','entry','2026-11-08T11:00:00Z','{"role":"user","text":"lunch","photo_ids":["photo"],"via":"telegram","blocks":[]}')`,
		"records":       `INSERT INTO records(op_id,record_id,record_type,variable,kind,record_date,state,at,payload) VALUES ('record-op','record','waist','body','void','2026-11-08','pending','2026-11-08T11:00:00Z','{"op_id":"record-op","voids":"previous","client_id":"record-client","request_hash":"hash","payload":{"method":"tape","measured_at":"2026-11-08T11:00:00Z"}}')`,
		"import_cursor": `INSERT INTO import_cursor VALUES ('food','opaque cursor','2026-11-08T11:00:00Z')`,
	}
	for table, statement := range fixtures {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
	wal, err := os.Stat(source + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("no live WAL: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "snapshot ?#'.sqlite")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions %o", info.Mode().Perm())
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(backup + suffix); !os.IsNotExist(err) {
			t.Fatalf("backup has sidecar %s: %v", suffix, err)
		}
	}
	mustInsert(t, s, ownRow("after-snapshot"))
	// Restore only the snapshot file into an independent directory. Leave the
	// source database open so neither source shutdown nor its sidecars help.
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restore.sqlite")
	if err := os.WriteFile(restoredPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := restored.ListRows(ctx, "2026-11-08", "2026-11-08")
	if err != nil || len(rows) != 2 {
		t.Fatalf("restored rows: %d %v", len(rows), err)
	}
	for i, want := range []Row{want, imported} {
		got := rows[i]
		if got.ID != want.ID || got.Sequence != want.Sequence || got.VariablesValueID != want.VariablesValueID || got.Source != want.Source || !got.CreatedAt.Equal(want.CreatedAt) || !bytes.Equal(got.Data, want.Data) {
			t.Fatalf("restored row %d differs: %+v", i, got)
		}
	}
	if pending, err := restored.PendingExports(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("restored acknowledgement lost: %v %v", pending, err)
	}
	idem, err := restored.GetIdem(ctx, "client", testTime)
	if err != nil || idem.Status != 200 || string(idem.Response) != `{"done":true}` {
		t.Fatalf("restored idem: %+v %v", idem, err)
	}
	for table := range fixtures {
		if count(t, restored, table) != 1 {
			t.Fatalf("lost %s", table)
		}
		column := "payload"
		if table == "import_cursor" {
			column = "cursor"
		}
		var before, after string
		if err := s.db.QueryRow("SELECT " + column + " FROM " + table).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := restored.db.QueryRow("SELECT " + column + " FROM " + table).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatalf("changed %s", table)
		}
	}
	newRow := mustInsert(t, restored, ownRow("after-restore"))
	if newRow.Sequence <= imported.Sequence {
		t.Fatal("sequence reset after restore")
	}
}

func TestBackupWhileAnotherConnectionWrites(t *testing.T) {
	s, path := openTest(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// Force a multi-step backup, with independent writer commits during it.
	for i := 0; i < 40; i++ {
		r := ownRow(fmt.Sprintf("before-%d", i))
		r.Data, err = json.Marshal(map[string]string{"item": strings.Repeat("x", 32*1024)})
		if err != nil {
			t.Fatal(err)
		}
		mustInsert(t, s, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			if _, err := other.InsertRowWithOutbox(ctx, ownRow(fmt.Sprintf("during-%d", i)), "food"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	backup := filepath.Join(t.TempDir(), "concurrent.sqlite")
	backupErr := s.Backup(ctx, backup)
	writerErr := <-done
	if backupErr != nil || writerErr != nil {
		t.Fatalf("backup=%v writer=%v", backupErr, writerErr)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	n := count(t, restored, "food_rows")
	if n < 40 || n > 60 || count(t, restored, "export_outbox") != n {
		t.Fatalf("inconsistent snapshot with %d rows", n)
	}
	if err := restored.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "food_rows") != 60 {
		t.Fatal("live writes lost")
	}
}

func TestBackupFailureAndCancellation(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	mustInsert(t, s, ownRow("live"))
	if err := s.Backup(ctx, path); err == nil {
		t.Fatal("overwrote live database")
	}
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(existing, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, existing); err == nil {
		t.Fatal("overwrote existing backup")
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("existing destination changed: %q %v", data, err)
	}
	stale := filepath.Join(t.TempDir(), "stale")
	if err := os.WriteFile(stale+"-wal", []byte("stale wal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, stale); err == nil {
		t.Fatal("accepted stale sidecar")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale destination created: %v", err)
	}
	// Timeout while waiting for the source connection, after destination
	// reservation. This checks cleanup after a partially started backup.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	timed, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "cancelled.sqlite")
	if err := s.Backup(timed, dest); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled backup: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
			t.Fatalf("incomplete backup remains: %s %v", suffix, err)
		}
	}
	conn.Close()
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "food_rows") != 1 {
		t.Fatal("backup failure damaged source")
	}
	if err := s.Backup(timed, dest); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pre-cancelled backup: %v", err)
	}
	if err := s.Backup(ctx, ""); err == nil {
		t.Fatal("empty destination accepted")
	}
}

func TestIntegrityCheckDetectsForeignKeyViolationAndCorruption(t *testing.T) {
	s, _ := openTest(t)
	// Simulate a file modified by a client that did not enable foreign keys.
	if _, err := s.db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO export_outbox(row_id, target) VALUES ('missing', 'food')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	if err := s.IntegrityCheck(context.Background()); err == nil {
		t.Fatal("orphan not detected")
	}
	dest := filepath.Join(t.TempDir(), "bad-snapshot")
	if err := s.Backup(context.Background(), dest); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("invalid snapshot not removed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(path, []byte(strings.Repeat("not a database", 512)), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := integrityCheck(context.Background(), db); err == nil {
		t.Fatal("corruption not detected")
	}
}
