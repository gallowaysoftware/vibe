#!/usr/bin/env bash
# C13 bonus (defs.parity) — RE-RUN after #36. The 2026-08-05 run found that an
# uncommitted edit in an already-DIVERGED checkout flipped the level from WARN
# back to OK. #36 made divergence decide over every cell that reports a SHA and
# agreement only over the clean ones. This re-runs the exact sequence.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CHARLIE_ETC=$LAB/etc-charlie
doctor() { "$BIN" fleet doctor --json 2>/dev/null | jq -c '.checks[]|select(.id=="defs.parity")|{level,summary,detail}'; }
restart_charlie() { # $1 = XDG_CONFIG_HOME
  kill -TERM "$(cat "$LAB/run/announce-charlie.pid")" 2>/dev/null; sleep 2
  ( export XDG_CONFIG_HOME=$1 XDG_STATE_HOME=$LAB/state/ann-charlie XDG_RUNTIME_DIR=$LAB/run/rt-ann-charlie
    mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
    nohup "$BIN" fleet announce --cell charlie --registry "$VIBE_API" --token-file "$LAB/state/fleetd/vibe/token" \
      --llama-swap "http://127.0.0.1:$(cell_port charlie)" --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
      >>"$LAB/logs/announce-charlie.log" 2>&1 & echo $! > "$LAB/run/announce-charlie.pid" )
  sleep 25
}
shas() { state | jq -c '[.cells[]|{cell:.name, sha:(.presence.versions.defs_sha // "-"), dirty:(.presence.versions.defs_dirty // false)}]'; }

hr "0. make the shared backends dir a real git checkout"
# git -C, never `cd`. This script's whole job is to run `git init`,
# `git config user.*`, `git add -A` and `git commit` in a scratch tree; with
# a bare `cd` and no `set -e` a wrong FLEETLAB_DIR leaves the shell in the
# operator's OWN repo, where those four commands rewrite its identity and
# commit its uncommitted work under "fleetlab defs".
DEFS=$LAB/etc/vibe/backends
[[ -d $DEFS ]] || { echo "no $DEFS — is the lab up with this FLEETLAB_DIR?" >&2; exit 1; }
git -C "$DEFS" init -q . 2>/dev/null
git -C "$DEFS" config user.email lab@fleetlab; git -C "$DEFS" config user.name fleetlab
git -C "$DEFS" add -A >/dev/null; git -C "$DEFS" commit -qm "fleetlab defs" >/dev/null 2>&1 || true
git -C "$DEFS" log --oneline -1
sleep 25
shas
hr "1. every reporting cell at one clean SHA"
doctor

hr "2. give charlie its OWN checkout, one commit ahead"
rm -rf "${CHARLIE_ETC:?}"; mkdir -p "$CHARLIE_ETC/vibe"
git clone -q "$DEFS" "$CHARLIE_ETC/vibe/backends" || exit 1
ln -sfn "$LAB/etc/vibe/hosts.yaml" "$CHARLIE_ETC/vibe/hosts.yaml"
CDEFS=$CHARLIE_ETC/vibe/backends
git -C "$CDEFS" config user.email lab@fleetlab; git -C "$CDEFS" config user.name fleetlab
printf '# charlie-only comment\n' >> "$CDEFS/lab-embed-c.yaml"
git -C "$CDEFS" commit -qam "charlie: one commit ahead"
git -C "$CDEFS" log --oneline -1
restart_charlie "$CHARLIE_ETC"
shas
hr "3. divergence must be a WARN"
doctor

hr "4. now DIRTY the diverged checkout — strictly worse, so it must STAY a WARN"
printf '# uncommitted edit\n' >> "$CDEFS/lab-embed-c.yaml"
git -C "$CDEFS" status --porcelain
restart_charlie "$CHARLIE_ETC"
shas
doctor

hr "5. control: the same dirt with the checkouts AGREED is an UNKNOWN/OK about dirt, not a hidden WARN"
git -C "$CDEFS" checkout -q . && git -C "$CDEFS" reset -q --hard "$(git -C "$CDEFS" rev-parse HEAD~1)"
printf '# uncommitted edit only\n' >> "$CDEFS/lab-embed-c.yaml"
git -C "$CDEFS" log --oneline -1; git -C "$CDEFS" status --porcelain
restart_charlie "$CHARLIE_ETC"
shas
doctor

hr "6. cleanup: charlie back on the shared checkout"
restart_charlie "$LAB/etc"
shas; doctor
hr done
