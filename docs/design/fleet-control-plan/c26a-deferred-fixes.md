# C26a — the four fixes that were recorded instead of made

Status: **PR OPEN**, off `c26a-deferred-fixes` branched from `main` at
`e590b05`.

This phase has no subject of its own. It is the week's *deferral ledger*
paid off: four findings that were deliberately written down rather than
fixed, because each sat outside the unit that found it. They are unrelated
to each other and share one property, which is the only reason they ship
together — **each one was a defect somebody had already seen, understood
and left in place**, and a finding that survives being understood is the
kind that survives a year.

| # | finding | where it was found | why it was deferred |
|---|---|---|---|
| 1 | `fleetannounce.execSh` ran an operator's shell string through a bare `exec.CommandContext` | U3, while fixing the same defect in the daemon's two sites | fixing it meant reworking another package's test seam |
| 2 | `checkLlamaBinary` failed doctor for a binary nothing on the box declares | PR #14, from a cloud_peer laptop | the finding was about doctor; the PR was about peers |
| 3 | `aliasClaimants` excluded cloud_peer, so an alias could silently collide with a peer's model id | PR #14's own rule, applied to the next file over | the exclusion was documented as intentional and needed a decision, not an edit |
| 4 | `vibe profile new --kind cloud-peer` had no template | PR #14 | PR #14 legalised the shape; shipping a starter is a separate change |

Every guard below was mutation-verified: **44/44 registry entries caught in
52s** with the seven new ones included (§6).

---

## 1. The fourth `sh -c` site, and the seam in front of it

`internal/vibe/fleetannounce` runs a cell's drain/resume verb when a newer
`desired_intent` arrives. It built its own command:

```go
out, err := execCmd(ctx, "sh", "-c", cmd)
```

which is precisely the shape `internal/vibe/shellcmd` exists to stop.
`exec.CommandContext` kills the process it *started*; with `sh -c` that is
the shell, and the shell is frequently not where the work is. dash forks
the command, and **every** shell forks for a script containing a `;`, an
`&&`, a subshell or a background job — which is what a real
`cell_cmds.drain` looks like. The fork inherits the output pipes, and
`Cmd.Wait` waits for the pipe *copy*. So the 60s budget fires on schedule,
the error says `signal: killed`, and the call does not return until the
operator's `systemctl stop` finishes on its own.

### Why this one was harder than the other three

`execSh` sat behind an `execCmd` function-var seam that **every test in the
package replaces**. Moving the production path onto `shellcmd` without
touching the seam would have produced a bound that no test in the package
ever executes — the fix would have been real and the coverage would have
been theatre. A seam that tests replace with something skipping the bounds
is a seam that *guarantees* the bounds are never tested.

### The arrangement

Three pieces, in
[`fleetannounce.go`](../../../internal/vibe/fleetannounce/fleetannounce.go):

```go
var execSh = runShellVerb                      // the seam: still replaceable

func runShellVerb(ctx context.Context, cmd string) (string, error) {
	out, err := verbCmd(ctx, cmd).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func verbCmd(ctx context.Context, cmd string) *exec.Cmd {
	return shellcmd.New(ctx, cmd, verbKillGrace)  // the bound
}
```

- The **seam stays replaceable**: the four existing tests script verb
  outcomes exactly as before. Their signature changed from
  `(ctx, name, args...) ([]byte, error)` to `(ctx, cmd) (string, error)` —
  strictly better, since they were asserting on
  `strings.Join(args, " ")` to recover the command they had just passed in.
- The **production path goes through `shellcmd`**, and the seam's default
  value is pinned to `runShellVerb` by
  `TestVerbSeam_ProductionDefaultIsTheBoundedRunner` rather than assumed.
  That test is the answer to "what stops the default drifting back": a
  drifted default is invisible to every other test in the package, so its
  identity is asserted directly (`reflect.ValueOf(execSh).Pointer()`).
