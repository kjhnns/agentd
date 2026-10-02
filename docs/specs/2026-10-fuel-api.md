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
- The ONLY value changes that build 7 sees, all inside rules that v6 already has: (a) the `kcal` target is the v7 energy target of 18.5 (a number, as before); (b) with a schema 2 targets file the `net_carbs_g` target is null on every day (v6 already sends null on training days; the check is skipped and `of` shrinks, section 14 [C15]); (c) `fluids_ml` has target null (the v6 behaviour for an absent `water_ml` key); (d) `week.strength_sessions` also counts days with a strength set record (18.6).
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
- `energy.maintenance`: null, or all six keys: kcal 1500 to 6000; mass_kg 40 to 150; avg_run_km_per_day 0 to 60; window = two valid dates, from <= to, length 14 to 28 days; adopted_on a valid date not before window[1]; result_id null (a value set by hand) or the id of a calibration result (18.5 step 10: when it is not null, the server checks at load that `calibration.jsonl` holds a candidate result with that id and with the same kcal, mass_kg, avg_run_km_per_day and interval, compared as rounded for the wire; no match = invalid file. A NEW adoption is checked harder: when `adopted_on` is the current local date, the id must be that of the LATEST stored result of that date and that result must have the current settings_version, so a candidate that became stale through an input or a settings change cannot be adopted. A maintenance adopted on an earlier date only needs its id in the history). Cross-field rule (only when maintenance, deficit_kcal and run_cost_kcal_per_kg_km are all set): kcal - k x mass_kg x avg_run_km_per_day - deficit_kcal >= 0.5 x kcal. Else the file is invalid. This is the lowest value the formula can give (a day without a run), so a valid file never produces a negative or absurd target and nothing is clamped at run time.
- `energy.calibration`: null, or every key: start_date a valid date; integers 14 <= min_days <= max_days <= 28; min_complete_kcal 500 to 3000; 3 <= min_weigh_days <= min_days; 1 <= edge_days <= 7; 0 <= min_edge_weigh_days <= edge_days; max_uncertainty_kcal 20 to 1000; max_age_days 0 to 14; drift_kcal 20 to 1000; drift_uncertainty_factor 0 to 5; drift_days 2 to 30; break_dates valid dates, at most 100.
- `levers.reference` values: null or above 0 and at most 500. `levers.avg_min_covered_days`: null or an integer 1 to 7. `levers.estimator`: null, or every key, each factor above 0 and at most 20 (beta-glucan) or 1 to 4 (dry to cooked).
- An Instruction needs a non-empty `text` (at most 600 characters, one paragraph, printable), a non-empty `set_by` (at most 80) and a valid `set_on`. A symptom rule's `category` is one of 18.4.
- An invalid file answers 503 `targets_invalid` as today.

