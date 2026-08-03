# C7a — The usage ledger: counting tokens per cell, per model, per day

Status: EXECUTED (2026-08-03) on `feat/c7a-usage-ledger`, branched off
the C4/C5 line. Every mechanically verifiable gate is green; the seven
live gates need real cells and are listed unrun in
[§Execution](#execution-2026-08-03). Depends on C4 (announce +
presence). No dependency on C5/C6 beyond merge order.

C7a produces a durable, per-(cell, model, day) token ledger with **zero
price knowledge and zero UI**. [C7b](c7b-savings-screen.md) turns it
into money and a screen. The seam is deliberate: every way the savings
number can be silly lives in C7b, and none of it can be judged until
C7a's numbers exist to look at.

## The finding that shapes this phase

**llama-swap v239 already does the counting.** A metrics middleware on
every model-dispatched POST parses llama.cpp's `timings` block and
writes one row per request to SQLite, keyed by model id — wired
unconditionally, no flag, no config. It is exposed as:

- `GET /api/metrics/activity` → `ActivityPage{data,page,limit,total,total_pages}`
- `GET /api/metrics/stats?model=` → `{total_requests,total_input_tokens,total_output_tokens,total_cache_tokens,…}`

Row shape: `ActivityLogEntry{id, timestamp, model, req_path,
resp_status_code, tokens{cache_tokens, draft_tokens, draft_acc_tokens,
input_tokens, output_tokens, prompt_per_second, tokens_per_second},
duration_ms, error_msg, metadata}`
(`llama-swap@v239 internal/store/store.go:22-45`, routes at
`internal/server/server.go:261-270`, middleware
`internal/server/metrics_middleware.go:17-82`.)

So fleetd needs no new measurement mechanism — only collection,
attribution, and retention. **Recording happens at request completion**,
which is why this beats a counter scrape: nothing is lost when
llama-swap swaps a model out mid-burst.

### Why not the alternatives