- The **bound is proved through the unreplaced seam**. Both behavioural
  tests drive `Client.runVerb` — the real production entry point — with a
  caller deadline of 400ms, which beats the 60s `verbBudget` `runVerb`
  derives. Nothing is stubbed between the test and the shell.

`verbCmd` is split out so the structural test can read `SysProcAttr`,
`Cancel` and `WaitDelay` back off a built command on a platform where a
process cannot be driven out of its own group — the same layering
`shellcmd`'s own tests use.

### The two traps, and what avoids them here

Both make a test pass for the wrong reason, and both had already cost this
repo a CI round.

- **A command with no pipe-backed output has no pipe to block on.** With
  `Stdout`/`Stderr` nil, exec wires to `/dev/null` and the defect
  disappears entirely. `runVerb` uses `CombinedOutput`, so the pipes are
  real — and
  `TestDesiredIntentVerb_TheWaitIsBoundedEvenWhenTheKillCannotLand`
  asserts a **floor** (`elapsed >= budget + grace`), not only a ceiling.
  A ceiling alone passes on the `/dev/null` version.
- **A one-word command is not a fork.** bash exec-optimises
  `sh -c '<one word>'`; dash does not, so the same fixture exercises a
  different number of processes on the workstation and in CI. The fixtures
  fork explicitly:

  ```
  forkingVerb  = "sleep 10 & echo $! > %s; wait"
  escapedVerb  = "setsid sleep 10 & echo $! > %s; wait"
  ```

  `setsid(1)` rather than `set -m`: dash's `setjobctl` opens `/dev/tty` and
  declines **silently** with no controlling terminal, which under
  `go test` is always, while bash sets the group anyway. That is the same
  shell-shaped split that produced the original defect, one layer up.

The escape test additionally asserts the grandchild is **alive and in a
group of its own** when the call returns — otherwise it would have
exercised the group kill and not the wait delay, and reported success
either way.

## 2. `checkLlamaBinary`: a check that fired for a box it did not describe

`vibe doctor` hard-failed (exit non-zero) when `llama-server` was not on
`$PATH`, unconditionally. A laptop whose only profiles are `cloud_peer` —
pointed at a peer through a remote front, which PR #14 made a supported
configuration — failed doctor over a binary it will never invoke. The same
mis-fire hit **comfyui-only and mlx-only** boxes, so it is not a cloud_peer
bug and is not fixed as one: the applicable set is computed from what is
*declared*, not from any one backend kind's absence.

The file's own pattern is the fix. `checkRSVGForVision` and
`checkDockerForProfiles` both scan first and return `ok=false` when nothing
needs the tool; `checkLlamaBinary` now does the same, and so does
`checkLlamaVersion` — warning `skipped (llama-server not on $PATH)` on a
box that never spawns it is the same mis-fire one line down, and gating one
and not the other is how a guard ends up on some of its call paths. One
scan feeds both.

**C13's naming rule.** The check keeps the name `llama-server`. What it
proves has not changed — *llama-server is on `$PATH`* — only whether it
runs at all, and `docker` / `rsvg-convert (vision)` are named the same way,
after the tool, with applicability carried by the `ok=false` gate. A name
like `llama-server.needed` would describe the gate rather than the
assertion, which is the inversion C13 warns about.

**Two decisions inside the scan**, both of which change which boxes the
check fires on:

- A definition that pins its own `binary:` is **not** a user. It names an
  absolute path and never consults `$PATH`, so a fleet box running a custom
  build must not be failed over a binary nothing looks for.
- Declaration is read straight from the YAML, **not** through
  `profile.Load`. Every other scan in the file uses the loader, and for
  this check that would be a silent disarm: `profile.Load` validates, a
  `llama_server` profile whose GGUF has not been pulled yet fails to load,
  and the check would become not-applicable on exactly the box that most
  needs it. What is asked is what the file *declares*, and that is
  answerable without the model being on disk.
  `TestLlamaServerUsers_AnUnpulledModelStillDeclaresTheBinary` pins it,
  and fails loudly if the loader ever starts accepting a missing path.

