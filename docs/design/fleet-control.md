# Fleet control: node state, intent, and the control plane

Status: MERGED THROUGH C9 + C11 (2026-08-04), C10 IN REVIEW — C8 is
probe_model, C9 the alarm notifier and C11 hold_model; C10, the await
extensions (`--model --ready`, `--idle`, the lease handshake), is the
fourth of the first four v2 backlog items and is in review on its own
branch. Every phase C0–C9 and C11 is on `main` (#18–#25, #27, #28, #30),
plus one post-merge reconciliation PR for the three items no
single phase branch could reach. **Merged is not live-gated:** C5's,
C6's, C7a's, C7b's, C8's, C9's, C10's and C11's live gates need real
cells and were NOT run — the
phase docs and the README's status column say per phase which gates are
mechanical (green, repeated under `-race`) and which are still owed.
That README's status column is the authoritative per-phase state.

Synthesized from a five-reader research pass
(the three design docs, this codebase, the private house repo's ops
history, the flagship consumer, and a llama-swap / orchestrator-landscape
web pass), then a four-designer panel (reuse-maximal, purpose-built
panel service, automation-first, registration-first) scored by an
adversarial judge against a real night-long ops friction log. The
reuse-maximal design won as the base; the registration inversion is
folded in as the destination. Implementation is phased in
[fleet-control-plan/](fleet-control-plan/) (C0–C7); the year-two
backlog and adoption notes from a four-lens futurespective live in
[fleet-control-futures.md](fleet-control-futures.md).

Companion to [router-lifecycle.md](router-lifecycle.md) (cells, JIT/TTL,
rendering — that doc remains the lifecycle authority; nothing here
touches model lifecycle) and [topology.md](topology.md) (cell roles).
This doc **claims topology.md §3 build items 1 (per-cell render), 4
(fleet dashboard), and 5 (warm schedules)** and schedules them. It
consumes the `hosts.yaml` designed in [fleet.md](fleet.md) §4.1 by
adding a `cells:` section; fleet.md's `hosts:` inventory and converge
design stand unchanged.

Host and model names refer to the *reference fleet* (see topology.md);
substitute your own. Instance values — addresses, tokens, compose
stanzas, launchd plists — live in a private fleet repo, never here.

## 1. The problem

One night of moving inference and embedding work between three hosts
produced a friction log (preserved in the private fleet repo,
`docs/ops-friction-llama-servers-2026-08-02.md`) whose distilled pains
are the requirements for this design:

1. **No unified lifecycle verb.** Five process regimes coexisted (nohup,
   systemd user unit, launchd, vibe daemon, docker compose);
   start/stop/status/logs differ on every one.
2. **Nothing supervises health as throughput.** llama-server can degrade
   10–100× while `/health` stays green; only realistic batch probes or
   domain-truth signals (rows/min) catch it. Watchdogs were hand-built.
3. **Consumers hold static pointers.** Every move meant editing N config
   surfaces (front peer YAML, consumer `.env`s, application DB rows) and
   restarting N things. A stale peer entry sat silently until request
   time.
4. **Flag fingerprints are convention, not contract.** Embedding serving
   flags must match everywhere or retrieval silently degrades; nothing
   validates a running server against the expected flags.
5. **Load/unload is opaque.** The unload lever wasn't discoverable in
   the field; TTL behavior was unobservable; VRAM contention between
   co-resident servers resolved by whoever crashed first.
6. **Health endpoints and exit codes serve their own lifecycle, not
   supervision.** Supervisors need domain-truth signals; each one was
   improvised.

The log also proved a pattern: a connect-back relay (a resource dials
*out*, registers what it serves, consumers hit a stable front, absence
is a clean 503-with-last-seen). That inversion is the destination shape
here — reached last, not first, because most of the value ships earlier
with far less machinery.

And one meta-lesson: half of pain 5 was a **discoverability failure,
not a capability gap** — the unload API existed all along. This design
therefore starts by assembling and surfacing what exists before
building anything.

## 2. What already exists (the inventory that shapes the design)

| capability | where it lives | status |
|---|---|---|
| Model lifecycle: JIT start, TTL, matrix eviction, `DRAINING` semantics | llama-swap per cell (router-lifecycle.md) | running |
| Eject lever: `POST /api/models/unload/{model}`; per-cell web UI `/ui` (SSE-fed: model states, load/unload buttons, logs, metrics) | llama-swap ≥ v239 | running, under-documented |
| Availability protocol: consumer-side typed `UPSTREAM_DOWN` classification of the front's gateway errors (connect failures + 502/503/504 → `RouterError{UPSTREAM_DOWN}`, vamp's `internal/vamp/routererr.go`); batch consumers treat it as "defer and catch up" | consumers (the front itself emits plain gateway errors) | shipped |
| Fleet aggregation: `GET /api/fleet/state`, `GET /api/fleet/events` (SSE), start-duration history | `internal/vibe/fleetapi` (router-lifecycle A8a) | written; registry is a one-element slice at construction (`daemon.go`), additive by design |
| Remote authed control plane: Connect RPC on `:9001`, bearer token, `${VIBE_API}`/`${VIBE_TOKEN}` (README "Remote access") | vibe daemon | shipped |
| LAN-reachable cell proxy: `proxy_bind_all` | vibe daemon | shipped (PR #16) |
| MCP wire pattern: initialize / tools-list / tools-call over HTTP JSON-RPC | `internal/vibe/search/mcp.go` (320 lines) | shipped, clonable |
| Config render: backend defs → llama-swap config, `--check` drift gate, `--extras` merge, `peers:` emission for `cloud_peer` defs | `internal/vibe/router/render.go`, `vibe router render` | shipped |
| Outage-free config reload | llama-swap `-watch-config` | **enabled** in `deploy/front` (C0, 2026-08-02): dir mount + poll-based watcher (2s); verified live — new config active immediately, in-flight streams drain under a hardcoded 30s shutdown timeout, then force-close (clean EOF) |
| Wake-on-LAN with request buffering | llama-swap companion `wol-proxy` | exists upstream (upstream docs; unverified here); unadopted |

What does **not** exist anywhere: a place to record *intent* ("GPU
reclaimed for gaming, back ~23:00"), a host-level availability view
with last-seen, a unified verb over the process regimes, and a
rendered — rather than hand-maintained — front peer list.

## 3. The design in one paragraph

Add **one always-on control-plane deployment** of the existing vibe
daemon binary ("fleetd") on the front host, running the already-written
fleetapi aggregator over a cells list, plus three small new things: an
**intent store** (one JSON file), **drain/resume verbs** (Connect RPCs
that run a per-cell configured command), and an **MCP facade** (clone
of the search MCP pattern) so conversational agents flip fleet state
through typed tools instead of editing YAML. Make the front's peer
config **rendered** (`vibe router render --cell front`) and reloaded
via `-watch-config` so membership changes stop causing outages. Then,
as the final phase, invert discovery: cells **announce** themselves to
fleetd (heartbeat carrying models + flag fingerprints + intent echo,
with commands piggybacked on responses), and the front render derives
from live presence. The data plane — client → front → cell llama-swap —
is never touched; the SSE keepalive behavior that passed the six-client
gate is structurally unaffected because no new hop is added.

```
                      ┌──────────────────────────────────────────────┐
                      │ front host (always-on)                        │
clients ────────────► │  front  llama-swap:cpu  :9000   (exists)      │
(chat UIs, harnesses, │    peers-only config — RENDERED, hot-reloaded │
 pipelines, phones)   │    via -watch-config (C0/C2)                  │
                      │                                               │
 MCP clients ───────► │  fleetd  vibe daemon  :9001     (C1)          │
 (conversational      │    fleetapi aggregator (existing code)        │
  agents)             │    + intent store (JSON file)                 │
                      │    + /mcp facade (clone of search/mcp.go)     │
                      │    + announce registry (C3)                   │
                      └───────┬──────────────┬──────────────┬────────┘
                       probes │ + RPC (C1-2) │              │
                       announce heartbeats ◄─┘ (C3, dials OUT from cells)
                  ┌───────────┴────┐  ┌───────┴────────┐  ┌─┴──────────────┐
                  │ gpu-cell       │  │ roaming cell    │  │ heavy cell     │
                  │ llama-swap:9000│  │ (laptop)        │  │ (spark pair)   │
                  │ vibe daemon    │  │ llama-swap:9000 │  │ llama-swap:9000│
                  │  :9001         │  │ vibe daemon:9001│  │ + slim announce│
                  └────────────────┘  └─────────────────┘  └────────────────┘
```

## 4. The state model: three orthogonal axes

Conflating intent with availability is the failure mode: a design that
conflates them either nags the operator or routes to dead nodes. The
axes are kept separate because two of the three are already owned by
existing systems.

**Axis 1 — availability (observed, never stored).** Host reachability
(cheap TCP probe) + cell reachability (`/running`, `/v1/models` — the
probes fleetapi already does). From C3, presence comes from announce
heartbeats instead, with probes as fallback. Availability is never
declared by a human.

**Axis 2 — intent (declared, tiny, optional).** One JSON file owned by
fleetd:

```json
{ "gpu-cell": { "state": "drained", "reason": "gaming",
                "since": "2026-08-02T21:04:00Z", "eta": "23:00" } }
```

Absence of an entry means *serving* — the file is empty almost always.
Written by `vibe cell drain --reason …`, the MCP `drain_cell` tool, or
a bare curl. **Intent is for humans and agents asking why; it is never
consulted for request routing** — routing truth stays `UPSTREAM_DOWN`.
Intent is only ever declared, never inferred: the control plane must
not guess "drained?" from observations and act on the guess (display
uncertainty, yes; act, no).

**Axis 3 — model residency (llama-swap-owned, never duplicated).**
`stopped / starting / ready / draining` + TTL, straight from each
cell's llama-swap, exactly as fleetapi already merges it. Which model
the heavy cell holds lives here and only here.

**Node classes** qualify how absence is interpreted (set per cell in
`hosts.yaml cells:`):

| class | example | absence means | alarm? | catalog policy on absence (C3) |
|---|---|---|---|---|
| `always_on` | front, heavy cell, utility cell | something is wrong | yes | hold — ids stay listed, requests get `UPSTREAM_DOWN` |
| `opportunistic` | gpu-cell workstation | off, or reclaimed | no | hold |
| `roaming` | laptop | left the building | no | prune on clean withdraw or stale-timeout — the catalog stays honest |

The hold/prune split serves two masters: always-on consumers pin model
ids that must never 404 (hold), while a roaming cell's models genuinely
aren't part of the fleet when it's on a train (prune).

**The alarm column has a destination from C9** (`fleet.notify`): fleetd
evaluates that column against this table's own derived display states
and delivers it to one webhook. `always_on` absence alarms *only when
intent does not explain it* — a declared drain (DRAINED / OFF) is not an
alarm, an absence with no entry (DRAINED?) is. `opportunistic` and
`roaming` absence never alarms, and `INCONSISTENT` is a nag rather than
a page. Delivery — never evaluation — is gated by a declared fleet-scope
`notify.scope` (away/home) whose suppressions stay visible in
`fleet_status` and are digested on return.

**Membership** (which cells exist, which models each serves, with what
flags) is *config, not state*: backend defs + the `cells:` map,
rendered into the front's peers file. It changes rarely, through git.

### Derived display states (computed at read time, never stored)

| host | cell | intent | display |
|---|---|---|---|
| up | up | — | **SERVING** (+ resident models, or "up-cold, will JIT") |
| down | up | — | **SERVING** — a responding cell is proof of life; the host probe (e.g. a firewalled SSH port) loses to it |
| up | down | drained | **DRAINED** ("gaming, since 21:04, eta 23:00") |
| up | down | none | **DRAINED?** — deliberate stop or crash loop; flagged with a deep link to cell logs, never acted on |
| down | — | drained | **OFF** (was drained first) |
| down | — | none | **OFF/AWAY** with `last_seen` (per-cell transition timestamps recorded — and persisted — starting in C1; today's watcher keeps only a boolean up/down map) |
| no probe | down | none | **OFF/AWAY?** — without a `host_probe` the host/cell distinction is unknowable; shown with last_seen |
| up | up | drained | **INCONSISTENT** — resume forgot to clear intent; status nags until cleared |

## 5. Interfaces

**MCP facade (first-class — the operator's existing workflow is asking
an agent).** `internal/vibe/fleetmcp`, mounted at `/mcp` on the daemon
mux behind the same bearer auth, wire pattern cloned from
`internal/vibe/search/mcp.go`. Tools, in order of arrival:

| tool | phase | effect |
|---|---|---|
| `fleet_status()` | C1 | the derived-state table + resident models + start ETAs from history |
| `warm_model(model)` | C1 | 1-token request through the front — JIT *is* the start verb; reports ETA |
| `unload_model(cell, model)` | C1 | `POST {cell}/api/models/unload/{model}` |
| `drain_cell(cell, reason?, eta?)` | C2 | drain RPC to that cell's daemon + intent write |
| `resume_cell(cell)` | C2 | inverse; clears intent |
| `wake_cell(cell)` | C2 | Wake-on-LAN magic packet; explicit, never automatic |
| `render_front(dry_run?)` | C2 | `vibe router render --cell front` (`--check` when dry_run) |
| `probe_model(cell, model, rebaseline?)` | C8 | ask the cell to measure one RESIDENT model against its own baseline; refuses a cold model rather than loading it |
| `fleet_notify_scope(scope, reason?, until?)` | C9 | declare away/home — gates alarm DELIVERY only; alarms keep firing and stay visible in fleet_status |
| `fleet_notify_test(message?)` | C9 | send one message through the real webhook path (not an alarm: no dwell, no dedup, no away gate) |
| `hold_model(cell, model, for?, note?)` | C11 | suspend fleetd's own warm policy on a cell until an expiry — the evaluation afternoon. Stored as a lease with `hold: true`; not a pin (llama-swap's TTL is untouched) |
| `release_hold(cell, model)` | C11 | end a hold early; holds expire on their own |

**CLI.** `vibe cell status | await | drain | resume | wake | hold` — local
verbs run the configured per-cell command; remote verbs go through the
cell daemon's `:9001` Connect RPC (`${VIBE_API}`/`${VIBE_TOKEN}`
machinery, already shipped). `vibe cell await <cell> --up` is the
"parked batch job waits for the GPU box" primitive.

**HTTP.** Existing `GET /api/fleet/state` + `/api/fleet/events`; new
`POST /api/fleet/intent` (C1); `POST /api/fleet/wake` and
`POST`/`DELETE /api/fleet/lease` (C2); `POST /api/fleet/announce` (C3);
`POST /api/fleet/notify/scope` and `POST /api/fleet/notify/send` (C9).

*Amended C12.* Every route is declared in one table
(`fleetapi/routes.go`) that is both what the daemon mounts and what each
route grants below the control-plane token: `AccessTokenOnly` (the
default and everything undeclared, `/mcp` and the Connect RPCs
included), `AccessGuest` (the optional read-only bearer — exactly
`GET /api/fleet/state` and `GET /api/fleet/events`), `AccessPublic`
(exactly `GET /ui/fleet`, C5's static-asset exemption). Enforcement
stays in the daemon's bearer middleware, as a positive allowlist on
exact (method, path) evaluated before the mux cleans the path.

**Advisory leases (C2).** A batch consumer can register "I hold
`<model>` on `<cell>`: mid-batch, N rows left" with a TTL. Leases are
advisory only — they appear in the pre-drain report and in
`fleet_status`, they never block anything. They turn "did I just
strand a 19-hour job?" into a visible answer at drain time.

*Amended C4 + C11.* "Never block anything" still holds for everything
outside this control plane — no request, no drain, no resume, no render
ever waits on a lease. What a lease DOES defer is **fleetd's own
automatic policy**: C4's scheduled warms skip a leased cell (the
eviction-fight guard), C8's probes skip it, and C11's `hold: true`
leases additionally suspend the warm-target restore. The distinction to
keep: a lease constrains what fleetd initiates, never what an operator
or a client asks for.

**Web.** Deliberately last (C4): one static embedded page over
`/api/fleet/state` + `/events`, with thin buttons over the same
mutation endpoints and deep links to each cell's llama-swap `/ui`.
Until then the per-cell `/ui` is the model-level dashboard (the design
docs already call it "the interim dashboard") — what it can never show
is intent and cross-cell state, which live in `fleet_status` output.

## 6. Discovery: probes first, announce as the destination

C1–C2 observe availability by probing, and the front's peer list is
rendered from config. That already retires the stale-address class and
the reload outage. What it cannot retire: a dead peer is invisible
until probed or requested, config still flows *toward* cells, and every
cell needs an inbound port open to the front host (a real cost — one
field host's firewall blocks inbound by default, and the macvlan
pattern hides host ports from containers).

C3 inverts it, following the relay pattern the fleet already proved for
MCP servers: each cell's daemon (or a slim `vibe fleet announce` unit
where no full daemon runs) heartbeats to fleetd —

