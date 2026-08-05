# Fleet-control implementation plan (C0–C14)

Execution plan for [../fleet-control.md](../fleet-control.md). Each
phase is one PR, independently shippable, and pays for itself before
the next starts. Read the design doc first; each phase doc is written
to be implementable on its own after that.

| phase | title | new code | depends on | status |
|---|---|---|---|---|
| [C0](c0-quick-wins.md) | Quick wins: hot reload, autostart, discoverability | ~0 lines | — | merged (#18) |
| [C1](c1-observe-intent.md) | Observe + intent: fleetd, cells registry, `vibe cell status/await`, MCP facade | ~450 lines | C0 optional | merged (#19) |
| [C2](c2-actuate.md) | Actuate: drain/resume RPCs, wake, `render --cell front` | ~450 lines | C1 | merged (#20) |
| [C3](c3-announce.md) | The inversion: announce heartbeats, presence-derived render | ~600 lines | C2 | merged (#21) |
| [C4](c4-comfort.md) | Comfort: warm targets, warm schedules, the fleet page | ~300 lines | C3 (a read-only page could ship after C1; its action buttons need C2, fingerprint badges C3) | merged (#22); its 3 live gates ran, gate 4 superseded by C5 |
| [C5](c5-land-c4.md) | Land C4: the adversarial review pass C4 never got | ~400 lines | C4 | merged (#22); unit gates green; **live gates 4 + 6 PASS** (harness, 2026-08-05) |
| [C6](c6-substrate-repair.md) | Substrate repair: the C1–C3 findings against merged code | ~500 lines | independent of C5 | merged (#23); unit gates green; **live gates 1, 2 + 5's live half PASS** (harness) |
| [C7a](c7a-usage-ledger.md) | The usage ledger: tokens per cell, per model, per day | ~710 lines | C4 | merged (#24); unit gates green; live halves of 3, 6, 8 **PASS**, 1, 2, 4, 5 **PARTIAL** (harness) |
| [C7b](c7b-savings-screen.md) | The savings screen: what the fleet didn't spend | ~690 lines + ~100 KB data | C7a, C5 | merged (#25); unit gates green; live plausibility gate still UNRUN (needs a real week of priced traffic) |
| [C8](c8-probe-model.md) | probe_model: throughput health against the model's own baseline | ~900 lines | C3, C4 | merged (#27); unit gates 1-10 green; **L1-L3 PASS** (harness, CPU models), L4-L5 unrun (wall clock) |
| [C9](c9-fleet-notify.md) | `vibe fleet notify`: the alarm column, delivered | ~1100 lines | C2, C3, C4 | merged (#28); unit gates 1-13 green; **14b, 14c PASS** + 3 bonus gates (harness), 14a PARTIAL, 14d unrun |
| [C10](c10-await-extensions.md) | await extensions: `--model --ready`, `--idle`, the lease handshake | ~450 lines | C1, C2, C3, C4, C6, C9, C11 | merged (#29); unit gates 1-12 green; **13b PASS**, 13a PARTIAL, 13c **VOID**, 13d unrun |
| [C11](c11-hold-model.md) | hold_model: the pause button on the warm policy | ~450 lines | C2, C4, C5 | merged (#30); unit gates 1-11 green; **L1 + L4 PASS**, L3 PARTIAL (harness), L2 unrun |
| [C12](c12-guest-token.md) | Guest read-only token: sharing status without sharing drain | ~250 lines | C1, C5 | PR open; feature + self-review + adversarial-review commits; unit gates 1-14 (+11b) green; **L2 PASS** (52-case sweep ×2 fleets), L1 PARTIAL (needs a browser), L3 unrun |
| [C13](c13-doctor.md) | `vibe fleet doctor`: the sit-down-after-two-weeks audit | ~1500 lines | C1-C12 (composition) | PR open, branched off C12; unit gates U1-U16 green; **L1-L3 PASS** (harness), L4 PARTIAL (WoL needs metal) |
| [C14](c14-sleep-schedule.md) | `sleep_schedule`: the declared night, deferred by observation | ~1100 lines | C2, C3, C4, C11 | PR open, branched off C13; feature + self-review + adversarial-review commits (4 + 7 findings); unit gates U1-U18 green; **6 live gates UNRUN — 4 of them genuinely need metal** |

C10 (await extensions) is the last of the three branches cut from
`c9e8bcf` in parallel; C11 and then C9 landed ahead of it. None of the
three builds on the others, but C9 and C10 both extended
`vibe cell await`, so C10 carries the merge and the one decision git
could not make: `--notify` fires AFTER the `--lease` claim and reports
its outcome. C10's phase doc records how the rest of it landed — the
duplicated 20-line POST helper collapsed onto C9's `postFleet`, and the
textual conflict in `cellAwaitCmd` resolved as a union of both flag
sets (`c10-await-extensions.md`'s second addendum).

**Merged is not live-gated** — and for eleven phases that sentence hid a
mistake. Every C0–C7b PR merged on a green CI run of the mechanical
inner loop, and every phase from C5 on marked its live gates "NOT RUN,
needs the real fleet". On **2026-08-05** those gates were re-examined
and most of them turned out not to need the real fleet at all: they
needed a second **cell**, which is a process, not a machine. A local
harness ([`scripts/fleetlab`](../../../scripts/fleetlab/README.md))
stands four real llama-swap v239 cells, a real fleetd and both announcer
shapes on one box, and moved ~40 gates from asserted to watched —
surfacing five product defects that no unit test had.

Ground rule 10 applies to this table: a status cell is a claim about a
mechanical run. Two qualifications belong on every "PASS (harness)" in
it:

- **CPU models are not GPU models.** Every control-plane *edge* is real
  — ready transitions, inflight frames, idle windows, activity rows,
  TTL evictions — but nothing exercises a 6–10 minute cold start, VRAM
  pressure, or an eviction that costs real money. Where a gate's claim
  is about magnitude rather than mechanism, the magnitude is still owed.
- **One box is not a fleet.** No SSH, no TLS, no WoL, no suspend/resume,
  no laptop that leaves the building, no clock skew between hosts.

What still genuinely needs metal, in full: a real suspend/resume cycle
and a wattmeter (C14 L1); a magic packet on a real NIC plus the BIOS
switch that arms it (C14 L2, L5; C13 L4's wake half); a laptop that
physically leaves the LAN (C3 gate 1 against a real roaming box); a GPU
under real VRAM pressure (C8 L2's spill, C10 13a's cold-start
magnitude); a browser (C12 L1); and wall-clock duration — 24 h for
C8 L4, a week of priced traffic for C7b's plausibility gate. Everything
else that is still unrun is a time budget, and each phase doc says which
of the two it is.

Line counts are order-of-magnitude scoping signals, not budgets. Actual
C0–C4 spend ran 3.6–4.5× the estimate in every phase; price that in.

C5 and C6 were added on 2026-08-02 after an audit of the C0–C4
implementation run. C5 is not new scope — it is the self-review step
C1, C2 and C3 each got and C4 did not (see ground rule 9). C6 is the
same audit's findings against already-merged code, split out so
landing C4 stays reviewable.

C8 (2026-08-04) is the first v2-backlog item to land: `probe_model`,
ranked first in [fleet-control-futures.md](../fleet-control-futures.md)
§2 because friction pain 2 is the one guaranteed incident of the year.
It fills the per-model `probe` slot C3 reserved, and its single hardest
rule is that the measurement must never become an actuator — a probe
runs only against an already-resident model, and a `degraded` verdict
changes nothing but a display.

C9 (2026-08-04) is the backlog's second item: the alarm column finally
has a destination. Its one structural surprise is worth carrying
forward — the futures entry's "SSE-events-to-webhook bridge" is the
wrong shape for the policy it was meant to deliver, because two of the
four default alarms have no event to forward (see the phase doc's
opening section). It ships as a state differ over the same snapshot
every other surface renders.

C10 (2026-08-04) is the backlog's third item, and its one carried
finding is a rule rather than a feature: **missing evidence is never
idleness**. `--idle` had to answer "has this cell been quiet" for a
consumer that acts on the answer by taking the GPU for hours, and the
substrate cannot answer it everywhere — a cell fleetd holds no events
stream to produces no evidence at all. C4/C5 already lost a phase to
the softer version of this (fleetd's own uptime becoming the idle
clock), so await refuses, visibly, instead of guessing. The idle window
is also floored at the moment fleetd's watcher CONNECTED to the cell,
not at process start: silence you were not there for is not silence.
Landing after C9 gave it one decision git could not make: `--notify`
and `--lease` both hang off the end of the same wait, and the push goes
LAST, carrying the claim's outcome — a page that says the wait ended
while the box went to someone else is a page that lied.

C11 (2026-08-04) is backlog item 4, `hold_model`, and its one carried
rule is about where declarations live: **a hold is a lease**. The lease
store already had every property a hold needs — the (cell, model,
holder) key, TTL-at-read expiry, the atomic file, the pre-drain report,
`cells[].leases` — so the phase adds a flag to it rather than a second
store, and two of the three suppressions (scheduled warms, C8 probes)
come for free because their guards already skip on an active lease. The
phase's other job is honesty about what a hold is NOT: residency belongs
to llama-swap, so a hold stops fleetd evicting your challenger and
cannot stop the cell's own TTL.

C7a/C7b were added the same day: a "did my hardware pay for itself"
screen. They are split because C7a (counting) is mechanically
verifiable against llama-swap's own totals, while every way the
number can be *silly* lives in C7b (pricing, equivalence, energy,
payback) and none of it can be judged until real counts exist to look
at. C7a needs no new measurement mechanism — llama-swap already logs
per-request token counts to SQLite.

C12 (2026-08-04) is backlog item 6, and it carries one rule that
outlives it: **the route table is the allowlist**. The guest bearer
grants exactly `GET /api/fleet/state` and `GET /api/fleet/events`, and
the enforcement is a positive lookup keyed on exact (method, path) —
because a denylist silently grants every route added after it, and this
plan added routes in eight of its twelve phases. So the declaration
moved next to the mount: `fleetapi/routes.go` is simultaneously what
`Register` mounts and what each route grants, `Access` has no safe zero
value, and a route added without a decision fails a test rather than
inheriting one. C5's `/ui/fleet` bearer exemption folded into the same
table (unchanged: one entry, GET, exact-match, evaluated before
path-cleaning). The phase's other decision worth carrying: `usage` and
`savings` are refused to a guest even though both are read-only GETs —
state is instantaneous, the ledger is the household's history, and the
savings screen exposes more about the house than cell status does.

C13 (2026-08-05) is backlog item 7, the first Medium-tier item, and it
is almost entirely COMPOSITION: nearly every input already existed
(presence, the announce versions block, defs_sha/defs_dirty,
fingerprints, leases, the ledger, probe verdicts), and the value is in
the diagnosis. Four rules it carries forward. **UNKNOWN is a level, and
it is not OK** — this plan has been bitten by absent evidence reading as
a healthy zero in five phases, and a doctor, whose reward is a screen of
green, is where that mistake is cheapest to make. **A check is named for
what it proves**: `wake.configured` not `wake.armed`, `tls.not_after`
not `tls.valid` — ground rule 10 applied to check names, because an
operator reading a screen of OK must be reading true sentences. **The
report is read-only and that is tested twice**, behaviourally (state
files and queues byte-identical across a run) and structurally (a source
scan for mutating identifiers), because the command's whole value is
being safe to run mid-incident. And **the credential check uses the
resolver the actuation verbs use** (`fleetcfg.CellCredential`, which now
holds both of C6's deliberately-divergent precedences as named values) —
a doctor that resolved credentials its own way would be testing its own
code. Two gaps it surfaced rather than papered over: nothing has ever
populated `versions.llama_swap`, and the slim announcer sent no versions
or capacity block at all (fixed here).

C14 (2026-08-05) is backlog item 9, `sleep_schedule`, and it is the
first phase whose payoff is measured in watts: the opportunistic box
idles ~80 W × 8 h/night for nothing. The entire design is one sentence —
**a declared action, deferred by observation, is clean; observed
idleness INITIATING action is rejected and stays rejected** — and the
test applied to every line of it is that removing a guard could only
ever make the suspend happen at a cron minute already named. Four rules
it carries forward. **Only opportunistic cells sleep**, refused by name
for the other two: always_on absence alarms by design (teaching the
alarm evaluator that some always_on absences are fine is how a class
taxonomy stops meaning anything), and a roaming box cannot receive a
magic packet from another city. **Suspend is an RPC with no piggyback
fallback** — the queue is at-least-once and retires on a HIGHER announce
seq, which resets when a cell reboots, so the one verb whose redelivery
is catastrophic is precisely the one that crosses the boundary the
retirement rule depends on. **A suspend with no working wake is
unwritable**: the wake is a required field on the same entry, and a wake
cron that does not parse disables the suspend half too — a broken wake
must never yield a box that sleeps forever, it yields a box that never
sleeps. And **the sleeping box needs no new state anywhere**: it is
recorded as axis 2's ordinary drained intent with a reserved reason and
the wake time as the ETA, which renders as OFF with "asleep per
sleep_schedule, eta 07:15" through code C1 already shipped — the page
diff for this phase is empty. The one trap worth remembering is that
this only works because `CellSuspend` stamps the CELL's own intent
before it freezes; without that, C3's conflict rule hands the sleep
request back on the first heartbeat after waking and the box runs its
own drain verb at 07:15. Its adversarial pass found the two failure
shapes this plan keeps producing, one of each: a guard that lived in
only two of its three producers (`vibe cell suspend` held none of the
structural refusals, so it could take the front down), and an alarm that
paged about an opportunistic cell being switched off — the same
class-table violation C9 shipped, here on a nightly cadence.

A **post-merge reconciliation PR** (#26, 2026-08-03) closed the three
items no single phase branch could reach, because each needed code from
two branches at once: C6's MIN-G producer finished for
`warmtarget`/`warmsched` (C6 could only wire `unload_model` — the warm
loops were C4 files absent from its branch), C6's NIT-D (a debug
`t.Logf` in a C4 test C6 correctly refused to touch), and this table's
own status truth. It adds no new phase scope.

## Ground rules for the implementing agent

1. **The streaming contract is inviolable.** The data plane (client →
   front llama-swap → cell llama-swap → model) may be *observed* —
   C7 accounts for tokens there — but instrumentation must not change
   the bytes a client receives, the flush timing, or the latency of the
   streaming hot path, and a bug in an accounting path must never fail
   a user's request. SSE keepalive is load-bearing: clients kill
   stalled streams (Claude Code at ~5 min). Anything that changes
   streaming *behaviour* rather than merely observing it — buffering,
   coalescing, rewriting, or blocking on a consumer — stop and flag.

   *Amended 2026-08-02.* This rule originally read "Never touch the
   data plane. No changes to … `internal/vibe/proxy`". The repo owner
   relaxed it so token/cost accounting can live where the tokens
   actually are. What survives is the invariant the blanket ban existed
   to protect. A blanket ban is easier to obey and easier to obey
   *uselessly*; state the invariant instead.
2. **Respect the ownership axes** (design doc §4): availability is
   observed, intent is declared, residency belongs to llama-swap.
   Never store one system's state in another. Never act on inferred
   intent — the `DRAINED?` display state is a question, not a trigger.
3. **Boundary rule.** This repo gets mechanisms with reference-fleet
   example values only. Real addresses, tokens, MAC addresses, plists,
   and compose overrides go to the private fleet repo. If an
   instruction seems to require a house value here, it's wrong.
4. **Inner loop** (AGENTS.md): `go build ./...`, `go vet ./...`,
   `go test -race ./...`, `gofmt -l .`, `go mod tidy`,
   `golangci-lint run` — all green before any push. Proto changes:
   `buf generate`.
5. **Stdlib first.** No new dependencies without written justification
   in the PR. Everything in this plan is achievable with the existing
   set (cobra, yaml.v3, connectrpc, protobuf) + stdlib (`net/http`,
   `crypto/sha256`, `embed`).
6. **Update docs as you land.** Each phase PR updates: the phase doc's
   Status line, **this README's status column**, the design doc's
   roadmap state if scope shifted, and AGENTS.md if a new package or
   invariant appears. Future agents read the docs, not the
   conversation.
7. **Acceptance gates are the definition of done.** Each phase doc
   ends with gates; a phase is not complete until every gate passes
   (or a gate is explicitly waived in the PR description with a
   reason). Automated gates become tests in-repo; manual gates get a
   transcript in the PR description.
8. **When the docs and the code disagree, the code wins — then fix
   the doc.** File-level anchors in these docs were verified on
   2026-08-02 against `main` (post-PR #16); C5/C6's anchors were
   verified the same day against `3854d84`. Re-verify before relying
   on them; drift is expected, silent drift is not.
9. **Adversarial self-review is a separate, funded step.** Implementing
   a phase and adversarially reviewing your own implementation are two
   line items, not one. C1, C2 and C3 each landed as feature +
   `review: adversarial-review fixes` + `review: second-pass minors`,
   the addenda documenting 10, 10 and 11+ findings fixed pre-merge —
   including blockers. C4 landed as one commit and needed a whole extra
   phase ([C5](c5-land-c4.md)) to reach the same bar. **A phase with
   only a feature commit is not done.** Land the review as its own
   commit and write its addendum into the phase doc.
10. **A gate claim is a claim about a mechanical run.** "Unit tests:
    PASS" means the full inner loop, repeated (`-race -count=5` or
    more) — a single green run is not evidence, and CI's green check is
    not either when the failure is a race. A test's *name* is part of
    its assertion: `TestWarmTarget_SkipsAbsentAndDrainedCells` whose
    body only exercised `Stale` let a missing drained-skip pass a gate
    reported PASS, then propagated the same false claim into three
    other documents. Name tests for what the body proves.
