# C14 — sleep_schedule: the declared night, deferred by observation

Status: IN REVIEW (2026-08-05), feature + self-review + second-pass +
**adversarial-review** commits (ground rule 9's separate pass).
Branched off [C13](c13-doctor.md). Unit gates U1–U18 green on a full
local inner loop (`-race -count=5 ./...`, `golangci-lint run` 0 issues,
`gofmt -l .` silent, `go mod tidy` clean). Of the six live gates,
**L3 and L4 PASSED on 2026-08-05** under [C17](c17-gate-closure.md)
against the local multi-cell harness
([`scripts/fleetlab`](../../../scripts/fleetlab/README.md)) — a real
request deferring a declared suspend and then letting it fire, and a real
lease deferring one until `max_defer` abandoned it. The other **four
genuinely need metal**: a box that enters S3, a wattmeter, a magic packet
on a real NIC, a BIOS switch. The
implementer's own review found 4 items (REV-1…REV-4); the separate
adversarial pass found 7 more (REV2-1…REV2-7), including two that would
have taken the fleet down or paged every morning. All eleven are fixed
and mutation-verified — see the two addenda at the end.

Backlog item 9 in [fleet-control-futures.md](../fleet-control-futures.md)
§2, Medium tier:

> **`sleep_schedule`** — warm_schedule's dual for opportunistic cells:
> declared cron suspend, *guarded* (not triggered) by in-flight requests
> and active leases, paired wake entry = WoL + warm, status shows
> "asleep per schedule, wake 07:15". The 5090 box idles ~80W × 8h/night
> for nothing. Declared-action-deferred-by-observation is
> invariant-clean; the rejected direction (observed idleness
> *initiating* action) stays rejected.

## The invariant line, and it is the whole design

**A declared action, deferred by observation, is clean. Observed
idleness INITIATING action is rejected and stays rejected.**

A cron entry declares "suspend gpu-cell at 23:30". In-flight work,
active leases, a C11 hold, a declared drain and recent request activity
**defer** that declared action — they can delay it, and they can cause
it to be abandoned for the night, but nothing about them can ever
*cause* it. Nothing in this phase may infer "the box looks idle,
suspend it", and no code path here reads an observation and reaches an
action that was not already declared in config.

Ground rule 2 and design §8 invariant 2 are what make the distinction
load-bearing rather than stylistic: availability is OBSERVED, intent is
DECLARED, and acting on inferred intent is the failure this plan has
refused since C1. The design doc already rejects the mirror image of
this feature by name — **GPU-idle auto-resume** (§9: "heuristic that
acts on its own") — and the argument against it is identical to the
argument for this: `--until-exit` is fine because a human declared the
boundary; an nvidia-smi poller is not, because the *observation* is the
trigger. `sleep_schedule` is on the `--until-exit` side of that line by
construction: the cron IS the declaration, and every observation in the
phase can only subtract.

The practical test, applied to every line of this phase: **remove all
the guards and the feature still only ever suspends at 23:30.** If
removing a guard could make something happen that otherwise would not,
that guard is a trigger and it does not belong here.

## The friction

The 5090 box (`gpu-cell`, class `opportunistic`) is powered on roughly
always and serving roughly never between 23:30 and 07:15. Declared
idle draw is ~80 W; eight hours a night is ~0.64 kWh/night, ~230
kWh/year, and at the reference electricity price the same order as one
month of the fleet's entire notional savings. C7b already bills that
term against the fleet — **idle watts are a COST on the savings screen**,
and C4's warm targets deliberately increase it. This phase is the first
one that can make the number smaller instead of merely honest.

The existing answer is a host crontab entry running `systemctl suspend`,
which is exactly the thing C4 §2 moved warm schedules *out* of: it is
invisible to `--check`, invisible to git, invisible to `fleet_status`,
and — the part that matters — **unguarded**. A bare cron suspend at
23:30 takes the box down mid-generation, mid-batch, mid-evaluation and
mid-`vibe cell hold`, and leaves no trace saying why the fleet lost a
cell. The fleet control plane is the only thing in the house that knows
about in-flight requests, leases, holds and declared drains, which makes
it the only place the suspend can be guarded at all.

## Design

### 1. Scope: opportunistic cells only, and that is not a default

`sleep_schedule` entries are accepted only for cells whose declared
class is `opportunistic`. The other two classes are refused at wiring
time with a named reason, not silently ignored.

**`always_on` is refused because the class table says so.** Design §4
gives `always_on` the alarm column: absence means something is wrong and
it pages. A scheduled suspend of an always_on cell is a configuration
that pages the operator every night by design, and the only way to make
it not page would be to teach the alarm evaluator that some absences of
an always_on cell are fine — which is precisely how a class taxonomy
stops meaning anything. If a cell should sleep, it is not always_on; the
fix is one word in `hosts.yaml`, and it is the operator's word to
change.

**`roaming` is refused because the wake cannot work.** A laptop that
left the building is already absent, and the paired wake is a magic
packet on the house LAN: waking a box that is in a bag in another city
is not a thing this control plane can do, and a schedule whose wake half
is structurally unable to fire is a schedule that puts a box to sleep
forever. The one honest reading of "the laptop should sleep at night" is
that the laptop's own OS power management owns it — a decision that
belongs to the box, not to a fleet registry that cannot see it.

**The front is refused structurally**, by name, before class is even
consulted (`fleetcfg.FrontCell`): the front is the data plane and the
control plane. Suspending it is a total fleet outage, and design §11
already names the front host dying as the one outage nobody has a
runbook for.

### 2. The suspend is a cell RPC, deliberately not a piggyback command

C3 gave fleetd two ways to make a cell do something: a Connect RPC to
that cell's daemon (`daemon_url` + per-cell token) and the
piggyback command queue that rides announce responses. C6 wrote down
when to use which — the queue is the fallback for **delivery failures**,
and it is at-least-once.

Suspend takes the RPC and gets **no piggyback fallback at all**. The
reason is the delivery semantics, not the latency:

- Piggyback commands are retired only by an announce with a **higher
  seq**, and seq is per-boot (C6: "a seq reset (cell reboot) redelivers
  rather than pinning the slot"). Redelivery is the correct, considered
  behaviour for `warm`, `unload` and `probe` — a duplicate warm is a
  wasted second.
- A redelivered `suspend` is a box that puts itself back to sleep on the
  morning after any reboot. The one command in this plan whose
  redelivery is catastrophic is exactly the one that crosses the
  boundary the retirement rule depends on: **the cell's own continued
  liveness.**

So `sleep_schedule` requires `cells.<name>.daemon_url`, and an entry for
a cell without one is refused at wiring time with that sentence in the
log. This is a real narrowing versus C2's drain (which does fall back to
the announce path) and it is deliberate: the announce-only cells in this
fleet are `always_on` anyway, and §1 already refuses those.

**How a cell suspends is house-specific and stays out of this repo**
(ground rule 3). The repo ships the mechanism:

- `cell_cmds.suspend` in the CELL's `config.yaml`, beside the existing
  `drain` and `resume` verbs, run through the same `sh -c` +
  60s-timeout + stderr-capture path;
- the `CellSuspend` RPC that runs it, with the same error contract as
  `CellDrain` (`FailedPrecondition` when unconfigured,
  `Unavailable` + stderr when the command fails);
- a reference example, on reference-fleet values only:

  ```yaml
  cell_cmds:
    drain:   "systemctl --user stop llama-swap"
    resume:  "systemctl --user start llama-swap"
    suspend: "systemctl --user stop llama-swap && systemctl suspend"
  ```

Two contract notes belong with that example, because both are the
difference between a working night and a confusing one:

- **The command must RETURN.** `systemctl suspend` is asynchronous by
  design: it asks logind and returns, and the box freezes shortly after.
  A command that blocks until the machine is actually frozen makes the
  RPC fail with a transport error, which fleetd reports as
  `suspend outcome unknown` and — per the C2 one-writer rule — records
  no intent for. The paired wake still fires; the night is not lost, but
  the status is uglier than it needs to be.
- **Stopping the serving stack first is the house's call, and the
  reference example does it.** CUDA contexts do not reliably survive S3,
  so a llama-swap left running across a suspend is a llama-swap that
  answers `/health` and fails every request in the morning — friction
  pain 2 with a power button. The morning `resume` verb is what brings
  it back, and §5 explains why it runs automatically.

### 3. The guard ladder: every rung defers, every skip is named

One `suspendGuard(cell)` holds every rung and serves both producers (the
schedule loop and the explicit `suspend_cell` verb), for C8's reason:
"one shared guard so the producers cannot drift" is only true of the
rules the shared guard actually holds.

| # | rung | outcome | why |
|---|---|---|---|
| 1 | cell is the front | refuse | the data plane and the control plane ride it (§1) |
| 2 | unknown cell | refuse | a typo must not read as "already asleep" (C6's fail-fast rule) |
| 3 | class is not `opportunistic` | refuse | §1 |
| 4 | declared drain (`effectiveIntent` = drained) | **defer** | a drained box is a box the operator TOOK — for gaming, for a build, for a bench. Suspending it mid-game is the single worst thing this phase could do, and the drain is the operator's own declaration that they are using the hardware |
| 5 | active C11 hold | **defer** | a hold declares that fleetd must not act on its own policy on this cell; suspending it is the largest act available |
| 6 | cell is absent (never announced / stale / withdrawn / unreachable) | **skip** | there is nothing to suspend. Not a deferral and not a failure: no intent is recorded, because fleetd did not put this box to sleep and must not claim it did. It sits ABOVE the in-flight rungs deliberately — an absent cell reports no count either, and answering "in-flight unknown" would turn "there is no box here" into a deferral that retries all night |
| 7 | in-flight count UNREPORTED | **defer** | C5's M2, verbatim: unknown is not zero. A cell fleetd cannot measure is a cell fleetd must not suspend |
| 8 | in-flight > 0 | **defer** | someone is mid-generation |
| 9 | any active lease on the cell | **defer** | "did I just strand a 19-hour job?" — the pre-drain question, asked automatically at 23:30 |
| 10 | a `probe` command queued or handed-over-unacked (C8) | **defer** | a probe fleetd ASKED for is GPU work fleetd started; the answer rides a later heartbeat, and suspending the box loses both the measurement and the reason it was taken |
| 11 | no activity-observation channel (`observesActivity`) | **defer** | C4/C5's rule that "fleetd never looked" is not evidence of silence |
| 12 | any model on the cell was request-active within `quiet_for` | **defer** | §4 — the human at the keyboard |

Rungs 1–3 are refusals: a permanent configuration answer, logged once at
wiring time and reported in the status forever. Rungs 4–12 are
deferrals: the declared suspend stays pending and is re-evaluated every
minute until it fires or is abandoned (§6).

**Every skip is named in `fleet_status`,** in the entry's `state` +
`detail` + `last_skip` fields, exactly as C4's warm targets and C8's
probe targets do. A guard that silently skips forever is the failure C5
spent a whole phase fixing, and this one has a nightly cadence, which
means a silent skip costs 365 nights before anyone notices.

**Rung 10 answers the question the phase brief asks explicitly**: a cell
that is mid-probe may not be suspended. Two independent mechanisms cover
it — the probe request sitting in the piggyback queue (rung 10) and the
probe's own inference load showing in the cell's in-flight count (rung
8) — and the first is the one that holds when the request has been
handed over but the result has not landed yet.

### 4. The operator at the box at 23:29

This is the safety requirement, and in-flight counting alone does not
meet it: a human typing their next message to a chat UI has zero
in-flight requests. What protects them is rung 12, the **quiet window**:

> The suspend defers while ANY model on the cell has been
> request-active within `quiet_for` (default 15 minutes, floor 5
> minutes, clamped never skipped).

The activity stamps are C4's, unchanged: per-model last-request
timestamps derived from llama-swap's inflight SSE frames on fleetd's own
clock (`modelActivity`, stamped on both the start and the completion
edge — C5's fix, without which one generation longer than the window
reads as idle). No new measurement mechanism, and deliberately so: this
phase adds no observer, it subscribes to one that has been in
production since C4.

Two properties of that reuse are load-bearing:

- **The window is a DEFERRAL, never a trigger.** "Quiet for 15 minutes"
  can only delay 23:30. It cannot produce a suspend at 02:00, because
  nothing evaluates it at 02:00 unless a declared suspend is pending.
- **Unknown activity defers** (rung 11). C4's `observesActivity`
  distinguishes "fleetd watched and saw nothing" from "fleetd never
  looked", and only the first is evidence. Where fleetd has no
  observation channel to a cell, the suspend does not fire — which is
  the same conclusion C4 reached for the warm restore, for the same
  reason, and it means a broken events stream fails toward "the box
  stays up".

The operator sitting at the box at 23:29 therefore keeps their box: the
23:30 suspend defers, and it keeps deferring for as long as they keep
using it. When they stop, it fires — unless they stopped after the
abandonment deadline (§6), in which case the night is simply skipped and
the status says so.

Test-pinned as U6 and U7: a cell with activity 30 s ago at the fire
minute does not suspend and reports `deferred (last request 30s ago,
quiet window 15m)`, and a cell whose activity stamp is inside the window
still does not suspend after five further evaluations.

### 5. The paired wake: clear intent, WoL, await return, warm

The wake half is one field on the same entry (`wake:`), not a separate
list, because **a suspend entry with no wake is a box that never comes
back** and the config should make it unwritable. An entry without
`wake:` is refused; an entry whose wake cron fails to parse, or resolves
no fire time within the evaluator's eight-year horizon, disables the
**whole entry including the suspend half**. That last rule is the one
worth remembering: a broken wake never yields a box that sleeps
forever — it yields a box that never sleeps.

The sequence at the wake minute, in this order:

1. **Clear the sleep intent at fleetd, BEFORE the packet.** The entry
   recorded `{state: drained, reason: "asleep per sleep_schedule", eta:
   "07:15"}` when it suspended; the wake clears it by writing the
   `serving` request through the same `SetIntent` path a resume uses.
   Order matters: a box that comes back to find fleetd still requesting
   `drained` gets that request handed to it as `desired_intent` on its
   first heartbeat and dutifully runs its own `cell_cmds.drain` — a
   morning with the box powered on and the serving stack stopped. The
   clear-first ordering closes that window, and §7's cell-side intent
   stamp closes what is left of it.
2. **Only a sleep this schedule performed is cleared.** The clear
   matches on the reserved reason string; an operator's
   `vibe cell drain --reason gaming` at 22:00 is never cleared by a
   07:15 wake. Otherwise the schedule would silently resume a box the
   operator had declared they were using — inferred intent, acted on,
   which is the thing.
3. **Send the wake through C2's exact path** (`Server.SendWake`): magic
   packet, or the per-cell fallback command where fleetd's network
   position cannot reach L2 broadcast. Reusing it rather than
   re-implementing means the scheduled wake and `wake_cell` cannot
   diverge, and the fallback command keeps working. A cell that is
   already fresh/reachable skips the packet with a note.
4. **Await the return**, polling presence and reachability for
   `wake_grace` (default 10 minutes). This runs on its own goroutine so
   the entry's minute ticker keeps running; overlapping wakes are
   prevented by a per-entry flag.
5. **Warm the declared models** (`warm:`) through the front once the
   cell is back, via C4's `warmViaFront` with C6's piggyback fallback —
   the same path a warm schedule uses, so a warm that the front cannot
   deliver queues rather than lying.

**When the wake fails.** A wake that silently fails is a morning with no
fleet, so it is not allowed to be silent:

- the entry's status goes to `wake_failed` with `wake_failed_since` and
  a detail naming what was tried (packet vs command, and the target);
- `slog.Error`, not Warn;
- a `fleet.wakeFailed` event on `/api/fleet/events`;
- and a **C9 alarm** (`KindWakeFailed`), enabled by default.

That last one deserves its argument, because C9's default policy is "the
class table's alarm column and nothing else" and this adds a fifth kind.
The class table says an `opportunistic` cell's absence never alarms, and
that stays true — this alarm is not about observed absence. It fires
only when **fleetd itself declared the box asleep and its own paired
wake did not bring it back**: a declared action that did not complete,
which is a fact about the control plane's own promise rather than an
inference about the box. An operator who configures a sleep schedule has
implicitly asked to be told when the fleet cannot honour it, and the
alternative — discovering it at 09:00 — is the exact failure the futures
entry warns about. It is scoped to entries this schedule suspended, and
it clears (with C9's resolve notification) the moment the cell announces
fresh again.

**Checkable BEFORE the night it matters.** Three surfaces, deliberately
all before-the-fact:

- **wiring-time refusals**: no `wake:`, no `wake:` config in hosts.yaml
  (MAC or fallback command), no `daemon_url`, wrong class, unparseable
  cron — each refuses the entry loudly at fleetd startup rather than at
  23:30.
- **`fleet_status.sleep`**: every entry's resolved `next_suspend` and
  `next_wake`, in UTC on the wire and rendered locally, so a wrong
  timezone is visible at a glance (C4 §2's rule, applied to a schedule
  whose wrong-timezone failure mode is "the box sleeps through the
  working day").
- **`vibe fleet doctor`**: a new `sleep.schedule` check per entry —
  wake path configured, both fires resolvable, and the last wake's
  outcome. It composes C13's existing `wake.configured`, which already
  says the thing this phase needs said: whether a NIC is actually armed
  is **not observable from the control plane**, and sending a packet to
  find out is a mutation. The quarterly fire drill C13 pairs itself with
  is still the only real test of a WoL path, and this phase does not
  pretend otherwise — what it adds is that the drill now has a schedule
  to drill.

### 6. Deferral is bounded, and abandonment is normal

A deferred suspend is re-evaluated every minute and abandoned at
whichever comes first:

- `max_defer` after the declared minute (default 2h, clamped to
  [1m, 6h]); or
- the paired wake's next fire — **the suspend never fires after its own
  wake**. A 23:30 suspend that has been deferred until 07:00 must not
  take the box down for fifteen minutes; that is not a night's saving,
  it is an outage with a power-management theme.

Abandonment is recorded as `skipped` with the reason that was blocking
at the end, and the next night's fire is scheduled normally. A cell that
is busy every night simply never sleeps, visibly, which is the correct
outcome and a legible one: `fleet_status` and `vibe fleet doctor` both
show a schedule that has not fired.

There is no catch-up. A suspend missed because fleetd was down is a
suspend that does not happen; the next declared minute is the next
opportunity.

### 7. Where the state lives (and the ghost-drain trap)

**No new state axis, no new display state, no new intent vocabulary.**

A sleeping cell is recorded exactly as axis 2 was designed to record a
declared not-serving box:

```json
{ "gpu-cell": { "state": "drained",
                "reason": "asleep per sleep_schedule",
                "eta": "07:15",
                "since": "2026-08-05T23:30:00Z" } }
```

which renders, with **zero new rendering code**, as `OFF` in the display
table (host down + declared drain — "it was drained first"), as
`intent: asleep per sleep_schedule, eta 07:15` on the fleet page's cell
row, and identically in `vibe cell status`. The futures entry's
requirement — *status shows "asleep per schedule, wake 07:15"* — is met
by the substrate C1 built, and the page diff for this phase is empty.

That leaves one trap, and it is the reason the cell-side half exists.
C3's conflict rule makes registry intent a REQUEST until the cell echoes
it. If fleetd records `drained` at 23:30 while the cell's own echo still
says `serving` from some older instant, then the cell's **first
heartbeat after waking** receives that request as `desired_intent` and
runs `cell_cmds.drain` — a box that wakes at 07:15 and immediately stops
its serving stack. So:

> **`CellSuspend` stamps the cell's own local intent `drained` before it
> reports success**, on every invocation path, remote included.

This is not a violation of the C2 one-writer rule, which is about who
writes at *fleetd*: the local intent file is the CELL's record of its
own state, and a box that is about to freeze is not serving. With the
stamp, the wake-up heartbeat carries `drained` at a time NEWER than
fleetd's request, so C6's "a complied drain becomes the RECORD" branch
resolves it — no request is handed back, no ghost drain. Then the wake
half's `serving` request is newer still, the cell runs its own
`cell_cmds.resume`, and the serving stack comes back **because the
existing C2/C3 machinery already does exactly that**. The morning resume
is not new code; it is the drain conflict rule pointed at a night.

The residual window is one heartbeat wide and only opens if the box
freezes between running the suspend command and stamping — in which
case the cell wakes echoing `serving`, receives the drained request,
re-runs its (idempotent) drain verb, echoes `drained`, and the wake's
serving request resumes it one heartbeat later. It self-heals through
the same rule, and it is test-pinned rather than hoped for (U13).

### 8. The explicit verbs: `suspend_cell` and `vibe cell suspend`

The scheduled path is not the only one worth having — "goodnight, put
the 5090 to sleep" is a sentence the operator says to an agent, and
`vibe cell suspend` is the verb to type at the box. Both exist, both go
through the same RPC, and both apply the same guard with one
difference:

- **`force: true` / `--force` skips the POLICY rungs (4, 5, 9, 12) and
  the cell-side idle proof**, because an operator asking is not fleetd
  guessing (C11's line). It does not skip the structural refusals (1–3):
  a `force` that suspends the front is not an override, it is a bug.
- Without force, the explicit verbs refuse — with the guard's reason —
  and the reason is the whole value: "gpu-cell has 2 in-flight" is
  exactly what the operator needed to know before taking the box down.
- Both refuse a cell with **no wake path configured** unless forced,
  same rule as the schedule: a suspend nobody can undo remotely is a
  walk to the basement.

`CellSuspendRequest.require_idle` carries the distinction to the cell,
which enforces it a second time on its own in-flight counter —
`FailedPrecondition` when it is non-zero **and when it is unreported**,
because unknown is not zero on the receiving side either. C11's lesson,
stated once more: the sending side guarding and the receiving side not
is this repo's most repeated defect.

### 9. What this phase does NOT do

- **No idleness-initiated suspend, ever** — the whole §"invariant line".
- **No request-triggered wake.** Futures §4 kills it by name (a new
  data-plane hop, and it contradicts "wake is always explicit"). The
  wake here is a declared cron entry, which is a declaration, not a
  request.
- **No `sleep`/`asleep` display state, no third intent state, no new
  announce field.** §7.
- **No hibernate/poweroff verb.** The mechanism is a command the house
  writes; if the house writes `systemctl poweroff` there, the wake half
  is a WoL packet either way and this repo neither knows nor needs to.
  What the repo will not do is ship a *second* verb whose only
  difference is how permanently the box goes away.
- **No data-plane change.** `internal/vibe/proxy` is untouched; the diff
  against main for that path is empty.
- **No page change.** §7 — the intent row already renders it.

### 10. Files

| file | change |
|---|---|
| `proto/vibe/v1/control.proto` | `CellSuspend` RPC + request/response (`require_idle`, `reason`; resident models, in-flight, idle status) |
| `proto/vibe/v1/control.pb.go`, `vibev1connect/` | regenerated (`buf generate`) |
| `internal/vibe/daemon/daemon.go` | `CellCmds.Suspend`, `Config.SleepSchedule` |
| `internal/vibe/daemon/cell_suspend.go` | the `CellSuspend` RPC: idle proof, local-intent stamp, verb execution |
| `internal/vibe/daemon/sleep.go` | config → `fleetapi.SleepScheduleEntry` validation, clamps and refusals; the suspend function the loop calls |
| `internal/vibe/fleetapi/sleepsched.go` | the loop, `suspendGuard`, the wake half, the status block |
| `internal/vibe/fleetapi/fleetapi.go` | `StateSnapshot.Sleep`, server fields |
| `internal/vibe/fleetapi/intent.go` | `SetIntentAt` (review REV-1): the record's `since` is the instant the action was ISSUED, so a sleeping box's own echo can resolve it |
| `internal/vibe/fleetapi/notify.go` | the `wake_failed` condition (read off the snapshot, C9's rule) |
| `internal/vibe/fleetnotify/fleetnotify.go` | `KindWakeFailed` + its dwell + default-on |
| `internal/vibe/fleetapi/doctor.go` | `sleep.schedule` check |
| `internal/vibe/fleetmcp/sleep.go` | `suspend_cell` tool |
| `internal/vibe/fleetmcp/fleetmcp.go` | tool registration + dispatch |
| `internal/vibe/cli/cmd_cell_actuate.go` | `vibe cell suspend` |
| `internal/vibeclient/client.go` | `CellSuspend` method |
| tests | `fleetapi/c14_test.go`, `daemon/c14_test.go`, `fleetmcp/sleep_test.go`, `cli/cmd_cell_suspend_test.go` |
| docs | this file, the plan README row, futures item 9, design §5/§7, AGENTS.md, `deploy/fleetd/README.md` |

Cron parsing, the Vixie dom/dow rule, DST semantics and the eight-year
scan are **reused verbatim** from `fleetapi/warmsched.go` (`parseCron`,
`cronSpec.nextFire`). Not forked, not re-derived: that evaluator carries
a correctness fix (textual star flags) that a second copy would not have,
and two cron evaluators in one package is how one of them silently rots.

## Acceptance gates

### Unit gates (mechanical, in-repo)

- **U1 — the evaluator is shared, not forked.** A test asserts the sleep
  loop's next-fire computation against `warmsched.go`'s `parseCron` +
  `nextFire` on the same specs, including a `*/2` dom (textual star ⇒
  AND) and `dow=7`; a grep test fails on a second `parseCron` in the
  package.
- **U2 — class refusals.** `always_on`, `roaming` and the front are
  refused at wiring with distinct named reasons; `opportunistic` is
  accepted.
- **U3 — configuration refusals.** No `wake:`, no `daemon_url`, no
  hosts.yaml `wake:` block, unparseable suspend cron, unparseable wake
  cron, and a wake cron that never fires each disable the entry —
  including its suspend half — with the reason in the status.
- **U4 — the ladder, rung by rung.** Twelve subtests: front, unknown
  cell, wrong class, declared drain, C11 hold, absent cell, in-flight
  unreported, in-flight > 0, active lease, queued probe, no observation
  channel, recent activity. Each asserts no suspend AND the named reason
  in `fleet_status`.
- **U5 — the clean night.** All guards clear ⇒ exactly one suspend call,
  intent recorded with the reserved reason and the wake's local ETA,
  state `asleep`.
- **U6 — the operator at 23:29.** Activity 30 s before the fire minute
  defers; the detail names the quiet window.
- **U7 — deferral persists and is re-evaluated**, then fires on the
  first minute after the window clears.
- **U8 — abandonment.** A deferral that outlives `max_defer` is
  abandoned with the blocking reason; a deferral that reaches the paired
  wake minute is abandoned regardless of `max_defer`.
- **U9 — the suspend never fires after its wake** (the same property,
  asserted directly on ordering rather than through the clock).
- **U10 — the wake half, in order.** Intent cleared BEFORE the packet
  (observed through a scripted wake function that asserts the intent
  store's contents at call time), packet sent, return awaited, declared
  models warmed.
- **U11 — the wake clears only its own sleep.** An operator drain with a
  different reason survives the wake untouched.
- **U12 — wake failure is loud.** No return within `wake_grace` ⇒
  `wake_failed` + `wake_failed_since` + the `fleet.wakeFailed` event +
  the C9 condition; a later fresh announce clears all four.
- **U13 — the ghost-drain trap.** With the cell-side stamp, a
  suspend-then-wake cycle hands the returning cell NO `drained`
  desired_intent; without it (the stamp removed), the test observes the
  ghost drain — mutation-verified, so the guard cannot be deleted
  silently.
- **U14 — `CellSuspend` contract.** Unconfigured ⇒ `FailedPrecondition`;
  failing command ⇒ `Unavailable` + stderr; `require_idle` with
  in-flight > 0 ⇒ `FailedPrecondition`; `require_idle` with an
  UNREPORTED count ⇒ `FailedPrecondition`; without `require_idle` ⇒ runs.
- **U15 — the local-intent stamp happens on the remote path too**, and
  only after the command succeeds.
- **U16 — `suspend_cell` and `vibe cell suspend`** share the guard,
  report its reason on refusal, and `force` skips the policy rungs but
  never the structural ones.
- **U17 — the status block** carries `next_suspend`/`next_wake` for every
  entry and survives a `Close()` race (`-race`, loops stopped mid-defer).
- **U18 — no data-plane diff.** `git diff main -- internal/vibe/proxy` is
  empty, asserted in the PR body rather than in code, plus the existing
  fleetd-route-list test extended with no new route (this phase adds
  none).

### Live gates (four need a real box that really suspends; L3 and L4 PASSED — see the [gate table](#gates))

- **L1 — one real night.** `gpu-cell` suspends at the declared minute
  with nothing running, and `fleet_status` shows OFF with
  `asleep per sleep_schedule, eta 07:15`. Measure the wall draw before
  and after.
- **L2 — the wake.** WoL brings it back within `wake_grace`, the resume
  verb restarts llama-swap via the desired-intent path, the declared
  model warms, and the first morning request is served without a manual
  step.
- **L3 — the operator at 23:29.** Type into a chat UI at 23:29; the
  suspend defers, and keeps deferring while the session continues.
- **L4 — the mid-batch night.** Start an overnight batch with a lease;
  the suspend defers all night and is abandoned at the wake, visibly.
- **L5 — the wake that fails.** Disarm WoL in the BIOS (or block the
  broadcast), let a night run, and confirm the alarm arrives and
  `vibe fleet doctor` reports it in the morning.
- **L6 — the drill.** `vibe cell suspend gpu-cell` then `wake_cell` from
  a phone, start to finish, as the quarterly fire drill C13 asks for.

## Execution (2026-08-05)

### What shipped

Everything in §10's table, in two commits: the feature, then the
adversarial-review pass (ground rule 9). Two things came out differently
from the plan above and both are now reflected in the design sections:

- **Absence became a first-class guard outcome** (`SuspendBlock.Absent`)
  rather than a check beside the ladder, and it moved ABOVE the
  in-flight rungs. An absent cell reports no in-flight count either, so
  in the original order "there is no box here" answered as "in-flight
  unknown" and retried all night instead of stopping for it.
- **`SuspendBlock` gained `Structural`** so the explicit verbs' `force`
  has something principled to refuse: the front, an unknown cell and the
  wrong class are configuration answers, not tonight's conditions.

### Gates

| gate | result |
|---|---|
| U1–U18 (unit) | **PASS** — `go build ./...`, `go vet ./...`, `go test -race -count=5 ./...`, `gofmt -l .` silent, `go mod tidy` clean, `golangci-lint run` 0 issues |
| L1 (one real night) | **NOT RUN — needs metal.** Two physical facts, not one: a box that actually enters S3 and comes back, and a wattmeter on its cord. The *schedule* half is separable and locally runnable — `cell_cmds.suspend` is a declared command, so a harness cell can point it at a no-op and the fire/defer decision can be watched end to end (`scripts/fleetlab` writes exactly such a stub) — but "suspends at the declared minute" and "measure the wall draw before and after" are the gate, and neither is simulable. |
| L2 (the wake) | **NOT RUN — needs metal.** A magic packet on a real NIC reaching a powered-off machine, and that machine's BIOS/firmware honouring it. Loopback has no equivalent: there is nothing to wake. |
| L3 (the operator at 23:29) | **PASS (2026-08-05, C17, `scripts/fleetlab/gate-c14-l3.sh`)** — on the local multi-cell harness, with the cron minutes computed from the wall clock **in the fleet timezone** and `cell_cmds.suspend` pointed at the lab's no-op stub, so the DECISION is observable without suspending a workstation. At the declared minute (21:51:00Z) with a real request 24 s old, the entry went `deferred` — `cell bravo served a request 24s ago (quiet window 5m0s)` — stamped `deferred_since` and rolled `next_suspend` to tomorrow. The session then continued at one request a minute and the age was re-derived on every tick: 24s → 1m24s → 2m24s → 3m24s. At **21:59:00Z**, 5m24s after the last request and the first minute tick past the quiet window, the suspend FIRED: `cell-verbs.log` recorded `fake-suspend bravo`, the entry read `asleep / suspended` with `last_suspend` stamped, the registry intent became `{"state":"drained","reason":"asleep per sleep_schedule","eta":"18:14"}`, and the CELL's own `cell-intent.json` carried `drained` at **the same nanosecond** — C14's "CellSuspend stamps the cell's own intent before it freezes", observed. **Two lab artifacts, not defects:** the display is `INCONSISTENT` rather than `OFF` because the stub suspend leaves bravo running and announcing, so C3's evidence-over-declaration rule contradicts the declared drain (on metal the box is gone and it renders OFF); and the first attempt declared its cron in the LOCAL zone rather than the fleet's, which C4's published `next_fire` caught on sight — the rule worked, the harness operator was the one who needed it. |
| L4 (the mid-batch night) | **PASS (2026-08-05, C17, `scripts/fleetlab/gate-c14-l4.sh`)**, with `max_defer` shortened to 4 m — the substitution the row predicted, and the one thing the gate does not prove, since what "all night" adds is repetitions of a once-a-minute decision this run watched four times. A batch claimed a real lease through `vibe cell await bravo --model lab-embed-b --ready --lease overnight-batch --lease-ttl 30m`, then the box went quiet. At the declared minute (22:04:00Z) the entry went `deferred` naming **`1 active leases on bravo`** — and from 22:06 the quiet window was satisfied too, so for the last two minutes the lease was the ONLY blocker and the detail never changed. At `max_defer` (22:08:00Z) it abandoned, visibly: `skipped / abandoned after the defer window (1 active leases on bravo)`. Nothing was suspended (`cell-verbs.log` 0 bytes), **no intent was recorded** (`intent: null`), the display stayed `SERVING`, and two further minutes produced no return and no fire. |
| L5 (the wake that fails) | **NOT RUN — needs metal.** A BIOS switch to disarm, and a real night to fail across. |
| L6 (the drill) | **NOT RUN — needs metal.** A real box to suspend and wake, from a phone. |

**Assessment (2026-08-05, amended by C17 the same day).** The 2026-08-05
harness run moved most of the plan's live gates off "needs the real
fleet" by observing that they needed a second *cell*, not a second
*box*. C14 is the phase where that observation only partly applies:
four of its six gates are about a machine's power state, which no number
of processes on one box can stand in for. L3 and L4 were the exceptions,
they were owed, and [C17](c17-gate-closure.md) paid them — both PASS,
with every substitution (stub suspend verb, 4-minute `max_defer`,
fleet-timezone cron arithmetic) named in the rows above.

The four review fixes below are each **mutation-verified**: the guard
was removed, the named test failed, the guard was restored.

## Self-review addendum (2026-08-05, 4 findings, all fixed with regression tests)

**REV-1 — the sleep record was an unackable request, every night.**
`SetIntent` stamps `since` at the moment it is called, which for a
suspend is the moment the RPC *returned*. The cell stamps its own
`drained` echo while it is still running — i.e. earlier — so the
registry request was permanently newer than the only echo that would
ever exist. Consequence: every sleeping box rendered `intent: asleep per
sleep_schedule, eta 07:15 (requested, awaiting ack)` all night, waiting
for an ack a frozen machine cannot give, and (REV-2) the doctor called
it residue every morning. Fixed with `SetIntentAt`, which takes the
instant the action was ISSUED — not a fudge, that is genuinely when the
intent was formed — so C6's complied-drain branch resolves the request
into the record, keeping the reason and the ETA and dropping the pending
flag. `SetIntent` is now a one-line wrapper, so no other caller changed.
The explicit `suspend_cell` verb takes the same path.

**REV-2 — `intent.hygiene` would have turned yellow every morning.**
Even with REV-1, a sleeping cell that does not announce at all has no
echo to resolve anything, so the pending flag legitimately stands. C13's
check counts a request unacked for longer than `staleRequestAge` as
residue — which on a fleet doing exactly what it was configured to do
would be a permanent WARN, the failure C13's own review pass had to fix
three times. The check now recognises the reserved sleep reason and
reports OK. The regression test carries its own control: the same shape
with an ordinary drain reason is still WARN.

**REV-3 — the wake warmed models into a cell the operator had drained.**
The wake half fires on its cron whether or not this schedule was why the
box was away. An operator who drained the box for gaming at 22:00 and
was still playing at 07:15 would have had the declared models warmed
onto that GPU — the exact eviction fight C4 §1 exists to prevent, at an
hour when nobody is watching. The warms now take C4's drain guard and
the skip is named in the status.

**REV-4 — two entries for one cell were two loops racing one machine.**
`sleep_schedule` did not dedupe by cell, so a copy-paste typo produced
two independent suspend arms against one box; the second RPC lands on
something already freezing and reads as a flaky suspend. One night per
cell, refused loudly at wiring.

### Looked at and deliberately left alone

- **No early auto-resume.** A box that comes back before its wake (an
  operator pressing the power button at 03:00) keeps its sleep record
  until the declared wake clears it. Clearing it because the cell
  reappeared would be an observation initiating an action, which is the
  one thing this phase may not do — and the box is not stranded: the
  display reads DRAINED with "asleep per sleep_schedule, eta 07:15",
  which is the true sentence, and `vibe cell resume` is one command. The
  entry's own `asleep` bookkeeping is cleared, since that is fleetd's
  record of its own action rather than a statement about the box.
- **A suspend RPC that dies in transport stays "outcome unknown".** If
  the house's command blocks until the machine is frozen, the RPC fails
  and no intent is recorded. Inferring "it probably suspended" from a
  transport error is exactly the guess this plan refuses; the contract
  (the command must return) is documented instead, and the paired wake
  fires either way.
- **The residual ghost-drain window is one heartbeat wide.** If the box
  freezes between the suspend command returning and the local stamp, the
  cell wakes echoing `serving`, takes fleetd's drained request, re-runs
  its idempotent drain verb, echoes `drained`, and the wake's serving
  request resumes it. It self-heals through C3/C6's own rules, and both
  branches are pinned in
  `TestSleepSchedule_TheCellsOwnStampIsWhatPreventsTheGhostDrain`.

## Adversarial-review addendum (2026-08-05, 7 findings, all fixed with regression tests)

Ground rule 9's separate pass, run against the feature + self-review
commits above. Every fix below is **mutation-verified**: the production
change was reverted, the named test was watched to FAIL, and the change
was restored. Two of the seven are blockers — one takes the fleet down,
one pages the operator every morning about the thing the class table
forbids paging about.

- **REV2-1 — `vibe cell suspend` held none of the structural refusals
  (BLOCKER).** §8 says both explicit verbs "apply the same guard", and
  U16 says `force` "skips the policy rungs but never the structural
  ones". `suspend_cell` did. The CLI did not: it resolved the cell's
  daemon and called the RPC, full stop. So `vibe cell suspend front` ran
  the front's suspend verb — the data plane and the control plane, off
  — `vibe cell suspend laptop` put a roaming box to sleep that no packet
  on this LAN can wake, and a cell with no `wake:` block went down with
  no way back. The CLI is not a fleetd client, so it cannot call
  `SuspendGuard`; it now applies the structural half itself from
  `hosts.yaml` (front by name, class, wake path), and `--force`
  overrides only the wake-path rung. The **receiving side** grew the
  same refusal, because "the senders check and the receiver does not" is
  this repo's most repeated defect and a fourth caller is always one
  line away: a daemon whose `fleet.cell` is `front` refuses a REMOTE
  `CellSuspend` outright. Local invocation is untouched — a human at the
  front box typing the verb has declared it.
  `TestCellSuspendHoldsTheStructuralRefusals`,
  `TestCellSuspend_TheFrontRefusesARemoteSuspend`.
- **REV2-2 — the wake alarmed for a box this schedule never suspended
  (BLOCKER).** The wake half fires on its cron whether or not this
  schedule is why the box is away — which is correct, the cron declared
  it — but `failWake` did not know the difference. An `opportunistic`
  cell switched off on Friday therefore produced, every single morning:
  a packet, ten minutes of waiting, `wake_failed`, `slog.Error`, a
  `fleet.wakeFailed` event and the default-ON C9 alarm. Design §4's
  class table says an opportunistic cell's absence never alarms, ever,
  and C9 already shipped and had to fix this exact bug. The alarm's own
  justification is the fix: it is allowed into the default set only
  because it is *"fleetd suspended this box and its own paired wake did
  not bring it back"*, so it now fires only when the entry believes it
  performed that suspend — `st.asleep`, **or** the reserved sleep record
  was there to clear, which is what carries the fact across a fleetd
  restart. A wake that finds nothing is still visible in `fleet_status`
  and the log; it just is not a page.
  `TestSleepSchedule_AWakeForABoxItNeverSuspendedDoesNotAlarm`.
- **REV2-3 — the quiet window claimed silence fleetd was not running to
  observe (MAJOR).** Rung 12 read the per-model activity map and, when
  the cell had no stamp at all, *passed*. So a fleetd that came up at
  23:29 — a container restart, a `-watch-config` reload, a crash-loop
  recovery — saw one empty inflight frame (in-flight REPORTED zero, no
  per-model stamp), concluded the box had been quiet for fifteen
  minutes, and suspended the machine its operator had been using at
  23:28. That operator is the entire reason §4 exists. C4's
  `swapIdleFor` already measures unknown idleness from `Server.started`
  for precisely this reason ("fleetd cannot claim silence it was not
  running to observe"); the guard now does the same, and the deferral
  names the missing evidence. The `cleanNight` fixture was itself the
  tell: it asserted a clean night with *no* activity evidence, so it now
  carries evidence of quiet (a request an hour ago) instead of an
  absence of evidence.
  `TestSuspendGuard_QuietWindowIsFlooredAtFleetdsOwnStart`.
- **REV2-4 — the `observesActivity` rung cannot fire, and its test
  asserted a different rung (MAJOR, ground rule 10).** Rung 11 is
  reached only after rung 7 passed, and rung 7 passes only when
  `inFlightSeen[cell]` — which is one of the two terms
  `observesActivity` is an OR over. So rung 11 is unreachable by
  construction. `TestSuspendGuard_NoActivityObservationChannelDefers`
  claimed to pin it and asserted `"unknown is not zero"`, rung 7's
  message: a name claiming more than the body proves, which is the
  failure ground rule 10 was written for. The ladder is sound — rung 7
  subsumes rung 11 — so the rung stays as belt-and-braces with a comment
  saying so, and the test is renamed for what it proves and now pins the
  **subsumption** (no observation channel ⇒ no reported count) that is
  what actually makes the earlier rung answer first. No behaviour
  change; the honesty is the fix.
  `TestSuspendGuard_NoObservationChannelIsAnsweredByTheUnreportedRung`.
- **REV2-5 — the wake's warms skipped a drained cell but not a held one
  (MINOR).** REV-3 gave them C4's drain guard. A C11 hold is the same
  declaration one step stronger — "fleetd must not act on its own warm
  policy on this cell" — and a wake's declared `warm:` list is fleetd's
  own policy. The queued half was already covered
  (`dropHeldWarmsLocked`); this is the direct-through-the-front half,
  and it skips on a HOLD specifically, not on any lease (C11's rule).
  `TestSleepSchedule_WakeDoesNotWarmAHeldCell`.
- **REV2-6 — `vibe fleet doctor` called a suspend that fails every night
  OK (MINOR).** `checkSleep`'s default branch is right that a deferred
  or abandoned night is the policy working (C13's rule). It also
  swallowed `failed` — the state a suspend RPC that *errored* leaves
  behind. The commonest cause is `cell_cmds.suspend` unset on the cell,
  which fails identically at 23:30 every night forever while the audit
  renders green and the box keeps drawing its 80 W. Now a WARN under its
  own id, `sleep.suspend`, naming the likely cause; the deferred/skipped
  control is pinned in the same test.
  `TestDoctor_ASuspendThatFailsEveryNightIsNotOK`.
- **REV2-7 — `suspend_cell` gave the front remediation advice (NIT).**
  The way-back check ran ahead of the guard, and the front and
  `always_on` cells carry no `wake:` block, so refusing them produced
  "Add `cells.front.wake`, or pass force" — advice that reads as "then
  it would be allowed", for a cell that may never sleep under any
  configuration. Structural now answers first, then the way back, then
  tonight's conditions; the ordering the doc describes is preserved for
  the cells it was written about.
  `TestSuspendCell_ForceNeverBypassesAStructuralRefusal`.

### Looked at and deliberately left alone (adversarial pass)

- **A `suspend_cell`-suspended box gets no `wake_failed` alarm either.**
  REV2-2 keys the alarm on the schedule's own record, and the explicit
  verb writes the operator's reason, not `SleepIntentReason`. A schedule
  cannot distinguish "the operator suspended it with a verb" from "the
  operator switched it off at the wall", and the class table's answer
  when in doubt is silence.
- **`sleep_schedule` on a cell that never announces is a bad morning,
  and it stays refusable only by the operator.** The morning resume
  depends entirely on C3's `desired_intent` path, so a cell with a
  `daemon_url` but no announcer of its own would wake with its serving
  stack stopped and no request to restart it (`SetIntent(serving)`
  DELETES for a never-announced cell — C1's semantics). fleetd cannot
  know at wiring time whether a cell will ever announce, and the fleet's
  announce-only cells are `always_on`, which §1 already refuses. Noted
  rather than guessed at.
- **The MCP verb reports an error when the suspend succeeded but the
  intent write failed.** That is C2's shape, shared verbatim with
  `drain_cell` and `resume_cell`; changing it is a decision about all
  three, not about this phase.
- **The sleep block is guest-visible.** C12's grant is a ROUTE grant and
  `/api/fleet/state` is on it, so a guest now reads when this house
  sleeps. That follows C12's own rule ("anything on the fleet page is
  guest-visible") and the alternative — a guest-shaped snapshot — is the
  thing C9 and C12 both forbid. Flagged here because the next phase to
  add a field to `StateSnapshot` inherits the same question.
- **A wake sequence interrupted by `Close()` records a failed wake.**
  `awaitCellReturn` returns on `s.done`, so a fleetd shutting down
  mid-wake writes `wake_failed` into memory it is about to discard. It
  is not persisted and the notifier is stopping alongside it; guarding
  it would add a shutdown-shaped branch to a path whose correctness
  under `-race` is currently trivial.