- **Not `internal/vibe/proxy`.** Ground rule 1 was relaxed to permit
  instrumenting it. That permission is deliberately **left unspent**:
  AGENTS.md's "Router / model lifecycle" section records that `:9000 is
  llama-swap, not vibe` and cells run `disable_proxy: true`, so vibe's
  proxy is not in the fleet request path. A flawless tee would measure ~0% of fleet tokens. The
  strongest form of the surviving invariant is a gate:
  `git diff --stat internal/vibe/proxy/` is **empty** for this phase.
- **Not llama-server `/metrics`.** Its `server_metrics` struct has no
  cache counter — `prompt_tokens_total` is cache-miss-only with no
  `cache_n` companion — so it structurally cannot reconstruct billable
  input. It also needs `--metrics` in every cell's argv, which churns
  `flags_sha256` and excludes every strict-fingerprint def from the
  front render until all cells re-announce (C3 §5).
- **Not the OpenAI `usage` object.** Since llama.cpp commit `2b6b55a`
  (PR #16052, 2025-09-18) streaming responses emit `usage` **only** when
  the request carries `stream_options:{"include_usage":true}`. Real
  clients don't send it, so a usage-based design reports near-zero for
  exactly the traffic that matters. `timings` has no such gate — it is
  pushed on every response, streaming or not
  (`tools/server/server-task.cpp`, `to_json_oaicompat_chat_stream()`).
- **Not injecting `stream_options`.** It mutates the client's request
  and adds a client-visible empty-`choices` chunk that real clients
  mishandle. **`stream_options` must appear nowhere in this phase's
  diff** — that is a gate.
- **Not harness session files** (`~/.omp`, `~/.ccs`, `crush.db`,
  `webui.db`). They have no hardware attribution (omp records provider
  `fleet`), live on the client box rather than the execution hardware,
  and interleave token counts with the user's prompt content. A
  content-free cross-check is named as possible future work, not built.

## Design

### 0. Config first: turn on llama-swap's persistent store

**This is C7a's first commit and it lands alone.** llama-swap's default
activity store is an in-memory ring that `PruneActivity` caps at ~1000
rows and that evaporates on restart. Set `store: {path: …}` on every
cell **and** the front through the existing `--extras` verbatim
top-level merge (`router/render.go:33-41` — zero renderer code), then
soak it for 24 h before any accounting code exists.

This kills two failure modes in one config change: the 1000-row ring,
and activity ids restarting at 1 on every llama-swap restart.

### 1. Cell-side collection (`internal/vibe/usagemeter`)

The cell's own vibe daemon tails its local llama-swap over localhost
and maintains **cumulative** per-model counters in a state file.

**Why cell-side rather than a fleetd pull:** C3 was the inversion —
cells announce, the catalog is derived. A fleetd pull re-inverts it.
More concretely, **only the cell can resolve aliases**: the front's
rendered config is peers-only so llama-swap's `RealModelName` returns
false there and the front records whatever model string the client
typed, while the cell's config has `models:` populated and resolves
`qwen3.6-27b-tools` → `qwen3.6-27b`. Pricing must key on the canonical
def name, never on a string a client chose.

Tail with `?sort=id&order=desc&limit=999`, walking back to the stored
`last_row_id`. Persist `last_row_id` plus a store `epoch` minted when
the counter file is created.

**The id-reset rule.** If `max_id < last_row_id`, llama-swap restarted
on an in-memory store (SQLite AUTOINCREMENT restarts at 1): ingest
everything present, mint a **new epoch**, log it. Without this rule a
cell whose cursor sits at 47,000 reads zero for seven months and looks
exactly like an idle cell.

If a cell serves more than 999 requests between two announces on an
in-memory store, rows are genuinely lost. Detect the shortfall
(`expected = max_id − cursor` vs rows received) and report `lost_rows`
rather than absorbing it silently.

### 2. Token semantics — branch on `req_path`, not on backend kind

This is the subtlest thing in the phase and getting it backwards is a
1.8×–5× error.

llama.cpp emits `timings` on chat / completions / messages / infill but
**not** on `/v1/embeddings` or `/v1/rerank`. So the *same cell* yields
different `input_tokens` semantics per endpoint:

| basis | source | `input_tokens` means | rule |
|---|---|---|---|
| chat-family | `timings.prompt_n` | **cache-miss only** | `fresh = input_tokens`, `cached = max(cache_tokens, 0)` |
| embed / rerank | OpenAI `usage` | **full prompt, cached included** | `fresh = input_tokens`, `cached = 0` |
| any mlx row | OpenAI `usage` | full prompt | `fresh = input_tokens`, `cached = 0` |

Always clamp with `max(cache_tokens, 0)`: `-1` is llama-swap's
not-reported sentinel, and summing it subtracts a phantom token per
embed row.

llama-swap stores no field saying which parser won, so **record the
chosen branch as a `basis` string in the ledger.** C7b's pricing reads
it; without it the cache arithmetic is unreconstructable after the fact.

### 3. Classification — three corrections, all visible

Applied at the cell, all rendered later as visible lines rather than
silently absorbed:

- **Self-pokes.** C4's warm targets, warm schedules and `warm_model` all
  POST a real 1-token completion through the front, landing in the
  activity log as genuine metered requests. llama-swap's `Metadata` is
  populated only by internal handlers, so there is no header to tag
  them with. Rule: chat-family rows with `output_tokens <= 1` are
  `poke_req` — excluded from every billable sum. A 15 s warm loop
  across three cells can issue **more** requests than the human does:
  measured, ~5% of the money but up to 96% of the request count, which
  destroys every per-request average on the page. The only false
  positives are genuine one-token generations worth ~$0.
- **Unmeasured.** A row with `status == 200` and zero tokens is
  `unmeasured_req` — counted, **never summed as zero**. This is all mlx
  streaming traffic and every client-cancelled stream (`timings` rides
  only the final chunk, so a Ctrl-C yields a 200 with no parseable
  tokens). Do **not** estimate them from `duration_ms ×
  tokens_per_second` — that field is also 0 on those rows, so it would
  mean borrowing another row's rate, which manufactures numbers rather
  than measuring them. The loss is structural and concentrated on the
  longest, most valuable generations; label it, don't launder it.
- **Errors.** Only `status == 200` rows enter the requests figure;
  non-200s become `err_req`. They already contribute zero correctly
  (llama-swap's early branch never populates tokens) — state that so
  nobody "fixes" it.

### 4. Transport: piggyback the announce

Add an additive `AnnounceRequest.Usage *AnnounceUsage` field carrying
**cumulative** per-model totals plus `epoch`. C3's schema tolerates
unknown fields, so old fleetd ignores it and old cells simply omit it
(and render as unmeasured).

**Do not overload `AnnounceModel.Probe`** — it is explicitly reserved
for the v2 throughput block (C3 §1).

Cumulative totals make offline cells free: a missed heartbeat loses
nothing, the next successful announce carries the arrears, and the
delta rule is idempotent under retries, duplicate announces, and a
roaming Mac that served traffic off-LAN for a day. Cost is ~150 B per
model per 15 s — well under the ingest limit, but it makes the
heartbeat non-trivial; note it.

### 5. fleetd-side folding

Per (cell, model, epoch):

- same epoch, `total >= last` → add `total − last`
- same epoch, `total < last` → treat as reset, add `total`, log
- new epoch → start a new row, keep the old accumulated

**Never sum the front with the cells.** No-double-count is enforced as
a **whitelist, not a filter**: the ledger accepts only announce-carried
totals, and only cells announce; `fleetcfg.FrontCell` is skipped
structurally at fold time, with a named comment and a test. The front's
own rows are read separately (C7b) for cloud_peer ids only.

### 6. Storage

Append-only JSONL at `$XDG_STATE_HOME/vibe/fleet/usage.jsonl`, one line
per (day, cell, model, basis), holding **raw counts only — never
dollars**. Coalesce in memory, flush on a 60 s ticker and on shutdown;
on load, replay with last-line-wins per key; compact at daemon start
using the repo's tmp-file + rename discipline.

Deliberately **not** `history.go`'s rewrite-the-whole-file-on-every-
record: correct there because starts are minutes apart, wrong here
because fleetd folds an announce from every cell every 15 s.

```json
{"d":"2026-08-02","tz":"America/Toronto","cell":"gpu-cell",
 "model":"qwen3.6-27b","basis":"chat","epoch":"a3f1",
 "last_total":{...},"req":220,"in_fresh":660000,"in_cached":12540000,
 "out":176000,"poke_req":288,"err_req":3,"unmeasured_req":26,
 "busy_ms":4200000,"resident_s":54000}
