# C16 — The upgrade ritual: digest-pin the front, make the bump a sequence

Status: **PR OPEN** (2026-08-05), off `feat/c16-upgrade-ritual` branched
from `main` at `0c275fd`. Feature commit plus ground rule 9's adversarial
self-review commit (five findings, one of them a test that would have
reported two contradictory failures about one event — see the
[self-review addendum](#adversarial-self-review-addendum)); every fix
mutation-verified. Unit gates U1–U10 green on a full local inner loop.
**Live gates L1, L2 and L3 PASS** — the new conformance behaviours ran
against real llama-swap v239 and v247 binaries, the ritual's own
`preflight`/`canary` steps ran end to end against a candidate the script
fetched itself, and both new doctor checks ran through a real fleetd
beside a real llama-swap front. L4 (the fleetlab half of `canary`) and L5
(the six-client gate) are **UNRUN**, for the honest reasons recorded in
[Execution](#execution). See [Acceptance gates](#acceptance-gates).

Backlog item 13 in [fleet-control-futures.md](../fleet-control-futures.md)
§2:

> **The upgrade ritual** — digest-pin the front image, keep the
> six-client SSE gate as a checked-in runnable script, and make "canary
> cell → gate → fleet" the only sanctioned llama-swap bump. The SSE
> keepalive defense is upstream *behavior*, not structure; it is only as
> durable as the discipline around upgrades.

## The motivating incident (2026-08-05)

`ghcr.io/mostlygeek/llama-swap:cpu` was found serving **v247**, not the
**v239** the fleet runs and every phase doc's gate transcript describes.

v240+ replaced the `/api/events` in-flight wire. A `requests` array became
operation-tagged deltas — `{"operation":"upsert","request":{…}}`,
`{"operation":"remove","id":"93376"}` — with `requests` `omitempty` and
therefore **absent**. vibe counted the length of an array that was no
longer on the wire, got `0`, and reported that zero as *known*. Eight busy
guards disarm on a reported zero: C2's `drain --wait` quiescence, C14's
suspend, C8's probe guard, both C4 warm loops, the pre-drain report.

Nothing failed. Nothing paged. And the trigger was not a decision —
`deploy/front/docker-compose.yaml:30` read
`${FRONT_IMAGE:-ghcr.io/mostlygeek/llama-swap:cpu}`, so a routine
`docker compose pull` on the front host was the whole of it. Meanwhile
`deploy/front/README.md:94` *recommended* pinning a digest. Advice the
shipped default contradicts is advice nobody follows.

PR #37 fixed the parser and added a wire-versioned double plus a
conformance matrix over v239 and v247. That closed the hole. This phase
closes the *path* — and it is worth being precise about why the two are
different work. #37 makes a wire change visible **once it is on a box you
are testing**. Nothing in the repo decided *when* a box gets a new
llama-swap, and nothing reported that one had.

## What this is not

- **Not a version policy.** Nothing here refuses to run a new llama-swap,
  and nothing auto-upgrades anything. The fleet is allowed to be
  heterogeneous — that is the normal state during any roll — and the
  repo's job is that both wires stay gated, not that one wins.
- **Not a deployment tool.** `ritual.sh pin` prints a line to paste. It
  does not `docker compose up`, does not ssh anywhere, does not restart a
  cell. Actuation across the fleet has one channel (the daemon's bearer
  control plane, invariant 5) and an upgrade script is not it.
- **Not a replacement for the six-client rig.** `scripts/smoke/llama-swap`
  stays exactly what it is. This phase makes the ritual *compose* it, and
  adds the mechanical 8-second half of the same question so CI can hold
  the floor between full runs.
- **Not observability of the front's container.** fleetd has no docker
  socket and must not grow one. It reports what it is *told* about the
  front's image, beside what it can *observe* about the front's version,
  and says which is which.

## Design

### 1. The pin is the default, not the advice

`deploy/front/docker-compose.yaml` and `deploy/front/.env.example` both
ship

```
ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869ec0908538e421172fd576288e87c1bc330acde24517992507218d2c7c
```

The tag half names the build for a human; the digest half is what docker
resolves. `docker compose pull` on the front can no longer change which
llama-swap the fleet talks to.

v239 rather than "whatever `:cpu` is today" because the reference stack
should ship the build this repo can make claims about: `deploy/front/README.md`'s
verified `-watch-config` drain grace, its unauthenticated-`/health`
warning, and this phase's SIGTERM measurements were all taken on it, and
`internal/swaptest/fixtures/v239` is a recording of its wire. Moving to
v247 is not a diff — it is `ritual.sh`, which is the point.

`TestReferenceFrontStackShipsADigestPin` reads both files and asserts the
shape of the default. It is offline by construction (CI has no network and
cannot resolve a digest), and it exists because the previous state of the
world was a README recommending what the compose file undid.

### 2. Two doctor checks, because neither answers the other's question

**`front.image_pin` is a DECLARATION.** `fleet.front_image` in fleetd's
config; the reference deployment sets it to the same reference `.env`
resolves. Four verdicts:

| value | level | why |
|---|---|---|
| contains `@sha256:` | OK | pinned |
| any other reference | **WARN** | a floating tag: *"a `docker compose pull` on the front host can change which llama-swap the whole fleet talks to, with no change to this repo and no event anywhere"* |
| `unmanaged` | OK | the front is declared not to run from a container image — a systemd llama-swap on the front box is a supported deployment with no tag to float |
| unset | UNKNOWN | nothing declares it; the fix names both of the above |

The `unmanaged` literal is a closed vocabulary rather than a silent empty,
for C13's reason: *"the operator decided"* and *"nobody told fleetd"* must
not be spelled the same way. Without it, the legitimate non-container
deployment would sit on a permanent UNKNOWN, which is the thing C13's rule
forbids — and with it, an undeclared front is honestly unknown rather than
quietly OK (`TestFrontImagePin_UndeclaredIsNotOK`).

fleetd cannot observe this. It is a different container on a host whose
docker socket it must not have, so a declaration is the only honest
mechanism — and a declaration can drift from the deployment. Which is why:

**`versions.llama_swap` is an OBSERVATION**, and it finally has a
producer. C13 shipped this check reporting UNKNOWN *naming the missing
producer*, because guessing an admin endpoint from a box that cannot
verify it is worse than an honest gap. The endpoint exists:
`GET /api/version` → `{"version":"v239","commit":"dd81801","build_date":…}`,
verified here against real v239 and v247 binaries and against the
production router before a line of the producer was written.

Two feeds, because the fleet has two shapes of cell:

- **Cells announce it.** `daemon.fleetVersionsAt` reads its own
  llama-swap's `/api/version` with a 2s timeout and puts it in the
  versions block the heartbeat already carries. The slim announcer
  (`vibe fleet announce`) gets the same producer, C13's rule. Any failure
  leaves the field empty — never a guess — and the announce is never held
  up for it, because the heartbeat is the cell's only evidence of life.
- **fleetd reads the front's directly.** The front runs no announcer by
  design (fleetd renders its config; it serves no models), so the
  announce-fed matrix structurally excluded the one box the incident
  happened to. One read-only GET on the address fleetd already probes
  closes that without inventing an announcer.

And the matrix gained the check the incident actually needed: a version
this build has **no conformance recording for** is a WARN naming itself,
ahead of the plain-divergence branch. A fleet uniformly on an ungated
version is not a mid-upgrade; it is an upgrade that skipped the ritual,
and it is invisible to every other check — the wire either parses or reads
as an idle cell.

`fleetapi.GatedSwapVersions()` is a hand-written list, because production
has no business importing a test double.
`TestGatedSwapVersionsMatchesRecordings` and
`TestConformanceMatrixCoversEveryRecording` tie it to
`internal/swaptest/fixtures/` and to `ci.yml`'s matrix: three copies of one
fact, two tests that fail when they disagree.

### 3. Untrusted input, on both feeds

A cell's announced version is the fleet token's voice like everything else
on that wire, and it already passes fleetd's `clean` hygiene
(`announce.go`, 256 bytes, printable only) — the receiving side was
already right. The sending side bounds it at 64 bytes anyway, and the
front's direct read (which is *not* announce-shaped and so gets no
`clean`) applies the same rule itself: over 64 bytes or non-printable
returns `""`, which the matrix renders as a missing row rather than as
agreement.

### 4. What the ritual must catch, and where each thing is caught

Derived from the incident, one row per failure it could have had:

| failure | caught by | runs |
|---|---|---|
| the `/api/events` in-flight wire changes | `TestSwapContract` I1, I5 — folded through the REAL `fleetapi` parser, so an unrecognised shape must reach *unknown* | CI, both pins, every PR |
| `/api/metrics/activity` changes shape | `TestSwapContract` I2–I4 — classified through the REAL `usagemeter` | CI, both pins |
| **the SSE keepalive disappears** | **`TestSwapBehaviour` B1** (new) | CI, both pins |
| **SIGTERM stream handling changes** | **`TestSwapBehaviour` B2** (new) | CI, both pins |
| upstream moves at all | `drift.yml` against `latest`, unpinned | CI, Mondays |
| what any of it does to a *fleet* | `scripts/fleetlab` on the candidate | `ritual.sh canary` |
| real clients not tolerating the keepalive | `scripts/smoke/llama-swap` | `ritual.sh gate` |
| the fleet sitting on an ungated version | `versions.llama_swap` | every `vibe fleet doctor` |
| the deployment floating | `front.image_pin` | every `vibe fleet doctor` |

The two new ones are the phase's real technical content, because they are
*behaviours*, and a wire double cannot hold a behaviour. Both need a
llama-swap PROCESS (a signal is not a wire), so both are env-gated on
`LLAMA_SWAP_BIN` exactly as the live conformance target is; neither needs
llama.cpp, because the upstream is an in-test HTTP server whose readiness
and stream pacing are the parameters under test. That also isolates the
question to llama-swap's own behaviour: a real llama-server dying on the
same signal would be answering a different one.

**B1 — the loading-state keepalive.** A streaming completion is issued at
a model whose upstream refuses health checks for 6s. The client must be
receiving bytes within 2s; the first frame must be a delta an OpenAI SSE
parser accepts (not an error payload, which also arrives fast); the
upstream must still be unready at that instant; and the *same stream* must
go on to carry the upstream's own tokens. Mutation-verified: with
`sendLoadingState: false` the first byte arrives at 6.27s and the test
fails naming the stall timer.

**B2 — SIGTERM.** Measured, not assumed, and the measurement corrected a
documented claim — see [the finding](#the-c2-sigterm-claim-is-mis-attributed)
below. Three assertions: `/api/events` closes within 2s of the signal;
an in-flight *inference* stream is still delivering 1.5s later; and it is
force-closed 30s ± 5s after the signal. The window is bounded on **both**
sides because either direction is something an operator must act on — a
shorter grace truncates generations the cell unit was configured to
protect, a longer one needs a matching `TimeoutStopSec`.

### 5. The ritual is a script

`scripts/upgrade/ritual.sh`, five subcommands, `$UPGRADE_DIR` scratch
dirs, ports 9810-9819 and upstreams 6100+ — clear of production's
`:9000`/`:9001` and of `scripts/fleetlab`'s 9600-9799.

- **`preflight <v>`** fetches the release binary, resolves the front
  image's newest `<v>-cpu-b*` digest off ghcr with an anonymous token and
  `curl`, and reports whether the repo has a recording for `<v>` and
  whether `ci.yml` replays it.
- **`record <v>`** stands the candidate over a real llama-server and runs
  `TestRecord`, then says to commit the new `fixtures/<v>/` **beside** the
  old ones.
- **`canary <v>`** runs `TestSwapContract | TestSwapBehaviour |
  TestRecordingIsCurrent` against the candidate, and on success brings up
  `scripts/fleetlab` on that binary and runs `lab.sh prove`. A failure
  stops and says not to pin.
- **`gate <v>`** builds `slowmodel`, stands the candidate with the
  smoke rig's stanza, and runs `run-smoke.sh` + `kill-cancel-test.sh`.
- **`pin <v>`** prints the `FRONT_IMAGE=` line and the four things a human
  still does.

`TestUpgradeRitualIsRunnable` asserts the script is executable and that
each step actually invokes the rig it claims to compose — a ritual that
only prints instructions is the prose it replaced.

### 6. Automatable versus human, decided honestly

The full split is in `scripts/upgrade/README.md`. The decision that
matters here: **nobody performs a checklist at 2am**, so every step whose
omission caused the incident is code rather than an instruction.

- "check the front image is pinned" → `front.image_pin`, plus a shipped
  default that is already right.
- "check every cell is on the version you gated" → `versions.llama_swap`.
- "remember both wires must keep passing" → two tests over three copies of
  one fact.
- "re-record after a bump" → `TestRecordingIsCurrent`, which fires on the
  box where the upgrade happened.

What stays human, with no pretending: applying the pin, recreating the
front container, rolling cells one at a time, reading the doctor report,
and the two manual clients in the six-client gate (Open WebUI and pi).
Those are four commands and two browser sessions against a real fleet;
writing them down as a checklist is the honest treatment, because there is
no mechanism here that could perform them.

## Files

| file | what |
|---|---|
| `deploy/front/docker-compose.yaml` | digest-pinned default + why |
| `deploy/front/.env.example` | the same pin, and the pointer to the ritual |
| `deploy/front/README.md` | "the image is digest-pinned, and moving the pin is a procedure" |
| `internal/vibe/fleetapi/upgrade.go` | `front.image_pin`, `frontSwapVersion`, `GatedSwapVersions`, `ungatedSwapVersions` |
| `internal/vibe/fleetapi/doctor.go` | the two call sites + the ungated branch of `versions.llama_swap` |
| `internal/vibe/daemon/daemon.go` | `fleet.front_image` |
| `internal/vibe/daemon/doctor.go` | `DoctorHost.FrontImage` |
| `internal/vibe/daemon/announce.go` | the `/api/version` producer, both announcer shapes |
| `internal/vibe/cli/cmd_fleet.go` | the slim announcer passes its llama-swap URL |
| `internal/swaptest/behaviour_test.go` | B1 + B2 |
| `internal/swaptest/gated_test.go` | recordings ↔ `GatedSwapVersions` ↔ `ci.yml` |
| `internal/vibe/fleetapi/c16_test.go` | the doctor checks + the reference-stack pin + the runnable ritual |
| `scripts/upgrade/ritual.sh`, `README.md` | the ritual |
| `.github/workflows/ci.yml`, `drift.yml` | `TestSwapBehaviour` joins both conformance jobs |

## Acceptance gates

Unit (mechanical, in-repo):

| # | gate | result |
|---|---|---|
| U1 | `front.image_pin` returns OK / WARN / UNKNOWN / FAIL for pinned, floating, undeclared, `unmanaged` and control-character values, each naming its reason | PASS |
| U2 | an undeclared front image is never OK | PASS (separate test, so U1's table cannot drift into asserting it) |
| U3 | `ungatedSwapVersions` names exactly the reported versions with no recording, normalising `v260 (deadbeef)` to its tag | PASS |
| U4 | `frontSwapVersion` reads the front's `/api/version`; a 404, non-JSON, control-character or over-long answer, and a fleet with no front cell, all yield absence | PASS |
| U5 | the reference front stack's compose **default** and `.env.example` are both digest-pinned | PASS |
| U6 | `ritual.sh` is executable and every step invokes the rig it claims to compose | PASS |
| U7 | `GatedSwapVersions()` equals the recorded fixture dirs | PASS |
| U8 | `ci.yml`'s conformance matrix equals the recorded fixture dirs | PASS |
| U9 | C13's read-only source scan covers `upgrade.go` too, and passes | PASS |
| U10 | full inner loop: build, vet, `test -race -count=5`, gofmt, `go mod tidy`, golangci-lint | PASS |

Live (a real llama-swap binary, a real fleet, or real clients):

| # | gate | result |
|---|---|---|
| L1 | **B1 + B2 pass against a real v239 AND a real v247 binary**, and B1 fails when `sendLoadingState` is removed | **PASS** |
| L2 | `ritual.sh preflight` and the conformance half of `canary` run end to end against a candidate the script fetched itself | **PASS** |
| L3 | both new checks through the whole path — a real fleetd, a real llama-swap front, `vibe fleet doctor` — with `front.image_pin` moving UNKNOWN → WARN → OK → OK as the declaration changes and `versions.llama_swap` naming the version the front actually answers | **PASS** |
| L4 | the fleetlab half of `canary`: four real cells on the candidate binary, `lab.sh prove` green | **UNRUN** — see Execution |
| L5 | `ritual.sh gate`: the six-client rig at `DELAY_S=90` against a candidate | **UNRUN** — a time budget (~15 min at 90s, ~45 at 420s) plus two manual clients |
| L6 | the pin applied on the real front, doctor reporting `front.image_pin` OK and `versions.llama_swap` naming one version across the whole fleet | **UNRUN** — needs the fleet |

## Execution

### Verified upstream facts

Everything this phase asserts about llama-swap was measured on this box
before it was written down, against `~/.local/bin/llama-swap` (v239, the
version production runs) and a v247 binary extracted from
`ghcr.io/mostlygeek/llama-swap:cpu`:

- **`:cpu` is v247.** `docker run … --version` →
  `version: v247 (40027d6), built at 2026-08-04T05:36:51Z`. The digest
  behind the tag today is `sha256:a928468…`; `v239-cpu-b9994` is
  `sha256:6bae869…`. The incident report is confirmed independently.
- **`GET /api/version` exists on both** and answers the same shape. Also
  answered by the production router on `:9000` (a read-only GET).
- **The loading-state keepalive** emits `reasoning_content` delta frames
  from ~5 ms into a cold start, continuously, and the same stream then
  carries the real answer.
- **SIGTERM**, measured three ways — see the finding below.

### The C2 SIGTERM claim is mis-attributed

C2's gate 2 recorded, and AGENTS.md and `fleet-control.md` repeat, that
*"llama-swap's SIGTERM path cancels in-flight streams immediately (v239
verified: `CloseStreams()` precedes the graceful drain)"*. Measured
directly, on v239 and again on v247:

| observation | v239 | v247 |
|---|---|---|
| `/api/events` SSE closes after SIGTERM | **1.2 ms** | 1.4 ms |
| an in-flight inference stream is cancelled | **no** — kept delivering | no |
| a 20s stream that started 2s before SIGTERM | completed, `[DONE]` | completed |
| a 60s stream | force-closed at **30.003 s** with `http server shutdown error: context deadline exceeded`, truncated cleanly | same |

So `CloseStreams()` closes the **event/UI** streams. Inference streams get
the same 30s grace that `deploy/front/README.md` already documents for a
`-watch-config` reload — the two are the same behaviour, not the contrast
C2 drew.

**The operational conclusion is unchanged.** C2's own transcript records a
~39 s essay stream, which is longer than the grace and therefore *would*
have been truncated; `drain --wait` quiescing before the stop is exactly
right either way, and `TimeoutStopSec` must still exceed the grace. What
changes is the attribution — and futures item 7 ("upstream: SIGTERM-time
stream grace") is aimed at behaviour that does not exist as described:
upstream already grants 30s, and the request worth making is that the
grace be configurable.

This is a doc correction across files this branch may not touch; it is
written up under [For the reconciliation pass](#for-the-reconciliation-pass)
rather than applied. B2 now pins the measured behaviour, so the next
change to it is a test failure rather than a rediscovery.

### Gate results

**L1 PASS.** `TestSwapBehaviour` against `LLAMA_SWAP_BIN=~/.local/bin/llama-swap`
(v239): B1 6.44 s, B2 30.31 s, both green. Against the v247 binary: B1
6.50 s, B2 30.30 s, both green. Mutation runs, each reverted after:
`sendLoadingState: false` → B1 fails at 6.27 s naming the stall timer;
`sigtermGrace = 10s` → B2's force-close assertion fails. The suite is not
vacuous in either direction.

**L2 PASS.** `ritual.sh preflight v239` fetched the release binary,
reported `version: v239 (dd81801)`, resolved
`ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869…` (the exact
string now shipped as the compose default), and confirmed both the
recording and the CI matrix entry. `preflight v247` resolved
`v247-cpu-b10276@sha256:a928468…` — which is what `:cpu` currently points
at, closing the loop on the incident. The conformance half of `canary`
then ran the full `TestSwapContract|TestSwapBehaviour|TestRecordingIsCurrent`
list against the fetched v247 binary over a real llama-server and a real
GGUF: `ok … 38.688s`.

**L3 PASS.** A real fleetd (`fleet_registry: true`, scratch XDG triple,
control plane on `:9816`) beside a real llama-swap front on `:9815`, with
`vibe fleet doctor` run four times against it, changing only
`fleet.front_image`. Ports 9815-9817 and a scratch config dir, so nothing
touched production or another agent's lab.

```
front llama-swap: {"build_date":"2026-07-11T21:47:14Z","commit":"dd81801","version":"v239"}

### undeclared (front_image=<unset>)
UNKNOWN front.image_pin      -      nothing declares which image the front runs
OK      versions.llama_swap  -      every reporting cell runs llama-swap v239

### floating tag (front_image=ghcr.io/mostlygeek/llama-swap:cpu)
WARN    front.image_pin      -      front image is a floating tag: ghcr.io/mostlygeek/llama-swap:cpu

### digest pinned (front_image=…v239-cpu-b9994@sha256:6bae869…)
OK      front.image_pin      -      front image is digest-pinned

### unmanaged (front_image=unmanaged)
OK      front.image_pin      -      the front is declared not to run from a container image
```

Two things this proves that no unit test can. The declared half moves
through all four verdicts with the deployment as its only input. And
`versions.llama_swap` — UNKNOWN in every C13 transcript, because nothing
had ever written the field — reports a real version read off a real
llama-swap over HTTP, for the front, which announces nothing.

**L4 UNRUN, and the reason is not "needs metal".** `scripts/fleetlab`
binds fixed ports 9600-9799 and upstreams 5980-6019, and another agent's
lab instance held them for the whole of this build. Two labs cannot
coexist, and `lab.sh down`'s sweep is anchored partly on that shared
upstream port range — running mine would have been entitled to kill
theirs. This is a harness limitation worth fixing (a port-offset knob) and
it is noted for the futures doc rather than papered over. The step is
scripted and the command is
`FLEETLAB_DIR=… LLAMA_SWAP=<candidate> ./scripts/upgrade/ritual.sh canary <v>`.

**L5 UNRUN.** A time budget, not a hardware one: ~15 minutes at
`DELAY_S=90` for the automated five clients, ~45 at the recorded 420s, plus
two manual clients. The rig itself is unchanged and its last recorded runs
are checked in at `scripts/smoke/llama-swap/results-*.txt`.

**L6 UNRUN.** Applying the pin and rolling cells needs the fleet; SSH is
blocked and the LAN does not route from here.

### Adversarial self-review addendum

Ground rule 9, run against the feature commit. Five findings, all fixed
with the fix mutation-verified.

**REV-1 (blocker) — a test that would have reported two contradictory
failures about one event.** B2's `inflClosed` was a buffered
`chan time.Time` written once by the scanner goroutine and read by *two*
assertions: (b) "the stream is still delivering 1.5s after SIGTERM" and
(c) "it is force-closed at the grace deadline". Whichever ran first
consumed the only value — so on a build that *did* cancel streams
immediately (exactly the case B2 exists to notice) (b) would fail
correctly and (c) would then block for 45s and report "the in-flight
stream was still open", about a stream it had just watched close. The
same buffered-channel mistake had already cost this build an hour in
`startSwap`, where cleanup and `terminate` both waited on `done`.

Fixed by closing the channel and recording the instant beside it
(`atomic.Int64`), so every waiter observes it. Verified by shrinking the
upstream to a 2-chunk stream: the fixed test fails in **0.71s** with
`(b) stopped within 1.5s` **and** `(c) ended 404.862229ms after SIGTERM` —
both true. The old shape would have added a 45s false claim.

**REV-2 — C16's checks escaped C13's read-only structural scan.**
`TestDoctor_ReadOnly…`'s AST scan enumerates the files on the doctor path
by name (`doctor.go`, `../daemon/doctor.go`, `../fleetmcp/doctor.go`,
`../cli/cmd_fleet_doctor.go`). `fleetapi/upgrade.go` contributes two
checks to the report and *dials the front*, and was in none of them. A
guard that lives in four of five files is C13's own lesson one file over.
`upgrade.go` added to the list.

**REV-3 — `ritual.sh pin` warned where it had to refuse.** It printed
`WARNING: no recording for <v>. canary cannot have passed. Stop.` and
then printed the `FRONT_IMAGE=` line anyway. That is precisely the
checklist step nobody performs at 2am, and what it prints is the
2026-08-05 configuration formatted as a result. Now a `die`.

**REV-4 — `in_ci_matrix` used `grep -E '\b'`**, a GNU extension, in a
script whose binary fetch claims Darwin support. Replaced with a
`tr`/`grep -qx` pipeline.

**REV-5 — the normalisation was untested in the direction that matters.**
`ungatedSwapVersions` matches after trimming a build string, but the test
only exercised `v260 (deadbeef)`, which is ungated whether or not the
trim happens. Added `v239 (dd81801)` → gated, and stated in the test that
the *reported* string stays verbatim.

Two things looked wrong and are deliberate:

- **`llamaSwapVersion` uses `context.Background()` with its own 2s
  timeout** rather than the announce context, which is C4's `warmCtx`
  smell. It is the existing shape of `fleetCapacity` on the same
  heartbeat (`nvidia-smi`, 3s), and 2s is well inside the daemon's
  `announceStopTimeout` of 3s, so it cannot extend shutdown past a bound
  that already exists. Threading a context through
  `Versions func() *AnnounceVersions` would change a C3 interface for no
  reachable failure.
- **`front.image_pin` is emitted on a non-registry daemon too**, as an
  UNKNOWN beside `fleetd.role`'s WARN. Every other check behaves the same
  way there, and the report already leads with "this daemon is not a
  fleet registry".

## For the reconciliation pass

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. What belongs in each:

### AGENTS.md

Under "Test doubles and upstream contracts", after the fixtures bullets:

- **Two upstream BEHAVIOURS are gated, not just the wire**
  (`internal/swaptest/behaviour_test.go`, C16). The loading-state
  keepalive (a streaming completion at a still-loading model must be
  delivering parseable delta frames within 2s, and the same stream must go
  on to carry the model's own tokens) and SIGTERM's stream handling
  (`/api/events` closes at once; in-flight *inference* streams are NOT
  cancelled; anything still running at the 30s grace is force-closed).
  Both are env-gated on `LLAMA_SWAP_BIN` because a signal is not a wire,
  and both use an in-test HTTP upstream so the question stays about
  llama-swap rather than about llama.cpp. The grace window is bounded on
  both sides on purpose — either direction changes what
  `TimeoutStopSec` has to be.
- **`llama-swap's SIGTERM path does NOT cancel in-flight inference
  streams.`** Correct the C2 bullet under "Fleet actuation": measured on
  v239 and v247, `CloseStreams()` closes the EVENT streams (`/api/events`
  drops in ~1ms) while inference streams keep flowing and are force-closed
  at a hardcoded 30s — the same grace a `-watch-config` reload gets, not a
  contrast with it. `drain --wait` is unaffected and still required: a
  generation longer than the grace is truncated either way, which is what
  C2's ~39s essay actually observed.

A new section, "The upgrade ritual (fleet-control C16)":

- **The front image is digest-pinned by default**, and moving the pin is
  `scripts/upgrade/ritual.sh` (preflight → record → canary → gate → pin),
  never an edit. `TestReferenceFrontStackShipsADigestPin` fails if the
  reference compose default or `.env.example` goes back to a floating tag.
- **`front.image_pin` is declared, `versions.llama_swap` is observed, and
  the pair is the point.** fleetd has no docker socket and must not grow
  one, so whether the deployment is pinned can only be declared
  (`fleet.front_image`, with `unmanaged` as the closed-vocabulary
  declaration that the front runs no container). What catches a
  declaration nobody applied is the observed version matrix.
- **`versions.llama_swap` has a producer** (C13 reported it UNKNOWN naming
  the gap): each cell reads its own llama-swap's `GET /api/version`,
  verified against real v239 and v247 binaries; fleetd reads the FRONT's
  directly, because the front runs no announcer and is the box the
  incident happened to. Failure yields absence, never a guess.
- **A version with no recording is a WARN ahead of plain divergence.**
  `fleetapi.GatedSwapVersions()`, `internal/swaptest/fixtures/` and
  `ci.yml`'s matrix are three copies of one fact, pinned to each other by
  two tests. Adding a recording without adding it to the other two is red.

### docs/design/fleet-control-plan/README.md

- A `C16` row: *The upgrade ritual: digest-pin the front, make the bump a
  sequence* — ~700 lines, depends on #37's conformance work, status
  "PR open; unit gates U1-U10 green; **L1-L3 PASS**, L4-L6 unrun".
- A paragraph after C14's, along the lines of: C16 (2026-08-05) is backlog
  item 13, and it is the first phase whose subject is the repo's own
  discipline rather than the fleet's state. Its one carried rule is that
  **a defence that lives in upstream behaviour is only as durable as the
  pin under it**: the SSE keepalive and SIGTERM's stream grace are things
  llama-swap *does*, not things this repo owns, and a floating tag on the
  front turned "we verified this" into "we verified this once". Two
  corollaries worth keeping. **The declared and the observed halves are
  both required** — fleetd cannot see the front's image and a config value
  cannot see the running version, so `front.image_pin` and
  `versions.llama_swap` are a pair, and either alone reads as an answer.
  And **the mid-state is the normal state**: old recordings are kept
  rather than replaced and CI replays every one, because a fleet spends
  most of an upgrade with two llama-swap versions in it.
- Also worth a line in the "what still needs metal" paragraph: C16's L4 is
  blocked by `scripts/fleetlab`'s fixed ports rather than by hardware —
  two lab instances cannot coexist on one box, which the parallel-agent
  workflow now hits routinely.

### docs/design/fleet-control.md

- §7 (or wherever C2's drain semantics are stated): correct the SIGTERM
  claim as above — event streams are cancelled immediately, inference
  streams get a hardcoded 30s grace, and `drain --wait` remains the answer
  because a generation longer than the grace is truncated.
- The "honest gaps" / upstream-opportunity list: futures item 7 should be
  restated as *"make llama-swap's 30s shutdown grace configurable"*, not
  *"stop cancelling streams on SIGTERM"* — the latter describes behaviour
  upstream does not have.
- The status table's `fleet doctor` row (or §3's fleetd description): note
  that `versions.llama_swap` now has a producer, and that the front's
  version is read directly because it runs no announcer.

### fleet-control-futures.md (this branch DOES own it)

Item 13 is marked SHIPPED with the three notes worth carrying, and item 7
gets the correction. A new small item is added for `scripts/fleetlab`'s
fixed ports.
