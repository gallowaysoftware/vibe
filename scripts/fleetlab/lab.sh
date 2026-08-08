#!/usr/bin/env bash
# fleetlab — a real multi-CELL vibe fleet on one box.
#
# Stands up 4 REAL llama-swap v239 processes (a peers-only front + 3 model
# cells with different hosts.yaml classes), a real fleetd (vibe daemon with
# fleet_registry: true) and a real `vibe fleet announce` loop per cell, all
# on scratch XDG dirs so the production fleet at :9000 / ~/.config/vibe is
# never read or written.
#
#   ./lab.sh up      build + write configs + start everything + wait healthy
#   ./lab.sh down    stop everything (idempotent, works after a crash)
#   ./lab.sh status   what is running + a fleetd state summary
#   ./lab.sh prove    run the four end-to-end proofs and print raw output
#   ./lab.sh render   re-render every cell config from the backend defs
#   ./lab.sh env      print the exported env for driving vibe CLI by hand
#   ./lab.sh ports    print this instance's derived port table
#
# Ports are DERIVED from FLEETLAB_PORT_BASE (default 9600 — today's table
# exactly). See ports.sh. At the default base:
#   9640 front llama-swap      9641 alpha   9642 bravo   9643 charlie
#   9651/9652/9653             per-cell host_probe TCP listeners
#   9720 fleetd proxy_port (proxy disabled)   9721 fleetd control plane
#   9723 bravo's cell daemon   9724 the C9 webhook sink
#   5980/5990/6000/6010        each llama-swap's upstream startPort
#
# A production fleet on the same box (llama-swap on :9000, a daemon on
# :9001, upstreams 5800+, ~/.config/vibe, ~/.local/state/vibe) is never
# read or written: every process here gets its own XDG triple under $LAB,
# and every kill pattern is anchored on a lab-only path or on THIS
# instance's own derived upstream window. ports.sh refuses outright a
# base whose windows would cover a production port.
#
# TWO INSTANCES ON ONE BOX need their own FLEETLAB_DIR *and* their own
# FLEETLAB_PORT_BASE (bases are multiples of 200: 9600, 9800, 10000 …).
# Neither half is optional: the dir separates the configs, the base
# separates the ports, and `down` sweeps on both.
#
# Knobs (all optional):
#   FLEETLAB_DIR    scratch root                  (default /tmp/fleetlab)
#   FLEETLAB_PORT_BASE  this instance's 200-port block (default 9600)
#   LLAMA_SWAP      llama-swap v239+ binary       (default ~/.local/bin/llama-swap)
#   LLAMA_SERVER    llama-server binary           (default ~/.local/bin/llama-server)
#   CHAT_GGUF       a small chat GGUF             (the chat-path gates need it)
#   BGE_LARGE       a small embedding GGUF        (alpha + charlie)
#   BGE_M3          a second embedding GGUF       (bravo)
# Small CPU models are the point: the gates are about control-plane
# edges, not throughput, and a lab that cannot saturate a GPU cannot
# disturb the production models resident on one.

set -uo pipefail

LAB=${FLEETLAB_DIR:-/tmp/fleetlab}
REPO=${FLEETLAB_REPO:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)}
# The port table and the collision guard. Sourced, not copied: a second
# copy of the port list is the bug this instance-isolation work exists to
# remove. It exits non-zero on a base that is not a multiple of 200 or
# that would cover a production port.
# shellcheck source=scripts/fleetlab/ports.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/ports.sh"
BIN=$LAB/bin/vibe
LLAMA_SWAP=${LLAMA_SWAP:-$HOME/.local/bin/llama-swap}
LLAMA_SERVER=${LLAMA_SERVER:-$HOME/.local/bin/llama-server}
# A marker only this lab's processes carry, so `sweep` can never match a
# host_probe belonging to another lab instance on the same box.
TAG=$(basename "$LAB")

