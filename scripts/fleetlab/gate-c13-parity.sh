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
      --llama-swap http://127.0.0.1:9643 --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
      >>"$LAB/logs/announce-charlie.log" 2>&1 & echo $! > "$LAB/run/announce-charlie.pid" )
  sleep 25
}
shas() { state | jq -c '[.cells[]|{cell:.name, sha:(.presence.versions.defs_sha // "-"), dirty:(.presence.versions.defs_dirty // false)}]'; }

hr "0. make the shared backends dir a real git checkout"
cd "$LAB/etc/vibe/backends"
git init -q . 2>/dev/null; git config user.email lab@fleetlab; git config user.name fleetlab
git add -A >/dev/null; git commit -qm "fleetlab defs" >/dev/null 2>&1 || true
git log --oneline -1
sleep 25
shas
hr "1. every reporting cell at one clean SHA"
doctor

hr "2. give charlie its OWN checkout, one commit ahead"
rm -rf "$CHARLIE_ETC"; mkdir -p "$CHARLIE_ETC/vibe"
git clone -q "$LAB/etc/vibe/backends" "$CHARLIE_ETC/vibe/backends"
ln -sfn "$LAB/etc/vibe/hosts.yaml" "$CHARLIE_ETC/vibe/hosts.yaml"
cd "$CHARLIE_ETC/vibe/backends"
git config user.email lab@fleetlab; git config user.name fleetlab
printf '# charlie-only comment\n' >> lab-embed-c.yaml
git commit -qam "charlie: one commit ahead"
git log --oneline -1
restart_charlie "$CHARLIE_ETC"
shas
hr "3. divergence must be a WARN"
doctor

hr "4. now DIRTY the diverged checkout — strictly worse, so it must STAY a WARN"
printf '# uncommitted edit\n' >> "$CHARLIE_ETC/vibe/backends/lab-embed-c.yaml"
( cd "$CHARLIE_ETC/vibe/backends" && git status --porcelain )
restart_charlie "$CHARLIE_ETC"
shas
doctor

hr "5. control: the same dirt with the checkouts AGREED is an UNKNOWN/OK about dirt, not a hidden WARN"
( cd "$CHARLIE_ETC/vibe/backends" && git checkout -q . && git reset -q --hard "$(git rev-parse HEAD~1)" )
printf '# uncommitted edit only\n' >> "$CHARLIE_ETC/vibe/backends/lab-embed-c.yaml"
( cd "$CHARLIE_ETC/vibe/backends" && git log --oneline -1 && git status --porcelain )
restart_charlie "$CHARLIE_ETC"
shas
doctor

hr "6. cleanup: charlie back on the shared checkout"
restart_charlie "$LAB/etc"
shas; doctor
hr done
