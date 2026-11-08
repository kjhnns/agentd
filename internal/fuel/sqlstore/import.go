package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LegacyLine preserves an entire JSONL envelope. Identity is a content hash
// plus its occurrence number, so repeated identical journal events survive.
type LegacyLine struct {
	File, Identity string
	Data           json.RawMessage
	EventKey       string // JournalEventKey for journal lines; empty for other files.
}

// ImportBatch is a read-only legacy replay plus the full remote history.
// Operations contains final replayed journal operations in their original order.
type ImportBatch struct {
	Lines      []LegacyLine
	Operations []json.RawMessage
	Food       []Row
	Records    []json.RawMessage
}

type ImportSummary struct{ Lines, FoodAdded, FoodUpdated, RecordsAdded, RecordsUpdated int }

// Import commits the complete migration atomically. It creates no exports.
func (s *Store) Import(ctx context.Context, b ImportBatch) (ImportSummary, error) {
	var summary ImportSummary
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return summary, err
	}
	defer tx.Rollback()
	journalChanged := false
	for _, line := range b.Lines {
		if line.Identity == "" || !json.Valid(line.Data) {
			return summary, errors.New("invalid legacy line")
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO legacy_import_lines(source,identity,payload) VALUES (?,?,?) ON CONFLICT DO NOTHING", line.File, line.Identity, []byte(line.Data))
		if err != nil {
			return summary, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return summary, err
		}
		if n == 0 {
			continue
		}
		if err := importLine(ctx, tx, line); err != nil {
			return summary, fmt.Errorf("%s: %w", line.File, err)
		}
		journalChanged = journalChanged || line.File == "journal.jsonl"
		summary.Lines++
	}
	if journalChanged {
		for _, raw := range b.Operations {
			var op struct {
				ID    string    `json:"op_id"`
				Entry string    `json:"entry_id"`
				Item  string    `json:"item_id"`
				Date  string    `json:"date"`
				State string    `json:"state"`
				At    time.Time `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				return summary, err
			}
			if op.ID == "" || !validDate(op.Date) || op.At.IsZero() {
				return summary, errors.New("invalid journal operation")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO operations(op_id,entry_id,item_id,record_date,state,created_at,payload) VALUES (?,?,?,?,?,?,?)
    ON CONFLICT(op_id) DO UPDATE SET state=excluded.state,payload=excluded.payload`, op.ID, op.Entry, op.Item, op.Date, op.State, stamp(op.At), []byte(raw))
			if err != nil {
				return summary, err
			}
		}
	}
	for _, r := range b.Food {
		if err := r.validate(); err != nil {
			return summary, err
		}
		if r.Source != SourceVariablesImport || r.VariablesValueID == "" {
			return summary, errors.New("invalid imported food identity")
		}
		old, err := scanRow(tx.QueryRowContext(ctx, "SELECT "+rowColumns+" FROM food_rows WHERE variables_value_id = ?", r.VariablesValueID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return summary, err
		}
		if err == nil && old.Source == SourceFueld && old.OpID == r.OpID {
			continue // Already bound echo; the local payload remains authoritative.
		}
		if err == nil && old.Source == SourceVariablesImport && old.OpID == r.OpID && old.RecordDate == r.RecordDate && old.CreatedAt.Equal(r.CreatedAt) && string(old.Data) == string(r.Data) {
			continue
		}
		added := errors.Is(err, sql.ErrNoRows)
		// A local echo binds an alias, it does not add a physical row.
		if added && r.OpID != "" {
			var count int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM food_rows WHERE source='fueld' AND op_id=?", r.OpID).Scan(&count); err != nil {
				return summary, err
			}
			added = count == 0
		}
		if _, err := upsertImportedRow(ctx, tx, r); err != nil {
			return summary, err
		}
		if added {
			summary.FoodAdded++
		} else {
			summary.FoodUpdated++
		}
	}
	for _, raw := range b.Records {
		added, changed, err := importRecord(ctx, tx, raw, true)
		if err != nil {
			return summary, err
		}
		if added {
			summary.RecordsAdded++
		} else if changed {
			summary.RecordsUpdated++
		}
	}
	return summary, tx.Commit()
}

