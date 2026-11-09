package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// RemoveUnconfirmedShadowRow reconciles a failed legacy operation, including
// during startup replay. Confirmed remote rows remain for late compensation.
// The journal and operation envelopes retain the failed request for audit.
func (s *Store) RemoveUnconfirmedShadowRow(ctx context.Context, opID string) error {
	if opID == "" {
		return errors.New("sqlstore: missing failed operation id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM export_outbox WHERE row_id IN
		(SELECT id FROM food_rows WHERE source='fueld' AND op_id=? AND variables_value_id IS NULL)`, opID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM food_rows WHERE source='fueld' AND op_id=? AND variables_value_id IS NULL`, opID); err != nil {
		return err
	}
	return tx.Commit()
}

// PutShadowRow records the observed legacy outcome. It never exports. Replays
// and uncertain POST acknowledgements retain the first local sequence. An
// import that raced the POST acknowledgement is adopted without losing its ID.
func (s *Store) PutShadowRow(ctx context.Context, r Row, target string) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.Source != SourceFueld || r.OpID == "" || target == "" {
		return errors.New("sqlstore: invalid shadow row")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := scanRow(tx.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM food_rows WHERE (source='fueld' AND op_id=?1) OR variables_value_id=?2 OR (source='variables_import' AND op_id=?1 AND ?2 IS NULL) ORDER BY source='fueld' DESC, local_seq LIMIT 1", r.OpID, nullable(r.VariablesValueID)))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = insertRow(ctx, tx, r)
	case err != nil:
		return err
	default:
		if old.OpID != r.OpID || (old.VariablesValueID != "" && r.VariablesValueID != "" && old.VariablesValueID != r.VariablesValueID) {
			return ErrConflict
		}
		r.ID = old.ID
		if old.Source == SourceFueld {
			r.Data = old.Data
		}
		if r.VariablesValueID == "" {
			r.VariablesValueID = old.VariablesValueID
		}
		// Once confirmed, an old pending replay cannot change the timestamp.
		if old.VariablesValueID != "" {
			r.CreatedAt = old.CreatedAt
		}
		_, err = tx.ExecContext(ctx, `UPDATE food_rows SET source='fueld', variables_value_id=?, data=?, created_at=? WHERE id=?`, nullable(r.VariablesValueID), []byte(r.Data), stamp(r.CreatedAt), r.ID)
	}
	if err != nil {
		return err
	}
	var done any
	if r.VariablesValueID != "" {
		done = stamp(r.CreatedAt)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO export_outbox(row_id,target,done_at) VALUES(?,?,?) ON CONFLICT(row_id,target) DO UPDATE SET done_at=coalesce(export_outbox.done_at,excluded.done_at)`, r.ID, target, done)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MirrorLegacy copies complete legacy envelopes. The legacy stores remain the
// authority, including response replacement and record state transitions.
// Journal keys come from JournalEventKey, shared with legacy imports.
type legacyExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) MirrorLegacy(ctx context.Context, kind, key string, payload json.RawMessage) error {
	return mirrorLegacy(ctx, s.db, kind, key, payload)
}
func mirrorLegacy(ctx context.Context, db legacyExecutor, kind, key string, payload json.RawMessage) error {
	if !json.Valid(payload) {
		return errors.New("sqlstore: invalid legacy JSON")
	}
	p := string(payload)
	var query string
	switch kind {
	case "journal":
		if key == "" {
			return errors.New("sqlstore: missing journal event key")
		}
		_, err := db.ExecContext(ctx, `INSERT INTO journal(kind,at,entry_id,op_id,payload,event_key) VALUES(json_extract(?1,'$.t'),json_extract(?1,'$.at'),json_extract(?1,'$.entry.id'),json_extract(?1,'$.op_id'),?1,?2) ON CONFLICT(event_key) DO NOTHING`, p, key)
		return err
	case "operation":
		query = `INSERT INTO operations(op_id,entry_id,item_id,record_date,state,created_at,payload) VALUES(json_extract(?1,'$.op_id'),json_extract(?1,'$.entry_id'),json_extract(?1,'$.item_id'),json_extract(?1,'$.date'),json_extract(?1,'$.state'),json_extract(?1,'$.created_at'),?1) ON CONFLICT(op_id) DO UPDATE SET state=excluded.state,payload=excluded.payload`
	case "feed":
		query = `INSERT INTO feed(local_seq,id,entry_id,at,payload) VALUES(json_extract(?1,'$.seq'),json_extract(?1,'$.id'),json_extract(?1,'$.entry_id'),json_extract(?1,'$.at'),?1) ON CONFLICT(id) DO NOTHING`
	case "records":
		query = `INSERT INTO records(op_id,record_id,record_type,variable,kind,record_date,variables_value_id,state,at,payload) VALUES(json_extract(?1,'$.op_id'),json_extract(?1,'$.record_id'),json_extract(?1,'$.type'),json_extract(?1,'$.var'),json_extract(?1,'$.kind'),json_extract(?1,'$.record_date'),nullif(json_extract(?1,'$.value_id'),''),json_extract(?1,'$.state'),json_extract(?1,'$.at'),?1) ON CONFLICT(op_id) DO UPDATE SET variables_value_id=excluded.variables_value_id,state=excluded.state,payload=excluded.payload`
	case "idem":
		var r IdemRecord
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if r.ExpiresAt.IsZero() {
			r.ExpiresAt = r.At.Add(7 * 24 * time.Hour)
		}
		_, err := db.ExecContext(ctx, `INSERT INTO idem(client_id,hash,kind,at,expires_at,payload) VALUES(?,?,?,?,?,?) ON CONFLICT(client_id) DO UPDATE SET hash=excluded.hash,kind=excluded.kind,at=excluded.at,expires_at=excluded.expires_at,payload=excluded.payload`, r.ClientID, r.Hash, r.Kind, stamp(r.At), stamp(r.ExpiresAt), p)
		return err
	default:
		return errors.New("sqlstore: unknown legacy kind")
	}
	_, err := db.ExecContext(ctx, query, p)
	return err
}

// Status describes durable copies, not permission to promote to H2. A null
// backup time means no successful Backup has been recorded by this database.
type Status struct {
	Counts     map[string]int64 `json:"counts"`
	LastWrite  *time.Time       `json:"last_write"`
	LastBackup *time.Time       `json:"last_backup"`
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	out := Status{Counts: map[string]int64{}}
	// One read transaction gives mutually consistent counts during writes.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, table := range []string{"food_rows", "operations", "journal", "idem", "feed", "records", "export_outbox"} {
		var n int64
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return out, err
		}
		out.Counts[table] = n
	}
	for key, query := range map[string]string{"pending": "SELECT count(*) FROM export_outbox WHERE done_at IS NULL", "imported": "SELECT count(*) FROM food_rows WHERE source='variables_import'"} {
		var n int64
		if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return out, err
		}
		out.Counts[key] = n
	}
	for key, dst := range map[string]**time.Time{"last_write": &out.LastWrite, "last_backup": &out.LastBackup} {
		var v string
		err := tx.QueryRowContext(ctx, "SELECT value FROM store_metadata WHERE key=?", key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		at, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return out, err
		}
		*dst = &at
	}
	return out, tx.Commit()
}

func (s *Store) NoteShadowWrite(ctx context.Context, at time.Time) error {
	return s.noteTime(ctx, "last_write", at)
}
func (s *Store) noteTime(ctx context.Context, key string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO store_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, stamp(at))
	return err
}
