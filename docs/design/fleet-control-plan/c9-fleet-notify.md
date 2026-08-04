# C9 — `vibe fleet notify`: the alarm column, delivered

Status: EXECUTED + REVIEWED, PR #28 OPEN — not merged (2026-08-04), off
`feat/c9-fleet-notify`, branched off `main` at `c9e8bcf` (C8 merged).
Every mechanically verifiable gate is green under `-race -count=5`; the
four live gates need real hardware and are **NOT RUN** — see
[§Execution](#execution-2026-08-04). Second v2-backlog item to land
([futures](../fleet-control-futures.md) §2 item 2). Depends on C3
(presence), C4 (the render loop's fingerprint pass), C2 (leases) and
C8 only for the one alarm kind that is deliberately OFF by default.

The design's class table has an **alarm? yes** column
([fleet-control.md](../fleet-control.md) §4) and nothing reads it. The
`fleet.cellStale` event is published to an SSE stream whose only
consumers are a web page nobody has open and a `vibe cell await` that is
waiting for something else. This phase gives that column a destination.

## The finding that shapes this phase

**The alarm column cannot be expressed as an event forward.** The
futures entry sketches "SSE-events-to-webhook bridge", and the first
hour of reading the substrate says that shape is wrong for the policy it
is supposed to deliver:

| default alarm | what the event stream actually offers |
|---|---|
| always-on staleness | `fleet.cellStale` — fires once per cell, correctly, but says nothing about the cell's CLASS or its declared intent. A drained always_on cell emits the identical frame. |
| persistent fingerprint mismatch | `fleet.fingerprintMismatch` fires **once and then goes silent forever**. It is published from `renderLoop.renderPass`, which runs only on a render trigger; the trigger for drift is `modelFingerprintChanged`, which compares the hash against the PREVIOUS announce. A cell that keeps announcing the same wrong hash changes nothing, triggers nothing, renders nothing, and emits nothing. The most persistent mismatch possible produces exactly one frame. |
| drain-with-active-lease | **has no event at all.** `SetIntent` publishes nothing, and the announce-echo path that records a drain performed at the box publishes nothing either. |
| await-unblocked | `fleet.cellReturned` exists, but the interesting question is not "did something return" — it is "did the thing I was told was broken get better". |

So: events are edges, alarms are conditions, and two of the four
conditions have no edge. A forwarder would page on flap (every edge is a
page), page on the wrong cells (no class filter), miss the fingerprint
alarm after its first minute, and be structurally unable to see the
lease one.

**C9 is therefore a state differ, not an event bridge.** It evaluates
alarm CONDITIONS against `Server.Snapshot` — the same derived document
the fleet page and `vibe cell status` render — on a ticker, and runs a
dwell/dedup state machine over the result. Two consequences fall out for
free and both are load-bearing:

- **The pager and the page can never disagree.** The absence alarm is
  literally `Display ∈ {DRAINED?, OFF/AWAY, OFF/AWAY?}`, read off the
  same `displayState` the human reads. There is no second derivation of
  "is this cell absent" to drift from the first.
- **Dwell on both edges is what kills flap**, and dwell is only
  definable on a condition. An edge forwarder has nothing to hold.

The SSE stream is untouched and stays what the page and `vibe cell
await` consume. The evaluation tick (30 s) is not subscribed to it: with
a 2-minute dwell the event would advance the alarm by at most 30 s, and
the two conditions that matter most are not on the stream anyway.

## Design

### 1. Ownership: where an alarm is allowed to read from

C9 adds **no fourth state axis**. Every condition is a pure function of
the existing three as already resolved into `CellSnapshot`:

| alarm kind | condition, in terms of the snapshot |
|---|---|
| `cell_absent` | `Class == always_on` **and** `Display ∈ {DRAINED?, OFF/AWAY, OFF/AWAY?}` |
| `fingerprint_drift` | the (cell, model) is in the render loop's mismatch set |
| `drain_with_lease` | `Intent.State == "drained"` **and** `len(Leases) > 0` |
| `model_degraded` | any model's `Probe.Verdict == degraded` — **available, OFF by default** |

Three rules govern the reads:

- **Declared intent may SUPPRESS an alarm; inferred intent may not do
  anything.** An always_on cell that is absent with a declared drain
  (`DRAINED`, or `OFF` — "it was drained first") is explained, and
  paging on an explanation is how a notifier gets muted. An always_on
  cell absent with NO intent entry is `DRAINED?` — the design's "deliberate
  stop or crash loop" — and that is precisely the alarm. Note the
  direction carefully: `DRAINED?` is read as *absence with no
  explanation*, which is a fact about the intent store, not a guess at
  what the operator meant. Nothing here acts on a guess, and nothing
  here acts at all — the only effect is an outbound message.
- **`INCONSISTENT` is not an alarm.** The cell answers; the fleet is
  serving; the design already calls it a nag. Paging on a nag trains the
  operator to ignore the pager.
- **A degraded model is not an alarm by default**, for exactly the
  reason C8 refused to withdraw a degraded model from the render: the
  measurement has a false-positive tail, and the class table does not
  list it.

### 2. The default policy IS the class table, and nothing else

`alarms:` defaults to `[cell_absent, fingerprint_drift,
drain_with_lease]`. `model_degraded` is implemented, documented, and
absent from the default list. The class filter on `cell_absent` is the
alarm column verbatim: `always_on` = yes, `opportunistic` = no,
`roaming` = no. A laptop closing its lid at 23:00 every night must
produce zero notifications forever, or the notifier is worse than
nothing — item 2's whole premise is that an unread channel is the
failure mode, and a channel you have learned to swipe away is an unread
channel.

A gate names this: the default set is asserted to be exactly those
three, and a roaming cell taken through withdraw → stale → return
produces no notification while an always_on cell taken through the same
sequence produces one.

### 3. Persistence: a threshold, not an event

Every alarm kind carries a **fire dwell** and every alarm carries a
shared **clear dwell**. The per-key state machine:

```
idle ──cond true──▶ pending ──true for fire_dwell──▶ FIRE ──▶ active
  ▲                    │                                        │
  └──── cond false ────┘                              cond false│
                                                                ▼
idle ◀── false for clear_dwell ── RESOLVE ◀──────────────── clearing
                                                  cond true ──▶ active
```

Defaults: `cell_absent` 2 m, `fingerprint_drift` 15 m,
`drain_with_lease` 0 (a stranded batch job is being truncated right now;
a two-minute-late page is a bereavement notice), `model_degraded` 10 m.
Clear dwell 1 m for all.

**Persistent fingerprint mismatch is defined as: present in the render
loop's mismatch set continuously for 15 minutes.** That requires a set,
because as established above the event fires once. `renderPass` now
rebuilds `Server.fpMismatch` from each pass — the pass IS the
evaluation, so a rebuild is the honest update — preserving `FirstSeen`
for surviving entries. Resolution is always triggered: a changed hash
fires `modelFingerprintChanged` → render trigger → pass → the entry
disappears.

**The dwell is only as trustworthy as the evaluator behind it, so the
status names the evaluator.** The render loop starts only when
`fleet.front_config` is set. Without it there are no passes, the
mismatch set is permanently empty, and this alarm can never fire —
so `notify.fingerprint_source` reports `unavailable (no
fleet.front_config: the render loop that verifies fingerprints is not
running)` rather than letting a silent zero read as "no drift". This is
C5's rule — a guard that cannot be evaluated says so — applied to an
alarm.

### 4. Coalescing: what stops 200 notifications

Three mechanisms, each with one job:

1. **Dwell on both edges.** A cell flapping with a period shorter than
   the fire dwell never leaves `pending`, so it produces **zero**
   notifications — not one per flap, not one per cycle. This is the
   primary answer and it is the one that is tested with a literal 200
   transitions.
2. **Active keys never re-fire.** One notification per (key,
   transition). An alarm that has fired stays `active`, silently, until
   it resolves. Nothing repeats, ever — there is no re-notify interval,
   because a re-notify interval is a pager that goes off all night for a
   box you already know about, and `fleet_status` is where "still
   broken" is answered.
3. **A token bucket as the absolute backstop** (default 12/hour, burst
   4). Anything the first two let through is paced by it. A
   rate-limited notification is **deferred, not dropped**: it sits in a
   bounded (32) dedup-by-key queue and goes out when a token is
   available. Dropping would make the bucket a shredder for exactly the
   alarms that arrive in a storm.

Keys are `kind + "\x00" + scope` (`cell_absent\x00gpu-cell`,
`fingerprint_drift\x00gpu-cell\x00bge-m3`). Notification identity is the
key, so a re-worded detail string never re-notifies.

**Not persisted.** A fleetd restart re-fires every still-true alarm once
its dwell elapses again. That is arguably correct (the problem is still
happening and you have a new fleetd), and a crash loop is bounded by the
token bucket. Persisting it would add a store whose staleness could
suppress a real alarm — the worse failure.

### 5. away/home: a fleet-scope declared intent that cannot go quiet forever

Vacation is the case where every alarm is both true and useless. The
declaration is:

```json
{"scope":"away","since":"2026-08-10T12:00:00Z","until":"2026-08-24T00:00:00Z",
 "reason":"vacation","by":"kyle"}
```

at `$XDG_STATE_HOME/vibe/fleet/notify-scope.json`, beside `intent.json`
and `leases.json`.

**Why not in `intent.json`.** That file is keyed by cell name and every
reader — `resolveIntent`, `decorate`, the warm loops, `probeGuard` —
treats a key as a cell that announces, echoes, and can be reconciled
against. A `__fleet__` pseudo-key would be a cell that never echoes, so
the C3 conflict rule would hold a permanent pending request against it.
Own file, own reader, own noun.

**Why this does not violate the ownership axes.** It is axis 2
(declared), and it declares a fact about the OPERATOR, not about a cell:
*"messages sent to me will not be read."* It is consumed by exactly one
thing — notification delivery. It changes no cell's state, no display,
no routing, no render, no warm. The name is `notify.scope`, not
`fleet.state`, so a future phase cannot mistake it for a fleet-level
DRAIN and start acting on it.

Four rules keep "away" from becoming a silent mute:

1. **Away suppresses DELIVERY, never EVALUATION.** Alarms fire into the
   state machine exactly as at home. `fleet_status`'s `notify` block
   shows every active alarm, the scope, and a `suppressed` count with
   the suppressed keys.
2. **Coming home sends one digest**, naming what was suppressed and what
   is still active — not a burst of eleven notifications at the airport.
   The digest is the reason suppression is deferral rather than deletion.
3. **`until` expires by itself**, evaluated lazily at read (leases'
   precedent). The expiry triggers the same digest.
4. **An explicit request is never suppressed.** `vibe fleet notify test`
   and `vibe cell await --notify` are a human asking for a message right
   now; the away gate applies to ALARMS only. Otherwise the one command
   that proves the pager works is the one command that silently does
   nothing while you are away — which is when you would most want to
   check.

### 6. Delivery: stdlib, bounded, and unable to wedge the daemon

`internal/vibe/fleetnotify` holds a `Sink` interface and one
implementation: `WebhookSink`, an `net/http` POST.

- **Two body formats.** `text` (default) posts the human-readable
  message as the body with ntfy's native `Title` / `Priority` / `Tags`
  headers — the shape that renders as a readable phone notification.
  `json` posts a structured document for generic webhook consumers.
  Header values are stripped of non-printables and length-capped before
  they are set: a control character in a cell name would otherwise make
  Go's transport reject the request, turning a hostile announce into a
  muted pager.
- **Bounded retry.** 4 attempts, 1 s → 2 s → 4 s backoff, 10 s per
  attempt. **A 4xx is the far side answering** (C6's rule, verbatim from
  the piggyback fallback): a bad topic or a rotated token is a permanent
  failure, counted and surfaced, never retried.
- **It cannot block anything.** Delivery runs on one worker goroutine
  behind a bounded (64) channel; the evaluator's enqueue is
  non-blocking and counts drops. Every request and every backoff sleep
  is bound to a context cancelled by `s.done`, so `Close()` cannot be
  held hostage — C4's `warmCtx` rule, which exists because an unlinked
  timeout already blocked `Close()` once in this package.

### 7. Secrets: the URL is a credential and must not be printable

An ntfy topic URL is bearer-equivalent: whoever has it can publish to
your phone, and whoever has it can read the topic. Rules:

- **Config carries a path, not a value, by preference.**
  `fleet.notify.url_file` (and `token_file`) follow `fleet.token_file`'s
  existing convention. `url:` inline is supported and documented as
  acceptable only for a 0600 config.
- **It never reaches a log, a status document, an event or an error.**
  The status surface shows `Redact(url)` — scheme, host, `/…`, and eight
  hex of `sha256(url)` so two configs are distinguishable without either
  being reconstructable. Query strings and userinfo are dropped wholesale
  (plenty of webhooks put the token there).
- **`*url.Error` is the trap.** `http.Client.Do` returns an error whose
  `Error()` contains the full URL, so every failure path in this package
  unwraps it, and a final scrub replaces any surviving occurrence of the
  URL or its path with `<redacted>`. A gate greps a whole failing run —
  logs, status JSON, error strings — for the secret.
- **This repo ships no endpoint.** The documented example is
  `https://ntfy.example.invalid/vibe-fleet-EXAMPLE`; the real one is a
  private-fleet-repo value (ground rule 3).

### 8. Surfaces

| surface | what |
|---|---|
| config | `fleet.notify:` block (daemon `FleetConfig`) |
| status | `StateSnapshot.Notify` — scope, alarms, counters, redacted endpoint, fingerprint source |
| HTTP | `POST /api/fleet/notify/send` (explicit message), `POST /api/fleet/notify/scope` (away/home) — both fleetd-only |
| MCP | `fleet_notify_scope`, `fleet_notify_test` |
| CLI | `vibe fleet notify status \| test \| away \| home`, `vibe cell await --notify` |

The fleet page gets no new route and no new mutation endpoint (C5's
rule): the away/home toggle is a `tools/call` on `fleet_notify_scope`
like every other button.

## Acceptance gates

1. **Default-policy gate (unit).** The default alarm set is exactly
   `{cell_absent, fingerprint_drift, drain_with_lease}`. `model_degraded`
   is implemented and produces no notification under the default policy
   even with a degraded model in the snapshot.
2. **Class gate (unit).** An `always_on` cell going absent alarms after
   its dwell; `opportunistic` and `roaming` cells taken through the
   identical absence sequence produce nothing. Mutation-verified:
   deleting the class filter makes the roaming half fail.
3. **Declared-intent gate (unit).** An always_on cell absent with a
   declared drain (`DRAINED`, and `OFF`) does not alarm; the same cell
   absent with no intent entry (`DRAINED?`, `OFF/AWAY`, `OFF/AWAY?`)
   does. `INCONSISTENT` does not alarm.
4. **Flap gate (unit).** 200 absent/present transitions at a period
   below the fire dwell produce **zero** notifications. One continuous
   absence produces exactly one, and staying absent for an hour produces
   no second one.
5. **Persistence gate (unit).** A mismatch present for less than the
   fingerprint dwell notifies never; one that persists past it notifies
   exactly once; the render pass rebuilds the mismatch set so a resolved
   drift clears and (if it had fired) resolves. With no render loop
   running, the status names the missing evaluator instead of reporting
   zero drift.
6. **Lease gate (unit).** A drain recorded against a cell holding an
   active lease alarms immediately, naming the holder; a drain with no
   lease does not; lease expiry resolves it.
7. **Away gate (unit).** Away suppresses delivery while the alarm stays
   visible in `fleet_status` with a suppressed count; returning home
   emits exactly one digest naming the suppressed keys; an `until` that
   has passed behaves as home; an explicit send is delivered while away.
8. **Secret gate (unit, mutation-verified).** With a secret URL
   configured and a delivery that fails at the transport, the URL, its
   path and its token appear in **no** log line, **no** error string and
   **no** field of `/api/fleet/state`. Removing the scrub makes it fail.
9. **Shutdown gate (unit).** With a webhook that accepts the connection
   and never responds and a saturated queue, `Close()` returns within
   2 s and `goleak`-equivalent inspection shows no worker left running.
   The evaluator makes progress while the sink is stuck.
10. **Retry gate (unit).** Transport error, 500 and 429 are retried up to
    the cap with backoff; **404 and 403 are not retried**, are counted,
    and surface in the status.
11. **Role gate (integration).** `POST /api/fleet/notify/send` and
    `POST /api/fleet/notify/scope` 404 on a daemon without
    `fleet_registry`, in `daemon/fleet_registry_test.go`'s probe list
    with every other fleetd route.
12. **Streaming-contract gate.** `git diff --stat main..HEAD --
    internal/vibe/proxy` is empty for the whole phase.
13. **Full inner loop** (ground rule 4) under `-race -count=5`, plus
    `golangci-lint run`, plus ground rule 9's adversarial self-review as
    its own commit.
14. **Live gates (need real hardware; NOT RUN here).**
    a. A real ntfy topic receives `vibe fleet notify test` on a phone.
    b. A genuine always_on outage (stop the heavy cell's llama-swap)
       pages once after the dwell and sends one resolve when it returns.
    c. A real vacation window: declare away, take a cell down, confirm
       silence + a visible suppressed count, come home, confirm one
       digest.
    d. A def edited on the front but not pushed to its cell pages once
       after 15 minutes and resolves when the cell re-renders.

## Out of scope

- **A generic event forwarder.** No "forward these SSE types" passthrough.
  The reasoning is §The finding; a forwarder is the design that produces
  the noise this item exists to avoid.
- **A fleetd-side await/watch registry** ("notify me when gpu-cell is
  up"). That would be a second place storing "someone is waiting", and
  the waiting process already exists and already knows. `vibe cell await
  --notify` is the answer; alarm resolve notifications are the passive
  half.
- **Repeat/escalation policy**, on-call schedules, acknowledgement,
  multiple sinks, per-alarm routing. One sink, one operator, no
  acknowledgement — this is a homelab, and `fleet_status` is the
  acknowledgement.
- **Notification of fleetd's own death.** A process cannot page about
  its own absence; that is a dead-man's-switch on the receiving side
  (ntfy supports it) and belongs to the private fleet repo's deployment,
  not here.
- **ETA-expiry alarms** ("drained until 23:00, it is now 02:00"). Real,
  tempting, and an inference about what the operator meant. If it lands
  it lands as its own declared thing.
- **Anything on the data plane.** No proxy changes, no new hop.

Estimated ~1100 lines + tests. The plan's calibration (C0–C4 ran
3.6–4.5× their estimates) applies.

## Execution (2026-08-04)

### What shipped

| piece | where |
|---|---|
| policy engine (kinds, dwell/dedup state machine, away gate, digest, rate bucket) | `internal/vibe/fleetnotify/fleetnotify.go` (new package) |
| webhook sink + redaction + retry classification | `internal/vibe/fleetnotify/webhook.go` |
| delivery worker (bounded queue, ctx-bound retry) | `internal/vibe/fleetnotify/deliver.go` |
| conditions, scope store, routes, status block | `internal/vibe/fleetapi/notify.go` |
| the fingerprint mismatch SET | `fleetapi/render_loop.go` (`applyFingerprints` → `setFingerprintMismatches`) |
| config + wiring | `daemon/notify.go`, `FleetConfig.Notify`, `paths.NotifyScopeFile()` |
| agent verbs | `fleetmcp/notify.go` — `fleet_notify_scope`, `fleet_notify_test` |
| CLI | `cli/cmd_fleet_notify.go` (`status\|test\|away\|home`), `vibe cell await --notify` |
| page | `fleet.html` — a notify strip + away/home and test buttons, both `tools/call` |

`internal/vibe/fleetnotify` imports no fleet package, in either
direction: fleetapi builds `[]Condition` and the tracker decides. That
seam is why the whole policy — every dwell, the flap kill, the away
digest, the bucket — is testable on an explicit clock with no server, no
socket and no timer.

Three things the doc did not spell out and the code had to decide:

- **The tracker lock is never held across a snapshot.** `notifyReport`
  renders the notify block from inside `probeSnapshot`, and the
  evaluator calls `Snapshot` — holding `notifyMu` across it deadlocks.
  The evaluator snapshots, derives conditions, then takes the lock only
  for `Step`.
- **A failed scope persist is honoured in memory anyway**, unlike every
  intent writer, which rolls back. The operator declared away and is
  about to stop reading; refusing the declaration because a disk write
  failed is the one outcome nobody wants. It logs, and the live state is
  visible in `fleet_status`.
- **Setting both `url` and `url_file` is an error**, not a precedence
  rule. Which one won is exactly the question an operator cannot answer
  from a log line that must not print either value.

### Gates

| gate | result |
|---|---|
| 1. Default policy | **PASS** — `TestDefaultAlarmsAreExactlyTheClassTablesAlarmColumn`, `TestTracker_DisabledKindNeverNotifiesEvenWhileTrue`, `TestNotifyConditions_DegradedModelIsAConditionButNotADefaultAlarm` |
| 2. Class | **PASS** — `TestNotifyConditions_OnlyAlwaysOnAbsenceAlarms`, `TestNotifyConditions_AlwaysOnAbsenceAlarmsInEveryUnexplainedDisplay`; mutation-verified (dropping the class filter fails it) |
| 3. Declared intent | **PASS** — `TestNotifyConditions_DeclaredDrainSuppressesButInferredIntentNeverDoes`, `TestNotifyConditions_InconsistentIsANagNotAnAlarm`; mutation-verified |
| 4. Flap | **PASS** — `TestTracker_TwoHundredFlapsBelowTheDwellNotifyZeroTimes` (literally 200 transitions → 0 notifications), `TestTracker_FlapBackDuringTheClearDwellNeitherResolvesNorRefires`, `TestTracker_FiresOnceAfterTheDwellAndNeverRepeats`; mutation-verified |
| 5. Persistence | **PASS** — `TestNotifyConditions_FingerprintMismatchRidesTheRenderPassSet` (+ mutation on `FirstSeen`), `TestRenderPass_PopulatesAndClearsTheFingerprintMismatchSet` drives the REAL loop, `TestNotifyStatus_ShowsSuppressionAndNamesTheFingerprintEvaluator` covers the no-render-loop case |
| 6. Lease | **PASS** — `TestNotifyConditions_DrainWithAnActiveLeaseAlarmsAndNamesTheHolder`, `TestTracker_ZeroDwellKindFiresOnTheFirstEvaluation` |
| 7. Away | **PASS** — `TestTracker_AwaySuppressesDeliveryButKeepsTheAlarmVisible`, `TestTracker_ComingHomeSendsExactlyOneDigestNamingWhatWasSuppressed`, `TestTracker_AwayNeverSuppressesAnExplicitMessage`, `TestNotifyScope_AwayExpiresByItselfAtTheDeclaredInstant`, `TestNotifySend_IsDeliveredWhileAway`; mutation-verified |
| 8. Secret | **PASS** — `TestWebhookSink_TransportFailureLeaksNeitherTheURLNorTheTopic`, `TestWebhookSink_ErrorBodyIsScrubbedOfTheTopic`, `TestDeliverer_LastErrorIsScrubbedThroughTheSink`, `TestDeliveryLogsNeverCarryTheSecret`, `TestNotifyStatus_NeverCarriesTheWebhookURL`, `TestNotifyConfigErrorsNeverEchoTheEndpoint`. Both guards are pinned INDEPENDENTLY: the first pass found that removing the `*url.Error` unwrap still passed because the scrub covered it, so the test now also asserts the unwrap ran |
| 9. Shutdown | **PASS** — `TestNotifyLoop_CloseReturnsPromptlyWhileTheWebhookHangs` (a webhook that accepts and never answers, a saturated queue, a one-hour backoff: `Close` returns), `TestDeliverer_CancellationAbandonsABackoffImmediately`, `TestDeliverer_EnqueueNeverBlocksAndCountsTheDrop` |
| 10. Retry | **PASS** — `TestWebhookSink_FourXXIsNotRetriedAndFiveXXIs` (404/403/400 vs 429/500/502), `TestDeliverer_RetriesARetryableFailureUpToTheCapThenReportsIt`, `TestDeliverer_APermanentFailureIsNotRetried`; mutation-verified |
| 11. Role | **PASS** — both routes added to `daemon/fleet_registry_test.go:TestDaemon_FleetRegistryOff_NoMCP` |
| 12. Streaming contract | **PASS** — `git diff --stat main..HEAD -- internal/vibe/proxy` is empty |
| 13. Inner loop | **PASS** — build / vet / `gofmt -l .` (silent) / `go mod tidy` (clean) / `golangci-lint run` (0 issues) / `go test -race -count=5 ./...` run four times end to end, all green (see the honest note below) |
| 14. Live gates (a–d) | **NOT RUN** — no route to the fleet's hardware from the implementing environment. No transcripts are fabricated |

**One honest note on gate 13.** An early full run printed a bare `FAIL`
line through a `tail` pipeline that discarded the per-package detail; it
did not reproduce in four subsequent complete `-race -count=5` runs (20
iterations of every test), nor in a 20× run of `fleetnotify` and a 10×
run of the new `fleetapi` tests, nor in a `-count=3` run of the four
packages this phase touches. It is recorded rather than explained away:
if a flake surfaces in CI, `internal/vibe/supervisor` (69 s, timing-
heavy, untouched by this phase) is the first place to look.

### Adversarial self-review (ground rule 9)

Landed as its own commit against the feature commit. Six findings, four
fixed with a mutation-verified regression test, two documented.

1. **A partial `Dwell` override silently zeroed every other kind
   (major).** `NewTracker` merged the dwell map per MAP (`if p.Dwell ==
   nil`), so a caller setting one threshold left the rest at zero —
   which means "fire on the first evaluation". A config with
   `dwell: {cell_absent: 5m}` would have turned the 15-minute
   fingerprint persistence rule into an instant page, i.e. the exact
   noise this phase exists to prevent, by omission. Production was safe
   only because `daemon.notifyPolicy` starts from `DefaultPolicy` and
   overlays; the library was one caller away. Merges per KIND now.
   `TestTracker_APartialDwellOverrideKeepsTheOtherKindsDefaults`.
2. **"Away" delivered the rate-limited backlog (major).**
   `drainDeferred` ran on every `Step`, including while away, so a
   notification the bucket held back the minute before the operator left
   went out mid-vacation. The deferral queue now only drains at home.
   `TestTracker_AwayHoldsTheRateLimitedBacklogToo`.
3. **The explicit-send title skipped the field hygiene every other
   ingest gets (minor).** It becomes an HTTP header at the sink, where
   `headerSafe` would silently mangle it; a 400 that names the problem
   beats a delivered message with its title eaten. The MESSAGE
   deliberately still allows newlines.
   `TestNotifySend_RejectsAControlCharacterTitleButAcceptsAMultilineBody`.
4. **The status block aliased the runner's `enabled` slice (minor)** —
   the shape C7a's review had to fix on `UsageReport`. Copied now.
5. **The evaluator/status lock order was a real deadlock one line
   away**, and nothing tested it: `notifyReport` runs INSIDE
   `probeSnapshot` and takes `notifyMu`, while `evalNotify` calls
   `Snapshot`. Taking the lock one line earlier hangs every
   `/api/fleet/state` in the process.
   `TestNotifyLoop_StatusAndEvaluationDoNotDeadlock` (mutation-verified:
   moving the lock above the snapshot hangs the test).
6. **A plaintext `http://` endpoint now warns at construction** (the
   topic is IN the path, so it travels unencrypted every request).
   Named, not refused — loopback and a reverse-proxied self-hosted ntfy
   are legitimate.

**Verified sound, not changed:** the both-edges dwell, the no-re-fire
rule, the digest, the front/class filter and the declared-vs-inferred
intent split all hold under mutation (each has a named test that fails
when the production line is broken); no lock is held across an HTTP call
or a `publish`; both notify goroutines are `wg.Add`-ed outside their
goroutine and exit on `s.done`; `git diff --stat main..HEAD --
internal/vibe/proxy` is empty.

**Known and accepted (documented, not fixed):**

- **Alarm state is not persisted.** A fleetd restart re-fires every
  still-true alarm once its dwell elapses again. That is arguably
  correct — the problem is still happening — and a crash loop is bounded
  by the token bucket. A store would add a staleness that could suppress
  a real alarm, which is the worse failure.
- **Explicit sends bypass the rate bucket.** They are bounded by the
  bearer auth, the 2000-byte message cap and the bounded queue (a flood
  becomes counted drops). Rate-limiting them would mean the one command
  that proves the pager works could silently do nothing.
- **The digest can in principle be evicted from a full deferral queue**
  if 32 further notifications arrive in the same evaluation round as the
  return from away. It is emitted first and takes the first available
  token, so this needs 32 simultaneous alarms to reach.
- **An `eta` that has passed does not alarm.** "drained until 23:00, it
  is now 02:00" is an inference about what the operator meant; if it
  ever ships it ships as its own declared thing.

### What the live gates would prove that the unit gates cannot

Everything about whether the alarms are the RIGHT alarms. The unit
gates prove the policy does what it says; only a week of real operation
says whether 2 minutes is the right absence dwell, whether 15 minutes is
long enough to avoid paging mid-deploy, and whether a dozen an hour is a
ceiling that ever binds. The most likely correction after live use is
the fingerprint dwell.
