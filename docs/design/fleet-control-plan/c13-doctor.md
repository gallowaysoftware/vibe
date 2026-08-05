# C13 — `vibe fleet doctor`: the sit-down-after-two-weeks command

Status: **PR OPEN** (2026-08-05), off `feat/c13-doctor` branched from
`feat/c12-guest-token` at `96f793c` (**C12 merges first**). Feature
commit, ground rule 9's adversarial self-review commit (five findings,
three of them ways this command could have MISREPORTED — see the
[self-review addendum](#adversarial-self-review-addendum)), and the
independent review pass (eight more, six of them the same failure in
the other direction: a check that cries wolf on a healthy fleet — see
the [review addendum](#adversarial-review-addendum-2026-08-05-8-findings-all-fixed-with-regression-tests)).
Every fix in both is mutation-verified. Unit gates
U1–U16 are green on a full local inner loop (`go build`, `go vet`,
`go test -race -count=5`, `golangci-lint run` 0 issues, `gofmt -l .`
silent, `go mod tidy` clean) plus a local end-to-end run of the real
command against a real fleetd. Live gates **L1, L2 and L3 PASSED on
2026-08-05** against the local multi-cell harness
([`scripts/fleetlab`](../../../scripts/fleetlab/README.md)) — four real
cells with real credentials and a real announcer to kill; **L4 is
PARTIAL** (its kill-fleetd half ran; the reboot and the WoL wake need a
physical box and a real NIC). The bonus `defs.parity` gate, whose first
run found a level inversion, **re-ran clean after #36 under
[C17](c17-gate-closure.md)**. See [Execution](#execution).

Backlog item 7 in [fleet-control-futures.md](../fleet-control-futures.md)
§2, the first Medium-tier item:

> **`vibe fleet doctor`** — both-direction token auth per cell, def-SHA
> parity, llama-swap version matrix, cert `notAfter` on the proxy +
> registry, disk free on the front, WoL-armed assertion, roaming-cell
> agent-loaded assertion. The sit-down-after-two-weeks command. Pair with
> a quarterly 15-minute fire drill (kill fleetd, reboot the front, run
> doctor, one WoL wake).

## The friction

Twelve phases have built a control plane that answers *"what is the fleet
doing right now"* extremely well. Nothing answers *"is the fleet still
put together correctly"*.

The difference is not academic. Every one of these is a real failure this
plan has already shipped the machinery to detect and has no surface that
reports it:

- fleetd's container was recreated without its state volume. It minted a
  fresh control-plane token, every client 401s, and the only evidence is
  a startup log line nobody reads and a counter on a status document
  nobody correlates with it (C1's state contract; `deploy/fleetd/README`
  warns about exactly this).
- The laptop's announce agent stopped loading after an OS update. The
  cell is `OFF/AWAY?`, which is also what it looks like on a commute, so
  nothing is wrong until the week you need it.
- One cell's def checkout is three commits behind. Its fingerprints
  match its own defs perfectly; the *fleet* disagrees about what
  `qwen3.6-27b` is.
- A cell was upgraded to a new llama-swap; the rest were not. The
  upgrade ritual (futures item 13) exists precisely so this is a
  canary-then-fleet sequence, and nothing reports where the fleet
  actually sits.
- An advisory lease from a batch job that finished eleven days ago is
  still suppressing that cell's warm schedule and its probes.
- `notify.scope` has been `away` since a trip three weeks ago. Alarms
  fire, are counted, and are delivered nowhere.

Each is individually cheap to check and individually easy to never think
about. That is what a doctor command is for: one sitting, one command,
every "is it still wired up" question at once.

## What this is not

- **Not a fixer.** Doctor reports and stops. There is no `--fix`, no
  `--repair`, no interactive prompt that offers to drain, warm, unload,
  re-render or rotate anything. §1 makes this testable rather than
  merely stated.
- **Not a new access path.** Doctor reaches exactly as far as the
  control plane fleetd already has: the presence table, the state
  snapshot, `hosts.yaml`, and one read-only RPC to cell daemons that
  already declare a `daemon_url`. It does not shell out to SSH, does not
  read cell filesystems, and does not learn a new address for anything.
- **Not `vibe doctor`.** That command (`cmd_doctor.go`) checks whether
  *this box* can run vibe: binaries on `$PATH`, XDG dirs, port
  conflicts, GPU. `vibe fleet doctor` checks whether *the fleet* is
  still assembled. They share a shape and deliberately not a code path
  (different levels — see §2 — and this one crosses a wire).
- **Not a monitor.** It runs when a human or an agent asks. C9 is the
  thing that watches continuously, and its default policy is
  deliberately tiny; doctor is allowed to be exhaustive precisely
  because nobody is paged by it.

## Design

### 1. Read-only, and exactly what that means

**Doctor must be safe to run at any time, including mid-incident, on a
fleet the operator does not currently understand.** That is the whole
value proposition: the command you reach for when something is wrong is
worthless if you have to reason about what it will do first.

So the rule, stated so it can be tested:

> The doctor path issues **no verb**. It never drains, resumes, warms,
> unloads, probes, wakes, renders, queues a piggyback command, writes
> intent, takes or releases a lease, sends a notification, or writes any
> config.

Two honest qualifications, both of which are properties of *reading*
fleet state rather than of doctoring:

1. **Doctor's evidence is the same `StateSnapshot` every other surface
   renders.** It calls `Server.Snapshot`, which probes each cell's
   `/running` and `/v1/models` exactly as `GET /api/fleet/state` does —
   including the last-seen sighting record every state read makes
   (`noteSighting` → `last-seen.json`, age-gated). That file is an
   observation log, not one of the three axes, and a doctor that
   duplicated the snapshot to avoid touching it would violate C9's rule
   that the pager, the page and the CLI render one document. So: doctor
   writes no *fleet state*; a state read's own bookkeeping is unchanged
   by being read from here.
2. **The outbound credential check is one `Status` RPC per cell that
   declares a `daemon_url`** (§4). `Status` is the daemon's read-only
   "what profile is active" call. It is the *lightest authenticated
   read* the control plane has, which is what makes it the right probe.

Enforcement is two tests, not a comment:

- **Behavioural** (`TestDoctor_WritesNoFleetState`): build a Server with
  an intent store, a lease store, a usage ledger and queued piggyback
  commands, hash every state file, run `Doctor`, and assert every hash
  and every queue is unchanged. This is the assertion that matters — it
  would catch a mutation introduced through any path, including one that
  did not exist when this phase was written.
- **Structural** (`TestDoctor_ReachesNoMutatingVerb`): a source scan of
  the doctor files for the mutating identifiers of this package
  (`SetIntent`, `queueCommand`, `QueueCommand`, `queueWarm`,
  `warmViaFront`, `setLease`, `saveIntents`, `persistIntents`,
  `writeAtomic`, `renderPass`, `Enqueue`, `os.WriteFile`,
  `os.Create`…). The same idiom C12 used for `mux.HandleFunc` and C7a
  for `Truncate(24*time.Hour)`. It is the drift guard; the behavioural
  test is the proof.

### 2. Four levels, and UNKNOWN is not OK

Every check reports exactly one of:

| level | means | exit contribution |
|---|---|---|
| `OK` | the question was asked and the answer is good | — |
| `WARN` | the answer is bad but the fleet still works | 2 |
| `FAIL` | the answer is bad and something is broken now | 1 |
| `UNKNOWN` | **the check could not be evaluated**, with the reason | 3 |

`UNKNOWN` exists because this repo has been bitten by absent evidence
reading as a healthy zero more times than by any other single mistake:

- C5's M2: a warm target measured idleness from a fabricated floor, so a
  cell nobody was watching read as idle forever.
- C5/C8's shared guard rule: *a guard that cannot be evaluated is a
  skip*, and an unreported in-flight count is not a zero one.
- C7a: `unmeasured_req` is counted, never summed as zero, and never
  estimated.
- C9: `fingerprint_source: unavailable` rather than an empty mismatch
  set reading as "no drift".

A doctor is the surface where that mistake is most tempting and most
expensive: the whole point is a screen of green, and the cheapest way to
get one is to score "I couldn't check" as "fine". So `UNKNOWN` renders
in its own colour, in its own block, and carries its own exit code.

**The counterpart discipline is naming.** Ground rule 10 says a test's
name is part of its assertion; the same is true of a check's. A check is
named for what it *proves*, never for the question it wishes it could
answer:

- `wake.configured` — not `wake.armed`. The control plane can see that a
  MAC or a fallback command is declared. It cannot see whether the NIC
  has PME enabled, and it must not imply that it can (§9).
- `tls.not_after` — not `tls.valid`. Doctor reads the leaf certificate's
  expiry and deliberately does not verify the chain (§8).
- `auth.inbound` — proven by an authenticated announce that landed, not
  by an intention to announce (§4).

Where a check genuinely proves the strong claim, it says so. Where it
proves a weaker one, its name is the weaker one, so an operator reading
a screen of `OK` is reading true sentences.

### 3. Where the report is computed, and the one new route

**At fleetd.** Almost every input is fleetd-side state: the presence
table, the intent store, leases, the usage ledger, the render loop's
fingerprint set, the warm/probe/notify status blocks, and the position
from which cell credentials are actually used. A CLI-side doctor would
have to re-derive all of it over the wire and would still get the
credential checks wrong, because the credential that matters is the one
*fleetd* resolves, not the one the operator's shell holds.

So:

- `fleetapi/doctor.go` computes a `DoctorReport` from the same server
  the page and the pager read.
- **`GET /api/fleet/doctor`** serves it, declared `AccessTokenOnly` in
  C12's route table and gated `enabled: fleetdRole`. This is C7b's
  precedent exactly: `savings` got its own route rather than a field on
  `/api/fleet/state` because it is an expensive on-demand aggregate and
  every SSE line debounce-refreshes state. Doctor is more so — it makes
  N outbound RPCs and M TLS dials. It must never ride a document the
  page polls.
- **`fleet_doctor`** in the MCP facade renders the same report for
  agents, like every other read tool.
- **`vibe fleet doctor`** fetches it and renders it for humans (§10),
  adding the handful of checks that are only true *at the client* (§4's
  `auth.client_env`).

Adding a route means adding a row to `fleetapi/routes.go` with a
declared `Access` — one line, one decision, and `AccessTokenOnly` is the
answer here without argument: the report names config paths, credential
sources, disk figures and cert expiries. It is the least guest-shaped
document in the fleet. It also goes into
`daemon/fleet_registry_test.go:TestDaemon_FleetRegistryOff_NoMCP`'s
probe list, per AGENTS.md's rule that every new fleetd route does.

### 4. Both-direction token auth, per cell

The futures item's headline. The two directions have completely
different evidence, and conflating them is how a "token check" ends up
proving nothing.

**Inbound (cell → fleetd) is already proven, continuously, by the
announce.** `POST /api/fleet/announce` is bearer-authed like every other
TCP route; a cell whose token is wrong gets a 401 and never appears in
the presence table. So:

| observation | verdict |
|---|---|
| fresh announce within `3×interval + 5s` | `OK` — "authenticated Ns ago (seq N)" |
| announced before, now stale | `WARN` — was authenticating, now silent; names both causes (box away vs. announcer died vs. token rotated) |
| never announced | `UNKNOWN` — this cell may not run an announcer at all; probe-derived availability is the fallback (C1 semantics) |

Nothing new is built for this. That is the point: the heartbeat *is* the
credential test, and reading it that way costs zero plumbing.

One honesty note the check carries when it applies: `daemon.auth_rejected`
counts 401s **without attributing them to a cell** — fleetd does not know
which caller failed, because a failed bearer check happens before any
body is parsed. So when `auth_rejected > 0` *and* some cell has never
announced, the check says so: the rejections may be that cell's announcer
holding a stale token, and doctor cannot tell. Reporting the ambiguity is
the honest form; inventing an attribution is not.

**Outbound (fleetd → cell) requires actually trying.** For each cell
with a `daemon_url`, doctor issues one `Status` RPC using the credential
resolved by **the same resolver the actuation verbs use**:

| result | verdict |
|---|---|
| 200 | `OK` — "credential accepted (source: cells.X.token_file)" |
| 401 / `Unauthenticated` | `FAIL` — the credential is wrong; names the source so the fix is one file |
| transport error / timeout | `UNKNOWN` — cannot distinguish a wrong token from a box that is off; says which |
| no `daemon_url` | `OK` **only when inbound is fresh** — "no outbound path configured; the announce piggyback queue is this cell's actuation channel (C3)", else `UNKNOWN` |

That last row is the one worth arguing about. After C3, `daemon_url` is
an optimization rather than a requirement, so a cell without one is not
misconfigured — and scoring a deliberate configuration as `UNKNOWN`
forever teaches the operator to ignore the level that exists to be
noticed. The resolution: **everything configured works** is `OK`; the
verdict is `UNKNOWN` only when a question with an answer could not be
answered. An announce-only cell that is *also* not announcing has no
proven credential path in either direction, and that is genuinely
unknown.

**The front is the one structural exception, in both directions.** In
the reference deployment (`deploy/front`) the front is a llama-swap
container with no vibe daemon and no announcer: fleetd reaches it by
probe and through the render mount, so it holds no credential either
way and has no def checkout of its own (its llama-swap config *is*
fleetd's render). `auth.inbound`, `auth.outbound` and
`versions.reported` therefore treat a non-announcing front as OK /
skipped rather than UNKNOWN. This is the one place the phase trades a
strictly-honest UNKNOWN for signal, and the trade is deliberate: three
permanent UNKNOWNs on a correctly-configured fleet is how an operator
learns to ignore the level that exists to be noticed. A front that
*does* announce, or that *does* declare a `daemon_url`, is checked like
any other cell.

**Using the same resolver is load-bearing**, not tidiness. A doctor that
resolves credentials its own way tests its own code. C6's NIT-E left the
two existing resolvers deliberately divergent:

- fleetmcp (fleetd, long-lived): `cells.X.token_file` → `$VIBE_TOKEN` →
  local token file. Per-cell file **first**, because one env var in a
  long-lived process must not silently void every per-cell token in
  `hosts.yaml`.
- the CLI (a human typing one command): `$VIBE_TOKEN` → `cells.X.token_file`
  → local token file. Explicit env **first**, because that is what an
  operator means by exporting it.

Both are correct for their context and neither is discoverable from a
401. C13 extracts them into one place — `fleetcfg.CellCredential(cell,
env, preference, localToken)` returning `(Credential, error)` — with the
two preferences as named values, and repoints both call sites at it with
behaviour unchanged (the existing tests pin that). One rung is
deliberately *added* while merging them: an **empty** `token_file` is now
an error like an unreadable one. It is the same failure with a different
spelling — a credential that resolves to `""` produces an opaque 401
from a remote box — and doctor turns it into a named FAIL
(`CellAuthResult.CredentialErr`) rather than the UNKNOWN a transport
failure earns, because a local misconfiguration is definitely known. Doctor then reports
the *source*, which is the fact an operator actually needs:

```
OK    auth.credential   gpu-cell   cells.gpu-cell.token_file (~/…/gpu.token)
WARN  auth.credential   mac-cell   $VIBE_TOKEN in fleetd's environment — no
                                   cells.mac-cell.token_file, so this cell
                                   shares a fleet-wide credential
```

And the C6 finding gets the surface it always needed, in **both**
environments:

- `auth.credential` (server): `$VIBE_TOKEN` set in fleetd's own
  environment while cells declare `token_file`s → `WARN`, stating that
  the per-cell file wins *here* (post-C6) and that the env var is a
  fleet-wide credential for every cell without one.
- `auth.client_env` (client-side, CLI only): `$VIBE_TOKEN` set in the
  *doctor's own* environment while `hosts.yaml` declares per-cell
  `token_file`s → `WARN`. This one is still live: `vibe cell drain
  --cell X` from that shell uses the env token and ignores
  `cells.X.token_file` entirely. Before C6 this was true of fleetd too,
  which is what made "the whole `cells.X.token_file` config is dead" a
  real finding rather than a hypothetical.

### 5. def-SHA parity, and checking the evidence exists first

`AnnounceVersions` (C3 §1) carries `llama_swap`, `vibe`, `defs_sha` and
`defs_dirty`. **Before asserting parity, doctor asserts the block is
populated** — otherwise "every cell agrees" is what a fleet of silent
cells looks like.

`versions.reported`, one row per cell:

| observation | verdict |
|---|---|
| block present with a `defs_sha` | `OK`, listing the vibe build |
| announcing, block absent or empty | `UNKNOWN` — names the producer gap (see below) |
| not announcing | `UNKNOWN` — no announce, no versions |

`defs.parity`, one row for the fleet:

| observation | verdict |
|---|---|
| every cell reporting a SHA reports the same one, and it matches fleetd's own defs checkout when fleetd has one | `OK` |
| two or more distinct SHAs | `WARN`, listing cell → SHA |
| a cell reports `defs_dirty: true` | that cell is `UNKNOWN` — **a dirty checkout's SHA does not describe what is running**, so it can neither agree nor disagree. Scoring it `OK` because the string matches is exactly the absent-evidence mistake §2 exists to prevent |
| no cell reports a SHA | `UNKNOWN` |

**A producer gap this phase surfaces rather than papers over.** Two
announcers exist: the daemon's loop (`daemon/announce.go:fleetVersions`)
fills `vibe` + `defs_sha` + `defs_dirty`; `vibe fleet announce` — the
slim announcer for cells with no daemon, which is the *heavy cell*'s
deployment — passes no `Versions` provider at all, so it announces
none of it. C13 wires the same provider into the slim announcer
(`Versions` and `Capacity` both — §7 needs the second), which is the one
piece of genuinely new plumbing in this phase and is ~20 lines shared
with the daemon.

### 6. The llama-swap version matrix, and a field with no producer

`AnnounceVersions.LlamaSwap` has existed since C3 and **nothing has ever
written it**. Neither announcer fills it; the field is a reservation, like
`AnnounceModel.Probe` was before C8.

The tempting move is to guess an endpoint — dial the cell's llama-swap
and read a version from some admin path. This phase declines, for the
reason the boundary rule exists: the repo cannot verify the endpoint
(no fleet reachable from CI or from the implementing environment), and a
doctor check that silently reports `UNKNOWN` because it is calling a
path that does not exist is *worse* than one that reports `UNKNOWN`
because nothing fills the field — the first is indistinguishable from a
cell being down.

So `versions.llama_swap` reports:

| observation | verdict |
|---|---|
| two or more distinct versions across cells | `WARN` — the upgrade ritual (futures 13) says canary → gate → fleet, and this is the mid-state |
| one version across every reporting cell | `OK`, naming it |
| no cell reports one | `UNKNOWN`, **with the precise reason**: "no announcer populates `versions.llama_swap` today; the field is reserved. A cell can fill it by ⟨the provider hook⟩." |

The check is built now because the matrix is the deliverable and the
data path is one provider away; the honest `UNKNOWN` is what an unwired
field must produce until somebody wires it on a box where the endpoint
can be verified. This is written down here so the next agent does not
read the `UNKNOWN` as a bug in doctor.

### 7. Disk free — and whose disk it actually is

The futures item says "disk free on the front". The front is a
llama-swap container (`deploy/front`); fleetd is a *different* container
(`deploy/fleetd`) that may or may not share a host with it. Doctor must
not report fleetd's disk as the front's.

Three subjects, three separate rows under `disk.free`, each naming what
it measured:

1. **fleetd's own state dir.** `intent.json`, `leases.json`,
   `usage.jsonl` and `last-seen.json` live here, and C6's rule is that a
   failed persist leaves memory unresolved — a full state filesystem
   turns every intent write into a retry loop. `FAIL` when the directory
   is missing or below 256 MiB free, `WARN` below 2 GiB.
   **Writability is deliberately not asserted**: establishing it means
   writing a probe file, and §1 promises this command writes nothing. A
   missing directory is the failure the state contract is actually
   about, and that one is a stat.
2. **The front's render mount**, and only when `fleet.front_config` is
   set — that key *is* the declaration that fleetd shares a filesystem
   with the front's config (C3's render mount contract). Same
   thresholds. Without the key, this row is absent and
   `front.render_mount` (§13) reports the consequence.
3. **Each cell's announced `capacity.disk_free_gb`**, which is the cell
   filesystem the *models* live on. `WARN` below 10 GiB — a cell that
   cannot pull a 24 GB quant is a cell that has quietly left the
   membership-churn loop. `UNKNOWN` for cells that announce no capacity
   block (the slim-announcer gap §5 also closes).

The threshold constants are named and commented in one place. They are
opinions; the numbers are always printed beside the verdict so an
operator can disagree with the verdict and still use the number.

### 8. Cert `notAfter` — read, deliberately not verified

Every fleet URL doctor knows about (`cells.*.url`, `cells.*.daemon_url`,
`fleetd_url`) that is `https://` gets one TLS dial, and the leaf
certificate's `NotAfter` is reported:

| observation | verdict |
|---|---|
| no `https://` URL anywhere in the fleet | `OK` — "no TLS endpoints configured; every fleet URL is plain HTTP on the LAN". This is a definitive negative, not an absence of evidence |
| expiry more than 21 days out | `OK`, with the date |
| expiry within 21 days | `WARN`, with the date and days remaining |
| already expired | `FAIL` |
| dial fails | `UNKNOWN` — a host that is off and a broken TLS config are indistinguishable from here |

**The chain is deliberately not verified**, and the check's name says
`not_after` rather than `valid` because of it. A LAN fleet's certs are
self-signed or issued by a household CA that fleetd's container may not
carry; failing them would produce a permanent red that means nothing.
The dial therefore skips verification, reads the leaf, and the message
states that the chain was not checked — so `OK` cannot be misread as
"this endpoint's TLS is trusted".

### 9. WoL: what is checkable, and what the fire drill is for

`wake.configured`, one row per cell that is not the front:

| observation | verdict |
|---|---|
| `wake:` block present (MAC validated at load, or a fallback `cmd`) | `OK` — "wake configured (MAC / cmd). Whether the NIC is armed is not observable from the control plane; the quarterly fire drill's one wake is that test" |
| cell absent right now with no `wake:` block, class `always_on` or `opportunistic` | `WARN` — nothing can bring this box back without walking to it |
| cell present with no `wake:` block | `OK` — nothing to wake |
| roaming class with no `wake:` | `OK` — a laptop that left the building is not woken by a magic packet |

The strong assertion the futures item asks for — *is WoL armed* — is not
reachable from the control plane, and doctor says so in the message
rather than approximating it. **Sending a magic packet to find out is a
mutation** (`POST /api/fleet/wake` exists and doctor does not call it),
which is why the futures entry pairs this command with a fire drill in
the first place. Doctor's job is to make sure the *configuration* half
is right before the drill tests the other half.

### 10. The roaming-cell agent-loaded assertion — the one real derivation

This is the check that could not exist without the substrate, and it is
worth the phase on its own.

For a cell whose class is `roaming` (and any cell with a `host_probe`):

| host_probe | fresh announce | verdict |
|---|---|---|
| up | yes | `OK` — "announcer running (last heartbeat Ns ago)". The agent is loaded, *proven* |
| up | no | `WARN` — **"the box answers but is not announcing"**: the announce agent is not loaded / not running, or it is running and being rejected. Names both, and cross-references `daemon.auth_rejected` when it is non-zero — because if rejections are climbing, the second explanation is the likely one |
| down | — | `UNKNOWN` — the box is away; whether its agent would load is unknowable from here |
| no host_probe | — | `UNKNOWN` — without a host probe, "cell down" and "host down" are the same observation (C1's `OFF/AWAY?`) |

Row 2 is a *diagnosis*, not a reading: fleetd just proved it can reach
the box at L4 and the box is not heartbeating, so the announcer is the
thing that is broken. It is exactly the failure mode that otherwise
hides behind `OFF/AWAY?` for a week, and it composes entirely from
evidence C1 and C3 already collect.

### 11. The hygiene checks (why "two weeks" is in the name)

These need no new evidence at all; each is a verdict over a status block
that already exists.

| check | question | `WARN` when |
|---|---|---|
| `fleetd.token` | did fleetd mint a *new* control-plane token at this start? | minted at all; minted **plus** `auth_rejected > 0` is the unmounted-state-volume signature and says so. `FAIL` never: fleetd is serving |
| `auth.rejections` | is some client holding a stale token? | `daemon.auth_rejected > 0`, stating that fleetd cannot attribute a rejection to a cell (the bearer check runs before any body is parsed). The C12 guest counter rides the detail, never folded in |
| `intent.hygiene` | is a drain/resume request stuck unacked? | a request older than `3×interval` with no echo, or a `DRAINED?` / `INCONSISTENT` display state (reported, **never acted on** — invariant 2) |
| `leases.age` | is a forgotten lease suppressing policy? | any active advisory lease older than 24h. Holds (C11) are listed with their remaining time and are capped at 24h by construction |
| `fingerprint.drift` | is a cell serving a model with the wrong flags? | the render loop's mismatch set is non-empty. `UNKNOWN` when the render loop is not running — C9's `fingerprint_source: unavailable`, verbatim |
| `probe.verdicts` | is anything measurably slow? | any `degraded` model (with C8's runbook: probe → `unload_model` → probe), or a probe target that has been `skipped` for its whole life, with the skip reason |
| `warm.policy` | is the warm policy actually running? | a target `skipped` with a standing reason, or a schedule with no resolved `next_fire`. Always prints the resolved zone, because a wrong `TZ` is the failure C4 built the field to make visible |
| `usage.flow` | is the ledger being fed? | an announcing non-front cell contributing no usage rows (C7a's `store:` extras missing → the activity log is a 1000-row in-memory ring), or `lost_rows > 0` |
| `notify.reach` | will anyone be told? | scope has been `away` for more than 7 days, with the suppressed count. Unconfigured is `OK` with a note — C9's position is that no webhook is the status quo, not a regression |

### 12. Output: one report, two renderings, four exit codes

**Human (default).** Sorted by severity — `FAIL`, then `WARN`, then
`UNKNOWN`, then `OK` — because the reader at 2 a.m. reads top-down and
stops when they find the thing. The `OK` block stays in the output
rather than being hidden behind a flag: "I checked these eleven things
and they are fine" is half the value of a sit-down command.

```
vibe fleet doctor — fleetd http://10.x.x.x:9001 — 2026-08-05 22:14 EDT
4 cells · 21 checks · 1 FAIL · 3 WARN · 2 UNKNOWN · 15 OK

FAIL    auth.outbound     gpu-cell   401 from the cell daemon: credential rejected
                                     source: cells.gpu-cell.token_file (~/…/gpu.token)
                                     fix: that file and the cell's own token file differ

WARN    leases.age        gpu-cell   lease held by "overnight-batch" for 11d
                                     it suppresses this cell's warm schedule and probes
…
UNKNOWN versions.llama_swap          no announcer populates versions.llama_swap today
```

**Structured (`--json`).** The whole `DoctorReport`, unmodified — the
same document the MCP tool returns and the same one the HTTP route
serves, plus the client-side checks the CLI adds. One shape for humans,
agents and scripts (C9's rule).

Four flags and no more: `--api` (the usual resolution order), `--json`,
`--problems-only` (hide the OK block), `--exit-zero` (for a wrapper that
reads the output itself) and `--timeout`. There is no `--fix`, no
`--cell`, and no flag that turns a level off.

**Exit codes**, so a cron entry or an agent can branch without parsing:

| code | meaning |
|---|---|
| 0 | every check `OK` |
| 1 | at least one `FAIL` |
| 2 | at least one `WARN`, no `FAIL` |
| 3 | at least one `UNKNOWN`, no `FAIL` and no `WARN` — *the report is incomplete* |

3 is deliberately not "success". A fleet with a laptop that is out of
the house genuinely produces an incomplete report, and the code says so
instead of pretending the questions were answered.

### 13. When fleetd itself is down

The fire drill this command is paired with begins by killing fleetd. A
doctor that fails blind at that moment is the wrong tool.

So `vibe fleet doctor` degrades exactly like `vibe cell status`
(`renderDegraded`): fleetd unreachable is itself a `FAIL` row naming the
resolved address and the transport error, every server-side check
reports `UNKNOWN` with "fleetd unreachable" as the reason, and the
checks that are true at the client still run:

- `hosts.yaml` parses and validates (a broken registry is the reason
  fleetd is not coming back, surprisingly often),
- `auth.client_env` (§4),
- a direct HTTP probe of each cell's `url`, so the operator still learns
  which boxes are up while the control plane is down.

An https `fleetd_url` whose certificate has expired needs no separate
check here: the transport error IS the diagnosis, and it rides the
`fleetd.reachable` row's detail verbatim (`x509: certificate has
expired`).

`front.render_mount` deserves its own line here because it is the check
whose absence is invisible: without `fleet.front_config`, the render
loop never runs, so the catalog is not presence-derived, fingerprints
are never enforced, and C9's fingerprint alarm has no evaluator. That is
`WARN` with all three consequences named, not a silent `OK`.

## Files

| file | change |
|---|---|
| `internal/vibe/fleetapi/doctor.go` | new: `DoctorReport`, `DoctorCheck`, `Level`, `Server.Doctor(ctx)`, every server-side check |
| `internal/vibe/fleetapi/routes.go` | one row: `GET /api/fleet/doctor`, `AccessTokenOnly`, `enabled: fleetdRole` |
| `internal/vibe/fleetcfg/fleetcfg.go` | `CellCredential(cell, env, pref, localToken)` — the one credential resolver, with the two documented preferences |
| `internal/vibe/fleetmcp/actuate.go` | `cellClient` repointed at it (behaviour unchanged) |
| `internal/vibe/fleetmcp/doctor.go` | new: the `fleet_doctor` tool |
| `internal/vibe/cli/cmd_cell_actuate.go` | `resolveCellClient` repointed at it (behaviour unchanged) |
| `internal/vibe/cli/cmd_fleet_doctor.go` | new: `vibe fleet doctor`, the renderer, the client-side checks, the degraded path |
| `internal/vibe/daemon/doctor.go` | new: the host facts (`statfs`, token-minted, `$VIBE_TOKEN` presence, the def checkout) and the outbound credential probe |
| `internal/vibe/daemon/daemon.go` | wires both into `fleetapi.Options`; records `tokenMinted` at startup |
| `cmd/vibe/main.go`, `internal/vibe/cli/root.go` | `cli.ExitCode(err)` so a command whose OUTPUT is the message can choose its status without main printing an error line on top of it |
| `internal/vibe/daemon/announce.go` | the versions/capacity provider extracted so the slim announcer shares it |
| `internal/vibe/cli/cmd_fleet.go` | slim announcer gains the same `Versions` + `Capacity` providers (§5) |
| `internal/vibe/daemon/fleet_registry_test.go` | the new route joins the fleetd-only probe list |
| `deploy/fleetd/README.md` | the command and the quarterly fire drill, where an operator finds them |
| `AGENTS.md`, this plan's `README.md`, `fleet-control-futures.md` | the C13 invariants, the status row, and backlog item 7 marked SHIPPED |

`fleetapi` gains no import of `vibeclient`: the outbound prober is
injected through `Options` exactly as `daemonInfo` is, which keeps the
observability substrate free of the RPC client and makes the check
trivially fakeable in tests.

## Acceptance gates

Mechanical (become tests in-repo):

- **U1** `Doctor` writes no fleet state: intent, lease, ledger and
  notify-scope files byte-identical after a run; command queues
  unchanged (§1, behavioural).
- **U2** No mutating identifier appears in the doctor source (§1,
  structural).
- **U3** Every check emits one of the four levels, and no check emits a
  zero-value level (the C12 `AccessUndecided` discipline applied to
  verdicts).
- **U4** `auth.inbound`: fresh announce → `OK`; stale → `WARN`; never
  announced → `UNKNOWN`, and the message names the `auth_rejected`
  ambiguity when the counter is non-zero.
- **U5** `auth.outbound`: 200 → `OK` naming the credential source; 401 →
  `FAIL`; transport error → `UNKNOWN`; no `daemon_url` + fresh announce
  → `OK`; no `daemon_url` + no announce → `UNKNOWN`.
- **U6** The credential resolver's two preferences behave exactly as the
  pre-C13 call sites did (the existing fleetmcp and CLI tests pass
  unchanged, plus a direct table test of both orders).
- **U7** `defs.parity`: identical SHAs → `OK`; divergent → `WARN` listing
  cells; every reporter `defs_dirty` on one SHA → `UNKNOWN`; none
  reported → `UNKNOWN`. **A dirty checkout may only ADD concern**: a
  dirty cell on a DIFFERENT SHA stays `WARN` (and so does a dirty fleetd
  on a different SHA) — see the
  [live-gate addendum](#live-gate-addendum-2026-08-05-dirtiness-silenced-a-real-divergence).
- **U8** `versions.llama_swap` with no reporting cell → `UNKNOWN` whose
  message names the missing producer (not a silent `OK`).
- **U9** `disk.free`: the three subjects are reported separately and the
  front row is absent when `fleet.front_config` is unset.
- **U10** `tls.not_after`: no https URLs → `OK` (definitive negative);
  an expiring cert → `WARN`; an expired one → `FAIL`; an unreachable one
  → `UNKNOWN`. Exercised against an `httptest` TLS server with a
  generated cert.
- **U11** `roaming.announcer`: host up + no fresh announce → `WARN`
  naming both causes; host down → `UNKNOWN`; no host_probe → `UNKNOWN`.
- **U12** Exit codes 0/1/2/3 map to the four severities as documented.
- **U13** `--json` emits the same document the HTTP route serves.
- **U14** `GET /api/fleet/doctor` is token-only (guest 401), and 404s on
  a daemon without `fleet_registry`.
- **U15** fleetd unreachable → the degraded path runs, reports `FAIL`
  for fleetd, `UNKNOWN` for server-side checks, and still evaluates the
  client-side ones.
- **U16** The full inner loop green under `-race -count=5`.

Live (written as "need the real fleet"; L1–L3 turned out to need a
multi-*cell* fleet, which the 2026-08-05 harness supplies — see the
[gate results](#gate-results) for what ran):

- **L1** Run against the real fleet from a workstation: every check
  produces a verdict, and every `UNKNOWN` has a reason an operator can
  act on. The gate is *judgement*: does the screen tell the truth about
  a fleet whose state is already known?
- **L2** Induce a wrong credential (point one cell's `token_file` at a
  stale value) and confirm `auth.outbound` FAILs for that cell alone,
  naming the source file.
- **L3** Stop a roaming cell's announce agent while the box stays up;
  confirm `roaming.announcer` WARNs within one staleness window and
  recovers to `OK` when reloaded.
- **L4** The quarterly fire drill the futures entry pairs with this
  command: kill fleetd → run doctor (degraded path, §13) → restart
  fleetd → reboot the front → run doctor → one WoL wake. The drill is
  the test `wake.configured` cannot be.

## Execution

Landed 2026-08-05 on `feat/c13-doctor` off C12's branch at `96f793c`:
the feature commit plus ground rule 9's adversarial self-review commit.
About 1,500 lines including tests.

### What the design got right, and the four things the code changed

Written before the implementation, so the differences are the honest
part:

1. **`disk.free` on the state dir does not assert writability.** The
   design said it would; establishing writability means creating a probe
   file, and §1's promise is that this command writes nothing. The check
   stats the directory (missing is a FAIL — that *is* the state-contract
   failure) and reports free bytes. Recorded in §7.
2. **The front needed a structural exception in three checks.** The
   first end-to-end run against a real fleetd produced three permanent
   UNKNOWNs for the front on a perfectly-configured fleet — inbound
   auth, outbound auth and `versions.reported` — because the reference
   front is a llama-swap container with no daemon and no announcer.
   Permanent UNKNOWNs on a healthy fleet are how an operator learns to
   ignore the level, so the front is OK/skipped in those three when it
   does not announce, and checked normally when it does (§4).
3. **`versions.llama_swap` has no producer at all.** The design
   anticipated the field might be unpopulated; it is unpopulated
   *everywhere*, by both announcers, and has been since C3. The check
   ships reporting UNKNOWN with the missing producer named, and the
   phase deliberately does NOT guess at a llama-swap version endpoint it
   cannot verify (§6).
4. **The slim announcer gained the versions + capacity providers.** The
   one piece of genuinely new plumbing: `vibe fleet announce` passed no
   `Versions` and no `Capacity`, so the heavy cell — the box whose def
   checkout is most likely to drift, and whose disk holds the biggest
   weights — was the one cell that reported neither. `daemon.FleetVersions`
   and `daemon.FleetDiskCapacity` are now shared by both announcers.

### Local end-to-end run (not a fleet gate)

The command was run against a real `vibe daemon` in the fleetd role on
this box, over a three-cell `hosts.yaml` whose cells do not exist:
27 checks, 0 FAIL / 2 WARN / 11 UNKNOWN / 14 OK, exit 2, every UNKNOWN
carrying a reason. That exercised the whole path — the route, the
outbound credential probe against an unreachable daemon, the TLS
none-configured branch, the renderer and the exit codes — and it is what
surfaced finding 2 above. **It is not a live gate**: the cells were
fake, no model was resident, and nothing about real credentials, real
certs or a real roaming laptop was tested.

### Gate results

| gate | result |
|---|---|
| U1–U16 | PASS (`go build`, `go vet`, `go test -race -count=5 ./...`, `golangci-lint run` 0 issues, `gofmt -l .` silent, `go mod tidy` clean) |
| U1 / U2 mutation-verified | PASS — injecting a `SetIntent` call into `Doctor` fails both the behavioural test (`intent.json changed across a doctor run`) and the source scan, at the right line |
| L1 | **PASS (2026-08-05, local multi-cell harness — [`scripts/fleetlab`](../../../scripts/fleetlab/README.md)).** 38 checks over 4 real cells: 0 FAIL / 4 WARN / 8 UNKNOWN / 26 OK, exit 2. The gate is judgement, and the judgement holds: every UNKNOWN named the missing evidence *and* a fix — `versions.llama_swap` ("a reserved announce field and no announcer populates it yet — this is a missing producer, not an unreachable fleet"), `disk.free` ("the announced disk figure is zero … a doctor must not invent either reading"), `probe.verdicts` ("nothing measures throughput on this fleet — no cell has produced a verdict, so *nothing is slow* is unproven"). The write-nothing promise was verified by snapshot/rerun/snapshot around four runs (3× `--json` + 1× the HTTP route): `intent.json`, `leases.json`, the usage ledger, the notify-scope file, `last-seen.json`, a cell's `cell-intent.json` and `model-probe.json` all byte-identical afterwards, cell-side probe `attempts` unmoved at 4, residency unchanged. **Caveat:** a lab fleet has no TLS, no real weights disk and no roaming laptop, so those checks were exercised in their not-configured / not-reporting branches. |
| L2 | **PASS (2026-08-05, same harness).** The file behind `cells.bravo.token_file` was overwritten while bravo's daemon held the original cached: `FAIL auth.outbound bravo — the cell daemon REFUSED fleetd's credential / source: cells.bravo.token_file (…) unauthenticated: 401 Unauthorized / fix: drain/resume/unload for this cell will 401 until the two sides carry the same token.` The other three cells' `auth.outbound` stayed OK — the failure was scoped to one cell and named its source file, which is the whole gate. |
| L3 | **PASS (2026-08-05, same harness).** Only the roaming cell's announcer was SIGKILLed; its host_probe and llama-swap kept answering. `WARN roaming.announcer charlie — the box answers but is not announcing / fleetd just reached this box at L4, so the announce agent is not loaded, not running, or is being rejected. auth.rejections is 24 and climbing, which makes the rejected explanation the likely one.` Both causes named, as designed. Then the host itself was taken down and the same check correctly became `UNKNOWN … box is away; whether its announcer would load is unknowable from here` — the distinction the check exists to make. |
| L4 | **PARTIAL (2026-08-05).** The kill-fleetd half ran: pointed at a dead port the CLI produced `FAIL fleetd.reachable` with `fix: inference is unaffected — fleetd is read-and-request-only` and `UNKNOWN fleetd.checks — every fleet-side check is unevaluated`, while still evaluating the client-side ones. **The rest needs metal**: a physical box to reboot and a real NIC to receive a magic packet. `wake.configured` is by design a configuration check because arming is not observable from here, and this drill is the one test that could contradict it. Nothing local substitutes. |
| bonus — defs parity | **PASS (re-run 2026-08-05 after #36, C17, `scripts/fleetlab/gate-c13-parity.sh`).** The first run found the inversion described in the note below; #36 fixed it (divergence now decides over every cell that reports a SHA, agreement only over the clean ones), and the exact sequence was re-run against a real four-cell fleet. (1) One clean SHA: `OK — every reporting cell is at 515d1bd.` (2) charlie given its own checkout one commit ahead: `WARN — cells disagree about the def checkout / 515d1bd: alpha, bravo · fb82b92: charlie. fleetd's own def checkout is at 515d1bd.` (3) **an uncommitted edit in the diverged checkout keeps the WARN**, and appends `Working tree dirty (the SHA names the base commit, not what is running): charlie (fb82b92).` (4) Control — the same dirt with the checkouts AGREED is `OK` with the dirt named. Dirty-and-diverged is now strictly worse than clean-and-diverged; the level no longer inverts at the moment a cell becomes more drifted. |
| bonus — U13 / U15 | **PASS.** `--json` and the HTTP document agree on every `{id, subject, level}` except the CLI's extra `auth.client_env` check, which inspects the calling shell's `$VIBE_TOKEN` and cannot exist server-side (`cli/cmd_fleet_doctor.go:239`). And `auth.client_env` earned its place: it WARNed `$VIBE_TOKEN is set in THIS shell and overrides every per-cell token_file … for: bravo`, which predicted the exact 401 that `vibe cell drain bravo --yes` then produced, and which succeeded with the variable unset. |

**The one thing the live run found.** `doctor.go:582` downgrades a
**known** divergence from WARN to OK when the diverged cell's checkout
goes dirty: two checkouts disagreeing reports `WARN defs.parity`, and
adding an uncommitted edit to the already-diverged one reports
`OK defs.parity — every reporting cell is at a1cde82. Uncomparable
(working tree dirty): bravo (c2b8449).` The level an operator scans
inverts at the moment the cell becomes *more* drifted. It is consistent
with the pinned test (`TestDoctor_DefsParityTreatsADirtyCheckoutAsUncomparable`
— dirty can neither agree nor disagree) and the excluded cell is named
in the OK detail, so this is a judgement call rather than a defect. But
if the intent is "a dirty tree is unknowable", a cell whose last
*commit* is already known to differ could keep the WARN and say so.

**Resolved.** #36 took that reading: `defs.parity` now decides
DIVERGENCE over every cell that reports a SHA and AGREEMENT only over
the clean ones, so dirty-and-diverged stays a WARN with the dirt named
beside it. C17 re-ran the exact four-step sequence against a real
four-cell fleet on 2026-08-05 and watched the level hold — see the
bonus row in the [gate table](#gate-results).

### Adversarial self-review addendum

Ground rule 9's separate pass, against the feature commit. Five
findings, all fixed with mutation-verified regression tests. The theme
is the one this command is most exposed to: **a diagnostic tool that
misreports is worse than no tool**, and three of the five were ways
this one could have lied.

**REV-1 — the outbound credential probes ran serially** (would
misdiagnose). Each probe is bounded at 5s and the report at 20s, so a
fleet with three boxes off spends the whole deadline dialling them —
and every cell *behind* those three then reports "context deadline
exceeded" for a call that was never made. On a fleet whose whole point
is opportunistic and roaming cells, that is the normal case, not the
edge one. Fixed by fanning out exactly like `Snapshot`'s own cell round.
Pinned by `TestDoctor_OutboundProbesRunInParallel` (mutation-verified:
serial takes 1.05s for 7 cells at 150 ms each and fails the bound).

**REV-2 — the TLS dial ignored the report's context.**
`tls.DialWithDialer` takes a `net.Dialer` and no context, so a client
that disconnected mid-report left one 3s dial per https endpoint
running, and the `--timeout` flag did not bound them. Replaced with
`tls.Dialer.DialContext`. Pinned by
`TestDoctor_TLSDialHonoursTheReportContext` (a pre-cancelled context
must return in well under the dial timeout).

**REV-3 — `DoctorHost.Version` was collected and never rendered.** The
daemon filled fleetd's own build and def SHA and nothing reported them,
which is exactly the asymmetry `defs.parity` exists to catch: the box
that writes the front's render is part of the fleet's version story.
Now a `versions.reported` row of its own, carrying the DIRTY flag like
any cell's.

**REV-4 — the CLI's doctor fetch reused the state-sized HTTP client**
(would misdiagnose, and worst of all). `resolveFleetd`'s client carries
a 10s timeout sized for `/api/fleet/state`; the doctor report is bounded
server-side at 20s and legitimately takes longer on a fleet with absent
cells. A fleetd that was merely SLOW would have been reported as DEAD,
with the degraded report's confident `FAIL fleetd did not answer` — the
single worst output this command can produce, because it is rendered at
exactly the moment an operator is deciding whether fleetd needs
restarting. The request context (the `--timeout` flag) is the bound now;
the transport is still the shared one so a unix-socket target still
dials the socket. Pinned by
`TestFleetDoctorFetchIgnoresTheStateSizedTimeout` (mutation-verified).

**REV-5 — the local target rendered as `http://vibe.local`.** The
unix-socket target carries a placeholder host so the URL parses;
printing it as the address the report came from invites someone to curl
a host that does not exist. Renders as "the local daemon (unix socket)".

Two things the pass looked at and deliberately left:

- **The report is not on the fleet page.** C4's page is a phone in a
  hallway with three fat buttons; a 27-row audit is not a phone screen,
  and adding it would mean either a new route or a doctor-shaped view of
  a document the page does not otherwise fetch. The MCP tool covers the
  agent case and the CLI covers the desk.
- **`--json` does not sort by severity while the human renderer does.**
  Deliberate: the JSON keeps emission order (grouped by subject) so two
  runs diff cleanly, and a consumer that wants severity order has the
  level on every check.

## Adversarial-review addendum (2026-08-05, 8 findings, all fixed with regression tests)

Ground rule 9's independent pass over the feature commit *and* the
self-review commit. Every fix below is mutation-verified: the production
change was reverted and the named test watched to FAIL, then restored.

One theme runs through six of the eight, and it is the theme this
command is most exposed to: **a diagnostic that cries wolf is a
diagnostic nobody reads.** The phase doc argues this for `UNKNOWN` (§2:
"a permanent UNKNOWN on a healthy fleet teaches the operator to ignore
the level") and then shipped four checks that emit a permanent `WARN` on
a healthy fleet — including two that contradict a declaration the
operator made with this same control plane.

- **The TLS dials were serial and shared the report's one deadline
  (MAJOR — the report described dials it never made).** This is REV-1
  verbatim, one subsystem over: the self-review fanned out the outbound
  credential probes and left `checkTLS` in a queue. Measured, ten
  blackholed https endpoints at `tlsDialTimeout` each: the report
  consumed its **entire 20s budget** (20.007s) and the endpoints behind
  the queue were reported as `TLS dial failed — a host that is off and a
  broken TLS listener are indistinguishable from here` for a handshake
  that was never attempted. Fixed by fanning out exactly like
  `checkAuth`. Two independent pins:
  `TestDoctor_TLSDialsRunInParallel` (six blackholed endpoints inside a
  three-dial budget; serial takes 0.90s and reports four unobserved
  endpoints) and — because "the budget expired" and "the host is off"
  must never render as the same sentence again —
  `TestDoctor_TLSRowNamesTheBudgetWhenTheReportRanOut`, which is a real
  honesty fix in its own right: when `ctx.Err() != nil` the row now says
  *not reached inside the report's deadline … this row is about the
  BUDGET, not about the host.*
- **`warm.policy` accused the operator of their own declaration
  (MAJOR — a C11 conflict git reports no conflict for).** A skipped warm
  target rendered as "the warm policy is not doing what it was declared
  to do" whatever the reason, and C4's reasons include `cell drained`
  and C11's `held: …`. So one report said, three rows apart, *"1 active
  lease(s), none outliving a day — active holds: heavy/qwen held by
  hold, 4h0m left"* and *"the warm policy is not doing what it was
  declared to do: target heavy/qwen skipped: held"* — about the same
  hold. The same WARN fired for the whole duration of any `vibe cell
  drain`, the most ordinary verb in the control plane, and for an
  `opportunistic` or `roaming` box being absent, which the design §4
  class table says is not news at all. `explainedCells` now derives the
  explanation from the **StateSnapshot** (declared intent → C11 hold →
  class-normal absence, C11's ladder order), never from a loop's prose,
  and a skip it explains is reported OK **with the reason named**.
  `TestDoctor_DeclaredSuppressionIsNotAPolicyFailure` covers all three.
- **`probe.verdicts` WARNed because the fleet was in use (MAJOR).** The
  check flagged any target whose state was `skipped` *right now*, but
  C8's guard set skips on in-flight work, an **unreported** in-flight
  count, a model that is not resident and an active lease — so a
  declared target on a working fleet is skipped most passes, and a cell
  fleetd has no events stream to is skipped *forever* (`cell heavy
  in-flight unknown — not spending GPU time blind`). §11 asks for the
  target "skipped for its whole life"; that is `LastAsk == nil`, and it
  is what the check now reports, minus the explained set above. Every
  other skip still rides the detail line, so no reason is swallowed.
  `TestDoctor_ProbeSkipIsAFindingOnlyWhenTheTargetWasNeverAsked`.
- **`intent.hygiene` flagged a drain one second after it was requested,
  and called it "undeclared" (MAJOR).** `staleRequestAge` gated the
  *pending* bucket and not the *residue* one, so a fresh request landed
  in residue anyway: between `vibe cell drain` and the cell's next
  heartbeat the display is `INCONSISTENT` by design (evidence outranks
  the declaration), and doctor reported that as `undeclared state`. Two
  errors in one line — the intent is emphatically declared, and the
  window is the normal middle of every drain. `DRAINED?` (genuinely no
  entry) and `INCONSISTENT` (declared, not yet reconciled) are now
  separate sentences, and the ack window gates both.
  `TestDoctor_IntentHygieneDoesNotAccuseAFreshDrainRequest`.
- **`versions.reported` scored a cell OK with no `defs_sha`, and
  `defs.parity` dropped it (MAJOR-ish).** §5's own rule is *assert the
  block is populated before asserting parity over it*; the code asserted
  only that the block was not entirely empty, so a cell announcing a
  vibe build and no def SHA — a backends dir that is not a git checkout,
  or an announcer with no `git` on `PATH` — read as `OK, versions block
  present`, and the divergence branch of `defs.parity` printed
  `abc123: a · def456: b` with the third cell nowhere on the page. The
  cell most likely to be the problem was the one the report omitted. No
  `defs_sha` is now UNKNOWN, and "Not reporting a SHA: …" rides **every**
  branch. `TestDoctor_VersionsWithoutADefsSHACannotJoinParity`.
- **A genuinely full cell disk read as "no disk figure" (MINOR).**
  `disk.free` treated `disk_free_gb == 0` as an absent measurement with
  the detail "the cell's capacity block is absent or reports nothing" —
  a claim doctor cannot make, since the producer leaves the field at
  zero when `statfs` fails and a filesystem with nothing left announces
  the same zero. UNKNOWN is still the right level (the wire does not
  separate them) but the reason now names **both** readings, which is
  the house rule the `auth_rejected` attribution note already follows.
  `TestDoctor_ZeroDiskFigureNamesBothReadings`.
- **A stat failure was always reported as "state dir missing" (MINOR).**
  A permission or I/O error on a directory that is mounted sent the
  operator to remount a volume that is already there. `errors.Is(err,
  fs.ErrNotExist)` now decides the sentence.
  `TestDoctor_StateDirUnreadableIsNotReportedAsMissing`.
- **The read-only source scan covered one of the four doctor files
  (MINOR, test truth).** `TestDoctor_ReachesNoMutatingVerb` parsed
  `fleetapi/doctor.go` — the file *least* able to mutate anything, since
  it holds no client and no config. `daemon/doctor.go` holds the
  `vibeclient`, where reaching for `CellDrain` is a one-line edit, and
  the behavioural test cannot see it either (the prober is injected and
  the fixture injects a fake). The scan now covers all four files and
  bans the RPC verbs as well; mutation-verified by planting a
  `client.CellDrain` in `daemon/doctor.go` and watching it fail at the
  right line.

Two smaller things fixed in passing, both inside the TLS work: one
endpoint named three times (`fleetd_url`, `cells.X.url`,
`cells.X.daemon_url` pointing at one host) produced three identical rows
under one key and paid for three handshakes
(`TestDoctor_TLSDialsEachEndpointOnce`), and `certNotAfter` grew a
per-server dial timeout so the fan-out is testable in milliseconds
rather than 3s per endpoint.

Two things the pass looked at and left:

- **`leases.age` measures REMAINING time, not age.** `Lease` carries no
  creation stamp, so "older than 24h" is not computable; a lease with
  more than a day left is the closest honest proxy, and the summary says
  what it measured ("a lease will outlive the day"). Noted here so the
  next reader does not take the check ID literally.
- **`usage.flow` still WARNs for an announcing cell that served nothing
  in seven days** — including one commissioned this morning. The ledger
  keeps no "this cell's meter has ever reported" bit, so a
  newly-announcing cell and a cell with no `store:` in its llama-swap
  extras are the same observation. The detail names both causes, and the
  declared-suppression fix above removes the drained/held case, which
  was the noisiest one.

## Live-gate addendum (2026-08-05): dirtiness silenced a real divergence

Found by standing a real 4-cell fleet up (four llama-swap v239
processes, a real fleetd, real announcers, real traffic) and running
these gates against it. Fixed on `fix/live-gate-truth`.

With alpha, charlie and fleetd on one defs SHA and bravo on another,
doctor reported exactly what it should: `WARN defs.parity — cells
disagree about the def checkout`. Touching a file in bravo's checkout
— nothing else; same divergence, same three boxes — flipped the same
finding to `OK`.

The cause is one line of the shape §5 exists to prevent, applied in the
wrong direction. A dirty checkout's SHA does not describe what is
running, so the first cut dropped the cell from the comparison
entirely; with bravo gone, one SHA was left standing and the check
reported agreement. But "cannot vouch for itself" is not "has nothing
to say": a dirty tree can still **disagree**, and dirty-and-diverged is
strictly more alarming than clean-and-diverged — different base
commits, plus uncommitted edits on top of one of them. This is the
worst failure available to a diagnostic, going quiet exactly as the
situation gets worse, and it is `UNKNOWN`-is-not-`OK` wearing a
different hat: absent evidence subtracted concern instead of adding it.

The check now separates the two questions it was conflating.
**Divergence** is decided over every cell that reports a SHA, dirty or
not. **Agreement** may rest only on the clean ones. So:

| fleet | verdict |
|---|---|
| every reporter clean, one SHA | `OK` |
| any two reporters on different SHAs (dirty or not) | `WARN`, both SHAs named, dirty trees flagged |
| one SHA, at least one clean reporter, some dirty | `OK`, naming the dirty cells |
| one SHA, every reporter dirty | `UNKNOWN` — "the matching SHA proves nothing about what is running" |
| nobody reports a SHA | `UNKNOWN` — nothing to compare |

The same trap sat one level down and is fixed with it: fleetd's own
checkout on a *different* commit from the cells is a `WARN` (the render
it writes comes from another tree), and `!host.DefsDirty` suppressed
that `WARN` in the strictly worse case where fleetd had edits on top of
the different commit. A dirty fleetd on the *agreed* SHA now says so in
the detail without changing the level.

Both directions are mutation-proven in
`fleetapi/defsparity_test.go`: restoring either line makes a named test
fail with the live fleet's shape.

**Review pass.** The fleetd-vs-cells comparison existed only on the
agreement branch, so the box writing the front's render dropped out of
the report the moment the cells ALSO disagreed — the one report where
"so which tree is the render coming from" is the operator's next
question. fleetd's own checkout is now named on every branch that names
SHAs; the agreement branch still grades on it, because that is the
branch where a mismatch is the only finding there is. The pass also
renamed `TestDoctor_DefsParityTreatsADirtyCheckoutAsUncomparable` —
its name and comment stated the rule this addendum replaced, and its
four subtests all still pass, which is precisely how a false rule
survives (ground rule 10).
