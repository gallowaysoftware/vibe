# Fleet control futures: the year-of-living-with-it view

Status: BACKLOG (2026-08-02). A four-lens futurespective run against
[fleet-control.md](fleet-control.md) and the C0–C4 plan: a simulated
year of daily operation, a 2 a.m. red team, an adoption/product lens,
and an adjacent-capability lens. Nothing here changes C0–C4 scope —
items that had to move into the phase docs already did (state
contract, warm-vs-lease guard, announce schema versioning, flap
hysteresis, cron TZ). This doc is the ranked v2 backlog and the
strategic notes, so the next planning session starts here instead of
re-deriving it.

## 1. What a year of use actually looks like (the findings that reframe)

- **Membership churn is weekly, not rare.** The plan automates fleet
  *state* well and fleet *membership* not at all — but this operator
  tries new model releases constantly, and every def add/edit is five
  manual surfaces plus a killed warm model. A year in, def
  authoring/distribution is where the hours go.
- **The MCP verbs win every desktop interaction within weeks; the C4
  page's real audience is the phone in the hallway.** Build it as a
  token-once, PWA-friendly page with three fat buttons, or it gets
  opened in month one and never again.
- **Deferred pain 2 (throughput rot) is the guaranteed Monday-slow
  incident of the year.** `fleet_status` will answer every question
  except the one asked when something feels slow.
- **Intent hygiene lives or dies on verb reach.** Every reclaim that
  bypasses `vibe cell drain` (Steam icon double-click, muscle-memory
  `systemctl`, a crash) deposits `DRAINED?` residue; fifty betrayals
  later the intent column isn't trusted. The verb must live where
  reclaim happens.
- **The roaming cell that becomes the *best* node breaks the class
  taxonomy's hidden assumption.** When the laptop hosts the fleet's
  best coder model, prune-class means that id 404s on every commute;
  opportunistic means permanent noise. No doc currently decides this.

## 2. Ranked v2 backlog

Small (days):

1. **`probe_model(cell, model)`** — **SHIPPED as
   [C8](fleet-control-plan/c8-probe-model.md) (2026-08-04).** A canned
   realistic batch completion scored against the model's own rolling
   baseline; marks `degraded` in `fleet_status`. Fills the reserved
   `probe` slot; converts friction pain 2 from deferred to answered.
   Two notes for whoever reads this next: the baseline did NOT reuse
   `fleetapi/history.go` (it reuses its *shape* — the verdict has to be
   computable on the cell, whose registry may be down, and the keyspace
   is `(model, flags_sha256, metric)`), and a degraded model is NOT
   withdrawn from the render — see the phase doc §5 for why.
2. **`vibe fleet notify`** — **SHIPPED as
   [C9](fleet-control-plan/c9-fleet-notify.md) (2026-08-04.)** Default
   policy is the class table's alarm column only (always_on absence,
   persistent fingerprint mismatch, drain-with-active-lease), gated by
   an away/home fleet-scope declaration. Three notes for whoever reads
   this next. **It is not the SSE bridge this entry sketched**: a
   persistent fingerprint mismatch publishes exactly ONE event ever
   (renderPass runs on triggers, and a steady wrong hash triggers
   nothing) and drain-with-lease publishes none at all, so the alarm
   column is not expressible as an event forward — C9 evaluates
   conditions against the same snapshot the page renders.
   "**Await-unblocked**" landed in two pieces rather than a watch
   registry: the resolve notification every fired alarm sends when it
   clears, plus `vibe cell await --notify` for a human parked on a cell.
   And the away scope lives in its own file, NOT in `intent.json` —
   every reader of that store treats a key as a cell that announces and
   echoes.
3. **`vibe cell await --model <id> --ready` and `--idle <duration>`**
   — **SHIPPED as [C10](fleet-control-plan/c10-await-extensions.md)
   (2026-08-04).** Cell-up is not model-warm (a 6–10 min gap on the
   heavy cell), and resume-then-immediately-chatting shouldn't fire the
   parked batch. Three notes for whoever reads this next. `--idle` does
   ride C4's fold, but per CELL, not per model: the resource a batch
   contends for is the GPU, and C4's per-model window goes quiet the
   moment llama-swap TTL-unloads the model someone was using thirty
   seconds ago. The hard rule is that **missing evidence is never
   idleness** — on a cell fleetd has no live events stream to, `--idle`
   keeps waiting and says why rather than firing the batch, and the
   window is floored at the moment the watcher CONNECTED. And the lease
   composition landed as two flags, `--unleased` (wait for other
   holders to clear) and `--lease <holder>` (claim on success), which
   keeps leases advisory: the waiter opts in, nothing blocks.
