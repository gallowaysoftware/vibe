#!/usr/bin/env bash
# C18 L1-L4 — `vibe model try` against a real llama-swap.
#
# The four claims this rig exists to WATCH rather than assert:
#
#   L1  applying a trial def is adopted by a real `-watch-config` llama-swap
#       within two poll intervals, so the `Live` bit is an observation.
#   L2  that adoption costs what C0 measured — the old upstream drains and a
#       generation still running at 30s is force-closed — so the sentence
#       `--now` prints is describing the real thing.
#   L3  the trial id appears in fleet_status for its cell and NEVER in the
#       rendered FRONT config, and both facts reverse on `end`.
#   L4  `end` restores the cell's llama-swap config byte-for-byte.
#
# It uses the lab's `bravo` cell: opportunistic, its own llama-swap, its own
# announcer. The incumbent is whatever def bravo already serves.
#
# PORT CONTENTION: scripts/fleetlab binds fixed ports and `down`'s sweep is
# anchored partly on a shared upstream range, so a second lab instance on one
# box is entitled to kill the first's processes (futures item 15). Set
# FLEETLAB_DIR and run this only when you own the lab.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=${CELL:-bravo}
PORT=${PORT:-9642}
SPORT=${SPORT:-6000}
INCUMBENT=${INCUMBENT:-lab-chat-b}
TRIAL=trial-c18
CFG=$LAB/cells/$CELL/config.yaml

# The candidate is a LOCAL copy of a model the lab already has: this rig is
# gating the sequence and the rollback, not HuggingFace's CDN. `try` is
# driven step by step through the same journal a real run writes.
CANDIDATE=${CANDIDATE:-$LAB/models/lab-chat-b.gguf}

export XDG_CONFIG_HOME=$LAB/etc
export XDG_STATE_HOME=$LAB/state/ann-$CELL
export XDG_RUNTIME_DIR=$LAB/run/rt-$CELL

catalog() { curl -fsS -m 10 "http://127.0.0.1:$PORT/v1/models" | jq -r '.data[].id' | sort; }
running() { curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -c '[.running[]|{model,state}]'; }
frontcfg() { cat "$LAB/cells/front/config.yaml" 2>/dev/null || cat "$LAB/front/config.yaml" 2>/dev/null; }
cellmodels() { state | jq -c ".cells[]|select(.name==\"$CELL\")|[.models[].id]"; }

hr "0. preconditions"
[[ -f $CANDIDATE ]] || { echo "no candidate weights at $CANDIDATE — set CANDIDATE=<a .gguf the lab has>"; exit 2; }
[[ -f $LAB/etc/vibe/backends/$INCUMBENT.yaml ]] || { echo "no incumbent def $INCUMBENT"; exit 2; }
echo "# incumbent def:"; cat "$LAB/etc/vibe/backends/$INCUMBENT.yaml"
cp "$CFG" /tmp/c18-pretrial-config.yaml
echo "# banked the pre-trial config at /tmp/c18-pretrial-config.yaml (L4's reference)"
echo "# catalog before: $(catalog | tr '\n' ' ')"

hr "1. plan + fetch + stage (--dry-run first: nothing must change)"
"$BIN" model try file://local --file "$(basename "$CANDIDATE")" --like "$INCUMBENT" --as "$TRIAL" \
  --dest "$(dirname "$CANDIDATE")" --dry-run --skip-disk-check
echo "# config unchanged after --dry-run: $(cmp -s "$CFG" /tmp/c18-pretrial-config.yaml && echo yes || echo NO)"
echo
echo "# NOTE: the fetch is a real HuggingFace pull. To gate the sequence without one,"
echo "#       point --dest at a directory that ALREADY holds the file under the same"
echo "#       basename: hfdownload returns immediately for a present, correctly-sized file."

hr "2. L1 — apply, and watch a real -watch-config adopt it"
BEFORE=$(date +%s)
"$BIN" model try file://local --file "$(basename "$CANDIDATE")" --like "$INCUMBENT" --as "$TRIAL" \
  --dest "$(dirname "$CANDIDATE")" --skip-disk-check --now
echo "# elapsed to adoption: $(( $(date +%s) - BEFORE ))s  (C0 measured the -watch-config poll at 2s)"
echo "# catalog after:  $(catalog | tr '\n' ' ')"
echo "# journal:"; "$BIN" model try status

hr "3. L3 — visible on the CELL, absent from the FRONT"
echo "# fleet_status models for $CELL: $(cellmodels)"
echo "# does the rendered front config mention $TRIAL? $(frontcfg | grep -c "$TRIAL") occurrence(s)  <- MUST be 0"
echo "# front render warnings (the exclusion must be LOUD):"
"$BIN" router render --cell front --stdout 2>&1 >/dev/null | grep -i trial || echo "  (none — if a trial def is in fleetd's checkout this is a FAILURE)"

hr "4. L2 — the cost of a reload, watched"
echo "# start a long generation against the incumbent, then re-render mid-stream."
echo "# C0's measured behaviour: the new config is live immediately, the OLD upstream"
echo "# drains in-flight streams under a hardcoded 30s, then force-closes (clean EOF)."
( curl -sS -m 120 -N "http://127.0.0.1:$PORT/v1/chat/completions" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$INCUMBENT\",\"stream\":true,\"max_tokens\":2000,\"messages\":[{\"role\":\"user\",\"content\":\"Count slowly to two hundred, one number per line.\"}]}" \
    | tail -c 400; echo "  <- stream ended at $(date -Is)" ) &
STREAM=$!
sleep 5
echo "# touching the config at $(date -Is)"
touch "$CFG"
wait $STREAM
echo "# running after the reload: $(running)"

hr "5. L4 — end, and compare the config byte-for-byte"
"$BIN" model try end
echo "# config restored byte-for-byte: $(cmp -s "$CFG" /tmp/c18-pretrial-config.yaml && echo YES || echo NO)"
cmp -s "$CFG" /tmp/c18-pretrial-config.yaml || diff -u /tmp/c18-pretrial-config.yaml "$CFG"
echo "# catalog after end: $(catalog | tr '\n' ' ')"
echo "# fleet_status models for $CELL: $(cellmodels)"
echo "# weights kept (no --purge): $(ls -la "$(dirname "$CANDIDATE")" | grep -c "$(basename "$CANDIDATE")")"
"$BIN" model try status

hr "done — every line above is an observation; nothing here asserts"
