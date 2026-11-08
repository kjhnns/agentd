// Package sqlstore implements the H1 SQLite store without a dependency on fuel.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Source string

const (
	SourceFueld           Source = "fueld"
	SourceVariablesImport Source = "variables_import"
)

// ErrConflict means an existing identity cannot be reassigned to another row
// or request. Callers must reconcile it, rather than overwrite history.
var ErrConflict = errors.New("sqlstore: identity conflict")

// Store owns one serialized connection. DSN pragmas also apply if database/sql
// replaces that connection. Other processes use WAL and the busy timeout.
type Store struct {
	db       *sql.DB
	readOnly bool
}

// Open opens a filesystem database, creating its parent directories as 0700
// and a new database as 0600. It applies all migrations before returning.
func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("sqlstore: a filesystem path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", databaseURI(path, true))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mode string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err == nil && mode != "wal" {
		err = fmt.Errorf("sqlstore: WAL unavailable: %s", mode)
	}
	if err == nil {
		err = s.migrate(ctx)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func databaseURI(path string, wal bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	if wal {
		q.Add("_pragma", "journal_mode(WAL)")
	} else {
		q.Add("_pragma", "journal_mode(DELETE)")
	}
	q.Set("_txlock", "immediate")
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}).String()
}

func (s *Store) Close() error { return s.db.Close() }

