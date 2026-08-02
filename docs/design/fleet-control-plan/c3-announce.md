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
  desired-intent when daemon_url is absent.
- **Announce-side defs load once at client start** (daemon or slim):
  a def edit takes effect on the next announcer/daemon restart. Defs
  change via git + converge, so this is the natural cadence.
- **RenderCount is held in a package-level sync.Map keyed by *Server**
  (the render loop couldn't extend the Server struct under its
  contract) — one Server per process in practice; noted for a future
  tidy.
- **fleet.front_config is the render mount contract**: fleetd sees the
  front's watched config dir (rw) and writes atomically into it;
  -watch-config applies. The dry-run path (C2's render_front) verified
  parity before authorship flipped.

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
  additive.
- `commands` carries the piggybacked verbs (drain, resume, unload,
  warm). The cell executes and reflects results in its next announce.
  This retires the C2 requirement that fleetd can reach a cell's
  `:9001` — after C3, `daemon_url` is an optimization (lower latency
  for interactive drains), not a requirement. **Commissioning a new
  cell = install daemon/announcer + `hosts.yaml` entry + registry URL.**

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
  `/api/fleet/events` (the existing SSE stream) — `cell_stale`,
  `cell_withdrawn`, `cell_returned`, `model_degraded` (reserved).
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

## Out of scope

The v2 `probe` throughput block (reserved only), warm targets/page
(C4), any data-path tunneling (absent cells are answered by the front,
never by fleetd — invariant 1).
