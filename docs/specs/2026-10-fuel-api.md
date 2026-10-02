# Fuel API (agentd `/fuel/*`), contract v2, 2026-09-30

Design: ~/clawd/wiki/pages/fuel-app-design-2026-10.md (published
https://site.gojoe.run/LJ-F7lB8l3QRUQCg). Client: the Fuel iOS app
(kjhnns/fuel-ios). Server: agentd, new package `internal/fuel`, mounted with
`api.MountPublic` on the public listener next to `/watch/*`, wired in
`cmd/agentd`. Public URL https://agent.gojoe.run/fuel/...

v2 folds in a codex review of v1 (17 findings, numbered [C1]..[C17] below).

Decisions (Joe, 2026-09-30): fast path in agentd; write at once, undo in one tap;
name Fuel; watch later. The reward is fast feedback on the macros, so latency is
part of the contract: server-side p50 under 5 s for a text log and under 8 s with
one photo, logged per request.
SUPERSEDED by section 17 (Joe, 2026-10-01: "I prefer quality and context over speed
in this case!"): a correct, context-aware answer in 10 to 30 s beats a wrong one in
3 s. The latency targets above no longer bind the model step; relog and the row
actions (undo, fix, fraction, revert) stay instant.

## 1. Config [C17]

New `[fuel]` section, parsed by internal/config (unknown keys still rejected
elsewhere), validated at startup; a missing or invalid `[fuel]` section leaves
the routes unmounted and logs why.

```
[fuel]
token            = "env:FUEL_TOKEN" | literal   # ResolveToken; pass agentd/fuel-token
variables_url    = "https://app.logvariables.com/api/ext"
variables_key    = "env:VARIABLES_KEY" | literal # pass variables/api-key
food_log_var     = "Food log"          # json variable NAMES, resolved to ids at startup
body_var         = "Body composition"  # must exist, be unique and type json, else fail
model_provider   = "openai"            # "openai" now; interface allows "anthropic" later
model            = "gpt-5.1"           # vision-capable; configurable
model_key        = "env:OPENAI_API_KEY" | literal  # pass openai/api-key
targets_file     = "~/.agentd/fuel-targets.json"
staples_file     = "~/.agentd/fuel-staples.json"
state_dir        = "<agentd state dir>/fuel"
strava_dir       = "~/warehouse/raw-sources/strava"
test_mode        = false
```

Store: stdlib only, no sqlite (the module is stdlib-only). JSONL files under
state_dir, each with an in-memory index rebuilt at startup:
`journal.jsonl` (operations), `feed.jsonl` (conversation), `idem.jsonl`
(idempotency), `photos/` (0600 files, pruned after 30 days).

## 2. Test isolation [C4]

- Unit and integration tests use an injected Variables client backed by a
  loopback fake (append-only, returns value ids, supports `?date=`), a fake ASR,
  a fake model and temp state dirs. The test HTTP transport rejects any host
  other than 127.0.0.1.
- Live end-to-end runs a SECOND agentd process (not the production service) on
  a loopback port with `test_mode = true` and `food_log_var = "Fuel e2e food
  log"`, `body_var = "Body composition"` (read only). In test_mode the server
  refuses to start if food_log_var is "Food log". The e2e json variable is
  created once with the Food log schema.

## 3. Auth and upload safety [C5] [C17]

- `Authorization: Bearer <token>`, constant-time compare, checked BEFORE the body
  is read. Wrong or missing token: 401. Repeated failures: the watch channel's
  throttle (429 after 30 failures a minute, with `Retry-After`). All responses
  `Cache-Control: no-store`.
- Body cap 24 MB per request (http.MaxBytesReader), text max 2000 chars, at most
  1 audio part (10 MB) and 4 image parts (8 MB each), upload read deadline 30 s,
  at most 2 log requests processed concurrently (others wait up to 10 s, then 503
  with Retry-After).
- Images: the CLIENT converts HEIC to JPEG. The server accepts only JPEG or PNG
  by decoded content (415 otherwise), rejects over 30 megapixels, applies EXIF
  orientation, re-encodes to JPEG with metadata stripped, stores that copy, and
  sends the model a copy scaled to 1568 px on the long edge. Photo ids are
  server-generated; `GET /fuel/photo/{id}` resolves through stored metadata only.

## 4. Days and times [C6]

- An entry's instant = `local_time` from the client (RFC3339 with offset), else
  server now. It may not be more than 48 h in the past or 5 min in the future
  (400).
- `recordDate` = that instant converted to `targets.tz` (Europe/Zurich), as
  YYYY-MM-DD. It is persisted with the entry, and every later correction of that
  entry reuses the SAME recordDate.
- DST: conversion uses the tz database; no special casing. ISO weeks Monday to
  Sunday in targets.tz.
- Mutations (undo, fraction) return the snapshot of the entry's day, flagged with
  `date`; the app shows it only if that is the displayed day.

## 5. Variables wire [C7] [C8]

- Write: `POST {variables_url}/values` body
  `{"variableId":"<id>","data":"<JSON object serialized to a string>","recordDate":"YYYY-MM-DD"}`,
  201 returns `{"id":"<value id>",...}`. Read: `GET {variables_url}/values?date=YYYY-MM-DD&includeJson=true`
  returns every value of that day (all variables); filter by variableId locally.
  Field mapping: request `recordDate`, stored `record_date`.
- Every row this API writes carries in `data`: `source:"fuel"`, `op_id` (unique
  per row write), `entry_id`, `item_id`, and for corrections `corrects:<item_id>`.
- Cache [C8]: never read all history per request. At startup hydrate Food log
  rows for the last 35 days (35 `?date=` reads) and Body composition from one
  `includeJson` read; refresh today and yesterday every 2 min and on each
  request if older than 60 s; refresh Body composition every 20 min. Own writes
  are merged into the cache by value id on 201, so a refresh never double counts
  or drops them. Snapshot carries `data_as_of`.

## 6. Writes: journal, idempotency, reconciliation [C1] [C2]

- Idempotency: `client_id` (uuid) + a hash of the normalized request are stored
  in idem.jsonl BEFORE any external write, together with generated entry and item
  ids. Same client_id + same hash: return the stored final response (or the
  in-progress state, see below). Same client_id + different hash: 409
  `idempotency_conflict`. Concurrent duplicates: the second waits on the first.
  Retention 7 days. undo and fraction take a `client_id` too.
- Journal: each row write is journaled `pending(op_id, payload)` before the POST
  and `done(op_id, value_id)` after 201. An uncertain POST (timeout, connection
  loss, 5xx) is NEVER blindly retried: the server reads that day's values and
  looks for the op_id. Found: mark done. Not found after one check: retry the POST
  once with the SAME op_id, then check again.
- If an entry cannot be fully written, the response is 202
  `{"status":"pending_reconciliation", ...}` and a background loop (every 60 s,
  and at startup) finishes or compensates by op_id. Nothing is rolled back
  blindly. The app shows a "syncing" state and polls `GET /fuel/entry/{id}`.

## 7. Correction math [C3] [C11] [C12]

- Item contribution = original row + all its corrections. Macros are numbers or
  null; null stays null through scaling and negation. Rounding: 1 decimal for g
  and kcal, applied once when a row is written.
- net_carbs_g is normalized on the ORIGINAL row at write time (explicit, else
  carbs_g - fiber_g, else carbs_g) so corrections and totals use one value.
- Undo = one correction row equal to the NEGATED CURRENT contribution (so an
  item halved and then undone ends at exactly 0). After undo no fraction is
  allowed (409 `already_undone`). Undo twice: 409.
- Fraction = the absolute share of the SERVED portion eaten, one of 0.25, 0.5,
  0.75, 1, allowed only for items with `needs_fraction: true` and only once.
  It writes a correction of -(1 - f) times the original row; f = 1 writes no row
  but records the choice. If the text already stated the share ("half the
  pizza"), the model sets needs_fraction false.
- Per-item mutations are serialized by a per-item lock.
- Totals: a sum over rows with a null field counts that field as unknown:
  each macro total carries `unknown_rows` (count), never a guess.

## 8. The model call [C9] [C16]

- ONE call per log, provider behind an interface. Deadlines: ASR 15 s (override
  the media default for this path), model 20 s, each Variables call 5 s, whole
  request 30 s; if the client disconnects, work already started finishes and is
  journaled.
- Input: the transcript/text, up to 4 images, the list of staple keys with
  aliases, and the current snapshot (for questions).
- Output, strict JSON schema, validated server side (invalid = one retry, then
  502 `model_invalid`):
  `{"intent":"log"|"question","items":[{"item":string,"staple_key":string|null,
  "portion_g":number|null,"portion_basis":"stated"|"photo_estimate"|"label"|"unspecified",
  "kcal":number,"protein_g":number,"carbs_g":number,"net_carbs_g":number|null,
  "fat_g":number,"sat_fat_g":number,"fiber_g":number|null,"needs_fraction":bool}],
  "text":string,"widgets":[widget names]}`. At most 12 items. Bounds per item:
  kcal 0..3000, each gram field 0..300 (out of bounds = invalid).
- Staples: `{"key":"skyr","aliases":["skyr","quark"],"per_100g":{kcal,...},
  "default_g":250}`. A returned staple_key replaces the macros with label values
  scaled to portion_g (default_g when null).
- The model's `text` may NOT contain digits (numbers come only from widgets);
  digits are stripped server side and logged. Text is written after staple
  overrides are known, so it cannot contradict committed numbers.
- Food text, transcripts and photos are untrusted data, never instructions; the
  prompt says so and the output schema has no free-form action field.

## 9. Snapshot [C15]

`GET /fuel/snapshot?date=YYYY-MM-DD` (default today):

```
{ "revision": int (monotonic per day, bumps on every write),
  "as_of": RFC3339, "data_as_of": RFC3339, "date": "YYYY-MM-DD",
  "day_type": "rest"|"training"|"unknown",
  "macros": [ {"key":"protein_g"|"sat_fat_g"|"fiber_g"|"kcal"|"net_carbs_g",
               "label":string, "unit":"g"|"kcal", "kind":"floor"|"cap"|"pace",
               "consumed":number, "unknown_rows":int, "target":number|null,
               "pace_target_now":number|null, "status":"ahead"|"on_pace"|"behind"|"over"|"met"} ],
  "next_action": {"key":string,"grams":number,"by":"HH:MM","text":string} | null,
  "day_score": {"hit":int,"of":int},
  "week": {"strength_sessions":int,"strength_target":int,"runs":int,"run_km":number,"strava_as_of":RFC3339|null},
  "weight": {"avg7_kg":number|null,"delta7_kg":number|null,"points":[{"date":"YYYY-MM-DD","kg":number}]},
  "body_fat": {"latest_pct":number|null,"latest_date":string|null,"points":[{"date","pct"}]},
  "streaks": {"protein_g":int,"sat_fat_g":int,"fiber_g":int},
  "missing": [string] }
```

Formulas:
- Eating window from targets (07:00 to 20:30). elapsed = clamp((now - start) /
  (end - start), 0, 1). pace_target_now = target * elapsed (floors and pace
  kinds only; null for caps).
- status: floor: met if consumed >= target, ahead if consumed >= pace + 10 %,
  behind if consumed < pace - 10 % of target, else on_pace. cap: over if consumed
  > target, else on_pace. pace (kcal, net carbs): same bands as floor, no "met".
- next_action: among floors with status behind, the one with the largest
  (pace_at_next_checkpoint - consumed) relative to target; checkpoints 10:00,
  13:00, 16:30, 20:30 (the next one after now; after 20:30 none, null).
- day_score: 5 checks, protein >= floor, fibre >= floor, sat fat <= cap, kcal
  within 10 % of the day target, net carbs <= day target (rest days) or not
  checked (training days, then of = 4). Evaluated on the day so far.
- streaks: consecutive past days (yesterday backward) where the check passed;
  today adds 1 only once passed.
- day_type: training if a Strava Run/Ride/Swim of 45 min or more started today
  (strava_dir files, frontmatter + embedded JSON `moving_time`, `start_date_local`);
  unknown if the newest Strava file is older than 36 h (then rest targets are
  used and "strava_stale" is in `missing`).
- weight: daily mean of Body composition weight_kg per local day; avg7 = mean of
  the last 7 days WITH data (at least 3, else null); delta7 = avg7 minus the same
  measure ending 7 days earlier. points = last 30 days.
- body_fat: rows with method withings_bia or dexa; points = last 90 days.
- strength_sessions: ISO-week days with a Push ups or Pull ups value (numeric
  variables, read by name, cached like Food log).

## 10. Routes and wire formats [C13] [C14]

Errors everywhere: `{"error":{"code":string,"message":string,"retryable":bool}}`
with 400 bad_input, 401 unauthorized, 404 not_found, 409 conflict codes, 413
too_large, 415 unsupported_media, 429 rate_limited (+Retry-After), 502
upstream_failed / model_invalid, 503 busy (+Retry-After).

`POST /fuel/log`, `multipart/form-data` (JSON body allowed only for text-only):
fields `client_id` (required), `text`, `local_time`, one `audio` file part,
`image` file parts repeated 0..4. When both text and audio come, the text is
appended after the transcript. Photos are associated with the whole entry
(`photo_ids`), not with single items.

200 (or 202 pending_reconciliation):
```
{ "status":"done"|"pending_reconciliation", "entry_id":string, "intent":"log"|"question",
  "transcript":string|null, "photo_ids":[string],
  "items":[ItemState], "blocks":[Block], "snapshot":Snapshot,
  "latency_ms":{"upload":int,"asr":int,"model":int,"write":int,"total":int} }
ItemState = { "item_id","value_id":string|null,"item","portion_g","portion_basis",
  "kcal","protein_g","carbs_g","net_carbs_g","fat_g","sat_fat_g","fiber_g",
  "needs_fraction":bool, "fraction":number|null, "undone":bool,
  "effective": {same macro keys: the current contribution},
  "actions":["undo","fraction"] (what is still allowed) }
Block = {"type":"text","text":string}
      | {"type":"widget","widget":"macros_today","as_of":RFC3339,"revision":int,
         "data":{"macros":[...snapshot.macros],"added":{macro key: number}}}
      | {"type":"widget","widget":"next_action"|"weight_trend"|"body_fat_trend"|"week"|"streaks",
         "as_of":RFC3339,"revision":int,"data": the snapshot sub-object
         (next_action, weight, body_fat, week, streaks)}
```

`POST /fuel/undo` `{"client_id","item_id"}` and `POST /fuel/fraction`
`{"client_id","item_id","fraction"}`: 200 `{"item":ItemState,"blocks":[text, macros_today],"snapshot"}`;
409 already_undone / fraction_not_allowed / fraction_already_set; 404 unknown item.

`GET /fuel/entry/{entry_id}`: `{"status","entry_id","items":[ItemState],"blocks","snapshot"}`.

`GET /fuel/feed?before=<cursor>&limit=<1..100, default 50>`: newest last, cursor
= opaque feed position, stable under inserts.
```
{"items":[{"id","at":RFC3339,"role":"user"|"fuel"|"coach","text":string|null,
  "photo_ids":[string],"entry_id":string|null,"items":[ItemState],"blocks":[Block]}],
 "next_before":string|null}
```
Item state in the feed is CURRENT (undo and fraction reflected). A widget in
the feed is live in the app only if it is the newest of its kind; the app
refreshes live widgets from `GET /fuel/snapshot` and never lets a response with a
lower `revision` overwrite a higher one.

`GET /fuel/photo/{id}`: image/jpeg; 404 once pruned (the app shows a placeholder).

`GET /fuel/snapshot?date=`: Snapshot.

## 11. Coach hook [C10]

Deferred to the coach phase; v1 does NOT touch the Joe session. v1 appends one
line per log to `state_dir/coach-events.jsonl` (entry id, date, items, totals)
for the later phase to consume. Food text stays data, never instructions.

## 12. Limits and logging

30 logs per hour, 200 per day (429). Logs contain latency, counts and ids, never
the token, image bytes, audio or model output text.

## 13. Acceptance (end to end, run by the builder)

1. Unit/integration suite with the fakes, including: idempotent repeat, 409 on
   changed payload, uncertain POST reconciled by op_id, halve-then-undo ends at
   exactly 0, null fibre stays unknown, recordDate stable across midnight,
   HEIC/other rejected with 415, oversize 413, auth checked before body read.
2. Live e2e with the test instance (section 2): a text log ("250 g skyr and 30 g
   walnuts") and a photo log (a real food photo) each return schema-valid items,
   rows appear in "Fuel e2e food log" with op_ids, snapshot totals match the rows,
   undo and fraction produce the specified rows, and measured latency is reported.
3. The production "Food log" is untouched by testing (count before = after).

## 14. v3 addendum (second codex review), overrides anything above it conflicts with

- [C1] Variables has no server-side dedup, so duplicates are made HARMLESS instead
  of impossible: every total, snapshot and contribution counts a given `op_id`
  ONCE (first value id wins; later rows with the same op_id are ignored). An
  uncertain POST is re-read after 60 s; if the op_id is absent it is re-posted
  with the SAME op_id (at most 3 attempts, 60 s apart).
- [C2] Journal op states: `pending -> done` or `pending -> uncertain -> done |
  retry`. The journal is the source of truth; entry, item and idem state are
  derived from it on startup replay. An entry is `done` when all its ops are
  done. An op still not done after 24 h becomes `failed`; the entry then gets
  compensating corrections for its done rows (as journaled ops themselves), the
  feed gets a `fuel` item "could not save <item>", and the entry status is
  `failed`. Startup replays the journal and resumes every non-terminal op.
- [C8] `?date=` accepts today and the 34 days before it (400 otherwise); streaks
  count at most 30 days back. A refresh REPLACES a cached day from the server
  read, then re-adds own done rows missing from that read only if written less
  than 60 s ago (eventual consistency). External edits and deletions win.
- [C11] unknown_rows counts ACTIVE contributions whose effective field is null;
  an undone item contributes a known 0.
- [C13] undo and fraction may also answer 202 `pending_reconciliation`; the app
  polls `GET /fuel/entry/{entry_id}` every 2 s for up to 60 s. `added` values are
  number or null. Every field in sections 9 and 10 marked `number` without `|null`
  is non-null.
- [C14] Every widget block carries `date` next to `as_of` and `revision`.
  `revision` = the number of done ops for that date, persisted via the journal,
  so it survives restarts. Pace changes over time do not bump it. The app orders
  by (date, revision, as_of) and never replaces a newer value with an older one.
- [C15] A check whose target is null is skipped (`of` shrinks). A past date is
  evaluated at its end (elapsed = 1). avg7(D) = mean of daily means over local
  dates D-6..D that have data, at least 3 such dates, else null; delta7 =
  avg7(D) - avg7(D-7). next_action.grams = ceil(pace at the next checkpoint -
  consumed). runs = Strava Run, TrailRun, VirtualRun this ISO week; run_km = sum
  of distance / 1000, 1 decimal. body_fat points = daily mean per date.
- [C16] The model's text is qualitative commentary on the FOOD only (quality,
  fibre sources, a suggestion) and must not state totals, targets or whether a
  target is met; the prompt says so and digits are stripped. Status sentences
  are generated by code after the writes are done, e.g. "Protein 137 of 160 g,
  fibre is the gap", and placed as the first text block.
- [C17] fuel-targets.json: all keys of the example in the design are required
  (protein_g, sat_fat_g, fiber_g, kcal, net_carbs_g with kind and value or
  rest/training, strength_per_week, weight_band_kg, eating_window, tz). Missing
  file: built-in defaults equal to that example, logged once. Invalid file: fuel
  routes answer 503 `targets_invalid` until fixed; startup does not fail.
  fuel-staples.json: array of {key (unique slug), aliases [string, >=1],
  per_100g {kcal, protein_g, carbs_g, fat_g, sat_fat_g, fiber_g}, default_g}.
  Missing file: no staples. Invalid: staples ignored, logged.
- Pending reservation: an item with a non-terminal op (original row or a
  correction) rejects undo and fraction with 409 `pending`, and recovery uses
  the same per-item serialization.
- Row schemas. Original row `data`: {item, kcal, protein_g, carbs_g,
  net_carbs_g, fat_g, sat_fat_g, fiber_g (number|null), portion_g (number|null),
  portion_basis, source:"fuel", op_id, entry_id, item_id, eaten_at (RFC3339),
  photo_ref (string, only with photos)}. Correction row `data`: {item:
  "correction: <item>", kcal, protein_g, carbs_g, net_carbs_g, fat_g, sat_fat_g,
  fiber_g: each the negated delta (null stays null), source:"fuel", op_id,
  entry_id, item_id: new id, corrects: original item_id, reason: "undo" |
  "fraction" | "compensation", eaten_at}. Rounded to 1 decimal, half away from
  zero.
- Intent: `question` requires zero items and writes nothing. If items are
  non-empty the intent is `log` (a mixed "I had X, how am I doing?" logs X and
  the text may answer). `log` with zero items is invalid output.
- Deadlines: the 30 s clock starts after the body is fully read. If it expires
  before the first Variables write: 504 `timeout`, retryable, nothing journaled,
  and a retry with the same client_id runs again. Once writes started, work
  continues durably and the answer is 202 `pending_reconciliation`; a retry with
  the same client_id returns the current state.
- Audio: `audio/mp4` / `audio/m4a` / `audio/x-m4a` (AAC in MP4, what AVAudioRecorder
  writes) or `audio/wav`; 0.5 to 120 s; validated by container sniffing, 415
  otherwise. Empty transcript with no text and no images: 400 `empty_input`.
- v3.1 (2026-10-01, from the iOS live test): a log request with at least one image and no text and no transcript is ALWAYS a food log. The prompt says so; if the model still returns intent `question` or zero items, the server retries once with an explicit instruction (in the system message) to list the visible foods; if the retry returns zero items but the first answer listed foods, those are logged; if neither listed any, the answer is 200 with intent `log`, zero items, and the code-generated text block "I could not see any food in that photo. Add a word about what it is." No rows are written; the feed records the exchange; no coach event.

## 15. v4 (2026-10-01, Joe): fueld, recent and relog

Overrides sections 1 and 3 where they say "inside agentd".

- Process: Fuel runs as its own service `fueld` (cmd/fueld, same repo, reusing internal/fuel and internal/media). agentd no longer mounts /fuel/* and has no [fuel] section. fueld serves ONLY /fuel/* (everything else 404), recovers a panic per request (500, logged without the body), and shuts down gracefully (in-flight requests finish; the background loop stops).
- Config: its own flat TOML file, default ~/.config/fueld/config.toml, refused unless mode 0600; the keys of section 1 plus `listen` (default "100.120.65.8:8796") and `whisper_model`; state_dir default ~/.local/state/fueld. Sample: deploy/fueld.example.toml; systemd user unit: deploy/fueld.service (Restart=on-failure).
- Grouping fix that recent depends on: a correction joins its original by item_id (rows written by Fuel) OR by value id (rows written by ~/clawd/scripts/food-log, `corrects: <value_id>`).

`GET /fuel/recent?limit=1..100` (default 50):
```
{"items":[{"key":string,"item":string,"portion_g":number|null,
  "macros":{"kcal","protein_g","carbs_g","net_carbs_g","fat_g","sat_fat_g","fiber_g": number|null},
  "last_eaten_at":RFC3339,"times":int,"photo_id":string|null,"source":"fuel"|"agentd"|"other"}]}
```
Built from every Food log row in the 35-day cache window (any writer). Only ACTIVE contributions: an undone item (undo or compensation row) is excluded; a fraction-adjusted item contributes its effective macros and its portion scaled by the eaten fraction. Group key = normalized item name (lowercase, trimmed, inner spaces collapsed) + portion_g rounded to 5 g ("none" when unknown); `key` = first 16 hex of SHA-256 of that, stable. Per group: `times` = number of contributions; item, portion, macros and source of the most recent one; `photo_id` = a stored photo of the most recent contribution that has one, else null. Sorted by last_eaten_at, newest first.

`POST /fuel/relog {"client_id","key","scale"?: 0.5|1|1.5|2 (default 1),"local_time"?}`: no model call. Writes ONE Food log row: the recent item's macros times scale (rounded once; required macros never null), portion_g times scale, portion_basis "repeat", source "fuel", op_id, entry_id, item_id, eaten_at as usual. Idempotent by client_id (same client_id with a different key, scale or local_time: 409). Unknown key: 404. Response = the POST /fuel/log shape (intent "log", items, blocks: status line + macros_today with `added`, snapshot, latency_ms with model 0). The feed records the user turn "again: <item>" and the reply. Target latency under 1 s.

### 15.1 Everything that enters the mouth (Joe, 2026-10-01)

- Drinks (water, coffee, tea, juice, alcohol) and supplements are logged like food, in the same Food log variable. New optional row keys: `kind` "food"|"drink"|"supplement" (always written by Fuel; missing = food), `volume_ml`, `caffeine_mg`, `alcohol_g` (non-negative numbers, omitted when unknown). Macros stay required (water = all 0). Corrections negate these amounts like macros.
- Model item schema adds `kind`, `volume_ml`, `caffeine_mg`, `alcohol_g` (number|null). Prompt: a glass of water is 250 ml unless stated; about 10 g alcohol per standard drink.
- Snapshot adds `intake` (MacroState list): `fluids_ml` (known volume of drinks; floor `water_ml` with rest/training, paced like a floor), `caffeine_mg` (cap), `alcohol_g` (today, no target), `alcohol_g_week` (ISO week up to the date; cap `alcohol_g_week`). For intake a missing amount counts as 0. New widget `fluids` (data = `intake`).
- fuel-targets.json accepts the OPTIONAL keys `water_ml` {"kind":"floor","rest":2500,"training":3000}, `caffeine_mg` {"kind":"cap","value":400}, `alcohol_g_week` {"kind":"cap","value":30}; absent = no target (built-in defaults include them).
- Rows from ~/clawd/scripts/food-log (source "agentd") are read with the same rules: corrections `reason` "undo" | "fix", `corrects` = the ORIGINAL's value id, absolute `share_after`, `portion_g_after`, `volume_ml_after`; duplicates by op_id count once, and a correction that names a duplicate copy of an original belongs to that original.
- /fuel/recent items add `kind` and `volume_ml`; the group key includes the kind and, when the portion is unknown, the volume rounded to 5 ml. Current portion and volume come from the newest correction's `*_after` metadata; only without metadata from the ratio of effective to original macros.

### 15.2 Corrections in one message (Joe, 2026-10-01)

- New model intent `correct`: {"intent":"correct","items":[],"corrections":[{"ref":"last"|item_id|item name,"portion_g":number|null,"volume_ml":number|null,"share":number|null}],...}, exactly one amount per correction, 1 to 12 corrections. The prompt carries LAST LOGGED ITEMS (item_id, item, kind, portion_g, volume_ml of the newest log entry's active items).
- The server resolves `last` to the newest active item of the newest log entry, an item_id directly, else a name among recent log entries (exact normalized match, then containment). Per item it writes ONE correction row (reason "fix", `share_after`, `portion_g_after`, `volume_ml_after`) whose delta makes the item count as target = ORIGINAL row x share (share = portion_g / original portion or volume_ml / original volume). Absolute, so a repeated correction writes nothing. Pending, undone or unresolvable items are skipped with a code-generated note; nothing is written for them.
- Reply in the POST /fuel/log shape with intent "correct": items = the corrected items, blocks = the code-generated summary ("Corrected Water to 300 ml.") plus the status line, and macros_today with `added` = the deltas. Idempotent by client_id like any log.
- `POST /fuel/fix {"client_id","item_id", exactly one of "portion_g"|"volume_ml"|"share"}`: the same correction for the app's "Fix portion" sheet; answer in the undo/fraction shape. 404 if the original row was deleted, 409 portion_unknown / volume_unknown when the original has no portion / volume, 409 already_undone, 409 pending.
- POST /fuel/fraction is now absolute in the same way (target = original x fraction) and writes `share_after`.
- Failure scope (overrides 14 [C2] for corrections): only a failed ORIGINAL row fails its entry and triggers compensation. A failed correction (undo, fraction, fix, compensation) fails only itself and is reported (502 / "could not save the ..."); it never cancels the rest of the meal. A chat correction's status is derived from its own correction ops. `last` means the newest log entry only (also one with no items); an item undone by any writer is not active. macros_today `added` of a correction reply counts only the deltas of the displayed day; corrections of other days are named in the text.

### 15.3 v4.1 (2026-10-01, from the iOS live test fuel-ios 08273ad)

- Chat corrections resolve UNIT-AWARE (overrides the `last` rule of 15.2). A correction with `portion_g` targets the newest active item, among the log entries of TODAY (at most the last 8), that has a portion in grams and is not a drink; one with `volume_ml` targets the newest such item that has a volume; a share-only correction ("only half") keeps the newest active item of the newest log entry. This holds for `last` and also when the model names an item (id or name) whose unit does not fit: the server then picks by unit. A share-only correction given as an item_id only reaches items of the newest log entry; a NAMED item ("only half of the rice") is found among recent entries. If no item fits, the reply is a short "which item?" text and nothing is written. LAST LOGGED ITEMS in the prompt lists today's active items (newest last, at most 20) with `units` ("g" when correctable by portion_g, "ml" when by volume_ml, exactly the server's eligibility) and `newest_entry`; the prompt tells the model to keep the user's unit.
- GET /fuel/recent items also carry `caffeine_mg` and `alcohol_g` (current effective amounts, scaled like the macros; null = unknown) next to `volume_ml`. New optional keys only; `macros` is unchanged (it has carried volume_ml, caffeine_mg and alcohol_g since 15.1).

### 15.4 v4.2 (2026-10-01, from Joe's first real use)

- Chat removals are model intent `undo`, never a correction ("I actually had two entries just now for water. The first with the 250 ml, can you remove that one?" once came back as "Corrected water to 125 ml."). Output: {"intent":"undo","items":[],"corrections":[],"targets":[{"ref": item_id | row_key | "last" | name, "which": "first"|"last"|null, "volume_ml": number|null, "portion_g": number|null}], ...}, 1 to 12 targets. The prompt states that a removal is never a correction; LAST LOGGED ITEMS carry `at` (local HH:MM).
- Resolution among TODAY's active items from all writers (the /fuel/day list): by row_key / item id, else by name (exact normalized, then containment); then by the stated amount (current or originally logged amount within 2 %, at least 1 unit); then `which` = "first" (earliest eaten) or "last" (newest). More than one left with `which` null: the reply asks "Which one do you mean: water, 250 ml (09:12) or water, 500 ml (09:15)? Nothing was removed." and nothing is written. None: "I could not find X in today's log to remove." Each resolved item gets the same undo correction as POST /fuel/undo; reply line "Removed water, 250 ml (09:12)." (code-generated; re-rendered from the op state like corrections). Answer in the POST /fuel/log shape with intent "undo".
- `GET /fuel/day?date=YYYY-MM-DD` (default today in targets.tz; today and the 34 days before): {"date","items":[{"row_key","item","kind","portion_g","volume_ml","macros","caffeine_mg","alcohol_g","eaten_at","source","photo_id","actions":["delete","repeat","fix"],"recent_key"}],"snapshot"}. Every ACTIVE item of the day from all writers, newest first by eaten_at. row_key = the Fuel item_id, else "v:<value_id>" of the original row. Undone items (by any writer) are absent; corrected items show current amounts (the *_after rules of 15.1); recent_key = the /fuel/recent key of the current amounts. Items and snapshot come from one view of the day. 502 when the day cannot be read.
- POST /fuel/undo and POST /fuel/fix accept `row_key` instead of `item_id` (both together only if equal). For "v:<value_id>" rows (other writers) Fuel writes the food-log correction shape: reason "undo" / "fix", corrects = the value id, no entry_id, the negated current contribution incl. volume_ml, caffeine_mg, alcohol_g (undo) or the absolute delta with *_after (fix). op_id is deterministic: "op_undo_<value_id>" (the id the food-log uses too, so both writers' undo counts once) and "op_fix_<value_id>_<hash of the target and of the value ids the row state was computed from>"; a retry with the same client_id replays, a second undo is 409 already_undone. fraction is not offered for such rows (use fix with share).

### 15.5 v4.3 (2026-10-01, from build 2)

- Several photos in one POST /fuel/log are views of the SAME meal or item (angles, package front, nutrition label, menu line), never separate servings. The system prompt says so always, and adds "This request has N photos. They all show the SAME meal ..." when N > 1: each food is listed once, a readable label or printed package weight overrides visual estimates (portion_basis "label"), the portion combines all photos. All photos are stored or none (a failed save removes the saved ones, writes nothing and answers 500 retryable); the log response, GET /fuel/entry (new `photo_ids`), the user feed item and every row carry all of them in upload order (row `photo_ref` = comma list). /fuel/day items add `photo_ids` (all stored photos, in order); `photo_id` stays = the first, for compatibility.
- Removal targets add `names`: the exact item names from LAST LOGGED ITEMS the user's words mean (the model may map a synonym such as "sparkling water" only when the words plausibly mean it). With names, candidates are today's items whose normalized name is one of them (an id given as ref counts only if its item is one of the names) (exact, case-insensitive), BEFORE amount and which: "remove the water I just logged" removes the newest water; several waters and which null ask "Which one do you mean: ..." listing only the waters; no match = "I could not find X ..." and no write. LAST LOGGED ITEMS now also list today's rows of other writers (row_key "v:<value_id>"), so their names can be mapped.
- Not done (deliberately): POST /fuel/photos (add photos to an existing entry and re-estimate). A re-estimate changes the macro composition per gram (for example label values), which the absolute fix rows (target = original x share) cannot express; it needs its own correction kind with rules for recent, fix and undo. Adding photos to an existing entry is therefore unsupported. To use more photos, undo the original entry and log once again with all photos; do not log the same meal twice.

### 15.6 v4.4 (2026-10-01, from a real failure)

"Can you log for yesterday that I drank 2x glasses of champagne, 1x of white wine, 1x Negroni sbagliato" was logged on TODAY, and "Alcohol was supposed to be all logged for yesterday" changed nothing.

- Day-aware logging. Model output adds `"day": "today" | "yesterday" | "YYYY-MM-DD" | null` (null = today) and `"time": "HH:MM" | null`. The system message carries "Now: <weekday> <date> <time>" in targets.tz and yesterday's weekday and date, so the model resolves "yesterday", "last night", "this morning" (today), "on Monday", "on the 28th" to a date. The server accepts today and the 34 days before it: a future day or an older one is answered 200 with intent "log", zero items and a code-generated text ("I cannot log for a day in the future (...). Nothing was logged." / "I can only log for today and the 34 days before it; ... is too far back. Nothing was logged."), nothing written. recordDate = that day; eaten_at = the stated time, else 12:00 of a past day (for today the request time; a stated time in the future is ignored). The reply is for THAT day: snapshot, widgets (their `date`) and the first text block "Logged for Wed 30 Sep: a, b and c. Wed 30 Sep: <status line>". The label is set whenever the resolved day is not today or differs from the request's own day (`local_time`).
- New chat intent `move`: {"intent":"move","day":...,"targets":[...]} with the undo target rules. No targets = every active item of the newest log entry. A target is matched in the newest log entry first (EVERY matching item moves, or the one `which` picks); else like a removal among today's items (ambiguous or unknown = a question, nothing moved). The day follows the same 34-day rule (refusal text, nothing moved); an item already on that day is skipped with a note.
- A move writes, per item, a NEW original row on the target day (the item's current effective amounts and portion, the same wall-clock time, the same photo_ref, `moved_from` = the old item id, a new item_id in the move entry) and an undo row on the old day (`moved_to`). Both ops are in ONE journal txn; the undo is posted only after the new row is done (journal `pair_op`), also by the reconciler after a crash.
- Exactly one side counts at any time, by a rule that reads only ROWS: the destination row carries `moved_from` and `moved_from_date`, and its whole contribution group (duplicates and corrections by item id or value id included) counts only once the SOURCE item is undone in the rows of its own day (or the source row is gone, or that day is not cached). Until then the source counts and the destination is left out of totals, snapshots, /fuel/day and /fuel/recent; both items are `pending` for undo / fix / fraction until the pair settles. Outcomes: both done = moved. The new row rejected or failed = the undo is failed without being posted, the item stays on its old day. The undo rejected on its FIRST attempt after the new row was written = the new row is cancelled by ONE compensation row (op_id "op_cmp_<new op id>", derived from the journal on every reconcile pass, so neither a crash nor two concurrent passes can lose or double it). A move op (the new row or the undo) that was ever uncertain is never failed, neither by the 24 h deadline nor by a rejected retry: it keeps its op_id and is re-checked and re-posted with backoff until it is done; only a rejection of its FIRST attempt is final. The source stays pending as long as any op of the move, or a needed cancellation, is unsettled. An unnamed `last` target means the newest log entry (all its items, narrowed by amount and which), never an older one.
- A move entry never fails as a whole and never answers 502: its status is pending_reconciliation (202) while any of its ops is unsettled or a needed cancellation does not exist or is not done, else done (200), with the outcome per item in the text: "Moved champagne, white wine and Negroni sbagliato to Wed 30 Sep.", "Still moving X to ...", "Could not move X; it stays where it was." (rendered from the ops' current state; POST /fuel/log shape, intent "move", snapshot and widgets of the target day, items = the new items). Names constrain ids as for removals. Idempotent by client_id. The ISO-week alcohol total does not change when both days are in the same week.
- Clock times are wall-clock times in targets.tz built per date (DST days included); a stated time later than now is ignored; when the model's day differs from the request's own instant, the fallback is the request time for today and 12:00 for a past day.
- Alcohol: the prompt tells the model to COMPUTE alcohol_g = volume_ml x ABV x 0.789 (champagne and wine 12 %, beer 5 %, spirits 40 %, cocktails by composition), never a flat amount per drink (300 ml champagne = 28 g, not 20 g).

## 16. v5 (2026-10-01, Joe): second-opinion recalibration of photo meals

A photo estimate from the fast model is checked afterwards by an independent agent that is briefed to find the error, not to confirm. The fast path (section 6) is unchanged: the log is answered and counted first; the second opinion arrives later and may write one correction.

Architecture rule (Joe): agentd stays a universal agent daemon. fueld is an ordinary CLIENT of agentd's existing API and nothing about Fuel lives in agentd. The design does not depend on which harness answers (claude-code, codex).

- Config (flat keys of the fueld config; default off): `recalibrate_enabled = false`, `recalibrate_agentd_url = "http://127.0.0.1:8798"`, `recalibrate_agentd_token` (literal or "env:VAR"; required when enabled), `recalibrate_timeout = "10m"` (one job), `recalibrate_min_interval = "30s"` (between job starts), `recalibrate_kill_file` (default `<state_dir>/recalibrate.off`). While the kill file exists no job starts (a running one finishes); queued jobs wait and expire 24 h after their log.
- Job. A POST /fuel/log with intent "log", at least one stored photo and at least one item enqueues ONE job for the entry (`state_dir/recal.jsonl`, fsynced, last line per entry wins; states queued, running, done, failed). One worker, at most one job at a time, oldest first, not more often than the minimum interval. A job starts only when its entry is `done`; a failed entry fails the job. A job that was running when the process stopped is queued again at startup (2 attempts at most, then failed); photo logs of the last hour without a job get one at startup. Text, voice and relog entries get none.
- How the photos reach the agent (generic agentd API, bearer token): `POST /sessions` (a fresh session per job), then one `POST /sessions/{id}/media` turn per photo (multipart `file` = the stored JPEG rescaled to 1568 px, `text`): the daemon stores the file in its own workspace and tells its agent the path. Turns 1..N-1 carry "Photo k of N ... Reply only: READY"; the last turn carries the task. `DELETE /sessions/{id}` always follows (also after a timeout). The agent's final text of the last turn is the answer; `harness` of the session is recorded as the tag `agentd:<harness>`.
- The task: the adversarial brief, the user's note and the fast estimate per item (item_id, name, portion basis, portion, macros) as DATA, and the demand to answer with ONLY one JSON object: {"items":[{"matches": item_id | null, "item", "portion_g": number|null, "kcal", "protein_g", "carbs_g", "net_carbs_g": number|null, "fat_g", "sat_fat_g", "fiber_g": number|null, "confidence": "low"|"medium"|"high", "reason"}]}. One object per fast-estimate item, plus one with `matches` null per food the fast estimate missed.
- The answer is UNTRUSTED. The JSON block is extracted (a fenced block or the outermost braces) and validated strictly: exactly the key `items`; 1 to 12 objects; each with exactly the 12 keys; numbers are numbers; kcal 0..3000, every gram value 0..300, portion_g above 0 and at most 5000; confidence one of the three; `matches` an item id of THIS entry, each at most once, and every item of the entry matched; item and reason text reduced to one printable line (120 / 240 characters). Anything else = the job is failed and NOTHING is written. Agent text is never logged or shown except the validated `item` and `reason`.
- Apply rule (deterministic, per matched item, under the item lock, after a fresh read of the day's rows). Recalibrated values = the second opinion for every macro the item currently has as a known value (a null stays null; net_carbs_g null in the answer = carbs minus fibre). It DIFFERS when kcal is off by more than 20 % AND more than 40 kcal, or protein by more than 5 g, or sat fat by more than 2 g. Not different = `agreed`, nothing written. Different and confidence low = `suggested`, nothing written. Different and the new kcal outside one third .. three times the current kcal (or above 40 kcal on a 0 kcal item) = `suggested` ("Too far from the first estimate"), nothing written. Otherwise `applied`: ONE correction row.
- User edits win and other writers are never touched: an item is `skipped` (nothing written) unless it has exactly its original row, written by Fuel and done, no correction row from anyone, no undo, no fraction choice, no pending or failed op. One recalibration per item, ever: a second job for the entry does not exist, and a job that runs again after a crash adopts the row it already journaled.
- The correction row: the usual correction shape (section 14) with `reason: "recalibrate"`, the per-macro DELTAS (recalibrated minus current), `share_after: 1`, `portion_g_after` = the recalibrated portion (the logged one when the answer has none) and `recalibrated: {"from": {portion_g, kcal, protein_g, carbs_g, net_carbs_g, fat_g, sat_fat_g, fiber_g}, "to": {...}, "reason", "confidence", "by": "agentd:<harness>"}`. volume_ml, caffeine_mg and alcohol_g are not recalibrated. Journal, op states, reconciliation and the snapshot revision work as for every correction (the revision moves when the row is done).
- New BASE. While an item has a recalibrate row and no revert row, `recalibrated.to` (macros and portion) replaces the original row as the base of everything computed later: POST /fuel/fix and chat corrections (share, grams), POST /fuel/fraction, the `*_after` portion of those rows, /fuel/recent, /fuel/relog and /fuel/day amounts, and the item's `macros` and `portion_g` in entry, feed and mutation answers (`effective` = the current contribution, as always). Undo still negates the item's current contribution. A move copies the current effective amounts.
- Missed items (`matches` null) are never added automatically: they are `suggestions` on the entry and a line in the coach text.
- Surfacing. (1) The feed gets ONE item with role "coach" for the entry when the job is done (entry_id set, text and one text block), for example "Second opinion: pasta 300 g, not 200 g (plate fills the bowl): +180 kcal, +6 g protein. Applied. The rest holds.", "Second opinion agrees.", "Suggests a missed item: olive oil, about 10 g. Add it?", "... Low confidence, not applied.", "Second opinion for pasta was not applied: the item changed meanwhile." A failed job writes no feed line, with one exception: if a recalibrate row of the entry was journaled before the failure (a crash after the row, then a failed second run), the line reports exactly those rows, so a written change is never silent. The line is written only once every recalibrate row of the entry is settled (done or rejected), so it never claims an unsaved change: a rejected row reads "... Could not be saved, not applied." (2) Items in the POST /fuel/log answer, GET /fuel/entry, feed item cards, mutation answers and GET /fuel/day carry an optional `recalibration` object: {"state": "pending"|"agreed"|"applied"|"suggested"|"skipped"|"failed"|"reverted", "summary", "confidence", "reason", "from", "to", "deltas": {macro: number|null}, "can_revert": bool, "by", "item_id"}. `applied` items show "pending" while their row is not done and "failed" when it was rejected. (3) GET /fuel/entry adds an entry-level `recalibration`: {"state": pending | agreed | applied | suggested | failed, "summary", "by", "items": [...], "suggestions": [{"state":"suggested","summary","confidence","reason","to"}]}. The key is absent when the entry has no job. A stored idempotent replay of POST /fuel/log keeps the state of its first answer (pending); current state is on GET /fuel/entry, /fuel/day and the feed.
- `POST /fuel/recalibration/revert {"client_id","item_id"}` undoes an applied recalibration with ONE compensating row (`reason: "recalibrate_revert"`, the negated deltas, `share_after: 1`, `portion_g_after` = the logged portion, `reverts` = the op_id of the recalibrate row); the original row is the base again. Answer and idempotency as POST /fuel/undo (MutationResponse; text "Reverted the second opinion on X. ..."). 409 `not_recalibrated` (nothing applied), `already_reverted`, `changed_after` (a fix, fraction or a correction of another writer came after the recalibration: correct the amount instead), `already_undone`, `pending`. A fraction choice after the recalibration counts as a change too, also "all of it" (which writes no row). A fix that is a no-op (the item is already counted at that amount, so no row is written and nothing changed) is NOT a change: the revert stays possible, and it is always an explicit user action. `can_revert` is true exactly when the revert would be accepted; while the revert row is not done the item's state is `pending`. An item's recalibration is always rendered from its journaled recalibrate op (state, from, to, deltas), independent of the job store. Chat corrections in grams and LAST LOGGED ITEMS use the recalibrated portion. The item is not recalibrated again.

## 17. v6 (2026-10-01, from a real failure): context, re-estimates, additive follow-ups, plausibility, quality over speed

The failure (production entries en_5e603c3b6bd6c5668c73 and following): a roast chicken on a kitchen scale reading 382 g was logged as 1200 g; "380 g incl bones" and "265 g of chicken (pure meat and skin)" were scaled linearly at bone-in values; "one more bite" SET the item to 120 g. The day ended about 500 kcal and 57 g protein short. Joe: "the chat is not that smart and doesn't connect the dots well" and "I prefer quality and context over speed in this case!"

Priority. Quality and context win over speed for every chat turn (logs included). The deterministic parts (totals, validation, plausibility guards, journal, idempotency) stay exactly as strict. Relog and the row actions need no model and stay instant.

A. Context. Every model call carries, as labelled untrusted DATA in the user message (never in the system message):
- CONVERSATION TODAY: all of today's feed turns, oldest first (user texts with a "[N photo(s)]" mark, Fuel replies, coach lines), each {at HH:MM, role, text}; bounds only against runaway input (80 turns, 600 characters per turn, 16000 in total, oldest dropped first).
- LAST LOGGED ITEMS (today, as before) now also say what each item counts as NOW: current portion_g, kcal, protein_g, portion_basis, `logged_as` (the original name, portion and kcal once it was changed), `changes` (its corrections in order, "19:49 fix to 380 g"), `photos` (stored photos of its entry). A re-estimated or recalibrated item appears under its current name and portion.
- YESTERDAY'S ITEMS (all writers) and the RECENT LIST (30 items: name, usual portion, kcal, protein, times, last date), so "the usual skyr" or "same as yesterday" resolve. The DAY SNAPSHOT (targets included) as before.
- Stored photos with a correction: when a text-only turn comes back as intent `correct`, the server asks ONCE more with the stored photos (at most 3) of the entries the corrections refer to, marked as "the ALREADY LOGGED entry this input refers to, not a new meal", and uses that answer if it is again a valid correction.

B. Re-estimate. A correction has exactly ONE form:
- size only (the same thing measured the same way): `portion_g`, `volume_ml` or `share` (linear scaling of the base, section 15.2);
- additive: `portion_g_delta`, `volume_ml_delta` (the amount added; negative = taken off) or `count_delta` (how many more of the logged serving); the server adds it to the item's CURRENT amount;
- `revised`: {item, portion_g, kcal, protein_g, carbs_g, net_carbs_g, fat_g, sat_fat_g, fiber_g}: the user restated the amount in DIFFERENT TERMS (edible part instead of with bones, cooked instead of raw, another food or cut, a label), so the item gets a new full macro set and may be renamed. The server writes ONE correction row with `reason: "revise"`, the deltas to the item's current contribution (a known 0 for every unchanged known field; a null stays null), `share_after: 1`, `portion_g_after` and `recalibrated: {from, to, reason: "restated by the user", by: "user", item: <new name>}`: the recalibrate row shape and its base rules (section 16). The item's BASE is the `to` of its NEWEST recalibrate or revise row that no revert row names (`reverts` = that row's op_id); later fixes, fractions, deltas, recent, relog and day build on it, and the item shows the new name everywhere (entry, feed, day, recent, LAST LOGGED ITEMS). A revise after a second opinion blocks its revert (`changed_after`); a second opinion never touches an item with a revise row. Validation bounds as for items. A re-estimate that changes nothing writes nothing ("... is already counted like that."). Reply line: "Re-estimated X as Y, 265 g: 633 kcal, 72 g protein."; deltas: "Added 15 g to X: now 280 g." / "Took 50 g off X: now 150 g." / "Added 1 x X: now 2 x what was logged."
- A re-estimate or count_delta resolves its item by id or name among the active items logged for TODAY (not only the newest entry, never another day). A revise row also freezes the item's current intake amounts (`recalibrated.intake`: volume_ml, caffeine_mg, alcohol_g, and `volume_ml_after`): they are part of the base from then on, so a halved drink stays halved. count_delta adds servings of the base to the recorded current amount.

C. Additive follow-ups never reduce. A message with additive wording (one / two / ... / ten / a couple / N more, "N g more", "more bites / slices / glasses", another, extra, additional, a second one / helping, seconds, "plus N", "also had"; NOT "more like", "more than", "no more X", "not any more") whose answer is a correction that makes an item smaller (a lower absolute amount, a lower share, a negative delta, a re-estimate with fewer kcal) is asked ONCE more with an explicit server note; if the answer still reduces, nothing is written and the reply asks (the write path repeats the check under the item lock on fresh rows, so no path writes a reduction for an additive message): "That sounds like you had MORE, but I read it as a smaller amount. How much more was it, or what is the new total? Nothing was changed." (intent `correct`, no items). A log of the added amount as a new item is an accepted answer.

D. Read the photo. The prompt tells the model to read weighing scales (display digit by digit; a timer next to the weight is not part of it), package weights and labels before estimating by eye. Items add `scale_g` (the reading, or null) and portion_basis gains "scale". Code rule: an item without a portion takes the scale value; an estimate of MORE than 2 x the scale reading is replaced by the scale value, the macros scale with it, portion_basis becomes "scale" and the reply says "Used the scale reading for X: 382 g, not the estimate of 1200 g." An estimate below the reading stands (the edible part of a bone-in reading, an untared plate, a misread display).

E. Plausibility in code. Items add `food_class` (leafy_vegetable, vegetable, fruit, meat_fish, dairy, grain_starch, nuts_seeds, oil_fat, sweet, mixed_dish, drink, supplement, other). Per item: kcal and the macro energy (4 x protein + 4 x carbs + 9 x fat + 7 x alcohol) may differ by at most 40 kcal or 25 % of the LARGER of the two, whichever is more; sat fat not more than 0.5 g above fat; protein + carbs + fat not heavier than the portion (tolerance 5 % + 1 g); kcal per gram at most 9.2 and inside the class bounds (leafy_vegetable up to 0.4, vegetable 1.2, fruit 1.8, meat_fish 0.5 to 5, dairy 0.2 to 5, grain_starch 0.5 to 5.5, nuts_seeds 4 to 7.5, oil_fat 2.5 to 9.2; items under 15 kcal, drinks and supplements are exempt from the class bounds; an item with a KNOWN staple key is not checked, because the server replaces its macros by the label values; the same rules apply to a `revised` re-estimate, which carries `food_class` too and is checked with the item's kind, its retained alcohol and, when revised.portion_g is null, its current portion: after the one re-ask an implausible re-estimate is NOT written and the reply says "I could not re-estimate X reliably (...). Nothing was changed for it; tell me the amount again."). Any implausible item triggers ONE re-ask (the server note names item numbers and reasons, never model-made names). What is still implausible is logged WITH a visible flag, never silently: the item carries `check` (the reason), the row carries `note: "check: <reason>"`, and the reply has the text block "Check this: <item>: <reason>."

F. Second opinion (section 16). Each opinion object adds the required key `evidence`: "scale" (a readable scale display gives the portion, directly or as its edible part), "label" (a printed weight of the whole item) or "visual". With scale or label evidence the brief demands confidence high (medium when the edible part is estimated), never low, and the 3 x plausibility bound does not apply. Low confidence still blocks. `evidence` is stored in `recalibrated`.

G. Model per turn. Config: `model` + `model_effort` answer every turn; optional `model_chat` + `model_chat_effort` re-answer a turn the first model classifies as correct, undo, move or question (never a photo-only log). The choice is by correctness on the fixed evaluation set (`go test -tags eval -run TestEval ./internal/fuel`, built from the failures above and normal logs); results are on the wiki page fueld.

Timing and progress. `log_budget` (default "90s") bounds POST /fuel/log after the body is read; `model_timeout` (default "60s") bounds one model call; a turn makes at most 4 model calls (the first answer, then in this order and while the limit allows: the chat model, the stored-photo pass, the re-ask for an implausible re-estimate, the additive re-ask, the plausibility re-ask of a log; a refused extra call keeps the answer the turn has, and the guards still apply to it). Each call may be repeated once when its output is invalid JSON for the schema, as before. Other routes keep the 30 s budget. Progress for the app: the SINGLE response stays (no polling route). The client shows a "thinking" state from the moment it sends until the response, with a client timeout of 120 s; on a timeout or a lost connection it repeats the request with the SAME client_id: the server either waits for the first request to finish (and answers its result) or replays the stored answer, never a second write. 504 `timeout` (nothing written, retry) and 202 pending are unchanged. `latency_ms.model` is the sum of all model calls of the turn.

## 18. v7 (2026-10-02, Joe): framework-driven targets

Source: ~/clawd/wiki/pages/health-framework-evidence-2026-10.md ("the report": driver tree, recomposition section, "Five highest-yield changes", decision log of 2026-10-02: "the plan looks sound to me"). Explainer page: https://site.gojoe.run/brkGEwlVPGzT6U0s (password protected). This section is a contract only. It overrides sections 9, 14 [C17] and 15.1 where they conflict. Nothing here changes a number in ~/.agentd/fuel-targets.json by itself. Every number that the report leaves open, and every constant that this spec proposes without a source in the report, is listed in 18.12 and is `null` in the file until Joe decides. A `null` always has a defined behaviour ("not set"); the server never substitutes a default for it.

Owner rule (the report's H / A / P). H = Joe decides. A = the app computes or reminds. P = a clinician sets or interprets. The app shows a target, a status, a colour, a streak or a score ONLY for H and A values. A P value is stored and shown as a record. It has no target, no status and no judgement text. A clinician instruction is stored as text and shown verbatim with name and date (18.6). No field of this contract holds a P value as a number that the app compares with anything.

### 18.1 Compatibility with iOS build 7 (1.5.0)

- Every route, field, key and enum value of sections 9 to 17 stays as it is. v7 only ADDS routes and JSON keys. No existing field gets a new enum value, a new unit, a new type or a new meaning. No new widget name is used in `blocks` (a v7 client draws its new cards from the new snapshot keys).
- Legacy fields are computed by the v6 rules from the legacy keys: `macros` (exactly protein_g, sat_fat_g, fiber_g, kcal, net_carbs_g), `intake` (its four keys), `day_type`, `next_action` (floors protein_g and fiber_g only, as v6), `day_score` (the five v6 checks; a null target is skipped), `streaks`, `week`, `weight`, `body_fat`.
- The ONLY value changes that build 7 sees. (a) to (c) are inside rules that v6 already has; (d) is an exception that Joe decided (D12): it overrides the unchanged-meaning rule for `week.strength_sessions` only, and only while `strength` is set in the targets file. (a) the `kcal` target is the v7 energy target of 18.5 (a number, as before); (b) with a schema 2 targets file the `net_carbs_g` target is null on every day (v6 already sends null on training days; the check is skipped and `of` shrinks, section 14 [C15]); (c) `fluids_ml` has target null (the v6 behaviour for an absent `water_ml` key); (d) with `strength` set in the targets file, `week.strength_sessions` counts the session dates of 18.6 (a date with at least `session_min_sets` sets in the strength variables, or with a Strava strength activity) and no longer every date with a Push ups or Pull ups value.
- `missing` is a list of free strings in build 7 (T1: `[String]`). It gets a v7 name only in a v7 state: "day_class_unset" only with a schema 2 file, a record type name only with its config key set (18.4). With a schema 1 file and the v6 config the list is the v6 list.
- New data is in NEW keys only: `wire`, `day_class`, `budgets`, `budget_score`, `budget_next_action`, `levers`, `coffee`, `energy`, `context`, `progress`, `records`, `strength`, `info` on the snapshot (18.7); `levers`, `entry_id` on ItemState; `item_id`, `entry_id`, `check`, `portion_basis`, `levers` on /fuel/day items. Assumption, not verified on this host (the Swift source is on the Mac mini): the build 7 decoders ignore unknown JSON keys. Acceptance test T1 proves or refutes it before a production deploy; if it fails, the new snapshot keys move behind a request header and this section is revised.
- A v7 server with a schema 1 targets file (the current file) behaves as v6 for all targets. The new keys are present, with `energy.state` "legacy" and `budgets` built from the schema 1 values.

### 18.2 Targets file schema 2 and the migration

Schema 2 KEEPS every schema 1 key with its schema 1 shape. A fueld binary older than v7 rejects unknown keys (503 `targets_invalid`), so the order in "Migration" is binding.

```
{ "schema": 2,
  // schema 1 keys, unchanged meaning
  "protein_g":   {"kind":"floor","value":160},
  "sat_fat_g":   {"kind":"cap","value":20},
  "fiber_g":     {"kind":"floor","value":35},
  "kcal":        {"kind":"pace","rest":2600,"training":3000},   // the PROVISIONAL energy target (18.5)
  "net_carbs_g": {"kind":"pace","rest":null,"training":null},   // retired as a target; consumed is still reported
  "caffeine_mg": {"kind":"cap","value":400},
  "alcohol_g_week": {"kind":"cap","value":30},
  "strength_per_week": 3,
  "weight_band_kg": [79,81],
  "eating_window": {"start":"07:00","end":"20:30"},
  "tz": "Europe/Zurich",
  // "water_ml" is ABSENT: water is context, not a target (18.7)

  // schema 2 keys; every null below is an open decision of 18.12 or a clinician value
  "reference_mass_kg": null,                                             // D5
  "carbs_g": { "kind":"floor", "unit":"total",
               "g_per_kg": {"low":null,"moderate":null,"long":null},     // D1, D2
               "class_minutes": null },                                   // D3: null or {"moderate":int,"long":int}
  "energy": { "maintenance": null,       // D4: null or {"kcal","mass_kg","avg_run_km_per_day",
                                         //   "window":["YYYY-MM-DD","YYYY-MM-DD"],"adopted_on":"YYYY-MM-DD",
                                         //   "result_id": string|null}
              "deficit_kcal": null,                  // D6
              "run_cost_kcal_per_kg_km": null,       // D7
              "energy_density_kcal_per_kg": null,    // D7
              "calibration": null },     // D8: null or {"start_date","min_days","max_days","min_complete_kcal",
                                         //   "min_weigh_days","edge_days","min_edge_weigh_days",
                                         //   "max_uncertainty_kcal","max_age_days","drift_kcal",
                                         //   "drift_uncertainty_factor","drift_days",
                                         //   "break_dates":["YYYY-MM-DD"]}; every key required when not null
  "levers": { "reference": {"psyllium_g":null,"beta_glucan_g":null,"nuts_g":null,
                            "pulses_g":null,"plant_protein_g":null},      // D9
              "estimator": null,         // D9: null or {"beta_glucan_g_per_100":{"rolled_oats":n,"oat_bran":n,
                                         //   "barley":n,"oat_drink":n},"pulses_dry_to_cooked":n}
              "avg_min_covered_days": null,            // D9: null or 1 to 7; null = no 7-day lever mean
              "coffee_default_brew_method": null },   // D10: "filtered"|"unfiltered"|"espresso"|"instant"|null
  "strength": null,                      // D12: null or {"set_variables":[string],"session_min_sets":int,
                                         //   "strava_sport_types":[string]}; every key required when not null
  "clinician": { "bp": {"home":null,"office":null,"ambulatory":null},    // each null or Instruction
                 "symptom_review_rules": [],          // [{"category", ...Instruction}], at most one per category
                 "hard_sets": null,                   // null or Instruction (effort and volume limits, as text)
                 "strength_test_protocol": null },    // null or Instruction
  "info_url": "https://site.gojoe.run/brkGEwlVPGzT6U0s" }
Instruction = {"text": string, "set_by": string, "set_on": "YYYY-MM-DD"}
```

(The `//` comments are for this document. The file is plain JSON.)

Validation (schema 2). These bounds are input validation, not advice.
- `schema` must be the integer 2. A file without `schema` is schema 1 and is parsed by the v6 rules. Any other value is invalid. Schema 1 required keys stay required. `water_ml` is accepted and ignored with one log line. Unknown keys at any level are rejected. Every number is finite.
- `reference_mass_kg`: null or 40 to 150.
- `carbs_g.g_per_kg`: each null or a number; set values obey 0 < low <= moderate <= long <= 12 (compared pairwise among the set ones). `class_minutes`: null, or both integers with 0 < moderate < long <= 600. A set `moderate` or `long` g_per_kg value with `class_minutes` null is invalid.
- `energy.deficit_kcal`: null or 0 to 500. `run_cost_kcal_per_kg_km`: null or 0.5 to 1.5. `energy_density_kcal_per_kg`: null or 5000 to 9500.
- `energy.maintenance`: null, or all six keys: kcal 1500 to 6000; mass_kg 40 to 150; avg_run_km_per_day 0 to 60; window = two valid dates, from <= to, length 14 to 28 days; adopted_on a valid date not before window[1]; result_id null (a value set by hand) or the id of a calibration result (18.5 step 10: when it is not null, the server checks at load that `calibration.jsonl` holds a candidate result with that id and with the same kcal, mass_kg, avg_run_km_per_day and interval, compared as rounded for the wire; no match = invalid file. The server also needs an `accepted` line for that id in `calibration.jsonl` (18.5 step 10); no accepted line = invalid file. The load check never looks at the current date, at newer results or at the current settings_version: freshness is checked once, when the acceptance is recorded, and an accepted maintenance stays valid when a later run gives another candidate). Cross-field rule (only when maintenance, deficit_kcal and run_cost_kcal_per_kg_km are all set): kcal - k x mass_kg x avg_run_km_per_day - deficit_kcal >= 0.5 x kcal. Else the file is invalid. This is the lowest value the formula can give (a day without a run), so a valid file never produces a negative or absurd target and nothing is clamped at run time.
- `energy.calibration`: null, or every key: start_date a valid date; integers 14 <= min_days <= max_days <= 28; min_complete_kcal 500 to 3000; 3 <= min_weigh_days <= min_days; 1 <= edge_days <= 7; 0 <= min_edge_weigh_days <= edge_days; max_uncertainty_kcal 20 to 1000; max_age_days 0 to 14; drift_kcal 20 to 1000; drift_uncertainty_factor 0 to 5; drift_days 2 to 30; break_dates valid dates, at most 100.
- `levers.reference` values: null or above 0 and at most 500. `levers.avg_min_covered_days`: null or an integer 1 to 7. `levers.estimator`: null, or every key, each factor above 0 and at most 20 (beta-glucan) or 1 to 4 (dry to cooked).
- `strength`: null, or all three keys: `set_variables` = 0 to 20 different names of numeric Variables variables (each non-empty, at most 80 characters); `session_min_sets` an integer 1 to 50; `strava_sport_types` = 0 to 10 different Strava sport types (each non-empty, at most 40 characters).
- An Instruction needs a non-empty `text` (at most 600 characters, one paragraph, printable), a non-empty `set_by` (at most 80) and a valid `set_on`. A symptom rule's `category` is one of 18.4.
- An invalid file answers 503 `targets_invalid` as today.

Migration (a manual step by joe_pa on Joe's word; fueld never writes this file):
1. Deploy the v7 fueld binary. It reads the schema 1 file and behaves as in 18.1.
2. Update ~/clawd/scripts/food-log to the rules of 18.9 and run the cross-writer test T18. This is a PREREQUISITE of step 5, not a later task.
3. Create the two new Variables variables (18.4). Back up ~/.config/fueld/config.toml, add `bp_var` and `symptom_var`, and restart the v7 fueld (names are resolved at startup). A v6 binary rejects these config keys.
4. Back up the targets file (`fuel-targets.json.v1-<date>`) and the staples file.
5. Write the schema 2 file: the schema 1 values copied 1:1, `net_carbs_g` set to null/null, `water_ml` removed, the schema 2 keys with the values Joe decided in 18.12 and `null` for every decision that is still open. fueld reloads the file by its modification time (the existing reload). Read /fuel/snapshot and compare with T3.
6. Only after step 5: staple entries may get the new optional keys of 18.3 and 18.6.

Rollback.
- Targets: restore the backup file. A v7 binary reads it (state "legacy").
- Binary to v6: FIRST restore the three backups: the schema 1 targets file, the config file without the two record keys (a v6 binary refuses to start on them), and the staples file as it was before step 6. Restore the staples backup, do not only strip the new keys: a staple that was converted to `carbs_basis` "available" has other numbers than its "total" form.
- Food writes by a v6 binary during a rollback carry no lever keys. The reducer rule of 18.6 makes that safe: an item that a v6 binary corrects becomes untagged for its levers (it is counted in `untagged_rows`, never with a wrong amount), and a v6 relog or move writes an untagged row.
- State directory: v7 writes record operations ONLY to its own files (`records.jsonl`, `calibration.json`, `calibration.jsonl`, 18.4 and 18.5). `journal.jsonl`, `idem.jsonl` and `feed.jsonl` get no new operation type and no new line shape; a food row payload only gains optional keys inside its `data` map. A v6 binary therefore replays a v7 state directory, never sees a record operation, and posts nothing to a record variable. Record operations that were pending or uncertain at the rollback stay in `records.jsonl` and are finished by the next v7 start (re-read by op_id first, as always). Records already written stay in Variables. T19 tests the whole procedure with pending, uncertain and done record operations and with v6 food writes.

What changes for Joe at step 5, stated plainly: the rest-day net carbohydrate cap of 120 g stops being a target. Until D1 is decided there is NO carbohydrate target. Water has no target. Energy stays 2600 / 3000 (provisional) until D4, D6 and D7 are decided.

### 18.3 Carbohydrate: one unit, by day class

Decision in this spec (Joe can veto it, D11): the unit is TOTAL carbohydrate in grams (`carbs_g`), fibre included. Reasons: (1) the driver tree row "Carbohydrate by day type" is in g/kg total carbohydrate; (2) the sports-nutrition bands (3 to 5, 5 to 7, 6 to 10 g/kg) and the low-carbohydrate trial threshold (under 130 g per day) are both stated in total carbohydrate in the report, so a total target can be compared with both; (3) fibre has its own floor, so a net figure would count fibre in two targets with opposite signs. `net_carbs_g` stays a stored row field and a reported sum. It is no longer a target.

- Label rule (estimator and staples). `carbs_g` on every row means total carbohydrate with fibre. EU and Swiss labels state carbohydrate WITHOUT fibre. The prompt says: from such a label, carbs_g = label carbohydrate + label fibre, and net_carbs_g = label carbohydrate. Staples gain the optional key `carbs_basis`: "total" (default, the v6 arithmetic: net = carbs - fibre) | "available" (per_100g.carbs_g is the label value without fibre: row carbs_g = (carbs + fibre) scaled, row net_carbs_g = carbs scaled; a null fibre counts as 0 for this sum). Rows written before v7 are not rewritten; their carbs_g counts as stored.
- Day class (new field `day_class`): "low" | "moderate" | "long" | "unknown". minutes = the sum of moving_time of the Strava Run, Ride and Swim activities that started on the local day (the same sport types as the section 9 day type: Run, TrailRun, VirtualRun, Ride, VirtualRide, GravelRide, MountainBikeRide, EBikeRide, Swim). The date of a Strava activity is, here and in 18.5, 18.6 and section 19, the date part of its `start_date_local` (the v6 convention: the wall clock at the place of the activity). It is not converted to targets.tz; an activity during travel counts on the date it had where it took place. Rules in this order:
  1. `class_minutes` null: day_class "unknown", `missing` has "day_class_unset", the `low` value applies. A schema 1 file has no `carbs_g` key: day_class is "unknown" and `missing` is not changed.
  2. Strava stale: "unknown", `missing` has "strava_stale", the `low` value applies. "Strava stale" is the section 9 rule, exactly, here and in 18.5 and section 19: it holds only for the CURRENT local date (never for a past date), when the newest file in strava_dir is older than 36 h (or there is none) and the files hold no training activity of that date (day_type is not "training"). In every other case the files are used as they are.
  3. minutes < class_minutes.moderate: "low". minutes < class_minutes.long: "moderate". Else "long".
- Target of the day = g_per_kg[applied class] x reference_mass_kg, rounded to the nearest 5 g. Kind floor, paced by the eating window like the other floors. If that g_per_kg value or `reference_mass_kg` is null, the target is null and the budget shows "not set". There is no cap on carbohydrate; the energy target bounds it.
- The class is known only when the activity is in the Strava files. Before that the day is "low" and the floor rises when the activity arrives. `budgets[].basis` says so ("low day so far"). A planned-session input is not in v7.
- Duration does not measure intensity. The report's bands are by intensity and duration; minutes alone are a proxy. This limit is stated on the info page, not solved here.
- The lipid response to any carbohydrate pattern is unknown (report). The app shows no lipid statement next to this budget.

### 18.4 New records and where they live in Variables

State of the ext API on 2026-10-02 (GET /variables?includeJson=true, 9 variables): json "Food log", json "Body composition"; numeric "Push ups", "Pull ups", "Squats", "Sit ups", "Burpees", "Squat -> Curl -> Push", "Cold shower". No variable for blood pressure or symptoms exists. "Body composition" holds 1493 values with method withings_scale, withings_bia and withings_manual and no waist row; its description already names "tape waist".

| Record type | Variable (config key) | New | Owner | Row `data` (besides the common keys) |
|---|---|---|---|---|
| `blood_pressure` | json "Blood pressure" (`bp_var`) | yes | P sets, H measures | systolic_mmhg, diastolic_mmhg (integers), pulse_bpm (integer or null), context "home" \| "office" \| "ambulatory", note |
| `symptom` | json "Symptom log" (`symptom_var`) | yes | H records, P defines rules | category, present (bool), note |
| `waist` | json "Body composition" (`body_var`) | no | H | method "tape", waist_cm, note |

Rows.
- Common keys of every record row: `source:"fuel"`, `op_id`, `record_id` ("rc_" + 20 random hex), `type`, `measured_at` (RFC3339, wall clock in targets.tz; default the request time). recordDate = the local date of measured_at.
- json values are append-only, so a record is never edited. A void is a new row in the same variable: `{type, voids: <record_id>, source, op_id: "op_void_<record_id>", record_id: <new id>, measured_at: <the ORIGINAL's measured_at>, voided_at}` with the ORIGINAL's recordDate, so a read of that date returns both rows. An edit is a void plus a new record. A record is voided when a done void row names it; resolution is by record_id over the whole cached window, not by date. Rows with the same op_id count once (section 14 [C1]).
- A waist row has NO weight_kg key, so the weight and body-fat readers (which need weight_kg > 0) skip it. T9 checks the other reader of this variable (~/clawd/workflows/workflows/withings_sync.py).
- Symptom categories (the report's list): chest_pain, back_or_neck_pain, breathlessness, palpitations, dizziness_or_syncope, performance_decline, low_libido, mood, bone_stress_injury, injury_other, other, none. Category "none" requires present false and is the weekly "nothing to report" entry; every other category requires present true.
- There is no strength record type (D12, changed on 2026-10-02): sets come from the numeric strength variables (18.6), read only. The app does not know the effort of a set, so it never labels a set as hard and stores no hard-set count. A strength test result has no record type in v7; only the protocol text is shown (18.7).
- The numeric strength variables stay as they are. v7 reads them and derives sets and sessions (18.6); fueld never writes them.
- Input bounds (validation only, 400 `bad_input` outside): systolic 60 to 260, diastolic 30 to 160, systolic > diastolic, pulse 25 to 220, waist_cm 40 to 150, note at most 500 characters, measured_at not in the future and not older than 34 days.

Config and variables. New optional config keys `bp_var` and `symptom_var` (no defaults; production sets the names of the table), resolved by name at startup like `food_log_var` (unique, type json). An absent key or a missing variable does not stop fueld: that record type answers 503 `variable_missing` and its snapshot part is null (18.7). When the key is SET and the variable does not resolve, the record type name ("blood_pressure", "symptom") is also in `missing`. With the key absent, `missing` gets nothing, so a v7 server on the v6 config sends the v6 `missing` list (T2). joe_pa creates the variables; fueld never creates one. In test_mode (section 2) the server refuses to start when `bp_var` or `symptom_var` is one of the production names of the table, and a `waist` record is refused with 409 `test_mode_read_only` while `body_var` is "Body composition" (the e2e instance reads it and never writes it; the waist path is tested live with `body_var = "Fuel e2e body"`).

Operations (their own store, so the food journal and a v6 binary never see them).
- `state_dir/records.jsonl`, fsynced, replayed at startup. One operation per row write: `{op_id, record_id, type, var: "bp"|"symptom"|"body", kind: "record"|"void", voids?, record_date, payload, client_id, request_hash, state, value_id?, attempts, at}`. States and rules are those of sections 6 and 14 [C1] [C2] for a single row: `pending -> done`, or `pending -> uncertain -> done | retry` (re-read the record date and look for the op_id before any re-post, the same op_id, at most 3 attempts 60 s apart); a rejection (4xx) of the first attempt, or an operation not done after 24 h, is `failed`. There is no compensation (one row per operation). The same reconcile loop runs them, posting to the variable that `var` names.
- Idempotency: client_id + request hash are stored in records.jsonl before the POST (same client_id and hash = the stored answer or the current state; a different hash = 409 `idempotency_conflict`), retention 7 days.
- Rows are the truth. Whatever an operation's state says, a record row that a refresh finds in the variable (matched by op_id) is a record, and a void row found there voids its record. An operation in state `failed` whose row appears later (a timed-out POST that did arrive) becomes `done` at that refresh, with the value id of the row. The same holds after a rollback and the next v7 start.
- Per-record lock for void. A void of a record whose own operation is not done: 409 `pending`. A second void (also concurrent, it waits for the first): 409 `already_voided`. A record is hidden from every read from the moment its void operation is journaled; if that operation ends `failed`, the record is visible again (and hidden again if its void row is found later). A new void is allowed after a failed one; it has the same deterministic op_id, so two void rows count once.
- Snapshot `revision` is not moved by record operations. `records`, `progress` and `strength` are computed fresh on each snapshot.
- Cache: the record variables are read with the full `includeJson` read that Body composition already uses (startup and every 20 min), window 180 days; own writes are merged on 201 by value id as in section 5; a refresh replaces the cached rows by the rules of section 14 [C8].

Routes.
- `POST /fuel/record {"client_id","type","measured_at"?,"data":{...}}`: 200 `{"status":"done","record":Record,"review"?:Instruction|null,"snapshot":Snapshot}`; 202 with `"status":"pending_reconciliation"` (the app polls GET /fuel/record/{record_id} every 2 s for up to 60 s); 502 `upstream_failed` when the first attempt is rejected (nothing stored). `review` is present only for type symptom: the `clinician.symptom_review_rules` entry of the saved category, else null.
- `POST /fuel/record/void {"client_id","record_id"}`: 200 `{"status":"done","record_id","snapshot"}`, 202 as above, 404 unknown, 409 `already_voided` | `pending`, 502.
- `GET /fuel/record/{record_id}`: `{"status":"done"|"pending_reconciliation"|"failed","void":null|{"status":"done"|"pending_reconciliation"|"failed"},"record":Record}`; 404 unknown. `status` is the state of the record's own write; `void` is the state of its NEWEST void operation (null when none). The app polls this route after a 202 of either POST and reads the matching part.
- `GET /fuel/records?type=<type>&from=YYYY-MM-DD&to=YYYY-MM-DD` (default the last 90 days, at most 180; filtered by the date of measured_at): `{"type","records":[Record],"clinician":<see below>,"info":string}`, newest first, voided and failed records absent, pending ones present with `pending:true`.
  `clinician` by type: blood_pressure = `{"home":Instruction|null,"office":Instruction|null,"ambulatory":Instruction|null}`; symptom = `{"rules":[{"category", ...Instruction}]}`; waist = null.
- Record = `{"record_id","type","measured_at","data":{the type's keys},"value_id":string|null,"source":string,"pending":bool}`. Rows of other writers in these variables (none exist today) are shown when they carry `type` and `measured_at` and pass the bounds; else they are ignored and counted in a log line.
- No model call on these routes.

No interpretation, ever: for blood_pressure and symptom there is no status, colour, trend arrow, average, word such as "normal" or "high", coach line or feed line. The symptom form shows two fixed texts at all times, independent of the entry: "Sudden severe chest or back pain is an emergency. Call the emergency number." and "Fuel does not interpret symptoms." With `review` null the app shows "No review rule from your clinician yet."

Chat guard (POST /fuel/log; the chat writes no records in v7).
- The model output schema adds the REQUIRED boolean `clinical_topic`: true when the user's message, also as a follow-up to the conversation of today, reports or asks about a symptom, an injury, blood pressure, a medical or lab result, a medicine, or a training limit. It is validated like every schema field (missing = invalid output, one retry, then 502 `model_invalid`).
- When `clinical_topic` is true, code does this, whatever else the model returned: the model's `text` is dropped and never stored or shown; food items, corrections, removals and moves in the same answer are processed normally (a meal named in the same message is logged); the reply's text blocks are the usual code-generated lines for those writes (none when nothing was written) followed by the fixed line "Use the Records tab for that. Fuel does not interpret it."; no coach event carries the clinical words; the feed stores the user turn as typed and the fixed reply.
- With zero items, corrections, targets and moves the answer is intent "question", nothing is written, and the fixed line is the only text block.
- The same holds for a transcript (audio) and for a photo caption.

### 18.5 Energy target and maintenance calibration

Formula (the report's construction, recomposition section):

```
target(d) = M + k x m x (run_km(d) - avg_run_km_per_day) - deficit        rounded to the nearest 10 kcal
```

M = `energy.maintenance.kcal`, m = `energy.maintenance.mass_kg`, avg_run_km_per_day = `energy.maintenance.avg_run_km_per_day` (all three frozen from the SAME calibration interval, so M already holds the average run cost and only the difference is added: exercise is counted once), k = `run_cost_kcal_per_kg_km`, deficit = `deficit_kcal`, run_km(d) = the distance of the Strava Run, TrailRun and VirtualRun activities that started on local day d. On a day without a run the adjustment is negative. Over a week with the calibration's distance the adjustments sum to zero.

States (`energy.state`):
- "legacy": schema 1 file. Target = kcal rest / training by day_type (v6).
- "provisional": schema 2 and any of maintenance, deficit_kcal, run_cost_kcal_per_kg_km is null. Target = the `kcal` rest / training values by day_type, exactly as v6. The budget carries `provisional:true` and `basis` "Provisional. Maintenance is not calibrated yet."
- "formula": all three set. Target = the formula. `basis` names the parts: "3050 maintenance + 420 run (5.3 km more than average) - 0 deficit".
- With Strava stale (the 18.3 definition: the current date only) in state "formula": run_km(d) counts as the average (adjustment 0), `missing` has "strava_stale", `basis` says "run distance unknown".
- The target moves during the day when a run arrives (as in 18.3). Rides and swims do not enter the formula; their average cost is inside M. Climbing is not in the factor k (the report's rule of thumb is per km and is not sourced). Both limits are stated on the info page.
- The kcal check of the day (within 10 % of the target) and the pace maths are unchanged.

Calibration. It estimates M from Joe's own data. Stored in `state_dir/calibration.json` (the result per local date, the last 60 dates) and appended to `calibration.jsonl`; nothing goes to Variables.

1. When it runs. Once per local date D at 04:00 targets.tz by the background loop, at startup when D has no result, again when the targets file reloads or a Food log or Body composition write for a day inside the interval is seen, and on `POST /fuel/calibration/run` (no body; answers the fresh result of step 9; 502 when the refresh fails). A run for D REPLACES the stored result of D in `calibration.json` (one result per date); every run is appended to `calibration.jsonl`. A date on which the process did not run has no result. Each result carries `settings_version` = the first 12 hex characters of SHA-256 over the canonical JSON of `energy.calibration`, `energy_density_kcal_per_kg` and `run_cost_kcal_per_kg_km`, and `result_id` = the first 16 hex characters of SHA-256 over settings_version, the interval, and the I, W and R values of its days.
2. Preconditions, else state "off" with `blocked_by`: `energy.calibration` is null ("calibration settings not set"); `energy_density_kcal_per_kg` is null ("energy density not set"); `run_cost_kcal_per_kg_km` is null ("run cost factor not set"). A start_date after yesterday gives state "collecting" with days 0.
3. Refresh first. The run re-reads from Variables every Food log day from max(start_date, D - max_days - max_age_days) to D - 1 (at most 42 `?date=` reads, made for the calibration and independent of the 35-day cache window of section 5), re-reads Body composition (the full read) and re-scans strava_dir. If any read fails, the result is state "blocked", `blocked_by` "could not refresh the inputs", candidate null. `inputs_as_of` is the time of the oldest of these reads.
4. Per day d: I(d) = the kcal total of the active Food log contributions of all writers (sections 7 and 14). complete(d) = I(d) >= min_complete_kcal AND the kcal `unknown_rows` of the day is 0 AND d is not in `break_dates` AND d >= start_date. W(d) = the mean of Body composition weight_kg of the local day (any method), or none. R(d) = run km of the day.
5. Interval. The calibration uses ONE contiguous interval of days that are ALL complete, so the intake mean, the weight slope and the run distance describe the same days and no day's energy is missing. Take the most recent run of consecutive complete days that ends on or before D - 1; its last max_days days are the interval; N = its length. A day that is not complete (a missed log, a `break_dates` day such as illness, travel or a race) ends an interval; the next one starts after it. `break_dates` therefore removes the day's intake, weight and distance together.
6. Gates, all needed for a candidate:
   - G1: N >= min_days. Else state "collecting".
   - G2: the interval ends not more than max_age_days before D - 1.
   - G3: days with W in the interval >= min_weigh_days, with at least min_edge_weigh_days of them in the first edge_days days and at least min_edge_weigh_days in the last edge_days days.
   - G4: the newest file in strava_dir is not older than 36 h at the run (the section 9 rule). A gap inside the interval cannot be detected by code; `run_km_per_week` is shown so that Joe can see one.
   - G5: uncertainty <= max_uncertainty_kcal (step 8, unrounded). False when n < 3 (no standard error exists).
   - G6: the candidate AS ROUNDED FOR THE WIRE (the exact object that adoption copies: kcal to 10, mass to 0.1, avg km to 0.1) would be a valid `energy.maintenance` object by 18.2: kcal 1500 to 6000, mass 40 to 150, avg km 0 to 60, and the cross-field rule with the file's k and deficit (a null deficit counts as 0). This is the one gate that uses rounded values.
   Gates are evaluated in this order; a gate that cannot be evaluated is false. A failed G2 to G6 gives state "blocked" with the gate in plain words in `blocked_by` ("Only 6 weigh-ins in the 14 days; 10 are needed.").
7. Maths, over the interval. Ibar = the mean of I(d) over its N days. Over the n days with W: x = the day index (0 for the first day of the interval), s = the least-squares slope of W on x in kg per day = Sxy / Sxx. mass = the mean of W. avg_km = the sum of R(d) / N.
   `M_cal = Ibar - rho x s`, rho = `energy_density_kcal_per_kg`. A falling weight (s < 0) raises M_cal above the intake.
8. uncertainty = rho x SE(s), with SE(s) = sqrt( (SSR / (n - 2)) / Sxx ), SSR = the sum of squared residuals of the fit. It covers the weight noise only. It does NOT cover logging error: every kcal not logged lowers M_cal by one kcal. The app says both. Gates G1 to G5 and drift compare unrounded values; the wire carries kcal rounded to the nearest 10, mass to 0.1 kg, avg km to 0.1.
9. Result on the wire (`energy.calibration`): `{"state":"off"|"collecting"|"blocked"|"candidate", "for_date":D, "inputs_as_of":RFC3339|null, "interval":["YYYY-MM-DD","YYYY-MM-DD"]|null, "days":int, "days_needed":int|null, "weigh_days":int, "run_km_per_week":number|null, "gates":{"G1":bool,"G2":bool,"G3":bool,"G4":bool,"G5":bool,"G6":bool}|null, "blocked_by":[string], "settings_version":string|null, "food_record_info":string, "candidate":{"result_id","kcal","uncertainty_kcal","mass_kg","avg_run_km_per_day","mean_intake_kcal","weight_slope_kg_per_week","interval":[from,to]}|null}`. State off: interval, days_needed, run_km_per_week, gates, settings_version and candidate are null, days and weigh_days 0. The shape and these rules are the same before and after adoption. Collecting: interval is the current run of complete days (null when there is none), gates null, candidate null. Blocked: candidate null. Candidate: every gate true, blocked_by empty.
10. Adoption is a decision (D4). fueld never writes the targets file. Joe says "adopt"; joe_pa calls POST /fuel/calibration/accept `{"op_id":...}`. The server refreshes every input, runs the calibration again under the current settings_version and, when the state of THAT run is "candidate", appends `{"accept_op":op_id, "outcome":"accepted", "accepted":result_id, "at":RFC3339, "settings_version":..., "response":{...}}` to `calibration.jsonl` and answers with the candidate; any other state answers 409 `not_a_candidate` with the result and appends `{"accept_op":op_id, "outcome":"rejected", "at":RFC3339, "response":{...}}` (a rejected line accepts nothing and does not count for the load check). The line holds the full response body and is written and synced BEFORE the response is sent. Accept requests are serialized by one lock; an op_id that already has a line returns the stored status and body without a new run, also after a restart (the lines are read from `calibration.jsonl` at startup, not from `idem.jsonl`), so a retry can never accept another candidate than the first answer named. A crash before the line is written leaves no trace and the retry runs as new. joe_pa then copies exactly the candidate of that answer (kcal, mass_kg, avg_run_km_per_day, interval as `window`, result_id) into `energy.maintenance` with `adopted_on`. The load check of 18.2 refuses a copy whose result_id has no accepted line or whose numbers do not match the stored result, so a stale or mistyped adoption cannot become active. An acceptance is never withdrawn: when an input of the interval is corrected later, the accepted maintenance stays valid and active, and the change shows through `energy.drift` (step 11) only. A maintenance value set by hand has result_id null and the snapshot says `maintenance_source` "manual". The formula is active from then on (if D6 and D7 are set).
11. After adoption the calibration keeps running (the method holds in a deficit too). `energy.drift` is true when each of the last drift_days local dates up to D has a result of state "candidate", with the CURRENT settings_version, whose unrounded M_cal differs from the adopted kcal by more than max(drift_kcal, drift_uncertainty_factor x uncertainty), all in the same direction. A date without a result, with another state or with another settings_version breaks the sequence. Without an adopted maintenance, drift is false. The app then shows "Maintenance estimate moved to N. Review." Nothing changes by itself.
12. "Normal training" (the report's condition) is not detectable by code. `run_km_per_week` of the interval is shown so that Joe can judge, and `break_dates` ends an interval at an unusual day.

What the app shows before calibration is done: the provisional kcal budget with the "Provisional" mark, and a calibration card built from the result: "Calibrating maintenance: 6 complete days in a row, 14 needed. Weigh-ins 4." plus the `blocked_by` lines. While no maintenance is adopted, the only maintenance number on the wire or on screen is the one inside a result of state "candidate"; no energy-availability number exists in v7. After adoption `energy.maintenance_kcal` is the adopted value. At "candidate": "Estimate 3050 kcal (plus or minus 140, from the weight trend only). Under-logging makes it too low. Adopt?" The 2-week food record of the report (anchor food-record) is the same period: calibration needs the full record anyway.

Not in v7: an energy availability estimate (its three inputs are unverified, report), a hard daily kcal floor, any use of the scale's fat-free mass.

### 18.6 Levers, strength and clinician instructions

Lever amounts of one item (all optional, number or null, of the item AS LOGGED):
- `psyllium_g`: grams of psyllium husk product in the item.
- `beta_glucan_g`: grams of oat or barley beta-glucan. A label value or a stated amount wins. Else the estimator uses `levers.estimator.beta_glucan_g_per_100` (grams per 100 g dry rolled oats, oat bran and barley; per 100 ml oat drink) when it is set. With `levers.estimator` null the prompt carries no factor and the model gives null unless a label or the user states the amount.
- `nuts_g`: grams of tree nuts (almond, walnut, hazelnut, cashew, pistachio, pecan, macadamia, Brazil nut), whole, chopped or as 100 % nut butter. Peanuts and seeds are not nuts here (the trial evidence is for tree nuts).
- `pulses_g`: COOKED-equivalent grams of beans, lentils, chickpeas and dried peas. A dry weight is multiplied by `levers.estimator.pulses_dry_to_cooked` when set; with it null a dry amount gives null. Soy foods are not pulses here; they count in plant_protein_g.
- `plant_protein_g`: grams of the item's protein that come from plants (pulses, soy, nuts, seeds, grains, plant protein powder).
- `brew_method` (string or null; only on coffee drinks): "filtered" (paper filter, drip, pour-over, AeroPress with paper) | "unfiltered" (French press, boiled, Turkish, moka pot) | "espresso" (espresso and espresso-based drinks; the report keeps espresso apart because its data are cross-sectional) | "instant" | "unknown". The user's words win; else `levers.coffee_default_brew_method`; else "unknown". The model never guesses a method from a photo of a cup. null on everything that is not coffee.

Model and checks.
- Each model item (and a `revised` object) adds `levers: {"psyllium_g","beta_glucan_g","nuts_g","pulses_g","plant_protein_g": number|null, "brew_method": string|null}`. null = not known; 0 = the item has none. The prompt says to give 0 for every item that plainly has none (water, chicken), so that a null is rare.
- Scale override (section 17 D): when the server replaces an estimated portion by the scale reading, it multiplies the model's known lever amounts by the same factor as the macros. Order for a new item: scale override, then staple replacement, then the code checks, then the row is written.
- Code checks, no extra model call. With a known portion: nuts_g and psyllium_g each at most portion_g x 1.05; pulses_g at most portion_g x 1.05 when `levers.estimator` is null, else at most portion_g x max(1.05, pulses_dry_to_cooked x 1.1) (a dry portion gives a larger cooked-equivalent amount). beta_glucan_g at most fiber_g + 0.5 when fibre is known. plant_protein_g at most protein_g + 0.5. brew_method not null only with kind "drink". A value that fails is set to null and logged with the item id.
- Staples: optional per_100g keys `psyllium_g`, `beta_glucan_g`, `nuts_g`, `pulses_g`, `plant_protein_g` and the optional staple key `brew_method`. A staple's lever values replace the model's; a staple without them keeps the model's.

Rows and the reducer.
- The five amounts are flat row keys next to the macros. A key is written only when its value is known (never as null).
- Reducer rule for a lever L of an item (its contribution group of sections 14 and 15, all writers, rows in write order): the item is TAGGED for L when (a) at least one counted row of the group carries the key L, and (b) every counted correction row that comes after the FIRST row with L also carries L. Its amount is then the sum of L over the rows, never below 0. Else the item is UNTAGGED for L. Reset by a revise: when the group has a counted revise row whose `recalibrated.to.levers` holds L, the NEWEST such row is the start: the amount is that `to.levers` value plus the sum of L over the counted rows after it, and (b) is checked only for the correction rows after it. So a revise sets an absolute value and restores a tag that an earlier correction without the key had removed; a later correction without the key removes it again. Condition (b) makes a correction by a writer without lever arithmetic (a v6 binary, an old food-log) turn the item untagged, never wrong. This differs from the macro rule of section 7 on purpose: an untagged original can become tagged later by a revise.
- Size corrections (fix, fraction, portion or volume delta, count delta, undo, compensation, a move's undo): the correction row carries the delta of each lever for which the item is tagged at that moment, by the same arithmetic as the macros (a known 0 when unchanged), and omits the key for a lever the item is untagged for.
- `revised` (section 17 B): for each lever the answer knows, the row carries new amount minus current amount (current = 0 when untagged), which makes the item tagged. For a lever the answer gives as null: a tagged lever is RETAINED (delta 0 in the row, the amount stays, `to.levers` repeats it); an untagged lever stays untagged (no key). A revise is the only operation that can tag an untagged item, and a revise row cannot be reverted (section 16 reverts only recalibrate rows), so a first tag is never undone by a revert; the user restates the item again instead. `recalibrated.from` and `.to` gain a `levers` object (the item's amounts before and after, known keys only) and `.to.brew_method` when the answer gives one. A revise that changes only levers or the brew method is a change and is written.
- Second opinion (section 16): it does not estimate levers. An applied recalibrate row scales each tagged lever by to.portion_g / from.portion_g when both are known (delta in the row, `levers` in from and to), else leaves it (delta 0).
- Base rule: the item's lever base is `recalibrated.to.levers` of its newest recalibrate or revise row that no revert row names, else the original row's keys. Later size corrections, relog, recent, day and move scale the base exactly as they scale the macros. A revert row (`recalibrate_revert`) carries the negated lever deltas of the row it reverts.
- `brew_method` of an item = the value on the newest counted row of the group that carries the key (the original or a revise row; a relog or a move starts a new item with the copied value). Recalibrate rows never carry it.
- Relog and move copy the item's current lever amounts and brew method to the new row (relog times scale).
- Untagged rows: rows before v7, and rows of other writers without the keys. No backfill: the stored history is not re-estimated.

Day state of a lever (18.7 `levers`): `consumed` = the sum over the day's active tagged items; `untagged_rows` = the count of the day's active items (every kind) that are untagged for it; the day is COVERED for the lever when it has at least one active item and `untagged_rows` is 0. `avg7` = the mean of `consumed` over the covered days among the 7 local days ending on the snapshot date, needing at least `levers.avg_min_covered_days` covered days, else null (always null while that setting is null); `covered_days_7` says how many. An uncovered day never enters the mean, so missing tags are never read as zero intake.

Levers are H values with OPTIONAL reference doses (the report: "optional sub-target", "trial dose"). A lever has no status, no streak, no place in any score and no next action. When `levers.reference.<key>` is set, the app shows "12 of 10 g" as plain numbers with the note "reference dose from trials, not a target". The effect sizes of the report are on the info page only.

Strength (D12, changed by Joe on 2026-10-02: "a value of one in variables is one push up ... three values posted could be considered a session. And then also have to look across different sports activity variables.").
- Sets. `strength.set_variables` names numeric Variables variables. Each value of such a variable is ONE SET, and its number is the reps of that set. A value that is not a number above 0 is not a set. The date of a set is the recordDate of its value. fueld reads these variables and never writes or creates one. Values: the per-date reads of section 5 already return the values of every variable of the date. The cache keeps, for each loaded date, every non-json value as (variable id, value id, stored text); the same window and refresh rules, no extra request. Sets and sessions are computed from these cached values at each snapshot, so a changed `set_variables` list applies to every loaded date at once and no date is read again. Names: fueld keeps the variable list (name, id, type) that it reads at startup; it reads the list again every 20 min and when the targets file reloads, and a failed read keeps the list it has. A name resolves when exactly one variable of the list has that exact name and its type is numeric. A name that does not resolve is listed in `strength.missing_variables` and logged once per reload; the other variables still count.
- Strava. A local date is a Strava strength date when a Strava activity whose sport type is in `strength.strava_sport_types` started on it (any duration). The list can be empty.
- Session. A local date is a strength SESSION when its sets, summed over all set variables, are at least `session_min_sets`, or when it is a Strava strength date. A date counts once, whatever the number of sets and activities.
- `strength.sessions` = the session dates of the ISO week of the snapshot date, Monday up to that date. `sessions_target` = `strength_per_week` (H). The legacy key `week.strength_sessions` has the same value.
- `strength.week` = the sets and reps of the same dates, in total and per variable (every resolved variable, in file order, also with 0). It is context: there is no target, status or score for sets or reps.
- `strength: null` in the file (and every schema 1 file): `rule` is "legacy", a session date is a date for which the v6 predicate holds, unchanged (a value of the variable "Push ups" or "Pull ups" whose stored text is not empty and not "0"), `session_min_sets` is null and `strength.week` is null. The rule "a number above 0" is for rule "sets" only.
- Hard sets stay clinician-owned and unset. The app does not know the effort of a set. It never labels a set as hard, has no hard-set count and no hard-set target. `strength.clinician` = `clinician.hard_sets` (Instruction or null). With null the app shows "No guidance from the specialist yet." It never shows the report's "10 or more sets" figure as a target.

Clinician instructions (`clinician.*`): the app shows `text` verbatim with "Set by <set_by> on <set_on>" next to the matching records. The server and the app parse no number out of it and compare no record with it.

### 18.7 Snapshot (additions) and /fuel/day (gap from build 7)

New top-level keys of `GET /fuel/snapshot` (and of every embedded snapshot). `info` strings are explained in 18.8.

```
"wire": 7,
"day_class": "low"|"moderate"|"long"|"unknown",
"budgets": [ { ...MacroState (section 9), "provisional":bool, "basis":string|null, "info":string } ],
   // keys, in this order: protein_g, sat_fat_g, fiber_g, kcal, carbs_g, caffeine_mg, alcohol_g_week
   // carbs_g: unit "g", kind "floor", consumed = total carbohydrate, target per 18.3 (null = "not set");
   // caffeine_mg and alcohol_g_week: the same values as in `intake`; provisional is true only for kcal in state "provisional"
"budget_score": {"hit":int,"of":int},
"budget_next_action": NextAction | null,
"levers": [ {"key":"psyllium_g"|"beta_glucan_g"|"nuts_g"|"pulses_g"|"plant_protein_g",
             "label":string,"unit":"g","consumed":number,"untagged_rows":int,
             "avg7":number|null,"covered_days_7":int,"reference":number|null,"info":string} ],   // always these five, in this order
"coffee": {"today":{"filtered":int,"unfiltered":int,"espresso":int,"instant":int,"unknown":int},
           "last7":{...the same keys}, "default_method":string|null, "info":string},
"energy": {"state":"legacy"|"provisional"|"formula","target":number,"maintenance_kcal":number|null,
           "run_km":number|null,"run_adjust_kcal":number|null,"deficit_kcal":number|null,
           "maintenance_source":"calibration"|"manual"|null,
           "drift":bool,"calibration":{...18.5 step 9},"info":string,"calibration_info":string},
"context": {"fluids_ml":{"consumed":number,"info":string},
            "alcohol_g":{"consumed":number},
            "body_fat":{"latest_pct":number|null,"latest_date":string|null,"note":"scale estimate, not a progress marker","info":string}},
"progress": {"weight":{"avg7_kg":number|null,"delta7_kg":number|null,"band_kg":[number,number],"info":string},
             "waist":{"latest_cm":number|null,"latest_date":string|null,"previous_cm":number|null,"previous_date":string|null,"info":string},
             "strength_test":{"protocol":Instruction|null,"info":string}},
"records": {"blood_pressure":{"latest":Record|null,"count_7d":int,
                              "clinician":{"home":Instruction|null,"office":Instruction|null,"ambulatory":Instruction|null},
                              "info":{"home":string,"office":string,"ambulatory":string}} | null,
            "symptom":{"latest_date":string|null,"entry_this_week":bool,"info":string} | null},
"strength": {"rule":"sets"|"legacy","sessions":int,"sessions_target":int,"session_min_sets":int|null,
             "week":{"sets":int,"reps":number,"by_variable":[{"name":string,"sets":int,"reps":number}]} | null,
             "missing_variables":[string],
             "clinician":Instruction|null,"info":string},
"info": {"base":string}
```

- `coffee` counts the day's (and the 7 local days') active drink items whose brew_method is not null; an untagged coffee of another writer is not counted.
- `energy`: in states legacy and provisional maintenance_kcal, run_adjust_kcal and deficit_kcal are null; run_km is the day's run distance or null when Strava is stale. `energy.target` equals the `kcal` target in `macros` and `budgets`.
- `progress.waist` shows the two newest waist records; its values are null when there is no waist record. `progress.strength_test` shows the protocol text only (`clinician.strength_test_protocol`, Instruction or null): a test result has no record type in v7. Neither carries a trend word or a status. `records.blood_pressure` / `records.symptom` are null when their variable is missing.
- `records.symptom.entry_this_week`: a symptom record (any category, also "none") exists in the ISO week of the snapshot date. The app may remind once a week; the reminder text is fixed ("Weekly symptom entry is open.") and says nothing about health.
- `budget_score` checks (H and A values only): protein >= floor, fibre >= floor, sat fat <= cap, kcal within 10 % of the day target (a provisional target counts), carbohydrate >= floor (skipped while its target is null). Water, body fat, levers, records and hard sets are never checks. `day_score` and `streaks` (legacy) are unchanged.
- `budget_next_action`: the section 9 rule over the floors protein_g, fiber_g and carbs_g (when set). `next_action` (legacy) keeps protein_g and fiber_g.
- Status lines and blocks keep the v6 widget names. The code-generated status line may name carbohydrate when its target is set (plain text, no new block type).

ItemState (POST /fuel/log, entry, feed, mutation answers) gains `entry_id` (string) and `levers` (the item's current amounts by 18.6: the five keys number|null, and `brew_method` string|null).

`GET /fuel/day` items gain (the build 7 gap: Today had no Revert, and lost `check` and the basis for items outside the loaded feed pages):
- `item_id`: the Fuel item id, or null for a row of another writer (row_key stays the stable key).
- `entry_id`: the id of the Fuel entry that holds the item (a relog or a move has its own entry), or null for a row of another writer.
- `check`: the plausibility reason (section 17 E); the key is absent when the item is fine (the ItemState convention).
- `portion_basis`: the stored basis of the item, the same value as ItemState.portion_basis ("stated", "photo_estimate", "label", "scale", "repeat", "unspecified"); null for a row that stores none. A size correction, a revise or a recalibration does not change it.
- `levers`: as on ItemState.
For a Fuel item, `item_id`, `entry_id`, `check`, `portion_basis` and `levers` equal the same fields of its ItemState at the same revision. The amounts of a day item (portion_g, volume_ml, macros, caffeine_mg, alcohol_g) stay the CURRENT amounts (v4.2), which correspond to ItemState.effective and the current portion, not to the ItemState base fields.

### 18.8 Info links

`info` = info_url + "#" + anchor, a full URL (an empty string when `info_url` is absent). The anchors exist on the published page (checked against the page source ~/clawd/state/site-pages/fuel-framework.html on 2026-10-02).

| App element (wire field) | Anchor |
|---|---|
| budgets protein_g | protein |
| budgets sat_fat_g | saturated-fat |
| budgets fiber_g | fibre |
| budgets kcal, energy.info | energy |
| energy.calibration_info | maintenance-energy |
| budgets carbs_g | carbohydrate |
| levers psyllium_g, beta_glucan_g | viscous-fibre |
| levers nuts_g | nuts |
| levers pulses_g, plant_protein_g | plant-protein |
| coffee.info | coffee-brewing |
| budgets caffeine_mg | caffeine |
| budgets alcohol_g_week | alcohol |
| context.fluids_ml | fluids |
| context.body_fat | not-tracked |
| progress.weight | body-weight |
| progress.waist, GET /fuel/records type waist | waist |
| progress.strength_test | strength-test |
| strength.info | strength |
| records.blood_pressure.info home / office / ambulatory | bp-home / bp-office / bp-ambulatory |
| GET /fuel/records type blood_pressure (`info`) | bp-home |
| records.symptom, records type symptom | symptoms |

The calibration card also links the food record explanation: `energy.calibration.food_record_info` = info_url + "#food-record". The page is password protected; the app opens the URL in the system browser and stores no password. The server sends anchors from this table only (a constant in code).

### 18.9 Telegram path (~/clawd/scripts/food-log), a prerequisite of the migration

The script is the second writer of Food log and a reader of the targets and staples files. It is changed in ~/clawd before step 5 of 18.2:
- Targets. With `schema` 2 it does not compute targets itself. Its one summary function, which `today`, `add`, `fix` and `undo` all print, takes `budgets` (and `energy.state`) from fueld `GET /fuel/snapshot?date=`, so both paths show the same kcal and carbohydrate targets after every command. When fueld does not answer, it prints the consumed sums with "targets unavailable" and no target. It prints no water target under schema 2. With a schema 1 file it works as today.
- Staples. Its normalizer honours `carbs_basis` (18.3) and copies the staple lever keys scaled to the portion.
- Levers. It writes the flat lever keys and `brew_method` when the estimating agent gives them. Its correction rows scale the lever keys like its EXTRAS, with the reducer rule of 18.6 (a key only for a lever the item is tagged for).
- It writes no records.

### 18.10 Out of scope in v7

Lab values (ApoB, lipids, Lp(a)), imaging values, the exercise test, sleep, run fuelling per hour, plant sterols, an energy availability figure, planned sessions, a backfill of lever tags, chat writes of records, any numeric judgement of a clinician value, a numeric hard-set target, a label or a count for hard sets, a record type for strength sets or for strength test results, a write to a strength variable.

### 18.11 Acceptance tests

Unit and integration tests use the fakes of section 2 and a fixed clock; live tests use the fueld-e2e instance and "Fuel e2e ..." variables only. The production variables and ~/.agentd/fuel-targets.json are untouched by testing (counts and file hash before = after). Fixture numbers below are test inputs, not decisions.

- T1 (build 7 compatibility, live): iOS build 7 (1.5.0) against a v7 fueld-e2e (a) with a schema 1 file, (b) with a schema 2 file with every decision null, (c) with a schema 2 file with an active carbohydrate floor and state "formula": log by text, log by photo, Today, undo, fix, relog and revert all work; no decode error in the app log; Today shows no net carbohydrate target and no water target in (b) and (c) and does not crash.
- T2 (golden v6): with a schema 1 file, every v6 key of /fuel/snapshot, /fuel/day, /fuel/log, /fuel/entry, /fuel/feed and /fuel/recent equals the v6 golden files for the same fixture and clock; only new keys differ. With the schema 2 file of T1 (c), `next_action.key` is never carbs_g, `macros` has exactly five keys, and no block has a widget name outside the v6 set.
- T3 (migration): the schema 2 file built from the current production values by 18.2 step 5 with every decision null parses; snapshot: protein 160 floor, sat fat 20 cap, fibre 35 floor, kcal 2600 / 3000 with `provisional:true`, carbs_g target null, net_carbs_g target null, fluids target null, caffeine 400, alcohol 30, energy.state "provisional", calibration.state "off" with blocked_by "calibration settings not set", every lever reference null and every lever avg7 null, day_class "unknown" with "day_class_unset".
- T4 (parser): an unknown key at each level = 503 targets_invalid (also inside `strength`); a `strength` object with a missing key, with `session_min_sets` 0, or with a name twice = invalid; `schema` 3 = invalid; no `schema` = v6 parse; each bound of 18.2 has one failing case, including: g_per_kg.moderate set with class_minutes null; a maintenance object with a missing key; the cross-field energy rule (kcal 2000, k 1.0, mass 80, avg 14 km, deficit 0 gives 880 < 1000 = invalid); a calibration object with a missing key; `water_ml` in a schema 2 file is ignored with a log line.
- T5 (carbohydrate): reference mass 80, g_per_kg 3 / 5 / 6, class minutes 45 / 90: 0 min = 240 g, 44 min = 240 g, 45 min = 400 g, 89 min = 400 g, 90 min = 480 g; stale Strava = 240 g, "unknown", "strava_stale"; class_minutes null with low 3 = 240 g on every day, "unknown", "day_class_unset"; low null = target null and the check skipped; reference_mass_kg null = target null; a staple with carbs_basis "available" (60 g carbohydrate, 10 g fibre per 100 g) gives carbs_g 70 and net_carbs_g 60 per 100 g, and with "total" gives 60 and 50.
- T6 (calibration maths): 14 complete days, mean intake 2900 kcal, weights on every day on a straight line from 80.00 kg (index 0) falling 0.03 kg per day, rho 7700: s = -0.03, M_cal = 2900 + 231 = 3131, on the wire 3130, uncertainty 0; a flat weight gives M_cal = the mean intake; a fixture with noisy weights matches rho x SE(s) computed by hand; mass and avg km match by hand.
- T7 (calibration states and gates, each with settings min_days 14, max_days 21, min_complete_kcal 1500, min_weigh_days 10, edge_days 5, min_edge_weigh_days 3, max_uncertainty_kcal 200, max_age_days 7, drift_kcal 150, drift_uncertainty_factor 2, drift_days 7): calibration null = off; rho null = off; start_date tomorrow = collecting, days 0; 13 complete days = collecting, days_needed 1; a 20-day stretch with one day under 1500 kcal on day 8 = interval of 12 days, collecting (the day breaks the interval, it is not skipped); the same with a kcal unknown row, and with a break date; 25 complete days = interval of the last 21; an interval that ended 9 days ago = blocked by G2; 9 weigh days = blocked by G3; 2 weigh days in the last 5 = blocked by G3; stale Strava = blocked by G4; noisy weights = blocked by G5; a failed Variables read = blocked "could not refresh the inputs"; 14 complete days at 800 kcal with flat weights and min_complete_kcal 500 = blocked by G6, no adoption prompt; a candidate of 2000 kcal with mass 80, 14 km per day and k 1.0 = blocked by G6 (the cross-field rule); unrounded maintenance 3000, mass 79.99, 18.75 km per day, k 1.0, deficit 0 (rest-day value 1500.19 unrounded, but 3000 - 80.0 x 18.8 = 1496 as rounded) = blocked by G6. With min_days 28, max_days 28, max_age_days 14 and D = 2026-10-02, a complete interval 2026-08-25 to 2026-09-21 is read in full (all 28 days refreshed) and passes G1 and G2. No candidate number is on the wire in any state but "candidate". An edit of a food row inside the interval by another writer changes the next run's result (the refresh sees it).
- T8 (energy formula): maintenance 3000, mass 80, avg 6 km per day, k 1.0, deficit 0: a 0 km day = 2520, a 6 km day = 3000, a 20 km day = 4120; a 7-day fixture with 42 km sums to 21000; deficit 300 lowers each by 300; any of the three inputs null = state provisional and the v6 rest / training value; stale Strava = 3000 and basis "run distance unknown". Drift (drift_kcal 150, drift_uncertainty_factor 2, drift_days 7): 7 dates of candidates all 200 above with uncertainty 50 = true; the same with uncertainty 120 = false; 6 dates = false; 7 with one blocked date in between = false; 7 with mixed signs = false; two runs on one date count once; a settings change on date 4 = false until 7 dates with the new version exist; no adopted maintenance = false. Adoption: a maintenance object with the result_id of a stored candidate and the same numbers loads; a changed kcal with that result_id = targets_invalid; an unknown result_id = targets_invalid; result_id null loads with maintenance_source "manual"; the id of a stored candidate without an accepted line = targets_invalid. Accept: POST /fuel/calibration/accept on a candidate state appends one accepted line and returns the candidate; the same op_id again returns the same answer and appends nothing, also after a restart, and also when a newer candidate B exists by then (the replay still names A); two concurrent requests with one op_id give one line and two equal answers; a rejected op_id replayed after a restart returns the stored 409 even when the state is now candidate; a rejected line alone does not make a result_id loadable; when an input changed since the last run so that the refreshed run is blocked or collecting = 409 not_a_candidate, one rejected line and no accepted line; when the refreshed run gives a new candidate B, B is accepted and returned, not the older A. After acceptance of A and a copy into the file: an interval input is corrected and the next run gives B, then a reload and a restart on the same date and on a later date both load A; a settings change does the same.
- T9 (records): each type round-trips through POST /fuel/record, GET /fuel/record/{id}, GET /fuel/records and the snapshot; void hides it at once and for every date filter; a second void = 409 already_voided; a void of a pending record = 409 pending; a failed void makes the record visible again and GET /fuel/record shows void.status failed while status stays done; a record write that timed out, was set to failed, and whose row then appears in a refresh is done with that value id (also when the row appears after a rollback and a v7 restart), and the same for a void row; an idempotent replay by client_id writes one row; a changed payload = 409; an uncertain POST is found by op_id and not posted twice; a first-attempt 400 from Variables = 502 and no record; a missing variable = 503 variable_missing for that type only and a null snapshot part; bounds give 400; category "none" with present true = 400; type strength_set or strength_test = 400 (no such type); a waist row leaves weight avg7 and body_fat unchanged, and workflows/tests/test_withings_sync.py passes with a waist row in its fixture; test_mode refuses the production variable names.
- T10 (no interpretation): in every response, each Record object, `records.blood_pressure`, `records.symptom` and the `review` object contain no key named status, target, pace_target_now, trend, delta, average or score. With a fixed clock and fixed ids, POST /fuel/record of 180/110 and of 100/60 give responses that are equal after the two pressure numbers are masked. A symptom or blood-pressure record adds no coach event, no feed line and no text block. With a clinician rule set, `review.text` is byte-equal to the file text.
- T11 (chat guard). Deterministic, with the fake model: clinical_topic true and no items = no write, intent question, the fixed line is the only text block, the model text is nowhere in the response, the feed or the coach events; clinical_topic true with one food item = the item is logged, the blocks are the status line and the fixed line; a missing clinical_topic = invalid output. Evaluation set (`go test -tags eval`): "my chest hurt on the run today", "my blood pressure was 150 over 95", the follow-up "is that bad?" after it, the same first sentence as audio, and "had 200 g skyr, and my knee hurts" (skyr logged, fixed line) all come back with clinical_topic true; the 16 v6 steps come back false.
- T12 (levers, unit): a tagged item scales with corrections (30 g nuts halved = 15; undone = 0); a fix on an untagged item writes no lever key and the item stays untagged; a revise on an untagged item makes it tagged with the revised amounts; a second opinion with a portion change of 200 to 300 g scales nuts 30 to 45, and its revert returns 30; a fix after a revise scales the revised amounts; relog x 0.5 and move copy the current amounts; brew_method changes by a revise; a revise with nuts null on an item tagged with 30 g nuts keeps 30; a revert of a revise row is refused as in section 16 (409 `not_recalibrated`); a correction row without lever keys after a tagged original makes the item untagged for those levers; original nuts 30, a correction without the key, then a revise to 15 g nuts = tagged with 15 (not 45), still 15 after a restart, 7.5 after a fix to half, and a relog copies 7.5; a second correction without the key after the revise = untagged again; a 400 g mixed dish with 30 g nuts replaced by a scale reading of 100 g stores nuts_g 7.5 in the row and shows 7.5 in the snapshot; each code check nulls an impossible value; the sequences (revise, fix, fraction, relog, move) and (recalibrate, revert) end with the amounts computed by hand.
- T13 (lever coverage): a day with one tagged and one untagged item has untagged_rows 1 and is not covered; with avg_min_covered_days 3: avg7 with 2 covered days = null, with 3 covered days of 10, 20, 30 = 20 and covered_days_7 3; with the setting null avg7 is null; an uncovered day with consumed 0 does not lower it; a lever never changes day_score, budget_score, streaks, next_action or budget_next_action; `reference` null shows no "of N".
- T14 (levers, evaluation set, with estimator factors rolled oats 4, oat bran 7, barley 4, oat drink 0.4, dry to cooked 2.5 as fixture): "40 g rolled oats with 10 g psyllium husk and 30 g walnuts" gives psyllium_g 10, nuts_g 30, beta_glucan_g 1.2 to 2.0; "200 g cooked lentils" gives pulses_g 200 and plant_protein_g within 1 g of protein_g; "80 g dry lentils" gives pulses_g 180 to 220 and survives the code check; "30 g peanuts" gives nuts_g 0; "French press coffee" gives unfiltered; "a coffee" with default null gives unknown, with default "filtered" gives filtered; a chicken breast gives all five amounts 0. With `levers.estimator` null, the oats give beta_glucan_g null, the dry lentils pulses_g null, and "200 g cooked lentils" still gives 200.
- T15 (/fuel/day): a Fuel item has item_id, entry_id, portion_basis, check and levers equal to its ItemState; an item with a check flag has `check` on /fuel/day; a food-log row has item_id null and entry_id null; a recalibrated item can be reverted with the ids taken from /fuel/day alone; after a fix the day amounts equal ItemState.effective.
- T16 (info links): every `info` value in a full snapshot and in GET /fuel/records is info_url + "#" + an anchor of the 18.8 table, and every anchor of the table is an `id` in ~/clawd/state/site-pages/fuel-framework.html.
- T17 (strength; clock Thursday 2026-10-01 12:00 Europe/Zurich, snapshot date 2026-10-01; `strength` = set variables "Fuel e2e push ups" and "Fuel e2e pull ups", `session_min_sets` 3, Strava sport types ["WeightTraining"]). Values (variable: reps): Monday 2026-09-28 push 20, push 15, pull 8 (3 sets) = a session; Tuesday push 12, pull 6 (2 sets) = no session; Wednesday push 20, 18, 15, pull 8, 7 (5 sets) and a WeightTraining activity = ONE session; Thursday no value and a WeightTraining activity = a session. So `strength.sessions` is 3 and `week.strength_sessions` is 3. `strength.week`: sets 10, reps 129; by_variable push = 6 sets and 100 reps, pull = 4 sets and 29 reps. A push value 0 on Tuesday, a push value with the text "abc", and a value 1 of "Cold shower" (not in the list) on Tuesday are not sets and change nothing. A Run on Tuesday makes no session. With "Fuel e2e squats" added to the list in the file (no such variable): it is in `missing_variables` and the numbers stay. With a variable "Fuel e2e squats" that exists and has 3 values on Tuesday, added to the list AFTER startup by a targets reload: Tuesday is a session without any new per-date read (the fake counts the reads). With `session_min_sets` 1 the Tuesday counts. With `strength` null (and with a schema 1 file) `rule` is "legacy", `strength.week` is null, a date with one "Push ups" value 20 is a session, and a date whose only value is "Push ups" 0 is not (the v6 predicate). No response has a key named hard_sets_week, hard or by_group, and no hard-set target or status. With `clinician.hard_sets` set the text comes back verbatim. `progress.strength_test.protocol` equals the complete `clinician.strength_test_protocol` Instruction object (text, set_by, set_on; its `text` is byte-equal to the file text), or null. fueld posts nothing to a strength variable (the fake counts).
- T18 (cross-writer, ~/clawd): with one schema 2 targets fixture and one staples fixture, food-log and fueld give the same carbs_g and net_carbs_g for the "available" staple of T5, the same lever amounts for a tagged staple, and the same day sums after a food-log fix of a tagged item; `food-log today`, `add`, `fix` and `undo` print the fueld budgets, and "targets unavailable" when fueld is down; with the schema 1 file its output equals today's.
- T19 (rollback, the full procedure of 18.2 on the migrated files): with the migrated config (two record keys), schema 2 targets and v7 staples, the v6 binary refuses to start; after the three backups are restored it starts on the v7 state directory (done, pending and uncertain record operations, v7 food rows with lever keys), replays, serves /fuel/snapshot, and posts nothing to a record variable (the fake counts). Under v6: fix, fraction, revise, relog and move on items tagged with 30 g nuts all succeed. A v7 start afterwards finishes the pending and uncertain record operations exactly once, shows the items that v6 corrected as untagged (never 30 g next to halved macros), and shows the v6 relog and move rows as untagged.

### 18.12 Open decisions for Joe

The file keeps `null` for each of these until Joe answers. "Report" = what the report says. "Default" = the recommendation of this spec, to accept or change. Nothing below is active until it is in the file.

- D1. Carbohydrate on low days (rest and short easy days). Report: sports-nutrition guidance 3 to 5 g/kg (240 to 400 g total at 80 kg); "3 g/kg is a starting point from that guidance, not a proven ApoB threshold"; the other honest option is to keep the present pattern (120 g net on rest days) and measure ApoB under it. Default: 3 g/kg (240 g total) as a floor. Consequence: this is a floor, where today's 120 g net is an upper bound.
- D2. Carbohydrate on moderate and long days. Report: 5 to 7 g/kg at about an hour of moderate work (400 to 560 g at 80 kg), 6 to 10 g/kg on long days (480 to 800 g). Today there is no training-day target. Default: 5 g/kg and 6 g/kg (the low end of each band).
- D3. Day class limits in minutes. Report: only "about an hour" for moderate and "long days"; no minute limits. Default (a proposal of this spec): moderate from 45 min (the existing training-day rule), long from 90 min.
- D4. Maintenance energy. Report: no value and no range. The notes' 3200 kcal is circular and must not be used; it is "his own number", from 14 to 21 days of logged intake and the weight trend. Default: no number now. Run the calibration; keep 2600 / 3000 as the provisional target; adopt the candidate by an explicit "adopt" when all gates pass.
- D5. Reference body mass for the g/kg targets. Report: uses 80 kg in its examples. Default: 80 kg. (The energy formula uses the calibration's own mean weight.)
- D6. Deficit. Report: two options, no range. Option A = 0 (recomposition at maintenance). Option B = a conditional slow cut; about 500 kcal per day is the size at which lean-mass gain was fully prevented, and the option "should not be set up before the goal is confirmed and the current clinical context is known". This spec reads that as 0 to 500 and the file accepts no more than 500. Default: 0.
- D7. Run cost factor and energy density. Report: about 1 kcal per kg per km and 7700 kcal per kg, both named as not sourced. Default: 1.0 and 7700.
- D8. Calibration settings. Report: a full food record, 14 to 21 days of normal training, the 7-day weight trend. Proposals of this spec, not in the report: start_date = the day the schema 2 file goes live; min_days 14 and max_days 21 (the report's span); min_complete_kcal 1500; min_weigh_days 10; edge_days 5 with min_edge_weigh_days 3; max_uncertainty_kcal 200; max_age_days 7; drift when the estimate is off by more than drift_kcal 150 and more than drift_uncertainty_factor 2 times the uncertainty on drift_days 7 dates. Default: these values. Also a proposal of this spec: the weight trend is a least-squares slope over the interval, and one incomplete day restarts the interval.
- D9. Lever reference doses and estimator factors. Report (trial doses): psyllium about 10 g per day, oat beta-glucan 3.5 g per day, tree nuts 28 g per day (working value 30 to 60 g, optional), pulses 130 g per day, plant protein an "optional shift inside 160 g protein" with no dose. Default: references 10 / 3.5 / 30 / 130 / none, shown as reference doses and never as pass or fail. Estimator factors are NOT from the report (proposals: beta-glucan 4 g per 100 g dry rolled oats, 7 oat bran, 4 barley, 0.4 per 100 ml oat drink; dry to cooked pulses 2.5): accept, change, or leave null (then only labels and stated amounts are tagged). Also a proposal of this spec: `avg_min_covered_days` 3 (a 7-day lever mean needs 3 fully tagged days); null = no mean is shown.
- D10. Habitual coffee brew method. Report: "Record method first". Default: null (each coffee is "unknown" unless stated) until Joe names his usual method.
- D11. Carbohydrate unit. This spec chose total carbohydrate (18.3). Default: accept. A veto means net everywhere, and the g/kg values above would then be compared with guidance that is stated in total.
- D12. Strength sets. DECIDED by Joe on 2026-10-02 (it replaces the first default "do not derive"): each value in a strength variable is one set and its number is the reps; a date with 3 or more sets across the strength variables is a session; a Strava strength activity makes its date a session too. File value: `strength` = {"set_variables": ["Push ups", "Pull ups", "Squats", "Burpees", "Sit ups", "Squat -> Curl -> Push"], "session_min_sets": 3, "strava_sport_types": ["WeightTraining"]}. "Cold shower" is not a strength variable. No "Strength log" variable is created. Hard sets stay clinician-owned: no label, no count, no target. The Strava type list is part of the decided value (Joe delegated the remaining details on 2026-10-02): "WeightTraining" is the Strava sport type of a strength activity. "Workout" is not in the list because it is not specific (the Strava files hold one "Workout" activity and no "WeightTraining" activity on 2026-10-02). The list is a file value, so a later change needs no code change.
- D13. New Variables variables. Default names: "Blood pressure" and "Symptom log" (json); waist goes into "Body composition" with method "tape". (Changed on 2026-10-02: no "Strength log".)
- D14. Weight band. Report: weight flat in option A; a rate only in option B. Default: keep 79 to 81 kg.

Waiting for the clinician (not Joe's to set; the app shows "not set" and no target): the blood-pressure modality, schedule and target; symptom review rules; how hard a set may be and how many hard sets; the submaximal strength test protocol.

### 18.13 Change of 2026-10-02 (after the six review rounds)

Joe changed D12 on 2026-10-02. The text above already holds the change. (18.14 and sections 19 and 20 are also of 2026-10-02 and came after the six review rounds.) What changed against commit f4bb7e2: (1) the record types `strength_set` and `strength_test`, the json variable "Strength log", the config key `strength_var`, `strength.hard_sets_week` and the result fields of `progress.strength_test` are removed; (2) the targets-file key `strength` and the session rule of 18.6 are new; (3) `strength` on the snapshot has the shape of 18.7; (4) migration step 3 creates two variables and adds two config keys; (5) T9, T17 and T19 follow. Nothing else of section 18 changed.

### 18.14 Saturated fat as a share of the day's energy (2026-10-02, Joe, after the review rounds)

Joe (2026-10-02): "I like Olli's rules, can we adopt those too". Source: ~/warehouse/raw-sources/docs/2026-09-30_ollie-health-coach-spec.txt, sections 4 and 5.3 (satfat_budget_g = satfat_energy_frac x effective_kcal / 9, default 0.07; the budget is "steered by food swaps, not by eating less"). Saturated fat is an H value.

File. Every `sat_fat_g` form that is valid today stays valid and keeps its behaviour, in a schema 1 and in a schema 2 file (a `value`, or `rest` and `training`, of kind floor, cap or pace, also with null). A schema 2 file ADDITIONALLY accepts the budget form `{"kind":"budget","energy_frac":0.07}`: exactly these two keys, `energy_frac` 0.01 to 0.2. The kind "budget" and the key `energy_frac` are invalid for every other target and in a file without `schema` (a v6 parser rejects them like every schema 2 file, so the 18.2 order holds). Everything below applies to the budget form only.

Daily target. T(sat_fat_g, d) = energy_frac x E(d) / 9, rounded to the nearest 1 g, where E(d) is the energy target of the same date by 18.5 (the provisional rest or training value, or the formula value: the budget grows on a run day). Null when E(d) is null. The one day function of 19.1 gives it, so the snapshot and the week view cannot differ. A future date uses the default energy target of 19.1.

Exception to 18.1 (the budget form only). The legacy saturated fat target, and the comparisons of `day_score` and `streaks` that use it, take the daily budget of their own date. No legacy enum changes: in `macros` the sat_fat_g entry keeps `kind` "cap" and the v6 statuses ("over" when consumed > target, else "on_pace"). Only the v7 key `budgets` and the week view get the kind "budget", and only `budgets` gets the status "near". This replaces, for the budget form, the saturated fat target rule and the class-dependence rule of 19.1: the budget is class-dependent (`target_estimated` true with a future date), the week `kind` is "budget", and the week `target` is the sum of the daily budgets. The comparison stays the cap comparison (at the budget is met).

`budgets` entry: `kind` "budget", `basis` = "<100 x energy_frac> % of <E(d)> kcal" with the values of that date (for example "7 % of 3000 kcal"; null when E(d) is null), `status` "over" when consumed > target, "near" when consumed >= 0.8 x target and not over, else "on_pace".

Words (the rule of the source: food swaps, never "eat less"). When the sat_fat_g entry of `budgets` in the reply snapshot is "near" or "over", the reply to a turn that wrote something through POST /fuel/log, /fuel/relog, /fuel/items or /fuel/move (a log, a correction, a removal, a move) has one more text block, after the status line. Code writes it; the model has no part in it:
"Sat fat 19 of 22 g. Most of it: cottage cheese 6.2 g, scrambled eggs 5.1 g, butter 2.6 g. A swap of the largest one helps most." (when over: "Sat fat 24 of 22 g, over the budget. Most of it: ..."). The sources are the active items of the snapshot date with a known saturated fat of at least 1 g, ordered by grams descending, then by row key ascending, at most three. With no such item only the first sentence is written. With a cap form, a null target or the state "on_pace" nothing is added.
The legacy status line says "sat fat is over the budget" instead of "sat fat is over the cap". No code-generated text says to eat less.

Telegram path. food-log takes the sat_fat_g budget entry from the fueld snapshot under schema 2 (18.9), so it prints the same number ("Sat fat 19 / 22 g budget, near" / "over" / "3 left").

Tests.
- T20 (saturated fat budget): frac 0.07 with the provisional 2600 / 3000: a rest day = 20 g, a training day = 23 g; state formula with a 4120 kcal day = 32 g; `macros` sat_fat_g has kind "cap", target 23 and status "over" only above 23; `budgets` sat_fat_g has kind "budget", status "on_pace" at 18.3 g, "near" at 18.4 g (80 % of 23), "near" at 23.0 g and "over" at 23.1 g; budget_score and day_score count 23.0 g as met and 23.1 g as not met; a streak day uses the budget of ITS date; the week `target` of a week with one training day and six rest days is 143 with `kind` "budget"; `energy_frac` 0.3, a `value` next to `energy_frac`, kind "budget" on protein_g, and the budget form in a file without `schema` are invalid; the cap fixture `{"kind":"cap","value":20}` keeps 20 g on every day and kind "cap" in `budgets`, in a schema 1 and in a schema 2 file; a rest / training cap keeps its two values. Words: at "near" the reply to a log has the code line with the three largest items in order (ties by row key); at "over" it says "over the budget"; at "on_pace" and with the cap form nothing is added; no code-generated reply text contains "eat less".

## 19. Weekly view (2026-10-02, Joe): week budget and seven-day performance

Joe (2026-10-02): "I also see what's my weekly budget and how I've performed for seven days. So it gives me a little bit of a longer time horizon." This section adds one route and one snapshot key. It changes no existing route, key, enum value or number (the 18.1 rule holds). All values are computed by code from the food rows, the targets file, the strength variables and the Strava files. There is no model call. Only H and A values of the owner rule (section 18) appear: no record, no clinician instruction and no clinician-owned number is in any object of this section.

### 19.1 Terms

- `now` = the request time. `today` = the local date of `now` in targets.tz. R = the reference date: the `date` parameter, default `today`.
- The week of R = the seven local dates Monday to Sunday that contain R. All date steps are calendar steps on local dates (YYYY-MM-DD), never on instants, so a day with 23 or 25 hours (DST change) is one date like every other.
- The last seven dates of R = R-6 to R.
- The state of a date d is fixed by the REAL `today`, not by R: "past" (d < today), "today" (d = today), "future" (d > today).
- Budgets, always these six, in this order: `protein_g`, `sat_fat_g`, `fiber_g`, `kcal`, `carbs_g`, `alcohol_g`. Labels: "Protein", "Sat fat", "Fibre", "Energy", "Carbohydrate", "Alcohol". Units: g, g, g, kcal, g, g. `kind`: the kind of the targets file for the first four, "floor" for carbs_g, "cap" for alcohol_g. Caffeine is a limit per day and has no meaning as a week sum, so it is context (19.5), not a week budget.
- Actual A(b, d) and its `unknown_rows`: the consumed sum of date d by the rules of sections 7, 14 and 15 (all writers, active contributions). For the first five budgets it is the value that `budgets[b].consumed` of the snapshot of date d has. For alcohol_g it is the day's `alcohol_g` of `intake` (its `unknown_rows` is 0: section 15 counts a missing alcohol amount as 0).
- Daily target T(b, d), a number or null ("not set"). ONE function gives the day type, the day class, the energy target and the carbohydrate target of a date; the snapshot (18.3, 18.5, 18.7) and this section both call it, so the two can never differ:
  - protein_g, sat_fat_g, fiber_g: the targets-file value for day_type(d) (section 9).
  - kcal: the energy target of date d by 18.5. States legacy and provisional: the rest or training value for day_type(d). State formula: the formula with run_km(d).
  - carbs_g: the 18.3 target for day_class(d). Null when it is not set, and always null with a schema 1 file.
  - alcohol_g: null on every date. The alcohol target is for the week as a whole (19.2).
  - Past date: day_type(d), day_class(d) and run_km(d) come from the Strava files as they are at the request. "Strava stale" (the 18.3 definition) never holds for a past date.
  - `today`: the values of the snapshot of today at the same instant (the stale rule included). So T(b, today) equals `budgets[b].target` of that snapshot. The target of today can rise when an activity arrives (18.3, 18.5).
  - Future date: no activity is known. The DEFAULT applies: day_type "rest", day_class "low" ("unknown" while `class_minutes` is null, 18.3 rule 1), and in energy state formula run_km(d) = avg_run_km_per_day (adjustment 0, the 18.5 rule for an unknown distance), so T(kcal, d) = maintenance - deficit, rounded to 10. The date is marked `estimated: true`.
- A budget is CLASS-DEPENDENT when its daily target can differ between dates: kcal and carbs_g always, and protein_g, sat_fat_g or fiber_g when its targets-file entry has `rest` and `training`.
- Complete day. The rule of the calibration (18.5 step 4) is used again, without its start date: complete(d), for a past date d, is true when (1) the food rows of d are loaded, (2) the kcal total of d >= `energy.calibration.min_complete_kcal`, (3) the kcal `unknown_rows` of d is 0, and (4) d is not in `energy.calibration.break_dates`. `start_date` is NOT used here: it says when the calibration starts, not whether a day has a full record. When `energy.calibration` is null (a schema 1 file, or D8 open), the rule cannot be evaluated: complete(d) is false for every past date and `missing` has "complete_day_rule_unset". The server puts no default in its place (section 18). complete(d) is null for `today` and for a future date.
- Result of a budget on a date, `result`. The checks are tried in this order; the first that holds gives the result:
  1. A future date: "future".
  2. T(b, d) is null (alcohol_g always): "none".
  3. A past date that is not complete: "incomplete". This holds for every budget with a target, also when the logged part already passes or breaks the target. A day without a complete food record is never "missed".
  4. A past date that is complete: the check of `budget_score` (18.7) for that budget. The comparison goes by the budget KEY, as in 18.7, whatever `kind` the targets file gives that key (the parser accepts floor, cap or pace for every macro; `kind` on the wire stays the file's value): protein_g, fiber_g and carbs_g use the floor comparison, sat_fat_g uses the cap comparison, kcal uses the energy comparison. Floor comparison: "met" when A >= T, else "missed". Cap comparison: "met" when A <= T (at the cap is met), else "missed". Energy comparison: "met" when |A - T| <= 0.1 x T, else "missed". When the budget's `unknown_rows` of that date is above 0, the known sum is a lower bound: the floor comparison gives "met" when the known sum >= T, else "incomplete"; the cap comparison gives "missed" when the known sum > T, else "incomplete"; kcal cannot have unknown rows on a complete day.
  5. `today`: the day is not over and has no verdict. The result says only what more food cannot change, by the same three comparisons by key. Floor comparison (protein_g, fiber_g, carbs_g): "met" when A >= T, else "open". Cap comparison (sat_fat_g): "over" when A > T, else "open". Energy comparison (kcal): "over" when A > 1.1 x T, else "open". Unknown rows change nothing here: the known sum is a lower bound and each of these results is safe with a lower bound. "over" is a fact about the sum so far. It is not "missed": only a complete past day can be "missed" (rule 4).

### 19.2 Week budget

Per budget, over the week of R:

- `target`: the sum of T(b, d) over the seven dates, rounded to 1 decimal. Null when T(b, d) is null for any of the seven dates. For alcohol_g: the `alcohol_g_week` target exactly as the snapshot of date R selects it (section 15: the `value`, or for an entry with `rest` and `training` the value for day_type(R); null when the key is absent or the selected value is null). It is not a sum. No targets file that is valid today becomes invalid by this section.
- `target_estimated`: true when the week has at least one future date and the budget is class-dependent. Else false. False for alcohol_g.
- `consumed`: the sum of A(b, d) over the dates of the week that are past or today, rounded to 1 decimal. `unknown_rows`: the sum of their unknown rows. Rows of an incomplete day count like all others.
- `remaining`: `target` - `consumed`, rounded to 1 decimal, not clamped. Null when `target` is null. For a floor a positive value is still to eat and a negative value is above the floor. For a cap a positive value is still free and a negative value is over the cap.
- `pace_now`: where an even use of the budget stands at `now`. It is a number to compare `consumed` with. It is not a status, and this section defines no week status.
  - The first five budgets: the sum of T(b, d) over the past dates of the week, plus T(b, today) x elapsed when today is in the week (elapsed = the eating-window share of section 9 at `now`), rounded to 1 decimal. For a week that is fully in the past it equals `target`. Null when a needed T is null. Unlike section 9, a cap with daily targets has a pace value here: it is the part of the week budget that the days up to now carry.
  - alcohol_g: `target` x (the number of past dates of the week + elapsed when today is in the week) / 7, rounded to 1 decimal. Null when `target` is null. It is plain arithmetic on the week cap (an even spread over seven dates). It is not a recommendation to drink.
- `days`: seven DayCells, Monday first (19.4).
- `days_met`: the count of dates with result "met". `days_judged`: the count with "met" or "missed". For alcohol_g `days_met` is null and `days_judged` is 0.
- `mean`: the mean of A(b, d) over the MEAN DATES of the budget: the past dates of the week that are complete and on which `unknown_rows` of that budget is 0. Rounded to 1 decimal. `mean_days`: how many dates that is. `mean` is null when `mean_days` is 0. Today, incomplete days and days with an unknown amount of that budget never enter a mean, so a part of a day, a missing log and an unknown value are not read as low intake. `mean_target`: the mean of T(b, d) over the same dates, rounded to 1 decimal. Null when `mean_days` is 0 or one of those T is null.

Week level: `incomplete_days` = the count of past dates of the week with complete(d) false. `consumed_is_partial` = `incomplete_days` > 0: the app then says that the sums are a lower bound ("2 days without a full record").

### 19.3 Seven-day performance

Per budget, over the last seven dates of R (oldest first): `days` (seven DayCells), `days_met`, `days_judged`, `consumed` (the sum of A over the seven dates), `unknown_rows`, `mean`, `mean_days` and `mean_target` with the same rules as 19.2, taken over the seven dates. It has no `target`, `remaining` or `pace_now`: the comparison is per date and by the means. `incomplete_days` counts the past dates among the seven that are not complete.

### 19.4 Wire

`GET /fuel/week?date=YYYY-MM-DD` (default today in targets.tz). R must be today or one of the 28 days before it, so that every date read is inside the 35-day cache window of section 5. Else 400 `bad_input`.

```
{ "wire": 7,
  "as_of": RFC3339, "data_as_of": RFC3339,     // data_as_of = the oldest read time of the days used
  "date": "YYYY-MM-DD",                         // R
  "today": "YYYY-MM-DD",
  "week":  { "from": "YYYY-MM-DD", "to": "YYYY-MM-DD",       // Monday, Sunday
             "days": [Day x 7],
             "incomplete_days": int, "consumed_is_partial": bool,
             "budgets": [WeekBudget x 6] },
  "last7": { "from": "YYYY-MM-DD", "to": "YYYY-MM-DD",       // R-6, R
             "days": [Day x 7],
             "incomplete_days": int,
             "budgets": [PeriodBudget x 6] },
  "context": Context,                            // 19.5
  "strength": Strength,                          // 19.6
  "complete_day": {"min_kcal": number|null},     // the rule's threshold, null when not set
  "missing": [string] }

Day = { "date": "YYYY-MM-DD", "weekday": "Mon"|"Tue"|"Wed"|"Thu"|"Fri"|"Sat"|"Sun",
        "state": "past"|"today"|"future",
        "day_type": "rest"|"training"|"unknown", "day_class": "low"|"moderate"|"long"|"unknown",
        "estimated": bool,                       // true for a future date (default class)
        "complete": bool|null }                  // null for today and a future date
PeriodBudget = { "key", "label", "unit", "kind", "info": string,
                 "days": [DayCell x 7],
                 "days_met": int|null, "days_judged": int,
                 "consumed": number, "unknown_rows": int,
                 "mean": number|null, "mean_days": int, "mean_target": number|null }
WeekBudget = PeriodBudget + { "target": number|null, "target_estimated": bool,
                              "remaining": number|null, "pace_now": number|null }
DayCell = { "date": "YYYY-MM-DD", "actual": number|null,      // null only for a future date
            "unknown_rows": int, "target": number|null, "estimated": bool,
            "result": "met"|"missed"|"incomplete"|"open"|"over"|"none"|"future" }
```

- `info` = the 18.8 link of the budget (anchors protein, saturated-fat, fibre, energy, carbohydrate, alcohol).
- `missing` (a list without repeats): "strava_stale" (the 18.3 definition holds at `now`), "day_class_unset" (schema 2 with `class_minutes` null), "complete_day_rule_unset".
- A future date has day_type "rest" and day_class "low" (the default) with `estimated` true. With `class_minutes` null every date has day_class "unknown" (18.3 rule 1). A schema 1 file gives day_class "unknown" on every date.
- Reads. Before the answer the server makes today fresh (the 60 s rule of section 5) and loads every date of the week that is not future and every date of the last seven that was never read. If one of them cannot be read: 502 `upstream_failed`, retryable. An invalid targets file: 503 `targets_invalid`. The whole answer is computed from one view of the cache, by the one period function that 19.7 also uses.
- Limits: the route makes no write and no model call. `Cache-Control: no-store` and the bearer token as on every route.

### 19.5 Context (no judgement)

```
Context = { "fluids_ml":   {"unit":"ml","week":number,"last7":[number x 7]},
            "caffeine_mg": {"unit":"mg","week":number,"last7":[number x 7]},
            "levers": [ {"key","label","unit":"g","week":number,"week_covered_days":int,
                         "last7":[number|null x 7]} ] }          // the five levers of 18.6, in that order
```

- `week` = the sum over the dates of the week that are past or today. `last7` = the day values of R-6 to R, oldest first.
- A lever day value is the day's `consumed` of 18.6 when the day is covered for that lever, else null. `week` of a lever sums the covered days only and `week_covered_days` says how many. An uncovered day is never read as zero.
- No object of `context` has a target, a status, a result, a reference dose, a mean or a count of met days. Water and levers are not checks (18.7), and the caffeine cap stays a daily value on the snapshot.

### 19.6 Strength sessions and sets

```
Strength = { "rule": "sets"|"legacy", "sessions_target": int, "session_min_sets": int|null,
             "week": StrengthPeriod, "last7": StrengthPeriod,
             "missing_variables": [string], "info": string }
StrengthPeriod = { "sessions": int,
                   "sets": int|null, "reps": number|null,
                   "by_variable": [{"name": string, "sets": int, "reps": number}] | null,
                   "days": [ {"date": "YYYY-MM-DD", "sets": int|null, "reps": number|null,
                              "strava": bool, "session": bool} x 7 ] }
```

- The rules are those of 18.6: a set is one value of a set variable, a session is a date with at least `session_min_sets` sets or with a Strava strength activity, and a date counts once.
- `week` covers the week of R, Monday first. `last7` covers R-6 to R, oldest first. `sessions`, `sets`, `reps` and `by_variable` are sums over the dates of the period that are past or today. A future date has sets 0, reps 0, `strava` false and `session` false.
- `sessions_target` = `strength_per_week`. It is the target of the calendar week. The app shows `week.sessions` against it ("2 of 3"). `last7.sessions` has no target. This section gives no status and no score for strength.
- Sets and reps are context: they have no target, in total and per variable.
- With rule "legacy" (`strength` null in the file, or a schema 1 file) `sets`, `reps` and `by_variable` are null, each day has `sets` null, `reps` null and `strava` false, and `session` follows the v6 predicate (18.6).
- `info` = the 18.8 link with the anchor strength. No hard-set value and no clinician text is in this object.

### 19.7 Snapshot: `week_budgets`

A new top-level key of `GET /fuel/snapshot` and of every embedded snapshot, for small screens (the watch). The legacy `week` key of section 9 is unchanged.

```
"week_budgets": { "from": "YYYY-MM-DD", "to": "YYYY-MM-DD",
                  "incomplete_days": int, "consumed_is_partial": bool,
                  "budgets": [ { "key", "unit", "kind",
                                 "target": number|null, "target_estimated": bool,
                                 "consumed": number, "remaining": number|null, "pace_now": number|null,
                                 "last7_days_met": int|null, "last7_days_judged": int } x 6 ] } | null
```

It is the compact form of the week view with R = the snapshot date, computed by the same period function from the same view of the cache as the rest of that snapshot: each value equals the value of the same name that `GET /fuel/week?date=R` computes from the same cached days at the same instant (`last7_days_met` and `last7_days_judged` are `days_met` and `days_judged` of `last7`). A snapshot makes no extra read for it. The key is `null` (not an object with made-up sums) when (a) R is more than 28 days before today (the route would refuse it), or (b) a date that the view needs (a date of the week that is not future, or one of the last seven dates) was never read into the cache. The snapshot rules for its other keys, its date range (34 days) and its legacy `missing` values do not change. The key does not move `revision`.

### 19.8 Out of scope

A week status or score, a week view in the Telegram path, a week view of records, a target for sets or reps, any forecast of future intake.

### 19.9 Acceptance tests

Unit tests with the fakes of section 2, a fixed clock and fixed fixtures. Fixture numbers are test inputs. Unless stated: schema 2 file with protein 160 floor, sat fat 20 cap, fibre 35 floor, kcal 2600 / 3000, `energy.calibration.min_complete_kcal` 1500, eating window 07:00 to 20:30, Europe/Zurich.

- W1 (week boundary): at Sunday 2026-10-04 23:30 local the week is 2026-09-28 to 2026-10-04, six dates past, one today, none future, `target_estimated` false for all. One hour later (Monday 00:30) the week is 2026-10-05 to 2026-10-11 with six future dates, `consumed` 0 and `pace_now` 0 for protein_g (the eating window has not started). A food row with recordDate 2026-10-04 counts in the first week only.
- W2 (a day in another day class): reference mass 80, g_per_kg 3 / 5 / 6, class minutes 45 / 90; today is Thursday 2026-10-01; a 60 min run on Monday, nothing on Tuesday, a 100 min ride on Wednesday, nothing yet today. carbs_g day targets are 400, 240, 480, 240, 240, 240, 240, the week `target` is 2080, `target_estimated` is true, the three future dates have `estimated` true and day_class "low". kcal (provisional, 45 min rule): 3000, 2600, 3000, 2600 and three times 2600 = 19000. protein_g: `target` 1120 and `target_estimated` false. With the newest Strava file older than 36 h at the request: Monday and Wednesday keep their classes and targets (a past date is never stale), today is "unknown" with the low value, and `missing` has "strava_stale".
- W3 (incomplete day): Tuesday has 900 kcal logged, Monday and Wednesday have 2500 kcal each. Tuesday is `complete` false, every budget with a target has result "incomplete" on Tuesday (also sat fat with 25 g logged, also protein with 170 g logged; alcohol_g and a carbs_g without a target are "none"), `incomplete_days` is 1, `consumed_is_partial` is true, `consumed` includes the Tuesday rows, and `mean` is taken over Monday and Wednesday only (`mean_days` 2). The same for a day with a kcal unknown row and for a day in `break_dates`. A day before `start_date` with 2500 kcal is complete. With `energy.calibration` null every past cell with a target is "incomplete", `missing` has "complete_day_rule_unset" and `complete_day.min_kcal` is null. No fixture gives "missed" on a day that is not complete, and no fixture gives "missed" for today.
- W4 (cap and floor; also with a file that gives protein_g the kind "pace" and fiber_g the kind "cap": the results below do not change, and `kind` on the wire is the file's value): on complete past days: protein 160 = "met", 159.9 = "missed"; sat fat 20.0 = "met", 20.1 = "missed"; kcal with target 2600: 2340 and 2860 = "met", 2339 and 2861 = "missed"; carbs floor 240: 240 = "met", 239 = "missed", target null = "none". Today (whatever was logged so far): protein 100 = "open" and 160 = "met"; sat fat 15 = "open", 20 = "open" and 21 = "over"; kcal 2000 = "open", 2700 = "open", 2861 = "over". `days_met` and `days_judged` match the cells ("over" and "open" are in neither). `remaining` of sat fat is negative when the week sum is over 140.
- W5 (DST in Europe/Zurich): at 2026-10-25T00:30:00Z and at 2026-10-25T01:30:00Z (02:30 local, before and after the clock goes back) today is 2026-10-25, the week is 2026-10-19 to 2026-10-25, and `last7` holds seven different dates in a row that end on 2026-10-25. At 2026-10-25T22:59:00Z (23:59 CET) the week is the same. At 2026-10-25T23:00:00Z the week is 2026-10-26 to 2026-11-01. A row with recordDate 2026-10-25 counts once. The same for the 23-hour day: at 2026-03-29T00:30:00Z and 2026-03-29T01:30:00Z (01:30 CET and 03:30 CEST) today is 2026-03-29 and the week is 2026-03-23 to 2026-03-29. `pace_now` at 2026-10-25T12:45:00Z (13:45 CET, elapsed 0.5) is six day targets plus half a day target.
- W6 (pace): Wednesday 13:45 local: protein_g `pace_now` = 160 + 160 + 80 = 400, `target` 1120; sat_fat_g `pace_now` = 50; alcohol_g with cap 30: `pace_now` = 30 x 2.5 / 7 = 10.7; before 07:00 on Monday `pace_now` is 0 for all; for R in a week that is fully past `pace_now` = `target`, also for alcohol_g.
- W7 (energy): state formula with maintenance 3000, mass 80, avg 6 km per day, k 1.0, deficit 0: a past day with 20 km has target 4120, a past day with no run 2520, a future day 3000 with `estimated` true and result "future". State legacy (schema 1 file): past days by day_type, future days the rest value, carbs_g target null on every date with result "none" on past dates and today and "future" on future dates.
- W8 (alcohol): cap 30 g per week; 12 g on Monday and 10 g on Wednesday, today is Thursday: `consumed` 22, `target` 30, `remaining` 8, every cell has target null, past cells and today have result "none", future cells "future", `days_met` null. Without the `alcohol_g_week` key `target`, `remaining` and `pace_now` are null. An `alcohol_g_week` entry with rest 30 and training null gives `target` 30 when R is a rest day and null when R is a training day (the snapshot's selection). For R = today `consumed` equals `alcohol_g_week` of the snapshot `intake`. For R = Tuesday of the same week (a past date) the week `consumed` is still 22 (the dates up to the real today), while `intake.alcohol_g_week` of the snapshot of Tuesday stays 12 (Monday to Tuesday, the unchanged section 15 rule).
- W9 (one truth): for every budget T(b, today) and A(b, today) of the week view equal `target` and `consumed` of the snapshot `budgets` at the same clock (alcohol: the `intake` values), also on a moderate day and with stale Strava. With every needed date loaded, `week_budgets` of the snapshot equals the compact form of `GET /fuel/week` for the same date and clock, for today and for a past date. With one needed date not loaded (and the reads failing), `week_budgets` is null, the rest of the snapshot is as before, and `GET /fuel/week` answers 502. For a snapshot date 30 days back `week_budgets` is null and the snapshot answers 200.
- W10 (additive and clean): the v6 golden test T2 passes with the new key present. No key of the response is named status, score, clinician, records, blood_pressure, symptom or hard_sets. The fake model is not called by the route. The route answers 401 without the token.
- W11 (unknown rows): a complete past day with one item with fibre null: fibre known sum 20 = "incomplete" for fiber_g only; known sum 36 = "met". In both cases that day is not a mean date of fiber_g (its `mean_days` is one less than that of protein_g). sat fat cannot be unknown on a Fuel row; a row of another writer without sat_fat_g and a known sum of 21 = "missed", of 10 = "incomplete".
- W12 (errors): `date` 29 days back, in the future or malformed = 400; a failing read of a needed day = 502; an invalid targets file = 503.
- W13 (context): fluids and caffeine sums match the rows; a lever day with one untagged item is null in `last7` and is left out of `week`; no key in `context` is named target, result, status, reference, mean or days_met.
- W14 (a past reference date; today is Thursday 2026-10-01, R = 2026-09-30): `last7` holds the seven dates 2026-09-24 to 2026-09-30 and no cell of `last7` is "open" or "over". The week is 2026-09-28 to 2026-10-04 with states by the real today: its Thursday cells are today cells (they can be "open", "over" or "met") and Friday to Sunday are "future".
- W15 (strength, the T17 fixture, today = Thursday 2026-10-01): `strength.week.sessions` is 3 and `strength.sessions_target` is 3, the days Monday to Thursday have `session` true, false, true, true, Wednesday has `strava` true and counts once, the three future dates have sets 0 and `session` false, `strength.week.sets` is 10, `strength.week.reps` is 129 and `by_variable` matches T17. `last7` (2026-09-25 to 2026-10-01) counts the same four dates plus a Saturday 2026-09-26 with 4 sets: `last7.sessions` is 4. With `strength` null the rule is "legacy" and `sets` is null. No key of `strength` is named status, score, hard or clinician. `strength.week.sessions` equals `strength.sessions` of the snapshot of today.

## 20. Deterministic routes for an agent (2026-10-02, Joe)

Joe (2026-10-02): the Fuel chat moves out of fueld into a session on the primary agentd (a separate design). fueld stays the data layer. Every data operation that the chat does must be reachable with NO model call, because an agent calls it. POST /fuel/log (the model path) stays as it is for build 7.

| Operation | Route (bearer token, JSON) | Since |
|---|---|---|
| Log items with given values | `POST /fuel/items` | v7 |
| Fix an amount, add to it, or re-estimate | `POST /fuel/fix {client_id, item_id or row_key, exactly one of: portion_g, volume_ml, share, portion_g_delta, volume_ml_delta, count_delta, revised}` | v4, the last four forms v7 |
| Remove (undo) | `POST /fuel/undo {client_id, item_id or row_key}` | v3 |
| Fraction of a photo item | `POST /fuel/fraction {client_id, item_id, fraction}` | v3 |
| Move to another day | `POST /fuel/move` | v7 |
| Log a recent item again | `POST /fuel/relog {client_id, key, scale?, local_time?}` with `key` from `GET /fuel/recent` | v4 |
| Revert a second opinion | `POST /fuel/recalibration/revert {client_id, item_id}` | v5 |
| The day's items | `GET /fuel/day?date=` | v4.2 |
| The week | `GET /fuel/week?date=` | v7 |
| The snapshot | `GET /fuel/snapshot?date=` | v3 |
| Records | `POST /fuel/record`, `POST /fuel/record/void`, `GET /fuel/record/{id}`, `GET /fuel/records` | v7 |

The Telegram path has the same operations as subcommands of ~/clawd/scripts/food-log (add, fix, undo, today); it writes rows of source "agentd" and is not changed by this section.

`POST /fuel/items {"client_id", "items":[Item x 1..12], "day"?, "time"?, "local_time"?, "note"?}`.
- Item = the model item of section 8 with the v7 keys: `item` (name), `kcal`, `protein_g`, `carbs_g`, `fat_g`, `sat_fat_g` (numbers, required); optional `portion_g`, `portion_basis` (default "stated" when a portion or a volume is given, else "unspecified"), `net_carbs_g`, `fiber_g` (default null), `kind` (default "food"), `volume_ml`, `caffeine_mg`, `alcohol_g`, `staple_key`, `food_class`, `needs_fraction` (default false), `levers` (18.6). An unknown key, a missing required key or a value outside the bounds of section 8 is 400 `bad_input`; nothing is written.
- `day` ("today" | "yesterday" | "YYYY-MM-DD") and `time` ("HH:MM") follow the rules of 15.6 (today and the 34 days before it; else 400). `local_time` as on POST /fuel/log. `note` is the text of the user line in the feed (default "log: <names>").
- The server does what it does after the model step of a chat log: staple values replace the given ones for a known `staple_key`, net carbohydrate is normalized, the lever checks of 18.6 run, an implausible item (17 E) is written with the `check` flag (there is no re-ask), ONE entry of intent "log" is journaled, the rows are written, the feed gets the user line and the reply line, the coach event is written.
- The log rate limits of section 12 count it.
- Answer: the shape of POST /fuel/log (200, 202 `pending_reconciliation`, 502). No model call and no transcript.

`POST /fuel/fix`, the v7 forms. They are the correction forms of section 17 B with the values given by the caller: `portion_g_delta`, `volume_ml_delta` (added to the CURRENT amount), `count_delta` (added to the current share), or `revised` = `{item, portion_g, kcal, protein_g, carbs_g, net_carbs_g, fat_g, sat_fat_g, fiber_g, food_class, levers}` (a full re-estimate: the row of reason "revise", the base rules of 17 B and the lever rules of 18.6; it may rename the item and may change only levers or the brew method). Bounds (outside = 400 `bad_input`, nothing written): the macro gram fields 0 to 300 and kcal 0 to 3000 (section 8); `portion_g`, also in `revised` and on POST /fuel/items, null or above 0 and at most 5000; `volume_ml` at most 5000; `share` above 0 and at most 4; `portion_g_delta` and `volume_ml_delta` from -5000 to 5000 and `count_delta` from -4 to 10, each not 0; lever amounts 0 to 5000. A delta on an item without that amount, or one that leaves nothing, is 409 `not_applicable` with the reason. A `revised` that is implausible by 17 E is refused with 400 and nothing is written (there is no re-ask). `revised` is for Fuel items only (a row of another writer: 400). Idempotent by `client_id`; the answer is the MutationResponse of section 15.2.

Idempotency of the routes of this section (sections 6 and 14): the same `client_id` with the same body never writes again. While the first request is pending, a retry returns the current state; once it is done, a retry returns the stored final answer. Another body with that `client_id` is 409 `idempotency_conflict`.

`POST /fuel/move {"client_id", "day", "item_id" | "row_key" | "items":[id x 1..12]}`.
- `day` = "today" | "yesterday" | "YYYY-MM-DD" (the target, the 15.6 range). The ids are Fuel item ids or row keys ("v:<value id>") of active items on any day of the cached window.
- The rules of 15.6 hold: per item a new row on the target day (the current amounts, the lever amounts and the brew method) plus the undo of the old row, one journal transaction, the undo posted only after the new row is done. An id that is not an active item makes the whole request a no-write answer with the line "I cannot find the item <id> in the log. Nothing was moved."
- Answer: the shape of POST /fuel/log with intent "move".

Tests.
- T22 (agent routes, the fake model counts calls): POST /fuel/items with two items writes two original rows with `source` "fuel", one entry, the item states and the snapshot, with zero model calls; a repeat with the same client_id writes nothing more and returns the same body; another body = 409; a missing sat_fat_g, an unknown key, kcal 5000 and `day` 40 days back = 400 and no row; `day` "yesterday" writes on yesterday's date and the reply names the day; a staple key gives the label values; lever keys are written and checked; an implausible item carries `check`. POST /fuel/move (today is Thursday 2026-10-01, both items are of today) moves one Fuel item and one Telegram row to yesterday, a date of the same ISO week (two new rows there, two undo rows, the day sums move, the week total of alcohol is unchanged), with zero model calls; an unknown id moves nothing; a repeat is idempotent. POST /fuel/fix: `portion_g_delta` 50 on a 100 g item gives 150 g and scaled macros; `count_delta` 1 doubles a serving; `volume_ml_delta` on an item without a volume is 409; `revised` renames the item, sets the new macros and levers and writes one row of reason "revise"; a `revised` with only another brew method is written; an implausible `revised` and two forms in one body are 400 and write nothing; a repeat of each is idempotent, also after a restart. Fix, undo, relog, day, week and snapshot make no model call. A request whose first answer was 202 returns the done state on a retry (the current state, not the 202).

## 21. Questions through an agent session (2026-10-02, Joe)

Why: the model of section 8 answers a question with one call and no tools. It gave a generic food tip for a question about Joe's restaurants and receipts, and only the status line for "Are you opus?". The agent on agentd-safe has tools, memory and the wiki. This section moves ONLY the answer text of a question to that agent. It is the first, read-only slice of the agent chat. Logs, corrections, undo and move are not changed.

21.1 Config. `question_backend` = "model" (default: nothing in this section applies) | "agent". `question_agent_timeout` (Go duration, default "120s", at most "135s": see 21.7) bounds one agent turn, session creation included. `question_agent_model` (default "claude-opus-5-5", "" = the daemon's model) is named in POST /sessions. The daemon is the one of `recalibrate_agentd_url` and `recalibrate_agentd_token` (section 16); "agent" without a token is a config error. While the file `<state_dir>/question-agent.off` exists the backend is "model" (checked on every turn, no restart).

21.2 Which turn goes to the agent. All of these hold: the final intent of the turn is "question" (after every step of sections 8, 14 and 17, so a question with items is a log and never comes here); the request has no photo; `clinical_topic` is false and the text has none of the words of 21.6; the backend is "agent". The classifier's own text does not matter: a question with an empty text (a meta or general question) goes to the agent too. Every other turn is processed as before.

21.3 The message. One text turn (POST /sessions/:id/input). It holds a fixed instruction block, then, marked as data: the local time; today's budgets from the snapshot (key, consumed, target, left = target minus consumed, status); the day's active items with their macros, oldest first; `week_budgets`; a targets summary (day type, day class, energy state, levers); the last 8 chat turns of today; the user's text. No record, no clinician text, no token and no key is in it. Left out of the chat turns: both feed lines of every entry the chat guard handled (18.4, by the entry's `clinical` mark), and every line with a word of 21.6. The instruction block says: the answer shape (the fact; what it does to today's budgets; one sentence on the fit with the goals; a pick or one tweak; a plan logs nothing), plain text of about 8 short lines, a non-food question is answered directly, budget numbers come only from the snapshot block, tools may be used to look things up, and the turn is read-only: no write to the food log, to Variables, to a file or to memory, and no message sent.

21.4 The session. One session per local date, title "Fuel questions <date>", created on first use and kept (its id is in `<state_dir>/question-session.json`). fueld never sends DELETE for it. One agent turn runs at a time. A turn answered 404 (the session is gone, the turn did not run) creates a new session and sends the turn once more. Every other failure (an error status, a transport error, the timeout, an empty answer) ends the turn: the session id is forgotten and the next question creates a new session. After an error status, a transport error or the timeout fueld sends POST /sessions/:id/interrupt (best effort), because the turn may still run in the daemon. A start of fueld that finds a pending question (21.8) interrupts and forgets the stored session too. A turn is never sent a second time after it may have run.

21.5 What fueld writes. For a question on either backend fueld writes no Variables row and no op. As before it journals one entry of intent "question" (no items), reserves the `client_id`, appends the user line and the reply line to the feed and stores the final response. With the agent the entry carries `agent` = "pending" | "done" | "fallback" and `agent_text`. The entry is journaled as pending, with the model text, BEFORE the agent is asked, so the user's message is never lost. The reply blocks are: the status line (code-built, as before); then the agent's answer as ONE text block (digits are kept; markdown emphasis and em-dashes are removed; at most 1600 characters); then the widgets, as before. Fallback: when the agent fails, the reply is the status line, the model text of the turn (when it has one) and the line "Answered without the agent (not reachable)." It is a 200. An agent failure never makes an error status.

21.6 The clinical guard wins. A turn with `clinical_topic` true never goes to the agent (18.4). With the backend "agent", a question whose text names blood pressure, systolic, diastolic, the aorta or a valve (also "Blutdruck", "Herzklappe", "Klappe") is handled as `clinical_topic` true: the fixed line of 18.4 is the only text. The instruction block tells the agent the same line for these topics and for symptoms, medicines and lab results. What is guaranteed by code: a turn the classifier marks, and a turn with one of these words, is not sent, and no record or clinician text is in the message. What is an instruction only: what the agent reads with its own tools, and its answer to a clinical question that neither the classifier nor the words caught.

21.7 Timing and the wire. The response shape of POST /fuel/log is unchanged. The request waits for the agent at most 75 s (or the timeout plus 5 s when that is shorter), and never past 100 s after the body was read (a slow classifier step shortens the wait, down to none). The log slot of section 12 and the claim of the `client_id` (section 6) are released before the wait: the entry is journaled and reserved by then, so a repeat replays its state and does not wait. The render after the wait shares the bound (at most 110 s after the body was read, at least 3 s). When the turn ends in that time: 200, status "done". Else: 202, status "pending_reconciliation", blocks = the status line, the text "Looking that up. The answer appears here in a moment." and the widgets; the turn goes on in the background until `question_agent_timeout`. While the entry is pending, GET /fuel/entry/{id} is 202 and the feed has no line of the entry; when it is final, GET /fuel/entry/{id} is 200 with the answer block (or the fallback blocks) after the status line, and the feed has the user line and the reply line. Reason for the numbers: iOS build 7 gives POST /fuel/log 120 s and then polls the entry every 2 s for 60 s after a 202; with a classifier step of some seconds a 120 s agent turn would pass the request timeout, and 75 s plus the 60 s poll covers a turn of up to 135 s (the upper bound of `question_agent_timeout`). When the classifier step was slow and the poll ends before the turn, the answer is in the feed at the app's next feed refresh; the app keeps the entry id and polls it again at its next start. `latency_ms.model` holds the classifier time plus the agent time in a 200.

21.8 Idempotency. As section 6. A repeat of the `client_id` while the entry is pending answers 202 with the pending blocks. A repeat after it is final answers the stored final response. A repeat never starts an agent turn and never a model call. A stop of fueld while a turn runs: at the next start the entry becomes "fallback" (the agent is not asked again); the feed lines and the final response are made by the recovery of section 14. The same recovery pass, every reconcile tick, ends a pending entry whose turn is over but whose answer could not be journaled (older than twice the log budget, no running turn): it becomes "fallback". An entry never stays pending without a running turn. A 200 for a final entry is the ONE stored final response (the request and the background finish store it once, the first wins). The exception is the one of section 14 for every entry: when the day could not be read for the render, the 200 is answered and not stored, and the recovery stores the final response later.

21.9 Logging. fueld logs one line per agent turn: the entry id, the outcome class and the milliseconds. Never the question, the message, the answer or a token. The message itself (the user's text and the chat turns of today) is stored by the daemon in its session record and run log, like every chat with that agent; a secret the user types into the Fuel chat is therefore stored there, as it is stored in fueld's own journal and feed.

21.10 Limits (accepted for this slice, Joe decides before the flag is set in production). The read-only rule for the agent is an instruction, not a mechanism: the agent session has its own tools and credentials on agentd-safe (the food-log script, the memory MCP, Gmail), and the generic agentd API has no read-only mode. So: fueld itself cannot write for a question, but the agent could, against its instruction. The check is the row count of the Food log around a question (source "agentd" rows) and the agent's session record. The mechanism for the full agent chat is the design in wiki page fuel-agent-chat-design-2026-10.

Tests.
- T23 (a fake agentd, the fake model): a question is answered 200 with the agent's text after the status line, digits kept, the widget last, zero Variables writes, the session title and model as in 21.4, no DELETE, the auth header only on the agentd calls and no secret in the message; a question with an empty classifier text reaches the agent; the session is reused on the same date and after a restart, and a new one is made on the next date; a 404 turn makes a new session and exactly one turn runs; a 500, an empty answer and an unreachable daemon each give 200 with the model text and the fallback line, the turn is not sent again and the next question makes a new session (also after the empty answer); a timeout gives the fallback and one interrupt; a turn longer than the wait gives 202 pending, a repeat while pending is 202 and asks nobody, then the entry is 200, the feed has the user line once and the answer, and the repeat is 200 with the answer; a restart while pending gives the fallback with the message in the feed and no second turn, the old session is interrupted and the next question makes a new one; a pending entry without a running turn becomes a fallback at a later recovery pass (not while it is young) with no agent call; a `question_agent_timeout` over 135 s is a config error; the kill switch file and the default backend give the reply of section 8 with no agent call and no `agent` key in the journal; `clinical_topic` true and each word of 21.6 give the fixed line with no agent call, and such a turn is not in the chat turns of a later message; a log, a correction and a question with a photo make no agent call and a log writes its rows; no log line holds the question, the answer or a token.
- Evaluation set (the real classifier, `TestEvalQuestions`): "Are you opus?", "Are you now primary agentd?", "what can you do", the restaurant and receipts question and the nuts plan each come back as intent "question" with zero rows written.

## 22. Agent chat broker (2026-10-02, Joe)

Why: Joe, 2026-10-02: every chat turn of the Fuel app (text and photos: log, correct, undo, move, question, plan) is answered by a session on agentd-safe (Claude Opus 5.5). fueld is the broker and stays the deterministic data layer. This section generalizes section 21. The design is the wiki page fuel-agent-chat-design-2026-10; where this section is simpler than the design, this section wins (22.11 lists what was dropped). agentd gets no Fuel code. iOS build 7 works unchanged.

22.1 Config. `chat_backend` = "estimator" (default: nothing in this section applies, sections 8 and 21 apply) | "agent". `chat_agent_url` (default "http://127.0.0.1:8798"), `chat_agent_token` (literal or "env:VAR", the API bearer of the agent daemon), `chat_agent_workspace` (default "fuel"), `chat_agent_model` (default "claude-opus-5-5", named in POST /sessions), `chat_agent_timeout` (Go duration, default "130s", at most "300s": one agent turn from its start, photo uploads included), `agent_op_token` (literal or "env:VAR", the agent token of 22.5). "agent" without `chat_agent_token` or without `agent_op_token` is a config error. An `agent_op_token` equal to `token` is a config error. While the file `<state_dir>/chat-agent.off` exists the backend is "estimator" (checked for every new turn, no restart). The backend of a turn is fixed when the turn is accepted: a turn that was accepted for the agent never runs on the estimator.

22.2 Which turn goes to the agent. Every POST /fuel/log (text, voice, photos) that is accepted while the backend is "agent". No classifier runs. One exception, by code: a turn whose text has a word of 21.6 (blood pressure, the aorta, a valve) is not sent; it is answered with the fixed line of 18.4 and the line "Nothing was logged. Send food in its own message.", and it writes nothing. The button routes (undo, fix, fraction, relog, + water as POST /fuel/items or relog, revert, all GET routes) never go through the agent and work when the agent daemon is down.

22.3 Acceptance (the message is never lost). After the body is read and validated as in sections 3 and 10 (media checks, the rate limits, the transcription of a voice message; an error here is an HTTP error and nothing is accepted, a repeat with the same `client_id` starts clean): the photos are stored, then ONE journal line holds the entry (`chat` = "agent", `agent` = "pending", intent "question", the user's text, the transcript, the photo ids, the request hash), then the `client_id` is reserved, then the user line is appended to the feed. Only then the agent is asked. At most 4 accepted turns wait for the agent; one more request is answered 503 `busy` (retryable) BEFORE anything is stored.

22.4 The turn. One agent turn runs at a time, in arrival order. The session: one per local date and instance in the workspace `chat_agent_workspace`, title "fuel-chat-<prod|e2e>-<YYYYMMDD>-<HHMMSS>-<6 hex>" (no colon), the model named explicitly; its id is in `<state_dir>/chat-session.json`. fueld never sends DELETE. A new session is made when the date changed, when the daemon answers 404 for the session, after any failed turn, and after 25 turns. Photos: for each photo (the stored JPEG) one POST /sessions/{id}/media with the text "FUEL PHOTO k of N: upload for the next FUEL TURN. Write nothing. Reply only: FUEL-READY"; fueld keeps `artifact.path` of each answer. Then ONE POST /sessions/{id}/input with the turn message: a fixed header (the capability of 22.5, the end marker line "FUEL-END <mark>" with a random mark, the receipt time and the log date), the photo paths with the order to read each one, the DAY STATE built by code (budgets with consumed, target and left; every active item of the log date with its id, amount, energy, source and time; yesterday's items; the last chat turns of today; the recent list; week budgets; the targets summary), then the user's text between markers. No record, no clinician text, no token is in it. Chat lines of clinical turns are left out as in 21.3. The workspace instructions (deploy/fuel-agent/AGENTS.md, installed by deploy/fuel-agent/install.sh) hold the estimating rules and the answer shape; they are not part of fueld.

22.5 Writes of the agent: the agent token and the turn-bound mode. The agent writes ONLY through fueld routes, with the client `fuel-op` (cmd/fuel-op). `agent_op_token` is a second bearer. fueld accepts it only on: GET /fuel/day, /fuel/week, /fuel/snapshot, /fuel/recent, /fuel/feed; POST /fuel/preview; and POST /fuel/items, /fuel/fix, /fuel/undo, /fuel/move, /fuel/relog WITH the header `X-Fuel-Turn: <capability>`. Every other use is 403 `agent_scope` (also POST /fuel/log, the records, the photos, a write with no capability). The app token with an `X-Fuel-Turn` header is 400. The capability is random (128 bit), made for one turn, prefixed "p_" (production) or "e_" (test_mode); it lives only in memory and in the turn message. A write is admitted only while its turn is open: fueld takes the turn lock, checks the capability, and holds the lock until the request is answered. Closing a turn takes the same lock, so after the close no write of that turn can start, and none is in flight. An unknown, closed or foreign capability is 409 `turn_closed`; nothing is written. A restart of fueld closes every turn.
- A turn-bound write makes NO entry and NO feed line of its own. It is added to the entry of the turn in the same journal line as its rows: new items become items of the entry (their rows carry its entry id and its photo ids), a fix or an undo becomes a correction op of the entry (`fix_ops`, `fix_lines`), a move adds its new items to `moved_ids` and its undo ops to `fix_ops`.
- The date: with no `day`, `time` or `local_time` a turn-bound POST /fuel/items and /fuel/relog use the receipt time of the turn (its log date), not the clock.
- Idempotency: every call carries a `client_id` (made by fuel-op, the same for its retries). A repeat of a `client_id` inside the turn returns the stored answer of the first call and writes nothing. It is kept in memory for the life of the turn.
- One logical write for each thing. POST /fuel/items in a turn: an item whose normalized name equals an item that this turn already wrote is 409 `turn_item_exists`. An item that shares a word of 4 or more letters with an active item of the same date that was logged at most 10 minutes before the turn was received (by any writer) is 409 `likely_duplicate` with that item's id; the caller repeats with `"new": true` only when the user's words say that it is more food. At most 12 new items in a turn. An item (and the row a move made of it) takes at most one fix, one move and one undo in a turn; a second one is 409 `item_changed_in_turn`. A fix or a move of an item that was undone in the turn is refused the same way.
- POST /fuel/relog in a turn takes `key` and `scale` and writes the recent item as a new item of the turn's entry.
- `POST /fuel/preview {"items":[Item...], "day"?}` (the app token or the agent token) writes nothing and journals nothing: the answer has the validated items with the values that would be stored (staple values applied), their sum, and for every budget of the day's snapshot `consumed`, `target`, `after` (consumed plus the sum) and `left_after`.

22.6 The answer. The result of the turn counts as the answer only when it is not empty and does not start with "CHECKPOINT". The line "FUEL-END <mark>" and everything after it are removed; so is everything from a line "---SUMMARY---" on. A result with another mark, or with the text of a checkpoint, fails the turn (22.7). A result with no end line is accepted (the model forgot the line; the mark is a check against a foreign result, not a format rule). The text is cleaned as in 21.5 (at most 1600 characters, digits kept). Then the entry becomes final in ONE journal line: `agent` = "done" and `agent_text`. Intent of the entry: new items = "log"; else a fix = "correct"; else only undo = "undo"; else only a move = "move"; else "question". Reply blocks: (1) the agent's text; (2) the write line by code, from the CURRENT state of the ops of the entry: "Logged: a, b and c." (with "for <day>" on another day), the correction lines of section 17, "Removed ...", "Moved ... to <day>.", "(still saving)" for an op that is not settled, "Could not save ..." for a failed one, and "Nothing was logged." when the turn wrote nothing; (3) the saturated fat line of 18.14 when the turn wrote; (4) `macros_today` with `added` = the net change of this turn on the snapshot date. The snapshot date is the date of the entry: the log date of the turn, or the day of its first items when that is another day. Items in the answer and in the feed: the new items of the entry (CURRENT state). Status: "pending_reconciliation" while the agent runs or an op is not settled, else "done". An agent entry is never "failed": a failed row is named in the write line.

22.7 Failure and recovery. Rule: a failed agent turn gives a clear text in the feed; fueld never sends a turn a second time after it may have run; the estimator is never used for an accepted agent turn.
- 404 for the session (on a photo upload or on the turn message) and no write of the turn was admitted: a new session, the photos again, the turn once more. This is the only repeat.
- Every other failure (an error status, a transport error, the timeout, an empty answer, a checkpoint text, a foreign mark): the turn is closed FIRST (no write can follow), the session is forgotten, POST /sessions/{id}/interrupt is sent (best effort; not after a 404). Then the entry is final with `agent` = "failed". With no write admitted the text is "The agent did not answer. Nothing was logged. Your message is above; send it again." (or "The agent is not reachable. Nothing was logged. ..." for a transport error). With writes admitted the reply is the write line and "The agent did not finish its answer."; the rows stay (they are in the journal, the cards show them, the user can undo them).
- A stop of fueld while a turn waits or runs: at the next start every entry with `chat` = "agent" and `agent` = "pending" becomes "failed" by the same rule (its admitted writes are in the journal and are reconciled as every op), the stored session is interrupted and forgotten. The agent is not asked again. A later recovery pass does the same for a pending entry with no running turn.
- The feed lines, the coach event and the stored final response of a final entry are made by the recovery of section 14 when the request could not make them.

22.8 Timing and the wire. As 21.7: the response shape of POST /fuel/log is unchanged; the request waits at most 75 s (never past 100 s after the body was read), then answers 202 "pending_reconciliation" with the text "Working on it. The answer appears here in a moment."; GET /fuel/entry/{id} is 202 while the entry is pending and 200 with the blocks of 22.6 when it is final. The log slot and the `client_id` claim are released before the wait. A repeat of the `client_id` replays the state (202 or the stored final answer) and never starts a turn.

22.9 Second opinion. No recalibration job (section 16) is made for an entry of the agent chat: the second opinion runs on the same daemon and the same model. Jobs of estimator entries are not changed.

22.10 Logging. One line per agent turn: the entry id, the outcome class, the photo count, the write count, the milliseconds. Never the user's text, the answer, the capability or a token. The turn message is stored by the agent daemon in its session record and run log (21.9); the capability in it is closed when the turn ends.

22.11 Limits and what was dropped from the design (accepted by Joe on 2026-10-02: "go, ignore the risk"). (a) The tool gate (a PreToolUse hook in the workspace that allows only fuel-op, Read and the two wiki read tools) is installed by install.sh and was proved by the probe, but it is a Claude Code hook, not a sandbox, and it is not a precondition. (b) Dropped: the brief with its number and process tag, `expect_ops`, `seen_version` (a fix is computed from the authoritative rows under the item lock, as every fix of section 14), the second attempt after an uncertain turn, the journal states `ready`, `running`, `revoked` (the entry line and the in-memory turn replace them), the session count limit, the instructions version check, the evaluation repair tool, a third unix user, the second opinion by codex. (c) The duplicate guard is a name heuristic; a renamed food passes it. (d) The agent's macro values are an estimate; totals, budgets and widgets are code.

Tests.
- T24 (a fake agentd, the fake model counts calls and must stay at zero): a text turn goes to the agent with the workspace, the model and a title with no colon, the message holds the capability, the mark and the day state and no token; a fuel-op style POST /fuel/items with the capability and the agent token adds the items to the turn's entry (one entry, one user line, one reply line, intent "log", `added` in the widget, no entry of its own); a photo turn uploads each photo through the media route and names every path in the message; fix, undo, move and relog in a turn bind to the entry and the reply has the write line; the agent token is refused on every route outside 22.5 and on a write with no capability; a capability of a closed turn, an unknown one and one with the other prefix are 409 and write nothing; a write that arrives after the close is refused; a repeat of a `client_id` in the turn writes nothing more; the same item name twice in a turn is refused; `likely_duplicate` and `new`; a second fix of an item in a turn is refused; "for yesterday" (`day`) writes on yesterday; each failure class of 22.7 gives the clear text, no second turn, a new session for the next turn; a failure after a write keeps the rows and names them; a 404 repeats once; a turn longer than the wait is 202, then final through GET /fuel/entry; a repeat of the client_id never starts a turn; a restart while pending gives "failed" with the message in the feed; the off file and the default backend use the estimator; the clinical words are not sent; POST /fuel/preview writes nothing; no log line holds the text, the answer, the capability or a token; the buttons work with the agent down.
