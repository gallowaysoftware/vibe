#!/usr/bin/env bash
# Six-client cold-start smoke rig for llama-swap's sendLoadingState keepalive
# (docs/design/router-lifecycle.md section 6.3, roadmap gate A1; re-run
# through the peer hop for A5 by pointing BASE_URL at the front router).
#
# Usage:
#   BASE_URL=http://127.0.0.1:9000 MODEL=slowmodel DELAY_S=420 ./run-smoke.sh
#
# Requires llama-swap already running with the slowmodel stanza from
# README.md. By default the model is unloaded before EVERY client so each
# one experiences the full cold start (COLD_EACH=0 for a quick warm pass).
# Every client is wrapped in timeout(1) so the rig never hangs.
# Gate semantics: PASS = streamed answer OR cleanly surfaced error;
# FAIL = hang past DELAY_S+120s or garbage.

set -u -o pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:9000}"
MODEL="${MODEL:-slowmodel}"
DELAY_S="${DELAY_S:-420}"
FIRST_BYTE_BUDGET_S="${FIRST_BYTE_BUDGET_S:-5}"
COLD_EACH="${COLD_EACH:-1}"
PROMPT="say hi in three words"
QWEN_BIN="${QWEN_BIN:-$HOME/.nvm/versions/node/v24.15.0/bin/qwen}"
CLAUDE_BIN="${CLAUDE_BIN:-claude}"
RESULTS_DIR="${RESULTS_DIR:-$PWD}"
TIMEBOX=$((DELAY_S + 120))
RESULTS_FILE="$RESULTS_DIR/results-$(date +%Y%m%d-%H%M%S).txt"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ROWS=()
FAILED=0

say() { printf '%s\n' "$*"; }

record() { # client status duration_s first_byte_s notes
  ROWS+=("$1|$2|$3|$4|$5")
  [ "$2" = FAIL ] && FAILED=1
  say "[$1] $2 (${3}s) $5"
}

now() { date +%s.%N; }

elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b - a}'; }

openai_body() { # $1 = true|false (stream)
  printf '{"model":"%s","stream":%s,"max_tokens":48,"messages":[{"role":"user","content":"%s"}]}' \
    "$MODEL" "$1" "$PROMPT"
}

model_listed_running() {
  curl -sf --max-time 5 "$BASE_URL/running" 2>/dev/null | grep -q "\"$MODEL\""
}

# Best-effort: unload so the next client sees a real cold start. Endpoint per
# router-lifecycle.md section 4.1; tolerate absence (older llama-swap) with a
# warning so a warm run is at least labelled as such.
unload_model() {
  [ "$COLD_EACH" = 1 ] || return 0
  curl -sf --max-time 15 -X POST "$BASE_URL/api/models/unload/$MODEL" >/dev/null 2>&1 ||
    say "warn: POST /api/models/unload/$MODEL failed; next client may run warm"
  for _ in $(seq 1 30); do
    model_listed_running || return 0
    sleep 1
  done
  say "warn: $MODEL still listed in /running after unload"
}

