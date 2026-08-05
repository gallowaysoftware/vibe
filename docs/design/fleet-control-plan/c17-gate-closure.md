# C17 — closing the gate-honesty gap

Status: **PR OPEN** (2026-08-05), off `feat/c17-gate-closure` branched
from `main` at `0c275fd`. **No production Go code changes** — this phase
is a measurement pass, and its output is evidence. What it adds is
fourteen repeatable gate scripts under
[`scripts/fleetlab/`](../../../scripts/fleetlab/README.md); what it
changes is every gate ROW it was able to move, in the phase doc that owns
it. `internal/vibe/` is untouched, so `go.mod`/`go.sum` are
byte-identical and the `internal/vibe/proxy` diff is empty by
construction.

PR #34 published a coverage table across the plan: 26 gates PASS, 10
PARTIAL, 13 "STILL-NEEDS-METAL". Its own entries then admitted that
**eight of those thirteen were not physical at all** — "NOT PHYSICAL",
"NONE — fully runnable on the harness", "NONE — runnable on the harness
today", "Not attempted", "Dropped for scope". A gate that needs a
wattmeter and a gate nobody got to are different facts, and filing them
under one heading is precisely the failure ground rule 10 exists to name.
It is also the second time this plan has made it: #34 itself exists
because eleven phase docs said "needs the real fleet" when they needed a
second *process*.

This phase ran them.

## Result

**Fourteen gate rows moved.** Every gate #34 named as runnable was run.
Two findings came out of running them — one a live product defect in
C8's baseline binding, one a documented figure that is wrong by 3x.

| phase | gate | was | now |
|---|---|---|---|
| C7a | 2 — no double count (live half) | PARTIAL | **PASS** |
| C7a | 5 — self-traffic (live half) | PARTIAL | **PASS** |
| C8 | L4 — the 96/day cap | NOT RUN | **PASS**, boundary seeded on disk (§C8 L4) |
| C8 | L5 — embed probe baseline | NOT RUN | baseline half **PASS**; flag-change half **FAIL** — a real defect (§finding 1) |
| C9 | 14a — a real sink | PARTIAL | **PASS** for the ntfy half; the phone half is still owed |
| C9 | 14d — a def edited on the front | NOT RUN | **PASS** |
| C10 | 13d — two shells, a lease handshake | NOT RUN | **PASS** |
| C11 | L2 — a hold is not a pin | NOT RUN | **PASS** |
| C11 | L3 — inheritance (schedule half) | PARTIAL | **PASS** |
| C12 | L1 — the hallway test (DOM half) | PARTIAL | **PASS**, real Gecko |
| C12 | L3 — guest token rotation | NOT RUN | **PASS** |
| C13 | bonus — defs parity after #36 | PASS/PARTIAL | **PASS**, the inversion is fixed |
| C14 | L3 — the operator at 23:29 | NOT RUN | **PASS** |
| C14 | L4 — the mid-batch night | NOT RUN | **PASS** |

Every run is against `scripts/fleetlab` — four real llama-swap v239
processes, a real fleetd, both announcer shapes, CPU models — on
`FLEETLAB_DIR=/tmp/fleetlab-c17`, 2026-08-05. The harness README's two
standing qualifications apply to all of it: **CPU models are not GPU
models**, and **one box is not a fleet**.

## The two findings

### Finding 1 (product defect): a cell's probe specs and fingerprints are frozen at announcer start

**Where:** `internal/vibe/modelprobe/hooks.go:18` (`SpecsFromDefs` runs
once, over a defs slice loaded once), reached from
`internal/vibe/daemon/announce.go:35` and
`internal/vibe/cli/cmd_fleet.go:239`. Both announcer shapes pass the same
startup snapshot into `fleetannounce.Config.Defs` as well.

**What C8 promises.** The phase doc §4 and AGENTS.md both say the
baseline is "keyed `(model, flags_sha256, metric)` **so a def edit starts
a fresh baseline instead of reporting a config change as a regression**".