ETC=$LAB/etc               # XDG_CONFIG_HOME (shared: defs + hosts.yaml + fleetd config)
ETC_BRAVO=$LAB/etc-bravo   # XDG_CONFIG_HOME for bravo's own cell daemon
STATE=$LAB/state           # per-process XDG_STATE_HOME roots live under here
RUN=$LAB/run               # pidfiles + XDG_RUNTIME_DIR roots
LOGS=$LAB/logs
CELLS=$LAB/cells

FLEETD_STATE=$STATE/fleetd
# FLEETD_URL and BRAVO_DAEMON_URL come from ports.sh.
TOKEN_FILE=$FLEETD_STATE/vibe/token

CHAT_GGUF=${CHAT_GGUF:-$HOME/models/qwen2.5-coder-7b/qwen2.5-coder-7b-instruct-q4_k_m.gguf}
BGE_LARGE=${BGE_LARGE:-$HOME/models/bge-large-en-v1.5-gguf/bge-large-en-v1.5-q8_0.gguf}
BGE_M3=${BGE_M3:-$HOME/models/bge-m3-GGUF/bge-m3-Q8_0.gguf}

say()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }

# vibe_env exports the scratch XDG triple. EVERY vibe invocation in this
# script goes through it, so no lab process can reach the real fleet's
# config or state.
vibe_env() { # $1 = state subdir name
  export XDG_CONFIG_HOME=$ETC
  export XDG_STATE_HOME=$STATE/$1
  export XDG_RUNTIME_DIR=$RUN/rt-$1
  export VIBE_API=$FLEETD_URL
  mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"
  chmod 700 "$XDG_RUNTIME_DIR" 2>/dev/null || true
}

cmd_env() {
  cat <<EOF
export XDG_CONFIG_HOME=$ETC
export XDG_STATE_HOME=$FLEETD_STATE
export XDG_RUNTIME_DIR=$RUN/rt-fleetd
export VIBE_API=$FLEETD_URL
export VIBE_TOKEN=\$(cat $TOKEN_FILE 2>/dev/null)
# then, e.g.:
#   \$BIN cell status
#   \$BIN cell drain bravo --wait 30s --reason lab   # bravo has a real cell daemon
#   \$BIN cell resume bravo
#   \$BIN fleet doctor
#   \$BIN cell await bravo --model lab-embed-b --ready --timeout 2m
#   curl -sH "Authorization: Bearer \$VIBE_TOKEN" \$VIBE_API/api/fleet/state | jq .
#   curl -sH "Authorization: Bearer \$VIBE_TOKEN" \$VIBE_API/api/fleet/usage | jq .
#   curl -sH "Authorization: Bearer \$VIBE_TOKEN" -X POST \$VIBE_API/mcp \\
#     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}'
#   xdg-open $FLEETD_URL/ui/fleet
EOF
}

# ---------------------------------------------------------------- pidfiles

pidfile() { echo "$RUN/$1.pid"; }

start_bg() { # $1 name, rest: command
  local name=$1; shift
  local pf; pf=$(pidfile "$name")
  if pid_alive "$name"; then warn "$name already running (pid $(cat "$pf"))"; return 0; fi
  mkdir -p "$RUN" "$LOGS"
  nohup "$@" >>"$LOGS/$name.log" 2>&1 &
  echo $! >"$pf"
}

pid_alive() {
  local pf; pf=$(pidfile "$1")
  [[ -f $pf ]] || return 1
  local pid; pid=$(cat "$pf" 2>/dev/null) || return 1
  [[ -n $pid ]] && kill -0 "$pid" 2>/dev/null
}

stop_one() {
  local name=$1 pf pid
  pf=$(pidfile "$name")
  [[ -f $pf ]] || return 0
  pid=$(cat "$pf" 2>/dev/null)
  if [[ -n ${pid:-} ]] && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null
    for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
  fi
  rm -f "$pf"
}

