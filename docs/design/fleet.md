# vibe fleet: multi-host serving, unified routing, cloud keys

Status: DESIGN (2026-07-12). Synthesized from a three-architect / two-judge
design panel plus a July-2026 web-research pass (models, DGX Spark platform,
router landscape, SSH orchestration). The winning base was the ops-first
design; grafts from the schema-minimalist and consumer-experience designs are
folded in and marked where the decision was contested.

> **PARTIALLY SUPERSEDED (2026-07-12, same day):** a follow-up
> research + design pass on routing and on-demand model lifecycle
> ([router-lifecycle.md](router-lifecycle.md)) adopts llama-swap as the
> routing/lifecycle substrate. That replaces this document's §6 router v2
> (routes.yaml chains, format-affinity walker, pre-first-byte fallback,
> `max_concurrency`), the health engine, the per-host slot map,
> `EnsureModelAvailable`, and the JSONL metrics plan — and overturns the
> "no request-triggered autostart" rule (autostart is now the product).
> The provisioning half (hosts.yaml, SSH+systemd converge, model
> distribution, doctor, Spark commissioning, cloud-key resolution) stands
> and is a dependency of the new design.

## 1. Why

The fleet is growing from one box to four:

| host | hardware | role |
|---|---|---|
| `localmodel` (existing) | 9950X3D, 64GB, RTX 5090 32GB, CachyOS | controller: vibe daemon, fast-decode dense models (27-31B), ComfyUI/media, OWUI, coding frontends |
| `spark-1` (incoming) | DGX Spark GB10, 128GB unified, aarch64 | big-MoE serving (head node for dual-node vLLM) |
| `spark-2` (incoming) | DGX Spark GB10, 128GB unified, aarch64 | big-MoE serving (dual-node worker) or independent single-node serving |
| `llamaloft` (probable) | RTX 3080 Ti 12GB, CachyOS | always-on utility plane: embed, rerank, classifier, STT, TTS |
| cloud | Anthropic (+ optional OpenAI-compat keys) | fallback + frontier-model routes |

vibe today is a single-host daemon: one active GPU profile + service-mode
sidecars, an OpenAI proxy on :9000 that routes by model id, `exec.Command`
everywhere. The extension: vibe provisions and operates the remote hosts over
SSH, and the :9000 proxy becomes the fleet router — many models by name,
local or remote or cloud, with fallback chains and per-route metrics.

## 2. Research verdicts (July 2026)

Full sources and numbers live in the session research digest; the decisions:

- **Router: extend vibe's own Go proxy. Not LiteLLM** (official PyPI package
  compromised 2026-03-24 with a credential-stealing payload; Python+Postgres
  operational weight; mid-rewrite to Rust through Dec 2026). Not Bifrost/
  Olla/GPUStack either — Bifrost is the escape hatch if we ever want virtual
  keys/spend dashboards. The 2026 unlock: llama.cpp, Ollama, and LM Studio
  now natively serve the Anthropic Messages API, so **format-affinity
  routing** (Anthropic-format traffic only to Anthropic-speaking upstreams,
  OpenAI-format to OpenAI-speaking ones) removes any need for
  OpenAI<->Anthropic schema translation — the hard 20% that justifies whole
  gateway products. vibe never translates schemas.
- **Remote orchestration: agentless SSH push + remote systemd user units.**
  x/crypto/ssh + pkg/sftp with a keepalive/reconnect wrapper (~300 lines;
  goph is dormant, k3s/GPUStack/exo/Ray rejected for a 4-host single-user
  fleet). systemd owns crash respawn + reboot survival; vibe owns
  convergence, health probes, journal streaming, weights distribution.
- **Dual-Spark serving stack: vLLM TP=2 in NVIDIA Docker containers over the
  ConnectX-7 200GbE RoCE link.** llama.cpp RPC across nodes is NOT a
  substitute (memory leaks, unclean tensor split). NVFP4 only via
  vendor-prequantized checkpoints.