```json
{ "cell": "gpu-cell", "seq": 41,
  "intent": { "state": "serving" },
  "models": [ { "id": "qwen3.6-27b", "state": "ready",
                "flags_sha256": "…", "fingerprint": "advisory",
                "probe": null } ],
  "capacity": { "vram_total_gb": 32, "vram_free_gb": 9.5 } }
```

— and the response carries desired intent and piggybacked commands
(drain, unload, warm), which **retires the inbound-port requirement
entirely**: commissioning a new cell becomes "install the daemon, point
it at the registry." Two rules keep it honest:

- **The cell's echo wins.** Registry-side intent is a *request* until
  the cell echoes it; the UI shows "drain requested, awaiting ack."
  No split-brain: the box you're standing at is always right.
- **Fingerprints become a contract.** `flags_sha256` is computed from
  the rendered serving argv (minus binary path and port, home paths
  normalized). Mismatch always raises a loud event; it excludes the
  model from the front render (**fail-closed**) only for defs marked
  `fingerprint: strict` — embed-class models, where drift is silent
  retrieval damage — and only when the mismatch comes from the def's
  OWN cell (another cell's announce can never yank it). Chat models
  stay fail-open: a hash-normalization bug must not yank a working
  model from the catalog.

**The trust note (written down because it's easy to miss):** the fleet
bearer token is *every cell's voice*. `POST /api/fleet/announce`
authenticates the connection, never the cell name — any token holder
can announce as any registered cell, and a forged announce can fake
SERVING for a dead box, prune a roaming cell's catalog entries (via a
fake `withdrawing`), and cancel pending drain/resume requests (via a
forged newer echo — bounded by a future-skew clamp at ingest). This is
parity with the token's existing powers (MCP drain_cell, unload_model,
intent POST) for the documented LAN posture, but treat token
distribution as cell-root. Per-cell announce credentials are a futures
item (both-direction per-cell auth).

