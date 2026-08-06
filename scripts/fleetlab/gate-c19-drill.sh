#!/usr/bin/env bash
# C19 fire drill — kill the front host, stand a standby up from the
# mirror, and time it.
#
# This is the gate that makes the runbook real. A checklist nobody
# performs is worth nothing, and every step below is a step
# docs/runbooks/front-failover.md tells a human to take, executed against
# a real fleetd, a real llama-swap front and three real announcing cells.
#
# What it proves, in order:
#   1. the mirror captures the state that dies with the front host
#   2. `restore` REFUSES while the fleet's own address still answers
#      (two fronts is worse than none, and nothing else would notice)
#   3. killing the front host does not stop inference — the cells keep
#      serving (invariant 4)
#   4. a standby restored from the archive assumes the identity: the
#      SAME control-plane token, the SAME addresses, the declared drain
#      still declared, the append-only ledger intact
#   5. how long that took, measured rather than estimated
#
# DISRUPTIVE: it SIGKILLs the lab's fleetd and its front llama-swap, and
# leaves the standby running in their place. Run `./lab.sh down && ./lab.sh up`
# afterwards to get the lab back to its documented shape. Do not run it
# beside another rig against the same FLEETLAB_DIR.
#
#   FLEETLAB_DIR=/tmp/fleetlab ./gate-c19-drill.sh
#
# Runtime ~3 min, most of it waiting for announce heartbeats.

set -uo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")" || exit 1
# shellcheck source=/dev/null
. ./gl.sh

LLAMA_SWAP=${LLAMA_SWAP:-$HOME/.local/bin/llama-swap}
DRILL=$LAB/c19-drill
MIRROR=$DRILL/mirror
SB=$DRILL/standby                    # the standby box's disks
SB_STATE=$SB/state                   # -> $SB_STATE/vibe is XDG_STATE_HOME/vibe
SB_ETC=$SB/etc                       # -> $SB_ETC/vibe is XDG_CONFIG_HOME/vibe
SB_FRONT=$SB/front-config/config.yaml

FRONT_PORT=9640
FLEETD_PORT=9721
LAB_STATE_DIR=$LAB/state/fleetd/vibe
LAB_ETC_DIR=$LAB/etc/vibe
LAB_FRONT_CFG=$LAB/cells/front/config.yaml

die() { echo "DRILL ABORTED: $*" >&2; exit 1; }
ledger_lines() { [[ -f $1 ]] && wc -l <"$1" | tr -d ' ' || echo 0; }
sha_of() { [[ -f $1 ]] && sha256sum "$1" | cut -c1-12 || echo "(absent)"; }
have() { command -v "$1" >/dev/null 2>&1 || die "need $1 on PATH"; }
have jq
[[ -x $BIN ]] || die "no lab binary at $BIN — run ./lab.sh up first"
[[ -x $LLAMA_SWAP ]] || die "no llama-swap at $LLAMA_SWAP"
state >/dev/null 2>&1 || die "fleetd is not answering at $VIBE_API — run ./lab.sh up first"

rm -rf "$DRILL"
mkdir -p "$MIRROR" "$SB_STATE" "$SB_ETC" "$(dirname "$SB_FRONT")"

# ─────────────────────────────────────────────────────────── 0. seed state
hr "0. state worth losing"
# The append-only ledger is the one file in the state dir that cannot be
# reconstructed from anywhere: cells announce CUMULATIVE totals, so a
# fresh ledger restarts the running total rather than back-filling. Put a
# real metered row in it, through the front, and wait for fleetd's 60s
# flush — a mirror of an empty file proves nothing about a mirror of a
# growing one.
echo "one real completion through the front (loads a 7B on CPU; ~30-60s)"
curl -fsS -m 300 -H 'Content-Type: application/json' \
  "http://127.0.0.1:$FRONT_PORT/v1/chat/completions" \
  -d '{"model":"lab-chat","max_tokens":16,"messages":[{"role":"user","content":"say hi"}]}' \
  | jq -r '.usage // "no usage block"'
LEDGER=$LAB_STATE_DIR/fleet/usage.jsonl
for _ in $(seq 1 60); do [[ -s $LEDGER ]] && break; sleep 3; done
[[ -s $LEDGER ]] || echo "NOTE: no ledger rows after 180s — the survival check below proves less than it should"

