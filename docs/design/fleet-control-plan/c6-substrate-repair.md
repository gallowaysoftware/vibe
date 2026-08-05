# C6 — Substrate repair: the C1–C3 findings against merged code

Status: MERGED (2026-08-03, PR #23, squashed as `cc98389`), EXECUTED +
REVIEWED, off `fix/c6-substrate-repair`,
branched off `main` at `322712f` per this doc's own rule. Depends on
[C5](c5-land-c4.md) only for merge order — none of this blocks landing
PR #22. Gates 1 and 2 are live and still NOT run.
See the execution
addendum and the adversarial-review addendum at the end (8 further
findings, all fixed with mutation-verified tests).

Two findings were only *half* landable here and were finished by the
post-merge reconciliation PR (#26), once C4's files were on `main`:

- **MIN-G** wired the piggyback producer for fleetmcp's `unload_model`
  only; `warmtarget`/`warmsched` are the other two producers the C3 doc
  names, and they were C4 files absent from this branch. #26 adds the
  same fallback there — same announced-model validation, same rule that
  a definitive 4xx is a real error and is not reported as "queued".
- **NIT-D** was correctly refused here (the line did not exist in merged
  code). It exists now; #26 folds it into the assertion below it.

Everything here is in **merged** code (`main` at `322712f`, PRs
#19–#21). It was found by the same verification pass that produced C5,
and it is separated for one reason: mixing merged-substrate repair into
the branch that lands C4 makes #22 unreviewable and unrevertable.

Four of these break a promise the design makes explicitly — M5 and M6
are user-visible, M7 is a stated invariant, M8 means a state machine
named in a passing gate has no coverage at all.

Anchors re-verified 2026-08-02 against `3854d84`. Ground rule 8 applies.

## 1. Presence and intent — promises the design makes and the code breaks

**M5 — presence staleness discards positive probe evidence** (major)
`presence.go:124-132`.

In the not-fresh branch, `decorate` sets `snap.Reachable = false`
unconditionally, ignoring `probeOK` — about twenty lines below the
comment asserting the opposite rule. So an announcer death (crashed
unit, rotated token ⇒ persistent 401, renamed cell ⇒ 400) makes a
healthy, serving cell report `reachable=false` / `OFF/AWAY?` *alongside
its live ready-model list and a `last_seen` of one second ago*.
Downstream, `vibe cell await <cell> --up` (`cli/cmd_cell.go:431`) never
fires — the design's parked-batch-job primitive hangs against a working
cell.

Fix: `snap.Reachable = probeOK`. The `if p.Withdrawn` block that pins
`HostReachable=false` then contradicts a succeeding probe — either gate
it on `!probeOK` too, or leave `HostReachable` alone and let
`displayState` render `INCONSISTENT` (cellUp && drained), which is what
that state is for.

Wrong way: restoring `Reachable` in `snapshotCell` after `decorate` —
`decorate` is also called from the render and warm paths and must own
the merge.

Test: presence with `Announcing=true, Stale=true, Models=[{ready}]` and
`snap.Reachable=true` on entry ⇒ stays true, display `SERVING`. Same
with `Withdrawn=true`. Existing fresh-branch cases must not regress.

**M6 — drain reason and ETA are destroyed the moment the cell acks**
(major)
`announce.go:306-311` (the delete), `announce.go:34-37` (the echo type
carries no reason/eta), `presence.go:142-146` (the bare rebuild).

The conflict rule deletes the registry intent on a newer echo, and the
echo has no reason/eta fields, so `decorate` rebuilds a bare
`Intent{State:"drained", Since: echo.Since}`. Verified: pre-ack
`{drained gaming 23:00} pending=true`; post-ack `{drained "" ""}`. This
is axis 2's headline feature — `fleet-control.md` §4's example, §7's
"visible in every status surface", and the C4 page pill at
`fleet.html:137` — gone within one heartbeat for **every announcing
cell**, i.e. every cell after C3.

Fix, both halves, landing together:

1. In `recordAnnounce` (the block at `:303-316`): when the newer echo
   **agrees** with the request and the state is `"drained"`, re-store
   rather than delete —
   `next[req.Cell] = Intent{State: "drained", Reason: req.Reason,
   ETA: req.ETA, Since: echo.Since}` — and only `delete` when the echo
   diverges, or the request was `"serving"`.
   **Subtlety:** you must *not* keep a `"serving"` entry on agreement —
   `decorate` treats a stored serving entry as `servingRequest` and
   would set `IntentPending=true` forever (`presence.go:141,151`). With
   `Since` set exactly to `echo.Since`, `decorate`'s override at
   `presence.go:142` correctly does not fire and `IntentPending` stays
   false.
2. In `decorate`: when the echo-drained override fires and
   `hasIntent && intent.State=="drained"`, carry `Reason`/`ETA` into the
   rebuilt effective intent. This is also the only fix for the reason
   surviving a fleetd restart of a locally-drained cell.

Do this in the same pass as MIN-C so the entry is written into a clone
that is persisted before the swap, not into the live map.

Test: `SetIntent(cell,"drained","gaming","23:00")` → `recordAnnounce`
with echo `{drained, Since=now+1s}` ⇒ `Reason=="gaming"`,
`ETA=="23:00"`, `IntentPending==false`. A diverging echo
(`{serving, newer}`) must still clear the request.

**MIN-C — intent paths mutate memory before persisting** (minor)
`announce.go:307, :317-321` (`recordAnnounce`) and `:366, :372-375`
(`pruneStaleServingRequest`) — the reverse of C1's clone→persist→swap
discipline, so a failed write resurrects a resolved drain on restart.

Fix: build `next` under `s.mu`, unlock, persist, and only then re-lock
and swap `s.intents = next`. Both already run under `s.intentMu`, so the
swap is safe. On persist failure in `recordAnnounce`, return the
response **without** dropping the request so the drop retries next
heartbeat. Note `recordAnnounce` currently deletes in place while
`SetIntent` swaps the map pointer; after this change every writer swaps,
removing the mixed-discipline hazard.

Test: point `intentPath` at an unwritable path, drive `recordAnnounce`
with a newer echo ⇒ `Snapshot` still reports the drained intent (memory
did not diverge from disk), and the next successful persist resolves it.

**MIN-D — cell-side intent file can be torn or lost** (minor)
`fleetannounce.go:168-184` (fixed `.tmp` at `:179`); `SetLocalIntent`
releases `c.mu` at `:157` before persisting at `:158`.
`fleetapi/intent.go:175-197` got this right — mirror it:
`os.CreateTemp(dir, ".intent-*")` + write + Close + Chmod(0600) +
Rename, removing the tmp on every error path. Then serialise ordering
with a dedicated `persistMu` held across (mutate, write). Unique tmp
names alone are **not** sufficient — they fix corruption but leave the
last-writer-wins inversion between the announce loop and the drain RPC.

Test: concurrent `SetLocalIntent("drained")`/`("serving")` from N
goroutines under `-race` ⇒ the on-disk file always parses and matches
`LocalIntent()` at quiesce.

**MIN-E — announce-derived `last_seen` is never persisted** (minor)
`announce.go:324-327`; the probe path persists only the first sighting
(`presence.go:40-48`). Wrong for exactly the no-inbound-port cells C3
exists for. Fix with an **age-gated** persist (per-cell
`lastSeenPersisted`, write when older than `staleAfter`, plus
unconditionally on the stale/withdrawn/returned transitions already
computed at `:285-293`), and give `noteSighting` the same treatment.
Wrong way: persisting on every announce — at 15 s cadence × N cells
that is a full-file rewrite per heartbeat.

**MIN-F — `intent.state = "withdrawing"` has no producer** (nit)
Validators and consumers exist (`announce.go:183, :253`;
`fleetannounce.go:143`); no assignment site outside tests. The design's
clean-undock path is unreachable without hand-editing state. Pick one:
add the producer (a `vibe cell withdraw` / daemon-shutdown hook calling
`SetLocalIntent("withdrawing")` before the announce loop stops — this is
what makes prune-without-waiting-out-`staleAfter` reachable), or delete
the state from the enum and let staleness carry the case. Do not leave
it half-wired: the render loop's prune policy is written assuming a clean
withdraw exists.

## 2. Announce plumbing

**MIN-H — piggybacked commands are lost silently** (minor)
`announce.go:333` → `:408-414` (`drainCommands` deletes) while delivery
happens later at `:156-158`. At-most-once, silently.

Fix: make it at-least-once keyed on the announce seq — `drainCommands`
moves the queue to an `inflight[cell]` slot stamped with `req.Seq`
rather than deleting; the next announce from that cell with a **higher**
seq drops the slot before draining new work. Both verbs (unload/warm)
are idempotent, so a duplicate is harmless. Cheaper stopgap: check
`Encode`'s error and re-queue — but say plainly that this does not cover
a successful write the cell never read, which is the common failure.

**MIN-G — the piggyback queue has no production producer** (nit)
`announce.go:425` (`queueCommand`; callers are tests only), while
`c3-announce.md:55-58` describes it in the present tense. Either wire
the producer — fleetmcp's unload/warm tools and warmtarget/warmsched
falling back to `queueCommand` when a cell has no `daemon_url` or the
interactive call fails, validating `Model` against the cell's announced
set first, as the comment at `:416-419` already demands — or amend the
doc to say the queue is plumbed but unproduced. **Fix MIN-H before
wiring a producer, not after.**

**MIN-I — `stalenessLoop` calls `wg.Add(1)` inside the goroutine**
(minor) `announce.go:467-469`, launched from `fleetapi.go:298`, racing
`Close()`'s `wg.Wait()`. Every sibling loop adds before `go`. Move the
`Add` to `Start`; do not add a second without removing the first.

**MIN-J — the lease store is unvalidated and unbounded** (minor)
`leases.go:79-128`, validation only at `:85-99` — while the announce
ingest sanitises exactly this class of data. (The audit's "20 lines
away" is wrong; `validateAnnounce` is in `announce.go:161-208`.)

Fix: extract the `clean(label, v)` closure from `validateAnnounce`
(`announce.go:170-180`) to a package helper and call it for
`req.Model`, `req.Holder`, `req.Note`; bound the TTL (e.g. 168 h); cap
`len(next)` — after the expiry prune at `:129-136`, so expired entries
don't count toward the cap.

## 3. Render loop

**M7 — fingerprint enforcement only runs inside a render pass** (major)
`render_loop.go:218` (the only `applyFingerprints` call) and `:166-191`
(the select has no ticker); passes fire only on membership transitions
(`announce.go:274-279, :339-348, :380-394`).

A steady-state `flags_sha256` change with a stable model-id set fires
nothing. So a strict-mode mismatch on an `always_on` or `opportunistic`
cell — exactly where strict embed defs live — raises no
`fleet.fingerprintMismatch` and causes no exclusion until some unrelated
transition, often "whether the roaming laptop happens to be open".
`fleet-control.md` §6 says *"Mismatch always raises a loud event."*

Fix: extend the transition test in `recordAnnounce` (`:274-279`) with
`func modelFingerprintChanged(prev, next []AnnounceModel) bool` —
build `map[id]FlagsSHA256` from `prev` and return true when any `next`
entry's non-empty hash differs for the same id — then trigger a render
on `modelChanged || modelFingerprintChanged(...)`. `prevModels` is
already captured at `:265`. This reuses the existing coalescing and the
1/min cooldown, and `renderPass`'s unchanged-content check keeps it
write-free when only the event needed to fire.

Wrong way: folding the hash into `modelSetChanged` wholesale — that
struct also carries `State`, which flips running/stopped constantly and
would turn every model start/stop into a membership transition.

Test: two announces from an `always_on` cell with identical id sets where
only `flags_sha256` changes ⇒ a `fleet.fingerprintMismatch` event, and
for a `fingerprint: strict` def the model leaves the rendered config.
Mirror case (sha unchanged ⇒ no extra render) so the trigger doesn't fire
every announce.

**MIN-A — the render loop writes the front config `0600`** (minor)
`render_loop.go:398-418` (`os.CreateTemp` at `:399`, no `Chmod`) versus
`router/render.go:651-673` (`tmp.Chmod(0o644)` at `:665` — the audit's
`:657` is off by eight). The file exists to be read by another process,
and this also silently downgrades an operator's `0644` file.

Fix: `tmp.Chmod(0o644)` after the Write and **before**
`tmp.Close()`/`os.Rename`; better, preserve an existing file's mode when
`os.Stat` succeeds. Chmod-after-rename leaves a window where the watcher
reads an unreadable file — the exact failure the atomic path exists to
prevent.

**MIN-B — fingerprint home-normalization is one-sided** (minor)
`router/fingerprint.go:50-55`. The rewrite is relative to the *local*
`$HOME`, so a def carrying a literal absolute `/home/<user>/…` path
hashes differently on cell and fleetd forever; on a `strict` def that
fail-closed yanks a working model.

Fix: make it box-independent — fold a token to `~/…` when it starts with
the local home, **or** matches `^/home/[^/]+/`, **or** starts with
`/root/`. State the trade in the comment: this makes two users' trees on
one box hash identically, i.e. it fails **open** — the right bias, since
a false mismatch yanks a working model while a false match misses one
flavour of drift. Wrong ways: dropping path-valued flags from the hash
(a genuine `--model` change would stop being detected), or making fleetd
trust the cell's hash.

**NIT-B — `renderCounts` is a never-cleaned package global** (nit)
`render_loop.go:69-73`, `:106-107`, `:78-83`; no delete anywhere. Move it
to the `Server` struct as `renderWrites atomic.Int64` (same place C4
added `warmStates`/`schedStates`) and delete the `sync.Map`. No lock
ordering concern — atomic, never taken under `s.mu`. Delete the false
justification comment with it (see DOC-4).

## 4. Actuation and CLI

**MIN-N — `--wait` silently no-ops, and the local cell key is hardcoded
`"front"`** (major)
`daemon/cell_drain.go:68, :142-149, :153`.

Nothing in the RPC response, CLI output, or MCP text says a requested
wait was skipped — an operator who asked for quiescence gets an
immediate stream-cancelling drain and no indication. Separately,
`InFlight("front")` is a literal, so a fleetd-role box reads a
*different* cell's in-flight counter.

Fix: (1) add `optional string wait_status = 5;` to `CellDrainResponse`
(`proto/vibe/v1/control.proto`) with values `not_requested` / `waited` /
`skipped_no_inflight_data`; change `awaitQuiescence` to return
`(status string, err error)` so the skip branch reports rather than only
logging; print it in `printDrainReport`
(`cli/cmd_cell_actuate.go:278`) and append it in `toolDrainCell`
(`fleetmcp/actuate.go:101-121`). (2) add
`func (d *Daemon) localCellKey() string` returning `d.cfg.Fleet.Cell`
when non-empty **and** `d.cfg.FleetRegistry` is set, else
`fleetcfg.FrontCell`.

**Wrong way:** substituting the `fleetcfg.FrontCell` constant for the
`"front"` literal — the constant *is* `"front"` (`fleetcfg.go:47`), so
that is a no-op rename that fixes nothing.

Requires `buf generate` (ground rule 4).

**MIN-O — three shipped strings still assert the drain assumption C2
falsified** (minor)
`fleetmcp/fleetmcp.go:270-271`, `cli/cmd_cell_actuate.go:45`,
`daemon/daemon.go:155`, plus the design source at
`fleet-control.md:294-296`. The truth, established by C2's own live gate
and recorded at `AGENTS.md:154`: llama-swap v239's SIGTERM calls
`CloseStreams()` **before** its graceful drain, so the unit stop cancels
in-flight streams immediately and `--wait` is what drains them first.
Rewrite all four. The **MCP description is the highest-value one** — an
agent reads it as fact and will tell an operator their stream is safe.

**MIN-P — the CLI swallows a `token_file` read error** (minor)
`cli/cmd_cell_actuate.go:186-194` (the swallow is `err == nil` at
`:188`); `fleetmcp/actuate.go:36-46` makes it a typed error. Adopt
fleetmcp's shape. Keep the `$VIBE_TOKEN` short-circuit **ahead** of it
(an explicit override must not be defeated by an unreadable file), and
keep `vibeclient.ResolveToken()` reachable only when `TokenFile` is
empty — do not make it the fallback for a read failure, which is exactly
the silent path being removed.

**NIT-E — `$VIBE_TOKEN` overrides every per-cell `token_file`** (minor)
`fleetmcp/actuate.go:36`. In fleetd this makes the whole
`cells.X.token_file` config dead. Invert the precedence **for the
daemon** — `TokenFile` first when set, env only when empty — and log
once at startup when both are present. **Leave the CLI's order alone**
(`cli/cmd_cell_actuate.go:186`): for a human typing one command,
`$VIBE_TOKEN`-wins is correct. The two call sites should intentionally
diverge, and the comment at `actuate.go:22-25` must say so instead of
claiming it mirrors the CLI.

**MIN-Q — `warm_model` refuses chat-class models** (minor)
`fleetmcp/fleetmcp.go:429-432` rejects any id in `model_classes`,
including one classed `chat`, with a self-contradicting message. Skip
chat-class entries rather than deleting the guard — its purpose
(refusing to fire a chat completion at an embed/rerank id) is correct
and must keep failing loudly. Pair with a class-vocabulary check in
`fleetcfg.validate()` so a typo'd class can't silently gate it either.

**MIN-R — strict hosts.yaml decode hard-fails fleetd startup** (minor)
`fleetcfg/fleetcfg.go:121-124` (`KnownFields(true)` at `:122`); the abort
is `daemon/daemon.go:438-444`. A hosts.yaml that also carries
fleet.md §4.1's top-level `hosts:` inventory kills fleetd, though the
plan said "leave room". Add an inert `Hosts map[string]yaml.Node
\`yaml:"hosts,omitempty"\`` to `fleetcfg.File` — parsed and ignored — so
the two schemas share one filename. **Do not turn `KnownFields` off**:
the comment at `:117-120` exists precisely because a typo'd key must not
silently degrade a cell's display semantics.

**MIN-S — `vibe cell await` parks forever on a typo'd cell** (minor)
`cli/cmd_cell.go:340` (`--timeout` default 0), `:434` (the unknown-cell
error), `:393-396` (the retry that swallows it). Fix with **unknown-cell
validation, not a default timeout**: a sentinel `errUnknownCell` wrapped
by `checkCellReachable`, and `errors.Is` fail-fast in `awaitCell`. Do
**not** make every `checkCellReachable` error fatal — a restarting
fleetd is a transport error and must keep retrying, which is the point
of the loop. Leave `--timeout 0`: a nonzero default would silently break
the documented overnight-batch idiom.

**NIT-F — `vibe fleet announce` declares an `intentPath` knob it never
exposes** (nit) `cli/cmd_fleet.go:38, :66-68, :76, :85-89`. Either
delete the dead variable and pass `paths.CellIntentFile()` directly, or
register the flag (two announcers on one box must not share an intent
echo file).

## 5. Test truth

**M8 — C3's staleness state machine has zero coverage** (major)
`announce_test.go:173-183` `markStaleOnce` is a test-file **copy** of the
predicate at `announce.go:480`, called from `announce_test.go:77`.
Mutation-proven: short-circuiting the production predicate leaves the
**entire repo** green. `pruneStaleServingRequest` sits at 0.0% coverage,
`stalenessLoop` at 35% — while gate 6 names "staleness state machine"
and reports PASS.

Fix: (1) make the interval injectable — an unexported
`stalenessTick time.Duration` on `Server`, defaulting to 5 s in `New`,
used at `announce.go:470` instead of the literal. (2) Delete
`markStaleOnce`, and rewrite the assertion to back-date `ReceivedAt`, run
the **real** loop with a ~10 ms tick, and assert the whole tail: the
`EventCellStale` publish (`:489`), the render trigger (`:490`), and
`pruneStaleServingRequest` having dropped the serving intent (`:491`) —
that last one is the 0.0% function and the only reason the loop has side
effects at all.

**Wrong way:** keeping `markStaleOnce` and adding assertions to it — the
mutation above would still pass.

**MIN-K — `TestCellDrain_ReportThenCmdThenIntent` doesn't assert its
ordering** (minor) `daemon/cell_drain_test.go:78-146` (assertion at
`:139-146`) records one marker and concedes the ordering in a comment.
Have the intent handler and the `/running` handler both `mark(...)`, then
assert `["report", "cmd:true", "intent"]` and delete the comment. Guard
against the second `/running` probe by recording only the first.

**MIN-L — `TestRegistryFailureBackoffAndRecovery` tests neither** (minor)
`fleetannounce/fleetannounce_test.go:379-398` asserts only that `Run`
returns nil against a dead address. Replace with an httptest registry
that counts calls and returns 503 for the first N then 200; assert the
attempt count over the window is bounded well below `interval/duration`
(backoff grows) and that a successful announce lands after the flip
(recovery). Assert a **range**, not an exact count, or it becomes
timing-flaky on loaded CI.

**NIT-D — leftover debug `t.Logf`** at
`daemon/fleet_registry_test.go:207`. Delete, or fold into the `t.Errorf`
below.

## 6. Doc truth

- **DOC-3 / `c3-announce.md:204-205`** (the audit said `:203`) names SSE
  events in snake_case that the code never emits. The real constants are
  dotted camelCase: `fleet.cellStale`, `fleet.cellWithdrawn`,
  `fleet.cellReturned`, `fleet.fingerprintMismatch`; `model_degraded`
  stays reserved with no emitter. Add `fleet.cellUp`/`fleet.cellDown`
  (probe-path transitions, `fleetapi.go:571`) if the list is meant to be
  exhaustive. **Do not rename the constants to match the doc** — the CLI
  and live SSE consumers already depend on the dotted names.
- **DOC-4 / `c3-announce.md:63-66`** claims `RenderCount` is a package
  global because "the render loop couldn't extend the `Server` struct
  under its contract". No such contract exists, and the same commit
  (`322712f`) adds three fields to that struct. This is an invented
  justification — the only one found in the whole run. Replace it with
  the truth (an accident of authoring order) and fix the source comment
  at `render_loop.go:69-72` in the same edit, or the fiction regenerates
  from the code next time someone documents it. Pairs with NIT-B.
- **DOC-6 / `deploy/front/README.md:75-82, :86-91`** still speaks in
  hand-edit voice though C2/C3 automated the writes. Rewrite to present
  tense: the per-peer `models:` lists are derived, fleetd renders and
  writes them atomically on every membership transition, `-watch-config`
  applies in place, hand edits are emergency-only and the next render
  overwrites them. **Keep the v239 30 s-drain paragraph** — still true
  and load-bearing.

## Acceptance gates

1. **Probe-evidence gate (live).** Kill a cell's announcer while its
   llama-swap keeps serving ⇒ the cell still reads reachable, and
   `vibe cell await <cell> --up` returns instead of hanging.
2. **Drain-reason gate (live).** Drain a cell with a reason and ETA;
   after the cell acks and three further heartbeats, both are still
   visible in `fleet_status`, the CLI, and the fleet page — and survive
   a fleetd restart.
3. **Fingerprint-steady-state gate.** Mutate a serving flag on a
   `strict` def on an `always_on` cell with no membership change ⇒ loud
   event and exclusion, without waiting for an unrelated transition.
4. **Staleness mutation gate.** Short-circuit the `announce.go:480`
   predicate ⇒ the suite must now **fail**. This is the test for the
   test.
5. **Wait-status gate.** `drain --wait` against a cell that never
   reported in-flight counts ⇒ the CLI and the MCP tool both say the
   wait was skipped.
6. **Full inner loop** including `buf generate` for the proto change.
7. **Adversarial self-review pass**, landed as its own `review:` commit
   with an addendum in this doc (ground rule 9).

## Out of scope

Anything that lands C4 — that is [C5](c5-land-c4.md). The v2 throughput
`probe` block (still reserved). Per-cell announce credentials (the fleet
token remains every cell's voice; tracked in
[../fleet-control-futures.md](../fleet-control-futures.md)).

## Execution addendum (2026-08-03)

Branch `fix/c6-substrate-repair` off `main` at `322712f`. Four commits:
the two majors + their neighbours, the actuation/CLI set, the plumbing
set, and MIN-O's last two sites.

**Anchor drift.** Every anchor in this doc was verified against
`3854d84` (the C4 branch), but the work lands against `322712f`
(`main`), where C4's +36 lines in `announce.go` had not been added.
`announce.go` anchors therefore sit ~28 lines EARLIER on main: the M6
delete was `:296` not `:306`, MIN-I's `wg.Add` `:440` not `:467`, M8's
predicate `:452` not `:480`. Same code, same findings.

Two anchors did not exist in merged code at all:

- **M7's fix instructions** reference `prevModels` "already captured at
  `:265`" and warn against folding the hash into `modelSetChanged`.
  Neither exists on `main` — both are C4 additions. `prevModels` is now
  captured here too, and `modelFingerprintChanged` is a separate
  predicate, so the merge with C4 is a two-boolean union rather than a
  rewrite.
- **NIT-D** (`daemon/fleet_registry_test.go:207`, a leftover `t.Logf`)
  is in C4's fleet-page auth test. It is not on `main`. **Not fixed
  here — it belongs to PR #22.** *(It did not land with #22 either; the
  post-merge reconciliation PR #26 folded the body length into the
  `t.Errorf` below it, where it is diagnostic on failure instead of
  noise on every run.)*

**Judgement calls, stated because they diverge from the letter of the
doc:**

- **MIN-F** asked for a `withdrawing` producer that calls
  `SetLocalIntent("withdrawing")`. `Client.Withdraw` sends the goodbye
  heartbeat but deliberately does NOT persist it: the echo file is the
  cell's durable drained-vs-serving record, and a persisted
  `withdrawing` read back at next boot would either lie (the box is
  here) or erase a drain the operator still means. Wired into the
  daemon's shutdown path and `vibe fleet announce`.
- **MIN-G** wired the producer rather than amending the doc, but only
  for `unload_model` — `warmtarget`/`warmsched` are C4 code and not on
  this branch. MIN-H landed first, as instructed. *(Finished in PR #26
  once C4 was on `main`: both warm loops now fall back to
  `QueueCommand`, and the warm path grew a typed `warmHTTPError` so the
  4xx-is-not-a-delay rule is decidable there too rather than parsed out
  of a string.)*
- **MIN-B** folds `/home/<user>/…`, `/root/…` and `/Users/<user>/…` in
  addition to the local `$HOME`, and says in the comment that this fails
  open.
- **The plan README's status column was not updated**: it exists only on
  the C4/C5 doc branch (commit `92a50bd`), together with this file. This
  file was imported from there so the phase has its work order and its
  addendum in one place; the README row lands with #22.

**Behaviour changes visible in existing tests** (both updated, neither
weakened):

- `TestResumeViaAnnounceEndToEnd` asserted the drained request is
  DELETED on the cell's ack. That deletion is M6. It now asserts the
  entry survives as the RECORD, carries the reason, and is not pending.
- `TestCellAwaitUnknownCell` asserted that a typo'd cell keeps waiting.
  That is MIN-S. Renamed `…FailsFast` and inverted, with a 3s context as
  the regression detector.

**Gate results.**

1. Probe-evidence gate (live) — **PASS (2026-08-05, local multi-cell
   harness — [`scripts/fleetlab`](../../../scripts/fleetlab/README.md)).**
   All three announcers SIGKILLed at once (two slim `vibe fleet announce`,
   one in-daemon loop) with every llama-swap left serving. Staleness
   landed at t+40–50 s and each cell then read `stale=true` but
   `reachable=true host_reachable=true display=SERVING` — the announce
   was retired, the probe was not — and `vibe cell await alpha --up`
   returned on the probe alone. This is the substitution the design
   asks for: a cell whose announcer is dead while its llama-swap serves.
2. Drain-reason gate (live) — **PASS (2026-08-05, same harness).**
   `drain_cell {cell: bravo, reason: …, eta: …}` through fleetd against
   bravo's real cell daemon. Pre-ack the display was `INCONSISTENT`; the
   ack landed at seq 25 and the intent was re-stored with
   `intent.since == echo.since` to the nanosecond — the equality the
   reason/ETA survival depends on. Reason and ETA then survived three
   further heartbeats and a fleetd restart.
3. Fingerprint-steady-state gate — PASS, as
   `TestRenderLoop_SteadyStateFingerprintDriftEnforced` (drift with an
   identical id set on an `always_on` cell ⇒ event + strict exclusion)
   plus its mirror (unchanged sha ⇒ no extra render).
4. Staleness mutation gate — PASS. With `if false &&` prefixed to
   `stalenessLoop`'s predicate (`announce.go:452` before this branch's
   edits, `:558` after), `go test ./internal/vibe/fleetapi/` FAILS:
   `TestAnnouncePresenceTransitions` ("cell never went stale") and
   `TestStalenessLoop_PublishesPrunesAndTriggersRender` ("timed out
   waiting for fleet.cellStale"). Predicate restored; suite green.
5. Wait-status gate — PASS at unit level
   (`TestCellDrain_WaitSkipIsReported`, `TestPrintDrainReport_WaitStatus`,
   `TestMCPDrainCellReportsSkippedWait`), and the end-to-end run landed
   in the same 2026-08-05 harness session as gate 2:
   `vibe cell drain bravo --wait 15s --reason 'gate 4a'` reported
   `resident at drain: lab-embed-b / in-flight requests at drain: 0 /
   waited for in-flight requests to reach zero before draining` — the
   wait status printed from a real cell's real in-flight counter.
6. Full inner loop — PASS, including `buf generate` for
   `CellDrainResponse.wait_status`.
7. Adversarial self-review — PASS, landed as its own `review:` commit;
   see the addendum below.

## Adversarial-review addendum (2026-08-03, 8 findings, all fixed with
regression tests)

Every fix below is mutation-verified: the production change was reverted
and the named test was watched to FAIL, then restored.

- **Withdraw raced the live announce loop, and its goodbye was
  undone (MAJOR)** — `daemon.go`'s shutdown called
  `announce.Withdraw` while `ann.Run(ctx)` was still in flight.
  `Client.seq` and `authRejected` were loop-owned and unsynchronised, so
  the two paths were a **data race** (`-race` reproduces it); worse, on
  the `shutdown rpc` path `ctx` is never cancelled, so the loop kept
  heartbeating after the withdraw — observed as
  `[serving withdrawing serving]` — and leaked past `Run`'s return. The
  loop now owns a cancel + done channel; `Daemon.withdrawAnnounce`
  stops it (bounded 3s) and only then says goodbye, and seq/authRejected
  moved under mutexes so the exported API is safe either way.
  `TestWithdrawAnnounceStopsLoopFirst`, `TestWithdrawConcurrentWithRun`.
- **The announce loop never started without a backends dir (MAJOR,
  pre-existing C3)** — `startAnnounce` guarded with `os.IsNotExist`,
  which does NOT unwrap, while `router.LoadDefs` wraps its `ReadDir`
  error. A cell with no `~/.config/vibe/backends` (defless catalog, defs
  not yet converged) logged "announce loop not started" and stayed
  invisible to fleetd — the exact cell C3 exists for. Now `errors.Is`.
  Found by writing the withdraw test against a clean XDG dir.
- **MIN-A was a no-op on every box that had the bug (MAJOR)** —
  `writeAtomic` preserved an existing file's mode, and every fleetd
  deployed before C6 left a **0600** front config behind, so the fix
  inherited 0600 forever on exactly the machines it was written for.
  Mode is now `perm | 0o044`: an operator's widening survives, a pre-fix
  0600 is repaired. `TestWriteAtomic_ModeIsReadable` grew the upgrade
  case.
- **The lease cap refused the DELETE that drains it (MINOR)** — the
  `len(next) > maxLeases` check ran for both verbs, so an over-cap store
  (a downgrade, a hand edit, a pre-cap file) answered
  `409 ... expire or delete some first` to the delete that was the way
  out. The cap now gates growth only. `TestLeaseCapGatesGrowthOnly`.
- **A definitive 4xx was queued as a piggyback (MINOR)** — MIN-G's
  fallback fired on any non-200, so llama-swap answering "no such model"
  told the agent the unload was "queued for its next announce" about a
  verb the cell will refuse identically. Transport errors and 5xx still
  fall back (that is the delivery failure the queue is for); 4xx is now
  the error it is. `TestMCPUnloadDefiniteAnswerIsNotQueued` plus its
  mirror `TestMCPUnloadUnreachableCellQueues`, which is also the first
  end-to-end test of the queue → announce-response path.
- **MIN-Q's actual fix had no test (test truth)** — reverting
  `class != ModelClassChat` left the whole suite green: the existing
  warm test uses an *unpinned* id. `TestMCPWarmModelChatClassIsAllowed`
  covers both halves (chat warms, embed still refuses loudly).
- **MIN-Q's config half and MIN-R had no tests at all (test truth)** —
  nothing exercised the class vocabulary or the inert `hosts:` key, so
  deleting either from `fleetcfg` kept the suite green.
  `TestLoad_ModelClassVocabulary`, `TestLoad_ToleratesHostInventory`
  (which also pins that `KnownFields(true)` stays strict about the keys
  it owns).
- **MIN-P/NIT-E had no tests (test truth)** — the two token orders are
  *deliberately opposite*, which is exactly the pair a later refactor
  unifies by accident. `TestResolveCellClientTokenPrecedence` (CLI:
  `$VIBE_TOKEN` wins, unreadable file is a named error) and
  `TestCellClientTokenPrecedence` (fleetd: the per-cell file wins).

**Verified, not changed.** `TestRenderLoop_SteadyStateFingerprintDriftEnforced`
really does fail when `modelFingerprintChanged` is short-circuited (gate
3 re-confirmed here, since gate 4 is the only one the doc proved that
way); M5, M6 and MIN-C likewise fail under mutation. `decorate` copies
presence under the lock and `recordAnnounce` no longer dereferences the
live `*Presence` after unlocking (the C4-class bug is absent).
`modelFingerprintChanged`'s deliberate blind spot — an id the cell was
NOT already announcing — is covered by C4's `modelSetChanged` on merge,
so it was left as the doc specifies.

**Judgement call, recorded not fixed.** A daemon *restart* on a roaming
cell now sends a withdraw, which prunes its defs from the front render
until `MinHealthyStreak` fresh announces (~45s). That is the documented
prune-fast/re-add-slow policy meeting MIN-F's shutdown hook; a restart
is indistinguishable from an undock from inside the process. Worth
knowing before someone debugs a 45s catalog gap after `systemctl restart
vibe` on the laptop.

**Gates after the review pass.** Full inner loop re-run: `go build`,
`go vet`, `gofmt -l` (silent), `go mod tidy` (clean), `golangci-lint run`
(0 issues), `go test -race -count=5 ./...` — all green. Gates 1 and 2
remain live and NOT run.

## Merge addendum — C6 against main with C4+C5 landed (2026-08-03)

`main` moved to `28a8073` (PR #22, C4+C5 squashed) while this branch sat
at `322712f`. Merged main IN (no rebase — one resolution, and the PR
squashes anyway). Four conflicted files, plus two that auto-merged in
regions worth naming.

**`recordAnnounce` — the forecast conflict, resolved as a union.** C4
added `modelSetChanged` (id-set membership) and C6 added
`modelFingerprintChanged` (hash drift at a stable id set). Both capture
the same `prevModels`; both are re-render reasons. One capture, one
trigger: `if modelChanged || fingerprintChanged`. The unlock sits where
C4 put it (before the events switch, which reads only locals), so the
trigger never fires under `s.mu`.

**`Server` struct + `New`.** Additive both ways, no semantic overlap:
C4's `modelActivity` / `lastInFlightModels` / `started` / `warmStates` /
`schedStates` alongside C6's `stalenessTick` / `renderWrites` /
`cmdInflight` / `lastSeenPersisted`.

**`presence.go`, auto-merged, verified by hand.** C5 factored the
declared-intent rule into the free function `resolveIntent` and made
`decorate` delegate; C6 changed the same function's echo-override branch
to carry the operator's reason/ETA (M6) and changed `decorate`'s
not-fresh branch to stop retiring probe evidence (M5). The result has
exactly ONE copy of the rule, and it is the copy carrying M6 — which
matters because the C4 warm loops read it through `effectiveIntent`.

**`render_loop.go`, auto-merged, verified by hand.** C5's backend-kind
guard in `applyFingerprints` (the `ModelCmd` nil-deref) and C6's
`writeAtomic` mode repair are both present; C5's error-tolerant
`applyFingerprints` signature composes with C6's `renderWrites` counter.

**One real defect the merge created, found by C5's test suite.** C6's
clone → persist → swap made the in-memory swap conditional on
`saveIntents` succeeding. `saveIntents` treats an EMPTY path as the
error `"intent store disabled"`, so on a server with no intent store the
C3 conflict rule stopped resolving anything: a resume performed at the
box left the request pending forever, and C4's warm loops — which read
`s.intents` whether or not a store exists — kept the target skipped.
`TestWarmTarget_SkipsDrainedCell/newer_serving_echo_resolves_the_drain_request`
caught it.

Fixed by distinguishing a write that FAILED from a write that can never
succeed: `Server.persistIntents` returns nil when `intentPath == ""`
(nothing on disk for memory to diverge from, no restart to resurrect a
resolved drain) and both resolution writers — the announce conflict rule
and `pruneStaleServingRequest` — go through it. `setIntent` still calls
`saveIntents` directly, so an operator POST to a disabled store keeps
failing loudly. C6's MIN-C guarantee is untouched: its test drives a
genuine write failure against a non-empty path, and still passes.

**Gates after the merge.** `go build`, `go vet`, `gofmt -l` (silent),
`go mod tidy` (clean), `golangci-lint run` at CI's pinned v2.12.2
(0 issues), `go test -race -timeout 240s ./...` and
`go test -race -count=2 ./...` — all green. Gates 1 and 2 remain live
and NOT run: nothing in this merge was exercised against real hardware.
