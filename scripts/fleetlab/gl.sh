#!/usr/bin/env bash
# shared helpers for the C17 gate scripts
LAB=${FLEETLAB_DIR:-/tmp/fleetlab}
BIN=$LAB/bin/vibe
export XDG_CONFIG_HOME=$LAB/etc
export XDG_STATE_HOME=$LAB/state/fleetd
export XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
export VIBE_API=http://127.0.0.1:9721
export VIBE_TOKEN=$(cat "$LAB/state/fleetd/vibe/token" 2>/dev/null)

state() { curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/state"; }
mcp() { # $1 tool, $2 json args
  curl -fsS -m 300 -H "Authorization: Bearer $VIBE_TOKEN" -H 'Content-Type: application/json' \
    -X POST "$VIBE_API/mcp" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":${2:-\{\}}}}"
}
mcptext() { mcp "$@" | jq -r '.result.content[0].text // .error.message // .'; }
ts() { date -Is; }
hr() { printf '\n=== %s  %s ===\n' "$(date -Is)" "$*"; }
