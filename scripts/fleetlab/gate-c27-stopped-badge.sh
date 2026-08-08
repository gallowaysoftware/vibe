#!/usr/bin/env bash
# C27 — STOPPED, on a cell that is actually down, against a real fleetd.
#
# C24's rig (gate-c24-stop-record.sh) fires the shipped hook WITHOUT the
# unit stopping, so its cell keeps announcing and the fleet renders
# INCONSISTENT — correct, and useless for looking at this phase's display
# state. This rig takes the stack down first, the way a `systemctl stop`
# would: alpha's llama-swap dies, alpha's announcer dies, alpha's host
# probe stays up (the box is fine; the serving stack is not).
#
# That is the exact row C27 splits:
#
#   host up, cell silent, NO intent entry          → DRAINED?   (step 2)
#   host up, cell silent, the unit's stop record   → STOPPED    (step 4)
#   host up, cell silent, a human's declaration    → DRAINED    (step 8)
#
# Alpha is always_on, which makes the same run the hazard gate: a display
# state that is not named in absentAlarm's switch silently stops paging,
# and the lab's webhook sink is where that shows up as a fact rather than
# as a unit test. Steps 5 and 6 read it.
#
#   FLEETLAB_DIR=/tmp/fleetlab-c27 FLEETLAB_PORT_BASE=10300 ./gate-c27-stopped-badge.sh
#
# Prints raw evidence, never a verdict. Puts alpha back.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

HOOK=$(cd "$(dirname "$0")/../../deploy/cell" && pwd)/vibe-cell-intent.sh
CELL=alpha
SWAPCTL=$LAB/bin/swapctl.sh
ANN_PID=$LAB/run/announce-$CELL.pid

cell() {
  state | jq -c '.cells[] | select(.name=="'"$CELL"'")
    | {display, class, reachable, host_reachable, announcing: (.presence.announcing // false),
       stale: (.presence.stale // false), intent}'
}
hook() { VIBE_FLEETD_URL=$VIBE_API VIBE_CELL=$CELL VIBE_TOKEN_FILE=$LAB/state/fleetd/vibe/token \
  sh "$HOOK" "$1"; echo "hook exit: $?"; }
alarms() { # what the webhook sink was handed about this cell, newest last
  grep -a "$CELL" "$LAB/logs/notify.log" 2>/dev/null | tail -4 || echo "  (nothing about $CELL in the sink log)"
}
badge() { # what the page would put on that badge
  curl -fsS -m 10 "$VIBE_API/ui/fleet" | grep -aE '=== "(STOPPED|DRAINED)"|b-stopped \{'
}

hr "0. alpha before anything — always_on, serving, nothing declared"
cell
"$BIN" cell status 2>&1 | head -8

hr "1. the stack goes down the way a systemctl stop takes it down"
"$SWAPCTL" "$CELL" stop
if [[ -f $ANN_PID ]]; then
  kill -TERM "$(cat "$ANN_PID")" 2>/dev/null && rm -f "$ANN_PID" && echo "announcer for $CELL: stopped"
fi
echo "# host probe is untouched: the BOX is fine, the serving stack is not"

hr "2. once the announce goes stale (3× interval): DRAINED? — nothing recorded anything"
sleep 65
cell

hr "3. the shipped ExecStopPost hook, on a cell that really is down"
echo "# $HOOK"
hook stopped

hr "4. the phase: the same cell now reads STOPPED, and the record is fleetd's own"
sleep 3
cell
"$BIN" cell status 2>&1 | head -8

hr "5. the page has a badge class for it (a state that fell through would render b-off)"
badge

hr "6. THE HAZARD: an always_on cell that STOPPED must still page"
# fleetd is restarted first, deliberately. Without it the cell_absent
# condition raised during step 2 (DRAINED?, no entry) is still active and
# would still be active whatever this phase did to the display — the
# alarm would be inherited evidence, which is no evidence. A fresh
# process has seen NOTHING about alpha except STOPPED, and the intent
# store it reloads from disk still holds the unit's record.
kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null; sleep 3
( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
  nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! >"$LAB/run/fleetd.pid" )
for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && break; sleep 0.5; done
echo "# fleetd restarted; the record survived the restart:"
cell
echo "# cell_absent's dwell is 2 minutes — waiting it out rather than declaring it fired"
sleep 150
state | jq -c '.notify | {delivery, active: [.alarms[]? | {key, kind, since}]}'
echo "# and what the webhook was actually handed:"
alarms

hr "7. the doctor does not get quieter"
DOCTOR=$("$BIN" fleet doctor 2>&1 || true)
grep -aiE 'intent\.hygiene' -A6 <<<"$DOCTOR" || echo "  (no intent.hygiene line)"

hr "8. a human's declaration on the same silent cell still reads DRAINED"
mcptext drain_cell '{"cell":"'"$CELL"'","reason":"c27 gate: a human said why","eta":"23:00"}'
sleep 3
cell

hr "9. put alpha back"
mcptext resume_cell '{"cell":"'"$CELL"'"}'
hook started
"$SWAPCTL" "$CELL" start
( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/ann-$CELL XDG_RUNTIME_DIR=$LAB/run/rt-ann-$CELL
  mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"
  nohup "$BIN" fleet announce --cell "$CELL" --registry "$VIBE_API" \
    --token-file "$LAB/state/fleetd/vibe/token" \
    --llama-swap "http://127.0.0.1:$(cell_port "$CELL")" \
    --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
    >>"$LAB/logs/announce-$CELL.log" 2>&1 & echo $! >"$ANN_PID" )
sleep 25
cell

hr done
