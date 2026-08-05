#!/usr/bin/env bash
# C14 L4 — the mid-batch night. A LEASE held across the declared minute must
# defer the suspend, and the deferral must be ABANDONED at max_defer, visibly,
# without recording any intent.
#
# max_defer is set to 4m rather than a whole night: the deferral loop is a
# once-a-minute re-evaluation, so what "all night" adds is repetitions of a
# decision this run watches four times. That substitution is the one thing
# this gate does not prove.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=bravo; MODEL=lab-embed-b
FTZ=America/Toronto
QUIET=${QUIET:-5m}
DEFER=${DEFER:-4m}
SUSPEND_IN=${SUSPEND_IN:-3}
WAKE_IN=${WAKE_IN:-30}

restart_fleetd() {
  kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null
  sleep 3
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
    nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && break; sleep 0.5; done
}
sleepblk() { state | jq -c '.sleep.entries[]?'; }
leases() { curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/leases" | jq -c '[.leases[]|{cell,model,holder,expires_at}]'; }
cron_at() { TZ=$FTZ date -d "+$1 minutes" "+%-M %-H * * *"; }

SUS=$(cron_at $SUSPEND_IN); WAK=$(cron_at $WAKE_IN)
hr "0. declare the night: suspend '$SUS', wake '$WAK', quiet_for $QUIET, max_defer $DEFER (fleet tz $FTZ, now $(TZ=$FTZ date +%H:%M:%S))"
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
sleepblk

hr "1. the overnight batch claims a lease and starts work"
"$BIN" cell await "$CELL" --model "$MODEL" --ready --lease overnight-batch --lease-ttl 30m \
  --lease-note "C17 C14 L4 mid-batch night" --timeout 2m
leases
echo "# the box then goes quiet — no requests from here, so ONLY the lease can block the suspend"
echo "# activity: $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.activity")"

hr "2. the declared minute arrives with the lease live"
for i in $(seq 1 40); do
  sleep 15
  printf 't+%-5s %s\n' "$((i*15))s" "$(sleepblk)"
  if [[ -s $LAB/logs/cell-verbs.log ]]; then echo "# !!! SUSPEND VERB RAN (it must not have):"; cat "$LAB/logs/cell-verbs.log"; break; fi
  [[ $(sleepblk | jq -r .state) == skipped ]] && { echo "# abandoned — the night is over"; break; }
done

hr "3. after the abandonment: no intent recorded, nothing suspended"
state | jq -c ".cells[]|select(.name==\"$CELL\")|{display,intent}"
echo "# suspend verb log: $(wc -c <"$LAB/logs/cell-verbs.log") bytes"
echo "# the cell's own intent file:"; jq -c . "$LAB/state/bravo/vibe/fleet/cell-intent.json" 2>/dev/null || echo "  (none)"
sleepblk

hr "4. two more minutes past the abandonment: it must NOT come back"
for i in 1 2 3 4 5 6 7 8; do sleep 15; printf 't+%-5s %s  verbs=%s\n' "$((i*15))s" "$(sleepblk | jq -c '{state,detail}')" "$(wc -c <"$LAB/logs/cell-verbs.log")"; done

hr "5. cleanup: release the lease, drop the schedule"
curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" -X DELETE "$VIBE_API/api/fleet/lease" \
  -H 'Content-Type: application/json' -d "{\"cell\":\"$CELL\",\"model\":\"$MODEL\",\"holder\":\"overnight-batch\"}"; echo
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
restart_fleetd
leases
hr done