```

~250 B/line; 5 cells × 8 models × 366 days ≈ 3 MB/year. Roll buckets
older than two years into monthly ones.

`last_total` lives in the **same line** as the bucket counts, so a crash
cannot leave the cursor and the counter disagreeing.

Storing raw counts is what lets C7b re-price the whole history when the
price table updates.

**Day bucketing uses an explicit `*time.Location`** from a new
fleet-level `timezone` config field, following the precedent
`cronSpec.nextFire` already sets (`warmsched.go:144-157`):
`y, m, d := ts.In(loc).Date()`. **Never `Truncate(24*time.Hour)`** — it
truncates against absolute time since the epoch and always lands on UTC
midnight regardless of the Location the value carries, silently. fleetd
runs containerized and defaults to `TZ=UTC`, so an evening session would
otherwise split 40/60 across two bars.

### 7. Exposure

C7a ships no UI. It exposes the raw ledger through a `fleet_savings`-
shaped MCP tool returning **tokens only, no dollars**, so the phase is
usable from an agent the day it lands. (A phase with no visible payoff
is the classic phase that never gets finished.)

## Acceptance gates

1. **Store gate (config-only, lands and soaks first).** After a 24 h
   soak, `GET /api/metrics/activity?limit=1&sort=id&order=desc` on each
   cell shows `total` > 1000 (proving `PruneActivity` never ran) and the
   row count survives a deliberate llama-swap restart. Transcript in
   the PR.
2. **No-double-count gate (live).** Drive one identifiable generation
   client → front → a known cell. Exactly one increment lands,
   attributed to that cell; the fleet total equals one copy, not two.
   Repeat with an **alias** id (`qwen3.6-27b-tools`) and assert it lands
   on the canonical def name, once. A unit test renders a front config
   and a cell config and asserts the front is skipped **structurally,
   by name**, not by filter.
3. **Cache-semantics gate (live).** Send the same ~20k-token prompt
   twice to a chat model: run 2's delta shows `in_cached` ≈ run 1's
   `in_fresh`, and `in_fresh` ≈ 0. Then an embedding request asserts
   `basis == "embed"`, `in_cached == 0`, and that no `-1` sentinel
   entered any sum.
4. **Unmeasured gate (live).** (a) Stream to the mlx cell with no
   `stream_options`; (b) Ctrl-C a generation mid-stream on a llama.cpp
   cell; (c) force a 503. Assert (a) and (b) land in `unmeasured_req`,
   (c) in `err_req`, none adds tokens, **none is summed as zero**, and
   the cell reports "N of M requests reported tokens".
5. **Self-traffic gate (live).** With a warm target active on a model
   whose TTL evicts it, run for one hour. Every warm poke lands in
   `poke_req` and contributes zero to money and to the requests figure.
   Cross-check the cell's `poke_req` against fleetd's own count of warms
   issued; >10% disagreement fails.
6. **Offline gate (live).** Power off a cell for an hour after it served
   traffic (roaming shape: serve off-LAN if the Mac is available). On
   return, cumulative totals reconcile with no loss and no double count
   against the cell's own `/api/metrics/stats`.
7. **Restart-idempotency gate (live).** Kill fleetd mid-window and
   restart. The ledger neither loses nor replays a bucket. Repeat with a
   kill between fold and flush.
8. **Epoch gate.** Restart a cell's llama-swap with an in-memory store
   so ids restart at 1: the cell detects `max_id < cursor`, mints a new
   epoch, and fleetd starts a new row rather than flatlining that cell
   to zero. Unit test covering same-epoch monotonic, same-epoch lower
   total, and new epoch.
9. **Timezone gate (unit).** A session spanning local midnight in
   `America/Toronto` lands in the correct two buckets with the correct
   split; spring-forward (23 h) and fall-back (25 h) days bucket
   correctly. A grep test asserts `Truncate(24 * time.Hour)` appears
   nowhere in the phase.
10. **MTP gate (unit).** A synthetic row with `draft_tokens` /
    `draft_acc_tokens` populated accumulates **identically** to a
    non-speculative row with the same `predicted_n` — nobody ever adds a
    draft column to a billable figure, and a `-1` draft sentinel does
    not enter a sum.
11. **Streaming-contract gate.** `git diff --stat internal/vibe/proxy/`
    is empty for the entire phase, and a grep asserts `stream_options`
    appears nowhere in the diff.
12. **Full inner loop** (ground rule 4), plus ground rule 9's
    adversarial self-review pass as its own commit.

## Out of scope

Money, prices, equivalence, energy, payback and the screen — all
[C7b](c7b-savings-screen.md). Throughput/latency/occupancy panels
(llama-swap's `/ui` and Prometheus own those). Per-user, per-project or
per-harness attribution.

Estimated ~710 lines + tests. Note the plan's own calibration: C0–C4 ran
3.6–4.5× their line estimates.

## Execution (2026-08-03)

Branched off `feat/c4-fleet-comfort` (C4 + C5 landed).

### What shipped

| piece | where |
|---|---|
| cell-side collector | `internal/vibe/usagemeter/usagemeter.go` (new package) |
| announce wire field | `fleetapi/announce.go` — `AnnounceRequest.Usage *AnnounceUsage` |
| cell-side hook | `fleetannounce.Config.Usage func(context.Context) *AnnounceUsage`, wired from `daemon/announce.go` and `vibe fleet announce` via `usagemeter.Snapshotter` |
| fleetd ledger | `fleetapi/usage.go` — fold, day bucketing, JSONL flush/compact |
| config | `fleet.timezone` (`daemon.FleetConfig`), `Config.FleetLocation()` |
| paths | `paths.CellUsageFile()`, `paths.UsageLedgerFile()` |
| exposure | `GET /api/fleet/usage?since=YYYY-MM-DD` + MCP `fleet_usage` |

Two deliberate deviations from the doc, both narrower than what the
text implied:

- **Basis is a three-value vocabulary, not two.** `chat` (llama.cpp
  `timings`), `embed` (OpenAI `usage` on embeddings/rerank), and
  `other` for the remaining model-dispatched endpoints llama-swap
  meters (audio, images, `/props`, `count_tokens`). `other` uses the
  embed arithmetic — whatever prompt figure arrives is taken as
  complete. The doc's third table row ("any mlx row") needs **no branch
  at all**: mlx answers chat paths from `usage`, so `input_tokens` is
  already the full prompt AND no cache figure is reported, so
  `max(cache, 0)` is 0 and `fresh + cached` degenerates to exactly the
  right answer. Branching on backend kind would have been both wrong
  and unnecessary.
- **`resident_s` and `lost_rows` are their own basis values**
  (`resident`, `cell`) rather than columns on a token row. §6's example
  line shows `resident_s` beside the token counts, but residency is
  fleetd-observed and basis-independent, and loss is a property of the
  cell's READ, not of any model. Separate basis values keep the
  `(day, cell, model, basis, epoch)` keyspace and the replay rule
  uniform while making it impossible for either number to land in a
  token sum.

Two things the doc did not spell out and the code had to decide:

- **The cursor is keyed WITHOUT the day.** `last_total` lives on the
  bucket line as §6 requires, but the in-memory cursor is
  `(cell, model, basis, epoch)`. Keyed by day, the first fold after
  local midnight would read `last_total` as zero and bill the cell's
  entire lifetime again — a silent 100× on the first bar of every day.
  Test: `TestUsageFold_DayRolloverBillsTheDeltaNotTheLifetime`.
- **The id-reset check runs BEFORE the walk-back, not after.** Resetting
  the cursor after reading rows would leave the shortfall arithmetic
  comparing rows-read (measured against the old cursor) with a span
  measured from the new one, reporting a phantom `lost_rows` of every id
  on the box.

`fleet.timezone` also now feeds C4's warm schedule (`StartScheduleLoop`
previously got `nil` → process zone). C4 §2's rule is that a schedule's
zone is declared; before this phase there was nothing to declare it
with. Unset keeps the old behavior exactly.

### Doc drift found (ground rule 8)

- `AGENTS.md:620-626` → the `:9000 is llama-swap, not vibe` bullet is at
  **AGENTS.md:666** (the "Router / model lifecycle" section). C5's doc
  pass shifted it. Fixed above.
- `warmsched.go:118-131` → `cronSpec.nextFire` is at
  **warmsched.go:136-157** (comment 136-143, func 144-157). Fixed above.
- `router/render.go:33-41` → `Options.ExtrasPath` is at **32-41**
  (comment 32-40, field 41). Close enough to leave.
- `llama-swap@v239 internal/store/store.go:22-45` and the routes at
  `internal/server/server.go:261-270` → verified against a checkout at
  `1f3c68e`: `TokenMetrics` at store.go:22-31, `ActivityLogEntry` at
  33-46, `ActivityPage` at 82-88; routes at **server.go:303-304**. All
  the field names and semantics the doc asserts are correct, including
  the two that matter most: `buildMetrics` overwrites `inputTokens` with
  `timings.prompt_n` when `timings` exists (store.go's parser, metrics.go
  442-457), and `cachedTokens`/`draftTokens`/`draftAccTokens` all default
  to **-1**. Confirmed too: `PruneActivity` runs only when
  `store.IsInMemory()` (metrics.go:76-80), so §0's `store: {path:}` does
  retire the 1000-row ring; and `shared.FetchContext` calls
  `cfg.RealModelName` and falls back to the client's string when it
  misses (shared/http.go:117-127) — which is exactly why collection is
  cell-side.

### Gates

| gate | result |
|---|---|
| 1. Store gate (24 h soak) | **NOT RUN** — live, needs real cells |
| 2. No-double-count | unit half **PASS** (`TestUsageFold_FrontCellIsSkippedStructurallyByName`); live half **NOT RUN** |
| 3. Cache semantics | unit half **PASS** (`TestClassify_ChatAddsCacheAndEmbedDoesNot`, `TestClassify_NegativeCacheSentinelClampsToZero`); live half **NOT RUN** |
| 4. Unmeasured | unit half **PASS** (`TestClassify_ZeroTokenTwoHundredIsUnmeasuredNotZero`, `TestClassify_NonTwoHundredIsErrorAndContributesNothing`); live half **NOT RUN** |
| 5. Self-traffic | unit half **PASS** (`TestClassify_OneTokenChatRowIsAPokeExcludedFromBillableSums`); live half **NOT RUN** |
| 6. Offline | unit analogue **PASS** (cumulative totals + `TestUsageLedger_FlushAndReplayAreIdempotent`); live **NOT RUN** |
| 7. Restart idempotency | **PASS** (`TestUsageLedger_FlushAndReplayAreIdempotent`, incl. the kill-between-fold-and-flush case) |
| 8. Epoch | **PASS** (`TestPoll_IDResetMintsANewEpochAndReingests`, `TestUsageFold_SameEpoch*`, `TestUsageFold_NewEpochStartsANewRowAndKeepsTheOld`) |
| 9. Timezone | **PASS** (`TestUsageDayKey_SplitsAtLocalMidnightNotUTCMidnight`, `TestUsageDayKey_HandlesBothDSTDiscontinuities`, `TestNoTruncateBasedDayBucketing`) |
| 10. MTP | **PASS** (`TestClassify_DraftTokensNeverEnterAnySum`) |
| 11. Streaming contract | **PASS** — `git diff --stat internal/vibe/proxy/` empty; `stream_options` absent from the diff |
| 12. Inner loop | **PASS** — build / vet / gofmt / mod tidy / `test -race -count=5` / golangci-lint |

### Adversarial self-review (ground rule 9)

Landed as its own commit against the feature commit. Seven findings,
all fixed, each with a regression test where one was possible.

1. **Two overlapping `Poll`s double-counted the whole window
   (blocker).** `Poll` read the cursor, released the lock for the HTTP
   round trip, then folded. Two concurrent calls both saw the old cursor
   and both ingested the same rows. Invisible to `-race` — it is a logic
   race, not a memory one. Fixed with a whole-poll `pollMu`.
   `TestPoll_ConcurrentPollsDoNotDoubleCount`.
2. **`lost_rows` was assigned, not folded as a delta.** The cell reports
   it cumulatively like every other counter, so every day after a loss
   repeated the same number and any cross-day sum was wrong. It now runs
   through the same cursor machinery as the token counters (and is
   therefore persisted the same way).
   `TestUsageFold_LostRowsFoldAsADeltaAcrossDays`.
3. **Compaction after a degraded read was silent data loss.**
   `newUsageLedger` compacts by rewriting the file from memory; if
   `load` could not read the whole file (open error, scanner error), the
   rewrite deleted whatever had not been read. Compaction is now skipped
   on a degraded read. Unparseable LINES still compact away — that is
   the cleanup, and it is why JSONL was chosen.
   `TestUsageLedger_DegradedReadSkipsCompaction`.
4. **A first-boot backfill could hold the heartbeat hostage.** `Poll`
   runs inline in the announce loop, and a ten-page walk against a slow
   store on a 10s-per-request client is up to 100 s. The heartbeat is
   the cell's only evidence of life. `PollAndSnapshot` now derives a 20 s
   deadline; an interrupted backfill costs nothing, because the cursor
   only advances on a completed poll.
5. **A cell that lost every row it read announced nothing.** `Snapshot`
   returned nil on an empty model set, so a total loss was
   indistinguishable from an idle cell — the exact confusion `lost_rows`
   exists to prevent. It now reports the loss with no models, and `fold`
   accepts a model-less usage block.
   `TestSnapshot_ReportsALossWithNoModels`,
   `TestUsageFold_ModellessLossStillLands`.
6. **Ledger file mode depended on which writer ran first** (compaction's
   tmp+rename made 0600, the append path made 0644). Both are 0600 now,
   matching the intent and lease stores.
7. **Residency truncated every heartbeat gap to whole seconds.**
   Announces land on a jittered ~15 s cadence, so truncation loses half
   a second per heartbeat — a steady ~3% under-count, and exactly the
   kind of quiet bias C7b would build an energy figure on. It rounds now.
   `TestUsageResidency_RoundsRatherThanTruncatesTheGap`.

Known and accepted, documented rather than fixed:

- **A first poll against a large persistent store reports the
  un-walked prefix as `lost_rows`.** With no prior state the cursor is 0
  and the walk is bounded at ~10 k rows, so a box with months of history
  announces a large one-time loss. That is honest — those rows were
  genuinely never counted — and the alternative (starting the cursor at
  `max_id` and pretending) would silently discard the same history.
- **Residency credit lands entirely on the day of the announce**, so a
  heartbeat straddling local midnight misattributes at most one
  staleness bound (~50 s).
- **The front skip is by cell NAME.** A box that serves the front
  llama-swap while announcing under some other cell name would double
  count. That is a topology error (the front owns no models), not a
  fold-time condition the ledger can detect.

### §0 is an operator step, not a code change

The `store: {path: …}` extras block is per-cell llama-swap config and
lands in the private fleet repo (ground rule 3). Nothing in this repo
renders it: `router.Options.ExtrasPath` merges the file verbatim. Until
each cell has it, that cell's activity log is the 1000-row in-memory
ring and the collector will honestly report `lost_rows` and mint a new
epoch on every llama-swap restart.