# A declared drain is axis 2's headline feature and the thing whose loss
# turns a deliberate outage into a DRAINED? mystery. It also stops
# bravo's llama-swap, which is why bravo is excluded from the
# still-serving evidence below.
mcptext drain_cell '{"cell":"bravo","reason":"c19 fire drill","eta":"23:00"}'
sleep 2
BEFORE_INTENT=$(state | jq -c '.cells[] | select(.name=="bravo") | {display, intent: .intent}')
BEFORE_TOKEN=$(cat "$LAB_STATE_DIR/token")
BEFORE_LEDGER=$(ledger_lines "$LEDGER")
BEFORE_LEDGER_SHA=$(sha_of "$LEDGER")
echo "bravo:        $BEFORE_INTENT"
echo "ledger lines: $BEFORE_LEDGER (sha $BEFORE_LEDGER_SHA)"
echo "token sha:    $(printf %s "$BEFORE_TOKEN" | sha256sum | cut -c1-12)"

# fleet.mirror_max_age is what turns "a mirror ran" into a doctor verdict;
# fleet.front_image is what the standby has to RUN, and a recovery that
# pulls a different llama-swap is how it becomes the v247 incident (C16).
PIN=ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869ec0908538e421172fd576288e87c1bc330acde24517992507218d2c7c
if ! grep -q mirror_max_age "$LAB_ETC_DIR/config.yaml"; then
  cp "$LAB_ETC_DIR/config.yaml" "$DRILL/config.yaml.c19bak"
  sed -i "s|^fleet:$|fleet:\n  mirror_max_age: 36h\n  front_image: $PIN|" "$LAB_ETC_DIR/config.yaml"
  grep -q mirror_max_age "$LAB_ETC_DIR/config.yaml" || die "could not declare mirror_max_age"
  echo "declared fleet.mirror_max_age: 36h and the front's digest pin (both restored on exit)"
fi

# ──────────────────────────────────────────────────────────── 1. the mirror
hr "1. vibe fleet mirror — twice, so the second archive carries the first's receipt"
for i in 1 2; do
  "$BIN" fleet mirror --out "$MIRROR" \
    --state-dir "$LAB_STATE_DIR" --config-dir "$LAB_ETC_DIR" \
    --front-config "$LAB_FRONT_CFG" || die "mirror run $i failed"
  sleep 1
done
hr "1b. verify — every sha256 recomputed off the archive"
"$BIN" fleet mirror verify "$MIRROR" || die "verify failed"

# ───────────────────────────────────────── 2. the refusal, while it is alive
hr "2. restore REFUSES while the fleet's own address still answers"
if "$BIN" fleet mirror restore "$MIRROR" --state-dir "$SB_STATE/vibe" --config-dir "$SB_ETC/vibe" \
     --front-config "$SB_FRONT"; then
  die "restore ran against a LIVE fleet — the takeover guard did not fire"
fi
[[ -e $SB_STATE/vibe/token ]] && die "the refusal still wrote files"
echo "(refused, wrote nothing — correct)"

# ────────────────────────────────────────────── 3. the front host dies
hr "3. SIGKILL fleetd and the front llama-swap (the front host dying)"
T0=$(date +%s.%N)
for name in fleetd swap-front; do
  pid=$(cat "$LAB/run/$name.pid" 2>/dev/null)
  [[ -n ${pid:-} ]] && kill -9 "$pid" 2>/dev/null
  echo "killed $name (pid ${pid:-none})"
done
sleep 1
curl -fsS -m 3 "http://127.0.0.1:$FLEETD_PORT/ui/fleet" >/dev/null 2>&1 && die "fleetd survived the kill"
curl -fsS -m 3 "http://127.0.0.1:$FRONT_PORT/v1/models" >/dev/null 2>&1 && die "the front survived the kill"

hr "3b. invariant 4: the CELLS keep serving with the control plane gone"
for c in alpha:9641 charlie:9643; do
  n=$(curl -fsS -m 5 "http://127.0.0.1:${c#*:}/v1/models" | jq -r '.data | length' 2>/dev/null)
  printf '  %-8s /v1/models -> %s model(s)\n' "${c%%:*}" "${n:-UNREACHABLE}"
done
echo "  (bravo is deliberately drained by step 0 and is expected to be down)"

# ──────────────────────────────────────────────── 4. stand up the standby
hr "4. restore onto the standby box"
"$BIN" fleet mirror restore "$MIRROR" \
  --state-dir "$SB_STATE/vibe" --config-dir "$SB_ETC/vibe" --front-config "$SB_FRONT" \
  || die "restore failed"

# The one edit a lab needs and a real deployment does not: fleet.front_config
# is a path INSIDE fleetd's container in the reference stack (/front-config/
# config.yaml), identical on any host that runs the same compose. Here it is
# an absolute host path that belongs to the box we just killed.
sed -i "s|front_config: .*|front_config: $SB_FRONT|" "$SB_ETC/vibe/config.yaml"

