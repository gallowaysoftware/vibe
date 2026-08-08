# C23 — a port base for `scripts/fleetlab`: one lab's teardown cannot reach another's

Status: **PR OPEN**, off `c23-fleetlab-port-base` branched from `main` at
`499d4e8`. Backlog item 15
([fleet-control-futures.md](../fleet-control-futures.md) §2):

> **A port offset for `scripts/fleetlab`.** It binds fixed ports
> (9600-9799, upstreams 5980-6019), so two lab instances cannot coexist
> on one box — and `down`'s sweep is anchored partly on that shared
> upstream range, so the second instance is entitled to kill the first's
> processes. This blocked C16's L4 gate outright. One
> `FLEETLAB_PORT_BASE` knob threaded through `CELL_LIST` and the sweep
> patterns; small, and the parallel-agent workflow this repo now uses
> hits it immediately.

The entry reads like a convenience knob. It is not, and the difference
decides the shape of the phase: **the teardown sweep is the dangerous
half**. Two labs failing to start is an inconvenience that announces
itself in the first ten seconds. One lab's `down` killing the other's
llama-servers is a *silent* failure that surfaces somewhere else — in
another operator's session, as a gate that fails for a reason nowhere in
their diff. So the acceptance bar for this phase is not "two labs can
start". It is **instance A's `down` provably cannot touch instance B's
processes**, with a test that goes red when the fix is removed.

It also closes C16's L4, which had been recorded as unrun for exactly
this reason. L4 ran on 2026-08-08 and **passed** — §5.

---

## 1. What was actually shared

`lab.sh`'s `sweep` had four patterns. Three were already anchored on the
instance's own `$FLEETLAB_DIR`:

```
llama-swap -config $LAB/
$LAB/bin/vibe
$LAB/bin/notify-sink.py
$TAG-hostprobe            # TAG = basename $LAB
```

The fourth was not:

```
pgrep -f 'llama-server .*--port (59[89][0-9]|60[01][0-9])'
```

A regex over 5980-6019 — a constant, hardcoded twice (TERM and KILL), and
identical in every instance because every instance had identical ports.
`reap_orphan_upstreams` had a second spelling of the same idea
(`--port ${sport:0:3}[0-9]`, a three-digit-stem prefix trick).

**Why the llama-server children are the hard half.** They are not started
by the lab; llama-swap starts them, building their argv from the rendered
config. Their command line carries the *model* path and the *port* and
nothing that says which lab they belong to. Anchoring them on
`$FLEETLAB_DIR` the way the other three are anchored would mean changing
what `vibe router render` emits — and `vibe fleet doctor`'s `defs.parity`
check re-renders and diffs, so a lab-only argv would read as drift. Their
port therefore *is* their identity, which is why the port table had to
become per-instance before the sweep could be fixed.

## 2. The design

### One knob, one table

`FLEETLAB_PORT_BASE` (default **9600**) is the base of a 200-port block.
[`scripts/fleetlab/ports.sh`](../../../scripts/fleetlab/ports.sh) derives
everything from it and both `lab.sh` and `gl.sh` source it — there is no
second copy of the port list, which was explicit in the backlog entry and
is the reason `gl.sh` was in scope at all.

| | derivation | default |
|---|---|---|
| cells front/alpha/bravo/charlie | `base+40 … +43` | 9640-9643 |
| host probes | `base+51 … +53` | 9651-9653 |
| fleetd proxy / control | `base+120` / `base+121` | 9720 / 9721 |
| bravo's cell daemon | `base+123` | 9723 |
| C9 webhook sink | `base+124` | 9724 |
| **upstream startPorts** | `ups+0/10/20/30`, `ups = base-3620` | 5980/5990/6000/6010 |
| listen window | `[base, base+199]` | 9600-9799 |
| upstream window | `[ups, ups+39]` | 5980-6019 |

The upstream window keeps its historical **distance** from the listen
window rather than moving inside it, and that is deliberate: the default
base then reproduces today's table byte for byte, and every gate script,
phase-doc transcript and README table that names 9641 or 5990 keeps
working unchanged. `TestDefaultBaseIsTodaysTable` asserts the whole table
against the literal values, so "the default preserves today's ports" is a
checked claim rather than an intention.