**What happens.** fleetd re-reads the defs on every render pass
(`internal/vibe/fleetapi/render_loop.go:210`). A cell's announcer never
re-reads them. So on a fleet using C0's shipped `-watch-config` hot
reload — which is how the harness and the reference stack both apply a
render — a def edit takes effect in llama-swap immediately and is
invisible to the prober until its process restarts. Samples measured
under the NEW flags are scored against, and folded INTO, the OLD flags'
baseline.

**Repro** (`scripts/fleetlab/gate-c8-l5-staleflags.sh`, transcript
2026-08-05):

```
0. running argv: --threads 4          baselines: [{flags:3937ef13b048, n:8, verdict:ok}]
1. edit lab-embed-c.yaml --threads 4 -> 3, re-render, do NOT restart the announcer
   running argv now: --threads 3      <- llama-swap -watch-config applied it
   probe: {"value":22.96,"baseline_p50":29.01,"ratio":0.79,"verdict":"ok",
           "flags_sha256":"3937ef13b048..."}    <- same key, contaminated window
   baselines: [{flags:3937ef13b048, n:9, verdict:ok}]
2. restart the announcer. Same def, same running argv, nothing else changed.
   probe: {"value":22.96,"baseline_p50":null,"samples":null,"verdict":"unknown",
           "flags_sha256":"b998ecc3044a..."}    <- the fresh baseline, one restart late
   baselines: [{flags:3937ef13b048, n:9, verdict:ok},
               {flags:b998ecc3044a, n:1, verdict:unknown}]
```

**Severity.** A `--threads 4 -> 3` edit is a 21% slowdown, which the
hysteresis absorbs. A real one — `--n-gpu-layers` dropped, `--ctx-size`
raised, a quantisation swap — is exactly the 3x that flips `degraded`,
and C8's whole design is that the key stops a *configuration change*
being reported as a *regression*. It also runs the other way: the new
config's samples enter the old window and drag the median, so the
baseline an operator later compares against is a blend of two
configurations. Two smaller consequences share the cause — the announced
C3 `flags_sha256` is equally stale, so a def edit hot-applied at the CELL
raises a *spurious* fingerprint mismatch and, for a `fingerprint: strict`
def, excludes a correctly-serving model from the front render until the
announcer restarts.

