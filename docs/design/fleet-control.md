# Fleet control: node state, intent, and the control plane

Status: MERGED THROUGH C21 (2026-08-06). Every phase C0–C21 is on `main`
(#18–#25, #27–#33, #38–#44), plus a post-merge reconciliation PR (#26)
for the three items no single phase branch could reach and a
reconciliation pass (C22) that applied C15–C21's shared-doc changes in
one go. The v2 backlog beyond C14 landed as C15 (the llama-swap
credential), C16 (the upgrade ritual), C17 (gate closure), C18
(`vibe model try`), C19 (front-failover identity), C20 (the invariant
harness) and C21 — which is a **rejection**, not a feature: the
visible-repoint alias tier is refused, with the argument recorded.
**Merged is not live-gated:** many live gates ran on
[`scripts/fleetlab`](../../scripts/fleetlab/README.md),
which is four llama-swap processes on one box — CPU models are not GPU
models and one box is not a fleet. A real S3 suspend, a magic packet on
a real NIC, a laptop that physically leaves the LAN and a second
physical box taking the front's address have **not** been exercised.
[fleet-control-plan/README.md](fleet-control-plan/README.md)'s status
column and its owed table are the authoritative per-phase state.

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
[fleet-control-futures.md](fleet-control-futures.md). (The plan was
scoped C0–C7 when this paragraph was written; it ran to C21.)

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

*Amended C19.* fleetd's state dir is the one thing in this picture that
dies with the front host, and it now has a documented off-host path:
`vibe fleet mirror` is a HOST command on a timer (never a fleetd loop —
it has to keep working when fleetd is what broke), and
[../runbooks/front-failover.md](../runbooks/front-failover.md) is the
recovery. **Failover is MANUAL by design.** An automatic front
promotion is the silent rerouting invariant 3 forbids, one layer down
from a router that retries a dead cell elsewhere, and the failure it
would produce — two boxes answering on the same address, cells
announcing to one and clients routed to the other, with both catalogs
plausible — is the clearest available illustration of why that
invariant is worth its cost. The only thing the code contributes to
that problem is a **refusal**: `restore` dials the recorded addresses
first and stops if anything answers, with `--force` for the operator
who has already moved the address. fleetd's own involvement is reading
the receipt the command leaves behind, which doctor renders as
`mirror.age` and `mirror.contents`.

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

*Amended C20.* "Fail toward no evidence, never toward confirmed idle"
has been prose in this doc since C4 and now has a type:
`internal/vibe/observed.Value[T]`, whose zero value is UNKNOWN and whose
value is unexported, so a caller cannot read a measurement without
also handling its absence. `Server.InFlight` and the two activity
stamps return one. The rule was restated in five phases because absent
evidence kept reading as a healthy zero; the point of the type is that
the shape stops being writable on the paths that matter, rather than
being detected after the fact.

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

*Amended C24.* This axis gains a second class of author. A cell unit's
own stop hook is one of the declarers — the reserved reason
`stopped out of band` (`fleetapi.StopIntentReason`, written by fleetd
when a hook posts `state: unit_stopped`) marks a record whose author is
the unit rather than a human. It is still a declaration, not an
inference: nothing reads a probe and concludes intent. What separates it
from every other entry on this axis is that it carries **no why**, so
the control plane refuses it five things a human's declaration gets — it
is never handed back as `desired_intent`, it loses to the cell's own
drained echo, it never counts as a pending request, it never overwrites
an entry that does carry a why, and **it does not explain an absence**
(an `always_on` cell whose stack crashed fires the same `ExecStopPost`
as one an operator stopped, and must alarm exactly as before). The
paired `ExecStartPost` (`unit_started`) retires the record and nothing
else. The first of those five is the load-bearing one: an announcing
cell answers a drained `desired_intent` by RUNNING `cell_cmds.drain`, so
a record handed back would stop the stack it only described — measured
on the harness, where the same cell's llama-swap survives the record and
dies to a human's drain in the same run
(`scripts/fleetlab/gate-c24-stop-record.sh`). Packaging: `deploy/cell/`.

**Open, deliberately**: a stop record renders **DRAINED** with the
reserved reason, which is what C24 chose and compensated for by making
every why-consuming surface ignore it. The alternative — display stays
`DRAINED?`, the intent block carries the record — is arguably truer to
the display table below and costs nothing in C9 or the doctor, but it
makes `Display == DRAINED?` no longer imply `Intent == nil`, which
several surfaces read as a pair today.

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
aren't part of the fleet when it's on a train (prune). *Clarified C21:*
an alias declared on a roaming cell's def prunes **with** it — the id
404s rather than moving to another cell's model. That is not new
policy; "the catalog stays honest" is what this row has always said, and
C21 found the code quietly doing the opposite.

*Amended C14.* An `opportunistic` cell may also be absent because a
declared `sleep_schedule` put it there: a cron suspend, deferred by
in-flight work, leases, holds, a declared drain and a quiet window, and
paired with a wake that clears the record before sending the packet. It
adds no state to this table — the sleeping box is an ordinary declared
drain with a reserved reason and the wake time as its ETA, so it reads
as OFF with "asleep per sleep_schedule, eta 07:15". Its absence still
never alarms; what alarms is the wake that did not deliver.

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

*Amended C26a.* **The rendered catalog is a namespace**, and every
client-facing id in it — a `models` key, an alias, a cloud peer's model —
is unique by construction, with `router.Render` refusing a config that
says otherwise. A peer's model ids are canonical in the same sense def
names are, so an alias equal to one is unresolvable and `alias_owner`
cannot arbitrate it (that key settles which of two *alias claimants*
wins). Peer ids are not themselves claimants, because `Render` emits no
aliases for a peer stanza.

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
| `fleet_doctor()` | C13 | the read-only audit: every "is it still wired up" check at once (auth, def parity, the llama-swap version matrix, TLS expiry, disk, wake config, announcer liveness, plus intent / lease / fingerprint / probe / warm / ledger / notify hygiene). Four levels, and UNKNOWN means the check could not be evaluated — never "fine". It mutates nothing and is safe to call mid-incident |
| `suspend_cell(cell, reason?, force?)` | C14 | put an opportunistic box to sleep (`cell_cmds.suspend`), guarded by in-flight work, leases, holds, an outstanding probe, recent activity and a declared drain. `force` overrides tonight's conditions, never the structural refusals (the front, the wrong class). `wake_cell` is the way back |

**CLI.** `vibe cell status | await | drain | resume | wake | suspend | hold` — local
verbs run the configured per-cell command; remote verbs go through the
cell daemon's `:9001` Connect RPC (`${VIBE_API}`/`${VIBE_TOKEN}`
machinery, already shipped). `vibe cell await <cell> --up` is the
"parked batch job waits for the GPU box" primitive. Two fleet-scoped
verbs sit beside them: `vibe fleet doctor` (C13, the audit above) and
`vibe fleet mirror` (C19, the off-host state capture — a HOST command on
a timer, deliberately not a fleetd loop, whose receipt feeds doctor's
`mirror.age` / `mirror.contents`).

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
entirely**: commissioning a new cell becomes "install the daemon, add
its `hosts.yaml` entry, point it at the registry." The registry entry is
not optional and never becomes so — `POST /api/fleet/announce` refuses a
cell absent from `hosts.yaml`, because an announce that accepted an
unknown NAME would be a fleet-wide write from an unauthenticated one
(§6: the token authenticates the connection, never the cell it claims to
be). What C3 retires is the inbound port, not the membership record.
Two rules keep it honest:

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

