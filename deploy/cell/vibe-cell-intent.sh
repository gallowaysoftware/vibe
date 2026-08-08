#!/bin/sh
# vibe-cell-intent.sh — the cell unit's own record of its start and stop
# (fleet-control C24). Driven from the unit, not by a human:
#
#   ExecStartPost=-/usr/local/lib/vibe/vibe-cell-intent.sh started
#   ExecStopPost=-/usr/local/lib/vibe/vibe-cell-intent.sh stopped
#
# It RECORDS. It does not actuate: it starts nothing, stops nothing,
# signals nothing, and the record it writes is one fleetd refuses to hand
# back to this cell as a command. The whole point of the hook is the
# stop nobody declared — `systemctl stop`, a reboot, a crash — and for
# that case the honest record is "the unit stopped at 21:04", never a
# guess at why.
#
# Everything that can go wrong leaves the intent axis UNKNOWN: no entry,
# no stale entry, no "available". A cell with no entry renders DRAINED?
# — a question, which is exactly what the fleet knows. This script
# therefore never fails: it says what it could not do, on stderr, into
# the unit's journal, and exits 0.
#
# Environment (set them in the drop-in, see llama-swap.service.d/):
#   VIBE_FLEETD_URL     fleetd base URL, e.g. http://front-host:9001
#   VIBE_CELL           this cell's name in hosts.yaml, e.g. gpu-cell
#   VIBE_TOKEN_FILE     file holding fleetd's control-plane bearer token
#   VIBE_INTENT_TIMEOUT total seconds to spend, 1-30 (default 3)

set -u

# The reserved reason. It MUST match fleetapi.StopIntentReason byte for
# byte: fleetd keys four behaviours on it — the record is never handed
# back as a command, it loses to the cell's own drained echo, it is never
# counted as an unacked request, and it never explains an absence to the
# always_on alarm. A typo here does not fail loudly; it records an
# ordinary human-looking drain that silences the alarm for a crash.
# internal/vibe/cli's C24 gate pins the two strings together.
VIBE_STOP_REASON='stopped out of band'

say() { echo "vibe-cell-intent: $*" >&2; }

# give_up is the only failure path there is. The axis keeps whatever it
# already had — which after an out-of-band stop is nothing at all — and
# the unit's own lifecycle is not disturbed by our inability to talk.
give_up() {
	say "$1"
	say "intent NOT recorded; the fleet keeps saying UNKNOWN (DRAINED?) for this cell, which is the honest answer"
	exit 0
}

case "${1:-}" in
stopped)
	state=drained
	# systemd hands the stop's outcome to ExecStopPost. It is worth a
	# journal line and it is NOT worth recording: "exit-code" and
	# "success" are the same fact on this axis — the stack is down —
	# and the difference between them is a WHY this hook must not
	# invent.
	say "unit stopped (result=${SERVICE_RESULT:-unknown} status=${EXIT_STATUS:-} code=${EXIT_CODE:-})"
	;;
started)
	# The paired half. It retires the stop record this script wrote and
	# nothing else: fleetd leaves a human's declared drain exactly where
	# it is, so starting the unit inside a declared reclaim does not
	# quietly cancel the reclaim.
	state=serving
	;;
*)
	give_up "usage: ${0##*/} stopped|started"
	;;
esac

url=${VIBE_FLEETD_URL:-}
cell=${VIBE_CELL:-}
token_file=${VIBE_TOKEN_FILE:-}
timeout=${VIBE_INTENT_TIMEOUT:-3}

[ -n "$url" ] || give_up "VIBE_FLEETD_URL is unset"
[ -n "$cell" ] || give_up "VIBE_CELL is unset"
[ -n "$token_file" ] || give_up "VIBE_TOKEN_FILE is unset"

# The cell name goes into a JSON document unescaped, so it is checked
# rather than quoted: hosts.yaml cell names are [A-Za-z0-9._-] and a
# name that is not cannot be recorded by this hook at all.
case "$cell" in
*[!A-Za-z0-9._-]* | '') give_up "VIBE_CELL=$cell is not a plain cell name ([A-Za-z0-9._-])" ;;
esac
case "$timeout" in
'' | *[!0-9]* | 0 | 0*) give_up "VIBE_INTENT_TIMEOUT=$timeout is not a positive integer" ;;
esac
[ "$timeout" -le 30 ] || give_up "VIBE_INTENT_TIMEOUT=$timeout exceeds the 30s ceiling: this runs inside the unit's stop, and a hook that outlives TimeoutStopSec is a shutdown that hangs"

command -v curl >/dev/null 2>&1 || give_up "curl is not installed"
[ -r "$token_file" ] || give_up "token file $token_file is unreadable"

token=''
IFS= read -r token <"$token_file" || true
[ -n "$token" ] || give_up "token file $token_file is empty"

# One request, hard-bounded on both ends: at most 1s to get a connection
# and $timeout in total. No --retry — a retry loop inside a stop hook is
# how a shutdown hangs, and there is nothing here worth waiting for. The
# unreachable case is a supported outcome, not an error to work around.
code=$(
	curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
		--connect-timeout 1 --max-time "$timeout" \
		--header 'Content-Type: application/json' \
		--header "Authorization: Bearer $token" \
		--data "{\"cell\":\"$cell\",\"state\":\"$state\",\"reason\":\"$VIBE_STOP_REASON\"}" \
		"${url%/}/api/fleet/intent"
) || give_up "fleetd at ${url%/} is unreachable or slower than the ${timeout}s bound (curl said why, one line up)"

case "$code" in
200)
	if [ "$state" = drained ]; then
		say "recorded: $cell stopped out of band (fleetd shows DRAINED, with no declared reason)"
	else
		say "recorded: $cell started; any stop record for it is retired"
	fi
	;;
*)
	give_up "fleetd answered HTTP $code"
	;;
esac
