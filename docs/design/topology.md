# Fleet topology & user experience

Status: PROPOSED 2026-07-13. Companion to `router-lifecycle.md` (the router
mechanics — llama-swap cells, JIT/TTL, rendering) and `fleet.md` (the
provisioning half — hosts.yaml, converge, model distribution). This doc
answers: **what runs where, and what does using it feel like** for each of
the four use cases.

## 1. The boxes and their roles

| box | hardware | availability | role |
|---|---|---|---|
| **unraid** | no GPU, 128GB DDR4, iGPU, big storage | always-on | **The front.** llama-swap front cell (CPU image, peers only — zero local models), persistent OWUI, NPM + Authelia, SearXNG, toqui, fleet dashboard. The one URL everything talks to. |
| **anvil** (this box) | 5090 32GB | opportunistic (dev + games) | GPU cell: 27B/31B-class JIT models + ComfyUI. Explicitly allowed to be off or full of game. |
| **loft** | 3080Ti 12GB, R5 2600, 32GB | always-on (once built) | Utility-plane cell: embed/rerank/classifier/STT/TTS pinned (`ttl: 0` + preload, ~9-10GB), small swap pool in the remainder. |
| **spark pair** | 2× DGX Spark, 125GB unified, QSFP400 | always-on (once built) | Heavy cell on spark-1 (owns both nodes): dsv4flash via dual-node vLLM; single-node models (e.g. qwen3-coder-next) on spark-2. |
| **cloud** | — | always | `cloud_peer` defs on the front (anthropic today; others are one def away). Never a silent fallback — always an explicit model id. |

The key move vs today: **the front relocates from anvil to unraid.** The
front is a pure proxy (the official `ghcr.io/mostlygeek/llama-swap:cpu`
image runs it; peers-only config, no GPU, no model binaries), and it must
live on the box that is never off, because it is the stable address every
client — phone, coding harness, toqui, vamp — is configured with, forever:

```
http://unraid:9000/v1          (LAN / VPN, OpenAI + Anthropic formats)
https://llm.<domain>           (via NPM, if/when an HTTPS name is wanted)
```

