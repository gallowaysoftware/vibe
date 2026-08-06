# C20 — The invariant harness: the review step, made mechanical

Status: **PR OPEN** (2026-08-06), off `feat/c20-invariant-harness`
branched from `main` at `e144f8b`. Unit gates U1–U14 green on a full
local inner loop (`go test -race -count=3` over every touched package).
The **mutation harness runs green at 16/16** — 21 s with a warm build
cache, 58 s from cold, in its own CI job. See
[Acceptance gates](#acceptance-gates).

The mandate, in the repo owner's words:

> add tests to catch this stuff, relying on reviews for logic isn't
> working

## 0. What the evidence actually says

Reviews *do* work. Ground rule 9's adversarial pass has found 39+ real
defects across this plan, four of them blockers, every one of them in
code that was already green in CI. The problem is not that the technique
fails; it is that it is **expensive, non-deterministic, and finds the
same classes over and over**.

That last part is the opening this phase takes. A defect class that
recurs eight times is not a series of unlucky reviews — it is a shape the
codebase makes easy to write. So the job here is not "more tests". It is
to take each recurring class and either remove the shape or make a
machine watch for it, so a review stops being the only thing between the
class and `main`.

The classes, with their real occurrences:

| # | class | occurrences |
|---|---|---|
| 1 | absent evidence read as a healthy value | 6+ — C4's warm policy, the v247 wire's `len(requests)==0`, C10's `--idle`, C16's `versions.llama_swap`, C13's `defs.parity` |
| 2 | the sending side guards, the receiving side doesn't | 6+ — B1's `ModelCmd` nil-deref, unclamped announce counters, a cell writing fleetd's reserved basis, `flags_sha256` skipping `clean()` |
| 3 | a guard that lives in one of N call paths | 3+ — the warm class guard (1 of 4), the llama-swap credential (1 of 7), C16's version reader missing C15's authorizer |
| 4 | an error return that is really a VALID STATE | 2 — `saveIntents` on an empty path, `mergeExtras` on a missing file |
| 5 | a lock-escaping pointer | 1 — C4's shipped race |
| 6 | a context that ignores shutdown | 2 — `context.Background()` in an `s.wg` loop, a timeout guard inert behind `WithoutCancel` |
| 7 | a test asserting less than its name claims | many — the "retire" assertion that held because a fresh `Server` starts with an empty map |
| 8 | shell rig safety | 1 blocker — a bare `cd` before `git init` + `git add -A` |

Four of them are answered here. Classes 5, 6 and 7 are recorded in
[§7](#7-what-this-phase-does-not-mechanise-and-why) with what a
mechanical check would and would not buy; class 2 is the one this phase
addresses only obliquely, and says so.

## 1. Class 1, removed rather than detected: `internal/vibe/observed`

The highest-value item is the one that removes a shape instead of
watching for it, and class 1 has exactly one shape:

```go
count, reported := s.inFlight[cell], s.inFlightSeen[cell]   // two maps that must agree
n, _ := d.fleet.InFlight(cell)                              // …and one line that drops the bit
```

Every occurrence is a correct pair *somewhere* and a dropped bit
*somewhere else*. The pair cannot be made correct by discipline: a map
miss, a `delete`, a blank identifier and a freshly declared field all
produce the same confident zero, and a *reported* zero is a positive
claim of idleness that eight busy guards disarm on.

```go
// Package observed carries measurements whose ZERO VALUE is "nobody looked".
type Value[T any] struct {
	v     T
	known bool
}

func Known[T any](v T) Value[T]

func (o Value[T]) Observed() (T, bool)  // the ONE reader: the pair, together
func (o Value[T]) IsKnown() bool        // for guards that only care that evidence exists
func (o Value[T]) OrElse(fallback T) T  // absence, with the meaning WRITTEN DOWN
```

Three properties do the work:

- **The zero value is unknown.** A field nobody initialised, a map miss
  and a `delete` all already mean "no evidence". There is deliberately no
  `Unknown()` constructor: a plain declaration says it better.
- **The value is unexported.** There is no way to reach the number
  without answering the question.
- **`OrElse` makes the claim visible.** "Read absence as 0" is sometimes
  right — a display rendering an idle cell — and it now has to be typed
  out at the call site where a reviewer can see it.

`MarshalJSON` emits `null` for an unknown, because a state document that
spells absence `0` hands every downstream reader the same false
measurement in a format that has a perfectly good word for absence.

### What was migrated

**The in-flight path, in full.** `Server.inFlight` and
`Server.inFlightSeen` collapse into one
`map[string]observed.Value[int]`, and `Server.InFlight(cell)` returns
`observed.Value[int]` instead of `(int, bool)`. The eight consumers —
both warm loops, the probe guard, the sleep guard, the activity block,
the drain report, the suspend RPC and the drain wait — each read it
through `.Observed()`.

**C4's per-model activity stamps**, `modelLastActivity` and
`cellLastActivity`, which are the *original* class-1 carrier: "when did
this model last serve a request, and has it ever". Both now return
`observed.Value[time.Time]`.

### The migration found a live defect

`awaitQuiescence` (C2's `drain --wait`) checked "has this cell ever
reported an in-flight count" **once**, before the wait, and then spelled
every subsequent read:

```go
n, _ := d.fleet.InFlight(cell)
if n == 0 {
    return fleetapi.DrainWaitWaited, nil
}
```

The count can go back to unreported *mid-wait*: the cell's events stream
drops (`clearInFlight`) or sends a frame this build cannot fold
(`disarmInFlightLocked`). The dropped bool turned that into `n == 0`,
which the loop read as quiescence and reported to the operator as
`waited`. That is not exotic — a cell restart, a network blip or an
llama-swap upgrade all produce it — and `drain --wait` exists precisely
because the stop that follows force-closes in-flight streams at
llama-swap's hardcoded 30 s.

Fixed: an unreported count mid-wait returns `skipped_no_inflight_data`,
the `wait_status` that already means "no data", rather than a claim about
the box. Pinned by
`TestCellDrainWait_DoesNotClaimQuiescenceWhenTheEvidenceStops`, which
drives the real watcher against a real events stream and drops the
connection mid-wait — and mutation-verified, both by hand and as a
registry entry.

This is the argument for the type in one paragraph: the defect had been
there since C2, three review passes had read that function, and it became
visible the moment the number stopped being reachable without the bit.

### And three scans keep the shape out

All three walk the whole module — every previous occurrence was in a
different package — and all three carry a written-reason exemption table
plus a floor on how much was examined.

1. **`TestObservedIsNeverReadWithADiscardedKnownBit`** — `x.Observed()`
   assigned with `_` in the second position. Zero exemptions today.
2. **`TestNoNewValueAndKnownBitFieldPair`** — a struct field `foo`
   beside `fooSeen` / `fooKnown` / `fooReported` / `fooOK` / … where the
   partner is `bool` (or a map/slice/pointer to one, which is how
   `inFlightSeen` was spelled). Two exemptions, both C7b's power
   accumulator.
3. **`TestNoNewMeasurementAndBoolReturn`** — a function returning
   `(numeric, bool)`. Restricted to *numeric* first results on purpose: a
   comma-ok map lookup returning `(Lease, bool)` is fine, because nobody
   can mistake a zero `Lease` for a real one, while a count or an instant
   has a perfectly plausible zero. Seven exemptions, each a parser or an
   arithmetic helper whose bool means "the input was empty".

### Honest accounting: what is NOT migrated

The scans above are what make this list a commitment rather than a
sentence. Everything below is currently *exempted with a reason*, which
means a future phase that touches it has to re-decide rather than
inherit:

- **C7b's power term** (`dayNet.Power`/`PowerKnown`, `cellAgg.power`/
  `powerKnown`, `cellPowerCost`). It is an accumulator plus
  "did-anything-contribute", written and read in the same function;
  `observed.Value` would spell every `+=` as
  `Known(OrElse(0)+cost)`, which is worse code for no change in what is
  representable. Re-examine if the accumulation ever moves away from its
  guard.
- **C7a's `-1` not-reported sentinels** (`cache_tokens`, `draft_tokens`,
  `draft_acc_tokens`). These arrive from llama-swap's wire, where `-1` IS
  the encoding; the clamp at ingest is the guard, and it is already
  test-pinned. A `Value[int64]` at the parse boundary would be an
  improvement and is a bigger diff than this phase should carry.
- **C8's `samples < 5 ⇒ unknown` verdict** and **C13's four-level
  `UNKNOWN`**. Both already model absence as a first-class *value* rather
  than as a zero; they are the pattern this type generalises, not
  instances of the defect.
- **`CellActivity`'s `*int` / `*float64` / `*time.Time`** and **C7b's
  "every money field is a pointer"**. The nil-pointer convention is the
  same idea with a different spelling, chosen because these are JSON
  documents where `omitempty` on a pointer is the idiomatic absence. Not
  worth churning; noted so nobody "fixes" one to match the other.
- **`fleetcfg`, `usagemeter`, `modelprobe` and `router`** carry no
  `(value, known)` pairs today — verified by the scans, which is the
  point of running them module-wide rather than over `fleetapi`.

## 2. The centrepiece: `internal/mutation`

Every adversarial-review addendum in this plan carries a table headed
`| mutation | red |`. The reviewer broke a production predicate, watched
a **named** test go red, and restored the line. It is the only technique
in this project that reliably distinguishes a guard that works from a
guard that is merely present.

It is also entirely manual, so it happens when somebody funds a review
pass and never afterwards. A guard whose test was mutation-verified in
C11 and quietly stopped covering it in C14 is, from CI's point of view,
indistinguishable from one that still works.

So the table becomes data.

```go
type Mutation struct {
	Name     string
	File     string   // repo-relative
	Find     string   // must appear EXACTLY once
	Replace  string   // must still COMPILE
	Pkg      string   // ./internal/vibe/fleetapi/
	MustFail []string // these tests must go red
	Why      string   // what breaks in the FLEET
}
```

The runner copies the tree once per worker, applies one entry at a time,
runs exactly the named tests with `-run '^(A|B)$' -v`, restores the file,
and judges from `go test`'s **own per-test output lines** rather than
from a process exit status — the exit status cannot tell a fired guard
from a failed build.

### The three guards on the registry itself

These matter as much as the findings, and each is separately tested:

- **Unprotected.** The mutation applies and every named test still
  passes ⇒ red, naming the guard. That is the finding.
  (`TestRunnerReportsAnUnprotectedGuard` runs an inert edit through the
  real runner and asserts it is reported, so the harness has not merely
  been observed agreeing.)
- **Stale.** `Find` does not match exactly once ⇒ red. Exactly once, not
  at least once: an ambiguous pattern would mutate whichever copy came
  first and the entry would keep passing while covering something else.
  This is C16's stale-exemption idea, and the failure message says
  explicitly *do not delete this entry to make it green*.
- **Does not compile.** A mutated tree that produces no test verdicts is
  a registry error, not a catch. "The suite went red" is not the claim;
  "this named assertion went red" is. It fired for real during
  development — an entry whose `Replace` inserted a declaration above the
  import block — and reported exactly that.

Plus a **baseline round**: before any mutation, every named test is run
unmutated and must PASS. Without it, "the mutation made it fail" is
unfalsifiable — a test that was already red, or that never ran, would
read as a catch. A named test that does not exist is caught more cheaply
still, by the always-on registry audit.

### What is in the registry

Sixteen entries, seeded from the mutation records already in the phase
docs' addenda, weighted toward the load-bearing ones rather than all of
them (an entry costs a package rebuild, and a registry nobody will wait
for gets deleted):

| entry | guards |
|---|---|
| `inflight/unknown-operation-reads-as-a-reported-zero` | the v247 disarm — the defect that silently turned off eight busy guards |
| `warm-target/in-flight-request-no-longer-blocks-the-restore` | C5's M-series: a generation longer than the window is not idleness |
| `warm-target/fleetd-uptime-becomes-the-idle-clock` | C4/C5's `observesActivity` — the rule §1 forbids |
| `drain-wait/evidence-loss-reported-as-quiescence` | this phase's own finding |
| `c15/streamCell drops the llama-swap credential` | C15's AST scan, which caught C16 at merge time |
| `c4/the warm class guard leaves the restore` | `model_classes`, at fire time AND structurally |
| `c12/a route added without an access decision` | `AccessUndecided` — "forgot" must not be spelled like "decided" |
| `c12/the guest surface silently widens to the ledger` | state, never history |
| `c19/a fleet state file falls outside the mirror` | `TestMirrorCoversEveryFleetStateFile` |
| `c13/a mutating verb reaches the doctor` | the read-only promise, structurally |
| `c7a/a day bucket computed by truncation` | `Truncate(24*time.Hour)` landing on UTC midnight silently |
| `c9/the webhook URL stops being scrubbed` | an ntfy topic URL is bearer-equivalent |
| `c20/an observed.Value read with the known bit discarded` | scan 1 above |
| `c20/a new (value, known-bit) field pair` | scan 2 above |
| `c20/a new (measurement, bool) return` | scan 3 above |
| `c20/an unguarded cd in a shell rig` | §4's shell lint |

The last four are this phase proving its own checks CATCH: each plants a
real violation in a real file and asserts the named scan goes red.

### Cost, honestly

**21 s with a warm build cache; 58 s from cold**, measured on the
workstation: 16 mutations, four workers, one ~8 MB tree copy each, and
the baseline round is what warms the cache. In its own CI job, like the
conformance matrix, so the blocking `test` job's ~15 s stays ~15 s.

The **staleness half runs in the blocking job**: `TestMutationRegistryIsCurrent`
compiles nothing, costs milliseconds, and is what catches an entry that
has come detached from the line it guards. An entry that has silently
stopped covering anything must be visible on a PR nobody waits for the
slow job on.

Env-gated (`VIBE_MUTATION_TEST=1`), never build-tagged — AGENTS.md's rule:
this module has zero `//go:build` lines and a tagged-out file is skipped
by vet and the linter and rots uncompiled.

## 3. Class 3, generalised: `internal/astscan`

C15's `TestEveryLlamaSwapRequestIsAuthorized` is forty lines of `go/ast`
bespoke to one verb, and it earned its keep immediately — it caught
C16's unauthorized `/api/version` reader at merge time, a genuine
cross-phase composition failure no unit test could see. Class 3 has
occurred at least three times and will occur again; the scan should be a
declaration, not a rewrite.

```go
astscan.Rule{
	Name:         "every warm producer consults model_classes (C4)",
	Dir:          ".",
	Trigger:      []string{"warmFn", "warmViaFront"},
	Require:      []string{"warmClassRefusal", "WarmClassRefusal"},
	Exempt:       map[string]string{ /* function name -> written reason */ },
	MinProducers: 3,
	Because:      "…what breaks if a producer skips the rung",
}
```

Two guards come with the engine rather than being remembered per scan:

- **`MinProducers`** — a rule that matches nothing PASSES, which is how a
  rename retires a guard nobody notices is gone. C15's hand-rolled floor
  was `found == 0`; the rule-based one is the actual producer count, so
  deleting three of four is caught too.
- **`Exempt` is a map to a REASON, and an unused exemption is an error.**
  C16's stale-exemption idea, generalised.

Deliberately syntactic. Resolving types needs either the deprecated
`ParseDir`-then-`Check` dance or `golang.org/x/tools`, and this repo adds
no dependency; a syntactic scan over-reports rather than under-reports,
which is the safe direction for a guard.

### The instances

- **C15's rule, in both packages.** `fleetapi` (4 producers: `getJSON`,
  `readSwapVersion`, `warmViaFront`, `streamCell`) and `fleetmcp` (3:
  `toolWarmModel`, `toolUnloadModel`, `getJSON`) — the "1 of 7" from the
  class table. Both twins now express the same rule; both keep their own
  floors. C15's nil-authorizer escape hatch
  (`TestOnlyTheCellSideReaderSkipsSwapAuth`) is untouched: this rule sees
  the CALL, that one sees what was passed.
