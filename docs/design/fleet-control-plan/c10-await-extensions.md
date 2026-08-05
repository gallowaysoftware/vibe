# C10 — await extensions: model-ready, cell-idle, and the lease handshake

Status: EXECUTED + REVIEWED + ADVERSARIALLY REVIEWED + MERGED WITH C9
(2026-08-04), off `feat/c10-await-extensions`, merged with `main` at
`03bb4d5` (C11 then C9).
Every mechanically verifiable gate is green under `-race -count=5`; the
four live gates need real hardware and are **NOT RUN** — see
[§Execution](#execution-2026-08-04). Third v2-backlog item
([futures](../fleet-control-futures.md) §2 item 3). Depends on C1
(`vibe cell await`, `/api/fleet/state`), C2 (advisory leases), C3
(presence + the events stream), C4 (the inflight fold that answers "is
this cell busy") and C6 (await's fail-fast rule). Branched off `main`
at `c9e8bcf` (C8 merged; C9's PR #28 was open and is deliberately not
depended on — see [§Merge order](#merge-order)). C9 landed as #28 after
this branch's review finished, and both phases extended
`vibe cell await`; the union and the one semantic decision git could not
make are in [§Merging C9](#merging-c9-2026-08-04).

`vibe cell await gpu-cell --up && ./overnight-batch.sh` is the idiom C1
shipped and the one this operator actually uses. It answers the wrong
question twice:

- **Cell-up is not model-warm.** `--up` returns the instant llama-swap
  answers `/v1/models`. On the heavy cell the model the batch needs is
  then 6–10 minutes from its first token, so the batch spends its first
  request on a cold start it did not budget for — or, worse, times out
  against a client stall timer and reports the fleet broken.
- **Cell-up is not cell-free.** `vibe cell resume` is followed within
  seconds by a reachable cell, so a batch parked on `--up` fires into a
  GPU whose owner just sat down at it. The batch and the human then
  fight over one 24 GB card, and llama-swap resolves it by evicting
  whichever model lost the race.

C10 gives await the two conditions that fix those, plus the lease
handshake that turns "wait for quiet" into "wait for quiet, then say
you are here".

## The finding that shapes this phase

**The evidence for "idle" is not uniformly available, and await must
refuse rather than guess.** C4 hit this first and its answer is the
precedent this phase is bound by: the warm-target policy measures
idleness from the inflight SSE stream, and where fleetd has no such
stream to a cell it *skips the target* instead of restoring, because
otherwise fleetd's own uptime becomes the clock ("no requests seen" is
absence of observation, not evidence of silence — `warmtarget.go`'s
`observesActivity`). C5 had to add that guard after the warm policy
reached the rejected behaviour four different ways.

`await --idle` is the same trap with a bigger blast radius: a warm
restore that fires wrongly costs one model load, while an `--idle` that
fires wrongly launches a 19-hour batch job into a box someone is using.
So:

> **Missing evidence is never idleness.** `--idle` unblocks only on
> positive evidence of silence over a window fleetd was demonstrably
> watching. With no live observation channel the wait continues and
> says why, on stdout, every time the reason changes — it does not
> quietly succeed, and it does not quietly hang either.

That single rule is what the design below is arranged around.

## Design

### 1. Ownership: what await is allowed to read

C10 adds **no state axis and no store**. Every condition is a read of
something an existing axis already owns:

| condition | read from | axis |
|---|---|---|
| `--up` / `--down` | `CellSnapshot.Reachable` | 1, observed (unchanged) |
| `--model <id> --ready` | `CellSnapshot.Models[].State == "ready"` | 3, **llama-swap's residency, observed** |
| `--idle <dur>` | the inflight SSE fold + the watcher's own connection state | 1, observed |
| `--unleased` | `CellSnapshot.Leases` (TTL-filtered at read) | 2, declared |
| `--lease <holder>` | `POST /api/fleet/lease` | 2, declared |

Two consequences are load-bearing:

- **Model readiness is OBSERVED, never stored.** fleetd already merges
  the cell's `/running` into its `/v1/models` catalog (or takes the
  cell's announce as the catalog when probes cannot reach it). await
  reads that same merged row. Nothing new is persisted, nothing caches
  a "warm" bit, and residency stays llama-swap's property — this is the
  invariant that kills a phase, and the way not to break it is to add
  no field that outlives one snapshot.
- **await asks fleetd, never a cell.** The CLI's whole surface stays
  `GET /api/fleet/state` + `GET /api/fleet/events` + the C2 lease
  routes. No new HTTP route, no new MCP tool, no probe of a cell from
  the operator's laptop. (The degraded direct-probe fallback stays what
  `vibe cell status` has: it answers *reachability*, and this phase
  does not extend it — see [§Out of scope](#out-of-scope).)

### 2. `--model <id> --ready`

`--ready` is satisfied when the cell's snapshot carries a model row
whose `ID` (or llama-swap `Name`) equals the id and whose `State` is
`ready`. Four rules around it:

- **The front is refused, loudly.** `vibe cell await front --model X
  --ready` errors immediately. The front's rendered config is
  peers-only (C2), so a peer's id there is a routing entry, not a
  resident process, and waiting for it to report `ready` is a wait that
  can never end. This is C8's `probeGuard` front rule, for the same
  reason, applied to the other verb that would otherwise park forever.
- **An unknown model id fails fast, like an unknown cell** (C6's
  regression: await on a typo'd cell parked until reboot, and
  `--timeout 0` is the documented overnight idiom). The judgement is
  only made on positive evidence: the cell must be **reachable** and
  its catalog **non-empty** before an absent id is called a typo.
  Otherwise a drained cell — which announces an empty model list by
  design (C4) — would turn every `await --model` into an instant error
  during exactly the outage the operator is waiting out.
- **A transport error keeps retrying.** Unchanged from C6, and now
  test-pinned on the model path too: fleetd restarting is the case the
  retry loop exists for, and it must stay distinguishable from fleetd
  answering "no such model".
- **A cold start reports its ETA.** While the row says `starting`, the
  status line carries `StartHistory`'s p50 for that id (C1's honest-ETA
  source, already in the snapshot). It is a fleet-wide median over
  recorded starts, labelled as such — an ETA is not a promise.

`--model` and `--ready` are required together. `--model` alone is
rejected rather than being given an implicit meaning, because the
obvious future flags (`--gone`, `--degraded`) would each want a
different one.

### 3. `--idle <duration>`: riding C4's window, not rebuilding it

**Idle is cell-scoped, always, even with `--model` set.** The resource
a parked batch contends for is the GPU, not the model: a cell serving a
*different* model is exactly as unavailable as one serving yours. A
per-model idle window would be the more precise answer to a question
nobody asked.

The evidence comes from the same fold C4 keyed its warm targets on
(`watcher.go:trackInFlight`, the per-cell `/api/events` stream). C10
reuses it and adds exactly one field to it:

| input | owner | reused as-is? |
|---|---|---|
| `Server.InFlight(cell)` — count + *reported* bool | C4 | yes, verbatim, including "unreported is not zero" |
| the inflight frame fold | C4 | yes; **C10 stamps one cell-level timestamp per frame** |
| `s.cellUp[cell]` — is the events stream connected right now | C1's watcher | yes |
| `cellUpSince[cell]` — when it connected | **new** | — |

**Why a cell-level frame stamp rather than C4's per-model map.** C4
answers "has *this model* been idle", which it computes as the shortest
idle across the residents. C10 asks "has *this cell* been idle", and the
two differ on a model that was busy and has since been TTL-unloaded: it
is no longer resident, so a residents-only maximum would report an idle
cell 30 seconds after the operator's last request. Every inflight frame
is an EDGE (an add or a remove — this is what C4's completion-edge fix
established, and why per-model stamps needed both edges), so "the last
frame" dominates every per-model stamp on that cell and needs no
residency list. It errs toward *not idle*, which is the safe direction
for a gate on a batch job.

The rule, then:

```
idle(cell) =
  if the events stream is not connected NOW        → UNKNOWN ("no evidence")
  if in-flight is reported and > 0                 → 0 (busy)
  else now − max(last inflight frame, stream connect time)
```

Three decisions in that expression, each of which is a trap someone has
already fallen into:

1. **The clock starts when fleetd started WATCHING THIS CELL**, not at
   fleetd start and never at zero. A cell whose watcher reconnected 8
   seconds ago has 8 seconds of provable silence, no matter how quiet
   its frame history looks — fleetd may not claim silence it was not
   connected to observe. This is `warmtarget.go`'s "measure from
   `Server.started`, never a fabricated floor", tightened from
   per-process to per-cell because the reconnect is exactly when a
   long-running request is invisible to us.
2. **An unreported in-flight count is not a zero one** (C4's warm
   schedules, C8's probe guard). Here it does not block the wait —
   with a live stream and no frame ever, the honest reading is "fleetd
   has been watching since T and has seen no request edge", and rule 1
   already bounds how much that claims. It is surfaced in the status
   line either way.
3. **A stale observation channel is no channel.** C4's
   `observesActivity` accepts "an inflight frame was seen once, ever"
   as evidence that fleetd watches a cell; that bit is sticky forever,
   so a cell whose stream died an hour ago still reads as observed.
   await requires the stream to be connected **now**, because the whole
   value of `--idle` is that the silence is current.

**When the evidence is missing.** The wait continues and prints the
reason — naming the cell, the missing channel, and the fact that this
is a refusal to guess — the first time it appears and on every change
thereafter. On `--timeout`, the error names the unmet condition
distinctly from a plain timeout, so a script's log says *idle could not
be evaluated* rather than *the cell never went idle*. There is no
`--assume-idle` escape hatch and there must not be one; the operator's
escape hatch is to drop `--idle`, which is at least honest about what
the batch is doing.

The cells this bites are the ones fleetd holds no live `/api/events`
stream to — a cell behind NAT, or one whose `url` is in `hosts.yaml` and
simply does not answer from where fleetd runs. (*Corrected 2026-08-05:
this read "the ones fleetd has no `url` for (announce-only membership)".
Every `hosts.yaml` cell carries a `url` — the loader requires one — and a
cell absent from `hosts.yaml` cannot announce at all, so neither half of
that sentence described a reachable state. The discriminator is and
always was the observation channel, which is what the code checks.*) The
one activity signal they do carry is C7a's announce-borne cumulative
token counters; wiring those into an idle window is deliberately left
alone (see [§Out of scope](#out-of-scope)).

### 4. Composition: await + idle + leases = a scheduling primitive

The futures entry promises that `--idle` "with leases composes into a
real scheduling primitive". Precisely what that means, with the parts
named:

```sh
vibe cell await gpu-cell --up \
    --model qwen3.6-27b --ready \
    --idle 10m \
    --unleased \
    --lease nightly-eval --lease-ttl 6h --lease-note "1,200 rows" \
  && ./nightly-eval.sh
```

1. `--up` is C1: the cell answers.
2. `--ready` is llama-swap's residency, observed: the model is warm, so
   the batch's first request is not a 10-minute cold start.
3. `--idle 10m` is C4's window: nobody has issued a request to that
   cell in ten minutes, measured over a window fleetd was watching.
4. `--unleased` is C2's advisory store read as a *declaration*: no
   other consumer has said "I am mid-batch here". **Leases held by this
   invocation's own `--lease` holder are ignored** — a crashed run of
   the same job must not deadlock against its own residue.
5. `--lease` is the same store written: on success, and only on
   success, await POSTs the lease before it exits 0. The next `vibe
   cell drain` prints "would strand nightly-eval", C9's
   drain-with-active-lease alarm has something to fire on, and a second
   batch's `--unleased` waits for this one.

**Leases still block nothing** (C2's invariant). `--unleased` is an
opt-in *by the waiting process*, which is the only party entitled to
decide that someone else's declaration matters to it. This is why the
lease is not folded into `--idle`: idle is observation, a lease is a
declaration, and a flag that silently meant both would be a third thing
that is neither.

**A refused lease fails the command.** If the POST fails, await exits
non-zero and `&& ./batch.sh` correctly does not run: the operator asked
for the claim, and a batch that runs undeclared is exactly the state
the pre-drain report cannot see. The race window between the last
evaluation and the POST is real, bounded by one poll interval, and left
open — leases are advisory, and the honest fix for a hard mutex is a
hard mutex, which this fleet does not need.

### 5. One evaluation, one snapshot

Every requested condition is evaluated against **the same** state
document. Polling each condition independently would let `--ready` be
true at 03:00:05 and `--idle` true at 03:05:05 and call the composite
satisfied, having never observed a moment when both held.

The C3 events fast-path survives with one change: for a plain
`--up`/`--down` wait, a matching transition event still returns
immediately (that is what makes the existing sub-second unblock work).
With any extra condition requested, a matching event instead triggers
an **immediate re-poll** — an event proves reachability changed, not
that a model is warm.

### 6. Surfaces

| surface | what |
|---|---|
| CLI | `vibe cell await <cell> [--model <id> --ready] [--idle <dur>] [--unleased] [--lease <holder> --lease-ttl <dur> --lease-note <s>]` |
| status | `CellSnapshot.Activity` — `observed`, `observed_since`, `in_flight`, `last_request`, `idle_s`, `reason` |
| HTTP | none new. Activity rides `/api/fleet/state`; the lease POST is C2's route |
| MCP | none new. `fleet_status` returns the same snapshot, so an agent can read idleness without a tool being added for it |

`--timeout 0` remains wait-forever and remains the overnight idiom.
Every added condition is opt-in: an existing `vibe cell await X --up`
invocation is byte-for-byte unchanged in behaviour.

## Acceptance gates

1. **Ready gate (unit).** `--model <id> --ready` blocks while the row
   says `stopped`/`starting` and unblocks when it says `ready`; a
   `--model` whose row never appears keeps waiting; the success line
   names the model. Mutation-verified: accepting any non-stopped state
   makes it fail.
2. **Unknown-model gate (unit).** A reachable cell with a non-empty
   catalog and no such id fails fast (like C6's unknown cell) even with
   `--timeout 0`; an **unreachable** cell and a reachable cell with an
   **empty** catalog both keep retrying; a 500 from fleetd keeps
   retrying. Mutation-verified: dropping the reachable/non-empty
   qualification turns the drained-cell case into an error.
3. **Front gate (unit).** `--model X --ready` against the `front` cell
   is refused before any wait, naming peers-only as the reason.
4. **Idle-evidence gate (unit, both halves).** With a live stream and a
   frame older than the window, `--idle` unblocks. With **no** live
   observation channel it does **not** unblock, the reason is printed,
   and a `--timeout` error names the missing evidence rather than
   reporting a cell that never went idle. Mutation-verified: treating
   `observed == false` as idle makes the second half fail.
5. **Idle-window gate (unit, server).** `fleetapi` computes idle as
   `now − max(last frame, stream connect)`: a cell whose watcher just
   connected reports a small idle even when no frame ever arrived, an
   in-flight count > 0 reports busy, an unreported count is not
   reported as zero, and a frame resets the window. Mutation-verified:
   removing the connect-time floor makes the reconnect case fail.
6. **Composite gate (unit).** All conditions are judged against one
   snapshot: a scripted fleetd where `ready` and `idle` are true in
   *different* polls never unblocks; one where they are true together
   does.
7. **Lease-composition gate (unit).** The full primitive: batch A waits
   on `--idle --unleased`, unblocks, POSTs its lease; batch B with the
   same flags then does **not** unblock while A's lease is live; A's
   own re-run ignores its own holder. A lease POST that fails makes
   await exit non-zero.
8. **No-regression gate (unit).** C1's and C6's await tests pass
   unchanged in behaviour: plain `--up`, `--down`, the SSE fast path,
   and unknown-cell fail-fast.
9. **Role/route gate.** C10 adds no HTTP route and no MCP tool;
   `daemon/fleet_registry_test.go`'s probe list is unchanged and stays
   correct.
10. **Streaming-contract gate.** `git diff --stat main..HEAD --
    internal/vibe/proxy` is empty for the whole phase.
11. **Full inner loop** (ground rule 4) under `-race -count=5`, plus
    `golangci-lint run`, plus ground rule 9's adversarial self-review as
    its own commit.
12. **Live gates (need real hardware; NOT RUN here).**
    a. On the heavy cell: `vibe cell await gpu-cell --model <heavy>
       --ready` returns only after the model reports ready, and the
       elapsed time matches the 6–10 minute cold start the phase exists
       for.
    b. `vibe cell await gpu-cell --idle 2m` while a chat is in progress
       stays blocked, and unblocks 2 minutes after the last token —
       which also verifies that llama-swap v239 emits an inflight
       remove-edge frame the fold can see.
    c. The no-observation case: a cell fleetd holds no live
       `/api/events` stream to prints the no-evidence line and never
       unblocks on `--idle`.
    d. The full primitive end to end on two shells: B waits for A's
       lease to expire.

## Out of scope

- **Idle for unwatched cells from C7a's usage counters.** The
  cumulative per-model token totals on each announce *are* an activity
  signal, at one-heartbeat resolution, for exactly the cells that have
  no events stream. It is the obvious next evidence channel and it is
  not this phase: the cursors are the ledger's, a second consumer of
  them needs its own design, and a 15-second-resolution idle window
  quietly behaving differently from a live one is worse than a refusal
  that says so.
- **A fleetd-side wait registry.** C9 already rejected storing "someone
  is waiting" fleet-side, for the reason that still holds: the waiting
  process exists and already knows.
- **`--idle` in the degraded direct-probe fallback.** With fleetd down
  there is no inflight fold to read, and a direct `/running` poll from
  the operator's laptop measures residency, not activity. The fallback
  keeps answering only what it can.
- **Per-model idle.** §3 — the contended resource is the cell.
- **Blocking leases.** C2 said advisory and meant it; `--unleased` is a
  waiter's opt-in, not a lock.
- **`--until-exit`, WoL, resume-on-idle, or anything that ACTS.** await
  waits and reports; the only thing it writes is the lease it was
  explicitly asked to take.
- **Anything on the data plane.** No proxy changes, no new hop.

Estimated ~450 lines + tests. The plan's calibration (C0–C4 ran 3.6–4.5×
their estimates) applies.

## Merge order

C9 (PR #28) was open, not merged, when this branch was cut, so C10
branches off `main` at `c9e8bcf` and does not build on C9. Two
consequences, both deliberate:

- `vibe cell await --notify` (C9) and these flags are independent; the
  merge will conflict textually in `cellAwaitCmd`/`awaitCell` and the
  resolution is mechanical (both add flags to the same command).
- The lease POST uses its own small helper rather than C9's
  `fleetdTarget.postFleet`, which does not exist on `main`. If both
  land, fold `acquireLease` onto `postFleet` — the duplication is a
  merge-order artifact, not a design position.

**What happened:** C9 merged first (#28). Both consequences played out
as written — 4 files, 9 hunks, one of them code — and the resolution is
recorded in [§Merging C9](#merging-c9-2026-08-04). It was mechanical
except in one place, which the section argues rather than asserts: the
two phases each appended a step to the END of the same wait, and only a
human can say which step goes last.

## Execution (2026-08-04)

### What shipped

| piece | where |
|---|---|
| the activity block (`CellActivity`, `activityFor`) | `internal/vibe/fleetapi/activity.go` (new file) |
| the two inputs it needed: per-cell frame stamp, stream-connect stamp | `fleetapi/watcher.go` (`trackInFlight`, `setCellUp`), `Server.lastInFlightFrame` / `.cellUpSince` |
| `CellSnapshot.Activity`, emitted for every cell on every snapshot | `fleetapi/fleetapi.go` |
| the conditions, their validation, the lease claim | `internal/vibe/cli/cmd_cell_await.go` (new file) |
| the one-snapshot wait loop + the events fast-path narrowing | `cli/cmd_cell.go` (`awaitCell`, `cellAwaitCmd`) |

Four things the doc did not spell out and the code had to decide:

- **The busy check must come before the window, and it short-circuits.**
  `activityFor` returns `idle_s = 0` with a reason the moment a reported
  in-flight count is non-zero. This also silently invalidated the first
  version of the connect-floor test, which set up an in-flight request
  and therefore never reached the floor at all — see the review addendum.
- **A dropped stream deletes `cellUpSince`** rather than keeping it as
  history. The field is only ever read as "the window I can vouch for",
  and a stale one is a claim about a channel that is gone.
- **`LastRequest` survives the drop**; the window does not. When the
  stream is down the block reports the last frame it saw and no
  `idle_s` — history is still worth showing, it just cannot be a gate.
- **`--up` and `--down` together is now an error.** The old code let
  `--down` silently win. With five more flags on the command, a silently
  resolved contradiction is one more way to wait for the wrong thing.

### Gates

| gate | result |
|---|---|
| 1. Ready | **PASS** — `TestCellAwaitReady_BlocksUntilLlamaSwapReportsTheModelReady` (poll-counted, so an early return is a failure, not a coincidence); mutation-verified (`State != "stopped"` counts as ready → fails on poll 1) |
| 2. Unknown model | **PASS** — `TestCellAwaitReady_UnknownModelOnAReachableCellFailsFast`, `TestCellAwaitReady_AnEmptyOrUnreachableCatalogIsNotAnUnknownModel` (drained + absent), `TestCellAwaitReady_TransportErrorsKeepRetrying`; mutation-verified (dropping the reachable/non-empty qualification fails both sub-cases) |
| 3. Front | **PASS** — `TestAwaitFlagsRejectWaitsThatCouldNeverEnd/front_cannot_report_a_peer_ready`, plus every other never-ending or contradictory combination in the same table |
| 4. Idle evidence | **PASS** — `TestCellAwaitIdle_UnblocksOnObservedSilence` (positive control), `TestCellAwaitIdle_MissingEvidenceNeverUnblocksAndSaysWhy` (timeout error names the missing evidence, stdout carries the refusal), `TestCellAwaitIdle_InFlightRequestsAreNotIdleEvenPastTheWindow`; mutation-verified (treating `!Observed` as idle unblocks and fails) |
| 5. Idle window (server) | **PASS** — `TestActivity_WithoutALiveStreamReportsNoEvidenceAndNoIdleWindow`, `TestActivity_StreamDropRetiresTheObservationWindow`, `TestActivity_IdleIsFlooredAtTheStreamConnectNotTheFrameHistory`, `TestActivity_IdleGrowsFromTheLastFrameOnceItIsInsideTheWindow`, `TestActivity_AnyFrameIsActivityIncludingTheCompletionEdge`, `TestActivity_UnreportedInFlightIsNotAReportedZero`, `TestActivity_InFlightRequestsReportBusyNotIdle`, `TestSnapshotAlwaysCarriesAnActivityBlock`; mutation-verified four ways (drop the connect floor, drop the `Observed` gate, ignore the in-flight count, stop stamping frames) |
| 6. Composite | **PASS** — `TestCellAwaitComposite_ConditionsMustHoldInTheSameSnapshot` (alternating polls, each condition true half the time, never together), `TestAwaitEvaluate_JudgesEveryConditionAgainstOneSnapshot`, `TestCellAwaitExtras_ATransitionEventDoesNotSatisfyAModelCondition`; the last is mutation-verified (removing `!cond.extras()` from the events fast-path unblocks on `fleet.cellUp` with the model still starting) |
| 7. Lease composition | **PASS** — `TestCellAwaitLeases_OtherHoldersBlockOwnHolderDoesNotAndSuccessClaims` drives the real cobra command through all three phases (blocked by another holder → own residue ignored → the claim it takes blocks the next batch), `TestCellAwaitLease_ARefusedClaimFailsTheCommand`; mutation-verified (not skipping our own holder deadlocks the re-run) |
| 8. No regression | **PASS** — C1/C6's `TestCellAwaitUnblocksOnTransition`, `TestCellAwaitViaEventsStream`, `TestCellAwaitUnknownCellFailsFast`, `TestCellAwaitDown` pass with only their call sites updated for the struct argument; `TestCellAwaitCmd_PlainUpIsUnchanged` pins the old output line verbatim through the real command |
| 9. Role/route | **PASS** — no HTTP route and no MCP tool added; `daemon/fleet_registry_test.go`'s probe list is untouched and still complete |
| 10. Streaming contract | **PASS** — `git diff --stat main..HEAD -- internal/vibe/proxy` is empty |
| 11. Inner loop | **PASS** — `go build ./...`, `go vet ./...`, `gofmt -l .` (silent), `go mod tidy` (`git diff --exit-code` clean), `golangci-lint run` (0 issues), `go test -race -count=5 ./...` (exit 0, 27 packages ok, no DATA RACE), re-run end to end after the review commit |
| 12. C9 union (`--notify` beside the lease claim) | added by the merge — see [§Merging C9](#merging-c9-2026-08-04) |
| 13. Live gates (a–d) | **NOT RUN** — no route to the fleet's hardware from the implementing environment. No transcripts are fabricated |

### Adversarial self-review (ground rule 9)

Landed as its own commit against the feature commit. Six findings, five
fixed with a mutation-verified regression test, one documented.

1. **A hollow test passed a gate (major, and the gate was mine).**
   `TestActivity_IdleIsFlooredAtTheStreamConnectNotTheFrameHistory`
   set up an in-flight request before backdating, so `activityFor`
   short-circuited to the busy branch and the test never reached the
   floor it is named for — removing the floor entirely left it green.
   This is ground rule 10 exactly: a test's name is part of its
   assertion. Fixed by completing the request first; the mutation now
   fails it (`idle_s = 3600`).
2. **The timeout error blamed the cell for the registry (major).**
   With fleetd unreachable for the whole wait there are no unmet
   conditions to report, so a `--model --ready --idle` wait timed out
   saying "the cell never came up" — about a cell it had never
   successfully asked about. The last real error is now carried and
   named. The subtlety that made this a two-round fix: the error from
   the poll that was interrupted BY the timeout is
   `context deadline exceeded`, which would have replaced every real
   cause with a restatement of the timeout, so an error is only recorded
   while `ctx.Err() == nil`.
   `TestCellAwaitTimeout_NamesTheRegistryFailureNotTheCell`,
   mutation-verified both ways.
3. **A pre-C10 fleetd was reported as a cell with no evidence (minor).**
   Version skew is guaranteed in this fleet (C3's announce rule) and an
   older fleetd simply sends no `activity` field. "No activity evidence
   for this cell" sends the operator to look at the cell instead of the
   registry; the message now names the skew.
   `TestCellAwaitIdle_AnOlderFleetdSendsNoActivityBlockAndSaysSo`.
4. **`idle_s` off the wire could overflow into a random duration
   (minor).** `time.Duration(f * float64(time.Second))` is
   implementation-defined for an out-of-range float, and the mutation
   test shows what that actually produced: `-2562047h47m16s` from
   `1e30`, i.e. a garbled number turning into a *negative* window rather
   than an obviously wrong one. Clamped to [0, 100 years] before the
   conversion. `TestCellAwaitIdle_AnAbsurdIdleFromTheWireIsClamped`.
5. **The events goroutine outlived a successful `--timeout 0` wait
   (minor).** Only the timeout path derived a cancellable context, so
   the overnight idiom — the one invocation guaranteed to use
   `--timeout 0` — returned with its SSE connection still open until the
   caller's context died. Harmless in a process about to exit and a leak
   anywhere else. `TestCellAwaitReleasesItsEventStreamOnSuccess`
   (mutation-verified: restoring the old shape hangs it).
6. **A lease's MODEL is deliberately not compared** — documented, not
   changed. `--unleased` blocks on any holder of the CELL, for the same
   reason `--idle` is cell-scoped: the contended resource is the GPU.

**Verified sound, not changed:** the one-snapshot evaluation, the
events fast-path narrowing, the busy-before-window ordering, the
unknown-model qualification and the own-holder skip each have a named
test that fails when the production line is broken (nine mutations run
in total); no lock is held across an HTTP call; `activityFor` takes
`s.mu` once and builds outside it; `git diff --stat main..HEAD --
internal/vibe/proxy` is empty.

**One out-of-phase fix, recorded because it is not this phase's code.**
CI failed once on `TestRenderLoopUnchangedNoWrite` (C3-era,
`render_loop_test.go`) with `RenderCount = 1 after changed render, want
2` on a run whose own log printed `renders=2` — the probe's `writeFile`
seam signals the test BEFORE `renderPass` increments the counter, so
`waitWrite` can return one instruction ahead of it. It is a race in the
TEST, not in the render loop, and it reproduces on nothing this phase
touched (60 local `-race` iterations were green before the fix, which is
how a flake this narrow gets missed). Fixed with a polling
`waitRenderCount` helper at the two sites that read the counter
immediately after a write; the assertions that a write must NOT have
happened stay immediate, because they already follow `assertSilent`.

**Known and accepted (documented, not fixed):**

- **A warm counts as activity.** C4's warm-target restore and
  warm-schedule fires are real metered requests (C7a counts them as
  `poke_req`), so a scheduled warm pushes `--idle` out by a full window.
  That is correct — the GPU genuinely just did work — but an operator
  whose `warm_schedule` fires every 15 minutes cannot use `--idle 20m`,
  and nothing warns about the interaction.
- **`--idle front` measures whatever the front's own inflight frames
  cover.** The front proxies to peers, and whether llama-swap reports
  proxied requests as in-flight there is unverified. `--idle` against
  the front is not refused (unlike `--ready`, which cannot ever be true
  there), but do not read it as "the fleet is quiet" until gate (b)
  says what those frames contain.
- **The claim is not atomic.** Between the last evaluation and the
  lease POST there is one poll interval in which another batch can
  unblock on the same cell. Leases are advisory; a real mutex is a
  different feature and this fleet has one operator.
- **No `--idle` for cells fleetd does not watch.** §Out of scope names
  the usage-counter channel that could serve them; today they get a
  refusal with a reason, which is the correct failure.

### What the live gates would prove that the unit gates cannot

Two things, and they are the two assumptions the design rests on.

**Whether llama-swap's inflight frames are really edges.** Every unit
test drives `trackInFlight` directly, so they prove what the fold does
with a frame, never that v239 sends one when a generation ends. If it
turns out inflight frames are periodic rather than edge-driven, `--idle`
never fires; if the completion edge is missing, `--idle` fires one
window after a request STARTS. Gate (b) is the one that answers it, and
it is worth running before anything trusts `--idle` overnight.

**Whether a request already running at stream-connect is visible.** The
connect-time floor bounds this to one window, and a completion frame
closes it, but the case where fleetd reconnects mid-generation and the
generation outlives the window is real and untested against hardware.
The honest statement today is that `--idle` is safe against everything
fleetd has watched and bounded — not that it is safe against everything.

## Adversarial-review addendum (2026-08-04, 7 findings, all fixed with regression tests)

Ground rule 9's second pass, run against C10 **merged with `main` at
`e7adae4`** — C11 (`hold_model`) landed after this branch was cut and
shares the lease store `--lease`/`--unleased` write and read. Landed as
its own `review:` commit. Every fix below is mutation-verified: the
production change was reverted and the named test was watched to FAIL,
then restored (twelve mutations in total, including re-running five of
the first pass's to confirm the refactor did not hollow them out).

- **A parked wait re-polled `/api/fleet/state` once per SSE frame from
  anywhere in the fleet (MAJOR).** `handleUpstream` publishes EVERY
  upstream llama-swap payload — `logData`, `modelStatus`, `metrics` —
  from EVERY cell onto `/api/fleet/events`, and the wait loop fell
  through to a fresh poll on any frame it did not return on.
  `/api/fleet/state` is an uncached probe round (`Snapshot` singleflights
  concurrent callers but caches nothing), so each frame cost two HTTP
  GETs per cell — against the same llama-swaps serving the traffic that
  generated the frames. Harmless for C1's `--up`, which usually returns
  on its first poll; C10 makes the long wait the *point*, so an
  overnight `--idle` turns a generation on any cell into a probe flood.
  A frame is now a reason to re-evaluate only when it is a transition
  for THIS cell, which is what §5 already said.
  `TestCellAwaitDoesNotRePollOnUnrelatedEvents`.
- **`--down` unblocked on `fleet.cellStale` against a cell that was
  still answering (MAJOR, C6's finding on the path C6 did not reach).**
  Staleness retires the ANNOUNCE, never the probe — `presence.go`'s
  not-fresh branch sets `snap.Reachable = probeOK` precisely so a cell
  whose announcer died while llama-swap keeps serving stays reachable —
  but the events fast-path took `fleet.cellStale` as a verdict and
  printed `gpu-cell is down (fleet.cellStale)` for exactly that cell.
  `fleet.cellWithdrawn` is the same shape (C6 gates the withdraw's
  `HostReachable=false` on the probe failing too), and `fleet.cellUp`/
  `fleet.cellDown` are the events STREAM, not the two GETs `Reachable`
  is defined by. **The snapshot is now the only verdict**; a transition
  triggers an immediate re-poll, which is what preserved C3's sub-second
  unblock in the first place. `TestCellAwaitDown_AStaleAnnouncerIsNotADownCell`
  (both event types). C3's `TestCellAwaitViaEventsStream` needed its
  fixture made self-consistent — it emitted `fleet.cellReturned` from a
  state document that said the cell was NOT reachable, i.e. it was
  asserting that the event alone could return.
- **`--lease` accepted C11's reserved holder, and only failed after the
  wait (MAJOR).** The claim is POSTed when the wait succeeds, so under
  `--timeout 0` — the documented overnight idiom — a batch parks all
  night, unblocks at 03:00 and exits non-zero on a `400` that was
  decidable before the first poll. `hold` is worse than late: C11
  reserves that holder, and `--unleased` skips its OWN holder, so
  `--unleased --lease hold` would step over the operator's do-not-touch
  declaration on the way to failing. `validateAwaitFlags` now refuses
  the reserved holder (pointing at `vibe cell hold`), an over-bound
  `--lease-ttl` (`fleetapi.MaxLeaseTTL`, exported for this), and a
  control character in `--model`/`--lease`/`--lease-note` — every
  refusal fleetd would issue, issued before the wait starts.
  `TestCellAwaitLease_ReservedHolderAndOverBoundTTLAreRefusedBeforeTheWait`
  asserts **zero** state polls happened.
- **The evidence gap was dropped on the success path (MAJOR).** §3 rule
  2 promises the unreported-in-flight qualification is "surfaced in the
  status line either way", and it was not: `activityFor` sets
  `Reason = "no inflight frame seen yet; idle measured from the stream
  connect"`, and `evalIdle` printed `idle 15m0s (>= 10m0s)` and threw it
  away. That is the one reading of `--idle` resting on the cell's
  SILENCE rather than on an edge fleetd watched, so if live gate (b)
  finds v239 does not emit the frames the fold needs, the failure mode
  is a silent green light — the M2 trap with nothing on screen to catch
  it. fleetd's qualification now rides both the progress and the success
  line. `TestCellAwaitIdle_ASatisfiedWindowStillNamesTheEvidenceGap`.
- **A pre-connect in-flight count was reported as a live one (MINOR).**
  `s.inFlight[cell]` survives a stream drop; the knowledge does not. A
  request that finished while fleetd was disconnected takes its remove
  edge with it, so after a reconnect the cell reports `in_flight: 1`,
  `idle_s: 0`, "1 request(s) in flight" — a claim about now built from a
  frame predating the observation window this phase exists to enforce,
  and one that never resolves, because the frame that would clear it
  only arrives when someone issues a NEW request. `--idle` therefore
  parked forever on a genuinely idle cell, with a false reason. It still
  refuses to unblock — fleetd honestly does not know, and that is the
  fail-closed direction — but says so as the missing evidence it is, and
  self-heals on the next frame. This is the connect floor pointed the
  other way. `TestActivity_APreConnectInFlightCountIsNotEvidenceAboutNow`
  plus `TestActivity_AnInFlightCountInsideTheWindowStillReportsBusy` as
  the positive control.
- **"Print only on change" changed nothing (MINOR, and the test said
  otherwise).** The progress line is deduplicated on its rendered text,
  which embeds a second-resolution idle counter that moves on every
  poll — so against a real fleetd it deduplicates nothing and an
  overnight `--timeout 0 --idle` wait logs one line per poll for eight
  hours. The original test froze `idle_s` and so could never see it
  (ground rule 10 again). `condResult` grew a stable `key` and the loop
  deduplicates on that. `TestCellAwaitProgressSurvivesAMovingIdleCounter`
  drives a MOVING counter and asserts one line.
- **A C11 hold rendered as a consumer with an odd name (MINOR).**
  `--unleased` printed `leased by hold`. C11's rule for surfaces that
  carry no hold flag is to key on the reserved holder; `evalLeases` now
  does, and prints `held: <model>`.
  `TestCellAwaitUnleased_AHoldIsNamedAsAHold`.

**Verified sound, not changed.** `--timeout 0` is still wait-forever and
the flag's default is now pinned by a test (`DefValue == "0s"`). The
unknown-model fail-fast still requires a REACHABLE cell with a NON-EMPTY
catalog, transport errors still retry, and the one-snapshot rule, the
connect floor, the `Observed` gate, the cell-level frame stamp and the
own-holder skip each still fail a named test when their production line
is broken. `--idle` deliberately does NOT consult a C11 hold: a hold
suspends what **fleetd** initiates, and an operator's batch is not
fleetd guessing (C11 §"what it does NOT touch") — `--unleased` is how a
batch respects one, and it now names it.

**Known and accepted, added by this pass.** A 4xx from fleetd's `/state`
(a stale token, an `--api` pointed at a daemon without the fleetd role)
retries forever under `--timeout 0` rather than failing fast like an
unknown cell. It is visible — every attempt prints `(retrying)` with the
status — and narrowing it risks the 502/503-while-restarting case the
retry loop exists for, so it is recorded rather than changed.

### Gates, re-run after the review commit

Gates 1–11 **PASS** on the merged tree; the live gates (a–d) remain
**NOT RUN** — no route to the fleet's hardware from the reviewing
environment either, and no transcripts are fabricated. Full inner loop:
`go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy`
clean, `golangci-lint run` **0 issues**, `go test -race -count=5 ./...`
**exit 0, 27 packages ok, no DATA RACE**. Gate 10 re-checked against the
merge base: `git diff --stat main..HEAD -- internal/vibe/proxy` is empty.

## Merging C9 (2026-08-04)

C9 (`vibe fleet notify`) merged to `main` as #28 after this branch's
adversarial pass finished. Both phases extended `vibe cell await`, so
the merge conflicted in 4 files / 9 hunks — `cli/cmd_cell.go` (4),
`AGENTS.md` (1), the plan README (3), the design doc's status line (1).
**Both feature sets survive**; this is a union, not a choice. Eight of
the nine hunks were textual. The ninth is the decision below.

### The decision: `--notify` fires AFTER the lease claim

C10's flow is *wait → claim the lease → report*, and a REFUSED claim
fails the command (a batch that runs undeclared is invisible to the
pre-drain report that exists to protect it). C9's is *wait → push one
message*. Merged, three orderings were available:

1. push before the claim,
2. push after the claim, only on success,
3. push after the claim, always, carrying its outcome.

**Chosen: 3.** Argue it from what "await-unblocked" means to the person
it is for — a human parked on a cell, away from the terminal, whose
phone is the only surface they will read. Their question when it buzzes
is not "did the wait end". It is **"is the box mine"**:

- Ordering 1 answers a question they did not ask, and can answer it
  wrongly: the page says `gpu-cell is up` while the lease went to
  someone else and the command exited non-zero. A page that has to be
  re-checked against a terminal is a page that failed.
- Ordering 2 answers the refusal with **silence**, which reads as *no
  news, still waiting* — the single worst reading, because it is
  indistinguishable from the wait not having ended and it is the one
  someone goes to bed on. Failure is exactly when the message matters
  most; the notifier that only pages on success is C9's own
  "learns-to-swipe-it-away" failure inverted.
- Ordering 3 costs one bounded HTTP POST of latency (10 s client
  timeout) on a wait usually measured in hours, and buys a message that
  is true when it arrives.

So: **exactly one push, sent last, naming the outcome.** On a refusal
the title reads `… is up, but the lease was refused`, the body carries
fleetd's own refusal and `the command exited non-zero and nothing is
holding <cell>`, **and the command still fails** — C10's rule is
unchanged; the push reports the failure rather than softening it.

Two consequences worth keeping:

- **`--notify` still fires only on an UNBLOCKED wait** (C9's rule,
  untouched): a timeout and a fail-fast typo push nothing. The wait
  ending is the event; the lease outcome rides along because it is the
  same moment.
- **The push repeats the terminal's success line verbatim.**
  `awaitSuccessLine` is now the one renderer and both surfaces call it,
  the same reason `condResult` exists. C10's rule that a qualification
  fleetd attached to the evidence rides the SUCCESS line is worth least
  precisely where it gets dropped — and the phone is the surface read by
  the operator who is not watching the terminal. `awaitCell` therefore
  RETURNS the satisfied `awaitEval` instead of only printing it.

### The rest of the union

- `cellAwaitCmd`: both flag sets, both help paragraphs. `--notify`
  needs no new validation and composes with every condition.
- `acquireLease` folded onto C9's `fleetdTarget.postFleet` exactly as
  §Merge order predicted — same twenty lines, same error text
  (`POST /api/fleet/lease: HTTP …`). The RunE's context boilerplate
  became C9's `cmdContext`.
- The push's payload is clamped and made printable before it is sent
  (`notifyText` / `clampBytes`, budgets under fleetd's 256-byte title
  and 2000-byte message). It quotes a refusal body fleetd wrote — up to
  4 KB of it — and fleetd 400s a title with a control character in it. A
  `--notify` that 400s is a human who never gets paged.
- **Re-verified, still true:** `--lease hold` is refused BEFORE the wait
  starts (`validateAwaitFlags` runs in RunE ahead of `resolveFleetd` and
  `awaitCell`; `TestCellAwaitLease_ReservedHolderAndOverBoundTTLAreRefusedBeforeTheWait`
  asserts no HTTP call is made). The merge moved nothing on that path.

### Gates, re-run after the C9 merge

Gates 1–11 **PASS** on the merged tree, plus one new gate for the union;
the live gates (now numbered 13) remain **NOT RUN** — still no route to
the fleet's hardware, and no transcripts are fabricated.

| gate | result |
|---|---|
| 12. C9 union | **PASS** — `TestCellAwaitNotify_FiresAfterTheLeaseClaimAndCarriesItsOutcome` (ordered route log `[lease notify]`, message = the terminal's line verbatim including fleetd's evidence qualification, plus the lease line), `TestCellAwaitNotify_ARefusedLeaseStillPagesAndStillFailsTheCommand`, `TestCellAwaitNotify_APlainWaitPagesOnceWithNoLeaseLine`, `TestCellAwaitNotify_AFailedWaitPagesNothing` (timeout and fail-fast typo), `TestCellAwaitNotify_APushFailureWarnsAndKeepsTheExitCode`, `TestNotifyPayloadIsBoundedAndPrintable`; mutation-verified three ways — pushing before the claim, skipping the push on a refusal, and building the message independently of `awaitSuccessLine` each fail a named test |
| 13. Live gates (a–d) | **NOT RUN** — no route to the fleet's hardware from the implementing environment |

Full inner loop on the merged tree: `go build ./...`, `go vet ./...`,
`gofmt -l .` silent, `go mod tidy` clean, `golangci-lint run`
**0 issues**, `go test -race -count=5 ./...` **exit 0, no DATA RACE**.
Gate 10 re-checked against the new merge base:
`git diff --stat origin/main...HEAD -- internal/vibe/proxy` is empty.
