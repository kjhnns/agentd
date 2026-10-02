#!/bin/bash
# Installs a Fuel workspace on agentd-safe (unix user agentd) and the fuel-op
# binary, from this repo. Run as johannes on the box; it uses sudo for the
# files of user agentd. It does not restart agentd and does not touch fueld.
#
#   deploy/fuel-agent/install.sh <workspace> <fueld url> [pass entry of the agent token]
#   deploy/fuel-agent/install.sh fuel-e2e http://100.120.65.8:8797
#   deploy/fuel-agent/install.sh fuel     http://100.120.65.8:8796
set -euo pipefail
WS=${1:?workspace name, for example fuel or fuel-e2e}
URL=${2:?the fueld URL, for example http://100.120.65.8:8796}
ENTRY=${3:-fuel/agent-token}
case "$WS" in *[!a-z0-9-]*|main) echo "bad workspace name" >&2; exit 1 ;; esac
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
W=/home/agentd/agentd-workspace/$WS
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
chmod 755 "$TMP"

(cd "$REPO" && go build -o "$TMP/fuel-op" ./cmd/fuel-op)
printf 'url = %s\ntoken_entry = %s\n' "$URL" "$ENTRY" > "$TMP/fuel-op.conf"
printf '{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"%s/.claude/gate.sh"}]}]}}\n' "$W" > "$TMP/settings.json"
printf '# Memory index\n\nThis workspace has no memory pages. Shared knowledge: the wiki, read with the memory tools wiki_search and wiki_read.\n' > "$TMP/INDEX.md"

sudo -u agentd -H mkdir -p /home/agentd/.local/bin "$W/instructions" "$W/memory/pages" "$W/work" "$W/.claude"
sudo install -o agentd -g agentd -m 755 "$TMP/fuel-op" /home/agentd/.local/bin/fuel-op
sudo install -o agentd -g agentd -m 644 "$HERE/AGENTS.md" "$W/instructions/AGENTS.md"
sudo install -o agentd -g agentd -m 755 "$HERE/gate.sh" "$W/.claude/gate.sh"
sudo install -o agentd -g agentd -m 644 "$TMP/settings.json" "$W/.claude/settings.json"
sudo install -o agentd -g agentd -m 644 "$TMP/fuel-op.conf" "$W/fuel-op.conf"
sudo install -o agentd -g agentd -m 644 "$TMP/INDEX.md" "$W/memory/INDEX.md"
sudo -u agentd -H bash -c 'cd "$1" && ln -sfn instructions/AGENTS.md CLAUDE.md && ln -sfn instructions/AGENTS.md AGENTS.md && { [ -e context.md ] || : > context.md; }' _ "$W"

# The gate self-test: a hook that does not refuse is no gate.
gate() { sudo -u agentd -H "$W/.claude/gate.sh" >/dev/null 2>&1 <<<"$1"; }
ok=1
allow() { gate "$1" || { echo "gate self-test FAILED (refused): $1" >&2; ok=0; }; }
refuse() { if gate "$1"; then echo "gate self-test FAILED (allowed): $1" >&2; ok=0; fi; }
allow  '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op items --turn e_ab12 '"'"'[{\"item\":\"eggs\",\"kcal\":1}]'"'"'"}}'
allow  '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day --date 2026-10-02"}}'
allow  '{"tool_name":"Read","tool_input":{"file_path":"/home/agentd/agentd-workspace/main/work/media/web/x.jpg"}}'
allow  '{"tool_name":"mcp__memory__wiki_read","tool_input":{}}'
allow  "{\"tool_name\":\"Write\",\"tool_input\":{\"file_path\":\"$W/context.md\"}}"
refuse '{"tool_name":"Bash","tool_input":{"command":"whoami"}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day; pass ls"}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day | cat"}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day $(pass ls)"}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day\npass ls"}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/.local/bin/fuel-op day","run_in_background":true}}'
refuse '{"tool_name":"Bash","tool_input":{"command":"/home/agentd/clawd/scripts/food-log today"}}'
refuse '{"tool_name":"Read","tool_input":{"file_path":"/home/agentd/.agentd/config.toml"}}'
refuse "{\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"$W/.claude/settings.json\"}}"
refuse "{\"tool_name\":\"Read\",\"tool_input\":{\"file_path\":\"$W/../main/work/media/../../../../.agentd/config.toml\"}}"
refuse "{\"tool_name\":\"Write\",\"tool_input\":{\"file_path\":\"$W/instructions/AGENTS.md\"}}"
refuse '{"tool_name":"mcp__memory__post_turn","tool_input":{}}'
refuse '{"tool_name":"Agent","tool_input":{}}'
refuse '{"tool_name":"WebFetch","tool_input":{}}'
refuse 'not json'
[ "$ok" = 1 ] || { echo "the gate is NOT working; fix it before a session uses this workspace" >&2; exit 1; }

echo "installed workspace $WS -> $URL (token entry $ENTRY)"
echo "gate self-test: ok"
sudo -u agentd -H head -1 "$W/instructions/AGENTS.md"
echo "sha256 installed: $(sudo -u agentd -H sha256sum "$W/instructions/AGENTS.md" | cut -d' ' -f1)"
echo "sha256 source:    $(sha256sum "$HERE/AGENTS.md" | cut -d' ' -f1)"
sudo -u agentd -H test -f "/home/agentd/.password-store/$ENTRY.gpg" && echo "pass entry $ENTRY: present for user agentd" || echo "pass entry $ENTRY: MISSING for user agentd (fuel-op cannot authenticate)"
