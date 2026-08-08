# C24 — drain where reclaim happens

Status: **PR OPEN**, off `c24-drain-where-reclaim-happens` branched from
`main` at `cb8b336`. Two commits: the packaging + the rules it needs to
be safe, and ground rule 9's adversarial self-review (§8b — five
findings, all fixed, two of them the phase's own headline defect reached
by routes the feature commit had not looked at). Backlog item 5
([fleet-control-futures.md](../fleet-control-futures.md) §2):

> **Drain where reclaim happens** — a documented Steam launch-option /
> desktop-shortcut wrapper for `vibe cell drain --until-exit --`, plus an
> `ExecStopPost` hook on cell units that best-effort records out-of-band
> stops as intent. One line of packaging that decides whether the intent
> axis stays trustworthy.

The mechanism has existed since C2: `drainUntilExit` drains, runs a
command, and resumes on any exit. What was missing is the line an
operator pastes into Steam. This phase is that line, its
desktop-shortcut and shell forms, and the unit drop-in for the reclaims
that will bypass all three.

**It is not only packaging, because the second half turned out to be one
heartbeat away from being an actuator.** A cell unit's stop hook that
POSTs `{"state":"drained"}` writes a registry intent REQUEST; fleetd
hands a request to an announcing cell as `desired_intent`; and
`fleetannounce.reconcile` answers a drained desired_intent by **running
`cell_cmds.drain`**. The hook meant to describe a stop would have caused
one, on the first heartbeat after the box came back, through a path
nothing in the hook can see. §3 is that finding; §4 is the reserved
reason that closes it and the three other holes beside it.

---

## 1. The two paths are asymmetric, and the docs push the first

| | file | what the fleet learns |
|---|---|---|
| declared | `deploy/cell/vibe-reclaim.sh` | reason, ETA, and a deterministic resume — "gaming, since 21:04, eta 23:00" |
| recorded | `deploy/cell/vibe-cell-intent.sh` | that the unit stopped, and when. **Never why.** |

The futures entry's own diagnosis is the reason for the asymmetry:
*"Every reclaim that bypasses `vibe cell drain` deposits `DRAINED?`
residue; fifty betrayals later the intent column isn't trusted."* The
wrapper removes the betrayals for the case that produces most of them —
a human taking the GPU for a game — because it puts the verb in the
launcher, where the reclaim actually happens. The hook is for what is
left, and what is left is exactly the class of stop where nobody
declared anything: `systemctl stop`, a reboot, a crash.

So the hook must never pretend to more than it has. It records a fact
with a timestamp. Every surface in this control plane that answers *why
is this box down?* — C9's always_on alarm, `vibe fleet doctor`'s intent
hygiene — behaves exactly as it did before the record existed (§4).

## 2. The wrapper

```
Steam ▸ Properties ▸ Launch Options:  /usr/local/bin/vibe-reclaim.sh %command%
.desktop:                             Exec=…/vibe-reclaim.sh /usr/bin/blender %F
shell:                                vibe-reclaim.sh ./train.sh
```

Three decisions worth writing down:

- **`exec`, not a call.** The deferred resume lives inside the `vibe`
  process. A shell sitting between Steam and vibe is one more thing that
  has to forward SIGINT, and it does not.
  `TestC24WrapperExecsSoSignalsReachTheResume` asserts the launcher's
  child pid IS the wrapped process, then signals it and checks the
  signal arrived.
