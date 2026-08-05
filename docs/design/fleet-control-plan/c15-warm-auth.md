# C15 — the warm credential

Status: **PR OPEN** ([#38](https://github.com/gallowaysoftware/vibe/pull/38),
2026-08-05), off `feat/c15-warm-auth` branched from `main` at `0c275fd`.
Four commits: the feature, `fleet.front_extras` (the render half — see
§6, it is not optional scope), ground rule 9's adversarial self-review
commit (two findings, one of them an eighth producer living outside
fleetd), and a post-CI fix (the page's credential line, and the AST
scans off the deprecated `parser.ParseDir`). Every production predicate
in the phase is **mutation-verified**: 24 reverts, each confirmed to turn
a named test red and then restored. Unit gates U1–U12 green on a full
local inner loop (`go build`, `go vet`,
`go test -race -timeout 300s ./...` repeated, `gofmt -l .` silent,
`go mod tidy` clean, `golangci-lint run` 0 issues **at CI's v2.12.2** —
the first push failed CI on a rule the locally-installed v2.0.0 did not
have, so the local linter was upgraded to match the gate; a stale
linter is a gate you are not actually running).

**The live gate PASSED on 2026-08-05** against two real llama-swap v239
processes with `apiKeys` set, a real fleetd, a real announcer and a real
CPU model — the fleet the reference stack is not. Raw transcript in
[Execution](#execution).

## The defect

Recorded at [c5-land-c4.md:784](c5-land-c4.md), and confirmed by grep
(there was no `Authorization` header anywhere in the warm path):

> **`warmViaFront` sends no Authorization header** (known limit, C4). If
> the front llama-swap is configured with `apiKeys`, every warm — target
> and schedule alike — fails with a 401 recorded as `warm failed: ...
> HTTP 401`. The reference front has no `apiKeys`, which is why the C4
> live gate passed. Out of scope to fix here: it needs a front credential
> in `hosts.yaml`, which is config-surface design, not a review fix.

That config-surface design is this phase. It turned out to be a bigger
hole than the note describes, and the measurement is what showed it.

### What llama-swap's `apiKeys` actually gates

Measured against a real `llama-swap v239 (dd81801)` binary before any of
this was designed, because the whole phase rests on it:

| route | no key | `Authorization: Bearer` | `X-Api-Key` |
|---|---|---|---|
| `/health` | **200** | 200 | 200 |
| `/v1/models` | 401 | 200 | 200 |
| `/running` | 401 | 200 | 200 |
| `/api/events` | 401 | 200 | 200 |
| `/api/metrics/activity` | 401 | 200 | 200 |
| `/api/models/unload/<id>` | 401 | 200 | — |
| `/v1/chat/completions` | 401 | 200 | — |
| `/ui/` | 401 | 200 | 200 |

A **wrong** key gets 401 too, never 403, and the body is
`{"error":"unauthorized: invalid or missing API key","src":"llama-swap"}`
— which names no configuration at all. `/health` is the ONE exemption,
which C0's gate had already recorded and which matters here: a cell can
pass a health check while refusing every route fleetd uses.

So the C5 note understates it. Against a keyed front, a pre-C15 fleetd
loses:

- every warm (the recorded defect),
- **every probe** — `/running` and `/v1/models`, so the cell reads
  `reachable: false` and displays `OFF/AWAY?` while it is serving,
- **the whole `/api/events` stream** — and with it the in-flight
  evidence, the per-model activity stamps, and every idle window built on
  them: C4's warm policy, C10's `await --idle`, C14's quiet window,
  C8's busy guard,
- the front catalog check `warm_model` does before warming,
- `unload_model`,
- C7b's actual-cloud-spend tail of the front's activity log.

Only the announce path survives, because cells dial OUT. The fleet would
look like a fleet where every cell is off and the operator's own verbs
fail, with one 401 in a log to explain it.

`deploy/front/README.md:73` records that `apiKeys` is a supported and
documented posture for this stack; the reference deployment happens to
run without it, which is exactly why nothing noticed for eleven phases.

## What this is not

- **Not TLS, not a CA, not mTLS.** llama-swap speaks one credential and
  it is a bearer string. Transport security on the LAN stays the reverse
  proxy's job (C13's `tls.not_after` reads those certs).
- **Not a new auth mechanism for the vibe control plane.** fleetd's own
  bearer token (C1) and the guest token (C12) are untouched; this is the
  third credential in the fleet and it authenticates a different server.
- **Not the CELL-side half.** The announcer, C8's prober and the cell's
  own usage collector all dial their OWN localhost llama-swap. See
  [§8](#8-what-this-phase-does-not-cover-the-cell-side-dialers) — it is a
  different config surface, it is named, and its worst failure mode is
  written down.
- **Not a change to the data plane.** `internal/vibe/proxy` has an empty
  diff. Clients keep presenting whatever credential they already present;
  nothing about the streaming path moves.

## Design

### 1. Where the credential lives

`hosts.yaml` gains one per-cell field:

```yaml
cells:
  front:
    url: "http://front.example:9000"
    class: always_on
    swap_key_file: /etc/vibe/front-swap.key   # example path; the VALUE lives in the private fleet repo
```

**Why not reuse `token_file`.** The obvious shortcut — "the front cell's
`token_file`, used as a bearer for data-plane calls" — is wrong, and the
front is the case that settles it:

- They authenticate **different servers**. `token_file` is the cell's
  *vibe daemon* control plane (`:9001`), which can drain, resume and
  suspend the box. `swap_key_file` is *llama-swap*'s own OpenAI + admin
  surface on the model port. Handing the drain-capable token to
  llama-swap puts it in a process that logs request metadata, for no
  gain.
- They are **minted by different things** and rotate on different
  schedules: vibe mints the daemon token (C1's CREATED-vs-loaded line);
  the operator writes the llama-swap key into llama-swap's own config.
- **The front has no `token_file` to reuse.** In the reference
  deployment it is a llama-swap container with *no vibe daemon at all* —
  C13's `auth.inbound`/`auth.outbound` checks both special-case it for
  exactly this reason. Reusing `token_file` would mean inventing a daemon
  credential for a box with no daemon, on the one cell every warm goes
  through.

**Why per-cell rather than one fleet-level key.** Cells are keyed
independently in practice (the front is the LAN-facing one; a cell behind
it may or may not be), and a fleet that uses one shared key just points
every cell's `swap_key_file` at the same path. The narrower surface costs
nothing and the wider one cannot express the common case.

**Boundary rule.** The path is config and lives here; the value lives in
the private fleet repo. Same shape as `token_file` and C9's
`notify.url_file`.

### 2. Resolution, and failing closed

`fleetcfg.SwapCredentialFor(cell) (SwapCredential, error)`:

- No `swap_key_file` (or no `hosts.yaml`, or an unknown cell) →
  `{Configured: false}`, **nil error**. llama-swap's default is no auth,
  that is the reference posture, and a single-box daemon has no
  `hosts.yaml` at all.
- Declared and readable → `{Key, Configured: true, Source: "cells.X.swap_key_file (path)"}`.
  `Source` names the ORIGIN, never the value — C13's `Credential.Source`
  discipline.
- Declared and unreadable / empty / carrying a control character →
  **typed error, and the caller sends nothing**. The control-character
  check exists because net/http rejects such a header with "invalid
  header field value", naming no config; the error reports the offending
  byte by POSITION, since printing it would print part of the key.

Read at USE, never cached: rotating a key needs no fleetd restart, and a
64-byte file read costs microseconds against a 15-second tick.

### 3. Every producer, and how that is enforced

Seven producers reach a llama-swap from fleetd. All seven now go through
**one** authorizer, `fleetapi.AuthorizeSwap(req, cell)` (exported because
fleetmcp owns three of them and a second resolver is how two copies
drift):

| producer | call | cell whose key |
|---|---|---|
| `fleetapi.warmViaFront` | `POST /v1/chat/completions` | `front` |
| `fleetapi.getJSON` (snapshot) | `GET /running`, `GET /v1/models` | the cell probed |
| `fleetapi.streamCell` | `GET /api/events` | the cell watched |
| `fleetmcp.toolWarmModel` | `POST /v1/chat/completions` | `front` |
| `fleetmcp.modelInCatalog` | `GET /v1/models` | `front` |
| `fleetmcp.toolUnloadModel` | `POST /api/models/unload/<id>` | the target cell |
| `daemon.cloudSpendPoller` → `usagemeter` | `GET /api/metrics/activity` | `front` |

`warmViaFront` is a method on `*Server` now; the three warm loops
(targets, schedules, C14's post-wake warms) get it as their default
`warmFn`, so all three are covered by the one change.

**"A guard in one of four call paths is not a guard" is enforced
structurally.** `TestEveryLlamaSwapRequestIsAuthorized` (one in
`fleetapi`, one in `fleetmcp`) walks the package AST: every function that
builds an `http.NewRequest*` must also call the authorizer, and the test
fails if the scan finds no builders at all — an inert scan passes on an
empty package. Verified by deleting the authorizer call from `streamCell`
and watching it name `watcher.go: streamCell`.

The self-review found an **eighth** producer that this scan cannot see
because it does not live in fleetd: `cli.probeCellDirect`, the DEGRADED
fallback behind `vibe cell status` and the client half of
`vibe fleet doctor`. See [§7](#7-the-eighth-producer).

### 4. The three failure modes, which are three different sentences

Before this phase the whole space was one string: `warm failed: ... HTTP
401`. It is now three, because the fix differs:

| kind | what happened | verdict |
|---|---|---|
| `unauthorized` | the llama-swap demands a key; hosts.yaml declares none | a MISCONFIGURATION |
| `rejected` | fleetd presented the declared key; llama-swap refused it | a ROTATION (or a key that never matched) |
| `unresolvable` | `swap_key_file` is declared but will not read | local, fleetd's side, no request sent |

Each is recorded per cell in `Server.swapAuth`, surfaces in
`fleet_status.swap_auth` and in the doctor's `swap.credential` check, and
is logged ONCE per transition (not once per tick — this is reached from a
15s ticker and a 3s probe round).

`unresolvable` is additionally **loud at config load**: fleetd resolves
every declared key at startup (`daemon.checkSwapKeys`) and logs
`slog.Error` per broken one, naming the file and what it breaks. It is
deliberately NOT fatal — a fleetd that refuses to start over one cell's
missing key file takes the whole control plane down for a box that may be
switched off — and every affected call fails closed and visibly anyway.
Same posture as C12's guest token: fail closed, stay up, say so. The
other half of the question ("does this llama-swap DEMAND a key?") is not
knowable at config load; it arrives on the first probe round and lands in
the same two surfaces.

**The credential value never travels.** Not in a log line, not in an
error string, not in `fleet_status`, not in an event payload, not on the
page. `swap_auth` carries the failure KIND and a sentence naming
`cells.<name>.swap_key_file` — the config key, not the path, because that
document is guest-readable (C12: a route grant is not a field grant). The
PATH appears only in the doctor report and the daemon log, both
token-only. `TestSwapKeyNeverAppearsInAnySurface` marshals the state
document, the doctor report and the warm error and greps for the key —
and then asserts the diagnosis is still *useful*, because a redaction
that redacts everything passes the first half while telling the operator
nothing.

### 5. No silent retry loop against a 401

This is the rule the phase brief singles out, and it is not theoretical:
the warm-target restore fires from a 15-second ticker whose precondition
("the default is not resident") a 401 can never satisfy. Unguarded, a
misconfigured fleet answers with **5,760 identical 401s a day**, forever,
with no other symptom.

`SwapAuthRefusal(cell)` is the suppression: while a cell has a credential
failure recorded, automated calls to its llama-swap are skipped with the
diagnosis as their reason, re-arming after `swapAuthRetry` (5 min) so a
fixed key file recovers **without a fleetd restart**. In practice recovery
is faster than that: the next `/api/fleet/state` probe round presents the
new key, gets a 200, and `NoteSwapStatus` clears the record — any status
other than 401/403 clears it, because a far side that answered anything
else accepted the credential.

Three placement rules:

- **It is a rung in `evalWarmTarget`, not only a check inside
  `restore`.** Checked only at fire time it left `emptySince` cleared on
  every skip (C11's "a SKIP is not an observation of emptiness"), so the
  warm row alternated between `waiting (nothing resident (confirming))`
  and `skipped` on successive ticks and buried the reason. The ladder is
  now **drained > held > swap-credential > evidence**: the two
  declarations outrank it, because a drained or held cell would not be
  warmed with a working credential either, and naming the front's key on
  a box the operator took for gaming is the wrong sentence.
- **It is ALSO checked at fire time**, the shape `warmClassRefusal`
  uses: the rung keeps the status honest, the fire-time check makes the
  rule hold for any producer that reaches `restore` another way. Both
  halves are pinned by separate tests, because a fix with no test of its
  own is how C5's addendum says this repo loses guards.
- **It never gates an operator's verb.** `warm_model` fires regardless
  and hands back the diagnosis from its synchronous catalog read. An
  operator asking is not fleetd guessing, and the operator is the one
  person who can go and fix the file.

**A 401 is never queued to the piggyback.** C6's rule (a 4xx is the far
side answering) already covers it, and the typed `*warmHTTPError`
survives the new wrapping so `definitiveWarmRefusal` still sees it. The
tempting alternative — "the front refused us, so send the warm to the
cell's own llama-swap on the next announce" — is rejected by name: it
routes around a broken credential, makes the fleet look healthy while its
config is wrong, and the cell may be keyed too.

The probe and watcher paths are deliberately NOT suppressed. They are the
evidence path; refusing to probe would freeze the display at the moment
it matters, and the watcher already backs off to 30 s on its own.

### 6. `fleet.front_extras` — the render must not delete the credential

Found while building the live gate, and it is why the credential alone
would not have worked.

The front's llama-swap config is a **derived artifact**: fleetd rewrites
it on every membership transition (C3's render loop), from the backend
defs alone. The renderer emits nothing it did not derive — so a front
configured with `apiKeys:` loses them at the next presence change. Hours
after someone set the credential up, with no configuration having
changed, every warm starts failing. A front credential fleetd erases is
not a credential.

So `fleet.front_extras` names a YAML file whose top-level sections merge
into every render — the same merge `vibe router render --extras` already
performs, plumbed through `RenderLoopConfig.FrontExtras` into
`router.Options.ExtrasPath`. `apiKeys:` is the motivating case; `store:`
(C7a's activity log, which the front also needs and the renderer also
does not emit) has exactly the same shape.

The doctor's new `front.extras` check is a **FAIL** for precisely that
trap: a front that declares a `swap_key_file`, a fleetd that renders its
config, and no `front_extras`. It is OK-with-a-reason in the two
configurations that are fine (no render mount; no key declared), because
a permanent WARN on a healthy fleet is how an operator learns to ignore a
level (C13).

### 6b. The page

A credential-suppressed warm renders as `skipped` like every other skip,
and the page is the surface an operator watches. The status strip now
carries `llama-swap credential: <cell> <kind>` — the cell and the KIND,
not the detail sentence (it names `cells.<name>.swap_key_file`, and the
state document is guest-readable), and obviously not the key. The
sentence stays on the token-only surfaces: `fleet_status` and
`vibe fleet doctor`. No new route, no new fetch — the field rides the
state document the page already polls (C7b's rule).

### 7. The eighth producer

`cli.probeCellDirect` backs two surfaces, both reached exactly when
fleetd is already broken: `vibe cell status`'s DEGRADED fallback and
`vibe fleet doctor`'s client-side `cell.direct` check. Both dialled
`/v1/models` unauthenticated, so on a keyed fleet they reported **every
cell as down** — absent evidence read as a fact, on the screen someone is
reading mid-incident.

It now resolves the cell's key from the `hosts.yaml` it already loaded,
fails closed on an unreadable key file, and reports a refused credential
as its own state: `auth?` in the status table (with the reason in the
models column) and a doctor **FAIL** saying "llama-swap answers but
refuses this box's credential". A cell that answered is not a cell that
is down, and the two have completely different fixes.

### 8. What this phase does NOT cover: the cell-side dialers

Three components dial their OWN localhost llama-swap and are NOT covered:

- `fleetannounce` — `GET /running`, `GET /v1/models` per heartbeat, plus
  the piggyback `warm` and `unload` verbs it executes;
- `modelprobe` (C8's cell-side prober) — `GET /running` and the probe
  completion;
- `usagemeter`'s cell-side collector (C7a) — `GET /api/metrics/activity`.

They are excluded because they are a **different config surface**: they
run on the cell, take `--llama-swap <url>` on the CLI, and a slim
announcer's box may hold no `hosts.yaml` at all. `usagemeter.Config`
already grew the `APIKeyFile` field here (fleetd's cloud-spend tail needs
it), so the cell-side half of that one is a two-line wiring change
whenever it is wanted.

**The hazard, written down so the next agent does not have to find it.**
`fleetannounce.announceOnce` treats a `gatherModels` failure as
`models = nil` — deliberately, because "the unit is stopped reads as an
empty model list". A 401 from the cell's own llama-swap is NOT the unit
being stopped, but it presents identically, and an empty model list is
what C4's warm policy reads as *nothing resident* → restore. So a cell
keyed without a cell-side credential would announce an empty catalog
forever and invite a warm on every grace window. Fixing it properly needs
a wire distinction between "no models" and "could not read models", which
is a phase, not a line. Recorded in
[fleet-control-futures.md](../fleet-control-futures.md)'s territory —
see [For the reconciliation pass](#for-the-reconciliation-pass).

## Files

| file | change |
|---|---|
| `internal/vibe/fleetcfg/fleetcfg.go` | `Cell.SwapKeyFile`, `SwapCredential`, `SwapCredentialFor`, tilde expansion |
| `internal/vibe/fleetapi/swapauth.go` | **new** — the authorizer, the three failure kinds, the sticky refusal, the status block |
| `internal/vibe/fleetapi/fleetapi.go` | `getJSON` takes a cell; `swapAuth` map; `SwapAuth` on `StateSnapshot` (built after the probe round) |
| `internal/vibe/fleetapi/watcher.go` | `/api/events` carries the credential |
| `internal/vibe/fleetapi/warmtarget.go` | `warmViaFront` is a method and authenticates; the ladder rung; `swapWarmError` |
| `internal/vibe/fleetapi/warmsched.go` | the schedule's guard rung |
| `internal/vibe/fleetapi/sleepsched.go` | the post-wake warm's rung |
| `internal/vibe/fleetapi/render_loop.go` | `FrontExtras` → `router.Options.ExtrasPath` |
| `internal/vibe/fleetapi/doctor.go` | `swap.credential` (per cell), `front.extras`, `DoctorHost.FrontExtras` |
| `internal/vibe/fleetmcp/fleetmcp.go` | warm_model (both halves), the catalog check, unload_model |
| `internal/vibe/usagemeter/usagemeter.go` | `Config.APIKeyFile` + `authorize` |
| `internal/vibe/daemon/swapkey.go` | **new** — the loud config-load resolution pass |
| `internal/vibe/daemon/daemon.go` | `fleet.front_extras`; `checkSwapKeys` at fleetd startup |
| `internal/vibe/daemon/cloudspend.go` | the front's key into the collector |
| `internal/vibe/daemon/doctor.go` | `FrontExtras` into `DoctorHost` |
| `internal/vibe/cli/cmd_cell.go` | `probeCellDirect` authenticates; `auth?` state |
| `internal/vibe/cli/cmd_fleet_doctor.go` | `cell.direct` distinguishes refused-credential from no-answer |
| `internal/vibe/fleetapi/fleet.html` | one status-strip line: cell + failure kind, no sentence, no key |
| `scripts/fleetlab/gate-c15-warm-auth.sh` | **new** — the live gate rig |

`internal/vibe/proxy` diff: **empty**. `go.mod`/`go.sum`: **byte-identical**.

## Acceptance gates

### Unit

| # | gate | result |
|---|---|---|
| U1 | A warm through a keyed front carries `Authorization: Bearer <key>`; an unkeyed fleet sends no header at all | PASS |
| U2 | A 401 with no key declared and a 401 with a declared key produce DIFFERENT sentences, and each records its own kind | PASS |
| U3 | A 401 stays a definitive refusal and is never queued to the piggyback | PASS |
| U4 | A declared-but-unreadable key file sends ZERO requests, names the file, and records `unresolvable`; empty and control-character keys are refused by the resolver without leaking bytes | PASS |
| U5 | The warm-target loop sends exactly ONE warm at a front that 401s, then holds `skipped` with the diagnosis across 50 more ticks, and stamps no `last_restore` | PASS |
| U6 | The suppression re-arms after `swapAuthRetry`, `Since` survives a repeat, and any non-401 status clears the record (including a 404) | PASS |
| U7 | The warm schedule and C14's post-wake warm both skip with the diagnosis; each rung pinned separately, plus the fire-time rung inside `restore` | PASS |
| U8 | Snapshot probes and the events stream carry the credential; a 401 produces a `swap_auth` row in the SAME state document that reports the cell unreachable | PASS |
| U9 | The key value appears in no state document, no doctor report and no error string — and the diagnosis is still useful | PASS |
| U10 | Every `http.NewRequest*` in `fleetapi` and in `fleetmcp` calls the authorizer (AST scan; fails on an empty scan) | PASS |
| U11 | `swap.credential` is FAIL / FAIL / FAIL / OK / OK / UNKNOWN across the six configurations; `front.extras` FAILs exactly on the erase-the-key trap; the render is given the extras path | PASS |
| U12 | The fleet page renders the cell and the failure KIND, and NOT the detail sentence (which names the config path) | PASS |

Plus 24 mutation checks — each production predicate reverted, a named
test confirmed red, the line restored. The two that mattered most:
deleting the authorizer from `streamCell` is caught by the AST scan by
NAME, and dropping `ExtrasPath` from the render loop turns
`TestFrontRenderPreservesTheOperatorsExtras` red.

### Live

L1 (**PASS**, 2026-08-05) — the whole point of the phase, run twice on a
purpose-built rig with `apiKeys` on a real front. See below.

## Execution

### Live gate: `scripts/fleetlab/gate-c15-warm-auth.sh`

`scripts/fleetlab/lab.sh`'s four cells hold ports 9640–9653 and a second
lab instance was already running on this box, so this gate is a
standalone rig on 9660–9671 with the same isolation discipline (scratch
XDG triple, `CUDA_VISIBLE_DEVICES=""`, kill patterns anchored on its own
path — a production llama-swap on `:9000` and the vibe daemon on `:9001`
are untouchable). It stands: two real `llama-swap v239` processes (a
peers-only front with `apiKeys`, plus one model cell serving a real CPU
7B), a real fleetd with a warm target AND a per-minute warm schedule, and
a real `vibe fleet announce` loop. It runs the fleet twice — no
credential declared, then declared — and prints raw evidence rather than
a verdict.

**HALF 1 — the front runs with `apiKeys`, `hosts.yaml` declares no key:**

```
"warm": {
  "targets": [{ "cell": "heavy", "model": "lab-chat", "state": "skipped",
    "detail": "front's llama-swap requires an API key (HTTP 401) and hosts.yaml declares no
               cells.front.swap_key_file: warms, catalog reads, /running probes and the events
               stream to it all fail until one is set" }],
  "schedule": [{ "cron": "*/1 * * * *", "model": "lab-chat",
    "next_fire": "2026-08-05T21:57:00Z",
    "last_note": "skipped (front's llama-swap requires an API key (HTTP 401) and hosts.yaml
                  declares no cells.front.swap_key_file: ...)" }]
},
"swap_auth": { "cells": [{ "cell": "front", "kind": "unauthorized",
    "since": "2026-08-05T21:55:35Z", "retry_after": "2026-08-05T22:01:55Z",
    "detail": "..." }] },
"cells": [ { "name": "front", "reachable": false, "display": "OFF/AWAY?", "models": [] },
           { "name": "heavy", "reachable": true,  "display": "SERVING", "models": ["lab-chat"] } ]

doctor:
{"id":"swap.credential","cell":"front","level":"fail",
 "summary":"this cell's llama-swap is refusing fleetd's credential (unauthorized, since 1m ago)"}
{"id":"swap.credential","cell":"heavy","level":"ok",
 "summary":"no llama-swap API key declared, and none demanded"}

hand warm, no credential   -> HTTP 401
hand warm, with the key    -> HTTP 200
```

The front reading `OFF/AWAY?` while serving is the wider hole in one
line: the probes are 401ing too. The two hand warms are the control —
the front really is demanding a key, and the model really does load.

**HALF 2 — the same fleet, `cells.front.swap_key_file` declared:**

```
"warm": {
  "targets": [{ "cell": "heavy", "model": "lab-chat", "state": "holding",
                "detail": "target resident" }],
  "schedule": [{ "cron": "*/1 * * * *", "model": "lab-chat",
                 "last_fire": "2026-08-05T21:58:00Z", "last_note": "warmed" }]
},
"swap_auth": null,
"cells": [ { "name": "front", "reachable": true, "display": "SERVING", "models": ["lab-chat"] },
           { "name": "heavy", "reachable": true, "display": "SERVING", "models": ["lab-chat"] } ]

doctor:
{"id":"swap.credential","cell":"front","level":"ok","summary":"llama-swap API key declared and accepted"}
{"id":"front.extras","cell":"front","level":"ok",
 "summary":"the front's non-derived config is declared and merged into every render"}

hand warm, no credential   -> HTTP 401     (the front's config did not change)
hand warm, with the key    -> HTTP 200
```

The warm target reached `holding: target resident` — the model actually
loaded through the front — and the schedule recorded a real `last_fire`.

**HALF 3 — does fleetd's own render keep the front's `apiKeys`?**
`apiKeys` is stripped from the rendered front config, then a membership
transition is forced (the cell's llama-swap is killed and restarted):

```
apiKeys lines after the strip: 0
front_renders after the transition: 1
apiKeys lines in the re-rendered front config: 1
  2 "msg":"front config re-rendered from presence"
```

fleetd rewrote the config from presence and the operator's `apiKeys` came
back with it. Without `fleet.front_extras` that write is what silently
disarms the credential.

**fleetd's own log, both halves:**

```
1 "msg":"llama-swap API keys resolved"      (the loud config-load pass, half 2)
1 "msg":"llama-swap credential failure"     (ONE line, not one per 15s tick)
```

Two qualifications, per the plan README's rule for every harness result:
CPU models are not GPU models (the mechanism is real, the magnitude of a
cold start is not), and one box is not a fleet (no network, no TLS, no
clock skew). Neither qualification touches what this gate measures — a
header, a status code, and what the fleet says about them.

### What the design got right, and the two things the code changed

The `hosts.yaml`-field / one-authorizer / three-sentences shape survived
implementation unchanged. Two things did not:

1. **The scope was wrong in the brief and in C5's note.** "Warms fail"
   turned out to be "warms, probes, the events stream, the catalog check,
   unload and the cloud-spend tail all fail". Measuring the real binary
   first is what caught it; designing from the note would have shipped a
   credential on the warm path and left a fleet whose cells all read
   `OFF/AWAY?`.
2. **`fleet.front_extras` was not in the brief at all**, and without it
   the phase is a feature that disarms itself at the next membership
   transition. It is not adjacent scope; it is the same feature.

### Adversarial self-review addendum

Ground rule 9's separate pass, over the two feature commits.

- **REV-1 (blocker-shaped): the eighth producer.** `cli.probeCellDirect`
  — the DEGRADED path behind `vibe cell status` and the client half of
  `vibe fleet doctor` — dialled every cell unauthenticated and rendered a
  refusing cell as `down`. The first pass missed it because the enforcing
  AST scan covers `fleetapi` and `fleetmcp`, and this producer is in
  `cli`. Fixed, with the refused-credential state distinguished from
  "no answer" in both surfaces. *This is the phase's own recurring defect
  class landing inside the phase that exists to fix it.*
- **REV-2: C13's read-only structural scan did not know about this
  phase's mutators.** `swapCredential` RECORDS an unresolvable
  declaration on the way past, so a doctor check reaching for it would
  quietly make the report mutating — and C13's whole promise is that the
  command is safe mid-incident. The doctor reads
  `hosts.SwapCredentialFor` directly for exactly that reason; the banned
  list now names `swapCredential`, `recordSwapAuth`, `clearSwapAuth`,
  `NoteSwapStatus` and `AuthorizeSwap` so the next edit cannot quietly
  change it.
- **REV-3 (found while writing the loop test, landed in the feature
  commit):** the credential rung had to sit ABOVE the residency read, not
  only at fire time. C11's "a SKIP is not an observation of emptiness"
  clears `emptySince` on every skip, so a fire-time-only check made the
  warm row alternate between `waiting (nothing resident (confirming))`
  and `skipped` on successive ticks — no extra warms went out, but the
  reason was buried every other tick. Both halves are now present and
  separately pinned.

## For the reconciliation pass

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. Here is what belongs in each.

### `AGENTS.md`

A new bullet in the fleet-control section, after C14's:

> - **The llama-swap credential (fleet-control C15).** `hosts.yaml`'s
>   per-cell `swap_key_file` is the API key a cell's llama-swap demands
>   (`apiKeys:`), presented as `Authorization: Bearer`.
>   `fleetapi/swapauth.go` is the ONE authorizer and `AuthorizeSwap` is
>   the only way a request reaches a llama-swap — an AST test in
>   `fleetapi` and `fleetmcp` fails any `http.NewRequest*` in a function
>   that does not call it.
>   - **It is not `token_file`.** That authenticates the cell's vibe
>     DAEMON (drain/resume/suspend); this authenticates llama-swap's own
>     OpenAI + admin surface. The front settles it: in the reference
>     deployment it runs llama-swap and no daemon at all, so it has no
>     `token_file` to reuse — and it is the cell every warm goes through.
>   - **`apiKeys` gates everything except `/health`** (measured on v239):
>     `/v1/models`, `/running`, `/api/events`, `/api/metrics/activity`,
>     `/api/models/unload/*` and `/v1/chat/completions` all 401, and a
>     WRONG key gets 401 too (never 403). So a keyed fleet without this
>     lost not just its warms but every probe, the whole in-flight
>     evidence stream and every idle window built on it. `/health`
>     answering is not evidence that anything else will.
>   - **Three failure kinds, three fixes**: `unauthorized` (demanded, not
>     declared), `rejected` (declared, refused), `unresolvable` (declared,
>     unreadable — loud at config load, and no request is sent). They
>     surface in `fleet_status.swap_auth` and doctor's `swap.credential`.
>     The key VALUE appears nowhere: not a log, not an error, not a status
>     document, not the page (C9's webhook-URL rule).
>   - **A 401 suppresses the AUTOMATED warm producers for 5 minutes and
>     re-arms**; the warm loops are tickers, so an unguarded 401 is a
>     5,760-a-day retry loop. An operator's `warm_model` is never
>     suppressed. A 401 is still a definitive 4xx and is never queued to
>     the piggyback — routing around a broken credential hides it.
>   - **The rung is drained > held > swap-credential > evidence**, and it
>     is checked BOTH in `evalWarmTarget` and at fire time in `restore`
>     (C11's clear-`emptySince`-on-skip makes a fire-time-only check flap
>     the status row).
>   - **`fleet.front_extras` is part of the credential**: the front's
>     config is derived and rewritten on every membership transition, and
>     the renderer emits no `apiKeys`, so without the extras merge fleetd
>     deletes the key it was just given. Doctor's `front.extras` FAILs on
>     exactly that combination.
>   - **The page carries the KIND, not the sentence** (`fleet.html`'s
>     status strip): the detail names `cells.<name>.swap_key_file` and the
>     state document is guest-readable, so the sentence stays on
>     `fleet_status` and doctor. No new route — C7b's rule.
>   - **The cell-side dialers are NOT covered** (fleetannounce,
>     modelprobe, the cell's own usagemeter): different config surface.
>     The hazard to know: `announceOnce` maps a `gatherModels` failure to
>     an EMPTY model list, so a keyed cell without a cell-side credential
>     announces "nothing resident" — which is what C4's warm policy reads
>     as "restore".

Also amend C11's ladder line (currently "drained > held > stale >
unreachable > policy") to carry the credential rung.

### `docs/design/fleet-control-plan/README.md`

Add the row:

> | [C15](c15-warm-auth.md) | The warm credential: a llama-swap API key fleetd can present | ~750 lines | C4, C5, C13 | PR open; feature + front_extras + adversarial-review commits; unit gates U1–U11 green (24 mutation checks); **L1 PASS** (purpose-built two-swap rig with real `apiKeys`) |

And a paragraph after C14's:

> C15 (2026-08-05) closes the defect C5 recorded and could not fix: the
> warm path sent no credential, so a front configured with llama-swap's
> `apiKeys` failed every warm with a 401. Measuring a real v239 first
> showed the note understated it — `/health` is the only exempt route, so
> such a fleet also lost every probe, the whole `/api/events` stream and
> every idle window built on it, the catalog check, `unload_model` and
> the cloud-spend tail; only the announce path survived, because cells
> dial out. Two rules it carries forward. **A 401 stops the automated
> producers, it does not feed them** — the warm loops are tickers, so an
> unguarded credential failure is 5,760 identical 401s a day, and the
> suppression is sticky, self-clearing and re-arming rather than a
> restart-to-recover flag. And **a credential the control plane erases is
> not a credential**: the front's config is a derived artifact fleetd
> rewrites on every membership transition, so `fleet.front_extras` (the
> operator-owned half of that file) shipped with the key rather than
> after it. Its self-review found this plan's most repeated defect inside
> the phase written to fix it — an eighth producer, in `cli` rather than
> fleetd, where the enforcing AST scan could not see it.

### `docs/design/fleet-control.md`

§6 (the threat/credential section) gains the third credential beside the
fleet token and C12's guest token: the per-cell llama-swap API key,
`swap_key_file`, presented on the DATA plane's own port and never
interchangeable with the daemon's bearer. Worth stating there that the
fleet now has three credentials with three different blast radii, and
that `/health` answering proves nothing about the rest of a llama-swap.

### `docs/design/fleet-control-futures.md`

One new backlog entry, from §8: **the cell-side llama-swap credential**
— the announcer, C8's prober and the cell's own usage collector, plus the
wire distinction between "no models" and "could not read models" that
`announceOnce` currently collapses. Medium tier; it only bites a fleet
that keys its cells, but its failure mode (an empty announced catalog
read as "nothing resident") points the warm policy at a live box.
