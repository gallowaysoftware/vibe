# vibe router + model lifecycle: adopt llama-swap, federate by cell

Status: DESIGN (2026-07-12). Synthesized from a 4-angle web-research pass
(router landscape, lifecycle evidence, placement patterns, config/UX
ergonomics), an adversarial claim-verification pass, and a three-designer /
two-judge panel. Both judges independently picked the adopt-first design as
base; the grafts from the two native designs are folded in and marked.

**This document supersedes the router/lifecycle half of
[fleet.md](fleet.md):** fleet.md §6 (router v2: routes.yaml chains,
format-affinity walker, pre-first-byte fallback, `max_concurrency`), the
health engine, the per-host slot map, `EnsureModelAvailable`, and the JSONL
metrics TeeReader are **not built**. fleet.md's provisioning half (hosts.yaml,
SSH+systemd converge, model distribution, doctor, Spark commissioning) stands
unchanged and this design depends on it.

## 1. The requirement

Three verbatim asks:

1. **Routing like LiteLLM** — many model names, multiple hosts, cloud API
   keys, one endpoint.
2. **On-demand model loading with an idle timeout** — plus periodic refresh,
   on the belief that long-loaded servers degrade.
3. **The combination**: "I used dsv4flash but stopped using it for an hour
   and it timed out; I try to use qwen through vibe and since the 2x spark
   cluster is empty and that is a valid profile for the cluster, vibe starts
   it up."

Note that (3) overturns fleet.md's explicit "no request-triggered autostart"
rule. Autostart is now the product.

## 2. Build-vs-buy verdict

**ADOPT [llama-swap](https://github.com/mostlygeek/llama-swap) (MIT, Go,
single binary, v239 July 2026, 5k+ stars) as the routing + lifecycle
substrate — one instance per "cell" (localmodel, spark-pair, llamaloft), federated via
its `peers` mechanism, with the localmodel instance taking over :9000. vibe's
`proxy.go` is retired.** vibe shrinks to what cannot be bought: config
rendering + SSH/systemd converge, model distribution, the dual-Spark pair
scripts, ComfyUI, vamp, frontends, and a thin fleet-state aggregator for the
future web UI.

Why not the alternatives (all re-verified July 2026):