# --- client 1: curl SSE ------------------------------------------------------
run_curl_sse() {
  unload_model
  local out="$WORK/curl-sse.out" meta rc code ttfb total comments outcome notes
  meta=$(timeout -k 10 "$TIMEBOX" curl -sS -N -X POST "$BASE_URL/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d "$(openai_body true)" \
    -o "$out" -w '%{http_code} %{time_starttransfer} %{time_total}' 2>"$WORK/curl-sse.err")
  rc=$?
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    record curl-sse FAIL "$TIMEBOX" - "hang: no completion within ${TIMEBOX}s"
    return
  fi
  read -r code ttfb total <<<"$meta"
  if [ "$rc" -ne 0 ]; then
    record curl-sse FAIL "${total:-?}" "${ttfb:-?}" "curl exit $rc: $(head -c 120 "$WORK/curl-sse.err")"
    return
  fi
  comments=$(grep -c '^:' "$out" || true)
  if grep -q '^data: \[DONE\]' "$out"; then
    outcome=STREAMED
  elif grep -q '"error"' "$out"; then
    outcome=CLEAN-ERROR
  else
    record curl-sse FAIL "$total" "$ttfb" "http $code, no [DONE] and no error payload"
    return
  fi
  notes="http=$code sse-comment-lines=$comments"
  if ! awk -v a="$ttfb" -v b="$FIRST_BYTE_BUDGET_S" 'BEGIN{exit !(a < b)}'; then
    record curl-sse FAIL "$total" "$ttfb" "$notes first byte >= ${FIRST_BYTE_BUDGET_S}s: keepalive missing"
    return
  fi
  record curl-sse "$outcome" "$total" "$ttfb" "$notes"
}

# --- clients 2+3: python SDKs ------------------------------------------------
# Prefer uv (isolated, pinned dep); fall back to a system python that already
# has the package; otherwise SKIP rather than hang.
py_runner() { # $1 = package -> echoes runner words
  if command -v uv >/dev/null 2>&1; then
    echo "uv run --with $1 python"
    return 0
  fi
  if python3 -c "import $1" >/dev/null 2>&1; then
    echo "python3"
    return 0
  fi
  return 1
}

write_openai_py() {
  cat >"$WORK/openai_smoke.py" <<'PY'
import sys, time
from openai import OpenAI

base, model, prompt = sys.argv[1], sys.argv[2], sys.argv[3]
# Deliberately default timeouts: the design claims SSE keepalives reset the
# SDK's per-read gap, so overriding them would mask the thing under test.
client = OpenAI(base_url=base + "/v1", api_key="smoke")
t0 = time.monotonic()
first = None
try:
    stream = client.chat.completions.create(
        model=model, stream=True, max_tokens=48,
        messages=[{"role": "user", "content": prompt}])
    text = []
    for chunk in stream:
        if first is None:
            first = time.monotonic() - t0
            print(f"RIG_FIRST_CHUNK_S={first:.2f}", flush=True)
        for c in chunk.choices:
            if c.delta and c.delta.content:
                text.append(c.delta.content)
    print("RIG_ANSWER=" + "".join(text))
    print("RIG_OUTCOME=streamed")
except Exception as e:
    print(f"RIG_OUTCOME=clean-error {type(e).__name__}: {e}")
    sys.exit(3)
PY
}

write_anthropic_py() {
  cat >"$WORK/anthropic_smoke.py" <<'PY'
import sys, time
import anthropic

base, model, prompt = sys.argv[1], sys.argv[2], sys.argv[3]
# base_url is the router root: the SDK itself appends /v1/messages.
client = anthropic.Anthropic(base_url=base, api_key="smoke")
t0 = time.monotonic()
first = None
try:
    text = []
    with client.messages.stream(
            model=model, max_tokens=48,
            messages=[{"role": "user", "content": prompt}]) as stream:
        for piece in stream.text_stream:
            if first is None:
                first = time.monotonic() - t0
                print(f"RIG_FIRST_CHUNK_S={first:.2f}", flush=True)
            text.append(piece)
    print("RIG_ANSWER=" + "".join(text))
    print("RIG_OUTCOME=streamed")
except Exception as e:
    # A surfaced error is a recordable outcome, not a hang (see README).
    print(f"RIG_OUTCOME=clean-error {type(e).__name__}: {e}")
    sys.exit(3)
PY
}

run_python_sdk() { # $1 = client-name $2 = package $3 = script
  local runner out="$WORK/$1.out" rc start end dur first outcome
  if ! runner=$(py_runner "$2"); then
    record "$1" SKIP - - "neither uv nor python3-with-$2 available"
    return
  fi
  unload_model
  start=$(now)
  # shellcheck disable=SC2086 # runner is deliberately word-split
  timeout -k 10 "$TIMEBOX" $runner "$3" "$BASE_URL" "$MODEL" "$PROMPT" >"$out" 2>&1
  rc=$?
  end=$(now)
  dur=$(elapsed "$start" "$end")
  first=$(sed -n 's/^RIG_FIRST_CHUNK_S=//p' "$out" | head -1)
  outcome=$(sed -n 's/^RIG_OUTCOME=//p' "$out" | head -1)
  case $rc in
  0) record "$1" STREAMED "$dur" "${first:--}" "answer: $(sed -n 's/^RIG_ANSWER=//p' "$out" | head -c 60)" ;;
  3) record "$1" CLEAN-ERROR "$dur" "${first:--}" "$(printf '%s' "$outcome" | head -c 140)" ;;
  124 | 137) record "$1" FAIL "$dur" "${first:--}" "hang: killed by timeout after ${TIMEBOX}s" ;;
  *) record "$1" FAIL "$dur" "${first:--}" "exit $rc: $(tail -c 140 "$out" | tr '\n' ' ')" ;;
  esac
}