# sweep kills anything a previous crashed run left behind. Every pattern is
# anchored on THIS instance's identity — its $LAB path, or its own derived
# upstream window — so it can never match the production llama-swap (:9000,
# config in ~/.config, upstreams 5800-5809), the production daemon, or
# ANOTHER LAB INSTANCE.
#
# The upstream half used to be `--port (59[89][0-9]|60[01][0-9])`: a
# constant every instance shared, because every instance had the same
# ports. A second lab's `down` was therefore entitled to kill the first's
# llama-servers, and it surfaces in the other operator's session as a
# phantom bug — a gate that fails for a reason nowhere in their diff. It
# blocked C16's L4 outright. The window is now derived from
# FLEETLAB_PORT_BASE, and ports.sh's multiple-of-200 rule is what
# guarantees two instances' windows do not overlap.
sweep() {
  local pat pids sig
  # One list, walked twice (TERM then KILL). It used to be written out
  # twice, and a pattern added to one copy and not the other is a process
  # that survives `down` for a reason nobody can see.
  local -a patterns=(
    "llama-swap -config $LAB/"
    "$LAB/bin/vibe "
    "$LAB/bin/notify-sink.py"
    "$TAG-hostprobe"
  )
  for sig in TERM KILL; do
    for pat in "${patterns[@]}"; do
      pids=$(pgrep -f -- "$pat" 2>/dev/null | grep -v "^$$\$")
      [[ -n $pids ]] && kill -"$sig" $pids 2>/dev/null
    done
    # llama-server children of a KILL-9'd swap: only this instance's window.
    pids=$(lab_upstream_pids "$LAB_UPSTREAM_LO" "$LAB_UPSTREAM_HI")
    [[ -n $pids ]] && kill -"$sig" $pids 2>/dev/null
    [[ $sig == TERM ]] && sleep 1
  done
  return 0
}

# ---------------------------------------------------------------- configs