All four shapes are tested (`TestLlamaServerUsers_TheFourShapesOnDisk` plus
the three `TestCheckLlamaBinary_*`): declared + present, declared + absent
(still FAIL), nothing declared + absent (not-applicable), and a mixed box.

## 3. `aliasClaimants` and cloud_peer — the decision, and why

**Decided: peer model ids become RESERVED canonical ids; they do not become
alias claimants; and the rendered catalog gets a uniqueness backstop under
both.**

The finding: `aliasClaimants` excluded `cloud_peer` defs entirely, so peer
model ids participated in nothing. A `llama_server` def whose alias equalled
a peer's model id resolved cleanly and rendered **two entries claiming one
client-facing id**, with no error — llama-swap then serves whichever wins.
The peers-map clash check in `Render` covers `def.Name` and only `def.Name`.

This is PR #14's rule met a second time, and AGENTS.md already carries it:

> A cloud peer's catalog ids are its `cloud_peer.models` entries, **never
> its def name.** Every other backend kind is served under its def name, so
> code that keys a map by `def.Name` and is looked up by *catalog id* works
> for all of them and silently misses for peers.

### Not claimants

Deliberately, and the exclusion's doc comment now says why rather than
merely stating it. `Render` emits **no aliases for a peer stanza** —
`p := &swapPeer{Proxy: cp.BaseURL, Models: cp.Models}`, nothing else. A
peer that *won* an alias would take that alias off the def that would
actually have served it and advertise nothing in its place: the "advertise
an id nothing can route to" failure PR #54 closed, arrived at from the
other side. `TestResolveAliases_APeerStillClaimsNoAliases` pins it,
including the case where the peer declares `router.alias_owner: true`.

### But reserved

`aliasClaimants` now returns a second value: `map[modelID]peerDefName`. An
alias equal to one of those ids takes the same path an alias equal to a def
*name* has always taken — an unresolvable error, with a message naming the
peer and the two edits that fix it.

**Why not `router.alias_owner` arbitration.** The brief offered it as an
option; it does not fit, and the reason is worth writing down.
`alias_owner` settles which of two *alias claimants* keeps an alias. It has
never had anything to say about an alias that equals a **canonical id** —
`names[a]` (an alias equal to a def name) has been an unresolvable error
since the resolver was written, with no owner override, because an id is
not a claim, it is the thing claims are measured against. A peer's model
ids are canonical in exactly that sense. Making them arbitrable would mean
a `llama_server` def could *win* an id the peer still renders, which
resolves the error message and not the collision.
`TestResolveAliases_APeerModelIDIsCanonicalAndCannotBeAliased` asserts both
halves: the error, and that `alias_owner` does not dissolve it.

### The backstop, and why the resolver alone was not enough

The resolver sees **defs**, so it says nothing about:

- a `llama_server` def *named* after a peer's model id (no alias anywhere);
- two cloud peers listing the same model id.

Both render two entries under one id in silence — the same unacceptable
outcome by a path the reservation cannot see. So `Render` gains
`checkCatalogIDsUnique(cfg)`, which walks the **rendered config** — models
keys, their aliases, and every peer's models — and refuses a duplicate.
Running it over the artifact rather than over the defs is what makes it
cover the class instead of the two paths into it that are known today.
Iteration is sorted, because an error message that names a different pair
on different runs is most of the way to no error at all.

The two guards are separately mutation-registered, and the first run of the
harness proved the point: deleting the reservation left
`TestRender_TheAliasPeerCollisionSurfacesFromRender` **green**, because the
backstop caught the render anyway. The test now asserts the resolver's
wording, since only the resolver's error fires for `ResolveAliases` —
fleetd's checkout-wide pass, where no render happens at all. That is a
finding the harness produced about a test written in the same hour, which
is the argument for the harness.

