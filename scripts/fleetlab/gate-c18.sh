#!/usr/bin/env bash
# C18 L1-L4 — `vibe model try` against a real llama-swap.
#
# The four claims this rig exists to WATCH rather than assert:
#
#   L1  applying a trial def is adopted by a real `-watch-config` llama-swap
#       within two poll intervals, so the `Live` bit is an observation.
#   L2  that adoption costs what C0 measured — the old upstream drains and a
#       request still running at 30s is force-closed — so the sentence
#       `--now` prints is describing the real thing.
#   L3  the trial id appears in fleet_status for its cell and NEVER in the
#       rendered FRONT config, and both facts reverse on `end`.
#   L4  `end` restores the cell's llama-swap config byte-for-byte.
#
# It uses the lab's `bravo` cell, which is the only lab cell with a full vibe
# cell daemon — and therefore the only one with a `fleet.cell:` for
# `vibe model try` to place a trial on. Everything below is derived from what
# `lab.sh` actually writes; the first cut of this rig named a `lab-chat-b` def
# and a `$LAB/models/` directory that the lab has never created, so it exited
# at its own precondition check.
#
#   bravo's XDG_CONFIG_HOME is $LAB/etc-bravo (fleet.cell: bravo, proxy_port
#   9642). Running under gl.sh's $LAB/etc instead makes this box the FRONT
#   cell, and `try` refuses the front structurally — correctly, and long
#   before it gets anywhere near L1.
#
#   bravo's only def is lab-embed-b (BGE_M3, an EMBEDDING server). So the
#   trial is an embed trial: C18 reads the probe kind off the def's rendered
#   argv, `--embeddings` makes it KindEmbed, and both the warm and the probe
#   use /v1/embeddings. A chat completion against this cell measures a 400.
#
# PORT CONTENTION: scripts/fleetlab binds fixed ports and `down`'s sweep is
# anchored partly on a shared upstream range, so a second lab instance on one
# box is entitled to kill the first's processes (futures item 15). Set
# FLEETLAB_DIR and run this only when you own the lab.
#
# CONFIG PATH: `vibe model try` writes the llama-swap config the same way
# `vibe router render` does, and llama-swap reads whatever its `-config` names.
# In this lab those are two DIFFERENT files: the cells run
# `-config $LAB/cells/<cell>/config.yaml`, while the XDG default under
# bravo's own XDG_CONFIG_HOME is $LAB/etc-bravo/llama-swap/config.yaml. The
# first cut of this rig let `try` write the default, so L1 timed out at 60s
# reporting NOT LIVE and L4 compared a file the command had never touched —
# both gates green-looking and both vacuous. `--config "$CFG"` is what makes
# L1 and L4 observations of anything.
#
# startPort HAZARD: `vibe router render` — which is what `try`'s apply calls —
# hardcodes `startPort: 5800`, production's upstream range on this box. Fixing
# it AFTER the command returns is too late once the config path is right: the
# apply waits for llama-swap to adopt the file and then warms a model through
# it, so the upstream binds 5800 while the rig is still blocked. sport_guard
# rewrites it continuously in the background instead, inside llama-swap's own
# 2s poll, and fix_startport is kept as the fatal backstop.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=${CELL:-bravo}
# Derived, not hardcoded: lab.sh has moved bravo's proxy_port before, and a
# rig that keeps its own copy points at whichever llama-swap used to be
# there. Same for the upstream startPort, which is read off the config this
# rig is about to bank — restoring to anything else fails L4 for a reason
# that has nothing to do with C18.
PORT=${PORT:-$(sed -n 's/^proxy_port: *\([0-9]*\)$/\1/p' "$LAB/etc-bravo/vibe/config.yaml" 2>/dev/null)}
SPORT=${SPORT:-$(sed -n 's/^startPort: *\([0-9]*\)$/\1/p' "$LAB/cells/${CELL}/config.yaml" 2>/dev/null)}
INCUMBENT=${INCUMBENT:-lab-embed-b}
TRIAL=trial-c18
CFG=$LAB/cells/$CELL/config.yaml