4. **`hold_model(cell, model, for)`** — **SHIPPED as
   [C11](fleet-control-plan/c11-hold-model.md) (2026-08-04).** Suspends
   the warm-target restore for an evaluation afternoon; without it,
   restore-after-idle dutifully evicts the challenger you stepped away
   from. Three notes for whoever reads this next. It is **not a new
   store**: a hold is a LEASE with `hold: true`, because the lease store
   already had the key, the TTL, the file, the pre-drain report and the
   status surface — and two of the three suppressions (scheduled warms,
   C8 probes) then cost no code at all, since those guards already skip
   on an active lease. Warm TARGETS skip on a hold specifically, not on
   any lease, because the restore is already evidence-gated and what a
   hold adds is the one case the evidence cannot reach. And a hold is
   **not a pin** — residency stays llama-swap's, so the cell's own TTL
   can still unload the held model; the hold only guarantees fleetd
   won't be the one to evict it.
5. **Drain where reclaim happens** — a documented Steam launch-option
   / desktop-shortcut wrapper for `vibe cell drain --until-exit --`,
   plus an `ExecStopPost` hook on cell units that best-effort records
   out-of-band stops as intent. One line of packaging that decides
   whether the intent axis stays trustworthy.
6. **Guest read-only token** — **SHIPPED as
   [C12](fleet-control-plan/c12-guest-token.md) (2026-08-04).** A second
   bearer honored only on `GET /api/fleet/state` + `/events`. Three
   notes for whoever reads this next. The interim this entry named (a
   reverse-proxy path allowlist) stays a valid deployment but was not
   what shipped: the allowlist has to stay true across future phases,
   and one in a file this repo cannot test is one that drifts the first
   time a route is added — so the declaration lives in the route table
   itself, `Access` has no safe zero value, and a route added without a
   guest decision fails a test. **`/api/fleet/usage` and
   `/api/fleet/savings` are refused to a guest** despite being read-only
   GETs (item 8's ledger is a behavioural log of the household at day
   resolution; the savings screen adds what the hardware and the power
   cost). And the fleet page renders read-only for a guest, learning
   which credential it holds from a response header on a request it
   already makes — no probe, no new route, and no per-viewer field in
   the one state document every surface renders.
7. **Upstream: llama-swap SIGTERM-time stream grace** (found by C2's
   drain gate, 2026-08-02). The signal handler calls `CloseStreams()`
   before the graceful drain, which cancels in-flight inference
   streams immediately — unit stop is NOT a graceful drain on v239
   (contrast `-watch-config` reloads, which do drain 30s). C2 works
   around it with `drain --wait` (quiescence before stop); the real
   fix is upstream: don't cancel inference streams on SIGTERM, or make
   the grace configurable. Until it lands, keep `--wait` as the
   documented answer and don't assume unit-stop is graceful anywhere
   else in the design.

Medium:

7. **`vibe fleet doctor`** — **SHIPPED as
   [C13](fleet-control-plan/c13-doctor.md) (2026-08-05).** Both-direction
   token auth per cell, def-SHA parity, the llama-swap version matrix,
   cert `notAfter`, disk free, wake configuration, the roaming-cell
   announcer assertion, plus intent / lease / fingerprint / probe / warm
   / ledger / notification hygiene. The quarterly fire drill this entry
   pairs it with stays the other half — see below. Four notes for whoever
   reads this next. **UNKNOWN is a level and it is not OK**: a check that
   could not be evaluated says so, in its own block, with its own exit
   code — the sit-down command is exactly where "I couldn't check" is
   cheapest to score as "fine". **Checks are named for what they PROVE**:
   `wake.configured`, not `wake.armed` — the control plane cannot see a
   NIC's arming and sending a packet to find out is a mutation, which is
   why this entry always paired the command with a drill. **Two of the
   seven named inputs turned out not to exist**: nothing has ever
   populated `versions.llama_swap` (a C3 reservation), so the matrix
   reports UNKNOWN naming the missing producer rather than guessing at an
   endpoint this repo cannot verify; and the slim announcer sent no
   versions or capacity block at all, so the heavy cell — the box most
   likely to drift — was the one cell reporting neither (fixed there).
   And **"disk free on the front" is not fleetd's disk**: fleetd is its
   own container, so the report separates fleetd's state dir, the front's
   render mount (only where `fleet.front_config` declares the shared
   mount) and each cell's announced capacity.