write_configs() {
  mkdir -p "$ETC/vibe/backends" "$LOGS" "$RUN" "$CELLS"

  # --- backend defs. `cell:` is what places a def on a cell and turns it
  #     into a peers: stanza on the front render (C2 §6).
  cat >"$ETC/vibe/backends/lab-chat.yaml" <<EOF
name: lab-chat
cell: alpha
backend:
  external: true
  llama_server:
    path: $CHAT_GGUF
    alias: lab-chat
    context: 2048
    parallel: 1
    gpu_layers: 0
    jinja: true
    extra_args: ["--threads", "8"]
estimated_vram_gb: 0
lifecycle:
  ttl: 10m
EOF

  cat >"$ETC/vibe/backends/lab-embed-a.yaml" <<EOF
name: lab-embed-a
cell: alpha
fingerprint: strict
backend:
  external: true
  llama_server:
    path: $BGE_LARGE
    alias: lab-embed-a
    context: 512
    parallel: 1
    gpu_layers: 0
    extra_args: ["--embeddings", "--threads", "4"]
estimated_vram_gb: 0
lifecycle:
  ttl: 10m
EOF

  cat >"$ETC/vibe/backends/lab-embed-b.yaml" <<EOF
name: lab-embed-b
cell: bravo
backend:
  external: true
  llama_server:
    path: $BGE_M3
    alias: lab-embed-b
    context: 512
    parallel: 1
    gpu_layers: 0
    extra_args: ["--embeddings", "--threads", "4"]
estimated_vram_gb: 0
lifecycle:
  ttl: 10m
EOF

  cat >"$ETC/vibe/backends/lab-embed-c.yaml" <<EOF
name: lab-embed-c
cell: charlie
backend:
  external: true
  llama_server:
    path: $BGE_LARGE
    alias: lab-embed-c
    context: 512
    parallel: 1
    gpu_layers: 0
    extra_args: ["--embeddings", "--threads", "4"]
estimated_vram_gb: 0
lifecycle:
  ttl: 10m
EOF

  # --- hosts.yaml: the single source of cell membership. Three classes,
  #     which is what C3's render policy and C9's alarm column key on.
  cat >"$ETC/vibe/hosts.yaml" <<EOF
fleetd_url: "$FLEETD_URL"

cells:
  front:
    url: "http://127.0.0.1:$(cell_port front)"
    class: always_on
    power: {source: declared, watts_idle: 12, watts_busy: 20}
  alpha:
    url: "http://127.0.0.1:$(cell_port alpha)"
    class: always_on
    host_probe: "127.0.0.1:$(cell_probe alpha)"
    power: {source: declared, watts_idle: 80, watts_busy: 300}
    capital_cost: 1200
    capital_note: "lab cell, synthetic number"
  bravo:
    url: "http://127.0.0.1:$(cell_port bravo)"
    class: opportunistic
    host_probe: "127.0.0.1:$(cell_probe bravo)"
    daemon_url: "$BRAVO_DAEMON_URL"
    token_file: "$STATE/bravo/vibe/token"
    wake: {cmd: "$LAB/bin/fake-wake.sh bravo"}
    power: {source: declared, watts_idle: 40, watts_busy: 120}
  charlie:
    url: "http://127.0.0.1:$(cell_port charlie)"
    class: roaming
    host_probe: "127.0.0.1:$(cell_probe charlie)"

model_classes:
  lab-embed-a: embed
  lab-embed-b: embed
  lab-embed-c: embed
  lab-chat: chat

pricing:
  electricity_price_per_kwh: 0.15
  models:
    lab-chat:
      twin: "Qwen/Qwen2.5-Coder-7B-Instruct"
      counterfactual: interactive
EOF

  # --- fleetd config. disable_proxy so nothing binds a router port; the
  #     control plane is base+121. front_config points at the front cell's
  #     -watch-config file, which is what turns on C3's render loop.
  cat >"$ETC/vibe/config.yaml" <<EOF
proxy_port: $LAB_PROXY_PORT
disable_proxy: true
http_addr: "127.0.0.1:$LAB_FLEETD_PORT"
llama_binary: $LLAMA_SERVER
fleet_registry: true
fleet:
  front_config: $CELLS/front/config.yaml
  timezone: America/Toronto
  guest_token_file: $FLEETD_STATE/vibe/guest-token
  notify:
    url: "http://127.0.0.1:$LAB_NOTIFY_PORT/fleetlab"
    interval: 15s
    rate_per_hour: 60
    burst: 10
warm_targets:
  - cell: alpha
    model: lab-chat
    restore_after_idle: 2m
probe_targets:
  - cell: alpha
    model: lab-chat
    every: 15m
EOF

  # --- bravo's own cell daemon. proxy_port IS bravo's llama-swap port with
  #     the proxy disabled — that is the production shape (llama-swap owns
  #     the router port) and it is what the in-daemon announce loop uses as
  #     its local llama-swap URL (daemon/announce.go).
  mkdir -p "$ETC_BRAVO/vibe"
  ln -sfn "$ETC/vibe/backends" "$ETC_BRAVO/vibe/backends"
  ln -sfn "$ETC/vibe/hosts.yaml" "$ETC_BRAVO/vibe/hosts.yaml"
  cat >"$ETC_BRAVO/vibe/config.yaml" <<EOF
proxy_port: $(cell_port bravo)
disable_proxy: true
http_addr: "127.0.0.1:$LAB_BRAVO_DAEMON_PORT"
bind_all: false
llama_binary: $LLAMA_SERVER
cell_cmds:
  drain: "$LAB/bin/swapctl.sh bravo stop"
  resume: "$LAB/bin/swapctl.sh bravo start"
  suspend: "$LAB/bin/fake-suspend.sh bravo"
fleet:
  cell: bravo
  registry_url: "$FLEETD_URL"
  token_file: "$TOKEN_FILE"
  timezone: America/Toronto
EOF

  # --- the cell verbs. A lab stand-in for `systemctl --user stop/start
  #     llama-swap`: same pidfile the harness uses, so `lab.sh down` still
  #     cleans up whatever state a drain/resume left behind.
  mkdir -p "$LAB/bin"
  cat >"$LAB/bin/swapctl.sh" <<EOF
#!/usr/bin/env bash
# swapctl.sh <cell> <start|stop|status> — the lab's cell_cmds drain/resume.
set -uo pipefail
LAB=$LAB
LLAMA_SWAP=$LLAMA_SWAP
EOF
  cat >>"$LAB/bin/swapctl.sh" <<'EOF'
cell=${1:?cell}; act=${2:?start|stop|status}
pf=$LAB/run/swap-$cell.pid
cfg=$LAB/cells/$cell/config.yaml
port=$(sed -n "s/^$cell:\([0-9]*\)$/\1/p" "$LAB/cells/ports")
case $act in
  stop)
    [[ -f $pf ]] || { echo "swapctl: $cell already stopped" >&2; exit 0; }
    pid=$(cat "$pf"); kill -TERM "$pid" 2>/dev/null
    for _ in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || break; sleep 0.25; done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
    rm -f "$pf"; echo "swapctl: stopped $cell (pid $pid)" ;;
  start)
    if [[ -f $pf ]] && kill -0 "$(cat "$pf")" 2>/dev/null; then echo "swapctl: $cell already running" >&2; exit 0; fi
    nohup setsid env CUDA_VISIBLE_DEVICES= "$LLAMA_SWAP" -config "$cfg" -listen "127.0.0.1:$port" -watch-config \
      >>"$LAB/logs/swap-$cell.log" 2>&1 &
    echo $! >"$pf"; echo "swapctl: started $cell on $port (pid $(cat "$pf"))" ;;
  status)
    if [[ -f $pf ]] && kill -0 "$(cat "$pf")" 2>/dev/null; then echo up; else echo down; exit 1; fi ;;
