package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestShadowRowReplayAndImportRace(t *testing.T) {
	for _, importFirst := range []bool{false, true} {
		name := "pending-first"
		if importFirst {
			name = "import-first"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			own := ownRow("local")
			echo := own
			echo.ID = "foreign"
			echo.Source = SourceVariablesImport
			echo.VariablesValueID = "remote"
			if importFirst {
				if _, err := s.UpsertImportedRow(ctx, echo); err != nil {
					t.Fatal(err)
				}
				// A refresh can see the POST before its lost reply queues the
				// uncertain outcome. Adopt that echo even without an ack ID.
				if err := s.PutShadowRow(ctx, own, "food"); err != nil {
					t.Fatal(err)
				}
				if n := count(t, s, "food_rows"); n != 1 {
					t.Fatalf("pending echo duplicated: %d", n)
				}
				if pending, err := s.PendingExports(ctx); err != nil || len(pending) != 0 {
					t.Fatalf("known echo left pending: %+v %v", pending, err)
				}
			} else {
				if err := s.PutShadowRow(ctx, own, "food"); err != nil {
					t.Fatal(err)
				}
			}
			own.VariablesValueID = "remote"
			if err := s.PutShadowRow(ctx, own, "food"); err != nil {
				t.Fatal(err)
			}
			rows, _ := s.ListRows(ctx, "2026-11-08", "2026-11-08")
			if len(rows) != 1 || rows[0].Source != SourceFueld {
				t.Fatalf("race duplicated row: %+v", rows)
			}
			id, seq := rows[0].ID, rows[0].Sequence
			own.VariablesValueID = ""
			own.CreatedAt = own.CreatedAt.Add(-time.Hour)
			if err := s.PutShadowRow(ctx, own, "food"); err != nil {
				t.Fatal(err)
			}
			rows, _ = s.ListRows(ctx, "2026-11-08", "2026-11-08")
			if rows[0].ID != id || rows[0].Sequence != seq || rows[0].VariablesValueID != "remote" || !rows[0].CreatedAt.Equal(testTime) {
				t.Fatalf("replay changed identity: %+v", rows)
			}
			pending, err := s.PendingExports(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("ack regressed: %+v %v", pending, err)
			}
			own.VariablesValueID = "other"
			if err := s.PutShadowRow(ctx, own, "food"); !errors.Is(err, ErrConflict) {
				t.Fatalf("alias conflict: %v", err)
			}
		})
	}
}
func TestShadowLegacyEnvelopes(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	events := []struct{ kind, key, payload string }{
		{"journal", "1:hash", `{"t":"txn","at":"2026-11-08T12:00:00Z","entry":{"id":"e"},"items":[{"id":"i"}],"fraction":{"f":0.5},"ident":{"client_id":"client"}}`},
		{"operation", "", `{"op_id":"op","entry_id":"e","item_id":"i","date":"2026-11-08","state":"uncertain","created_at":"2026-11-08T12:00:00Z","data":{"future":null}}`},
		{"feed", "", `{"seq":42,"id":"f42","entry_id":"e","at":"2026-11-08T12:00:00Z","blocks":[{"text":"kept"}]}`},
		{"records", "", `{"op_id":"rec-op","record_id":"r","type":"waist","var":"body","kind":"void","record_date":"2026-11-08","state":"uncertain","at":"2026-11-08T12:00:00Z","payload":{"voids":"r"}}`},
	}
	for _, e := range events {
		for i := 0; i < 2; i++ {
			if err := s.MirrorLegacy(ctx, e.kind, e.key, json.RawMessage(e.payload)); err != nil {
				t.Fatal(e.kind, err)
			}
		}
	}
	for _, table := range []string{"journal", "operations", "feed", "records"} {
		if n := count(t, s, table); n != 1 {
			t.Fatalf("%s has %d rows", table, n)
		}
	}
	var payload string
	if err := s.db.QueryRow("SELECT payload FROM journal").Scan(&payload); err != nil || payload != events[0].payload {
		t.Fatalf("journal envelope %s %v", payload, err)
	}
	var seq int64
	if err := s.db.QueryRow("SELECT local_seq FROM feed").Scan(&seq); err != nil || seq != 42 {
		t.Fatalf("feed seq %d %v", seq, err)
	}
	idem := IdemRecord{ClientID: "client", Hash: "hash", Kind: "fix", At: testTime, ExpiresAt: testTime.Add(7 * 24 * time.Hour), Response: json.RawMessage(`{"done":true}`)}
	b, _ := json.Marshal(idem)
	if err := s.MirrorLegacy(ctx, "idem", "", b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIdem(ctx, "client", testTime)
	if err != nil || string(got.Response) != string(idem.Response) {
		t.Fatalf("idem %+v %v", got, err)
	}
}
func TestShadowStatusAndBackup(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	initial, err := s.Status(ctx)
	if err != nil || initial.LastWrite != nil || initial.LastBackup != nil {
		t.Fatalf("initial %+v %v", initial, err)
	}
	if err := s.PutShadowRow(ctx, ownRow("one"), "food"); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteShadowWrite(ctx, testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, filepath.Join(t.TempDir(), "snapshot.sqlite")); err != nil {
		t.Fatal(err)
	}
	// A second process used by a backup job sees and records the same metadata.
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	st, err := other.Status(ctx)
	if err != nil || st.Counts["food_rows"] != 1 || st.Counts["pending"] != 1 || st.LastWrite == nil || !st.LastWrite.Equal(testTime) || st.LastBackup == nil {
		t.Fatalf("status %+v %v", st, err)
	}
	before := *st.LastBackup
	if err := s.Backup(ctx, ""); err == nil {
		t.Fatal("bad backup succeeded")
	}
	after, err := other.Status(ctx)
	if err != nil || !after.LastBackup.Equal(before) {
		t.Fatalf("failed backup advanced status %+v %v", after, err)
	}
}

func TestImportedEchoTimestampSurvivesReplay(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "acknowledged"}[confirmed], func(t *testing.T) {
			s, path := openTest(t)
			ctx := context.Background()
			own := ownRow("local")
			if confirmed {
				own.VariablesValueID = "remote"
			}
			if err := s.PutShadowRow(ctx, own, "food"); err != nil {
				t.Fatal(err)
			}
			before, _ := s.ListRows(ctx, own.RecordDate, own.RecordDate)
			echo := own
			echo.ID, echo.Source, echo.VariablesValueID = "import", SourceVariablesImport, "remote"
			echo.CreatedAt = testTime.Add(time.Hour)
			if _, err := s.UpsertImportedRow(ctx, echo); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.PutShadowRow(ctx, own, "food"); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ListRows(ctx, own.RecordDate, own.RecordDate)
			if err != nil || len(rows) != 1 || rows[0].ID != before[0].ID || rows[0].Sequence != before[0].Sequence || !rows[0].CreatedAt.Equal(echo.CreatedAt) {
				t.Fatalf("remote timestamp lost on replay: %+v %v", rows, err)
			}
			if pending, err := s.PendingExports(ctx); err != nil || len(pending) != 0 {
				t.Fatalf("echo pending: %+v %v", pending, err)
			}
		})
	}
}

