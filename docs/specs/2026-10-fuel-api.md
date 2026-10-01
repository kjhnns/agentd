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
