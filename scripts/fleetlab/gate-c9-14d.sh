#!/usr/bin/env bash
# C9 14d — a def edited on the FRONT but not pushed to its cell: the
# fingerprint mismatch persists past its dwell, pages once, and resolves when
# the two sides agree again.
#
# The lab shares one backends dir between fleetd and the slim announcers, so
# "edited on the front but not on the cell" cannot exist there. This gate
# gives alpha its OWN config root first — which is what a real cell has.
#
# The fingerprint_drift dwell is lowered from 15m to 30s through the ordinary
# fleet.notify.dwell CONFIG key. No code is changed; what shrinks is the
# waiting, not the state machine.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=alpha; MODEL=lab-embed-a; PORT=$(cell_port alpha)
CELLETC=$LAB/etc-$CELL
SINK=$LAB/logs/notify.log
FRONTCFG=$LAB/cells/front/config.yaml

restart_fleetd() {
  kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null; sleep 3
  ( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
    nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
  # Falling out of this loop is a DEAD DAEMON, not a slow one; there is no
  # `set -e` here, so the refusal has to be the exit. Otherwise `fp` and
  # `notifyblk` print empty for the whole run and "no drift was detected"
  # is indistinguishable from "nothing was detecting".
  for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "fleetd did not answer within 30s of the restart — REFUSING to continue (everything after this would measure a daemon that is not running; see $DLOG)" >&2
  exit 1
}
restart_announcer() { # $1 = XDG_CONFIG_HOME
  kill -TERM "$(cat "$LAB/run/announce-$CELL.pid")" 2>/dev/null; sleep 2
  ( export XDG_CONFIG_HOME=$1 XDG_STATE_HOME=$LAB/state/ann-$CELL XDG_RUNTIME_DIR=$LAB/run/rt-ann-$CELL
    mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
    nohup "$BIN" fleet announce --cell "$CELL" --registry "$VIBE_API" --token-file "$LAB/state/fleetd/vibe/token" \
      --llama-swap "http://127.0.0.1:$PORT" --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
      >>"$LAB/logs/announce-$CELL.log" 2>&1 & echo $! > "$LAB/run/announce-$CELL.pid" )
  sleep 25
}
notifyblk() { state | jq -c '.notify | {alarms, delivery: {sent: .delivery.sent, failed: .delivery.failed}, fingerprint_source}'; }
# The ANNOUNCED fingerprint is on the presence block; `.cells[].models[]` is
# the merged /running view and has no flags_sha256, so reading it there
# prints `null` for the whole run — the drift and its absence look identical.
fp() { state | jq -r ".cells[]|select(.name==\"$CELL\")|.presence.models[]|select(.id==\"$MODEL\")|.flags_sha256[0:12]"; }
# The front render is peers-only: a model is a SEQUENCE ITEM under its
# cell's `models:` list (`            - lab-embed-a`), never a mapping key.
# Matching `$MODEL:` counts 0 whether the strict def is in the render or
# not, which is the answer this gate exists to distinguish.
inrender() { grep -c "^ *- $MODEL\$" "$FRONTCFG"; }
sinklines() { grep -c . "$SINK" 2>/dev/null || echo 0; }

hr "0. give $CELL its own config root — a real cell does not share the front's defs dir"
mkdir -p "$CELLETC/vibe"
cp -r "$LAB/etc/vibe/backends" "$CELLETC/vibe/backends"
ln -sfn "$LAB/etc/vibe/hosts.yaml" "$CELLETC/vibe/hosts.yaml"
restart_announcer "$CELLETC"
echo "# announced flags_sha256 for $MODEL: $(fp)"
echo "# $MODEL peers stanzas in the front render: $(inrender)"
echo "# notify: $(notifyblk)"

hr "1. lower the fingerprint_drift dwell to 30s (config only)"
[[ -f $LAB/etc/vibe/config.yaml.c17bak ]] && cp "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
cp "$LAB/etc/vibe/config.yaml" "$LAB/etc/vibe/config.yaml.c17bak"
python3 - "$LAB/etc/vibe/config.yaml" <<'PY'
import sys,re
p=sys.argv[1]; s=open(p).read()
s=s.replace("    burst: 10\n","    burst: 10\n    dwell:\n      fingerprint_drift: 30s\n    clear_dwell: 30s\n")
open(p,'w').write(s)
PY
sed -n '/notify:/,/^warm_targets/p' "$LAB/etc/vibe/config.yaml"
restart_fleetd
sleep 20
echo "# sink lines before: $(sinklines)"; echo "# notify: $(notifyblk)"

hr "2. edit the def ON THE FRONT ONLY (--threads 4 -> 6); the cell keeps its copy"
sed -i 's/"--embeddings", "--threads", "4"/"--embeddings", "--threads", "6"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
grep -n extra_args "$LAB/etc/vibe/backends/$MODEL.yaml" "$CELLETC/vibe/backends/$MODEL.yaml"

hr "3. watch the mismatch appear, dwell, and page exactly once"
for i in $(seq 1 16); do
  sleep 15
  printf 't+%-5s fp=%s render_stanzas=%s sink=%s notify=%s\n' "$((i*15))s" "$(fp)" "$(inrender)" "$(sinklines)" "$(notifyblk)"
done
echo "# the alarm deliveries:"; grep -a "fingerprint" "$SINK" | tail -5
echo "# events stream carried:"; timeout 5 curl -sN -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/events" | head -5 || true
echo "# fleetd's own log ($DLOG — NOT logs/fleetd.log, which stays empty):"
grep -a -i "fingerprint\|strict-fingerprint" "$DLOG" | tail -5

hr "4. push the def to the cell — the two sides agree again"
cp "$LAB/etc/vibe/backends/$MODEL.yaml" "$CELLETC/vibe/backends/$MODEL.yaml"
restart_announcer "$CELLETC"
for i in $(seq 1 10); do
  sleep 15
  printf 't+%-5s fp=%s render_stanzas=%s sink=%s notify=%s\n' "$((i*15))s" "$(fp)" "$(inrender)" "$(sinklines)" "$(notifyblk)"
done
echo "# the resolve delivery:"; grep -a "fingerprint" "$SINK" | tail -3

hr "5. cleanup"
sed -i 's/"--embeddings", "--threads", "6"/"--embeddings", "--threads", "4"/' "$LAB/etc/vibe/backends/$MODEL.yaml"
cp "$LAB/etc/vibe/backends/$MODEL.yaml" "$CELLETC/vibe/backends/$MODEL.yaml"
mv "$LAB/etc/vibe/config.yaml.c17bak" "$LAB/etc/vibe/config.yaml"
restart_fleetd
restart_announcer "$CELLETC"
echo "# fp back to: $(fp)  render_stanzas=$(inrender)"
hr done
