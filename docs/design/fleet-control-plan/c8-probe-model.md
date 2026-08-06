# C8 — probe_model: throughput health, scored against the model's own baseline

Status: MERGED (#27, 2026-08-04), off `feat/c8-probe-model`
branched from `main` at `8658c2e` (C0–C7b + the post-merge
reconciliation PR). Feature commit, ground rule 9's adversarial
self-review commit, and a second (independent) adversarial review
commit — six further findings, all mutation-verified, listed in the
[second addendum](#adversarial-review-addendum-second-pass). Every
mechanically verifiable gate is green under `-race -count=5`. Live gates
L1–L3 **PASSED on 2026-08-05** against the local multi-cell harness
([`scripts/fleetlab`](../../../scripts/fleetlab/README.md)) — real
llama-swap cells and real probes, but CPU models, so the degradation in
L2 was induced by throttle rather than by VRAM spill. **L4 and L5 ran
under [C17](c17-gate-closure.md) the same day**: L4 PASSES at the cap
boundary (with the 24 h window seeded on disk, stated in the row), L5's
baseline half PASSES — and **L5's flag-change half FAILS against a real
defect**: a cell's probe specs are frozen at announcer start, so a def
edit hot-applied by `-watch-config` does NOT rebind the baseline key.
That is C17 finding 1 and it is unfixed. See
[§Execution](#execution-2026-08-04).

C8 fills the `probe` slot [C3](c3-announce.md) §1 reserved on
`AnnounceModel` and converts the design doc's
[friction pain 2](../fleet-control.md#10-friction-pain-scorecard-at-plan-completion)
from *deferred with a designed slot* to *answered*. It is backlog item 1
in [fleet-control-futures.md](../fleet-control-futures.md) §2 — the
highest-ranked v2 item, because pain 2 is the one guaranteed incident of
the year:

> **Nothing supervises health as throughput.** llama-server can degrade
> 10–100× while `/health` stays green; only realistic batch probes or
> domain-truth signals catch it. (design §1, pain 2)

`fleet_status` today answers every question except the one asked when
something *feels* slow. After C8 it answers that one too, with a number
and a baseline beside it.

## The shape of the answer

One canned, deterministic completion (or embedding batch) issued **on
the cell, against a model that is already resident**, timed, and scored
against the median of that model's own recent healthy samples. The
verdict — `ok` / `degraded` / `unknown` — rides the announce in the
reserved per-model `probe` block. fleetd displays it, publishes a
transition event, and does **nothing else with it**.

That last clause is the whole design. Everything below is the set of
rules that keep a measurement from becoming an actuator.

## Design

### 1. Who runs the probe: the cell's announcer

The cell, not fleetd. Four reasons, in descending order of how badly
the alternative fails:

1. **Only the cell can refuse to load.** A probe issued through the
   front is a request like any other, and llama-swap's whole contract is
   JIT-on-request: a probe for a model that isn't resident *starts it*.
   That is precisely the eviction C4's warm policy was built to avoid
   ("a pinned default re-warms on a timer and evicts the model the
   operator just swapped in" — design §9's rejected row). Only the box
   holding the model can check `/running` on localhost microseconds
   before issuing and abort instead. §2 makes this a rule.
2. **Attribution.** C7a established it for tokens and it holds
   identically for throughput: the front's rendered config is
   peers-only, so llama-swap's `RealModelName` resolves nothing there
   and the front records whatever string the client typed. Only the cell
   maps `qwen3.6-27b-tools` → `qwen3.6-27b`. A baseline keyed on a
   client's alias is a baseline of nothing.
3. **It measures the model, not the path.** A fleetd-side probe through
   the front measures LAN + the front's proxy hop + the cell's queue +
   the model. When the number moves you cannot say which moved. The
   cell-side probe reads llama.cpp's own `timings` block, which excludes
   queueing entirely (§4).
4. **C3's inversion.** Cells announce, the catalog is derived, and after
   C3 `daemon_url` is an optimization rather than a requirement. A
   fleetd-side prober needs a route to every cell and re-inverts the
   phase this plan spent its largest PR on.

fleetd keeps exactly two jobs: it **asks** (scheduling and the MCP verb,
because it is the only party that knows about leases and in-flight work
fleet-wide), and it **displays**. The asking travels on the C3 piggyback
command queue, so an announce-only cell with no inbound port is probeable
like any other.

### 2. When a probe may cause a load: never

The rule, stated so it can be tested:

> A probe is issued **only** for a model the cell's own llama-swap
> reports as `ready` at the moment of issue. A probe never loads,
> re-loads, or keeps alive a model. There is no flag, no argument and no
> config value that relaxes this.

Consequences, all deliberate:

- `probe_model` on a stopped model is a **refusal with an instruction**,
  not a load: *"qwen3.6-27b is not resident on gpu-cell; a probe must not
  load it. Warm it first (`warm_model`), then probe."* Warming is a
  declared act with its own verb; folding it into a measurement is how
  the measurement becomes an actuator.
- The residency check happens **twice**: fleetd checks announced
  residency before queueing (cheap, ~1 heartbeat stale), and the cell
  re-checks its own `/running` immediately before issuing (authoritative).
  Only the second one is load-bearing; the first exists so the command
  queue does not fill with probes that will all be refused.
- The residual race is one TTL eviction landing between the cell's
  `/running` read and its request — milliseconds — and its cost is one
  JIT load of a model that was resident a moment earlier. Stated, not
  hidden.
- A probe does **not** count as activity for C4's warm targets. Those key
  on fleetd's inflight-frame timestamps, and a probe does produce an
  inflight frame, so a probe *does* reset a warm target's idle window.
  That is the honest direction to err: a probe delays a restore by one
  window rather than causing one.

### 3. Respecting the operator's box: guards and budget

C4's warm schedule established the pattern and C8 reuses it verbatim
(`warmsched.go:evalScheduleEntry`), including the rule that **a guard
which cannot be evaluated is a skip**:

| guard | evaluated by | on failure |
|---|---|---|
| cell drained (`effectiveIntent`) | fleetd, before queueing | skip, noted |
| cell stale / withdrawn / never announced | fleetd | skip, noted |
| model not announced `ready` | fleetd | skip, noted |
| in-flight > 0 **or unreported** | fleetd | skip, noted ("unknown in-flight is not zero in-flight") |
| active lease on the cell | fleetd | skip, noted |
| model not resident **now** | cell, at issue | refuse, reported in the next announce |
| cooldown / daily cap | cell | refuse, reported |

Everything a skip costs is one line in `fleet_status`; nothing retries
in a tight loop.

**The budget, explicit and bounded.** This is the one place the control
plane deliberately generates inference load, so the numbers are written
down rather than implied:

| knob | value | where enforced |
|---|---|---|
| chat probe | 1 request, deterministic ~200-token prompt, `max_tokens: 64`, `temperature: 0`, non-streaming | `modelprobe` (constant) |
| embed probe | 1 request, 64 fixed short inputs (the field-proven 64-input batch shape) | `modelprobe` (constant) |
| per-probe wall bound | 3 min, then abandoned and recorded as a failed attempt (a model degraded 100x still answers inside it: 64 tokens at 0.5 tok/s is ~128 s) | `modelprobe` |
| min gap between two probes of the same model | 5 min | cell (hard floor, survives a duplicated command) |
| max probes per cell per rolling 24 h | 96 | cell (hard cap) |
| scheduled interval floor | 5 min, clamped like `minRestoreAfterIdle` | daemon config wiring |
| concurrent probes per cell | 1 (single-flight) | cell |

Worst case per cell per day: 96 × (~200 prompt + 64 generated) ≈ 25 k
tokens. The cell-side caps are the real bound — the piggyback queue is
at-least-once by design (C6), so a redelivered command must not buy a
second probe, and it doesn't.

**Probes are metered as ordinary traffic.** C7a's poke rule is
`output_tokens <= 1`, and a 64-token probe is not a poke, so probe
tokens land in the billable columns of the usage ledger. That is
~25 k tokens/cell/day at the cap and typically ~1 k. Tagging them is not
possible (llama-swap's `Metadata` is populated only by its internal
handlers — C7a §3), so the honest move is to say so here and keep the
budget small enough that it doesn't matter.

### 4. What is measured, and against what baseline

**Metric.** Decode throughput, in output tokens per second, from
llama.cpp's own `timings` block on the non-streaming response
(`timings.predicted_per_second`). It excludes prompt processing and
queueing, which is what makes it comparable across runs. When the
response carries no `timings` (mlx, or a future engine), the fallback is
end-to-end `completion_tokens / wall_seconds` — recorded under a
**different metric name** (`e2e_tok_s` vs `decode_tok_s`) and keyed
separately, because comparing the two would manufacture a regression out
of a parser change. Embedding probes measure `embed_inputs_s`.

TTFT is recorded for context and **is not scored**: a probe that lands
behind a real request measures the queue, and scoring that would fire
the alarm exactly when the box is busy — the inverse of the signal.

**Baseline location: the cell, beside the prober.** Not
`fleetapi/history.go`, and the reasoning is worth keeping:

- `history.go` is fleetd's, and the *verdict* has to be computable on the
  cell — C3's cardinal rule is that an unreachable registry never affects
  the box. A cell whose registry is down must still know its model is
  degraded and say so on the next successful heartbeat.
- Its keyspace is wrong: `map[model][]{at, seconds}` versus C8's
  `(model, flags_sha256, metric)`.
- What C8 *does* reuse is its **shape**, deliberately: a small rolling
  window (cap 20 samples), persisted as one JSON file, rewritten on every
  record through tmp+rename. `history.go`'s comment says rewrite-on-record
  is right because "starts are rare (seconds-to-minutes apart at best)".
  Probes are minutes-to-hours apart, so the same reasoning applies, and
  C7a's opposite choice (append-only JSONL) was driven by a 15-second
  fold cadence that does not exist here.

**The baseline key includes `flags_sha256`.** A def edit — new quant, new
`-ngl`, a changed draft model — produces a different server, and scoring
the new one against the old one's numbers reports a regression that is
really a change. On a fingerprint change the baseline for that key is
simply empty: verdict `unknown`, "baselining (2/5 samples)". This is the
same hash C3 already computes and announces; C8 adds no second notion of
identity.

**Scoring.** `ratio = value / baseline_p50`, where `baseline_p50` is the
median of the stored samples for that key, excluding the current one.

- fewer than 5 samples → `unknown` (never `degraded` without a baseline;
  the false-alarm class this rules out is "a fresh cell screams on boot")
- `ratio < 0.50` → `degraded`
- `ratio >= 0.65` → `ok`
- in between → the previous verdict is kept (hysteresis, so a model
  sitting near the line doesn't drip transition events)

**Baseline updates: healthy samples only.** A `degraded` sample does not
enter the window. The alternative — every sample updates — means a
genuine, persistent 2× regression washes out of a 20-sample median in
about eleven probes and the status quietly goes green while the box is
still slow. The cost of the choice is that a *legitimate* permanent
slowdown (a llama.cpp build that trades tok/s for something else, and
therefore changes no flags) stays flagged forever, so the escape hatch is
explicit rather than automatic: `probe_model(..., rebaseline: true)`
clears that key's window and starts over. The status carries
`degraded_since` and the baseline's own age so "flagged for six days
against a baseline from July" is legible.

### 5. `degraded` is a per-model health state and lives nowhere else

The three ownership axes (design §4) are availability (observed), intent
(declared) and residency (llama-swap's). A degraded model on a serving
cell is a **fourth thing**, and this phase's main way to fail is to let
it leak into the first one. So:

- `degraded` is carried on the **model row** —
  `AnnounceModel.Probe`, surfaced as `ModelState.Probe` in
  `/api/fleet/state` — never on `CellSnapshot.Reachable`,
  `CellSnapshot.Display` or `Intent`. A cell with three healthy models
  and one degraded one is `SERVING`, exactly as it is.
- It never changes what the front render emits. `render_loop.go`'s
  exclusion path stays fingerprint-only.
- It never triggers a warm, an unload, a drain or a render.
- It is not inferred from anything. No verdict is ever computed from
  observed traffic, TTL churn, or a cell's silence — only from a probe
  that actually ran and returned a number. "No probe has run" is
  `unknown`, and `unknown` reads as *nothing is known*, not as *fine*.

**Why not withdraw a degraded model from the render** (design §10's
sketch says "marking models degraded → withdrawn from render"): three
reasons, and this doc is the work order, so it decides.
(a) A degraded chat model still answers; yanking its id from the catalog
turns a slow model into a fleet-wide 404 for every consumer pinning it —
the failure mode the class table's `hold` policy exists to prevent.
(b) The probe has a false-positive tail by construction (§4's queueing
note), and a fail-closed action on a measurement with a false-positive
tail is exactly the "blanket fail-closed fingerprints" alternative
design §9 already rejected. (c) The operator's fix is one existing verb:
`unload_model` — the next request JIT-reloads a clean server — so the
runbook is *probe → unload → probe*, and every step of it is declared.

### 6. Wire format (additive, v1-safe)

`AnnounceModel.Probe` changes from `any` (always `null` in v1) to
`*AnnounceProbe`. Byte-compatible in both directions: a v1 cell sends
`"probe": null` and decodes to `nil`; a v1 fleetd receiving a populated
block decodes it into `any` and ignores it, which is C3's unknown-field
tolerance doing its job.

```json
{ "id": "qwen3.6-27b", "state": "ready", "flags_sha256": "9f2c…",
  "probe": { "kind": "chat", "spec": "chat/v1:64out",
             "at": "2026-08-04T14:03:11Z",
             "metric": "decode_tok_s", "value": 41.7,
             "baseline_p50": 44.9, "samples": 12, "ratio": 0.93,
             "verdict": "ok", "ttft_ms": 210,
             "flags_sha256": "9f2c…" } }
```

The announce response's `commands[]` gains the `probe` verb
(`{"verb":"probe","model":"qwen3.6-27b","rebaseline":false}`). An old
cell receiving it logs "unknown piggyback verb" and continues — already
its behaviour, unchanged.

Ingest hygiene follows C7a's precedent exactly: the announce is untrusted
input, so strings are length/control-char checked and numbers are clamped
non-negative, but an unrecognised `kind` or `metric` is **not** rejected —
a cell one version ahead must not have its whole heartbeat (and with it
presence, and the intent echo) refused over an accounting field. `verdict`
is enum-checked and an unknown value reads as `unknown`, because that
field drives an event.

### 7. What fleetd does with it

- **Display**: `ModelState.Probe` on every cell snapshot, so the MCP
  `fleet_status`, `GET /api/fleet/state` and the C4 page all show the
  same numbers. The page grows one badge on a degraded model span — no
  new route, no new mutation surface (C5's exact-match bearer exemption
  is untouched).
- **Events**: `fleet.modelDegraded` / `fleet.modelRecovered` on verdict
  transitions only (never on every heartbeat). C3 §3 reserved
  `model_degraded` with no emitter; the emitted names follow the code's
  dotted-camelCase convention like every other fleet event.
- **A `probe` block in `fleet_status`** beside `warm`: per target, its
  last request, its last result, its next due time, and the reason for
  the most recent skip. A guard that silently skips forever is the
  failure C5 spent a phase fixing.
- **Scheduling**: `probe_targets:` in the fleetd config — declared, never
  implicit. No entries means no probing at all, which is the default.

### 8. Files

| piece | where |
|---|---|
| cell-side prober + baseline | `internal/vibe/modelprobe/` (new) |
| wire types, ingest, events | `fleetapi/announce.go` |
| scheduler, guards, status block | `fleetapi/probe.go` (new) |
| snapshot surfacing | `fleetapi/fleetapi.go`, `fleetapi/display.go` |
| page badge | `fleetapi/fleet.html` |
| cell wiring (verb + blocks) | `fleetannounce/fleetannounce.go` |
| daemon wiring | `daemon/announce.go`, `daemon/probe.go` (new), `daemon/daemon.go` |
| slim-announcer wiring | `cli/cmd_fleet.go` |
| MCP verb | `fleetmcp/fleetmcp.go`, `fleetmcp/probe.go` (new) |
| state file | `paths.CellProbeFile()` |

## Acceptance gates

1. **A probe never loads a model (unit).** With the cell's `/running`
   reporting the model absent (and with it reporting `stopped`), the
   prober issues **no** request to `/v1/chat/completions` and returns a
   refusal naming residency. Mutation-tested: deleting the residency
   check makes the test fail.
   `TestProbe_RefusesAModelThatIsNotResidentAndIssuesNoRequest`.
2. **fleetd's scheduler respects the C4 guards (unit).** A probe target
   is skipped, with the reason in `fleet_status`, when the cell is
   drained, when the cell is stale, when in-flight > 0, when in-flight is
   **unreported**, when an active lease names the cell, and when the
   model is not announced `ready` — and it queues exactly one `probe`
   command when none of those hold.
   `TestProbeSchedule_*`.
3. **Degraded does not leak into availability (unit).** A cell
   announcing a `degraded` verdict on one model still renders
   `SERVING`, `reachable: true`, unchanged `intent`, and its model set is
   unchanged in the front render. Mutation-tested against a deliberate
   leak.
   `TestProbe_DegradedModelDoesNotChangeCellDisplayOrRender`.
4. **Baseline scoring (unit).** Under 5 samples → `unknown`, never
   `degraded`. A 3× slowdown against a 10-sample baseline → `degraded`
   with the ratio. A sample inside the hysteresis band keeps the previous
   verdict. A `flags_sha256` change starts a fresh baseline rather than
   scoring across it. A degraded sample does not enter the baseline
   window; `rebaseline` clears it.
   `TestScore_*`, `TestBaseline_*`.
5. **Budget is enforced on the cell (unit).** Two probe commands inside
   the cooldown produce one probe; the 24 h cap refuses the 97th; two
   concurrent probes produce one request and one "already running".
   `TestProbe_CooldownAndCapAreEnforcedOnTheCell`,
   `TestProbe_SingleFlight`.
6. **The heartbeat is never held hostage (unit).** A probe that takes
   longer than an announce interval does not delay the announce loop: the
   command handler returns immediately and the result appears in a later
   heartbeat. Mutation-tested against a synchronous implementation.
   `TestAnnounceProbeCommand_DoesNotBlockTheHeartbeat`.
7. **Wire compatibility (unit).** A v1 announce with `"probe": null`
   round-trips unchanged; a populated block survives a
   marshal/unmarshal cycle; an announce carrying an unknown `kind`,
   unknown `metric`, or a garbage `verdict` is **accepted** (verdict
   normalised to `unknown`) rather than costing the cell its heartbeat;
   negative values are clamped at ingest.
   `TestAnnounceProbe_*`.
8. **Events fire on transitions only (unit).**
   `fleet.modelDegraded` once when the verdict flips, nothing on repeat
   heartbeats carrying the same verdict, `fleet.modelRecovered` on the
   way back.
   `TestProbeEvents_FireOnTransitionsOnly`.
9. **Streaming contract (mechanical).**
   `git diff --stat main..HEAD -- internal/vibe/proxy` is empty for the
   whole phase.
10. **Full inner loop** (ground rule 4): build, vet, gofmt, `go mod
    tidy`, `go test -race -count=5 ./...`, `golangci-lint run` — plus
    ground rule 9's adversarial self-review as its own commit.

### Live gates (need real cells; NOT runnable from the implementing
environment)

L1. **Real baseline, real verdict.** Probe a resident chat model on the
    gpu-cell ten times over an evening; confirm the samples cluster,
    `verdict: ok`, and the `p50` matches what `llama-bench`-style manual
    timing says within ~10%.
L2. **Induced degradation is caught.** Force the classic failure: load a
    second large model so the first spills out of VRAM (or start a
    competing process), then probe. Verdict flips to `degraded`,
    `fleet.modelDegraded` lands on the events stream, `fleet_status`
    shows the ratio, and `unload_model` + a fresh probe returns it to
    `ok`.
L3. **The load rule holds in the field.** `probe_model` a model that is
    *not* resident on a cell with a TTL that just evicted it: the cell
    refuses, `nvidia-smi` shows no load, and the front catalog is
    unchanged.
L4. **Budget observed.** With a 15-minute `probe_targets` interval on
    two cells for 24 h, the cell-side counters show ≤ 96 probes each and
    C7a's ledger shows the corresponding token delta within the stated
    envelope.
L5. **Embed probe on the utility cell.** A 64-input batch against the
    embedding model produces `embed_inputs_s` with a stable baseline;
    changing a serving flag starts a fresh baseline instead of reporting
    a regression.

## Out of scope (deliberately)

- **Withdrawing degraded models from the front render.** §5 decides
  against it and says why. If it is ever revisited, it belongs behind the
  same `fingerprint: strict` opt-in embed defs already use, not as
  default behaviour.
- **Auto-remediation** (probe → unload → re-probe without a human). The
  measurement must not become an actuator; the runbook is documented
  instead. `sleep_schedule`-style declared-action-deferred-by-observation
  is the sanctioned shape if this is ever wanted.
- **Latency SLOs, percentile dashboards, Prometheus export.** llama-swap's
  `/ui` and metrics own throughput display; C8 owns one comparison
  against one baseline.
- **Probing through the front, or fleetd-side probing of any kind.** §1.
- **Tool-call-rate / quality scoring** (`vibe bench replay`, futures item
  11). C8 measures speed only; a quality regression is a different phase
  with a different corpus.
- **Tagging probe traffic out of the C7a ledger.** Not possible without a
  llama-swap change (§3); documented instead.
- **Per-cell probe credentials or a separate probe token.** The fleet
  token remains every cell's voice (design §6); a forged announce can
  already fake `SERVING`, and after C8 it can additionally fake
  `degraded` — which does nothing but display, by §5's construction.

Estimated ~700 lines + tests, on the plan's calibration (C0–C4 ran
3.6–4.5× their estimates).

## Execution (2026-08-04)

### What shipped

| piece | where |
|---|---|
| cell-side prober + rolling baselines | `internal/vibe/modelprobe/modelprobe.go` (new package) |
| def → probe spec (kind, fingerprint binding) | `internal/vibe/modelprobe/hooks.go` — `SpecsFromDefs`, `Hooks` |
| wire block + ingest hardening + transition events | `fleetapi/announce.go` — `AnnounceProbe`, `normalizeProbe`, `probeEvents`, `CloneProbe` |
| scheduler, shared guard, status block | `fleetapi/probe.go` — `ProbeTarget`, `evalProbeTarget`, `probeGuard`, `attachProbes`, `probeReport` |
| snapshot surfacing | `fleetapi/fleetapi.go` — `ModelState.Probe`, `StateSnapshot.Probe` |
| page badge + footer lines | `fleetapi/fleet.html` (no new route) |
| cell wiring (verb + blocks) | `fleetannounce/fleetannounce.go` — `Config.Probes`, `Config.RunProbe`, the `probe` command case |
| daemon wiring | `daemon/probe.go` (new), `daemon/announce.go`, `daemon/daemon.go` (`probe_targets:`) |
| slim-announcer wiring | `cli/cmd_fleet.go` |
| MCP verb | `fleetmcp/probe.go` (new) + the tool entry in `fleetmcp/fleetmcp.go` |
| state file | `paths.CellProbeFile()` → `$XDG_STATE_HOME/vibe/fleet/model-probe.json` |

Four things the doc did not spell out and the code had to decide:

- **The cooldown is keyed on the last ATTEMPT, not the last result.** A
  refusal (not resident, cell busy) deliberately carries the previous
  measurement forward so the status keeps showing it — and keying the
  5-minute gap on that timestamp would let a run of refusals lock a model
  out of probing for as long as they kept arriving.
  `TestProbe_RefusalsDoNotExtendTheCooldown`.
- **The daily cap counts attempts, not completions**, and its window
  rolls rather than resetting at midnight. An abandoned probe spent the
  GPU time too.
- **Probe kind is read off the rendered argv**, not the model name and
  not a new def field: `--embedding`/`--pooling` means embed,
  `--reranking` means *disabled* (its request body is a query plus
  documents, so the embed batch would measure a 400 and call it
  throughput), `cloud_peer` means disabled (every probe of one is a paid
  request). Reusing the argv the C3 fingerprint is already computed over
  means the kind and the baseline key come from the same source of truth.
- **The front cell is refused on both producers.** Its rendered config is
  peers-only, so a probe there measures a peer THROUGH the front — the
  confounded measurement §1 rejects.

C8 adds **no HTTP route**: the verdict rides `/api/fleet/state` (already
mounted) and the verb rides `/mcp` (already fleetd-gated), so
`daemon/fleet_registry_test.go`'s route list is unchanged and C5's
exact-match bearer exemption is untouched.

### Gates

| gate | result |
|---|---|
| 1. A probe never loads a model | **PASS** — `TestProbe_RefusesAModelThatIsNotResidentAndIssuesNoRequest`, `TestProbe_RefusesAModelThatIsStartingRatherThanReady`. Mutation-verified: deleting the residency check fails both. |
| 2. Scheduler respects the C4 guards | **PASS** — `TestProbeSchedule_{QueuesOneProbeWhenEveryGuardPasses,SkipsADrainedCell,SkipsWhenInFlightIsUnreported,SkipsABusyCell,SkipsACellWithAnActiveLease,SkipsAModelThatIsNotResident,SkipsAStaleCellAndOneThatNeverAnnounced}`. Mutation-verified on the unreported-in-flight branch. |
| 3. Degraded does not leak into availability | **PASS** — `TestProbe_DegradedModelDoesNotChangeCellDisplayOrAvailability` (through the REAL snapshot path), `TestProbe_DegradedModelStaysInTheAnnouncedModelSet`. Mutation-verified: a leak added in `attachProbes` fails it. The first version of this test called `decorate` alone and did NOT catch that mutation — ground rule 10 in miniature; the body was rewritten before the gate was claimed. |
| 4. Baseline scoring | **PASS** — `TestScore_{UnderMinSamplesIsUnknownNeverDegraded,ThreeTimesSlowdownAgainstABaselineIsDegraded,HysteresisBandKeepsThePreviousVerdict}`, `TestBaseline_{DegradedSamplesDoNotEnterTheWindow,RebaselineClearsTheWindow,FlagsChangeStartsAFreshBaseline}`, `TestProber_BaselinesSurviveARestart` |
| 5. Budget enforced on the cell | **PASS** — `TestProbe_{CooldownRefusesTheSecondProbeAndKeepsTheLastResult,RefusalsDoNotExtendTheCooldown,DailyCapRefusesPastTheBudget,SingleFlight}`. Mutation-verified on the cooldown key. |
| 6. The heartbeat is never held hostage | **PASS** — `TestProberStart_ReturnsWhileTheProbeIsStillRunning` (the production guarantee; mutation-verified — a synchronous `Start` deadlocks the test) and `TestAnnounceProbeCommand_ReturnsWhileTheProbeIsStillRunning` (the wire half, asserting the announce completed while the probe was demonstrably still in flight). |
| 7. Wire compatibility | **PASS** — `TestAnnounceProbe_{V1NullRoundTripsUnchanged,UnknownKindAndMetricAreAccepted,GarbageVerdictNormalizesToUnknown,NegativeNumbersAreClampedAtIngest,OversizedStringsAreRejected,FutureTimestampIsClampedAtIngest}` |
| 8. Events fire on transitions only | **PASS** — `TestProbeEvents_FireOnTransitionsOnly` (mutation-verified), `TestProbeEvents_LosingEvidenceIsNotARecovery` |
| 9. Streaming contract | **PASS** — `git diff --stat main..HEAD -- internal/vibe/proxy` is empty |
| 10. Full inner loop + review commit | **PASS** — build / vet / `gofmt -l .` (silent) / `go mod tidy` (clean) / `go test -race -count=5 ./...` / `golangci-lint run` (0 issues), re-run after the review commit |
| L1 (live) | **PASS (2026-08-05, local multi-cell harness)** — 6 real 64-token probes at a resident chat model on a real llama-swap cell: 16.282, 13.608, 15.725, 16.221, 16.096, 15.325 decode tok/s. The verdict stayed `unknown` for the first five (`samples_behind` 0…4) and became `ok` the moment the fifth sample landed: `{"value":15.325,"baseline_p50":16.096,"samples":5,"ratio":0.952,"verdict":"ok"}`. **Not covered:** the ±10% cross-check against manual `llama-bench` timing, and the model is a 7B on CPU rather than a GPU-resident model. |
| L2 (live) | **PASS (2026-08-05, same harness), by a substituted cause.** Degradation was induced with a 30% duty-cycle `SIGSTOP`/`SIGCONT` throttle on the cell's llama-server, not by VRAM spill. The next probe read `{"value":4.506,"baseline_p50":15.910,"ratio":0.283,"verdict":"degraded"}`, `fleet.modelDegraded` landed on `/api/fleet/events` exactly once, `fleet_status.probe.degraded` listed the model, and removing the throttle returned it to `ok`. The "**degraded changes nothing**" half was checked hard: display, `reachable`, intent, class and the front's model list were identical before and after, and the rendered front config was **byte-identical** across both degraded episodes. Also confirmed: neither degraded sample entered the baseline window (11 attempts, 8 window entries), and a probe that exceeded `probeTimeout` produced a failed attempt with the previous measurement carried forward — never a fabricated slow reading. **Not covered:** spill-induced degradation, which is what an operator will actually hit. |
| L3 (live) | **PASS (2026-08-05, same harness)** — `probe_model` at a non-resident model on two different cells refused with `a probe must not load it; warm it first`, and **zero** requests were issued: the cell's llama-swap log mentions of the model stayed at 0 across the window and the other cell's `/running` stayed `[]`. Verified from the upstream's own logs rather than `nvidia-smi`, which the CPU lab has no equivalent of. Bonus, same session: the 5-minute cell-side cooldown refused both producers (scheduler and MCP verb) from the same clock, carried the previous measurement forward, and refusals did not extend the cooldown. |
| L4 (live) | **PASS at the boundary (2026-08-05, C17, `scripts/fleetlab/gate-c8-l4.sh`)**, with the substitution stated plainly: the 24 h window was **pre-seeded on the cell's own state file** (`paths.CellProbeFile()`'s `attempts` array, which `attemptsSinceLocked` reads) rather than accumulated over 24 h, and the real `probe_model` verb then drove the real piggyback queue into the real budget code. Seeded 96-in-window → refused `daily cap reached (96 probes in the last 24h)` with the previous measurement carried forward and **no attempt spent** (still 96). Seeded 95-in-window + 6 rows 25–30 h old (101 on disk) → **allowed**, a real measurement landed (`{"value":15.677,"samples":4,"verdict":"unknown"}`), and the file pruned to 96 — the window rolls. Cooldown then cleared on disk so the CAP, not the 5-minute gap, answered the next ask: refused again, attempts still 96. Ledger half: one probe costs 101.7 tokens, averaged over 6 rows read off llama-swap's activity API by `gate-c7a-partials.sh` — **not** by this rig, whose ledger lines read `/api/fleet/usage` as `.rows` when the document's key is `buckets` and so printed `null` throughout (C17 finding A2, fixed there; the cap half of the row is untouched by it). **Not covered:** 24 h of the SCHEDULER asking, which is wall clock and nothing else. |
| L5 (live) | **SPLIT (2026-08-05, C17, `scripts/fleetlab/gate-c8-l5.sh` + `gate-c8-l5-staleflags.sh`).** Baseline half **PASS**: six real 64-input batches at `lab-embed-c` on charlie (real bge-large-en-v1.5, real llama-swap) under the unchanged flags, kind read off the rendered argv as `embed`, metric `embed_inputs_s` — 29.484, 28.684, 31.067, 30.303, 28.542, 29.222 inputs/s (a seventh, 22.964, landed after the flag edit and belongs to the failing half below), `unknown` under 5 samples and then `{"samples":5,"ratio":0.968,"verdict":"ok"}`, `{"samples":6,"ratio":1.003,"verdict":"ok"}`. Flag-change half **FAIL**: with `--threads 4 → 3` re-rendered and **applied by llama-swap's `-watch-config`** (verified in `/running`'s argv), the next probe still announced `flags_sha256 3937ef13b048…`, scored the slower sample against the old baseline (`22.96` vs `29.01`, ratio 0.79) and folded it INTO that window (n → 9). Restarting the announcer — same def, same running argv, nothing else changed — produced the fresh key `b998ecc3044a…` at `samples: null, verdict: unknown`. Cause and repro: [C17 finding 1](c17-gate-closure.md#finding-1-product-defect-a-cells-probe-specs-and-fingerprints-are-frozen-at-announcer-start) — `modelprobe.Hooks` gets a defs slice loaded once at announcer start, while fleetd re-reads defs every render pass. Not fixed there (C17 ships no code); the gate is correct and the code is not. |

### Adversarial self-review (ground rule 9)

Landed as its own commit. Four findings, each with a mutation-verified
regression test.

1. **Probe blocks were copied shallowly (minor).** A struct copy carries
   `DegradedSince` and `BaselineAt` — captured under a lock, dereferenced
   after it, the exact shape C7a's review had to fix on `UsageReport`.
   `fleetapi.CloneProbe` now deep-copies and every hand-off uses it.
   `TestCloneProbe_DoesNotAliasTheTimePointers`.
2. **`next_due` was declared, documented and never populated (minor).**
   `TestProbeSchedule_PublishesTheNextDueTime`.
3. **`probe_model` accepted the front cell (minor)** while the scheduler's
   config wiring refused it. `TestProbeModelTool_RefusesTheFrontCell`.
4. **A cell-supplied `probe.at` was unbounded at ingest (minor)** — every
   other cell timestamp is clamped (C3's echo rule).
   `TestAnnounceProbe_FutureTimestampIsClampedAtIngest`.

**Verified sound, not changed:** the load rule holds under mutation at
both producers; presence model slices are replaced, never mutated in
place, so the probe pointers a snapshot reads cannot tear; `probeGuard`
is one function shared by the scheduler and the MCP verb, so they cannot
drift; `Run` marks the attempt BEFORE measuring, so an abandoned probe
still costs budget.

### Known and accepted (documented, not fixed)

- **A probe resets C4's warm-target idle window.** Probes produce
  inflight frames like any request, and fleetd's per-model activity map
  cannot tell them apart. The effect is to DELAY a restore by one window,
  never to cause one — the honest direction to err.
- **Probe traffic is metered as ordinary traffic by C7a.** A 64-token
  probe is not a poke (`output_tokens <= 1`), and llama-swap's `Metadata`
  is populated only by its internal handlers, so there is no way to tag
  it. Bounded by the budget — but the bound this bullet originally gave
  ("≤ ~25 k tokens/cell/day at the cap, typically ~1 k") was a **chat**
  figure, and C17 measured both kinds on the harness (2026-08-05):
  **101.7 tokens per chat probe** (~9.8 k/cell/day at the 96 cap) and
  **768.0 tokens per embed probe** (~**73.7 k**/cell/day), because
  `embedBatch = 64` and `cannedEmbedInput` is a 12-word sentence. An
  embed target costs roughly 3x the ceiling this bullet claimed. The
  budget is doing its job; the sentence describing it was wrong.
- **Between fleetd's guard evaluation and the cell's execution sits up to
  one heartbeat.** Work that starts inside that window meets a probe.
  The cost is one bounded completion, and the cell still re-checks
  residency (the guard that actually matters).
- **A `note`-only result (refusal) keeps the previous verdict visible.**
  A refusal is not evidence about the model's health, so it must not
  erase what was known — but it does mean a stale `ok` can sit beside a
  string of refusals. `at` and the note say so.
- **Kind detection scans the rendered argv textually**, so a quoted
  argument containing `--embedding` would be read as a flag. The failure
  mode is a chat def probing as embed, which fails loudly on the first
  probe rather than reporting a wrong number.

### Adversarial-review addendum (second pass)

Ground rule 9's review pass run by a second agent against the merged
feature + self-review commits. Six findings, no blockers — the load rule,
the guard set, the events and the ownership-axis separation all held
under mutation (see "mutation-verified sound" below). Each fix landed
with a regression test that was mutation-verified: the production change
reverted, the named test observed to fail, the change restored.

1. **A rerank def was probeable whenever `--reranking` was not the first
   flag (minor).** `kindFromArgv` returned on the first recognised flag,
   so `--reranking` alone disabled the def while `--embedding
   --reranking` or `--pooling rank --reranking` — the way a reranker is
   actually configured, since llama.cpp's rerank mode IS pooling-type
   rank — read as a plain embedding server. Same def, opposite answers,
   decided by which flag the operator typed first. The consequence is a
   64-input embed batch against a rank-pooled server every five minutes:
   a 400 recorded as a failed probe, burning the attempt budget, exactly
   the mis-probe the doc says is prevented. Disabling flags are now
   scanned across the whole argv before the kind is decided.
   `TestSpecsFromDefs_RerankIsDisabledWhateverTheFlagOrder`.
2. **`samples` counted the window AFTER the new sample, not the baseline
   behind the verdict (minor).** The field's own contract is "how many
   backed it" and the threshold is `minSamples = 5`, so the fifth probe
   announced `samples: 5, verdict: unknown` — which reads as a broken
   scorer rather than as "four samples so far". `baseline_at` had the
   same off-by-one and dated a baseline of zero on a fresh cell's first
   probe. Both now describe the window `baseline_p50` was computed from.
   `TestRecord_SamplesCountsWhatBackedTheVerdictNotTheWindowAfterIt`,
   `TestRecord_BaselineAtIsNilUntilThereIsABaseline`.
3. **The `degraded` roll-up answered for cells that stopped announcing
   (minor).** `probe.degraded` is the one-line answer to "is anything
   slow RIGHT NOW", and it walked every presence entry — so a cell
   withdrawn a week ago kept reporting its last verdict as current. C6's
   rule (staleness retires the announce as evidence) applies with extra
   force to the one field that is purely evidence. It now reads fresh
   announces only; the model row keeps the verdict either way, so
   nothing is hidden, only un-claimed.
   `TestProbeReport_DegradedRollUpReadsFreshAnnouncesOnly`.
4. **The front-cell refusal was in both producers and in neither
   guard (minor).** §3 and AGENTS.md both say one `probeGuard` serves the
   scheduler and the MCP verb "so they cannot drift" — but the front rule
   lived in the daemon's config filter and in `fleetmcp/probe.go`, i.e.
   in two hand-written copies outside the thing that exists to prevent
   drift. A shared guard only shares the rules it holds. `probeGuard`
   now refuses the front (the MCP verb still errors loudly first, so its
   behaviour is unchanged), which also turns a front `probe_targets`
   entry reaching `StartProbeLoop` from a silently dropped config line
   into a visible skip. `TestProbeGuard_RefusesTheFrontCellItself`.
5. **`probeTargetState`'s time pointers escaped the lock that owns them
   (nit).** The self-review fixed exactly this shape on `AnnounceProbe`
   (`CloneProbe`) and missed the struct one file over: `evalProbeTarget`
   captured `st.LastAsk` under `s.mu` and dereferenced it after the
   unlock, and `probeReport` handed out shallow copies carrying the same
   two pointers. Provably safe today — `setProbeState` replaces the
   pointer rather than writing through it — and precisely the shape this
   repo has already shipped a race in. Deref under the lock,
   `cloneProbeTargetState` on the way out.
   `TestProbeReport_DoesNotHandOutTheSchedulerStatePointers`.
6. **`probe_targets:` was undocumented in the reference stack (nit).**
   `deploy/fleetd/README.md` is where C4's `warm_targets`/`warm_schedule`
   and C7b's `power`/`capital_cost` are documented for an operator; C8's
   config was not. Added, with the floor, the front refusal and the
   "model as the CELL announces it" rule stated. A `SpecsFromDefs` def
   whose argv fails to render now also logs — the kind silently falls
   back to chat, which is a guess.

**Mutation-verified sound, not changed.** Deleting the cell-side
residency check fails both gate-1 tests; treating unreported in-flight as
zero fails the scheduler guard test; leaking a verdict into
`snap.Reachable` fails the ownership-axis test; firing the degraded event
without the transition check fails the events test. `probeEvents` runs
under the same lock and against the same `prevModels` the render triggers
use. `Run` marks the attempt before measuring, so an abandoned probe
still costs budget.

**Considered and deliberately not changed.**

- **The scheduler floor and the cell cooldown are both 5 minutes**, so a
  target declared at exactly the floor has roughly every second ask
  refused by the cooldown (the ask lands slightly before the gap
  expires). The effect is a ~10-minute effective period at the most
  aggressive legal setting, and it is self-explaining — the refusal
  carries the reason and the last measurement. §3's budget table declares
  both figures as 5 minutes; changing one silently would be worse than
  the papercut.
- **`Prober.Start`'s goroutine is untracked and builds its context from
  `context.Background()`.** That is deliberate — the announce loop's ctx
  is cancelled the moment `executeCommand` returns, which is the whole
  point of the hand-off — and it cannot hang shutdown, because nothing
  waits on it. The cost is that an in-flight probe outlives the daemon by
  up to `probeTimeout` and writes its state file afterwards.
- **`Run` checks residency on the requested id while `record` keys on
  `spec.Model`.** Both are the canonical def name under every shipped
  `Specs` implementation (`Hooks`), so they cannot diverge today; a
  future alias-resolving `Specs` would need to reconcile them.