- **LiteLLM — rejected.** Zero lifecycle (cannot start/stop/unload anything —
  the entire hard half). Plus: 2026-03 PyPI supply-chain compromise
  (credential-stealing 1.82.7/1.82.8), CVE-2026-42208 pre-auth SQLi actively
  exploited within 36h of disclosure (harvesting exactly the provider keys
  we'd store in it), mid-flight Rust rewrite, Python+Postgres weight for one
  user. Its `model_list` naming idea is imitated in config, not adopted.
- **GPUStack — rejected.** Only tool that natively drives multi-node vLLM,
  but has NO idle timeout / scale-to-zero (the #1 ask), no Anthropic format,
  no cloud keys, and it would fight the pinned jasl/sparkrun Spark recipes.
- **llamactl — evaluated (the verify pass's "big miss"), rejected for now.**
  Closest single tool (on-demand start + idle timeout + LRU eviction + remote
  nodes + web UI), but: no evidence of `/v1/messages`, no cloud-key peers,
  cannot express a 2-host tensor-parallel unit, v0.x single-maintainer.
  Revisit if it matures.
- **llama.cpp router mode** (`--sleep-idle-seconds` shipped in PR #18228) —
  real but single-host, llama.cpp-only, ~600MiB residual VRAM per sleeping
  child. llama-swap manages llama-server children anyway; we get these
  improvements for free underneath it.
- **Bifrost / Olla / TensorZero (archived 2026-06) / Kong / Envoy / Portkey /
  KubeAI / Ray Serve / SkyPilot** — routing-only, dead, k8s-shaped, or wrong
  weight class. KubeAI's queue-during-scale-up semantics are what llama-swap
  already implements.
- **Build native** (fleet.md §6 + a lifecycle engine) — rejected. It means
  reimplementing, solo, the precise feature set llama-swap has hardened over
  181 releases, with scenario 3 landing months out instead of days.

Why llama-swap specifically — it is the only surveyed tool with ALL of:

- per-model `ttl` idle-unload, countdown anchored at request **completion**
  (a 20-minute generation can't be killed under itself);
- health-gated request holding: requests for a cold model queue and forward
  when the backend passes health;
- **`sendLoadingState`** — SSE keepalive comments streamed every second while
  a model loads. The verify pass showed this is *mandatory*: Claude Code has
  a hardcoded ~5-minute **byte-silence** stall timeout (issue #39906) that no
  env var fixes, so a silent 6-10 min Spark cold start fails every time;
  continuous SSE comment bytes defeat it;
- Anthropic `/v1/messages` + `count_tokens` endpoints;
- `peers` with `apiKey` injection (both `Authorization: Bearer` and
  `x-api-key`) — **an Anthropic cloud key is just a peer**;
- `cmd`/`cmdStop` as arbitrary commands — a docker-compose'd dual-node vLLM
  pair is startable/stoppable as one model;
- the `matrix`/`evict_costs` solver: declared-valid co-residency sets,
  cheapest-eviction selection;
- in-flight drain (WaitGroup → cmdStop → 5s grace → kill);
- REST + multiplexed SSE events API with a per-cell web UI already on it —
  the exact contract vibe's future web UI wants.

**Honesty clause — what stays hand-rolled:** (1) the dual-Spark pair
start/stop scripts (every tool surveyed punts on 2-host TP units); (2) config
rendering + converge (fleet.md P2, unchanged); (3) the fleet-state
aggregator. Where this glue could rot into a worse orchestrator is catalogued
in §13.

## 3. Architecture

```
                   clients: claude code, qwen-code, pi, OWUI, vamp
                                    │  (unchanged: localmodel:9000)
                      ┌─────────────▼──────────────┐
                      │ llama-swap "front" @ localmodel │  :9000
                      │  local models: localmodel 5090  │  (llama.cpp, tabby, comfyui*)
                      │  peers:                    │
                      │   spark-cell → spark-1:9100│
                      │   llamaloft-cell  → llamaloft:9100   │
                      │   anthropic  → api.anthropic.com (apiKey peer)
                      └───────┬──────────┬─────────┘
                              │          │
             ┌────────────────▼───┐   ┌──▼──────────────────┐
             │ llama-swap @spark-1│   │ llama-swap @ llamaloft   │ :9100
             │ :9100 — the "cell" │   │ ttl:0 everything,   │
             │ owns BOTH sparks:  │   │ preloaded at boot   │
             │  dsv4flash (dual,  │   │ (embed/rerank/      │
             │   cmd=pair-up.sh)  │   │  classify/STT/TTS)  │
             │  qwen3-coder-next  │   └─────────────────────┘
             │   (single, s1)     │
             │  qwen35-122b       │
             │   (single, s2 via  │
             │    docker -H ssh://)│
             └────────────────────┘

  vibe daemon @ localmodel: renders the 3 llama-swap configs from backends/ +
  hosts.yaml, converges them over SSH+systemd, distributes models, runs
  doctor, owns ComfyUI + frontends + the vamp control plane, aggregates
  fleet state for the web UI.
```

Structural decisions:

1. **Cell = one llama-swap instance = one eviction domain.** The Spark *pair*
   is one cell, hosted on spark-1. This dissolves the dual-node problem every
   tool "can't" solve: per-model `cmd` is arbitrary, so the spark-1 instance
   starts spark-2 containers via `docker -H ssh://spark-2` inside the wrapper
   script and health-checks the vLLM head locally. One instance holds one
   coherent ledger over both Sparks; the `matrix` declares valid combinations
   (dsv4flash excludes everything; one single-node model per Spark may
   coexist). Separate llama-swaps per Spark would split the ledger and
   reintroduce cross-host placement.
   - *Settled (judge Q): cell lives on spark-1, not localmodel.* spark-1 down
     means the pair is down regardless (it's the TP head), and co-locating
     the ledger with the hardware keeps the start path local. The residual
     orphan risk (spark-2 containers after a spark-1 llama-swap restart) is
     handled by the idempotent-preflight rule in §7.
2. **The front owns :9000** — every existing client config keeps its URL.
3. **Anthropic is a peer, not a special case.** `model: claude-sonnet-5`
   reaches the real API with the key injected; `model: coder` reaches metal.
4. **localmodel's own models move under the front llama-swap.** `cmd` = the same
   llama-server/tabby invocations vibe renders today. vibe's supervisor +
   respawn watcher stop managing LLM processes (systemd `Restart=always` on
   the llama-swap unit + its child management replace them). The tabby EXL3
   sampler-default gotcha is fixed at the proxy with per-model
   `filters.setParams: {min_p: 0.05, repetition_penalty: 1.05}` —
   client-invisible.
5. **ComfyUI (time-boxed experiment, A7)**: register as a model with `cmd` =
   vibe's comfyui launch, `checkEndpoint: /system_stats`, reached via
   `/upstream/comfyui/...` raw passthrough (JIT-starts it). That folds media
   into the 5090 eviction matrix. **Pre-agreed abort criteria**: if
   `/upstream` doesn't proxy ComfyUI's WebSocket traffic, or vamp's ComfyUI
   client needs changes beyond a base URL, abort — ComfyUI stays
   vibe-managed (status quo).

## 4. Config schema

Two layers: **vibe source of truth** (user edits `backends/*.yaml` +
`hosts.yaml`) and **rendered llama-swap configs** (vibe generates + converges;
hand-editable in A1 before the renderer exists).

### 4.1 vibe source: `backends/<name>.yaml` gains `cell:` + `lifecycle:`

```yaml
# backends/dsv4flash.yaml
cell: spark-cell                  # replaces fleet.md host:; placement = cell membership
formats: [openai]
context: 131072
estimated_vram_gb: 100            # per node; render-time capacity lint only
lifecycle:
  ttl: 45m                        # idle-unload; ALSO the NCCL-idle-death guard (vllm#42742)
  refresh: nightly_if_idle        # rendered as a systemd timer hitting the unload API
  evict_cost: 3                   # matrix solver input; higher = evicted last
  start_timeout: 15m              # feeds the cell's healthCheckTimeout budget
backend:
  vllm_pair:                      # NEW union kind; renders pair-up/down scripts
    image: ghcr.io/spark-arena/dgx-vllm-eugr-nightly@sha256:...   # digest-pinned
    model: deepseek-ai/DeepSeek-V4-Flash
    alias: dsv4flash
    aliases: [coder-max]
    port: 8000
    head: spark-1
    workers: [spark-2]
    args: [--tensor-parallel-size=2, --kv-cache-dtype=fp8, ...]
    env_extra: {TORCH_NCCL_HEARTBEAT_TIMEOUT_SEC: "14400"}   # idle-death belt+braces

# backends/anthropic.yaml
cell: front
formats: [anthropic, openai]
backend:
  cloud_peer:                     # NEW union kind; renders a llama-swap peer stanza
    base_url: https://api.anthropic.com
    api_key_cmd: op read op://Private/anthropic-api/credential
    models: [claude-opus-4-8, claude-sonnet-5]

# backends/qwen-embed.yaml (llamaloft utility plane)
cell: llamaloft-cell
lifecycle: {ttl: 0, preload: true}   # pinned: never unloads, loaded at cell boot
backend:
  llama_server: {path: ~/models/qwen3-embedding-0.6b.gguf, alias: qwen-embed}
```

Renderer contract:

| vibe field | llama-swap output |
|---|---|
| `lifecycle.ttl` | model `ttl` (seconds; 0 = never) |
| `lifecycle.preload: true` | `hooks.on_startup.preload` |
| `lifecycle.evict_cost` | `matrix.evict_costs[alias]` |
| `lifecycle.start_timeout` | cell `healthCheckTimeout` = max over cell's models |
| `lifecycle.refresh: nightly_if_idle` | systemd timer: `curl -X POST :9100/api/models/unload/<alias>` at 04:00 (drain first; unloaded = no-op, so "if idle" is free) |
| `aliases` | model `aliases` |
| tabby sampler defaults | model `filters.setParams` |
| `cloud_peer` | front `peers.<name>` with `apiKey` + raised per-peer timeouts |
| co-residency (§6) | `matrix.sets` per cell |

Render-time lint (hard failures): alias collisions across the whole
namespace; any matrix-valid co-resident set exceeding the cell's declared
capacity; the llamaloft pinned set exceeding 12GB.

Cloud keys: no literals in YAML. Converge resolves `api_key_cmd` into a 0600
systemd `EnvironmentFile`; the config references `${env.VAR}`. Re-resolve on
401 = re-run `vibe converge`.

### 4.2 Rendered spark-cell config (the interesting one)

```yaml
# rendered by vibe on spark-1 — do not edit
healthCheckTimeout: 900            # container + 149GB weights + CUDA graphs
sendLoadingState: true             # SSE keepalive during load — load-bearing
macros:
  pair_up: "/home/kyle/.local/lib/vibe/spark-pair-up.sh"
  pair_down: "/home/kyle/.local/lib/vibe/spark-pair-down.sh"
models:
  dsv4flash:
    cmd: ${pair_up} dsv4flash          # cleanup preflight → CX-7 preflight → worker → head → barrier
    cmdStop: ${pair_down} dsv4flash
    proxy: http://127.0.0.1:8000
    checkEndpoint: /health
    ttl: 2700
    aliases: [coder-max]
  qwen3-coder-next:
    cmd: docker run --rm --name vibe-qcn --net=host --gpus all ... --port 8001
    proxy: http://127.0.0.1:8001
    ttl: 2700
  qwen35-122b:
    cmd: docker -H ssh://kyle@spark-2 run --rm --name vibe-q122 ... --port 8002
    cmdStop: docker -H ssh://kyle@spark-2 stop vibe-q122
    proxy: http://192.168.200.12:8002   # CX-7 iface; runs ON spark-2, managed FROM spark-1
    ttl: 2700
matrix:
  vars: {DSV: "dsv4flash", QCN: "qwen3-coder-next", Q122: "qwen35-122b"}
  sets:
    dual_only: "DSV"
    singles: "QCN | Q122 | (QCN & Q122)"
  evict_costs: {dsv4flash: 3, qwen3-coder-next: 2, qwen35-122b: 1}
```

Front config (localmodel, :9000): local models (qwen-code, gemma4-chat, pi-tabby
with `filters.setParams`, comfyui experiment) in a one-occupant `matrix`, plus
peers: `spark-cell` (`responseHeader: 1800` — see §7 budget chain),
`llamaloft-cell`, `anthropic` (`apiKey: ${env.ANTHROPIC_API_KEY}`).

## 5. Lifecycle state machine + the degradation verdict

**Owner: llama-swap, per cell.** vibe owns no model state at runtime — it
observes via `/api/events`. This is the biggest simplification vs fleet.md
(health engine, slot map, and EnsureModelAvailable state all disappear).

```
  STOPPED ──request/preload──▶ STARTING ──health 200──▶ READY
     ▲        (queue + SSE loading comments)             │
     │  start fail / healthCheckTimeout → error          │ ttl idle expiry (from
     │                                                   │ COMPLETION) / unload API
     └──── cmdStop, 5s grace, kill ◀── DRAINING ◀────────┘ / matrix eviction
```

Spark-cell extra phases live inside the wrapper scripts, not the state
machine: `pair-up.sh` = **unconditional cleanup preflight (`pair-down` both
nodes first)** → CX-7 preflight (ethtool 200000, MTU 9000, peer ping) →
worker container → head container → barrier on cluster-size==2 → exec vLLM.
Any failure exits nonzero, fail fast, no retries, no state files.
**Diagnosability contract (hard A6 requirement)**: pair-up streams BOTH
nodes' container journals (prefixed) to its own stdout so llama-swap's log
stream shows a minute-7 failure from one place.

### Is "long-loaded models degrade" true? (research verdict)

**Not a myth, but conditional — a policy knob, not a law.**

- **Dual-node vLLM: worse than true.** vllm#42742 documents idle death on
  this exact hardware (2x GB10 + CX-7): after >2.5h idle, the NCCL heartbeat
  monitor SIGABRTs a worker and the API server becomes a zombie that answers
  `/health` but fails every request. **The TTL is the fix** — a 45-min idle
  unload means the cluster never idles long enough to die. Belt-and-braces:
  `TORCH_NCCL_HEARTBEAT_TIMEOUT_SEC=14400` in the recipe env, plus the A6
  deep-health watchdog (§13.5).
- **llama.cpp: real but episodic** leak classes (CUDA-graph leak #20315,
  slowdown-until-restart #10227, LoRA leak #19217). TTL-unload already yields
  a fresh process for intermittently-used models; `refresh: nightly_if_idle`
  (a rendered systemd timer) covers steadily-used ones. Cheap insurance
  against a recurring bug class, not physics.
- **As a universal law: refuted** (fragmentation tracks pressure, not
  uptime). So: no in-place restart machinery, no max-request recycle
  counters.
- **vLLM sleep mode: explicitly not designed around.** Multi-node sleep is
  unproven/contradicted (endpoints need `VLLM_SERVER_DEV_MODE=1`; flags
  reportedly no-op on distributed workers; idle NCCL is itself the killer).
  Bench on the Sparks someday; if it works it's a `cmd`-level optimization,
  not an architecture change.

## 6. Scenario walkthroughs

### 6.1 "Routing like LiteLLM"

`GET :9000/v1/models` returns the merged namespace: front locals + every
peer's advertised models (spark, llamaloft, claude-*). Aliases give friendly names
(`coder-max` → dsv4flash, `coder-local` → localmodel qwen); vibe lints collisions
at render time. Cloud: `model: claude-sonnet-5` on either
`/v1/chat/completions` or `/v1/messages` forwards with the key injected.
Given up vs LiteLLM: same-name deployment groups with weighted failover —
accepted; with 3 hosts, distinct explicit names are more predictable (§9
covers fallback).

### 6.2 "On-demand loading with idle timeout"

Any request naming an unloaded model JIT-starts it; every model carries
`ttl`; countdown from completion; unload drains first. llamaloft = `ttl: 0` +
preload, exempt. Per-request TTL override (Ollama `keep_alive`) doesn't exist
in llama-swap — honest gap; vamp compensates with the lease (§10), a chat
user pins via the cell UI.

### 6.3 THE scenario: dsv4flash timed out; qwen requested; cluster empty

1. **T-60m**: last dsv4flash response completes; TTL countdown (2700s) starts.
2. **T-15m**: TTL fires → drain (no-op) → `pair-down.sh` → both Sparks empty.
3. **T0**: harness sends `POST /v1/chat/completions {"model": "qwen35-122b",
   "stream": true}` to :9000.
4. Front: not local → peer spark-cell → forwards to spark-1:9100
   (responseHeader budget 1800s — moot for streaming, see step 6).
5. Spark cell: model STOPPED; matrix `singles` set — nothing resident, no
   eviction needed. Launches `docker -H ssh://spark-2 run ...`; request
   queues; STARTING. Start is singleflight by construction — concurrent
   requests attach to the same load.
6. **Client experience (the crux)**: `sendLoadingState: true` opens the SSE
   response immediately — HTTP 200, `text/event-stream`, a loading comment
   every second until healthy. First byte in ~1s, steady bytes thereafter.
   - **Claude Code**: both its 600s `API_TIMEOUT_MS` default and its
     hardcoded ~5-min byte-silence stall timer are defeated because bytes
     flow. For dsv4flash's 6-10 min start, additionally set
     `API_TIMEOUT_MS=1800000` in the documented client settings (wall-clock
     budget is separate from stall detection).
   - **OpenAI/Anthropic SDK clients (qwen-code, pi, OWUI)**: httpx-style read
     timeouts are per-read-gap on streams; 1s keepalives reset indefinitely.
   - **Why not the alternatives**: 503+Retry-After — SDKs retry 5xx over
     seconds, not 8 minutes; fails every client. Silent hold — killed by the
     stall timer. Synthetic *content* chunks — pollutes what agentic tools
     parse. SSE comments are spec-legal and parser-ignored.
   - **Failure after commit-to-200**: start failure / healthCheckTimeout ends
     the stream with an error payload; clients surface it; a retry re-queues.
     Accepted cost of streaming-first UX.
7. **T0+~3min** (single-node) / **+6-10min** (dsv4flash): health 200 → READY
   → queued requests forward → tokens. Subsequent requests are warm.

### 6.4 Bonus: dsv4flash requested while qwen35-122b is mid-generation

Matrix says they can't coexist. The dsv4flash request queues; qwen's
in-flight drains to completion (never killed); `cmdStop`; `pair-up.sh`; the
requester holds through drain + start behind SSE keepalives. No min-residency
anti-thrash guard exists — acceptable for one human; §13.3.

## 7. Placement, eviction, budgets

- **Placement is declared, not computed.** A backend's `cell:` IS its
  placement; the model name uniquely identifies its cell via the peer table.
  No cross-cell scheduler. gguf-parser-go style VRAM estimation is relegated
  to `vibe doctor` advisories + render-time lint.
- **Eviction**: matrix solver per cell; human-declared valid sets; cheapest
  eviction by `evict_cost`. Pinned = `ttl: 0` + preload (llamaloft). localmodel = one
  big occupant (`big:` set), restoring the "one 5090 tenant" rule with
  automatic instead of manual swaps.
- **Dual-node is one unit**: one matrix var; the solver can't half-evict it;
  health is head-only; teardown both-or-error.
- **Cell restart safety (graft)**: llama-swap cannot adopt already-running
  processes, so a spark-cell restart while dsv4flash is loaded either kills
  it or orphans a spark-2 container. Rules: (1) `pair-up.sh` starts with an
  unconditional both-node cleanup; (2) **A6 acceptance test**: restart the
  spark-cell llama-swap while dsv4flash is loaded AND while it is starting —
  verify no orphan survives on spark-2 and document observed reload
  semantics; (3) `vibe doctor` gains an orphan-container check
  (`docker ps` on both Sparks vs cell `/running`).
- **Invariant + test (graft)**: a vibe daemon/converge restart never
  restarts a loaded model. (Structurally true — vibe doesn't supervise LLMs
  anymore — pinned with a test so a refactor can't regress it.)
- **Budget chain (settled judge Q)**: worst case 6.4 = drain (one full
  generation, minutes) + 6-10 min start. For **streaming** requests the
  front's responseHeader timeout is moot (200 arrives in ~1s); the governing
  budgets are the cell's healthCheckTimeout (900s, start only — queue wait is
  separate) and the client's wall-clock budget (document
  `API_TIMEOUT_MS=1800000` for coder-max sessions). For **non-streaming**
  requests the hold is bounded by min(front responseHeader 1800s, cell
  budgets) — acceptable because every interactive client streams and vamp is
  patched to stream (§10); stray non-streamers get a bounded silent hold,
  documented.
- **Concurrency**: engine-side `--max-num-seqs` plus single-user reality
  replaces fleet.md's queue-not-reject semaphore; if concurrency collapse
  shows up, set `concurrencyLimit` and let vamp retry rare 429s. Client
  disconnect must cancel upstream generation (Go proxy propagates context
  cancellation) — verified in the A1 smoke with a mid-stream kill.

## 8. Health probing

vLLM `/health` lies after NCCL death, so the A6 watchdog probes with a
1-token completion on a timer and unloads on failure. **Invariant (graft):
probe traffic must not reset the TTL clock** — otherwise the watchdog keeps
the pair alive forever and defeats idle-unload. Probe the vLLM head directly
on its engine port, bypassing llama-swap; verify during A6 that this doesn't
touch the idle timer. This stays a monitoring cron, not an orchestrator.

## 9. Cloud fallback policy

**No silent automatic local→cloud fallback. Cloud is an explicit name.**
Auto-diverting a cold start to Anthropic would spend metered money without
consent, mask autostart regressions, and swap model behavior mid-session —
poison for agentic tools.

- Per-route control is the model-name grammar: `coder-max` = dsv4flash, wait
  for it; `claude-sonnet-5` = cloud, now. omp already does the
  local-vs-Claude split client-side; OWUI exposes both entries in its picker.
- **Reserved spec (graft, build only if lived experience demands it)**: an
  `on_cold: start | fallback_then_start | fallback` per-route policy and
  suffix grammar (`:cloud`, `:local`, `:now`) for the human-chat case where
  someone won't stare at an 8-minute hold. Reserve the names in the namespace
  now; implement in the thin front shim only if the A5/A6 experience proves
  intolerable. Every fallback, if ever built, emits an event — never silent.
- Hard-down (host unreachable, not cold): the request errors with the
  upstream error. The escape hatch remains a ~200-line pre-first-byte retry
  shim (fleet.md §6.1 semantics) — deferred until proven necessary.

## 10. vamp contract

- `EnsureCapability` becomes: resolve capability → model name → fire a
  1-token **streaming** warm request at :9000 and wait (keepalives make it
  patient) → return `{base_url, model}`. `vamp run --warm` fires all
  capabilities in parallel so cold starts overlap.
- **Typed errors survive (graft)**: a ~100-line shim in vamp maps warm/request
  failure bodies to `START_FAILED | NOT_FOUND | CAPACITY | UPSTREAM_DOWN`
  (replaces both `IsVRAMRejection` and fleet.md's Connect error scheme).
- **Lease (settled judge Q)**: TTL-vs-long-pipelines is real — a 50-min
  ComfyUI stage between LLM calls would let the Spark model get reaped
  mid-run. Decision: vamp-side heartbeat, implemented once in vamp's
  inference client — during a pipeline run, any capability already warmed is
  re-warmed with a 1-token request whenever `elapsed_since_last_use >
  ttl/2`. No llama-swap changes required. Opportunistic upstream PR
  (per-request `keep_alive` honored at the proxy) can replace the heartbeat
  later; do not block on it.
- vamp stages are forced to stream-and-collect (removes the non-streaming
  hold class entirely for pipelines).

## 11. Observability + web-UI surface

Per cell, llama-swap already serves the proven contract: `GET /running`,
`GET /api/events` (SSE: modelStatus + logData), `/logs/stream[/{model}]`,
`/api/metrics` + Prometheus `/metrics`, request/response captures
(`/api/captures` — answers the qwen tool-call-rate question offline via
`vibe metrics tool-calls`), `POST /api/models/unload/{model}`,
`/upstream/{model}`, plus its own `/ui` per cell (the interim dashboard).

vibe glue — `vibe fleet api` on the :9001 control plane:

- `GET /api/fleet/state` — hosts (hosts.yaml + SSH liveness), cells, models
  with state/ttl/aliases, converge drift, model-library presence, **and
  persisted per-model start-duration history (graft)** — the honest-ETA
  source for cold-start progress bars.
- `GET /api/fleet/events` — per-cell `/api/events` multiplexed with
  host/cell tags + vibe events (converge, doctor, ensure progress).
- Existing Connect RPCs stay the mutation surface. The future web UI = a
  static frontend over exactly these three surfaces + deep links to cell UIs.

## 12. Migration: survives / mutates / dies

**Survives**: `backends/*.yaml` as source of truth (gains `cell:` +
`lifecycle:`); `hosts.yaml` + converge + doctor + model distribution
(fleet.md P2/P4 verbatim); profiles as *frontend* definitions (write_files,
mcps, managed frontends) whose `backend_ref` becomes "which model name to
render into the config"; vamp; ComfyUI code; :9000 as the client URL.

**Mutates**: `vibe start <backend>` → sugar for a streaming warm request;
`vibe ps` → cell `/running`; `vibe stop` → unload API; tabby sampler defaults
→ rendered `filters.setParams`; capabilities.yaml values → model
names/aliases; the daemon keeps ComfyUI/frontends/converge/fleet-api and
stops supervising LLM processes.

**Dies (breaking, sanctioned)**: `internal/vibe/proxy/proxy.go`; fleet.md
router v2 + health engine + slot map + `EnsureModelAvailable` + JSONL metrics
(the majority of fleet P1/P3/P7 code is never written — that's the payoff);
the classifier-sidecar co-start routing (relocates to llamaloft); the active-slot
/ `vibe swap --evict` concepts (the matrix decides).

**Namespace rename (settled judge Q)**: canonical model ids are decided once,
in A2, as a mapping table in this doc's PR — then ONE atomic pass over
capabilities.yaml, frontend templates, OWUI model list, and omp config.
Model IDs are the fleet's client-visible contract; no incremental renames.

**Dependency policy (settled judge Q)**: llama-swap is version-pinned by
converge like any vLLM image; the renderer emits a documented config subset;
upgrades are deliberate and gated on re-running the six-client smoke rig.
**No forking.** If the keepalive relay fails (see A1/A5 gate), the
pre-agreed bail-out is a **thin vibe front shim on :9000** (peekModel +
keepalive writer + peer forwarding, ~fleet.md router scoped way down) while
llama-swap keeps owning cell lifecycle — hybrid, not fork. Reimplementation
trigger: project abandonment AND a blocking defect.

## 13. Honest gaps ledger

1. **Pair scripts** are a ~100-line bash 2-node orchestrator. Kept
   deliberately dumb: cleanup preflight, fail fast, no retries, no state
   files. If they grow retry loops, stop and revisit GPUStack/llamactl.
2. **No per-request TTL override** — vamp compensates with the lease
   heartbeat; chat pins via the cell UI. Upstream PR before fork if it
   chafes.
3. **No min-residency anti-thrash guard** — near-impossible with one human;
   revisit only if vamp + interactive use provably collide.
4. **No automatic cloud failover** (§9) — the reserved `on_cold` spec is the
   fix if the philosophy proves wrong.
5. **Deep health** — the §8 watchdog closes the zombie window; TTL keeps it
   small.
6. **`/v1/models` peer aggregation, `/v1/messages` peer routing, and whether
   local engines serve `/v1/messages` natively through llama-swap** (i.e.
   can Claude Code drive *local* models, or is anthropic-format effectively
   cloud-only at launch) — asserted by docs, verified only by the A1/A5
   smoke.
7. **Renderer drift** — llama-swap schema moves; renderer follows; version
   pin + gated upgrades (§12).

## 14. Roadmap (each phase independently useful)

| phase | scope | effort | proves |
|---|---|---|---|
| A0 | ~~Commit working tree~~ (done: fc8541e / 94a1fbe / cfe4fb7) | — | — |
| A1 | **llama-swap on localmodel at :9000**, hand-written config: current localmodel models + ttl + `sendLoadingState` + anthropic peer; vibe proxy retired behind a flag. **Gate: the six-client smoke rig** — claude code, qwen-code, pi, OWUI, OpenAI SDK, Anthropic SDK against a synthetic `sleep 420` slow-start backend; pass = streamed answer or cleanly surfaced error, no hangs, recorded per client; plus mid-stream client-kill cancels upstream. **Fail → the §12 front-shim bail-out, decided now, not under duress** | M | The riskiest bet, day one, one box |
| A2 | vibe renderer: backends+hosts → llama-swap configs + units; converge push (fleet P2 plumbing); **canonical model-id rename pass (mapping table)** | M | Config source of truth |
| A3 | llamaloft cell: always-on plane, preload, peer | S | Federation + pinning |
| A4 | model distribution (`vibe model ensure`, fleet P4) | M | — |
| A5 | spark-1 single-node cell (qwen3-coder-next), peer; **scenario 3 minus dual-node with a real ~3-min cold start; re-run the six-client rig through the peer hop** | M | Peer-hop hold + keepalive end-to-end |
| A6 | dual-node: pair scripts (cleanup preflight + both-journal streaming), matrix, `docker -H ssh://` spark-2, NCCL env, nightly-if-idle timer, deep-health watchdog (TTL-neutral probe), **cell-restart acceptance test** | L | Scenario 3 verbatim |
| A7 | vamp: capabilities→names, streaming warm-ahead + `--warm`, typed-error shim, lease heartbeat; ComfyUI-under-llama-swap experiment (abort criteria §3.5) | M | Pipelines + media coexistence |
| A8 | `vibe fleet api` aggregator (state + events + start-duration history); dead-code deletion (proxy.go, respawn paths); AGENTS.md/fleet.md updates | M | Web-UI substrate |

**Riskiest bet** (front-loaded into A1/A5): that `sendLoadingState` keepalive
(a) survives the peer hop unbuffered, (b) is tolerated by Claude Code's
parser and stall detector for 8+ minutes, (c) fails cleanly after
commit-to-200. All asserted by verified research, never proven end-to-end
with these clients. A1 tests it with a fake slow start before any Spark
exists; the bail-out is pre-agreed (§12) and is a shim, not a fork.

## 15. A1 executed (2026-07-12) — gate results

llama-swap v239 owns localmodel:9000 (systemd unit, Restart=always); daemon runs
`disable_proxy: true`; the four LLM backends are `external: true`; profiles
activate against the router catalog (`vibe start long_form` verified live).

Six-client gate at 420s synthetic cold start, cold per client: **curl-sse,
openai-python, anthropic-python, claude-code, qwen-code all STREAMED** (OWUI
and pi manual, procedures in scripts/smoke/llama-swap/README.md). Two
findings that amend this doc's research-derived assumptions:

1. **v239's loading state covers ONLY `/v1/chat/completions`**
   (`internal/router/loading.go` whitelists that path; payload is
   `reasoning_content` deltas, not SSE comments). The Anthropic
   `/v1/messages` path is a silent hold. No upstream issue exists —
   candidate small PR (Anthropic streams have a spec-legal `ping` event).
2. **The Claude Code ~5-min byte-silence stall timer did NOT reproduce**:
   both Anthropic-path clients survived 420s of first-byte silence
   (anthropic SDK first byte at 420.7s; claude-code answered at 434.8s with
   `API_TIMEOUT_MS` raised). So the silent `/v1/messages` hold is currently
   tolerable even at Spark-scale starts — treat as version-dependent client
   behavior, re-run the rig (`DELAY_S=420 ./scripts/smoke/llama-swap/run-smoke.sh`)
   after client upgrades and before relying on it in A5/A6.

Known residuals: llama-swap has no VRAM preflight (a too-big JIT load
surfaces as a clean in-stream error — observed live when a game held 9GB);
alias collisions resolved by convention (base model keeps the shared alias,
variants addressed by backend name); `includeAliasesInList: true` is worth
setting once the A2 renderer owns the config.

## 16. Live hardware validation (2026-07-12, GPU freed)

Every lifecycle mechanic verified against real models on the 5090; numbers
here are the baseline for regressions:

| mechanic | result |
|---|---|
| JIT autostart, 26GB qwen3.6-27b | ready in 3.9s page-cached (disk-cold will be slower); evicted resident ComfyUI automatically |
| swap under request, qwen → 29GB gemma-4-31b-mm | 8.1s total incl. drain+stop+load; `/running` never showed two big tenants |
| kill-cancel | mid-LOAD kill: queued request dropped, upstream never saw it; mid-STREAM kill: upstream stopped generating |
| six-client 420s cold start | all five automated clients STREAMED (see §15) |
| TTL reaper | ttl:30 model self-unloaded on schedule; ttl:0 (pinned) member survived |
| group co-residency | `routing.router.settings.groups` (v239 exact path): persistent `{swap:false, exclusive:false}` group held embed-bge (preloaded via hooks.on_startup) + coder-7b + a fake model simultaneously; embeddings + chat served concurrently |
| ComfyUI-as-swap-tenant (§3.5 experiment) | **PASS all abort criteria**: JIT via `/upstream/comfyui/system_stats` (4.3s), WebSocket proxies through `/upstream/comfyui/ws` (status frame received), internal/comfyui client builds its WS URL path-aware (ws.go: TrimRight(path)+"/ws") + has a polling fallback → base-URL change only |

Consequences adopted into the plan: (1) ComfyUI is now a llama-swap model
(unlisted, ttl 1800) — media and coding models displace each other
automatically; (2) vamp MUST address ComfyUI via
`:9000/upstream/comfyui`, never `:8188` directly — direct requests are
invisible to llama-swap's in-flight tracking, so the TTL could reap
ComfyUI mid-workflow; (3) the llamaloft utility plane maps to a persistent
non-exclusive group exactly as simulated; (4) a model's own
`reasoning_content` chunks are indistinguishable in-band from llama-swap's
loading states — clients that budget max_tokens tightly on reasoning models
will see "empty" answers (generation semantics, not router misbehavior).

## 17. Waves 1+2 executed (2026-07-12, same day)

**Code shipped** (A2, A7, A8a): `vibe router render` (+`--check`, `--stdout`,
`--extras` for entries defs can't express); `lifecycle:`/`router:` schema
blocks; `cloud_peer` and external-`comfyui` backend kinds; canonical-id
`${MODEL_ALIAS}` expansion; vamp typed `RouterError` + streaming warm +
lease heartbeat (`keep_warm`, default 20m) + `vamp run --warm`;
`/api/fleet/state` + `/api/fleet/events` + persisted start-duration history.

**Live cutover done**: the llama-swap config is RENDERED (hand-written A1
file retired); ALL six LLM defs are external (incl. both text gemmas —
`alias_owner` on the mm variant), `anthropic` is a `cloud_peer` def,
ComfyUI is external via its def (fixed port required; `vibe ps` shows
`backend: http://127.0.0.1:9000/upstream/comfyui`, which vamp dials
directly); vamp capabilities are canonical backend ids (profile-era names
`qwen-code`/`fast`/`long_form`/`gemma_long_form` retired from
capabilities.yaml); the comfyui profile dropped `mode: service`.

**Simulated A5 (peer-hop) gate**: a scratch "sim cell" llama-swap on :9101
(groups: persistent utility plane + swap pool) is peered from the front.
Six-client rig against a 90s cold start on the PEER: all five automated
clients STREAMED, and the crux held — **first byte in 0.7ms through two
llama-swap hops** (the front relays the cell's loading-state bytes
unbuffered). Rig limitation found: `COLD_EACH` unloads via the front's
unload API, which doesn't reach peer models — only the first client of a
run gets a true peer cold start. Real-cell reruns should unload on the
cell directly.

**Still pending hardware**: A3/A4 (llamaloft provisioning + model distribution),
real A5/A6 (Sparks: single-node cell, dual-node pair scripts, NCCL
workarounds, deep-health watchdog), A8b (delete proxy.go + LLM respawn
paths once nothing daemon-supervised serves LLMs — bge-embed service is
the last holdout on localmodel, destined for llamaloft).
