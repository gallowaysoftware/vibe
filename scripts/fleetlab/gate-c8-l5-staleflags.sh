#!/usr/bin/env bash
# C8 L5, second half — isolating WHY the flag change did not start a fresh
# baseline: the cell's probe specs are built once, at announcer start.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=charlie; MODEL=lab-embed-c; PORT=$(cell_port charlie); SPORT=$(cell_sport charlie)
PROBEFILE=$LAB/state/ann-$CELL/vibe/fleet/model-probe.json
restart_announcer() {
  kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/ann-$CELL XDG_RUNTIME_DIR=$LAB/run/rt-ann-$CELL
    mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
    nohup "$BIN" fleet announce --cell "$CELL" --registry "$VIBE_API" --token-file "$LAB/state/fleetd/vibe/token" \
      --llama-swap "http://127.0.0.1:$PORT" --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
      >>"$LAB/logs/announce-$CELL.log" 2>&1 & echo $! > "$LAB/run/announce-$CELL.pid" )
  sleep 25
}
load() { curl -fsS -m 180 "http://127.0.0.1:$PORT/v1/embeddings" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"input\":\"C17 C8 L5c load\"}" >/dev/null; sleep 15; }
argv() { curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -r '.running[]?|.cmd' | tr '\n' ' ' | grep -o -- "--threads [0-9]*"; }
baselines() { jq -c '[.baselines[]|{flags:(.flags_sha256[0:12]),n:(.samples|length),verdict}]' "$PROBEFILE"; }

hr "0. the def on disk, and the argv llama-swap is ACTUALLY serving"
grep -n extra_args "$LAB/etc/vibe/backends/$MODEL.yaml"
grep -n -- "--threads" "$LAB/cells/$CELL/config.yaml"
load; echo "# running argv: $(argv)"
echo "# baselines: $(baselines)"

hr "1. set --threads 3 again, WITHOUT restarting the announcer"
sed -i 's/"--embeddings", "--threads", "4"/"--embeddings", "--threads", "3"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
render_cell "$CELL" "$SPORT" || exit 1
sleep 10; load
echo "# running argv now: $(argv)   <- llama-swap -watch-config applied it"
sleep 320
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 35
echo "# announced probe: $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.models[]|select(.id==\"$MODEL\")|.probe|{value,baseline_p50,samples,ratio,verdict,flags:.flags_sha256[0:12]}")"
echo "# baselines: $(baselines)"

hr "2. now restart the announcer — same def, same argv, nothing else changed"
restart_announcer
load
sleep 320
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 35
echo "# announced probe: $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.models[]|select(.id==\"$MODEL\")|.probe|{value,baseline_p50,samples,ratio,verdict,flags:.flags_sha256[0:12]}")"
echo "# baselines: $(baselines)"

hr "3. restore"
sed -i 's/"--embeddings", "--threads", "3"/"--embeddings", "--threads", "4"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
render_cell "$CELL" "$SPORT"
restart_announcer
hr done
