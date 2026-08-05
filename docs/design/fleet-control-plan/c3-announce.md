# C3 — The inversion: cells announce, the catalog is derived

Status: EXECUTED (2026-08-02). All six gates passed; the reference
fleet's front config is now a presence-derived artifact (authorship
flipped during gating with zero catalog disturbance).

Gate results (live, reference fleet):

1. **Ungraceful vanish: PASS.** SIGSTOP'd announcers: roaming cell
   marked stale at ~50s and pruned from the front catalog at ~65s
   (stale + render); opportunistic cell went stale and HELD (its ids
   stayed listed). `fleet.cellStale`/`fleet.cellReturned` on the events
   stream; last_seen accurate to the last heartbeat. Re-add followed
   the hysteresis exactly: healthy_streak climbed 1→2→3 over ~30s and
   the re-add render landed inside the 1/min cap window (~80s after
   resume) — prune fast, re-add slow, renders coalesced.
2. **Mid-stream membership render: PASS.** A streaming essay through
   the front completed uncorrupted ([DONE]) across the roaming-prune
   render (C0's 30s drain semantics cover the -watch-config apply).
3. **Conflict rule: PASS.** `drain_cell` via MCP on a cell without
   daemon_url went the announce path: status showed `INCONSISTENT` +
   "intent: … (requested, awaiting cell ack)"; `vibe cell resume` on
   the box between request and ack kept the cell serving, and the
   newer serving echo dropped the registry request — no retry, no
   split-brain. (MCP drain_cell/resume_cell now fall back to
   desired-intent when daemon_url is absent — the C3 "daemon_url is an
   optimization" made real.)
4. **Commissioning dry-run: PASS.** Scratch cell (llama-swap on a
   spare port + `vibe fleet announce` slim + a hosts.yaml entry + one
   shared def) appeared in fleet_status and the front catalog with
   zero front-side hand edits; teardown pruned it per class. Caught
   one real leak en route: a slim announcer on a multi-cell box
   inherited the box's unassigned defs — `gatherModels` now intersects
   defs with the cell's own llama-swap catalog (defless catalog ids
   announce hashless + log-once).
5. **Fingerprint: PASS.** Strict-marked def mutated on the cell: loud
   `fleet.fingerprintMismatch` event (expected/got/mode/defs_sha
   payload) + model excluded from the render. Advisory def mutated:
   event only, model kept serving. The gate first caught a systematic
   false mismatch on EVERY def: tilde expansion — `~/models` renders
   `/root/models` on fleetd (root) and `/home/<user>/models` on cells.
   Canonicalization now normalizes home-anchored paths to `~` (a real
   weights-path swap still mismatches — test covers both sides).
6. **Unit tests: PASS** — announce schema/validation, presence
   transitions + staleness, conflict rule both directions, command
   drain, client seq/echo/backoff/persistence, def∩catalog
   intersection, canonicalization (flag order, port strip, quoting,
   home normalization), render loop (cold-start hold, class policy,
   hysteresis, cap, unchanged-no-write, strict/advisory, never-
   announced untouched), daemon announce wiring + intent echo through
   the real listeners.

Implementation notes beyond the doc's letter:

- **desired_intent vs commands:** drain/resume ride desired_intent
  (they carry reason/eta/since and the reconciliation rule); the
  commands[] queue carries one-off verbs (unload/warm) for cells that
  can't be reached interactively. MCP drain/resume fall back to
  desired-intent when daemon_url is absent. *(C6 + the post-merge
  reconciliation PR: the queue has **three** producers, all of them
  fallbacks and all validating the model against the cell's ANNOUNCED
  set first. MIN-G landed the first in C6 — the MCP `unload_model` tool
  queues when the cell's llama-swap admin port does not answer. #26
  landed the other two, which are C4 files and could not be touched
  from C6's branch: the warm-target restore and the warm-schedule fire
  queue a `warm` when the front warm fails to deliver, or when the cell
  has no front route at all.*
  *(Corrected 2026-08-05: that second case was described here as "a cell
  known only through its announces", which cannot happen — see §1's
  commissioning line. The front's peers are rendered from `hosts.yaml`
  and every `hosts.yaml` cell carries a `url`, so "no front route" means
  the cell is **absent from the registry**: a def's `cell:`, a warm
  target or a sleep entry naming a box that was never commissioned. The
  error now says that instead.)*
  *All three share one rule: the queue is for DELIVERY failures.
  Transport errors and 5xx fall back; a definitive 4xx is the far side
  ANSWERING, and the cell would refuse the same verb identically, so it
  stays the error it is rather than being reported as "queued for the
  next heartbeat". Delivery is at-least-once, retired by an announce
  with a higher seq: deleting the batch at hand-off lost it whenever
  the response never arrived.)*
- **Announce-side defs load once at client start** (daemon or slim):
  a def edit takes effect on the next announcer/daemon restart. Defs
  change via git + converge, so this is the natural cadence.
- **RenderCount was held in a package-level sync.Map keyed by *Server**,
  and this doc claimed the render loop "couldn't extend the Server
  struct under its contract". No such contract ever existed — the same
  commit added three fields to that struct. It was an accident of
  authoring order dressed up as a constraint, and the invented
  justification is worth recording because it is the only one the C0–C4
  run produced. C6/NIT-B moved it to `Server.renderWrites`
  (`atomic.Int64`) and deleted the map and the fiction together.
- **fleet.front_config is the render mount contract**: fleetd sees the
  front's watched config dir (rw) and writes atomically into it;
  -watch-config applies. The dry-run path (C2's render_front) verified
  parity before authorship flipped.

Adversarial-review addendum (2026-08-02, two reviewers — 11+ findings,
all fixed pre-merge with regression tests):

- **False acks (HIGH)**: reconcile stamped intent regardless of verb
  outcome — a failed or unconfigured drain verb still echoed drained,
  which fleetd then resolved as satisfied, while the drained echo
  suppressed the INCONSISTENT detector. Verbs now return success and
  intent stamps only then; a verb-less cell's requests stay pending
  (visible, actionable) forever instead of lying.
- **Ghost requests (MAJOR)**: every local `vibe cell drain` on an
  announcing cell manufactured an eternal "requested, awaiting ack"
  (the C2 POST stamped a newer request than the cell's own). The POST
  is now skipped for announcing cells (the echo IS the record), and
  reconcile re-stamps an already-in-state request so it resolves.
- **Resume via announce was a silent no-op (BLOCKER)**: "serving"
  deleted the intent entry, so desired_intent could never carry it.
  The intent store now keeps a resolvable serving REQUEST for
  announcing cells (dropped when the cell echoes serving newer); for
  never-announced cells, "serving" still deletes (C1 semantics).
- **Probe evidence beats declaration**: a probe that just answered now
  stands over a drained echo (INCONSISTENT nags again); the echo
  decides availability only when probes can't reach the cell.
- **Echo clock clamp**: cell-supplied `since` is capped at now+2min —
  a forged/skewed future can't cancel requests from the future (and
  the "cell clocks are never consulted" rule holds except here).
- **Fingerprint binding**: enforcement only fires for the def's HOME
  cell — an announce from another cell carrying a garbage hash can't
  yank a strict def. Unassigned defs skip enforcement (the front's own
  checkout is their render truth).
- **Impersonation documented**: the fleet token is every cell's voice
  (announce authenticates the connection, never the name) — threat
  note added to design §6 and the fleetd README; per-cell credentials
  remain a futures item.
- Smaller: transition-gated events (no steady-state withdraw drip),
  intent.state enum + field hygiene at ingest (length, control
  chars), 64-deep command queue cap, PathEscape on piggyback unload,
  capped announce response decode, 401-distinct client logging, and
  decorate() copying presence under the lock (a torn slice read could
  panic the state handler).

## Goal

Cells dial **out** and say what they serve; the front's peer config
becomes a derived artifact re-rendered on membership transitions;
absence becomes a first-class, queryable state; commissioning a new
cell touches only the new cell. No inbound port on any cell is
required by the control plane after this phase.

## Design

### 1. Announce protocol

`POST {fleetd}/api/fleet/announce` (bearer), body:

```json
{ "v": 1, "cell": "gpu-cell", "seq": 41,
  "intent": { "state": "serving" },
  "models": [ { "id": "qwen3.6-27b", "state": "ready",
                "flags_sha256": "9f2c…", "fingerprint": "advisory",
                "probe": null } ],
  "capacity": { "vram_total_gb": 32, "vram_free_gb": 9.5,
                "disk_free_gb": 212 },
  "versions": { "llama_swap": "v247", "vibe": "…",
                "defs_sha": "abc123", "defs_dirty": false } }
```

Schema rules that are ~10 lines now and unretrofittable once
mixed-version announcers exist: `"v": 1` is required; **receivers
tolerate unknown fields** (the fleet guarantees version skew — the
laptop updates when docked, the heavy cell quarterly). The
`versions` block feeds a version matrix in `fleet_status`, and
`defs_sha`/`defs_dirty` turn a fingerprint mismatch report into
"cell is 3 commits behind" instead of a 2 a.m. mystery (the C0
canonical-checkout convention is otherwise unverified).

Response:

```json
{ "interval_s": 15,
  "desired_intent": { "state": "drained", "reason": "requested via MCP" },
  "commands": [ { "verb": "unload", "model": "qwen3.6-27b" } ] }
```

- `models` comes from the cell's local llama-swap (`/running` +
  rendered config); `state` maps llama-swap's states through.
- `probe` is a **reserved per-model field** (null in v1) for the v2
  throughput-health block (friction pain 2): a realistic batch probe
  spec + latest result for *that model*, letting the announcer mark
  it `degraded` individually. Reserve it in the schema now so v2 is
  additive. *(Filled by [C8](c8-probe-model.md), 2026-08-04: a typed
  `*AnnounceProbe` that still marshals to `null` for a cell which does
  not probe, so the reservation cost nothing and v2 was additive as
  intended.)*
- `commands` carries the piggybacked verbs (drain, resume, unload,
  warm). The cell executes and reflects results in its next announce.
  This retires the C2 requirement that fleetd can reach a cell's
  `:9001` — after C3, `daemon_url` is an optimization (lower latency
  for interactive drains), not a requirement. **Commissioning a new
  cell = install daemon/announcer + `hosts.yaml` entry + registry URL.**
  All three, and the middle one is enforced: an announce naming a cell
  the registry does not carry is refused `400 unknown cell`. There is
  therefore no such thing as a cell fleetd knows only through its
  announces — "announce-only" throughout this plan means fleetd cannot
  DIAL the cell (no reachable inbound port, no `daemon_url`), never that
  the cell is missing from `hosts.yaml`. Loosening the check would turn
  every announce into a fleet-wide write from an unauthenticated name,
  which §6's threat note already rules out.

**Conflict rule (verbatim from the design panel; do not soften):** the
cell's *echoed* intent is truth. `desired_intent` is a request; until
the cell echoes it, surfaces show "drain requested, awaiting cell ack."
A cell-side `vibe cell resume` (the human at the box) wins over a
stale registry request. No exceptions — split-brain resolves toward
the box.

### 2. Announce client

Two forms, one implementation (`internal/vibe/fleetannounce`):

- **In-daemon loop**: cells already running a vibe daemon announce
  from it, reusing the `fleet:` config block C2 §1 introduced
  (`cell`, `registry_url`, `token_file`) unchanged.
- **`vibe fleet announce` slim mode**: a flag-configured foreground
  loop for cells that run llama-swap without a full daemon (the heavy
  cell). Runs as a trivial systemd unit. Same code path.

Cadence: `interval_s` from the response (default 15s), jittered.
Failure: log-once then quiet retry with backoff; an unreachable
registry must never affect serving (invariant: control plane failure
≠ data plane failure).

### 3. fleetd: presence table

- Announces upsert a presence entry `{cell, seq, models, capacity,
  intent_echo, received_at}`; `last_seen` and availability in
  `/api/fleet/state` come from presence when a cell announces, probe
  fallback otherwise (a cell is "announcing" once seen; flag cells
  that regress from announce to probe).
- Staleness: `stale_after = 3 × interval_s + jitter allowance` (~50s
  at default cadence), derived **exclusively from fleetd-side
  `received_at`** — `seq` is a per-boot hint only (it resets on cell
  reboot) and cell-reported clocks are never consulted, which also
  retires clock skew as a failure class. Transitions publish on
  `/api/fleet/events` (the existing SSE stream). The emitted type
  strings are dotted camelCase — `fleet.cellStale`,
  `fleet.cellWithdrawn`, `fleet.cellReturned`,
  `fleet.fingerprintMismatch`, plus the probe-path
  `fleet.cellUp`/`fleet.cellDown` — and the CLI and live SSE consumers
  depend on those names, so the doc follows the code here, never the
  reverse (C6/DOC-3). `model_degraded` was reserved with no emitter;
  [C8](c8-probe-model.md) is the emitter, named `fleet.modelDegraded`
  (with `fleet.modelRecovered`) to match that convention.
- **Cold start:** on fleetd startup the presence table is empty and
  must not be mistaken for a withdrawn fleet — hold the last-rendered
  front config and defer any presence-driven re-render until
  `stale_after` has elapsed or a full announce wave has landed.
  Never write an empty-peers config because fleetd just rebooted.
- **Availability transitions are subscribable**: `vibe cell await`
  switches from polling to the events stream when available (keep the
  poll fallback).

### 4. Presence-derived front render

On membership transitions (cell withdrawn/returned/stale, model set
changed), fleetd re-renders the front peers config through the same
`render --cell front` code path (import, not shell-out), applying the
**class policy** (design doc §4):

- `roaming` cells: **prune** — on clean withdraw (an announce with
  `intent.state: "withdrawing"`, sent by the AC-power hook below) or
  staleness, their models leave the rendered `peers:` (and thus the
  catalog). Catalog honesty for cells that genuinely leave.
- `always_on` / `opportunistic` cells: **hold** — models stay listed;
  requests get typed `UPSTREAM_DOWN`. Consumer-pinned ids never 404.
- Debounce is not enough — add **hysteresis**: prune fast, re-add
  slow (a cell re-enters the render only after M consecutive healthy
  heartbeats), cap renders at ~1/min with coalescing, and expose a
  renders-per-day counter in `fleet_status` so a flap storm (laptop
  on power-saving wifi, crash-looping cell) is visible instead of
  silently churning the catalog under consumers' cached dropdowns.
  Write atomically (tmp + rename) **with the tmp file in the watched
  directory itself** (rename across filesystems fails; verify in
  C0's gate that llama-swap's watcher ignores the tmp filename);
  C0's `-watch-config` applies it with zero restarts.

Intent-aware rendering also unlocks the C2 deferral: a drained cell
can now be *rendered out* (held, marked) so unload-all drain modes
become safe — optional, keep unit-stop as the default drain.

### 5. Fingerprints as contract

- `flags_sha256` = SHA-256 over the model's rendered serving argv,
  canonicalized: drop argv[0] (binary path) and the port argument,
  sort remaining `--flag value` pairs lexicographically, join with
  `\x00`. Both sides derive from the same defs (C0's canonical-source
  convention), so a mismatch means real drift on that cell.
- Backend defs gain `fingerprint: strict | advisory` (default
  advisory). On mismatch: always publish a loud `fingerprint_mismatch`
  event + surface in status; **exclude from the front render only when
  `strict`** (embed-class models, where drift is silent retrieval
  damage). Chat-class models stay fail-open by design — do not
  "harden" this.

### 6. Roaming-cell sensing (private-repo work, tracked here)

A small hook on the laptop: on AC-power loss / pre-sleep, send one
announce with `intent: {state: "withdrawing"}` and stop the serving
stack cleanly; on AC restore + dock, resume and announce. (macOS:
launchd + `pmset -g batt` polling or a sleep-watcher; the mechanism is
house-specific.) Heartbeat staleness remains the lid-slam backstop —
the hook is an optimization that makes withdraw *clean and immediate*
instead of ~50s late.

## Acceptance gates

1. **Ungraceful vanish:** cut a cell's network (or hard-sleep the
   laptop). Roaming: models pruned from the front catalog within the
   stale window; opportunistic: still listed, requests get
   `UPSTREAM_DOWN`; events stream shows the transition; `last_seen`
   accurate.