The front render becomes presence-derived (with the class-based
hold/prune policy above), debounced, written to the watched config —
picked up with zero restarts. fleetd's probe loop demotes to a fallback
for cells that don't announce.

## 7. The three scenarios, end to end

**Workstation GPU reclaim (intent-driven).** `vibe cell drain --reason
gaming --eta 23:00` — or the same sentence to an agent, which calls
`drain_cell`. The pre-drain report shows in-flight work and any
advisory leases first; then the cell's llama-swap unit stops. The stop
does **not** let generations finish: llama-swap's SIGTERM path calls
`CloseStreams()` *before* its graceful drain (v239, established by C2's
live gate), so in-flight streams are cancelled at the stop and
`--wait <dur>` is what lets them finish first — the response now says
whether that wait actually happened (C6). Requests for that cell's ids
then fail with the gateway errors consumers already classify as
`UPSTREAM_DOWN`, batch consumers defer by design, chat users pick
another model. Intent (reason + ETA) is visible in every status
surface. One writer per invocation path: when the drain is invoked
through fleetd/MCP, fleetd records intent only after the drain RPC
succeeds; a drain invoked locally at the box writes intent itself,
best-effort. A failed drain never records intent.
`vibe cell drain --until-exit -- <game>` wraps the session and resumes
deterministically on exit — resume is *never* triggered by a GPU-idle
heuristic (rejected: a 2 a.m. surprise generator). Powered off? Nothing
to do: OFF/AWAY with last-seen. `wake_cell` sends WoL when explicitly
asked.

