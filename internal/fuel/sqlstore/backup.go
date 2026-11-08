package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IntegrityCheck runs SQLite's full integrity_check and foreign_key_check.
// A successful result has no diagnostics and includes all committed WAL data.
func (s *Store) IntegrityCheck(ctx context.Context) error { return integrityCheck(ctx, s.db) }

func integrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("sqlstore: integrity_check: %s", result)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("sqlstore: integrity_check returned %d results", n)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("sqlstore: foreign_key_check failed")
	}
	return rows.Err()
}

// Backup creates a self-contained SQLite online snapshot, including committed
// WAL pages, at a NEW destination path. It never overwrites an existing backup
// or the live database. The destination directory must exist. Failure removes
// the incomplete snapshot; success checks integrity and syncs file and directory.
// Photos and configuration must be backed up separately by the deployment.
// Only writable sources record last_backup; read-only sources remain untouched.
func (s *Store) Backup(ctx context.Context, destPath string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if destPath == "" {
		return errors.New("sqlstore: backup destination required")
	}
	destPath, err = filepath.Abs(destPath)
	if err != nil {
		return err
	}
	// A stale sidecar must never be attached to a newly reserved destination.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e := os.Lstat(destPath + suffix); !os.IsNotExist(e) {
			if e != nil {
				return e
			}
			return fmt.Errorf("sqlstore: backup sidecar exists: %s", suffix)
		}
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				_ = os.Remove(destPath + suffix)
			}
		}
	}()
	if err = f.Close(); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	err = conn.Raw(func(raw any) (err error) {
		bc, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlstore: driver has no online backup API")
		}
		b, err := bc.NewBackup(databaseURI(destPath, false))
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, b.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := b.Step(128)
			if err != nil {
				var se *sqlite.Error
				if !errors.As(err, &se) || (se.Code()&255 != sqlite3.SQLITE_BUSY && se.Code()&255 != sqlite3.SQLITE_LOCKED) {
					return err
				}
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if !more {
				return ctx.Err()
			}
		}
	})
	err = errors.Join(err, conn.Close())
	if err != nil {
		return err
	}
	// Normalize to a single rollback-journal file even if the source header
	// copied by the backup API selected WAL. This also validates the snapshot.
	db, err := sql.Open("sqlite", databaseURI(destPath, false))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	err = integrityCheck(ctx, db)
	err = errors.Join(err, db.Close())
	if err != nil {
		return err
	}
	f, err = os.OpenFile(destPath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destPath))
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	if !s.readOnly {
		return s.noteTime(ctx, "last_backup", time.Now())
	}
	return nil
}