2. **Mid-stream membership render:** trigger a presence-driven
   re-render while a slow-start stream is in flight through the front
   (C0's gate, now fired by the real mechanism). Stream survives.
3. **Conflict rule:** issue `drain_cell` via MCP; before the cell
   acks, status shows "requested"; run `vibe cell resume` on the box
   between request and ack; the cell stays serving and the request is
   dropped, not retried forever.
4. **Commissioning dry-run:** bring up a scratch cell (llama-swap +
   slim announcer on a spare port) with only a `hosts.yaml` entry and
   the registry URL; it appears in status and the front catalog with
   zero front-side hand edits; tear down; it prunes/holds per class.
5. **Fingerprint:** mutate one serving flag on a `strict` def's cell;
   loud event + model excluded from render; same mutation on an
   `advisory` def: event only, still served.
6. Unit tests: canonicalization (flag order, port stripping),
   staleness state machine, class policy render matrix, conflict rule.

### Independent re-run (2026-08-05, local multi-cell harness)

Gate 1's class-policy half was re-run from scratch on
[`scripts/fleetlab`](../../../scripts/fleetlab/README.md) — a fleet
built for the purpose, with one cell of each class — and reproduced the
reference fleet's result on different hardware:

- **Prune half.** All three announcers SIGKILLed at once. Front render
  before: `alpha=[lab-chat, lab-embed-a] bravo=[lab-embed-b]
  charlie=[lab-embed-c]`. After staleness, fleetd logged `pruning roaming
  cell from front render cell=charlie stale=true withdrawn=false` and
  rewrote the config to alpha+bravo only — the `always_on` and
  `opportunistic` cells **held** their model ids while equally stale.
- **Hold half.** With the opportunistic cell's llama-swap actually
  stopped, the front still listed it and
  `POST /v1/embeddings model=lab-embed-b` returned **502** with
  `peer proxy error: dial tcp …: connection refused` — the id resolved
  and the failure was typed at the peer, not an unknown-model 404. That
  is the gate's `UPSTREAM_DOWN` requirement, observed.
- **Re-add hysteresis.** With the announcers restarted, `healthy_streak`
  climbed 1…5 with the roaming cell's peer stanza absent throughout,
  then `roaming cell re-added to front render cell=charlie
  healthy_streak=5`. Re-add is slower than prune, as designed.

What this re-run does **not** cover: a laptop that physically leaves the
LAN. SIGKILL is a faithful stand-in for a vanished announcer and not for
a vanished route.

## Out of scope

The v2 `probe` throughput block (reserved only), warm targets/page
(C4), any data-path tunneling (absent cells are answered by the front,
never by fleetd — invariant 1).

## Live-gate addendum (2026-08-05): "announce-only membership" was never a state

Found while running the C4/C10/C13 live gates against a real 4-cell
fleet on one box (four llama-swap v239 processes, a real fleetd, real
announcers). Fixed on `fix/live-gate-truth`; **the code was right and
the docs were wrong**, which is ground rule 8 doing its job.

Several documents — this one's reconciliation note, the design doc's
commissioning line, C10's evidence section, `AGENTS.md`'s piggyback
rule, and a `frontCanRoute` comment plus a test *name* in `fleetapi` —
described a cell "fleetd knows only through its announces", or one
"fleetd has no `url` for". Neither can happen:

- `POST /api/fleet/announce` refuses a cell absent from `hosts.yaml`
  with `400 unknown cell "…" (not in the registry)`
  (`fleetapi/announce.go`). A cell cannot announce itself into
  existence.
- `fleetcfg` requires a `url` on every cell it does carry, so every
  registry cell has one.

So §1's commissioning line is the correct statement of the contract and
the rest were loose paraphrases of it: what C3 retires is the inbound
PORT, not the membership record. The check stays exactly as strict — an
announce endpoint that accepted unknown cell names would be a
fleet-wide write from an unauthenticated NAME, and §6's threat note
already says the fleet token authenticates the connection and never the
cell it claims to be. A forged announce can fake SERVING, prune a
roaming catalog or cancel a pending drain; letting it also *create* the
cell it does that to is not a trade this plan makes.

**"Announce-only" keeps its useful meaning and only that one**: fleetd
cannot DIAL the cell (no reachable inbound port, no `daemon_url`), so
announces are the only channel to it. That is the sense C4's
`observesActivity`, C8's piggybacked probes and C10's `--idle` refusal
all use, and all three were already correct.

One code consequence. `warmtarget.go`'s "the cell has no front route"
branch is reached when fleetd holds no `url` for a cell — which, given
the above, means the cell is **not in the registry**. It said
`(announce-only)`, which sent an operator looking for a dead announcer,
and the piggyback attempt that follows then confirmed the wrong
diagnosis with "cell has never announced". It now names the missing
`hosts.yaml` entry (`fleetapi.noFrontRoute`, one helper for all three
producers: the warm target, the warm schedule and C14's post-wake
warm). The reachable production shape is a backend def whose `cell:`
names a box that was never commissioned — the daemon already skips a
`warm_target` naming an unknown cell at wiring, but nothing validates a
def's `cell:` against `hosts.yaml`.
