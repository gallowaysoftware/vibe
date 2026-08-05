#!/usr/bin/env bash
# C8 L4 — the 96/day probe cap, observed at its boundary.
#
# HONESTY: the 24 h window is PRE-SEEDED on the cell's own state file rather
# than accumulated over 24 h of wall clock. What this run observes is the cap
# BOUNDARY and the window's ROLL-OFF against the real production budget code
# (modelprobe.budgetRefusal / attemptsSinceLocked) reached through the real
# MCP verb and the real piggyback queue. It does not observe 24 h of
# scheduling, and it is not a substitute for that.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=alpha; MODEL=lab-chat; PORT=9641
PROBEFILE=$LAB/state/ann-$CELL/vibe/fleet/model-probe.json

usage_row() { curl -fsS -m 20 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/usage" \
  | jq -c "[.rows[]? | select(.cell==\"$CELL\" and .model==\"$MODEL\")] | {req:(map(.req)|add), out:(map(.out)|add), in_fresh:(map(.in_fresh)|add), in_cached:(map(.in_cached)|add)}"; }
seed() { # $1 = count inside the window, $2 = count OUTSIDE it (older than 24h)
  python3 - "$PROBEFILE" "$1" "$2" "$MODEL" <<'PY'
import json,sys,datetime
path,n_in,n_out,model=sys.argv[1],int(sys.argv[2]),int(sys.argv[3]),sys.argv[4]
st=json.load(open(path))
now=datetime.datetime.now(datetime.timezone.utc)
def iso(dt): return dt.isoformat().replace('+00:00','Z')
att=[iso(now-datetime.timedelta(minutes=10+ i*14)) for i in range(n_in)]          # 10m .. ~22h ago
att+=[iso(now-datetime.timedelta(hours=25+i)) for i in range(n_out)]              # 25h+ ago: outside
st['attempts']=sorted(att)
st.setdefault('last_attempt',{})[model]=iso(now-datetime.timedelta(minutes=30))   # cooldown clear
json.dump(st,open(path,'w'))
print(f"seeded: {n_in} attempts inside the 24h window, {n_out} outside it")
PY
}
restart_announcer() {
  kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/ann-$CELL XDG_RUNTIME_DIR=$LAB/run/rt-ann-$CELL
    mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
    nohup "$BIN" fleet announce --cell "$CELL" --registry "$VIBE_API" --token-file "$LAB/state/fleetd/vibe/token" \
      --llama-swap "http://127.0.0.1:$PORT" --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
      >>"$LAB/logs/announce-$CELL.log" 2>&1 & echo $! > "$LAB/run/announce-$CELL.pid" )
  sleep 20
}
attempts() { jq -c '{attempts:(.attempts|length), last_attempt}' "$PROBEFILE"; }

hr "0. make sure $MODEL is resident (an operator request, never a probe)"
curl -fsS -m 300 "http://127.0.0.1:$PORT/v1/chat/completions" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say ok\"}],\"max_tokens\":4}" | jq -c '{model,usage}'
curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -c '[.running[]|{model,state}]'
sleep 20
echo "# ledger before: $(usage_row)"
cp "$PROBEFILE" "$PROBEFILE.c17bak" 2>/dev/null || true

hr "1. seed 96 attempts inside the window: the cap is spent"
kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2
seed 96 0
attempts
restart_announcer
echo "# probe_model with the budget spent:"
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 35
echo "# announced probe block: $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.models[]|select(.id==\"$MODEL\")|.probe")"
echo "# cell-side after the refusal (an attempt must NOT have been spent): $(attempts)"

hr "2. seed 95 inside + 6 outside: the window ROLLS, so the 96th is allowed"
kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2
seed 95 6
attempts
restart_announcer
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 40
echo "# announced probe block: $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.models[]|select(.id==\"$MODEL\")|.probe")"
echo "# cell-side after: $(attempts)"

hr "3. the 97th, immediately after: refused again"
python3 - "$PROBEFILE" "$MODEL" <<'PY'
import json,sys,datetime
path,model=sys.argv[1],sys.argv[2]
st=json.load(open(path))
st['last_attempt'][model]=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=30)).isoformat().replace('+00:00','Z')
json.dump(st,open(path,'w'))
print("# cooldown cleared on disk so the CAP is what answers, not the 5m gap")
PY
restart_announcer
mcptext probe_model "{\"cell\":\"$CELL\",\"model\":\"$MODEL\"}"
sleep 20
echo "# cell-side: $(attempts)"

hr "4. what one probe costs the C7a ledger"
sleep 30
echo "# ledger after: $(usage_row)"
echo "# llama-swap's own rows for the probe request(s):"
curl -fsS -m 10 "http://127.0.0.1:$PORT/api/metrics/activity" | jq -c '[.data[] | select(.model=="'"$MODEL"'")][-3:] | .[] | {id,model,req_path,input_tokens,cache_tokens,output_tokens,resp_status_code}'

hr "5. restore the cell's real probe state"
[[ -f $PROBEFILE.c17bak ]] && { kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2; mv "$PROBEFILE.c17bak" "$PROBEFILE"; restart_announcer; }
attempts
hr done