Cells appearing (loft built, sparks commissioned) or disappearing (anvil
gaming) changes the *catalog*, never the *address*. When anvil relocates
its front role it keeps a cell llama-swap on anvil:9000 — but with its
`anthropic` peer removed (the front owns cloud peers; otherwise the front
importing anvil's catalog would double-list claude). Anvil-local clients
also repoint to the unraid front for a uniform catalog; the peer-hop relay
is proven unbuffered (0.7ms first byte, router-lifecycle §17), so the extra
LAN hop costs nothing perceptible.

### Availability semantics (honest, not magical)

- Request to a model on a **down cell** → typed UPSTREAM_DOWN error from
  the front, immediately. The phone UX is "pick a model that's up," and
  the fleet dashboard / OWUI catalog shows what that is. No silent
  rerouting (design law from router-lifecycle §7).
- Request to a model on an **up cell but cold** → JIT load with streamed
  loading state. Verified through two hops for 420s holds.
- anvil gaming: either leave it (VRAM-exceeded surfaces as a clean
  in-stream error — the documented no-preflight residual) or
  `systemctl --user stop llama-swap` before a session, which flips the
  whole cell to honest UPSTREAM_DOWN. A `vibe cell drain/resume` wrapper
  is a nice-to-have, not a requirement.

## 2. The four use cases, end to end

### UC1 — persistent chat/research/websearch (the Claude-app replacement)

**Stack (all on unraid, all off-the-shelf):** OWUI as a normal unraid
docker app behind NPM + Authelia — same pattern as every other app —
plus a SearXNG container for web search. OWUI is a PWA: "install" it from
the phone browser and it lives on the home screen like the Claude app did.

- Auth: Authelia forward-auth with trusted-header SSO
  (`WEBUI_AUTH_TRUSTED_EMAIL_HEADER=Remote-Email`) so there is ONE login,
  not Authelia-then-OWUI. NPM must strip inbound `Remote-*` headers
  (standard trusted-header caveat — a client that can send the header IS
  that user).
- Models: OWUI points at `http://<unraid>:9000/v1` (localhost once the
  front is there). The dropdown is the live fleet catalog: spark models,
  anvil models when awake, loft utilities, claude. Picking a cold model
  just works — JIT + loading-state hold is the core router feature.
- Sharing is inherent: if a coding session already has dsv4flash loaded
  on the spark cell, a phone chat addressed to `dsv4flash` lands on the
  same vLLM instance (batched alongside), and resets its TTL. One model,
  N clients, zero coordination.
- Interim (today, before loft/sparks): stand this up pointed at
  anvil:9000. Claude peer models work even mid-game (cloud needs no VRAM);
  local models work when anvil is awake. Migrating later = repointing one
  URL (or zero, if the front moves to unraid first).
- NOT reused: the distillery OWUI stacks. Those are episodic, tool-loop
  tuned, vibe-profile-launched appliances on anvil; this is a persistent
  general instance with its own webui.db on unraid's SSD cache.

### UC2 — multi-model agentic coding with cloud offload

Harnesses (omp / qwen-code / pi / opencode / Claude Code) run wherever you
work and address models by id through the front: local worker ids
(`qwen3.6-27b-mtp-q6_k` on anvil; later `qwen3-coder-next` on spark-2,
`dsv4flash` on the pair) and cloud ids (`claude-opus-4-8`,
`claude-sonnet-5`) in one catalog, one base URL, both API formats. Cloud
offload is explicit model choice inside the harness (omp already routes
default/smol/task tiers), not router magic. The Opus-architect +
local-worker kanban handoff (see agentic-coding-handoff memory) rides on
this unchanged — both sides are just model ids on :9000.

### UC3 — LLM backend for self-hosted apps (toqui, and the next one)

Pattern for any app: `OPENAI_BASE_URL=http://<front>:9000/v1`, a model id
from the catalog, and design for lazy loading (first request after idle
takes seconds-to-minutes; stream it or show a warming state). Apps on
unraid reach the front over localhost; nothing exposes :9000 past the
LAN/VPN. Utility calls (embeddings, rerank, classification) go to loft's
pinned models — instant, never cold. Big-model calls share whatever the
fleet already has warm.

**toqui specifics** (surveyed 2026-07-13; `backend/internal/ai/`): it is
already fleet-shaped. Its OpenAI-compatible provider streams always, does
tool calls heavily, needs no embeddings, does no startup health check,
has a 5-minute per-request timeout, and retries connection-refused/5xx
with backoff — i.e. lazy loading works with zero toqui changes. Wiring:

```
AI_PROVIDER=openai
OPENAI_BASE_URL=http://<front>:9000/v1
OPENAI_API_KEY=anything-nonempty
OPENAI_MODEL_FAST / _SMART / _BEST = <catalog ids>
```

Three constraints its survey surfaced:
- **Tier mapping must be swap-safe.** One toqui turn runs up to 7
  sequential LLM calls (`maxToolLoopIterations`) and can alternate tiers
  call-to-call. If FAST and SMART map to models that evict each other on
  one cell, every turn becomes a swap storm. Rule: within one cell, tiers
  map to the SAME model or to a co-resident group (llama-swap groups,
  `swap: false`) — e.g. today all three tiers → `qwen3.6-27b-tools` on
  anvil; later FAST → a small always-warm spark/loft model grouped
  alongside SMART/BEST → `dsv4flash`.
- **It is a tool-loop client**, so on Qwen it takes the `-tools` def
  (visible-content tool calls), same rule as the distillery stacks. Image
  input arrives as `image_url` data-URLs — if trips use photos, the tier
  model needs vision (gemma-4-31b-mm locally; dsv4flash has it).
- **Its Claude provider is hardcoded to api.anthropic.com** — only the
  OpenAI-compatible provider routes through the front. Fine (cloud-direct
  is allowed); just don't expect toqui's claude path to appear in fleet
  metrics.

### UC4 — vamp / pipelines

Already integrated (router-lifecycle A7): capabilities resolve to backend
ids, typed RouterErrors, streaming warm probes, keep_warm lease heartbeat,
`vamp run --warm` overlapping cold starts. Cross-host pipelines (ComfyUI
on anvil + text on sparks + TTS on loft) are just per-stage base URLs on
the same front — fleet.md §8's per-group BaseURL/ModelID carries it.

## 3. Gaps: build vs buy

Off the shelf (no code):
- Front on unraid: official llama-swap CPU docker image + a peers-only
  config file on a share. unraid-native.
- OWUI + SearXNG + NPM + Authelia on unraid: existing app patterns;
  trusted-header SSO is config, not code.
- Phone app: OWUI PWA.

Build (all small, and all already-roadmapped except the first):
1. **Per-cell rendering** — `vibe router render --cell <name>`: hosts.yaml
   (fleet.md §4.1) gains a `cell:` assignment per backend def; the front
   render emits peers-only (cells + cloud), each GPU cell render emits its
   local defs. Extends the A2 renderer; the alias/owner rules already
   exist. This is the one new piece this doc adds to the roadmap.
2. **Config distribution** — fleet.md P2 converge (render → hash → scp →
   restart unit), with an unraid variant (docker restart over SSH instead
   of systemd user unit).
3. **Model distribution** — fleet.md P4 `vibe model ensure` unchanged.
4. **Fleet dashboard** — the A8a substrate (/api/fleet/state + /events)
   needs its web UI; host it on unraid behind Authelia like everything
   else.
5. **Warm schedules (optional)** — a cron hitting the front with a 1-token
   request warms dsv4flash before waking hours; llama-swap `hooks` only
   cover startup, so scheduled warming lives outside (cron/systemd timer,
   or a `vibe warm` subcommand later if the cron annoys).

## 4. Sequencing (interleaves with router-lifecycle §17 remainder)

1. **Now (no new hardware):** persistent OWUI + SearXNG on unraid behind
   Authelia, pointed at anvil:9000 — UC1 in interim form, and it shakes
   out the NPM/Authelia/PWA UX while the fleet is still one box.
2. **Front relocation:** llama-swap:cpu on unraid + `--cell` rendering;
   anvil demotes to a cell (drops its anthropic peer); every client
   repoints to unraid:9000 once, forever. Do this before the new boxes so
   they join as peers with zero client churn.
3. **Loft arrives:** provision (fleet P2/P3), utility plane pinned, front
   peers it. UC3's embeddings/classify path goes always-on.
4. **Sparks arrive:** commissioning per dgx-spark recipes; spark-2
   single-node first (fleet P5), pair + dsv4flash after (P6/A6). UC1's
   daily-driver model and UC2's heavy worker go live.
5. **A8b cleanup:** once loft hosts bge-embed, delete vibe's proxy.go +
   LLM respawn paths (router-lifecycle §17).
