# Fleet topology & user experience

Status: PROPOSED 2026-07-13, largely built out since. Companion to
`router-lifecycle.md` (the router mechanics — llama-swap cells, JIT/TTL,
rendering) and `fleet.md` (the provisioning half — hosts.yaml, converge,
model distribution). This doc answers: **what runs where, and what does
using it feel like.**

Cell names, hardware and model ids below describe a *reference fleet* —
the one this design was built and proven against. Yours will differ; the
roles and the rules are the transferable part.

## 1. Cell roles

| role | typical hardware | availability | what it does |
|---|---|---|---|
| **front** | no GPU; whatever box is never off | always-on | `deploy/front`: llama-swap in peers-only mode (CPU image, zero local models) — the one URL everything talks to. Naturally also the **model library** host: a bulk share that downloads land in once. |
| **GPU cell** | a workstation-class card | opportunistic | Chat/vision/coder models JIT-loaded, plus ComfyUI. Explicitly allowed to be off, or full of game. |
| **utility cell** | a small always-on GPU | always-on | The utility plane: embed/rerank/classifier/STT/TTS pinned (`ttl: 0` + preload), a small swap pool in the remainder. |
| **heavy cell** | large unified memory, fast interconnect | always-on | The big models — including multi-node serving where the interconnect allows. |
| **roaming cell** | a laptop that docks | roaming | Serves like a GPU cell while docked on AC; leaves the building without notice. Absence is normal, not an incident — its catalog entries prune when it goes (fleet-control.md §4). |
| **cloud** | — | always | `cloud_peer` defs on the front. Never a silent fallback — always an explicit model id. |

The central move: **the front lives on the box that is never off**, not
on the GPU box. The front is a pure proxy (the official
`ghcr.io/mostlygeek/llama-swap:cpu` image runs it; peers-only config, no
GPU, no model binaries), and it is the stable address every client —
phone, coding harness, vamp — is configured with, forever:

```
http://<front-host>:9000/v1     (LAN / VPN, OpenAI + Anthropic formats)
https://llm.<domain>            (via a reverse proxy, if an HTTPS name is wanted)
```

Cells appearing or disappearing changes the *catalog*, never the
*address*. A box that previously hosted the front keeps a cell
llama-swap of its own — but drops its cloud peers, since the front owns
those and otherwise the same cloud ids appear twice in the catalog.
Clients on that box repoint to the front too, for a uniform catalog; the
peer-hop relay is proven unbuffered (0.7 ms first byte,
router-lifecycle §17), so the extra LAN hop costs nothing perceptible.

### Availability semantics (honest, not magical)

- Request to a model on a **down cell** → typed UPSTREAM_DOWN error from
  the front, immediately. The UX is "pick a model that's up," and the
  catalog shows what that is. No silent rerouting (design law from
  router-lifecycle §7).
- Request to a model on an **up cell but cold** → JIT load with streamed
  loading state. Verified through two hops for 420 s holds.
- A GPU cell in use for something else (gaming, training): either leave
  it — VRAM-exceeded surfaces as a clean in-stream error, the documented
  no-preflight residual — or stop its llama-swap unit, which flips the
  whole cell to honest UPSTREAM_DOWN.

## 2. The four use cases, end to end

### UC1 — persistent chat/research/websearch

An Open WebUI instance plus a SearXNG container for web search, deployed
as a normal application stack behind whatever reverse proxy and SSO the
host already uses. Open WebUI is a PWA: "install" it from a phone
browser and it lives on the home screen like a vendor chat app.

See `docs/openwebui-client.md` for the wiring and the four gotchas
(persistent config masking env, pinning the session secret,
trusted-header SSO forgery, tool-loop model defs).

- Models: point it at `http://<front-host>:9000/v1`. The dropdown is the
  live fleet catalog — heavy-cell models, GPU-cell models when awake,
  utility models, cloud. Picking a cold model just works; JIT plus the
  loading-state hold is the core router feature.
- Sharing is inherent: if a coding session already has a model loaded on
  a cell, a phone chat addressed to the same id lands on that instance
  (batched alongside) and resets its TTL. One model, N clients, zero
  coordination.
- Keep the persistent general instance separate from episodic,
  profile-launched appliances. Different lifecycles, different storage.

### UC2 — multi-model agentic coding with cloud offload

