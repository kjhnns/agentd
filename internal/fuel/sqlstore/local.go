package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
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
			_, e = tx.ExecContext(ctx, `INSERT INTO food_rows(id,source,variables_value_id,op_id,record_date,created_at,data) VALUES(?,'fueld',?,?,?,?,?) ON CONFLICT DO NOTHING`, v.ID, nullable(v.RemoteID), nullable(v.OpID), v.Date, stamp(v.At), []byte(v.Raw))
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
// When export is disabled, imports never reconcile export obligations.
func (s *Store) ImportLocal(ctx context.Context, values []LocalValue, date string, at time.Time, reconcileExports bool) error {
	// Sequences are permanent. Assign them in remote chronology, independently
	// of API response order, for food, body composition and numeric inputs alike.
	values = append([]LocalValue(nil), values...)
	sort.SliceStable(values, func(i, j int) bool {
		if !values[i].At.Equal(values[j].At) {
			return values[i].At.Before(values[j].At)
		}
		return values[i].RemoteID < values[j].RemoteID
	})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE local_values SET deleted=1 WHERE own=0 AND (?='' OR record_date=?)`, date, date); err != nil {
		return err
	}
	acks := map[string][]string{}
	conflicts := map[string]bool{}
	for _, v := range values {
		if v.RemoteID == "" || v.Variable == "" || !validDate(v.Date) || v.At.IsZero() {
			return errors.New("invalid imported value")
		}
		var id, raw, oldDate, oldVariable string
		var own bool
		err = tx.QueryRowContext(ctx, `SELECT id,own,raw,record_date,variable FROM local_values WHERE remote_id=? OR (own=1 AND op_id=? AND ?<>'') ORDER BY own DESC,seq LIMIT 1`, v.RemoteID, v.OpID, v.OpID).Scan(&id, &own, &raw, &oldDate, &oldVariable)
		if err == nil && own {
			if !reconcileExports {
				continue
			}
			local := LocalValue{Variable: oldVariable, Date: oldDate, Raw: raw}
			if !LocalPayloadEqual(local, v) {
				conflicts[id] = true
			} else {
				acks[id] = append(acks[id], v.RemoteID)
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
	// Validate the entire batch before acknowledging any echo. A second row
	// with the same deterministic op_id must not hide a conflicting payload.
	for id := range conflicts {
		if err = conflictLocalExport(ctx, tx, id); err != nil {
			return err
		}
	}
	for id, remotes := range acks {
		if conflicts[id] {
			continue
		}
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT state FROM export_outbox WHERE row_id=?`, id).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if state == "conflict" {
			continue
		}
		for _, remote := range remotes {
			if err = bindLocalAlias(ctx, tx, id, remote); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE export_outbox SET done_at=coalesce(done_at,?),state='done',last_error='' WHERE row_id=? AND state<>'conflict'`, stamp(at), id); err != nil {
			return err
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

// LocalPayloadEqual compares all semantic fields, including fields present on
// only one side. The Variables presentation title does not affect reducers.
func LocalPayloadEqual(local, remote LocalValue) bool {
	if local.Variable != remote.Variable || local.Date != remote.Date {
		return false
	}
	var a, b map[string]any
	if json.Unmarshal([]byte(local.Raw), &a) != nil || json.Unmarshal([]byte(remote.Raw), &b) != nil || a == nil || b == nil {
		return false
	}
	delete(a, "title")
	delete(b, "title")
	return reflect.DeepEqual(a, b)
}

type LocalExport struct {
	LocalValue
	Attempts    int
	Next        time.Time
	State       string
	UncertainAt time.Time
	EmptyReadAt time.Time
	DependsOn   string
}

func (s *Store) LocalExports(ctx context.Context, now time.Time) ([]LocalExport, error) {
	values, err := s.LocalValues(ctx, "")
	if err != nil {
		return nil, err
	}
	groups, err := localExportGroups(values)
	if err != nil {
		return nil, err
	}
	// Include sleeping predecessors when building dependencies. Filtering them
	// in SQL would allow a later correction to overtake an earlier retry.
	rows, err := s.db.QueryContext(ctx, `SELECT v.id,v.variable,v.record_date,v.raw,coalesce(v.op_id,''),e.attempts,e.state,coalesce(e.next_at,''),coalesce(e.uncertain_at,''),coalesce(e.empty_read_at,'') FROM export_outbox e JOIN local_values v ON v.id=e.row_id WHERE e.done_at IS NULL ORDER BY v.seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalExport
	last := map[string]string{}
	selected := map[string]bool{}
	for rows.Next() {
		var e LocalExport
		var next, uncertain, empty string
		if err := rows.Scan(&e.ID, &e.Variable, &e.Date, &e.Raw, &e.OpID, &e.Attempts, &e.State, &next, &uncertain, &empty); err != nil {
			return nil, err
		}
		for _, field := range []struct {
			raw string
			dst *time.Time
		}{{next, &e.Next}, {uncertain, &e.UncertainAt}, {empty, &e.EmptyReadAt}} {
			if field.raw != "" {
				*field.dst, err = time.Parse(time.RFC3339Nano, field.raw)
				if err != nil {
					return nil, err
				}
			}
		}
		group := groups[e.ID]
		e.DependsOn = last[group]
		last[group] = e.ID
		if e.Next.After(now) || e.State == "conflict" || (e.DependsOn != "" && !selected[e.DependsOn]) || len(out) >= 100 {
			continue
		}
		out = append(out, e)
		selected[e.ID] = true
	}
	return out, rows.Err()
}

