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
5. **Drain where reclaim happens** — **SHIPPED as
   [C24](fleet-control-plan/c24-drain-where-reclaim-happens.md)
   (2026-08-08).** The wrapper, its Steam / `.desktop` / shell forms and
   the unit drop-in all live in `deploy/cell/`, beside the two
   deployments that were already there. Four notes for whoever reads this
   next. **The hook half was one heartbeat from being an actuator**: a
   registry drained entry is handed to an announcing cell as
   `desired_intent`, and `fleetannounce.reconcile` answers that by
   RUNNING `cell_cmds.drain` — so the naive "POST drained on stop" stops
   the serving stack of a box that has just come back, through a path
   nothing in the hook or the unit file can see. What closes it is a
   reserved reason (`fleetapi.StopIntentReason`, C14's pattern) that
   makes the record structurally incapable of producing a command, in
   both directions: the start half retires a stop record and **only** a
   stop record, so it can neither clear a human's declared reclaim nor
   store a serving request. **The hook's wire verbs are STATES**
   (`unit_stopped` / `unit_started`) rather than that reason on
   `drained`/`serving`, because a pre-C24 front does not know the reason
   and would record — and then hand back — an ordinary drain request;
   an unknown state is a 400 on every build, so skew degrades to doing
   nothing. **A recorded stop explains nothing**: a crash
   fires the same `ExecStopPost` as `systemctl stop`, so an `always_on`
   cell still alarms and `vibe fleet doctor` still calls the stop
   undeclared — the record adds the *when*, never the *why*, and every
   surface that answers "why is this box down" is as loud as it was
   before. **`ExecStopPost` alone was not enough**, which is a correction
   to this entry: without the paired `ExecStartPost` the record outlives
   the stop and the box comes back INCONSISTENT — a stale drain, the very
   thing the entry set out to prevent. And a **follow-up this phase did
   not take**: on an announcing cell the vibe daemon keeps heartbeating
   `serving` after the serving stack stops (the echo is `{state, since}`
   and cannot say "the stack under me is down"), so the record renders
   INCONSISTENT rather than DRAINED until the announcer goes stale. The
   fix is a reason or a serving-stack liveness bit on the announce echo —
   an additive wire change, and its own small phase.
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
   drain gate, 2026-08-02) — **RESTATED, see item 14.** The finding as
   written is mis-attributed: measured directly for C16 on both v239 and
   v247, `CloseStreams()` closes the **event** streams (`/api/events`
   drops in ~1 ms) and in-flight *inference* streams keep flowing until a
   hardcoded 30 s deadline force-closes them — the same grace a
   `-watch-config` reload gets, not a contrast with it. C2's ~39 s essay
   stream was longer than that grace, which is why it looked cancelled.
   `drain --wait` (quiescence before stop) remains correct and required,
   and `TimeoutStopSec` must still exceed the grace. The upstream ask
   that survives is item 14's: make the 30 s configurable.

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
8. **Usage rollups** — **SHIPPED as
   [C7a](fleet-control-plan/c7a-usage-ledger.md) (2026-08-03, PR #24) +
   [C7b](fleet-control-plan/c7b-savings-screen.md) (2026-08-03, PR #25)**, with
   one clause inverted, one clause that shipped structurally and was
   ungated for five days, and **the hole this entry named for itself
   still open — not attempted-and-failed, but not closeable from this
   repo**. Checked clause by clause against the code on 2026-08-08
   (C25); five notes for whoever reads this next.

   **The scrape shipped INVERTED, and the inversion is the
   improvement.** This entry asked for a fleetd pull on the probe loop.
   What shipped is a cell-side collector (`internal/vibe/usagemeter`)
   that tails its OWN llama-swap over localhost and piggybacks
   cumulative counters on the announce
   (`internal/vibe/daemon/announce.go:68`,
   `internal/vibe/usagemeter/usagemeter.go:762`), folded by fleetd at
   `internal/vibe/fleetapi/usage.go:253`. Three reasons the pull was
   wrong, all argued in C7a: a pull re-inverts C3, only the cell can
   resolve an alias to the canonical def name a price may key on, and
   the endpoint is `/api/metrics/activity` — one row per request,
   written at request COMPLETION — not the counter `/metrics` this
   entry assumed, so a model swapping out mid-burst loses nothing. The
   parenthetical's real worry is answered twice: `store: {path: …}` on
   every cell (C7a §0), plus the epoch/id-reset rule for the cells
   where it is not set.

   **Persistence, the route and the tool shipped as written.**
   Per-(day, cell, model, basis, epoch) buckets in an append-only JSONL
   ledger (`fleetapi/usage.go:79-95`, `paths/paths.go:111`);
   `GET /api/fleet/usage` (`fleetapi/routes.go:154`); `fleet_usage`
   (`fleetmcp/fleetmcp.go:349`, dispatched at `:565`). C7b added the
   money half as a second pair rather than widening these
   (`fleet_savings`, `fleetmcp.go:365`).

   **Both pricings shipped, and the counterfactual came out narrower
   than this entry imagined.** Cloud-peer ids are priced at the real
   model's real rate off the FRONT's activity log
   (`daemon/cloudspend.go:49-107`) — a bill reconstruction, not a
   counterfactual. Local ids are priced counterfactually
   (`fleetapi/savings.go`), but against **the same open-weight model
   rented from a real host**, not against a frontier model. The
   "$310 that makes the homelab quotable" framing above is precisely
   the trap C7b was written to avoid: the frontier comparable moves the
   answer about seventy-fold, and a screen that can only render triumph
   will. The honest shape of that sentence is a range with the caveat
   attached to the same payload the agent reads.

   **"Counts only, never bodies" shipped structurally and was NOT
   gated.** It holds because `usagemeter.ActivityRow`
   (`usagemeter.go:63-71`) decodes a strict SUBSET of llama-swap's row:
   llama-swap also sends `error_msg` (built from the upstream's
   response body), `metadata` (arbitrary client strings) and
   `has_capture`, the last of which advertises a verbatim
   prompt-and-completion capture retrievable at
   `GET /api/captures/{id}`. `encoding/json`'s ignore-unknown-fields
   default was the entire mechanism, and that default is a property of
   a struct definition that one line can change. Closed on 2026-08-08
   with two named, mutation-verified gates in
   `internal/vibe/usagemeter/bodies_test.go` —
   `TestActivityRow_CannotCarryABody` (the type may hold only the
   counting fields) and
   `TestPoll_TextOnTheWireReachesNeitherTheStateFileNorTheWire` (a real
   poll over a row carrying prompt-shaped text in all three fields
   leaks it to neither the state file nor the announce).

   **The named hole is still open, and it is a routing decision rather
   than missing code.** Cloud calls that go THROUGH the front are now
   counted — that is C7b §6, and it did not exist when this entry was
   written. Cloud calls that bypass the front are invisible to every
   mechanism here, because the control plane's only observation point
   is a llama-swap the request never touches; no instrumentation this
   repo could add would see them, so this is not a gate anybody failed
   to attempt. Completing the ledger means the operator pointing the
   harness at the front, which is a config act on the client box. What
   the code owes in the meantime is to say so where the number is read,
   and it does: the savings caveat names "or bypassing the front is
   missing, so the token totals are a floor"
   (`fleetapi/savings.go:251`), and it rides the MCP payload rather
   than the page precisely so an agent cannot quote the number without
   it.
9. **`sleep_schedule`** — **SHIPPED as
   [C14](fleet-control-plan/c14-sleep-schedule.md) (2026-08-05).**
   Declared cron suspend, guarded (not triggered) by in-flight requests,
   leases, holds, a declared drain and a quiet window; paired wake entry
   = clear-intent + WoL + warm. This entry's framing survived contact
   intact: declared-action-deferred-by-observation is invariant-clean,
   and the practical test that keeps it that way is that removing any
   guard could only ever make the suspend happen at a cron minute
   already named. Four notes for whoever reads this next. **Only
   opportunistic cells sleep** — always_on is refused because its
   absence alarms by design, roaming because a magic packet cannot reach
   another city, and the front structurally. **Suspend gets no piggyback
   fallback**: that queue is at-least-once and retires on a higher
   announce seq, which resets on a cell reboot, so a redelivered suspend
   is a box that puts itself back to sleep the morning after — it is an
   RPC or it is refused. **A sleeping box needed no new state**: it is
   axis 2's ordinary drained intent with a reserved reason and the wake
   time as the ETA, so "asleep per schedule, wake 07:15" renders through
   C1 code and the page diff is empty. And the wake half's failure is
   the phase's one new **alarm** (`wake_failed`, default on) — not an
   observation of absence, which for an opportunistic cell is normal
   forever, but a declared action of the control plane's own that did
   not complete.
10. **Visible-repoint alias tier** — **REJECTED as
    [C21](fleet-control-plan/c21-alias-tier.md) (2026-08-06)**, which is
    what this entry asked for. The proposal: a catalog id (`best-coder`)
    whose resolution to a concrete cell model is *shown* in the catalog,
    re-resolves only on membership transitions, with a loud event. Four
    notes for whoever reads this next. **Enumerate the states first**:
    candidate-present and no-candidate-at-all are byte-identical to a
    declared alias plus §4's class policy (and the entry's own open
    question, 404-or-hold, is one C3 already answered), so the entire
    delta is the third state — a `200 OK` from a model the caller did not
    name. Prune and hold are both fail-LOUD; this would have been the
    first mechanism here that is not. **Visibility is a property of who
    reads it, not of the mechanism**: the event, `fleet_status` and the
    page are all read by the OPERATOR, who already knows the laptop left,
    while the consumer's only channels are `/v1/models` — which attributes
    an id to a **peer**, never to a model — and the completion response,
    whose `model` field is endpoint-dependent (measured on v239: a chat
    response named the concrete model, an embeddings response echoed the
    alias straight back). Making it honest to the consumer means rewriting
    responses at the front, which is invariant 1, so the one mechanism
    that would rescue it is structurally unavailable. **The workaround is
    one line, not two ids**: `router.aliases` + `router.alias_owner: true`
    on the def, repointed by moving the owner line — a commit, a diff, an
    author, a render; membership through git, as §4 says. And **the
    feature had already shipped invisibly since C3** — alias ownership was
    resolved over the defs that SURVIVED the roaming prune, so a departing
    owner handed its alias to a co-claimant on another cell, proven end to
    end against merged `main` on real llama-swap processes. That is also
    the answer to "is a loud event enough": the prune logs a loud line at
    exactly the right instant, and nobody noticed for five phases. C21
    closes it (resolution runs over the DECLARED def set at both render
    layers) and the revisit conditions are named in the phase doc §10.
11. **`vibe bench replay`** — offline replay of a cell's
    `/api/captures` through a candidate model: tok/s, tool-call rate,
    divergence vs recorded prod responses. Replay in place, emit only
    scores (captures are private traffic; never sync them into a
    bench corpus). Makes "new release dropped" answerable against
    *your* workload. The invariant-violating version — live shadow
    routing at the front — stays dead.
12. **Front failover identity** — **SHIPPED as
    [C19](fleet-control-plan/c19-front-failover.md) (2026-08-05).**
    `vibe fleet mirror` (create / verify / restore, stdlib tar+gzip),
    two doctor checks, `docs/runbooks/front-failover.md`, and a fire
    drill that kills a real fleetd and times the recovery
    (`scripts/fleetlab/gate-c19-drill.sh`: 10.1 s to a standby with the
    same token, the same declared intent and a byte-identical ledger).
    Three notes worth carrying. **"Don't build HA" is an invariant, not
    a budget decision**: an automatic promotion is the silent rerouting
    the design forbids, so the mechanism's whole contribution to the
    two-boxes-answering problem is a REFUSAL — `restore` dials the
    fleet's own addresses and stops. **The mirror cannot live in
    fleetd**, because it has to keep running when fleetd is what broke,
    and fleetd cannot see the host paths its own state is mounted from;
    it is a host command on a timer, and the only thing fleetd does is
    read the receipt it leaves. And **enumerating the state was most of
    the work and produced a correction**: C8's probe baselines and the
    C7a cursor are CELL-side and do not die with the front, while the
    ledger, the intent store, the leases and the rendered front config
    do — `TestMirrorCoversEveryFleetStateFile` scans `paths.go` so the
    next phase's state file cannot quietly fall outside the archive.
13. **The upgrade ritual** — **SHIPPED as
    [C16](fleet-control-plan/c16-upgrade-ritual.md) (2026-08-05).**
    Digest-pinned front image as the shipped default,
    `scripts/upgrade/ritual.sh` (preflight → record → canary → gate →
    pin), and two doctor checks. This entry's framing was right and its
    emphasis was slightly off. **The keepalive is not the only upstream
    behaviour the fleet leans on**: SIGTERM's treatment of in-flight
    streams is the other, and it now has a conformance invariant beside
    the keepalive's — measuring it corrected a claim C2 made and three
    documents repeat (see below). **The declared and observed halves are
    both required**: fleetd has no docker socket, so "is the deployment
    pinned" can only be declared (`front.image_pin`), and a declaration
    nobody applied is caught only by observing what each cell actually
    runs (`versions.llama_swap`, which finally has a producer —
    `GET /api/version`, verified against real v239 and v247 binaries).
    And **the mid-state is the normal state**: recordings accumulate
    rather than replace, so a fleet halfway through a roll is a gated
    configuration rather than an untested one.
14. **Make llama-swap's shutdown grace configurable** (upstream;
    supersedes item 7's framing). Measured on v239 and v247 for C16:
    SIGTERM closes the `/api/events` streams within ~1 ms, does **not**
    cancel in-flight inference streams, and force-closes whatever is
    still running at a hardcoded 30 s. Item 7 asked upstream to stop
    cancelling streams on SIGTERM; upstream does not cancel them. The
    real ask is that the 30 s be a flag, so a cell whose generations run
    longer can stop cleanly. `drain --wait` stays the local answer either
    way and `TimeoutStopSec` must still exceed the grace.
15. **A port offset for `scripts/fleetlab`** — **SHIPPED as
    [C23](fleet-control-plan/c23-fleetlab-port-base.md) (2026-08-08).**
    It bound fixed ports (9600-9799, upstreams 5980-6019), so two lab
    instances could not coexist on one box — and `down`'s sweep was
    anchored partly on that shared upstream range, so the second instance
    was entitled to kill the first's processes. This blocked C16's L4
    gate outright; L4 ran and passed the day the knob landed. Three notes
    for whoever reads this next. **The sweep was the whole feature**, not
    the ports: two labs "starting" is a convenience, one lab's `down`
    provably not reaching the other's processes is the property, and the
    llama-server children are the hard half because they carry no lab
    path on their argv. **The collision rule is mechanical** — a base
    must be a multiple of 200, which is what makes two instances' windows
    disjoint, and the script refuses one that is not. And **the guard is
    part of the feature**: a base whose windows would cover `:9000` or
    `:9001` is refused before `down` reaches the sweep at all.

Large:

14. **`vibe model try <hf-id> --cell <name>`** — **SHIPPED as
    [C18](fleet-control-plan/c18-model-try.md) (2026-08-05).** Download,
    scaffold, render, apply at cell-idle, warm, report tok/s vs the
    incumbent, with a journal that makes every step reversible. Three
    corrections to this entry's framing, all found while building it.
    **"A family template" is the wrong template**: it encodes what a
    model FAMILY wants, while the def already serving on the box encodes
    what THIS GPU, THIS llama.cpp build and THIS context budget want —
    and the second list is what decides whether the candidate loads. So
    one flag (`--like <def>`) supplies both the template and the
    comparison. **"Apply at cell-idle" is an invariant question, not a
    scheduling one**: observed idleness INITIATING a config change is
    the rejected direction, so the apply is C14's declared-action-
    deferred-by-observation and the deferral reuses C10's
    `awaitCell --idle` rather than a second notion of idle. And
    **`--cell <name>` ships as "the cell you are on"**: every step
    writes a file on the box that will serve the model, fleetd is
    read-and-request-only, and a verb that makes another box download
    20 GB and rewrite its router config is a phase rather than a flag.
    Two things it deliberately cannot do: promote a trial into the
    fleet catalog (that is a commit to a shared git repo with a human
    on it — the trial def carries `trial: true` and `router.Render`
    excludes those from the FRONT render), and price the comparison
    (C7b's arithmetic needs a window of real traffic a trial does not
    have; the resource half is weights-on-disk and `estimated_vram_gb`).

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
- Automatic alias re-resolution — a catalog id whose target fleetd
  chooses from presence (C21, item 10 above). The DECLARED form stays:
  `router.aliases` + `router.alias_owner`, repointed by a human moving one
  line. An exclusion removes a catalog id; it never re-points one.
- Router-enforced silent cloud spillover (the reserved `on_cold`
  namespace is the only future home; agent-side declared policy +
  provider spend caps + a `cloud_budget_status` tool cover the real
  need the day C1 ships).
