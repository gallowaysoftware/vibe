#!/usr/bin/env bash
# C21 L1 — the alias tier, live.
#
# Two questions, one rig:
#
#   1. Is the two-id workaround REAL? A declared alias (router.aliases on a
#      def) must be a first-class front catalog id: listed by the front's
#      /v1/models and routed to its cell. If it is not, the rejection's
#      recommended workaround is broken and the decision has to be rewritten.
#
#   2. Does the shipped code repoint? Two defs on different cells claim one
#      alias, the ROAMING cell is the declared owner. When the laptop leaves,
#      the alias must LEAVE THE CATALOG — not quietly start naming the model
#      on the other cell. Pre-C21 it named the other cell's model, with no
#      event and no status anywhere; that is the invisible version of the
#      feature this phase rejected.
#
# Run against a lab already up:
#   FLEETLAB_DIR=... ./lab.sh up && FLEETLAB_DIR=... ./gate-c21-alias.sh
#
# Side effects on the lab (idempotent, not undone — take the lab down after):
# appends a router: block to three defs, re-renders alpha's and charlie's
# cell configs, and stops+restarts charlie's announcer.
#
# FLEETLAB_DIR must be SHORT. Every daemon binds
# $FLEETLAB_DIR/run/rt-*/vibe/vibe.sock, and a path over the 108-byte
# sun_path limit fails with `bind: invalid argument` whose only visible
# symptom is `lab.sh up` reporting "fleetd did not come up".
set -uo pipefail
source "$(dirname "$0")/gl.sh"

FRONT=http://127.0.0.1:$(cell_port front)
DEFS=$LAB/etc/vibe/backends
FRONT_CFG=$LAB/cells/front/config.yaml
[[ -d $DEFS ]] || { echo "no $DEFS — is the lab up with this FLEETLAB_DIR?" >&2; exit 1; }

fail=0
check() { # $1 label, $2 = 0/1 from the caller
  if [[ $2 -eq 0 ]]; then printf 'PASS  %s\n' "$1"; else printf 'FAIL  %s\n' "$1"; fail=1; fi
}

# peer_of ID — which peer stanza in the front's rendered config serves ID,
# or "" when the catalog does not carry it at all. yq is not assumed.
# Two peers claiming one id prints "a+b" rather than picking one: a rig
# that silently returns the last match would report the expected cell
# while the catalog carried a duplicate.
peer_of() {
  python3 - "$FRONT_CFG" "$1" <<'PY'
import sys, re
path, want = sys.argv[1], sys.argv[2]
peer, inmodels = "", False
hits = []
for line in open(path):
    m = re.match(r"^    (\S+):\s*$", line)
    if m:
        peer, inmodels = m.group(1), False
        continue
    if re.match(r"^        models:\s*$", line):
        inmodels = True
        continue
    if inmodels:
        m = re.match(r"^            - (\S+)\s*$", line)
        if m:
            if m.group(1) == want:
                hits.append(peer)
        else:
            inmodels = False
print("+".join(hits))
PY
}

models_json() { curl -fsS -m 10 "$FRONT/v1/models"; }
has_model() { models_json | python3 -c 'import sys,json;print(any(m["id"]==sys.argv[1] for m in json.load(sys.stdin)["data"]))' "$1"; }

# settle ID WANT — the front polls its config file every 2s, so every
# assertion about the SERVED catalog has to wait for the reload rather than
# read the file fleetd just wrote. A rig that skips this measures the
# previous configuration and calls it a verdict.
settle() {
  local want=$2
  for _ in $(seq 1 15); do
    [[ $(has_model "$1") == "$want" ]] && return 0
    sleep 1
  done
  return 1
}

hr "0. patch the defs: one sole-claimant alias, one contested alias owned by the ROAMING cell"
# best-coder: alpha (always_on) alone claims it. This is the workaround.
python3 - "$DEFS/lab-chat.yaml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
if "router:" not in s:
    s += "router:\n  aliases: [best-coder]\n"
    open(p, "w").write(s)
PY
# best-embed: charlie (roaming) is the DECLARED OWNER, alpha also claims it.
python3 - "$DEFS/lab-embed-c.yaml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
if "router:" not in s:
    s += "router:\n  aliases: [best-embed]\n  alias_owner: true\n"
    open(p, "w").write(s)
PY
python3 - "$DEFS/lab-embed-a.yaml" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
if "router:" not in s:
    s += "router:\n  aliases: [best-embed]\n"
    open(p, "w").write(s)
PY
tail -n 3 "$DEFS/lab-chat.yaml" "$DEFS/lab-embed-a.yaml" "$DEFS/lab-embed-c.yaml"

hr "1. re-render the two cells so their own llama-swaps answer to the aliases"
render_cell alpha || exit 1
render_cell charlie || exit 1
sleep 5   # -watch-config polls at 2s

