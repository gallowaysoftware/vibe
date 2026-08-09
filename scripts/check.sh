#!/usr/bin/env bash
# The inner loop, as one command. `scripts/check.sh` is what CI runs, in
# CI's order, so the first thing that fails here is the first thing that
# fails there.
#
# Convenience is NOT the argument. The whole gate is ~35s and six commands;
# nobody needed a wrapper for that. The argument is the lint cache.
#
# `golangci-lint`'s cache is per-USER (~/.cache/golangci-lint), not
# per-checkout, and this repo is worked in `.claude/worktrees/*` several at
# a time. Measured on this box: a run exited 1 with 7 issues in 0 seconds,
# every one of them naming a SIBLING worktree that no longer existed on
# disk; after `golangci-lint cache clean`, 0 issues in 2s. That run analysed
# nothing and reported a verdict anyway.
#
# The false-red is the annoying case and it is self-announcing — the paths
# are visibly not yours. The dangerous case is its mirror, and it is
# documented nowhere: a cached CLEAN verdict served over a tree that was
# never linted. It looks exactly like a pass. Pinning GOLANGCI_LINT_CACHE
# inside the worktree is what makes both impossible: the cache is created
# with the worktree and discarded with it, so it can only ever describe
# this tree.
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"

# Per-WORKTREE, not per-user. `git rev-parse --show-toplevel` resolves to
# the worktree's own root, so two worktrees can never share a verdict.
# .gitignore'd; `git worktree remove` takes it with the tree.
export GOLANGCI_LINT_CACHE="$root/.cache/golangci-lint"
mkdir -p "$GOLANGCI_LINT_CACHE"

go build ./...
go vet ./...
golangci-lint run
go test -race -timeout 240s ./...
# gofmt exits 0 whether or not it printed filenames, so the emptiness of the
# output is the assertion — the same shape ci.yml uses.
out=$(gofmt -l . | tee /dev/stderr)
[ -z "$out" ] || { echo "gofmt found unformatted files (see above)" >&2; exit 1; }
# `go mod tidy` alone proves nothing: it exits 0 having rewritten go.mod.
# The diff is the gate, and AGENTS.md's inner loop omitted it for months.
go mod tidy
git -C "$root" diff --exit-code go.mod go.sum

echo "check.sh: green (build, vet, lint, race tests, gofmt, tidy)"