**Not fixed here.** This phase changes no production code, and the fix is
a design question that belongs to whoever owns it: re-read defs per
announce (cheap, but a `LoadDefs` failure then needs C4's "a guard that
cannot be evaluated is a skip" treatment on a new path), or watch the
defs dir, or state plainly that a def change requires an announcer
restart — `vibe router render`'s "apply it:" line currently says
`systemctl --user restart llama-swap` and nothing about the announcer.
The gate row records the FAIL.

### Finding 2 (a documented number that is wrong): the probe-traffic envelope is a chat figure

C8's "Known and accepted" says probe traffic is "**Bounded by the budget:
≤ ~25 k tokens/cell/day at the cap, typically ~1 k**". Measured on the
harness:

| probe kind | tokens per probe | × the 96/day cap |
|---|---|---|
| chat (`chat/v1:64out`) | 101.7 (6 rows, alpha) | ~9.8 k/cell/day |
| embed (`embed/v1:64in`) | 768.0 (13 rows, charlie) | **~73.7 k/cell/day** |

The chat figure sits comfortably inside the stated bound. The embed one
is ~3x over it, and it is not an accident of the lab: `embedBatch = 64`
and `cannedEmbedInput` is a 14-word sentence
(`internal/vibe/modelprobe/modelprobe.go:77,482`), so ~768 prompt tokens
is what the shipped probe sends at any embedding model. The bound was
written from the chat probe and never revisited when the embed one
landed. Corrected in C8's own bullet as part of this pass; no code change
is implied — the budget is doing its job, the sentence describing it was
wrong.

## The gates that ran

Each is a standalone script beside `lab.sh`, per the harness README's
rule: drive the fleet through the CLI or the HTTP API and **print raw
evidence rather than a verdict**.

### C8 L4 — the 96/day cap (`gate-c8-l4.sh`)

The gate as written needs 24 h of wall clock at a 15-minute interval. A
time seam would have been a production change, so this run does the other
thing and **says so**: the 24 h window is **pre-seeded on the cell's own
state file** (`paths.CellProbeFile()`'s `attempts` array, which is what
`attemptsSinceLocked` reads), the announcer is restarted so the prober
loads it, and the real MCP verb then drives the real piggyback queue into
the real budget code. What this observes is the cap BOUNDARY and the
window's ROLL-OFF. It does not observe 24 h of scheduling and is not a
substitute for it.

```
1. seeded 96 attempts inside the 24h window
   probe -> "note":"daily cap reached (96 probes in the last 24h)"
   the previous measurement is carried forward (value 15.677, at 21:54:25Z)
   cell-side attempts after the refusal: 96   <- a refusal spends no budget
2. seeded 95 inside + 6 outside (25h-30h ago) = 101 rows on disk
   probe -> ALLOWED, real measurement {"value":15.677,"samples":4,"verdict":"unknown"}
   cell-side attempts after: 96               <- the 6 stale rows pruned; the window rolls
3. cooldown cleared on disk so the CAP answers, not the 5m gap
   probe -> refused; attempts still 96
4. what one probe costs the ledger: alpha/lab-chat, 101.7 tokens/probe
```

### C8 L5 — embed probe on a bge cell (`gate-c8-l5.sh`, `gate-c8-l5-staleflags.sh`)

The baseline half **passes**. Seven real 64-input batches at
`lab-embed-c` on charlie (a real bge-large-en-v1.5 under a real
llama-swap); kind read off the rendered argv as `embed`, metric
`embed_inputs_s`:

```
29.484, 28.684, 31.067, 30.303, 28.542, 29.222, 22.964 inputs/s
verdict `unknown` while the window was under 5, then:
{"value":28.542,"baseline_p50":29.484,"samples":5,"ratio":0.968,"verdict":"ok"}
{"value":29.222,"baseline_p50":29.145,"samples":6,"ratio":1.003,"verdict":"ok"}
```

The flag-change half **fails** — see finding 1. The gate is not badly
specified; the code does not do what the gate says.

One unexplained lab-side event, recorded so a later run does not have to
rediscover it: mid-way through the first attempt, charlie's
`llama-server` exited between requests (`[WARN] group: running
lab-embed-c exited: upstream exited unexpectedly`, once in the whole
session, no OOM in the kernel log, no coredump). Six consecutive 64-input
batches afterwards did not reproduce it. What the control plane did while
it was gone is worth keeping: every subsequent probe was refused with
*"lab-embed-c is not resident on charlie — a probe must not load it; warm
it first"*, and the previous measurement stayed visible. The load rule
held against a model that vanished underneath it.

### C9 14a — a real sink (`gate-c9-14a.sh`)

The webhook half passed in #34 against a local receiver; what was open
was whether a real ntfy topic accepts the payload vibe sends. It does. A
random public topic, a lab fleet, no house values:

```
$ vibe fleet notify test "C17 gate 14a: a real ntfy topic, a lab fleet"
queued; if it does not arrive, `vibe fleet notify status` has the delivery counters
$ curl 'https://ntfy.sh/<topic>/json?poll=1'
{"id":"QocB5QpIauI2","event":"message","topic":"vibe-fleetlab-...",
 "title":"vibe fleet: test","message":"C17 gate 14a: a real ntfy topic, a lab fleet",
 "tags":["vibe-fleet","explicit"],"priority":3}
```

C9 gate 8 (the URL is a credential) re-verified in the field at the same
time: `fleet_status` shows `https://ntfy.sh/... (id 8518d5e7)`, and the
topic appears **zero** times in the whole state document and **zero**
times in fleetd's log. **Still owed:** a phone with the ntfy app
subscribed — that is a device, not a process.

### C9 14d — a def edited on the front but not the cell (`gate-c9-14d.sh`)

The lab shares one backends dir between fleetd and its slim announcers,
so "edited on the front but not on the cell" could not exist there. The
script gives alpha its **own** config root first — which is what a real
cell has — and then edits only the front's copy. The `fingerprint_drift`
dwell is lowered 15m -> 30s through the ordinary `fleet.notify.dwell`
CONFIG key: what shrinks is the waiting, not the state machine.

```
22:56:43Z  mismatch appears: "lab-embed-a on alpha has served mismatched serving flags
           since 22:56:43Z (strict; expected 30c382205f86, got fd4b94606e78)"
22:56:53Z  alarm state `pending` (dwell running)
22:57:38Z  state `active`, fired_at stamped, sink line 0 -> 1, exactly once:
           Priority 4 · Tags vibe-fleet,firing,fingerprint_drift
           Title "fleet: alpha/lab-embed-a"
           ...no repeat over the next 2.5 minutes of continued drift
20:00:14   the def is pushed to the cell; state `clearing`
20:01:23   exactly one resolve: Priority 3 · Tags vibe-fleet,resolved,fingerprint_drift
           the alarms list empties. Two sink lines for the whole episode.
```

Run separately (it is C3's rule, reached by the same edit): the
`fingerprint: strict` def **left the front's rendered peers within 45 s**
of the drift and came back within 20 s of the def being reverted.

### C10 13d — two shells, a lease handshake (`gate-c10-13d.sh`)

Two genuinely separate processes.

```
A: vibe cell await bravo --model lab-embed-b --ready --idle 20s --unleased \
     --lease batchA --lease-ttl 90s
   -> unblocked at exactly 20 s: "bravo is up: lab-embed-b ready; idle 20s (>= 20s);
      no other leases" / "lease held: batchA on bravo/lab-embed-b for 1m30s"
B: the same flags, holder batchB, started while A's lease was live
   -> "await bravo: up; lab-embed-b ready; idle 20s (>= 20s); leased by batchA"
      blocked for the whole lease, unblocked 155 s in — 5 s (one poll) after A's
      lease expired — and took its own.
A again, mid-wait: returned in 0 s ("no other leases"), ignoring its OWN holder,
   and re-claimed — which is what moved B's unblock from 90 s to 155 s.
```

The gate as written says "`--idle --unleased`". It cannot be run that
way: `--lease` requires `--model`, and `--model` requires a condition
(`vibe: --model needs a condition: use --model <id> --ready`). The full
primitive is therefore `--model X --ready --idle D --unleased --lease H`,
and C10's own validation is why — both refusals fire before the wait
(`validateAwaitFlags`). The gate text is worth restating in that form.

### C11 L2 — a hold is not a pin (`gate-c11-l2.sh`)

The def's TTL was dropped to 45 s — an order of magnitude under the
20-minute hold — re-rendered, and applied by `-watch-config`.

```
challenger loaded; hold taken: "held lab-embed-a on alpha until 18:50 ADT (20m0s)
  not a pin: llama-swap's own TTL can still unload the model —
  the hold only stops fleetd causing it."
t+40s  running=[{lab-embed-a ready ttl:45}]  warm=skipped / held: lab-embed-a, 20m left
t+60s  running=[]                            warm=skipped / held: lab-embed-a, 19m left
       llama-swap: "[INFO] <lab-embed-a> Unloading model, TTL of 45s reached"
t+240s running=[]                            warm=skipped / held: lab-embed-a, 16m left
       queued piggyback commands for alpha: none
       front config sha256 identical before and after
--release -> waiting / "nothing resident (confirming)" x3 ticks, then
             holding / "restored (nothing resident)" — C11's empty-grace window, live
```

The cell sat with nothing resident for four minutes while fleetd declined
to fix it. That is the documented limitation behaving as documented.

### C11 L3 — inheritance, the schedule half (`gate-c11-l3.sh`)

#34 ran the probe half only. With a `* * * * *` warm schedule:

```
no hold      22:49:00 last_note "warmed" · 22:50:00 last_note "warmed"
hold taken   22:51, 22:52, 22:53 all arrive — last_fire FROZEN at 22:50:00 and
             last_note "skipped (held: lab-embed-a, 9m left (C17 C11 L3b) on alpha)"
--release    22:53:00 "warmed" · 22:54:00 "warmed"
```

The probe half re-ran in the same session: `No probe issued: alpha is
held: lab-chat, 7m left`.

### C12 L1 — the hallway test, DOM half (`marionette.py`)

The half #34 could not do needs a browser, and there is one: headless
Firefox driven over Marionette (a ~90-line stdlib-only Python client; no
new Go dependency, nothing in the module). The page's token lives in
`localStorage`, so the client seeds it and reloads. Real Gecko, real
`fetch`, real SSE.

```
GUEST TOKEN                              OPERATOR TOKEN
chip: block|"read-only"                  chip: display:none
buttons: []                              buttons: ["drain","resume","wake"] x4 cells,
                                                  + "warm", "away", "test"  (15)
no `savings` tab                         `savings` tab present
no warm row                              warm row + "warm goes through the front"
4 cell rows, live status, footer counters — both
```

And the "updates live" half, with the guest token loaded and **no
reload**, while an operator drained and resumed a cell from a shell:

```
before   bravo  SERVING
t+5s     bravo  INCONSISTENT  intent: C17 L1 live-update check (requested, awaiting ack)
t+35s    bravo  DRAINED       intent: ... last seen 4s ago
resume -> INCONSISTENT -> t+20s  bravo  SERVING
```

One consequence worth naming rather than discovering later: the guest
view still renders `notify: home · http://127.0.0.1:9724/... (id
4a75d5da)`. That is C12's stated rule working (the grant is a ROUTE
grant, so anything on the page is guest-visible) and the URL is redacted
— but "a guest sees the notify endpoint's redacted form" is a fact about
the hallway test nobody had looked at.

### C12 L3 — guest token rotation (`gate-c12-l3.sh`)

```
0. old token: state 200 · events 200 · usage 401 · /mcp 401
1. vibe token --guest --regenerate --yes -> new value on disk (0600), and the CLI says
   "# Restart the daemon for the new guest token to take effect."
2. BEFORE the restart: old=200 new=401    <- the running fleetd holds the old value
3. restart fleetd
4. old token: state 401 · events 401
5. new token: state 200 · usage 401 · /mcp 401 · X-Vibe-Auth: guest
6. counters: the rotated-out token's refusals landed in `auth_rejected` (+2), NOT in
   `guest_rejected`; the new token's refusals past its two routes landed in
   `guest_rejected` (+2).
```

Step 6 is the interesting one and it is correct: `guest_rejected` counts
401s from a **valid** guest token on a route it does not cover
(`internal/vibe/daemon/auth.go:149`), while a rotated-out value is a
stale token, which is exactly what `auth_rejected` means. The two
counters keep their meanings across a rotation.

### C13 bonus — defs parity, re-run after #36 (`gate-c13-parity.sh`)

#34 found `doctor.go` downgrading a KNOWN divergence from WARN to OK the
moment the diverged cell's checkout went dirty. #36 fixed it. The exact
sequence, re-run:

```
1. one clean SHA:            OK    "every reporting cell is at 515d1bd."
2. charlie one commit ahead: WARN  "515d1bd: alpha, bravo · fb82b92: charlie.
                                    fleetd's own def checkout is at 515d1bd."
3. dirty the diverged one:   WARN  (level unchanged) + " Working tree dirty (the SHA names
                                    the base commit, not what is running): charlie (fb82b92)."
4. control — agreed + dirty: OK    "every reporting cell is at 515d1bd. Working tree dirty..."
```

Dirty-and-diverged is now strictly worse than clean-and-diverged, and
dirty-and-agreed still reads OK with the dirt named. The inversion is
gone.

### C14 L3 — the operator at 23:29 (`gate-c14-l3.sh`)

Cron minutes are computed from the wall clock **in the fleet timezone**;
`cell_cmds.suspend` is the lab's no-op stub, so the DECISION is
observable without suspending a workstation.

```
21:51:00Z  the declared minute, with a real request 24 s old:
           state `deferred`, "cell bravo served a request 24s ago (quiet window 5m0s)"
           next_suspend rolls to 2026-08-06T21:51:00Z; deferred_since stamped
...the session continues, one request a minute, and the age is re-derived every tick:
           24s -> 1m24s -> 2m24s -> 3m24s -> ...
21:59:00Z  5m24s after the last request — the first minute tick past the quiet window:
           cell-verbs.log: "2026-08-05T18:59:00-03:00 fake-suspend bravo"
           sleep entry: state `asleep`, detail "suspended", last_suspend stamped
           intent: {"state":"drained","reason":"asleep per sleep_schedule","eta":"18:14"}
           the CELL's own cell-intent.json: {"state":"drained","since":"...21:59:00.004151695Z"}
           — the same nanosecond, which is C14's "CellSuspend stamps the cell's own
           intent before it freezes" rule, observed
```

Two lab artifacts, stated so nobody reads them as defects. The display is
`INCONSISTENT`, not `OFF`: the stub suspend leaves bravo running and
announcing, so availability evidence contradicts the declared drain and
C3's "evidence over declaration" rule does exactly what it should. On
metal the box is gone and it renders OFF. And the first attempt at this
gate declared its cron in the LOCAL zone rather than the fleet's, so
`next_suspend` resolved an hour late and nothing fired for eleven
minutes — which C4's "every schedule's resolved `next_fire` shows in
`fleet_status` so a wrong zone is visible" caught on sight. The rule
works; the harness operator was the one who needed it.

### C14 L4 — the mid-batch night (`gate-c14-l4.sh`)

`max_defer` is 4m rather than a whole night. The deferral is a
once-a-minute re-evaluation, so what "all night" adds is repetitions of a
decision this run watches four times — that substitution is the one thing
the gate does not prove.

```
22:01:03Z  vibe cell await bravo --model lab-embed-b --ready --lease overnight-batch
           -> "lease held: overnight-batch on bravo/lab-embed-b for 30m0s"
           the box then goes quiet; no requests from here
22:04:00Z  the declared minute: state `deferred`, "1 active leases on bravo"
           (from 22:06 the quiet window was satisfied too, so the lease was the ONLY
            blocker for the last two minutes — the detail never changed)
22:08:00Z  max_defer: state `skipped`,
           "abandoned after the defer window (1 active leases on bravo)"
           suspend verb log: 0 bytes · intent: null · display: SERVING
           two further minutes: no return, nothing fired
```

### C7a 2 and 5 — the two ledger PARTIALs (`gate-c7a-partials.sh`)

Gate 2's live half was PARTIAL because "the harness front is a bare
llama-swap with no collector, so the skip-by-name guard was proved by
outcome rather than by driving front-collected rows into the fold". So
this run **commissions a collector on the front** — a slim
`vibe fleet announce --cell front` against the front's own llama-swap —
and drives ten requests client -> front -> alpha:

```
the front cell's own collector, cumulative, on the wire:
  {"last_row_id":14,"models":[{"model":"lab-chat","basis":"chat","req":10,
    "in_fresh":48,"in_cached":342,"out":40,"poke_req":3,"err_req":1}]}
buckets in /api/fleet/usage attributed to the front: 0
usage.jsonl lines carrying cell=front: 0  (of 35 lines)
alpha's row, where those 10 requests were actually served: req 19
```

Non-zero front rows arrived at the fold and were refused by name. That is
the half the unit test was standing in for.

Gate 5's two unexercised cases:

```
a poke that is NOT one token
  max_tokens=1 -> completion 1 -> poke_req 4->5, req unchanged 19, out unchanged 496
  max_tokens=6 -> completion 6 -> req 19->20, out 496->502, in_fresh +9, poke unchanged
  (llama-swap rows behind it: id 26 out=1, id 27 out=6)
probe traffic as a second self-traffic producer
  6 chat probe rows on alpha at 101.7 tokens each; 13 embed probe rows on charlie at
  768.0 each; none classified poke_req — they land in `req`, exactly as C8's
  "Known and accepted" says. (And see finding 2 for what that costs.)
```

## What is still owed, and the physical fact each one needs

"Needs hardware" is not an answer. Each row names the specific physical
fact, and nothing here is a scheduling problem dressed up as a physical
one.

| gate | the physical fact it needs |
|---|---|
| C7a 1 — 24 h store soak | 24 h of wall clock. Nothing else: the compressed run (1200 rows, nothing pruned, survived a restart) already proved the mechanism. The harness can run it unattended. |
| C7a 4 — the cancelled-stream branch | A model server that omits `timings` on an aborted stream. This llama-server build reports them anyway, so a SIGKILLed client still meters normally; the branch needs an mlx cell, which is a second machine of a different architecture. |
| C7b 9 — is the savings number believable | A week of real traffic on cells whose watts are **measured** rather than declared, priced against a real open-weight twin. The lab serves CPU bge embeddings: there is no meaningful twin for them, and a synthetic `watts_idle` prices a fiction. |
| C8 L2 — spill-induced degradation | A GPU under real VRAM pressure. #34 substituted a `SIGSTOP` duty-cycle throttle, which proves the scorer, not the cause an operator will actually hit. |
| C8 L4 — 24 h of scheduling | 24 h of wall clock at a 15-minute interval on two cells. The cap BOUNDARY is now proven (above); a day of the scheduler asking is not. |
| C8 L5 — the flag-change half | Nothing physical. It FAILS on the harness today — finding 1. |
| C10 13a — cold-start magnitude | A model whose ready transition takes 6–10 minutes: a 30B+ on a GPU with cold page cache. The harness model is ready in ~25 s, so the semantics are proven and the magnitude is not. |
| C12 L1 — the phone | Nothing. The DOM half is closed with a real browser engine; a phone was always convenient rather than required, and the remaining delta is screen size. |
| C13 L4 — the fire drill | A physical box to reboot, and a real NIC to receive a magic packet. `wake.configured` is a configuration check precisely because arming is not observable from here. |
| C14 L1 — one real night | A box that actually enters S3 and returns, and a wattmeter on its cord. The *schedule* half is now closed (L3). |
| C14 L2 — the wake | A magic packet on a real NIC reaching a powered-off machine, and firmware that honours it. Loopback has nothing to wake. |
| C14 L5 — the wake that fails | A BIOS switch to disarm WoL, and a night to fail across. |
| C14 L6 — the quarterly drill | A real box to suspend and wake, end to end, from a phone. |
| C9 14a — the phone half | A device with the ntfy app subscribed to the topic. The topic itself now demonstrably accepts the payload. |

## Gates on this phase

This phase ships no Go code, so its own gates are about not breaking
anything and about the honesty of what it wrote.

| gate | result |
|---|---|
| G1. No production code changes | **PASS** — `git diff main..HEAD --stat -- internal/ cmd/ proto/` is empty. |
| G2. Streaming contract | **PASS** — `git diff --stat main..HEAD -- internal/vibe/proxy` is empty (vacuously, by G1). |
| G3. No new dependencies | **PASS** — `go mod tidy` leaves `go.mod`/`go.sum` byte-identical; the browser client is stdlib Python driving a browser that is not in the module. |
| G4. Full inner loop | **PASS** — `go build ./...`, `go vet ./...`, `go test -race -timeout 240s ./...`, `gofmt -l .` silent, `go mod tidy` clean, `golangci-lint run` (v2.12.2) 0 issues. |
| G5. Every gate row this phase touched names how it was run | **PASS** — each edited row carries the script name and the date, and every substitution (seeded budget window, shortened dwell, shortened `max_defer`, stub suspend verb, CPU models) is named in the row itself, not only here. |
| G6. Shared files untouched | **PASS** — `AGENTS.md`, `docs/design/fleet-control-plan/README.md` and `docs/design/fleet-control.md` are unmodified on this branch; everything that belongs in them is in the next section. |

## For the reconciliation pass

Three shared files are off-limits to this branch (they are wave 1's
conflict axis). Here is exactly what belongs in each.

### `AGENTS.md`

1. **In the C8 section**, after the baseline bullet, add finding 1 as a
   limitation:

   > - **A cell's probe specs and announced fingerprints are frozen at
   >   announcer start** (`modelprobe.Hooks` gets a defs slice loaded once,
   >   in `daemon/announce.go` and `cli/cmd_fleet.go`). fleetd re-reads
   >   defs every render pass; a cell never does. Under C0's
   >   `-watch-config` hot reload a def edit therefore takes effect in
   >   llama-swap while the baseline key and the announced `flags_sha256`
   >   still describe the old argv — so the new configuration's samples are
   >   scored against, and folded into, the old configuration's baseline,
   >   and a hot-applied edit at the cell raises a spurious fingerprint
   >   mismatch. Restarting the announcer is the only thing that rebinds
   >   either. Verified 2026-08-05 on the harness
   >   (`scripts/fleetlab/gate-c8-l5-staleflags.sh`).

2. **In "Test doubles and upstream contracts"**, beside "The local rigs
   stay":

   > `scripts/fleetlab/gate-*.sh` are the per-gate rigs (one per phase
   > gate, sourcing `gl.sh`); `marionette.py` drives a headless Firefox for
   > the one gate that needs a DOM. They print raw evidence, never a
   > verdict — a rig that prints PASS is a rig that can print PASS while
   > wrong.

3. **Nothing else.** C17 adds no package, no route and no invariant.

### `docs/design/fleet-control-plan/README.md`

1. **Status column**, replacing each live-gate clause:
   - C7a: `live halves of 2, 3, 5, 6, 8 **PASS**, 1 and 4 PARTIAL (harness)`
   - C8: `**L1-L4 PASS** and L5's baseline half PASS (harness); **L5's flag-change half FAIL** — see C17 finding 1`
   - C9: `**14b-14d PASS** + 3 bonus gates (harness); 14a PASS bar the phone`
   - C10: `**13b, 13d PASS**, 13a PARTIAL, 13c **VOID**`
   - C11: `**L1-L4 PASS** (harness)`
   - C12: `**L1-L3 PASS** (L1's DOM half via headless Firefox)`
   - C13: `**L1-L3 + defs-parity PASS** (harness), L4 PARTIAL (WoL needs metal)`
   - C14: `**L3, L4 PASS** (harness); L1, L2, L5, L6 need metal`
2. **A C17 row** in the phase table:

   `| [C17](c17-gate-closure.md) | Gate closure: run the gates that were never attempted | 0 lines (14 gate scripts) | C7a-C14 | PR open; 14 gate rows moved; 2 findings |`

3. **The paragraph after the table** ("What still genuinely needs metal,
   in full") is superseded by C17's
   [owed table](#what-is-still-owed-and-the-physical-fact-each-one-needs):
   C12 L1 is off the list, C8 L4's cap is proven, C14 L3/L4 are done, and
   C8 L5 moved from "unrun" to "fails".
4. **A ground-rule amendment worth making explicit** in rule 10: *"not
   attempted" and "not possible" are different statuses and must never
   share a heading.* This plan has now written the conflated version
   twice — #34, and the eleven phase docs #34 itself corrected.

### `docs/design/fleet-control.md`

Nothing is owed. C17 changes no design decision. If its roadmap section
tracks gate state, it should pick up the same status strings as the plan
README above.