hr "2. wait for fleetd's presence loop to re-render the front"
# A roaming cell triggers a render on every announce (announce.go), so
# charlie's next heartbeat is the trigger. No restart, no poke.
for _ in $(seq 1 20); do
  [[ -n $(peer_of best-embed) ]] && break
  sleep 3
done
grep -A6 "^    alpha:" "$FRONT_CFG"; grep -A6 "^    charlie:" "$FRONT_CFG"

hr "3. Q1 — a declared alias is a real front catalog id"
settle best-coder True
p=$(peer_of best-coder); echo "front config: best-coder -> peer '$p'"
[[ $p == alpha ]]; check "best-coder renders under its def's cell" $?
[[ $(has_model best-coder) == True ]]; check "the front's /v1/models lists best-coder" $?
echo "--- what the front's own catalog says about the alias ---"
models_json | python3 -c 'import sys,json;print([m for m in json.load(sys.stdin)["data"] if m["id"] in ("best-coder","best-embed")])' 
echo "--- completion for best-coder through the front (routes to alpha's lab-chat) ---"
body=$(curl -fsS -m 300 -X POST "$FRONT/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"best-coder","messages":[{"role":"user","content":"reply with the single word: proof"}],"max_tokens":8}')
echo "$body" | head -c 600; echo
echo "$body" | grep -q '"content"'; check "the front SERVED a completion for the alias" $?

hr "4. Q2 — the contested alias resolves to its DECLARED owner while the owner is present"
p=$(peer_of best-embed); echo "front config: best-embed -> peer '$p'"
[[ $p == charlie ]]; check "best-embed names the declared owner's cell (charlie), not the co-claimant" $?
p=$(peer_of lab-embed-a); [[ $p == alpha ]]; check "the co-claimant still renders under its own name" $?

hr "5. the roaming owner leaves the building"
kill -TERM "$(cat "$LAB/run/announce-charlie.pid")" 2>/dev/null
echo "charlie's announcer stopped at $(ts); waiting for stale (3*interval + 5s) + the prune render"
for _ in $(seq 1 40); do
  grep -q "^    charlie:" "$FRONT_CFG" || break
  sleep 3
done
state | python3 -c 'import sys,json;d=json.load(sys.stdin);print([(c["name"],c.get("presence",{}).get("stale"),c["display"]) for c in d["cells"]])'
echo "--- the front catalog after the prune ---"
sed -n '/^peers:/,$p' "$FRONT_CFG"

hr "6. Q2 — the alias LEFT the catalog and did not repoint"
settle best-embed False
grep -q "^    charlie:" "$FRONT_CFG"; [[ $? -ne 0 ]]; check "the roaming cell is pruned from the front" $?
p=$(peer_of best-embed); echo "front config: best-embed -> peer '${p:-<absent>}'"
[[ -z $p ]]; check "best-embed is ABSENT from the catalog (pre-C21 it named alpha's lab-embed-a)" $?
[[ $(has_model best-embed) == False ]]; check "the front's /v1/models no longer lists best-embed" $?
echo "--- an embeddings request for the departed alias ---"
# Body under $LAB, not a fixed /tmp name: every other rig here keeps its
# artifacts in the lab dir, and `curl -o` on a predictable world-writable
# path follows whatever symlink is already sitting there.
out=$LAB/c21-embed.out
code=$(curl -s -o "$out" -w '%{http_code}' -m 60 -X POST "$FRONT/v1/embeddings" \
  -H 'Content-Type: application/json' -d '{"model":"best-embed","input":"proof"}')
echo "HTTP $code"; head -c 400 "$out"; echo
# 000 is curl failing to connect, which is not evidence about the catalog.
[[ $code != 200 && $code != 000 ]]; check "the departed alias FAILS rather than being answered by another cell's model" $?
p=$(peer_of lab-embed-a); [[ $p == alpha ]]; check "alpha keeps serving its own id throughout" $?

hr "7. the owner comes home"
( export XDG_CONFIG_HOME=$LAB/etc XDG_STATE_HOME=$LAB/state/ann-charlie XDG_RUNTIME_DIR=$LAB/run/rt-ann-charlie
  mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
  nohup "$BIN" fleet announce --cell charlie --registry "$FLEETD_URL" \
    --token-file "$LAB/state/fleetd/vibe/token" --llama-swap "http://127.0.0.1:$(cell_port charlie)" \
    --llama-server "${LLAMA_SERVER:-$HOME/.local/bin/llama-server}" \
    >>"$LAB/logs/announce-charlie.log" 2>&1 & echo $! > "$LAB/run/announce-charlie.pid" )
for _ in $(seq 1 40); do
  [[ $(peer_of best-embed) == charlie ]] && break
  sleep 3
done
p=$(peer_of best-embed); echo "front config: best-embed -> peer '${p:-<absent>}'"
[[ $p == charlie ]]; check "the alias returns with its declared owner (re-add hysteresis honoured)" $?

hr "result"
[[ $fail -eq 0 ]] && echo "C21 L1: PASS" || echo "C21 L1: FAIL"
exit $fail