## 4. The `cloud-peer` starter template

`internal/vibe/cli/profile_templates/cloud-peer.yaml`. Adding the file is
the whole mechanism: `validProfileKinds()` derives `--kind` from the embed,
so `vibe profile new <name> --kind cloud-peer` works the moment it exists.

Content follows the existing starters (a `__PROFILE_NAME__` placeholder,
`REPLACE`-marked lines, prose that says what each field buys) and
`profiles/omp.example.yaml` for the peer half. It ships a frontend, because
that is the shape PR #14 legalised and the reason most peer profiles exist;
the block is documented as droppable for a headless peer consumed over the
proxy.

**Boundary.** Example values only: `<router-host>` in the prose,
`https://api.REPLACE-provider.example` for the base URL, and an environment
variable **name** (`REPLACE_PROVIDER_API_KEY`) where a key would go — with
the comment saying the value lives in the *router's* environment and that
vibe renders config, never secrets. `TestProfileNew_KindCloudPeer` asserts
no `sk-`, no `Bearer `, and no reference-fleet hostname survives into a
generated file.

**Verification.** Two tests, in the two packages that own the two halves:

- `TestProfileInit_Kinds` (cli) already generates *every* bundled kind,
  fills its markers and runs it through `profile.Load`. The `cloud-peer`
  case fills three strings and touches nothing on disk — no weights, no
  venv, no image — which is the strongest fill any kind in that table
  needs. A template the CLI can emit and the loader then refuses is a
  broken command, not a broken profile.
- `TestCloudPeerTemplateLoadsAndRenders` (daemon), modelled on
  `example_profile_test.go`, runs the filled template through
  `frontendModelVars` and `frontend.ActivateWithContext` — the real
  activation path — and asserts `${MODEL_ALIAS}` resolves to the peer's
  single model id, `${MODEL_CONTEXT}` renders as a **number**, nothing
  unexpanded survives, and no credential-shaped value reaches the rendered
  file.

## 5. Ownership axes

Nothing here gives vibe an opinion about a peer's lifecycle.
`TestCloudPeerTemplateLoadsAndRenders` asserts `cloud_peer` still
normalises to `external` and that the template declares
`estimated_vram_gb: 0`; §3's decision is about *routing identity*, which is
the router's business and not the peer's residency. §2 removes an opinion
rather than adding one — doctor no longer asserts anything about a box that
declares nothing.

## 6. Gates

Every row below is a run, on this branch, at the code as it stands.

| gate | command | result |
|---|---|---|
| build | `go build ./...` | PASS |
| vet | `go vet ./...` | PASS |
| tests | `go test -race ./...` | PASS (38 packages, `GOTEST_EXIT=0`) |
| fmt | `gofmt -l .` | PASS (no output) |
| tidy | `go mod tidy` | PASS (no diff) |
| lint | `golangci-lint run` | PASS (`0 issues.`) |
| mutation | `VIBE_MUTATION_TEST=1 go test -run TestMutationsAreCaught ./internal/mutation/` | PASS — **44/44 in 52s** |

### The seven new registry entries

| entry | mutation | red |
|---|---|---|
| `c26a/the desired-intent verb stops sharing the builder` | disarm `SysProcAttr`/`Cancel`/`WaitDelay` at the `shellcmd.New` call site | all three `fleetannounce` bound tests |
| `c26a/the verb seam's default drifts off the builder` | repoint `execSh` at a disarmed closure | same three |
| `c26a/doctor fails a box for a binary nothing on it declares` | delete the `len(users) == 0` gate | `TestCheckLlamaBinary_NothingDeclaresItIsNotApplicable` |
| `c26a/the declaration scan stops seeing backend defs` | drop `scan(backendsDir, …)` | `TestLlamaServerUsers_TheFourShapesOnDisk` |
| `c26a/a cloud peer's model ids stop being canonical` | delete the `peerIDs` population loop | `TestResolveAliases_APeerModelIDIsCanonicalAndCannotBeAliased`, `TestRender_TheAliasPeerCollisionSurfacesFromRender` |
| `c26a/the rendered catalog stops being checked for duplicate ids` | drop the `checkCatalogIDsUnique` call | `TestRender_NoCatalogIDIsAdvertisedTwice` |
| `c26a/the cloud-peer starter stops being loadable` | empty the template's `models:` list | `TestProfileInit_Kinds` |

