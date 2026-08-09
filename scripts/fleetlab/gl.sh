#!/usr/bin/env bash
# shared helpers for the C17 gate scripts
LAB=${FLEETLAB_DIR:-/tmp/fleetlab}
# The SAME port table lab.sh derives, from the SAME knob. A rig that keeps
# its own copy of :9641 drives whichever lab holds the default base — which,
# once two instances can coexist, is somebody else's. Pass the pair
# (FLEETLAB_DIR + FLEETLAB_PORT_BASE) the lab was started with.
# shellcheck source=scripts/fleetlab/ports.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/ports.sh"
BIN=$LAB/bin/vibe
export XDG_CONFIG_HOME=$LAB/etc
export XDG_STATE_HOME=$LAB/state/fleetd
export XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
export VIBE_API=$FLEETD_URL

# The token, and a refusal if it is not there.
#
# `export VIBE_TOKEN=$(cat … 2>/dev/null)` could not fail: `export` reports
# its OWN status, the `2>/dev/null` swallowed the reason, and a missing file
# left VIBE_TOKEN empty. Every rig then sent `Authorization: Bearer ` at a
# live fleetd, got 401 on every call, and printed the empty/refused
# responses as its evidence — a lab that is not there reading exactly like a
# feature that did not fire. The likeliest cause is also the one this repo
# has already been bitten by: the WRONG FLEETLAB_DIR, i.e. a rig pointed at
# a lab that was never brought up (C17's blocker, from the other end).
#
# `exit` rather than `return`: gl.sh is sourced by rig scripts, most of
# which have no `set -e`, so a non-zero return would be discarded and the
# rig would carry on with an empty token — which is the defect, not the fix.
VIBE_TOKEN=$(cat "$LAB/state/fleetd/vibe/token" 2>/dev/null || true)
if [[ -z $VIBE_TOKEN ]]; then
  echo "gl.sh: no fleetd token at $LAB/state/fleetd/vibe/token — REFUSING to run a gate against a lab that is not up (every assertion below would measure a 401). Check FLEETLAB_DIR (currently $LAB) and run ./lab.sh up." >&2
  exit 1
fi
export VIBE_TOKEN

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

# render_cell CELL [STARTPORT] — re-render one cell's llama-swap config the
# way lab.sh does, INCLUDING lab.sh's verification that the startPort
# rewrite applied. `vibe router render` hardcodes startPort: 5800, which is
# production's upstream range on this box; a rig that renders and does not
# check has silently pointed a lab cell at the production ports. Every
# failure here is fatal to the rig, because everything after it would be
# measuring the wrong process.
# STARTPORT defaults to the cell's derived upstream port, so a rig no
# longer has to repeat a constant that moves with FLEETLAB_PORT_BASE.
render_cell() {
  local cell=$1 sport=${2:-$(cell_sport "$1")} out=$LAB/cells/$1/config.yaml
  "$BIN" router render --cell "$cell" --extras "$LAB/cells/$cell/extras.yaml" \
    --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" --out "$out" >/dev/null 2>&1 ||
    { echo "render $cell FAILED" >&2; return 1; }
  sed -i "s/^startPort: 5800$/startPort: $sport/" "$out"
  grep -q "^startPort: $sport$" "$out" ||
    { echo "$cell: startPort rewrite did not apply — REFUSING to continue (5800 is production's range)" >&2; return 1; }
}
