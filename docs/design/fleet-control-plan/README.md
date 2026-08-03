# Fleet-control implementation plan (C0–C7)

Execution plan for [../fleet-control.md](../fleet-control.md). Each
phase is one PR, independently shippable, and pays for itself before
the next starts. Read the design doc first; each phase doc is written
to be implementable on its own after that.

| phase | title | new code | depends on | status |
|---|---|---|---|---|
| [C0](c0-quick-wins.md) | Quick wins: hot reload, autostart, discoverability | ~0 lines | — | merged (#18) |
| [C1](c1-observe-intent.md) | Observe + intent: fleetd, cells registry, `vibe cell status/await`, MCP facade | ~450 lines | C0 optional | merged (#19) |
| [C2](c2-actuate.md) | Actuate: drain/resume RPCs, wake, `render --cell front` | ~450 lines | C1 | merged (#20) |
| [C3](c3-announce.md) | The inversion: announce heartbeats, presence-derived render | ~600 lines | C2 | merged (#21) |
| [C4](c4-comfort.md) | Comfort: warm targets, warm schedules, the fleet page | ~300 lines | C3 (a read-only page could ship after C1; its action buttons need C2, fingerprint badges C3) | merged (#22) |
| [C5](c5-land-c4.md) | Land C4: the adversarial review pass C4 never got | ~400 lines | C4 | merged (#22) |
| [C6](c6-substrate-repair.md) | Substrate repair: the C1–C3 findings against merged code | ~500 lines | independent of C5 | merged (#23) |
| [C7a](c7a-usage-ledger.md) | The usage ledger: tokens per cell, per model, per day | ~710 lines | C4 | executed + reviewed (unit gates green; 7 live gates unrun) |
| [C7b](c7b-savings-screen.md) | The savings screen: what the fleet didn't spend | ~690 lines + ~100 KB data | C7a, C5 | planned |

Line counts are order-of-magnitude scoping signals, not budgets. Actual
C0–C4 spend ran 3.6–4.5× the estimate in every phase; price that in.

C5 and C6 were added on 2026-08-02 after an audit of the C0–C4
implementation run. C5 is not new scope — it is the self-review step
C1, C2 and C3 each got and C4 did not (see ground rule 9). C6 is the
same audit's findings against already-merged code, split out so
landing C4 stays reviewable.

C7a/C7b were added the same day: a "did my hardware pay for itself"
screen. They are split because C7a (counting) is mechanically
verifiable against llama-swap's own totals, while every way the
number can be *silly* lives in C7b (pricing, equivalence, energy,
payback) and none of it can be judged until real counts exist to look
at. C7a needs no new measurement mechanism — llama-swap already logs
per-request token counts to SQLite.

## Ground rules for the implementing agent

1. **The streaming contract is inviolable.** The data plane (client →
   front llama-swap → cell llama-swap → model) may be *observed* —
   C7 accounts for tokens there — but instrumentation must not change
   the bytes a client receives, the flush timing, or the latency of the
   streaming hot path, and a bug in an accounting path must never fail
   a user's request. SSE keepalive is load-bearing: clients kill
   stalled streams (Claude Code at ~5 min). Anything that changes
   streaming *behaviour* rather than merely observing it — buffering,
   coalescing, rewriting, or blocking on a consumer — stop and flag.

   *Amended 2026-08-02.* This rule originally read "Never touch the
   data plane. No changes to … `internal/vibe/proxy`". The repo owner
   relaxed it so token/cost accounting can live where the tokens
   actually are. What survives is the invariant the blanket ban existed
   to protect. A blanket ban is easier to obey and easier to obey
   *uselessly*; state the invariant instead.
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
   Status line, **this README's status column**, the design doc's
   roadmap state if scope shifted, and AGENTS.md if a new package or
   invariant appears. Future agents read the docs, not the
   conversation.
7. **Acceptance gates are the definition of done.** Each phase doc
   ends with gates; a phase is not complete until every gate passes
   (or a gate is explicitly waived in the PR description with a
   reason). Automated gates become tests in-repo; manual gates get a
   transcript in the PR description.
8. **When the docs and the code disagree, the code wins — then fix
   the doc.** File-level anchors in these docs were verified on
   2026-08-02 against `main` (post-PR #16); C5/C6's anchors were
   verified the same day against `3854d84`. Re-verify before relying
   on them; drift is expected, silent drift is not.
9. **Adversarial self-review is a separate, funded step.** Implementing
   a phase and adversarially reviewing your own implementation are two
   line items, not one. C1, C2 and C3 each landed as feature +
   `review: adversarial-review fixes` + `review: second-pass minors`,
   the addenda documenting 10, 10 and 11+ findings fixed pre-merge —
   including blockers. C4 landed as one commit and needed a whole extra
   phase ([C5](c5-land-c4.md)) to reach the same bar. **A phase with
   only a feature commit is not done.** Land the review as its own
   commit and write its addendum into the phase doc.
10. **A gate claim is a claim about a mechanical run.** "Unit tests:
    PASS" means the full inner loop, repeated (`-race -count=5` or
    more) — a single green run is not evidence, and CI's green check is
    not either when the failure is a race. A test's *name* is part of
    its assertion: `TestWarmTarget_SkipsAbsentAndDrainedCells` whose
    body only exercised `Stale` let a missing drained-skip pass a gate
    reported PASS, then propagated the same false claim into three
    other documents. Name tests for what the body proves.
