# C18 — `vibe model try`: the membership-churn loop as one command

Status: **PR OPEN**, off `feat/c18-model-try` branched from `main` at
`c2127f3`. Backlog item 14 (the only Large-tier entry in
[fleet-control-futures.md](../fleet-control-futures.md) §2) — the weekly
loop the futures doc calls "the year's dominant toil".

Three commits: the feature, ground rule 9's adversarial self-review
(**seven findings, one blocker** — see the
[addendum](#adversarial-self-review-addendum-ground-rule-9)), and an
independent adversarial-review pass over both (**eleven findings, one
blocker** — see the
[second addendum](#adversarial-review-addendum-ground-rule-9-second-pass)).
Unit gates U1–U14 green on a full local inner loop (`go build`,
`go vet`, `go test -race ./...` ×5 on the touched packages plus
`-race ./...` whole-module, `gofmt -l .` silent, `go mod tidy`
byte-clean, `golangci-lint run` 0 issues). Twenty-three production
predicates are **mutation-verified**: reverted one at a time, each
confirmed to turn a NAMED test red by reading `go test`'s own exit
status, then restored. One fix (atomic def write) is deliberately NOT
mutation-verified and says so.

**Live gates: see [Acceptance gates](#acceptance-gates).** The rig is
committed (`scripts/fleetlab/gate-c18.sh`); whether it ran is recorded
there per gate, honestly.

## The toil

The loop, as it is actually performed today, once or twice a week:

1. a release drops; find the right quant on HuggingFace
2. `hf download …` into the library, wait 20 minutes, hope there is room
3. copy the nearest def, hand-edit the weights path, the name, the alias
4. `vibe router render`
5. `systemctl --user restart llama-swap` — which evicts whatever was
   resident and truncates anything mid-generation
6. send a request to warm it, wait out the cold start
7. eyeball a few completions, form an impression, forget to write it down
8. keep it, or delete three files and re-render

Steps 3 and 8 are where it goes wrong. A hand-copied def inherits the
incumbent's `alias:`, its `router.aliases`, its `huggingface:` block and
its `preload:` — so the candidate quietly becomes the thing every client
already points at, and the "delete three files" step is performed from
memory at 11 p.m.

Nothing in that list is a missing mechanism. The download exists
(`internal/vibe/hfdownload`), the def shape exists
(`profile.BackendDef`), the render exists (`router.Render`), the
measurement exists (C8's `modelprobe`), the idleness verdict exists
(C4's activity fold, read through C10's rule), and the two declarations
exist (C2's lease store, C11's hold). **C18 is composition.** What it
adds is the ORDER, a journal that makes the order resumable and
reversible, and the refusals.

## What C18 does not do

Written first, because the phase's honesty depends on this list being
accurate rather than aspirational (ground rule 10 applied to scope).

- **It does not run against another cell.** `--cell` must name this
  box's own cell; anything else is refused by name
  (`Runner.RefuseRemote`). Every step of a trial writes a file on the
  box that will serve the model — the weights, the def, the rendered
  llama-swap config — and fleetd is read-and-request-only by
  construction. There is no verb in this control plane that makes
  another box download 20 GB and rewrite its router config, and adding
  one is a phase, not a flag. The futures entry's `--cell <name>` is
  therefore delivered as "the cell you are on", and the gap is named
  rather than faked over SSH the design does not have.
- **It does not promote.** A trial def carries `trial: true`. Promotion
  is deleting that line and committing the def to the fleet's def
  checkout — a git operation with a human on it. Nothing here can
  perform it, and the scaffolded file says so in its own header.
- **It does not restart llama-swap.** It writes the config and then
  OBSERVES whether the cell's llama-swap adopted it. A cell running with
  `-watch-config` adopts it within ~2 poll intervals; a cell without it
  never does, and the command says exactly that, with the command to
  run. Same posture `vibe router render` has always had.
- **It has no family-template registry.** The template is `--like <def>`:
  the def already serving on this box. That is not a shortcut, it is the
  better answer — a family template encodes what a model family wants,
  while the incumbent def encodes what THIS GPU, THIS llama.cpp build
  and THIS operator's context budget want, and the second list is what
  actually decides whether the candidate loads.
- **It measures llama_server GGUF candidates only.** An `mlx_server`
  candidate is a snapshot DIRECTORY, not a file, and needs its own pull
  path; `--like <an mlx def>` is refused with that sentence.
- **It takes one sample of each side.** Not a benchmark. See
  [The comparison](#the-comparison).
- **It does not price the comparison.** C7b's energy and equivalence
  arithmetic would need a window of real traffic through the candidate,
  which a trial by definition does not have. The resource half is
  reported as weights-on-disk and `estimated_vram_gb` — real numbers
  that are not money.

## Design

### 1. The crux: applying is a declared action deferred by observation

This is C14's sentence, and C18 is the second phase to need it.

Writing the cell's llama-swap config is **disruptive**. C0's measured
gate: `-watch-config` on v239 polls at 2 s, and on reload the new config
is live for new requests immediately while the OLD upstream drains
in-flight streams under a **hardcoded 30 s** shutdown timeout, then gets
force-closed (clean EOF, never garbage). So applying a trial evicts
every resident model on the cell and truncates any generation still
running after half a minute.

The ownership axes forbid the obvious fix. "The cell looks idle so I
applied a config change" is fleetd acting on INFERRED intent, and it is
rejected — the same rejected direction C14's `sleep_schedule` had to
route around. C14's answer, verbatim here:

> a DECLARED action deferred by observation is clean; observed idleness
> INITIATING action is rejected and stays rejected.

The operator declares the trial by typing the command. Observation may
only DELAY the apply. The machinery is not re-invented: the wait is
C10's `awaitCell` with `--idle` semantics, which is the operator-facing
form of the same idleness notion C14's `quiet_for` reads —
`CellSnapshot.Activity`, derived from C4's inflight fold and the
watcher's own connection state. That brings C10's rule with it for free,
and it is the rule that matters most here:

> **Missing evidence is never idleness.** Where fleetd has no live
> `/api/events` stream to the cell it keeps waiting and prints why.
> There is no `--assume-idle` and there must not be one.

`--now` is C14's `force`, exactly: **about tonight's conditions, never
about configuration.** It skips the deferral, prints what that costs
(truncated streams past 30 s, every resident evicted), and skips nothing
structural — not the front refusal, not the remote-cell refusal, not the
disk check, not the def validation.

The test to apply to any change here is C14's: removing a guard could
only ever make the apply happen at a moment the operator already asked
for.

### 2. A trial must never become fleet catalog

"No silent rerouting, no silent fallback" is load-bearing for this
phase, and the answer is structural rather than a flag.

The front's peer map is what makes a model id routable fleet-wide. It is
produced by `router.Render` with `Cell: front`, from **fleetd's own def
checkout** (`renderLoop.renderPass` calls `LoadDefs(rl.cfg.BackendsDir)`),
filtered by presence. So on the reference fleet a def written into a
CELL's backends dir cannot reach the front render at all — different
box, different checkout.

That is true and it is not enough. `scripts/fleetlab`, a single-box
setup, and every new adopter run fleetd and a cell against the SAME
`$XDG_CONFIG_HOME/vibe/backends`. So the guard lives in `Render`:

```go
case front && def.Trial:
    warn("backend %s is a trial def (trial: true); excluded from the front render …")
    continue
```

One guard, in the one function every renderer goes through (the CLI,
`RenderToFile`, and fleetd's presence loop) — "a guard that lives in one
of four call paths is not a guard" answered by there being one path.

The exclusion is **loud**. A def that vanishes from the catalog without
a sentence is the bug, not the fix, and the same `Warnf` channel already
carries every other cell-placement exclusion.

Consequences, stated precisely because the phase brief asks for them:

- **When does the trial id become visible in the front catalog?** Never,
  while `trial: true` is set. It becomes visible when a human deletes
  that line, commits the def to the fleet's def checkout, and fleetd's
  next render pass loads it.
- **Who can route to it?** Only something talking directly to that
  cell's own llama-swap by id — which is what this package's warm and
  probe do. The front answers 404: the trial is not in any peer stanza.
- **Is it visible at all?** Yes, and deliberately. The cell announces it
  like any other catalog id, so it appears in `vibe cell status`, in
  `fleet_status` and on the fleet page as a model on that cell, and its
  tokens land in C7a's ledger under its own id. A trial you cannot see
  is a trial you forget to end.
- **How is it removed?** `vibe model try end` — see
  [Rollback](#4-rollback-is-a-path-not-a-comment).

`trial: true` also carries two validation rungs, both structural:
it requires `cell:` (an unassigned def renders wherever a render runs,
and a trial's weights are on exactly one box) and `backend.external:
true` (a non-external def is not served by the router, so the
front-render exclusion would not apply to it and the flag would be
decoration). `cloud_peer` is refused: no weights to try, no cell to try
them on.

### 3. The derivation drops every claim on the fleet

`deriveDef` copies the incumbent's serving flags and replaces the
weights. What it DROPS is the safety, and each drop is a way a candidate
could otherwise quietly change what the fleet already serves:

| dropped | why |
|---|---|
| `router.aliases` / `alias_owner` | they are the incumbent's client-facing names. Inheriting them is a render error at best and a silent re-point at worst — the exact failure "no silent rerouting" names. |
| `llama_server.alias` (reset to the trial's own name) | same, one level down. |
| `huggingface:` | a later `vibe pull` would re-fetch the INCUMBENT's file into the trial's path. |
| `mmproj`, `draft_model`, `spec_type`, `spec_draft_n_max`, `chat_template_file` | paths to another model's companion files. A candidate is a different model; loading its predecessor's projector is not a comparison. |
| `fingerprint: strict` | a candidate's hash drift must never yank models out of the front render. |
| `lifecycle.preload` | it loads the candidate at every llama-swap start, forever, including after the trial is forgotten. |
| `lifecycle.refresh` | a declaration about the fleet's steady state; a candidate has none. |

Kept: `context`, `gpu_layers`, `flash_attn`, `cache_type_k/v`, `jinja`,
`parallel`, `extra_args`, `binary`, `estimated_vram_gb`,
`lifecycle.ttl`, `lifecycle.start_timeout`. Those describe how THIS box
serves a model of this size, which is the whole reason `--like` beats a
family template.

The scaffolded file is written with a header naming the source def, the
HF coordinate, how to roll it back and how to promote it — and the
promotion sentence says **review every flag**, because they describe a
different model.

### 4. Rollback is a path, not a comment

The journal (`$XDG_STATE_HOME/vibe/fleet/model-trial.json`) records
states that name **what is true on disk**, not what the command was
doing when it stopped:

| state | true on disk |
|---|---|
| `planned` | the journal, and nothing else |
| `fetched` | the weights, at their final path, at the advertised size |
| `staged` | the trial def in the backends dir, and a render including it PROVEN to succeed |
| `applied` | the cell's llama-swap config carries the trial, and a byte-exact copy of the previous config is banked |
| `measured` | both sides probed, the comparison stored |

Re-running the command resumes from wherever the journal is. `end` walks
it backwards, from any state, including one written by a process that
died — which is the case it exists for.

The failure matrix the phase brief asks for:

| failure | what is left behind | is the fleet in its prior state? |
|---|---|---|
| download half-completes / is killed | a `.partial` (direct path) or `.incomplete` (hf CLI) sibling; journal at `planned` | **yes** — nothing about serving changed. Re-run resumes the byte range. |
| download completes short | the short file, refused by size, journal still `planned` | **yes**. `hfdownload` stages and renames, so a short file at the final path is a wrong file, not a partial one — it is refused rather than served. |
| the scaffolded def will not render | **nothing** — the def is written, the render is proven, and on failure the def is REMOVED before the error returns | **yes**, and this one is load-bearing: `router.LoadDefs` treats one bad def as a hard error for the whole directory, so a surviving unrenderable def would break every later render and announce on the box, *including the rollback's*. |
| `apply` writes the config but llama-swap never adopts it | the written config, journal at `applied`, `live: false` | **no, and it says so.** This is the no-`-watch-config` cell. The command names the restart, names its cost, and names `end`. The rollback is still owed and the journal still holds it. |
| the model loads and answers garbage | the trial, applied | detectable only as a failed or absent measurement — a 200 with bad prose is not something any probe in this repo can see, and [the report says that](#the-comparison). `end` rolls it back. |
| the cell goes away mid-trial | it is this box; the journal survives the reboot | **yes** — `vibe model try status` reports the state and `end` performs the rollback in a later process. |
| fleetd unreachable at `end` | the lease and the hold | **yes for the files.** The declarations are released FIRST and independently: a fleetd that is down must not leave a stranded config, and a config rollback that fails must not leave the warm policy suspended for four hours either. Un-released declarations expire on their own and the command says so. |
| the re-render at `end` fails for an unrelated reason | nothing | **yes** — the banked bytes are restored verbatim, and the report says which path ran. |

`end` does NOT delete the weights unless `--purge`. Re-pulling 20 GB to
re-run a trial is the toil this verb exists to remove.

### 5. The two declarations

A trial makes exactly two, and taking the right one for each is the
composition that makes the measurement mean something.

- **A plain C2 lease on the cell**, holder `model-try`. The trial is a
  consumer using the GPU, which is what the advisory store was built to
  declare: it appears in the pre-drain report, and C8's probes and the
  warm schedules already skip a leased cell, so the measurement is not
  competing with the fleet's own traffic. A lease fleetd will not accept
  **fails the command** — C10's rule that a batch which runs undeclared
  is invisible to the report that exists to protect it, applied to a
  config rewrite.
- **A C11 hold on the INCUMBENT.** Without it, fleetd's warm-target
  restore notices the cell went idle after the apply, warms the default
  model and evicts the candidate mid-comparison — the case C11 was built
  for, arriving on a fifteen-second ticker.

The hold is only taken **when there is not one already**. C11 says a
re-issue refreshes the same key, so posting over an operator's hold
would overwrite their note and their expiry, and this trial's `end`
would then delete a declaration somebody else still means. The journal
records what it took (`leased`, `held`) and `end` releases exactly that.

Both are bounded (4 h) and self-expiring; a forgotten trial does not
suspend the warm policy forever.

### 6. The comparison

Both sides are measured with C8's `modelprobe` — the same canned
deterministic request, temperature 0, 64 generated tokens, scored off
llama.cpp's own `timings` block where the engine reports one. The kind
(chat vs embed) is read off the def's **rendered argv**, C8's source,
never from the model name.

**What is controlled for:**

- the same box, the same llama-swap, the same canned prompt and token
  budget, the same probe code;
- both sides measured WARM — a warm request precedes each probe, and
  C8's cardinal rule (a probe never loads a model) means a cold model is
  refused rather than timed;
- both after the same config reload, which evicted everything, so
  neither side is carrying residency from before the trial;
- the cell is under a lease, so the fleet's own scheduled warms and
  probes are not running against it.

**What is NOT controlled for, and is printed with the number:**

- **one sample each.** C8 needs five before a verdict means anything;
  this is two. The incumbent's rolling C8 baseline is printed beside its
  sample precisely because it IS the multi-sample number.
- **the trial is measured SECOND.** On a single-GPU cell loading one
  evicts the other, so the two cannot be interleaved, and whichever goes
  second has the warmer page cache — the trial's weights were written to
  this disk minutes ago.
- **this compares DEFS, not models.** The trial inherits the incumbent's
  flags and brings its own quantization.
- **throughput is not quality.** A faster model that tool-calls worse is
  a worse model, and no probe in this repo measures that. The report
  says so in those words.

The comparative sentence is produced **only when both sides reported the
same metric**. `decode_tok_s` excludes queueing and `e2e_tok_s` does
not; putting them under one percentage would be the mistake C8 avoids by
keeping the metric in the baseline key, made on the surface a human
reads. When they differ, the report says so and prints no ratio.

C7b's precedent is the model for all of it: state what a number does not
mean, on the same screen as the number. A screen that can only render
triumph will.

### 7. Disk

The library lives on the box with the ever-growing model library, and
the failure this guards is not "the download fails" — it is "the
download succeeds and everything else stops": a full filesystem breaks
llama-swap, the activity store and the next pull at once.

- Free space is read through the same `statfs` the doctor's disk check
  uses (`daemon.FleetDiskCapacity`), so the number in the refusal and
  the number on the report are the same number.
- The pull is refused unless `free − size ≥ min_free` (default 20 GiB,
  `--min-free`).
- **An unknown size is not a small one.** A HEAD that returned no
  `Content-Length`, or failed, is a REFUSAL — absent evidence must never
  read as a healthy value, this repo's most-repeated defect class.
  `--skip-disk-check` is the operator's explicit override and the only
  way past it.
- An unreadable filesystem is likewise a refusal, not a zero.
- It refuses outright to download over a path another def already
  serves: that is the one download failure that takes a working model
  with it.

### 8. Credentials

Every request this package issues goes to **this box's own 127.0.0.1
llama-swap**, and carries no `Authorization` header — the same posture
C8's cell-side prober and C16's `ReadOwnSwapVersion` hold, for C15 §8's
stated reason: the llama-swap credential is declared per CELL in
fleetd's `hosts.yaml`, and a cell need not hold that file at all. A cell
that keys its own llama-swap gets a 401 here, reported as itself.
Inventing a credential resolver in this package is exactly what C15
declined to do, and its futures item still owns the fix.

C15's AST scan (`TestEveryLlamaSwapRequestIsAuthorized`) covers the
`fleetapi` package, where fleetd's producers live. Nothing in C18 is a
fleetd producer.

## Files

| file | what |
|---|---|
| `internal/vibe/modeltry/modeltry.go` | the journal, the steps, the refusals, the derivation |
| `internal/vibe/modeltry/report.go` | the comparison renderer, the caveats, `status` |
| `internal/vibe/modeltry/c18_test.go` | U1–U12 |
| `internal/vibe/cli/cmd_model_try.go` | `vibe model try` / `status` / `end`, the deferral, the two declarations |
| `internal/vibe/cli/c18_test.go` | the control-plane half |
| `internal/vibe/profile/backend_def.go` | `trial: true` + `validateTrial` |
| `internal/vibe/router/render.go` | the front-render exclusion |
| `internal/vibe/paths/paths.go` | `ModelTrialFile` |
| `internal/vibe/cli/root.go` | mount `model` |
| `internal/vibe/modelprobe/modelprobe.go` | `Config.ReadOnly` (review pass): the cell daemon owns the probe state file; a trial reads it and writes nothing |
| `internal/swaptest/swaptest.go` | `Cell.RemoveModel` (review pass): a config reload dropping an id is the only observable a rollback has |
| `scripts/fleetlab/gate-c18.sh` | the live rig |

No new HTTP route, no new MCP tool, no proto change, no new store on
fleetd, no new dependency, and an **empty diff for `internal/vibe/proxy`**.

## Acceptance gates

### Unit (all green, `go test -race ./...` repeated)

| # | gate | test |
|---|---|---|
| U1 | a trial def is excluded from the FRONT render, named in a warning, and still rendered on its own cell | `TestFrontRenderExcludesTrialDefsAndNamesThem` |
| U2 | `trial: true` requires `cell:` and `backend.external`, and refuses `cloud_peer` | `TestTrialDefValidation` |
| U3 | the derivation keeps the serving flags and drops all seven fleet claims | `TestDeriveDefKeepsFlagsAndDropsFleetClaims` |
| U4 | the front cell, a remote cell, a non-external `--like`, a `--like` on another cell, a name collision and a second concurrent trial are all refused | `TestPlanRefusals` |
| U5 | unknown size, unreadable filesystem and insufficient headroom all refuse; `--skip-disk-check` overrides | `TestDiskRefusals` |
| U6 | a failed render at staging leaves NO def behind and does not touch the config | `TestStageLeavesNothingBehindWhenTheRenderFails` |
| U7 | the full sequence, then `end`, restores the config byte-for-byte | `TestTrialRoundTripRestoresTheConfigByteForByte` |
| U8 | `end` falls back to the banked bytes when the re-render cannot run | `TestEndRestoresFromTheBankedBytesWhenTheRenderCannotRun` |
| U9 | applied-but-not-live is its own answer, names `-watch-config`, and refuses the measurement | `TestAppliedButNotLiveIsItsOwnAnswer` |
| U10 | both sides are probed against a real llama-swap double and the report prints its caveats; a ratio only when the metrics match | `TestMeasureProbesBothSidesAndSaysWhatItIsNot`, `TestDeltaOnlyWhenTheMetricsMatch` |
| U11 | a corrupt journal is an ERROR, never "no trial"; the journal survives a process death; the staged def round-trips through the real loader and equals `--dry-run`'s preview | `TestCorruptJournalIsNeverReadAsNoTrial`, `TestJournalSurvivesAProcessDeath`, `TestStagedDefRoundTripsThroughTheRealLoader` |
| U13 | re-running RESUMES the same trial and refuses a different one; a journal from another cell or another config path is refused by both `Plan` and `end`; a FAILED rollback keeps the journal and the banked config | `TestReRunningResumesTheSameTrial`, `TestJournalFromAnotherCellOrConfigIsRefused`, `TestFailedRollbackKeepsTheJournalAndTheBackup` |
| U12 | the trial takes a lease and a hold-on-the-incumbent, leaves an existing hold alone in both directions, refuses to apply undeclared, skips re-declaring on resume, and rolls back with fleetd gone | `TestTrialNowDeclaresTheLeaseAndTheHold`, `TestTrialLeavesAnExistingHoldAloneInBothDirections`, `TestEndReleasesOnlyWhatTheTrialTook`, `TestTrialRefusesToApplyUndeclared`, `TestTrialDeclarationsAreSkippedOnResume`, `TestEndStillRollsBackWhenFleetdIsGone` |
| U14 | the adversarial-review pass's eleven findings: `--dry-run` never rolls back a resumed trial and still closes its own journal; both declarations are on disk before the apply; the candidate's VRAM estimate is not the incumbent's; `end` waits for llama-swap to drop the id and still reports one that never went; `end` re-renders a staged trial the config still names and writes nothing when it does not; `--idle 0` and `--min-free 0` are refused; a different `--revision`/`--dest` does not resume; a re-derived def is restaged; llama-swap's body is scrubbed; the cell's shared probe state is never rewritten | `TestDryRunNeverRollsBackAnInFlightTrial`, `TestTrialDeclarationsAreOnDiskBeforeTheApply`, `TestTrialVRAMEstimateIsNotTheIncumbentsNumber`, `TestEndWaitsForLlamaSwapToDropTheTrial`, `TestEndStillReportsACatalogThatNeverLetGo`, `TestEndRerendersAStagedTrialTheConfigStillNames`, `TestEndDoesNotWriteAConfigTheTrialNeverTouched`, `TestTrialGateRefusesAZeroIdleWindow`, `TestTrialGateRefusesAZeroMinFree`, `TestResumeRefusesADifferentRevisionOrDest`, `TestResumeRestagesADefTheIncumbentChanged`, `TestWarmErrorsAreStrippedOfControlCharacters`, `TestMeasureNeverWritesTheCellsSharedProbeState` |

**Mutation-verified predicates (12).** Each production line reverted,
the named test confirmed red, the line restored:
the front-render trial exclusion; staging's def removal on a failed
render; the fetch size check; the corrupt-journal error; the not-live
measurement refusal; the alias reset in the derivation; `bankConfig`;
the unknown-size disk refusal; the existing-hold check; the resume
branch; the journal/runner agreement check; the failed-rollback
retention.

**Not** mutation-verified, and named rather than counted: the atomic
def write (REV-2). No unit test distinguishes `os.WriteFile` from
tmp+rename, because the failure is a process death between the two
syscalls. It is here on the argument, not on a red test.

### Live (`scripts/fleetlab/gate-c18.sh`)

| # | gate | status |
|---|---|---|
| L1 | against a real llama-swap v239 with `-watch-config`: stage + apply a trial def, and watch the id appear in `/v1/models` within two poll intervals — the `Live` claim is a real observation | **UNRUN** — see below |
| L2 | the same apply with a generation in flight: confirm C0's 30 s drain/force-close is what happens, so the `--now` warning is describing the real cost | **UNRUN** |
| L3 | a real fleetd + announcer: confirm the trial id appears in `fleet_status` for the cell and does NOT appear in the rendered front config; `end`, and confirm it disappears from both | **UNRUN** |
| L4 | `end` from each of the five journal states, with the config compared byte-for-byte against a pre-trial copy | **UNRUN** |
| L5 | the magnitude gate: a real GPU, a real 20 GB pull, a real cold start on both sides, and a report a human agrees with | **UNRUN — needs metal.** CPU models are not GPU models (the plan README's standing qualification): nothing in a lab exercises a 6–10 minute cold start or VRAM pressure, and this phase's whole output is a magnitude. |

**L1–L4 were NOT RUN, in either session** — and the review pass found
that the rig as first committed could not have been: it named an
incumbent def and a candidate directory the lab has never created, ran
under fleetd's config rather than the cell's, and pointed `--dest` at
the incumbent's own weights (REV2-8). It has been rewritten against what
`scripts/fleetlab/lab.sh` actually writes and its premises were checked
against a real lab tree, but the corrected rig has **not been executed
either**, so every row above stays UNRUN. A gate claim is a claim about
a mechanical run. The reason is the one C16 hit and
[futures item 15](../fleet-control-futures.md) names: `scripts/fleetlab`
binds fixed ports (9600-9799, upstreams 5980-6019) with no offset knob,
this phase was built alongside a sibling phase in the same checkout, and
`down`'s sweep is anchored partly on that shared upstream range — so the
second instance is entitled to kill the first's processes. A wave-1
agent recorded its gate UNRUN for exactly this and that was the right
call. **A gate claim is a claim about a mechanical run** (ground rule
10); these were not run, and the rig is committed so the next person
with a free lab runs them rather than re-derives them.

L5 is a different kind of unrun: it needs metal, not a time budget.


## Adversarial self-review addendum (ground rule 9)

Seven findings against the feature commit, all fixed in the review
commit. Ranked by what they would have cost.

**REV-1 (blocker) — re-running could not resume.** `Plan` is the only
entry point and it REFUSED when a journal existed, so the phase's
central promise ("every step is journalled; re-run resumes") was
unreachable code. The exact case: the download finishes, the idle wait
times out at `--wait 2h`, the operator re-runs in the morning — and gets
"a trial is already in flight", with no way forward except `end`, which
throws the 20 GB pull away. `Plan` now resumes when the request names
the SAME trial and refuses when it names a different one, saying which
is open. Resuming re-derives the def from the CURRENT incumbent, so a
def edit made between the fetch and the apply is picked up rather than
silently serving a stale derivation.

**REV-2 — the trial def was written non-atomically.** `router.LoadDefs`
treats ONE unparseable def as a hard error for the whole directory, so a
torn write breaks every render and every announce on the box until
somebody finds the file — including the render `end` needs. The staging
step already went to great lengths to avoid exactly this (it proves the
render and removes the def if it fails); writing the file with a plain
`os.WriteFile` left the same hole open one syscall earlier. Now
tmp+rename, like every other file this repo writes.

**REV-3 — a comment described the opposite of the code, and the
behaviour it described was destructive.** `measureOne`'s comment claimed
`rebaseline=true`; the call passed `false`. `false` is correct — `true`
would WIPE the incumbent's C8 rolling window on this box, which is the
only multi-sample number the report prints and the one thing that makes
a single sample interpretable. A future agent "fixing the code to match
the comment" would have deleted it. This is the comment-rot failure
AGENTS.md warns about, in its most expensive form.

**REV-4 — `end` deleted the journal and the banked config even when the
rollback had FAILED.** Both were removed unconditionally on the way out,
including on the path that had just reported "the config may still
contain the trial". That turns a recoverable half-rollback into a
permanent one and makes `status` report no trial on a box still serving
it. A failed rollback now KEEPS both and says so; only a clean one
closes.

**REV-5 — an unverified rollback read as a clean one.** `stillListed`
correctly reports an unreadable catalog as "not listed, and here is why
I could not check", but the CLI printed the note only when the model WAS
still listed. So the one case where the rollback is unproven printed
nothing at all — absent evidence rendering as a healthy value, this
repo's most-repeated defect class, shipped again. The note now prints
whenever there is one.

**REV-6 — the journal was trusted to describe this box.** `end` renders
with `r.opt.ConfigPath` but restores to `t.ConfigPath`, and nothing
compared them. Editing `fleet.cell` or the proxy port mid-trial (both
one line in `config.yaml`) would make the rollback re-render for the
wrong cell and drop banked bytes onto a file they never came from.
`agreesWithJournal` now refuses on either mismatch, naming both values
and how to recover, and it guards `Plan` and `end` alike.

**REV-7 — two smaller ones.** `--dry-run` left the journal open, so the
very next real invocation resumed a trial the operator never started —
and if they had changed a flag between the two, it would have been
refused as a different one; the dry run now closes the journal through
the real rollback path. And `awaitCatalog` built its deadline from the
injectable clock while sleeping on the wall clock, which spins forever
under a frozen `Now`; both are `time.Now` now, with the reason written
down.

## Adversarial-review addendum (ground rule 9, second pass)

An independent adversarial pass over the merged feature + self-review
commits. **Eleven findings, one blocker.** All fixed in a third commit;
every production predicate below was mutation-verified — reverted one at
a time, the NAMED test confirmed red by reading `go test`'s own exit
status, then restored.

**REV2-1 (BLOCKER) — `--dry-run` performed a full rollback of an
in-flight trial and reported that nothing had changed.** REV-1 taught
`Plan` to RESUME an open journal; REV-7 taught `--dry-run` to close "the
journal Plan opened" by calling `End`. Composed, they are a rollback
wearing the word *dry*: on a trial already `applied`, `vibe model try
<same args> --dry-run` removed the trial def, re-rendered the cell's
llama-swap config — which `-watch-config` reloads, evicting every
resident model and truncating anything past 30 s — deleted the journal,
and printed *"nothing was downloaded, nothing on this box changed, and
no trial is open."* The operator's next `status` reports no trial on a
box whose models were just evicted. `End` works from any state, so the
decision cannot live in `End`: `Trial.Resumed` (process-local, never
serialized) records which of the two `Plan` did, and
`Runner.DiscardIfFresh` closes only a journal this invocation opened.
Pinned by `TestDryRunNeverRollsBackAnInFlightTrial`, which drives the
real sequence to `applied` and then dry-runs it.

**REV2-2 (major) — the two declarations were never written to the
journal until the apply had already succeeded.** `trialDeclareAndWait`
set `t.Leased` / `t.Held` in memory; the next `save` was inside `Apply`,
*after* `bankConfig` and the render. Anything that failed in between —
an unwritable config dir, a def someone else broke, a killed process —
lost both records. Then `vibe model try end` releases nothing (it
releases exactly what the journal says the trial took), the lease and
the C11 hold sit for four hours suppressing the fleet's warm policy
invisibly, and the re-run parks forever: `--unleased` skips the trial's
own *lease* holder but not the reserved `hold` holder, so the trial
deadlocks against its own declaration. Each declaration is now persisted
the moment it is taken, and a journal write that fails fails the command
with the manual release. `TestTrialDeclarationsAreOnDiskBeforeTheApply`
reads the journal back the way a later process would.

**REV2-3 (major) — the candidate's VRAM estimate was the incumbent's
number.** `deriveDef` copied `estimated_vram_gb`, and `measureOne` reads
it back off the def to fill the report's *estimated VRAM* row under the
candidate's name. So the one row that answers "and how much bigger is
it" — the row the phase doc introduces with *"a model that is 15% faster
and 40% larger has not obviously won"* — was equal on both sides by
construction, for every trial, forever. A hand-measured claim about
another model's weights presented as a measurement of this one; the
repo's absent-evidence-as-a-healthy-value class, in the report the whole
phase exists to print. Not inherited now, and it renders "not declared".
An external def is never VRAM-preflighted, so nothing else wanted it.

**REV2-4 (major) — `end` told the operator to restart llama-swap on
every successful rollback.** `stillListed` read `/v1/models` ONCE,
microseconds after the re-render. A real `-watch-config` llama-swap
notices a config change on its own ~2 s poll (C0's measured gate), so
the read always found the trial still listed and printed *"llama-swap has
not reloaded — restart it to finish the rollback"*, which evicts
everything resident. `Apply` already knew this and polls; the rollback
side did not. It now polls the same `CatalogWait` budget for the id to
LEAVE, and an unreadable catalog is still reported as unverified rather
than as gone. A warning that fires on the success path is one nobody
reads on the failure path.

**REV2-5 (major) — `--idle 0` silently disabled the deferral that is the
whole phase.** `awaitConds.evalIdle` only runs when the window is
positive, and C18 never called C10's `validateAwaitFlags`. So
`--idle 0` (or any negative duration) applied the config the moment the
cell answered — which is what `--now` does, minus `--now`'s sentence
about truncated generations and evicted residents — while printing
*"waiting for bravo to be observed idle for 0s (fleetd's evidence, not a
guess)"*. Skipping the wait is spelled `--now`, exactly once.
`validateTrialGate` refuses it before the twenty-minute pull, C10's
issue-the-refusal-first rule.

**REV2-6 (major) — a `staged` rollback left the cell serving the
trial.** `end` re-rendered only from `applied`, on the reasoning that
`staged` never wrote the config. That is a promise about what the TRIAL
did, not about what anyone else did: an operator (or `vibe start`) who
runs `vibe router render` while a trial is staged puts the trial into
the cell's llama-swap config, and then `end` removes the def, re-renders
nothing, and reports *"trial closed"* while the id is still being
served — with its def gone, so the next render cannot even reproduce it.
`end` now re-renders from `staged` too, gated on the config actually
declaring the trial as a `models:` key, so the ordinary path still writes
nothing and cannot create a config on a box that has none
(`TestEndDoesNotWriteAConfigTheTrialNeverTouched` pins that half).

**REV2-7 (major) — the trial's prober rewrote the cell daemon's probe
state.** `Measure` builds a `modelprobe.Prober` on
`paths.CellProbeFile()`, which until C18 had exactly one writer: the cell
daemon's own prober. The file is a whole-file rewrite from in-memory
state (`history.go`'s shape), so a second process holding it for the
twenty minutes two cold loads take discards every baseline sample and
every 96/day budget entry the daemon recorded in that window — including
the rolling window the report calls *the only multi-sample number on the
page*. `modelprobe.Config.ReadOnly` loads and never writes back; the
trial reads the incumbent's window to print it and leaves the file
byte-for-byte as it found it. `TestMeasureNeverWritesTheCellsSharedProbeState`
seeds the file through a real writable prober so the baseline key is
C8's, not the test's idea of it.

**REV2-8 (major, rig) — `scripts/fleetlab/gate-c18.sh` could not run.**
The committed rig was gated UNRUN and shipped as "the next person with a
free lab runs them rather than re-derives them"; it named an incumbent
def (`lab-chat-b`) and a candidate directory (`$LAB/models/`) that
`scripts/fleetlab/lab.sh` has never created, so it exits 2 at its own
precondition check. Verified mechanically against a real lab tree at
`/tmp/fleetlab` (defs are `lab-chat`, `lab-embed-{a,b,c}`; there is no
`$LAB/models`). Three more, each of which stops the run one step later:
it exported gl.sh's `XDG_CONFIG_HOME=$LAB/etc`, which is **fleetd's**
config and declares no `fleet.cell` at all, so `vibe model try` refuses
before it starts; `--dest $(dirname $CANDIDATE)` with the candidate
being the incumbent's own GGUF resolves to the weights path a def
already serves, which `Plan` refuses by design; and step 4 sent a chat
completion to bravo's only def, an **embedding** server. Rewritten
against what `lab.sh` actually writes: bravo's own `etc-bravo` config,
`lab-embed-b` as the incumbent, a copied candidate under a name no def
serves, `PORT`/`SPORT` derived from the lab rather than hardcoded, and
the embed request C18's own kind detection would issue. It also now
re-applies lab.sh's `startPort` rewrite after every render and refuses to
continue if it does not take — `vibe router render` emits
`startPort: 5800`, which gl.sh names as production's upstream range on
this box, and `try`'s apply is a render. **Still UNRUN** (see the gate
table).

**REV2-9 (minor) — the resume identity ignored `--revision` and
`--dest`.** `Plan` compared def name, repo, file and `--like`. Re-running
with a corrected `--revision` therefore RESUMED the old one silently and
served the revision the operator had just replaced; a different `--dest`
kept the weights at the old path. Both are part of what the trial IS, and
both are now in the comparison, with the open trial's revision and
weights path named in the refusal.

**REV2-10 (minor) — resuming at `staged` discarded the re-derivation.**
REV-1 states that resuming re-derives the def from the CURRENT incumbent
"so a def edit made between the fetch and the apply is picked up rather
than silently serving a stale derivation". `Stage` returned early on
`staged`, which is precisely the state between the fetch and the apply,
so the re-derived def was computed and thrown away. `Stage` now compares
the derived bytes to the file, restages when they differ, and restores
the previous def if the re-derived one does not render — a `staged` trial
already has a def the box renders, so failing the way a first stage fails
would leave the journal describing a file that is not there.

**REV2-11 (minor) — llama-swap's response body reached the terminal
unscrubbed.** A non-200 warm puts 512 bytes of the far side's body into
`Measurement.Note`, which is SAVED to the journal and re-printed by every
later `vibe model try status`. `cli.termSafe` exists for exactly this and
could not be applied here, because the string is stored before any
renderer sees it; `printableSnippet` holds the same rule at the point of
capture.

**Mutation-verified predicates (11).** `DiscardIfFresh`'s resumed guard
and its call site (two mutations); both declaration saves; the dropped
`estimated_vram_gb`; `stillListed`'s poll deadline; `end`'s staged
re-render, in both directions (it must fire when the config names the
trial and must NOT fire when it does not); the `--idle` refusal; the
`--min-free 0` refusal; the resume identity's revision and dest terms
(two mutations); `Stage`'s restage branch; `printableSnippet`;
`modelprobe`'s `ReadOnly`.

**Also looked at and found sound**, so the next pass need not re-derive
it: the front-render exclusion covers every renderer because there is one
`Render`; `deriveDef` drops all seven fleet claims and `router.ModelCmd`
renders `${PORT}` unconditionally, so a pinned `port:` on the incumbent
cannot collide; `resolveAliases` skips a self-alias, so the trial's
`alias: <own name>` is inert; the C15 AST scans are package-scoped to
`fleetapi` and `fleetmcp` and C18 is neither, and its unauthenticated
loopback requests match C8's and C16's cell-side posture; `Apply` banks
before it renders; `End` keeps the journal and the bank on a failed
rollback; the `warmCtx`-style bound on every request is present
(`warmTimeout`, the 5 s catalog timeout) and no goroutine outlives the
command.

**One thing named rather than fixed.** A trial def is a new untracked
file in the fleet's def checkout for the trial's duration, so C13's
`defs.parity` reports that cell's tree DIRTY while a trial is open. That
is true and it is what dirty means; a future phase that wants doctor to
say "dirty, and it is a trial" needs `defs_dirty` to carry a reason,
which is a wire change this branch is not making.

## For the reconciliation pass

Everything below belongs in a shared doc this branch may not touch
(`AGENTS.md`, `docs/design/fleet-control-plan/README.md`,
`docs/design/fleet-control.md`).

### For `AGENTS.md`, after the C17 block

> - **`vibe model try` (fleet-control C18).** `internal/vibe/modeltry`
>   (the journal, the steps, the refusals, the derivation),
>   `internal/vibe/cli/cmd_model_try.go` (the deferral and the two
>   declarations), `profile.BackendDef.Trial`, and one exclusion in
>   `router.Render`. It adds no HTTP route, no MCP tool, no proto
>   change, no fleetd state and no dependency.
>   - **A trial def never reaches the FRONT render**, and the guard is
>     `case front && def.Trial` in `router.Render` — the one function
>     every renderer goes through (the CLI, `RenderToFile`, fleetd's
>     presence loop). On the reference fleet it is unreachable (fleetd
>     renders from its OWN def checkout, and the trial def is in the
>     cell's); it is exactly reachable on a single box where fleetd and
>     the cell share `backends/`, which is what `scripts/fleetlab` and
>     every adopter have. The exclusion is LOUD: a def that vanishes
>     from the catalog without a sentence is the bug, not the fix.
>     `trial: true` requires `cell:` and `backend.external: true` and
>     refuses `cloud_peer`. **Promotion is deleting one line** — nothing
>     in vibe can do it, because entering the fleet catalog is a change
>     to a shared git repo with a human on it.
>   - **Applying is C14's sentence again**: a declared action deferred
>     by observation. The write is DISRUPTIVE (C0 measured it:
>     `-watch-config` reloads, the old upstream drains under a hardcoded
>     30 s, then force-closes), so the wait is C10's `awaitCell` with
>     `--idle` — the SAME idleness notion, not a second one — and C10's
>     "missing evidence is never idleness" comes with it. `--now` is
>     C14's `force`: about tonight's conditions, never about
>     configuration; it skips the deferral, prints its cost, and skips
>     nothing structural.
>   - **The derivation drops every claim on the fleet** and keeps only
>     the serving flags: `router.aliases`/`alias_owner`, the
>     `llama_server.alias` (reset to the trial's own name),
>     `huggingface:`, `mmproj`/`draft_model`/`spec_*`/
>     `chat_template_file`, `fingerprint: strict`,
>     `lifecycle.preload`/`refresh` all go. Inheriting an alias is a
>     silent re-point of every client already using it, which is the
>     failure "no silent rerouting" names.
>   - **Two declarations, and the right one for each**: a plain C2 lease
>     on the cell (holder `model-try` — the trial is a CONSUMER, and
>     C8's probes and the warm schedules already skip a leased cell) and
>     a C11 HOLD on the INCUMBENT (so the warm-target restore does not
>     reload it the moment the cell looks idle and evict the candidate).
>     The hold is taken only when there is not one already, because C11
>     re-issue refreshes the same key and `end` would then delete an
>     operator's own; the journal records what it TOOK and releases
>     exactly that. A lease fleetd refuses FAILS the command (C10's
>     undeclared-batch rule applied to a config rewrite).
>   - **The journal states name what is TRUE ON DISK**, not what the
>     command was doing — `planned` / `fetched` / `staged` / `applied` /
>     `measured` — which is what makes a killed run resumable and
>     reversible from a later process. `Load` treats a corrupt journal
>     as an ERROR, never as "no trial": it is the only record of what a
>     rollback has to undo. `end` releases the declarations FIRST and
>     independently of the file rollback, then re-renders, falling back
>     to a byte-exact banked copy when the render fails for an unrelated
>     reason. Weights survive unless `--purge`.
>   - **Staging proves the render BEFORE the def survives it**:
>     `router.LoadDefs` treats one bad def as a hard error for the whole
>     directory, so an unrenderable trial def left in place would break
>     every later render and announce on the box, including the
>     rollback's.
>   - **Absent evidence is a refusal here too**: an unknown HuggingFace
>     size is not a small file and an unreadable filesystem is not an
>     empty one — both refuse the pull, with `--skip-disk-check` as the
>     only override. It also refuses to download over a path another def
>     already serves.
>   - **The report states what it is not** (C7b's rule): one sample each
>     against C8's five-sample minimum, the trial measured SECOND with
>     the warmer page cache, defs-not-models, and throughput-is-not-
>     quality. The comparative ratio is printed ONLY when both sides
>     reported the same metric — `decode_tok_s` excludes queueing and
>     `e2e_tok_s` does not.
>   - Cell-local by construction: `--cell` must be this box's own cell.
>   - **`--dry-run` closes only a journal IT opened.** `Plan` resumes an
>     open trial and `End` works from any state, so "close the journal
>     Plan opened" performed a full rollback of somebody's in-flight
>     trial — def removed, cell config re-rendered, every resident model
>     evicted — under a flag whose whole promise is that nothing
>     happens, and then printed "nothing on this box changed". The
>     decision cannot live in `End`; it is `Trial.Resumed` (never
>     serialized) plus `Runner.DiscardIfFresh`.
>   - **A declaration is journalled the moment it is taken**, not when
>     the sequence finishes. `end` releases exactly what the journal
>     says the trial took, so a lease or hold that lived only in the
>     process's memory is one nothing ever releases — four hours of a
>     suppressed warm policy, plus a re-run that parks forever because
>     `--unleased` skips the trial's own LEASE holder and not the
>     reserved `hold` one.
>   - **`estimated_vram_gb` is NOT inherited.** It is a hand-measured
>     claim about the incumbent's weights, and the report prints it in a
>     column headed by the candidate's name; copying it made the
>     resource half of the comparison equal by construction on every
>     trial. Undeclared renders as "not declared".
>   - **Both sides of the catalog observation POLL.** `-watch-config`
>     notices a write on its own ~2 s cadence, so a single read taken
>     right after a render answers about the config that was there
>     before it: `Apply` waits for the id to APPEAR and `end` waits for
>     it to LEAVE, on the same budget. A restart warning that fires on
>     every successful rollback is one nobody reads on the failure path.
>   - **`--idle 0` is refused, not honoured.** `awaitConds.evalIdle`
>     only runs on a positive window, so a zero silently skipped the
>     deferral that is the whole phase, without `--now`'s sentence about
>     what that costs. Skipping the wait is spelled `--now`, once.
>     `--min-free 0` is refused for the same shape of reason: zero is
>     how the flag spells "unset", so it restored the default floor
>     instead of removing it.
>   - **A `staged` rollback re-renders when the config still names the
>     trial.** `staged` promises the TRIAL never wrote the config, not
>     that nobody else ran `vibe router render` in between; after that,
>     removing the def alone leaves the cell serving a trial `end` has
>     just reported closed, with no def left to reproduce it. Gated on
>     the config actually declaring the id, so the ordinary path still
>     writes nothing and cannot create a config on a box with none.
>   - **The trial's prober is READ-ONLY on the cell's probe state**
>     (`modelprobe.Config.ReadOnly`). That file is a whole-file rewrite
>     from memory and the cell daemon's prober is its owner; a second
>     process holding it for the twenty minutes two cold loads take
>     discards every baseline sample and budget entry recorded meanwhile
>     — including the rolling window the report exists to print.
>     Every step writes a file on the box that will serve the model and
>     fleetd is read-and-request-only; cross-cell `try` is a phase, not
>     a flag.
>   - Requests go to this box's own 127.0.0.1 llama-swap with **no**
>     `Authorization` header — C15 §8's cell-side posture, same as C8's
>     prober and `ReadOwnSwapVersion`. Do not invent a credential
>     resolver here; C15's futures item owns it.

### For `docs/design/fleet-control-plan/README.md`

Table row:

```
| [C18](c18-model-try.md) | `vibe model try`: the churn loop as one command | ~1400 lines | C0, C2, C4, C8, C10, C11, C14 (composition) | PR open; feature + self-review + adversarial-review commits (7 + 11 findings, one blocker each); unit gates U1-U14 green, 23 predicates mutation-verified; **live gates L1-L4 UNRUN — the rig as first committed could not run and has been corrected but not executed; L5 needs metal** |
```

And a paragraph in the phase-notes prose:

> C18 (2026-08-05) is backlog item 14, the only Large-tier entry, and it
> is the second phase after C14 to be built entirely around one
> sentence: **a declared action deferred by observation is clean;
> observed idleness initiating action is rejected.** Applying a def edit
> rewrites a cell's llama-swap config, which `-watch-config` reloads —
> evicting every resident model and truncating any generation still
> running at 30 s — so the apply had to be deferred, and the deferral
> reuses C10's `awaitCell --idle` rather than inventing a second notion
> of idle. Three rules it carries forward. **Promotion is deleting one
> line**: a trial def is marked `trial: true`, `router.Render` excludes
> those from the FRONT render, and nothing in vibe can promote one,
> because entering the fleet catalog is a change to a shared git repo
> with a human on it. **The incumbent def is the better family
> template** — a family template encodes what a model family wants, the
> incumbent encodes what THIS GPU and THIS build want, and the second
> list is what decides whether the candidate loads — which is why one
> flag (`--like`) supplies both the template and the comparison. And
> **the journal states name what is true on disk**, which is what makes
> a killed run resumable and, more importantly, reversible from a later
> process; `end` works from any of the five, releases only the
> declarations the trial itself took, and falls back to a byte-exact
> banked config when the re-render fails for a reason unrelated to the
> trial. Its honest boundary is that `--cell` must name the box you are
> on: every step writes a file where the model will run, fleetd is
> read-and-request-only, and cross-cell `try` is a phase rather than a
> flag. Its adversarial pass found the phase's own composition biting
> back twice: the self-review's two fixes met each other and turned
> `--dry-run` into a silent full rollback of an in-flight trial (`Plan`
> resumes; `End` works from any state; "close the journal Plan opened"
> does not distinguish), and `Measure` made a short-lived second writer
> of C8's cell-side probe file, whose whole-file rewrite discards every
> baseline sample the cell daemon recorded while a trial runs. Both are
> the same shape: a component that was correct alone acquiring a second
> caller.

### For `docs/design/fleet-control.md`

Nothing. C18 adds no state axis, no display state, no route and no
protocol.

### For `scripts/fleetlab/README.md`

> `gate-c18.sh` runs against **bravo**, the only lab cell with a full
> vibe cell daemon and therefore the only one with a `fleet.cell:` for
> `vibe model try` to place a trial on. It must export bravo's own
> `XDG_CONFIG_HOME=$LAB/etc-bravo` — gl.sh's `$LAB/etc` is *fleetd's*
> config, which declares no `fleet.cell` at all. bravo's only def is
> `lab-embed-b`, an embedding server, so the trial is an embed trial and
> every request in the rig is `/v1/embeddings`. And because `try`'s
> apply IS a `vibe router render`, it writes `startPort: 5800` —
> production's upstream range on this box — so the rig re-applies
> lab.sh's rewrite after every render and exits rather than continue if
> it does not take.

### Cross-branch note

This branch was developed alongside a sibling wave-2 phase in the same
checkout, which is how it learned the shape of futures item 15 (the
fleetlab port offset) from the other direction: two agents cannot run
the lab at once, and the second one honestly records UNRUN. If the
sibling phase also lands a `paths.go` entry, the two are additive and
adjacent; there is no semantic conflict.