### The collision rule: multiples of 200

A base must be a multiple of 200, and the script **refuses** one that is
not. This is not tidiness — it is what carries the sweep. Two instances
whose bases differ therefore differ by at least a full block, so both
their listen windows *and* both their upstream windows are disjoint by
construction. That disjointness is the entire safety argument for the one
sweep pattern that cannot be path-anchored.

The alternative — let any base through and rely on the operator — makes
the sweep's correctness depend on arithmetic done by a human at 1 a.m.
between two other tasks. Ground rule: if a property is load-bearing,
check it mechanically.

### The production guard

`ports.sh` refuses a base whose listen **or** upstream window overlaps:

| range | what it is |
|---|---|
| 9000-9001 | the production llama-swap and the production vibe daemon |
| 5800-5809 | production's upstream range |
| 9810-9819 | `scripts/upgrade/ritual.sh`'s own listeners |
| 6100-6139 | `ritual.sh`'s own upstream range |

Both windows are checked against all four, because the mapping puts the
upstreams 3620 below the base — so a base of **12600** is harmless-looking
and puts upstreams on 8980-9019, which covers `:9000`. That case has its
own test.

The guard runs at *source* time, so it fires before `down` reaches the
sweep. `TestGuardRefusesBeforeAnythingIsSwept` proves that with a live
victim process: `down` on base 9000 exits 2, prints the reason, never
prints `down (idempotent)`, and the victim is still running. A guard that
reports the mistake after the kill is not a guard.

