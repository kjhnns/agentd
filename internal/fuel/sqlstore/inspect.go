package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OpenReadOnly never creates a database or runs migrations. WAL remains visible;
// immutable=1 is deliberately not used because it would ignore live WAL pages.
func OpenReadOnly(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("sqlstore: a filesystem path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a database file: %s", path)
	}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(ON)"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err == nil && (version < 1 || version > len(migrations)) {
		err = fmt.Errorf("sqlstore: unsupported schema version %d", version)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, readOnly: true}, nil
}

// RowCounts reports every application table, including tables added by a later
// migration. Names are quoted from sqlite_schema, never interpolated user input.
func (s *Store) RowCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, name := range names {
		var n int64
		quoted := strings.ReplaceAll(name, `"`, `""`)
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "`+quoted+`"`).Scan(&n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, nil
}
