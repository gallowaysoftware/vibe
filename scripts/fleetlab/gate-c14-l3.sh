#!/usr/bin/env bash
# C14 L3 — the operator at 23:29. A REAL request at a lab cell must defer the
# declared suspend, and keep deferring while the session continues; the
# suspend then fires on its own once the box is genuinely quiet.
#
# The cron minutes are computed from the wall clock IN THE FLEET TIMEZONE
# (fleet.timezone: America/Toronto), so the "declared minute" is real.
# cell_cmds.suspend is the lab's no-op stub, so the DECISION is observable
# without suspending a workstation.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=bravo
FTZ=America/Toronto
QUIET=${QUIET:-5m}     # floor is 5m
DEFER=${DEFER:-20m}
SUSPEND_IN=${SUSPEND_IN:-2}   # minutes from now
WAKE_IN=${WAKE_IN:-25}

restart_fleetd() {
  kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null   # pidfile only: bravo runs the same binary
  sleep 3
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
    nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && break; sleep 0.5; done
}
sleepblk() { state | jq -c '.sleep.entries[]?'; }
cellblk()  { state | jq -c '.cells[]|select(.name=="'"$CELL"'")|{display,intent}'; }
poke() { curl -fsS -m 180 "http://127.0.0.1:$(cell_port bravo)/v1/embeddings" -H 'Content-Type: application/json' \
           -d '{"model":"lab-embed-b","input":"C17 C14 L3 the operator is typing"}' >/dev/null && echo "# request served at $(date -Is)"; }

cron_at() { TZ=$FTZ date -d "+$1 minutes" "+%-M %-H * * *"; }
SUS=$(cron_at $SUSPEND_IN); WAK=$(cron_at $WAKE_IN)

hr "0. declare the night: suspend '$SUS', wake '$WAK' (fleet tz $FTZ; now there is $(TZ=$FTZ date +%H:%M))"
[[ -f $LAB/etc/vibe/config.yaml.c17bak ]] && cp "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
cp "$LAB/etc/vibe/config.yaml" "$LAB/etc/vibe/config.yaml.c17bak"
cat >>"$LAB/etc/vibe/config.yaml" <<EOF
sleep_schedule:
  - cell: $CELL
    suspend: "$SUS"
    wake: "$WAK"
    quiet_for: $QUIET
    max_defer: $DEFER
EOF
: >"$LAB/logs/cell-verbs.log"
restart_fleetd
sleepblk; cellblk

hr "1. the operator types — a real request ~25s before the declared minute"
sleep $(( SUSPEND_IN*60 - 25 - $(date +%S) )) 2>/dev/null || sleep 5
poke

hr "2. the declared minute arrives while the session is live (a request every 60s)"
for i in $(seq 1 12); do
  sleep 15
  printf 't+%-5s %s\n' "$((i*15))s" "$(sleepblk)"
  (( i % 4 == 0 )) && poke
done

hr "3. the session ends. silence from here; quiet_for is $QUIET"
LASTPOKE=$(date -Is)
for i in $(seq 1 32); do
  sleep 15
  printf 't+%-5s %s\n' "$((i*15))s" "$(sleepblk)"
  if [[ -s $LAB/logs/cell-verbs.log ]]; then echo "# SUSPEND VERB RAN:"; cat "$LAB/logs/cell-verbs.log"; break; fi
done
echo "# last request was at $LASTPOKE"

hr "4. what the fleet says about a sleeping box"
cellblk
sleepblk
"$BIN" cell status 2>&1 | head -12
echo "# the CELL's own intent record (C14: CellSuspend stamps it locally before it freezes):"
jq -c . "$LAB/state/bravo/vibe/fleet/cell-intent.json" 2>/dev/null || echo "  (none)"

hr "5. cleanup: drop the schedule, resume the cell"
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
restart_fleetd
"$BIN" cell resume "$CELL" 2>&1 | head -3
sleep 20; cellblk
hr done