# The candidate is a LOCAL copy of a GGUF the lab already has, under a name
# no def serves: this rig gates the sequence and the rollback, not
# HuggingFace's CDN, and `try` refuses outright to download over a path
# another def already serves. `hfdownload.Download` returns immediately for a
# present file whose size it cannot check, which is what makes the fetch step
# a no-op here.
CAND_DIR=${CAND_DIR:-$LAB/trial-models}
CANDIDATE=${CANDIDATE:-$CAND_DIR/c18-candidate.gguf}
SOURCE_GGUF=${SOURCE_GGUF:-${BGE_LARGE:-$HOME/models/bge-large-en-v1.5-gguf/bge-large-en-v1.5-q8_0.gguf}}

# bravo's OWN config + state. gl.sh points these at fleetd; `vibe model try`
# resolves fleet.cell, the backends dir and the journal from them, so running
# under fleetd's would try (and refuse) to place a trial on the front.
export XDG_CONFIG_HOME=$LAB/etc-bravo
export XDG_STATE_HOME=$LAB/state/bravo
export XDG_RUNTIME_DIR=$LAB/run/rt-bravo
# VIBE_API/VIBE_TOKEN were captured by gl.sh before this and still point at
# fleetd, which is where the lease and the hold go.

catalog() { curl -fsS -m 10 "http://127.0.0.1:$PORT/v1/models" | jq -r '.data[].id' | sort; }
running() { curl -fsS -m 10 "http://127.0.0.1:$PORT/running" | jq -c '[.running[]|{model,state}]'; }
frontcfg() { cat "$LAB/cells/front/config.yaml" 2>/dev/null; }
cellmodels() { state | jq -c ".cells[]|select(.name==\"$CELL\")|[.models[].id]"; }

# fix_startport CFG — the render inside `try` writes production's 5800. Every
# failure here is fatal to the rig: everything after it would be measuring
# the wrong process, and on this box those ports belong to production.
fix_startport() {
  grep -q "^startPort: $SPORT\$" "$1" && return 0
  sed -i "s/^startPort: 5800\$/startPort: $SPORT/" "$1"
  grep -q "^startPort: $SPORT\$" "$1" || {
    echo "startPort rewrite did not apply to $1 — REFUSING to continue (5800 is production's range)" >&2
    exit 2
  }
}

# sport_guard CFG — the same rewrite, continuously, for the duration of a
# command that renders WHILE we are blocked on it. `try`'s apply writes the
# config, waits for llama-swap to adopt it and then warms a model through it,
# so a rewrite that only runs afterwards has already let an upstream bind
# production's range. 100ms is well inside llama-swap's 2s -watch-config poll.
GUARD_PID=
sport_guard() {
  ( while :; do
      if grep -q '^startPort: 5800$' "$1" 2>/dev/null; then
        sed -i "s/^startPort: 5800\$/startPort: $SPORT/" "$1"
      fi
      sleep 0.1
    done ) & GUARD_PID=$!
}
sport_unguard() { [[ -n $GUARD_PID ]] && kill "$GUARD_PID" 2>/dev/null; GUARD_PID=; }
trap sport_unguard EXIT

hr "0. preconditions"
[[ -f $LAB/etc-bravo/vibe/config.yaml ]] || { echo "no bravo cell daemon config — run \`lab.sh up\` first"; exit 2; }
[[ -n ${PORT:-} && -n ${SPORT:-} ]] || { echo "could not derive PORT/SPORT from the lab — is it up? (PORT='${PORT:-}' SPORT='${SPORT:-}')"; exit 2; }
[[ -f $LAB/etc/vibe/backends/$INCUMBENT.yaml ]] || { echo "no incumbent def $INCUMBENT"; exit 2; }
[[ -f $SOURCE_GGUF ]] || { echo "no source GGUF at $SOURCE_GGUF — set SOURCE_GGUF"; exit 2; }
mkdir -p "$CAND_DIR"
[[ -f $CANDIDATE ]] || cp "$SOURCE_GGUF" "$CANDIDATE"
echo "# incumbent def:"; cat "$LAB/etc/vibe/backends/$INCUMBENT.yaml"
echo "# candidate: $CANDIDATE ($(stat -c%s "$CANDIDATE") bytes)"
cp "$CFG" "$LAB/c18-pretrial-config.yaml"
echo "# banked the pre-trial config at $LAB/c18-pretrial-config.yaml (L4's reference)"
echo "# catalog before: $(catalog | tr '\n' ' ')"