- **NEW: `TestEveryWarmProducerConsultsTheClassGuard`.** Every function
  in `fleetapi` that fires a warm must consult `warmClassRefusal`:
  `restore`, `evalScheduleEntry`, `wakeWarm`. This is the guard that has
  *already* been shipped incomplete once — until the 2026-08-05 live gate
  only `warm_model` honoured `model_classes`, and a `warm_schedule` fired
  five 500-ing chat completions at an embed-class id and then queued them
  to the cell.

The rule is about the FIRE-time rung rather than the wiring-time one on
purpose: the wiring rung produces a status row, but the fire-time rung is
what decides whether a request leaves the box.

## 4. Class 8: `internal/shelllint`

The one blocker in this plan's history that was not a Go defect (C17 A1):
`gate-c13-parity.sh` ran `cd "$LAB/etc/vibe/backends"` bare, under
`set -uo pipefail` with **no `-e`**, and then `git init`,
`git config user.email/name`, `git add -A`, `git commit`. With a wrong or
absent `FLEETLAB_DIR` the `cd` fails, the shell stays in the operator's
current directory, and those four commands run in the operator's own
repository. It was reproduced in a scratch repo: the rig rewrote the
local git identity and committed the working tree as *"fleetlab defs"*.

The rigs are not incidental. Every live gate in this plan runs through
them, they run beside a production llama-swap on `:9000` and a production
vibe daemon on `:9001`, and they are written fast. Three rules:

