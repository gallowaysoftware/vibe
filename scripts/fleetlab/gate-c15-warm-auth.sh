#!/usr/bin/env bash
# fleet-control C15 live gate: the warm credential, against a REAL
# llama-swap configured with apiKeys.
#
# The defect C5 recorded is invisible on the reference fleet, because the
# reference front has no apiKeys. This gate builds the fleet that DOES:
# two real llama-swap v239 processes (a peers-only front with apiKeys +
# one model cell), a real fleetd with a warm target and a warm schedule,
# and a real slim announcer. It then runs the fleet twice — once with no
# credential declared, once with the right one — and prints the raw
# fleet_status warm rows, swap_auth block and doctor rows both ways.
#
# It prints EVIDENCE, not a verdict (scripts/fleetlab/README.md's rule: a
# rig that prints PASS is a rig that can print PASS while wrong).
#
# It is standalone rather than a lab.sh gate for one reason: lab.sh's
# four cells hold 9640-9653, and a second fleet on one box needs its own
# ports. Same isolation discipline: scratch XDG triple, CUDA hidden, kill
# patterns anchored on this rig's own path.
#
#   ./gate-c15-warm-auth.sh          full run (up, both halves, down)
#   ./gate-c15-warm-auth.sh down     clean up after a crash
#
# Ports: 9660 front swap, 9661 heavy swap, 9662 heavy host_probe,
#        9670 fleetd proxy (disabled), 9671 fleetd control plane,
#        5960/5970 upstream startPorts.

set -uo pipefail

LAB=${C15LAB_DIR:-/tmp/fleetlab-c15}
REPO=${C15LAB_REPO:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)}
BIN=$LAB/bin/vibe
LLAMA_SWAP=${LLAMA_SWAP:-$HOME/.local/bin/llama-swap}
LLAMA_SERVER=${LLAMA_SERVER:-$HOME/.local/bin/llama-server}
CHAT_GGUF=${CHAT_GGUF:-$HOME/models/qwen2.5-coder-7b/qwen2.5-coder-7b-instruct-q4_k_m.gguf}

# The front's apiKeys value. A lab value in a lab file: the real fleet's
# key lives in the private repo, never here (ground rule 3).
SWAP_KEY=${C15LAB_KEY:-lab-front-apikey-c15}

ETC=$LAB/etc
STATE=$LAB/state
RUN=$LAB/run
LOGS=$LAB/logs
CELLS=$LAB/cells
FLEETD_URL=http://127.0.0.1:9671
TOKEN_FILE=$STATE/fleetd/vibe/token
KEY_FILE=$LAB/front-swap.key

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

vibe_env() {
  export XDG_CONFIG_HOME=$ETC XDG_STATE_HOME=$STATE/$1 XDG_RUNTIME_DIR=$RUN/rt-$1
  export VIBE_API=$FLEETD_URL
  mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR" 2>/dev/null
}

sweep() {
  # Every pattern is anchored on this rig's own path or its upstream port
  # range, so a production llama-swap on :9000 (upstreams 5800+) and any
  # other lab instance are untouchable.
  pkill -f "llama-swap -config $LAB/" 2>/dev/null
  pkill -f "$LAB/bin/vibe " 2>/dev/null
  pkill -f "llama-server .*--port 59[67][0-9]" 2>/dev/null
  pkill -f "c15-hostprobe" 2>/dev/null
  sleep 1
}

cmd_down() { sweep; note "swept $LAB"; }

wait_http() { # url, seconds, [auth]
  local i
  for ((i=0; i<$2*4; i++)); do
    if [[ -n ${3:-} ]]; then
      curl -fsS -m 2 -H "Authorization: Bearer $3" "$1" >/dev/null 2>&1 && return 0
    else
      curl -fsS -m 2 "$1" >/dev/null 2>&1 && return 0
    fi
    sleep 0.25
  done
  return 1
}

