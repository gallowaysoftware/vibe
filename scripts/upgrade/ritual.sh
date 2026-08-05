#!/usr/bin/env bash
# ritual.sh — the only sanctioned way to move the fleet's llama-swap.
#
# canary -> gate -> fleet, as commands rather than as prose. Read
# scripts/upgrade/README.md for WHY each step exists and which parts a
# human still has to watch.
#
#   ./ritual.sh preflight <version>   what is this build, and is it gated?
#   ./ritual.sh canary    <version>   the wire, the behaviours, a real 4-cell fleet
#   ./ritual.sh record    <version>   capture fixtures for a version we have none for
#   ./ritual.sh gate      <version>   the six-client SSE cold-start gate
#   ./ritual.sh pin       <version>   print the FRONT_IMAGE line to paste
#
# <version> is a llama-swap release tag: v247.
#
# It never touches production. Nothing here reads or writes ~/.config/vibe,
# ~/.config/llama-swap, the daemon on :9001 or the router on :9000; the
# processes it starts itself live under $UPGRADE_DIR (default
# /tmp/vibe-upgrade) on ports 9810-9819 with upstreams at 6100+.
#
# The one exception is deliberate and worth knowing before you run it:
# `canary` step 2 hands off to scripts/fleetlab, which binds ITS fixed
# 9600-9799 range and whose `down` sweep is anchored on the shared upstream
# ports. A second lab on this box will be killed by it (futures item 15).
#
# Knobs:
#   UPGRADE_DIR    scratch root                    (default /tmp/vibe-upgrade)
#   LLAMA_SERVER   llama-server binary             (default ~/.local/bin/llama-server)
#   IMAGE_REPO     the front's image repository    (default ghcr.io/mostlygeek/llama-swap)
#   IMAGE_FLAVOUR  the front's image flavour        (default cpu — the front owns no GPU)

set -uo pipefail

DIR=${UPGRADE_DIR:-/tmp/vibe-upgrade}
REPO=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
LLAMA_SERVER=${LLAMA_SERVER:-$HOME/.local/bin/llama-server}
IMAGE_REPO=${IMAGE_REPO:-ghcr.io/mostlygeek/llama-swap}
IMAGE_FLAVOUR=${IMAGE_FLAVOUR:-cpu}
GGUF_URL=https://huggingface.co/ggml-org/models/resolve/main/tinyllamas/stories260K.gguf

BIN=$DIR/bin
LOGS=$DIR/logs
GGUF=$BIN/stories260K.gguf

# say/note go to STDERR: fetch_swap and resolve_digest are read through
# command substitution, and a progress line on stdout becomes part of the
# path the caller then tries to execute.
say()  { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }
note() { printf '   %s\n' "$*" >&2; }
die()  { printf '\033[31mritual: %s\033[0m\n' "$*" >&2; exit 1; }

need_version() {
  [[ ${1:-} =~ ^v[0-9]+$ ]] || die "need a llama-swap release tag, e.g. v247 (got '${1:-}')"
}

# ─── the candidate binary ────────────────────────────────────────────────────

swap_bin() { echo "$BIN/llama-swap-$1"; }

fetch_swap() {
  local v=$1 out; out=$(swap_bin "$v")
  if [[ -x $out ]] && "$out" --version >/dev/null 2>&1; then echo "$out"; return; fi
  mkdir -p "$BIN"
  local os arch
  case "$(uname -s)" in Linux) os=linux;; Darwin) os=darwin;; *) die "unsupported OS";; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) die "unsupported arch";; esac
  local url="https://github.com/mostlygeek/llama-swap/releases/download/${v}/llama-swap_${v#v}_${os}_${arch}.tar.gz"
  note "fetching $url"
  curl -sSfL -o "$DIR/ls.tgz" "$url" || die "no such release: $v"
  tar xzf "$DIR/ls.tgz" -C "$BIN" llama-swap || die "release tarball has no llama-swap"
  mv "$BIN/llama-swap" "$out"
  chmod +x "$out"
  echo "$out"
}

