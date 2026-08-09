#!/usr/bin/env bash
# C9 14a — the half that was open: does a REAL ntfy topic accept the payload
# vibe sends? (The phone half stays open; nothing here proves a push arrived
# on a device.) A random public topic, a lab fleet, no house values.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

TOPIC=vibe-fleetlab-$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
URL=https://ntfy.sh/$TOPIC
restart_fleetd() {
  kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null; sleep 3
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
    nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
  # Falling out of this loop is a DEAD DAEMON, not a slow one. Returning 0
  # there (the old shape: `&& break`, then the loop's own success) let every
  # step after the restart run against nothing and report its emptiness as a
  # finding. There is no `set -e` here, so the refusal has to be the exit.
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "fleetd did not answer within 30s of the restart — REFUSING to continue (everything after this would measure a daemon that is not running; see $DLOG)" >&2
  exit 1
}

hr "0. point the notifier at a real ntfy topic (random, public, lab-only content)"
# Restore a backup an earlier aborted run left behind BEFORE taking this
# run's. Without it the `cp` below snapshots an already-rewritten config, the
# cleanup in step 4 restores the rewrite, and the lab keeps pointing at a
# dead ntfy topic from then on. Three sibling rigs already do this.
[[ -f $LAB/etc/vibe/config.yaml.c17bak ]] && cp "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
cp "$LAB/etc/vibe/config.yaml" "$LAB/etc/vibe/config.yaml.c17bak"
sed -i "s#url: \"http://127.0.0.1:$LAB_NOTIFY_PORT/fleetlab\"#url: \"$URL\"#" "$LAB/etc/vibe/config.yaml"
grep -n "url:" "$LAB/etc/vibe/config.yaml"
restart_fleetd
sleep 5

hr "1. vibe fleet notify test"
"$BIN" fleet notify test "C17 gate 14a: a real ntfy topic, a lab fleet" 2>&1 | head -5
sleep 5
state | jq -c '.notify.delivery'

hr "2. read the topic back from ntfy's own API — did it accept and store it?"
curl -fsS -m 20 "https://ntfy.sh/$TOPIC/json?poll=1&since=10m" | jq -c '{id,time,event,topic,title,message,tags,priority}'

hr "3. the status must never carry the URL (C9 gate 8, in the field)"
# Capture the state ONCE and prove it arrived before searching it. As a
# pipeline (`state | jq | grep -c … || echo "0 occurrences…"`) a curl that
# 404s or a jq that errors trips pipefail, the `||` fires, and the rig prints
# the same reassuring "0 occurrences" it prints when the endpoint is clean:
# a dead API reading as proof of no leak. The only `||` left below is grep's
# own no-match exit, which is the finding this step is here to make.
ST=$(state) || { echo "FAILED to read /api/fleet/state — this step proves NOTHING about leakage" >&2; exit 1; }
[[ -n $ST ]] || { echo "/api/fleet/state returned empty — this step proves NOTHING about leakage" >&2; exit 1; }
printf '%s' "$ST" | jq -c '.notify | {endpoint: (.endpoint // "absent"), delivery}'
STRINGS=$(printf '%s' "$ST" | jq -r '.. | strings') ||
  { echo "jq failed over the captured state — this step proves NOTHING about leakage" >&2; exit 1; }
printf '%s\n' "$STRINGS" | grep -c "ntfy.sh/$TOPIC" || echo "0 occurrences of the topic anywhere in /api/fleet/state"
# fleetd's OWN log, not $LAB/logs/fleetd.log — that file holds only what the
# process wrote before installing its slog handler, which is nothing, so
# grepping it counts zero forever and reads exactly like "the daemon never
# said it". See gl.sh's $DLOG note. -a because daemon.log trips grep's
# binary heuristic, which is why the five sibling rigs use it.
grep -a -c "$TOPIC" "$DLOG" || echo "0 occurrences of the topic in fleetd's own log ($DLOG)"

hr "4. restore the local sink"
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
restart_fleetd
hr done
