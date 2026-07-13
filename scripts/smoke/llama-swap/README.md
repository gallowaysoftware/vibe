# llama-swap six-client cold-start smoke rig (A1/A5 gate)

The gate for `docs/design/router-lifecycle.md` roadmap phases A1 and A5:
prove that llama-swap's `sendLoadingState` SSE-keepalive-during-model-load
actually keeps real clients alive through a multi-minute cold start, and
that failures surface cleanly. Specifically the "riskiest bet" from the
design doc: that the keepalive (a) survives a peer hop unbuffered, (b) is
tolerated by Claude Code's SSE parser and its hardcoded ~5-minute
byte-silence stall timer, (c) fails cleanly after commit-to-200; plus that
a client kill cancels the upstream request.

Base URL and model name are parameters everywhere, so the same rig re-runs
unchanged through the peer hop for A5.

## Files

- `slowmodel/main.go` — fake inference engine with a slow cold start. Part
  of the main Go module (stdlib only, so no go.sum impact); build it
  explicitly to get a binary. For the first `--delay` it does not listen at
  all (health checks fail-connect, exactly like a loading vLLM), then it
  binds and serves `GET /health`, `GET /v1/models`,
  `POST /v1/chat/completions` (stream and non-stream), and — so the
  Anthropic-format clients can be exercised whichever way llama-swap handles
  `/v1/messages` (forward vs translate, honest-gaps item 6) —
  `POST /v1/messages` (stream and non-stream) and
  `POST /v1/messages/count_tokens`. `--die-after N` makes it exit N after
  binding for failure-path tests. Every request line is logged to stderr.
- `run-smoke.sh` — the six-client harness. Writes a results table to
  `results-<timestamp>.txt` in the current directory.
- `kill-cancel-test.sh` — mid-load and mid-stream client-kill cancellation
  test.

## 1. Build slowmodel

```
cd <repo-root>
go build -o scripts/smoke/llama-swap/slowmodel/slowmodel ./scripts/smoke/llama-swap/slowmodel
```

Standalone sanity check (no llama-swap needed):

```
./scripts/smoke/llama-swap/slowmodel/slowmodel --port 18099 --delay 10s &
curl -sf http://127.0.0.1:18099/health          # connection refused for 10s, then {"status":"ok"}
curl -sN http://127.0.0.1:18099/v1/chat/completions -X POST \
  -H 'Content-Type: application/json' \
  -d '{"model":"slowmodel","stream":true,"messages":[{"role":"user","content":"hi"}]}'
kill %1
```

## 2. llama-swap config stanza

Add to the llama-swap config (adjust paths; `sh -c` wrapper so slowmodel's
stderr lands in a file `kill-cancel-test.sh` can grep):

```yaml
sendLoadingState: true          # the thing under test
healthCheckTimeout: 600         # must exceed --delay
models:
  slowmodel:
    cmd: sh -c "exec /ABS/PATH/TO/vibe/scripts/smoke/llama-swap/slowmodel/slowmodel --port 18099 --delay 420s 2>>/tmp/slowmodel.log"
    proxy: http://127.0.0.1:18099
    checkEndpoint: /health
    ttl: 300
```

Failure-path variants (add alongside, test one at a time):

```yaml
  slowmodel-neverup:            # start failure: healthCheckTimeout must end the
    cmd: sh -c "exec .../slowmodel --port 18098 --delay 9999s 2>>/tmp/slowmodel.log"
    proxy: http://127.0.0.1:18098
    checkEndpoint: /health      # with healthCheckTimeout: 60 the held stream
                                # must terminate with an error payload, not hang
  slowmodel-dies:               # backend dies after coming up
    cmd: sh -c "exec .../slowmodel --port 18097 --delay 15s --die-after 20s 2>>/tmp/slowmodel.log"
    proxy: http://127.0.0.1:18097
    checkEndpoint: /health
```

Pass criterion for both variants: the client receives a cleanly surfaced
error (SSE stream terminated with an error payload / non-2xx), never a hang.

## 3. Run the rig

```
DELAY_S=420 BASE_URL=http://127.0.0.1:9000 MODEL=slowmodel \
  ./scripts/smoke/llama-swap/run-smoke.sh
```

Knobs (env vars): `BASE_URL` (default `http://127.0.0.1:9000`), `MODEL`
(default `slowmodel`), `DELAY_S` (default 420 — MUST match the `--delay` in
the config; the per-client timebox is `DELAY_S+120`), `COLD_EACH` (default 1:
unload the model before every client so each experiences a full cold start —
the real gate; set 0 for a quick warm regression pass), `QWEN_BIN`,
`CLAUDE_BIN`, `RESULTS_DIR`, `FIRST_BYTE_BUDGET_S` (default 5).

A full cold-each run at 420s is ~45 minutes; use `DELAY_S=90` (and a 90s
config) for iteration, 420s for the recorded gate run.

