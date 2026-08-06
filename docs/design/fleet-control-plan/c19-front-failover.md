# C19 — Front failover identity: the state that dies with the front host, and the path back

Status: **PR OPEN** (2026-08-05), off `feat/c19-front-failover` branched
from `main` at `c2127f3`. Feature commit plus ground rule 9's adversarial
self-review commit; every fix mutation-verified. Unit gates U1–U17 green
on a full local inner loop. **Live gate L1 PASS** — the fire drill ran
against a real four-cell fleetlab: mirror, refusal, `SIGKILL` of a real
fleetd and a real llama-swap front, restore onto a standby, **10.1 s** to
three cells announcing again with the same token, the same declared
intent and a byte-identical usage ledger. L2 (the same drill on real
hardware) is **UNRUN** — it needs the fleet. See
[Acceptance gates](#acceptance-gates).

Backlog item 12 in [fleet-control-futures.md](../fleet-control-futures.md)
§2:

> **Front failover identity** — a DNS name for :9000 (so a spare box can
> assume it), rendered config + compose + nightly fleetd-state tarball
> mirrored off the front host, and a half-page cold-standby runbook (the
> gpu-cell can run `llama-swap:cpu` with the same peers file in ~10
> minutes). The front host dying is the one total-fleet outage; don't
> build HA, write down the path.

## "Don't build HA" is an invariant, not a budget decision

It reads like scope control. It is not: **the control plane changes what
the catalog SAYS, never where a request GOES**, and an automatic front
promotion is precisely the silent rerouting that rule forbids. A health
check that moves an address is the same mechanism as a router that
retries a dead cell elsewhere, one layer down — and this plan has spent
nineteen phases keeping that mechanism out.

So there is no leader election, no watcher, no health-check-driven
promotion, and nothing in this phase runs on a schedule except a backup.
What the code contributes to the failure mode HA exists for — two boxes
answering at once — is a **refusal**: `restore` dials the fleet's own
recorded addresses and stops if anything answers. That is a mechanism
that can only ever make a takeover *not* happen.

### What two fronts actually costs, which is why the refusal is the feature

Worth stating precisely, because "split brain" is usually hand-waved and
here it is enumerable from the code:

- **Clients split between two catalogs.** Half the fleet's requests reach
  a front whose peers file is a snapshot from whenever the standby was
  built.
- **Both fleetds accept announces** — the fleet token authenticates the
  connection, not the cell (design §6) — so each has a plausible-looking
  presence table covering some of the heartbeats.
- **Both queue piggyback commands**, both run C4's warm loops, both fire
  C4 schedules and C14's `sleep_schedule`. Two independent controllers
  actuating one fleet; a suspend can land on a box the other just warmed.
- **Both fold the same cumulative announce totals into two append-only
  ledgers** (C7a). Neither is the record afterwards, and the ledger is
  the one file in the state dir that cannot be reconstructed from
  anywhere: cells report LIFETIME totals, so a merge would double-count
  and a choice would discard.

Nothing detects any of this, because each half looks healthy from where
it stands. The only cheap defence is to make the second one refuse to
start, and the only honest place for that is the command a human runs.

## 1. The state that dies with the front host

Enumerated from the tree rather than remembered. The mirror's whole value
is completeness, so the table is bound to `paths.go` by
`TestMirrorCoversEveryFleetStateFile`: every function there that returns
a `StateHome()`-rooted path is either captured, with a sentence saying
what the fleet loses without it, or excused by name with a reason.

| file | producer | what its loss costs |
|---|---|---|
| `token` | C1 | the control-plane bearer. A standby that mints a fresh one 401s every cell, every client and every MCP registration at once. |
| `guest-token` | C12 | the read-only bearer; a fresh one silently revokes every guest. Captured only when it lives inside the mirrored state dir, which is the deployment `deploy/fleetd/README.md` already requires. |
| `fleet/intent.json` | C1 | declared intent (axis 2). Its loss turns every deliberate drain into a `DRAINED?` mystery and re-arms C9's absence alarms. |
| `fleet/leases.json` | C2, C11 | advisory leases and holds. A lost hold re-arms the warm policy against a model somebody is evaluating. |
| `fleet/last-seen.json` | C1 | last sightings; without it every away box looks newly absent. |
| `fleet/start-history.json` | C1 | cold-start ETAs. Accuracy, not correctness. |
| `fleet/usage.jsonl` | C7a | **irreplaceable.** Cells announce CUMULATIVE totals, so a fresh ledger restarts the running total rather than back-filling, and C7b's payback bars are computed from it. |
| `fleet/front-cloud-usage.json` | C7b | the front's cloud-spend cursor; losing it re-ingests and double-counts that window. |
| `fleet/notify-scope.json` | C9 | the away/home declaration. Losing it delivers holiday alarms. |
| `fleet/mirror-receipt.json` | C19 | the previous run's receipt, so a restored fleetd knows how old its own state was. |
| `hosts.yaml`, `config.yaml` | C1+ | membership, and every declared policy: warm targets and schedules, probe targets, the sleep schedule, notify, the front mount. |
| `backends/*.yaml` | — | the render INPUT. A standby with no defs cannot render the front's config at all, and `renderPass` correctly refuses to write a peerless one over a good one. |
| the front's **rendered** config | C3 | the file that makes a standby serve in minutes instead of after a re-render. It carries the peer list verbatim — and, after C15, whatever `front_extras` merged in, including `apiKeys`. |

**And the correction the enumeration produced.** Three state files are
CELL-side and do *not* die with the front: `cell-intent.json`,
`cell-usage.json` and C8's `model-probe.json` baselines. C8 put the
baselines on the cell deliberately ("a cell whose fleetd is down must
still know its model is degraded"), and the payoff shows up here — the
fleet's per-model performance history survives the outage that takes
everything else. The mirror captures them if the box it runs on happens
to be a cell too, and the manifest's `not_covered` says plainly that
other cells' copies are not its business.

### What it deliberately does not carry, and how the runbook gets it

Credential **values** referenced by `hosts.yaml` (`token_file`,
`swap_key_file`) and by `fleet.notify` (`url_file`, `token_file`) are
recorded as REFERENCES — path, exists, mode, and one sentence on what
breaks without them — and never as bytes. Ground rule 3: the values live
in the private fleet repo, and the runbook's step 4 is to fetch them.

fleetd's own token is the exception, and the boundary is worth stating
because it looks inconsistent: **the control-plane token IS the identity
being assumed.** Excluding it turns a ten-minute recovery into a
fleet-wide re-key at the worst possible moment. Other boxes' credentials
are a different thing — the front host holds copies for convenience, and
a mirror that scoops up every secret it can reach is how one compromised
backup target becomes a compromised fleet. So: fleetd's own tokens are
carried (archive `0600`, `credentials: true` in the manifest, said out
loud in the doctor check and on every terminal that prints it), and
`--no-secrets` exists for a destination the operator does not trust, at
the stated price of a re-key and a re-render.

The rest of the not-covered list is fixed data in every manifest, because
discovering it during a recovery is too late: model weights, the
llama-swap image (**the digest `fleet.front_image` pins** — C16, and a
standby that pulls a different one is how a recovery becomes the v247
in-flight-wire incident), the cells' own state, DNS and the LAN address
itself, systemd units, docker, the vibe binary, and anything not passed
to `--include`.

## 2. The mechanism: `vibe fleet mirror` (create | verify | restore)

`internal/vibe/fleetmirror`, stdlib only (`archive/tar`, `compress/gzip`,
`crypto/sha256`). One `.tar.gz` per run, `0600`, named
`fleet-mirror-<UTC>.tar.gz` so "newest" is a string comparison, with
`manifest.json` written FIRST inside it and beside it.

**It is a host command on a timer, not a fleetd loop**, and that is the
one structural decision worth defending. Two reasons, both fatal to the
alternative: the mirror has to keep working when **fleetd is the thing
that broke**, and fleetd cannot see the host paths its own state is
mounted from (`/state/vibe` is a bind mount; the archive has to be
written *outside* both). A control plane that backed itself up would also
be a control plane that writes on a schedule nobody asked for, on the
read-and-request-only side of invariant 4.

- **create** walks the table above, records a sha256 per entry, and
  refuses an `--out` inside the state or config dir — a mirror stored on
  the box it mirrors is not a mirror. It cannot tell whether a path is a
  remote mount, and says so rather than pretending. Absent files are
  `missing` (a fleet with no leases has no `leases.json`); unreadable
  ones are `errors`, because the file it could not read is exactly the
  one the recovery will want.
- **verify** reads the archive back and recomputes every hash. A mirror
  nobody has ever read back is a belief, not a backup — and `restore`
  runs it before writing anything, so a corrupt archive fails BEFORE it
  has half-replaced a working state dir.
- **restore** plans first and writes second: every refusal is decided
  before the first byte lands, so it never stops halfway with one fleet's
  token beside another fleet's intent. Destinations are explicit and an
  unset one is a named SKIP, never a guess. Modes are RESTORED, not
  inherited (a `0600` token that lands `0644` has published the fleet's
  root credential). Existing files are kept unless `--overwrite`.
  Entries under `extras/` are never placed automatically — they came from
  absolute paths on the old host — and every archive path is validated
  against `..`, absolute and unclean forms, because a backup tool that
  unpacks `../..` is an arbitrary-write primitive wearing a helpful name.

## 3. Identity

`hosts.yaml` is the coupling: cells announce to `fleetd_url`, every
client and every rendered peer entry dials `cells.front.url`. A standby
assumes the identity by answering on those addresses — which is one DNS
record if they are names, and taking an IP if they are not. This fleet's
front is a macvlan container on a static address, so the mirror does not
call that a misconfiguration; it **warns once, at mirror time**, where
the information is actionable rather than as a permanent doctor WARN that
an operator would learn to ignore.

The manifest records the whole identity — `fleetd_url`, the front's URL,
every cell's URL/class/daemon_url, the image pin, the timezone — because
the box doing the recovery does not have `hosts.yaml` until step 3.

**The takeover probe** is a TCP dial of those recorded addresses, and it
is deliberately not an HTTP request: it needs no credential, so it works
on a standby that has not fetched the private repo yet, and it builds no
llama-swap request for C15's rule to be about. Its one honest false
positive is documented at the call site and in the runbook — if the
operator has ALREADY moved the address to this box and started something
on it, the probe reaches itself and refuses. That is why the runbook's
order is restore first, address last, and why `--force` exists.

## 4. Two doctor checks, because a stale backup is the classic failure

`fleet.mirror_max_age` in fleetd's config; the receipt the mirror leaves
in the state dir fleetd already reads. No new mount, no new route, no
network call, and C13's injection shape unchanged (`daemon/doctor.go`
reads the file; `fleetapi` still knows nothing about the filesystem).

**`mirror.age`** — the name is what it proves. It answers "when did a
mirror last run *here*", not "is there a good backup": nothing on this
box can verify that the destination is off-host or that the archive is
readable from the standby.

| state | level |
|---|---|
| `unmanaged` | OK — declared to be handled outside vibe |
| unset | UNKNOWN, naming both fixes (C16's `front_image` rule: "the operator decided" and "nobody told fleetd" must not be spelled the same way) |
| not a duration / not positive | FAIL |
| declared, no receipt | **FAIL** — declared and never run |
| receipt unreadable / no timestamp | UNKNOWN with the reason |
| **receipt stamped in the future** | UNKNOWN — a clock step forward would otherwise make a stale mirror read as fresh, forever, silently. This repo's oldest defect class in this phase's shape. |
| age ≤ max | OK |
| max < age ≤ 3× max | WARN — one missed run is a timer that did not fire |
| > 3× max | FAIL — a mechanism that stopped, not a run that slipped |

**`mirror.contents`** is emitted only when a receipt exists, so a fleet
with no mirror gets one UNKNOWN rather than two. Read failures are a WARN
naming the file that is therefore not backed up (usually a permission —
fleetd's container writes its state as root). Absences are NOT a finding
and are still NAMED, so "absent" and "not looked for" stay different
answers.

## 5. Rehearsal: `scripts/fleetlab/gate-c19-drill.sh`

C16 shipped an upgrade ritual with exactly this problem — a checklist
nobody performs is worth nothing — and the futures entry pairs this item
with a quarterly 15-minute fire drill. The drill is runnable now, on one
box, against four real llama-swap cells and a real fleetd, and it
executes every step of the runbook:

1. put a real metered row in the ledger (a completion through the front),
   declare a drain, record the token;
2. `vibe fleet mirror` twice (so the second archive carries the first's
   receipt) and `verify`;
3. attempt a restore **while the fleet is alive** — it must refuse and
   write nothing;
4. `SIGKILL` fleetd and the front llama-swap: the front host dying;
5. show the cells still serving (invariant 4, mechanically);
6. restore onto a standby, start a front and a fleetd on the same
   addresses, and **time it**;
7. compare the token, the declared intent and the ledger across the
   outage, run `vibe fleet doctor` on the standby, and resume the drained
   cell through the restored control plane.

It is the one rig in `scripts/fleetlab` that deliberately kills lab
processes; its header and the README say so, and `./lab.sh down && up`
restores the documented shape.

## 6. What is still manual, and how long the real thing takes

The drill's mechanical half is **10 seconds**. The runbook budgets
**10–15 minutes** for the real recovery, and the difference is entirely
human or physical:

- **confirming the old front is DOWN** — the one step that must not be
  automated, and the step the whole design rests on;
- fetching the credential files from the private repo;
- `docker pull` of the pinned image if the standby has not cached it;
- DNS TTL (or moving an IP, and the ARP cache after it);
- disabling the old box's units before it comes back.

Nothing here tries to shorten those. `restore` prints them as a numbered
list, with the image digest filled in from the manifest.

## Files

| file | what |
|---|---|
| `internal/vibe/fleetmirror/mirror.go` | the state table bound to `paths.go`, `Create`, the manifest/receipt types, `NotCovered`, prune, `Newest` |
| `internal/vibe/fleetmirror/restore.go` | `Verify`, `safeName`, `TakeoverProbe`, `Restore` |
| `internal/vibe/fleetmirror/mirror_test.go` | U1–U10 |
| `internal/vibe/cli/cmd_fleet_mirror.go` | `vibe fleet mirror` / `verify` / `restore` and their renderers |
| `internal/vibe/cli/cmd_fleet.go` | one `AddCommand` |
| `internal/vibe/fleetapi/mirror.go` | `UnmanagedMirror`, `MirrorFacts`, `checkMirror` |
| `internal/vibe/fleetapi/doctor.go` | the call site + the two `DoctorHost` fields |
| `internal/vibe/fleetapi/c19_test.go` | U11–U15 |
| `internal/vibe/fleetapi/c13_test.go` | the read-only source scan learns `mirror.go` |
| `internal/vibe/daemon/doctor.go` | `mirrorFacts()` — the receipt read |
| `internal/vibe/daemon/daemon.go` | `fleet.mirror_max_age`, `LoadConfigFrom` |
| `internal/vibe/paths/paths.go` | `MirrorReceiptFile` |
| `docs/runbooks/front-failover.md` | the half-page runbook |
| `scripts/fleetlab/gate-c19-drill.sh` | the fire drill |
| `scripts/fleetlab/README.md`, `deploy/fleetd/README.md` | the rig row; the state contract's "and this is why C19 exists" |

## Acceptance gates

Unit (mechanical, in-repo):

| # | gate | result |
|---|---|---|
| U1 | every `StateHome()`-rooted path in `paths.go` is either mirrored or excused by name, and the scan FAILS when a row is deleted | PASS (mutation-verified) |
| U2 | credential paths from `hosts.yaml` are recorded as references and their BYTES are nowhere in the archive; fleetd's own token IS in it | PASS |
| U3 | `--no-secrets` drops the tokens and the rendered front config, warns about each, and the token's bytes are absent | PASS |
| U4 | an `--out` inside the state or config dir is refused | PASS |
| U5 | an absent state file is `missing`; an unreadable one is an `error` — and never the other way round | PASS |
| U6 | `verify` catches a rewritten entry, a dropped entry, a `../` escape and an unknown manifest version | PASS |
| U7 | `restore` refuses while the fleet's own address answers, writes nothing, names the address, and `--force` proceeds | PASS (mutation-verified) |
| U8 | restore keeps the token's `0600`, skips existing files without `--overwrite`, reports unset destinations as skips, never places `extras/`, and writes nothing on `--dry-run` | PASS |
| U9 | `prune --keep N` deletes only this package's own archives | PASS |
| U10 | the receipt round-trips through the state dir at exactly the path fleetd reads | PASS |
| U11 | `mirror.age` returns OK / WARN / FAIL / UNKNOWN for all eleven states, each naming its reason, and every non-OK carries a fix | PASS |
| U12 | an undeclared mirror expectation is never OK — with and without a receipt (separate test, so U11's table cannot drift into asserting it) | PASS |
| U13 | a receipt stamped in the future is not fresh, and 30 s of NTP jitter is not a clock step | PASS |
| U14 | `mirror.contents` WARNs on read failures naming the file, and stays OK for absences while still naming them | PASS |
| U15 | `mirror.contents` is not emitted when no readable receipt exists (one UNKNOWN, not two) | PASS |
| U16 | C13's read-only source scan covers `fleetapi/mirror.go`, and passes | PASS |
| U17 | full inner loop: build, vet, `test -race -count=5`, gofmt, `go mod tidy` byte-identical, golangci-lint | PASS |

Live (a real fleet, or the harness):

| # | gate | result |
|---|---|---|
| L1 | the fire drill end to end against a real four-cell fleetlab: mirror → refusal → SIGKILL → restore → standby → timings → survival | **PASS** — see Execution |
| L2 | the same drill on real hardware: the gpu-cell box as the standby, the pinned image pulled, DNS repointed, cells re-announcing across a LAN | **UNRUN** — needs the fleet (SSH blocked, the LAN does not route from here) |
| L3 | a real off-host destination (NFS/CIFS mount) over a week of nightly timer runs, with `mirror.age` moving OK → WARN when the timer is stopped | **UNRUN** — wall clock, not hardware |

## Execution

### L1 PASS — the fire drill, 2026-08-05

`FLEETLAB_DIR=/tmp/fleetlab-c19`, four real llama-swap v239 cells, a real
fleetd, both announcer shapes. Raw transcript, trimmed to the evidence:

```
=== 0. state worth losing ===
{ "completion_tokens": 11, "prompt_tokens": 31, "total_tokens": 42 }
Drained bravo (reason: c19 fire drill, eta 23:00). Intent recorded.
bravo:        {"display":"DRAINED","intent":{...,"since":"2026-08-06T01:41:35.626292336Z"}}
ledger lines: 3 (sha f1221ff2a381)
token sha:    e7ed5ed46fd6

=== 1. vibe fleet mirror ===
wrote .../fleet-mirror-20260806T014137Z.tar.gz (14 files, 5531 bytes)
  contains the control-plane token: this archive is as sensitive as the front host.
  warnings:
    - the front is addressed by literal IP (http://127.0.0.1:9640): a standby must TAKE that address.
  credentials referenced and deliberately NOT carried:
    cell_token   bravo      /tmp/fleetlab-c19/state/bravo/vibe/token (600)
OK: 14 entries, every sha256 matches.

=== 2. restore REFUSES while the fleet's own address still answers ===
STILL ANSWERING: fleetd at 127.0.0.1:9721
STILL ANSWERING: the front at 127.0.0.1:9640
vibe: the fleet's address still answers ... two fleetds both accept announces, both fire
warm and sleep schedules, and both fold the same cumulative totals into two ledgers that
cannot afterwards be reconciled.
(refused, wrote nothing — correct)

=== 3. SIGKILL fleetd and the front llama-swap (the front host dying) ===
killed fleetd (pid 42608) / killed swap-front (pid 42517)
  alpha    /v1/models -> 2 model(s)
  charlie  /v1/models -> 1 model(s)      <- invariant 4: inference is unaffected

=== 4. restore onto the standby box ===
  written  state/token, state/guest-token, state/fleet/{intent,last-seen,start-history,
           mirror-receipt}.json, state/fleet/usage.jsonl, config/{hosts,config}.yaml,
           config/backends/*.yaml, front/config.yaml            (14 files)
restored 14 files. Still yours to do:
  2. run the front on the PINNED image: ghcr.io/mostlygeek/llama-swap:v239-cpu-b9994@sha256:6bae869…

=== 5. recovery timings (from the SIGKILL) ===
  fleetd answering                   1.0s
  the front serving the catalog      1.0s
  all 3 cells announcing again      10.1s

=== 6. what survived ===
  token identical:   yes
  bravo before:      {"display":"DRAINED","intent":{...,"since":"...:35.626292336Z"}}
  bravo after:       {"display":"DRAINED","intent":{...,"since":"...:35.626292336Z"}}
  ledger lines:      3 -> 3   (sha f1221ff2a381 -> f1221ff2a381)

=== 6b. fleet doctor on the standby ===
OK      fleetd.token       control-plane token loaded from the state dir (not minted at this start)
OK      front.image_pin    front image is digest-pinned
OK      mirror.age         state mirrored less than a minute ago (declared limit 36h)
OK      mirror.contents    last mirror captured 14 files (5.4 KiB)

=== 7. the restored control plane can actuate ===
Resumed bravo. Models return by JIT on next request; intent cleared.
```

Six things here that no unit test can prove:

1. **The announcers were never touched.** They kept the token and the
   registry URL they had before the kill, and reattached to a different
   process on the same address. That is what "assuming the identity"
   means, and it only works because the token came out of the archive —
   `fleetd.token` reporting *loaded* rather than *minted* is the C1 log
   line doing its job on a box that has never run fleetd before.
2. **The ledger is byte-identical across the outage**, including the row
   from the completion issued minutes earlier. C7a's append-only file is
   the one thing here that could not have been rebuilt.
3. **The declared drain survived with the same `since`**, so C3's
   conflict rule sees the record rather than a new request — a lost
   intent store would have resumed bravo at the next heartbeat.
4. **The refusal fired against a live fleet and wrote nothing**, which
   is the only part of this phase that protects the fleet from the
   recovery.
5. **The pin travelled.** `front.image_pin` is OK on a box that has never
   seen the compose file, and `restore` printed the digest to run.
6. **Inference was unaffected throughout** (invariant 4), with the
   control plane dead for the whole window.

The one edit the lab needs and a real deployment does not:
`fleet.front_config` is an absolute host path in fleetlab and a container
mount (`/front-config/config.yaml`) in the reference stack, so the drill
rewrites it after the restore and says why at the line that does it.

**Qualifications, per the plan README's rule.** 10.1 s is the *mechanism*
on one box with no network, no image pull, no DNS and no human. It is
evidence that the state survives and the identity is assumable; it is not
a prediction of a real recovery, which the runbook puts at 10–15 minutes
for the reasons in §6. Nothing here crossed a LAN.

### L2, L3 UNRUN

L2 needs the fleet: a second physical box taking the front's address,
the pinned image pulled onto it, and a real DNS change. SSH is blocked
from here and the LAN does not route. L3 is a week of wall clock against
a real NFS destination; nothing physical blocks it.

### Inner loop

`go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy`
byte-identical (no new dependency: `archive/tar`, `compress/gzip`,
`crypto/sha256`, `encoding/json` are all stdlib), `golangci-lint run` 0
issues, `go test -race ./...` green, `-count=5` green on `fleetmirror`,
`fleetapi`, `daemon` and `cli`.

### C15 and C13, checked rather than assumed

- **C15**: this phase adds no fleetd→llama-swap request. The takeover
  probe is a TCP dial (`net.DialTimeout`) in `fleetmirror`, which builds
  no HTTP request at all — chosen partly for that reason, and partly
  because it needs no credential on a standby that has not fetched the
  private repo yet. `TestEveryLlamaSwapRequestIsAuthorized` is unchanged
  and green.
- **C13**: `fleetapi/mirror.go` contributes two checks to the report, so
  it joins the read-only source scan's file list (C16's REV-2 lesson, one
  file over). `daemon/doctor.go`'s new read is `os.ReadFile` through
  `fleetmirror.ReadReceipt` — a read, on a path that already reads
  `intent.json`'s directory — and the scan's banned list (`Create`,
  `WriteFile`, …) still passes.

## For the reconciliation pass

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. What belongs in each:

### AGENTS.md

A new section, "Front failover (fleet-control C19)":

- **There is no automatic failover and there must not be one.** An
  automatic front promotion is the silent rerouting invariant 3 forbids,
  one layer down from a router that retries a dead cell elsewhere.
  `internal/vibe/fleetmirror` makes a MANUAL recovery fast; the only
  thing it contributes to the two-boxes-answering problem is a REFUSAL
  (`TakeoverProbe` — a TCP dial of the recorded `fleetd_url` and front
  URL; `restore` stops if anything answers, `--force` is for the one
  false positive where the operator already moved the address).
- **`vibe fleet mirror` is a HOST command on a timer, never a fleetd
  loop.** It has to keep working when fleetd is what broke, and fleetd
  cannot see the host paths its own state is bind-mounted from. fleetd's
  only involvement is READING the receipt the command leaves in the state
  dir (`paths.MirrorReceiptFile`), which is what `mirror.age` and
  `mirror.contents` render — no new mount, no new route, no network call,
  and C13's injection shape unchanged.
- **The mirrored state table is bound to `paths.go`**
  (`TestMirrorCoversEveryFleetStateFile`): a new `StateHome()`-rooted
  path is either captured with a sentence saying what its loss costs, or
  excused by name in `notFleetState`. A phase that adds a state file and
  neither mirrors nor excuses it fails. Three files are deliberately
  cell-side and do NOT die with the front — `cell-intent.json`,
  `cell-usage.json` and C8's `model-probe.json` baselines.
- **Credential values are never carried; paths are.** `hosts.yaml`'s
  `token_file`/`swap_key_file` and `fleet.notify`'s files become
  `references` (path, exists, mode, consequence) for the runbook to fetch
  from the private repo. fleetd's OWN token and guest token ARE carried,
  because they are the identity being assumed and excluding them turns a
  recovery into a fleet-wide re-key — so the archive is `0600`, the
  manifest sets `credentials: true`, and every surface says so.
  `--no-secrets` is the opt-out, at the price of a re-key and a
  re-render.
- **`verify` before every restore, and plan-then-write.** Modes are
  restored rather than inherited (a `0600` token landing `0644` publishes
  the fleet's root credential), archive paths are validated against `..`
  and absolute forms, `extras/` is never placed automatically, and an
  unset destination is a named skip rather than a guess.
- **`fleet.mirror_max_age`**: unset is UNKNOWN, `unmanaged` is the
  closed-vocabulary opt-out, over 3× is a FAIL, and a receipt stamped in
  the FUTURE is UNKNOWN — a clock step forward would otherwise make a
  stale mirror read as fresh forever.
- Under the fleetlab notes: `scripts/fleetlab/gate-c19-drill.sh` is the
  one rig that deliberately kills lab processes (`./lab.sh down && up`
  after it), and it is the quarterly fire drill the runbook names.

### docs/design/fleet-control-plan/README.md

- A `C19` row: *Front failover identity: the state that dies with the
  front host, and the path back* — ~1,400 lines, depends on C1–C16
  (composition), status "PR open; unit gates U1–U17 green; **L1 PASS**
  (harness fire drill, 10.1 s recovery), L2 needs the fleet, L3 is wall
  clock".
- A paragraph after C17's, along the lines of: C19 (2026-08-05) is
  backlog item 12, and it is the first phase whose subject is the fleet's
  own death. Its one carried rule is that **"don't build HA" is an
  invariant, not a budget decision** — an automatic front promotion is
  the silent rerouting invariant 3 forbids, so the code's entire
  contribution to two-boxes-answering is a refusal a human can override.
  Two corollaries. **The backup cannot live in the thing it backs up**:
  the mirror is a host command on a timer because it must survive fleetd,
  and fleetd's only role is reading the receipt. And **enumerating the
  state was the work** — the table is bound to `paths.go` by a test, and
  producing it corrected a standing assumption: C8's probe baselines and
  the C7a cursor are cell-side and survive the front, while the ledger,
  the intent store and the rendered front config do not.
- In the "what still needs metal" paragraph: C19's L2 (a second physical
  box taking the front's address, over a real LAN with a real DNS
  change).

### docs/design/fleet-control.md

- §3 (fleetd as a deployment) or the invariants list: note that fleetd's
  state dir now has a documented off-host path (`vibe fleet mirror`,
  `docs/runbooks/front-failover.md`) and that failover is MANUAL by
  design, with the two-fronts failure mode spelled out — it is the
  clearest available illustration of why invariant 3 is worth its cost.
- The status table: `vibe fleet mirror` beside `vibe fleet doctor`, and
  `mirror.age` / `mirror.contents` in the doctor's check list.

### A note for whoever merges C18

C18 (`vibe model try`) adds `paths.ModelTrialFile()`. On merge it will
fail `TestMirrorCoversEveryFleetStateFile` until it is classified — which
is the test working, not a conflict. The right answer looks like an entry
in `notFleetState`: the trial journal is CELL-side by C18's own design
(the rollback has to be reachable from the box that can perform it), and
therefore does not die with the front — the same reasoning that already
excuses `cell-usage.json` and C8's baselines. One line, in the test's
exclusion map, with that sentence.