# resolve_digest prints "<tag> <digest>" for the newest image build of this
# release in the front's flavour. Anonymous ghcr token, stdlib curl: the
# repo has no registry client and does not need one.
resolve_digest() {
  local v=$1 tok tag digest last="" page
  tok=$(curl -sSfL "https://ghcr.io/token?scope=repository:${IMAGE_REPO#ghcr.io/}:pull&service=ghcr.io" |
        sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  [[ -n $tok ]] || die "could not get an anonymous ghcr token for $IMAGE_REPO"
  # The tag list is paginated and long (4000+ tags); walk it.
  for _ in $(seq 1 40); do
    page=$(curl -sSfL -H "Authorization: Bearer $tok" \
      "https://ghcr.io/v2/${IMAGE_REPO#ghcr.io/}/tags/list?n=1000${last:+&last=$last}") || break
    local tags; tags=$(echo "$page" | tr ',' '\n' | sed -n 's/.*"\([^"]*\)".*/\1/p' | grep -E "^${v}-${IMAGE_FLAVOUR}-b[0-9]+$")
    [[ -n $tags ]] && tag=$(echo "$tags" | sort -V | tail -1)
    last=$(echo "$page" | tr ',' '\n' | sed -n 's/.*"\([^"]*\)".*/\1/p' | tail -1)
    [[ -z $last ]] && break
    [[ -n ${tag:-} ]] && break
  done
  [[ -n ${tag:-} ]] || die "no ${v}-${IMAGE_FLAVOUR}-b* tag in $IMAGE_REPO"
  digest=$(curl -sSI -H "Authorization: Bearer $tok" \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
    "https://ghcr.io/v2/${IMAGE_REPO#ghcr.io/}/manifests/$tag" |
    tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}')
  [[ -n $digest ]] || die "could not resolve a digest for $tag"
  echo "$tag $digest"
}

have_recording() { [[ -d $REPO/internal/swaptest/fixtures/$1 ]]; }
# grep -E without \b: BSD grep does not have it, and this script claims to
# run on the Darwin cell too.
in_ci_matrix()   { grep -E "^[[:space:]]*llama-swap: \[" "$REPO/.github/workflows/ci.yml" |
                     tr -d ' []' | tr ',' '\n' | grep -qx "$1"; }

fetch_gguf() {
  [[ -s $GGUF ]] && return
  mkdir -p "$BIN"
  note "fetching a 1.2 MB model (a real llama.cpp timings block, no GPU)"
  curl -sSfL -o "$GGUF" "$GGUF_URL" || die "could not fetch $GGUF_URL"
}

# ─── steps ───────────────────────────────────────────────────────────────────

cmd_preflight() {
  local v=$1 bin tagdigest
  need_version "$v"
  mkdir -p "$DIR" "$LOGS"
  say "preflight $v"
  bin=$(fetch_swap "$v") || exit 1
  note "binary:  $("$bin" --version)"
  tagdigest=$(resolve_digest "$v") || exit 1
  note "image:   $IMAGE_REPO:${tagdigest%% *}@${tagdigest##* }"
  local running; running=$("$(swap_bin "$v")" --version 2>&1 | sed -n 's/version: \([^ ]*\).*/\1/p')
  if have_recording "$v"; then
    note "wire:    internal/swaptest/fixtures/$v exists"
  else
    note "wire:    NO recording for $v — 'ritual.sh record $v' captures one"
  fi
  if in_ci_matrix "$v"; then
    note "ci:      ci.yml replays $v on every PR"
  else
    note "ci:      $v is NOT in ci.yml's conformance matrix — add it beside the existing pins"
  fi
  [[ $running == "$v" ]] || note "NOTE: the binary reports '$running', not '$v'"
  note ""
  note "next: ritual.sh canary $v"
}

cmd_record() {
  local v=$1 bin port upstream
  need_version "$v"
  mkdir -p "$DIR" "$LOGS"
  say "record $v"
  bin=$(fetch_swap "$v") || exit 1
  fetch_gguf
  [[ -x $LLAMA_SERVER ]] || die "no llama-server at $LLAMA_SERVER (set LLAMA_SERVER)"
  port=9812; upstream=6100
  cat > "$DIR/record.yaml" <<EOF
healthCheckTimeout: 120
logLevel: info
store:
  path: $DIR/record-store.db
models:
  conformance:
    proxy: http://127.0.0.1:$upstream
    cmd: >
      $LLAMA_SERVER --model $GGUF --host 127.0.0.1 --port $upstream
      --ctx-size 512 --n-gpu-layers 0 --no-warmup
    ttl: 300
EOF
  CUDA_VISIBLE_DEVICES=-1 "$bin" -config "$DIR/record.yaml" -listen "127.0.0.1:$port" > "$LOGS/record.log" 2>&1 &
  local pid=$!
  trap 'kill -TERM '"$pid"' 2>/dev/null' EXIT
  for _ in $(seq 1 60); do curl -sf "http://127.0.0.1:$port/running" >/dev/null && break; sleep 0.5; done
  # A completion first: the recording is of a wire that has carried traffic.
  # A failure here must STOP: TestRecord would otherwise capture an
  # activity page and an events stream from a wire that never carried a
  # request, and a fixture recorded off an idle llama-swap is exactly the
  # shape the in-flight bug hid inside.
  curl -sf -m 120 "http://127.0.0.1:$port/v1/chat/completions" -H 'Content-Type: application/json' \
    -d '{"model":"conformance","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}' >/dev/null || {
    kill -TERM $pid 2>/dev/null; trap - EXIT
    die "the candidate served no completion — read $LOGS/record.log; a recording of an idle wire proves nothing"
  }
  ( cd "$REPO" && SWAPTEST_RECORD_URL="http://127.0.0.1:$port" SWAPTEST_RECORD_MODEL=conformance \
      go test -count=1 -run TestRecord ./internal/swaptest/ )
  local rc=$?
  kill -TERM $pid 2>/dev/null; trap - EXIT
  [[ $rc -eq 0 ]] || die "TestRecord failed — read $LOGS/record.log"
  note "commit internal/swaptest/fixtures/$v BESIDE the existing directories, add $v to"
  note "ci.yml's conformance matrix and to fleetapi.GatedSwapVersions(), then re-run canary."
}

cmd_canary() {
  local v=$1 bin rc
  need_version "$v"
  mkdir -p "$DIR" "$LOGS"
  say "canary $v — step 1/2: the contract and the behaviours"
  bin=$(fetch_swap "$v") || exit 1
  fetch_gguf
  [[ -x $LLAMA_SERVER ]] || die "no llama-server at $LLAMA_SERVER (set LLAMA_SERVER)"
  ( cd "$REPO" && LLAMA_SWAP_BIN="$bin" LLAMA_SERVER_BIN="$LLAMA_SERVER" LLAMA_GGUF="$GGUF" \
      CUDA_VISIBLE_DEVICES=-1 go test -count=1 -timeout 600s -v \
      -run 'TestSwapContract|TestSwapBehaviour|TestRecordingIsCurrent' ./internal/swaptest/ )
  rc=$?
  if [[ $rc -ne 0 ]]; then
    note ""
    note "STOP. The failing invariant names the semantic that moved. Do not pin this build."
    note "If the wire changed on purpose: ritual.sh record $v, teach the fold the new shape,"
    note "then run canary again. Both wires must pass — a heterogeneous fleet is normal."
    exit 1
  fi

  say "canary $v — step 2/2: a real four-cell fleet on the candidate"
  note "scripts/fleetlab stands four REAL llama-swap processes, a real fleetd and both"
  note "announcer shapes on localhost. This is where drain, suspend, the probe guard and"
  note "both warm loops meet the candidate's in-flight wire."
  FLEETLAB_DIR=$DIR/fleetlab LLAMA_SWAP="$bin" "$REPO/scripts/fleetlab/lab.sh" up || {
    FLEETLAB_DIR=$DIR/fleetlab "$REPO/scripts/fleetlab/lab.sh" down
    die "the lab did not come up on $v"
  }
  FLEETLAB_DIR=$DIR/fleetlab LLAMA_SWAP="$bin" "$REPO/scripts/fleetlab/lab.sh" prove | tee "$LOGS/lab-prove-$v.txt"
  rc=${PIPESTATUS[0]}
  FLEETLAB_DIR=$DIR/fleetlab "$REPO/scripts/fleetlab/lab.sh" down
  [[ $rc -eq 0 ]] || die "the lab's proofs failed on $v — see $LOGS/lab-prove-$v.txt"
  note ""
  note "next: ritual.sh gate $v"
}

cmd_gate() {
  local v=$1 bin port delay
  need_version "$v"
  mkdir -p "$DIR" "$LOGS"
  delay=${DELAY_S:-90}
  say "gate $v — the six-client SSE cold-start gate"
  note "This is the one thing no unit test replaces: real clients (curl, the OpenAI and"
  note "Anthropic SDKs, claude, qwen) held open across a cold start by llama-swap's"
  note "loading-state keepalive. TestSwapBehaviour B1 proves the keepalive EXISTS;"
  note "only this proves the clients tolerate it."
  bin=$(fetch_swap "$v") || exit 1
  ( cd "$REPO" && go build -o scripts/smoke/llama-swap/slowmodel/slowmodel ./scripts/smoke/llama-swap/slowmodel ) \
    || die "could not build slowmodel"
  port=9810
  cat > "$DIR/gate.yaml" <<EOF
healthCheckTimeout: 600
sendLoadingState: true
logLevel: info
models:
  slowmodel:
    cmd: sh -c "exec $REPO/scripts/smoke/llama-swap/slowmodel/slowmodel --port 6110 --delay ${delay}s 2>>$LOGS/slowmodel.log"
    proxy: http://127.0.0.1:6110
    checkEndpoint: /health
    ttl: 300
EOF
  "$bin" -config "$DIR/gate.yaml" -listen "127.0.0.1:$port" > "$LOGS/gate-swap.log" 2>&1 &
  local pid=$!
  trap 'kill -TERM '"$pid"' 2>/dev/null' EXIT
  for _ in $(seq 1 60); do curl -sf "http://127.0.0.1:$port/running" >/dev/null && break; sleep 0.5; done
  ( cd "$DIR" && DELAY_S=$delay BASE_URL="http://127.0.0.1:$port" MODEL=slowmodel \
      RESULTS_DIR="$LOGS" "$REPO/scripts/smoke/llama-swap/run-smoke.sh" )
  local rc=$?
  DELAY_S=$delay SLOWMODEL_LOG="$LOGS/slowmodel.log" BASE_URL="http://127.0.0.1:$port" \
    "$REPO/scripts/smoke/llama-swap/kill-cancel-test.sh" || rc=1
  kill -TERM $pid 2>/dev/null; trap - EXIT
  note ""
  note "results: $LOGS/results-*.txt — commit the recorded run beside the existing ones in"
  note "scripts/smoke/llama-swap/ (DELAY_S=420 for the real thing; this run used ${delay}s)."
  note "OWUI and pi stay manual: scripts/smoke/llama-swap/README.md sections 5 and 6."
  [[ $rc -eq 0 ]] || die "the gate reported a failure"
  note ""
  note "next: ritual.sh pin $v"
}

cmd_pin() {
  local v=$1 tagdigest
  need_version "$v"
  say "pin $v"
  # A refusal, not a warning. "Note the warning and stop" is the checklist
  # step nobody performs; a pin with no recording behind it is the
  # 2026-08-05 configuration, printed as if it were a result.
  have_recording "$v" ||
    die "no internal/swaptest/fixtures/$v — canary cannot have passed. Run 'ritual.sh record $v' first."
  tagdigest=$(resolve_digest "$v") || exit 1
  local tag=${tagdigest%% *} digest=${tagdigest##* }
  note "Paste into deploy/front/.env on the FRONT host, and update the default in"
  note "deploy/front/docker-compose.yaml so a fresh deployment inherits it:"
  echo
  echo "FRONT_IMAGE=$IMAGE_REPO:$tag@$digest"
  echo
  note "then, in this order, by hand:"
  note "  1. docker compose up -d       on the front (the digest changed, so the container is recreated)"
  note "  2. vibe fleet doctor          front.image_pin OK, versions.llama_swap names $v"
  note "  3. roll ONE cell, watch it announce, run vibe cell drain --wait against it"
  note "  4. roll the rest; fleetapi.GatedSwapVersions() and ci.yml carry $v"
  note ""
  note "The front is a pure proxy and the cells own the models — so the front moves first,"
  note "and it moves alone. A fleet on two llama-swap versions is a supported state; that"
  note "is why both wires stay gated rather than the newest one replacing the old."
}

case "${1:-}" in
  preflight) shift; cmd_preflight "${1:-}";;
  record)    shift; cmd_record    "${1:-}";;
  canary)    shift; cmd_canary    "${1:-}";;
  gate)      shift; cmd_gate      "${1:-}";;
  pin)       shift; cmd_pin       "${1:-}";;
  *) awk 'NR>1 { if (/^#/) { sub(/^# ?/, ""); print; next } exit }' "${BASH_SOURCE[0]}"; exit 2;;
esac
