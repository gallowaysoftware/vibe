# C7b — The savings screen: what the fleet didn't spend

Status: EXECUTED (2026-08-03) on `feat/c7b-savings-screen`. Unit gates
1–8 and 10 are green; gate 9 (live plausibility) needs a real week of
ledger data and is unrun. Depends on [C7a](c7a-usage-ledger.md) for the
ledger and on C4 for the page. Implementation notes, including where the
code deviates from this document, are in the
[addendum](#implementation-addendum-2026-08-03).

C7a counts tokens. C7b prices them and draws the screen: *did my
hardware sort of pay for itself?*

Every way this number can be silly lives in this phase. Two design
choices dominate everything else:

| lever | wrong choice | error |
|---|---|---|
| what you compare against | frontier model instead of the same model hosted | **~72×** |
| how prompt tokens are priced | one rate instead of fresh + cache-read | **~5×** |

They compound. A design that gets both wrong ships a number roughly
**350× too high**, and a screen that reports "paid for itself in 85
days" when the honest answer is "16.7 years" is worse than no screen.

## Design

### 1. Equivalence: the same model, rented

**Default and headline: the same-model twin.** Config declares one twin
id per local model (`qwen3-coder-30b` → `Qwen/Qwen3-Coder-30B-A3B-Instruct`).
The price is the **median** across vendored rows with `open_weights ==
true`, both prices > 0, and `status != deprecated`, carrying `n`/`min`/
`max` so the page renders a **range**, not fake precision.

This removes the equivalence judgement entirely and answers the only
defensible question: *what would this exact workload have cost at a real
host?* It is also the conservative direction, which is what makes the
screen arguable rather than flattering. Footnote on the page that hosted
twins are themselves fp8-quantized — that is the honest defence of
comparing a Q5/Q6 local build against them, rather than pretending
quantization doesn't exist.

**The frontier comparable is an optional, clearly-labelled second row**
("if I'd used claude-opus-5 instead: …") behind a config field that
**requires** a free-text `rationale`, rendered on screen beside the
number. **Ship no default frontier mapping** — a shipped mapping would
be an unearned claim by the repo; a config field with a written
rationale is the owner's claim, which is a claim someone can argue with.

Pair it with a per-model `counterfactual: interactive | batch | free`
multiplier (1.0 / 0.5 / 0.0, default interactive). Anthropic's Batch API
is a flat 50%, and the workload class local fleets are best at —
overnight sweeps, bulk generation — is batch-shaped. One multiplier, one
label, its own line.

An unmapped model renders **"unpriced"**: its tokens stay in the token
column and leave the money column, and the header reads "N% of tokens
priced" whenever coverage < 100%.

### 2. The price table: vendored, dated, normalized

A runtime fetch is impossible (the page must load on a LAN with no
internet) and a config-declared table would make every price an
unsourced house value. **Vendor a pruned, normalized static artifact**
embedded with `//go:embed` next to `fleet.html`. Vendoring a *data* file
is not a dependency under ground rule 5.

- **Primary: models.dev `api.json`** (MIT) — the only source with a
  first-class `open_weights` flag and many hosting providers per
  identical model, which is exactly what the twin median needs.
- **Secondary: LiteLLM `model_prices_and_context_window.json`** (MIT)
  for the two fields models.dev lacks: `mode` (chat/embedding/rerank)
  and `input_cost_per_query` (rerank is priced per search, its token
  price literally 0).
- **Not OpenRouter** — its ToS bars scraping/redistribution. **Not
  Artificial Analysis** — needs a key, redistribution by permission.

Both MIT notices travel with the file, plus each upstream commit SHA and
the fetch date. Normalize at vendor time to per-MTok floats so the
runtime parses one small schema vibe controls. Pruned: ~500 rows,
~100 KB.

**Dated snapshots, not a single table.** Each row carries
`effective_from`; a day bucket is priced at the newest snapshot whose
`effective_from <= that day`. This is one date comparison, and it exists
from day one so the second snapshot isn't a redesign. It is
load-bearing: the Opus tier fell 67% in a single release, and an
always-recompute-at-current-prices rule would retroactively erase money
that genuinely was not spent at the time.

**A price of exactly 0 means UNKNOWN, not free.** Dozens of chat, embed
and rerank rows carry 0. Zero must render "unpriced" and drop out of the
money column while its tokens stay in the token column — otherwise the
embed cell reports $0.00 savings forever and nobody notices.

Refresh: a dev-only `vibe fleet prices vendor` subcommand (stdlib only)
run by a human on an internet-connected box. It fetches both upstreams,
prunes, normalizes, appends a new dated snapshot, **cross-checks
models.dev against LiteLLM and fails loudly on any >2× disagreement**,
and prints a diff summary. CI never has network. The page renders
`as_of` and shows a staleness chip past 90 days, so a stale table is
visible rather than silently wrong.

### 3. Prompt tokens are priced in two buckets, always

```
cost = fresh × input_rate
     + cached × cache_read_rate
     + output × output_rate
```

Take the cached fraction from **measurement** — C7a's `in_cached`, from
llama.cpp's `timings.cache_n` — never from a config constant. Real
corpora run ~92% cached input on local traffic; pricing every prompt
token at full input rate overstates by ~5×.

Where the twin's host publishes no cache discount — and most don't; only
57 of 519 open-weight-host rows carry one — price cached tokens at
**full input rate** and label the row "no cache discount published for
this host".

Render the cached percentage as a first-class field ("92% of your prompt
tokens were cache reads"). It is simultaneously the screen's biggest
error term and its most interesting fact.

**The direction of the trap inverts by endpoint** — that's why C7a
records `basis`. Chat rows: billable input = `in_fresh + in_cached`.
Embed/rerank/mlx rows: billable input = `in_fresh` alone. Getting it
backwards is 1.8× low, or 5× high if overcorrected.

### 4. Energy: in, declared only

Per cell, `watts_idle` and `watts_busy` in hosts.yaml, plus a
fleet-level `electricity_price_per_kwh` (reference example `0.15`):

```
wh = watts_idle × resident_seconds
   + (watts_busy − watts_idle) × busy_seconds
```

`busy_seconds` comes from C7a's `busy_ms`; `resident_seconds` from
announce samples at 15 s resolution. Bill idle/resident separately from
busy — a cell holding a warm model 20 h/day is mostly reporting its idle
constant, and **C4's warm targets deliberately increase exactly that.**

Energy is *not* decorative. Against an honest twin-priced headline it
lands around 11–16% of net savings — a material term. Put the inverted
rule on the page: **if the electricity line looks like 1% of savings,
the comparable is too expensive.**

Declared watts is sufficient because ±40% on a ~12% term is ±5% of the
answer — inside the noise of the equivalence choice (72×) and the cache
tier (5×). Spend the engineering budget there instead. The measurement
ladder is genuinely unattractive: `nvidia-smi` has **no** cumulative
energy field (`total_energy_consumption` is DCGM/NVML, not
`query-gpu`), so a sampler would have to integrate itself and would
still be GPU-only — undercounting wall energy 20–40% at load and 2–3× at
idle. RAPL is root-only post-PLATYPUS. The M3's sudoless options need
cgo into a private IOKit framework.

Ship a per-cell `power: {source: declared}` discriminator with
`nvidia_smi` and `ha_entity` **named as future values and not built** —
Home Assistant's `GET /api/states/<entity>` is the one honest
measurement path and is stdlib-trivial, so it slots into the same field
later. Declared figures render with a `~` prefix so a declared cell
never looks measured.

Display gross and net on adjacent lines — "$21 not spent − $2.50
electricity → $18.50 net" — never gross alone, never net alone.

### 5. Hardware payback

One declared `capital_cost` per cell plus one **required**
`capital_note` free-text string, rendered beside it. No `capital_cost`
→ **no payback bar at all**.

For dual-use hardware the convention is the **upgrade delta** — what the
card cost minus what a gaming-adequate card would have cost:

```yaml
capital_cost: 2100
capital_note: "example: dual-use GPU, upgrade delta over a gaming-adequate card"
```

**Reject usage-weighted attribution by name** so nobody reinvents it:
serving-hours ÷ powered-hours sounds more principled and is strictly
worse — llama-swap holding a model resident means an idle cell
attributes ~90% of the GPU to AI purely for sitting there, so the metric
rewards idling. Gaming hours aren't instrumented anyway.

**Do not amortize.** Amortization requires inventing a useful-life
constant and answers "should I buy one?", a purchasing question. This
screen answers "has it paid for itself yet?", a scoreboard question, and
a scoreboard is a running total against a fixed threshold. That is also
the second argument for daily buckets: a rolled-up total can give the
percentage but never the **date**, and can never be re-priced.

Render: % of capital recovered, the exact date break-even was crossed
(walk the daily series), and a trailing-28-day projection clamped to
">10 years at this rate", with "—" when the trailing rate is zero.

**The payback strip must be allowed to be embarrassing.** On twin-priced
arithmetic a real cell plausibly reads "3% of $3,100 · >10 years at this
rate". That is the screen doing its job. A screen that can only render
triumph will render triumph regardless of the truth — which is why the
unflattering states get designed first.

`capital_cost` is the most personal value in the fleet config: the
public repo ships obviously-synthetic examples and real numbers live in
the private fleet repo (ground rule 3).

### 6. Actual cloud spend, beside it, at the same size

fleetd tails the **front's** activity log for `cloud_peer` model ids
only and prices them at the real model's real rate — a bill
reconstruction, not a counterfactual. It gets its own line under the
hero at the same type size.

This is the induced-demand control. Notional savings are a
counterfactual that never happened; actual spend is a fact. **If both
rose together, the fleet added work rather than replacing it** — and the
reader can see that without the page having to model willingness to pay.

### 7. The screen

A second **view** inside the same embedded `fleet.html` at the same
single route, selected by **hash routing** (`#savings`).

Hash routing is not a style preference. `/ui/fleet/savings` would need
the bearer middleware's exact-match exemption (`daemon/auth.go:139-142`)
widened to a prefix, which [C5](c5-land-c4.md) §7 forbids in as many
words. A fragment never reaches the server, so `r.URL.Path` stays
`/ui/fleet`, deep links work in a bare tab, and C5's claim that the page
adds exactly one route survives literally.

Data from **one new read-only** `GET /api/fleet/savings?window=7d|30d|all`,
backed by an exported `Savings(ctx, window)` that fleetmcp also serves as
a `fleet_savings` tool. **Not** hung off `/api/fleet/state`: every SSE
line triggers a debounced full state re-GET (`fleet.html:203-206`), so a
ledger aggregate would recompute on every fleet event for a screen
nobody is looking at; `StateSnapshot` goes verbatim to agents through
`fleet_status`, so every agent turn would carry a money table it didn't
ask for; and `Snapshot` runs under a 3 s probe budget a slow ledger read
would eat.

**State the rule restatement verbatim in the PR** or a reviewer reads
this as a C4 violation: the *page* still adds zero routes; *fleetapi*
adds one read-only GET (its fourth, after state/events/leases); every
mutation stays on `POST /mcp`. The savings view has **no action
buttons**, which is what makes that trivially true.

Mechanism is ~15 lines of JS: a `<nav>` between `.meta` and
`#token-gate`, the existing `<main>` children wrapped verbatim in
`<section id="view-fleet">`, a new `<section id="view-savings" hidden>`,
and a `showView()` dispatcher on `hashchange` plus at the end of
`boot()`. Savings loads lazily on first entry and at most once per 60 s,
deliberately **not** wired to the state refresh. The live table keeps
rendering while hidden, so switching back never stalls.

Bar widths come from `el.style.width` in JS, **never** an interpolated
`style="…"` attribute, so C5's PAGE-1 test passes.

```
 FLEET SAVINGS                                    window: 30d ▾   as_of 2026-07-28

   $18.83 not spent          − $2.51 power        = $16.32 net
   actual cloud spend, same window: $41.07        92% of prompt tokens were cache reads

 ┌ cell ────────── priced as ─────────────── in (fresh/cached) ── out ── cloud-eq ── power ── net ─┐
 │ gpu-cell        Qwen3-Coder-30B @ host     0.66M / 12.54M    0.18M    $14.90    $2.09   $12.81 │
 │   ↳ qwen3.6-27b (78%)  median of 6 hosts, $0.07–$0.30 /MTok
 │   ↳ bge-m3 (22%)       unpriced — no published rate                                            │
 │ laptop          Qwen3-8B @ host            0.09M /  0.41M    0.02M     $3.93    $0.42    $3.51 │
 │   ↳ partial — 41 of 116 requests reported tokens (mlx, streaming)                              │
 │ utility         —                          —                  —      unmeasured    —        — │
 ├─────────────────────────────────────────────────────────────────────────────────────────────────┤
 │ FLEET · 2 of 3 cells measured · 78% of tokens priced         $18.83    $2.51   $16.32 │
 └─────────────────────────────────────────────────────────────────────────────────────────────────┘

 PAYBACK (lifetime)
   gpu-cell   ████░░░░░░░░░░░░░░░░░░░░░░░░  3% of $3,100    >10 years at this rate
              dual-use GPU, upgrade delta over a gaming-adequate card
   laptop     ██████████░░░░░░░░░░░░░░░░░░ 34% of $800      break-even ~2027-04
```

### 8. Empty and unflattering states — design these first

- No `capital_cost` → no payback bar. Not 0%, not ∞%, not an invented
  denominator.
- Model absent from the price table → tokens still show; header reads
  "N% of tokens priced".
- No wattage → POWER is an em dash, NET is labelled "net (power not
  counted)".
- Empty ledger → the fresh-install panel, **not** a `$0.00` hero.
- < 14 covered days → "too early to project".
- Every money and token field is a pointer or paired with a `measured
  bool`, following the invariant `InFlight` already encodes
  (`fleetapi.go:386-398`). An unmeasured cell renders an em dash plus a
  reason chip and is **excluded from the total**. `$0` renders only for
  a genuine measured zero.

### 9. The caveat the page displays

Rendered under the hero, collapsed by default, in the house's dry voice:

> This is an estimate of money **not spent**, and it is an upper bound.
> It counts only the tokens the fleet actually measured — anything
> served while a cell was unmeasured, restarting, cancelled mid-stream,
> or bypassing the front is missing, so the token totals are a floor.
> Those tokens are priced against the **same open-weight model rented
> from a real host**, at the rates in effect on the day the work ran,
> because pricing a 27B local model as a frontier model moves this
> number about seventy-fold and tells you nothing. Prompt tokens are
> split into fresh and cache-read and priced separately, because most of
> an agentic prompt is a re-sent prefix that a host bills at a fraction
> of the rate. The figure assumes you would have paid for every one of
> these tokens, which you would not have — local models get used
> casually precisely because the marginal token is free — which is why
> actual cloud spend sits beside it at the same size. Electricity is
> subtracted from declared wattage, not measured, and is good to roughly
> ±30%. Payback counts the capital number in hosts.yaml and nothing
> else — not the hours spent building and running the fleet, which very
> likely exceed everything shown here.

## Acceptance gates

1. **Golden pricing gate (unit).** A **frozen** 3-row fixture table —
   one chat row with 4× in/out asymmetry and a `cache_read`, one
   embedding row, one per-query rerank row — plus fixed counts asserts
   exact dollar output **to the cent**. A vendored-table refresh cannot
   silently change the arithmetic without failing this. Second assertion
   in the same test: a row priced 0 renders "unpriced", never
   "$0.00 saved".
2. **Cache-tier gate (unit).** The same token counts priced with and
   without the fresh/cached split differ by the expected ~5×, and a
   chat row and an embed row with identical raw counts price
   **differently** because `basis` differs.
3. **Equivalence gate.** Twin-priced and frontier-priced headlines for
   the same window differ by the expected order of magnitude, the
   frontier row refuses to render without a `rationale`, and no default
   frontier mapping ships (grep the config examples).
4. **Dated-price gate (unit).** A two-snapshot table prices a day before
   and after `effective_from` at the correct respective rates — a price
   cut does not retroactively rewrite history.
5. **Re-price gate.** Bump the vendored snapshot; the whole history
   re-prices from the same raw ledger with no re-collection.
6. **Empty and unflattering states gate.** Every case in §8 renders as
   specified. Explicitly assert the ">10 years at this rate" clamp and
   the missing-`capital_cost` no-bar case.
7. **Page invariant gate.** The page still registers exactly **one**
   route; the auth exemption still matches `/ui/fleet` by exact match
   with no prefix; `#savings` deep-links in a bare tab and prompts for
   the token; every figure comes from the new authed GET; a Go test over
   the embedded file asserts no `esc(` inside a double-quoted attribute;
   no external asset, CDN or build step is introduced (grep for
   `http://`, `https://`, `<script src`, `<link`).
8. **Timer-hygiene gate.** The savings poll is cleared in the same
   clear-then-assign block as C5's `pollTimer` / `streamAbort`, so
   re-entering the token does not stack a fourth timer.
9. **Live plausibility gate.** With a real week of ledger data, a human
   reads the screen and the number is *defensible* — the twin is a real
   host's real price, the cached fraction matches the cells' own
   `/api/metrics/stats`, and the power line is not 1% of savings. This
   gate is a judgement call and is allowed to fail the phase.
10. **Full inner loop** (ground rule 4) — including `go mod tidy`
    asserting **no new module requirement**, since the vendored price
    JSON is a data file, not a dependency — plus ground rule 9's
    adversarial self-review pass as its own commit.

## Out of scope

Modelling willingness-to-pay or induced demand (show actual spend beside
notional savings and let the reader discount). Pricing operator time —
no hourly rate; one footer sentence instead, because an invented rate
would be the most manipulable number on the page. Any power sampler.
Throughput, latency, occupancy or live-residency panels — llama-swap's
`/ui` and Prometheus own those and stay out. *(The boundary test: if a
panel answers "how fast / how loaded / what's resident right now", it is
not C7. C7 adds one derived, retained, money-shaped rollup that neither
can produce, because neither knows a price, neither aggregates across
cells, and both forget on restart. This is how C7 complements
c4-comfort.md's "metrics dashboards are out of scope" rather than
contradicting it — say so in the PR.)* Any mutation surface. Per-user,
per-project or per-harness attribution. The word "saved".

Estimated ~690 lines + tests + a ~100 KB vendored data file.

## Implementation addendum (2026-08-03)

What shipped, and where the code diverged from the plan above. Ground
rule 8: the code wins, so this section is the truth.

### Where it lives

| piece | file |
|---|---|
| price table + arithmetic | `internal/vibe/prices/prices.go` |
| vendoring tool | `internal/vibe/prices/vendor.go`, `vibe fleet prices vendor` (`internal/vibe/cli/cmd_fleet.go`) |
| vendored artifact | `internal/vibe/prices/prices.json` (4,853 rows, 480 KB) |
| pricing / power / capital config | `internal/vibe/fleetcfg/fleetcfg.go` (`pricing:`, `power:`, `capital_cost`) |
| the engine | `internal/vibe/fleetapi/savings.go` (`Savings(ctx, window)`, `GET /api/fleet/savings`) |
| cloud spend | `internal/vibe/daemon/cloudspend.go` + `fleetapi.StartCloudSpendLoop` |
| the view | `internal/vibe/fleetapi/fleet.html` (`#savings`) |
| agent surface | `fleetmcp` tool `fleet_savings` |

The table is embedded in its own package rather than "next to
fleet.html" as §2 says. The vendoring tool and the runtime must share
one schema, the tool lives in the CLI, and `fleetapi` importing the CLI
is not a thing — so the shared artifact went to the package both of them
import. Nothing else about §2 changed.

### The vendored table is REAL, not the example

The box that ran this phase had network, so `vibe fleet prices vendor`
fetched both upstreams for real:

- models.dev `api.json` @ `774d80647eb03527c0a3cbdb3f10b0395cdae9c4` (MIT)
- LiteLLM `model_prices_and_context_window.json` @
  `bf1a8fe40329eb018ef420057766ce95a43baaa3` (MIT)

Both licences and both commits ride in the artifact, per snapshot. 336
OpenRouter rows were excluded at vendor time (its terms bar
redistribution, and a row sourced from models.dev's mirror of it is the
same content). 31 cross-source disagreements past 2× were reviewed and
their rows DROPPED — mostly LiteLLM entries carrying per-MTok values in
a per-token field (wandb), which is exactly the units error the check
exists for. The artifact carries the dropped conflicts so the drop is
auditable.

Deviations from §2 worth naming:

- **The prune is "keep every priced row", not ~500 rows.** Pruning to
  500 would have meant choosing which hosts count, which is the exact
  judgement the median exists to avoid. The cost is 480 KB of embedded
  data instead of ~100 KB.
- **Snapshots after the first are OVERLAYS** (changed/new rows plus a
  removed list) rather than full tables, so a multi-year price history
  stays inside one embedded file. `Table.At(day)` applies the base plus
  every overlay up to that day.
- **A disagreement DROPS the row** as well as failing the run.
  `--max-disagreements <n>` is how a human who has read the list
  proceeds; the dropped rows never silently resolve to one source's
  number.
- **A day older than the base snapshot prices at the base** and is
  flagged `BeforeBase`. Refusing to price it would report a fleet that
  served traffic as having saved zero, which is a stronger claim than
  "we don't know what the rates were that far back".

### Qualification is mode-aware

§1 says the twin median takes rows with "both prices > 0". That is right
for chat and wrong for the other two modes: an embedding row has no
output price and a rerank row's token price is literally 0 because it
bills per query. So the filter is per mode — chat needs input and output,
embedding needs input, rerank needs a per-query rate. A price of exactly
0 still means UNKNOWN everywhere.

Related: a workload's `basis` and a row's `mode` must agree. Chat counts
priced against an embedding row (or the reverse) is a config error, and
it renders "unpriced" rather than inventing a number.

### C7a additions this phase needed

Two, both additive:

- **A cell-level residency row** (`model: ""`, basis `resident`).
  Per-model rows cannot be the energy denominator: summing them bills a
  multi-resident box's idle watts once per model, and taking the max
  under-counts a cell that alternated models through the day. The
  max-across-models fallback still covers ledgers written before this
  phase.
- **A `cloud` basis**, reserved to fleetd like `resident` and `cell`,
  keyed on the FRONT cell. It is the one place the front legitimately
  appears in the ledger: cloud models are served by no cell, so there is
  nothing to double-count. `FoldCloudUsage` merges a model's bases before
  folding, because two bases for one cloud model would otherwise resolve
  against the same cursor twice in one pass.

`busy_seconds` is capped at the day's residency: concurrent requests each
report their own wall time, and a cell cannot be busy longer than it was
on.

### Gate status

| gate | state |
|---|---|
| 1 golden pricing (exact to the cent, 0 = unpriced) | PASS — `prices_test.go:TestGoldenPricing_ExactToTheCent`, `TestZeroRateIsUnpricedNotFree` |
| 2 cache tier (~5×, basis changes the bill) | PASS — `TestCacheTier_SplitVsSingleRate`, `TestBasisChangesTheBill` |
| 3 equivalence (twin vs frontier, rationale required, no default mapping) | PASS — `c7b_test.go:TestSavings_FrontierComparableIsOrdersOfMagnitudeAboveTheTwin`, `fleetcfg_test.go:TestLoad_FrontierRequiresARationale`, `TestNoDefaultFrontierMappingInConfigOrShippedExamples` |
| 4 dated prices | PASS — `TestDatedSnapshots_PriceTheDayNotToday`, `TestSavings_DatedPricesDoNotRewriteHistory` |
| 5 re-price from the same ledger | PASS — `TestSavings_RepricesFromTheSameLedger` |
| 6 empty and unflattering states | PASS — six tests, including the `>10 years at this rate` clamp and the missing-`capital_cost` no-bar case |
| 7 page invariants (one route, exact-match exemption, no external asset) | PASS — `TestFleetPage_SavingsIsAViewNotARoute`, `TestFleetPage_SavingsViewInvariants`, `daemon/fleet_registry_test.go` |
| 8 timer hygiene | PASS — `TestFleetPage_SavingsTimerIsClearedInBoot` |
| 9 live plausibility | NOT RUN — needs a real week of ledger data on real cells |
| 10 full inner loop, no new module | PASS — build, vet, gofmt, `go mod tidy` (clean), `golangci-lint` (0 issues), `go test -race -count=5 ./...` |

### The rule restatement, verbatim (§7 asks for it in the PR)

The *page* still adds zero routes: the savings screen is a hash-routed
view inside the same embedded `fleet.html` at the same `GET /ui/fleet`.
*fleetapi* adds one read-only GET — `/api/fleet/savings`, its fourth
after state/events/leases. Every mutation stays on `POST /mcp`. The
savings view has no action buttons, which is what makes that trivially
true.

And the C4 boundary restated: if a panel answers "how fast / how loaded /
what's resident right now", it is not C7 and llama-swap's `/ui` and
Prometheus keep it. C7 adds one derived, retained, money-shaped rollup
that neither can produce, because neither knows a price, neither
aggregates across cells, and both forget on restart.

### Adversarial review addendum (ground rule 9, 2026-08-03)

Eight findings against the feature commit, all fixed in the review
commit. Three were honesty defects — the class of bug this phase exists
to prevent — and one was found by executing the page rather than reading
it.

1. **A measured-but-unpriced cell reported `$0.00`.** A cell whose every
   model was unpriced still got `Gross = money(0)`, so §8's "$0 renders
   only for a genuine measured zero" was violated in the flattering
   direction on the most likely first-run configuration (no `pricing:`
   block at all). Money is now absent with a reason, the tokens stay in
   every token column, and the fleet total stays `nil` rather than
   summing to zero. Pinned by
   `TestSavings_MeasuredButUnpricedCellIsNotAZero`.
2. **Cloud spend that was measured but unpriceable read "not
   measured".** The requests happened and the bill exists; what was
   missing was a rate. It now says so and names the models, with the
   `priced_as` fix in the message. Pinned by
   `TestSavings_MeasuredButUnpricedCloudSaysSo`.
3. **§4's inverted rule was missing from the page.** "If the electricity
   line looks like 1% of savings, the comparable is too expensive" is a
   design requirement, not a footnote — it is the reader's cross-check on
   the equivalence choice. Now a note whenever power is under 3% of
   gross. Pinned by `TestSavings_ElectricityThatLooksFreeIsFlagged`.
4. **`Savings(ctx, …)` ignored its context.** A whole-history walk on an
   HTTP request now aborts with an ERROR rather than returning a short
   report: a document missing three days is indistinguishable from a
   fleet that served nothing those days.
5. **A `#savings` deep link landed on the fleet view when the first
   state fetch failed** — `showView()` ran after `await refresh()`. One
   unreachable cell would have sent the reader to the wrong screen. The
   view is now picked before the first await, and a token-less tab
   resolves its view before the gate.
6. **The page had never been executed.** A DOM stub run of
   `renderSavings()` against a real report caught an `innerHTML` →
   `querySelector` round trip and confirmed the unflattering path renders
   ("<1% of $3,100 · >10 years at this rate"), the empty panel swaps in,
   and the payback fill width lands on the element. Two presentation
   bugs fell out: a sub-1% recovery rendered as a flat "0%" (now "<1%",
   because a screen whose job is believability at small numbers must not
   look broken there), and capital rendered as "$3100.00" instead of
   "$3,100".
7. **Actual cloud spend rendered smaller than the notional headline.**
   §6 says "at the same size" for a reason: shrinking the fact makes the
   counterfactual look bigger than it is.
8. **Two test names overclaimed** (ground rule 10).
   `TestSavings_FrontierIsTheSeventyFoldError` asserted an order of
   magnitude, not 72×, and `TestNoDefaultFrontierMappingShips` checks the
   zero config plus the shipped example docs, not the whole repo. Both
   renamed to what their bodies prove.

Not fixed, recorded instead:

- A same-day re-vendor appends a second snapshot with the same
  `effective_from` rather than merging into it. Resolution is
  last-wins, so the prices are right; the artifact is just untidier than
  it needs to be.
- Power is counted only for days that carry a residency sample, so a
  cell that served traffic with no residency rows shows an em dash
  rather than an estimate. That is the honest reading, but it errs
  toward a LARGER net — the one place in this screen that does.
