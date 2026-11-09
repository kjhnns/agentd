package sqlstore

import (
	"context"
	"database/sql"
)

// ExportEntry describes an outstanding obligation. RowID resolves to a food row;
// Target is a destination variable ID. LastError should contain a safe diagnostic,
// never a response body or token.
type ExportEntry struct {
	RowID     string
	Target    string
	Attempts  int
	LastError string
}

// PendingExports returns obligations in local row order. No network IO is done.
func (s *Store) PendingExports(ctx context.Context) ([]ExportEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.row_id, e.target, e.attempts, e.last_error
        FROM export_outbox e LEFT JOIN food_rows f ON f.id = e.row_id LEFT JOIN local_values v ON v.id=e.row_id
        WHERE e.done_at IS NULL ORDER BY coalesce(v.seq,f.local_seq), e.target`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExportEntry{}
	for rows.Next() {
		var e ExportEntry
		if err := rows.Scan(&e.RowID, &e.Target, &e.Attempts, &e.LastError); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordExportAttempt increments the durable attempt count. Use an empty
// diagnostic before a POST, or record a safe error class for a failed attempt.
// Exporter reconciliation and scheduling are intentionally outside H1.
func (s *Store) RecordExportAttempt(ctx context.Context, rowID, target, diagnostic string) error {
	r, err := s.db.ExecContext(ctx, `UPDATE export_outbox SET attempts = attempts + 1, last_error = ?
        WHERE row_id = ? AND target = ? AND done_at IS NULL`, diagnostic, rowID, target)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}
