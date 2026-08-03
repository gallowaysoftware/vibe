# C5 — Land C4: the review pass C4 never got

Status: EXECUTED (2026-08-03), on `feat/c4-fleet-comfort` alongside C4
itself. Every §1–§8 item below is implemented; the live gates (gate 4's
warm-policy run, gate 6's malformed-def run) are NOT run — they need
real cells, and are listed as such in the acceptance-gate section. See
the addendum at the end for the adversarial pass (gate 9). This phase
exists because C4 shipped without the adversarial self-review step its
three siblings each got.

C1, C2 and C3 each landed as three commits — feature, `review:
adversarial-review fixes`, `review: second-pass minors` — whose addenda
document 10, 10 and 11+ findings fixed pre-merge, including blockers.
C4 has one commit. Every defect below is the class those passes caught
for the other phases. **C5 is that missing pass, run as its own phase.**

Scope: everything required to merge PR #22 honestly. That includes two
defects in C3-authored code (B1, M4) — they are crash- and
data-integrity-class, and the fleet page C4 adds is the surface that
displays the render loop they break. The remaining C1–C3 substrate
findings are [C6](c6-substrate-repair.md), against `main`.

Every anchor below was re-verified against `3854d84` + the uncommitted
working tree on 2026-08-02 by a 9-agent verification pass: 64 confirmed
defects across C0–C4 (2 blockers, 19 majors, 43 minors and nits), of
which this doc carries the 35 that belong to landing C4 — the other 29
are C6. Two further reported findings were checked and **dismissed**
— see §7. Where the original
audit's anchor was wrong, the corrected one is used and the error
noted. Ground rule 8 still applies: re-verify before relying.

## Starting state

- HEAD `3854d84` on `feat/c4-fleet-comfort`; **PR #22 open, unmerged**.
  `main` is at `322712f`.
- **PR #22's green `test (stable)` check is a coin flip** — the race
  below reproduces 6/20 isolated and 6/12 full-package. Do not use it
  as the merge signal.
- The working tree carries an **unfinished but correct-in-spirit** cron
  fix (`warmsched.go`, `c4_test.go`). It is a real fix caught by the
  previous agent unprompted, minutes before it ran out of budget. Keep
  the OR semantics; replace the star detection (CRON-2). It is not
  `gofmt`-clean, which is the only reason two lint gates are red.

## 1. Gate integrity — the mandated inner loop must be green

**B2 — data race in committed test code** (major; the audit called it a
blocker, but it is test-only)
`internal/vibe/fleetapi/c4_test.go:120-128` (identical at HEAD and in
the worktree). Writer: `warmtarget.go:244` `setWarmState`, reached from
`warmtarget.go:117`.

`TestWarmTarget_SkipsAbsentAndDrainedCells` copies the
`*warmTargetState` **pointer** under `s.mu`, unlocks, then reads
`st.State` while the 20 ms warm loop writes it. Production is fine —
`warmReport` (`warmtarget.go:296-309`) copies by value under the lock,
explicitly so a reader never holds loop-owned pointers. But this
falsifies ground rule 4 on a pushed branch and falsifies this plan's
own `c4-comfort.md:41` "Unit tests: PASS".

Fix — copy the **value** inside the critical section:

```go
var got string
s.mu.Lock()
for _, ws := range s.warmStates { got = ws.State }
s.mu.Unlock()
if got != "skipped" { t.Errorf("state = %s, want skipped", got) }
```

Cleaner still: assert through the production accessor `s.warmReport()`,
which already copies under the lock and is in-package.

Wrong ways: (a) keeping the captured pointer and merely wrapping the
`st.State` read in `s.mu.Lock()` — correct today, but it leaves a
lock-escaping pointer in the test and re-arms the identical bug on the
next edit; (b) stopping the loop or sleeping longer — hides the race
without removing it.

Verify with `go test -race -count=20 -run TestWarmTarget
./internal/vibe/fleetapi/`. **One green run proves nothing.**

**B2-sibling — same shape, dormant** (nit)
`c4_test.go:315` in the worktree (`:296` at HEAD), with unlocked reads
at `:320, :333, :345, :348` (worktree) / `:301, :314, :326, :329`
(HEAD). `TestScheduleGuardSkipsBusyAndLeased` has no concurrent writer
today, so it does not fire. Fix it with the same idiom. Note `st` is
also passed *into* `evalScheduleEntry` and must stay a pointer there —
only the assertion reads need the locked copy.

**gofmt** — `gofmt -w internal/vibe/fleetapi/c4_test.go`. Worktree-only;
the committed tree is clean. This alone clears `gofmt -l .` and
`golangci-lint run`.

## 2. Crash and config integrity (C3 code, but blocking)

**B1 — unguarded `ModelCmd` call panics fleetd** (blocker)
`render_loop.go:309-331` (call at `:324`) → `router/render.go:530-543`
(deref at `:537`). The pointer field is
`internal/vibe/profile/profile.go:123` — *not* `:109` as both the audit
and the evaluation report stated; `:109` is the `Backend` struct
declaration.

`applyFingerprints` calls `router.ModelCmd` for any def whose name
matches an announced model id. `ModelCmd` dereferences
`def.Backend.LlamaServer.Binary` whenever `MLXServer` is nil, so a
`comfyui` / `http_server` / `tabby_api` / `cloud_peer` def panics. No
`recover()` exists anywhere in the fleetd path; under systemd this is a
restart loop, since the same announce arrives again.

**Reachability is narrower than the audit claimed.** `applyFingerprints`
skips models with an empty hash (`render_loop.go:306-308`), and vibe's
own announcer only sets `FlagsSHA256` for llama/mlx kinds
(`fleetannounce.go:461`). So the repo's own announcer **cannot** trigger
it; it needs an announce supplying a non-empty hash for a model id
matching a cell-assigned non-llama/mlx def — a forged, third-party, or
future announcer. Since C3 documents that the fleet token is every
cell's voice, any token holder can crash fleetd. Robustness/DoS, not an
everyday crash loop — but the asymmetry is the lesson: the **sending**
side has this guard with a comment naming the hazard; the **receiving**
side, which by construction must not trust the sender, omits it.

Fix, all three parts:

1. In `applyFingerprints`, right after `def := byName[m.ID]; if def ==
   nil { continue }`, mirror the announcer's guard:
   `if def.Backend.LlamaServer == nil && def.Backend.MLXServer == nil {
   continue }`.
2. Harden `router.ModelCmd` to return an error rather than deref:
   `if ls := def.Backend.LlamaServer; ls == nil { return "",
   fmt.Errorf("backend %s: ModelCmd requires llama_server or
   mlx_server", def.Name) }`.
3. Change the three `return nil, fmt.Errorf("fingerprint %s: …")` sites
   (`:326, :330, :355`) to `slog.Warn(...); continue`. Without this,
   part 2 converts the crash into a **permanent render freeze** — one
   bad def stops prune, re-add and fingerprint enforcement fleet-wide,
   forever, because `p.Announcing` is never cleared.

Wrong ways: hardening `ModelCmd` alone without part 3; wrapping the loop
in `recover()` (hides the skew the fingerprint exists to report and
leaves `rl.pruned` half-updated); treating a verification error as a
strict-mode exclusion — fail-open is the documented bias
(`render_loop.go:288-294`), so a render bug must not yank a working
model.

Test: a def named X assigned to cell `gpu` whose backend is comfyui /
http_server / tabby_api / cloud_peer (each), while `gpu` announces
`{id: X, flags_sha256: "deadbeef"}` — the pass must complete, write the
config, and not panic. Second case: an mlx def with `huggingface` set
and `model_dir` empty announced with a non-empty sha — the pass must
still render and write, and a later legitimate transition must still
re-render (proves no freeze).

**M4 — empty backends dir overwrites the live front config** (major)
`render_loop.go:210-252` (`LoadDefs` at `:211-214`, unguarded write at
`:243-248`); `router/render.go:129-157`.

`LoadDefs` returns `(nil, nil)` for an empty dir, `Render` then succeeds
with a header-only config, and `renderPass` writes it over a good one.
The cold-start hold guards only the *presence* axis. The shipped deploy
makes this reachable: `deploy/fleetd/README.md:26-32` tells operators to
`mkdir -p` the backends dir.

Fix, input-side, right after `LoadDefs`:

```go
if len(defs) == 0 {
    if fi, err := os.Stat(rl.cfg.FrontConfigPath); err == nil && fi.Size() > 0 {
        return fmt.Errorf("refusing to render an empty front config: no backend defs under %s (fix the defs mount)", rl.cfg.BackendsDir)
    }
}
```

Gate on the **input** (zero defs loaded) and on the existing file being
non-empty — **never** on the output (zero peers/models). A peerless
render is legitimate when every def is unassigned or every roaming cell
is pruned; gating there would deadlock the legitimate empty-fleet case.

Test: `LoadDefs` returning `(nil, nil)` with a pre-existing non-empty
`FrontConfigPath` ⇒ no write, `RenderCount` stays 0. Companion case:
defs present but all excluded by class policy or fingerprints ⇒ the
write **must** still happen, proving the guard is input-side.

## 3. Warm-target policy — the design's headline rule, wrong four ways

The design's rule (§1 of [c4-comfort.md](c4-comfort.md)) is: restore
keys on the swapped-in model going **request-idle**, never on a clock,
and the pin/keep-warm alternative that evicts the operator's model
mid-session is rejected — *"do not build that, even as an option."*
Four separate defects arrive at that rejected behaviour anyway.

**M1 — drained cells are warmed, not skipped** (major)
`warmtarget.go:114-126` (skip branch `:115-119`).

`evalWarmTarget` consults only `p.Stale || p.Withdrawn`. `grep -n
'intents\|IntentEcho' warmtarget.go` returns zero hits — neither intent
store is ever read. A drained cell keeps announcing an empty model list
**by design** (`fleetannounce.go:250-256`: "the unit is stopped reads as
an empty model list"), so the nothing-resident branch fires a warm about
every 45 s for the whole drain. Where the drain leaves llama-swap up, the
warm **succeeds** — reloading the model onto the GPU the operator just
reclaimed.

This is the plan's most expensive doc error: the drained skip is
asserted in four places and implemented in none — `warmtarget.go:22-24`,
`AGENTS.md:247`, `c4-comfort.md:41`, and the test *name*
`TestWarmTarget_SkipsAbsentAndDrainedCells` whose body only sets
`Stale`. Four artifacts asserting behaviour never written will actively
stop the next agent from writing it. **Fix the code; do not soften the
docs.**

Fix: factor `decorate()`'s effective-intent rule
(`presence.go:142-150` — the registry request wins unless the echo is
drained and newer/unrequested) into
`func (s *Server) effectiveIntent(cell string) (Intent, bool)`, and call
it at the **top** of `evalWarmTarget`, before both the presence and
probe branches: if the state is `"drained"`, `s.setWarmState(st,
"skipped", "cell drained")` and return.

Wrong ways: (a) checking only `p.IntentEcho` — misses an operator drain
requested through fleetd that the cell hasn't echoed yet; (b) checking
only `s.intents` — misses a drain performed at the box, whose only
record is the echo; (c) gating on `snap.Display` — `decorate` turns
drained-echo + answering-probe into `INCONSISTENT`
(`display.go:26-29`), and that cell must still be skipped; (d) inferring
drain from `len(residents)==0` or from unreachability — ground rule 2
forbids acting on inferred intent, and an empty cell is exactly the
nothing-resident restore case.

Test: announce `{intent: drained, models: nil}` for a cell with a warm
target ⇒ `warmFn` never called and `st.State == "skipped"`. Plus the
conflict-rule case: registry intent drained but a **newer serving echo**
must **not** skip.

**CC-1 — a long request reads as idle and is evicted mid-generation**
(major; found by this phase's completeness sweep, not in the original
audit)
`warmtarget.go:179-192` (`applyWarmEval` default branch) and
`watcher.go:146-153` (`trackInFlight`).

Two gaps compose into the rejected design. First, `applyWarmEval` never
consults the cell's in-flight count at all — only the timestamp map.
Second, `trackInFlight` stamps `modelActivity` **only for models
currently in the frame's request list**, and the file's own comment
(`watcher.go:128-131`) says frames are add/remove edges — so a request
gets a stamp when it *starts* and never when it *finishes*. One
generation longer than `restore_after_idle` therefore reads as idle and
is evicted while still streaming.

Fix, both halves:

1. In `applyWarmEval`, before `swapIdleFor` in the default branch:
   `if n, reported := s.InFlight(t.Cell); reported && n > 0 {
   s.setWarmState(st, "waiting", fmt.Sprintf("cell busy (%d
   in-flight)", n)); return true }`.
2. In `trackInFlight`, also stamp the **completion** edge: keep
   `lastInFlightModels map[string][]string` on `Server`, and for every
   model present in the previous frame's list but absent from this one,
   write `s.modelActivity[key] = time.Now()` before replacing the stored
   list — so "last activity" means started **or** finished.

Wrong way: having the warm loop bump the timestamp every tick while
`inFlight > 0` — that erases the completion edge and turns the policy
back into a keep-warm timer.

Note the busy gate only helps where fleetd can watch the cell;
`reported == false` (announce-only cells) still has no in-flight signal,
which is M2.

Test: feed one inflight frame with `requests=[{model:"challenger"}]`,
advance past `RestoreAfterIdle` with no further frame ⇒ no restore. Then
feed the remove frame (`requests=[]`) ⇒ the idle window restarts from
the **completion**, not the start.

**M2 — "no activity data" is treated as "fully idle"** (major)
`warmtarget.go:206-220` (unknown branch `:207-213`); caller `:186-190`.

The `!ok` branch sets `idle = idleFloor` (1 h) with a comment claiming
"idle since fleetd start" — but there is no start timestamp on `Server`
(`startedAt` at `fleetapi.go:164` is per-model starting→ready tracking).
`modelActivity` has exactly one writer, `trackInFlight`, fed only by the
cell's own `/api/events` SSE. So an announce-only cell — the
no-inbound-port case C3 exists for — or a cell served through vibe's own
proxy never populates it at all, and any `restore_after_idle <= 1h` (the
doc's own example is 30 m) fires on the first 15 s tick. Same after any
fleetd restart with a quiet resident swap.

Fix, minimum: add `started time.Time` to `Server` (set in `New`,
`fleetapi.go:242`) and use `idle = now.Sub(s.started)` in the `!ok`
branch, so a fresh fleetd cannot claim an hour of silence it never
observed.

Better, still local: require *some* activity telemetry before
restoring — `s.inFlightSeen[cell]` already exists
(`watcher.go:147-148`); when false, `setWarmState(st, "skipped", "no
activity evidence")`. **Verify first** that llama-swap emits an inflight
frame on connect and not only on add/remove — if only on add/remove, a
genuinely quiet cell never sets it and the target would never restore.

Durable: extend `AnnounceModel` (`announce.go:38-46` — additive and
v1-safe, receivers tolerate unknown fields, and the reserved `Probe`
field sits right there) with `last_request_at` and `in_flight`, populate
them in `fleetannounce` from llama-swap, and have `swapIdleFor` prefer
the announced value, refusing the target when neither source has data.
**Record whichever answer is chosen in `c4-comfort.md` §1.**

Wrong way: raising `idleFloor` — trades M2 for a worse M3 and still
invents evidence.

Test: a resident model with no `modelActivity` entry and
`RestoreAfterIdle = 1h` must **not** warm on the first eval; the state
detail must name the missing evidence.

**M3 — `restore_after_idle` above 1 h can never fire** (major)
`warmtarget.go:199-222` (seed `:205`, min-only update `:218-219`);
status string `:191`; config path `daemon/warm.go:30-41`.

`swapIdleFor` seeds `oldest := idleFloor` and only ever lowers it, so
the returned idle caps at 1 h while the caller tests `idle >=
RestoreAfterIdle`. Any window above 1 h is silently inert and the status
reports a fabricated `idle 1h0m0s of 4h0m0s` forever. `daemon/warm.go`
validates only parse-and-positive, so `4h` is accepted and logged as
valid.

Fix: `oldest := time.Duration(-1)`; in the loop `if oldest < 0 || idle <
oldest { oldest = idle }`; after the loop `if oldest < 0 { oldest = 0 }`
(defensive — `residents` is non-empty at the only call site). `idleFloor`
must remain **only** the unknown-activity substitute.

Wrong way: raising `idleFloor` to 24 h — moves the cap but makes every
unknown-activity model claim 24 h of idleness and warm instantly (M2,
amplified).

Test: `swapIdleFor` with activity 3 h in the past returns ~3 h, not 1 h;
`applyWarmEval` with `RestoreAfterIdle = 4h` does not warm at 3 h idle
but does once activity is 4 h+ stale.

**NIT-A — the comment inverts its own math** `warmtarget.go:196-198`,
`:205`, `:218-219`. `swapIdleFor` returns the **shortest** idle window
(the most recently used resident), not the oldest. Rename `oldest` →
`minIdle` and reword: *"reports the shortest idle window across the
resident models — the most recently used one — so restore fires only
when even the busiest resident has been quiet for the whole window."*
Comment/rename only; do not change the comparison.

**CC-6 — the idle and grace windows are wall-clock** (nit)
`warmtarget.go:159-172`, `:199-221`, `watcher.go:151`. `time.Now().UTC()`
strips the monotonic reading, so `time.Since`/`Sub` fall back to wall
clock: an NTP step forward skips the empty-grace the live gate had to
add after two early restores, and a step backward wedges it. Neither
field is ever serialised, so `.UTC()` buys nothing here. Drop `.UTC()`
on those three duration clocks only — leave `LastRestore`, `LastFire`,
`lastSeen`, `ReceivedAt` and `NextFire` alone; those are calendar values.

**NIT-G — `restore_after_idle` has no lower bound and the model id is
never validated** `daemon/warm.go:30-41`; tick floor
`warmtarget.go:73-76`. Clamp below 1 m with a warning (clamp, not skip —
a too-eager warm is a working policy, a skipped target is a silently
absent one), and validate `wt.Model` against `router.LoadDefs` the way
the schedule branch already resolves `cellOfModel`. Do **not** push the
floor into `startWarmLoopWithConfig` — its tick is a test seam and
`c4_test.go:106` deliberately uses millisecond values.

## 4. Warm schedules — cron semantics and the guard

**CRON-1 — committed cron ANDs dom/dow where Vixie ORs** (blocker)
`git show HEAD:internal/vibe/fleetapi/warmsched.go` `:135-141`.

`"0 9 1 * 1"` next-fires **2027-02-01** instead of 2026-08-03. Verified
by running the committed function verbatim in a scratch module. A second
symptom: `"0 0 29 2 1"` returns `ok=false` — reported as "no fire time
within a year" and never runs at all. `nextFire` calls `matches`, so
every consumer is wrong the same way, including the `next_fire` shown on
the fleet page and in `fleet_status`.

The worktree WIP already fixes the OR half. **Keep it. Do not ship its
star detection** — see CRON-2.

Final form, byte-for-byte Vixie (`cron.c` `find_jobs`):

```go
if c.domStar || c.dowStar {
    return c.dom[t.Day()] && c.dow[int(t.Weekday())]
}
return c.dom[t.Day()] || c.dow[int(t.Weekday())]
```

**CRON-2 — the WIP detects the star by set cardinality** (major)
Worktree `warmsched.go:143-154`.

The WIP uses `domStar := len(c.dom) == 31` / `dowStar := len(c.dow) ==
7`. Vixie's rule is **textual**: the field is a star iff its first
character is `*`. Five expressions diverge in both directions —
`1-31` and `0-6` are read as stars when Vixie treats them as restricted;
`*/2` is read as restricted when Vixie treats it as a star (`entry.c`
sets `DOM_STAR` on any field beginning with `*`).

Fix: add `domStar, dowStar bool` to `cronSpec` (`warmsched.go:25-27`)
and set them in `parseCron` (`:33`) inside the existing `switch i` —
`case 2: c.dom = cf; c.domStar = strings.HasPrefix(f, "*")` and
`case 4: c.dow = cf; c.dowStar = strings.HasPrefix(f, "*")` — using the
**raw field string**, before comma splitting. It cannot live in
`parseCronField`/`parseCronPart`: those receive one comma-part and don't
know the field index. `cronSpec` is copied by value into
`scheduleEntryLoop`; bool fields travel fine.

Two wrong ways: (a) OR-ing `HasPrefix` over the comma-parts — Vixie peeks
only the first character of the whole field, so `1,*/2` is **not** a
star; (b) **keeping the WIP's 4-way switch after making the flags
textual** — with `domStar` true for `*/2`, `case domStar: return
c.dow[...]` drops the dom restriction entirely. Replace the switch with
the two-line form above, which also collapses star&&star correctly.

**CRON-3 — the test table has no both-restricted case** (major)
Committed `c4_test.go:230-240`; worktree `:235-241` plus
`TestCronNextFireDomDowOr` at `:258-273`.

The one interesting rule in cron semantics was untested, which is how
the AND bug passed a gate reported PASS. Add:

| case | spec | from | want |
|---|---|---|---|
| dow-wins (WIP has it) | `0 9 1 * 1` | 2026-08-02 10:00 | 2026-08-03 09:00 |
| **dom-wins (missing)** | `0 9 1 * 1` | 2026-08-31 10:00 | 2026-09-01 09:00 |
| explicit ≠ star | `0 9 1-31 * 1` | 2026-08-04 00:00 | 2026-08-04 09:00 |
| explicit ≠ star | `0 9 1 * 0-6` | 2026-08-02 10:00 | 2026-08-03 09:00 |
| stepped **is** star | `0 9 */2 * 1` | 2026-08-03 10:00 | 2026-08-17 09:00 |
| stepped **is** star | `0 9 1 * */2` | 2026-08-02 10:00 | 2026-09-01 09:00 |

Rows 3–6 fail against the current WIP; row 2 fails against HEAD. Then
fold `TestCronNextFireDomDowOr` into the table or fix its comment — as
written it duplicates the row at `:240`.

**CC-3 — the schedule guard is bypassed whenever the cell can't be
resolved** (major; new)
`warmsched.go:246-256` worktree / `:232-242` HEAD; `daemon/warm.go:46-57`.

The entire guard — in-flight **and** lease checks — sits inside
`if cell != ""`, and the production `cellOfModel` collapses a hard
`router.LoadDefs` error into that same empty string. `LoadDefs` errors on
an unreadable backends dir *or any single malformed YAML in it*. So one
bad def file silently converts every scheduled warm into an unguarded
warm — starting exactly the eviction fight §2 exists to prevent — and
`fleet_status` shows a bare `warmed`.

Fix: change the `cellOfModel` seam to distinguish resolve-**failure**
from resolved-but-no-cell — `func(string) (string, error)` through
`StartScheduleLoop` / `startScheduleLoopWithConfig` / `scheduleEntryLoop`
/ `evalScheduleEntry`, returning the `LoadDefs` error in
`daemon/warm.go`. On resolve failure: skip, and put the reason in
`note`. On resolved-but-no-cell: keep firing, but write `warmed
(unguarded: no def/cell)` into `note`. Same treatment for `reported ==
false` at `:249` — unknown in-flight is not zero in-flight.

Subtlety: a front-only model alias with no backend def must stay
warmable, so plain no-cell must **not** become a hard skip. Only resolve
failure and unknown in-flight do.

**CC-4 — zombie goroutine + clobbered terminal note** (minor; new)
`warmsched.go:273-287` worktree / `:259-273` HEAD; `:190-204`.

`parseCron` accepts `0 0 30 2 *` (dom 1-31 and month 2 are each
individually valid), so `nextFire` burns the full ~2.1 M-iteration scan
at startup, returns false — and the goroutine is started anyway, because
the gate tests only the parse result. Separately, the `!ok` branch sets
`st.LastNote = "no further fire time…"` and two statements later
`st.LastNote = note` overwrites it.

Fix: gate the goroutine on parse **and** initial `nextFire`; move
`st.LastNote = note` before the re-park block (or make `!ok` append).

**CRON-4 — the 4-year bound misses century non-leap gaps** (nit)
`warmsched.go:119`, `:126`; messages `:195`/`:280` worktree,
`:181`/`:266` HEAD; test message `c4_test.go:250`. `0 0 29 2 *` from
2096-03-01 returns `ok=false`. Raise to `8 * 366 * 24 * 60` and change
both "within a year" strings to match the actual bound. Do not lower it.

**CRON-5 / MIN-M — `dow=7` is rejected; Vixie accepts it as Sunday**
(minor) `c4_test.go:212` (the audit said `:213` — off by one); cause
`warmsched.go:38` `ranges[4] = {0, 6}`. Widen to `{0, 7}` and normalise
**at parse time** in the `case 4:` arm: `if cf[7] { cf[0] = true;
delete(cf, 7) }`. Normalising in `matches` instead would silently never
match, since `time.Weekday()` never returns 7. Flip `c4_test.go:212` to
`wantErr:false` and add `* * * * 8` as the error case so the upper bound
stays pinned. Note in the doc comment at `:32` that day/month **names**
(`sun`, `jan`) remain unsupported, rather than letting "the five standard
fields" imply full Vixie compatibility.

**CRON-6 / NIT-C — the DST claims** (nit)
`warmsched.go:117-118`, `c4_test.go:276-279` (HEAD `:259`). The doc's
named gates *are* asserted; the **fall-back** claim in the comments is
not, and is false — a daily fire inside the repeated hour fires twice.
Either implement first-occurrence-wins in `nextFire` (the correction
belongs there, not in `evalScheduleEntry` — `nextFire` is also what the
fleet page displays), or delete the claim and pin the double-fire
behaviour in a test. Given the warm is guarded by in-flight/lease checks,
documenting the double-fire is defensible; the comment asserting the
opposite is not.

## 5. Lifecycle

**CC-2 — `Close()` can block for ten minutes** (major; new)
`warmtarget.go:225-232` (`restore`) and `warmsched.go:260-262` worktree /
`:246-248` HEAD (`evalScheduleEntry`); `fleetapi.go:302-305` (`Close`).

Both warm paths call `warmFn` synchronously under
`context.WithTimeout(context.Background(), 10*time.Minute)` — a context
with no link to `s.done`. Both run on goroutines registered with
`s.wg.Add(1)` whose only exit is `<-s.done` in a select that isn't
reached while `warmFn` runs. `warmViaFront` uses `http.DefaultClient`
(zero timeout). So `Close()` → `s.wg.Wait()` can hang for the full ten
minutes on a warm against an unreachable front.

Fix: add `func (s *Server) warmCtx(d time.Duration) (context.Context,
context.CancelFunc)` that builds the timeout context and links
cancellation to `s.done`, and use it in both places.

Wrong way: copying fleetmcp's fire-and-forget goroutine — the warm loops
need the synchronous return to record `warm failed:` in
`st.Detail`/`LastNote` and to avoid stacking overlapping restores while
the 15 s ticker keeps firing.

Test: a `warmFn` that blocks until its ctx is done, fired once ⇒
`s.Close()` returns in ~1 s, not ten minutes.

## 6. Tests that must exist

**M9 — the idle-window input path is untested** (major)
`watcher.go:148-153`; fixture `daemon/cell_drain_test.go:277-283`;
direct poke `c4_test.go:75`. Replacing the loop with a no-op leaves the
whole repo green — mechanically proven. Combined with M2/CC-1, a JSON
shape drift in llama-swap's inflight frame silently converts the warm
policy into "always evict".

Fix: drive the real parser —

```go
s.trackInFlight("heavy", json.RawMessage(strconv.Quote(
    `{"requests":[{"model":"challenger"},{"model":"challenger"}]}`)))
```

then assert `s.InFlight("heavy") == 2` **and** that
`modelLastActivity("heavy","challenger")` returns `ok==true` with a fresh
timestamp. Rewrite `TestWarmTarget_IdleWindowStateMachine`'s window-reset
step (`c4_test.go:74-76`) to call `trackInFlight` instead of poking
`s.modelActivity`, so the window is driven end to end.

**Subtlety that will silently defeat this test:** the frame's `data` is a
JSON *string containing JSON* (double-encoded). A test handing
`trackInFlight` a bare object hits the first `json.Unmarshal` early
return and passes vacuously. Assert on the recorded timestamp — never
merely "did not panic". Also fix the daemon fixture at
`cell_drain_test.go:277-283` to emit distinct `{"model":"qwen"}` entries.

Plus regression tests for every item in §2–§5 above, as specified there.

## 7. The fleet page

**PAGE-1 — `esc()` is a text escaper used in an attribute** (minor)
`fleet.html:101` (esc), `:146` (the `href` built from the cell URL).
`esc()` does not escape `"`. The URL comes from operator config, which
bounds severity — but the fix is cheap:

```js
function attr(s) { return esc(s).replace(/"/g, "&quot;").replace(/'/g, "&#39;"); }
```

and `<a href="${attr(c.url)}/ui" …>`. Do **not** just add the two
replaces inside `esc()` — its output also feeds `textContent` (PAGE-2),
where entities would become visible artifacts. Quote-escaping does not
stop a `javascript:` URL in hosts.yaml from being a clickable script
link; gate the href on `/^https?:\/\//.test(c.url)` if you want that
closed too.

Test (Go, over the embedded file): assert no `esc(` appears inside a
double-quoted HTML attribute value, so future attribute interpolation
must go through `attr`.

**PAGE-2 — double-escaped into `textContent`** (nit)
`fleet.html:171-174`. Drop the `esc()` calls on `:172`; the `:174`
`textContent` assignment is already the safe sink.

**CC-5 — re-entering the token stacks readers and pollers** (minor; new)
`fleet.html:76-81` (`saveToken`), `:207-240` (`streamEvents`), `:244-250`
(`boot`). `boot()` retains neither the SSE chain nor the `setInterval`
handle, so a mid-session token rotation (401 → gate → re-save) leaves two
live readers and two pollers; each rotation adds a pair. And
`streamEvents` reschedules unconditionally every 3 s, so a stale token
re-attacks `/api/fleet/events` forever.

Fix: hold `let pollTimer = 0, streamAbort = null;` at module level; in
`boot()` clear both before assigning new ones, and thread
`{signal: streamAbort.signal}` into the events fetch. Don't reschedule on
a 401 (the gate is already showing and `saveToken` re-boots), and back
the retry off geometrically to a ~30 s cap, resetting on success.

**PAGE-3 and PAGE-5 are not defects — leave them alone.** The
`/ui/fleet` bearer exemption (`daemon/auth.go:139-142`) is exact-match
and GET-only, evaluated before mux path-cleaning, and survived every
widening attempt tried against it: percent-encoded slashes, traversal,
trailing slash, double slash. The daemon mux has no catch-all, so with
the fleet role off the path 404s rather than falling through. The page
likewise adds exactly one route, with every button going through `POST
/mcp`. **Do not "harden" the middleware to a prefix match or
`path.Clean`** — either change would *widen* the surface that is
currently closed. The only gap is test coverage of the boundary: add
unauthenticated `POST /ui/fleet` ⇒ 401 and `GET /ui/fleet/` ⇒ 401 to
`daemon/fleet_registry_test.go` (note: `daemon/`, not `fleetapi/` — the
audit cited the wrong package).

## 8. Doc truth

Ground rule 6 says future agents read the docs. Every item here is a doc
that will mislead one.

- **DOC-10 / `c4-comfort.md:3-5, :41`** — "EXECUTED. All four gates
  passed" and "Unit tests: PASS" are false at HEAD. Rewrite **after**
  the fixes land, and do not simply delete the claims: ground rule 7
  makes gates the definition of done, so the doc must record what the
  gate failed to catch. Status becomes "EXECUTED WITH FOLLOW-UPS …
  post-gate review found …; fixed in C5"; gate 4 must state that the
  suite passed at `-count=1` while `-race -count=10` reproduced a race,
  and that the "absent/drained skips" case only ever exercised *stale*.
- **DOC-9 / `AGENTS.md:247`** — asserts the drained skip. **Do not
  soften it**; it describes a real safety requirement. M1 makes it true.
  (`AGENTS.md:154`'s corrected drain semantics were re-verified as
  accurate — leave them.)
- **PAGE-4 / `c4-comfort.md:126`** — says EventSource; the
  implementation is a fetch-streamed reader because EventSource cannot
  carry the bearer header (`fleet.html:201` explains why). Correct the
  doc, not the code. While there, `:130-131` should say the buttons POST
  `/mcp` tools/call.
- **DOC-1 / `fleet-control.md:3`** — still `Status: DESIGN` after five
  executed phases. Make it "IMPLEMENTED THROUGH C4", distinguishing
  merged (C0–C3) from in-flight (C4); do not write a flat "EXECUTED".
- **DOC-2 / `README.md:8-14`** — the phase table has no status column.
  Add one, and add the table to ground rule 6's list of files a phase PR
  updates, or it rots again. *(Done as part of this plan update.)*
- **DOC-5 / `deploy/fleetd/README.md`** (major) — frozen at C1: lists 3
  of 7 MCP tools and documents none of C3/C4. Four edits: the real tool
  set (`fleet_status`, `warm_model`, `unload_model`, `drain_cell`,
  `resume_cell`, `wake_cell`, `render_front`, noting drain/resume need
  `daemon_url` + `token_file` or they fall back to desired-intent);
  `front_config`, `warm_targets`, `warm_schedule` in the config sample;
  a Timezone note (schedules evaluate in `TZ`, the image ships tzdata
  for exactly this, a wrong zone shows as a wrong `next_fire` — this is
  what the C4 gate hit); and `GET /ui/fleet` under "Consuming it" with
  its one security-relevant fact.
- **DOC-8 / `fleet-control.md:374`** — calls the front image "pinned"
  while the compose floats `:cpu` and the digest pin is opt-in. State
  the verified build (v239) and that the guarantee is conditional on
  pinning.
- **DOC-7 — boundary-rule scrub** (ground rule 3): `c0-quick-wins.md:28,
  37`; `c2-actuate.md:12, 13, 14`; `c4-comfort.md:10, 30`. Substitute
  `gpu-cell` / `laptop` / `<cloud-peer>` / `${env.<CLOUD>_API_KEY>}` /
  "the heavy cell's GPU". **Keep every gate assertion intact** — e.g.
  c2's "9 models incl. the alias union" is real evidence; rename the
  cell only. Do not delete the sentences.
- **MIN-T / `fleetmcp/actuate.go:165, :156-161`, `fleetmcp.go:314-315,
  :319`** — "the apply path arrives with C3" is stale; C3 shipped as
  `322712f`. The behaviour is right (dry-run only) and the reason is
  wrong: fleetd's presence-driven render loop owns the write path.
  Rewrite the reason, keep the behaviour — **do not add an apply mode**;
  two writers to one `-watch-config` file is what the atomic-write
  contract forbids.

## Acceptance gates

1. **Race gate.** `go test -race -count=20 -run TestWarmTarget
   ./internal/vibe/fleetapi/` clean, and `go test -race -count=5 ./...`
   clean. A single green run is not evidence.
2. **Panic gate.** The B1 table test (comfyui / http_server / tabby_api /
   cloud_peer defs against an announce carrying a hash) completes,
   writes, and does not panic — and a following legitimate transition
   still re-renders, proving no permanent freeze.
3. **Config-integrity gate.** Point fleetd at an empty backends dir with
   a good front config present ⇒ no write, loud error. Then the
   all-excluded case ⇒ the write still happens.
4. **Warm-policy gate (live, small models per the fleet's gate
   convention).** On a real cell: (a) drain the cell ⇒ zero warms for
   the whole drain, status `skipped`; (b) drive a generation longer than
   `restore_after_idle` ⇒ **not** evicted mid-stream, and the window
   restarts at completion; (c) `restore_after_idle: 2h` ⇒ status reports
   real idle, not a capped `1h0m0s`; (d) restart fleetd with a quiet
   resident swap ⇒ no immediate restore.
5. **Cron gate.** The six-row both-restricted table passes, plus `dow=7`,
   plus the Feb-29 case. Cross-check at least the two dom/dow rows
   against system `cron`/`croniter` rather than against our own
   reasoning.
6. **Schedule-guard gate.** With a deliberately malformed YAML in the
   backends dir, a scheduled warm **skips** with the resolve failure in
   `fleet_status` — it does not fire unguarded.
7. **Shutdown gate.** `Close()` returns within ~1 s while a warm is
   in flight against an unreachable front.
8. **Full inner loop** (ground rule 4): `go build ./... && go vet ./...
   && gofmt -l . && go mod tidy && golangci-lint run && go test -race
   -count=5 ./...`.
9. **The adversarial review pass itself.** After the above, run a real
   adversarial self-review over the whole C4 + C5 diff and land it as
   `review: adversarial-review fixes for C4/C5`, matching the C1/C2/C3
   pattern. The base rate on the siblings is 10–11 findings each;
   **expect it to find more than this document lists.** This phase is
   not done until that pass has run and its addendum is written into
   this doc.
10. **CI re-run.** Push and re-run CI; do not trust PR #22's existing
    green check (measured 50%+ failure before gate 1). Then merge #22.

### Gate results (2026-08-03)

| gate | result |
|---|---|
| 1 — race | **PASS.** `-race -count=20 -run TestWarmTarget` clean (43.9s). The pre-fix code was confirmed racy the same way: stashing the test fix reproduced a `DATA RACE` inside 20 runs. `-race -count=5 ./...` clean. |
| 2 — panic | **PASS.** `TestRenderLoopNonLlamaDefWithAnnouncedHashDoesNotPanic`, five sub-cases (comfyui / http_server / tabby_api / cloud_peer / unpulled mlx). Each completes, keeps the unverifiable def (fail-open), writes, and re-renders on the next transition. |
| 3 — config integrity | **PASS.** `TestRenderLoopEmptyDefsRefusesToOverwriteFrontConfig` (no write, `RenderCount` 0, file byte-identical) and `TestRenderLoopAllDefsExcludedStillWrites` (input-side proof). |
| 4 — warm policy | **NOT RUN (live).** Needs a real cell. Every sub-case has a unit regression instead — drained skip, mid-generation eviction + completion-edge restart, `>1h` window, no-activity-evidence — but the live run is still owed. |
| 5 — cron | **PASS.** Twelve-row table incl. all six both-restricted cases, `dow=7`, the century non-leap Feb-29. Cross-checked against Python `croniter`: nine of eleven checked rows agree exactly. The two that differ are the stepped-star rows, and croniter is the one out of step — it reads `*/2` as restricted, while cronie's `entry.c` sets `DOM_STAR`/`DOW_STAR` on any field whose first character is `*` (verified against cronie master). We follow the C implementation the format comes from; the divergence is recorded in the test. |
| 6 — schedule guard | **PARTIAL.** The unit half passes (`TestScheduleGuardSkipsWhenTheGuardCannotBeEvaluated`: resolve failure skips, unknown in-flight skips, front-only alias fires labelled). The end-to-end run with a real malformed YAML in a live backends dir is **NOT RUN**. |
| 7 — shutdown | **PASS.** `TestCloseUnblocksAnInFlightWarm` — `Close()` returns well inside 3s against a warm that blocks until its context dies. |
| 8 — inner loop | **PASS.** `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go mod tidy` (clean), `golangci-lint run` (0 issues), `go test -race -count=5 ./...`. |
| 9 — adversarial pass | **DONE**, addendum below. |
| 10 — CI re-run | **NOT RUN** — this phase does not push. |

## Addendum: the adversarial review pass (2026-08-03)

Gate 9, run over the whole C4 + C5 diff (`322712f..HEAD`) after §1–§8
landed. Six findings beyond the 35 this document lists; four fixed here,
two recorded as known limits.

1. **`warmCtx`'s cancel was not idempotent** (minor, C5-introduced).
   `context.CancelFunc` is documented as safe to call more than once; the
   returned closure did `close(stop)`, so a second call would panic. Both
   current call sites call it once, which is exactly how this survives
   until someone adds a third. Wrapped in a `sync.Once`.
2. **`modelSetChanged` misses a duplicate-id transition** (minor, C4).
   It compared slice lengths and then checked next⊆prev, so
   `[A,B] → [A,A]` reported "unchanged" and the render trigger was
   dropped. Duplicate ids are a protocol violation, but announces are
   untrusted input by C3's own threat note. Now compares id SETS.
3. **The fleet page's warnings panel grows without bound** (minor, C4).
   `$("warnings").prepend(w)` on every fingerprint-mismatch event, and a
   flapping strict def emits one per render (up to 1/min) — a tab left
   open for days accumulates DOM forever. Capped at 50.
4. **`boot()` rejections were unhandled** (nit, C4). `saveToken()` and
   the bottom-of-file `if (token) boot();` both called an async function
   without a catch, so a 401 during boot surfaced as an unhandled
   promise rejection in the console instead of the token gate the code
   already shows.
5. **Unknown in-flight now hard-skips a scheduled warm — and that rests
   on an unverified upstream behaviour** (known limit, C5-introduced by
   CC-3's mandate). `inFlightSeen[cell]` turns true on the first
   `inflight` frame. If llama-swap emits one on SSE connect, this never
   bites. If it emits only on add/remove edges, a fleetd restarted
   overnight would skip the 06:30 warm on a cell that served nothing
   since. It fails *visible* (`skipped (cell X in-flight unknown)` in
   fleet_status), not silent, and skipping is the safe direction — but
   it is a live check that is owed, and it is now written into
   `c4-comfort.md` §2 rather than left in a commit message.
6. **`warmViaFront` sends no Authorization header** (known limit, C4).
   If the front llama-swap is configured with `apiKeys`, every warm —
   target and schedule alike — fails with a 401 recorded as `warm
   failed: ... HTTP 401`. The reference front has no `apiKeys`, which is
   why the C4 live gate passed. Out of scope to fix here: it needs a
   front credential in `hosts.yaml`, which is config-surface design, not
   a review fix. Recorded so the next agent does not debug it from
   scratch.

Two items the audit reported were re-confirmed as **not defects** and
deliberately left alone: the `/ui/fleet` bearer exemption (exact-match,
GET-only, evaluated before mux path-cleaning — widening it to a prefix
match or `path.Clean` would be the actual vulnerability; the boundary is
now test-pinned in `daemon/fleet_registry_test.go`) and the page's
single-route surface.

## Out of scope

The C1–C3 substrate findings — presence discarding probe evidence, drain
reason/ETA destroyed on ack, fingerprint enforcement only inside a render
pass, the staleness state machine's zero coverage, and the announce /
intent / lease minors. Those are [C6](c6-substrate-repair.md), against
merged code, and must not hold up landing #22.
