# C21 — the visible-repoint alias tier: REJECTED, and the invisible one that already shipped

Status: **merged** (#44), off `feat/c21-alias-tier` branched from `main` at
`e144f8b`. Three commits: the decision + the fix, ground rule 9's
adversarial self-review (§8b — five findings, two fixed, one a silent
disarming of the fix itself), and the independent adversarial review pass
(§8c — nine findings, seven fixed, one blocker-shaped ordering defect and
one gate row that claimed a scenario its test never ran). Backlog item 10
([fleet-control-futures.md](../fleet-control-futures.md) §2), whose entry
asks for a decision rather than a feature:

> **Visible-repoint alias tier** — a catalog id (`best-coder`) whose
> resolution to a concrete cell model is *shown* in the catalog,
> re-resolves only on membership transitions, with a loud event. This is
> the named answer to the roaming-best-node problem; explicitly not
> per-request fallback (invariant 3 stands). Decide it deliberately in
> fleet-control.md §9 — adopt or reject with the two-id workaround
> documented — before the good laptop arrives.

**The decision is REJECT.** The argument is §1–§4. The workaround is §5,
and it is not a consolation prize: it already exists, it is one line of
YAML, and the live gate serves a completion through it.

**The phase is not only prose, because the feature turned out to be
already implemented — invisibly.** Two defs on different cells claiming
one alias, with the declared owner on the roaming cell, repoint the
catalog id to the *other cell's model* the moment the laptop leaves. No
event, no status field, no log line. §6 is the defect, §7 the fix, §8 the
live evidence: a `POST /v1/embeddings` for `best-embed` answered **HTTP
200 by a different model on a different box** than the def that declares
it, against merged `main`, on real llama-swap processes.

So this phase ships ~35 lines of production code whose entire purpose is
to make the rejection true in the code as well as in the doc.

---

## 1. What the feature would buy, enumerated

`best-coder` is a catalog id fleetd resolves to a concrete cell model,
re-resolving on membership transitions. There are exactly three states:

| state | alias tier behaviour | status quo behaviour | delta |
|---|---|---|---|
| the laptop is present | `best-coder` → the laptop's model | a declared alias on the laptop's def → the laptop's model | **none** |
| the laptop is gone, no other candidate | 404 on a declared id, or hold-the-last-target and fail `UPSTREAM_DOWN` | §4's class policy: roaming **prunes** (404), always_on/opportunistic **hold** (`UPSTREAM_DOWN`) | **none** — the futures entry's own open question ("404 or hold?") is a question C3 already answered, and either answer is the shipped one |
| the laptop is gone, another cell has a coder model | `best-coder` → the other cell's model | `best-coder` 404s | **the whole feature** |

Row 3 is the entire delta. Rows 1 and 2 are byte-identical to what ships
today, which is the first thing that should be suspicious about the
proposal: a mechanism whose value is concentrated in one branch is a
mechanism that should be judged on that branch alone.

Row 3 is: a consumer sends `best-coder` and a model it did not ask for
answers, successfully.

## 2. Why "it's visible" does not rescue row 3

The futures entry's claim is that visibility makes the substitution legal
— shown in the catalog, re-resolved only on membership transitions, with
a loud event. Each half was tested against the substrate rather than
against intuition.

**The visibility lands on the operator; the harm lands on the consumer.**
The event goes to `/api/fleet/events`, the resolution to `fleet_status`
and the fleet page. Every one of those is read by the operator — the one
party who already knows the laptop left, because they carried it out of
the house. The consumer is a harness config: `pi`, `qwen-code`, Claude
Code, a vamp capability, an overnight batch. Its channels to the fleet are
`/v1/models` and the completion response. Nothing else.

**What those two channels actually carry** (measured on the rig, v239,
§8):

- The front's `/v1/models` **does** attribute each id to a peer:
  `{"id":"best-embed","name":"charlie: best-embed","meta":{"llamaswap":{"peerID":"charlie"}}}`.
  So "shown in the catalog" is *partly true already* — at **cell**
  granularity. It names the box, never the model, and two boxes can serve
  the same weights.
- The completion response's `model` field is **endpoint-dependent and
  therefore not a channel**. Measured in the same run: a chat completion
  for `best-coder` came back `"model":"lab-chat"` (the concrete upstream
  id — a disclosure), and an embeddings request for `best-embed` came
  back `"model":"best-embed"` (the alias echoed straight back — no
  disclosure at all). One route tells the truth, the other repeats the
  question, and neither is a vibe guarantee: `useModelName` exists
  precisely to rewrite that field, and vibe already sets it for every mlx
  tenant.

Making the substitution reliably visible **to the consumer** means
stamping the resolution into the response — which means the front
rewriting bodies and SSE frames. The front is llama-swap; vibe has no hop
there and invariant 1 forbids adding one. **The one mechanism that would
make row 3 honest to the party that bears it is structurally
unavailable.** That is the decisive argument, and it does not depend on
anyone's taste.

**"Only on membership transitions" reduces the frequency and worsens the
diagnosis.** Per-request fallback is at least per-request: a wrong answer
and a right answer can be compared inside one session. A resolution that
changed at 09:14 and persists means a batch launched at 09:00 ran half on
one model and half on another, with no boundary marked anywhere. Every
evaluation number that straddles the transition is silently
incomparable — and comparing model quality is the reason this fleet has a
`vibe model try` (C18) and a probe baseline (C8) at all.

**A client cache makes it worse, not better.** Harnesses read `/v1/models`
once at startup. Under the prune the cache goes *wrong* and the failure is
loud: the id vanishes, requests 404, someone notices in seconds. Under a
repoint the cache stays *right about the id* and wrong about the model,
and nothing ever notices. The cache converts a fail-loud state into a
fail-quiet one.

**And the "loud event" was tested by accident and failed.** §6's defect
has shipped since C3. The prune it rides on logs
`pruning roaming cell from front render` — a loud line, in the journal,
at exactly the right instant — and the catalog silently changed meaning
underneath it for five phases without anyone noticing. An event is loud in
proportion to who is watching it during the two seconds it matters, and
nobody watches an event stream during those two seconds.

## 3. Where it sits against the invariants

Invariant 3 reads: *no silent rerouting, no silent fallback — the control
plane changes what the catalog SAYS, never where a request GOES.* The
feature's defence is that it changes only what the catalog says.

That defence is too literal. The catalog "saying" something is a
*statement about which model answers an id*. Rewriting the statement so
that the same id now names different weights is not an alternative to
rerouting the request; it is the same outcome reached by editing the map
instead of the route. **The invariant's subject is the consumer's
expectation, not the config file's mechanism.**

The sharper reading is invariant 2 plus §4's own sentence: **membership —
which cells exist, which models each serves — is *config, not state*; it
changes rarely, through git.** Choosing *which model an id names* is a
membership statement. Letting fleetd choose it from presence evidence is
an observation authoring a declaration, which is C14's corollary
(a declared action may be deferred by observation; an observation may
never initiate one) run backwards.

And note what C3's class policy does *not* do. Prune removes an id.
Hold keeps an id and lets the request fail `UPSTREAM_DOWN`. **Both
branches of the shipped policy are fail-LOUD: neither has ever returned
`200 OK` from a model the caller did not name.** The alias tier would be
the first mechanism in this design to do so. §9's rejected-alternatives
table already refuses "registry on the data path" for being *nice UX that
violates invariant 1*; this is nice UX that violates invariant 3 by a
longer route.

## 4. The problem is real; the owner of the answer is the consumer

The roaming-best-node problem is not imaginary. Futures §1 states it: when
the laptop hosts the fleet's best coder model, prune means the id 404s on
every commute and opportunistic means permanent noise.

But look at what actually hurts. Pinning `qwen3.6-35b-a3b-mlx` and getting
a 404 while the laptop is on a train is **correct**: the model genuinely is
not in the fleet. The pain is ergonomic — the operator edits a harness
config twice a day — and the fix for an ergonomic pain is not to make the
control plane lie.

This design has already answered this exact question once, in futures §4:
router-enforced silent cloud spillover is killed, and the sanctioned form
is named as **agent-side declared policy** plus a reserved `on_cold`
namespace. Same shape here. The party that should choose a substitute is
the party that bears the consequence of the substitution, and it is not
fleetd.

The fleet-aware version of that is cheap and stays clean, and is named in
§10 as the revisit condition rather than built here: a **read-only**
resolution — "which cell currently serves a chat model of class X" — that
a consumer calls **before** a session and then pins the concrete id it got
back. The consumer knows what it is talking to, the id it sends is the id
that answers, and the catalog never repoints. That is a lookup, not a
substitution.

## 5. The workaround, which is one line and already works

**It is not two ids.** The router has had declared aliases with explicit
ownership since C2, and they are first-class front catalog ids.

```yaml
# ~/.config/vibe/backends/laptop-coder.yaml
name: laptop-coder
cell: laptop
router:
  aliases: [best-coder]
  alias_owner: true          # this def owns the alias fleet-wide
```

```yaml
# ~/.config/vibe/backends/gpu-coder.yaml
name: gpu-coder
cell: gpu
router:
  aliases: [best-coder]      # a claimant, not the owner
```

- Consumers pin **`best-coder`**. The front lists it
  (`includeAliasesInList: true`) and routes it to the owner's cell. Gate
  §8 Q1 serves a real completion through it.
- **The repoint is the operator moving `alias_owner: true` one line
  down** — a commit in the defs repo, a diff, a timestamp, an author, and
  a `render_front`. Membership changed through git, which is what §4 says
  membership does.
- **When the laptop leaves and nobody moves the line, `best-coder`
  404s** — together with `laptop-coder`, because C3's roaming prune takes
  the def and its aliases as one unit. That is the honest state and it is
  the same 404 the concrete id gives. Gate §8 Q2 proves it against a real
  front.
- Collisions cannot be silent: two claimants and no `alias_owner` is a
  **render error** naming both defs, and (C21) it stays an error even
  after one claimant leaves.

What breaks when the laptop leaves, stated plainly: **the id stops
existing until a human says otherwise.** A harness pinned to `best-coder`
fails until either the laptop comes back — the alias returns with it,
after C3's re-add hysteresis; gate §8 step 7 — or the operator moves the
owner line. That is the cost of the rejection, it is a 404 rather than a
wrong answer, and it is the cost the design's own class table already
chose for roaming cells.

## 6. The defect: the feature already shipped, without any of its promises

Writing the test that pins the rejection is what found it.

`router.Render` resolves alias ownership over **the defs it is handed**.
fleetd's presence loop hands it **the survivors**: `applyClassPolicy` has
already dropped the pruned roaming cell's defs and `applyFingerprints` has
already dropped strict fingerprint mismatches. So when the declared owner
is pruned, the co-claimant is the only claimant left, wins the alias by
default, and the front renders the id under **its** peer.

That is the visible-repoint alias tier, minus the visibility, minus the
event, minus the operator ever declaring they wanted it.

It composes with a second half in the same function. A **cell** render
sees only that cell's own defs, so the losing claimant's llama-swap was
already configured to answer to the contested alias — which is what lets
the front's new route resolve end to end instead of 404ing at the cell.

Three triggers, all shipped:

1. **C3's roaming prune** — the exact scenario futures §1 describes. The
   laptop leaves; the alias moves to the desktop.
2. **C3/C5's strict fingerprint exclusion** — an embed def excluded for
   drift hands its alias to a co-claimant. The mechanism that exists to
   stop silent retrieval damage causes some.
3. **Render-internal exclusions**: C18 trial defs (`trial: true`),
   unassigned defs at the front, other cells' defs on a cell render.

And a fourth shape, worse than the others because the loud state heals
itself: two claimants with **no** `alias_owner` is a render *error*. Once
the roaming claimant is pruned the collision disappears, the render
**succeeds**, and the alias lands on whoever was left. The error going
away *was* the repoint.

## 7. The fix, in one sentence

**Alias ownership is decided over the DECLARED def set; an exclusion
removes an alias from the catalog, it never transfers it.**

Two sites, because two different layers do the excluding:

- **`router.Render`** resolves over every declared claimant in its input
  (`aliasClaimants(defs)`), not over the survivors of its own cell/trial
  selection. Fixes triggers 3, and the cell half of trigger 1.
- **`fleetapi.renderPass`** computes winners with the new exported
  `router.ResolveAliases(defs)` **before** either overlay runs, and passes
  them as `router.Options.AliasWinners`. Fixes triggers 1 and 2 — those
  defs never reach `Render` at all, so `Render` cannot know they exist.

An alias won by a def that is not rendered is emitted nowhere: the id
disappears from the catalog. That is invariant 3's fail-loud direction and
it is exactly what §4's roaming-prune row already promised.

`ResolveAliases`'s error is returned, not swallowed, so trigger 4's
collision **stays** an error after a claimant leaves. One consequence,
recorded rather than smoothed over: a fleet with an unresolvable alias
collision now has a **frozen** front catalog (the render fails every pass
and the last good file stands) instead of one that silently fixed itself
by repointing. That configuration is broken either way and already failed
every render while both claimants were present; the change is that it can
no longer un-break itself into a wrong answer. The message names both defs
and the one-line fix.

**One behaviour change worth calling out for an upgrading fleet.**
Broadening resolution to the declared set means a cross-cell alias
collision that used to render fine on a *cell* render is now an error
there too. It has always been an error on the front render, which is the
render that decides routing — so the change makes two renders agree, in
the loud direction. No reference-fleet or `scripts/fleetlab` def hits it.

### Files

| file | change |
|---|---|
| `internal/vibe/router/render.go` | `Options.AliasWinners`; exported `ResolveAliases`; `aliasClaimants`; `Render` resolves over the declared set |
| `internal/vibe/fleetapi/render_loop.go` | `renderPass` resolves over the DECLARED set and passes `AliasWinners`; the collision error fails the pass; `warnOrphanedAliases` names the ids an overlay removed (§8c REV2-1, REV2-4) |
| `internal/vibe/router/alias_scope_test.go` | 7 tests: the unassigned / trial / cross-cell transfers, the `AliasWinners` seam, the collision that must not heal, the cloud-peer exclusion, the non-nil-map guard |
| `internal/vibe/fleetapi/c21_test.go` | 4 tests through the **real** `router.Render` in the loop's seam: prune-does-not-repoint, collision-does-not-heal, alias-returns-with-its-owner, and the `AliasWinners` non-nil guard |
| `scripts/fleetlab/gate-c21-alias.sh` | the L1 rig (§8) |

No new HTTP route, no MCP tool, no proto change, no state file, no config
key, no page diff, no new dependency. A rejection should not grow a
surface.

## 8. Acceptance gates

### Unit

| # | gate | result |
|---|---|---|
| U1 | An excluded **unassigned** owner's alias does not transfer to a cell-assigned co-claimant | PASS |
| U2 | An excluded **trial** owner's alias does not transfer, and the trial exclusion still warns | PASS |
| U3 | A **cell** render does not give the losing claimant an alias another cell's def owns; the owner's own render keeps it | PASS |
| U4 | `ResolveAliases` + `AliasWinners`: a winner absent from the render is emitted nowhere; the pruned cell has no peer stanza | PASS |
| U5 | An unresolvable collision stays an error with one claimant missing — and resolving over the SURVIVOR set alone silently hands the alias over, which is the defect (§8c REV2-3 rewrote this body; as shipped it resolved the identical slice twice and nothing was ever missing) | PASS |
| U6 | `ResolveAliases` ignores cloud_peer defs (their ids come from `cloud_peer.models`) | PASS |
| U7 | Through the real renderer in the loop: pruning the roaming **owner** removes the alias from the catalog and does not repoint it at the co-claimant's cell | PASS |
| U8 | Through the real renderer: an unresolvable collision renders **nothing**, before and after the prune (`RenderCount == 0`) | PASS |
| U9 | Through the real renderer: the alias returns with its declared owner after C3's re-add hysteresis, and the co-claimant never holds it | PASS |
| U10 | `ResolveAliases` returns a NON-NIL map with nothing to resolve (no defs / no aliases / only a cloud peer) — a nil map re-enables `Render`'s fallback | PASS |
| U11 | The render loop passes a non-nil `AliasWinners` on every pass, including on a fleet that declares no aliases at all | PASS |
| U12 | Through the real renderer: the STRICT-FINGERPRINT exclusion (§6 trigger 2) does not transfer the alias either — the claim §7 makes and nothing exercised (§8c REV2-2) | PASS |
| U13 | A collision error still leaves C9's fingerprint-mismatch set evaluated (§8c REV2-1) | PASS |
| U14 | The pass NAMES the alias that left with a pruned owner, and stays silent when the pruned def owns none (§8c REV2-4, both halves) | PASS |
| U15 | comfyui defs are alias claimants: their declared alias survives and their name reserves (§8c REV2-5) | PASS |

**Mutation-verified**, five production predicates, each reverted,
confirmed red on a named test, restored:

| mutation | red |
|---|---|
| `Render` resolves over `modelDefs` (the survivors) again | U1, U2, U3 |
| `renderPass` stops passing `AliasWinners` | U7 — failure output prints the repointed config with `best-embed` under `alpha` |
| `renderPass` swallows the `ResolveAliases` error | U8 |
| `renderPass` omits `AliasWinners` entirely | U11 |
| `ResolveAliases` returns nil when there is nothing to resolve | U10 |

Inner loop, all on this branch's tree: `go build ./...` clean,
`go vet ./...` clean, `gofmt -l .` silent, `go mod tidy` leaves
`go.mod`/`go.sum` byte-identical, `golangci-lint run` **0 issues**,
`go test -race ./...` **exit 0** (31 packages; go test's own exit status,
not a pipeline's), and `go test -race -count=5` on
`internal/vibe/router` + `internal/vibe/fleetapi` **exit 0**.

### Live — L1 PASS (`scripts/fleetlab/gate-c21-alias.sh`, 2026-08-06)

Real llama-swap v239 processes, a real fleetd, real announcers, real CPU
models, three `hosts.yaml` classes. `FLEETLAB_DIR=/tmp/fleetlab-c21`;
production (`llama-swap :9000`, the vibe daemon `:9001`,
`~/.config/vibe`) untouched throughout and verified untouched after.

Defs patched on top of the standard lab fleet:

- `lab-chat` (cell **alpha**, `always_on`) — `router.aliases: [best-coder]`,
  sole claimant. *The workaround.*
- `lab-embed-c` (cell **charlie**, `roaming`) —
  `router.aliases: [best-embed]`, `alias_owner: true`. *The declared
  owner, on the cell that leaves.*
- `lab-embed-a` (cell **alpha**) — `router.aliases: [best-embed]`.
  *The co-claimant that used to inherit it.*

```
=== 3. Q1 — a declared alias is a real front catalog id ===
front config: best-coder -> peer 'alpha'
PASS  best-coder renders under its def's cell
PASS  the front's /v1/models lists best-coder
--- what the front's own catalog says about the alias ---
[{'id': 'best-coder', 'name': 'alpha: best-coder', 'meta': {'llamaswap': {'peerID': 'alpha'}}, ...},
 {'id': 'best-embed', 'name': 'charlie: best-embed', 'meta': {'llamaswap': {'peerID': 'charlie'}}, ...}]
--- completion for best-coder through the front (routes to alpha's lab-chat) ---
{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"proof"}}],
 "model":"lab-chat", ...}
PASS  the front SERVED a completion for the alias

=== 4. Q2 — the contested alias resolves to its DECLARED owner ===
front config: best-embed -> peer 'charlie'
PASS  best-embed names the declared owner's cell (charlie), not the co-claimant
PASS  the co-claimant still renders under its own name

=== 5. the roaming owner leaves the building ===
charlie's announcer stopped at 2026-08-06T06:54:36-03:00; waiting for stale + the prune render
[('alpha', False, 'SERVING'), ('bravo', False, 'SERVING'), ('charlie', True, 'SERVING'), ...]

=== 6. Q2 — the alias LEFT the catalog and did not repoint ===
PASS  the roaming cell is pruned from the front
front config: best-embed -> peer '<absent>'
PASS  best-embed is ABSENT from the catalog (pre-C21 it named alpha's lab-embed-a)
PASS  the front's /v1/models no longer lists best-embed
--- an embeddings request for the departed alias ---
HTTP 404
{"error":"no router for requested model","src":"llama-swap"}
PASS  the departed alias FAILS rather than being answered by another cell's model
PASS  alpha keeps serving its own id throughout

=== 7. the owner comes home ===
front config: best-embed -> peer 'charlie'
PASS  the alias returns with its declared owner (re-add hysteresis honoured)

C21 L1: PASS
```

Ten checks, green on two consecutive clean runs.

### Live — L2 PASS: the defect, on merged `main`, end to end

The same rig with **fleetd and the alpha cell render swapped to a binary
built from `origin/main` (`e144f8b`)** — everything else identical,
including the def files. Same departure:

```
### swapping fleetd to the PRE-FIX binary (origin/main e144f8b)
--- charlie fresh, pre-fix fleetd ---
    charlie:
        models: [lab-embed-c, best-embed]

### charlie leaves
--- charlie pruned, PRE-FIX fleetd ---
    alpha:
        proxy: http://127.0.0.1:9641
        models:
            - lab-chat
            - best-coder
            - lab-embed-a
            - best-embed        <-- the repoint

### the SERVED consequence
GET  /v1/models            -> [('best-coder','alpha'), ('best-embed','alpha')]
POST /v1/embeddings model=best-embed -> HTTP 200
     response model field: best-embed          <-- the alias, echoed back
     embedding dims: 1024
GET  http://127.0.0.1:9641/running -> {"model":"lab-embed-a","state":"ready", ...}

### what the control plane SAID about it
front_renders: 1
any alias mention in the state doc: False
(grep -iE "alias|best-embed" over fleetd's own log: no lines)
```

A request naming `best-embed` — an id whose only declared owner is
`lab-embed-c` on a cell that is off the fleet — was answered **200** by
`lab-embed-a` on another box. The response echoed the alias back rather
than naming the model that served it. `/api/fleet/state` contained no
mention of it, and fleetd logged nothing.

That is the paragraph the futures entry asked to be decided on, executing
in production code, with none of the three properties that were supposed
to make it legal.

The lab was then restored to this branch's binary and gate L1 re-run green
from the restored state; `./lab.sh down` afterwards.

### Not run, and why

- **A real roaming box.** Charlie is a llama-swap process on localhost
  whose announcer is killed. The prune path, the staleness clock, the
  re-add hysteresis and the catalog transition are all real; a laptop
  physically leaving the LAN is C3 gate 1's outstanding item and this
  phase does not close it.
- **A consumer harness observing the repoint.** The claim in §2 that no
  harness reads the response `model` field is reasoning about client code,
  not a measurement. What *was* measured is the substrate: what the two
  channels carry, and that one of them echoes the alias.

### A note on the rig, for the next agent

`scripts/fleetlab` needs a `FLEETLAB_DIR` short enough for a unix socket
path — a scratchpad under
`/tmp/claude-*/…/<uuid>/scratchpad/fleetlab-c21` failed with
`bind: invalid argument` on `run/rt-fleetd/vibe/vibe.sock` (108-byte
`sun_path` limit) and the only symptom is `lab.sh up` reporting "fleetd
did not come up". `/tmp/fleetlab-c21` works. This is a second reason
futures item 15 (a `FLEETLAB_PORT_BASE` knob) should also take a
path-length check.

## 8b. Adversarial self-review addendum (ground rule 9)

Five things the review pass went looking for. Two became commits, three
are recorded as checked-and-clean or checked-and-pre-existing, because
"I looked and it was fine" is worth as much to the next agent as a fix.

**REV-1 (fixed, blocker-shaped).** `Options.AliasWinners` uses `nil` to
mean "resolve it yourself", which is correct for the three callers holding
a whole checkout and is a **silent disarming** for the one caller that
does not. If `ResolveAliases` ever returned a nil map — or a future
refactor dropped the field from `renderPass`'s `Options` literal — fleetd
reverts to resolving over the survivors with no error, no log line and no
test failure, and the defect this phase exists to close comes back on
exactly the fleets nobody is watching. This is C15's `front_extras`
lesson: a declared-but-absent input must not degrade quietly. Pinned from
both ends (U10, U11), both mutation-verified. Considered and rejected:
removing the fallback entirely, because the CLI, `RenderToFile` and
`fleetmcp` all legitimately hold the full checkout and forcing them to
pre-compute would spread the invariant across four call sites instead of
concentrating it in one.

**REV-2 (fixed, rig honesty).** `peer_of` in the gate rig returned the
LAST peer claiming an id. A duplicate — one id under two peer stanzas —
would have printed the expected cell and passed the check. It now prints
`a+b`, which fails every equality. A rig that can only report the answer
it expects is C5's `TestWarmTarget_SkipsAbsentAndDrainedCells` again.

**REV-3 (checked, clean): every other `Render` caller.** Audited all four:
`cli.runRouterRender`, `router.RenderToFile`, `fleetmcp`'s `render_front`
dry-run and `modeltry`. Each passes an unfiltered `router.LoadDefs`
result, so each is correct with `AliasWinners` nil. `fleetapi.renderPass`
is the only pre-filtering caller in the module.

**REV-4 (checked, clean): does C18's trial scaffolder create the
collision?** It would be the common case if it did — `vibe model try
--like <def>` derives a candidate from the incumbent, and a copied
`router.aliases` would collide with the incumbent's on every trial.
`modeltry.deriveDef` already drops aliases and its comment names the same
hazard ("a render error at best, a silent re-point at worst"). C18 guarded
its own path before this phase found the general case. So U2's trial
scenario needs a hand-written `alias_owner: true` on a trial def — a
declaration, and after C21 it yields a missing id rather than a
substitution.

**REV-5 (checked, pre-existing, NOT fixed here): an alias colliding with a
cloud peer's MODEL id.** `resolveAliases` reserves def *names*, and a
`cloud_peer` def's name is a peer stanza key rather than a catalog id — so
`aliasClaimants` correctly excludes it (U6). But the ids a cloud peer
actually serves come from `cloud_peer.models`, and nothing checks an alias
against those. A def aliased `claude-sonnet-5` would render that id under
two peers. Unchanged by this phase in either direction, out of its scope,
and named here so it is not rediscovered as a C21 regression. The rig
change in REV-2 is what would catch it in a gate.

Also unchanged and worth stating: `fleetmcp`'s `render_front` dry-run
renders the FULL def set while fleetd renders the pruned one, so its diff
shows a pruned roaming cell as drift. That predates C21 (the peer stanzas
already differed); the alias is now one more line in a diff that was
already there.

## 8c. Adversarial-review addendum (ground rule 9, independent pass)

An independent pass over the two feature commits. Nine findings, seven
fixed on this branch, one recorded as pre-existing-and-accepted, one
recorded as needing a surface decision this pass declined to make alone.
Every fix is mutation-verified: the mutation, the test it turns red, and
that the guard is not inert are all listed below.

**REV2-1 (fixed, blocker-shaped — an abort that took the drift alarm with
it).** `renderPass` resolved aliases *before* `applyClassPolicy` and
`applyFingerprints`, and returns the collision error. `applyFingerprints`
is the only thing in this process that evaluates C9's persistent-drift
set (`setFingerprintMismatches`, rebuilt every pass, preserving
`FirstSeen`), and `applyClassPolicy` is the only thing that maintains the
prune/re-add hysteresis. So one unresolvable alias anywhere in the
checkout froze that set on its last value **forever**, while
`notifyStatus`'s `fingerprint_source` — which keys on `renderLoopOn`, not
on whether a pass has run — kept reporting the evaluator as live. A
mismatch that had since been repaired keeps paging; one that appeared
after the collision is never seen. That is this plan's recurring defect
class, absent evidence read as current, on the one subsystem whose job is
not being silent.

The resolution still reads the DECLARED set — `declared := defs` before
the overlays — but the CALL moved after them, so a failing pass fails
having done the same bookkeeping every other failing pass does. This
restores the pre-C21 side-effect ordering exactly: before this phase the
collision error came out of `Render`, downstream of both overlays.
Pinned by U13, red on the shipped ordering.

**REV2-2 (fixed, incomplete guard — 1 of 2 triggers tested).** §7 claims
`renderPass` fixes triggers 1 *and* 2, trigger 2 being C3/C5's strict
fingerprint exclusion — "the mechanism that exists to stop silent
retrieval damage causes some". All four loop tests drive the class-policy
prune; nothing anywhere exercised the fingerprint overlay's half, at
either layer. It is genuinely fixed, but it was asserted, not proven, and
this plan has already shipped a gate row whose body did not run its own
scenario. U12 now drives it through the real renderer: a strict-mode
owner excluded for drifted flags takes `best-embed` out of the catalog
rather than handing it to the co-claimant on the other cell.

**REV2-3 (fixed, a test that asserted less than its name — ground rule
10).** `TestResolveAliases_CollisionStaysAnErrorWithOneClaimantMissing`
called `ResolveAliases(all)` twice on the identical slice, with the
comment "so the error survives the prune". No claimant is ever missing;
the two calls are the same call. Gate row U5 then reported the scenario
as PASS. This is exactly the
`TestWarmTarget_SkipsAbsentAndDrainedCells` shape ground rule 10 was
written about, in the phase whose thesis is that a mechanism heals itself
into a wrong answer. The body now does both halves: resolving over the
SURVIVOR set is shown succeeding and handing `best-coder` to the
co-claimant — the defect, reproduced — and the declared set is shown
still refusing.

**REV2-4 (fixed, the fail-loud direction was silent to the operator).**
After the fix an excluded owner's alias vanishes from the catalog, and
nothing anywhere says so. The prune's own line names a CELL
(`pruning roaming cell laptop`); every exclusion *inside* `Render` warns
by name; the disappearance of `best-coder` — a catalog id a consumer
pinned — produces nothing at all. The operator's evidence is a harness
404 next to a co-claimant's box that is plainly up, which is the reading
§6 says nobody made for five phases. `warnOrphanedAliases` names the
model, its cell and the ids that left, once per pass, only for defs the
overlays dropped (the in-`Render` exclusions already warn there, so a
steady fleet adds no lines). U14 pins both halves — that it fires, and
that a pruned def owning no alias stays quiet, because a warning attached
to "a def was dropped" rather than "a winning alias was dropped" is a
line on every roaming prune forever.

**REV2-5 (fixed, an untested predicate in the new code).** A sweep of
`aliasClaimants`'s four predicates found that dropping `ComfyUI` from it
turned no test red — comfyui defs become `models:` entries like the other
two kinds and `RouterOpts` is accepted on any def, so dropping them would
delete a declared alias from every catalog and free its name for another
claimant. U15 covers claim, name reservation and collision. (The other
three predicates were already caught: cloud_peer by U6, `External` by the
router package's golden tests, mlx by `TestRender_MLXTenant`.)

**REV2-6 (fixed, rig safety).** `gate-c21-alias.sh` wrote the embeddings
response body to a fixed `/tmp/c21-embed.out` — every other rig in
`scripts/fleetlab` keeps its artifacts under `$LAB`, and `curl -o` on a
predictable world-writable path follows whatever symlink is already
there. It also accepted curl's `000` (could not connect) as proof that
the departed alias fails, which is a rig that passes when the front is
down.

**REV2-7 (fixed, doc drift).** The reconciliation section's README row
claimed "unit gates U1–U9 green, 3 predicates mutation-verified" against
§8's own U1–U11 and five mutations, and the AGENTS.md block described
`renderPass` as calling `ResolveAliases` "**before** `applyClassPolicy`
and `applyFingerprints`", which REV2-1 makes wrong. Both corrected in
place.

**REV2-8 (NOT fixed — needs a surface decision).** A failing `renderPass`
is invisible to every surface this plan has: no field in
`/api/fleet/state`, nothing on the fleet page, no `vibe fleet doctor`
check, no C9 condition. Its only trace is
`slog.Warn("presence-derived render failed")` in a containerized fleetd's
log. That is pre-existing — the zero-defs gate, the `front_extras` gate
and every `Render` error have always been silent — but C21 creates the
first **permanent** instance of it: §7 records, correctly, that an
unresolvable collision now freezes the front catalog instead of healing
into a repoint. A frozen catalog stops tracking the fleet indefinitely
(new models never appear, pruned cells never leave) while `front_renders`
sits still and every display reads green, which is the same "absent
evidence read as healthy" the README's C13 paragraph says this plan has
been bitten by in five phases. Not taken here because the fix is a new
status surface — a `front_render` block in the state doc plus a
`render.current` doctor check — and §7's own rule is that a rejection
should not grow one. It belongs to whoever owns the next fleetd status
phase; see the reconciliation section.

**REV2-9 (checked, pre-existing, accepted).** The strict-fingerprint
exclusion is driven by announce data, and the fleet token is every cell's
voice (design §6). After C21 a forged mismatch against a strict def
removes both the def's id and its aliases from the fleet catalog, where
before it removed the id and MOVED the alias to another cell's model.
Both outcomes are bad and the new one is the fail-loud half; enforcement
is still bound to the def's HOME cell, so a cross-cell announce cannot
reach it. Recorded rather than changed.

**Mutation-verified**, this pass's own guards. Each mutation applied to
the fixed tree, `go build` confirmed clean (a mutation the compiler
catches is not evidence), the suite run, the named test confirmed red,
the tree restored:

| mutation | red |
|---|---|
| `renderPass` resolves before the overlays again (the shipped ordering) | U13 only — U14 stays green, so the two fixes are independent |
| `warnOrphanedAliases` call removed | U14 only |
| `warnOrphanedAliases` warns for every dropped def, aliases or not | U14's quiet half only — the inert-guard check |
| `renderPass` resolves over the SURVIVORS (`defs`, not `declared`) | U12, U13, U14, U7, U8 |
| `aliasClaimants` drops comfyui defs | U15 (nothing before this pass) |
| `resolveAliases` picks `all[0]` instead of erroring on zero owners | U5, U8, U15, `TestRender_AliasCollision` |

Inner loop on the reviewed tree, go test's OWN exit status: `go build`,
`go vet`, `gofmt -l .` silent, `go mod tidy` byte-identical,
`golangci-lint run` **0 issues**, `go test -race ./...` **exit 0**, and
`go test -race -count=5` over `fleetapi` + `router` + `daemon` +
`modeltry` + `cli` **exit 0**.

**Not re-run by this pass: L1 and L2.** The rig's ports are fixed
(`:9640`, `:9721`) and collide with the band reserved for a concurrent
agent's lab, so the live transcripts in §8 stand as the build pass's runs,
not this one's. The review's production delta is an ordering change with
identical rendered output plus one log line, and neither L1 nor L2
asserts on either — but ground rule 10 means that is an argument, not a
run, and it is recorded as one.

## 9. What this phase deliberately does not do

- **No `resolve_best` verb.** §4 names it as the sanctioned shape and §10
  as the revisit condition. Building it is a phase, and it needs a
  consumer that will call it.
- **No alias-repoint event.** There is nothing to announce: after §7 an
  alias only moves when a human moves it, and that move is already a git
  commit and a render.
- **No alias surface in `fleet_status` or on the page.** The rendered
  front config is where alias resolution lives, `vibe router render
  --check` diffs it, and adding a second display of a static mapping is a
  surface that can drift from the file that decides.
- **`llama_server.alias` keeps its default-claim behaviour.** A def with
  no `router.aliases` still claims its backend alias, so pre-router client
  configs keep working. Only the *scope* of resolution changed.

## 10. When to revisit

Named conditions, in the order they are likely to arrive:

1. **llama-swap reports the resolved concrete model per request** — in
   `/v1/models` metadata *and* in the completion response, on every route,
   as a contract rather than an accident of which endpoint echoes what.
   Then the disclosure is upstream's, vibe's job is to render it, and §2's
   decisive argument is gone. Track it against the `useModelName` rewrite,
   which is the same field.
2. **A fleet-aware consumer exists.** If a harness (or a vamp capability,
   or an MCP-driven agent) will call a read-only resolution before a
   session and pin the concrete id it gets back, §4's sanctioned shape is
   worth a phase. The test that it is really the sanctioned shape: the id
   the client sends is the id that answers, always.
3. **The operator finds themselves moving `alias_owner` more than weekly.**
   That is the ergonomic pain becoming a real cost, and it is measurable —
   `git log` on the defs repo. It argues for making the *declared* repoint
   cheaper (a `vibe router alias <name> --owner <def>` verb that edits the
   def and renders), never for making it automatic.

What should **not** reopen it: a 404 during a commute. That is the design
working.

---

## For the reconciliation pass

> **APPLIED by C22 (PR #46, 2026-08-06)** — verified against the tree by
> C26b, which added the second half C26a owed this rule: a cloud peer's
> model ids are canonical, so an alias equal to one is unresolvable and
> `alias_owner` cannot arbitrate it.

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. Everything meant for them is below.
`docs/design/fleet-control-futures.md` **is** updated on this branch
(item 10 → REJECTED, plus §4's killed list).

### `docs/design/fleet-control.md` §9 — the row the futures entry asked for

Add to the rejected-alternatives table:

> | **Visible-repoint alias tier** (a catalog id like `best-coder` whose target fleetd re-resolves on membership transitions, shown in the catalog and evented) | Rejected 2026-08-06 ([C21](fleet-control-plan/c21-alias-tier.md)). Its two safe cases — candidate present, no candidate at all — are already what a declared alias plus §4's class policy do; the entire delta is the substitution case, which answers `200 OK` from a model the caller did not name. Prune and hold are both fail-LOUD, and this would be the first mechanism here that is not. The proposed defence, visibility, lands on the OPERATOR (event, `fleet_status`, the page) while the harm lands on the CONSUMER, whose only channels are `/v1/models` — which attributes an id to a peer, never to a model — and the completion response, which is endpoint-dependent (a chat response named the concrete model; an embeddings response echoed the alias back). Making it visible there means rewriting responses at the front, which is invariant 1. The workaround is a declared alias with `router.alias_owner` moved by hand: one line, in the diff, on the operator's clock — membership through git, as §4 says. C21 also closed the INVISIBLE version of this that had shipped since C3: alias ownership was resolved over the defs that survived the prune, so a pruned roaming owner handed its alias to a co-claimant on another cell, with no event and nothing in `fleet_status`. Revisit conditions are named in the phase doc §10. |

Two smaller edits in the same file:

- **§4, the class table's roaming row.** No change to the policy; consider
  appending to the paragraph under it: *"An alias declared on a roaming
  cell's def prunes with it — the id 404s rather than moving to another
  cell's model (C21)."* The hold/prune split's stated purpose ("the
  catalog stays honest") is exactly this, so it is a clarification, not
  new policy.
- **§8, invariant 3.** The invariant's text stands. If a corollary is
  wanted, C21's is: *"An exclusion removes a catalog id; it never
  re-points one. What the catalog says about an id must change only when
  a human declares it."*

### `docs/design/fleet-control-plan/README.md`

Status-table row:

> | [C21](c21-alias-tier.md) | The visible-repoint alias tier: **rejected**, and the invisible one that already shipped | ~55 lines + 15 tests | C2, C3 | PR open; feature + self-review + adversarial-review commits (5 + 9 findings); unit gates U1–U15 green, 11 predicates mutation-verified; **L1 + L2 PASS** (harness, 2026-08-06, build pass — not re-run after the review commit) |

And a prose paragraph in the sequence after C19's:

> C21 (2026-08-06) is backlog item 10, and it is the plan's first phase
> whose deliverable is a **decision**: the visible-repoint alias tier is
> REJECTED, with the workaround written down and the revisit conditions
> named. Two things it carries forward. **Visibility is not a property of
> a mechanism, it is a property of who reads it** — the alias tier's
> defence was that the resolution is shown and evented, and every one of
> those surfaces is read by the operator, who already knows the laptop
> left, while the consumer's only channels are `/v1/models` (which names
> the peer, not the model) and a completion response whose `model` field
> is endpoint-dependent; making it honest to the consumer means rewriting
> responses at the front, which is invariant 1. And **enumerate the
> feature's states before arguing about it**: two of the alias tier's
> three states are byte-identical to what ships, and the whole delta is
> the one state that answers `200 OK` from a model nobody asked for.
> Writing the test that pinned the rejection then found that the feature
> had **already shipped invisibly** since C3 — alias ownership resolved
> over the defs that survived the roaming prune, so a departing owner
> handed its alias to a co-claimant on another cell — which is also the
> phase's answer to "is a loud event enough": the prune logs a loud line
> at exactly the right instant, and this went unnoticed for five phases.

Ground rule 10 note for the table: L1/L2 are harness runs, so both README
qualifications apply — CPU models, one box, no laptop that physically
leaves the LAN.

### `AGENTS.md`

Under the fleet-control section, a new bullet block:

> - **Alias scope (fleet-control C21).** The visible-repoint alias tier is
>   **rejected** (design §9); `docs/design/fleet-control-plan/c21-alias-tier.md`
>   holds the argument, the workaround and the revisit conditions. The rule
>   the code now enforces:
>   - **Alias ownership is decided over the DECLARED def set, and an
>     exclusion removes an alias from the catalog rather than transferring
>     it.** `router.Render` resolves over `aliasClaimants(defs)` — every
>     external model def it was handed, before its own cell/trial/front
>     selection — and `fleetapi.renderPass` calls the exported
>     `router.ResolveAliases` on the full `LoadDefs` result (kept in
>     `declared` before `applyClassPolicy` and `applyFingerprints` drop
>     anything), passing the winners as `router.Options.AliasWinners`. The
>     CALL sits AFTER both overlays on purpose: its error aborts the pass,
>     and `applyFingerprints` is the only thing that evaluates C9's
>     persistent-drift set — resolving first froze that set on its last
>     value while the notify status still called the evaluator live.
>     Both sites are required:
>     the second excludes defs the first can never see. Resolving over the
>     survivors is how a pruned roaming owner silently handed `best-coder`
>     to another cell's model — measured end to end against merged `main`
>     on real llama-swap processes (phase doc §8, L2), a `200 OK` from a
>     model nobody asked for, with nothing in `fleet_status` and nothing in
>     the log.
>   - **The collision error must not heal by attrition.** Two claimants and
>     no `router.alias_owner` is a render error; `renderPass` returns it
>     rather than swallowing it, so it survives one claimant leaving. The
>     error disappearing WAS the repoint. A fleet in that state has a
>     frozen front catalog until a human sets an owner — which is the loud
>     direction, and it already failed every render while both claimants
>     were present.
>   - **The declared alias IS the answer to the roaming-best-node
>     problem**: `router.aliases` + `router.alias_owner: true`, repointed
>     by moving one line in the defs repo. Do not add an automatic
>     re-resolution, a `best-*` namespace, or a per-request fallback.
>     `scripts/fleetlab/gate-c21-alias.sh` serves a real completion through
>     a declared alias and proves the departed one 404s.
>   - **An alias that leaves the catalog is named in the log**
>     (`warnOrphanedAliases`). The prune's own line names a CELL, and the
>     co-claimant's box is still up, so nothing connected "laptop pruned"
>     to "`best-coder` stopped resolving". It fires only for defs the
>     overlays dropped — the exclusions inside `Render` warn there — and
>     only when the dropped def actually WON an alias.

### A carried gap, for whoever owns the next fleetd status surface

**A failing `renderPass` is invisible to every surface.** No field in
`/api/fleet/state`, nothing on the fleet page, no `vibe fleet doctor`
check, no C9 condition — only `slog.Warn("presence-derived render
failed")` inside a container. Pre-existing (the zero-defs gate, the
`front_extras` gate and every `Render` error have always been silent),
but C21 creates the first PERMANENT instance: an unresolvable alias
collision now correctly stays an error instead of healing into a repoint,
so the front catalog can stop tracking the fleet indefinitely while
`front_renders` sits still and every display reads green. Suggested
shape, deliberately not built in a rejection phase: a `front_render`
block on `StateSnapshot` carrying the last failure and its first-seen
time (the `swap_auth` block is the precedent — absent means healthy),
plus a `render.current` doctor check named for what it proves. See §8c
REV2-8.
