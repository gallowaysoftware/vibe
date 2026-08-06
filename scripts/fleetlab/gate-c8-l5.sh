#!/usr/bin/env bash
# C8 L5 — embed probe on a real bge cell: a 64-input batch produces
# embed_inputs_s with a stable baseline, and changing a serving flag starts a
# FRESH baseline instead of reporting a regression.
#
# charlie: roaming, one model (lab-embed-c), no warm target — nothing else on
# that cell competes for residency.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=charlie; MODEL=lab-embed-c; PORT=9643; SPORT=6010
PROBEFILE=$LAB/state/ann-$CELL/vibe/fleet/model-probe.json
ROUNDS=${ROUNDS:-4}

ensure_resident() { # a probe must never load a model; an OPERATOR may
  if [[ $(curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -r '[.running[]|select(.model=="'"$MODEL"'" and .state=="ready")]|length') != 1 ]]; then
    echo "# $MODEL not ready — loading it with a real request (not with a probe)"
    curl -fsS -m 180 "http://127.0.0.1:$PORT/v1/embeddings" -H 'Content-Type: application/json' \
      -d "{\"model\":\"$MODEL\",\"input\":\"C17 C8 L5 operator load\"}" >/dev/null
    sleep 20   # let the announce carry the new state to fleetd
  fi
}
probeblk() { state | jq -c ".cells[]|select(.name==\"$CELL\")|.models[]|select(.id==\"$MODEL\")|.probe"; }
# The ANNOUNCED fingerprint is on the presence block. `.cells[].models[]` is
# the merged /running + /v1/models view and carries no flags_sha256 at all,
# so reading it there prints `null` before and after a def edit — which is
# indistinguishable from the staleness this half is trying to detect.
baselines() { jq -c '{attempts:(.attempts|length), baselines:[.baselines[]|{model,metric,flags:(.flags_sha256[0:12]),n:(.samples|length),verdict}]}' "$PROBEFILE" 2>/dev/null; }

hr "0. the def's kind is read off the rendered argv (C8 §4): --embeddings => embed"
grep -n -- "--embeddings\|--reranking" "$LAB/cells/$CELL/config.yaml"
ensure_resident
baselines

for i in $(seq 1 "$ROUNDS"); do
  hr "$i. probe_model $CELL/$MODEL"
  ensure_resident
  mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
  sleep 35   # two heartbeats: the command rides one announce, the result the next
  echo "--- announced: $(probeblk)"
  echo "--- cell-side: $(baselines)"
  [[ $i -lt $ROUNDS ]] && { echo "# sleeping to clear the 5m cell-side cooldown"; sleep 275; }
done

hr "F1. a serving flag changes: --threads 4 -> 3 on this def only"
echo "# baseline key before: $(baselines)"
BEFORE=$(state | jq -r ".cells[]|select(.name==\"$CELL\")|.presence.models[]|select(.id==\"$MODEL\")|.flags_sha256")
echo "# announced flags_sha256 before: ${BEFORE:0:12}"
sed -i 's/"--embeddings", "--threads", "4"/"--embeddings", "--threads", "3"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
grep -n extra_args "$LAB/etc/vibe/backends/$MODEL.yaml"
render_cell "$CELL" "$SPORT" || exit 1
echo "# waiting for -watch-config + a fresh announce"; sleep 45
AFTER=$(state | jq -r ".cells[]|select(.name==\"$CELL\")|.presence.models[]|select(.id==\"$MODEL\")|.flags_sha256")
echo "# announced flags_sha256 after:  ${AFTER:0:12}   (changed: $([[ $BEFORE != "$AFTER" ]] && echo yes || echo NO))"

hr "F2. wait out the cooldown, then probe on the NEW flags"
sleep 300
ensure_resident
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 35
echo "--- announced: $(probeblk)"
echo "--- cell-side: $(baselines)"

hr "F3. restore the def"
sed -i 's/"--embeddings", "--threads", "3"/"--embeddings", "--threads", "4"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
render_cell "$CELL" "$SPORT"
hr done