func TestFailedShadowRowCleanupAtomicAndConfirmedSafe(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	own := ownRow("failed")
	if err := s.PutShadowRow(ctx, own, "food"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON food_rows BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUnconfirmedShadowRow(ctx, own.OpID); err == nil {
		t.Fatal("injected failure ignored")
	}
	if count(t, s, "food_rows") != 1 || count(t, s, "export_outbox") != 1 {
		t.Fatal("partial cleanup committed")
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_delete"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RemoveUnconfirmedShadowRow(ctx, own.OpID); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, "food_rows") != 0 || count(t, s, "export_outbox") != 0 {
		t.Fatal("failed row or obligation survived")
	}
	// A late echo is physical history and must survive further failure replay.
	echo := own
	echo.ID, echo.Source, echo.VariablesValueID = "late", SourceVariablesImport, "remote"
	if _, err := s.UpsertImportedRow(ctx, echo); err != nil {
		t.Fatal(err)
	}
	if err := s.PutShadowRow(ctx, own, "food"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUnconfirmedShadowRow(ctx, own.OpID); err != nil {
		t.Fatal(err)
	}
	if count(t, s, "food_rows") != 1 {
		t.Fatal("confirmed late row removed")
	}
	if pending, err := s.PendingExports(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("late echo pending: %+v %v", pending, err)
	}
}
