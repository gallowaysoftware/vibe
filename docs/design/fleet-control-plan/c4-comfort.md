# C4 — Comfort: warm targets, warm schedules, the fleet page

Status: MERGED (2026-08-03, PR #22, squashed as `28a8073`), WITH
FOLLOW-UPS. The three live gates
below are real runs and stand. Gate 4 as originally written did not:
post-gate review found a data race, a warm policy that reached the
design-rejected mid-session eviction four ways, and a cron evaluator
that ANDed dom/dow where Vixie ORs. All of it is fixed in
[C5](c5-land-c4.md), whose commits are on this branch.

> **Correction (2026-08-02, post-implementation audit; C5 landed
> 2026-08-03).** This phase is the only one of C0–C4 whose adversarial
> self-review never ran — the implementing agent's budget ran out
> mid-review. A 9-agent verification pass afterwards confirmed 64
> defects across C0–C4 — 2 blockers, 19 majors, 43 minors and nits —
> concentrated in this phase's warm policy. Against the claims
> originally on this page:
>
> - **"Unit tests: PASS" (gate 4) was false.** The suite passed at
>   `-count=1`; `-race -count=10` reproduced a data race in
>   `c4_test.go` in 30–50% of runs (measured 6/20 isolated, 6/12
>   full-package). PR #22's green CI check was a coin flip, not a merge
>   signal. **This is the lesson the gate wording now carries: a gate
>   claim is a claim about a repeated mechanical run.**
> - **"absent/drained skips" (gate 4) was never implemented.**
>   `evalWarmTarget` checked only `Stale`/`Withdrawn`; the drained
>   branch did not exist, and
>   `TestWarmTarget_SkipsAbsentAndDrainedCells` set only `Stale`
>   despite its name — the test's NAME carried the claim its body never
>   proved, and that false claim then propagated into three other
>   documents. The test is now `TestWarmTarget_SkipsStaleCells`, and
>   the drained case is `TestWarmTarget_SkipsDrainedCell`.
> - **The warm policy reached the design-rejected mid-session eviction
>   four separate ways** (drained cells warmed; unknown activity read as
>   a fabricated hour of idleness; a long request stamped only at its
>   start, so it read as idle and was evicted mid-generation; and any
>   `restore_after_idle` above 1h silently inert). §1 below says *"do
>   not build that, even as an option"* — the code built it by accident.
> - **Cron ANDed dom/dow where Vixie ORs**, so `0 9 1 * 1` next-fired
>   2027-02-01 instead of tomorrow, and the one interesting rule in cron
>   semantics had no test — which is how it passed a gate reported PASS.
> - **§3 described EventSource**; the implementation is a fetch-streamed
>   reader because EventSource cannot carry the bearer header. The code
>   was right, this page was wrong (fixed below).

Gate results (live, reference fleet):

1. **Warm-target gate: PASS.** On a real cell (two ~5 GB chat models —
   small footprint while the heavy cell's GPU hosted a game): swap loaded through
   the front, requests marked activity, then quiet — the restore fired
   at the 1m idle window (`last_restore` + target resident, state
   `holding / target resident`). The gate first exposed a live race
   (twice): residency is heartbeat-stale, so a swap mid-cold-start
   reads as "nothing resident" and the loop restored EARLY (both
   models warm). The empty-restore now requires the emptiness to
   persist for a **time-based grace (≥ one announce interval, default
   30s)**; regression tests cover grace-hold, grace-elapse-fires, and
   swap-appearing-mid-grace-resets.
2. **Schedule gate: PASS.** A `21 17`-style entry fired at :46:01
   (within seconds of its cron time, `last_note: "warmed"`), the model
   was resident before any organic request, and `next_fire` re-parked
   to tomorrow. The gate also caught the exact footgun the doc warns
   about: fleetd's alpine image had no tzdata, so `time.Local` was UTC
   and `next_fire` printed tomorrow instead of today — **visible at a
   glance in fleet_status precisely as designed**. tzdata is now in
   the reference Dockerfile; next_fire resolves in the declared TZ.
3. **Page gate: PASS.** Real browser: token prompt → table (4 cells,
   derived displays, per-row actions, status line). CLI drain flipped
   the gpu cell's row to DRAINED via SSE without reload; pressing Resume
   on the page round-tripped through `/mcp` ("Resume requested via
   announce") and the row flipped to SERVING 6s after the click
   (desired-serving → cell resume → echo → SSE). JIT round-trip
   verified after ("OK" through the front). Buttons POST `/mcp` only —
   the sole new route is static `GET /ui/fleet`. Two fixes landed en
   route: the static page needed a bearer exemption (a browser can't
   401-and-then-prompt; everything else stays gated, test-pinned), and
   the SSE stream now drives debounced state refreshes (it originally
   fed only the fingerprint-warnings panel — a 30s poll would have
   been the update path, failing the gate's intent).
4. **Unit tests: PASS at `-count=1` only — FAILED the real gate, fixed
   in C5.** As shipped: idle-window state machine (request resets,
   stale skips, nothing-resident restore, empty-grace timing), cron
   next-fire (leap year, DST spring-gap skip in America/Halifax,
   exact-minute boundaries), schedule guard (busy in-flight → skip,
   active lease → skip — the first mechanical lease consumer, clear →
   fire + re-park), page route served only under the fleet role. What
   the gate missed: `-race -count=10` reproduced a data race in the
   warm-target test; the "absent/drained skips" case only ever
   exercised *stale*; the idle-window INPUT path (`trackInFlight`) had
   zero coverage, so replacing it with a no-op left the repo green; and
   the cron table had no both-restricted dom/dow case. Post-C5 the gate
   is `-race -count=20 -run TestWarmTarget` plus `-race -count=5 ./...`,
   and every item above is covered.

Also landed this phase: a **model-set-change render trigger** (C3 doc
promised it, the implementation lacked it — a cell that starts or
stops serving a model now re-renders exactly like a membership
transition; verified live as def edits propagated to the catalog on
the next heartbeat).

## Goal

The heavy cell restores its wide-utility default model without fighting
the operator's swaps; models can be pre-warmed on a schedule; and the
fleet gets its single pane of glass — one static page over the
substrate built in C1–C3.

## Design

### 1. Warm targets (restore-after-idle)

fleetd config:

```yaml
warm_targets:
  - cell: heavy
    model: dsv4flash
    restore_after_idle: 30m
```

Semantics — **restore keys on the swapped-in model going request-idle,
never on a clock**:

- If the target model is resident: do nothing.
- If another model is resident (the operator swapped): watch that
  cell's activity (in-flight counts / last-request timestamps from
  presence + the cell's llama-swap metrics). Only when the resident
  model has served **no requests for `restore_after_idle`** does
  fleetd issue a warm command for the target (piggybacked or a 1-token
  request), which evicts the idle swap via the cell's normal matrix.
- Any request to the swapped-in model resets the idle window. The
  design-panel-rejected alternative (pin/keep-warm on a timer) re-warms
  the default *while the operator's chosen model is in use* and evicts
  it mid-session — do not build that, even as an option.
- The cell's own TTL still applies underneath; warm-target is fleetd
  policy layered on top, and it must tolerate the cell being drained
  or absent (skip silently; note in status).
- *Added C11.* The one case this rule is right and its conclusion is
  wrong — the challenger you walked away from at lunch — is answered by
  a declaration, not by better observation:
  [`hold_model`](c11-hold-model.md) suspends this restore on one cell
  until an expiry. It is a lease with `hold: true`, and it is the ONLY
  thing that suppresses the restore besides drain/stale/absent.

**Activity evidence (decided in C5, was left implicit).** "Request-idle"
needs a source of truth for last-request time. Today that is
`modelActivity`, fed only by the cell's own `/api/events` inflight
frames — both edges (a model appearing in the frame's request list AND
disappearing from it, so a long generation's completion restarts the
window rather than its start). When a model has **no** entry, fleetd
measures idleness from **its own process start**, never from a fabricated
floor: fleetd must not claim silence it was not running to observe. The
status detail names the missing evidence so the operator can see the
policy is running on weak data.

**…and only where fleetd is watching** (amended by the adversarial pass).
The from-start floor is honest evidence *only* when fleetd holds an
observation channel to the cell. Where it does not — an announce-only
cell, the no-inbound-port case C3 exists for — "no requests seen" is
absence of observation, and measuring from process start makes fleetd's
own **uptime** the clock this section forbids: once uptime passes
`restore_after_idle`, every resident swap reads as fully idle and is
evicted on the first tick. `observesActivity(cell)` is the
discriminator: an inflight frame ever received, or the cell's
`/api/events` stream open right now. Without one the target is
`skipped (no activity evidence …)` in `fleet_status` — visible, and in
the safe direction. A watched-but-quiet cell keeps working exactly as
before, so this does **not** depend on whether llama-swap emits an
inflight frame on connect.

The stronger options were considered and deferred:

- *Require `inFlightSeen[cell]` before restoring at all.* Correct only
  if llama-swap emits an inflight frame **on connect** and not solely on
  add/remove — unverified, and if it is add/remove-only a genuinely
  quiet cell would never restore. The `observesActivity` rule above is
  the half of this that needs no such verification (the stream being
  open is itself the evidence fleetd is looking); the strict form still
  needs a live check.
- *Announce the truth* — extend `AnnounceModel` with `last_request_at` /
  `in_flight` (additive and v1-safe; the reserved `Probe` field sits
  right there), populate them cell-side from llama-swap, and prefer the
  announced value. This is the durable answer for announce-only cells
  (the no-inbound-port case C3 exists for), where fleetd has no inflight
  stream at all — today those cells' warm targets simply skip, which is
  safe but is the feature not working. Deferred to a later phase, not
  because it is wrong but because it changes the wire protocol.

### 2. Warm schedules

```yaml
warm_schedule:
  - cron: "30 6 * * *"
    model: dsv4flash
```

A minimal cron evaluator in fleetd (stdlib time math over the five
standard fields is acceptable; a well-known tiny cron parser dep is
not — stdlib first, and the need is minute-granularity at best) firing
`warm_model`. This moves scheduled warming out of loose host crontabs
into fleet config, where `--check`/git see it. Two rules:

- **Scheduled warms reuse the warm-target guard.** A warm through the
  front makes llama-swap's matrix *evict* — an unconditional 06:30
  warm landing mid-overnight-batch on the heavy cell starts exactly
  the eviction fight §1 exists to prevent. Skip (and note in status)
  when any model on the target cell is non-idle or holds an active
  lease. This is also the first mechanical consumer the lease store
  gets.
  - **A guard that cannot be EVALUATED is a skip too** (C5). Resolving
    the model's cell goes through `router.LoadDefs`, which fails on an
    unreadable dir *or any one malformed YAML in it*; collapsing that
    into "no cell" silently converted every scheduled warm into an
    unguarded one. Resolve failure skips. An in-flight count that has
    never been REPORTED skips as well — unknown is not zero.
    Resolved-but-unassigned (a front-only alias) still fires, labelled
    `warmed (unguarded: no def/cell)`.
  - **Live check owed on that last rule.** `inFlightSeen` turns true the
    first time llama-swap sends an `inflight` frame for the cell. If
    llama-swap emits one on SSE connect, the skip window is empty in
    practice. If it emits only on add/remove edges, a fleetd restarted
    overnight would skip the 06:30 warm on a cell that served nothing
    since — visible in `fleet_status` as `skipped (cell X in-flight
    unknown)`, not silent, but wrong. Verify against a real cell before
    trusting scheduled warms after a fleetd restart.
- **Timezone is declared, not inherited.** fleetd runs in a container
  that defaults to UTC; `"30 6 * * *"` evaluated in UTC warms at
  ~23:30 local. Set `TZ` in the `deploy/fleetd` `.env` contract, and
  have `fleet_status` print each schedule's resolved next-fire
  timestamp so a wrong zone is visible at a glance rather than at a
  cold 6:30 a.m.

### 3. The fleet page

One static HTML file, embedded via `embed.FS` (house convention),
served by fleetd at `GET /ui/fleet` (path chosen to avoid any
collision with llama-swap's `/ui` on cells):

- Renders the derived-state table live: a **fetch-streamed reader** on
  `/api/fleet/events` (not `EventSource` — it cannot carry the bearer
  header, and the token is the whole auth story here), initial fill from
  `/api/fleet/state`. Per cell: display state (SERVING / DRAINED +
  reason/eta / OFF/AWAY + last-seen / …), class badge, resident models
  with states, leases, fingerprint warnings.
- Thin action buttons — drain / resume / wake / unload / warm — POSTing
  `tools/call` to `/mcp`, the same facade the MCP clients use. No new
  mutation surface: if a button needs a tool the MCP facade doesn't
  have, the facade is incomplete, fix that first.
- Deep links to each cell's llama-swap `/ui` (the model-level detail
  view stays there deliberately).
- Auth: the page and its API calls sit behind the daemon's bearer
  auth; the page prompts once and keeps the token in localStorage.
  LAN posture applies; external exposure only behind the house
  reverse-proxy auth (same rule as everything else on `:9001`).
- No framework, no build step, no external assets (it must load on a
  LAN with no internet). Target: one file, a few hundred lines.

## Acceptance gates

1. **Warm-target gate:** on a test cell, warm the non-default model,
   drive requests to it past the idle window's start (window resets),
   stop; after `restore_after_idle` of true idleness the default is
   resident again. Assert the default was *never* warmed while the
   swap was active.
2. **Schedule gate:** a `warm_schedule` entry fires within a minute of
   its cron time and the model is resident before the first organic
   request (verify via events history).
3. **Page gate:** with the page open, drain a cell from the CLI — the
   row updates via SSE without reload; press Resume on the page — the
   cell returns; all buttons round-trip through existing endpoints
   (verify no new server routes beyond static serving).
4. Unit tests: idle-window state machine (reset on request, skip on
   absent/drained cell), cron next-fire math (DST boundary included).

## Out of scope

Metrics dashboards (llama-swap's `/ui` + Prometheus endpoints already
exist), multi-user auth, anything mobile-app-shaped, the v2 throughput
probe (still reserved).
