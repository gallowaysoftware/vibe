#!/usr/bin/env bash
# C11 L3, the half that had not run: a hold must reach the WARM SCHEDULE
# guard, not only the probe guard. Reads `last_note`, which is the field
# that carries the reason.
set -uo pipefail
source "$(dirname "$0")/gl.sh"
restart_fleetd() {
  kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null; sleep 3
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
    nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
  # Falling out of this loop is a DEAD DAEMON, not a slow one; there is no
  # `set -e` here, so the refusal has to be the exit. Otherwise every sample
  # after the restart reads an empty schedule and the gate "passes".
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "fleetd did not answer within 30s of the restart — REFUSING to continue (everything after this would measure a daemon that is not running; see $DLOG)" >&2
  exit 1
}
sched() { state | jq -c '.warm.schedule[]?|{cron,model,next_fire,last_fire,last_note}'; }

# Restore a backup an earlier aborted run left behind BEFORE taking this
# run's, or the `cp` below snapshots a config that already carries the
# appended warm_schedule and step 4's restore makes it permanent. Three
# sibling rigs already do this.
[[ -f $LAB/etc/vibe/config.yaml.c17bak ]] && cp "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
cp "$LAB/etc/vibe/config.yaml" "$LAB/etc/vibe/config.yaml.c17bak"
cat >>"$LAB/etc/vibe/config.yaml" <<'EOF'
warm_schedule:
  - cron: "* * * * *"
    model: lab-chat
EOF
restart_fleetd; sleep 5
hr "1. two minutes with no hold"
for i in 1 2 3 4 5 6 7 8; do sleep 15; printf 't+%-4s %s\n' "$((i*15))s" "$(sched)"; done
hr "2. hold alpha; the schedule must skip, naming the hold"
"$BIN" cell hold alpha lab-embed-a --for 10m --note "C11 L3 schedule half" >/dev/null
for i in $(seq 1 10); do sleep 15; printf 't+%-4s %s\n' "$((i*15))s" "$(sched)"; done
hr "3. release"
"$BIN" cell hold alpha lab-embed-a --release >/dev/null
for i in 1 2 3 4 5 6; do sleep 15; printf 't+%-4s %s\n' "$((i*15))s" "$(sched)"; done
hr "4. cleanup"
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"; restart_fleetd
hr done
