# C11 — hold_model: the pause button on the warm policy

Status: MERGED (2026-08-04) via PR #30, feature + adversarial-review
commits. Unit gates 1–11 green on a full local inner loop
(`-race -count=5 ./...`, `golangci-lint run` 0 issues, `gofmt -l .`
silent, `go mod tidy` clean). Live gates **L1 and L4 PASSED on
2026-08-05** and **L3 is PARTIAL**, all against the local multi-cell
harness ([`scripts/fleetlab`](../../../scripts/fleetlab/README.md)) —
real cells, real llama-swap residency, a real fleetd restart. L2 is
unrun and needs no hardware. The independent
review pass (ground rule 9) found 6 further items, all fixed and
mutation-verified here — see the addendum at the end.

Backlog item 4 in [fleet-control-futures.md](../fleet-control-futures.md)
§2, one sentence long:

> **`hold_model(cell, model, for)`** — suspend the warm-target restore
> for an evaluation afternoon; without it, restore-after-idle dutifully
> evicts the challenger you stepped away from.

## The friction

C4's warm target is not broken, and this phase does not fix it. The
restore is *correct*: the heavy cell should come back to its
wide-utility default once the operator's swap goes quiet, and it should
key that decision on observed idleness rather than a clock (C4 §1's
whole argument).

The failure is that the operator's *reason* for the silence is
invisible to fleetd. Two afternoons produce identical evidence:

- the swap is finished with — restore the default, correctly;
- the swap is a challenger model under evaluation and the operator went
  to lunch — restore the default, and pay 6–10 minutes of cold start to
  get the challenger back, twice, because the second reload happens
  after the operator swaps it in again and goes to a meeting.

No amount of better observation separates those. The evidence is the
same; only the intent differs. So the answer is not a smarter idle
window — it is a way for the operator to **declare** the thing fleetd
cannot observe, with an expiry, so a forgotten declaration cannot
permanently disable the policy that was right to begin with.

## Design

### 1. A hold is a lease. It is not a new store.

The instruction to seriously evaluate the lease store before inventing
a parallel one survives contact with the requirements. Against what a
hold needs:

| hold needs | the lease store (C2) already has |
|---|---|
| a key of (cell, model, declarer) | `leaseKey(cell, model, holder)`, last-write-wins per key |
| self-expiry, enforced without a sweeper | Go-duration TTL, lazy expiry at read (`activeLeasesLocked`) |
| durability across a fleetd restart | `saveLeases`/`loadLeases`, atomic tmp+rename, corrupt = empty + loud |
| visibility in `fleet_status` | `CellSnapshot.Leases` via `decorate` |
| visibility at drain time | the pre-drain report (`fetchCellLeases`) |
| bounded growth from a runaway producer | prune-on-mutation + `maxLeases` |
| a mechanical consumer that skips on it | C4's warm-schedule guard (`leasesForCellActive`) |
| off-box string hygiene | `clean()` on model/holder/note |

That is the entire requirement list, already built, already
test-pinned, already persisted in the file an operator can read. A
second store would duplicate every row of that table — and ground rule
2 forbids storing one system's state in another precisely to stop the
second copy from being invented.

**So a hold is a lease with one added field: `hold: true`.**

Why a field rather than "any active lease suppresses the restore":

- A lease and a hold are different speech acts. A lease says *"I am
  using this"* — a note for the human about to drain the box. A hold
  says *"fleetd, do not act on your own policy here until T"*. Making
  every lease a policy override would silently repurpose every existing
  holder's note, and would leave no way to say "I'm mid-batch, and the
  warm policy is still correct."
- The flag is what makes the declaration legible in every surface that
  already renders leases, with no new plumbing: `fleet_status`, the
  pre-drain report, the page, `GET /api/fleet/leases`.

Why not the intent store (axis 2), given that a hold *is* declared
intent: axis 2's vocabulary is CELL state (`drained` / `serving`) that
the cell itself **echoes**, and C3's conflict rule resolves registry
requests against those echoes. No cell can echo a hold — a hold is a
statement about fleetd's own behaviour, not about the box. Adding a
third state to that enum would put a value into the conflict rule and
the display table that neither can resolve. The lease store is the
place this fleet already keeps declared, advisory, expiring holds on a
(cell, model); a hold belongs there.