write_configs() { # $1 = "keyed" | "unkeyed" (whether hosts.yaml declares the key)
  mkdir -p "$ETC/vibe/backends" "$CELLS/front" "$CELLS/heavy" "$LOGS" "$RUN"
  printf '%s\n' "$SWAP_KEY" >"$KEY_FILE"; chmod 600 "$KEY_FILE"

  cat >"$ETC/vibe/backends/lab-chat.yaml" <<EOF
name: lab-chat
cell: heavy
backend:
  external: true
  llama_server:
    path: $CHAT_GGUF
    alias: lab-chat
    context: 1024
    parallel: 1
    gpu_layers: 0
    extra_args: ["--threads", "6"]
estimated_vram_gb: 0
lifecycle:
  ttl: 30m
EOF

  local keyline=""
  [[ $1 == keyed ]] && keyline="    swap_key_file: $KEY_FILE"
  cat >"$ETC/vibe/hosts.yaml" <<EOF
fleetd_url: "$FLEETD_URL"

cells:
  front:
    url: "http://127.0.0.1:9660"
    class: always_on
$keyline
  heavy:
    url: "http://127.0.0.1:9661"
    class: always_on
    host_probe: "127.0.0.1:9662"

model_classes:
  lab-chat: chat
EOF

  # The front's operator-owned config half. apiKeys lives HERE, not in the
  # rendered file, because fleetd rewrites the rendered file on every
  # membership transition (C15's front_extras).
  cat >"$LAB/front-extras.yaml" <<EOF
apiKeys:
  - $SWAP_KEY
EOF

  cat >"$ETC/vibe/config.yaml" <<EOF
proxy_port: 9670
disable_proxy: true
http_addr: "127.0.0.1:9671"
llama_binary: $LLAMA_SERVER
fleet_registry: true
fleet:
  front_config: $CELLS/front/config.yaml
  front_extras: $LAB/front-extras.yaml
  timezone: America/Toronto
warm_targets:
  - cell: heavy
    model: lab-chat
    restore_after_idle: 1m
warm_schedule:
  - cron: "*/1 * * * *"
    model: lab-chat
EOF
}

render_cells() {
  local c out sport
  for c in front heavy; do
    sport=5960; [[ $c == heavy ]] && sport=5970
    cat >"$CELLS/$c/extras.yaml" <<EOF
store:
  path: $CELLS/$c/activity.db
EOF
    [[ $c == front ]] && cat >>"$CELLS/$c/extras.yaml" <<EOF
apiKeys:
  - $SWAP_KEY
EOF
    ( vibe_env fleetd
      "$BIN" router render --cell "$c" --extras "$CELLS/$c/extras.yaml" \
        --llama-server "$LLAMA_SERVER" --out "$CELLS/$c/config.yaml" >"$LOGS/render-$c.log" 2>&1
    ) || { cat "$LOGS/render-$c.log" >&2; die "render $c failed"; }
    sed -i "s/^startPort: 5800$/startPort: $sport/" "$CELLS/$c/config.yaml"
  done
}

start_fleet() {
  # CPU-only: the production GPU is holding real models, and this rig must
  # be unable to disturb them.
  export CUDA_VISIBLE_DEVICES=""
  ( cd "$LAB" && CUDA_VISIBLE_DEVICES="" "$LLAMA_SWAP" -config "$CELLS/front/config.yaml" \
      -listen 127.0.0.1:9660 -watch-config >"$LOGS/swap-front.log" 2>&1 & )
  ( cd "$LAB" && CUDA_VISIBLE_DEVICES="" "$LLAMA_SWAP" -config "$CELLS/heavy/config.yaml" \
      -listen 127.0.0.1:9661 -watch-config >"$LOGS/swap-heavy.log" 2>&1 & )
  ( exec -a c15-hostprobe socat TCP-LISTEN:9662,reuseaddr,fork /dev/null >/dev/null 2>&1 & )
  wait_http http://127.0.0.1:9660/health 20 || die "front llama-swap never came up"
  wait_http http://127.0.0.1:9661/health 20 || die "heavy llama-swap never came up"

  ( vibe_env fleetd; "$BIN" daemon >"$LOGS/fleetd.log" 2>&1 & )
  sleep 2
  [[ -f $TOKEN_FILE ]] || die "fleetd never minted a token"
  TOKEN=$(cat "$TOKEN_FILE")
  wait_http "$FLEETD_URL/api/fleet/state" 20 "$TOKEN" || die "fleetd control plane never answered"

  ( vibe_env announce
    "$BIN" fleet announce --cell heavy --registry "$FLEETD_URL" --token-file "$TOKEN_FILE" \
      --llama-swap http://127.0.0.1:9661 --llama-server "$LLAMA_SERVER" \
      >"$LOGS/announce-heavy.log" 2>&1 & )
  sleep 3
}