8. **Usage rollups** — fleetd scrapes each cell's `/api/metrics` on
   the probe loop (llama-swap's are in-memory and vanish on restart),
   persists per-model × cell × day counts, exposes
   `/api/fleet/usage` + an MCP tool. Price cloud-peer ids at API
   rates and local ids counterfactually: "the fleet served 41M tokens
   that would have been $310" is the number that makes the homelab
   quotable. Counts only, never bodies. Note: cloud calls that bypass
   the front hole this ledger — routing cloud through the front is
   what makes it complete.
9. **`sleep_schedule`** — warm_schedule's dual for opportunistic
   cells: declared cron suspend, *guarded* (not triggered) by
   in-flight requests and active leases, paired wake entry = WoL +
   warm, status shows "asleep per schedule, wake 07:15". The 5090 box
   idles ~80W × 8h/night for nothing. Declared-action-deferred-by-
   observation is invariant-clean; the rejected direction (observed
   idleness *initiating* action) stays rejected.
10. **Visible-repoint alias tier** — a catalog id (`best-coder`)
    whose resolution to a concrete cell model is *shown* in the
    catalog, re-resolves only on membership transitions, with a loud
    event. This is the named answer to the roaming-best-node problem;
    explicitly not per-request fallback (invariant 3 stands). Decide
    it deliberately in fleet-control.md §9 — adopt or reject with the
    two-id workaround documented — before the good laptop arrives.
11. **`vibe bench replay`** — offline replay of a cell's
    `/api/captures` through a candidate model: tok/s, tool-call rate,
    divergence vs recorded prod responses. Replay in place, emit only
    scores (captures are private traffic; never sync them into a
    bench corpus). Makes "new release dropped" answerable against
    *your* workload. The invariant-violating version — live shadow
    routing at the front — stays dead.
12. **Front failover identity** — a DNS name for :9000 (so a spare
    box can assume it), rendered config + compose + nightly
    fleetd-state tarball mirrored off the front host, and a half-page
    cold-standby runbook (the gpu-cell can run `llama-swap:cpu` with
    the same peers file in ~10 minutes). The front host dying is the
    one total-fleet outage; don't build HA, write down the path.
13. **The upgrade ritual** — digest-pin the front image, keep the
    six-client SSE gate as a checked-in runnable script, and make
    "canary cell → gate → fleet" the only sanctioned llama-swap bump.
    The SSE keepalive defense is upstream *behavior*, not structure;
    it is only as durable as the discipline around upgrades.

Large:

14. **`vibe model try <hf-id> --cell <name>`** — download to the
    library, scaffold a def from a family template, render, apply at
    cell-idle, warm, report tok/s vs the incumbent. Compresses the
    weekly membership-churn loop (the year's dominant toil) into one
    sentence to an agent.

## 3. The adoption story (if this is ever for others)

The r/LocalLLaMA archetype (gaming GPU box + a Mac + something
always-on) is exactly this fleet, and nothing off the shelf ships the
intent axis — "DRAINED: gaming, back 23:00" is the most relatable
screenshot this project can produce, and `vibe cell drain
--until-exit -- <game>` is a feature no one else has. But adoption
currently dies at hour one: the value ships as design docs, the
install story is llama-swap + vibe + units + compose + a private-repo
convention, and the README leads with "a task launcher." What would
tip a stranger from stealing the architecture to running the tool:

- **A two-box quickstart** (`vibe fleet init`: ask for cells, emit
  hosts.yaml + cell configs + front config + unit skeletons + a
  port-publish compose — macvlan demoted to an appendix; it's the
  hour-one footgun).
- **A public reference-fleet twin** — `examples/fleet/` with complete
  configs on `example.lan` values and a CI render-parity test. The
  boundary rule currently keeps the only complete working instance
  where nobody can see it.
- **README restructure + 30-second screencast**: phone picks a cold
  big model from the dropdown, loading-state streams, tokens flow
  (the shipped SSE-keepalive magic that defeats harness stall
  timers), then one drain command flips the catalog honest. Lead
  with that; demote the launcher and the media pipelines to
  sections. Lift the rejected-alternatives table and the
  honest-partial scorecard nearly verbatim into a "why not X"
  section — engineering honesty is the marketing.
- **Named limitation:** the flagship drain-for-gaming machine is,
  for half the audience, a Windows box; document the WSL2 pattern,
  don't promise more.
- Ship order for adopters inverts the operator's: quickstart → C1
  status/intent → the C4 page → C2/C3. Every early phase produces a
  screenshot, and adoption runs on screenshots.

## 4. Explicitly killed (so they stay killed)

- Live shadow routing at the front (data-plane hop; invariant 1) —
  offline capture replay is the sanctioned form.
- Request-triggered WoL / inline wol-proxy adoption (new data-plane
  hop + contradicts "wake is always explicit").
- A public read-only status page (intent leaks presence-of-a-person;
  there is no consumer the C4-page-behind-VPN doesn't serve).
- Router-enforced silent cloud spillover (the reserved `on_cold`
  namespace is the only future home; agent-side declared policy +
  provider spend caps + a `cloud_budget_status` tool cover the real
  need the day C1 ships).