*Amended C12 + C15.* **The fleet now holds three credentials with three
different blast radii, and none of them is interchangeable with
another.** (1) The control-plane bearer above: every verb, every cell's
voice. (2) C12's optional guest bearer: exactly
`GET /api/fleet/state` and `GET /api/fleet/events`, and nothing else,
enforced as a positive allowlist. (3) C15's per-cell `swap_key_file`:
the API key a cell's **llama-swap** demands (`apiKeys:`), presented on
the data plane's own port as `Authorization: Bearer`. The third is the
one that is easy to conflate with the first, so the front settles it —
in the reference deployment the front runs llama-swap and no daemon at
all, so it has no daemon token to reuse, and it is the cell every warm
goes through. Measured on v239: **`apiKeys` gates everything except
`/health`.** `/v1/models`, `/running`, `/api/events`,
`/api/metrics/activity`, `/api/models/unload/*` and
`/v1/chat/completions` all 401, and a wrong key gets 401 rather than
403 — so a keyed fleet without this credential loses not only its warms
but every probe, the whole in-flight evidence stream and every idle
window built on it. `/health` answering proves nothing about the rest
of a llama-swap. One further consequence is structural: the front's
config is a DERIVED artifact fleetd rewrites on every membership
transition and the renderer emits no `apiKeys`, so `fleet.front_extras`
— the operator-owned half of that file — is part of the credential, not
an accessory to it. The cell-side dialers (the announcer, C8's prober,
the cell's own usage collector, C18's trial prober) deliberately present
**no** credential — a scope boundary, not an oversight, with the seam
named in code (`fleetapi.ReadOwnSwapVersion`, the one entry point
allowed to skip the authorizer, guarded by a test that fails if the
exemption outlives its caller). Closing it is an **open item that is not
yet in the futures backlog**; the argument and the failure mode it
leaves behind — `announceOnce` mapping a `gatherModels` failure to an
EMPTY model list, which C4's warm policy reads as "restore" — are in
[C15's phase doc](fleet-control-plan/c15-warm-auth.md).

The front render becomes presence-derived (with the class-based
hold/prune policy above), debounced, written to the watched config —
picked up with zero restarts. fleetd's probe loop demotes to a fallback
for cells that don't announce.

## 7. The three scenarios, end to end

**Workstation GPU reclaim (intent-driven).** `vibe cell drain --reason
gaming --eta 23:00` — or the same sentence to an agent, which calls
`drain_cell`. The pre-drain report shows in-flight work and any
advisory leases first; then the cell's llama-swap unit stops. The stop
does **not** reliably let generations finish, and C16 corrected what
actually happens at it: llama-swap's SIGTERM path calls
`CloseStreams()`, which closes the **event** streams at once
(`/api/events` drops in ~1 ms) while in-flight **inference** streams
keep flowing and are force-closed at a hardcoded **30 s** — the same
grace a `-watch-config` reload gets, not a contrast with it. Measured on
real v239 *and* v247 binaries and gated in
`internal/swaptest/behaviour_test.go`; the earlier claim here, that
inference streams are cancelled at the stop, was wrong.
`--wait <dur>` is unaffected and still required, because a generation
longer than the grace is truncated either way — which is what C2's ~39 s
live gate actually observed. The response says whether that wait
happened (C6). Requests for that cell's ids
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
   llama-swap-owned.** No component stores what another owns. Its
   corollary, made explicit by C14: a DECLARED action may be DEFERRED by
   observation, and an observation may never INITIATE one.
3. **No silent rerouting, no silent fallback** (inherited from
   router-lifecycle.md). The control plane changes what the catalog
   *says*, never where a request *goes*. Its corollary, made explicit by
   C21: **an exclusion removes a catalog id; it never re-points one.**
   What the catalog says about an id may change only when a human
   declares it. C19 is the same invariant one layer down — a front that
   promoted itself automatically would be rerouting the whole fleet.
4. **fleetd is read-and-request-only.** If it dies, inference is
   unaffected; `vibe cell status` degrades to direct probes.
5. **The mutation surface is the daemon RPC**, bearer-authed, already
   shipped. No SSH keys inside containers, no docker.sock mounts.
6. **Boundary rule.** Mechanisms in this repo (public, generic);
   instance values — addresses, tokens, plists, compose overrides — in
   the private fleet repo.

*Amended C20.* Four of these are now enforced mechanically rather than
by review, and the gates are worth knowing by name: the llama-swap
**conformance matrix** (`internal/swaptest`, replayed against every
recorded wire version in CI, plus real binaries where available);
`internal/astscan`, the reusable "every function that does X must call
Y" scan behind C15's credential rule and C4's warm-class rule;
`internal/shelllint` over `scripts/`; and `internal/mutation`, a
registry that re-runs the `| mutation | red |` tables the phase addenda
already carried and reports UNPROTECTED when a guard's mutation leaves
every named test green. None of them replaces ground rule 9's review
pass — they encode the classes that pass has found repeatedly, which is
a different thing.

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
| **GPU-idle auto-resume** (nvidia-smi polling + timer) | Heuristic that acts on its own; `--until-exit` is the deterministic version. C14's `sleep_schedule` is the same line drawn once more: a cron DECLARES the suspend and observed idleness may only DEFER it. Observed idleness *initiating* an action stays rejected. |
| **Pin-via-keep-warm for the heavy cell** | A pinned default re-warms on a timer and evicts the model the operator just swapped in. Restore-after-idle instead. |
| **Blanket fail-closed fingerprints** | Fail-closed only for embed-class; a normalization bug must not yank a working chat model. |
| **Registry on the data path** (absent-cells answered by fleetd with reasoned 503 bodies) | Nice UX, violates invariant 1. Revisit only if typed `UPSTREAM_DOWN` + status surfaces prove insufficient. |
| **Visible-repoint alias tier** (a catalog id like `best-coder` whose target fleetd re-resolves on membership transitions, shown in the catalog and evented) | Rejected 2026-08-06 ([C21](fleet-control-plan/c21-alias-tier.md)). Its two safe cases — candidate present, no candidate at all — are already what a declared alias plus §4's class policy do; the entire delta is the substitution case, which answers `200 OK` from a model the caller did not name. Prune and hold are both fail-LOUD, and this would be the first mechanism here that is not. The proposed defence, visibility, lands on the OPERATOR (event, `fleet_status`, the page) while the harm lands on the CONSUMER, whose only channels are `/v1/models` — which attributes an id to a peer, never to a model — and the completion response, which is endpoint-dependent (a chat response named the concrete model; an embeddings response echoed the alias back). Making it visible there means rewriting responses at the front, which is invariant 1. The workaround is a declared alias with `router.alias_owner` moved by hand: one line, in the diff, on the operator's clock — membership through git, as §4 says. C21 also closed the INVISIBLE version of this that had shipped since C3: alias ownership was resolved over the defs that survived the prune, so a pruned roaming owner handed its alias to a co-claimant on another cell, with no event and nothing in `fleet_status`. Revisit conditions are named in the phase doc §10. |
| **Live shadow routing at the front** (the front duplicates each model-dispatched request to a candidate as it flows, and scores the copy) | Rejected 2026-08-08 ([C25](fleet-control-plan/c25-bench-replay.md) §2). Ground rule 1 permits observing the data plane; a shadow is a second *emission*. On a single-GPU cell it contends for the GPU serving the request it is measuring, so there is no version of it that leaves hot-path latency unchanged; it must buffer the request body before forwarding, which changes flush timing; a shadow for a non-resident candidate JIT-loads it and evicts the model serving the user, which is C8's cardinal rule violated in its worst direction; and its failure modes are either backpressure on a user's request or a silently short n. It is also a residency decision taken by the front, which axis 3 gives to llama-swap. The replacement is offline replay of captures llama-swap already holds and is already going to evict. |
| **Automatic front failover** (a standby that promotes itself when the front stops answering) | Rejected by C19, on invariant 3 rather than on cost. `vibe fleet mirror` makes a MANUAL recovery fast; the code's only contribution to two-boxes-answering is a refusal (`TakeoverProbe`) an operator can override. See §3. |

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
  2026-08-02) — and that guarantee is conditional on actually pinning.
  It was not, and the fleet paid for it: `deploy/front`'s compose
  floated the `:cpu` tag, a pull moved the front onto **v247**, and
  v247's in-flight wire change silently disarmed eight busy guards.
  **Closed by C16**: the reference compose default *and* `.env.example`
  are digest-pinned, `TestReferenceFrontStackShipsADigestPin` fails if
  either goes back to a floating tag, and moving the pin is
  `scripts/upgrade/ritual.sh` (preflight → record → canary → gate →
  pin), never an edit. Whether a given deployment is pinned can only be
  DECLARED (`fleet.front_image`, doctor's `front.image_pin`) because
  fleetd has no docker socket and must not grow one; what catches a
  declaration nobody applied is the observed `versions.llama_swap`
  matrix, which C16 gave its first producer. Behaviour
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
  box). **Partly answered by C19** — `vibe fleet mirror` captures the
  state that dies with the host and
  [../runbooks/front-failover.md](../runbooks/front-failover.md) is the
  path back, in ~10 s on the harness. It is not solved: recovery is
  MANUAL by design (§3), and the drill has **never run on metal** — a
  second physical box taking the front's address over a real LAN, with
  a real DNS change, is C19's L2 and remains UNRUN. Make the front
  address a DNS name early so recovery never means touching every
  consumer config.
- **The SSE-keepalive defense is upstream behavior, not structure.**
  Invariant 1 protects it from *this* plan, not from a llama-swap
  upgrade. The canary-cell → six-client-gate → fleet upgrade ritual
  **shipped as C16**, with the front digest-pinned by default and the
  two load-bearing upstream behaviours (the loading-state keepalive and
  SIGTERM's stream grace) gated against real binaries rather than
  assumed. What is still owed there is the ritual's own last mile: the
  six-client `gate` step and the pin applied on the real front have not
  been run (C16 L5, L6).