hr "4b. start the standby front and fleetd on the SAME addresses"
mkdir -p "$DRILL/run" "$DRILL/logs"
env CUDA_VISIBLE_DEVICES= "$LLAMA_SWAP" -config "$SB_FRONT" \
  -listen "127.0.0.1:$FRONT_PORT" -watch-config >"$DRILL/logs/standby-front.log" 2>&1 &
echo $! >"$DRILL/run/standby-front.pid"
env XDG_CONFIG_HOME="$SB_ETC" XDG_STATE_HOME="$SB_STATE" XDG_RUNTIME_DIR="$DRILL/run/rt" \
  "$BIN" daemon >"$DRILL/logs/standby-fleetd.log" 2>&1 &
echo $! >"$DRILL/run/standby-fleetd.pid"

cleanup() {
  hr "cleanup"
  for n in standby-fleetd standby-front; do
    p=$(cat "$DRILL/run/$n.pid" 2>/dev/null)
    [[ -n ${p:-} ]] && kill "$p" 2>/dev/null && echo "stopped $n"
  done
  [[ -f $DRILL/config.yaml.c19bak ]] && cp "$DRILL/config.yaml.c19bak" "$LAB_ETC_DIR/config.yaml" &&
    echo "restored the lab's config.yaml"
  echo "the lab's own fleetd and front are still dead: ./lab.sh down && ./lab.sh up"
}
trap cleanup EXIT

# ─────────────────────────────────────────────────────────── 5. the clock
hr "5. recovery timings (from the SIGKILL)"
elapsed() { echo "$(date +%s.%N) $T0" | awk '{printf "%.1fs", $1-$2}'; }
wait_for() { # $1 label, $2 seconds, rest: command
  local label=$1 limit=$2; shift 2
  local i
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then printf '  %-34s %s\n' "$label" "$(elapsed)"; return 0; fi
    sleep 0.5
  done
  printf '  %-34s NOT REACHED in %ss\n' "$label" "$limit"
  return 1
}
wait_for "fleetd answering" 60 curl -fsS -m 3 "http://127.0.0.1:$FLEETD_PORT/ui/fleet"
wait_for "the front serving the catalog" 60 curl -fsS -m 3 "http://127.0.0.1:$FRONT_PORT/v1/models"
export VIBE_TOKEN=$(cat "$SB_STATE/vibe/token" 2>/dev/null)
announced() { [[ $(state | jq -r '[.cells[] | select(.presence.announcing == true and .presence.stale != true)] | length' 2>/dev/null || echo 0) -ge 3 ]]; }
wait_for "all 3 cells announcing again" 120 announced

# ────────────────────────────────────────────────── 6. did the state survive
hr "6. what survived"
AFTER_TOKEN=$(cat "$SB_STATE/vibe/token")
AFTER_INTENT=$(state | jq -c '.cells[] | select(.name=="bravo") | {display, intent: .intent}')
AFTER_LEDGER=$(ledger_lines "$SB_STATE/vibe/fleet/usage.jsonl")
AFTER_LEDGER_SHA=$(sha_of "$SB_STATE/vibe/fleet/usage.jsonl")
printf '  token identical:   %s\n' "$([[ $BEFORE_TOKEN == "$AFTER_TOKEN" ]] && echo yes || echo 'NO — every cell and client would 401')"
printf '  bravo before:      %s\n' "$BEFORE_INTENT"
printf '  bravo after:       %s\n' "$AFTER_INTENT"
printf '  ledger lines:      %s -> %s   (sha %s -> %s)\n' "$BEFORE_LEDGER" "$AFTER_LEDGER" "$BEFORE_LEDGER_SHA" "$AFTER_LEDGER_SHA"
[[ $BEFORE_LEDGER == 0 ]] && echo '  NOTE: the ledger was empty before the kill, so its survival is untested here'

echo
echo "the announcers were never touched: they still hold the token and the registry URL"
echo "from before the kill, which is what 'assuming the identity' means."

hr "6b. fleet doctor on the standby — the receipt travelled in the archive"
"$BIN" fleet doctor 2>&1 | grep -Ei 'mirror|front\.image|fleetd\.' || true

hr "7. the restored control plane can actuate: resume bravo with the token that survived"
mcptext resume_cell '{"cell":"bravo"}'

hr "7b. state document from the standby"
state | jq -r '.cells | sort_by(.name)[] | "  \(.name)\tdisplay=\(.display)\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\tmodels=\((.models//[])|length)"'

hr "done — read the numbers above, not a verdict"