// Row is an original or correction food row. ID is a stable local identity;
// Sequence is assigned on insertion and never changes on import updates.
// Source describes storage ownership, separately from the payload's source.
// Data preserves the Variables JSON object verbatim: item, nullable macros,
// intake, lever keys, corrects, share_after, revision and move metadata, and
// unknown future keys. Missing, null and zero must remain distinguishable.
type Row struct {
	ID               string
	Sequence         int64
	Source           Source
	VariablesValueID string
	OpID             string
	RecordDate       string
	CreatedAt        time.Time
	Data             json.RawMessage
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func validDate(date string) bool {
	t, err := time.Parse("2006-01-02", date)
	return err == nil && t.Format("2006-01-02") == date
}

func (r Row) validate() error {
	if r.ID == "" || !validDate(r.RecordDate) || r.CreatedAt.IsZero() {
		return errors.New("sqlstore: row needs id, record_date and created_at")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &data); err != nil || data == nil {
		return errors.New("sqlstore: row data must be a JSON object")
	}
	if v, ok := data["op_id"]; ok {
		var id string
		if json.Unmarshal(v, &id) != nil || id != r.OpID {
			return errors.New("sqlstore: payload op_id differs from row op_id")
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const rowColumns = `id, local_seq, source, coalesce(variables_value_id, ''), coalesce(op_id, ''), record_date, created_at, data`

type scanner interface{ Scan(...any) error }

func scanRow(sc scanner) (Row, error) {
	var r Row
	var created string
	err := sc.Scan(&r.ID, &r.Sequence, &r.Source, &r.VariablesValueID, &r.OpID, &r.RecordDate, &created, &r.Data)
	if err == nil {
		r.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	}
	return r, err
}

func insertRow(ctx context.Context, tx *sql.Tx, r Row) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO food_rows
        (id, source, variables_value_id, op_id, record_date, created_at, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Source, nullable(r.VariablesValueID), nullable(r.OpID), r.RecordDate, stamp(r.CreatedAt), []byte(r.Data))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertRowWithOutbox commits a fueld row and its export obligation together.
// Duplicate local IDs or op IDs fail; no second obligation is created.
// Target identifies the destination Variables variable, not credentials.
func (s *Store) InsertRowWithOutbox(ctx context.Context, r Row, target string) (Row, error) {
	if err := r.validate(); err != nil {
		return Row{}, err
	}
	if r.Source != SourceFueld || r.OpID == "" || target == "" {
		return Row{}, errors.New("sqlstore: export needs a fueld row, op_id and target")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Row{}, err
	}
	defer tx.Rollback()
	r.Sequence, err = insertRow(ctx, tx, r)
	if err != nil {
		return Row{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO export_outbox(row_id, target) VALUES (?, ?)", r.ID, target); err != nil {
		return Row{}, err
	}
	if err = tx.Commit(); err != nil {
		return Row{}, err
	}
	return r, nil
}

// ListRows returns an inclusive date range in local insertion order, even when
// timestamps or record dates run backwards. Import updates keep that order.
func (s *Store) ListRows(ctx context.Context, from, through string) ([]Row, error) {
	if !validDate(from) || !validDate(through) || from > through {
		return nil, errors.New("sqlstore: invalid date range")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+rowColumns+" FROM food_rows WHERE record_date BETWEEN ? AND ? ORDER BY local_seq", from, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertImportedRow deduplicates by Variables value ID, preserving the first
// local ID and sequence. Imports never enqueue exports. A known fueld echo
// keeps the local payload and adopts the remote timestamp. An echo found by
// op_id binds its Variables ID; additional physical aliases remain imports.
func (s *Store) UpsertImportedRow(ctx context.Context, r Row) (Row, error) {
	if err := r.validate(); err != nil {
		return Row{}, err
	}
	if r.Source != SourceVariablesImport || r.VariablesValueID == "" {
		return Row{}, errors.New("sqlstore: import needs variables_import source and value id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Row{}, err
	}
	defer tx.Rollback()
	r, err = upsertImportedRow(ctx, tx, r)
	if err != nil {
		return Row{}, err
	}
	if err = tx.Commit(); err != nil {
		return Row{}, err
	}
	return r, nil
}

func upsertImportedRow(ctx context.Context, tx *sql.Tx, r Row) (Row, error) {
	old, err := scanRow(tx.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM food_rows WHERE variables_value_id = ?", r.VariablesValueID))
	if errors.Is(err, sql.ErrNoRows) && r.OpID != "" {
		old, err = scanRow(tx.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM food_rows WHERE source = 'fueld' AND op_id = ? AND variables_value_id IS NULL", r.OpID))
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.Sequence, err = insertRow(ctx, tx, r)
	case err != nil:
		return Row{}, err
	case old.Source == SourceFueld:
		if (old.VariablesValueID != "" && old.VariablesValueID != r.VariablesValueID) || old.OpID != r.OpID {
			return Row{}, ErrConflict
		}
		_, err = tx.ExecContext(ctx, "UPDATE food_rows SET variables_value_id = ?, created_at = ? WHERE id = ?", r.VariablesValueID, stamp(r.CreatedAt), old.ID)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE export_outbox SET done_at = coalesce(done_at, ?) WHERE row_id = ?", stamp(r.CreatedAt), old.ID)
		}
		old.VariablesValueID = r.VariablesValueID
		old.CreatedAt = r.CreatedAt
		r = old
	default:
		r.ID, r.Sequence = old.ID, old.Sequence
		_, err = tx.ExecContext(ctx, "UPDATE food_rows SET op_id = ?, record_date = ?, created_at = ?, data = ? WHERE id = ?",
			nullable(r.OpID), r.RecordDate, stamp(r.CreatedAt), []byte(r.Data), r.ID)
	}
	if err != nil {
		return Row{}, err
	}
	return r, nil
}

// MarkExportDone binds the remote alias and completes the obligation atomically.
// Repeating the same acknowledgement preserves the original completion time.
func (s *Store) MarkExportDone(ctx context.Context, rowID, target, valueID string, at time.Time) error {
	if valueID == "" || at.IsZero() {
		return errors.New("sqlstore: export acknowledgement needs value id and time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(f.variables_value_id, '') FROM food_rows f
        JOIN export_outbox e ON e.row_id = f.id WHERE f.id = ? AND e.target = ?`, rowID, target).Scan(&old); err != nil {
		return err
	}
	if old != "" && old != valueID {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "UPDATE food_rows SET variables_value_id = ? WHERE id = ?", valueID, rowID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE export_outbox SET done_at = coalesce(done_at, ?), last_error = '' WHERE row_id = ? AND target = ?", stamp(at), rowID, target); err != nil {
		return err
	}
	return tx.Commit()
}

// IdemRecord retains the existing request and response envelope plus an explicit
// expiry. Status zero denotes an in-progress reservation, as in idem.jsonl.
type IdemRecord struct {
	ClientID  string          `json:"client_id"`
	Hash      string          `json:"hash"`
	Kind      string          `json:"kind"`
	At        time.Time       `json:"at"`
	ExpiresAt time.Time       `json:"expires_at"`
	EntryID   string          `json:"entry_id,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	ItemIDs   []string        `json:"item_ids,omitempty"`
	Status    int             `json:"status,omitempty"`
	Response  json.RawMessage `json:"response,omitempty"`
}

// GetIdem returns sql.ErrNoRows for missing or expired records. The caller
// supplies its clock; expiry is exclusive, including at the exact boundary.
func (s *Store) GetIdem(ctx context.Context, clientID string, now time.Time) (IdemRecord, error) {
	var data []byte
	var r IdemRecord
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM idem WHERE client_id = ? AND expires_at > ?", clientID, stamp(now)).Scan(&data)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	return r, err
}

// PutIdem can advance a reservation to its response, but cannot replace an
// unexpired request with a different hash/kind or overwrite a final response.
// Expired keys may be reused. It is not yet wired to the service claim path.
func (s *Store) PutIdem(ctx context.Context, r IdemRecord, now time.Time) error {
	if r.ClientID == "" || r.Hash == "" || r.Kind == "" || r.At.IsZero() || !r.ExpiresAt.After(r.At) || !r.ExpiresAt.After(now) {
		return errors.New("sqlstore: invalid idempotency record or expiry")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldData []byte
	err = tx.QueryRowContext(ctx, "SELECT payload FROM idem WHERE client_id = ? AND expires_at > ?", r.ClientID, stamp(now)).Scan(&oldData)
	if err == nil {
		var old IdemRecord
		if err := json.Unmarshal(oldData, &old); err != nil {
			return err
		}
		if old.Hash != r.Hash || old.Kind != r.Kind || old.EntryID != r.EntryID || old.ItemID != r.ItemID {
			return ErrConflict
		}
		if len(old.Response) > 0 {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO idem(client_id, hash, kind, at, expires_at, payload) VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(client_id) DO UPDATE SET hash=excluded.hash, kind=excluded.kind, at=excluded.at, expires_at=excluded.expires_at, payload=excluded.payload`,
		r.ClientID, r.Hash, r.Kind, stamp(r.At), stamp(r.ExpiresAt), data)
	if err != nil {
		return err
	}
	return tx.Commit()
}
