package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// LocalValue retains a complete value, with a stable local identity independent
// of its remote alias. Variable is the configured name, never a credential.
type LocalValue struct {
	Sequence int64
	Aliases  []string
	ID       string
	Variable string
	Date     string
	At       time.Time
	OpID     string
	RemoteID string
	Own      bool
	Food     bool
	Raw      string
}
type LocalEvent struct {
	Stream string
	Data   json.RawMessage
}

// CommitLocal is the authority boundary: replay envelopes, all affected values,
// and export obligations become durable together, or none do.
func (s *Store) CommitLocal(ctx context.Context, events []LocalEvent, values []LocalValue, export bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if !json.Valid(e.Data) {
			return errors.New("invalid local event")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO local_events(stream,payload) VALUES(?,?)`, e.Stream, []byte(e.Data))
		if err != nil {
			return err
		}
		if e.Stream != "initialized" {
			seq, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := mirrorLegacy(ctx, tx, e.Stream, fmt.Sprintf("local:%d", seq), e.Data); err != nil {
				return err
			}
			if e.Stream == "journal" {
				if err := projectLocalOps(ctx, tx, e.Data); err != nil {
					return err
				}
			}
		}
	}
	for _, v := range values {
		if v.ID == "" || v.Variable == "" || !validDate(v.Date) || v.At.IsZero() {
			return errors.New("invalid local value")
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO local_values(id,variable,record_date,created_at,op_id,remote_id,own,food,raw) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, v.ID, v.Variable, v.Date, stamp(v.At), nullable(v.OpID), nullable(v.RemoteID), v.Own, v.Food, v.Raw)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			var raw, variable, date string
			if err := tx.QueryRowContext(ctx, `SELECT raw,variable,record_date FROM local_values WHERE id=?`, v.ID).Scan(&raw, &variable, &date); err != nil {
				return err
			}
			if raw != v.Raw || variable != v.Variable || date != v.Date {
				return ErrConflict
			}
			continue
		}
		if v.Food && v.Own {
			// H1 inspection and comparison tools keep seeing the authoritative rows.
			_, e = tx.ExecContext(ctx, `INSERT INTO food_rows(id,source,variables_value_id,op_id,record_date,created_at,data) VALUES(?,'fueld',?,?,?,?,?) ON CONFLICT DO NOTHING`, v.ID, nullable(v.RemoteID), nullable(v.OpID), v.Date, stamp(v.At), v.Raw)
			if e != nil {
				return e
			}
		}
		if export && v.Own && v.RemoteID == "" {
			if _, e = tx.ExecContext(ctx, `INSERT INTO export_objects(id) VALUES(?) ON CONFLICT DO NOTHING`, v.ID); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO export_outbox(row_id,target) VALUES(?,?) ON CONFLICT DO NOTHING`, v.ID, v.Variable); e != nil {
				return e
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO store_metadata(key,value) VALUES('last_write',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, stamp(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) LocalEvents(ctx context.Context, stream string) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM local_events WHERE stream=? ORDER BY seq`, stream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) LocalValues(ctx context.Context, date string) ([]LocalValue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,id,variable,record_date,created_at,coalesce(op_id,''),coalesce(remote_id,''),own,food,raw FROM local_values WHERE deleted=0 AND (?='' OR record_date=?) ORDER BY seq`, date, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalValue{}
	for rows.Next() {
		var v LocalValue
		var at string
		if err := rows.Scan(&v.Sequence, &v.ID, &v.Variable, &v.Date, &at, &v.OpID, &v.RemoteID, &v.Own, &v.Food, &v.Raw); err != nil {
			return nil, err
		}
		v.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	aliases, err := s.db.QueryContext(ctx, `SELECT remote_id,local_id FROM local_aliases`)
	if err != nil {
		return nil, err
	}
	defer aliases.Close()
	index := map[string]int{}
	for i, v := range out {
		index[v.ID] = i
		if v.RemoteID != "" {
			out[i].Aliases = append(out[i].Aliases, v.RemoteID)
		}
	}
	for aliases.Next() {
		var remote, id string
		if err := aliases.Scan(&remote, &id); err != nil {
			return nil, err
		}
		if i, ok := index[id]; ok {
			out[i].Aliases = append(out[i].Aliases, remote)
		}
	}
	return out, aliases.Err()
}
func (s *Store) LocalMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM store_metadata WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
func (s *Store) SetLocalMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO store_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ImportLocal reconciles a successfully completed scan atomically. Tombstones
// apply only to externally owned rows in its scope. Fueld payloads never change.
// The cursor advances in the same transaction, including for an empty scan.
func (s *Store) ImportLocal(ctx context.Context, values []LocalValue, date string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE local_values SET deleted=1 WHERE own=0 AND (?='' OR record_date=?)`, date, date); err != nil {
		return err
	}
	for _, v := range values {
		if v.RemoteID == "" || v.Variable == "" || !validDate(v.Date) || v.At.IsZero() {
			return errors.New("invalid imported value")
		}
		var id, raw, oldDate, oldVariable string
		var own bool
		err = tx.QueryRowContext(ctx, `SELECT id,own,raw,record_date,variable FROM local_values WHERE remote_id=? OR (own=1 AND variable=? AND op_id=? AND ?<>'') ORDER BY own DESC,seq LIMIT 1`, v.RemoteID, v.Variable, v.OpID, v.OpID).Scan(&id, &own, &raw, &oldDate, &oldVariable)
		if err == nil && own {
			if oldDate != v.Date || oldVariable != v.Variable {
				return ErrConflict
			}
			// An echo may add remote presentation fields, but must retain our keys.
			var local, remote map[string]any
			if json.Unmarshal([]byte(raw), &local) != nil || json.Unmarshal([]byte(v.Raw), &remote) != nil {
				return ErrConflict
			}
			for k, x := range local {
				a, _ := json.Marshal(x)
				b, _ := json.Marshal(remote[k])
				if string(a) != string(b) {
					return ErrConflict
				}
			}
			if err = bindLocalAlias(ctx, tx, id, v.RemoteID); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE export_outbox SET done_at=coalesce(done_at,?),last_error='' WHERE row_id=?`, stamp(at), id); err != nil {
				return err
			}
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			v.ID = id
		} else {
			v.ID = "variables:" + v.RemoteID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO local_values(id,variable,record_date,created_at,op_id,remote_id,own,food,raw) VALUES(?,?,?,?,?,?,0,?,?) ON CONFLICT(id) DO UPDATE SET variable=excluded.variable,op_id=excluded.op_id,food=excluded.food,record_date=excluded.record_date,created_at=excluded.created_at,raw=excluded.raw,deleted=0`, v.ID, v.Variable, v.Date, stamp(v.At), nullable(v.OpID), v.RemoteID, v.Food, v.Raw)
		if err != nil {
			return err
		}
		if v.Food {
			_, err = upsertImportedRow(ctx, tx, Row{ID: v.ID, Source: SourceVariablesImport, VariablesValueID: v.RemoteID, OpID: v.OpID, RecordDate: v.Date, CreatedAt: v.At, Data: json.RawMessage(v.Raw)})
			if err != nil {
				return err
			}
		}
	}
	// Remove imported food projections only after the complete scan succeeded.
	if _, err = tx.ExecContext(ctx, `DELETE FROM food_rows WHERE source='variables_import' AND id IN (SELECT id FROM local_values WHERE deleted=1)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO import_cursor(source,cursor,updated_at) VALUES('local:'||?, ?, ?) ON CONFLICT(source) DO UPDATE SET cursor=excluded.cursor,updated_at=excluded.updated_at`, date, stamp(at), stamp(at)); err != nil {
		return err
	}
	return tx.Commit()
}

type LocalExport struct {
	LocalValue
	Attempts int
	Next     time.Time
}

func (s *Store) LocalExports(ctx context.Context, now time.Time) ([]LocalExport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.id,v.variable,v.record_date,v.raw,coalesce(v.op_id,''),e.attempts FROM export_outbox e JOIN local_values v ON v.id=e.row_id WHERE e.done_at IS NULL AND (e.next_at IS NULL OR e.next_at<=?) ORDER BY v.seq LIMIT 100`, stamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalExport
	for rows.Next() {
		var e LocalExport
		if err := rows.Scan(&e.ID, &e.Variable, &e.Date, &e.Raw, &e.OpID, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// BeginLocalExport persists uncertainty BEFORE network IO. A crash or lost reply
// always leads to a reconciliation read, never a blind repeat of a POST.
func (s *Store) BeginLocalExport(ctx context.Context, e LocalExport, now time.Time) error {
	delay := time.Minute << min(e.Attempts, 6)
	_, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET attempts=attempts+1,next_at=? WHERE row_id=? AND done_at IS NULL`, stamp(now.Add(delay)), e.ID)
	return err
}
func (s *Store) FailLocalExport(ctx context.Context, id, diagnostic string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE export_outbox SET last_error=? WHERE row_id=?`, diagnostic, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO store_metadata(key,value) VALUES('export_errors','1') ON CONFLICT(key) DO UPDATE SET value=cast(value as integer)+1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO store_metadata(key,value) VALUES('export_last_error',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, diagnostic); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AckLocalExport(ctx context.Context, id, remote string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = bindLocalAlias(ctx, tx, id, remote); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE export_outbox SET done_at=?,last_error='' WHERE row_id=?`, stamp(at), id); err != nil {
		return err
	}
	return tx.Commit()
}

type LocalStatus struct {
	LastImportError  string  `json:"last_import_error"`
	Pending          int     `json:"pending"`
	OldestPendingAge float64 `json:"oldest_pending_age_seconds"`
	ErrorCount       int     `json:"error_count"`
	LastError        string  `json:"last_error"`
	LastImport       string  `json:"last_import"`
}

func (s *Store) LocalStatus(ctx context.Context, now time.Time) (LocalStatus, error) {
	var st LocalStatus
	var oldest sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT count(*),min(v.created_at) FROM export_outbox e LEFT JOIN local_values v ON v.id=e.row_id WHERE e.done_at IS NULL`).Scan(&st.Pending, &oldest)
	if err != nil {
		return st, err
	}
	if oldest.Valid {
		at, err := time.Parse(time.RFC3339Nano, oldest.String)
		if err != nil {
			return st, err
		}
		st.OldestPendingAge = max(0, now.Sub(at).Seconds())
	}
	err = s.db.QueryRowContext(ctx, `SELECT coalesce((SELECT value FROM store_metadata WHERE key='export_errors'),'0'),coalesce((SELECT value FROM store_metadata WHERE key='export_last_error'),''),coalesce((SELECT max(updated_at) FROM import_cursor WHERE source LIKE 'local:%'),'')`).Scan(&st.ErrorCount, &st.LastError, &st.LastImport)
	if err == nil {
		st.LastImportError, err = s.LocalMeta(ctx, "local_import_error")
	}
	return st, err
}

// Keep H1 inspection tables current in the same transaction as the H2 replay.
func projectLocalOps(ctx context.Context, tx *sql.Tx, raw json.RawMessage) error {
	var r struct {
		T        string            `json:"t"`
		Ops      []json.RawMessage `json:"ops"`
		OpID     string            `json:"op_id"`
		State    string            `json:"state"`
		At       string            `json:"at"`
		ValueID  string            `json:"value_id"`
		Attempts int               `json:"attempts"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	for _, op := range r.Ops {
		if err := mirrorLegacy(ctx, tx, "operation", "", op); err != nil {
			return err
		}
	}
	if r.OpID == "" {
		return nil
	}
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM operations WHERE op_id=?`, r.OpID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var op map[string]any
	if err = json.Unmarshal(b, &op); err != nil {
		return err
	}
	if op["state"] == "done" || op["state"] == "failed" {
		return nil
	}
	op["last_try"] = r.At
	if r.Attempts > 0 {
		op["attempts"] = r.Attempts
	}
	if r.State != "" {
		op["state"] = r.State
	}
	if r.State == "done" {
		op["value_id"] = r.ValueID
		op["done_at"] = r.At
	}
	b, err = json.Marshal(op)
	if err != nil {
		return err
	}
	return mirrorLegacy(ctx, tx, "operation", "", b)
}

// A remote echo may have been imported before its local operation was adopted.
// Preserve that alias, but keep only the fueld-owned value in the read view.
func bindLocalAlias(ctx context.Context, tx *sql.Tx, id, remote string) error {
	var oldID, oldOp, variable string
	var own bool
	err := tx.QueryRowContext(ctx, `SELECT id,coalesce(op_id,''),variable,own FROM local_values WHERE remote_id=?`, remote).Scan(&oldID, &oldOp, &variable, &own)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && oldID != id {
		var op, target string
		if err = tx.QueryRowContext(ctx, `SELECT coalesce(op_id,''),variable FROM local_values WHERE id=?`, id).Scan(&op, &target); err != nil {
			return err
		}
		if own || oldOp == "" || oldOp != op || variable != target {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE local_values SET remote_id=NULL,deleted=1 WHERE id=?`, oldID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM food_rows WHERE id=? AND source='variables_import'`, oldID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO local_aliases(remote_id,local_id) VALUES(?,?) ON CONFLICT(remote_id) DO UPDATE SET local_id=excluded.local_id`, remote, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE local_values SET remote_id=coalesce(remote_id,?),deleted=0 WHERE id=?`, remote, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE food_rows SET variables_value_id=coalesce(variables_value_id,?) WHERE id=?`, remote, id)
	return err
}