- **`--yes`.** A lease is advisory by design (§5 of the design doc: "this
  reports, it does not block"). Without `--yes`, `drainCell` refuses on
  a non-tty — so a lease somebody left open on Tuesday stops the game
  launching on Friday, and the wrapper comes back out of the launch
  options that evening. The pre-drain report still prints what was
  stranded; it lands in the launcher log.
- **The child's exit status is the wrapper's.** This needed a code
  change: `drainUntilExit` returns `exitCodeError`, `cli.ExitCode` did
  not know about it, and `main` printed `vibe: <cmd> exited with status
  3` and exited **1**. A launcher, a `.desktop` `Exec=` and every `&&`
  in a shell read that code; a wrapper that flattens every failure to 1
  is not a wrapper. Signal deaths (`ExitCode() == -1`) still fall
  through to the ordinary error line.

## 3. The finding: a recorded stop is one heartbeat from being a drain command

The naive hook is `curl -d '{"cell":"gpu-cell","state":"drained"}'`. Trace
what that becomes on an announcing cell (C3), which is the shape the
fleet is moving toward:

1. the box reboots; llama-swap comes back; the vibe daemon announces.
2. `handleAnnounce` finds a registry intent entry newer than the cell's
   echo and returns it as `desired_intent` — this is the conflict rule
   working exactly as designed.
3. `fleetannounce.reconcile` sees a drained desired_intent, and for a
   cell whose local state is serving it **runs `cell_cmds.drain`**.

The serving stack the hook merely wanted to describe is stopped, by the
control plane, without a human anywhere in the loop. Nothing in the hook
— and nothing in the unit file — is visible from that call site.

`TestC24StopRecordIsNeverHandedBackAsACommand` gates it, and its first
half is a **control**: an ordinary declared drain must still be handed
back, or the test would pass against a fleetd that hands nothing back to
anyone.

## 4. What closes it: one reserved reason, four refusals

`fleetapi.StopIntentReason = "stopped out of band"` marks a record whose
author is the unit. It is a reserved reason rather than a new field for
C14's reason — a stop is an ordinary drained entry on axis 2 and adds no
state to the design's table — and it buys four behaviours a human's
declaration must not get:

| # | rule | where | what it prevents |
|---|---|---|---|
| 1 | never handed back as `desired_intent` | `announce.go` | §3: the recorder actuating |
| 2 | loses to the cell's own **drained echo** | `announce.go` | the wrapper's declared drain being restamped "stopped out of band" — a drained echo is only ever produced by a declared drain at the box, and the box outranks the record |
| 3 | never counts as pending | `presence.go` | "requested, awaiting cell ack" for a stop nobody requested, and `intent.hygiene` yellow every night the box is off (C14's permanent-WARN shape) |
| 4 | does not explain an absence | `notify.go`, `doctor.go` | a crash-stopped `always_on` cell silencing its own alarm — `systemctl stop` and a crash fire the same `ExecStopPost` |
| 5 | never overwrites an existing declaration | `intent.go` | the C14 case: a scheduled suspend records `{drained, "asleep per sleep_schedule", eta 07:15}` and then takes the box down *through this unit stop*. Added by the self-review (§8b R-5) |

Rule 4 is the one to argue with, so: the record adds the WHEN and the
WHAT. The WHY is still missing, and a fleet whose heavy cell just died
must page exactly as loudly as it did yesterday. `vibe fleet doctor`
therefore keeps the cell in its *undeclared state* bucket, now with the
stop's timestamp and a fix line that names the wrapper.

The start half (`ExecStartPost`) is the same reserved reason on
`state: serving`, and it is deliberately not the ordinary clear:

- it retires **only** a stop record. A human's declared drain is left
  exactly where it is (and handed back to the hook so the journal can
  say so), because starting the unit inside a declared reclaim must not
  cancel the reclaim.
- it stores **nothing** for an announcing cell. The ordinary serving path
  stores a serving REQUEST, which rides `desired_intent` to the cell,
  where reconcile runs `cell_cmds.resume`. Same trap as §3, other verb.

Both halves are therefore structurally incapable of producing a command,
which is a stronger statement than "the script does not run one".

**Why the start half exists at all** (the futures entry asks only for
`ExecStopPost`): without it the record outlives the stop. The box comes
back, the record still says drained, and the fleet renders INCONSISTENT
until a human clears it — a stale drain, which is the exact failure this
phase exists to prevent. The unit's lifecycle declares the axis in both
directions or in neither.

## 5. What the hook writes, and what it writes when it cannot reach fleetd

On a stop, one bounded POST to `/api/fleet/intent`:

```json
{"cell":"gpu-cell","state":"unit_stopped"}
```

On a start, `{"cell":"gpu-cell","state":"unit_started"}`. The unit sends
no reason and no ETA — it knows that it stopped and when, and a hook
that could send a reason could dress a stop up as a declaration. fleetd
stamps `StopIntentReason` itself and drops anything else the wire
carried.

**The verbs are states, not a reason on `drained`/`serving`, and that is
the version-skew guard** (found by the self-review, §8b R-1). A pre-C24
fleetd handed `{"state":"drained","reason":"stopped out of band"}`
records an ordinary drained REQUEST — §3, live, against a front that is
one release behind. An unknown *state* is a 400 on every build this
endpoint has ever had, and a 400 lands in the hook's give-up path. The
same property makes a typo in the hook harmless: a misspelled state is
refused, where a misspelled reason would have recorded a human-looking
drain that all four rules let through.

**When it cannot reach fleetd it writes nothing at all.** No file, no
retry, no cached value, no "serving". The intent store keeps whatever it
had — after an out-of-band stop that is *no entry* — the fleet renders
`DRAINED?`, and that question mark is the truth. The hook says so on
stderr, into the unit's journal, and **exits 0**.

Exit 0 on every path is not laziness. A non-zero `ExecStopPost` puts the
unit into `failed`, where `OnFailure=` units fire: the recorder would
become a trigger through systemd rather than through fleetd. The drop-in
prefixes both hooks with `-` as well; either alone would do, and this is
the one place in the phase where belt and braces is cheap.

Everything else that can go wrong lands in the same place: no `curl`, an
unreadable or empty token file, an unset variable, a cell name that is
not a plain `[A-Za-z0-9._-]` name (it goes into a JSON document
unescaped, so it is validated rather than quoted), a `VIBE_INTENT_TIMEOUT`
over the 30s ceiling, an HTTP 400 for a cell fleetd has never heard of.
All of them: say why, record nothing, exit 0.

The bound is 1s to connect and `VIBE_INTENT_TIMEOUT` (default 3s) in
total, one request, no retries — because this runs inside the unit's stop
sequence and a hook that hangs is a shutdown that hangs.
`TestC24HookIsBoundedAgainstAHungFleetd` parks a server that accepts the
connection and never answers, which is the case `--connect-timeout`
alone does not cover.

## 6. Where the fragment lives, and why

`deploy/cell/` — a new directory beside `deploy/fleetd/` and
`deploy/front/`.

`deploy/` is already this repo's home for *artifacts installed on a
host*: a Dockerfile, two compose stacks, an example front config, a
README per role. A launcher wrapper, a hook script and a systemd drop-in
are exactly that, and the cell was the one role with no directory.
`scripts/` is the other candidate and is the wrong one: everything under
it is an in-repo rig that runs gates against the lab
(`scripts/fleetlab/`, `scripts/upgrade/ritual.sh`), not something an
operator copies to `/usr/local/bin`.

The drop-in ships as `llama-swap.service.d/50-vibe-intent.conf` with
reference-fleet example values only (`gpu-cell`, `front-host:9001`,
`/etc/vibe/fleetd-token`) — the boundary rule. It is an **example**,
which is precisely why it is gated: an example is what gets copied.
`TestC24UnitFragmentIsInstallable` parses it and fails if a hook loses
its `-` prefix, if either half goes missing, if a required `Environment=`
is absent, if `TimeoutStopSec` drops below llama-swap's 30s in-flight
stream grace (C16) plus the hook's own bound, or if the fragment ever
grows an `ExecStart=`, `Restart=` or `OnFailure=` — a fragment that
records a lifecycle must not change one.

Nothing installs, enables or starts a unit anywhere in this repo. The
hook's *script* is what the gates execute.

## 7. Known limitation: an announcing cell whose daemon outlives the stop

On a cell that announces, the vibe daemon is its own unit and keeps
heartbeating `serving` after the serving stack stops — `AnnounceIntent`
carries `{state, since}` and nothing else, so the echo cannot say "the
stack under me is down". `decorate` trusts a fresh announce over a failed
probe, so the pair renders **INCONSISTENT** rather than DRAINED until the
announcer goes stale.

The record is still written, still says when, and still cannot actuate;
it is the *display* that is wrong, and it is wrong in the nagging
direction rather than the confident one. The honest fix is a field on the
announce echo (a reason, or a serving-stack liveness bit), which is a
wire change and a phase of its own — written up as the C24 follow-up in
the futures doc. Cells fleetd reaches by probe render DRAINED with the
record, which is the intended shape.

## Files

- `deploy/cell/README.md` — install, both launcher forms, the failure
  semantics, the limitation above.
- `deploy/cell/vibe-reclaim.sh` — the declared path.
- `deploy/cell/vibe-cell-intent.sh` — the recorded path, both halves.
- `deploy/cell/llama-swap.service.d/50-vibe-intent.conf` — the drop-in.
- `deploy/cell/vibe-reclaim.desktop` — the shortcut form.
- `internal/vibe/fleetapi/intent.go` — `StopIntentReason`,
  `IsStopRecord`, the `unit_stopped` / `unit_started` wire verbs, the
  fill-if-absent rule and the start half's conditional retire.
- `internal/vibe/fleetapi/announce.go` — rules 1 and 2.
- `internal/vibe/fleetapi/presence.go` — rule 3.
- `internal/vibe/fleetapi/notify.go`, `doctor.go` — rule 4.
- `internal/vibe/cli/root.go` — the wrapper's exit status.
- `internal/vibe/fleetapi/c24_test.go`, `internal/vibe/cli/c24_test.go`.

## Acceptance gates

### Unit — PASS (`go test -race -count=5`, 2026-08-08)

| gate | test | proves |
|---|---|---|
| the wrapper drains, runs, resumes — on success and on non-zero exit | `TestDrainUntilExitResumesOnSuccessAndFailure` (C2, still green) + `TestC24WrapperPropagatesTheChildsStatus` | resume fires on any exit, and the status now survives |
| …and on SIGINT to the wrapper | `TestDrainUntilExitResumesOnContextCancel` (C2) + `TestC24WrapperExecsSoSignalsReachTheResume` | the signal reaches the process that owns the deferred resume |
| the wrapper builds the declared verb | `TestC24WrapperBuildsTheDeclaredDrain` | reason/ETA/`--yes`/`--until-exit`/`--`, and no command is EX_USAGE, not a drain |
| the hook records through the real route | `TestC24HookRecordsThroughTheRealIntentRoute` | shipped script → real `fleetapi` server → real store; bearer from the token file; start retires |
| **unreachable fleetd leaves the axis UNKNOWN** | `TestC24HookLeavesTheAxisUnknownWhenFleetdIsUnreachable` | exit 0, under the bound, store untouched, journal says so |
| **bounded against a fleetd that hangs** | `TestC24HookIsBoundedAgainstAHungFleetd` | `--max-time` fires; the 30s ceiling is refused before any request |
| **the hook actuates nothing** | `TestC24HookActuatesNothing` | 12 tripwires ahead of PATH, every path of the hook, log empty |
| a stop record is never a command | `TestC24StopRecordIsNeverHandedBackAsACommand` | with a control proving the hand-back is real |
| a declared drain outranks it | `TestC24StopRecordLosesToTheCellsOwnDrain` | and C6's complied-drain branch still keeps reason + ETA |
| it is not a pending request | `TestC24StopRecordIsNotAPendingRequest` | with a control on the human's version |
| the start half retires only its own record | `TestC24UnitStartRetiresOnlyItsOwnRecord`, `TestC24UnitStartNeverBecomesAResumeRequest` | no clobber, and no resume command |
| a crash still alarms | `TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm` | 6 cases incl. the declared-drain suppression that must survive |
| the doctor still calls it undeclared | `TestC24DoctorStillCallsTheStopUndeclared` | the sit-down command does not get quieter |
| the fragment stays installable | `TestC24UnitFragmentIsInstallable` | `-` prefixes, both halves, env, `TimeoutStopSec ≥ 33`, no `Restart=`/`OnFailure=` |
| the cross-language constants | `TestC24VersionedVerbsMatchTheScript` | the script's states are byte-identical to `fleetapi.IntentStateUnitStopped/Started`, and it sends no reason of its own |
| version skew degrades to nothing | `TestC24HookAgainstAPreC24Fleetd`, `TestC24VersionedVerbsAreTheWireContract` | a pre-C24 400 records nothing and names the skew; an unknown state is still a 400 |
| a stop record never overwrites a declaration | `TestC24StopRecordNeverOverwritesADeclaration` | C14's sleep record and a human's `gaming` both survive; the unit may refresh its own note |
| the scripts pass the shell linter | `TestScriptsAreSafe` (C20/C21) | the walk covers `deploy/` |

Mutation-checked rather than assumed:

- `IsStopRecord` → `return false`: **all 7** fleetapi C24 tests fail.
- `exec` removed from the wrapper: the signal gate fails on the pid
  comparison, naming the consequence.
- a `systemctl daemon-reload` added to the hook: the tripwire gate fails,
  naming the command.

The two that would not survive a rewrite of this phase's own code are in
C20's permanent registry (`internal/mutation`, +2 → 19 entries), so they
run on every PR rather than once here:

- `c24/a recorded stop is handed back to the cell as a command` —
  disarms the `stopRecord` branch in `handleAnnounce`; §3, mechanically.
- `c24/a unit's stop overwrites the declaration that knows why` —
  disarms the fill-if-absent guard; §8b R-5, mechanically.

### Live — NOT RUN, and the reason for each

Per ground rule 10, these are gates that **were not attempted**, each
with the specific fact it lacks. None of them is "not possible".

| gate | why not run |
|---|---|
| a real `.service` stopping with the drop-in installed | **Refused by the phase's own safety brief**: this box runs a production llama-swap on `:9000` and a vibe daemon on `:9001`, and the brief forbids installing, enabling or starting a systemd unit here. Needs a scratch box or a VM. |
| the hook during a real **shutdown** (network already going away) | same, plus it needs a real reboot. This is the case the bound exists for; the hung-server test is its stand-in and is strictly weaker — it proves the timeout, not the teardown ordering. |
| Steam actually substituting `%command%` | needs a Steam client and a game; the wrapper's argv handling is gated, the substitution is Valve's. |
| `TimeoutStopSec` vs llama-swap's 30s stream grace, end to end | needs a real unit stop with a real in-flight stream. The 30s figure is C16's measurement, carried, not re-measured here. |
| the fleetlab rig | `scripts/fleetlab/lab.sh` was **not run**: seven agents share this box and its `down` sweep is anchored partly on a shared upstream port range, so a teardown here surfaces as a phantom bug in someone else's phase. `internal/swaptest` covers the wire. |

### Inner loop — PASS (2026-08-08)

`go build ./...`, `go vet ./...`, `go test -race ./...` (0 failures),
`gofmt -l .` (empty), `go mod tidy` (no diff), `golangci-lint run`
(0 issues), plus `go test -race -count=5` on `internal/vibe/fleetapi`
and `internal/vibe/cli`.

## 8b. Adversarial self-review addendum (ground rule 9)

Five findings, all fixed in the review commit. Two of them are the same
class the phase was written to close — a record that can become a
command — reached by routes the feature commit did not look at.

**R-1 (blocker-shaped). Version skew turned the hook back into an
actuator.** The feature commit's hook posted
`{"state":"drained","reason":"stopped out of band"}`. A **pre-C24
fleetd** does not know the reserved reason: it records an ordinary
drained request, hands it to the announcing cell as `desired_intent`,
and the cell runs `cell_cmds.drain` — §3, live, on any fleet where the
drop-in reached a cell before the front was upgraded. The start half was
the same defect with `cell_cmds.resume`. The whole rule set was
inside-the-new-build reasoning about a wire two builds share.

*Fix:* the hook posts a **state**, `unit_stopped` / `unit_started`, which
every build of this endpoint that predates C24 answers with `400 state
must be "drained" or "serving"`. The reserved reason stays as the
internal marker; the wire carries a verb whose failure mode is refusal.
Gated by `TestC24HookAgainstAPreC24Fleetd` (a server replying with the
old handler's exact 400) and `TestC24VersionedVerbsAreTheWireContract`.
A second, unlooked-for benefit: it demotes the cross-language string pin
from silent-and-dangerous to loud — a typo in the state is a 400, a typo
in the reason would have been an ordinary drain.

**R-5 (blocker-shaped). A stop record clobbered C14's sleep record.** A
scheduled suspend writes `{drained, "asleep per sleep_schedule", eta
07:15}` and *then* takes the box down — through the same unit stop that
fires this hook. The hook's write landed second, so every scheduled
night ended with the fleet having forgotten it put the box to sleep:
"asleep per sleep_schedule, eta 07:15" became "stopped out of band", the
doctor called the night undeclared, and the wake's clear-first ordering
had a different record to clear than the one it wrote. The same shape
applied to a human's `--reason gaming` on a non-announcing cell.

*Fix:* a stop record is a fill-if-absent write. It never overwrites an
entry that carries a why; it may replace its own note (the timestamp is
the point) and a pending serving request (which carries no why and is a
resume the stopped box will never ack — `pruneStaleServingRequest`
already makes that judgement). Gated by
`TestC24StopRecordNeverOverwritesADeclaration`.

**R-2. The hook's journal line claimed something it had not verified.**
"recorded: gpu-cell started; any stop record for it is retired" is false
in exactly the case R-5 is about — fleetd may have left a declaration
alone. The hook does not read the response body, so it cannot know.
Reworded to state what the request MEANS rather than what happened; the
stop line is hedged the same way.

**R-3. An unbounded read inside the stop sequence.** `IFS= read -r token
<"$token_file"` on a FIFO or a character device blocks forever, and this
runs where a hang is a hung shutdown. `[ -f ]` now precedes `[ -r ]`.

**R-4. The wrapper's fail-closed behaviour was undocumented.** If the
drain fails, `--until-exit` never runs the command — the game does not
launch. That is the right default (a wrapper that launched anyway would
silently stop declaring, on the one box where nobody would notice), but
it was nowhere in the README, and the person it surprises is holding a
controller. Documented with its usual causes.

## What this phase deliberately does not do

- **No GPU-idle heuristic, no auto-resume.** Unchanged from C2 and worth
  restating in a phase about reclaim: resume is deterministic on the
  wrapped command's exit, or it is a human typing `vibe cell resume`.
- **No new display state.** A stop record renders DRAINED with its
  reserved reason. A fourth flavour of "down" would ripple into the page,
  the CLI, C9, the doctor and the design doc's §4 table, which this
  phase may not edit — raised for the reconciliation pass below instead.
- **No inference anywhere.** The record is written by the stop, not
  derived from a probe. Nothing in this phase reads availability and
  concludes intent.
- **No changes to the announce wire.** §7's limitation is real and the
  fix is a schema addition; a packaging phase is the wrong place for it.

## For the reconciliation pass

Nothing here was applied — the three shared documents are owned by the
reconciliation pass.

### `docs/design/fleet-control.md` §4 (axis 2, and the display table)

Axis 2 gains a second class of author. Suggested amendment:

> *Amended C24.* Intent is declared, and a cell unit's own stop hook is
> one of the declarers — the reserved reason `stopped out of band`
> (`fleetapi.StopIntentReason`, written by fleetd when a hook posts
> `state: unit_stopped`) marks a record whose author is the unit rather
> than a human. It is still a declaration, not an inference: nothing
> reads a probe and concludes intent. What separates it from every other
> entry on this axis is that it carries **no why**, so the control plane
> refuses it five things a human's declaration gets — it is never handed
> back as `desired_intent`, it loses to the cell's own drained echo, it
> never counts as a pending request, it never overwrites an entry that
> does carry a why, and **it does not explain an absence** (an
> `always_on` cell whose stack crashed fires the same `ExecStopPost` as
> one an operator stopped, and must alarm exactly as before). The paired
> `ExecStartPost` (`unit_started`) retires the record and nothing else.
> Packaging: `deploy/cell/`.

Open question for the design owner, deliberately left open: **should a
stop record render `DRAINED` or keep the `DRAINED?` question?** This
phase chose DRAINED-with-a-reserved-reason and compensated by making
every why-consuming surface ignore the record. The alternative — display
stays `DRAINED?`, the intent block carries the record — is arguably
truer to the table's own wording and costs nothing in C9 or the doctor,
but it makes `Display == DRAINED?` no longer imply `Intent == nil`, which
several surfaces read as a pair today.

### `docs/design/fleet-control-plan/README.md`

Status row: **C24 — drain where reclaim happens — MERGED**, futures item
5, with the one-line summary "the verb, in the launcher; the unit's own
stop, recorded and made incapable of commanding anything".

### `AGENTS.md`

Two additions, in the fleet section:

> - **A recorded stop is not a request (fleet-control C24).** A cell
>   unit's own hooks post `state: unit_stopped` / `unit_started` to
>   `/api/fleet/intent`; fleetd stamps `fleetapi.StopIntentReason` and
>   refuses such a record five things: `handleAnnounce` never returns it
>   as `desired_intent` (an announcing cell answers a drained
>   desired_intent by RUNNING `cell_cmds.drain` — the record would stop
>   the stack it only described), it is deleted the moment the cell
>   echoes a drain of its own, `resolveIntent` never marks it pending,
>   `absentAlarm`/`explainedCells` never let it explain an absence, and
>   `SetIntentAt` never lets it overwrite an entry that carries a why
>   (C14's sleep record is written *before* the suspend takes the box
>   down through that same unit stop). `unit_started` retires a stop
>   record and **only** a stop record: it never clears a human's
>   declaration and never stores a serving request. **The verbs are
>   states on purpose** — an unknown state is a 400 on every build of
>   this endpoint, so a drop-in installed against an older front degrades
>   to doing nothing instead of to actuating. Adding a sixth surface that
>   reads intent means deciding whether a record with no why belongs in
>   it.
> - **`deploy/cell/` is host-installed packaging, and its scripts are
>   under test.** `internal/vibe/cli/c24_test.go` executes the shipped
>   files — the wrapper's argv and `exec`, the hook's bound, its exit-0
>   contract and its tripwire-verified inertness — and parses the systemd
>   drop-in. Editing anything in `deploy/cell/` without running
>   `go test ./internal/vibe/cli/ -run TestC24` is how the example that
>   gets copied stops matching the code.
