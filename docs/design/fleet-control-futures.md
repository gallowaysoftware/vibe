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
2. **`vibe fleet notify`** — SSE-events-to-webhook bridge (ntfy or
   similar), default policy = the class table's alarm column only
   (always-on staleness, persistent fingerprint mismatch,
   drain-with-active-lease, await-unblocked). Without it the design's
   "alarm? yes" column terminates in an SSE stream nobody watches.
   Gate on an away/home fleet-scope intent so vacation isn't noisy.
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
4. **`hold_model(cell, model, for)`** — suspend the warm-target
   restore for an evaluation afternoon; without it, restore-after-idle
   dutifully evicts the challenger you stepped away from.
5. **Drain where reclaim happens** — a documented Steam launch-option
   / desktop-shortcut wrapper for `vibe cell drain --until-exit --`,
   plus an `ExecStopPost` hook on cell units that best-effort records
   out-of-band stops as intent. One line of packaging that decides
   whether the intent axis stays trustworthy.
6. **Guest read-only token** — a second bearer honored only on
   `GET /api/fleet/state` + `/events` (interim: reverse-proxy path
   allowlist, zero code). Sharing status today means sharing drain.
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

7. **`vibe fleet doctor`** — both-direction token auth per cell,
   def-SHA parity, llama-swap version matrix, cert `notAfter` on the
   proxy + registry, disk free on the front, WoL-armed assertion,
   roaming-cell agent-loaded assertion. The sit-down-after-two-weeks
   command. Pair with a quarterly 15-minute fire drill (kill fleetd,
   reboot the front, run doctor, one WoL wake).
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