The last two rows are a superset of what the backlog asked for. They are
in because `ritual.sh` *drives* a lab instance (C16's canary step 2), so
it is a base picker like any other and must not be able to eat its own
ports. The practical consequence is that 9800 is not a legal base; 10000
is the next one after the default.

### What `down` is anchored on now

Three `$FLEETLAB_DIR` patterns, unchanged, plus:

```
lab_upstream_pids "$LAB_UPSTREAM_LO" "$LAB_UPSTREAM_HI"
```

which reads `ps -eww -o pid=,args=`, matches `llama-server`, extracts the
`--port` value and compares it **numerically** against this instance's own
derived window. No regex over a digit range, no constant, and one helper
instead of the four hardcoded copies (two in `sweep`, one in
`reap_orphan_upstreams`, one in the prefix trick). `reap_orphan_upstreams`
now calls the same helper with the single cell's ten ports.

## 3. Gates

Unit — `internal/fleetlab`, `go test -race`:

| # | gate | result |
|---|---|---|
| U1 | **two instances on two non-default bases; `down` on A leaves every one of B's four process shapes alive AND B's upstream still answering HTTP 200** | PASS |
| U2 | *(negative control)* the same two instances on **one shared base**: A's `down` kills B's llama-server, while B's `$LAB`-anchored processes survive | PASS |
| U3 | the default base reproduces today's table exactly, field by field | PASS |
| U4 | a non-default base shifts **every** number in the table by the same amount and leaves nothing on the old block (bases 10000, 10200, 20000) | PASS |
| U5 | the guard refuses: listen window over `:9000`/`:9001`; upstream window over `:9000` (base 12600); listen window over production upstreams; listen window over `ritual.sh`; a base that is not a multiple of 200; a non-numeric base; an upstream window below 1024 | PASS |
| U6 | the guard refuses **before the sweep runs** — a live victim process survives a `down` on a refused base, and `down (idempotent)` is never printed | PASS |
| U7 | `internal/shelllint` still green over the new `ports.sh` and every edited rig | PASS |
| U8 | full inner loop: `go build`, `go vet`, `go test -race ./...`, `gofmt -l`, `go mod tidy`, `golangci-lint run` | PASS |

Live (a real four-cell lab, real llama-swap processes):

| # | gate | result |
|---|---|---|
| L1 | a full `lab.sh up` on a **non-default** base (10200): four cells `SERVING`, both announcer shapes, front rendered peers-only, a real embedding served through alpha, and the llama-server upstream observed on **6591** — inside the derived window, not the default one | PASS |
| L2 | `lab.sh down` on that instance: every lab process gone, `:9000` and `:9001` still listening | PASS |
| L3 | **C16's L4** — `ritual.sh canary v247` end to end on port base 10200 | PASS — see §5 |

### U1/U2 are one gate, and the second half is why the first means anything

U1 alone is consistent with a sweep that kills nothing at all. U2 puts the
two instances on **one** base — which is the world before this phase,
since every instance used to share one table — and asserts B's
llama-server *dies*. It also asserts B's other three survive, which
isolates the mechanism: the `$LAB`-anchored patterns were never the
problem and are not what U1 is testing.

### The mutation

The fix was neutered in the working tree — `sweep`'s derived window
replaced by a constant spanning both instances, i.e. "anchored on a shared
constant", the exact defect — and U1 run again:

```
--- FAIL: TestDownCannotReachAnotherInstance (1.30s)
    A base 20000 (upstreams 16380-16419), B base 20200 (upstreams 16580-16619)
    Messages: B's processes after A's down: lab-b/llama-server should still be running
```

Red, naming the right process, in 1.3 s. Fix restored, green again.

### How the stand-ins work, and why that is honest

The test cannot stand up two real four-cell labs in CI — no llama-swap
binary, no GGUFs, minutes of wall clock. It does not need to. Both
`pgrep -f` and the upstream scan match on **argv**, so a stand-in only has
to wear the right command line to be exactly as killable as the real
thing. Each instance gets four: this test binary, copied to a path whose
basename is the one the pattern names, re-executed in fake mode. The
llama-server stand-in deliberately lives in a **shared** bin dir carrying
no lab path at all — which is precisely the real llama-server's situation,
and means its port is the only handle the sweep has on it. The survivor is
proven to be still *serving*, not merely still scheduled, by an HTTP GET.

One trap worth recording. The first cut checked liveness with
`Process.Signal(0)`. These are the test's own children, so between exiting
and being reaped they are zombies — and `kill(pid, 0)` on a zombie
succeeds. A swept process would have read as a survivor and U1 would have
passed for the worst possible reason. It reads a reaper goroutine's
verdict instead.

## 4. What else moved

- **`gl.sh` sources the same table** and exports `VIBE_API` from it, so
  every gate rig follows the lab it was pointed at. `render_cell`'s
  `STARTPORT` argument is now optional and defaults to the cell's derived
  upstream port.
- **Eleven gate rigs** had literal ports (`PORT=9641`, `SPORT=5990`,
  `http://127.0.0.1:9643`, `render_cell alpha 5990`, the notify sink's
  9724, `marionette.py`'s usage line). All now derive. This was not
  optional polish: a rig with a literal `9641`, run against a lab on base
  10200, drives *another agent's* lab — the same cross-instance defect
  wearing a different hat.
- **`ritual.sh`** documents and passes `FLEETLAB_PORT_BASE` through to
  canary step 2. Its header used to describe the collision as a deliberate
  exception; it now describes the knob.
- **`lab.sh ports`** prints the derived table. It exists so U3/U4 can
  assert against something, which is the same reason C13's rigs print raw
  evidence rather than verdicts.

### Two rigs deliberately NOT converted

`gate-c15-warm-auth.sh` builds its own two-cell fleet on 9660-9671 with
upstreams 5960-5979 under its own `C15LAB_DIR`, and has its own sweep with
its own `pkill -f "llama-server .*--port 59[67][0-9]"`. `ritual.sh`'s own
listeners (9810-9819, upstreams 6100+) are likewise fixed. Neither
collides with any lab block — the guard now enforces that from the lab
side — but **two concurrent runs of the same rig still collide with each
other**. That is unchanged by this phase, it is stated in the README and in
`internal/shelllint`'s exemption table (whose comment used to promise item
15 as the fix and now says what item 15 did and did not cover), and it is
not claimed as fixed anywhere.

## 5. C16's L4

The backlog entry says the missing knob "blocked C16's L4 gate outright",
and [C16's phase doc](c16-upgrade-ritual.md) says the same in its
Execution section. It did, and the knob unblocks it: L4 is the fleetlab
half of `ritual.sh canary`, which is `lab.sh up` + `prove` + `down` on a
candidate binary.

```
UPGRADE_DIR=/tmp/vibe-upgrade-c23 FLEETLAB_PORT_BASE=10200 \
  ./scripts/upgrade/ritual.sh canary v247
```

Ran 2026-08-08. `ritual.sh` fetched the release itself —
`version: v247 (40027d6), built at 2026-08-04T05:36:51Z`. Step 1
(`TestSwapBehaviour` B1+B2, `TestSwapContract` over fake v239, fake v247
and `live/exec`) green in 38.9 s. Step 2 stood four real **v247**
llama-swap processes on cells 10240-10243 with upstreams 6580-6619:

- all four cells `SERVING`; alpha/bravo/charlie announcing; the front
  rendered peers-only with `models=4`;
- PROOF 2 — a real embedding **and** a real chat completion through alpha
  on the candidate (`content: "proof"`, 36 prompt / 2 predicted), and the
  candidate's `/api/metrics/activity` returning both rows with tokens;
- PROOF 4 — charlie's announcer SIGKILLed, `stale=true` at t+45 s, clean
  on restart;
- `lab.sh down` clean; `:9000` and `:9001` untouched throughout.

Exit 0, `next: ritual.sh gate v247`. **L4 PASS**, recorded in C16's gate
table and Execution section with the transcript summary.

It needed nothing else. That is worth stating plainly, because the row had
been sitting under a heading that also contains L5 (a time budget) and L6
(needs the fleet), and those are different kinds of not-done. L4 was never
blocked on hardware or on wall-clock — it was blocked on a defect in this
repo's own harness, for three days, while a `PASS` was available for the
cost of one knob. **"Not attempted" and "not possible" must never share a
heading**, and the sharpest version of that rule is this: when a gate is
recorded as blocked, the next question is *what would unblock it*, and if
the answer is "one of our own files" then the gate is not blocked, it is
queued.

## 6. Production safety

The lab runs beside a live llama-swap on `:9000` with a resident model and
real traffic and a live vibe daemon on `:9001`. Nothing in this phase read
or wrote `~/.config/vibe`, `~/.config/llama-swap` or
`~/.local/state/vibe`, and neither production process was signalled,
restarted, or sent a completion. Every run in §3 and §5 used a
non-default base (10200, or 20000+ in the unit tests), checked free with
`ss -ltn` first; `:9000` and `:9001` were confirmed still listening after
each teardown. The guard added in §2 is the mechanical form of that
discipline — the base that would have made this dangerous now does not
start.

## For the reconciliation pass

Nothing in this phase touches the three shared docs, so the following are
recorded here for whoever runs the reconciliation:

### AGENTS.md

- The parallel-agent section should state the fleetlab collision rule in
  one line: **a lab instance is `FLEETLAB_DIR` + `FLEETLAB_PORT_BASE`,
  both, always** — bases are multiples of 200, take one nobody else has,
  say which one you took, and never run `down` on the default while
  another agent is working. The dir alone is not isolation.
- If AGENTS.md names the inner loop's shell half, `internal/fleetlab` is a
  second Go package that runs real subprocesses and binds real ports;
  it skips itself when `bash`, `ps`, `pgrep` or `awk` is missing.

### docs/design/fleet-control-plan/README.md

- A `C23` row: *A port base for `scripts/fleetlab`: one lab's teardown
  cannot reach another's* — ~120 lines of shell plus a 400-line test
  package, backlog item 15, status "PR open; U1-U8 and L1-L3 PASS".
- C16's row needs its status corrected: **L4 PASS** as of 2026-08-08, so
  it reads "L1-L4 and L7 PASS, L5-L6 unrun" rather than "L4-L6 unrun".
- The "what still needs metal" paragraph, if it exists by then, must NOT
  list C16's L4 — it never needed metal. It is the worked example for the
  distinction the paragraph is about.
- A paragraph after C21's, along these lines: C23 (2026-08-08) is backlog
  item 15, and it is the second phase whose subject is the repo's own
  harness rather than the fleet. Its carried rule is that **a teardown is
  a claim about scope, and an unanchored kill pattern is the cheapest way
  to make a false one**. The corollary that generalises past this phase:
  when a process cannot be identified by a path you control, identify it
  by a *derived* range and make the derivation's disjointness a checked
  precondition — and pair every isolation assertion with a negative
  control that removes the isolation, or a sweep that kills nothing reads
  exactly like a sweep that is correctly scoped.

### docs/design/fleet-control.md

- Nothing. This phase changes no fleet semantics, no schema and no verb.