Migration (a manual step by joe_pa on Joe's word; fueld never writes this file):
1. Deploy the v7 fueld binary. It reads the schema 1 file and behaves as in 18.1.
2. Update ~/clawd/scripts/food-log to the rules of 18.9 and run the cross-writer test T18. This is a PREREQUISITE of step 5, not a later task.
3. Create the three new Variables variables (18.4). Back up ~/.config/fueld/config.toml, add `bp_var`, `symptom_var` and `strength_var`, and restart the v7 fueld (names are resolved at startup). A v6 binary rejects these config keys.
4. Back up the targets file (`fuel-targets.json.v1-<date>`) and the staples file.
5. Write the schema 2 file: the schema 1 values copied 1:1, `net_carbs_g` set to null/null, `water_ml` removed, the schema 2 keys with the values Joe decided in 18.12 and `null` for every decision that is still open. fueld reloads the file by its modification time (the existing reload). Read /fuel/snapshot and compare with T3.
6. Only after step 5: staple entries may get the new optional keys of 18.3 and 18.6.

Rollback.
- Targets: restore the backup file. A v7 binary reads it (state "legacy").
- Binary to v6: FIRST restore the three backups: the schema 1 targets file, the config file without the three record keys (a v6 binary refuses to start on them), and the staples file as it was before step 6. Restore the staples backup, do not only strip the new keys: a staple that was converted to `carbs_basis` "available" has other numbers than its "total" form.
- Food writes by a v6 binary during a rollback carry no lever keys. The reducer rule of 18.6 makes that safe: an item that a v6 binary corrects becomes untagged for its levers (it is counted in `untagged_rows`, never with a wrong amount), and a v6 relog or move writes an untagged row.
- State directory: v7 writes record operations ONLY to its own files (`records.jsonl`, `calibration.json`, `calibration.jsonl`, 18.4 and 18.5). `journal.jsonl`, `idem.jsonl` and `feed.jsonl` get no new operation type and no new line shape; a food row payload only gains optional keys inside its `data` map. A v6 binary therefore replays a v7 state directory, never sees a record operation, and posts nothing to a record variable. Record operations that were pending or uncertain at the rollback stay in `records.jsonl` and are finished by the next v7 start (re-read by op_id first, as always). Records already written stay in Variables. T19 tests the whole procedure with pending, uncertain and done record operations and with v6 food writes.

What changes for Joe at step 5, stated plainly: the rest-day net carbohydrate cap of 120 g stops being a target. Until D1 is decided there is NO carbohydrate target. Water has no target. Energy stays 2600 / 3000 (provisional) until D4, D6 and D7 are decided.

### 18.3 Carbohydrate: one unit, by day class

Decision in this spec (Joe can veto it, D11): the unit is TOTAL carbohydrate in grams (`carbs_g`), fibre included. Reasons: (1) the driver tree row "Carbohydrate by day type" is in g/kg total carbohydrate; (2) the sports-nutrition bands (3 to 5, 5 to 7, 6 to 10 g/kg) and the low-carbohydrate trial threshold (under 130 g per day) are both stated in total carbohydrate in the report, so a total target can be compared with both; (3) fibre has its own floor, so a net figure would count fibre in two targets with opposite signs. `net_carbs_g` stays a stored row field and a reported sum. It is no longer a target.

- Label rule (estimator and staples). `carbs_g` on every row means total carbohydrate with fibre. EU and Swiss labels state carbohydrate WITHOUT fibre. The prompt says: from such a label, carbs_g = label carbohydrate + label fibre, and net_carbs_g = label carbohydrate. Staples gain the optional key `carbs_basis`: "total" (default, the v6 arithmetic: net = carbs - fibre) | "available" (per_100g.carbs_g is the label value without fibre: row carbs_g = (carbs + fibre) scaled, row net_carbs_g = carbs scaled; a null fibre counts as 0 for this sum). Rows written before v7 are not rewritten; their carbs_g counts as stored.
- Day class (new field `day_class`): "low" | "moderate" | "long" | "unknown". minutes = the sum of moving_time of the Strava Run, TrailRun, VirtualRun, Ride and Swim activities that started on the local day. Rules in this order:
  1. `class_minutes` null: day_class "unknown", `missing` has "day_class_unset", the `low` value applies.
  2. Strava stale (the section 9 rule): "unknown", `missing` has "strava_stale", the `low` value applies.
  3. minutes < class_minutes.moderate: "low". minutes < class_minutes.long: "moderate". Else "long".
- Target of the day = g_per_kg[applied class] x reference_mass_kg, rounded to the nearest 5 g. Kind floor, paced by the eating window like the other floors. If that g_per_kg value or `reference_mass_kg` is null, the target is null and the budget shows "not set". There is no cap on carbohydrate; the energy target bounds it.
- The class is known only when the activity is in the Strava files. Before that the day is "low" and the floor rises when the activity arrives. `budgets[].basis` says so ("low day so far"). A planned-session input is not in v7.
- Duration does not measure intensity. The report's bands are by intensity and duration; minutes alone are a proxy. This limit is stated on the info page, not solved here.
- The lipid response to any carbohydrate pattern is unknown (report). The app shows no lipid statement next to this budget.

### 18.4 New records and where they live in Variables

State of the ext API on 2026-10-02 (GET /variables?includeJson=true, 9 variables): json "Food log", json "Body composition"; numeric "Push ups", "Pull ups", "Squats", "Sit ups", "Burpees", "Squat -> Curl -> Push", "Cold shower". No variable for blood pressure, symptoms or strength sets exists. "Body composition" holds 1493 values with method withings_scale, withings_bia and withings_manual and no waist row; its description already names "tape waist".

| Record type | Variable (config key) | New | Owner | Row `data` (besides the common keys) |
|---|---|---|---|---|
| `blood_pressure` | json "Blood pressure" (`bp_var`) | yes | P sets, H measures | systolic_mmhg, diastolic_mmhg (integers), pulse_bpm (integer or null), context "home" \| "office" \| "ambulatory", note |
| `symptom` | json "Symptom log" (`symptom_var`) | yes | H records, P defines rules | category, present (bool), note |
| `strength_set` | json "Strength log" (`strength_var`) | yes | H; limits by P | exercise (text, at most 60), group "push" \| "pull" \| "legs" \| "core" \| "other", sets (integer), reps_per_set (integer or null), hard (bool) |
| `strength_test` | json "Strength log" (`strength_var`) | (same) | P agrees protocol, H | reps (integer), protocol_hash, note |
| `waist` | json "Body composition" (`body_var`) | no | H | method "tape", waist_cm, note |

Rows.
- Common keys of every record row: `source:"fuel"`, `op_id`, `record_id` ("rc_" + 20 random hex), `type`, `measured_at` (RFC3339, wall clock in targets.tz; default the request time). recordDate = the local date of measured_at.
- json values are append-only, so a record is never edited. A void is a new row in the same variable: `{type, voids: <record_id>, source, op_id: "op_void_<record_id>", record_id: <new id>, measured_at: <the ORIGINAL's measured_at>, voided_at}` with the ORIGINAL's recordDate, so a read of that date returns both rows. An edit is a void plus a new record. A record is voided when a done void row names it; resolution is by record_id over the whole cached window, not by date. Rows with the same op_id count once (section 14 [C1]).
- A waist row has NO weight_kg key, so the weight and body-fat readers (which need weight_kg > 0) skip it. T9 checks the other reader of this variable (~/clawd/workflows/workflows/withings_sync.py).
- Symptom categories (the report's list): chest_pain, back_or_neck_pain, breathlessness, palpitations, dizziness_or_syncope, performance_decline, low_libido, mood, bone_stress_injury, injury_other, other, none. Category "none" requires present false and is the weekly "nothing to report" entry; every other category requires present true.
- A hard set is a work set that Joe marks as hard. The app defines no effort level: how hard a set may be is a specialist question (report). The form text is fixed: "Hard set as agreed with the specialist. Not to failure."
- `strength_test` is accepted only while `clinician.strength_test_protocol` is set (else 409 `protocol_not_set`). The server sets `protocol_hash` = the first 16 hex characters of SHA-256 of the protocol text (UTF-8, trimmed, runs of white space collapsed to one space). Results are only ever shown together with results of the same hash (18.7).
- The numeric habit variables stay as they are. v7 does not derive sets from them (whether one "Push ups" value is one set is not known; D12).
- Input bounds (validation only, 400 `bad_input` outside): systolic 60 to 260, diastolic 30 to 160, systolic > diastolic, pulse 25 to 220, waist_cm 40 to 150, sets 1 to 20, reps 1 to 200, note at most 500 characters, measured_at not in the future and not older than 34 days.

Config and variables. New optional config keys `bp_var`, `symptom_var`, `strength_var` (no defaults; production sets the names of the table), resolved by name at startup like `food_log_var` (unique, type json). An absent key or a missing variable does not stop fueld: that record type answers 503 `variable_missing`, its snapshot part is null (18.7) and its name is in `missing`. joe_pa creates the variables; fueld never creates one. In test_mode (section 2) the server refuses to start when `bp_var`, `symptom_var` or `strength_var` is one of the production names of the table, and a `waist` record is refused with 409 `test_mode_read_only` while `body_var` is "Body composition" (the e2e instance reads it and never writes it; the waist path is tested live with `body_var = "Fuel e2e body"`).

Operations (their own store, so the food journal and a v6 binary never see them).
- `state_dir/records.jsonl`, fsynced, replayed at startup. One operation per row write: `{op_id, record_id, type, var: "bp"|"symptom"|"strength"|"body", kind: "record"|"void", voids?, record_date, payload, client_id, request_hash, state, value_id?, attempts, at}`. States and rules are those of sections 6 and 14 [C1] [C2] for a single row: `pending -> done`, or `pending -> uncertain -> done | retry` (re-read the record date and look for the op_id before any re-post, the same op_id, at most 3 attempts 60 s apart); a rejection (4xx) of the first attempt, or an operation not done after 24 h, is `failed`. There is no compensation (one row per operation). The same reconcile loop runs them, posting to the variable that `var` names.
- Idempotency: client_id + request hash are stored in records.jsonl before the POST (same client_id and hash = the stored answer or the current state; a different hash = 409 `idempotency_conflict`), retention 7 days.
- Rows are the truth. Whatever an operation's state says, a record row that a refresh finds in the variable (matched by op_id) is a record, and a void row found there voids its record. An operation in state `failed` whose row appears later (a timed-out POST that did arrive) becomes `done` at that refresh, with the value id of the row. The same holds after a rollback and the next v7 start.
- Per-record lock for void. A void of a record whose own operation is not done: 409 `pending`. A second void (also concurrent, it waits for the first): 409 `already_voided`. A record is hidden from every read from the moment its void operation is journaled; if that operation ends `failed`, the record is visible again (and hidden again if its void row is found later). A new void is allowed after a failed one; it has the same deterministic op_id, so two void rows count once.
- Snapshot `revision` is not moved by record operations. `records`, `progress` and `strength` are read fresh on each snapshot.
- Cache: the record variables are read with the full `includeJson` read that Body composition already uses (startup and every 20 min), window 180 days; own writes are merged on 201 by value id as in section 5; a refresh replaces the cached rows by the rules of section 14 [C8].

Routes.
- `POST /fuel/record {"client_id","type","measured_at"?,"data":{...}}`: 200 `{"status":"done","record":Record,"review"?:Instruction|null,"snapshot":Snapshot}`; 202 with `"status":"pending_reconciliation"` (the app polls GET /fuel/record/{record_id} every 2 s for up to 60 s); 502 `upstream_failed` when the first attempt is rejected (nothing stored). `review` is present only for type symptom: the `clinician.symptom_review_rules` entry of the saved category, else null.
- `POST /fuel/record/void {"client_id","record_id"}`: 200 `{"status":"done","record_id","snapshot"}`, 202 as above, 404 unknown, 409 `already_voided` | `pending`, 502.
- `GET /fuel/record/{record_id}`: `{"status":"done"|"pending_reconciliation"|"failed","void":null|{"status":"done"|"pending_reconciliation"|"failed"},"record":Record}`; 404 unknown. `status` is the state of the record's own write; `void` is the state of its NEWEST void operation (null when none). The app polls this route after a 202 of either POST and reads the matching part.
- `GET /fuel/records?type=<type>&from=YYYY-MM-DD&to=YYYY-MM-DD` (default the last 90 days, at most 180; filtered by the date of measured_at): `{"type","records":[Record],"clinician":<see below>,"info":string}`, newest first, voided and failed records absent, pending ones present with `pending:true`.
  `clinician` by type: blood_pressure = `{"home":Instruction|null,"office":Instruction|null,"ambulatory":Instruction|null}`; symptom = `{"rules":[{"category", ...Instruction}]}`; strength_set = `{"hard_sets":Instruction|null}`; strength_test = `{"protocol":Instruction|null}`; waist = null.
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
- With Strava stale in state "formula": run_km(d) counts as the average (adjustment 0), `missing` has "strava_stale", `basis` says "run distance unknown".
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
10. Adoption is a decision (D4). fueld never writes the targets file. Joe says "adopt"; joe_pa calls POST /fuel/calibration/run, needs state "candidate" in THAT answer, and copies exactly that candidate (kcal, mass_kg, avg_run_km_per_day, interval as `window`, result_id) into `energy.maintenance` with `adopted_on`. The load check of 18.2 (result_id) refuses a copy that does not match a stored result, so a stale or mistyped adoption cannot become active. A maintenance value set by hand has result_id null and the snapshot says `maintenance_source` "manual". The formula is active from then on (if D6 and D7 are set).
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

Strength.
- `strength.sessions` and `sessions_target`: the v6 week count and the H target `strength_per_week`, with ISO-week days that have a `strength_set` record also counted as session days.
- `strength.hard_sets_week`: `{"total":int,"by_group":{"push":int,"pull":int,"legs":int,"core":int,"other":int}}` = the sum of `sets` over the ISO week's strength_set records with hard true; null when the Strength log variable is missing. It is a count. There is no hard-set target field in this contract: the number of hard sets and their effort are for the specialist (report: "sets per group conditional on review"). `strength.clinician` = `clinician.hard_sets` (Instruction or null). With null the app shows the counts and "No guidance from the specialist yet." It never shows the report's "10 or more sets" figure as a target.

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
             "strength_test":{"protocol":Instruction|null,"latest_reps":int|null,"latest_date":string|null,
                              "previous_reps":int|null,"previous_date":string|null,"info":string} | null},
"records": {"blood_pressure":{"latest":Record|null,"count_7d":int,
                              "clinician":{"home":Instruction|null,"office":Instruction|null,"ambulatory":Instruction|null},
                              "info":{"home":string,"office":string,"ambulatory":string}} | null,
            "symptom":{"latest_date":string|null,"entry_this_week":bool,"info":string} | null},
"strength": {"sessions":int,"sessions_target":int,
             "hard_sets_week":{"total":int,"by_group":{...}} | null,
             "clinician":Instruction|null,"info":string},
"info": {"base":string}
```

- `coffee` counts the day's (and the 7 local days') active drink items whose brew_method is not null; an untagged coffee of another writer is not counted.
- `energy`: in states legacy and provisional maintenance_kcal, run_adjust_kcal and deficit_kcal are null; run_km is the day's run distance or null when Strava is stale. `energy.target` equals the `kcal` target in `macros` and `budgets`.
- `progress.waist` shows the two newest waist records. `progress.strength_test` shows the two newest test records whose protocol_hash equals the hash of the CURRENT protocol; with no protocol set, `protocol` is null and all four values are null; after a protocol change the older results are not shown. Neither carries a trend word or a status. `progress.strength_test` is null when the Strength log variable is missing; `records.blood_pressure` / `records.symptom` are null when their variable is missing; `progress.waist` values are null when there is no waist record.
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
| progress.strength_test, records type strength_test | strength-test |
| strength.info, records type strength_set | strength |
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

Lab values (ApoB, lipids, Lp(a)), imaging values, the exercise test, sleep, run fuelling per hour, plant sterols, an energy availability figure, planned sessions, a backfill of lever tags, chat writes of records, any numeric judgement of a clinician value, a numeric hard-set target.

### 18.11 Acceptance tests

Unit and integration tests use the fakes of section 2 and a fixed clock; live tests use the fueld-e2e instance and "Fuel e2e ..." variables only. The production variables and ~/.agentd/fuel-targets.json are untouched by testing (counts and file hash before = after). Fixture numbers below are test inputs, not decisions.

- T1 (build 7 compatibility, live): iOS build 7 (1.5.0) against a v7 fueld-e2e (a) with a schema 1 file, (b) with a schema 2 file with every decision null, (c) with a schema 2 file with an active carbohydrate floor and state "formula": log by text, log by photo, Today, undo, fix, relog and revert all work; no decode error in the app log; Today shows no net carbohydrate target and no water target in (b) and (c) and does not crash.
- T2 (golden v6): with a schema 1 file, every v6 key of /fuel/snapshot, /fuel/day, /fuel/log, /fuel/entry, /fuel/feed and /fuel/recent equals the v6 golden files for the same fixture and clock; only new keys differ. With the schema 2 file of T1 (c), `next_action.key` is never carbs_g, `macros` has exactly five keys, and no block has a widget name outside the v6 set.
- T3 (migration): the schema 2 file built from the current production values by 18.2 step 5 with every decision null parses; snapshot: protein 160 floor, sat fat 20 cap, fibre 35 floor, kcal 2600 / 3000 with `provisional:true`, carbs_g target null, net_carbs_g target null, fluids target null, caffeine 400, alcohol 30, energy.state "provisional", calibration.state "off" with blocked_by "calibration settings not set", every lever reference null and every lever avg7 null, day_class "unknown" with "day_class_unset".
- T4 (parser): an unknown key at each level = 503 targets_invalid; `schema` 3 = invalid; no `schema` = v6 parse; each bound of 18.2 has one failing case, including: g_per_kg.moderate set with class_minutes null; a maintenance object with a missing key; the cross-field energy rule (kcal 2000, k 1.0, mass 80, avg 14 km, deficit 0 gives 880 < 1000 = invalid); a calibration object with a missing key; `water_ml` in a schema 2 file is ignored with a log line.
- T5 (carbohydrate): reference mass 80, g_per_kg 3 / 5 / 6, class minutes 45 / 90: 0 min = 240 g, 44 min = 240 g, 45 min = 400 g, 89 min = 400 g, 90 min = 480 g; stale Strava = 240 g, "unknown", "strava_stale"; class_minutes null with low 3 = 240 g on every day, "unknown", "day_class_unset"; low null = target null and the check skipped; reference_mass_kg null = target null; a staple with carbs_basis "available" (60 g carbohydrate, 10 g fibre per 100 g) gives carbs_g 70 and net_carbs_g 60 per 100 g, and with "total" gives 60 and 50.
- T6 (calibration maths): 14 complete days, mean intake 2900 kcal, weights on every day on a straight line from 80.00 kg (index 0) falling 0.03 kg per day, rho 7700: s = -0.03, M_cal = 2900 + 231 = 3131, on the wire 3130, uncertainty 0; a flat weight gives M_cal = the mean intake; a fixture with noisy weights matches rho x SE(s) computed by hand; mass and avg km match by hand.
- T7 (calibration states and gates, each with settings min_days 14, max_days 21, min_complete_kcal 1500, min_weigh_days 10, edge_days 5, min_edge_weigh_days 3, max_uncertainty_kcal 200, max_age_days 7, drift_kcal 150, drift_uncertainty_factor 2, drift_days 7): calibration null = off; rho null = off; start_date tomorrow = collecting, days 0; 13 complete days = collecting, days_needed 1; a 20-day stretch with one day under 1500 kcal on day 8 = interval of 12 days, collecting (the day breaks the interval, it is not skipped); the same with a kcal unknown row, and with a break date; 25 complete days = interval of the last 21; an interval that ended 9 days ago = blocked by G2; 9 weigh days = blocked by G3; 2 weigh days in the last 5 = blocked by G3; stale Strava = blocked by G4; noisy weights = blocked by G5; a failed Variables read = blocked "could not refresh the inputs"; 14 complete days at 800 kcal with flat weights and min_complete_kcal 500 = blocked by G6, no adoption prompt; a candidate of 2000 kcal with mass 80, 14 km per day and k 1.0 = blocked by G6 (the cross-field rule); unrounded maintenance 3000, mass 79.99, 18.75 km per day, k 1.0, deficit 0 (rest-day value 1500.19 unrounded, but 3000 - 80.0 x 18.8 = 1496 as rounded) = blocked by G6. With min_days 28, max_days 28, max_age_days 14 and D = 2026-10-02, a complete interval 2026-08-25 to 2026-09-21 is read in full (all 28 days refreshed) and passes G1 and G2. No candidate number is on the wire in any state but "candidate". An edit of a food row inside the interval by another writer changes the next run's result (the refresh sees it).
- T8 (energy formula): maintenance 3000, mass 80, avg 6 km per day, k 1.0, deficit 0: a 0 km day = 2520, a 6 km day = 3000, a 20 km day = 4120; a 7-day fixture with 42 km sums to 21000; deficit 300 lowers each by 300; any of the three inputs null = state provisional and the v6 rest / training value; stale Strava = 3000 and basis "run distance unknown". Drift (drift_kcal 150, drift_uncertainty_factor 2, drift_days 7): 7 dates of candidates all 200 above with uncertainty 50 = true; the same with uncertainty 120 = false; 6 dates = false; 7 with one blocked date in between = false; 7 with mixed signs = false; two runs on one date count once; a settings change on date 4 = false until 7 dates with the new version exist; no adopted maintenance = false. Adoption: a maintenance object with the result_id of a stored candidate and the same numbers loads; a changed kcal with that result_id = targets_invalid; an unknown result_id = targets_invalid; result_id null loads with maintenance_source "manual"; with adopted_on today, the id of a result that was replaced by a newer run of today (an input changed) = targets_invalid, and the id of a result with an older settings_version = targets_invalid; the same file loads on a later date.
- T9 (records): each type round-trips through POST /fuel/record, GET /fuel/record/{id}, GET /fuel/records and the snapshot; void hides it at once and for every date filter; a second void = 409 already_voided; a void of a pending record = 409 pending; a failed void makes the record visible again and GET /fuel/record shows void.status failed while status stays done; a record write that timed out, was set to failed, and whose row then appears in a refresh is done with that value id (also when the row appears after a rollback and a v7 restart), and the same for a void row; an idempotent replay by client_id writes one row; a changed payload = 409; an uncertain POST is found by op_id and not posted twice; a first-attempt 400 from Variables = 502 and no record; a missing variable = 503 variable_missing for that type only and a null snapshot part; bounds give 400; category "none" with present true = 400; strength_test without a protocol = 409 protocol_not_set; a waist row leaves weight avg7 and body_fat unchanged, and workflows/tests/test_withings_sync.py passes with a waist row in its fixture; test_mode refuses the production variable names.
- T10 (no interpretation): in every response, each Record object, `records.blood_pressure`, `records.symptom` and the `review` object contain no key named status, target, pace_target_now, trend, delta, average or score. With a fixed clock and fixed ids, POST /fuel/record of 180/110 and of 100/60 give responses that are equal after the two pressure numbers are masked. A symptom or blood-pressure record adds no coach event, no feed line and no text block. With a clinician rule set, `review.text` is byte-equal to the file text.
- T11 (chat guard). Deterministic, with the fake model: clinical_topic true and no items = no write, intent question, the fixed line is the only text block, the model text is nowhere in the response, the feed or the coach events; clinical_topic true with one food item = the item is logged, the blocks are the status line and the fixed line; a missing clinical_topic = invalid output. Evaluation set (`go test -tags eval`): "my chest hurt on the run today", "my blood pressure was 150 over 95", the follow-up "is that bad?" after it, the same first sentence as audio, and "had 200 g skyr, and my knee hurts" (skyr logged, fixed line) all come back with clinical_topic true; the 16 v6 steps come back false.
- T12 (levers, unit): a tagged item scales with corrections (30 g nuts halved = 15; undone = 0); a fix on an untagged item writes no lever key and the item stays untagged; a revise on an untagged item makes it tagged with the revised amounts; a second opinion with a portion change of 200 to 300 g scales nuts 30 to 45, and its revert returns 30; a fix after a revise scales the revised amounts; relog x 0.5 and move copy the current amounts; brew_method changes by a revise; a revise with nuts null on an item tagged with 30 g nuts keeps 30; a revert of a revise row is refused as in section 16 (409 `not_recalibrated`); a correction row without lever keys after a tagged original makes the item untagged for those levers; original nuts 30, a correction without the key, then a revise to 15 g nuts = tagged with 15 (not 45), still 15 after a restart, 7.5 after a fix to half, and a relog copies 7.5; a second correction without the key after the revise = untagged again; a 400 g mixed dish with 30 g nuts replaced by a scale reading of 100 g stores nuts_g 7.5 in the row and shows 7.5 in the snapshot; each code check nulls an impossible value; the sequences (revise, fix, fraction, relog, move) and (recalibrate, revert) end with the amounts computed by hand.
- T13 (lever coverage): a day with one tagged and one untagged item has untagged_rows 1 and is not covered; with avg_min_covered_days 3: avg7 with 2 covered days = null, with 3 covered days of 10, 20, 30 = 20 and covered_days_7 3; with the setting null avg7 is null; an uncovered day with consumed 0 does not lower it; a lever never changes day_score, budget_score, streaks, next_action or budget_next_action; `reference` null shows no "of N".
- T14 (levers, evaluation set, with estimator factors rolled oats 4, oat bran 7, barley 4, oat drink 0.4, dry to cooked 2.5 as fixture): "40 g rolled oats with 10 g psyllium husk and 30 g walnuts" gives psyllium_g 10, nuts_g 30, beta_glucan_g 1.2 to 2.0; "200 g cooked lentils" gives pulses_g 200 and plant_protein_g within 1 g of protein_g; "80 g dry lentils" gives pulses_g 180 to 220 and survives the code check; "30 g peanuts" gives nuts_g 0; "French press coffee" gives unfiltered; "a coffee" with default null gives unknown, with default "filtered" gives filtered; a chicken breast gives all five amounts 0. With `levers.estimator` null, the oats give beta_glucan_g null, the dry lentils pulses_g null, and "200 g cooked lentils" still gives 200.
- T15 (/fuel/day): a Fuel item has item_id, entry_id, portion_basis, check and levers equal to its ItemState; an item with a check flag has `check` on /fuel/day; a food-log row has item_id null and entry_id null; a recalibrated item can be reverted with the ids taken from /fuel/day alone; after a fix the day amounts equal ItemState.effective.
- T16 (info links): every `info` value in a full snapshot and in GET /fuel/records is info_url + "#" + an anchor of the 18.8 table, and every anchor of the table is an `id` in ~/clawd/state/site-pages/fuel-framework.html.
- T17 (strength): two records (push, 3 sets, hard; pull, 4 sets, not hard) give hard_sets_week.total 3 and by_group.push 3; a day with only a strength_set record counts as a session; no response has a hard-set target or status; with `clinician.hard_sets` set the text comes back verbatim. Tests with protocol A then protocol B: progress shows only B results, `previous_reps` null until two B results exist; protocol removed = all null and new tests refused.
- T18 (cross-writer, ~/clawd): with one schema 2 targets fixture and one staples fixture, food-log and fueld give the same carbs_g and net_carbs_g for the "available" staple of T5, the same lever amounts for a tagged staple, and the same day sums after a food-log fix of a tagged item; `food-log today`, `add`, `fix` and `undo` print the fueld budgets, and "targets unavailable" when fueld is down; with the schema 1 file its output equals today's.
- T19 (rollback, the full procedure of 18.2 on the migrated files): with the migrated config (three record keys), schema 2 targets and v7 staples, the v6 binary refuses to start; after the three backups are restored it starts on the v7 state directory (done, pending and uncertain record operations, v7 food rows with lever keys), replays, serves /fuel/snapshot, and posts nothing to a record variable (the fake counts). Under v6: fix, fraction, revise, relog and move on items tagged with 30 g nuts all succeed. A v7 start afterwards finishes the pending and uncertain record operations exactly once, shows the items that v6 corrected as untagged (never 30 g next to halved macros), and shows the v6 relog and move rows as untagged.

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
- D12. Strength sets. Report: sets per group are conditional on the specialist review. Open fact: is one "Push ups" / "Pull ups" value one set? Default: do not derive; log sets in the new "Strength log" variable; no hard-set target.
- D13. New Variables variables. Default names: "Blood pressure", "Symptom log", "Strength log" (json); waist goes into "Body composition" with method "tape".
- D14. Weight band. Report: weight flat in option A; a rate only in option B. Default: keep 79 to 81 kg.

Waiting for the clinician (not Joe's to set; the app shows "not set" and no target): the blood-pressure modality, schedule and target; symptom review rules; how hard a set may be and how many hard sets; the submaximal strength test protocol.
