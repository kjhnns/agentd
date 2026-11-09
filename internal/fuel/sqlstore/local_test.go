package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := s.ImportLocal(ctx, []LocalValue{v}, "", testTime); err != nil {
		t.Fatal(err)
	}
	before, _ := s.LocalValues(ctx, "")
	broken := v
	broken.RemoteID = ""
	v.Raw = `{"item":"edited","kcal":40}`
	if err := s.ImportLocal(ctx, []LocalValue{v, broken}, "", testTime.Add(time.Hour)); err == nil {
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
	if err := s.ImportLocal(ctx, nil, "", testTime.Add(time.Hour)); err != nil {
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
			if err := s.ImportLocal(ctx, []LocalValue{remote}, "", testTime); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitLocal(ctx, nil, []LocalValue{own}, true); err != nil {
				t.Fatal(err)
			}
			var err error
			if viaImport {
				err = s.ImportLocal(ctx, []LocalValue{remote}, "", testTime)
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