Harnesses (omp / qwen-code / pi / opencode / Claude Code) run wherever
you work and address models by id through the front: local worker ids
and cloud ids in one catalog, one base URL, both API formats. Cloud
offload is an explicit model choice inside the harness, not router
magic. An architect-model + local-worker handoff rides on this
unchanged — both sides are just model ids on `:9000`.

### UC3 — LLM backend for self-hosted apps

Pattern for any app: `OPENAI_BASE_URL=http://<front>:9000/v1`, a model
id from the catalog, and design for lazy loading (the first request
after idle takes seconds to minutes; stream it or show a warming
state). Apps co-located with the front reach it over localhost; nothing
exposes `:9000` past the LAN/VPN. Utility calls (embeddings, rerank,
classification) go to the utility cell's pinned models — instant, never
cold. Big-model calls share whatever the fleet already has warm.

Rules surfaced by putting a real tool-loop app on this:

- **Tier mapping must be swap-safe.** A multi-call agent turn can
  alternate tiers call-to-call; if two tiers map to models that evict
  each other on one cell, every turn becomes a swap storm. Within one
  cell, map all tiers to the SAME model or to a co-resident group
  (llama-swap groups, `swap: false`).
- **Tool-loop clients need the tool-calling def** (for Qwen-family
  models, the `-tools` variant with visible-content tool calls). Vision
  input needs a vision-capable tier model.
- **Cloud-direct providers bypass the front** and won't appear in fleet
  metrics — allowed, just expected.

### UC4 — vamp / pipelines

Already integrated (router-lifecycle A7): capabilities resolve to
backend ids, typed RouterErrors, streaming warm probes, keep_warm lease
heartbeat, `vamp run --warm` overlapping cold starts. Cross-host
pipelines (ComfyUI on the GPU cell + text on the heavy cell + TTS on the
utility cell) are just per-stage base URLs on the same front —
fleet.md §8's per-group BaseURL/ModelID carries it.

## 3. Gaps: build vs buy

Off the shelf (no code):
- The front (`deploy/front`, infra): official llama-swap CPU image plus a
  peers-only config file — plain compose, nothing built. Applications
  are separate stacks consuming its published `:9000`, so infra and apps
  upgrade independently.
- Chat UI: Open WebUI + SearXNG behind an existing proxy/SSO stack;
  trusted-header SSO is config, not code.
- Phone app: the Open WebUI PWA.

Build (all small, and all already roadmapped except the first):
1. **Per-cell rendering** — `vibe router render --cell <name>`:
   backend defs gain a `cell:` assignment (hosts.yaml, fleet.md §4.1,
   lists the cells); the front render emits peers-only (cells + cloud),
   each GPU cell render emits its local defs. Extends the A2 renderer;
   the alias/owner rules already exist. *Scheduled: fleet-control C2.*
2. **Config distribution** — fleet.md P2 converge (render → hash → scp →
   restart unit), with a docker-host variant (docker restart over SSH
   instead of a systemd user unit).
3. **Model distribution** — fleet.md P4 `vibe model ensure` unchanged.
4. **Fleet dashboard** — the A8a substrate (`/api/fleet/state` +
   `/events`) needs its web UI. *Scheduled: fleet-control C4.*
5. **Warm schedules (optional)** — a cron hitting the front with a
   1-token request warms a model before waking hours; llama-swap `hooks`
   only cover startup, so scheduled warming lives outside it.
   *Scheduled: fleet-control C4.*

Items 1, 4 and 5 — plus node state, intent, and cell presence, which
this doc never covered — are designed in
[fleet-control.md](fleet-control.md) and phased in
[fleet-control-plan/](fleet-control-plan/) (C0–C4).

## 4. Sequencing

The order that avoids client churn, whatever the hardware:

1. **Chat UI first, pointed at whatever cell exists today.** UC1 in
   interim form shakes out the proxy/SSO/PWA experience while the fleet
   is still one box.
2. **Relocate the front** to the always-on box (llama-swap:cpu +
   `--cell` rendering); the GPU box demotes to a cell and drops its
   cloud peers; every client repoints once, forever. Do this *before*
   adding boxes so they join as peers with zero client churn.
3. **Utility cell** — provision (fleet P2/P3), pin the utility plane,
   peer it from the front. UC3's embeddings/classify path goes
   always-on.
4. **Heavy cell** — single-node first (fleet P5), multi-node after
   (P6/A6). UC1's daily driver and UC2's heavy worker go live.
5. **Cleanup** — once the utility cell hosts the embedding service,
   delete vibe's proxy.go and LLM respawn paths (router-lifecycle §17).