esac
EOF
  chmod +x "$LAB/bin/swapctl.sh"

  # Neither of these does anything to the box. C14's suspend and wake are
  # declared commands; the lab declares harmless ones so the verbs can be
  # driven without putting a workstation to sleep.
  for v in fake-suspend fake-wake; do
    cat >"$LAB/bin/$v.sh" <<EOF
#!/usr/bin/env bash
echo "\$(date -Is) $v \$*" >> $LOGS/cell-verbs.log
EOF
    chmod +x "$LAB/bin/$v.sh"
  done

  # --- C9's webhook sink: a local ntfy stand-in that appends every
  #     delivery to a log, so alarm firing is observable without a network.
  cat >"$LAB/bin/notify-sink.py" <<'EOF'
import http.server, sys, datetime
LOG = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n).decode('utf-8', 'replace')
        hdr = {k: v for k, v in self.headers.items() if k.lower().startswith('x-') or k.lower() in ('title', 'priority', 'tags')}
        with open(LOG, 'a') as f:
            f.write(f"{datetime.datetime.now().isoformat()} {self.path} {hdr} {body}\n")
        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[2])), H).serve_forever()
EOF

  # port table swapctl.sh reads (it must not source lab.sh)
  local e n p
  : >"$CELLS/ports"
  for e in "${CELL_LIST[@]}"; do IFS=: read -r n p _ _ _ <<<"$e"; echo "$n:$p" >>"$CELLS/ports"; done

  # --- per-cell llama-swap extras: C7a §0's persistent activity store,
  #     the config production does not have yet.
  local c
  for c in $(all_cells); do
    mkdir -p "$CELLS/$c"
    cat >"$CELLS/$c/extras.yaml" <<EOF
store:
  path: $CELLS/$c/activity.db
EOF
  done
}

render_cells() {
  local c out sport
  for c in $(all_cells); do
    mkdir -p "$CELLS/$c"
    out=$CELLS/$c/config.yaml
    sport=$(cell_sport "$c")
    ( vibe_env fleetd
      "$BIN" router render --cell "$c" --extras "$CELLS/$c/extras.yaml" \
        --llama-server "$LLAMA_SERVER" --out "$out" >"$LOGS/render-$c.log" 2>&1
    ) || { warn "render $c failed:"; cat "$LOGS/render-$c.log" >&2; return 1; }
    # `vibe router render` hardcodes startPort: 5800 and --extras refuses to
    # override a key the renderer already emitted, so a box hosting several
    # llama-swap cells has to rewrite it here or every cell fights for the
    # same upstream ports (and with production's 5800-5809).
    sed -i "s/^startPort: 5800$/startPort: $sport/" "$out"
    grep -q "^startPort: $sport$" "$out" || warn "$c: startPort rewrite did not apply"
  done
}