**Laptop undock (availability-driven).** Zero control-plane actions per
dock cycle, by construction: absence is detected, not declared. C0
gives the cell reboot autonomy (autostart its serving stack); C3 adds
AC-power sensing for a *clean* withdraw (models pruned from the catalog
before the lid closes) with heartbeat staleness as the lid-slam
backstop. A hardware upgrade that makes the laptop a serious inference
node is a config diff — new backend defs, re-render — with no
control-plane change.

**Heavy-cell model swap (choice-driven).** llama-swap already is this
control plane: name the alternative model in any request (matrix
evicts the default), or press the button in the cell `/ui`, or
`warm_model` via MCP. The default model returns via a **warm-target
policy: restore only after the swapped-in model goes request-idle**
(C4) — never on a timer, which would evict the model the operator just
chose, possibly mid-session. Cold-start ETA comes from the persisted
start-duration history (built for exactly this).

## 8. Invariants

1. **The data plane is never touched.** No new hop between client and
   model. The six-client SSE gate's guarantees are preserved
   structurally, not re-verified per phase.
2. **Intent is declared, availability is observed, residency is
   llama-swap-owned.** No component stores what another owns.
3. **No silent rerouting, no silent fallback** (inherited from
   router-lifecycle.md). The control plane changes what the catalog
   *says*, never where a request *goes*.
