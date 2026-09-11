# agentd (spoke of Joe's agent workspace)

Knowledge: /Users/johannes/clawd/wiki/pages/agentd.md
Durable learnings go to that wiki page (dated), never into this file.

## What this is
Go daemon (module github.com/kjhnns/agentd) that wraps CLI coding harnesses
(Claude Code today) behind Telegram, WhatsApp, a local web UI and an HTTP API.
Deploy target: Joe's Mac, LaunchDaemon com.joe_pa.agentd, runtime home
~/.agentd/, listening on 127.0.0.1:8788. This checkout is source only.

## Layout
cmd/agentd/           the single binary (serve, run, chat, smoke, memory, init-workspace)
internal/             config, eventbus, runlog, harness/claudecode, channel/{telegram,web,whatsapp},
                      session, scheduler, notify, media, api, cli, workspace
config.example.toml   the only config committed; the real config.toml is gitignored

## Commands
build: go build ./...   (deploy binary: go build -o agentd ./cmd/agentd)
test: go test ./...   (live tests need AGENTD_LIVE_CLAUDE=1, see README "Run it")
run: go run ./cmd/agentd serve -config config.toml
deploy: copy the built binary to ~/.agentd/bin/agentd, then
        sudo launchctl kickstart -k system/com.joe_pa.agentd (details on the wiki page)

## Rules
- Commit only in this repo. Never commit secrets; secrets come from `pass show <path>`.
- Never run the daemon from ~/Documents under launchd: macOS TCC wedges the exec. Runtime lives in ~/.agentd/.
- Port is 8788 on this Mac, not the example's 8787 (that one is taken by the clawd dashboard).
- "OAuth session expired" is the claude CLI keychain tombstone, not an agentd bug; never delete ~/.claude/.credentials.json.
