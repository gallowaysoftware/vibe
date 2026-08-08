# C19 — Front failover identity: the state that dies with the front host, and the path back

Status: **PR OPEN** (2026-08-05), off `feat/c19-front-failover` branched
from `main` at `c2127f3`. Feature commit plus ground rule 9's adversarial
self-review commit (seven findings, two of them major — see the
[addendum](#adversarial-self-review-addendum)); every fix
mutation-verified. Unit gates U1–U23 green
on a full local inner loop. **Live gate L1 PASS** — the fire drill ran
against a real four-cell fleetlab: mirror, refusal, `SIGKILL` of a real
fleetd and a real llama-swap front, restore onto a standby, **10.1 s and
14.1 s** (two runs) to three cells announcing again with the same token,
the same declared intent and a byte-identical usage ledger. **Those
timings are UNVERIFIED against the committed rig** — the script in
`scripts/` could not have produced them, and what it was patched to on
the night is not recorded; see
[U3](#u3--the-two-cross-unit-fixes-2026-08-08). L2 (the same drill on real
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

The drill's mechanical half is **10 seconds** (measured, but on a
locally-patched rig — see the qualification under L1 and
[U3](#u3--the-two-cross-unit-fixes-2026-08-08); the order of magnitude is
what this section rests on and that is not in doubt). The runbook budgets
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
| U18 | *(review)* `restore` refuses payload bytes that changed between the verification and the write, and writes nothing | PASS (mutation-verified) |
| U19 | *(review)* an EMPTY backend-defs dir is a warning, and a populated one is not | PASS (mutation-verified) |
| U20 | *(review)* a mirror with no config dir says what is not in the archive | PASS (mutation-verified) |
| U21 | *(review)* two runs inside one second produce two archives, both verifiable | PASS (mutation-verified) |
| U22 | *(review)* the undeclared branch never renders a zero or future timestamp as an age, and still reports a usable one | PASS (mutation-verified) |
| U23 | *(review)* the manifest is the FIRST entry in the archive | PASS |
| U24 | *(review 2)* a manifest `rel` that escapes its slot is refused by `verify` AND by `restore`, and nothing is written outside the slot root | PASS (mutation-verified) |
| U25 | *(review 2)* `--overwrite` never destroys the append-only ledger: a divergent copy is preserved and named; a strict prefix makes no sidecar | PASS (mutation-verified) |
| U26 | *(review 2)* a manifest with no dialable address makes `restore` refuse rather than proceed; `--probe-addr` and `--force` are the two escapes, and a recorded address is still probed | PASS (mutation-verified) |
| U27 | *(review 2)* `--probe-addr` reaches `RestoreOptions` (a registered-but-unwired flag fails) | PASS (mutation-verified) |
| U28 | *(review 2)* a capture GAP is not a green `mirror.contents`, an advisory warning still is, and zero captured files is not a successful run | PASS (mutation-verified) |
| U29 | *(review 2)* a restore that would mix this box's state with the archive's is refused before the first byte; a fully-present slot is still a report | PASS (mutation-verified) |
| U30 | *(review 2)* a payload that changed after verification writes NOTHING, not "nothing further" | PASS (mutation-verified) |
| U31 | *(review 2)* the second archive of a second is the newest one: `--keep` does not delete it and `Newest` returns it | PASS (mutation-verified) |

Live (a real fleet, or the harness):

| # | gate | result |
|---|---|---|
| L1 | the fire drill end to end against a real four-cell fleetlab: mirror → refusal → SIGKILL → restore → standby → timings → survival | **PASS, with its timings UNVERIFIED** — the drill ran and the survival evidence stands, but steps 5–7 cannot execute under the committed script (U3 fix 2), so the recovery numbers came from a locally-patched copy nobody wrote down. See Execution and [U3](#u3--the-two-cross-unit-fixes-2026-08-08) |
| L2 | the same drill on real hardware: the gpu-cell box as the standby, the pinned image pulled, DNS repointed, cells re-announcing across a LAN | **UNRUN** — needs the fleet (SSH blocked, the LAN does not route from here) |
| L3 | a real off-host destination (NFS/CIFS mount) over a week of nightly timer runs, with `mirror.age` moving OK → WARN when the timer is stopped | **UNRUN** — wall clock, not hardware |
| L4 | *(review 2)* the five refusals driven through the real `vibe fleet mirror` binary against a synthetic front-host state dir | **PASS** — see Execution |

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

**Run twice.** The transcript above is the run against the feature
commit; the whole drill was re-run against the reviewed code and
measured **14.1 s** to the same three assertions, with every survival
check identical (`token identical: yes`, the same `since` on bravo's
intent, ledger sha `f1221ff2a381` unchanged, all four doctor checks OK).
The spread is one announce interval — a cell that heartbeated just before
the kill waits its full 15 s — so the honest figure is *inside one
heartbeat of the control plane coming back*, not a stopwatch number.

**Qualifications, per the plan README's rule.** ~10-14 s is the *mechanism*
on one box with no network, no image pull, no DNS and no human. It is
evidence that the state survives and the identity is assumable; it is not
a prediction of a real recovery, which the runbook puts at 10–15 minutes
for the reasons in §6. Nothing here crossed a LAN.

**And the timings are UNVERIFIED (added by U3, 2026-08-08).** The script
committed as `scripts/fleetlab/gate-c19-drill.sh` cannot produce a
*timed* "5. recovery timings" block: it `rm -rf`s `$DRILL` and then
re-creates four of the six directories it writes into, so step 4b's two
log redirections and two pidfile writes fail, the standby is never
started, and all three of step 5's lines come out `NOT REACHED in 60s`
(with steps 6 and 7 then interrogating a control plane that is not
running). That has been true
since the feature commit. **Whatever produced the 10.1 s and 14.1 s above
was therefore a locally-patched copy of this script, and what the patch
was is not recorded** — the L5 transcript below is explicit that ITS
6.1 s came from a `/tmp` copy with the two directories added, and no such
note was ever written for L1.

The numbers stay because deleting them would destroy the only record
there is, and because the *survival* half of this transcript (token
identical, the same `since` on bravo's intent, the ledger sha unchanged,
`fleetd.token` reporting *loaded*) does not depend on the timing block at
all and is unaffected. What is unverified is narrow and specific: the
seconds. **What would verify them:** re-run
`FLEETLAB_DIR=… ./gate-c19-drill.sh` against a four-cell fleetlab on the
fixed script and read the block off a run whose rig is byte-identical to
the one in the repository. U3 fixed the script and could not re-run it —
see [U3](#u3--the-two-cross-unit-fixes-2026-08-08) for why.

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

### Adversarial self-review addendum

Ground rule 9, run against the feature commit. Seven findings; every fix
is mutation-verified below (the mutation applied, the named test observed
red, the mutation restored).

**REV-1 (major) — `restore` verified one set of bytes and wrote another.**
`Restore` called `Verify`, which reads the archive and checks every
sha256, and then called `readArchive`, which reads it *again*. Between
the two reads the file can change: a mirror run finishing onto the same
name, a half-copied file on a network mount, a `cp` from a second
backup. The window is small and the claim is not — this command tells
somebody mid-incident that what it is placing on the standby has been
checked, and it had not been. Each payload is now re-hashed against the
manifest at write time and a mismatch stops the whole restore.

The guard cannot be provoked from outside the process, so it is tested
through a seam (`readArchiveFn`) rather than left unexercised: a guard
nothing exercises is a guard nobody trusts.

*Mutation:* remove the re-hash →
`TestRestore_RefusesContentThatChangedAfterVerification` fails with
"tampered payload was accepted". Restored.

**REV-2 (major) — the undeclared branch rendered an unusable timestamp
as an age.** `mirror.age` with no `fleet.mirror_max_age` reports the age
when it has one. It did that unconditionally, so a receipt with a zero
time read as *"a mirror ran 20599d ago"* and one stamped in the future as
*"a mirror ran less than a minute ago"* — the second being exactly the
sentence the declared branch grew a whole rung to prevent. Absent
evidence wearing a value, in the one branch that had not been given the
rule. `usableStamp` is now the single predicate both branches use.

*Mutation:* drop `usableStamp` from the undeclared branch →
`TestMirrorAge_UndeclaredNeverRendersAnUnusableStampAsAnAge` fails
naming the sentence. Restored.

**REV-3 (minor) — an EMPTY defs dir was captured silently.** It is the
one shape that looks like a successful capture and is not: a standby
restored from that archive cannot RENDER the front's config at all, and
fleetd correctly refuses to write the peerless result over a good one
(C3). Absent files were already reported as `missing`; a directory that
exists and holds nothing produced no row of any kind. Now a warning, at
mirror time, where it is still cheap to fix — and no warning when defs
are present, because a permanent warning on a correct configuration is
one an operator learns to ignore.

*Mutation:* disable the branch → `TestCreate_AnEmptyDefsDirIsAWarning`
fails. Restored.

**REV-4 (minor) — a mirror with no config dir said nothing about it.**
The CLI always passes one, so this is a library-API hole rather than a
live bug; it still means an archive could exist with no `hosts.yaml`, no
`config.yaml` and no defs, and nothing anywhere saying so.

*Mutation:* disable the warning →
`TestCreate_NoConfigDirSaysWhatIsNotInTheArchive` fails. Restored.

**REV-5 (minor) — `readArchive` bounded each entry and not the total.**
`Create` refuses to build an archive over 512 MiB; the reader enforced
only the 256 MiB per-file limit, so a hostile or corrupt archive could
be read into memory unbounded. Same limit, both directions.

**REV-6 (nit) — a claim the code did not make.** The package comment said
the manifest is written first "so a reader knows what it is holding
before it has spent the bytes". `Verify` reads the whole archive
regardless and tolerates any order. The comment now says what the order
actually buys (`tar tzf` shows a human the manifest first) and
`TestArchive_ManifestIsTheFirstEntry` pins the writer's behaviour rather
than an unenforced convention. Ground rule 10 applied to a comment.

**REV-7 (nit) — two runs inside one second silently overwrote.** The
archive name is second-resolution, so a double-fire left one archive and
two receipts, the older of which describes a file that no longer exists.
Now suffixed.

*Mutation:* remove the collision loop →
`TestCreate_TwoRunsInOneSecondDoNotCollide` fails. Restored.

Two things this pass looked at and left alone:

- **`--out` cannot verify that its destination is off-host.** It refuses
  a path inside the state or config dir and stops there: telling a mount
  from a local directory portably is not something this can do honestly,
  and a check that is right most of the time would be read as a
  guarantee. The manifest records the destination, `mirror.age` prints
  it, and the runbook says the sentence instead.
- **The takeover probe's false positive is not "fixed" by probing
  harder** (asking whether the answering fleetd is *this* one, say). Every
  version of that reasons about whether the operator has already moved
  the address — which is the thing the operator knows and the code does
  not. It stays a refusal with `--force` beside it, and the runbook
  orders the steps so the case does not arise.

Gates re-run after the fixes: `go build ./...`, `go vet ./...`,
`gofmt -l .` silent, `go mod tidy` byte-identical, `golangci-lint run`
**0 issues**, `go test -race ./...` green across the module, `-count=5`
green on `fleetmirror`, `fleetapi`, `daemon` and `cli`.

### Adversarial-review addendum (independent pass)

Ground rule 9's second reviewer, run against `cc77d0a`. Seven findings,
one of them a data-loss blocker and one an arbitrary-write blocker.
Every fix below is mutation-verified: the production line reverted, the
NAMED test observed red, the line restored.

**REV-1 (blocker) — `restore` wrote wherever the manifest's `rel` said.**
`safeName` guarded `Entry.Archive`. The field a restore actually JOINS
onto `--state-dir` is `Entry.Rel`, and nothing looked at it — so an
archive with a harmless `archive` of `state/token` and a `rel` of
`../../../../…` placed the file anywhere the restoring user could write,
with the mode the manifest asked for. A guard in one of two paths is not
a guard, and the path it was missing from is the one that writes.

The archive is untrusted input. It is not a cell and it is not the
front: it comes off a backup target, the machine in this design with the
weakest claim to being trusted, and the runbook has an operator restore
whichever archive `verify` liked. `Verify` now rejects a traversing
`rel` (so the read-only command catches it first), `Restore` re-checks
it, and the join is additionally asserted to land inside its slot root.

*Mutation:* remove both `safeRel` calls →
`TestRestore_RefusesAManifestRelThatEscapesItsSlot` fails at the
`verify` assertion. Restored.

**REV-2 (blocker) — `--overwrite` destroyed the append-only ledger.**
`usage.jsonl` is the one file in the state dir that cannot be rebuilt:
cells announce CUMULATIVE totals, so a row that goes does not come back
and C7b's payback bars are computed from what is left. `restore
--overwrite` replaced it with the archive's copy like any other file, so
a second restore from an older archive — the natural move when the
newest one has errors, which is what the runbook's step 2 tells you to
do — silently truncated the fleet's whole accounting history.

The receiving side now knows which files are append-only, from the same
`knownState()` table `Create` captures from rather than from the
manifest (an archive written by an older build gets the same
protection). If what is on disk is a prefix of the archive's copy the
archive is a strict superset and nothing is at risk; otherwise the
existing file is moved to `usage.jsonl.pre-restore-<stamp>`, named in
the report, and printed by the command. Nothing is destroyed and the
restore is still coherent.

*Mutation:* disable the append-only branch →
`TestRestore_PreservesTheAppendOnlyLedgerItWouldReplace` fails. A second
test, `TestRestore_ExtendingTheLedgerNeedsNoSidecar`, keeps the fix from
leaving a file behind on every correct restore. Restored.

**REV-3 (blocker) — the takeover refusal was disarmed by an absent
address.** `TakeoverProbe` dials `fleetd_url` and the `front` cell's
`url` out of the manifest. `fleetd_url` is `omitempty` and a fleet need
not declare a `front` cell, so a manifest can record nothing dialable at
all — and the probe then returns the same empty hit list it returns when
the old front is genuinely dead. `Restore` read that as "safe to
proceed".

This is the phase's ENTIRE contribution to the two-boxes-answering
problem, and it is this repo's oldest defect class (absent evidence
wearing a healthy value, now seven occurrences) sitting on the one guard
whose failure the design says nothing else would ever notice. The probe
now reports what it was able to dial, `Restore` refuses when that is
empty, and `--probe-addr host:port` is the escape that is still a probe.
`--force` is unchanged.

*Mutations:* remove the empty-probe refusal →
`TestRestore_RefusesWhenThereWasNothingToProbe` fails; and separately,
drop `ProbeAddrs` from the CLI's `RestoreOptions` (it was registered as a
flag before it was wired — a switch an operator flips mid-incident with
no effect) →
`TestFleetMirrorRestore_ProbeAddrFlagReachesTheProbe` fails.
`TestRestore_ARecordedAddressIsStillProbed` proves the fix did not turn
the probe off for fleets that do declare an address. Restored.

**REV-4 (major) — a capture GAP rendered as a green `mirror.contents`.**
The mirror's `Warnings` list mixed two different things: advisory notes
that are true of a correct fleet (the front is on a literal IP — this
house's is), and gaps, meaning something the mirror set out to carry and
did not. `--no-secrets` dropping the control-plane token, no config dir
so `hosts.yaml` never entered the archive, no backend defs so the
standby cannot render — each leaves an archive that cannot do the one
thing this phase exists for, and all of them scored OK with the reason
in a detail line. A doctor whose reward is a screen of green is exactly
where that mistake is cheapest to make (C13).

`Gaps` is now its own field on the manifest, the receipt and
`MirrorFacts`; `mirror.contents` WARNs on gaps and stays OK for
advisory warnings. Zero captured files — the wrong-`--state-dir`
signature — is also no longer a successful run.

*Mutation:* disable both branches →
`TestMirrorContents_ACaptureGapIsNotOK` and
`TestMirrorContents_AnEmptyArchiveIsNotOK` fail. Restored.

**REV-5 (major) — a partial restore blended two fleets into one state
dir.** `Restore`'s own comment claimed it "never stops halfway with a
state dir holding one fleet's token and another fleet's intent". The
exists-skip produced precisely that: a standby that already had its own
`token` kept it and took the archive's `intent.json`, `leases.json` and
ledger beside it. Every file parses, so nothing downstream ever notices;
the result is a fleetd that authenticates as one fleet and acts on
another's declarations. A slot is now all or nothing — the mix is
refused before the first byte, and both clean answers stay available
(`--overwrite`, or an empty destination). A slot whose files are ALL
already present is still a report, not a refusal.

*Mutation:* disable the rule →
`TestRestore_RefusesToMixTwoFleetsInOneStateDir` fails. Restored.

**REV-6 (major) — the "nothing written" claim was written half a
restore late.** The feature pass's own REV-1 added a re-hash of each
payload against the manifest, and put it INSIDE the write loop. Entries
are sorted, `state/token` sorts last, so a tampered token aborted a
restore that had already placed six files — the half-restored state dir
the plan step exists to prevent, and the error text says "nothing
further written" while the previous reviewer's test only checked that
the *token* had not landed. A test asserting less than its name claims,
guarding a fix that was correct in substance and misplaced by one scope.
Every payload is now verified before the first write.

*Mutation:* move the re-hash back inside the write loop →
`TestRestore_WritesNothingWhenAPayloadChangedAfterVerification` fails
with "wrote 6 files before noticing". Restored.

**REV-7 (major) — `--keep` deleted the archive it had just written.**
`…Z-1.tar.gz` compares LOWER than `…Z.tar.gz` (`-` is 0x2D, `.` is
0x2E), so the plain lexical sort called the second run of a second the
OLDEST archive in the directory. With `--keep 1` prune therefore deleted
the archive whose path had gone into the receipt one line earlier —
leaving `mirror.age` reporting a fresh mirror that is not on disk — and
`Newest` handed `restore <dir>` the older of the two. The feature pass
added the collision suffix and its test (`TwoRunsInOneSecondDoNotCollide`)
proved both files exist; neither it nor the ordering knew about the
other. Sorting is now by (stamp, collision counter).

*Mutation:* revert `newerFirst` to a string compare →
`TestArchiveOrder_TheSecondRunOfASecondIsTheNewer` fails on both halves.
Restored.

**REV-8 (minor, rig) — the drill's cleanup trap was installed after the
damage.** `gate-c19-drill.sh` rewrites the lab's `config.yaml` in step 0
and SIGKILLs fleetd and the front in step 3, but installed
`trap cleanup EXIT` in step 4. Any `die` in between — a mirror that
failed, a takeover guard that did not fire, a fleetd that survived the
kill — left the lab's config rewritten with nothing queued to put it
back, and the backup is only taken when the file does not already
mention `mirror_max_age`, so the next run would not re-take it. The trap
now goes in before the first mutation. Step 2 also grew a `2b` that
asserts REV-5's refusal against the real lab.

Two things this pass looked at and left alone:

- **The takeover probe still dials only fleetd and the front, not the
  cells.** Correct: the cells are supposed to be up, and probing them
  would turn every healthy fleet into a refusal.
- **`vibe fleet mirror` writes its receipt into the live state dir.**
  It is a write from a host command into a directory fleetd owns, which
  looks like a layering violation and is the only way `mirror.age` can
  exist without a new mount or route. It is one atomic rename of one
  file fleetd only ever reads.

Gates re-run after these fixes: `go build ./...`, `go vet ./...`,
`gofmt -l .` silent, `go mod tidy` byte-identical, `golangci-lint run`
**0 issues**, `go test -race -count=5 ./...` green across the module
(exit status read directly, not through a pipe).

**The fire drill was NOT re-run for this pass** — see L4 below for what
was run instead, and why it is not the same thing.

#### L4 PASS — the five refusals against the real binary, 2026-08-06

No fleetlab was standing (`lab.sh` was down and its fixed ports free,
but the drill is DISRUPTIVE and a sibling wave-2 agent shares this box),
so this pass drove `vibe fleet mirror` from a built binary against a
synthetic front-host state dir in a scratch directory. Nothing here
touched production's llama-swap `:9000` or the vibe daemon `:9001`.

```
=== mirror ===
wrote .../fleet-mirror-20260806T022236Z.tar.gz (7 files, 2868 bytes)
  contains the control-plane token: this archive is as sensitive as the front host.
  warnings:
    - the front is addressed by literal IP (http://127.0.0.1:19000) …
=== verify ===
OK: 7 entries, every sha256 matches.

A. restore onto a fresh standby
   takeover probe: 127.0.0.1:19001, 127.0.0.1:19000 answered nothing   <- REV-3: it says it LOOKED
   restored 7 files.

B. something answers on the front's address
   STILL ANSWERING: the front at 127.0.0.1:19000
   vibe: the fleet's address still answers … (nothing written)

C. --overwrite over a LONGER append-only ledger                        <- REV-2
   PRESERVED: … usage.jsonl.pre-restore-20260806T022250Z (it was not a prefix of the archive's copy)
   live ledger:      2 rows (the archive's)
   preserved sidecar: 4 rows (the box's own) — nothing destroyed

D. a state dir that already holds this box's token                     <- REV-5
   vibe: this box already holds part of a fleet's state: state/ already holds …/token
   token after: THIS-BOXES-OWN-TOKEN     (and no intent.json beside it)

E. a mirror whose hosts.yaml declared no address                       <- REV-3
   vibe: nothing to probe: the mirror recorded no fleetd or front address …
   wrote: 0 entries
   with --probe-addr 127.0.0.1:19099:
   takeover probe: 127.0.0.1:19099 answered nothing
   written  state/fleet/intent.json …
```

**What this is not.** No cells, no announces, no SIGKILL, no timing, no
second process assuming an identity. It exercises the five decisions this
review changed, through the real CLI, on real files — and nothing about
whether a fleet comes back. L1's drill remains the only evidence for
that, and it ran against the feature commit, not this one. Step 2b of
`gate-c19-drill.sh` (REV-5's refusal against a live lab) is written and
**unwatched**.

## U2 addendum — the third state the takeover probe could not express (2026-08-06)

An integration-test pass over the fleet control plane, on the observation
that this repo's entire vocabulary for "unreachable" is an immediate
ECONNREFUSED or an NXDOMAIN. Both fail in microseconds, so no timeout
path anywhere had ever executed. In `fleetmirror` that was not a coverage
gap; it was a live hazard in the one guard C19 rests on.

**The dormant seam.** `RestoreOptions.Dial` has been injectable since the
feature commit and **no test anywhere set it**. The only test of the
probe (`TestTakeoverProbe_QuietWhenNothingAnswers`) passes
`DialTimeout: 500ms` against a closed loopback port, which refuses
instantly — so `DialTimeout` never elapsed and nothing had ever checked
that it bounds anything.

**What that hid.** `TakeoverProbe` treated every dial error as
`continue`. A 2-second timeout and an instant RST produced the SAME empty
hit list with the address present in `probed`. So an old front sitting
behind a firewall that DROPs packets — the ordinary way a half-dead host
presents to a standby on another network — read as a dead one, and
`Restore` **proceeded**. That is two fronts under one identity: the exact
disaster the refusal exists to prevent, reached through the guard rather
than around it. A refusal means "nothing is there, go ahead"; a timeout
means "I could not find out"; collapsing them resolved the ambiguity in
the one direction that cannot be undone afterwards.

It is the same class as REV-3 one layer in. REV-3 separated *nothing to
dial* from *nothing answered* (`ErrNoProbeTargets`). This separates
*nothing answered* from *no answer either way*.

**The change** (`internal/vibe/fleetmirror/restore.go`, and nothing else
in production):

- `ScanTakeover` replaces `TakeoverProbe` as the real entry point and
  returns three slices — `Hits`, `Settled`, `Unknown`. Only "no hits AND
  a non-empty `Settled` AND an empty `Unknown`" is a green light.
- `classifyDialErr` names the DEFINITE negatives — `ECONNREFUSED`,
  `ECONNRESET`, `*net.DNSError{IsNotFound}` — and everything else is
  doubt. The list is closed by construction: a failure mode nobody
  anticipated lands in doubt and costs an operator one `--force`, where
  the other default costs two ledgers that can never be reconciled.
- `probeOne` holds the dialer to `DialTimeout` rather than trusting it,
  and a dial that overruns is unsettled — never a miss.
- `Restore` fails closed with `ErrProbeInconclusive`, checked AFTER the
  hits (an answered address is the more specific finding) and BEFORE the
  empty-probe branch (telling an operator "your mirror recorded no
  address" is a lie when one was recorded, dialed, and never answered).
- `RestoreReport.Unresolved` carries it out to `--json`. `Probed` now
  means SETTLED, and the deprecated two-return `TakeoverProbe` returns a
  NIL second slice whenever anything went unsettled, so a caller holding
  the old `len(probed) == 0` guard stops rather than proceeds.

The shape is copied from `certNotAfter`
(`internal/vibe/fleetapi/doctor.go`), which branches on `ctx.Err()` so
that "the budget ran out" is never reported as "the host is off", and is
honest in its summary about the residual.

**`--force` stays the only escape, deliberately.** C19's whole design is
that a HUMAN confirming the old front is DOWN is the step everything
rests on, and `--force` is the only thing in the system that asserts a
human did. A second, gentler flag for "the probe timed out, proceed
anyway" would be an easier lever reaching the same irreversible place, so
there is not one. `--probe-addr` remains the escape that is still a
probe: name the old front by IP and the refusal becomes an answer.
`TestRestore_ForceRemainsTheOnlyEscapeFromAnUnsettledProbe` pins it.

**`internal/vibe/observed` is NOT the carrier for this state**, and it is
worth writing down why, since C20 built it for exactly this defect class.
`observed.Value[T]` says *nobody looked*. This state is *somebody looked
and the network did not answer* — a different proposition, and the one
the operator has to act on. Three concrete reasons: (1) the load-bearing
part is the REASON string ("the dial timed out …"), which a
`Value[bool]` has nowhere to put, and without it the refusal cannot tell
an operator what to do next; (2) `Restore` never wants a boolean — it
wants to ENUMERATE the addresses it could not settle and name them, which
is a slice, not a scalar; (3) the JSON surface is read by a human
mid-incident, and `"unresolved": [{"addr": …, "why": …}]` says what
`{"answered": null}` cannot. What `observed` supplies here is the
DISCIPLINE — a zero value that means "no evidence" — and `TakeoverScan`
takes it structurally: an address contributes to exactly one of the three
slices, and the empty scan is the one that refuses. If a machine-readable
per-address tri-state is ever wanted on the wire, `observed.Value[bool]`
is the right thing to add BESIDE the reasons, not instead of them.

**Mutation table.** Each applied to the production line, the named test
observed RED, the mutation restored.

| # | mutation | red |
|---|---|---|
| 1 | timeout classified as `verdictNothingThere` | `TestClassifyDialErr_OnlyADefiniteNegativeMeansNothingIsThere` (3 subtests), `TestRestore_RefusesWhenTheProbeCouldNotFindOut` |
| 2 | `Restore`'s inconclusive branch disabled | `TestRestore_RefusesWhenTheProbeCouldNotFindOut`, `TestRestore_ATimedOutProbeIsNotReportedAsNothingToDial`, `TestRestore_ForceRemainsTheOnlyEscapeFromAnUnsettledProbe` |
| 3 | unsettled address also appended to `Settled` | `TestTakeoverProbe_TheTwoReturnShapeDegradesToARefusal`, `TestRestore_RefusesWhenTheProbeCouldNotFindOut`, `TestTakeoverProbe_ADialerThatIgnoresItsBudgetIsStillBounded` |
| 4 | `probeOne`'s watchdog disarmed | `TestTakeoverProbe_ADialerThatIgnoresItsBudgetIsStillBounded` ("the probe hung on a dialer that ignored its timeout") |
| 5 | nil-conn/nil-err guard removed | `TestTakeoverProbe_ADialerReturningNeitherIsUnsettled` (nil dereference in `probeOne`) |
| 6 | inconclusive checked AFTER the empty-probe branch | `TestRestore_ATimedOutProbeIsNotReportedAsNothingToDial` |
| 7 | unknown made to outrank a hit | `TestRestore_AnAnsweredAddressOutranksAnUnsettledOne` |
| 8 | a constant passed to `dial` in place of `timeout` | `TestTakeoverProbe_TheDialSeamIsHandedTheConfiguredTimeout` |
| 9 | `ECONNREFUSED` dropped from the definite negatives | `TestRestore_ASettledProbeStillProceeds`, `TestTakeoverProbe_TheDefaultDialerStillSettlesARealRefusal` — the guard must not have swallowed the ordinary recovery |
| 10 | the two-return wrapper returns `scan.Settled` on a mixed scan | `TestTakeoverProbe_TheTwoReturnShapeRefusesOnAMIXEDScan` (U2-R1) |
| 11 | `probeOne` reads the error before the connection | `TestTakeoverProbe_ADialerReturningBothIsAHit` (U2-R2) |
| 12 | `ECONNRESET` put back among the definite negatives | `TestClassifyDialErr_…/doubt/connection_reset_by_a_live_peer` (U2-R3) |
| 13 | the source scan excludes by basename | `TestNonTestCallersOf_FindsAPlantedCaller` (U2-R4) |

`TestNonTestCallersOf_FindsAPlantedCaller` is the same idea applied to
the source scan that keeps the deprecated two-return probe out of
production code: a guard that can only pass is not a guard.

**Independent review addendum (U2).** A second reviewer read the diff and
found four real defects in it, three of them the same class the change
exists to close — a fail-open hiding inside the fix.

**U2-R1 (high) — the two-return wrapper failed OPEN on a MIXED scan.**
Dropping the unsettled addresses and returning the settled ones was not
enough. fleetd refused (host rebooted) and the front timed out (alive,
behind a DROP rule) left ONE address in `probed` and no hits — which is
the all-clear, handed to a caller that cannot see the third state, for
the exact scan that most needs a refusal. Returning a SUBSET of the
evidence is the dropped bit wearing a different coat. `TakeoverProbe`
now returns a nil second slice whenever `Unknown` is non-empty. There
are no production callers today (`TestTakeoverProbe_HasNoProductionCallers`),
so it was latent — in the one function whose stated reason to exist is
safe degradation.
*Mutation:* restore `return scan.Hits, scan.Settled` →
`TestTakeoverProbe_TheTwoReturnShapeRefusesOnAMIXEDScan` fails naming the
all-clear. Restored.

**U2-R2 (medium) — a dialer returning BOTH a connection and an error was
scored as absence.** `probeOne` checked the error first, so a wrapper
that hands back a usable conn beside a non-fatal error — a proxy,
happy-eyeballs, a future TLS or SOCKS seam — had its CONNECTION dropped
unread and unclosed while its error was classified, which for an
`ECONNREFUSED` meant `verdictNothingThere`: the green light, with a
session established against the live front. The conn is now checked
first, and a connection is a hit whatever came with it. The asymmetric
twin of the `(nil, nil)` guard, going the dangerous way.
*Mutation:* check the error first →
`TestTakeoverProbe_ADialerReturningBothIsAHit` fails. Restored.

**U2-R3 (medium) — `ECONNRESET` was in the definite negatives.**
`ECONNREFUSED` means nothing is listening; `ECONNRESET` means something
answered and then tore the connection down — a reverse proxy up and
shedding load, a middlebox — which is evidence of a LIVE peer, nearer a
hit than a miss. Reading it as "go ahead" was fail-open inside the list
whose comment says it is closed by construction, and the first version of
this change had a test pinning it there. Dropped to doubt.
*Mutation:* put `ECONNRESET` back →
`TestClassifyDialErr_.../doubt/connection_reset_by_a_live_peer` fails.
Restored.

**U2-R4 (low) — the source scan excluded by BASENAME.** `definedIn` was
`"restore.go"`, so any file of that name anywhere in the repo was
invisible: a future `internal/vibe/fleetapi/restore.go` calling straight
into the deprecated probe would have passed the guard silently. Now
compared as a root-relative PATH, and the scan matches the bare
identifier so a caller taking the function VALUE is caught too.
*Mutation:* compare `filepath.Base(definedIn)` →
`TestNonTestCallersOf_FindsAPlantedCaller` fails on the same-named file
in another package. Restored.

The review also confirmed, by instrumented probes rather than by
reading: the ordering of the three refusals in `Restore` has no path
where an unsettled dial reads as go-ahead; `--force` leaves both
`Probed` and `Unresolved` nil rather than a false green light; the
watchdog's reaper really does close a late-arriving connection; and no
existing guard is weakened — `mirror.go` is untouched, so the
credential-values-never-enter-an-archive rule is intact, and
`TakeoverUnknown.Why` carries dial errors only.

**L5 PASS — `gate-c19-drill.sh` after the change, 2026-08-06.** A real
fleetlab (4 llama-swap processes, a real fleetd, 3 announcing cells) on a
shifted port range so a parallel wave could not collide: copies of
`lab.sh`/`gl.sh`/`gate-c19-drill.sh` under `/tmp/fleetlab-u2-scripts`
with 9640-9643→9820-9823, 9651-9653→9831-9833, 9720-9724→9826-9829,
upstreams 5980-6019→6080-6119. The rigs take every one of those as a
LITERAL — `FLEETLAB_DIR` is the only knob, there is no port base — so
running two of them at once is not possible without this, and nothing in
`scripts/` was edited to do it.

The teardown is the part that matters for a parallel wave, and it needed
a second edit: `sweep()`'s four pgrep patterns are anchored on `$LAB`
(safe under a private `FLEETLAB_DIR`), but its fifth kills
`llama-server .*--port (59[89][0-9]|60[01][0-9])` — the SHARED upstream
range, i.e. another agent's llama-servers. Re-anchored to this copy's own
`(60[89][0-9]|61[01][0-9])` before the lab was ever started. Production's
llama-swap `:9000` and daemon `:9001` were never touched: both were still
the same pids afterwards.

```
=== 2. restore REFUSES while the fleet's own address still answers ===
STILL ANSWERING: fleetd at 127.0.0.1:9827
STILL ANSWERING: the front at 127.0.0.1:9820
vibe: the fleet's address still answers: fleetd (127.0.0.1:9827), the front (127.0.0.1:9820). …
(refused, wrote nothing — correct)

=== 2b. restore REFUSES to mix this box's state with another fleet's (review REV-5) ===
vibe: this box already holds part of a fleet's state: state/ already holds …/token …
(refused, kept this box's token — correct)

=== 3. SIGKILL fleetd and the front llama-swap (the front host dying) ===
killed fleetd (pid 1788878)
killed swap-front (pid 1788788)
=== 3b. invariant 4: the CELLS keep serving with the control plane gone ===
  alpha    /v1/models -> 2 model(s)
  charlie  /v1/models -> 1 model(s)

=== 4. restore onto the standby box ===
takeover probe: 127.0.0.1:9827, 127.0.0.1:9820 answered nothing
  …
restored 14 files.

=== 5. recovery timings (from the SIGKILL) ===
  fleetd answering                   1.0s
  the front serving the catalog      1.0s
  all 3 cells announcing again       6.1s

=== 6. what survived ===
  token identical:   yes
  bravo before:      {"display":"DRAINED","intent":{"state":"drained","reason":"c19 fire drill",…}}
  bravo after:       {"display":"DRAINED","intent":{"state":"drained","reason":"c19 fire drill",…}}
  ledger lines:      3 -> 3   (sha e485a3c04cdc -> e485a3c04cdc)

=== 6b. fleet doctor on the standby ===
OK      fleetd.token         control-plane token loaded from the state dir (not minted at this start)
OK      front.image_pin      front image is digest-pinned
OK      mirror.age           state mirrored less than a minute ago (declared limit 36h)

=== 7. the restored control plane can actuate ===
Resumed bravo. Models return by JIT on next request; intent cleared.
```

Both directions on real sockets: the refusal fires against a live fleet,
and after the SIGKILL the same two addresses settle as definite negatives
and the restore proceeds — the third state did not make the ordinary
recovery any harder, and 6.1s to a standby holding the same token,
the same declared drain and the same ledger is the same shape L1
measured at ~10s.

This transcript predates the four review fixes below, and none of them
can move it: U2-R1 is in the deprecated wrapper, which `Restore` does not
call; U2-R2 and U2-R3 concern dialer shapes and an error class the drill
never produces (every address here answers or gives ECONNREFUSED); U2-R4
is test-only. L6 below is the same binary AFTER the fixes.

**L6 PASS — the new refusal through the real CLI, on a real socket.** The
drill above proves the ECONNREFUSED path; this is the other one, run
against the built binary after the review fixes, `--dry-run` throughout
so nothing could be written. The box's default route happened to be down,
which made the kernel answer `ENETUNREACH` rather than time out — a
better test than the one intended, because ENETUNREACH is an error class
this change never enumerated, and default-deny is what it exists to
prove:

```
$ vibe fleet mirror restore …tar.gz --state-dir … --dry-run --probe-addr 192.0.2.1:9443
dry run: nothing written.
vibe: the takeover probe could not find out whether the fleet's address still answers:
  declared address (192.0.2.1:9443): dial tcp 192.0.2.1:9443: connect: network is
  unreachable — neither an answer nor a definite refusal. …                       [exit 1]

$ … --probe-addr 127.0.0.1:1            # a definite negative
takeover probe: 127.0.0.1:1 answered nothing
  would-write    state/token …                                                    [exit 0]

$ … --probe-addr 192.0.2.1:9443 --force # the escape, still there
  would-write    state/token …                                                    [exit 0]
```

Note what the refusal did NOT print: the `takeover probe: … answered
nothing` line is absent, because an unsettled address is not in `Probed`.
That is the whole reason for keeping the two lists apart (and residual 3
is what remains of it in the mixed case).

**Residuals, all named rather than fixed here. The first is a KNOWN
FAIL-OPEN and the most important line in this addendum.**

1. **CLOSED by U3 (2026-08-08).** *A DNS `IsNotFound` is still judged a
   definite negative, and the
   reason it survives is fixture coupling, not conviction.* The first
   draft justified it as "the identity a standby assumes IS that name; if
   it resolves nowhere, no client is reaching the old front by it" —
   which reasons about the wrong host. Resolution happens on the CLIENT.
   A standby on a different VLAN whose resolver has no internal zone, or
   whose `/etc/resolv.conf` points at the front host that just died,
   NXDOMAINs every recorded name while the old front is alive and serving
   every client by IP. All addresses land in `Settled`, `Hits` and
   `Unknown` are both empty, and the restore PROCEEDS. That is this
   change's own hazard, surviving in the branch that was widened rather
   than narrowed.

   It is kept because roughly a dozen pre-existing restore tests reach
   their green light only through it (`newFixture` records
   `front.example.lan:9000` and `fleetd.example.lan:9001`). Closing it is
   two lines: classify `IsNotFound` as unsettled, and give
   `standby.opts()` a `Dial` returning `ECONNREFUSED` so no restore test
   touches the network. Both are in `mirror_test.go`, which this unit
   does not own. **Whoever owns that file next should do it.**

   *It was both lines, exactly as predicted, plus ten fixture setups —
   see [U3](#u3--the-two-cross-unit-fixes-2026-08-08).*

2. **CLOSED by U3 (2026-08-08), by the same two lines.** *The
   pre-existing restore tests now depend on the developer's
   resolver.* Before this change any dial error was `continue` and the
   address was appended to `probed` BEFORE the dial, so those tests
   passed on any resolver. Now, on a resolver that resolves
   `.example.lan` — ISP NXDOMAIN hijacking, a captive portal, a corporate
   wildcard zone — the dial to the hijacked address times out and the
   restore refuses, burning 4s per test first. The same two lines in
   residual 1 close this too, and it is the better reason to do them.

3. **`printRestore` in `internal/vibe/cli/cmd_fleet_mirror.go` prints the
   SUCCESS SENTENCE on a mixed refusal.** Its
   `len(Takeover) == 0 && len(Probed) > 0` line renders
   `takeover probe: … answered nothing` — character for character the
   line the drill transcript shows on a successful restore — while
   `Restore` is returning `ErrProbeInconclusive` about the address that
   is missing from that list. The refusal is intact (nothing is written,
   the error prints, the command exits non-zero) but the human-readable
   line above it reads as an all-clear, and `rep.Unresolved` is never
   printed at all. `restore` also has no `--json`, so the new field's
   only surface today is the error string, and no `--probe-timeout` flag
   exists for an operator whose network is merely slow. The patch, for
   the file's owner:

   ```go
   if len(rep.Takeover) == 0 && len(rep.Probed) > 0 && len(rep.Unresolved) == 0 {
       fmt.Fprintf(w, "takeover probe: %s answered nothing\n", strings.Join(rep.Probed, ", "))
   }
   for _, u := range rep.Unresolved {
       fmt.Fprintf(w, "COULD NOT SETTLE: %s at %s (%s)\n", u.What, u.Addr, u.Why)
   }
   ```
4. **The whole scan is bounded by N × 2 × `DialTimeout`**, since
   addresses are dialed in sequence: 8s for the two recorded ones at the
   2s default, N × 4s when an operator repeats `--probe-addr` (which is a
   `StringArrayVar` and unbounded). Sequential on purpose — this is the
   guard everything else rests on, two addresses is the normal case, and
   doctor's parallel fan-out buys seconds at the cost of being harder to
   reason about here. Also: a genuinely hung dialer leaves `probeOne`'s
   dial goroutine and its reaper alive until it returns. Harmless for a
   one-shot CLI; `ScanTakeover` is exported, and a daemon caller would
   accumulate two goroutines and a socket per probe.

5. **CLOSED by U3 (2026-08-08).** *`gate-c19-drill.sh` never creates
   `$DRILL/run` or `$DRILL/logs`*
   (it `rm -rf`s `$DRILL` and then makes only four dirs), so step 4b's
   two redirections and two pidfile writes fail, and steps 5-7 — the
   standby coming up, and the RECOVERY TIMING the drill exists to
   measure — report `NOT REACHED in 60s`. Pre-existing since the feature
   commit; C20 touched only the `rm -rf` line. The first run here hit it
   exactly that way; the transcript above is the second, with
   `"$DRILL/run/rt" "$DRILL/logs"` added to line 59's `mkdir -p` in the
   /tmp copy. The one-word fix belongs in `scripts/`, which this unit
   does not own.

   *U3 made the `mkdir -p` complete and made a missing directory FATAL
   rather than a `NOT REACHED`. It also followed this residual back to
   L1: the same defect means the committed script could never have
   produced L1's timings either, and that qualification is now on the
   L1 transcript.*

## U3 — the two cross-unit fixes (2026-08-08)

Off `origin/main` at `937c46a`, as `fix/cross-unit-dns-and-drill`. Both
were known, both were diagnosed in the U2 addendum above as residuals 1,
2 and 5, and neither could be fixed from the branch that found it: one
needed a test file that unit did not own, the other needed `scripts/`.
Nothing here is a new discovery. What is new is that they are closed.

### Fix 1 — the DNS fail-open in the takeover probe

Two changes in `internal/vibe/fleetmirror`, one production and one
fixture, and the second is not the smaller of the two:

1. `classifyDialErr` now returns `verdictUnsettled` for a
   `*net.DNSError` with `IsNotFound`, with a `Why` that says the doubt is
   about **this box's resolver** and not about the old front. NXDOMAIN
   therefore lands in `TakeoverScan.Unknown`, and `Restore`'s existing
   `len(scan.Unknown) > 0` branch turns an all-NXDOMAIN scan into
   `ErrProbeInconclusive` instead of a clean empty hit list. (Its message
   said "a probe that timed out", which was the only unsettled state that
   existed when it was written; it now says "never settled" and names the
   resolver case beside the firewall one.)
2. `standby.opts()` in `mirror_test.go` now carries a `Dial` that
   answers `ECONNREFUSED` for everything, so no restore fixture in this
   package reaches a resolver or a socket.

The second is not incidental to the first, it is what makes the first
honest. Before it, every restore fixture in this package got its green
light from `front.example.lan` and `fleetd.example.lan` failing to
resolve on the developer's box: the probe's own fail-open, load-bearing
underneath the suite whose job is to guard it. The same suite would have
passed identically on a standby whose resolver was broken while the old
front was up, which is the scenario the whole phase exists for.

**The fixture accounting**, because "roughly a dozen" was the reason this
was deferred and the split matters:

- **One assertion was pinning the bug**, and said so:
  `TestClassifyDialErr_OnlyADefiniteNegativeMeansNothingIsThere` listed
  `"the name does not resolve from here"` in its `definite` map — the map
  of errors that are ALLOWED to mean "nothing is there" — under a comment
  reading *"here under protest… when it is closed, this line moves
  down"*. It has moved down, into `doubt`. That comment is the reason
  this was a five-minute job rather than an archaeology one, and it is
  the argument for writing residuals down at the line rather than only in
  a doc.
- **Ten fixtures were merely resting on it** and needed setup, not
  rethinking: `TestRestore_PlacesFilesAndKeepsTheTokenMode`,
  `_DoesNotOverwriteWithoutBeingAsked`,
  `_SkipsSlotsWithNoDestinationAndNeverPlacesExtras`,
  `_DryRunWritesNothing`, `_RefusesContentThatChangedAfterVerification`,
  `_PreservesTheAppendOnlyLedgerItWouldReplace`,
  `_ExtendingTheLedgerNeedsNoSidecar`, `_ARecordedAddressIsStillProbed`,
  `_RefusesToMixTwoFleetsInOneStateDir` and
  `_WritesNothingWhenAPayloadChangedAfterVerification`. Measured, not
  estimated: with the production fix in and the test files reverted,
  those ten fail and nothing else does.
- **Two fixtures keep the real dialer, by name and with a reason.**
  `TestRestore_RefusesWhileTheFleetsOwnAddressAnswers` sets `Dial = nil`
  because its subject is a real socket answering, and
  `TestRestore_RefusesWhenThereWasNothingToProbe`'s `--probe-addr` leg
  does the same so one path in the review suite still gets its definite
  negative off an actual `connect()`. Both dial loopback literals, so no
  resolver is involved either way.

**The new test** is
`TestRestore_AResolverThatNXDOMAINsEverythingIsNotADeadFront`: both
recorded names NXDOMAIN, which is what one broken resolver does — it
fails for every name at once, which is exactly what made it look like
unanimous evidence of absence. It asserts `ErrProbeInconclusive`, an
empty `Probed`, both names in `Unresolved`, no token on disk, and that
`--force` still gets through.

**Mutation-bound.** Reverting `restore.go` alone: the new test fails with
`Expected error … but got nil` — the restore PROCEEDED, i.e. two fronts
under one identity — and the classifier's `doubt/the name does not
resolve from here` subtest fails with `expected 3, actual 2`. Both
restored.

### Fix 2 — `gate-c19-drill.sh`, and where L1's headline number came from

`scripts/fleetlab/gate-c19-drill.sh` `rm -rf`s `$DRILL` and then
re-creates four of the six directories it writes into. Fixed: `$DRILL/run`
and `$DRILL/logs` are in the `mkdir -p`, a `need_dir` helper makes a
missing directory FATAL wherever the script is about to write into one,
and step 4b now checks that the standby front and fleetd are still alive
a second after launch before the clock starts.

The point of the `need_dir` guard is ground rule 10 in miniature. `NOT
REACHED in 60s` reads as a measurement — the standby was watched for a
minute and did not come up. It was not one: nothing had been started, so
nothing was measured, and "not attempted" was wearing "did not make it"'s
clothes. Those must not share a line, so a missing directory now stops
the drill, and the `NOT REACHED` message says in as many words that the
standby was running.

**Following it back one step is the finding.** The defect has been there
since the feature commit (`cfb3d40`) with the `rm -rf` already above the
`mkdir -p`, so no run of the committed script can ever have had those
directories — they cannot be left lying around by earlier work, because
the script deletes their parent first. **The committed rig has never been
able to produce a timed "5. recovery timings" block; all three of its
lines come out `NOT REACHED`.** So L1's 10.1 s
and 14.1 s, and §6's "the drill's mechanical half is 10 seconds", came
from a patched copy of the script whose patch was never recorded. L5's
6.1 s has the same provenance and, to its credit, says so.

The numbers are qualified in place rather than deleted: they are the only
record there is, the survival evidence in the same transcripts does not
depend on them, and an order of magnitude of "seconds, not minutes" is
what §6 actually leans on. What is unverified is the seconds.

**L7 — the drill on the fixed script: NOT RUN, and it is not
'not possible'.** It is runnable; it was not run here. `gate-c19-drill.sh`
requires `./lab.sh up` (it refuses without a lab binary and a fleetd
answering at `127.0.0.1:9721`) and then drives the shared fleetlab port
range — front `9640`, cells `9641-9643`, fleetd `9721` — and SIGKILLs
processes in it. Four other agents were working this box concurrently and
`lab.sh`'s teardown sweep is not scoped to a single rig, so running it
would have killed their labs. The gate stays open.

What WAS run, and is therefore all that may be claimed: `bash -n` over
the script (clean), `internal/shelllint` (clean), the precondition path
end to end under a bogus `FLEETLAB_DIR` (aborts at the missing lab binary
before creating or deleting anything), and `need_dir` in isolation
(present directory passes, absent directory exits 1 and the line after it
never runs). That is a syntax-and-guard check. It is not the drill.

**To close L7:** `FLEETLAB_DIR=/tmp/fleetlab-c19 ./lab.sh up` then
`FLEETLAB_DIR=/tmp/fleetlab-c19 ./gate-c19-drill.sh` on a box where the
9640-9643/9721 range is free, and paste the "5. recovery timings" block
in beside L1's. If the fix is right the block prints three durations; if
it is wrong it now aborts with a named directory instead of three
`NOT REACHED` lines.

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

### Added by U3 (2026-08-08)

Two amendments to the three bullets above, both from this unit and
neither applied here:

- **AGENTS.md**, in the front-failover section's first bullet: the
  refusal fails closed on *doubt*, not only on an answer. A dial that
  times out, a name that NXDOMAINs, and any error class nobody
  enumerated all stop the restore (`ErrProbeInconclusive`); only an
  answer (`ErrTakeover`) and a definite `ECONNREFUSED` are conclusions.
  The NXDOMAIN case is worth its own clause, because it is the one that
  fires on **every recorded name simultaneously** — a standby's resolver
  is either useful or it is not — and it therefore used to look like
  unanimous evidence that the old front was gone. `--force` remains the
  single escape, and it is a claim that a human checked.
- **`docs/design/fleet-control-plan/README.md`**, C19's row: the status
  string should read *"L1 PASS (harness fire drill; recovery timings
  UNVERIFIED — see the C19 doc's U3 section)"* rather than
  *"L1 PASS (harness fire drill, 10.1 s recovery)"*. The drill ran and
  the state survived it; the seconds came off a rig that is not the one
  in the repository.

### A note for whoever merges C18

C18 (`vibe model try`) adds `paths.ModelTrialFile()`. On merge it will
fail `TestMirrorCoversEveryFleetStateFile` until it is classified — which
is the test working, not a conflict. The right answer looks like an entry
in `notFleetState`: the trial journal is CELL-side by C18's own design
(the rollback has to be reachable from the box that can perform it), and
therefore does not die with the front — the same reasoning that already
excuses `cell-usage.json` and C8's baselines. One line, in the test's
exclusion map, with that sentence.