4. **fleetd is read-and-request-only.** If it dies, inference is
   unaffected; `vibe cell status` degrades to direct probes.
5. **The mutation surface is the daemon RPC**, bearer-authed, already
   shipped. No SSH keys inside containers, no docker.sock mounts.
6. **Boundary rule.** Mechanisms in this repo (public, generic);
   instance values — addresses, tokens, plists, compose overrides — in
   the private fleet repo.

## 9. Rejected alternatives (by name, with reasons)

| alternative | verdict |
|---|---|
| **llama-dash** (purpose-built llama-swap dashboard, MIT, active 2026) | Closest adopt candidate; rejected: no intent concept, single-instance focus, duplicates the A8a substrate this repo already owns. Re-evaluate if the C4 page grows ambitions. |
| **GPUStack** | Polished fleet UI but a whole-platform commitment: deploys its own engines, can't front externally managed servers, current workers are Linux-only. |
| **llamactl** | Credible multi-node manager (llama.cpp/MLX/vLLM) but replaces llama-swap as the routing layer rather than sitting on top. |
| **paddler** | Right presence/buffering ideas, wrong shape: embedded llama.cpp only, replaces the serving stack. Steal its scale-from-zero buffering idea if ever needed. |
| **olla / LiteLLM router** | Redundant with llama-swap peers / cloud-gateway-shaped respectively. |
| **exo** | Solves a different problem (sharding one model across pooled devices). |
| **Home Assistant / MQTT in the control loop** | Ground truth lives on boxes that already run a daemon; HA would be one more config surface — the disease pain 3 describes. Fine as a *consumer* of `/api/fleet/state` later. |
| **GPU-idle auto-resume** (nvidia-smi polling + timer) | Heuristic that acts on its own; `--until-exit` is the deterministic version. |
| **Pin-via-keep-warm for the heavy cell** | A pinned default re-warms on a timer and evicts the model the operator just swapped in. Restore-after-idle instead. |
| **Blanket fail-closed fingerprints** | Fail-closed only for embed-class; a normalization bug must not yank a working chat model. |
| **Registry on the data path** (absent-cells answered by fleetd with reasoned 503 bodies) | Nice UX, violates invariant 1. Revisit only if typed `UPSTREAM_DOWN` + status surfaces prove insufficient. |