**Reserved holder.** A hold's holder is always the literal `hold`, so
that `release_hold(cell, model)` has a deterministic key to delete and
a re-issue is a refresh rather than a second entry. The endpoint
enforces the pairing in both directions: `hold: true` with any other
holder is rejected, and holder `hold` without `hold: true` is rejected
(otherwise a plain lease could squat on the release handle and get
deleted by someone else's release). Who is holding it goes in `note`,
which every surface already prints.

### 2. What a hold suppresses — and what it must not

A hold suppresses **fleetd's own automatic warm policy on that cell**:

| behaviour | held? | how |
|---|---|---|
| warm-target restore (C4 §1) | **suppressed** | new check in `evalWarmTarget` |
| scheduled warm (C4 §2) | **suppressed** | inherited: the schedule guard already skips on an active lease |
| throughput probe (C8) | **suppressed** | inherited: `probeGuard` already skips on an active lease |
| `vibe cell await --unleased` (C10) | **waits** | inherited: a hold is a lease |
| drain / resume / wake | untouched | a drain is a stronger declaration; the hold shows in the pre-drain report |
| `warm_model`, `unload_model`, `probe_model` (explicit MCP verbs) | untouched | an operator asking is not fleetd guessing |
| request routing, the front render, the catalog | untouched | the control plane changes what the catalog SAYS, never where a request goes |
| llama-swap's TTL | untouched | residency belongs to llama-swap |

**Both halves of the warm path, not just the decision** (review
finding 1). The warm loops check the hold when they DECIDE, but a warm
the front cannot deliver rides C3's piggyback queue and is handed to the
cell on a later announce — at-least-once, so it survives until a higher
seq retires it. A restore queued one tick before the hold was declared
would therefore still land and evict the held model. `drainCommands`
drops queued `warm` verbs for a held cell (`dropHeldWarmsLocked`).
`warm` is the only verb dropped and that is structural: every queued
warm comes from `queueWarm`, i.e. from fleetd's own policy, while
`unload` is an operator's verb and `probe` can be one.

Two of those deserve their reasons written down.

**Schedules: yes, suppressed, and it costs no code.** A scheduled warm
fires at a wall-clock time and lands on the same GPU; llama-swap's
matrix will evict the held model to satisfy it. That is exactly the
eviction fight C4 §2's guard exists to prevent, and the guard is
already "skip on an active lease naming the cell". A hold is an active
lease, so it inherits. Nothing here narrows that guard — a plain lease
keeps skipping schedules, as it has since C4.

**Warm targets: holds only, not every lease.** The asymmetry with
schedules is deliberate and principled. The scheduled warm is
unconditional in *time*, so it takes the broad guard. The warm-target
restore is already evidence-gated — it fires only when the cell is
in-flight-free and every resident has been quiet for the whole window —
so a working batch is protected by the guards C4/C5 already built. What
hold adds is the one case those guards cannot reach: **the evidence is
correct and the conclusion is still wrong.** That is a declaration, and
only a declaration should carry it. Widening it to every lease would
also mean a forgotten 168-hour batch lease silently disables the warm
policy for a week.

**A hold is not a pin.** This is the sentence that has to survive into
the tool description, because an operator will otherwise believe it:
fleetd does not keep the held model resident. Residency belongs to
llama-swap (invariant 2), and llama-swap's own TTL is free to evict a
held model exactly as before. What a hold buys is that **fleetd will
not be the one to evict it**, and that nothing fleetd does will pull
another model onto that GPU behind your back. If the cell's TTL is
shorter than lunch, hold changes nothing about what is resident when
you get back — and the honest fix for that is the cell's TTL, not a
control-plane pin.

### 3. Precedence: drained > held > stale > unreachable > policy

C5 moved the drain check to the front of `evalWarmTarget`'s ladder for
a specific reason (a drained cell announces an empty model list by
design, which the nothing-resident branch reads as "restore"). The hold
check slots in second. Every one of these is a skip, so the ordering
does not change *whether* the loop acts — it decides which reason the
operator is told, and a later agent will otherwise reorder it by
accident:

1. **drained** — the strongest declaration in the fleet, and about the
   whole cell. If the operator has reclaimed the box, "held" is not the
   interesting fact; the drain also predates this phase's guard.
2. **held** — a declaration, and answerable with no evidence at all.
   It is the answer to the question the operator is actually asking
   ("why has my default not come back?").
3. **stale / withdrawn** — no fresh evidence.
4. **unreachable** (probe fallback) — no evidence at all.
5. the policy proper (resident / nothing-resident / idle window).

A drain does **not** clear a hold, and a hold does not survive on its
own terms past its TTL. Clearing one declaration because another
arrived is acting on inferred intent; both expire on their own terms
(a drain when the operator resumes, a hold at `expires_at`).

**A skip is not an observation of emptiness** (carried finding, §7).

### 4. Expiry, bounds, and the operator who forgets

- `for` is a Go duration string, default **4h** (the evaluation
  afternoon the backlog entry names), maximum **24h**.
- The cap is deliberately tighter than the lease store's 168h. A lease
  is a note about work that is genuinely running; a hold **disables a
  policy the operator configured**, and a forgotten one must not
  survive a night's sleep. Re-issuing costs one command, and a re-issue
  is a refresh of the same key rather than a second entry.
- There is no unbounded hold and there must not be one. An operator who
  wants the warm target off permanently should delete the warm target —
  that edit is visible in git, and a hold is not.
- Release before expiry is `DELETE /api/fleet/lease` with the
  three-field key, i.e. exactly how a lease is released.

### 5. Visibility: remaining time, in the place the question is asked

- `fleet_status` → `cells[].leases[]` carries the hold with
  `"hold": true` and its absolute `expires_at`, beside `generated_at`.
  This is the machine-readable form; an agent computes the remainder.
- `fleet_status` → `warm.targets[].state = "skipped"`, with
  `detail: "held: <model>, 1h59m left (<note>)"`. This is the one
  surface where the question "why is my default not back?" is actually
  asked, so the remaining time is rendered there in words.
- `vibe cell status` prints `held: <model>, 1h59m left` in the
  intent column.
- The fleet page renders a held cell's row with a `held: <model> —
  warm policy paused, 1h59m left` line where lease lines already go.
- The pre-drain report already prints leases; a hold shows up there
  with its note, which is exactly the "heads up, someone is evaluating
  on this box" the report exists for.

### 6. Reachability: an MCP tool and a CLI verb, no new route

Both verbs are declarations against the existing lease endpoint. **C11
adds no HTTP route**, no proto change, and no cell-side code — a hold
is a statement about fleetd's behaviour, so no cell needs to know.

- MCP: `hold_model(cell, model, for?, note?)` and
  `release_hold(cell, model)`. Two tools rather than one with a
  `release` mode flag, following `drain_cell`/`resume_cell`.
- CLI: `vibe cell hold <cell> <model> [--for 4h] [--note "..."]` and
  `vibe cell hold <cell> <model> --release`. One command with a
  `--release` flag rather than two verbs, because the CLI pair is
  hold-then-forget: early release is the rare path and the flag keeps
  the argument shape identical.
- The fleet page gets the badge but **no button** (§8).

The front cell is refused by both, with C8's `probeGuard` wording: the
front serves no models of its own, and a hold there protects nothing
while looking like it does.

### 7. Files

- `internal/vibe/fleetapi/hold.go` — new. The hold vocabulary
  (`HoldHolder`, defaults, bounds), validation shared by the HTTP
  endpoint and the in-process verbs, `SetHold`/`ReleaseHold`/`HoldOn`,
  and the status detail.
- `internal/vibe/fleetapi/leases.go` — the store mutation is factored
  into `putLease`/`dropLease` so the endpoint and the in-process hold
  verbs share ONE clone-prune-cap-persist-swap path. Behaviour of the
  endpoint is unchanged (same 400/409/500 mapping).
- `internal/vibe/fleetapi/warmtarget.go` — the hold check, second in
  the ladder; and `setWarmState` now clears the empty-grace window on a
  **skip**. Carried finding: a skip is not an observation of emptiness.
  With a stale `emptySince` surviving a skip, the first tick after the
  hold (or the drain) expired could fire the empty-restore instantly
  against a cell whose model is mid-cold-start — the exact live race
  C4's gate 1 found and the grace window exists to prevent.
- `internal/vibe/fleetmcp/hold.go` — the two tools.
- `internal/vibe/cli/cmd_cell_hold.go` — the CLI verb.
- `internal/vibe/fleetapi/fleet.html` — the held row line.

## Acceptance gates

1. **A hold suppresses the warm-target restore (unit).** With every
   other condition satisfied — a swap resident, in-flight 0, activity
   observed, residents idle well past `restore_after_idle` — an active
   hold on the cell makes the evaluation skip and issue **no** warm.
   Mutation-tested: deleting the hold check makes it fail.
   `TestWarmTarget_ActiveHoldSuppressesTheRestoreAndIssuesNoWarm`.
2. **The hold expires on its own (unit).** The same setup with a hold
   whose `expires_at` is in the past restores on the next tick, with no
   release call anywhere in the test.
   `TestWarmTarget_ExpiredHoldStopsSuppressingWithoutARelease`.
3. **Precedence is drained > held > stale (unit).** A cell both drained
   and held reports `cell drained`; a cell both stale and held reports
   the hold; a held cell that is otherwise fine reports the hold with
   its remaining time.
   `TestWarmTarget_HoldPrecedence_DrainedWinsHeldBeatsStale`.
4. **A skip does not bank emptiness (unit).** A cell observed
   nothing-resident for part of the grace window, then held past the
   full grace, then released, must wait the FULL grace again before the
   empty-restore fires. Mutation-tested against the pre-C11 behaviour.
   `TestWarmTarget_SkipClearsTheEmptyGraceWindow`.
5. **The inherited suppressions are pinned (unit).** A hold skips a
   scheduled warm (C4 §2's guard) and refuses a C8 probe, through the
   REAL `evalScheduleEntry` and `ProbeGuard` paths, so a later refactor
   of `leasesForCellActive` cannot silently drop either.
   `TestHold_SuppressesScheduledWarm`, `TestHold_RefusesProbe`.
6. **A hold is a lease in the one store (unit).** `SetHold` produces
   exactly one entry, visible on `GET /api/fleet/leases` with
   `"hold": true`, in `cells[].leases` of the state snapshot, and in
   `leasesForCellActive`; `ReleaseHold` removes it; a re-issue refreshes
   the same key instead of adding a second entry; and it survives a
   store reload (`saveLeases` → `loadLeases`) with `Hold` and
   `ExpiresAt` intact.
   `TestHold_IsOneLeaseInTheLeaseStoreAndSurvivesReload`.
7. **Bounds and validation (unit).** `for` above 24h rejected; zero or
   negative rejected; unparseable rejected; `hold: true` with a holder
   other than `hold` rejected; holder `hold` without `hold: true`
   rejected; an unknown cell rejected; the front cell refused by name;
   note/model control characters rejected by `clean`.
   `TestHold_ValidationRejects*`.
8. **A hold changes nothing but fleetd's own policy (unit).** With an
   active hold: the cell's `Display`, `Reachable` and `Intent` are
   unchanged, its model set in the front render is unchanged, and the
   explicit verbs still act (`unload_model` on the held model itself
   reaches the cell, and the hold survives it). Mutation-tested against
   a deliberate leak into `Display`.
   `TestHold_DoesNotTouchAvailabilityIntentOrTheRender`,
   `TestMCPHoldDoesNotBlockExplicitVerbs`.
9. **The surfaces say the remaining time (unit).** `fleet_status`'s
   warm block names the hold and its remaining time; the page's held
   line renders from `hold`/`expires_at`; `vibe cell status` prints it;
   and the string has ONE implementation (`fleetapi.HoldLeft`).
   `TestHold_StatusSurfacesShowRemainingTime`,
   `TestCellStatusShowsAHoldWithItsRemainingTime`,
   `TestHoldLeft_ReadsInMinutesAndNeverGoesNegative`,
   `TestFleetPage_RendersHeldRows`.
10. **Streaming contract (mechanical).**
    `git diff --stat main..HEAD -- internal/vibe/proxy` is empty for
    the whole phase.
11. **Full inner loop** (ground rule 4): `go build ./...`,
    `go vet ./...`, `gofmt -l .` silent, `go mod tidy` clean,
    `go test -race -count=5 ./...`, `golangci-lint run` — plus ground
    rule 9's adversarial self-review as its own commit.

### Live gates (need real cells — a second *cell*, not a second box; see the results at the end)

L1. **The lunch test.** On the reference fleet: a warm target with a
    short `restore_after_idle`, a challenger warmed through the front,
    then `vibe cell hold <cell> <challenger> --for 30m`. Walk away past
    the window: the default is **not** restored, `fleet_status` shows
    `skipped / held: <model>, Nm left`, and the challenger is still
    resident. After expiry (or `--release`) the restore fires on the
    next tick.
L2. **Hold is not a pin.** With the cell's llama-swap TTL set shorter
    than the hold, confirm the TTL still evicts the held model, that
    fleetd does **not** re-warm anything while the hold stands, and
    that the status still reads `held`. This gate exists to prove the
    documented limitation is the real behaviour, not to prove a feature.
L3. **Inheritance in the field.** With a hold active, a `warm_schedule`
    entry due in the window is skipped with the lease reason, and
    `probe_model` on that cell refuses. Both are the C4/C8 guards, not
    new code — the gate is that a hold reaches them.
L4. **The hold survives a fleetd restart.** Take a 2h hold, restart the
    fleetd container, confirm the hold is still in `fleet_status` with
    the correct remaining time and still suppressing.

## Out of scope (deliberately)

- **Pinning residency.** §2: no TTL manipulation, no llama-swap config
  write, no keep-warm loop. `sleep_schedule`-shaped
  declared-action-deferred-by-observation is the sanctioned form if a
  future phase wants the model kept up; a control-plane pin is not.
- **A hold button on the fleet page.** The page gets the badge only. A
  hold takes a duration, and the page's design is three fat buttons for
  a phone in a hallway; a duration picker is a different design
  question. The MCP tool is the agent path and the CLI is the desk
  path.
- **Fleet-wide or class-wide holds** (`hold everything`, `hold all
  roaming cells`). One cell, one model, one expiry.
- **Inferring a hold** from an operator's manual `warm_model` or from a
  human request landing on a swapped-in model. That is acting on
  inferred intent — the invariant this whole plan is built around. The
  operator declares, or nothing happens.
- **Holding against a DRAIN.** A drain is a stronger declaration and
  proceeds; the hold shows in the pre-drain report, which is what that
  report is for.
- **A hold on the front cell.** Refused by name, C8's reason.
- **Extending the hold vocabulary to notify (C9) or the ledger (C7a).**
  A hold produces no alarm and no accounting row.

Estimated ~450 lines + tests, on the plan's calibration (C0–C4 ran
3.6–4.5× their estimates).

## Execution (2026-08-04)

### What shipped

- **`fleetapi/hold.go`** — `HoldHolder` / `DefaultHoldFor` (4h) /
  `MaxHoldFor` (24h), `validateHoldRequest` (the reserved-holder pairing
  and the tighter bound, shared by the endpoint and the in-process
  verbs), `SetHold` / `ReleaseHold` / `HoldOn`, `checkHoldTarget` (front
  + unknown-cell refusals) and `holdDetail` (the "N left" string).
  `HoldOn` is **cell-scoped** and reports the EARLIEST-expiring hold, so
  the countdown an operator reads is the one that runs out first.
- **`fleetapi/leases.go`** — `Lease.Hold`, and the store mutation
  factored into `putLease` / `dropLease` / `cloneLeases` /
  `commitLeases` / `pruneExpired`, so the HTTP endpoint and the hold
  verbs share one clone-prune-cap-persist-swap path. The endpoint's
  behaviour is unchanged: same 400s, the same 409 (now via a typed
  `errLeaseStoreFull`), the same "prune on every mutation, cap on POST
  only" — C6's cap/delete test passes untouched, which is what says the
  refactor preserved it.
- **`fleetapi/warmtarget.go`** — the hold check, second in the ladder;
  and `setWarmState` clears `emptySince` on `skipped`.
- **`fleetapi/warmsched.go` + `fleetapi/probe.go`** — the existing lease
  guards now NAME a hold when the active lease is one ("held: challenger,
  1h59m left" rather than "1 active leases"). The guard itself is
  untouched; only its reason string improves.
- **`fleetmcp/hold.go`** — `hold_model` and `release_hold`, with the
  not-a-pin sentence in the tool description AND in the success reply.
- **`cli/cmd_cell_hold.go`** — `vibe cell hold <cell> <model>
  [--for|--note|--release]`, reporting the expiry the REGISTRY stored
  rather than this process's clock.
- **`cli/cmd_cell.go` / `cli/cmd_cell_actuate.go`** — the hold in
  `vibe cell status`'s intent column, and `printLeasePrompt` rendering a
  hold as `HELD: <model> until 15:04 (evaluating)` in the pre-drain
  prompt.
- **`fleetapi/fleet.html`** — the held line and `leftUntil()`, which
  counts down from the absolute `expires_at` so a page left open
  overnight does not freeze on a stale string.

### Gates

Unit gates 1–11: **PASS**, run as the full inner loop —
`go build ./...`, `go vet ./...`, `gofmt -l .` (silent), `go mod tidy`
(clean), `golangci-lint run` (0 issues), `go test -race -count=5 ./...`
(all packages ok). Gate 10 verified: the branch's diff against `main`
touches no file under `internal/vibe/proxy`.

Four guards were **mutation-tested** — the production code was broken
and the named test observed to fail:

| mutation | test that failed |
|---|---|
| delete the hold check in `evalWarmTarget` | `TestWarmTarget_ActiveHoldSuppressesTheRestoreAndIssuesNoWarm` (warmed twice), `…HoldPrecedence…/held_outranks_stale` |
| delete `setWarmState`'s `emptySince` clear | `TestWarmTarget_SkipClearsTheEmptyGraceWindow` (restored on the first tick after the hold) |
| leak a hold into `CellSnapshot.Display` | `TestHold_DoesNotTouchAvailabilityIntentOrTheRender` |
| delete the endpoint's `validateHoldRequest` call | `TestHold_ValidationRejectsBadTargetsAndDurations` (3 subtests) |

Live gates, run 2026-08-05 against the local multi-cell harness
([`scripts/fleetlab`](../../../scripts/fleetlab/README.md)) — a real
fleetd with a configured warm target (`alpha/lab-chat`,
`restore_after_idle: 2m`) against a real llama-swap cell:

- **L1 — the lunch test: PASS.** With `lab-chat` resident and owned by
  the warm target, a challenger (`lab-embed-a`) was loaded through the
  cell, evicting it, and then held: `vibe cell hold alpha lab-embed-a
  --for 30m --note "C11 L1 lunch test"`. For the next **4 minutes** —
  twice the restore window, sampled every 20 s — the challenger stayed
  `ready`, the default was **never** restored, and the warm target read
  `{"state":"skipped","detail":"held: lab-embed-a, 30m left (C11 L1
  lunch test)"}` with the remaining time counting down correctly
  (30m → 26m). On `--release` the restore fired on the next evaluation:
  `lab-chat` was `starting` within 10 s and the state became
  `{"state":"holding","detail":"restored (swap idle 2m15s)"}`. The CLI's
  own refusal text is the design's, verbatim: *not a pin: llama-swap's
  own TTL can still unload the model — the hold only stops fleetd
  causing it.*
- **L2 — hold is not a pin: NOT RUN.** Runnable on the harness (set a
  cell's llama-swap TTL below the hold and wait it out); not attempted,
  because it costs a TTL window of wall clock and asserts a limitation
  rather than a feature. No hardware involved.
- **L3 — inheritance: PARTIAL.** The probe half ran and passes: with a
  hold active, `probe_model {cell: alpha, model: lab-chat}` answered
  `No probe issued: alpha is held: lab-chat, 10m left (C11 L3
  inheritance).` and carried the previous measurement forward rather
  than inventing one — the C8 guard reached by a C11 lease, which is
  exactly what the gate is for. The `warm_schedule` half was not run
  (the lab fleetd had no schedule configured at the time).
- **L4 — survives a fleetd restart: PASS.** With a 30 m hold standing,
  fleetd was SIGTERMed and restarted. `GET /api/fleet/leases` returned
  the hold with `expires_at` **identical to the nanosecond**
  (`2026-08-05T12:55:39.9060625Z` before and after), the warm target
  went `waiting/watching` for one tick and then back to `skipped / held:
  lab-embed-a, 26m left` — the correct remaining time — and it kept
  suppressing for a further 2 minutes of sampling with the challenger
  still resident.

**One cosmetic observation, not filed as a bug.** In the 10 s sample
immediately after `--release`, residency already showed `lab-chat
starting` while the warm block still read `skipped / held: …`. The warm
state is written after the restore's HTTP call returns, so during an
in-flight restore the status shows the *previous* evaluation's reason.
It self-corrects on the next tick and never misreports a completed
action.

### Adversarial self-review (ground rule 9)

Six findings against the feature commit, all fixed in the review commit:

1. **`ReleaseHold` on a typo'd cell reported "no active hold".** That
   reads as *already gone* to the operator who mistyped, which is the one
   answer that is definitely wrong. Now C6's fail-fast rule applies:
   unknown cell errors. The front refusal deliberately does NOT apply to
   release — a hold there cannot exist, so releasing one is a harmless
   no-op.
2. **The pre-drain prompt printed "hold holds glm-5: evaluating".** The
   prompt exists to tell an operator about to take the box that somebody
   is mid-evaluation on it; the lease vocabulary buried that. Now
   `HELD: glm-5 until 15:04 (evaluating)`, with "the hold does not block
   you" so nobody reads it as a refusal.
3. **`--release` silently ignored `--for` and `--note`**, leaving an
   operator believing they had shortened a hold. Both are now refused.
4. **A `vibe cell hold` against a non-fleetd daemon returned a bare
   404**, sending the operator to hunt for a typo in the cell name. The
   lease store exists only in the fleetd role, and the error now says so.
5. **The schedule and probe skip reasons said "1 active leases".** True,
   and useless to the operator who declared the hold thirty seconds
   earlier. Both now name the hold and its remaining time.
6. **Doc drift (ground rule 8):** fleet-control.md §5 still claimed
   leases "never block anything". Since C4 that has been false of
   fleetd's own policy (scheduled warms skip leased cells), and C11
   widens it. The paragraph now states the real rule — a lease
   constrains what fleetd INITIATES, never what an operator or a client
   asks for.

### Known and accepted (documented, not fixed)

- **A hold's expiry is wall-clock**, like every other lease: a large NTP
  step lengthens or shortens it. Inherited from C2's store; not worth a
  divergent clock for a 4-hour declaration.
- **Two holds on one cell** (two models under evaluation) are two lease
  entries. Both suppress; the status names the earliest-expiring one and
  moves to the next when it lapses.
- **The model on a hold is a label, not a scope.** A hold on a model the
  cell never announced still suppresses, because the suppression is
  cell-scoped by design (C10's `--idle` rule: the contended resource is
  the GPU). Validating the id against the catalog would refuse a
  legitimately un-announced experimental model.
- **`release_hold`'s "unknown cell" wording says "not in the registry"**
  where the sibling MCP tools say "not in hosts.yaml". Same file, two
  spellings; unified wording is a nit for a later sweep.

## Adversarial-review addendum (2026-08-04, 6 findings, all fixed with regression tests)

An independent pass over the feature + self-review commits (ground rule
9). Every fix below is mutation-verified: the production change was
reverted, the named test was watched to FAIL, then restored. The two
guards the implementing agent claimed were mutation-tested (the hold
check in `evalWarmTarget`, the `emptySince` clear in `setWarmState`)
were re-verified the same way and both hold.

- **A queued warm ignored the hold at DELIVERY (MAJOR)** — the classic
  shape here: the sending side guards, the receiving side does not. The
  warm loops check the hold when they decide, but a warm the front
  cannot deliver goes onto C3's piggyback queue, and that queue is
  at-least-once — the batch is handed out again on every announce until
  a HIGHER seq retires it. A restore queued one tick before the operator
  declared the hold (the announce-only cell case, where every restore
  queues) would still land on the next heartbeat and evict exactly the
  model the hold was taken to protect, with the status cheerfully
  reading `skipped / held`. `drainCommands` now calls
  `dropHeldWarmsLocked`, which clears queued `warm` verbs — from the
  pending queue AND from the in-flight redelivery slot — while a hold
  stands. Only `warm` is dropped, and that is structural rather than a
  judgement call: every queued warm comes from `queueWarm` (the
  warm-target restore, the warm schedule), i.e. from fleetd's own
  policy, while `unload` is an operator's explicit verb and `probe` can
  be one — "an operator asking is not fleetd guessing". Dropping rather
  than deferring, because the restore is re-decided every tick once the
  hold lifts and a warm delivered hours late is a stale decision.
  `TestHold_DropsQueuedPolicyWarmsAtDeliveryKeepsOperatorVerbs` (three
  cases: the positive control, the pending queue, the in-flight slot).
  The read needed `holdOnLocked` — `drainCommands` already holds `s.mu`,
  and calling the exported `HoldOn` there would have deadlocked.
- **`vibe cell hold --release` claimed a release it had not made
  (MAJOR)** — `DELETE /api/fleet/lease` answers `{"status":"deleted"}`
  whether or not the key existed, so an operator who mistyped the MODEL
  (the cell is validated, the model is not, by design — §"Known and
  accepted") was told "hold released — the warm policy resumes there"
  while the real hold went on suppressing it. The MCP verb got this
  right from the start ("No active hold on …"); `dropLease` already
  returned `existed` and only the wire dropped it. The endpoint now
  reports `{"status":"deleted","existed":<bool>}` (additive; `status`
  unchanged for C2 clients) and the CLI says which happened.
  `TestHold_ReleaseReportsWhetherAHoldWasThere`,
  `TestCellHoldReleaseReportsWhenThereWasNoHold`.
- **The pre-drain report still said "hold holds glm-5" (MAJOR)** — the
  self-review's finding 2 was fixed on ONE of the three surfaces that
  print leases before a drain. `vibe cell drain` prints the fleetd-read
  prompt (fixed) and then the RPC report three lines later (not fixed),
  so a single drain said `HELD: glm-5` and `lease: hold holds glm-5` in
  the same output; fleetmcp's `drain_cell` report said only the latter.
  Neither carries a hold flag — C11 adds no proto field — but the
  RESERVED HOLDER is a deterministic key, which is what reserving it
  buys. Both now render the hold as a hold; the CLI's lease lines also
  gained the `termSafe` its sibling prompt already had.
  `TestDrainReportNamesAHoldAsAHold` (one in `cli`, one in `fleetmcp`,
  the latter against an extracted `leaseLine` so it needs no live cell
  daemon).
- **Gate 8 asserted less than it claimed (MINOR)** — the gate says "and
  `unload_model` still queues", and no test anywhere exercised an
  explicit verb against a held cell. That is the invariant an agent is
  most likely to "helpfully" break, so it now has a test that unloads
  the HELD model itself and checks the hold survives it.
  `TestMCPHoldDoesNotBlockExplicitVerbs`. Gate 9 also named a test that
  does not exist (`TestHold_StatusAndCLISurfacesShowRemainingTime`);
  ground rule 10 cuts both ways, so the gate now names the tests that
  are actually there.
- **`leases.go` still promised leases "never block anything" (MINOR)** —
  the self-review's finding 6 corrected `fleet-control.md` §5 and left
  the same sentence at the top of the file the rule is about, which is
  where the next agent will read it. The package comment now carries the
  amended rule: a lease constrains what fleetd INITIATES, never what an
  operator or a client asks for.
- **The remaining-time string had three implementations (NIT)** —
  `holdDetail` rendered `1h59m59s left`, `vibe cell status` rendered
  `1h30m0s left`, the MCP reply a third way, and the phase doc's
  examples matched none of them. One exported `fleetapi.HoldLeft` now
  serves all three (minute granularity above a minute — a countdown that
  churns every second is a status line nobody can diff), and the doc's
  examples are true again. `TestHoldLeft_ReadsInMinutesAndNeverGoesNegative`.

### Looked at and deliberately left alone

- **The lease store's ownership and lock discipline**: `leaseMu` →
  `s.mu` ordering is consistent everywhere (no path takes `s.mu` first),
  `cloneLeases`/`commitLeases` persist-then-swap so a failed write
  leaves the observable map untouched, and C11 added no goroutine, no
  timer and no context. C6's cap/delete behaviour survives the
  `putLease`/`dropLease` refactor unchanged.
- **A hand-edited lease file can hold an unbounded hold.** `MaxHoldFor`
  is enforced at every write path but not at `loadLeases`. Every writer
  is fleetd itself, so this is operator-edits-own-state-dir territory;
  re-validating on load would also silently rewrite a file the operator
  is holding open. Noted, not fixed.
- **The hold is only as trustworthy as the fleet token** (design §6): any
  cell holding it can declare a hold on any other cell, bounded at 24h.
  Unchanged from C3's threat note; per-cell credentials remain the
  futures item.