# --- client 4: claude code ---------------------------------------------------
run_claude_code() {
  if ! command -v "$CLAUDE_BIN" >/dev/null 2>&1; then
    record claude-code SKIP - - "claude binary not found"
    return
  fi
  unload_model
  local out="$WORK/claude.out" rc start end dur
  start=$(now)
  # API_TIMEOUT_MS raises the wall-clock budget; the byte-silence stall timer
  # is the thing sendLoadingState must defeat (router-lifecycle.md 6.3).
  ANTHROPIC_BASE_URL="$BASE_URL" \
    ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-smoke}" \
    ANTHROPIC_MODEL="$MODEL" \
    ANTHROPIC_SMALL_FAST_MODEL="$MODEL" \
    API_TIMEOUT_MS=$((TIMEBOX * 1000)) \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1 \
    timeout -k 10 "$TIMEBOX" "$CLAUDE_BIN" -p "$PROMPT" --model "$MODEL" >"$out" 2>&1
  rc=$?
  end=$(now)
  dur=$(elapsed "$start" "$end")
  case $rc in
  0)
    if [ -s "$out" ]; then
      record claude-code STREAMED "$dur" - "answer: $(head -c 60 "$out" | tr '\n' ' ')"
    else
      record claude-code FAIL "$dur" - "exit 0 but empty output"
    fi
    ;;
  124 | 137) record claude-code FAIL "$dur" - "hang: killed by timeout after ${TIMEBOX}s" ;;
  *)
    if [ -s "$out" ]; then
      record claude-code CLEAN-ERROR "$dur" - "exit $rc: $(head -c 140 "$out" | tr '\n' ' ')"
    else
      record claude-code FAIL "$dur" - "exit $rc with no output"
    fi
    ;;
  esac
}

# --- client 5: qwen-code -----------------------------------------------------
run_qwen_code() {
  if [ ! -x "$QWEN_BIN" ]; then
    record qwen-code SKIP - - "qwen binary not found at $QWEN_BIN"
    return
  fi
  unload_model
  local out="$WORK/qwen.out" rc start end dur
  start=$(now)
  # Non-interactive one-shot: positional prompt. --bare skips extension /
  # auto-discovery startup work that would confound timing.
  OPENAI_API_KEY=smoke \
    OPENAI_BASE_URL="$BASE_URL/v1" \
    OPENAI_MODEL="$MODEL" \
    timeout -k 10 "$TIMEBOX" "$QWEN_BIN" --bare -m "$MODEL" \
    --openai-api-key smoke --openai-base-url "$BASE_URL/v1" \
    "$PROMPT" >"$out" 2>&1
  rc=$?
  end=$(now)
  dur=$(elapsed "$start" "$end")
  case $rc in
  0)
    if [ -s "$out" ]; then
      record qwen-code STREAMED "$dur" - "answer: $(head -c 60 "$out" | tr '\n' ' ')"
    else
      record qwen-code FAIL "$dur" - "exit 0 but empty output"
    fi
    ;;
  124 | 137) record qwen-code FAIL "$dur" - "hang: killed by timeout after ${TIMEBOX}s" ;;
  *) record qwen-code CLEAN-ERROR "$dur" - "exit $rc: $(head -c 140 "$out" | tr '\n' ' ')" ;;
  esac
}

# --- main --------------------------------------------------------------------
say "six-client cold-start smoke: base_url=$BASE_URL model=$MODEL delay_s=$DELAY_S timebox_s=$TIMEBOX cold_each=$COLD_EACH"
if ! curl -sf --max-time 5 "$BASE_URL/running" >/dev/null 2>&1 &&
  ! curl -sf --max-time 5 "$BASE_URL/v1/models" >/dev/null 2>&1; then
  say "error: nothing answering at $BASE_URL — is llama-swap up?"
  exit 2
fi

write_openai_py
write_anthropic_py

run_curl_sse
run_python_sdk openai-python openai "$WORK/openai_smoke.py"
run_python_sdk anthropic-python anthropic "$WORK/anthropic_smoke.py"
run_claude_code
run_qwen_code
record owui MANUAL - - "browser client: follow README.md manual procedure"
record pi MANUAL - - "TUI client: follow README.md manual procedure"

{
  say ""
  say "six-client cold-start smoke — $(date -Is)"
  say "base_url=$BASE_URL model=$MODEL delay_s=$DELAY_S timebox_s=$TIMEBOX cold_each=$COLD_EACH"
  say ""
  printf '%-18s %-12s %11s %13s  %s\n' CLIENT STATUS DURATION_S FIRST_BYTE_S NOTES
  for row in "${ROWS[@]}"; do
    IFS='|' read -r c s d f n <<<"$row"
    printf '%-18s %-12s %11s %13s  %s\n' "$c" "$s" "$d" "$f" "$n"
  done
  say ""
  say "gate: PASS per client = STREAMED or CLEAN-ERROR (no hangs). FAIL anywhere fails the A1/A5 gate."
} | tee "$RESULTS_FILE"

say ""
say "results written to $RESULTS_FILE"
exit "$FAILED"