## 10. Friction-pain scorecard at plan completion

| pain | end state |
|---|---|
| 1 — lifecycle verbs | One verb facade (`vibe cell …` / MCP) over the regimes; regime count also shrinks (C0). Underlying regimes remain — honest partial. |
| 2 — throughput health | **Answered (C8, 2026-08-04)**: the reserved per-model `probe` block is filled by a cell-side canned probe scored against that model's own rolling baseline, surfaced as `degraded` in `fleet_status` + `fleet.modelDegraded`. One deliberate departure from this row's original sketch: a degraded model is **NOT** withdrawn from the render. Yanking a slow-but-serving id turns it into a fleet-wide 404 for every consumer pinning it, and a fail-closed action on a measurement with a false-positive tail is the "blanket fail-closed fingerprints" alternative §9 already rejects. The remediation is a human verb (`unload_model`, then probe again). |
| 3 — static pointers | Front half retired: peer list rendered (C2), then presence-derived (C3); consumers already point at the stable front. |
| 4 — fingerprints | Contract via `flags_sha256` at announce (C3); strict for embed-class. |
| 5 — opaque load/unload | Retired: existing levers surfaced as verbs, residency + evictions visible in status/events. |
| 6 — supervision signals | One place answers "what is this cell doing and why" (state ⋈ intent ⋈ residency ⋈ last-seen). Domain-truth throughput signals ride with pain 2. |