func importLine(ctx context.Context, tx *sql.Tx, line LegacyLine) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line.Data, &m); err != nil || m == nil {
		return errors.New("expected object")
	}
	str := func(k string) string { var s string; _ = json.Unmarshal(m[k], &s); return s }
	at := str("at")
	switch line.File {
	case "journal.jsonl":
		if line.EventKey == "" {
			return errors.New("missing journal event key")
		}
		var entry struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(m["entry"], &entry)
		_, err := tx.ExecContext(ctx, "INSERT INTO journal(kind,at,entry_id,op_id,payload,event_key) VALUES (?,?,?,?,?,?) ON CONFLICT(event_key) DO NOTHING", str("t"), at, nullable(entry.ID), nullable(str("op_id")), []byte(line.Data), line.EventKey)
		return err
	case "coach-events.jsonl":
		if str("entry_id") == "" {
			return errors.New("missing entry_id")
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO coach_events(entry_id,at,payload) VALUES (?,?,?) ON CONFLICT(entry_id) DO NOTHING", str("entry_id"), at, []byte(line.Data))
		return err
	case "feed.jsonl":
		if str("id") == "" {
			return errors.New("missing feed id")
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO feed(id,entry_id,at,payload) VALUES (?,?,?,?) ON CONFLICT(id) DO NOTHING", str("id"), nullable(str("entry_id")), at, []byte(line.Data))
		return err
	case "idem.jsonl":
		var r IdemRecord
		if err := json.Unmarshal(line.Data, &r); err != nil {
			return err
		}
		if r.ClientID == "" || r.Hash == "" || r.Kind == "" || r.At.IsZero() {
			return errors.New("invalid idempotency record")
		}
		r.ExpiresAt = r.At.Add(7 * 24 * time.Hour)
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO idem(client_id,hash,kind,at,expires_at,payload) VALUES (?,?,?,?,?,?) ON CONFLICT(client_id) DO UPDATE SET hash=excluded.hash,kind=excluded.kind,at=excluded.at,expires_at=excluded.expires_at,payload=excluded.payload`, r.ClientID, r.Hash, r.Kind, stamp(r.At), stamp(r.ExpiresAt), raw)
		return err
	case "records.jsonl":
		_, _, err := importRecord(ctx, tx, line.Data, false)
		return err
	default:
		return fmt.Errorf("unknown legacy file %q", line.File)
	}
}

func importRecord(ctx context.Context, tx *sql.Tx, raw json.RawMessage, remote bool) (bool, bool, error) {
	var r struct {
		Op       string    `json:"op_id"`
		RemoteOp string    `json:"remote_op_id"`
		ID       string    `json:"record_id"`
		Type     string    `json:"type"`
		Var      string    `json:"var"`
		Kind     string    `json:"kind"`
		Date     string    `json:"record_date"`
		Value    string    `json:"value_id"`
		State    string    `json:"state"`
		At       time.Time `json:"at"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, false, err
	}
	if r.Op == "" || r.ID == "" || !validDate(r.Date) || r.At.IsZero() {
		return false, false, errors.New("invalid record envelope")
	}
	var old []byte
	var oldOp, oldValue string
	lookupOp := r.Op
	if remote && r.RemoteOp != "" {
		lookupOp = r.RemoteOp
	}
	err := tx.QueryRowContext(ctx, "SELECT op_id,coalesce(variables_value_id,''),payload FROM records WHERE variables_value_id=? OR (op_id=? AND (variables_value_id IS NULL OR variables_value_id=?)) ORDER BY local_seq LIMIT 1", r.Value, lookupOp, r.Value).Scan(&oldOp, &oldValue, &old)
	added := errors.Is(err, sql.ErrNoRows)
	if err != nil && !added {
		return false, false, err
	}
	if !added {
		if oldValue != "" && r.Value != "" && oldValue != r.Value {
			return false, false, ErrConflict
		}
		if !remote && oldOp != r.Op {
			if !strings.HasPrefix(oldOp, "v:") {
				return false, false, ErrConflict
			}
			if _, err := tx.ExecContext(ctx, "UPDATE records SET op_id=? WHERE op_id=?", r.Op, oldOp); err != nil {
				return false, false, err
			}
		}
		if remote && oldOp != r.Op {
			r.Op = oldOp
		}
		if string(old) == string(raw) {
			return false, false, nil
		}
		// Preserve the local operation envelope (request identity, attempts and
		// pending state), while binding the remote alias for reconciliation.
		if remote && !strings.HasPrefix(oldOp, "v:") {
			_, err = tx.ExecContext(ctx, "UPDATE records SET variables_value_id=? WHERE op_id=?", nullable(r.Value), oldOp)
			return false, oldValue != r.Value, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO records(op_id,record_id,record_type,variable,kind,record_date,variables_value_id,state,at,payload) VALUES (?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(op_id) DO UPDATE SET record_id=excluded.record_id,record_type=excluded.record_type,variable=excluded.variable,kind=excluded.kind,record_date=excluded.record_date,variables_value_id=coalesce(excluded.variables_value_id,records.variables_value_id),state=excluded.state,at=excluded.at,payload=excluded.payload`, r.Op, r.ID, r.Type, r.Var, r.Kind, r.Date, nullable(r.Value), r.State, stamp(r.At), []byte(raw))
	return added, true, err
}

// LegacyIdentity is stable across copies of the state directory and preserves
// repeated identical events without depending on file paths or timestamps.
func LegacyIdentity(raw []byte, occurrence int) string {
	return fmt.Sprintf("%x:%d", sha256.Sum256(raw), occurrence)
}

// JournalEventKey identifies a physical event in the append-only journal for
// both shadow writes and imports. endOffset includes the terminating newline;
// raw excludes only that newline, preserving all other bytes. This retains the
// original shadow key format and distinguishes repeated identical events.
func JournalEventKey(endOffset int64, raw []byte) string {
	return fmt.Sprintf("%d:%x", endOffset, sha256.Sum256(raw))
}
