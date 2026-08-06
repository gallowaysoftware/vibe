#!/usr/bin/env bash
# shared helpers for the C17 gate scripts
LAB=${FLEETLAB_DIR:-/tmp/fleetlab}
BIN=$LAB/bin/vibe
export XDG_CONFIG_HOME=$LAB/etc
export XDG_STATE_HOME=$LAB/state/fleetd
export XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
export VIBE_API=http://127.0.0.1:9721
export VIBE_TOKEN=$(cat "$LAB/state/fleetd/vibe/token" 2>/dev/null)

# fleetd's OWN log. $LAB/logs/fleetd.log holds only what the process wrote
# to stdout before it installed its handler, which is nothing: every slog
# line lands here instead. A rig that greps $LAB/logs/fleetd.log prints an
# empty result forever, which reads exactly like "the daemon said nothing".
DLOG=$LAB/state/fleetd/vibe/daemon.log

state() { curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/state"; }
mcp() { # $1 tool, $2 json args
  curl -fsS -m 300 -H "Authorization: Bearer $VIBE_TOKEN" -H 'Content-Type: application/json' \
    -X POST "$VIBE_API/mcp" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":${2:-\{\}}}}"
}
mcptext() { mcp "$@" | jq -r '.result.content[0].text // .error.message // .'; }
ts() { date -Is; }
hr() { printf '\n=== %s  %s ===\n' "$(date -Is)" "$*"; }

# render_cell CELL STARTPORT — re-render one cell's llama-swap config the
# way lab.sh does, INCLUDING lab.sh's verification that the startPort
# rewrite applied. `vibe router render` hardcodes startPort: 5800, which is
# production's upstream range on this box; a rig that renders and does not
# check has silently pointed a lab cell at the production ports. Every
# failure here is fatal to the rig, because everything after it would be
# measuring the wrong process.
render_cell() {
  local cell=$1 sport=$2 out=$LAB/cells/$1/config.yaml
  "$BIN" router render --cell "$cell" --extras "$LAB/cells/$cell/extras.yaml" \
    --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" --out "$out" >/dev/null 2>&1 ||
    { echo "render $cell FAILED" >&2; return 1; }
  sed -i "s/^startPort: 5800$/startPort: $sport/" "$out"
  grep -q "^startPort: $sport$" "$out" ||
    { echo "$cell: startPort rewrite did not apply — REFUSING to continue (5800 is production's range)" >&2; return 1; }
}
