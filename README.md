# agentd

> Name is a placeholder ("agentd" = agent daemon). It is renameable; nothing in
> the module path is load-bearing beyond `github.com/kjhnns/agentd`.

**Vision.** One local daemon that wraps CLI agentic coding harnesses (Claude Code
today, Codex or any other CLI tomorrow) so you can talk to your agent from
Telegram (and later a web/wrist UI) and swap the underlying harness WITHOUT
rewriting the surrounding machinery. It replaces today's brittle five-process
Telegram bridge (separate daemon + MCP proxy + runner + two watchdogs, coordinated
through a shared global settings file and macOS GUI-domain launchd agents) with
ONE supervised process, ONE config file, and NO harness-specific coordination in
the transport. Full architecture and the failure-mode analysis that motivates it:
see the design doc referenced at the bottom.

## Scope of THIS scaffold (Phase 0-1, revised/tightened)

This repo is the harness-agnostic **core**, built to the revised (not maximalist)
scope. What is IN:

- **Config** (`internal/config`): a single `config.toml` (`[server]`, `[[harness]]`,
  `[[channel]]`), parsed by a small dependency-free TOML subset. Ship
  `config.example.toml`; real `config.toml` is gitignored.
- **Event bus + normalized Event** (`internal/eventbus`): the cross-harness Event
  schema (design 3.4: `session_id, ts, source, kind, text, status?, tool?,
  confidence?`) and an in-process pub/sub.
