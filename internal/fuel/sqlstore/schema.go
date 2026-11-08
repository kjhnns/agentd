package sqlstore

import (
	"context"
	"fmt"
)

// Migrations are append-only. Each version, including user_version, commits in
// one transaction. JSON envelopes retain the complete existing journal/record
// shapes, including fields that are not yet queried by this standalone store.
var migrations = []string{`
CREATE TABLE food_rows (
    local_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE CHECK (id <> ''),
    source TEXT NOT NULL CHECK (source IN ('fueld', 'variables_import')),
    variables_value_id TEXT UNIQUE CHECK (variables_value_id <> ''),
    op_id TEXT CHECK (op_id <> ''),
    record_date TEXT NOT NULL,
    created_at TEXT NOT NULL,
    data BLOB NOT NULL CHECK (json_valid(data) AND json_type(data) = 'object'),
    CHECK (source <> 'variables_import' OR variables_value_id IS NOT NULL),
    CHECK (source <> 'fueld' OR op_id IS NOT NULL)
);
CREATE INDEX food_rows_date ON food_rows(record_date, local_seq);
CREATE INDEX food_rows_op ON food_rows(op_id);
CREATE UNIQUE INDEX food_rows_own_op ON food_rows(op_id) WHERE source = 'fueld';

CREATE TABLE operations (
    op_id TEXT PRIMARY KEY NOT NULL,
    entry_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    record_date TEXT NOT NULL,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
CREATE INDEX operations_entry ON operations(entry_id);
CREATE TABLE journal (
    local_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    at TEXT NOT NULL,
    entry_id TEXT,
    op_id TEXT,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
CREATE INDEX journal_entry ON journal(entry_id, local_seq);
CREATE TABLE idem (
    client_id TEXT PRIMARY KEY NOT NULL CHECK (client_id <> ''),
    hash TEXT NOT NULL,
    kind TEXT NOT NULL,
    at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
CREATE INDEX idem_expiry ON idem(expires_at);
CREATE TABLE feed (
    local_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    entry_id TEXT,
    at TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
CREATE INDEX feed_entry ON feed(entry_id, local_seq);
CREATE TABLE records (
    local_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    op_id TEXT NOT NULL UNIQUE,
    record_id TEXT NOT NULL,
    record_type TEXT NOT NULL,
    variable TEXT NOT NULL,
    kind TEXT NOT NULL,
    record_date TEXT NOT NULL,
    variables_value_id TEXT UNIQUE,
    state TEXT NOT NULL,
    at TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
CREATE INDEX records_date ON records(record_date, local_seq);
CREATE INDEX records_identity ON records(record_id);
CREATE TABLE export_outbox (
    row_id TEXT NOT NULL REFERENCES food_rows(id),
    target TEXT NOT NULL CHECK (target <> ''),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    done_at TEXT,
    PRIMARY KEY (row_id, target)
);
CREATE INDEX export_outbox_pending ON export_outbox(done_at);
CREATE TABLE import_cursor (
    source TEXT PRIMARY KEY NOT NULL,
    cursor TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`, `
ALTER TABLE journal ADD COLUMN event_key TEXT;
CREATE UNIQUE INDEX journal_event_key ON journal(event_key);
CREATE TABLE store_metadata (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL);
`, `
CREATE TABLE legacy_import_lines (
    source TEXT NOT NULL,
    identity TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload)),
    PRIMARY KEY(source, identity)
);
CREATE TABLE coach_events (
    local_seq INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id TEXT NOT NULL UNIQUE,
    at TEXT NOT NULL,
    payload BLOB NOT NULL CHECK (json_valid(payload))
);
`}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("sqlstore: unsupported schema version %d", version)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("sqlstore: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
