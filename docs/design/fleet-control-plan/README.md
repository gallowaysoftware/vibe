# Fleet-control implementation plan (C0–C27)

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
| [C7a](c7a-usage-ledger.md) | The usage ledger: tokens per cell, per model, per day | ~710 lines | C4 | merged (#24); unit gates green; live halves of 2, 3, 5, 6, 8 **PASS**, 1 and 4 PARTIAL (harness) |
| [C7b](c7b-savings-screen.md) | The savings screen: what the fleet didn't spend | ~690 lines + ~100 KB data | C7a, C5 | merged (#25); unit gates green; live plausibility gate still UNRUN (needs a real week of priced traffic) |
| [C8](c8-probe-model.md) | probe_model: throughput health against the model's own baseline | ~900 lines | C3, C4 | merged (#27); unit gates 1-10 green; **L1-L4 PASS** and L5's baseline half PASS (harness); **L5's flag-change half FAIL** — see C17 finding 1 |
| [C9](c9-fleet-notify.md) | `vibe fleet notify`: the alarm column, delivered | ~1100 lines | C2, C3, C4 | merged (#28); unit gates 1-13 green; **14b-14d PASS** + 3 bonus gates (harness); 14a PASS bar the phone |
| [C10](c10-await-extensions.md) | await extensions: `--model --ready`, `--idle`, the lease handshake | ~450 lines | C1, C2, C3, C4, C6, C9, C11 | merged (#29); unit gates 1-12 green; **13b, 13d PASS**, 13a PARTIAL, 13c **VOID** |
| [C11](c11-hold-model.md) | hold_model: the pause button on the warm policy | ~450 lines | C2, C4, C5 | merged (#30); unit gates 1-11 green; **L1-L4 PASS** (harness) |
| [C12](c12-guest-token.md) | Guest read-only token: sharing status without sharing drain | ~250 lines | C1, C5 | merged (#31); feature + self-review + adversarial-review commits; unit gates 1-14 (+11b) green; **L1-L3 PASS** (L1's DOM half via headless Firefox) |
| [C13](c13-doctor.md) | `vibe fleet doctor`: the sit-down-after-two-weeks audit | ~1500 lines | C1-C12 (composition) | merged (#32); unit gates U1-U16 green; **L1-L3 + defs-parity PASS** (harness), L4 PARTIAL (WoL needs metal) |
| [C14](c14-sleep-schedule.md) | `sleep_schedule`: the declared night, deferred by observation | ~1100 lines | C2, C3, C4, C11 | merged (#33); feature + self-review + adversarial-review commits (4 + 7 findings); unit gates U1-U18 green; **L3, L4 PASS** (harness); L1, L2, L5, L6 need metal |
| [C15](c15-warm-auth.md) | The warm credential: a llama-swap API key fleetd can present | ~750 lines | C4, C5, C13 | merged (#38); feature + `front_extras` + adversarial-review commits; unit gates U1-U12 green (24 mutation checks); **L1 PASS** (purpose-built two-swap rig with real `apiKeys`) |
| [C16](c16-upgrade-ritual.md) | The upgrade ritual: digest-pin the front, make the bump a sequence | ~700 lines | #37's conformance work | merged (#39); unit gates U1-U23 green; **L1–L4 and L7 PASS** (L4 on 2026-08-08, unblocked by C23), L5-L6 unrun |
| [C17](c17-gate-closure.md) | Gate closure: run the gates that were never attempted | 0 lines (14 gate rigs) | C7a-C14 | merged (#40); 14 gate rows moved; 2 findings; 9 review findings against its own rigs |
| [C18](c18-model-try.md) | `vibe model try`: the churn loop as one command | ~1400 lines | C0, C2, C4, C8, C10, C11, C14 (composition) | merged (#41); feature + self-review + 2nd-pass + independent-review commits (7 + 11 + 9 findings, 4 blockers); unit gates U1-U15 green, 35 predicates mutation-verified; **L1-L4 PASS** (harness, 2026-08-06), L5 needs metal |
| [C19](c19-front-failover.md) | Front failover identity: the state that dies with the front host, and the path back | ~1400 lines | C1-C16 (composition) | merged (#42); unit gates U1-U31 green; **L1, L4 and L7 PASS**. L1's own recovery timings were UNVERIFIED — the committed rig could not produce them — and are now MEASURED by L7 (2026-08-08): **9.6 s and 10.1 s** over two runs of the fixed rig. L2 needs the fleet, L3 is wall clock |
| [C20](c20-invariant-harness.md) | The invariant harness: the recurring defect classes, made mechanical | ~1600 lines | C1-C19 (composition) | merged (#43); unit gates U1-U15 green; **mutation harness 17/17**; no live gates by construction |
| [C21](c21-alias-tier.md) | The visible-repoint alias tier: **rejected**, and the invisible one that already shipped | ~55 lines + 15 tests | C2, C3 | merged (#44); feature + self-review + adversarial-review commits (5 + 9 findings); unit gates U1-U15 green, 11 predicates mutation-verified; **L1 + L2 PASS** (harness, 2026-08-06, build pass — not re-run after the review commit) |
| [C23](c23-fleetlab-port-base.md) | A port base for `scripts/fleetlab`: one lab's teardown cannot reach another's | ~120 lines of shell + a ~400-line Go test package | futures item 15 | merged (#57); unit gates U1-U9 green (isolation + a negative control that removes the isolation); **L1-L3 PASS**, L3 being C16's L4 |
| [C24](c24-drain-where-reclaim-happens.md) | Drain where reclaim happens: the launcher wrapper, and the unit's own stop | ~350 lines of shell + `fleetapi.StopIntentReason` | C2, C3, C14 | merged (#58); unit gates green over the SHIPPED files, +2 registry entries; **the live stop-record gate PASS** (harness, 2026-08-08, C26b — `gate-c24-stop-record.sh`, with a positive control); the unit half and the shutdown case still owed |
| [C25](c25-bench-replay.md) | `vibe model try --replay`: your own traffic as the benchmark, and the refusal that ships first | 1315 non-comment production + 2364 test lines | C8, C18 (composition), C7a (the activity walk) | merged (#59); delivered as a C18 flag rather than a top-level verb; design + independent review (12 findings, one a test that asserted nothing); unit gates U1-U14 green, 18 predicates mutation-verified; **L2 PASS on real v239 and v247**; L1, L3, L4 NOT RUN (L4 needs metal) |
| [C26a](c26a-deferred-fixes.md) | The four deferred fixes: the last `sh -c` site, a check that fired for the wrong boxes, a silent peer-id collision, the missing starter | ~250 lines | #14, U3, C13 | merged (#60); all gates PASS; 7 new registry entries (44/44 at merge; the registry reached **62** with C25 and **66** with C26b) |
| [C27](c27-stopped-display-state.md) | `STOPPED`: the unit's own stop record, given its own display state — the question C24 left open | ~35 production lines + ~400 test lines | C24, C9, C13 | merged (#63); the display-only invariant asserted over **108** combinations of class × availability × intent × reason (6 displays changed, **0** alarm or doctor outcomes); found and fixed the page's `b-off` fall-through; +1 registry entry (**67**); **live gate PASS** (harness — the webhook was handed "always_on cell alpha is STOPPED …" by a fleetd that had seen nothing else) |

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

What is still owed, and the physical fact each row needs. C17 rebuilt
this list by running every gate #34 called runnable; C16, C18, C19, C24
and C25 added their own; C26b re-read every row against its phase doc and
ran the two that turned out to be schedulable rather than physical.
"Needs hardware" is not an answer — each row names the specific fact, and
nothing here is a scheduling problem dressed up as a physical one.

| gate | the physical fact it needs |
|---|---|
| C3 gate 1 — the roaming withdraw | A laptop that physically leaves the LAN. The lab's roaming cell is a process on the same box. |
| C7a 1 — 24 h store soak | 24 h of wall clock, nothing else. The compressed run (1200 rows, nothing pruned, survived a restart) already proved the mechanism, and the harness can run it unattended. |
| C7a 4 — the cancelled-stream branch | A model server that omits `timings` on an aborted stream. This llama-server build reports them anyway, so the branch needs an mlx cell — a second machine of a different architecture. |
| C7b 9 — is the savings number believable | A week of real traffic on cells whose watts are **measured** rather than declared, priced against a real open-weight twin. The lab serves CPU bge embeddings; a synthetic `watts_idle` prices a fiction. |
| C8 L2 — spill-induced degradation | A GPU under real VRAM pressure. #34 substituted a `SIGSTOP` duty-cycle throttle, which proves the scorer, not the cause an operator hits. |
| C8 L4 — 24 h of scheduling | 24 h of wall clock at a 15-minute interval on two cells. The cap BOUNDARY is proven; a day of the scheduler asking is not. |
| C8 L5 — the flag-change half | **Nothing physical. It FAILS on the harness today** — C17 finding 1, a real product defect, not an unrun gate. |
| C9 14a — the phone half | A device with the ntfy app subscribed to the topic. The topic itself demonstrably accepts the payload. |
| C10 13a — cold-start magnitude | A model whose ready transition takes 6–10 minutes: a 30B+ on a GPU with cold page cache. The harness model is ready in ~25 s, so the semantics are proven and the magnitude is not. |
| C13 L4 — the fire drill | A physical box to reboot and a real NIC to receive a magic packet. `wake.configured` is a configuration check precisely because arming is not observable from here. |
| C14 L1 — one real night | A box that actually enters S3 and returns, and a wattmeter on its cord. The schedule half is closed (L3). |
| C14 L2 — the wake | A magic packet on a real NIC reaching a powered-off machine, and firmware that honours it. Loopback has nothing to wake. |
| C14 L5 — the wake that fails | A BIOS switch to disarm WoL, and a night to fail across. |
| C14 L6 — the quarterly drill | A real box to suspend and wake, end to end, from a phone. |
| C16 L5 — `ritual.sh gate` | A time budget (~15 min at `DELAY_S=90`, ~45 at 420) plus two manual clients. |
| C16 L6 — the pin on the real front | The fleet: the pinned image pulled onto the front host, doctor reporting one version across it. |
| C18 L5 — the magnitude gate | A real GPU, a real 20 GB pull, a real cold start on both sides, and a report a human agrees with. This phase's whole output is a magnitude. |
| C19 L2 — the failover drill on metal | A second physical box taking the front's address, over a real LAN, with a real DNS change and cells re-announcing across it. |
| C19 L3 — the nightly mirror | A real off-host destination (NFS/CIFS) over a week of timer runs, watching `mirror.age` move OK → WARN when the timer stops. Wall clock, not hardware. |
| C24 — a real `.service` stopping, and the shutdown case | A scratch box or VM. The phase's safety brief forbids installing, enabling or starting a systemd unit on this box, which runs a production llama-swap and a production vibe daemon. The shutdown case needs a real reboot on top: the network is already going away, which is the case the hook's bound exists for. |
| C24 — Steam substituting `%command%` | A Steam client and a game. The wrapper's argv handling is gated; the substitution is Valve's. |
| C24 — `TimeoutStopSec` vs llama-swap's 30 s stream grace, end to end | A real unit stop with a real in-flight stream. The 30 s figure is C16's measurement, carried. |
| C25 L1 — the n gate | A day of ordinary agentic traffic on the reference fleet, then one activity walk. Wall clock, not hardware. §1a's 125–160 captures is arithmetic until then, and `DefaultMaxSample`/`DefaultRateFloor` are set against that arithmetic. |
| C25 L3 — the leak gate on metal | A willingness to run it against the operator's own real traffic, which is the only way it means anything. U3 is its synthetic twin over the same surfaces. |
| C25 L4 — the magnitude gate | A real GPU, a real candidate, and a tool-call rate difference a human agrees with — the same qualification C18 L5 carries, for the same reason. |

Everything else that is still unrun is a time budget, and each phase doc
says which of the two it is. One row is neither: **C8 L5 fails**. C16's
L4 used to be the second such row and is the worked example for the
distinction this table is about — it never needed metal, it needed
`FLEETLAB_PORT_BASE`, and it PASSED on 2026-08-08 the day C23 shipped
the knob. C19's L7 is the same lesson one step further on: it was
recorded NOT RUN for a scheduling reason, ran unchanged once the knob
existed, and printed the timing block whose absence had left C19's
headline number resting on a rig nobody wrote down.

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

C15 (2026-08-05) closes the defect C5 recorded and could not fix: the
warm path sent no credential, so a front configured with llama-swap's
`apiKeys` failed every warm with a 401. Measuring a real v239 first
showed the note understated it — `/health` is the only exempt route, so
such a fleet also lost every probe, the whole `/api/events` stream and
every idle window built on it, the catalog check, `unload_model` and the
cloud-spend tail; only the announce path survived, because cells dial
out. Two rules it carries forward. **A 401 stops the automated
producers, it does not feed them** — the warm loops are tickers, so an
unguarded credential failure is 5,760 identical 401s a day, and the
suppression is sticky, self-clearing and re-arming rather than a
restart-to-recover flag. And **a credential the control plane erases is
not a credential**: the front's config is a derived artifact fleetd
rewrites on every membership transition, so `fleet.front_extras` (the
operator-owned half of that file) shipped with the key rather than after
it. Its self-review found this plan's most repeated defect inside the
phase written to fix it — an eighth producer, in `cli` rather than
fleetd, where the enforcing AST scan could not see it.

C16 (2026-08-05) is backlog item 13, and it is the first phase whose
subject is the repo's own discipline rather than the fleet's state. Its
one carried rule is that **a defence that lives in upstream behaviour is
only as durable as the pin under it**: the SSE keepalive and SIGTERM's
stream grace are things llama-swap *does*, not things this repo owns,
and a floating tag on the front turned "we verified this" into "we
verified this once". Two corollaries worth keeping. **The declared and
the observed halves are both required** — fleetd cannot see the front's
image and a config value cannot see the running version, so
`front.image_pin` and `versions.llama_swap` are a pair, and either alone
reads as an answer. And **the mid-state is the normal state**: old
recordings are kept rather than replaced and CI replays every one,
because a fleet spends most of an upgrade with two llama-swap versions
in it. The motivating incident is worth remembering by name — a floating
`:cpu` tag had moved the fleet onto v247, whose in-flight wire change
silently disarmed eight busy guards.

C17 (2026-08-05) ships no Go code. It committed fourteen gate rigs and
ran them, closing the "never attempted" gates the eleven phase docs
above had left as prose, and it produced two findings a green suite
never would: a cell's probe specs and announced fingerprints are frozen
at announcer start (C8 L5's flag-change half, which now **fails** rather
than being unrun), and the probe-traffic envelope is off by 3x. Its
adversarial pass then found the joke telling itself — **six of C17's own
evidence lines could not produce evidence**, printing `null`, `0` or
`"none"` under a heading promising a measurement, with three flipped
gate rows citing one of them. That is why G7 ("every evidence line in
every rig can produce evidence") exists as a standing gate, and why the
plan's standing sentence is now **running the rig is not the same as
reading it**.

C18 (2026-08-05) is backlog item 14, the only Large-tier entry, and it
is the second phase after C14 to be built entirely around one sentence:
**a declared action deferred by observation is clean; observed idleness
initiating action is rejected.** Applying a def edit rewrites a cell's
llama-swap config, which `-watch-config` reloads — evicting every
resident model and truncating any generation still running at 30 s — so
the apply had to be deferred, and the deferral reuses C10's
`awaitCell --idle` rather than inventing a second notion of idle. Three
rules it carries forward. **Promotion is deleting one line**: a trial
def is marked `trial: true`, `router.Render` excludes those from the
FRONT render, and nothing in vibe can promote one, because entering the
fleet catalog is a change to a shared git repo with a human on it. **The
incumbent def is the better family template** — a family template
encodes what a model family wants, the incumbent encodes what THIS GPU
and THIS build want, and the second list is what decides whether the
candidate loads, which is why one flag (`--like`) supplies both the
template and the comparison. And **the journal states name what is true
on disk**, which is what makes a killed run resumable and, more
importantly, reversible from a later process. Its honest boundary is
that `--cell` must name the box you are on: every step writes a file
where the model will run, fleetd is read-and-request-only, and
cross-cell `try` is a phase rather than a flag. Its adversarial pass
found the phase's own composition biting back twice: the self-review's
two fixes met each other and turned `--dry-run` into a silent full
rollback of an in-flight trial, and `Measure` made a short-lived second
writer of C8's cell-side probe file, whose whole-file rewrite discards
every baseline sample the cell daemon recorded while a trial runs. Both
are the same shape — a component that was correct alone acquiring a
second caller. The independent pass's two blockers are worth carrying:
`--unleased` cannot skip a C11 HOLD (it only skips its own lease
holder), so C18's deferral stood in front of a hold C18 was itself about
to place and deadlocked on every resume; and `router.Render`'s extras
merge treats a missing extras file as "no extras, no error", so an apply
rendered with the wrong extras path silently DELETED the cell's
`apiKeys:` and `store:` sections and the rollback re-created the loss.
Two of the nine findings — including that one — were invisible until
`gate-c18.sh` was executed, and executing the rig as committed is what
showed that L1 and L4 had been measuring a file the command never wrote.

C19 (2026-08-05) is backlog item 12, and it is the first phase whose
subject is the fleet's own death. Its one carried rule is that **"don't
build HA" is an invariant, not a budget decision** — an automatic front
promotion is the silent rerouting invariant 3 forbids, so the code's
entire contribution to two-boxes-answering is a refusal a human can
override. Two corollaries. **The backup cannot live in the thing it
backs up**: the mirror is a host command on a timer because it must
survive fleetd, and fleetd's only role is reading the receipt. And
**enumerating the state was the work** — the table is bound to
`paths.go` by a test, and producing it corrected a standing assumption:
C8's probe baselines and the C7a cursor are cell-side and survive the
front, while the ledger, the intent store and the rendered front config
do not.

C20 (2026-08-06) is the first phase whose subject is the plan's own
process. Its premise is that ground rule 9 works and does not scale —
39+ real defects, four blockers, all in green code, and **the same
classes every time**. Four rules it carries forward. **Removing a shape
beats detecting it**: `observed.Value[T]`'s zero value is UNKNOWN and
its value is unexported, so class 1 stops being writable on the
in-flight path, and the migration immediately found a live defect three
review passes had read past (`drain --wait` reporting the loss of its
evidence as quiescence). **A mutation table is data, not prose**:
`internal/mutation` runs the `| mutation | red |` tables the addenda
already carry, with an UNPROTECTED verdict when nothing fails and a
STALE verdict when the pattern stops matching, so a refactor cannot
silently retire coverage. **A structural scan needs a floor**:
`MinProducers` and the unused-exemption error are what stop the next
rename turning a guard into decoration, and C15's hand-rolled
`found == 0` was too weak. And **the checks are proven both ways** —
each one has a planted violation observed red and an assertion that
fails when its own target is empty, which is ground rule 10 applied to a
phase whose entire deliverable is tests.

C21 (2026-08-06) is backlog item 10, and it is the plan's first phase
whose deliverable is a **decision**: the visible-repoint alias tier is
REJECTED, with the workaround written down and the revisit conditions
named. Two things it carries forward. **Visibility is not a property of
a mechanism, it is a property of who reads it** — the alias tier's
defence was that the resolution is shown and evented, and every one of
those surfaces is read by the operator, who already knows the laptop
left, while the consumer's only channels are `/v1/models` (which names
the peer, not the model) and a completion response whose `model` field
is endpoint-dependent; making it honest to the consumer means rewriting
responses at the front, which is invariant 1. And **enumerate the
feature's states before arguing about it**: two of the alias tier's
three states are byte-identical to what ships, and the whole delta is
the one state that answers `200 OK` from a model nobody asked for.
Writing the test that pinned the rejection then found that the feature
had **already shipped invisibly** since C3 — alias ownership resolved
over the defs that survived the roaming prune, so a departing owner
handed its alias to a co-claimant on another cell — which is also the
phase's answer to "is a loud event enough": the prune logs a loud line
at exactly the right instant, and this went unnoticed for five phases.

C23 (2026-08-08) is backlog item 15, and it is the second phase whose
subject is the repo's own harness rather than the fleet. Its carried rule
is that **a teardown is a claim about scope, and an unanchored kill
pattern is the cheapest way to make a false one**: `lab.sh down` swept
llama-server children by a digit-range regex every instance shared, so a
second lab's teardown was *entitled* to kill the first's upstreams — and
that does not surface where it happens, it surfaces in another agent's
session as a gate failing for a reason nowhere in their diff. It blocked
C16's L4 for three days. The corollary that generalises past this phase:
when a process cannot be identified by a path you control, identify it by
a **derived** range and make the derivation's disjointness a checked
precondition (bases are multiples of 200, and a base whose window would
cover production is refused before `down` reaches the sweep) — and pair
every isolation assertion with a **negative control that removes the
isolation**, because a sweep that kills nothing reads exactly like a
sweep that is correctly scoped.

C24 (2026-08-08) is futures item 5, and the mechanism it packages has
existed since C2 — what was missing is the line an operator pastes into
Steam, and a record for the reclaims that bypass it. Its finding is that
**a hook that POSTs `{"state":"drained"}` is not a recorder**: fleetd
hands a registry intent back to an announcing cell as `desired_intent`,
and the cell answers a drained one by RUNNING `cell_cmds.drain`, so the
naive hook stops the serving stack of a box that has just come back, on
the first heartbeat, through a path nothing in the hook or the unit file
can see. `fleetapi.StopIntentReason` closes that and four more: a stop
record is never handed back as a command, loses to the cell's own drained
echo, never counts as a pending request, never overwrites an entry that
carries a why, and **never explains an absence** — a crash fires the same
`ExecStopPost` as `systemctl stop`, so an `always_on` cell still alarms.
The record adds the *when*; the *why* is still missing, and every surface
that answers "why is this box down" is exactly as loud as before. Its
second rule is about version skew: **the hook's verbs are STATES on
purpose**, because an unknown state is a 400 on every build of that
endpoint, so a drop-in installed against an older front degrades to doing
nothing rather than to actuating.

C25 (2026-08-08) is futures item 11, and it fills the half C18's own
report says throughput cannot answer — *a faster model that tool-calls
worse is a worse model*. Three things it carries. **The refusal ships
first**: the bytes a replay reads are the most private objects in the
fleet and this repository is public, so `internal/swaptest`'s capture
refusal landed in its own commit before any code that could fetch a
capture existed, as four mechanisms rather than a comment. **Correcting
the backlog entry was most of the design** — `/api/captures` plural does
not exist and never did; it is `GET /api/captures/{id}` out of a
size-bounded in-RAM FIFO, so there is no corpus to sync, the sample can
only live in one process's memory, and the phase shrinks to C18's
`measured` step with a different sampler. And **harvest-before-apply is
enforced by ORDERING, not by comment**, because C18's apply is a
`-watch-config` reload and a reload builds a llama-swap with a fresh
empty buffer: at the moment the candidate first exists, the sample is
gone. Its independent pass found twelve more, one of which it
mutation-proved was covered by nothing — the phase's most important
negative assertion drove at a dead fleetd, so the guard between an
ordinary `vibe model try` and a read of the operator's verbatim prompts
could be deleted with the named test still passing.

C26a (2026-08-08) is not a phase, it is a **deferral ledger being paid**:
four findings from this week's reconciliation backlog, each recorded
rather than fixed because it sat outside the unit that found it. Its
carried rule is that **a finding recorded outside the unit that found it
is a finding with no owner** — all four had been seen, understood and
left in place. The corollary that generalises: when a fix is deferred
because it crosses a unit boundary, the note must name *what the fix
costs*. All four cost under a day, and the one that looked hardest (a
call site behind a test seam every test replaces) produced the most
reusable rule — **moving production onto a shared builder is not done
until something fails when the seam's DEFAULT drifts off it**.

A **post-merge reconciliation PR** (#26, 2026-08-03) closed the three
items no single phase branch could reach, because each needed code from
two branches at once: C6's MIN-G producer finished for
`warmtarget`/`warmsched` (C6 could only wire `unload_model` — the warm
loops were C4 files absent from its branch), C6's NIT-D (a debug
`t.Logf` in a C4 test C6 correctly refused to touch), and this table's
own status truth. It adds no new phase scope.

A **reconciliation pass** (C22, 2026-08-06) is the same idea applied to
a different failure. This file, `AGENTS.md` and
[../fleet-control.md](../fleet-control.md) are the plan's conflict axis:
nearly every phase touches all three, and every merge conflict the plan
has produced landed in one of them. So C15 through C21 were **forbidden
from editing them** and each wrote a "For the reconciliation pass"
section into its own phase doc instead, stating exactly what belonged in
each shared file; C22 applies all seven in one pass. It changes no code,
and its own
gate is ground rule 8: every claim it carried over was checked against
the tree before it was written down, and four phases' drafted text
understated their own gate counts (C15 U11→U12, C16 U10→U23, C19
U17→U31 plus an unmentioned L4 PASS, C20's mutation harness 16→17). The
pattern is worth naming, because it will recur: **a reconciliation
section written mid-phase describes the phase as it stood at the time of
writing, not as it merged.** Read it as a draft, not as a record.

**C26b (2026-08-08)** is the second such pass, over the ten PRs that
merged after C22 — C23, C24, C25 and C26a's phase-doc sections plus items
recorded only in PR bodies (#14, #47-#50, #55-#60). Every applied section
now carries an **APPLIED by** marker at its own heading, because C22 left
none and the only way to tell an outstanding section from a spent one was
to diff the shared files by hand.

Four things it found that bookkeeping alone would not have.

- **Two phases proposed opposite rules under the same word.** C26a asked
  that a doctor check which cannot apply return *not-applicable, never
  pass and never fail*; `AGENTS.md` already said *never add a fifth level
  for "not applicable"*. Both are right, about different commands —
  `vibe doctor`'s checks answer `(result, bool)` and drop out entirely,
  while `vibe fleet doctor` has four levels and says OK-with-a-reason.
  Both are now in the file, each naming the other, because the failure
  mode is a future agent carrying one across.
- **C19's headline number was measured rather than qualified away.** The
  committed drill rig had never been able to print a timing block, so
  L1's 10.1 s / 14.1 s came from a patched copy nobody wrote down; #55
  fixed the rig and could not re-run it. C23's `FLEETLAB_PORT_BASE` is
  what made re-running possible, and the drill then produced **9.6 s and
  10.1 s** over two runs (C19 L7). The unverifiable transcripts keep
  their qualification; §6 now leans on a run anyone can repeat.
- **"It needs fleetlab" was the wrong reason three times, not once.** C25
  found the first — its L2 needed one llama-swap on a private port, not a
  fleet. The sweep found C19's L7 and C24's live gate parked the same
  way, and both are closed here, the second with a committed rig
  (`scripts/fleetlab/gate-c24-stop-record.sh`) so its evidence is
  repeatable rather than pasted. Every other unrun gate in the owed table
  above was re-read against its phase doc and names a physical fact or a
  wall clock. Before writing a live gate off, ask C25's question:
  **does this invariant need a FLEET, or one process?**
- **The guard on ground rule 1 could go red for a reason of its own.**
  `TestProxy_StreamingCompletionIsUnbuffered` failed once in CI on a diff
  touching zero lines of `internal/vibe/proxy`, and was re-run. Two real
  defects in the test, both in teardown: it left a parked upstream
  handler unreleased on every early-exit path (a `t.Fatal` above the
  release cost a measured 10 s of teardown *and* printed a
  ReverseProxy copy error belonging to no assertion — the CI signature,
  manufactured by the test itself), and both of its hops dialled
  `http.DefaultTransport`, which every `httptest.Server.Close` in the
  binary reaches into. Fixed in the test; `proxy.go` is byte-identical.
  The initiating CI event was **not** reproduced and the write-up says
  so. The guard is now registered in `internal/mutation`, because a
  flaky guard on the one invariant standing between a long generation
  and a client timeout is worse than no guard: it teaches the fleet to
  re-run.

**C27 (2026-08-08)** answers the one question C26b deliberately left
open, and its lesson is about *why* a naming question was worth a phase.
The stop record rendered `DRAINED` — a state whose meaning is "somebody
chose this and said why" — and the consumers that needed the difference
got it back by reaching past the display into `IsStopRecord`. **A display
state its own consumers have to work around is the wrong display state.**
Two things it carries forward. The invariant was written as a real test
rather than as a claim: 108 combinations of class × availability ×
intent × reason, each evaluated through `main`'s derivation *and*
today's, with the real alarm and the real doctor run over both (6
displays changed, 0 outcomes) — and the test fails if nothing changed, so
it cannot pass by proving a no-op. And the guard written for the new
state found an older one: `badgeClass` ended in `return "b-off"` with
three states relying on that fall-through, so a state the page had never
heard of rendered as a box that is merely off. **Every fall-through in a
rendering surface is a confident wrong answer waiting for a new enum
value.**

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
   *Amended C27:* nor is `STOPPED`. A display state actuates nothing,
   whatever it is derived from; a cell unit's own stop record is a fact
   about the stack, and the surface that renders it is not the surface
   that decides anything.
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

    *Amended 2026-08-06 (C17, applied by C22).* **"Not attempted" and
    "not possible" are different statuses and must never share a
    heading.** This plan has written the conflated version twice — #34,
    and the eleven phase docs #34 itself corrected — each time turning a
    gate nobody had tried into a gate nobody could try. The owed table
    above is the corrected shape: a row names the specific physical fact
    it lacks, or it says the work is a time budget, or it admits the
    gate FAILS. A gate rig is itself a test, so the naming half of this
    rule binds it too: an evidence line that cannot move is a column
    asserting less than its heading claims (C17's G7).