# ---------------------------------------------------------------- up

# A SIGKILLed llama-swap leaves its llama-server children holding the
# cell's upstream ports, and the replacement swap then 500s every request
# with "upstream command exited prematurely". Reap them for any cell whose
# swap is not currently running; a live cell's own children are left alone.
reap_orphan_upstreams() {
  local c sport pids
  for c in $(all_cells); do
    pid_alive "swap-$c" && continue
    sport=$(cell_sport "$c")
    # This cell's own ten upstream ports, numerically. The old
    # `${sport:0:3}[0-9]` prefix regex only worked while every startPort
    # happened to be a multiple of ten with a shared three-digit stem.
    pids=$(lab_upstream_pids "$sport" "$((sport + 9))")
    if [[ -n $pids ]]; then
      warn "reaping orphaned upstream(s) on $sport-$((sport + 9)) for cell $c: $pids"
      kill -TERM $pids 2>/dev/null; sleep 1; kill -KILL $pids 2>/dev/null
    fi
  done
  return 0
}

wait_http() { # url, seconds, [extra curl args...]
  local url=$1 secs=$2; shift 2
  for _ in $(seq 1 $((secs * 2))); do
    curl -fsS -m 2 "$@" "$url" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

cmd_up() {
  command -v go >/dev/null || die "go not on PATH"
  [[ -x $LLAMA_SWAP ]] || die "llama-swap not at $LLAMA_SWAP"
  [[ -x $LLAMA_SERVER ]] || die "llama-server not at $LLAMA_SERVER"
  [[ -f $CHAT_GGUF ]] || warn "chat gguf missing: $CHAT_GGUF (chat-path gates will not run)"

  say "building vibe from the working tree"
  mkdir -p "$LAB/bin"
  ( cd "$REPO" && go build -o "$BIN" ./cmd/vibe ) || die "go build failed"
  "$BIN" --version >/dev/null 2>&1 || true

  say "writing configs under $LAB"
  write_configs

  say "rendering per-cell llama-swap configs (vibe router render --cell N --extras)"
  render_cells || die "render failed"

  say "starting the C9 notification sink on $LAB_NOTIFY_PORT"
  start_bg notify-sink python3 "$LAB/bin/notify-sink.py" "$LOGS/notify.log" "$LAB_NOTIFY_PORT"

  say "starting host_probe listeners"
  local c port
  for c in $(model_cells); do
    port=$(cell_probe "$c")
    start_bg "probe-$c" socat -T 5 "TCP-LISTEN:$port,reuseaddr,fork,bind=127.0.0.1" \
      SYSTEM:"echo $TAG-hostprobe-$c"
  done

  # CUDA_VISIBLE_DEVICES="" is load-bearing, not tidiness: even with
  # --n-gpu-layers 0 llama.cpp still builds a CUDA backend and reserves
  # scheduler buffers, which aborts in libggml-cuda when the box's real
  # GPU is full (production holds ~31 of 32 GiB here). Hiding the device
  # makes the lab cells genuinely CPU-only and unable to disturb the
  # production models resident on the card.
  say "starting llama-swap cells (CPU-only: CUDA_VISIBLE_DEVICES=\"\")"
  reap_orphan_upstreams
  for c in $(all_cells); do
    start_bg "swap-$c" env CUDA_VISIBLE_DEVICES= "$LLAMA_SWAP" -config "$CELLS/$c/config.yaml" \
      -listen "127.0.0.1:$(cell_port "$c")" -watch-config
  done
  for c in $(all_cells); do
    wait_http "http://127.0.0.1:$(cell_port "$c")/v1/models" 20 \
      || die "cell $c did not answer /v1/models — see $LOGS/swap-$c.log"
  done

  say "starting bravo's cell daemon (in-daemon announce + cell_cmds) on $LAB_BRAVO_DAEMON_PORT"
  ( export XDG_CONFIG_HOME=$ETC_BRAVO XDG_STATE_HOME=$STATE/bravo XDG_RUNTIME_DIR=$RUN/rt-bravo
    mkdir -p "$XDG_STATE_HOME" "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
    start_bg cell-bravo "$BIN" daemon )
  for _ in $(seq 1 60); do [[ -s $STATE/bravo/vibe/token ]] && break; sleep 0.25; done
  [[ -s $STATE/bravo/vibe/token ]] || warn "bravo cell daemon never minted a token — see $LOGS/cell-bravo.log"

  say "starting fleetd (vibe daemon, fleet_registry: true) on $LAB_FLEETD_PORT"
  ( vibe_env fleetd; start_bg fleetd "$BIN" daemon )
  wait_http "$FLEETD_URL/ui/fleet" 30 || die "fleetd did not come up — see $LOGS/fleetd.log"
  for _ in $(seq 1 40); do [[ -s $TOKEN_FILE ]] && break; sleep 0.25; done
  [[ -s $TOKEN_FILE ]] || die "fleetd never minted $TOKEN_FILE"

  say "starting slim announcers (vibe fleet announce) for $(slim_cells)"
  for c in $(slim_cells); do
    ( vibe_env "ann-$c"
      start_bg "announce-$c" "$BIN" fleet announce \
        --cell "$c" --registry "$FLEETD_URL" --token-file "$TOKEN_FILE" \
        --llama-swap "http://127.0.0.1:$(cell_port "$c")" \
        --llama-server "$LLAMA_SERVER" )
  done

  say "waiting for the first announce wave"
  local n
  for _ in $(seq 1 60); do
    n=$(state | jq -r '[.cells[] | select(.presence.announcing == true)] | length' 2>/dev/null)
    [[ ${n:-0} -ge 3 ]] && break
    sleep 1
  done
  say "up. $(cmd_env | grep -c export) env lines available via: $0 env"
  cmd_status
}

# ---------------------------------------------------------------- down

cmd_down() {
  local c
  for c in $(slim_cells); do stop_one "announce-$c"; done
  stop_one cell-bravo
  stop_one fleetd
  for c in $(all_cells); do stop_one "swap-$c"; done
  for c in $(model_cells); do stop_one "probe-$c"; done
  stop_one notify-sink
  sweep
  say "down (idempotent)."
}

# ---------------------------------------------------------------- status

token() { cat "$TOKEN_FILE" 2>/dev/null; }
state() { curl -fsS -m 10 -H "Authorization: Bearer $(token)" "$FLEETD_URL/api/fleet/state" 2>/dev/null; }

cmd_status() {
  local name pf
  printf '%-16s %-8s %s\n' PROCESS PID STATE
  for name in notify-sink probe-alpha probe-bravo probe-charlie \
              swap-front swap-alpha swap-bravo swap-charlie \
              fleetd cell-bravo announce-alpha announce-charlie; do
    pf=$(pidfile "$name")
    if pid_alive "$name"; then printf '%-16s %-8s %s\n' "$name" "$(cat "$pf")" up
    else printf '%-16s %-8s %s\n' "$name" "-" down; fi
  done
  echo
  local s; s=$(state)
  if [[ -z $s ]]; then warn "fleetd state unavailable at $FLEETD_URL"; return 0; fi
  echo "fleetd $FLEETD_URL/api/fleet/state:"
  echo "$s" | jq -r '.cells | sort_by(.name)[]
    | "  \(.name)\tclass=\(.class)\tdisplay=\(.display)\treachable=\(.reachable)\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\tmodels=\((.models//[])|length)\tseq=\(.presence.seq // "-")"
  ' 2>/dev/null || echo "$s" | head -c 2000
}

# ---------------------------------------------------------------- prove

hr() { printf '\n\033[1m--- %s\033[0m\n' "$*"; }

cmd_prove() {
  local tok; tok=$(token)
  local alpha; alpha=http://127.0.0.1:$(cell_port alpha)

  hr "PROOF 1: fleetd state — every cell present with its hosts.yaml class"
  state | jq '[.cells | sort_by(.name)[] | {name, class, url, display, reachable, host_reachable,
      announcing: (.presence.announcing // false), stale: (.presence.stale // false),
      seq: .presence.seq, models: ((.models//[])|map(.id))}]'

  hr "PROOF 2: a real request through a cell, then that cell's /api/metrics/activity"
  echo "# POST $alpha/v1/embeddings model=lab-embed-a"
  curl -fsS -m 120 "$alpha/v1/embeddings" -H 'Content-Type: application/json' \
    -d '{"model":"lab-embed-a","input":"fleetlab proof two"}' | jq '{model, usage}'
  echo "# POST $alpha/v1/chat/completions model=lab-chat"
  curl -fsS -m 300 "$alpha/v1/chat/completions" -H 'Content-Type: application/json' \
    -d '{"model":"lab-chat","messages":[{"role":"user","content":"reply with the single word: proof"}],"max_tokens":8}' \
    | jq '{model, content: .choices[0].message.content, usage, timings: {prompt_n: .timings.prompt_n, cache_n: .timings.cache_n, predicted_n: .timings.predicted_n}}'
  echo "# GET $alpha/api/metrics/activity"
  curl -fsS -m 10 "$alpha/api/metrics/activity" | jq '{total, rows: [.data[] | {id, model, req_path, resp_status_code, tokens, duration_ms}]}'
  echo "# persistent store on disk (C7a §0 — the config production lacks):"
  ls -la "$CELLS"/*/activity.db 2>/dev/null

  hr "PROOF 3: announce loop reaching fleetd — last_seen advancing"
  echo "# sample 1"
  state | jq -r '.cells | sort_by(.name)[] | "\(.name)\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\tseq=\(.presence.seq // "-")\treceived_at=\(.presence.received_at // "-")"'
  echo "# sleeping 20s..."; sleep 20
  echo "# sample 2"
  state | jq -r '.cells | sort_by(.name)[] | "\(.name)\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\tseq=\(.presence.seq // "-")\treceived_at=\(.presence.received_at // "-")"'

  hr "PROOF 4: kill charlie's announcer -> it goes stale on the fleetd side"
  echo "# SIGKILL (no clean withdraw) so staleness, not the goodbye, is what fires"
  local pid; pid=$(cat "$(pidfile announce-charlie)" 2>/dev/null)
  [[ -n ${pid:-} ]] && kill -KILL "$pid" 2>/dev/null && rm -f "$(pidfile announce-charlie)"
  local i
  for i in $(seq 1 14); do
    sleep 5
    echo "# t+$((i*5))s"
    state | jq -r '.cells[] | select(.name=="charlie") | "charlie\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\treachable=\(.reachable)\tdisplay=\(.display)\tmodels=\((.models//[])|length)\treceived_at=\(.presence.received_at // "-")"'
    [[ $(state | jq -r '.cells[] | select(.name=="charlie") | .presence.stale // false') == true ]] && break
  done
  echo "# restart charlie's announcer"
  ( vibe_env ann-charlie
    start_bg announce-charlie "$BIN" fleet announce --cell charlie --registry "$FLEETD_URL" \
      --token-file "$TOKEN_FILE" --llama-swap "http://127.0.0.1:$(cell_port charlie)" --llama-server "$LLAMA_SERVER" )
  sleep 20
  state | jq -r '.cells[] | select(.name=="charlie") | "charlie\tannouncing=\(.presence.announcing // false)\tstale=\(.presence.stale // false)\tdisplay=\(.display)\tmodels=\((.models//[])|length)"'
}

# ---------------------------------------------------------------- main

case "${1:-}" in
  up)     cmd_up ;;
  down)   cmd_down ;;
  status) cmd_status ;;
  prove)  cmd_prove ;;
  render) render_cells && say "rendered" ;;
  env)    cmd_env ;;
  ports)  lab_ports_table ;;
  state)  state | jq . ;;
  *) sed -n '2,42p' "$0"; exit 2 ;;
esac