The last one mutates a **YAML template** rather than Go source, which the
harness supports unchanged and which is the right target: for a starter
file, the guard that matters is not that the file exists but that what the
command drops actually loads.

## 7. Production safety

`llama-swap` on `:9000` and the vibe daemon on `:9001` were live with a
resident model throughout. Nothing in this phase read or wrote
`~/.config/vibe`, `~/.config/llama-swap` or `~/.local/state/vibe`; neither
process was signalled, restarted, or sent a completion; no `vibe start`,
`vibe stop` or `vibe fleet doctor` ran against the live config.
`scripts/fleetlab` was not used at all — every test here runs against temp
dirs and fixtures. The only subprocesses this phase starts are the `sleep`
and `setsid sleep` its own bound tests fork, each under a 400ms deadline
and each asserted dead (or deliberately detached and then reaped) before
the test returns.

## For the reconciliation pass

Nothing in this phase edits the three shared docs. Recorded here for
whoever runs the reconciliation:

### AGENTS.md

- The cloud_peer rule already in AGENTS.md ("a peer's catalog ids are its
  `cloud_peer.models` entries, never its def name") has a **second half**
  worth adding, because §3 is the second time the first half alone was not
  enough to prevent the bug: *those ids are also **canonical** — an alias
  equal to one is unresolvable exactly as an alias equal to a def name is,
  and `router.alias_owner` does not arbitrate between an alias and an id.*
- If AGENTS.md names the operator-shell rule, it can now say **all four**
  call sites are on `internal/vibe/shellcmd` and that the registry holds a
  membership entry per site. The generalisable form: when a call site sits
  behind a test seam, the seam's *default value* needs its own assertion —
  moving production onto a shared builder is not done until something fails
  if the default drifts off it.
- A doctor rule, if there is a doctor section: **a check that cannot apply
  must return not-applicable, never pass and never fail** — and its
  applicability must be computed from what is *declared on disk*, read
  without validation, since validation failures are a different check's
  business and using them as a gate silently disarms this one.

### docs/design/fleet-control-plan/README.md

- A `C26a` row: *the four deferred code fixes — the last `sh -c` site, a
  doctor check that fired for the wrong boxes, a silent peer-id collision,
  and the missing cloud-peer starter*, status "PR open; all gates PASS,
  44/44 mutation".
- If there is a paragraph tracking the mutation registry's size, it moves
  from 37 to **44**.
- A paragraph after C24's, along these lines: C26a is not a phase, it is a
  **deferral ledger being paid**. Its carried rule is that a finding
  recorded outside the unit that found it is a finding with no owner, and
  the four here had each been seen and understood before they were left in
  place. The corollary that generalises: when a fix is deferred because it
  crosses a unit boundary, the note must name *what the fix costs* — all
  four of these cost under a day, and the one that looked hardest (the test
  seam) turned out to be the one that produced the most reusable rule.

### docs/design/fleet-control.md

- Nothing in §4 changes: availability is still OBSERVED, intent DECLARED,
  residency the router's, and `cloud_peer` still implies `external`.
- If §-whatever documents the rendered catalog, it can gain one sentence:
  **the router's catalog is a namespace, and every client-facing id in it —
  a models key, an alias, a peer's model — is unique by construction, with
  the render refusing a config that says otherwise.**
