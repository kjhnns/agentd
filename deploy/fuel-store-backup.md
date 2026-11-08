# Fuel store migration and backups

Build from the repository with the existing module cache:

```sh
GOPROXY=off GOSUMDB=off go build -o fuel-store ./cmd/fuel-store
```

The command reads the existing Variables API. It never posts values or creates
export obligations. Set `VARIABLES_API_KEY` through your existing secret setup.
The URL is the ext API base, with `/variables` and `/values` beneath it.

Use the daemon's configured `store_path` for every command below. When it is
omitted, the daemon defaults to `<state_dir>/fuel.sqlite`. For the supplied
configuration:

```sh
FUEL_STORE_PATH="$HOME/.local/state/fueld/fuel.sqlite"
# If fueld sets store_path or a different state_dir, set that resolved path here.
./fuel-store import --state-dir "$HOME/.local/state/fueld" \
  --variables-url "$VARIABLES_URL" --key-env VARIABLES_API_KEY \
  --food-var "$FOOD_VAR" --body-var "$BODY_VAR" \
  --db "$FUEL_STORE_PATH" --dry-run

./fuel-store import --state-dir "$HOME/.local/state/fueld" \
  --variables-url "$VARIABLES_URL" --key-env VARIABLES_API_KEY \
  --food-var "$FOOD_VAR" --body-var "$BODY_VAR" \
  --db "$FUEL_STORE_PATH"

./fuel-store compare --db "$FUEL_STORE_PATH" \
  --variables-url "$VARIABLES_URL" --key-env VARIABLES_API_KEY \
  --food-var "$FOOD_VAR" --from 2020-01-01 --to 2026-11-08
```

Import reads full history, including body values and optional Blood pressure and
Symptom log variables. Optional record variables are discovered by their exact
names. Use `--bp-var ID` and `--symptom-var ID` for renamed variables. Ambiguous
names, invalid IDs and malformed selected values fail explicitly. Absent optional
record variables are allowed.

Import preserves the five JSONL files in the database: journal, idem, feed,
coach-events and records. Journal operations are replayed into their final
states. Original envelopes, including pending operations, response bodies, feed
sequence numbers and unknown keys, survive. Idempotency expiry is seven days
after the original timestamp; expired entries remain stored for audit and are
excluded by the store's normal expiry lookup. Records include void operations.
Remote echoes bind existing record aliases without replacing local request
metadata or pending state. Reconciliation remains the service's responsibility.

Import adds schema version 2 for the import ledger and coach events. Older H1
binaries reject this version. Keep a pre-import snapshot for rollback.

Import commits atomically. A content-and-occurrence ledger prevents duplicate
JSONL events across repeat imports or copied state directories. Food values use
Variables IDs and retain distinct physical copies of the same logical operation.
Initial food sequence follows creation time, then value ID. Later updates retain
local sequence. Repeated imports report zero additions. Import does not delete
local data when Variables deletes a value; compare exposes resulting differences.

Missing JSONL files are allowed. A partial final line is reported and skipped
without truncating the source. Malformed complete lines fail. Dry runs perform
the same import against a temporary copy, including existing database contents,
and never create or migrate the requested database. Temporary copies are removed.

Stop fueld and other writers for a migration capture and final comparison. The
Variables API and five independent JSONL files do not provide a shared snapshot.
Do not import an older state capture over a newer running store. The dry-run
summary is a preview of that capture, not a reservation against concurrent writes.

Compare uses the service's reducers for duplicate operations, correction aliases,
undo, compensation and moves. It reads all dates to resolve move sources outside
the requested range. Totals use integer tenths. `+?N` means N contributions have
an unknown nutrient, and unknown counts must also match. Items means active named
items, as on the day endpoint. Every date in the inclusive range is printed,
including empty dates. Exit status is 0 for parity, 1 for any difference and 2 for
an input, read or integrity error. This command compares totals and item counts;
phase promotion still requires the broader revision, record and UI checks in
spec section 25.

## Nightly snapshot

```sh
install -m 0755 fuel-store "$HOME/.local/bin/fuel-store"
install -d -m 0700 "$HOME/.config/systemd/user" "$HOME/.config/fueld"
printf 'FUEL_STORE_PATH="%s"\n' "$FUEL_STORE_PATH" > "$HOME/.config/fueld/backup.env"
chmod 0600 "$HOME/.config/fueld/backup.env"
install -m 0644 deploy/fuel-store-backup.service deploy/fuel-store-backup.timer \
  "$HOME/.config/systemd/user/"
systemctl --user daemon-reload
systemctl --user enable --now fuel-store-backup.timer
systemctl --user start fuel-store-backup.service
systemctl --user list-timers fuel-store-backup.timer
journalctl --user -u fuel-store-backup.service
```

The timer runs at 03:20 in the host's local timezone. `Persistent=true` catches
missed runs when the user manager starts again. Keep the user manager running
across logout through the host's existing user-service setup. The unit requires
`~/.config/fueld/backup.env` and reads `FUEL_STORE_PATH` from it, with no fallback
path. Use an absolute path; EnvironmentFile values do not expand `~` or `$HOME`.
Keep it synchronized with the daemon's configured `store_path` when changing
configuration. The unit retains 14 snapshots; adjust the output directory and
retention with a unit override. The CLI default retention is 7;
`--keep 0` disables rotation.

```sh
fuel-store backup --db "$FUEL_STORE_PATH" \
  --out "$HOME/.local/state/fueld/backups" --keep 14
fuel-store restore-check --backup /path/to/fuel-20261108-0320.db
```

Backup opens an existing database and uses the SQLite online backup API, including
committed WAL data. The standalone copy passes `integrity_check` and
`foreign_key_check` before rotation. Files are named `fuel-YYYYMMDD-HHMM.db` in UTC,
with mode 0600. Two runs in the same minute fail instead of overwriting a snapshot.
Rotation removes only regular files matching that exact timestamped name, keeps
the newest names and never removes the live source. Failed backups do not rotate.
Restore-check opens read-only, runs both checks and prints all table row counts.
It does not migrate the copy or contact Variables.

## Existing off-box backup system

Add this snapshot directory to the existing host backup system. This repository
does not implement an upload or configure a remote destination. Include all
referenced photos, configuration, target and staple files, and the JSONL state
needed for rollback. Coordinate photo capture with pruning. Protect credentials
and copies with the same access controls as the live service. Schedule the
existing off-box job after a successful snapshot and verify its result separately.

Retain pre-cutover sets and the final Variables export outside the rotating
snapshot directory. Record snapshot time, schema and binary versions, photo
coverage, integrity result and off-box copy result. A database-only snapshot is
an incomplete recovery set. Before H2 and H4, retrieve an off-box set into an
isolated instance, run restore-check, and verify configuration, photos, daily
totals, corrections and moves, record views, feed pagination, idempotent retries
and pending export recovery without contacting production. Follow spec section
25 for the full promotion and rollback gates.
