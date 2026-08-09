# Contributing

Thanks for thinking about sending a patch. Both `vibe` and `vamp` live
in this repo and are MIT-licensed — feel free to fork, hack, and open
a PR.

This file is the short, mechanical version. For project-level
conventions an agent or new contributor needs to make changes that
fit (discriminated profile schema, cache invariants, stage-type
checklist, etc.), read [`AGENTS.md`](AGENTS.md) — it is the source of
truth.

## Reporting bugs / asking questions

Open an issue at
<https://github.com/gallowaysoftware/vibe/issues>. For bug reports,
include:

- `vibe --version` (or `vamp --version`) output.
- The minimal pipeline YAML or profile YAML that reproduces.
- The relevant lines from `$XDG_STATE_HOME/vibe/daemon.log` (for vibe
  bugs) or the run dir's `vamp.log` (for vamp bugs).
- Your OS + GPU (for inference-related issues).

If you're not sure whether something is a bug or expected behavior,
opening an issue to ask is fine.

## Setting up a dev environment

Prerequisites:

- Go 1.26+ (`go.mod` pins the floor).
- For the unit tests, nothing else.
- For end-to-end smokes against real inference: a CUDA-capable GPU,
  llama.cpp (`vibe doctor --install llama-cpp` will fetch it),
  optionally ComfyUI (`vibe doctor --install comfyui`).

```
git clone https://github.com/gallowaysoftware/vibe
cd vibe
./scripts/check.sh
```

`scripts/check.sh` **is** the gate. It runs what the blocking CI job
runs, in the same order, so the first thing that fails locally is the
first thing that fails on the PR:

```
go build ./...
go vet ./...
golangci-lint run                        # v2.12.2 — see .golangci.yml
go test -race -timeout 240s ./...
gofmt -l .                               # must print nothing
go mod tidy && git diff --exit-code go.mod go.sum
```

Two of those are easy to get wrong by hand, which is why the script
exists rather than a list you retype:

- **`golangci-lint` is part of the gate**, not a nicety. It is its own
  step in `ci.yml` (`bodyclose`, `misspell`, `unconvert` on top of the
  standard set) and `go vet` alone does not cover it. Install it with
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`.
  Its cache is per-**user**, not per-checkout, so a run in one checkout
  can report findings belonging to another — `check.sh` pins
  `GOLANGCI_LINT_CACHE` inside the working tree so that cannot happen.
- **`go mod tidy` on its own proves nothing.** It exits 0 having
  rewritten `go.mod`; the `git diff --exit-code` after it is the actual
  check.

Three other jobs run on every PR and are not part of the local gate,
because each is minutes rather than seconds and they run in parallel
with the blocking one:

| job | what it is |
| --- | --- |
| `mutation harness` | `internal/mutation`'s registry of {production line, edit that breaks it, tests that must go red}, applied one entry at a time to a copy of the tree. |
| `conformance (llama-swap v239 / v247)` | `internal/swaptest`'s invariant list against two pinned **real** llama-swap builds. |
| `drift` (scheduled, not on PRs) | the same list against whatever llama-swap released last. Meant to be red when upstream moves. |

To have the gate run itself before every push — it does not run
itself, and `core.hooksPath` is unset in a fresh clone:

```
git config core.hooksPath scripts/hooks
```

That is per-clone, never global; undo it with `git config --unset
core.hooksPath`, and skip it once with `git push --no-verify`.

## Sending a PR

1. **Fork and branch.** Branch off `main`. Name the branch for the
   change (`feat/<thing>`, `fix/<thing>`, `docs/<thing>`).
2. **Keep PRs focused.** One feature or one fix per PR. Refactors that
   ride along with a feature should be a separate commit (or a
   separate PR) so review can see them.
3. **Write a real commit message.** Conventional Commits style:
   `feat(vamp): <subject>`, `fix(vibe): <subject>`, `docs: <subject>`,
   `ci: <subject>`. The body says *why*, not what — `git diff` already
   shows the what.
4. **Test what you touched.** For non-trivial logic, add a unit test
   in the same package. For new stage types or executors, mock the
   external command/HTTP via the executor's injectable runner (every
   executor in `internal/vamp/*_executor.go` accepts one).
5. **Don't commit secrets, `dist/`, `*.pid`, `*.log`, `*.sock`.** All
   already in `.gitignore`, but worth saying.
6. **Open the PR against `main`.** CI runs automatically. Address any
   failures before requesting review.

A PR description that says what changed and why, plus a quick test plan
("ran the X smoke locally, behavior matches"), is enough — no template
required.

## Adding a stage type to `vamp`

See [`AGENTS.md`](AGENTS.md) "vamp stage rules" — the file lists every
file you need to touch (`Stage` struct, `Validate`, executor,
`stageCacheable`, JSON schema). The existing executors in
`internal/vamp/*_executor.go` are the working templates.

## Adding a backend to `vibe`

See [`AGENTS.md`](AGENTS.md) "vibe profile schema rules" — the
discriminator is sub-block presence under `backend:`, not a `kind:`
field. Mirror the existing `LlamaServer` / `ComfyUI` shape in
`internal/vibe/profile/profile.go` and add a `LaunchSpec` builder
in `internal/vibe/profile/launch.go` following the existing
`LlamaServerSpec` / `TabbyAPISpec` / `HTTPServerSpec` / `ComfyUISpec`
builders. Tilde-expand any new path fields in `Backend.normalize()`
(`internal/vibe/profile/backend_def.go`), not in `Load`.

## Releases

Tagged releases (`vX.Y.Z`) are cut by maintainers using goreleaser via
the `.github/workflows/release.yaml` workflow. External contributors
don't need to touch this — once a PR is merged to `main`, the next
release will pick it up.