- **Turnkey layers exist and we use them for what they're good at**
  (follow-up research 2026-07-12): NVIDIA Sync's **Cluster Assistant**
  (June 2026 OTA) automates the CX-7 fabric — topology detection, netplan,
  RoCE devices, node-to-node SSH, bandwidth verification — and nothing else
  (no docker/NCCL/models/vLLM). **sparkrun** (spark-arena, very active) is
  the community daily driver: `sparkrun run <recipe> --tp 2` handles image +
  model distribution, NCCL env, and multi-node launch from git recipe
  registries wrapping eugr's CI-tested images. No true whole-OS cluster
  image exists; NIM on Spark is single-node only; DeepSeek V4 Flash remains
  patch-carried (vLLM PR #41834 unmerged) but packaged in the
  sparkrun/eugr/tonyd2wild recipe ecosystem. Division of labor: Sync owns
  the fabric, sparkrun owns commissioning experiments, **vibe owns day-2**
  (frozen recipes as backend YAML, systemd units, health, routing).
- **Models** (see appendix A for recipes): DeepSeek V4 Flash (284B/13B-active,
  1M ctx) is validated for dual-Spark agentic coding at ~35-44 tok/s
  thinking-on with MTP — the user's leaning survives scrutiny — but it is the
  most setup-fragile option and its vLLM tool-call parser has open
  streaming-leak bugs. **Qwen3-Coder-Next 80B-A3B FP8 on a single Spark is
  the low-risk daily driver** (~43 tok/s, parser-stable with
  `--tool-call-parser qwen3_xml`) and stays primary until V4 Flash survives a
  week of real sessions. MiniMax M2.7 is the community-favorite alternative
  (requires solving thinking round-trip first). Chat: Qwen3.5-122B-A10B
  (single Spark) / Qwen3.5-397B-A17B (dual). Skip: GLM-5.2, MiniMax M3,
  Kimi K2.x (don't fit 256GB), gpt-oss-120b (superseded).
- **3080 Ti box: utility plane, llama.cpp-first** — Qwen3-Embedding-0.6B (+
  optional 4B), Qwen3-Reranker-0.6B, Qwen3-0.6B classifier (relocated from
  localmodel), faster-whisper large-v3-turbo, Kokoro (+ Qwen3-TTS for pipelines).
  Skip a 7-14B coder there (strictly dominated) and guard models. CachyOS
  with `linux-cachyos-lts` + precompiled `linux-cachyos-lts-nvidia-open`
  (no DKMS build to fail).
- **Media stays on the 5090** — every model in the current pipelines runs
  2.5-6x faster there than on a Spark; Spark ComfyUI (dockerized
  comfyui-aeon-spark) is opt-in overflow / BF16-quality experiments only.
- **Chat stack: stay on Open WebUI**, upgrade to >= 0.10.2, re-test web
  research in agentic/native mode (playwright loader no longer in the primary
  path).

## 3. Architecture in one paragraph

One vibe daemon, on localmodel, remains the only daemon (daemon-per-host
federation rejected: doubles the upgrade surface; everything downstream of
the supervisor already consumes only a URL). Remote hosts are operated
agentless over SSH; long-running remote processes are owned by remote systemd
user units (`Restart=always`, `enable --now`, lingering enabled) or
docker-under-systemd. vibe owns convergence (render → hash-compare → push →
reload), HTTP health probing, journal streaming, model distribution
(rsync-from-library, CX-7 path Spark↔Spark), and routing. systemd owns crash
respawn and reboot survival remotely — `watchBackendForRespawn` never runs
for remote backends, which sidesteps the "SSH blip looks like a crash" trap.
Cloud endpoints are backends with no process at all: activation = credential
check + route registration.

## 4. Config schema

Layout under `$XDG_CONFIG_HOME/vibe/`:

```
config.yaml            # existing daemon config + fleet: section
hosts.yaml             # NEW fleet inventory (one file — it IS the fleet)
backends/<name>.yaml   # existing BackendDef + host:/formats:/context:, two new union kinds
routes.yaml            # NEW virtual aliases -> ordered target chains (chains only — see 4.5)
profiles/<name>.yaml   # unchanged shape; profiles stay human-facing frontends on localmodel
```

State: `$XDG_STATE_HOME/vibe/fleet/<host>.json` (converge hash, unit set,
boot_id, last-seen), `metrics/requests-YYYY-MM-DD.jsonl`, `events.jsonl`
(health transitions, reboots, fallbacks).

### 4.1 hosts.yaml

```yaml
hosts:
  localmodel:
    local: true
    arch: amd64
    gpu: {kind: cuda, vram_gb: 32}
    model_dir: ~/models
    library: true                    # preferred rsync source (not sole: ensure --from <peer> works)
  spark-1:
    addr: 10.0.40.11
    ssh: {user: kyle, key: ~/.ssh/id_ed25519_fleet}
    arch: arm64
    platform: dgx-spark              # selects built-in doctor/converge check set (see 4.6)
    gpu: {kind: cuda-unified, vram_gb: 110}   # 128 phys minus OS + page-cache reserve
    model_dir: /home/kyle/models
    cluster: {iface: enp1s0f1np1, addr: 192.168.200.11, peer: spark-2}
  spark-2:
    addr: 10.0.40.12
    ssh: {user: kyle, key: ~/.ssh/id_ed25519_fleet}
    arch: arm64
    platform: dgx-spark
    gpu: {kind: cuda-unified, vram_gb: 110}
    model_dir: /home/kyle/models
    cluster: {iface: enp1s0f1np1, addr: 192.168.200.12, peer: spark-1}
  llamaloft:
    addr: 10.0.40.20
    ssh: {user: kyle, key: ~/.ssh/id_ed25519_fleet}
    arch: amd64
    platform: cachyos
    gpu: {kind: cuda, vram_gb: 12}
    model_dir: /home/kyle/models
```

Go: `internal/vibe/fleet/hosts.go` — `HostDef{Name, Addr, Local, SSH, Arch,
Platform, GPU, ModelDir, Library, Cluster}`. `LoadHosts()` validates
`cluster.peer` symmetry and duplicate addrs. `cluster.iface/addr` feed both
rsync path selection and NCCL env injection (4.3).

### 4.2 BackendDef extensions

Three new fields on **BackendDef only** (not the union — placement is a
property of a named reusable spec; profiles stay host-agnostic; inline
`backend:` blocks in profiles stay implicitly local):

```go
Host    string   // hosts.yaml key; "" == local (all existing backends load unchanged)
Formats []string // subset of {openai, anthropic}; default [openai];
                 // llama_server defaults to both (llama-server serves /v1/messages natively)
Context int      // for union kinds without a context field (vllm, cloud_api) — feeds ${MODEL_CONTEXT}
```

`backend_ref` resolution inherits all three with-override, like
`EstimatedVRAMGB`/`Mode` today. Rules:

- `host:` set (non-local) ⇒ `port:` pinned required (PickFreePort binds
  locally) and an alias required (routing is the only way to reach it).
  Backend spec emits `--host 0.0.0.0` and `--api-key <fleet token>` (5.5).
- `host:` set ⇒ all `os.Stat` validation gated off; remote paths validated by
  the converge observe pass with an actionable "not staged, run `vibe model
  ensure`" error. **Relative model paths join to `host.model_dir`** at
  launch-spec build time, so one backend file is host-relocatable.
- `tabby_api` + `host:` rejected (no aarch64/sm_121 exllamav3 path; its
  --disable-auth posture is loopback-only by design). `comfyui` + `host:`
  rejected in v1 (Spark ComfyUI overflow = dockerized `http_server`).
- **`http_server` gains `alias:`** and `serviceRouteAlias` generalizes beyond
  llama_server, so TTS/STT/rerank services auto-register proxy routes on
  activation. (Verified gap: `daemon.go` returns "" for every non-llama kind
  today, which is why utility services aren't proxy-routable.)

**Convention: backend name == served alias** (already true for all 11
existing backends). Every running backend's alias is directly routable with
zero routes.yaml config; routes.yaml holds only *virtual* aliases with
fallback chains. This keeps the capabilities.yaml migration a near-noop.

### 4.3 New union kind: `vllm`

First-class rather than `http_server`+docker because dual-node coordination
(worker units on a second host, NCCL env, start ordering, CX-7 preflight)
needs schema the generic wrapper can't express.

```yaml
# backends/v4-flash.yaml — pinned Recipe A (jasl fork, stability baseline; appendix A)
host: spark-1                        # head node (serves the API)
formats: [openai]                    # NOT anthropic until the vLLM /v1/messages smoke gate passes
context: 131072
estimated_vram_gb: 100               # per participating node
backend:
  vllm:
    # wraps jasl/vllm @ dda4668b59567416f86956cfe7bbc1eab371a61e (vLLM 0.21.1rc1 + PR #41834, SM12x)
    image: ghcr.io/spark-arena/dgx-vllm-eugr-nightly@sha256:REPLACE-digest-at-deploy
    model: deepseek-ai/DeepSeek-V4-Flash      # resolved under the host's model_dir
    alias: v4-flash
    port: 8000
    workers: [spark-2]               # rank-1 unit rendered + started on spark-2
    args:
      - --tensor-parallel-size=2
      - --kv-cache-dtype=fp8
      - --enable-expert-parallel
      - --enable-prefix-caching      # load-bearing for agent loops (27.6x warm prefill)
      - --max-model-len=131072
      - --gpu-memory-utilization=0.85   # unified-memory OOM guard
      - --max-num-seqs=2             # concurrency-collapse reports: proxy queues to 1 (6.3); revisit after commissioning
      - --reasoning-parser=deepseek_v4
      - --enable-auto-tool-choice
      - --tool-call-parser=deepseek_v4
      - '--speculative-config={"method":"deepseek_mtp","num_speculative_tokens":2}'   # nst=3 regresses
    health_path: /health
    startup_timeout: 12m             # container + 149GB weights + CUDA-graph capture
    parallel: 1                      # vamp foreach cap; V4 Flash is single-agent until proven otherwise
```

**Recipe provenance and the experiment→freeze workflow.** The pinned
image/flags in a `vllm` backend are not hand-assembled: they come from the
sparkrun / eugr recipe registries (CI-tested images; immutable dated tags
like `:2026-07-01`, never the `:latest` sentinel). Commissioning a new model
is done *interactively* with sparkrun (`sparkrun search deepseek`,
`sparkrun run <recipe> --tp 2`) until it works; the working recipe's image
digest + flags are then **frozen** into `backends/<name>.yaml`, where vibe's
systemd units give it what sparkrun doesn't: reboot survival, health-driven
coordinated pair restarts, route registration, and drift detection. vibe does
not shell out to sparkrun at runtime — the unit runs the same docker
invocation the recipe resolved to.

vibe **auto-injects** `NCCL_SOCKET_IFNAME`/`GLOO_SOCKET_IFNAME`/
`TP_SOCKET_IFNAME` from `hosts.yaml cluster.iface`, the rendezvous address
from the head's `cluster.addr`, and `NCCL_IB_GID_INDEX=3` whenever
`len(workers) > 0`; explicit `env:` wins. This is a mechanical guarantee in
code, not user-editable template content — a trimmed env block cannot
silently drop the interconnect pinning (the 14.9Gbps-misconfig failure mode
costs 38→8.4 tok/s). Containers always run `--net=host --gpus all` (NCCL
requires it; validation rejects port remaps). The `REPLACE-` sentinel gate
(already enforced by `profile.Validate`) refuses to load an unpinned image
digest.

### 4.4 New union kind: `cloud_api`

No process, no VRAM; activation = resolve credential + register routes.

```yaml
# backends/anthropic.yaml
mode: service
formats: [anthropic]
estimated_vram_gb: 0
backend:
  cloud_api:
    base_url: https://api.anthropic.com
    api_key_cmd: op read op://Private/anthropic-api/credential   # or api_key_env: ANTHROPIC_API_KEY
    inject: {header: x-api-key}
    headers: {anthropic-version: "2023-06-01"}
    models: [claude-opus-4-8, claude-sonnet-5]   # advertised + glob-routable; passthrough, untranslated
```

No key literals in YAML ever. The daemon resolves at registration and
re-resolves on 401.

### 4.5 routes.yaml (virtual aliases only)

```yaml
routes:
  coder:                                   # what harnesses send as "model"
    targets:                               # ordered; walked pre-first-byte only
      - {backend: qwen3-coder-next}        # model defaults to the backend's alias
      - {backend: v4-flash}                # promote to first after a clean week
      - {backend: anthropic, model: claude-sonnet-5}
    fallback_on: [dial_error, 429, 500, 502, 503]
    max_concurrency: 1                     # queue, don't reject: protects Spark prefix cache (6.3)
    owui: {tools: true, vision: false}     # consumed by `vibe owui sync`
  chat:
    targets:
      - {backend: gemma-4-31b-mm}
      - {backend: anthropic, model: claude-sonnet-5}
passthrough:
  - {match: "claude-*", backend: anthropic}
```

Direct aliases (every backend's own alias) need no entry. When
`target.model != alias` the router rewrites the JSON `model` field (body is
already buffered for the peek). One flat fleet-wide alias namespace —
backend aliases, route names, cloud model ids — collision-checked at daemon
start and `vibe route reload`. Hot-spare replicas of the same model on two
hosts are expressed as a chain under a virtual alias with per-host backend
names (`qwen3-coder-next@spark-1` style disambiguation); active-active
weighted balancing is explicitly out of scope for v1.

### 4.6 Platform check sets

`platform: dgx-spark` selects a built-in check/assert set (data in the vibe
binary, `internal/vibe/fleet/platform_checks.go`): driver `590.*` FAIL (GB10
CUDAGraph deadlock), CX-7 link 200Gb + MTU 9000 + peer ping, mlx5 firmware
package, clock lock `0,2150` applied, `vm.swappiness=1` +
`vm.vfs_cache_pressure=200`, DGX OS image pair-match before clustering.
`platform: cachyos` checks LTS kernel + precompiled nvidia-open module
package. Research pins update with vibe releases instead of rotting in
per-user YAML; `hosts.yaml tuning:` exists only to *override* defaults.

### What breaks (explicit)

1. Remote backends must pin ports; validator rejects `host:` + port 0.
2. Alias/port uniqueness becomes fleet-global — today's collisions (14002 x3,
   14004 x2, 8090 x4) get rejected; migration dedupes into a reserved
   15000-14999 fleet port block per host role.
3. `frontend:` becomes optional for llama_server profiles (kills the
   vestigial stubs on fast/long_form/gemma_long_form).
4. capabilities.yaml values become router aliases (profile-name fallback
   retained through the transition).
5. `${MODEL_ALIAS}`/`${MODEL_CONTEXT}` populated for all backend kinds
   (tabby bug already fixed in the working tree; `context:` on BackendDef
   covers vllm/cloud), and `VibeAPI` becomes configurable to localmodel's LAN
   address for remote-rendered configs.
6. schema.go, `Backend.isEmpty()`, the pointer-count loop, and the AGENTS.md
   schema rules all update in lockstep for host/formats/context/vllm/
   cloud_api/routes/hosts.
7. vamp's `IsVRAMRejection` string contract is replaced by typed Connect
   error details (vamp and daemon upgrade together).

## 5. Daemon / runtime

### 5.1 Packages

```
internal/vibe/remote/    # transport only: pooled ssh.Client per host, keepalive
                         # (SendRequest ticker 15s, reply deadline 10s, miss => kill+reconnect
                         # w/ backoff; mitigates golang/go #21478/#26643), Exec/Output/Push
                         # (sftp tmp+rename, 0600), journalctl -f streaming w/ auto-reconnect
internal/vibe/fleet/     # policy: hosts, desired-state rendering, batched observe pass
                         # (ONE ssh exec per host returns unit hashes/states, image digests,
                         # model manifest, boot_id, driver, clock, link speed as JSON),
                         # converge (diff table plan / apply), unit rendering, health engine
                         # (15s probe, 3-strike healthy→degraded→down, events.jsonl),
                         # model library + ensure
```

### 5.2 backendRuntime interface

```go
type backendRuntime interface {
    Start(ctx context.Context) error   // blocks until healthy or startup_timeout
    Stop(ctx context.Context) error
    Addr() *url.URL                    // http://127.0.0.1:p | http://spark-1:8000 | https://api.anthropic.com
    Healthy(ctx context.Context) bool
}
```

Implementations: `localProcess` (today's supervisor unchanged — respawn
watcher, VRAM preflight), `remoteUnit` (converge-if-drifted, `systemctl
--user start`, LAN health poll, journal streaming during start; **no respawn
watcher** — systemd owns it), `cloudEndpoint` (key resolve + probe; instant).
Everything downstream already consumes only `Status().Addr` — this is the
existing seam working as designed.

### 5.3 Per-host active slots + explicit swap

`d.active` becomes `map[string]*slotState` keyed by host; per-host semantics
are exactly today's (one exclusive GPU occupant + service residents). A
dual-node vllm backend claims the slot on head AND workers atomically.
Conflicts reject with the holder named. **`vibe start <backend> --evict`**
(and `vibe swap <backend>` sugar) stops the holder(s) — including a pair —
then starts; the vamp candidate walk never evicts (it gets typed `SLOT_HELD`
and moves to the next candidate), so pipelines can't silently tear down an
interactive session, and interactive users aren't stuck doing three-verb
dances.

At daemon boot, remote units are **adopted without restart** (hash-check +
health probe + route registration, seconds) — a vibe reinstall/daemon restart
on localmodel never touches Spark KV caches. Extending systemd-owned lifecycle to
localmodel's own local backends (so daemon restarts stop killing the 5090 model's
KV cache too) is a noted future option, not v1.

### 5.4 VRAM preflight: ledger for remote hosts

localmodel keeps nvidia-smi probing. Remote hosts use declared capacity
(`gpu.vram_gb`) minus the sum of resident backends' `estimated_vram_gb`
(unified-memory Sparks make point-in-time probes lie; page cache eats the
reading), sanity-checked against the observe pass.

### 5.5 Dual-node start/stop + auth

Start `v4-flash`: (1) CX-7 preflight — ethtool speed 200000, MTU 9000, peer
ping, mlx5 firmware — fail-fast so the silent-degradation failure mode is
caught before, not after, a mysterious slow week; (2) converge check on both
nodes; (3) worker unit then head unit; (4) poll head `/health` up to
`startup_timeout` streaming both journals; (5) register route + fire one
8-token pre-warm completion. Stop: head, worker, then re-check link state
(teardown wedging is documented; wedge → event + remediation hint). Crash: a
dead worker breaks the TP job while systemd restarts only the rank — the
head's health probe fails, backend goes degraded→down, and per-backend
`on_down: restart` does a coordinated pair stop/start with a budget (3/hour)
so a flapping unit pages via `vibe fleet status` instead of burning the night.

Auth: control plane unchanged (unix socket + :9001 bearer on localmodel; remote
hosts have no control plane). Data plane: remote model servers bind
`0.0.0.0:<pinned>` but require the **fleet token** (converge distributes the
existing state token, unit rendering passes `--api-key`; the router injects
it upstream). :9000 stays loopback by default; `bind_all` additionally
requires the token inbound — never an unauthenticated LAN inference plane in
front of metered cloud fallbacks. vibe never invokes sudo: `vibe host
bootstrap` **prints** the one-time root script (docker group, linger,
sysctls, clock-lock unit, CX-7 netplan, mlx5 packages); converge only
*asserts* those facts afterward.

## 6. Router

### 6.1 Request path

```
harness/OWUI/vamp → POST localmodel:9000 /v1/chat/completions|/v1/messages {"model": "coder"}
  1. format tag from path (/v1/messages → anthropic, else openai)
  2. peekModel (existing; cap 2→8 MiB for multimodal; over-cap streams w/o fallback, logged)
  3. exact alias → route | glob passthrough | default upstream (active profile — backward compatible)
  4. walk targets: skip format-incompatible + health-engine-down targets
  5. rewrite model if aliased; inject cloud key or fleet token; Host/TLS rewrite
  6. forward; SSE flushes immediately (existing FlushInterval -1)
```

Fallback is **pre-first-byte only** (dial error / listed status before any
byte is written; replay the buffered body against the next target).
**`first_byte_timeout` is disabled by default**: 30-250s TTFT on cold Spark
prefill is normal operation, and a timeout here would misclassify prefill as
an outage and silently spend cloud money. No request-triggered autostart — a
down target falls through the chain; ensure is a control-plane verb.

### 6.2 Registry lifecycle

Backend aliases register on activation (existing behavior, now for every
kind). routes.yaml virtual aliases are statically declared, always
registered; a route with zero healthy targets 503s with a JSON error naming
the down backends. `vibe route reload` (SIGHUP) diffs the registry without a
daemon restart.

### 6.3 Per-route concurrency

`max_concurrency` is a proxy-level semaphore that **queues rather than
rejects**. `coder: 1` single-tenants the Spark pair during coding sessions:
the documented V4 Flash failure modes (concurrency collapse to ~1 tok/s;
prefix-cache eviction turning 3s turn-TTFT into minutes) are prevented at the
router, not discovered in the session. Commissioning experiment: measure
turn-2 TTFT with and without a competing request stream; loosen only if it
passes.

### 6.4 /v1/models + OWUI

Advertise route aliases with >= 1 healthy target, plus passthrough cloud ids,
plus the active profile's models. Aliases are stable across fallbacks
(`coder` is `coder` whether V4 Flash or Claude serves it), so OWUI's manual
per-model capability flags stop churning — and `vibe owui sync` PATCHes them
from the per-route `owui:` block via OWUI's model-config API (timeboxed
feature; manual fallback documented, the admin API churns).

### 6.5 Metrics: tool-call success is a first-class column

`internal/vibe/proxy/metrics.go`: bounded TeeReader scanning — non-stream
JSON usage / finish_reason / tool_calls; SSE final-chunk usage (llama.cpp
timings preferred); Anthropic message_start/message_delta. Per-route **leak
detectors** (DSML marker fragments, stray `<think>` in content, `!!!!` runs)
catch the exact parser-bleed modes the research documented per model. One
JSONL line per request; `vibe metrics summary --by alias,backend` renders
tool-call rate / leak rate / p50-p95 TTFB. This settles V4-Flash vs
Qwen3-Coder-Next vs MiniMax by observed drop rate in *our* harnesses, not
SWE-bench deltas. No dashboard; jq is the dashboard.

## 7. Provisioning UX

```
vibe host ls|add|show|bootstrap|converge [--dry-run]|logs <host> <unit> [-f]
vibe model ls [--host] | ensure <model> --host h1,h2 [--from <peer>]
vibe fleet status                      # THE question: what runs where and why
vibe fleet logs -f                     # multiplexed journals, host-prefixed
vibe metrics summary [--by ...] [--since ...]
vibe route ls|reload|smoke <alias>     # smoke = ~50-request tool-call loop gate
vibe doctor [--host h|--fleet] [--stress]
vibe start|stop|swap|ps                # host-aware targets; --evict on start
```

`vibe fleet status` is backed by observe-pass facts + health state + metrics,
never by "what vibe last did": per host STATE/BOOT/SLOT/SERVICES/DRIFT rows,
per route TARGET(health)/FALLBACKS/req/tool%/leak/p95, and a 24h events tail
(reboots via boot_id, degradations, fallbacks, NRestarts creep).

Factory-fresh Spark to serving V4 Flash: console first-boot + ssh-copy-id →
**NVIDIA Sync Cluster Assistant** for the CX-7 fabric (requires the April
2026+ OTA; it does topology/netplan/RoCE/node-SSH/bandwidth-verify — vibe's
converge then *asserts* the result rather than configuring it) →
`vibe host add spark-1 --addr ... --user kyle` (probes arch/GPU/driver,
writes hosts.yaml) → `vibe host bootstrap spark-1` (prints the sudo script
for what Sync doesn't cover: docker group, linger, sysctls, clock lock;
user pastes once) → `vibe host converge spark-1` → **RMA-window stress test**
(`vibe doctor --host spark-1 --stress`: hours of uncapped load; overcurrent
hard-shutdowns are a per-unit hardware lottery and RMA is the only fix) →
`vibe model ensure deepseek-ai/DeepSeek-V4-Flash --host spark-1,spark-2`
(rsync from localmodel library, then spark-1→spark-2 over CX-7) → `vibe start
v4-flash` → `vibe route smoke coder` → set
`{"env":{"CLAUDE_CODE_ATTRIBUTION_HEADER":"0"}}` in ~/.claude/settings.json
(config file, not shell export — otherwise the mutating header zeroes
prefix-cache reuse and every turn re-prefills).

## 8. vamp integration

capabilities.yaml values become router aliases; resolution order: route alias
(new RPC) → backend name → profile name (compat). New control-plane RPC
`EnsureModelAvailable(alias)`:

- healthy target → returns `{base_url: proxy, model: alias}` immediately
  (vamp stops doing `/v1/models Data[0]` resolution; `modelIDForCurrent`
  dies);
- down local/remote target → blocks on coordinated start up to
  startup_timeout, streaming progress like Pull;
- slot conflict → typed `SLOT_HELD` → candidate walk moves on (never evicts);
- missing weights → typed `NOT_STAGED` naming the exact `vibe model ensure`
  command — **ensure never triggers a 200GB transfer mid-pipeline**;
  `ensure_pull: true` per capability is the explicit opt-in;
- cloud target → key resolve + probe, ~1s.

Typed Connect error details (`VRAM_EXCEEDED | HOST_UNREACHABLE | SLOT_HELD |
NOT_STAGED | NOT_FOUND`) replace the `IsVRAMRejection` substring contract.
`vamp run --warm` pre-ensures every declared capability in parallel before
wave 1 so multiple 10-minute cold starts overlap. Cross-host pipelines
(ComfyUI on localmodel + text on Sparks + TTS on llamaloft) work via per-host slots +
the per-group BaseURL/ModelID that `StageInput` already carries; foreach
fan-out caps on per-backend `parallel` instead of the singleton
`Status.Parallel`.

## 9. Roadmap (each phase lands green + independently useful)

| phase | scope | effort |
|---|---|---|
| P0 | Commit the working tree (write_files + cleanup pass) | S |
| P1 | Router v2 on one box: cloud_api kind, routes.yaml + reload, format affinity, header injection, model rewrite, pre-first-byte fallback, max_concurrency, JSONL metrics + `vibe metrics summary`, alias-advertised /v1/models. Immediately useful: Claude through :9000 (omp stops hardcoding), qwen tool-call rate measured | M |
| P2 | Remote plumbing: `internal/vibe/remote` + hosts.yaml + `vibe host add/bootstrap/converge/logs` + observe/doctor — no backend changes yet | M |
| P3 | Remote backends: `host:` on BackendDef, remoteUnit runtime, per-host slots + swap/--evict, fleet-token data plane, health engine + events. Deliver: llamaloft utility plane (embed/rerank/classifier/kokoro/whisper) as converged units surviving localmodel restarts | L |
| P4 | Model library + distribution: `vibe model ls/ensure` (rsync --partial --inplace, peer/CX-7 selection, hf fallback, size+xxh64 manifest), fleet doctor | M |
| P5 | Spark single-node: bootstrap/tuning encoding, arm64 llama-server units, qwen3-coder-next on spark-2 behind `coder`. The fleet becomes daily-driver useful here, before any dual-node fragility | M |
| P6 | Dual-node vLLM: `vllm` kind, NCCL auto-injection, CX-7 preflight, coordinated start/stop + on_down budget, `vibe route smoke` gate. Recipe pins come from commissioning with sparkrun/eugr (experiment→freeze, 4.3); flip `coder` to v4-flash-first after a clean week | L |
| P7 | vamp integration: EnsureModelAvailable, typed errors, capabilities → aliases, --warm, per-backend parallel | M |
| P8 | Hygiene: archive superseded profiles/composes, dedupe ports into the fleet block, template-expand frontend.args, `vibe owui sync`, AGENTS.md schema-rule updates | S |

P1 and P2 are independent after P0. P5-before-P6 is deliberate: single-node
Spark serving is boring and useful while the dual-node recipe is de-risked.

## 10. Top risks

1. **V4 Flash recipe fragility** (force-pushed forks, unmerged SM12x PR, open
   DSML streaming-leak bugs under tool_choice=auto). → digest+SHA pinning,
   smoke gate on every change, leak detectors, Qwen3-Coder-Next stays primary
   until a clean week; promotion is a routes.yaml edit.
2. **CX-7 silently degraded/wedged** (14.9Gbps misconfig → 8.4 tok/s). →
   preflight blocks start; post-stop re-check; doctor asserts; commissioning
   experiment: one deliberate wrong-iface NCCL run to confirm the preflight
   catches it.
3. **Spark overcurrent shutdown lottery.** → stress test inside the return
   window; clock lock; boot_id tracking makes a pattern data, not vibes.
4. **Network blip vs process death.** → vibe never respawns remote processes;
   HTTP-only health with 3-strike hysteresis; SSH keepalive separates host
   DEGRADED from backend DOWN; restart budget 3/hour.
5. **Prefix-cache eviction between agent turns.** → route max_concurrency=1;
   attribution header off; the turn-2 TTFT experiment gates any loosening.
6. **vLLM /v1/messages adapter fragility** (role-validation 400s vs newer
   Claude Code CLIs; thinking round-trip unverified). → vllm backends default
   `formats: [openai]`; llama.cpp covers Anthropic-format locally; the smoke
   gate decides if the tag ever flips.
7. **Unified-memory OOM near 127GB.** → converge-asserted sysctls +
   gpu-memory-utilization 0.85 + doctor check (the localmodel zram lesson, encoded
   this time).
8. **Schema churn bricking the daily driver.** → `host: ""` default keeps
   every existing backend loading unchanged; capability fallback retained
   through P7; breaking cleanups land last.

## Appendix A: model plan (July 2026 — re-verify at deploy time)

| slot | model | where | stack | expected |
|---|---|---|---|---|
| coding daily driver | Qwen3-Coder-Next 80B-A3B FP8 | one Spark | llama.cpp or vLLM, `--tool-call-parser qwen3_xml` | ~43 tok/s, SWE-bench ~71, parser-stable |
| coding flagship | DeepSeek V4 Flash FP8+MTP (Recipe A: jasl/vllm @ dda4668b..., vLLM 0.21.1rc1 + PR #41834) | dual Spark TP=2 | vLLM docker, deepseek_mtp nst=2 | ~44 tok/s decode, 35-44 thinking-on; 15-45s/turn cached; single-agent |
| coding flagship +60% | Recipe B: V4-Flash-DSpark checkpoint, dspark nst=3, --load-format safetensors | dual Spark | vLLM docker | ~71 tok/s bench; adopt after A is stable |
| chat (single Spark) | Qwen3.5-122B-A10B AutoRound INT4 | one Spark | vLLM | 28-38 tok/s (+MTP ~51 w/ patches) |
| chat (dual, intelligence play) | Qwen3.5-397B-A17B 4-bit | dual Spark | vLLM | ~30 tok/s, AA index 45 |
| wildcard (re-test ~Sept) | Ornith-1.0-397B; MiniMax M2.7 (needs thinking round-trip) | dual Spark | vLLM | 33 / 42 tok/s |
| fast dense + media | current Qwen3.6-27B / Gemma 4 31B + ComfyUI | localmodel 5090 | unchanged | unchanged |
| embed / rerank / classify / STT / TTS | Qwen3-Embedding-0.6B, Qwen3-Reranker-0.6B, Qwen3-0.6B, whisper large-v3-turbo, Kokoro (+Qwen3-TTS) | llamaloft 3080 Ti | llama.cpp + faster-whisper | ~9-10GB resident total |

Turnkey layer (July 2026): fabric via NVIDIA Sync Cluster Assistant;
commissioning via sparkrun (`uvx sparkrun setup`, git recipe registries,
spark-arena immutable dated image tags — never `:latest` sentinels) wrapping
eugr/spark-vllm-docker's CI-tested images (which carry the V4 Flash SM12x
patches via recipe; verified 44 tok/s TP=2+MTP practitioner run, hazyumps
variant adds EP + NCCL 2.30.4 + 384K ctx at ~31 tok/s). V4 Flash stays
patch-carried until vLLM PR #41834 or jasl's sm12x-stable lands upstream.
Both nodes must match driver/kernel/firmware exactly (mismatch costs +140%
prefill). `docker save | ssh docker load` beats registry copy for image
distribution.

Spark operational pins: DGX OS (not vanilla Ubuntu — Realtek NIC vanishing
risk), known-good stack DGX OS 7.4.0 / driver 580.126.09 / CUDA 13.0.2, avoid
driver 590.x, clock lock 0,2150, swappiness 1 / vfs_cache_pressure 200,
gpu-memory-utilization 0.85, both Sparks on identical images before
clustering, cluster traffic on the CX-7 iface IPs, MTU 9000,
NCCL_IB_GID_INDEX=3, prefix caching always on, contexts <= 64k for
interactive agentic work (TTFT 53s@32K/250s@128K cold).