state()  { curl -fsS -H "Authorization: Bearer $TOKEN" "$FLEETD_URL/api/fleet/state"; }
doctor() { curl -fsS -H "Authorization: Bearer $TOKEN" "$FLEETD_URL/api/fleet/doctor"; }

report() { # $1 = label
  say "$1 — raw fleet_status warm + swap_auth"
  state | jq '{warm: .warm, swap_auth: .swap_auth, cells: [.cells[] | {name, reachable, display, models: [.models[].id]}]}'
  say "$1 — doctor rows for the credential"
  doctor | jq -c '.checks[] | select(.id == "swap.credential" or .id == "front.extras") | {id, cell, level, summary}'
  say "$1 — did fleetd rewrite the front config, and did apiKeys survive it?"
  state | jq -r '"    front_renders (fleetd writes of the front config): \(.front_renders)"'
  grep -c apiKeys "$CELLS/front/config.yaml" | sed 's/^/    apiKeys lines in the rendered front config: /'
  say "$1 — the front is still demanding a key in BOTH halves (the control is fleetd's, not the front's)"
  curl -s -o /dev/null -w '    hand warm, no credential   -> HTTP %{http_code}\n' -m 10 -X POST \
    -H 'Content-Type: application/json' \
    -d '{"model":"lab-chat","max_tokens":1,"messages":[{"role":"user","content":"warm"}]}' \
    http://127.0.0.1:9660/v1/chat/completions
  curl -s -o /dev/null -w '    hand warm, with the key    -> HTTP %{http_code}\n' -m 300 -X POST \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $SWAP_KEY" \
    -d '{"model":"lab-chat","max_tokens":1,"messages":[{"role":"user","content":"warm"}]}' \
    http://127.0.0.1:9660/v1/chat/completions
}

case "${1:-run}" in
  down) cmd_down; exit 0 ;;
esac

[[ -x $LLAMA_SWAP ]]   || die "llama-swap not at $LLAMA_SWAP"
[[ -x $LLAMA_SERVER ]] || die "llama-server not at $LLAMA_SERVER"
[[ -f $CHAT_GGUF ]]    || die "chat GGUF not at $CHAT_GGUF"
command -v jq >/dev/null || die "jq required"

sweep
rm -rf "$LAB"; mkdir -p "$LAB/bin"
say "building vibe into $LAB/bin"
( cd "$REPO" && go build -o "$BIN" ./cmd/vibe ) || die "build failed"

say "HALF 1: the front runs with apiKeys and hosts.yaml declares NO credential"
write_configs unkeyed
render_cells
start_fleet
note "waiting 75s so the warm target and one cron minute both fire"
sleep 75
report "HALF 1 (no credential declared)"

say "HALF 2: the same fleet, with cells.front.swap_key_file declared"
pkill -f "$LAB/bin/vibe daemon" 2>/dev/null; sleep 1
write_configs keyed
( vibe_env fleetd; "$BIN" daemon >>"$LOGS/fleetd.log" 2>&1 & )
sleep 2
TOKEN=$(cat "$TOKEN_FILE")
wait_http "$FLEETD_URL/api/fleet/state" 20 "$TOKEN" || die "fleetd did not come back"
( vibe_env announce
  "$BIN" fleet announce --cell heavy --registry "$FLEETD_URL" --token-file "$TOKEN_FILE" \
    --llama-swap http://127.0.0.1:9661 --llama-server "$LLAMA_SERVER" \
    >>"$LOGS/announce-heavy.log" 2>&1 & )
