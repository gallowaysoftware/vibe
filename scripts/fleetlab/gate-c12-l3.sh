#!/usr/bin/env bash
# C12 L3 — guest token rotation. The old value must stop working and the
# new one must work, across a real fleetd restart.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

GF=$LAB/state/fleetd/vibe/guest-token
code() { curl -s -o /dev/null -w '%{http_code}' -m 10 -H "Authorization: Bearer $1" "$VIBE_API$2"; }
counters() { state | jq -c '.daemon|{guest_enabled,guest_rejected,auth_rejected}'; }

hr "0. the guest token as it stands, and the counters"
OLD=$(cat "$GF"); echo "# old guest token sha256: $(printf %s "$OLD" | sha256sum | cut -c1-16)"
ls -l "$GF"
echo "state=$(code "$OLD" /api/fleet/state) events-head=$(curl -s -o /dev/null -w '%{http_code}' -m 3 -H "Authorization: Bearer $OLD" "$VIBE_API/api/fleet/events") usage=$(code "$OLD" /api/fleet/usage) mcp=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST -H "Authorization: Bearer $OLD" "$VIBE_API/mcp")"
counters

hr "1. rotate: vibe token --guest --regenerate"
# `vibe token --guest --regenerate` PRINTS the new bearer on stdout (that is
# its job — the operator has to copy it somewhere). A gate transcript gets
# pasted into a PR, so the value is redacted here and the token is compared
# by digest, the way the rest of this script does it.
$BIN token --guest --regenerate --yes | while IFS= read -r line; do
  case $line in
    '#'*|'') printf '%s\n' "$line" ;;
    *) printf '<guest token redacted; sha256 %s>\n' "$(printf %s "$line" | sha256sum | cut -c1-16)" ;;
  esac
done
NEW=$(cat "$GF"); echo "# new guest token sha256: $(printf %s "$NEW" | sha256sum | cut -c1-16)"
[[ "$OLD" != "$NEW" ]] && echo "# value CHANGED on disk" || echo "# value UNCHANGED — gate fails here"
ls -l "$GF"

hr "2. before the restart, the running fleetd still holds the OLD value in memory"
echo "old=$(code "$OLD" /api/fleet/state) new=$(code "$NEW" /api/fleet/state)"

hr "3. restart fleetd"
kill -TERM "$(cat "$LAB/run/fleetd.pid")" 2>/dev/null  # pidfile only: bravo runs the same binary
sleep 3
( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/fleetd XDG_RUNTIME_DIR=$LAB/run/rt-fleetd
  nohup "$BIN" daemon >>"$LAB/logs/fleetd.log" 2>&1 & echo $! > "$LAB/run/fleetd.pid" )
for _ in $(seq 1 60); do curl -fsS -m 2 "$VIBE_API/ui/fleet" >/dev/null 2>&1 && break; sleep 0.5; done
# fleetd's slog lands in the state dir, not logs/fleetd.log (which is empty).
grep -a "guest" "$DLOG" | tail -3

hr "4. the phone with the OLD token"
echo "state=$(code "$OLD" /api/fleet/state) events=$(curl -s -o /dev/null -w '%{http_code}' -m 3 -H "Authorization: Bearer $OLD" "$VIBE_API/api/fleet/events")"
hr "5. the phone with the NEW token"
echo "state=$(code "$NEW" /api/fleet/state) usage=$(code "$NEW" /api/fleet/usage) mcp=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST -H "Authorization: Bearer $NEW" "$VIBE_API/mcp")"
echo "# X-Vibe-Auth header under the new guest token:"
curl -s -D- -o /dev/null -m 10 -H "Authorization: Bearer $NEW" "$VIBE_API/api/fleet/state" | grep -i 'x-vibe-auth' || echo "  (none)"
# The two counters mean different things and a rotation is where that shows.
# `guest_rejected` counts 401s from a VALID guest token on a route its grant
# does not cover (daemon/auth.go); a rotated-out value is not a guest token
# at all, it is a stale credential, which is what `auth_rejected` means. So:
# the OLD token's refusals must land in auth_rejected, and the NEW token's
# refusals past its two routes must land in guest_rejected.
hr "6. counters after the rotation (old-token refusals -> auth_rejected; new-token off-route refusals -> guest_rejected)"
counters
hr done
