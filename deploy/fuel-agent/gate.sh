#!/bin/bash
# Tool gate of a Fuel workspace on agentd-safe: a PreToolUse hook of Claude Code
# (docs/specs/2026-10-fuel-api.md, section 22.11). It allows fuel-op, Read of
# a photo or of the workspace, the two wiki read tools and the checkpoint
# write of context.md. Everything else is refused (exit 2). It is a hook, not
# a sandbox. Every error refuses.
W=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd) || exit 2
MEDIA=/home/agentd/agentd-workspace/main/work/media
in=$(cat) || exit 2
tool=$(jq -r '.tool_name // empty' <<<"$in" 2>/dev/null) || exit 2
[ -n "$tool" ] || { echo "fuel gate: unreadable tool call" >&2; exit 2; }
deny() { echo "fuel gate: $tool is not allowed in this workspace ($1)" >&2; exit 2; }
under() { case "$1" in "$2"|"$2"/*) return 0 ;; esac; return 1; }
case "$tool" in
  ToolSearch|mcp__memory__wiki_search|mcp__memory__wiki_read) exit 0 ;;
  Bash)
    q="'"
    jq -e --arg q "$q" '(.tool_input.run_in_background // false) == false
           and (.tool_input.command | type == "string")
           and (.tool_input.command | contains("\n") | not)
           and (.tool_input.command | test("^(/home/agentd/\\.local/bin/)?fuel-op( +([A-Za-z0-9_.,:=@%+/-]+|" + $q + "[^" + $q + "]*" + $q + "))* *$"))' \
      <<<"$in" >/dev/null 2>&1 && exit 0
    deny "only one fuel-op command, plain words or single-quoted arguments" ;;
  Read)
    p=$(jq -r '.tool_input.file_path // empty' <<<"$in" 2>/dev/null) || exit 2
    [ -n "$p" ] || deny "no path"
    r=$(realpath -m -- "$p" 2>/dev/null) || exit 2
    under "$r" "$W/.claude" && deny "not this path"
    under "$r" "$MEDIA" && exit 0
    under "$r" "$W" && exit 0
    deny "only photo paths of a turn and files of this workspace" ;;
  Write|Edit)
    p=$(jq -r '.tool_input.file_path // empty' <<<"$in" 2>/dev/null) || exit 2
    r=$(realpath -m -- "$p" 2>/dev/null) || exit 2
    [ "$r" = "$W/context.md" ] && exit 0
    deny "only context.md, at a checkpoint" ;;
esac
deny "tool not on the list"