note "waiting 100s (a CPU 7B cold start is ~25s, plus one cron minute)"
sleep 100
report "HALF 2 (credential declared)"

say "HALF 3: does fleetd's own render keep the front's apiKeys?"
note "stripping apiKeys from the rendered front config, then forcing a membership transition"
sed -i '/^apiKeys:/,+1d' "$CELLS/front/config.yaml"
grep -c apiKeys "$CELLS/front/config.yaml" | sed 's/^/    apiKeys lines after the strip: /'
pkill -f "llama-swap -config $CELLS/heavy/" 2>/dev/null
sleep 20
( cd "$LAB" && CUDA_VISIBLE_DEVICES="" "$LLAMA_SWAP" -config "$CELLS/heavy/config.yaml" \
    -listen 127.0.0.1:9661 -watch-config >>"$LOGS/swap-heavy.log" 2>&1 & )
sleep 45
state | jq -r '"    front_renders after the transition: \(.front_renders)"'
grep -c apiKeys "$CELLS/front/config.yaml" | sed 's/^/    apiKeys lines in the re-rendered front config: /'
grep -o '"msg":"front config[^"]*"' "$STATE/fleetd/vibe/daemon.log" | sort | uniq -c

# ── HALF 4: the adversarial-review pass's three findings, live ─────────
say "HALF 4a: a key file deleted at 03:00 — does the GUEST-readable state document leak its path?"
rm -f "$KEY_FILE"
sleep 20
state | jq -c '.swap_auth.cells[]? | {cell, kind, detail}'
if state | jq -e --arg p "$KEY_FILE" '(.|tostring) | contains($p)' >/dev/null; then
  note "LEAK: /api/fleet/state carries $KEY_FILE (C12 grants this document to the guest bearer)"
else
  note "no path in the state document (the sentence names cells.front.swap_key_file only)"
fi
note "queued commands for heavy (an unresolvable FRONT credential must not be routed around):"
state | jq -r '"    " + ((.cells[] | select(.name == "heavy") | .queued_commands // 0) | tostring) + " queued"' 2>/dev/null \
  || note "    (no queued_commands field on this snapshot)"
grep -o '"msg":"warm-target restore piggybacked on the announce"' "$STATE/fleetd/vibe/daemon.log" | wc -l \
  | sed 's/^/    warm-target restores piggybacked to the cell: /'

say "HALF 4b: fleet.front_extras declared but NOT readable — does the render still delete apiKeys?"
mv "$LAB/front-extras.yaml" "$LAB/front-extras.yaml.moved"
grep -c apiKeys "$CELLS/front/config.yaml" | sed 's/^/    apiKeys lines before the transition: /'
pkill -f "llama-swap -config $CELLS/heavy/" 2>/dev/null
sleep 20
( cd "$LAB" && CUDA_VISIBLE_DEVICES="" "$LLAMA_SWAP" -config "$CELLS/heavy/config.yaml" \
    -listen 127.0.0.1:9661 -watch-config >>"$LOGS/swap-heavy.log" 2>&1 & )
sleep 45
grep -c apiKeys "$CELLS/front/config.yaml" | sed 's/^/    apiKeys lines after the transition: /'
grep -o '"msg":"presence-derived render failed"' "$STATE/fleetd/vibe/daemon.log" | sort | uniq -c
grep -o 'front_extras[^"]*cannot be read' "$STATE/fleetd/vibe/daemon.log" | head -1 | sed 's/^/    refusal: /' 
doctor | jq -c '.checks[] | select(.id == "front.extras") | {id, level, summary}'
mv "$LAB/front-extras.yaml.moved" "$LAB/front-extras.yaml"

say "fleetd log lines about the credential (the daemon logs to its state dir, not stdout)"
grep -o '"msg":"[^"]*\(credential\|API key\)[^"]*"' "$STATE/fleetd/vibe/daemon.log" | sort | uniq -c

say "done — taking the rig down"
sweep