| rule | what it catches |
|---|---|
| `unguarded-cd` | a `cd` whose failure nothing handles, in a script with no `set -e`. `cd … \|\| exit 1` and `( cd … && … )` both pass |
| `rm-rf-bare-var` | `rm -rf "$VAR"` — `set -u` catches UNSET but not EMPTY, and an empty expansion deletes the current directory. `${VAR:?}` and `${VAR:-…}` pass |
| `unscoped-kill` | a `pkill`/`killall` pattern with no variable in it, which cannot be scoped to this rig and is entitled to kill a sibling lab's processes (futures item 15, from the port side) |

Stdlib only: shellcheck is a binary this repo does not vendor and CI
would have to install, and these three rules are the ones this project's
own history produced.

**It found five live `rm -rf` hazards** on the first run —
`gate-c13-parity.sh`, `gate-c15-warm-auth.sh`, `gate-c19-drill.sh` and
both `scripts/smoke/llama-swap/` rigs — all fixed here by adopting
`gate-c19-drill.sh`'s own `${SB_STATE:?}` idiom. Two `unscoped-kill`
findings in `gate-c15-warm-auth.sh` are **exempted with written
reasons**: llama-server's argv carries llama-swap's config path and not
the rig's `$LAB`, so its private `596x` port range is the only handle the
sweep has. Recorded rather than silently allowed, so the next reader
finds the hazard instead of rediscovering it — and futures item 15
(`FLEETLAB_PORT_BASE`) is the real fix.

