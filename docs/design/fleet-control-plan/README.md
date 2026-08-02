# Fleet-control implementation plan (C0–C4)

Execution plan for [../fleet-control.md](../fleet-control.md). Each
phase is one PR, independently shippable, and pays for itself before
the next starts. Read the design doc first; each phase doc is written
to be implementable on its own after that.

| phase | title | new code | depends on |
|---|---|---|---|
| [C0](c0-quick-wins.md) | Quick wins: hot reload, autostart, discoverability | ~0 lines | — |
| [C1](c1-observe-intent.md) | Observe + intent: fleetd, cells registry, `vibe cell status/await`, MCP facade | ~450 lines | C0 optional |
| [C2](c2-actuate.md) | Actuate: drain/resume RPCs, wake, `render --cell front` | ~450 lines | C1 |
| [C3](c3-announce.md) | The inversion: announce heartbeats, presence-derived render | ~600 lines | C2 |
| [C4](c4-comfort.md) | Comfort: warm targets, warm schedules, the fleet page | ~300 lines | C3 (a read-only page could ship after C1; its action buttons need C2, fingerprint badges C3) |

Line counts are order-of-magnitude scoping signals, not budgets.

## Ground rules for the implementing agent

1. **Never touch the data plane.** No changes to the request path
   (client → front llama-swap → cell llama-swap → model), to
   `internal/vibe/proxy`, or to anything that could affect SSE
   streaming behavior. If a change seems to need it, stop and flag.
2. **Respect the ownership axes** (design doc §4): availability is
   observed, intent is declared, residency belongs to llama-swap.
   Never store one system's state in another. Never act on inferred
   intent — the `DRAINED?` display state is a question, not a trigger.
3. **Boundary rule.** This repo gets mechanisms with reference-fleet
   example values only. Real addresses, tokens, MAC addresses, plists,
   and compose overrides go to the private fleet repo. If an
   instruction seems to require a house value here, it's wrong.
4. **Inner loop** (AGENTS.md): `go build ./...`, `go vet ./...`,
   `go test -race ./...`, `gofmt -l .`, `go mod tidy`,
   `golangci-lint run` — all green before any push. Proto changes:
   `buf generate`.
5. **Stdlib first.** No new dependencies without written justification
   in the PR. Everything in this plan is achievable with the existing
   set (cobra, yaml.v3, connectrpc, protobuf) + stdlib (`net/http`,
   `crypto/sha256`, `embed`).
6. **Update docs as you land.** Each phase PR updates: the phase doc's
   Status line, the design doc's roadmap state if scope shifted, and
   AGENTS.md if a new package or invariant appears. Future agents read
   the docs, not the conversation.
7. **Acceptance gates are the definition of done.** Each phase doc
   ends with gates; a phase is not complete until every gate passes
   (or a gate is explicitly waived in the PR description with a
   reason). Automated gates become tests in-repo; manual gates get a
   transcript in the PR description.
8. **When the docs and the code disagree, the code wins — then fix
   the doc.** File-level anchors in these docs were verified on
   2026-08-02 against `main` (post-PR #16). Re-verify before relying
   on them; drift is expected, silent drift is not.
