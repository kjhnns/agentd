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
  `error`), one `result` per turn. **This is the proven vertical (see below).**

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
  backoff, server-enforced chat-id allowlist, `sendMessage`. No tg-bridge reuse,
  no SSE, no MCP proxy, no enabledPlugins flag.
- **Session Manager** (`internal/session`): create/list/get/status/interrupt/
  teardown; routes Telegram inbound -> Claude Code turn -> reply back to Telegram.
- **Local API** (`internal/api`): bearer-gated HTTP on `127.0.0.1`. `GET /health`
  returns BOTH `transport_ok` and per-session `agent_ok` as distinct signals (the
  lesson from failure class 2). `GET/POST /sessions`, `POST /sessions/:id/input`,
  `GET /sessions/:id/events` (WebSocket), `DELETE /sessions/:id`,
  `POST /sessions/:id/interrupt`.

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
- Thin server-owned context (a per-session handoff/record) is a lightweight future
  add; we deliberately do NOT try to out-memory Claude Code.

## What is actually proven vs stubbed

- **Proven working:** the Claude Code PERSISTENT streaming vertical. A live gated
  test (`TestClaudePersistentContinuity`, `AGENTD_LIVE_CLAUDE=1`) opens ONE session
  and sends two turns: turn 1 sets a codeword, turn 2 recalls HELIOTROPE, proving
  the same process retained context across turns (not a cold start per message).
  `go run ./cmd/agentd chat` demos the same 2-turn persistent exchange. A
  fixture-based multi-turn parse test (`TestParseMultiTurnFixture`, recorded real
  transcript) proves the plumbing without a live call. Config parse, event bus,
  run-log durability, the API `/health` two-signal contract, and the Telegram
  allowlist + send (against a mock server) are unit-tested.
- **Compiles + unit-tested, NOT exercised against live infra:** the Telegram
  adapter against a real bot (needs a `TG_BOT_TOKEN`); the full end-to-end
  Telegram <-> Claude round-trip is wired in `session.RouteInbound` but has not been
  run against a live bot in this scaffold.

## Run it

```sh
# build + test
go build ./...
go test ./...

# prove the persistent multi-turn vertical (needs `claude` on PATH):
go run ./cmd/agentd chat     # 2 turns on ONE process; turn 2 recalls the codeword

# one-shot mapping smoke test:
go run ./cmd/agentd smoke -prompt "Reply with exactly the word PONG and nothing else"

# the live continuity test:
AGENTD_LIVE_CLAUDE=1 go test ./internal/harness/claudecode/ -run Continuity -v

# run the daemon:
cp config.example.toml config.toml   # edit bearer + allowlist
export TG_BOT_TOKEN=123456:your-bot-token   # optional; channel skipped if absent
go run ./cmd/agentd serve -config config.toml

# health (two signals):
curl -s -H "Authorization: Bearer change-me-bearer" http://127.0.0.1:8787/health
```

## Design doc

The full architecture, the five documented failure classes it fixes, the migration
map, and the phased plan live in the design doc (local wiki, not published because
it describes auth-token paths and chat ids):
`wiki/pages/agent-server-design.md` in the clawd workspace.

## License

MIT. See `LICENSE`.