The exemption key is `file:line:rule`, which moves with any edit on
purpose: an exemption should not outlive the line it exempts.

## 5. Every check proven both ways

Ground rule 10 applied to this phase's own deliverable. For each new
check: a planted violation observed RED, and an inertness assertion that
fails when the scan's own target is empty.

| check | proven to CATCH by | proven not INERT by |
|---|---|---|
| `TestObservedIsNeverReadWithADiscardedKnownBit` | registry entry `c20/an observed.Value read…` (rewrites `activity.go`'s read to drop the bit) | floor: ≥8 two-value `Observed()` reads module-wide |
| `TestNoNewValueAndKnownBitFieldPair` | registry entry `c20/a new (value, known-bit) field pair` | floor: ≥100 struct types examined; unused exemptions are errors |
| `TestNoNewMeasurementAndBoolReturn` | registry entry `c20/a new (measurement, bool) return` | floor: ≥500 functions with results; unused exemptions are errors |
| `astscan` (engine) | `TestRuleFindsTheUnguardedProducer` against a testdata fixture | `TestInertScanIsAnError`, `TestStaleExemptionIsAnError`, `TestUnreadableDirIsAnError` |
| `TestEveryLlamaSwapRequestIsAuthorized` (×2) | registry entry `c15/streamCell drops…` | `MinProducers` 4 and 3, actual counts |
| `TestEveryWarmProducerConsultsTheClassGuard` | registry entry `c4/the warm class guard…` | `MinProducers` 3 |
| `TestScriptsAreSafe` | registry entry `c20/an unguarded cd…` (un-guards `gate-c19-drill.sh`'s `cd`) | floor: ≥15 `.sh` files; unused exemptions are errors; each rule has its own catch + false-positive table |
| the mutation runner | `TestRunnerReportsAnUnprotectedGuard`, `TestRunnerRejectsANonCompilingMutation`, `TestRunnerRejectsAStaleFind` | baseline round; registry floor of 10 entries |

`TestCleanRuleReportsNoError` is the fourth kind of proof and matters
too: a check nobody can satisfy is a check the next agent deletes.

## 6. Files

New:

- `internal/vibe/observed/` — `observed.go` (the type),
  `observed_test.go`, `scan_test.go` (the three module-wide scans).
- `internal/astscan/` — `astscan.go` (the rule engine), `astscan_test.go`,
  `testdata/sample/sample.go` (the engine's own fixture).
- `internal/mutation/` — `mutation.go` (types, registry, tree copy, patch),
  `mutation_test.go` (the audit, the runner, the runner's own guards).
- `internal/shelllint/` — `shelllint.go`, `shelllint_test.go`.
- `internal/vibe/fleetapi/c20_test.go` — the warm-class producer rule.
- `internal/vibe/daemon/c20_test.go` — the drain-wait regression.
- `docs/design/fleet-control-plan/c20-invariant-harness.md` — this file.

Changed:

- `internal/vibe/fleetapi/fleetapi.go` — one `inFlight` map, `InFlight`
  returns `observed.Value[int]`.
- `internal/vibe/fleetapi/watcher.go` — the fold, the disarm, the clear,
  `modelLastActivity`.
- `internal/vibe/fleetapi/{activity,warmtarget,warmsched,probe,sleepsched}.go`
  — `.Observed()` at the read sites; `observesActivity` and
  `cellLastActivity`.
- `internal/vibe/daemon/cell_drain.go` — the mid-wait evidence fix.
- `internal/vibe/daemon/cell_suspend.go` — read site.
- `internal/vibe/fleetapi/c15_test.go`, `internal/vibe/fleetmcp/c15_test.go`
  — C15's scan expressed on `astscan`, with real floors.
- `.github/workflows/ci.yml` — the `mutation` job.
- five rigs under `scripts/` — `${VAR:?}` on `rm -rf`.

`internal/vibe/proxy` diff: **empty**. `go.mod`/`go.sum`:
**byte-identical**. No new dependency; `go/ast`, `go/parser`, `go/token`,
`os/exec` and `regexp` are stdlib.

## 7. What this phase does NOT mechanise, and why

Recorded rather than attempted, because a check that fires on correct
code is one the next agent turns off.

- **Class 2 (the sending side guards, the receiving side doesn't).** The
  general form needs to know that two functions are the two ends of one
  wire, which is a semantic fact no syntactic scan carries. What is
  reachable — and is the right shape for a future phase — is the
  *specific* form: `astscan` can already express "every handler reached
  from `POST /api/fleet/announce` must call `clamp`", once the
  reachability is spelled as a call-graph walk rather than a per-function
  check. C14's "all THREE producers hold the structural refusals,
  including the receiving side" is the pattern to bind it to.
- **Class 5 (lock-escaping pointer).** `go test -race` already catches
  the *consequence* when a test exercises the path; C4's shipped race was
  caught by review because no test drove two goroutines at it. A
  syntactic scan for "a pointer read under `s.mu` and dereferenced after
  `Unlock`" needs escape analysis, which needs types. The cheaper answer
  is `-race -count=5` on the concurrency-bearing packages, which ground
  rule 10 already demands.
- **Class 6 (context ignoring shutdown).** A scan for
  `context.Background()` inside a function that also touches `s.wg` is
  writable and would have caught the 10-minute `Close()` hang. It is not
  here because the false-positive rate on `daemon` and `cli` is
  unmeasured and this phase should not ship a noisy check. Sized: ~40
  lines on `astscan` once someone runs it over the tree and writes the
  exemptions.
- **Class 7 (a test asserting less than its name claims).** Not
  mechanisable in general — it is a claim about the relationship between
  a name and a body. What IS mechanisable is exactly what the mutation
  registry does: a test whose name claims to guard X, listed against a
  mutation of X, is red or the claim is false. Every registry entry is a
  class-7 check on its own `MustFail` list. The remaining gap is tests
  *not* in the registry, and the honest answer is that growing the
  registry is the fix.

## Acceptance gates

### Unit

| # | gate | result |
|---|---|---|
| U1 | `observed.Value`'s zero value is unknown; a measured zero is distinguishable from an unmeasured one and does not compare equal to it | PASS |
| U2 | JSON spells an unknown `null` and round-trips both ways | PASS |
| U3 | The whole in-flight path reads through `observed.Value`; the existing C4/C5/C8/C10/C14 in-flight tests pass unchanged | PASS |
| U4 | `drain --wait` reports `skipped_no_inflight_data`, never `waited`, when the in-flight report stops mid-wait (real watcher, real stream drop) | PASS |
| U5 | No `x.Observed()` in the module discards the known bit; the scan is non-inert (≥8 real reads) | PASS |
| U6 | No new `(value, knownBit)` struct field pair; two exemptions, both written; an unused exemption is an error | PASS |
| U7 | No new `(numeric, bool)` return; seven exemptions, each written; an unused exemption is an error | PASS |
| U8 | `astscan` finds an unguarded producer, exempts exactly one function, errors on an inert rule, errors on a stale exemption, errors on an unreadable dir, and is satisfiable | PASS |
| U9 | C15's scan holds in `fleetapi` (≥4 producers) and `fleetmcp` (≥3) on the rule engine, and still fails for a genuinely unauthorized builder | PASS |
| U10 | Every warm producer in `fleetapi` consults `warmClassRefusal` (≥3 producers) | PASS |
| U11 | `scripts/` is clean under all three shell rules (≥15 files); each rule catches its hazard and does not fire on the guarded idioms; two exemptions, both written | PASS |
| U12 | The mutation registry is current: ≥10 entries, each `Find` matching exactly once, each `MustFail` test actually declared, no duplicate names, every entry carrying a `Why` | PASS |
| U13 | The runner refuses a stale/ambiguous `Find` without writing, reports an inert mutation as UNPROTECTED, and reports a non-compiling mutation as a registry error | PASS |
| U14 | **16/16 registry mutations caught**, on a clean baseline, in 21 s warm / 58 s cold | PASS |

### Inner loop

`go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy`
byte-identical, `golangci-lint run` **0 issues**,
`go test -race -timeout 300s ./...` green module-wide, and
`go test -race -count=3` green over `fleetapi`, `daemon`, `fleetmcp`,
`observed`, `astscan`, `shelllint`, `mutation`, `fleetmirror`,
`fleetnotify`.

### Live

**None, and that is not a deferral.** Everything in this phase is a
property of the source tree and of `go test`; there is no fleet
behaviour to watch. The one production change — `awaitQuiescence`'s
mid-wait refusal — is exercised against a real `fleetapi.Server`, a real
watcher and a real SSE stream in `internal/vibe/daemon`, and the drop it
simulates is a closed connection, which is the same event a cell restart
produces. What a fleetlab run would add is the same assertion with more
processes around it; `scripts/fleetlab/gate-c11-l3.sh`'s drain path is
where it would go if someone wants it.

The five `rm -rf` edits to the rigs are `bash -n` clean and were reviewed
by hand; they change no behaviour except aborting on an empty variable.

## For the reconciliation pass

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. What belongs in each:

### AGENTS.md

A new section, "The invariant harness (fleet-control C20)":

- **`internal/vibe/observed` is where absent evidence lives.**
  `observed.Value[T]`'s zero value is UNKNOWN; the value is unexported so
  it cannot be read without the bit; `OrElse` is how a caller WRITES DOWN
  what absence means. `Server.InFlight` returns one, and so do
  `modelLastActivity` and `cellLastActivity`. Three module-wide scans in
  that package's tests keep the old shape out — a discarded known bit, a
  `(value, knownBit)` field pair, a `(numeric, bool)` return — each with
  a written-reason exemption table and an inertness floor. Add to the
  table with a reason; do not delete a scan.
- **`internal/mutation` is the review step, encoded.** A registry of
  `{name, file, find, replace, pkg, mustFail, why}`. A guard whose
  mutation leaves every named test green is UNPROTECTED; an entry whose
  `Find` stops matching is STALE and must be re-pointed, never deleted; a
  mutation that does not compile proves nothing. The staleness audit runs
  in the blocking test job (milliseconds); the runner is its own CI job
  behind `VIBE_MUTATION_TEST=1` (21 s warm, 58 s cold). **When a review
  pass mutation-verifies a guard by hand, add the entry** — that is the
  whole point.
- **`internal/astscan` is the reusable "every function that does X must
  call Y" scan.** C15's credential rule (both packages) and C4's warm
  class rule are the instances. `MinProducers` is not optional: a rule
  that matches nothing passes. An exemption is a function name mapped to
  a reason, and an unused exemption is an error.
- **`internal/shelllint` covers `scripts/`**: unguarded `cd` under no
  `set -e`, `rm -rf` on a bare `$VAR` (use `${VAR:?}` — `set -u` catches
  unset, not empty), and `pkill` patterns with no variable in them. Two
  exemptions, both in `gate-c15-warm-auth.sh`, both written down.
- Under the drain notes: **`drain --wait` refuses to claim quiescence
  from the LOSS of the in-flight report.** An unreported count mid-wait
  returns `skipped_no_inflight_data`, not `waited` — the stream can drop
  (`clearInFlight`) or send an unfoldable frame (`disarmInFlightLocked`)
  while the request is still running.

### docs/design/fleet-control-plan/README.md

- A `C20` row: *The invariant harness: the recurring defect classes, made
  mechanical* — ~1,600 lines, depends on C1–C19 (composition), status
  "PR open; unit gates U1–U14 green; **mutation harness 16/16**; no live
  gates by construction".
- A paragraph after C19's, along the lines of: C20 (2026-08-06) is the
  first phase whose subject is the plan's own process. Its premise is
  that ground rule 9 works and does not scale — 39+ real defects, four
  blockers, all in green code, and **the same classes every time**. Four
  rules it carries forward. **Removing a shape beats detecting it**:
  `observed.Value[T]`'s zero value is UNKNOWN and its value is
  unexported, so class 1 stops being writable on the in-flight path, and
  the migration immediately found a live defect three review passes had
  read past (`drain --wait` reporting the loss of its evidence as
  quiescence). **A mutation table is data, not prose**: `internal/mutation`
  runs the `| mutation | red |` tables the addenda already carry, with an
  UNPROTECTED verdict when nothing fails and a STALE verdict when the
  pattern stops matching — so a refactor cannot silently retire coverage.
  **A structural scan needs a floor**: `MinProducers` and the
  unused-exemption error are what stop the next rename turning a guard
  into decoration, and C15's hand-rolled `found == 0` was too weak.
  And **the checks are proven both ways** — each one has a planted
  violation observed red and an assertion that fails when its own target
  is empty, which is ground rule 10 applied to a phase whose entire
  deliverable is tests.
- In the "what still needs metal" paragraph: nothing from C20. It has no
  live gates by construction, which is stated in the phase doc rather
  than left as an unrun row.

### docs/design/fleet-control.md

- The invariants list, or §4's ownership axes: a pointer to
  `observed.Value[T]` as the *implementation* of "fail toward no
  evidence, never toward confirmed idle" — the rule has been prose since
  C4 and now has a type.
- The status/tooling table: `internal/mutation` and `internal/astscan`
  beside the conformance matrix as the third and fourth mechanical gates.

### A note on the C20 ↔ future-phase merge

`TestNoNewValueAndKnownBitFieldPair` and
`TestNoNewMeasurementAndBoolReturn` walk the whole module, so a phase in
flight that adds either shape will go red on merge. That is the check
working. The right answer is one of: use `observed.Value[T]`; or add the
key to the exemption table with a sentence saying why the pair is not an
evidence carrier. The wrong answer is widening `pairSuffixes` or
narrowing `numericResult`.

Likewise, a phase that adds an HTTP request builder to `fleetapi` or
`fleetmcp`, or a warm producer, must raise the corresponding
`MinProducers` — the floor is a count of what exists, and lowering it to
make a deletion quiet is the failure this phase exists to prevent.
