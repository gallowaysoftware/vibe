#!/usr/bin/env bash
# C11 L2 — a hold is NOT a pin. With the cell's llama-swap TTL set BELOW the
# hold, the TTL must still evict the held model; fleetd must warm nothing
# while the hold stands; the status must still read `held`.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=alpha; PORT=$(cell_port alpha); SPORT=$(cell_sport alpha)
HELD=lab-embed-a
DEFAULT=lab-chat   # the cell's warm target (restore_after_idle 2m)

running() { curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -c '[.running[]|{model,state,ttl}]'; }
warmblk() { state | jq -c '.warm.targets[]? | select(.cell=="'"$CELL"'")'; }
frontsha() { sha256sum "$LAB/cells/front/config.yaml" | cut -c1-16; }

hr "0. drop the held model's TTL to 45s (the whole point of the gate)"
sed -i 's/^  ttl: 10m$/  ttl: 45s/' "$LAB/etc/vibe/backends/$HELD.yaml"
tail -3 "$LAB/etc/vibe/backends/$HELD.yaml"
render_cell "$CELL" "$SPORT" || exit 1
grep -A1 "^    $HELD:" "$LAB/cells/$CELL/config.yaml" | head -4
grep -n "ttl:" "$LAB/cells/$CELL/config.yaml"
echo "# giving llama-swap -watch-config a moment"; sleep 8

hr "1. load the challenger ($HELD) with a real request; it evicts the default"
curl -fsS -m 180 "http://127.0.0.1:$PORT/v1/embeddings" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$HELD\",\"input\":\"C17 C11 L2\"}" | jq -c '{usage}'
running

hr "2. hold it for 20m — far longer than the 45s TTL"
warmrestores() { grep -a "warm-target restor" "$DLOG" 2>/dev/null | grep -a -c "\"cell\":\"$CELL\""; }
[[ -f $DLOG ]] || { echo "no $DLOG — fleetd's log is in the state dir; is the lab up?" >&2; exit 1; }
WARMLINES0=$(warmrestores)
"$BIN" cell hold "$CELL" "$HELD" --for 20m --note "C17 C11 L2 not-a-pin"
FRONT0=$(frontsha); echo "# front config sha256(16) before: $FRONT0"
echo "# fleetd warm-target actuations naming $CELL before the hold: $WARMLINES0"

hr "3. watch for 4 minutes: the TTL must evict, and fleetd must warm NOTHING"
for i in $(seq 1 12); do
  sleep 20
  printf 't+%-4s running=%s warm=%s\n' "$((i*20))s" "$(running)" "$(warmblk)"
done

hr "4. the ledger's view: did anything warm the default back?"
state | jq -c '.cells[]|select(.name=="'"$CELL"'")|{display,models:[.models[]|{id,state}]}'
# The queue is NOT on the state document — `.cells[].commands` does not
# exist there (commands ride the ANNOUNCE RESPONSE and are drained on
# delivery), so `.commands // "none"` prints "none" whether fleetd queued a
# warm or not. fleetd's own log is where a queued or delivered warm shows,
# and the count must be read as a DELTA against the pre-hold count.
echo "# fleetd's warm/piggyback actuation lines for this session ($DLOG):"
grep -a -E "warm-target|warm schedule|piggyback" "$DLOG" | tail -8
echo "# cumulative warm-target restores naming $CELL: $(warmrestores)  (pre-hold: $WARMLINES0 — EQUAL means fleetd warmed nothing under the hold)"
echo "# front config sha256(16) after: $(frontsha)  (before: $FRONT0)"
echo "# llama-swap's own log — what it says about the eviction:"
grep -a -i "unload\|ttl\|stopping" "$LAB/logs/swap-$CELL.log" | tail -8

hr "5. release the hold; the restore is allowed again"
"$BIN" cell hold "$CELL" "$HELD" --release
for i in 1 2 3 4 5 6; do sleep 15; printf 't+%-4s running=%s warm=%s\n' "$((i*15))s" "$(running)" "$(warmblk)"; done

hr "6. restore the def's TTL"
sed -i 's/^  ttl: 45s$/  ttl: 10m/' "$LAB/etc/vibe/backends/$HELD.yaml"
render_cell "$CELL" "$SPORT"
hr done
