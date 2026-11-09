package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func localTestValue(id string) LocalValue {
	return LocalValue{ID: id, Variable: "Food log", Date: "2026-11-08", At: testTime, OpID: "op_" + id, Own: true, Food: true, Raw: `{"item":"coffee","kcal":20}`}
}
func TestLocalAtomicBundleAndOutboxFailure(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_export BEFORE INSERT ON export_outbox BEGIN SELECT RAISE(ABORT,'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	events := []LocalEvent{{Stream: "journal", Data: json.RawMessage(`{"t":"txn","at":"2026-11-08T12:00:00Z"}`)}}
	vals := []LocalValue{localTestValue("destination"), localTestValue("source-undo")}
	if err := s.CommitLocal(ctx, events, vals, true); err == nil {
		t.Fatal("expected transaction failure")
	}
	for _, table := range []string{"local_events", "local_values", "food_rows", "export_outbox", "export_objects"} {
		var n int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s survived: %d %v", table, n, err)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_export`); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitLocal(ctx, events, vals, true); err != nil {
		t.Fatal(err)
	}
	pending, err := s.LocalExports(ctx, testTime)
	if err != nil || len(pending) != 2 {
		t.Fatalf("outbox %+v %v", pending, err)
	}
	loaded, err := s.LocalEvents(ctx, "journal")
	if err != nil || len(loaded) != 1 {
		t.Fatalf("events %v %v", loaded, err)
	}
}
func TestLocalImportBatchRollbackAndCursor(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	v := localTestValue("foreign")
	v.Own = false
	v.RemoteID = "remote"
	v.ID = "variables:remote"
	if err := s.ImportLocal(ctx, []LocalValue{v}, "", testTime, true); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LocalValues(ctx, "")
	broken := v
	broken.RemoteID = ""
	v.Raw = `{"item":"edited","kcal":40}`
	if err := s.ImportLocal(ctx, []LocalValue{v, broken}, "", testTime.Add(time.Hour), true); err == nil {
		t.Fatal("expected incomplete batch failure")
	}
	after, _ := s.LocalValues(ctx, "")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("partial import committed: %+v %+v", before, after)
	}
	var cursor string
	if err := s.db.QueryRow(`SELECT updated_at FROM import_cursor WHERE source='local:'`).Scan(&cursor); err != nil || cursor != stamp(testTime) {
		t.Fatalf("cursor %q %v", cursor, err)
	}
	if err := s.ImportLocal(ctx, nil, "", testTime.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	after, _ = s.LocalValues(ctx, "")
	if len(after) != 0 {
		t.Fatal("complete empty scan did not tombstone")
	}
}
func TestLocalIdentityConflictAndExportDisabled(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	v := localTestValue("one")
	if err := s.CommitLocal(ctx, nil, []LocalValue{v}, false); err != nil {
		t.Fatal(err)
	}
	v.Raw = `{"item":"different"}`
	if err := s.CommitLocal(ctx, nil, []LocalValue{v}, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity reused: %v", err)
	}
	var count int
	_ = s.db.QueryRow(`SELECT count(*) FROM export_outbox`).Scan(&count)
	if count != 0 {
		t.Fatal("export disabled filled outbox")
	}
}
func TestLocalPendingRecordAndBackoffSurviveRestart(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	v := localTestValue("record")
	v.Food = false
	v.Variable = "Blood pressure"
	if err := s.CommitLocal(ctx, nil, []LocalValue{v}, true); err != nil {
		t.Fatal(err)
	}
	entries, err := s.LocalExports(ctx, testTime)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if err := s.BeginLocalExport(ctx, entries[0], testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.FailLocalExport(ctx, v.ID, "HTTP 503"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err = s.LocalExports(ctx, testTime.Add(30*time.Second))
	if err != nil || len(entries) != 0 {
		t.Fatalf("backoff lost %+v %v", entries, err)
	}
	entries, err = s.LocalExports(ctx, testTime.Add(time.Minute))
	if err != nil || len(entries) != 1 || entries[0].Attempts != 1 {
		t.Fatalf("pending lost %+v %v", entries, err)
	}
	st, err := s.LocalStatus(ctx, testTime.Add(time.Minute))
	if err != nil || st.Pending != 1 || st.ErrorCount != 1 || st.LastError != "HTTP 503" || st.OldestPendingAge != 60 {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := s.AckLocalExport(ctx, v.ID, "remote-record", testTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st, _ = s.LocalStatus(ctx, testTime)
	if st.Pending != 0 {
		t.Fatal("ack lost")
	}
}

func TestLocalReconciliationAdoptsEarlierImportedEcho(t *testing.T) {
	for _, viaImport := range []bool{false, true} {
		t.Run(fmt.Sprint(viaImport), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			own := localTestValue("own")
			own.Raw = `{"op_id":"op_own","item":"coffee","kcal":20}`
			remote := own
			remote.ID = ""
			remote.Own = false
			remote.RemoteID = "remote-first"
			if err := s.ImportLocal(ctx, []LocalValue{remote}, "", testTime, true); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitLocal(ctx, nil, []LocalValue{own}, true); err != nil {
				t.Fatal(err)
			}
			var err error
			if viaImport {
				err = s.ImportLocal(ctx, []LocalValue{remote}, "", testTime, true)
			} else {
				err = s.AckLocalExport(ctx, own.ID, remote.RemoteID, testTime)
			}
			if err != nil {
				t.Fatal(err)
			}
			vals, err := s.LocalValues(ctx, "")
			if err != nil || len(vals) != 1 || !vals[0].Own || vals[0].RemoteID != remote.RemoteID {
				t.Fatalf("echo not adopted %+v %v", vals, err)
			}
			st, _ := s.LocalStatus(ctx, testTime)
			if st.Pending != 0 {
				t.Fatal("echo did not clear outbox")
			}
		})
	}
}

func TestLocalImportChronologyAllVariablesAndStableSequences(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var batch []LocalValue
	for _, variable := range []string{"Food log", "Body composition", "Push ups"} {
		for i := 3; i >= 0; i-- {
			v := localTestValue(fmt.Sprintf("%s-%d", variable, i))
			v.Variable, v.RemoteID, v.Own, v.Food = variable, v.ID, false, variable == "Food log"
			v.At = testTime.Add(time.Duration(i/2) * time.Second) // includes ties resolved by value id
			if variable == "Push ups" {
				v.Raw = fmt.Sprint(i)
			}
			batch = append(batch, v)
		}
	}
	if err := s.ImportLocal(ctx, batch, "", testTime, true); err != nil {
		t.Fatal(err)
	}
	before, err := s.LocalValues(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(before); i++ {
		a, b := before[i-1], before[i]
		if b.At.Before(a.At) || (b.At.Equal(a.At) && b.RemoteID < a.RemoteID) {
			t.Fatalf("wrong permanent sequence: %+v then %+v", a, b)
		}
	}
	for i, j := 0, len(batch)-1; i < j; i, j = i+1, j-1 {
		batch[i], batch[j] = batch[j], batch[i]
	}
	if err := s.ImportLocal(ctx, batch, "", testTime, true); err != nil {
		t.Fatal(err)
	}
	after, err := s.LocalValues(ctx, "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("reimport changed sequences: %v", err)
	}
}

func TestLocalImportCannotAcknowledgeConflictingDuplicate(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			own := localTestValue("own")
			if err := s.CommitLocal(ctx, nil, []LocalValue{own}, true); err != nil {
				t.Fatal(err)
			}
			good, bad := own, own
			good.RemoteID, bad.RemoteID = "good", "bad"
			bad.Raw = `{"item":"coffee","kcal":40}`
			batch := []LocalValue{good, bad}
			if reverse {
				batch[0], batch[1] = batch[1], batch[0]
			}
			if err := s.ImportLocal(ctx, batch, "", testTime, true); err != nil {
				t.Fatal(err)
			}
			st, err := s.LocalStatus(ctx, testTime)
			if err != nil || st.Pending != 1 || st.Conflicts != 1 || !reflect.DeepEqual(st.ConflictIDs, []string{own.ID}) {
				t.Fatalf("conflict hidden: %+v %v", st, err)
			}
			vals, err := s.LocalValues(ctx, "")
			if err != nil || len(vals) != 1 || vals[0].Raw != own.Raw || vals[0].RemoteID != "" {
				t.Fatalf("conflict bound an alias: %+v %v", vals, err)
			}
			// A partial later scan cannot silently clear a known conflict.
			if err := s.ImportLocal(ctx, []LocalValue{good}, own.Date, testTime, true); err != nil {
				t.Fatal(err)
			}
			st, err = s.LocalStatus(ctx, testTime)
			if err != nil || st.Pending != 1 || st.Conflicts != 1 {
				t.Fatalf("conflict cleared: %+v %v", st, err)
			}
		})
	}
}

func TestLocalFoodRowsRemainReadableForRollback(t *testing.T) {
	s, _ := openTest(t)
	v := localTestValue("rollback")
	ctx := context.Background()
	if err := s.CommitLocal(ctx, nil, []LocalValue{v}, true); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRows(ctx, v.Date, v.Date)
	if err != nil || len(rows) != 1 || string(rows[0].Data) != v.Raw {
		t.Fatalf("legacy reader cannot read local rows: %+v %v", rows, err)
	}
}

func TestLocalConflictStatusBoundsIDsAndRejectsAck(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		v := localTestValue(fmt.Sprintf("conflict-%d", i))
		if err := s.CommitLocal(ctx, nil, []LocalValue{v}, true); err != nil {
			t.Fatal(err)
		}
		if err := s.ConflictLocalExport(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AckLocalExport(ctx, "conflict-0", "remote", testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict ack accepted: %v", err)
	}
	st, err := s.LocalStatus(ctx, testTime)
	if err != nil || st.Conflicts != 8 || st.Pending != 8 || len(st.ConflictIDs) != 5 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestLocalUpgradePreservesUncertainAttemptsAndRollbackRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range migrations[:4] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"uncertain", "fresh", "done"} {
		v := localTestValue(id)
		if _, err := db.Exec(`INSERT INTO local_values(id,variable,record_date,created_at,op_id,own,food,raw) VALUES(?,?,?,?,?,1,1,?)`, id, v.Variable, v.Date, stamp(v.At), v.OpID, v.Raw); err != nil {
			t.Fatal(err)
		}
		// Version 4 local writes stored text in the legacy BLOB column.
		if _, err := db.Exec(`INSERT INTO food_rows(id,source,op_id,record_date,created_at,data) VALUES(?,'fueld',?,?,?,?)`, id, v.OpID, v.Date, stamp(v.At), v.Raw); err != nil {
			t.Fatal(err)
		}
		attempts := 1
		var done any
		if id == "fresh" {
			attempts = 0
		}
		if id == "done" {
			done = stamp(testTime)
		}
		if _, err := db.Exec(`INSERT INTO export_outbox(row_id,target,attempts,done_at) VALUES(?,?,?,?)`, id, v.Variable, attempts, done); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := s.LocalExports(context.Background(), time.Now())
	if err != nil || len(entries) != 2 {
		t.Fatalf("upgrade entries %+v %v", entries, err)
	}
	if entries[0].State != "uncertain" || entries[0].UncertainAt.Before(before) || !entries[0].EmptyReadAt.IsZero() || entries[1].State != "pending" {
		t.Fatalf("unsafe upgraded state: %+v", entries)
	}
	mayRetry, err := s.LocalExportMayRetry(context.Background(), entries[0], time.Now().Add(time.Hour))
	if err != nil || mayRetry {
		t.Fatalf("upgrade allowed retry without two reads: %v %v", mayRetry, err)
	}
	rows, err := s.ListRows(context.Background(), "2026-11-08", "2026-11-08")
	if err != nil || len(rows) != 3 {
		t.Fatalf("upgraded rollback rows unreadable: %+v %v", rows, err)
	}
}
