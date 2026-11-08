package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyBackupIncludesLiveWAL(t *testing.T) {
	source, path := openTest(t)
	ctx := context.Background()
	if _, err := source.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, source, ownRow("committed"))
	before := map[string][]byte{}
	for _, suffix := range []string{"", "-wal"} {
		var err error
		before[suffix], err = os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.db.Exec("DELETE FROM food_rows"); err == nil {
		t.Fatal("read-only connection allowed write")
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := ro.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range before {
		got, err := os.ReadFile(path + suffix)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read-only backup changed source %q: %v", suffix, err)
		}
	}
	snapshot, err := OpenReadOnly(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	c, err := snapshot.RowCounts(ctx)
	if err != nil || c["food_rows"] != 1 || c["export_outbox"] != 1 {
		t.Fatalf("lost WAL data: %v %v", c, err)
	}
	if err := snapshot.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("created missing database")
	}
}

func TestReadOnlyPreservesSchemaAndJournalMode(t *testing.T) {
	for version := 1; version <= len(migrations); version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old ?#.db")
			db, err := sql.Open("sqlite", databaseURI(path, false))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, migration := range migrations[:version] {
				if _, err := db.Exec(migration); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0400); err != nil {
				t.Fatal(err)
			}
			ro, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			// Force replacement connections, checking DSN-level protections too.
			ro.db.SetMaxIdleConns(0)
			for pragma, want := range map[string]int{"user_version": version, "query_only": 1} {
				var got int
				if err := ro.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
					t.Fatalf("%s=%d, want %d: %v", pragma, got, want, err)
				}
			}
			var mode string
			if err := ro.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
				t.Fatalf("journal mode %s: %v", mode, err)
			}
			if _, err := ro.db.Exec("CREATE TABLE forbidden(id)"); err == nil {
				t.Fatal("read-only handle allowed schema write")
			}
			ctx := context.Background()
			if err := ro.IntegrityCheck(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := ro.RowCounts(ctx); err != nil {
				t.Fatal(err)
			}
			if err := ro.Backup(ctx, filepath.Join(t.TempDir(), "backup.db")); err != nil {
				t.Fatal(err)
			}
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only inspection changed database: %v", err)
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
					t.Fatalf("read-only inspection created %s: %v", suffix, err)
				}
			}
		})
	}
}