- **Run-log** (`internal/runlog`): durable append-only JSONL, **fsync per record**,
  resume-by-replay (the durability model from clawd's `workflows/runner`). Every
  session event, inbound/outbound, and control decision is recorded.
- **Harness adapter interface** (`internal/harness`) + the **Claude Code adapter**
  (`internal/harness/claudecode`): CLAUDE-CODE-NATIVE, running a PERSISTENT,
  full-continuity streaming session. `Start` spawns ONE long-lived process per
  agentd session:
  `claude -p --input-format stream-json --output-format stream-json --verbose
  [--dangerously-skip-permissions]`. `Send` writes a user-message JSON envelope to
  its stdin and never closes stdin, so every turn runs on the SAME process with
  full conversation continuity (memory, context, tools carry across turns); a
  background reader maps stdout JSON lines to normalized Events (assistant text ->
  `output`, `tool_use` -> `tool_call`, each turn's `result` -> `result`, errors ->
  `error`), one `result` per turn. Claude's terminal `result` line repeats the
  final assistant text verbatim; the adapter BLANKS that duplicated text (the
  `result` event stays as the turn-terminal marker with its status metadata), so
  the reply reaches channels exactly once. Consumers wanting the turn's final
  reply text use the result text when present, else the turn's last `output`
  text. **This is the proven vertical (see below).**

  **Skip-permissions default.** With `skip_permissions = true` (Joe's default in
  `config.example.toml`) the adapter passes `--dangerously-skip-permissions`, so
  claude runs tools WITHOUT asking for approval: a hands-free autonomous agent.
  This disables Claude's own permission guardrail; agentd's confirm-gate is the
  intended safety layer for visible/destructive actions. Set it `false` to keep
  Claude's prompts. It is a per-`[[harness]]` config toggle.

  A one-shot `claude -p` path is retained only for the `smoke` subcommand; the real
  session path is the persistent one above.
- **Channel adapter interface** (`internal/channel`) + a fresh in-process
  **Telegram adapter** (`internal/channel/telegram`): getUpdates long-poll with
  backoff, server-enforced chat-id allowlist, `sendMessage`, and emoji progress
  reactions (see the Telegram reactions section below). No tg-bridge reuse,
  no SSE, no MCP proxy, no enabledPlugins flag.
- **Web channel** (`internal/channel/web`): a self-contained, authenticated local
  UI served by the SAME server, and the reference ChannelAdapter (see the
  dedicated section below).
- **Notification hub** (`internal/notify`): a generic, channel-agnostic wire from
  any producer of user-facing notifications (the scheduler today) to every
  notify-capable channel (see the Scheduler section's Notify discipline).
- **Workspace** (`internal/workspace`): the WORKSPACE is a first-class concept
  (design 3.5) — see the dedicated section below.
- **Session Manager** (`internal/session`): create/list/get/status/interrupt/
  teardown/**reset**; homes every session in a workspace (cwd = workspace root,
  composed system-prompt injection); routes Telegram inbound -> Claude Code turn
  -> reply back to Telegram; auto-commits workspace changes at end of turn; owns
  the warm-session lifecycle + context reset + GC sweeper (see the dedicated
  section below).
- **Local API** (`internal/api`): bearer-gated HTTP on `127.0.0.1`. `GET /health`
  returns BOTH `transport_ok` and per-session `agent_ok` as distinct signals (the
  lesson from failure class 2), plus per-session `pressure` + `pressure_source`.
  `GET/POST /sessions`, `POST /sessions/:id/input` (blocks for the turn and
  returns its `result`, so a non-streaming client can read the answer),
  `GET /sessions/:id/events` (WebSocket),
  `GET /sessions/:id/history` (conversation replay from the run-log, capped at
  the last 50 turns; `?turns=N` narrows it),
  `GET /sessions/past` (run-log-derived listing of sessions no longer live:
  id, label, first/last activity, turns, preview; most recent first, capped at
  100, `?limit=N` narrows),
  `POST /sessions/:id/continue` (start a NEW live session seeded with a compact
  transcript of a past one; continue-as-new-session, the source id is not
  resurrected), `DELETE /sessions/:id`,
  `POST /sessions/:id/interrupt`, `POST /sessions/:id/reset`. The web channel
  mounts `GET /ui`, the `/ws` WebSocket, and `POST /confirm/:token` on this same
  server via `(*Server).Mount`, behind the same bearer gate.

## Workspace: the agent's persistent home (design 3.5)

A **workspace** is the agent's persistent home directory. It holds the agent's
constitution, its long-term memory, its live handoff, and its working files —
and the harness runs **with cwd = the workspace root**, so all of it is visible
to the agent as plain files it can read and edit with its own tools.

Workspaces live under `~/.agentd/workspaces/<name>/` (override with
`[workspace] root` / `default` in config; `POST /sessions` can name one per
session). Scaffold one with `agentd init-workspace [name]`:

```
<workspace>/
  instructions/
    AGENTS.md        the root instruction set (harness-agnostic constitution)
    agents/          agent/persona definitions
    skills/          skill and workflow definitions
  CLAUDE.md          symlink/mirror of instructions/AGENTS.md, so Claude Code
                     auto-loads the constitution natively (free bonus)
  memory/
    INDEX.md         one line per page; the only memory part ever injected
    pages/<slug>.md  topic pages: frontmatter (title, hook, tags, updated)
                     + a body with [[wikilink]] cross-references
  context.md         live handoff: active threads, pending confirms,
                     decisions, blockers
  work/              scratch space for working files
  .git/              full local history of everything above
```

**Context injection ("index-in, pages-on-demand").** At session start the
Session Manager composes an injected system prompt from exactly three parts —
`instructions/AGENTS.md` + `memory/INDEX.md` + `context.md` — and passes it via
`SessionConfig.SystemPrompt` (`--append-system-prompt` on the Claude adapter).
The memory CORPUS is never injected: the index tells the agent WHAT knowledge
exists (one line per page: title + hook + link), and the agent opens
`memory/pages/<slug>.md` on demand with its file tools. This keeps the
injection token-sane while making all knowledge reachable.

**Memory is a semantic, interlinked wiki that EVOLVES, not accretes.** One
topic = one page. New information about a known topic RE-SYNTHESIZES that page
(rewrite to integrate, resolve contradictions) instead of appending, so the
wiki grows in knowledge rather than volume. Pages cross-reference each other
with `[[slug]]` wikilinks, forming a navigable graph. v1 is **agent-driven**:
the starter instruction set teaches the convention (when to create vs update,
keep INDEX current, no dangling links) and the agent writes the files; the Go
helpers + `agentd memory` CLI give it teeth:

```sh
agentd memory index  -workspace NAME    # rebuild INDEX.md from page frontmatter
agentd memory links  -workspace NAME    # list wikilinks; exit 1 + flag dangling ones
agentd memory add    -workspace NAME -slug s -title T -hook H [-body ...]
agentd memory get    -workspace NAME -slug s
```

Server-driven auto-extraction (the server mining transcripts into pages) is
explicitly DEFERRED.

**Every workspace is a git repo.** `init-workspace` runs `git init`, writes a
`.gitignore`, and makes the initial scaffold commit. From then on agentd
commits at natural boundaries (not per keystroke): after each completed harness
turn that changed tracked files (`[workspace] git_autocommit`, default true)
with a generated message naming the changed paths, and `agentd memory add`
commits page changes on their own. Because memory pages are re-synthesized in
place, **git history is how you see how the understanding of a topic evolved
over time** (`git -C <ws> log -p memory/pages/<slug>.md`). A remote is optional
and OFF by default: agentd never auto-pushes; `[workspace] remote` merely
records one for a manual, opt-in push.

## Session lifecycle + context reset (agentd owns RESET, not compaction)

Steady state is **ONE long-lived warm session per workspace** (single-active-
session policy; the Manager stays keyed by id so multi-session is a future
toggle). agentd keeps feeding turns to the same live harness process, which
manages its own context window internally. agentd does **not** compact. When the
window fills up (or a backstop trips), agentd performs a **controlled context
RESET**:

1. **Checkpoint-flush turn.** A bounded instruction is injected telling the agent
   to persist anything worth keeping (open threads, decisions, blockers, new
   durable learnings) into `context.md` and the relevant `memory/pages/*.md`,
   concisely, then stop. agentd waits for that turn so the git autocommit
   captures the artifacts.
2. **Teardown.** The harness process is stopped.
3. **Fresh, re-hydrated process.** A NEW process is started for the SAME session
   id, cwd = the workspace root, its system prompt re-composed by
   `ComposeSystemPrompt` over the now-updated artifacts.

The session identity (id/title/workspace) is continuous from agentd's side; only
the harness process (its context window) is new. **The workspace artifacts are
what make the reset lossless:** the fresh process reads the facts back from
`context.md`/memory, not from the old process's context. A live gated test
(`TestLiveLosslessResetNovember3`, `AGENTD_LIVE_CLAUDE=1`) proves this: it plants
"launch date is NOVEMBER 3", forces a reset, and the fresh (empty-context)
process answers "November 3" because the checkpoint-flush wrote it to the
artifact. If the checkpoint-flush errors or times out, agentd still tears down
and starts fresh (a wedged flush never strands the session); the incomplete
flush is logged.

**Context pressure is a harness-agnostic adapter signal** (real *or* proxy). The
`HarnessAdapter` reports a normalized `ContextPressure{ fraction 0..1, source }`
for the live session:

- **real** — the Claude Code adapter parses token usage from the stream-json
  `result`/assistant events (`input + cache_read + cache_creation + output`) and
  divides by the model context window (configurable, default 200000; a known
  model id sets its own window).
- **proxy** — adapters with no usage (a future Codex/PTY adapter) estimate
  pressure from cumulative turns / bytes exchanged / wall-clock since start
  (the largest normalized ratio). The Claude adapter uses this as a backstop
  before the first usage line; a proxy-only harness uses it throughout.

Pressure is observable via the session status and `GET /health` (`pressure` +
`pressure_source` per session).

**Reset triggers + backstops**, all config-tunable in `[session]`:

- `context_reset_pressure` (default 0.75) — reset when pressure crosses this
  after a completed turn.
- `idle_timeout` (default 30m) — a session idle this long is checkpoint-flushed
  and its process reclaimed (GC); the next inbound lazily re-hydrates a fresh one
  from the artifacts.
- `max_turns` (default 200) and `max_wallclock` (default 8h) — hard backstops
  that force a reset regardless of the pressure signal (this is what protects
  proxy-only harnesses and runaway sessions). 0 disables a backstop.
- **explicit** — `Manager.Reset` exposed as `POST /sessions/:id/reset`.
- **error/death** — the GC sweeper recovers a dead/stale process with a
  fresh-from-artifacts restart in place (always the safe fallback).

Pressure/turn/wallclock are checked at a natural boundary (after each completed
turn, in `Manager.Send`); idle/dead reclaim runs on a **GC sweeper** goroutine
(`gc_interval`, default 1m). Checks NEVER run mid-turn. An inbound that arrives
while a turn is running serializes as the next turn (a per-session turn lock),
never a parallel session.

## Scheduler / triggers: proactive jobs (internal/scheduler)

agentd is not only reactive: the **scheduler** runs continuous/scheduled jobs.
The model is deliberately thin: **a job = a triggered session turn.** A `[[job]]`
config block declares a TRIGGER, a target WORKSPACE, and a task PROMPT. When the
trigger fires, agentd creates-or-reuses the warm session homed in that workspace
and Sends the prompt as an ordinary turn; the harness does the work with its own
shell/file tools against the workspace (running whatever CLIs live there —
capabilities are CLIs in the workspace, not MCP servers), and the normal session
lifecycle handles events, **git autocommit**, context reset, and idle reclaim.
There is no parallel execution path.

**Trigger taxonomy (v1):**

- `schedule` — cron-like, dependency-free internal parser: `"@every 30m"`,
  `"@daily 07:00"`, or basic 5-field cron `"M H DOM MON DOW"` (`*`, `N`, `N-M`,
  `*/S`, lists; DOW 0-6 with 7=Sunday; standard DOM/DOW OR semantics). A job may
  set an IANA `tz` (e.g. `"Europe/Zurich"`) for `@daily`/cron; the default is the
  server's **local** timezone. `@every` is timezone-independent.
- `file` — a path watcher: fires when anything under `path` changes (poll-based
  mtime/size/count signature, default `poll = "10s"`, no fsnotify dependency;
  the first scan is the baseline, pre-existing content does not fire).
- `webhook` — `POST /hooks/:job` fires the job (gated by the same API bearer as
  the rest of the API; only webhook-kind jobs are exposed there).
- `message` — already exists: inbound channel messages drive turns via
  `session.RouteInbound`; not configured as a job.

**Serialization.** A job run reuses the workspace's live session, so the
per-session turn lock (`turnMu`) queues it behind any in-progress interactive
turn — never two harness turns concurrently in one workspace. v1 is additionally
conservative ACROSS workspaces: a single worker drains the run queue, so at most
**one job-driven harness turn runs at a time overall** (interactive turns in
other workspaces are unaffected). A fire while the same job is still
queued/running records a `skipped` run instead of stacking.

**Durability + catch-up policy.** The scheduler appends `job_fire` / `job_skip` /
`job_run` / `job_notify` records to `state/scheduler.jsonl` (append-only JSONL,
fsync per record — the same discipline as the run-log; records are mirrored into
the main run-log too) and rebuilds last-fire times + run history by replay on
startup. A restart therefore never double-fires a just-run schedule. Catch-up
policy is **skip-with-log**: occurrences missed while the server was down or
asleep (more than 5 minutes stale) are skipped and logged, not fired late; a
never-fired schedule starts counting from server start (no fire-on-startup).

**Notify discipline.** Per-job `notify` policy: `issues` (default — only non-ok
runs surface), `always`, `never`. A notify is recorded on the JobRun + the
durable log and handed to the `OnNotify` hook. That hook is now wired (in
`cmd/agentd`) to the **notification hub** (`internal/notify`): a generic,
channel-agnostic fan-out. Each channel that can surface a notification implements
a one-method `notify.Sink` (`Notify(Notification)`) and registers with the hub;
`Hub.Dispatch(n)` fans the notification out to ALL of them. So a job notification
reaches every channel WITHOUT the scheduler knowing any channel exists — the web
channel shows it in its Notifications panel, and Telegram (which also implements
`Notify`, sending to its allowlist) would deliver it when live. Telegram is still
NOT hard-wired to job results; it is just one uniform sink among others.

**Ops surface.**

```sh
agentd jobs list                 # jobs + next fire + last run (talks to the daemon)
agentd jobs run morning-triage   # fire now (manual trigger)
agentd jobs runs morning-triage  # recent run history

# API (bearer-gated like everything else):
GET  /jobs                       # list + next_fire + last_run
POST /jobs/:name/run             # fire now
GET  /jobs/:name/runs            # recent JobRun history
POST /hooks/:job                 # webhook trigger
```

See the `[[job]]` examples in `config.example.toml`.

## Web UI: the reference channel + wrist-app foundation (internal/channel/web)

The **web channel** is a self-contained, authenticated single-page UI served by
the SAME HTTP server as the rest of the API (no second port, no external assets —
the HTML/CSS/JS is embedded via `go:embed`, so it works offline and over
Tailscale). It is the **reference ChannelAdapter**: it implements the exact same
`channel.Adapter` contract as Telegram AND the `notify.Sink` capability, so the
core treats it uniformly. It is also the foundation the future **wrist/phone app**
is a client of — same routes, same WS protocol, just a native client instead of
the bundled page. It is responsive (single-column on phones), theme-aware
(light/dark, with a manual toggle), and deliberately v1-focused.

**What it shows.**

- **Session list** — every session with its status, turn count, and live context
  **pressure** bar (from `GET /sessions`, polled). Below it, a **Past sessions**
  section (from `GET /sessions/past`): sessions reclaimed by the idle GC or lost
  to a restart stay discoverable; clicking one shows its conversation read-only
  (history replay, markdown + dedupe applied), and **Continue** resumes it as a
  NEW seeded live session titled `continued:<source id>`.
- **Conversation / event view** — the active session's normalized events streamed
  live over the WS (output, tool calls, results, errors). On page load / session
  select the prior conversation is backfilled from `GET /sessions/:id/history`
  (replayed from the durable run-log, last 50 turns), so a browser refresh does
  not blank the pane; replayed turns render exactly like live ones, including
  the single-visible-reply dedupe.
- **Input box** — a typed message is sent over the WS and routed through the SAME
  `session.RouteInbound` path as any channel, so a web message drives the one warm
  web session exactly like a Telegram message drives its session.
- **Notifications panel** — fed by the notification hub, so scheduler job
  notifications (and any future producer) surface here in real time.
- **Confirm buttons** — Approve/Deny that `POST /confirm/:token`.
  NOTE (honest status): the UI + endpoint are wired end to end, but agentd does
  not yet EMIT confirm-gate tokens from the harness (no `needs_input` event
  carries a token today), so there is no live gate to resolve yet. When the
  confirm-gate plumbing lands, only the resolution target changes.

**Inbound vs outbound.** Inbound (typed messages) become a normalized
`channel.InboundMsg` and drive a turn via `RouteInbound` (warm web session, reused
per web user). Outbound agent events stream back over the WS from the shared event
bus; the turn's final reply is also pushed as a `reply` message; notifications
push over the same socket via the channel's `Notify`.

**Auth flow.** The UI reuses the existing API bearer (`api_bearer`). Because a
browser cannot set an `Authorization` header on navigation or a WebSocket
handshake, the same bearer is ALSO accepted as a `?token=` query param or an
`agentd_token` cookie (`(*Server).authed`). The flow: open
`http://127.0.0.1:8787/ui?token=<api_bearer>` once; the `/ui` handler stores the
token as a same-origin **httponly** cookie, which then authenticates the page, its
JSON calls, and the WS handshake. The token is stripped from the URL after load.
A bare `/ui` with no token/cookie returns 401 (so the surface is never
unauthenticated). The server binds `127.0.0.1` by default and is reachable over
Tailscale like the rest of the API. It is registered as a `[[channel]]
kind = "web"` block (default enabled; optional `title` / `path`).

```
GET  /ui                 # the SPA (sets the auth cookie from ?token= on first load)
GET  /ws                 # WebSocket: streams events + notifications, receives typed input
POST /confirm/:token     # Approve/Deny (stub: no live gate yet, see note above)
/                        # redirects to /ui
```

**ACP-readiness (cheap future-proofing).** Tool permissions are modeled as a
policy value on the adapter config (`PermissionMode: skip|prompt`) rather than a
hard-coded `--dangerously-skip-permissions` inline, so a future ACP-based adapter
can map the same policy to ACP's client-side permission handling. No ACP is built
here.

What is DEFERRED (explicit non-goals here, separate go/no-go decisions later):

- **Secrets Vault** subsystem (design 3.8) — the Telegram token comes from an env
  var / config for now.
- **Browser/CDP** subsystem (design 3.7).
- **Codex adapter** — the adapter interface is clean so a second harness is a
  drop-in, but only Claude Code is implemented (agnosticism is proven later with
  Codex).
- **Raw PTY / tmux transport** (design 3.2 universal fallback) — documented future
  option; this adapter is native stream-json only.
- **Semantic layer** (LLM classification of pane output, design 3.4) and the
  **wrist/phone UI** (design 3.5 / 5).
- **Server-driven memory extraction** — v1 memory is agent-driven (see the
  Workspace section); the server composing/injecting is built, the server
  WRITING memory from transcripts is not.

## CLI: talking to the agent from a terminal (internal/cli)

`agentd chat` and `agentd run` are **clients of the running daemon**, not a
channel. A terminal is not an inbound transport that has to be polled, so there
is deliberately no `ChannelAdapter` with a `Start`/`Inbound` loop here: the CLI
pushes a turn into an existing session over the SAME bearer-gated HTTP + WS API
the web UI uses. Everything else comes for free and stays consistent with the
other surfaces: sessions, history replay, past-session browsing, the workspace
and its memory injection, context reset, the `[session] turn_timeout` budget,
and the failure conventions. The CLI only decides WHICH session to attach to and
how to render a turn in a terminal.

```sh
# interactive REPL against the running daemon (streams events over the WS):
agentd chat

# one-shot for scripts: the answer on stdout, meaningful exit code
agentd run "what is on my calendar tomorrow"
echo "summarize this" | agentd run
agentd run -json -wait 90s "..."       # {"ok":true,"session":"...","result":"..."}
agentd run -file screenshot.png "what is this"   # core media ingest
agentd sessions                        # live sessions (find an id for -session)
```

**Session behaviour.** By default both commands attach to ONE persistent session
titled `cli:$USER`, so context carries across separate invocations exactly like a
Telegram chat does. `-new` starts a fresh one, `-session <id>` targets any live
session (including a `telegram:<chat>` one). These sessions are ordinary
sessions: they appear in `GET /sessions`, in `GET /sessions/past` once reclaimed,
and their history is readable in the web UI. If the daemon's idle GC reclaimed
the CLI session, the next invocation transparently opens a fresh one
(re-hydrated from the workspace artifacts), the same reclaim contract the
channels get.

**Progress + interrupt.** The REPL shows the terminal analogue of the Telegram
emoji chain: a `working` indicator with elapsed seconds, `needs input` when the
agent blocks on the user, then `done` or `error`. Tool calls stream to stderr,
answers to stdout. Ctrl-C interrupts the CURRENT TURN via
`POST /sessions/:id/interrupt` and keeps the session alive; Ctrl-C at an idle
prompt (or Ctrl-D, or `/exit`) quits. `/help` lists the slash commands
(`/session`, `/new`, `/reset`, `/sessions`, `/exit`).

**Exit codes** (the script contract): `0` success, `1` the turn failed, `2` the
invocation was wrong, `3` the daemon is not reachable (a sentence saying so, not
a raw dial error), `4` the turn outran `-wait`.

**Config + auth.** Resolved exactly as `agentd jobs` does, `bind` + `api_bearer`
from `config.toml`, with two additions a terminal needs: `$AGENTD_ADDR` /
`$AGENTD_TOKEN` override the file, and the config path itself is discovered
(`-config`, then `$AGENTD_CONFIG`, then `~/.agentd/config.toml`, then
`./config.toml`) so a bare `agentd run` works on a configured machine.

**The enabling change.** `POST /sessions/:id/input` now returns the turn's
`result`, not just `{"status":"sent"}` — the call already blocked for the whole
turn, so answering with an ack threw the answer away and left a non-streaming
client with no way to read it. The collect-the-result loop that made that
possible (subscribe before send, map a blanked result back onto the turn's last
streamed output, drain briefly after `Send` returns) used to exist in THREE
copies (`session.RouteInbound`, `scheduler.ManagerRunner`, and nothing at all in
the API); it is now one method, `session.Manager.SendAndCollect`, used by all
three.

## Multimodal media: voice, photos, documents (internal/media)

Media is a **CORE agentd capability, not a channel feature**. One service
(`internal/media`) owns ingestion for every channel; channel adapters do
**acquisition only** (download bytes from their provider and hand a Reader to
`media.Ingest`). The session layer renders artifact references into the
canonical turn text in exactly one place (`session.RenderInbound`), so a photo
sent over Telegram and a photo uploaded in the web UI produce the identical
turn for the model. Adapters never pre-bake marker text.

**The pipeline.**

- **Ingest** — `media.Ingest(ctx, Request{Reader, Filename, Mime, Source, ...})`
  streams the bytes to `<default workspace>/work/media/<source>/` (override with
  `media_dir`), enforces the `max_file_mb` cap, classifies the artifact
  (`image` | `audio` | `document`; video is explicitly out of scope), and
  returns an `Artifact{ID, Path, Kind, Mime, Size, Transcript, ...}`. Failures
  are TYPED (`ErrTooLarge` / `ErrUnsupported` / `ErrTranscribe`) so channels can
  reply politely instead of going silent. `work/media/` is gitignored inside the
  workspace repo so autocommit never versions binaries.
- **Voice** — audio is transcribed DURING ingest by a minimal, stdlib-only
  OpenAI Whisper client (`whisper-1`, `response_format=verbose_json`, language
  auto-detect unless `whisper_lang` is set, 300s timeout). Telegram opus `.oga`
  posts directly, no ffmpeg. On success the audio file is DELETED — the
  transcript is the artifact; on failure the file is kept. No `whisper_key` =
  voice is wired but disabled (typed failure, polite reply).
- **Session injection** — `RenderInbound` appends, per artifact:
  images -> `[The user sent an image saved at <path>. Use the Read tool to view
  it, then respond.]`; documents -> the same pointer annotated with
  name/mime/size; audio -> `[voice message, <N>s, transcribed]: <transcript>`.
- **Telegram** — the adapter consumes `voice` / `photo` (largest size) /
  `document` (+ `caption`), pre-checks the declared size against
  min(`max_file_mb`, 20 MB Bot API limit), then `getFile` + streamed download ->
  `Ingest` -> normalized `InboundMsg{Text: caption, Media: [artifact]}`. Each
  media message is handled in its own goroutine so a slow transcription never
  stalls the poll loop. Unsupported kinds (video / sticker / audio-file /
  animation) get a one-line decline; every failure produces a short reply —
  silence is never an outcome (the old adapter silently dropped all non-text).
- **Web** — the composer has an attach button (and drag-drop); the file POSTs to
  `POST /sessions/:id/media` (multipart `file` + optional `text`, same bearer /
  cookie gate as everything else), goes through the SAME `media.Ingest`, and is
  routed as an ordinary turn into that session.
- **Retention** — a sweeper deletes stored media older than `retention`
  (default 168h). Media events (ingested / transcribed / failed / swept, with
  byte counts) are recorded in the run-log.
- **Out of scope (parked):** outbound media sends (`SupportsMedia()` stays
  false), video/sticker processing, PDF text extraction.

```
POST /sessions/:id/media  # multipart upload (field "file", optional "text"):
                          # ingest -> render marker -> route one turn; returns
                          # {status, artifact} after the turn completes.
                          # 413 too large, 415 unsupported, 502 transcribe-failed,
                          # 503 media disabled, 404 unknown session, 401 no bearer.
```

Configured by the single `[media]` block (`config.example.toml`): `enabled`,
`whisper_key` (literal or `env:VAR`), `whisper_model`, `whisper_lang`,
`max_file_mb`, `retention`, `media_dir`.

## Telegram channel: emoji progress reactions

Every inbound Telegram message carries a visible progress marker on the user's
OWN message, so the state of a turn is legible at a glance without the bot
sending any chatter. This is the reaction chain ported from clawd's tg-bridge
(daemon receipt react + `tg-thinking-react` UserPromptSubmit hook +
`tg-turn-done` Stop hook), reimplemented as one in-process path:

| Stage | Emoji | Set by | When |
| --- | --- | --- | --- |
| received | 👀 | adapter (`handleUpdate`) | update passes the allowlist, before any work (media included, before download/transcription) |
| working | ⚡ | `session.RouteInbound` | a session has the prompt, turn in flight |
| needs input | 🤔 | `session.RouteInbound` | an `eventbus.KindNeedsInput` event arrives mid-turn |
| done | 👍 | `session.RouteInbound` | result collected and the reply was sent |
| error | 😱 | `session.RouteInbound` | turn failed, timed out, or the session could not be created |

Unsupported media kinds and non-allowlisted chats get NO reaction, matching
tg-bridge (the former gets a one-line text decline, the latter is silently
dropped).

Mechanics and guarantees:

- `Adapter.Ack(chatID, msgID, reaction)` calls the Bot API `setMessageReaction`.
  A one-element reaction list REPLACES the bot's previous glyph, so the chain
  reads as one changing marker rather than a pile of emoji.
- Telegram accepts reactions only from its own fixed whitelist. `✅` / `⚠️` /
  `❗` come back `REACTION_INVALID` for a bot in a private chat (verified in
  clawd on 2026-06-06), which is why "done" is 👍 and not a check mark. Every
  glyph above is on the whitelist.
- Reactions can never block or fail a turn. `Ack` enqueues onto ONE worker
  goroutine (bounded queue; a full queue drops with a log) and returns
  immediately, which also keeps the chain in lifecycle order. The HTTP call has
  its own 5s timeout, and an API rejection is logged and swallowed.
- Capturing `message_id` is what makes any of this possible: `InboundMsg` now
  carries `MsgID`, plus the provider `TS` (unix seconds) and `Sender` id.
- Config: `reactions` on the `[[channel]]` block, default `true`,
  `reactions = false` parks the whole chain (receipt included). Channels that
  cannot react (web) implement `Ack` as a no-op.

## WhatsApp channel (`internal/channel/whatsapp`)

The WhatsApp adapter opens **no WhatsApp connection of its own and needs no QR
pairing**. It drives the already-paired [`wacli`](https://wacli.sh) session
(whatsmeow, store at `~/.wacli`) as a serialized subprocess.

Why not link whatsmeow directly, which would be the natural fit for a Go server:

- The account already has exactly one linked device, owned by `wacli`. A second
  whatsmeow client on the **same device credentials is a takeover, not a second
  reader**: WhatsApp replies `connectionReplaced` (440) and the two clients
  knock each other offline in a loop. Reusing the one authenticated session is
  the only safe option, and it needs nothing from the user.
- agentd is a pure-stdlib single static binary (design 3.11). `go.mau.fi/whatsmeow`
  would drag in protobuf, libsignal and a cgo SQLite driver, which is a much
  bigger decision than one channel.

**The lock constraint shapes the design.** `wacli` guards that single session
with an *exclusive store lock*, so exactly one invocation may run at a time.
Inbound is therefore a cursor poll of wacli's **local DB** (`wacli messages list
--json --from-them --asc --after <cursor>`), which is a read and does not
contend, rather than `wacli sync --follow --webhook`: follow-mode holds the lock
and the WhatsApp connection for its entire lifetime, which would starve every
outbound send. Every invocation goes through one mutex plus `--lock-wait`.

| Concern | How |
| --- | --- |
| inbound | cursor poll, `poll` interval (default 20s); dedup by message id over a 60s window; `fromMe` never becomes a turn |
| access | DM and group policies are **independent**, each `open` / `allowlist` / `locked` (default `allowlist`), ported from Camila's `access.json` model. `allow` is the DM list and doubles as the **owner set** |
| groups | supported. `UserID` is the group JID (session key), `Sender` is the participant JID, so a group thread keeps one session while still attributing each turn |
| read-only groups | `readonly_groups` deliver inbound but refuse every visible action: no reply, no reaction, no blue ticks. Membership there alone is enough to deliver, and if a group is in both lists read-only wins |
| media | voice notes, images and documents go through `wacli media download` then `media.Ingest` with `Source: "whatsapp"`, identical treatment to Telegram. No marker text baked here; `session.RenderInbound` owns that |
| outbound | `wacli send text --json`; the returned id is the acceptance artifact. `Send` refuses any chat the policy does not permit. Visible actions are throttled to 1/s (WhatsApp bans on bursts) |
| reactions | native, via `wacli send react`. One reaction per message per sender and a new one **replaces** the previous, so the 👀 → ⚡ → 👍 / 😱 / 🤔 chain reads as one changing marker exactly like Telegram. Group reactions pass `--sender` |
| read receipts | `wacli chats mark-read`, the equivalent of Baileys `readMessages`. Queued on the **same ordered worker** as the reactions so the two receipts stay consistent |
| quote / reply-to | resolved, see below |
| health | the channel detects going **deaf**, see below |
| credentials | none in config. Auth *is* the existing wacli store; the only paths configured are `bin` and optionally `store` |

Config keys on the `[[channel]]` block: `kind = "whatsapp"`, `bin`, `store`,
`allow`, `policy_dm`, `policy_group`, `allow_groups`, `readonly_groups`, `poll`,
`sync`, `reactions`, `read_receipts`, `stale_after`, `enabled`.

### The @lid trap (read this before enabling)

`allow` entries are matched by digits, so one entry covers the bare number, the
phone JID and a device-suffixed JID. It does **NOT** cover the peer's `@lid`
JID, because a real `@lid` has **completely different digits** from the phone
number. WhatsApp routes some replies over `@lid`, so an allowlist with only the
phone number silently drops them, and the first symptom is the agent ignoring
the operator's own messages.

List both forms per person. Find the `@lid` with:

```
sqlite3 ~/.wacli/session.db 'select * from whatsmeow_lid_map'
```

The adapter logs a startup WARNING naming every `allow` entry that has no
`@lid` counterpart.

### Quote / reply-to threading

`wacli messages list --json` omits the quoted fields entirely, which is why
`ReplyTo` was empty in the first cut. The data does exist: the local DB carries
`messages.quoted_msg_id` and `messages.quoted_sender_jid` (confirmed populated
against a real store), and **`wacli messages show --json` exposes them**, in
snake_case rather than the PascalCase the other commands use. The adapter
therefore does one extra `messages show` lookup per delivered message to
populate `InboundMsg.ReplyTo`. That is a local read, so it does not contend on
the store lock, and a failed lookup logs and moves on rather than dropping the
turn: threading is a nice-to-have, the message is not.

### Detecting a channel that has gone deaf

A successful poll that returns nothing is **ambiguous**: either nobody messaged
us, or whatever refreshes wacli's local DB has died and the channel will now sit
quiet forever. That exact ambiguity is the classic silent-death failure mode; a
sibling service in this operator's fleet stayed blind for six days on it. The adapter disambiguates it by asking, on every successful poll,
how old the newest message in the store is across all chats and both directions.
If the syncer is alive that number stays bounded; if it died it grows without
limit. Past `stale_after` (default 3h) the channel reports `Health.OK = false`
and fires `OnUnhealthy` once per transition, which the server turns into a
notify-hub issue. Repeated poll failures (3 in a row) also flip it unhealthy.

### Operational note: the wacli store lock

`wacli` guards its single WhatsApp session with an **exclusive store lock**, so
exactly one wacli invocation touches a given store at a time. Three consequences
an operator should know before enabling this channel:

1. **Inbound is a poll, not a stream, on purpose.** `wacli sync --follow
   --webhook` would be the obvious live-push design and it does exist, but
   follow-mode holds the lock and the WhatsApp connection for its entire
   lifetime, which starves every outbound send. Local reads (`messages list`,
   `messages show`) do **not** contend, so the adapter polls the local DB and
   keeps the lock free for sends and reactions.
2. **`sync = false` is the safe default.** With it, the adapter only *reads*
   wacli's local DB and never syncs, so it coexists with whatever else already
   keeps that store fresh. Inbound latency is then bounded by that other job's
   cadence plus `poll`. If a host runs, say, a `wacli sync --once` from cron or
   launchd every 60 seconds, worst-case inbound latency is roughly 60s + `poll`.
3. **`sync = true` buys latency and demands exclusivity.** The adapter then runs
   `wacli sync --once` itself each tick, cutting inbound latency to about
   `poll`. Only turn it on if **nothing else syncs that store**. Two syncers do
   not degrade gracefully: they fight over the lock and both lose, with the
   loser reporting `store is locked (another wacli is running?)`. Retiring the
   other syncer first is the operator's call, and this repo does not make it.

Whatever the choice, a store that stops being refreshed is the dangerous case,
because a channel with a dead syncer looks exactly like a quiet one. That is
what `stale_after` and the health check below exist to catch.

### Prompt-injection posture

This channel has **no in-band control plane by construction**. There is no
pairing flow, no allowlist command and no approval message: access comes from
the config file, and nothing an inbound message says can change it. That
structurally removes the target Camila's Baileys channel had to defend with
prose in its MCP instructions. On top of that:

- A **non-owner in a group is a bystander**. Their message is delivered, because
  that is the point of being in a group, but it draws no visible response from
  us: no blue ticks and no lifecycle reaction on a stranger's message.
- Every visible action re-checks the policy from config at call time, so a turn
  cannot be talked into replying somewhere it should not.
- Text matching approval / allowlist-change phrasing is logged as a security
  note. That is a **detector for visibility, not the defence**; the defence is
  that there is nothing in-band to escalate to.

Out of scope, same as Telegram: outbound media (`SupportsMedia()` stays false),
video, gif and sticker processing (these get a one-line decline, never silence).

## What is actually proven vs stubbed

- **Proven working:** the Claude Code PERSISTENT streaming vertical. A live gated
  test (`TestClaudePersistentContinuity`, `AGENTD_LIVE_CLAUDE=1`) opens ONE session
  and sends two turns: turn 1 sets a codeword, turn 2 recalls HELIOTROPE, proving
  the same process retained context across turns (not a cold start per message).
  `go run ./cmd/agentd demo-chat` demos the same 2-turn persistent exchange
  (that harness-level demo used to own the `chat` name; `agentd chat` is now the
  interactive client of the RUNNING daemon). A
  fixture-based multi-turn parse test (`TestParseMultiTurnFixture`, recorded real
  transcript) proves the plumbing without a live call. Config parse, event bus,
  run-log durability, the API `/health` two-signal contract, and the Telegram
  allowlist + send (against a mock server) are unit-tested.
- **Compiles + unit-tested, NOT exercised against live infra:** the Telegram
  adapter against a real bot (needs a `TG_BOT_TOKEN`); the full end-to-end
  Telegram <-> Claude round-trip is wired in `session.RouteInbound` but has not been
  run against a live bot in this scaffold. The reaction chain is covered by
  httptest Bot API fixtures (`telegram_react_test.go`: metadata capture, the
  ordered 👀 -> ⚡ -> 👍 chain over a real `RouteInbound` turn, the needs-input
  glyph, a `REACTION_INVALID` rejection not breaking the turn, and
  `reactions = false` producing zero calls).
- **Web channel + notification hub — proven over the real HTTP/WS/scheduler
  stack:** unit + integration tests cover the notifier fan-out
  (`internal/notify`), the scheduler notify policy -> hub -> web sink path
  (issues-only suppresses a clean run; always/result delivers), the `/ui` bearer
  gate (401 without, 200 with), an authenticated Go WebSocket client round-trip
  (typed input -> `RouteInbound` -> normalized events back), a notification pushed
  over the WS, and the end-to-end money demo (`POST /jobs/:name/run` ->
  scheduler -> notifier -> web WS client) in
  `TestEndToEndSchedulerNotifyToWebClient`. The web inbound round-trip test uses a
  fake echo harness (no live claude); the browser UI itself was exercised via
  API/WS clients, not a headless browser.

## Run it

```sh
# build + test
go build ./...
go test ./...

# prove the persistent multi-turn vertical (needs `claude` on PATH):
go run ./cmd/agentd demo-chat   # 2 turns on ONE process; turn 2 recalls the codeword

# one-shot mapping smoke test:
go run ./cmd/agentd smoke -prompt "Reply with exactly the word PONG and nothing else"

# scaffold a workspace (instructions + memory wiki + handoff + git repo):
go run ./cmd/agentd init-workspace demo
go run ./cmd/agentd memory index -workspace demo
go run ./cmd/agentd memory links -workspace demo

# the live continuity test:
AGENTD_LIVE_CLAUDE=1 go test ./internal/harness/claudecode/ -run Continuity -v

# live proof that the workspace injection takes effect (ORCHID test):
AGENTD_LIVE_CLAUDE=1 go test ./internal/session/ -run LiveWorkspaceInjection -v

# THE MONEY TEST: live proof a context RESET is lossless (NOVEMBER 3 survives a
# checkpoint-flush -> teardown -> fresh re-hydrated process):
AGENTD_LIVE_CLAUDE=1 go test ./internal/session/ -run LiveLosslessReset -v -timeout 20m

# run the daemon:
cp config.example.toml config.toml   # edit bearer + allowlist
export TG_BOT_TOKEN=123456:your-bot-token   # optional; channel skipped if absent
go run ./cmd/agentd serve -config config.toml

# health (two signals):
curl -s -H "Authorization: Bearer change-me-bearer" http://127.0.0.1:8787/health

# talk to the RUNNING daemon from a terminal (see the CLI section):
go run ./cmd/agentd run "Reply with exactly the word PONG and nothing else"
go run ./cmd/agentd chat

# open the web UI (sets the auth cookie from ?token= on first load):
open "http://127.0.0.1:8787/ui?token=change-me-bearer"

# the web UI is bearer-gated: 401 without, 200 with:
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8787/ui                                  # 401
curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer change-me-bearer" \
  http://127.0.0.1:8787/ui                                                                          # 200
```

## Ops note: how this runs on Joe's Mac (2026-07-24)

Deployed IN PARALLEL with the legacy tg-bridge stack (coexistence, not
migration; tg-bridge and its watchdogs are untouched).

- **Port:** `127.0.0.1:8788`. NOT the example's 8787 — that port is taken by
  the clawd monitoring detail-dashboard on this machine.
- **Runtime home:** `~/.agentd/` — binary at `~/.agentd/bin/agentd`, real
  config at `~/.agentd/config.toml` (chmod 600), state (run-log, scheduler
  JSONL) under `~/.agentd/state/`. The repo checkout in `~/Documents/agentd`
  is source only; rebuild with `go build -o agentd ./cmd/agentd` and copy the
  binary to `~/.agentd/bin/` to deploy.
- **LaunchAgent:** label `com.joe_pa.agentd`, plist
  `~/Library/LaunchAgents/com.joe_pa.agentd.plist`, `KeepAlive true`, stdout +
  stderr to `/Users/me/clawd/logs/agentd.log`. Manage with
  `launchctl bootstrap|bootout gui/$(id -u) ...` and
  `launchctl kickstart gui/$(id -u)/com.joe_pa.agentd`.
- **Workspace:** `~/agentd-workspace/main` (its own git repo). Deliberately
  NOT `~/clawd` — the live PA workspace is never shared with a parallel setup.
  The `agentd memory` CLI resolves the workspace from `~/.agentd/config.toml`
  (`[workspace] root`/`default`), so a bare `agentd memory index` works on
  this Mac; the explicit form is
  `agentd memory index -root /Users/me/agentd-workspace -workspace main`
  (flags always win; `-config` / `$AGENTD_CONFIG` override the config path).
- **Secrets:** in `pass` — `agentd/web-token` (the API bearer),
  `agentd/telegram-token` (a dedicated bot, NOT the tg-bridge bot; two pollers
  on one token = getUpdates 409), and `openai/api-key` (the `[media]`
  `whisper_key` for voice transcription). All placed as literals in the
  chmod-600 `~/.agentd/config.toml`. Never in git; `config.toml` is gitignored.
- **Learning (macOS TCC):** launching the daemon from `~/Documents` via
  launchd WEDGES AT EXEC — the background process hangs inside dyld's `open()`
  of the binary (TCC-protected folder, kernel-side hang, not an error). Run
  launchd-managed binaries and their configs from a non-TCC path such as
  `~/.agentd/`; this is why the runtime home above exists.

## Design doc

The full architecture, the five documented failure classes it fixes, the migration
map, and the phased plan live in the design doc (local wiki, not published because
it describes auth-token paths and chat ids):
`wiki/pages/agent-server-design.md` in the clawd workspace.

## License

MIT. See `LICENSE`.