hr "1. plan + fetch + stage (--dry-run first: nothing must change)"
"$BIN" model try local/c18 --file "$(basename "$CANDIDATE")" --like "$INCUMBENT" --as "$TRIAL" \
  --dest "$CAND_DIR" --config "$CFG" --dry-run --skip-disk-check
echo "# config unchanged after --dry-run: $(cmp -s "$CFG" "$LAB/c18-pretrial-config.yaml" && echo yes || echo NO)"
echo "# journal after --dry-run (must be 'no trial'): $("$BIN" model try status)"

hr "2. L1 — apply, and watch a real -watch-config adopt it"
BEFORE=$(date +%s)
sport_guard "$CFG"
"$BIN" model try local/c18 --file "$(basename "$CANDIDATE")" --like "$INCUMBENT" --as "$TRIAL" \
  --dest "$CAND_DIR" --config "$CFG" --skip-disk-check --now
sport_unguard
fix_startport "$CFG"
echo "# elapsed to adoption: $(( $(date +%s) - BEFORE ))s  (C0 measured the -watch-config poll at 2s)"
echo "# catalog after:  $(catalog | tr '\n' ' ')"
echo "# journal:"; "$BIN" model try status

hr "3. L3 — visible on the CELL, absent from the FRONT"
echo "# fleet_status models for $CELL: $(cellmodels)"
echo "# fleet_status leases for $CELL:  $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.leases")"
echo "# does the rendered front config mention $TRIAL? $(frontcfg | grep -c "$TRIAL") occurrence(s)  <- MUST be 0"
echo "# front render warnings (the exclusion must be LOUD):"
"$BIN" router render --cell front --stdout 2>&1 >/dev/null | grep -i trial ||
  echo "  (none — the lab shares one backends dir between fleetd and bravo, so this IS reachable and its absence is a FAILURE)"

hr "4. L2 — the cost of a reload, watched"
echo "# start a batch embedding request against the incumbent, then touch the config."
echo "# C0's measured behaviour: the new config is live immediately, the OLD upstream"
echo "# drains in-flight work under a hardcoded 30s, then force-closes."
# 3000 chunks, not 200: after the measurement the incumbent is WARM, and a
# batch that finishes in under a second measures nothing about a reload. The
# body goes to /dev/null — piping it into `head` closes the socket and curl
# reports (23) at whatever moment head had enough, which reads exactly like a
# force-close and is not one.
( curl -sS -m 120 -o /dev/null -w "  <- HTTP %{http_code} after %{time_total}s (want a 30s force-close if it was still running)\n" \
    "http://127.0.0.1:$PORT/v1/embeddings" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$INCUMBENT\",\"input\":[$(printf '"chunk %s",' $(seq 1 3000))\"tail\"]}"
  echo "  <- request ended at $(date -Is)" ) &
STREAM=$!
sleep 3
echo "# touching the config at $(date -Is)"
touch "$CFG"
wait $STREAM
echo "# running after the reload: $(running)"

hr "5. L4 — end, and compare the config byte-for-byte"
sport_guard "$CFG"
"$BIN" model try end --config "$CFG"
sport_unguard
fix_startport "$CFG"
echo "# config restored byte-for-byte: $(cmp -s "$CFG" "$LAB/c18-pretrial-config.yaml" && echo YES || echo NO)"
cmp -s "$CFG" "$LAB/c18-pretrial-config.yaml" || diff -u "$LAB/c18-pretrial-config.yaml" "$CFG"
echo "# catalog after end: $(catalog | tr '\n' ' ')"
echo "# fleet_status models for $CELL: $(cellmodels)"
echo "# fleet_status leases for $CELL:  $(state | jq -c ".cells[]|select(.name==\"$CELL\")|.leases")  <- MUST be empty"
echo "# weights kept (no --purge): $(ls -l "$CANDIDATE" 2>/dev/null | wc -l)"
"$BIN" model try status

hr "done — every line above is an observation; nothing here asserts"