Then the cancellation test:

```
DELAY_S=420 SLOWMODEL_LOG=/tmp/slowmodel.log ./scripts/smoke/llama-swap/kill-cancel-test.sh
```

## 4. Pass criteria per client (from the design's A1 row)

Pass = streamed answer OR cleanly surfaced error; no hangs; result recorded
per client. Any hang past `DELAY_S+120` = FAIL = the pre-agreed section 12
front-shim bail-out.

| client | automated | pass looks like |
|---|---|---|
| curl-sse | yes | first byte < 5s (the keepalive; script logs SSE comment-line count), `data: [DONE]` eventually |
| openai-python | yes | streamed answer with DEFAULT SDK timeouts (keepalives must reset the per-read gap); first data chunk arrives ~cold-start time — that is expected, comments are parser-invisible |
| anthropic-python | yes | streamed answer, or a clean SDK exception (recorded — this doubles as the probe for whether llama-swap translates `/v1/messages` for local upstreams) |
| claude-code | yes | `claude -p` returns an answer; a clean error is recorded as CLEAN-ERROR; exit-by-timeout is FAIL (the stall-timer bet lost) |
| qwen-code | yes | one-shot positional-prompt mode (`qwen --bare -m MODEL 'prompt'` + `--openai-base-url`) returns an answer or clean error |
| OWUI | manual | see below |
| pi | manual | see below |
| kill-cancel | yes | upstream request dropped or cancelled (see script header for exact log signatures) |

## 5. Manual procedure: Open WebUI

1. Cold the model: `curl -X POST $BASE_URL/api/models/unload/slowmodel`.
2. Admin Settings -> Connections -> add an OpenAI API connection with URL
   `$BASE_URL/v1`, key `smoke`; refresh the model list.
3. New chat, select `slowmodel`, send "say hi in three words"; start a timer.
4. PASS: the UI holds (streaming indicator) through the full cold start and
   renders the answer — no timeout toast, no silently-dead chat, no error
   before `DELAY_S+120`. A clean error toast is a recordable CLEAN-ERROR.
5. Record duration and outcome in the results file by hand.

## 6. Manual procedure: pi

1. Cold the model (as above).
2. Point pi at the router: in pi's provider config set the OpenAI-compatible
   base URL to `$BASE_URL/v1`, API key `smoke`, model `slowmodel` (the vibe
   `pi` profile template hardcodes its base URL — edit the rendered config or
   a scratch copy, do not repoint the real profile mid-test).
3. Ask "say hi in three words"; PASS = answer or clean error within
   `DELAY_S+120`; a frozen TUI past that is FAIL.
4. Record duration and outcome by hand.

## 7. A5 re-run (peer hop)

Same rig, different target: point `BASE_URL` at the FRONT router (anvil
:9000) and `MODEL` at a model that the front resolves via `peers:` to the
cell that owns slowmodel (or the real spark model with its real ~3-min cold
start — then set `DELAY_S` accordingly and skip the slowmodel stanza). The
keepalive must arrive through the hop unbuffered: the curl-sse first-byte
check is the detector for a buffering peer proxy.

No real cell yet? Recreate the scratch sim cell that ran the recorded
2026-07-12 gate: a second llama-swap instance on :9101 with its own config —

```yaml
# sim-cell.yaml — scratch cell for the peer-hop gate
healthCheckTimeout: 600
sendLoadingState: true
models:
  peerslow:                      # the cold-start subject (90s "load")
    cmd: sh -c "exec /ABS/PATH/TO/vibe/scripts/smoke/llama-swap/slowmodel/slowmodel --port 18095 --delay 90s 2>>/tmp/peerslow.log"
    proxy: http://127.0.0.1:18095
    checkEndpoint: /health
    ttl: 300
  fastfake:                      # a fast control (2s)
    cmd: sh -c "exec /ABS/PATH/TO/vibe/scripts/smoke/llama-swap/slowmodel/slowmodel --port 18094 --delay 2s 2>>/tmp/fastfake.log"
    proxy: http://127.0.0.1:18094
    checkEndpoint: /health
    ttl: 300
```

```
llama-swap -config sim-cell.yaml -listen 127.0.0.1:9101
```

then peer it from the front via `~/.config/vibe/router-extras.yaml`:

```yaml
peers:
  sim-cell:
    proxy: http://127.0.0.1:9101
    models: [fastfake, peerslow]
```

re-render (`vibe router render`), restart the front, and run the rig with
`MODEL=peerslow DELAY_S=90`. Known rig limitation: `COLD_EACH` unloads via
the front's unload API, which does NOT reach peer models — only the first
client of a run experiences a true peer cold start; unload on the cell
(`curl -X POST http://127.0.0.1:9101/api/models/unload/peerslow`) between
clients for a full cold-each pass.
