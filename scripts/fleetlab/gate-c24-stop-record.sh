#!/usr/bin/env bash
# C24 — the unit's own stop, recorded, against a REAL fleetd and a real
# announcing cell.
#
# The phase's central finding is not testable against a double: fleetd
# hands a registry intent back to an announcing cell as `desired_intent`,
# and the cell answers a drained one by RUNNING `cell_cmds.drain`. So a
# hook that merely RECORDED a stop would, on the first heartbeat after
# the box came back, stop the serving stack it only meant to describe.
#
# Bravo is the lab's full cell daemon and its `cell_cmds.drain` really
# does stop its llama-swap, so "the record was not handed back as a
# command" is observable as a pid that does not change. That observation
# is worth nothing on its own — a pid also does not change when nothing
# in the fleet works — so step 1 is a POSITIVE CONTROL: a human's
# declared drain, on this same cell, in this same run, which does stop
# it. Same fleet, same verb path, two records: one actuates and one does
# not.
#
# It drives the SHIPPED hook (deploy/cell/vibe-cell-intent.sh), not a
# copy of its logic, and it installs no systemd unit: this box runs a
# production llama-swap and a production vibe daemon, and the phase's
# safety brief forbids touching either. Two things are therefore still
# owed after this rig: a real .service stopping (on a scratch box), and
# the shutdown case, where the network is already going away.
#
# The lab deviation worth reading the transcript with: the hook fires
# WITHOUT the unit actually stopping, so bravo keeps announcing and keeps
# echoing `serving`. The fleet renders INCONSISTENT rather than DRAINED —
# which is the correct rendering of "recorded as stopped, observably
# up", and on a real box the announce would simply be absent.
#
#   FLEETLAB_DIR=/tmp/fleetlab FLEETLAB_PORT_BASE=10200 ./gate-c24-stop-record.sh
#
# Wants a lab whose intent axis is clean (a fresh `lab.sh up`); step 0
# prints what it found rather than assuming, because a leftover
# declaration outranks this hook's record by design and would make every
# step below read as a pass for the wrong reason.
#
# Prints raw evidence, never a verdict. Leaves bravo as it found it.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

HOOK=$(cd "$(dirname "$0")/../../deploy/cell" && pwd)/vibe-cell-intent.sh
CELL=bravo
PIDFILE=$LAB/run/swap-$CELL.pid
CELL_URL=http://127.0.0.1:$(cell_port "$CELL")

bravo() {
  state | jq -c '.cells[] | select(.name=="'"$CELL"'")
    | {display, announcing: .presence.announcing, echo: .presence.intent_echo.state, intent}'
}
alive() { # cell_cmds.drain stops THIS process; the pid is the whole evidence
  local pid; pid=$(cat "$PIDFILE" 2>/dev/null)
  if [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null; then
    echo "swap-$CELL pid $pid: RUNNING"
  else
    echo "swap-$CELL pid ${pid:-<none>}: GONE"
  fi
}
catalog() { curl -fsS -m 5 "$CELL_URL/v1/models" | jq -c '{models: [.data[].id]}' 2>/dev/null || echo '(no answer)'; }
hook() { VIBE_FLEETD_URL=$VIBE_API VIBE_CELL=$CELL VIBE_TOKEN_FILE=$LAB/state/fleetd/vibe/token \
  sh "$HOOK" "$1"; echo "hook exit: $?"; }

hr "0. bravo before anything — the axis must be clean for the rest to mean anything"
bravo
alive
catalog

hr "1. POSITIVE CONTROL: a human's declared drain DOES reach this cell's stack"
mcptext drain_cell '{"cell":"'"$CELL"'","reason":"c24 gate: the control","eta":"23:00"}'
sleep 5
bravo
alive
catalog
echo "# and back:"
mcptext resume_cell '{"cell":"'"$CELL"'"}'
sleep 25
bravo
alive
catalog

hr "2. the shipped ExecStopPost hook: vibe-cell-intent.sh stopped"
echo "# $HOOK"
hook stopped

hr "3. the record as fleetd holds it (the reserved reason is fleetd's, not the hook's)"
bravo

hr "4. two announce waves later (interval 15s): the same pid, still serving"
sleep 35
bravo
alive
catalog

hr "5. the doctor does not get quieter about an undeclared stop"
# Captured first rather than piped: `fleet doctor` exits 2 on a WARN, and
# under `pipefail` that status is the PIPELINE's — so a piped grep that
# matched would still have run its `||` branch and printed "no line"
# underneath the lines it had just found.
DOCTOR=$("$BIN" fleet doctor 2>&1 || true)
grep -aiE 'intent\.hygiene' -A6 <<<"$DOCTOR" || echo "  (no intent.hygiene line)"

hr "6. the paired ExecStartPost retires the record"
hook started
sleep 3
bravo

hr "7. a human's declaration outranks the unit's record and is not overwritten"
mcptext drain_cell '{"cell":"'"$CELL"'","reason":"c24 gate: a human said why","eta":"23:00"}'
sleep 5
echo "# after the human's drain:"; bravo
hook stopped
sleep 3
echo "# after the unit's stop record lands on top of it — the reason must still be the human's:"
bravo

hr "8. put bravo back"
hook started
mcptext resume_cell '{"cell":"'"$CELL"'"}'
sleep 30
bravo
alive
catalog

hr done