## 11. Risks and unverified assumptions

- **`-watch-config` is verified on llama-swap v239** (C0,
  2026-08-02) — and that guarantee is conditional on actually pinning:
  `deploy/front`'s compose floats the `:cpu` tag and the digest pin is
  opt-in, so an unpinned pull can move off the verified build. Behaviour
  as verified on v239: poll-based watcher (2s) on the config path; the
  parent-directory mount sees atomic-rename writes; reloads activate
  the new config immediately and drain in-flight streams on a
  **hardcoded 30s** shutdown timeout — a longer stream is force-closed
  (clean EOF) at the grace boundary. Membership edits are rare enough
  that this beats the restart status quo; a configurable drain is an
  upstream contribution, not fleet config.
- **Down-peer catalog behavior** (does the front's `/v1/models` keep
  listing a dead peer's ids?) is unknown upstream; the design depends
  only on request-time `UPSTREAM_DOWN` until C3 makes the catalog
  presence-derived, which settles it locally.
- **Peer `models:` lists are static in llama-swap config.** Fine here:
  C2 renders them from defs; C3 re-renders on membership transitions.
  If llama-swap grows peer auto-discovery upstream, C3's render loop
  simplifies but nothing breaks.
- **Cell config pushes still kill warm models** (llama-swap cannot
  adopt running processes). The render/converge path must surface this
  cost loudly; the pending A6 cell-restart acceptance test covers it.
- **WoL from a macvlan container** (magic packets need L2 broadcast)
  must be validated on the front host; fallback is sending from any
  LAN box, or adopting upstream `wol-proxy`.
- **fleetd is one more always-on thing.** Mitigated by invariant 4 and
  by it being the same binary/deployment pattern as every other vibe
  daemon.
- **The front *host* dying is the one total-fleet outage** (data
  plane, control plane, and model library ride the same always-on
  box). Not solved here; the cold-standby identity + runbook is
  futures item 12 — at minimum, make the front address a DNS name
  early so recovery never means touching every consumer config.
- **The SSE-keepalive defense is upstream behavior, not structure.**
  Invariant 1 protects it from *this* plan, not from a llama-swap
  upgrade. The canary-cell → six-client-gate → fleet upgrade ritual
  (futures item 13) is the actual protection; adopt it with C0's
  digest pin.
