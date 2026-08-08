# C27 — STOPPED: the unit's own stop record, given its own display state

Status: **MERGED (#63, 2026-08-08)**, off `c27-stopped-display-state`
branched from `main` at `89f15c5`. Answers the open question C24 left in
[`fleet-control.md`](../fleet-control.md) §4 and
[c24](c24-drain-where-reclaim-happens.md) §"For the reconciliation pass",
which C26b deliberately did not decide:

> Open question for the design owner, deliberately left open: **should a
> stop record render `DRAINED` or keep the `DRAINED?` question?**

The answer is **neither**. It gets its own state, `STOPPED`, matching the
wire verbs C24 already ships (`unit_stopped` / `unit_started`).

---

## 1. Why the two candidates are both wrong

Three different facts were sharing two words:

| | what it means | who knows why |
|---|---|---|
| `DRAINED` | **somebody chose this.** `vibe cell drain`, the MCP tool, C14's schedule | a human or a schedule wrote the reason down |
| `DRAINED?` | **nobody knows anything.** The intent store holds no entry — the design's "deliberate stop or crash loop" | nobody, and there is no record either |
| the C24 stop record | the serving stack stopped, and when | **nobody.** `systemctl stop` and a crash-stop fire the identical `ExecStopPost` |

The record is more than the second and less than the first, and C24
rendered it as the first — a state whose whole meaning is *an operator
explained this*.

## 2. The confusion was already load-bearing

This is the argument for spending a phase on a word. Because `DRAINED`
lost the distinction, the consumers that need it reach **past** the
display state into `fleetapi.IsStopRecord` to get it back:

- `notify.absentAlarm` — `switch c.Display` decides nothing on its own
  for the DRAINED/OFF arms; a side-channel boolean decides them. Its
  comment is a paragraph explaining that a stop record must not suppress
  the `always_on` alarm, which is a paragraph about a distinction the
  display was supposed to carry.
- `doctor.explainedCells` and `doctor.checkIntentHygiene` — both branch
  on `IsStopRecord` ahead of the display switch, for the same reason.

A display state that its own consumers have to work around is the wrong
display state. `STOPPED` lets the derivation carry the fact once. The
side-channel does not vanish — §3 is why it cannot — but it shrinks from
"the thing that decides two arms" to "the thing that disambiguates OFF".

## 3. The decision: host unreachable stays OFF

`displayState`'s `drained && hostReachable != nil && !*hostReachable`
branch comes **before** the new `stopped` case, so a stop record on a box
whose host probe is also silent still renders **OFF**, not STOPPED.

Three reasons, in order of weight:

1. **The bigger fact wins.** OFF says the box is gone. STOPPED says a
   process on it stopped. When both are true, the one that decides what
   the operator does next — you cannot ssh in, the wrapper cannot run,
   `vibe cell resume` will not reach it — is the box. Rendering STOPPED
   there would replace a fact about the machine with a fact about a unit
   on it.
2. **OFF already means this.** Its table row is "was drained first" — the
   intent axis moved, then the host went away. A recorded stop is one
   instance of that sequence, and the commonest one: the unit's stop hook
   fires *during* the shutdown that is about to take the host with it.
3. **Nothing loses the distinction.** The record is still on the
   snapshot's `intent` field, which is where `absentAlarm` and both
   doctor checks read it — so an `always_on` box that stopped and then
   went away still pages, with the same "nothing recorded why" text it
   had before this phase. That is asserted, not assumed: it is one of the
   108 combinations in §5's invariant test.

The cost is honest and worth naming: `Display == OFF` is now the one
state that means "declared drain" for one cell and "recorded stop" for
another, and `absentAlarm` keeps its `IsStopRecord` side-channel for
exactly that arm.

## 4. What changed, surface by surface

| surface | change |
|---|---|
| `fleetapi/display.go` | `DisplayStopped`, one new `case` in the derivation, and `DisplayStates` — the enumerable list §6's guards are checked against |
| `fleetapi/notify.go` | `DisplayStopped` joins the ALARMING arm of `absentAlarm`'s switch. **This is the hazard** (§5) |
| `fleetapi/doctor.go` | `checkIntentHygiene`'s switch gains STOPPED as a defensive arm; the C24 comment corrected. `explainedCells` needed nothing — it keys on the record, never on the display |
| `fleetapi/fleet.html` | `.b-stopped` (violet, solid — not a shade of the amber DRAINED pair), a `badgeClass` line, and **every other state given its own line** so the fall-through could stop being `b-off` (§6) |
| `fleetapi/fleetapi.go` | `CellSnapshot.Display`'s doc comment: the state list, and the note that an OFF cell may carry a stop record too |
| `fleetmcp/fleetmcp.go` | `fleet_status`'s description — it is where an agent learns what a row means, and it now draws the three-way distinction rather than listing eight words |
| `deploy/cell/vibe-cell-intent.sh` | the hook's journal line said "the fleet now shows DRAINED with no reason". It shipped, an operator reads it in `journalctl`, and it was wrong the moment this phase landed |
| `deploy/cell/README.md` | the probe-reached shape, and the C24 §7 limitation sentence |
| `docs/design/fleet-control.md` §4 | the open question settled, a table row for STOPPED, the OFF row amended, and the alarm-column paragraph |

Grepped for every `"DRAINED"` literal and every `Display*` constant across
Go, HTML, shell and Markdown; the rest are historical phase docs (C1, C9,
C13, C19's transcripts) which are records of what those phases did and
are not edited.

## 5. The invariant, and the hazard it is written against

`absentAlarm`'s switch ends in `default: return "", false` — **no
alarm**. A new "down" state that is not named in it silently stops paging
for an `always_on` cell that went away, which is precisely the incident
C9's notifier exists for. This repo has shipped that class twice (C9's
class-table violation, C14's).

So the governing invariant:

> This change alters **DISPLAY only**. Alarm behaviour, doctor findings
> and suppression semantics are identical before and after, for every
> combination of class × availability × intent × reason.

`TestC27DisplayOnlyChangeLeavesEveryAlarmAndDoctorOutcomeIdentical`
enumerates **108** combinations — 3 classes × 3 host-probe outcomes (up /
down / no probe) × cell answering or not × 6 intent shapes (no entry, a
declared drain with a reason, one without, the C24 stop record, C14's
sleep record, a pending serving request). For each it computes the
display **twice**: once with `main`'s derivation, pasted into the test
verbatim as `displayStateBeforeC27`, and once with this phase's. Then it
runs the real `absentAlarm` and the real `checkIntentHygiene` on both
snapshots and asserts:

- the alarm's firing decision is identical,
- `main`'s own alarm predicate agrees with today's function on the state
  `main` would have shown (so no untouched arm moved),
- the doctor's level and bucket membership are identical,
- and both detail strings are equal **after substituting the old state's
  name for the new one** — the only difference a rename is allowed to
  make is the word.

**Result: 6 of 108 combinations changed display; 0 changed an alarm or a
doctor outcome.** The test logs both numbers and fails if the first is
zero, because a test that proves a no-op is a no-op proves nothing.

The six are the stop record with the host up or with no host probe, in
each of the three classes. (Cell answering → INCONSISTENT, unchanged;
host down → OFF, unchanged, per §3.)

One thing genuinely changes and is meant to: the alarm's **prose** names
the new state — "always_on cell heavy is STOPPED with no declared intent
(its unit recorded the stop; nothing recorded why)". That is not a
behaviour change. `fleetnotify.Condition.Key()` is `kind + "\x00" +
scope`, so dedup, re-fire and resolve are untouched by detail text.

## 6. A found defect: the page's fall-through

Writing the "every consumer names every state" guard failed on the page
before it failed on anything of this phase's. `badgeClass` ended in
`return "b-off"`, and `OFF` / `OFF/AWAY` / `OFF/AWAY?` had no lines of
their own — they *relied* on that fall-through. So a display state the
page had never heard of rendered exactly like a box that is merely off:
a confident wrong answer, in the surface most likely to be read at a
glance, and the browser version of the switch defect above.

Fixed rather than tested around: all eight states get a line, and the
fall-through is now `b-unknown` (dashed red — it must look *wrong*, not
look off). The guard asserts the fall-through class is not also a known
state's class, so it cannot regress to `b-off`.

## 7. Not in this phase

- **No new behaviour.** STOPPED actuates nothing, suppresses nothing and
  explains nothing. It is a word, and §5 is the proof that it is only a
  word.
- **C24 §7's limitation stands.** An announcing cell whose daemon
  outlives its serving stack still renders INCONSISTENT rather than
  STOPPED, because the cell is observably up. The honest fix is still a
  field on the announce echo — a wire change, and a phase of its own.
- **No CLI colour.** `vibe cell status` prints the display word; it has
  never coloured it, and a rename is the wrong moment to start.

## Files

- `internal/vibe/fleetapi/display.go` — `DisplayStopped`, the derivation,
  `DisplayStates`.
- `internal/vibe/fleetapi/notify.go`, `doctor.go`, `fleetapi.go`,
  `fleet.html`.
- `internal/vibe/fleetmcp/fleetmcp.go` — the tool description.
- `deploy/cell/vibe-cell-intent.sh`, `deploy/cell/README.md`.
- `internal/vibe/fleetapi/c27_test.go`, `internal/vibe/fleetmcp/c27_test.go`.
- `internal/vibe/fleetapi/c24_test.go` — one row added to the alarm
  table, and the pre-C27 pairing kept beside it.
- `internal/vibe/fleetapi/c14_test.go` — the wall-clock flake below.
- `internal/mutation/mutation.go` — the registry entry (+1 → 67).
- `scripts/fleetlab/gate-c27-stopped-badge.sh` — the live rig.
- `docs/design/fleet-control.md` §4, this plan's README (row + ground
  rule 2), `AGENTS.md` (the state list and the alarm bullet).

## Acceptance gates

### Unit — PASS (`go test -race`, 2026-08-08)

| gate | test | proves |
|---|---|---|
| **display only: no alarm or doctor outcome moves** | `TestC27DisplayOnlyChangeLeavesEveryAlarmAndDoctorOutcomeIdentical` | 108 combinations, both derivations, the real alarm and the real doctor; 6 displays changed, 0 outcomes |
| the derivation, including the decided case | `TestC27StoppedIsItsOwnDisplayState` | host up → STOPPED, no probe → STOPPED, host down → **OFF**, cell answering → INCONSISTENT, human → DRAINED, nothing → DRAINED? |
| **the hazard** | `TestC27StoppedAlarmsForAnAlwaysOnCell` | STOPPED pages for `always_on` and stays silent for the other two classes |
| the doctor keeps it undeclared, both routes | `TestC27DoctorKeepsAStoppedCellInTheUndeclaredBucket` | the record route and the defensive display route |
| every enumerating consumer names every state | `TestC27EveryDisplayStateIsNamedByEveryEnumeratingConsumer`, `TestC27FleetStatusDescribesEveryDisplayState` | `DisplayStates` scanned against the constants in the source file; `badgeClass` and the stylesheet read out of the EMBEDDED page; the MCP list. Written to fail for a ninth state nobody has invented yet |
| C24's own alarm table still holds | `TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm` (+1 row) | the new state alarms, and the pre-C27 pairing still alarms |

Mutation-verified, each one applied to the production line, the named
test watched RED, and the line restored byte-identical:

| mutation | red |
|---|---|
| `DisplayStopped` removed from `absentAlarm`'s alarming arm | `TestC27StoppedAlarmsForAnAlwaysOnCell`, `TestC24StopRecordDoesNotSilenceTheAlwaysOnAlarm`, and the 108-combination test |
| `case stopped:` disarmed in `displayState` | `TestC27StoppedIsItsOwnDisplayState` + the enumeration's own zero-change control |
| `DisplayStopped` removed from the doctor's switch | `TestC27DoctorKeepsAStoppedCellInTheUndeclaredBucket` (the defensive arm) |
| the `STOPPED` line deleted from `badgeClass` | `TestC27EveryDisplayStateIsNamedByEveryEnumeratingConsumer` |
| the fall-through returned to `b-off` | same test, on the fall-through assertion |
| `STOPPED` dropped from the MCP state list | `TestC27FleetStatusDescribesEveryDisplayState` — **after** the guard was fixed. It first stayed GREEN, because the prose after the list says "STOPPED" too and the check was a `Contains` over the whole description. The guard now scopes itself to the parenthesised list. A mutation that catches nothing is the finding |

Permanent, in `internal/mutation` (+1 → **67** entries, its own CI job):

- `c27/a stopped always_on cell no longer pages` — disarms the
  `DisplayStopped` case in `absentAlarm`'s switch. Verified caught by the
  harness (`VIBE_MUTATION_TEST=1`, 239 s for the full registry).

### Live — PASS, `scripts/fleetlab/gate-c27-stopped-badge.sh` (2026-08-08)

`FLEETLAB_DIR=/tmp/fleetlab-c27 FLEETLAB_PORT_BASE=10400`, a private
four-cell lab, ports checked free with `ss -ltn` first and released on
`down`.

C24's rig fires the hook *without* the unit stopping, so its cell keeps
announcing and renders INCONSISTENT — correct, and useless for looking at
this state. This rig takes the stack down first, the way a `systemctl
stop` does: alpha's llama-swap dies, alpha's announcer dies, alpha's host
probe stays up. Alpha is `always_on`, so the same run is the hazard gate.

```
=== 2. once the announce goes stale: DRAINED? — nothing recorded anything ===
{"display":"DRAINED?","reachable":false,"host_reachable":true,"stale":true,"intent":null}

=== 3/4. the SHIPPED hook, on a cell that really is down ===
vibe-cell-intent: recorded: alpha stopped; … the fleet now shows STOPPED — the stop, with no reason
{"display":"STOPPED","class":"always_on","reachable":false,"host_reachable":true,
 "intent":{"state":"drained","reason":"stopped out of band","since":"2026-08-08T23:34:01Z"}}

CELL           DISPLAY       CLASS          MODELS   INTENT / LAST SEEN
alpha          STOPPED       always_on      -        intent: stopped out of band; last seen 1m8s ago

=== 5. the page has a badge class for it ===
  .b-stopped { background: rgba(150,123,220,.15); color: var(--violet); }
  if (d === "STOPPED") return "b-stopped";

=== 6. THE HAZARD: an always_on cell that STOPPED must still page ===
# fleetd restarted first: the cell_absent raised in step 2 is inherited evidence,
# which is no evidence. A fresh process has seen NOTHING about alpha but STOPPED.
{"delivery":{"sent":1,…},"active":[{"key":"cell_absent/alpha","kind":"cell_absent",…}]}
2026-08-08T20:36:22 /fleetlab {'Tags': 'vibe-fleet,firing,cell_absent', 'Title': 'fleet: alpha'}
  always_on cell alpha is STOPPED with no declared intent (its unit recorded the stop;
  nothing recorded why) (last seen 2026-08-08T23:32:56Z)

=== 7. the doctor does not get quieter ===
WARN  intent.hygiene  undeclared state: alpha STOPPED (recorded by its own unit stop at
                      2026-08-08T23:34:01Z; nothing declared why)

=== 8. a human's declaration on the same silent cell still reads DRAINED ===
{"display":"DRAINED",…,"intent":{"reason":"c27 gate: a human said why","eta":"23:00",…}}
```

The fleetd restart in step 6 is the part that makes it a gate rather than
a screenshot: the alarm raised while the cell read `DRAINED?` would still
be active whatever this phase did to the display. A process that has only
ever seen `STOPPED` raising `cell_absent`, and a webhook payload with the
word in it, is the hazard closed against real code.

**Regression: `gate-c24-stop-record.sh` re-run on the same lab** — output
identical in kind to C26b's recorded transcript (bravo INCONSISTENT
throughout, the record never handed back, the human's reason never
overwritten, the start half retiring only its own record). C24's live
half is undisturbed.

### Inner loop — PASS (2026-08-08)

`go build ./...`, `go vet ./...`, `go test -race ./...` (0 failures),
`gofmt -l .` (empty), `go mod tidy` (no diff), `golangci-lint run` (0
issues, after `golangci-lint cache clean` — the documented cross-worktree
cache leak, not a finding).

## Incidental finding: a test that measured the wall clock

`go test -race ./...` was RED on `main` before a line of this phase was
written, and stayed red with the change stashed:
`TestSleepSchedule_AbandonsAfterTheDeferWindow`.

The C14 fixture's suspend cron is a real time of day (`30 23 * * *`,
evaluated in UTC) and the test's `now` is `time.Now()`. It advances the
clock 31 minutes to prove the deferral is abandoned — so a run that
started inside the 31 minutes before 23:30 UTC crossed the cron minute on
the second evaluation, re-armed the suspend, reset `DeferredSince` and
`deferUntil`, and never abandoned anything. About 2% of the day, this
test measured the wall clock instead of the abandonment rule, and it
would have failed this PR's CI at random.

Fixed in its own commit: the entry's suspend cron is pinned half a day
from whatever time it is, which is what the test needed all along
(`armed()` supplies the due suspend it actually drives). Verified with
`-count=3` at the failing hour.
