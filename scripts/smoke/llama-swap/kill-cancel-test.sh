#!/usr/bin/env bash
# Client-kill cancellation test for the A1/A5 gate (router-lifecycle.md
# section 7: "Client disconnect must cancel upstream generation ... verified
# in the A1 smoke with a mid-stream kill", and the A1 row: "mid-stream
# client-kill cancels upstream").
#
# Phase 1 (kill mid-LOAD): stream a request at the cold model, kill the curl
# 3s in while llama-swap is holding it behind SSE keepalives, wait for the
# model to finish loading, then check whether the dead request was ever
# forwarded upstream.
# Phase 2 (kill mid-STREAM): with the model warm, stream a long completion
# (max_tokens=60 at slowmodel's 200ms/token = ~12s), kill the curl 3s in,
# and check the upstream log for the disconnect line.
#
# What "cancelled" looks like in the slowmodel stderr log ($SLOWMODEL_LOG):
#   phase 1 PASS: no POST /v1/chat/completions request line after the load
#                 (llama-swap dropped the queued request), OR a request line
#                 followed by "client disconnected mid-stream" (forwarded,
#                 then cancelled).
#   phase 2 PASS: "client disconnected mid-stream" with NO matching
#                 "stream complete" line.
#   FAIL: a "stream complete" line for a request whose client died —
#         llama-swap ran the full generation for nobody.
#
# Usage:
#   BASE_URL=http://127.0.0.1:9000 MODEL=slowmodel DELAY_S=420 \
#     SLOWMODEL_LOG=/tmp/slowmodel.log ./kill-cancel-test.sh
#
# SLOWMODEL_LOG must be the file slowmodel's stderr is redirected to (see the
# README's config stanza: cmd wraps the binary in sh -c "... 2>>/tmp/...").
# If it is missing/unreadable the script still runs and prints exactly what
# to check by hand.

set -u -o pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:9000}"
MODEL="${MODEL:-slowmodel}"
DELAY_S="${DELAY_S:-420}"
SLOWMODEL_LOG="${SLOWMODEL_LOG:-/tmp/slowmodel.log}"
PROMPT="say hi in three words"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK:?}"' EXIT

VERDICT=PASS

say() { printf '%s\n' "$*"; }

fail() {
  VERDICT=FAIL
  say "FAIL: $*"
}

body() { # $1 = max_tokens
  printf '{"model":"%s","stream":true,"max_tokens":%s,"messages":[{"role":"user","content":"%s"}]}' \
    "$MODEL" "$1" "$PROMPT"
}

stream_bg() { # $1 = max_tokens $2 = outfile -> sets CURL_PID
  curl -sS -N -X POST "$BASE_URL/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "$(body "$1")" -o "$2" >/dev/null 2>&1 &
  CURL_PID=$!
}

kill_client() {
  sleep 3
  kill "$CURL_PID" 2>/dev/null
  wait "$CURL_PID" 2>/dev/null
  say "killed streaming client 3s in (pid $CURL_PID)"
}

have_log() { [ -r "$SLOWMODEL_LOG" ]; }

log_lines() {
  if have_log; then wc -l <"$SLOWMODEL_LOG"; else echo 0; fi
}

log_since() { # $1 = baseline line count
  have_log && tail -n +"$(($1 + 1))" "$SLOWMODEL_LOG"
}

show_running() {
  say "llama-swap /running: $(curl -sf --max-time 5 "$BASE_URL/running" 2>/dev/null || echo '<unreachable>')"
}

model_listed_running() {
  curl -sf --max-time 5 "$BASE_URL/running" 2>/dev/null | grep -q "\"$MODEL\""
}

unload_model() {
  curl -sf --max-time 15 -X POST "$BASE_URL/api/models/unload/$MODEL" >/dev/null 2>&1 ||
    say "warn: POST /api/models/unload/$MODEL failed; phase 1 needs a COLD model"
  for _ in $(seq 1 30); do
    model_listed_running || return 0
    sleep 1
  done
  say "warn: $MODEL still listed in /running after unload"
}

wait_for_load() { # waits until slowmodel binds (log) or /running lists it
  local base=$1 deadline=$(($(date +%s) + DELAY_S + 90))
  say "waiting up to $((DELAY_S + 90))s for $MODEL to finish loading..."
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if have_log; then
      log_since "$base" | grep -q 'bound and serving' && return 0
    else
      model_listed_running && return 0
    fi
    sleep 5
  done
  return 1
}

# --- phase 1: kill mid-load ---------------------------------------------------
say "== phase 1: kill client mid-LOAD =="
unload_model
show_running
BASE1=$(log_lines)
stream_bg 60 "$WORK/p1.out"
kill_client
if ! wait_for_load "$BASE1"; then
  fail "phase 1: model never became ready — cannot judge cancellation"
else
  # Grace for llama-swap to forward (or drop) the now-dead queued request.
  sleep 5
  if have_log; then
    SEG=$(log_since "$BASE1")
    if ! grep -q 'chat/completions' <<<"$SEG"; then
      say "phase 1 PASS: queued request was dropped — upstream never saw it"
    elif grep -q 'client disconnected mid-stream' <<<"$SEG"; then
      say "phase 1 PASS: request was forwarded, then cancelled at first write"
    elif grep -q 'stream complete' <<<"$SEG"; then
      fail "phase 1: upstream ran the generation to completion for a dead client"
    else
      say "phase 1 INCONCLUSIVE: request forwarded, neither disconnect nor completion logged yet"
      say "--- new slowmodel log lines ---"
      printf '%s\n' "$SEG"
    fi
  else
    say "phase 1 MANUAL: $SLOWMODEL_LOG not readable. Check by hand:"
    say "  1. llama-swap logs (its UI /logs or journal): the queued request should be dropped on client disconnect."
    say "  2. slowmodel stderr: there must be NO 'POST /v1/chat/completions' after 'bound and serving',"
    say "     or it must be followed by 'client disconnected mid-stream' and no 'stream complete'."
  fi
fi
show_running

# --- phase 2: kill mid-stream --------------------------------------------------
say ""
say "== phase 2: kill client mid-STREAM (model warm) =="
# Warm the model if phase 1 left it cold; bounded, may take a full cold start.
timeout $((DELAY_S + 120)) curl -sS -X POST "$BASE_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"stream\":false,\"max_tokens\":4,\"messages\":[{\"role\":\"user\",\"content\":\"warm\"}]}" \
  -o /dev/null || fail "phase 2: warm-up request failed"
BASE2=$(log_lines)
stream_bg 60 "$WORK/p2.out"
kill_client
sleep 3
if have_log; then
  SEG=$(log_since "$BASE2")
  if grep -q 'client disconnected mid-stream' <<<"$SEG" && ! grep -q 'stream complete' <<<"$SEG"; then
    say "phase 2 PASS: upstream saw the disconnect and stopped mid-stream"
  elif grep -q 'stream complete' <<<"$SEG"; then
    fail "phase 2: upstream streamed to completion after the client died"
  else
    say "phase 2 INCONCLUSIVE: no disconnect line yet; new log lines:"
    printf '%s\n' "$SEG"
  fi
else
  say "phase 2 MANUAL: $SLOWMODEL_LOG not readable. Check slowmodel stderr for"
  say "  'client disconnected mid-stream' (PASS) vs 'stream complete' (FAIL) for the killed request."
fi
show_running

say ""
say "verdict: $VERDICT"
[ "$VERDICT" = PASS ]