// Join item identities and aliases across corrections, moves and record voids.
// Keep exported history in the graph so dependencies survive every retry and
// restart, even after a move has acquired a new item id on another date.
func localExportGroups(values []LocalValue) (map[string]string, error) {
	parent := map[string]string{}
	var root func(string) string
	root = func(k string) string {
		if parent[k] == "" {
			parent[k] = k
		}
		if parent[k] != k {
			parent[k] = root(parent[k])
		}
		return parent[k]
	}
	keys := map[string]string{}
	for _, v := range values {
		key := v.Variable + "\x00" + v.ID
		keys[v.ID] = key
		var data map[string]any
		if v.Own || v.Food {
			if err := json.Unmarshal([]byte(v.Raw), &data); err != nil {
				return nil, err
			}
		}
		ids := append([]string{v.RemoteID, v.OpID}, v.Aliases...)
		for _, field := range []string{"item_id", "corrects", "moved_from", "record_id", "voids"} {
			id, _ := data[field].(string)
			ids = append(ids, id)
		}
		for _, id := range ids {
			if id != "" {
				parent[root(v.Variable+"\x00"+id)] = root(key)
			}
		}
	}
	for id, key := range keys {
		keys[id] = root(key)
	}
	return keys, nil
}

// BeginLocalExport schedules reconciliation backoff. Only an actual POST sets
// uncertainty, durably before network IO.
func (s *Store) BeginLocalExport(ctx context.Context, e LocalExport, now time.Time) error {
	delay := time.Minute << min(e.Attempts, 6)
	_, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET attempts=attempts+1,next_at=? WHERE row_id=? AND done_at IS NULL`, stamp(now.Add(delay)), e.ID)
	return err
}

func (s *Store) UncertainLocalExport(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET state='uncertain',uncertain_at=?,empty_read_at=NULL WHERE row_id=? AND done_at IS NULL`, stamp(now), id)
	return err
}

// An empty read is not an authoritative rejection. Require a settle window
// and a second successful empty reconciliation before allowing another POST.
func (s *Store) LocalExportMayRetry(ctx context.Context, e LocalExport, now time.Time) (bool, error) {
	if e.State != "uncertain" {
		return true, nil
	}
	if e.EmptyReadAt.IsZero() {
		_, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET empty_read_at=? WHERE row_id=? AND state='uncertain'`, stamp(now), e.ID)
		return false, err
	}
	return !e.UncertainAt.IsZero() && !now.Before(e.UncertainAt.Add(10*time.Minute)) && now.After(e.EmptyReadAt), nil
}

func (s *Store) RejectLocalExport(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET state='pending',uncertain_at=NULL,empty_read_at=NULL WHERE row_id=? AND done_at IS NULL`, id)
	return err
}

func conflictLocalExport(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE export_outbox SET state='conflict',done_at=NULL,last_error='payload conflict' WHERE row_id=?`, id)
	return err
}
func (s *Store) ConflictLocalExport(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = conflictLocalExport(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
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
	res, err := tx.ExecContext(ctx, `UPDATE export_outbox SET done_at=?,state='done',last_error='' WHERE row_id=? AND state<>'conflict'`, stamp(at), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return tx.Commit()
}

type LocalStatus struct {
	Conflicts        int      `json:"conflicts"`
	ConflictIDs      []string `json:"conflict_ids"`
	Uncertain        int      `json:"uncertain"`
	LastImportError  string   `json:"last_import_error"`
	Pending          int      `json:"pending"`
	OldestPendingAge float64  `json:"oldest_pending_age_seconds"`
	ErrorCount       int      `json:"error_count"`
	LastError        string   `json:"last_error"`
	LastImport       string   `json:"last_import"`
}

func (s *Store) LocalStatus(ctx context.Context, now time.Time) (LocalStatus, error) {
	var st LocalStatus
	var oldest sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT count(*),min(v.created_at) FROM export_outbox e LEFT JOIN local_values v ON v.id=e.row_id WHERE e.done_at IS NULL`).Scan(&st.Pending, &oldest)
	if err != nil {
		return st, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT coalesce(sum(state='conflict'),0),coalesce(sum(state='uncertain'),0) FROM export_outbox WHERE done_at IS NULL`).Scan(&st.Conflicts, &st.Uncertain); err != nil {
		return st, err
	}
	st.ConflictIDs = []string{}
	conflicts, err := s.db.QueryContext(ctx, `SELECT row_id FROM export_outbox WHERE done_at IS NULL AND state='conflict' ORDER BY row_id LIMIT 5`)
	if err != nil {
		return st, err
	}
	for conflicts.Next() {
		var id string
		if err = conflicts.Scan(&id); err != nil {
			conflicts.Close()
			return st, err
		}
		st.ConflictIDs = append(st.ConflictIDs, id)
	}
	err = conflicts.Err()
	conflicts.Close()
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
