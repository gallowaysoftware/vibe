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
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && break; sleep 0.5; done
}

hr "0. point the notifier at a real ntfy topic (random, public, lab-only content)"
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
state | jq -c '.notify | {endpoint: (.endpoint // "absent"), delivery}'
state | jq -r '.. | strings' | grep -c "ntfy.sh/$TOPIC" || echo "0 occurrences of the topic anywhere in /api/fleet/state"
grep -c "$TOPIC" "$LAB/logs/fleetd.log" || echo "0 occurrences of the topic in fleetd's log"

hr "4. restore the local sink"
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
restart_fleetd
hr done
