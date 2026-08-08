# C25 — `vibe bench replay`: your own traffic as the benchmark

Status: **DESIGN ONLY (2026-08-08)**, off `c25-bench-replay-design`
branched from `main` at `cb8b336`. No production code in this phase —
this doc plus one small gate closure in `internal/vibe/usagemeter`
(futures item 8's "counts only, never bodies" clause, which shipped
structurally and was never gated). Backlog item 11 in
[fleet-control-futures.md](../fleet-control-futures.md) §2, the last
substantial unbuilt entry:

> **`vibe bench replay`** — offline replay of a cell's `/api/captures`
> through a candidate model: tok/s, tool-call rate, divergence vs
> recorded prod responses. Replay in place, emit only scores (captures
> are private traffic; never sync them into a bench corpus). Makes "new
> release dropped" answerable against *your* workload. The
> invariant-violating version — live shadow routing at the front — stays
> dead.

The entry was written from expectation in July 2026 and never checked
against a binary. Checking it is what this doc is mostly about: **five
of its premises are wrong, and every one of them makes the phase
smaller and the privacy rule stronger.** §1 is the finding; everything
after it follows from §1.

---

## 1. The finding that reshapes the phase

`/api/captures` — plural, listable, a pool you draw a corpus from —
**does not exist and never did**. What exists is
`GET /api/captures/{id}`: one capture, by activity-row id, out of a
size-bounded FIFO buffer in RAM.

Checked directly against upstream at both pinned versions, because
[ground rule 8](README.md#ground-rules-for-the-implementing-agent) says
the code wins:

| fact | v239 | v247 |
|---|---|---|
| route | `GET /api/captures/{id}`, `internal/server/server.go:269` | same, `internal/server/server.go:311` |
| handler | `internal/server/apigroup.go:232-252` | `internal/server/apigroup.go:309-329` |
| payload type | `ReqRespCapture{ID, ReqPath, ReqHeaders, ReqBody, RespHeaders, RespBody}`, `internal/server/captures.go:14-21` | byte-identical, same file, same lines |
| storage | `cache.New(captureBufferMB * 1024 * 1024)` — in-process, `internal/server/metrics.go` | same |
| eviction | FIFO by insertion, `internal/cache/cache.go` | same |
| enumeration | none; `has_capture` on each `/api/metrics/activity` row | same |
| auth | `apiChain` — 401 without the key, like every route but `/health`. **Read off source, not measured** — see §4 | same |

**There is no version skew.** That is the phase's one piece of good
luck: the CI conformance matrix
([`.github/workflows/ci.yml`](../../../.github/workflows/ci.yml)) spans
v239 and v247, and this contract is identical across it. Five
consequences, in descending order of how much they change the design.

### 1a. There is no corpus, because llama-swap already threw it away

`captureBuffer` defaults to **10 MB** and `0` disables it. The buffer is
a `map[int][]byte` in the server process with FIFO eviction — not the
SQLite store. So a capture's lifetime is *"until 10 MB of newer
compressed traffic arrives"*, and **zero across a llama-swap restart**.
C7a §0's `store: {path: …}` — the config commit that made the activity
log durable — does nothing for captures. They were never in the store.

The backlog entry's hardest rule ("never sync them into a bench corpus")
turns out to be one this design mostly cannot break: there is nothing to
sync. What is left for the design to guarantee is that it does not
*build* the corpus llama-swap declined to keep, and §3 makes that
mechanical rather than promised.

The arithmetic is friendlier than it sounds. An agentic coding request
with a 100k-token prompt is roughly 400 KB of JSON, and JSON that
repetitive zstds to something like 60–80 KB, so 10 MB holds on the order
of 125–160 recent requests. That is a usable n. It is also a *guess*,
and §7's L1 exists to replace it with the real number off the real box.

### 1b. A config reload wipes the buffer — which sets the whole ordering

`-watch-config` does not mutate a running server. It builds a new one:
`server.New(newCfg, …)` at `llama-swap.go:244`, which calls
`newMetricsMonitor(…, cfg.CaptureBuffer, st)` at `internal/server/server.go:192`,
which allocates a **fresh, empty** `captureCache`. The activity *store*
is carried across (it is only rebuilt when the store path changes,
`llama-swap.go:228-242`); the capture buffer is not.

C18's whole apply step is a config write that triggers exactly this
reload. So:

> **At the moment the candidate first exists, the sample is gone.**

"Apply the trial, then replay the cell's captures" is not a thing that
can happen. The harvest must precede the apply, and the sample must be
held across it. §4 is that ordering, and the reason it is worth reading
twice is that **the ordering constraint and the privacy invariant turn
out to be the same constraint**: the only place a sample can live across
an apply is the memory of the one process running the command, and a
sample that lives only there is a sample that cannot be leaked.

The API stays honest about the wipe, which matters for §6's refusals:
`overlayCaptureState` (`internal/server/metrics.go`) recomputes
`has_capture` from the live buffer on every activity read, so after a
reload the old rows report `has_capture: false` rather than advertising
captures that 404.

### 1c. A capture is the most private object in the fleet

`ReqBody` and `RespBody` are the verbatim bytes: the user's prompt, the
system prompt, the tool definitions, the whole completion. Redaction is
five header names — `authorization`, `proxy-authorization`, `cookie`,
`set-cookie`, `x-api-key` (`internal/server/captures.go`,
`sensitiveHeaders`) — and **nothing in the bodies at all**.

`handleAPICapture` decompresses server-side and answers
`application/json`, so `[]byte` arrives as base64 strings. Two things
follow. **Ground rule 5 is satisfied**: reading a capture needs
`encoding/json` and `encoding/base64` and nothing else — no CBOR
decoder, no zstd decoder, no new dependency. And the wire is trivially
greppable, which is what makes §7's leak gates cheap to write.

Non-200 rows capture the request but never the response body
(`internal/server/metrics.go`, the `recorder.Status() != 200` branch) —
relevant to §5, because a request that made the model choke is the most
interesting one to replay and the one with no recorded answer to
diverge from.

### 1d. The enumeration path is one this repo already tails

`has_capture` is a field on `/api/metrics/activity` — the endpoint
`internal/vibe/usagemeter` already walks, page by page, newest-first,
with C15's key. The field is *already recorded* in this repo's
conformance fixtures for both versions
(`internal/swaptest/activity.go:37`, and every row of
`internal/swaptest/fixtures/v239/activity-page.json` and
`fixtures/v247/activity-page.json` carries `"has_capture": true`).

Those fixtures were recorded off the live `:9000` box on 2026-08-05
(`fixtures/v239/RECORDED`). So captures are on, right now, on the
reference fleet — not because anyone turned them on, but because
`captureBuffer` appears nowhere in vibe's renderer and the upstream
default is 10 MB. **The fleet has been retaining its own recent prompts
in RAM for months and no document said so.** That is a finding for the
operator independent of whether this phase is ever built, and §9 owes it
to `AGENTS.md`.

### 1e. The one place a capture could become a committed corpus is this repo

`internal/swaptest/record_test.go` records real endpoint responses off a
real llama-swap into files that get committed. Its author already has
the right instinct — `foldHome`'s comment says it in as many words:

> The repo is public and `/running` echoes the whole llama-server argv

— and the recorder performs two named redactions for that reason
(`logData` payloads, `$HOME`), both declared in `RECORDED`. **Captures
are the case that posture cannot handle.** Redaction works when the
sensitive part is a substring of something you still want; a capture's
sensitive part is the whole object. Fold the home directory out of a
prompt and you still have the prompt. There is no redaction that leaves
a useful capture fixture behind, so the only correct handling is refusal.
If the recorder ever learns `/api/captures/{id}`, the next fixture commit
publishes a real prompt from the recording box to a public repo.

So the rule the backlog entry wrote for the fleet applies first to the
repo: **the recorder must refuse the captures endpoint by name**, and
the swaptest double must serve `/api/captures/{id}` from a *synthetic*
capture written by hand. This is also ground rule 3 — reference-fleet
example values only — reaching a place nobody had applied it. §7's U8
and U9 are the gates.

---

## 2. What this is, and what it deliberately is not

### It is not live shadow routing at the front, and that is not a scoping call

The backlog entry says the shadow version "stays dead". It is worth
saying *why* in ground-rule terms, because "out of scope" is the kind of
reason that gets revisited by someone in a hurry.

[Ground rule 1](README.md#ground-rules-for-the-implementing-agent), as
amended 2026-08-02, permits *observing* the data plane and forbids
instrumentation that changes the bytes a client receives, the flush
timing, or the hot-path latency — and forbids a bug in an accounting
path failing a user's request. A shadow is not an observation. It is a
second emission, and there is no version of it that clears the rule:

- **It contends for the thing it is measuring.** On a single-GPU cell
  the shadow generation runs on the GPU serving the request it is
  shadowing. Not "adds latency in principle" — *directly slows the
  request whose latency it is recording*. There is no async, no
  fire-and-forget, no priority knob that makes a second decode free.
- **It must buffer the request before forwarding it.** The shadow needs
  the body; the front cannot both stream a body upstream and keep it.
  Buffering is a change in flush timing, named in the rule.
- **It loads models.** A shadow request for a candidate that is not
  resident is an ordinary request, and llama-swap's contract is
  JIT-on-request: it starts the candidate, which on a one-model cell
  evicts the model serving the user. That is [C8](c8-probe-model.md)'s
  cardinal rule violated in its worst direction — the measurement
  becomes an actuator, and the thing it actuates is eviction.
- **Its failure modes both break the rule.** A hung shadow dial either
  backpressures the real request (an accounting path failing a user's
  request) or drops silently (a metric that lies about its own n).
- **It is a residency decision made by the wrong party.** Ownership axis
  3: residency belongs to llama-swap. A front that decides what a cell
  loads has taken it.

And one that is not a ground rule but should be: the capture already
exists. A shadow *manufactures a second copy* of private traffic in a
second process. Replay works from bytes llama-swap already holds and is
already going to evict.

### It is not a new command tree

See §4. Replay is delivered inside [C18](c18-model-try.md)'s trial
sequence, not as a `vibe bench` namespace, because it needs C18's lease,
hold, journal, rollback and its "both sides warm, both after the same
reload" controls, and rebuilding those is the phase that just landed.

### It is not a health check, and it never marks a model

Replay writes nothing to the announce's `probe` block, never marks
`degraded`, never withdraws a model from the render, never unloads and
never re-points an alias. See §5's third rule.

### It cannot answer "is my incumbent getting worse over time"

That is the question C8 answers, and replay structurally cannot: each
run's sample is different traffic, so a score kept across runs compares
two workloads and calls the difference a model change. **Replay compares
two models against one sample in one run, and it stores no baseline.**
Said here rather than discovered later, because "just save the score and
trend it" is the obvious next feature and it is wrong.

---

## 3. The privacy invariant, as a mechanism

The project's repeated lesson is that prose rules are treated as
advisory by models and humans alike, and that a mechanical guarantee
belongs at the boundary. "Replay in place, emit only scores" therefore
gets five mechanisms, not a comment. The nearest precedent is one this
branch also closed: C7a's ledger never stores bodies because
`usagemeter.ActivityRow` is a struct that *cannot decode them*, and as
of 2026-08-08 that is gated by
`TestActivityRow_CannotCarryABody` rather than assumed.

**1. A result type that cannot carry a body.** The package has exactly
two shapes. `sample` is unexported, holds `[]byte`, has no
`MarshalJSON`, and appears in no exported signature. `Report` — the only
thing that leaves the package — has no `[]byte` field, no `map`, and no
`string` field that is not drawn from a closed set: a def name, a metric
name from `modelprobe`'s constants, or a tool name that §5 restricts to
the request's own declared list. Gated by a reflection walk over
`Report` and everything reachable from it, the same technique as
`TestActivityRow_CannotCarryABody`, so *adding a field* is what turns
the test red.

**2. The package cannot write a file.** An AST test asserts the replay
package's import set contains no writer and that `os.WriteFile`,
`os.Create`, `os.OpenFile` and `os.MkdirAll` appear nowhere in it. The
precedent is C7a's own gate style — `git diff --stat
internal/vibe/proxy/` empty, and `stream_options` appearing nowhere in
the diff — and the reason it is worth a test rather than a review note
is that the leak this prevents is one line long and looks helpful
(`// cache the sample so a resume doesn't re-harvest`).

**3. Bodies are converted to shapes at the boundary, once.** The scorer
does not take a capture. It takes a `shape`: `{hasToolCall bool,
toolName string, toolNameDeclared bool, argsValidJSON bool,
argsMatchSchema bool, finishReason enum, outTokens int, httpStatus
int}`. Extraction happens in one function, at the point of fetch, and
the `[]byte` is not reachable from anything downstream. "Emit only
scores" then holds by the shape of the call graph rather than by
discipline at each print site — which is the [C20](c20-invariant-harness.md)
posture applied to a new surface.

**4. No capture text reaches the terminal, including on the error
paths.** C18 needed `printableSnippet`/`checkPrintable` because
llama-swap's *own* error strings reach the operator's terminal. Replay's
rule is stronger and simpler: text originating in a capture reaches no
output at all. A failure mentioning a capture names its activity id and
its HTTP status, and nothing else. Gated end-to-end: a capture whose
bodies are a marker string, driven through the success path and every
refusal path, with the marker asserted absent from stdout, stderr, the
journal and every log line.

**5. Nothing crosses a box.** Replay runs on the cell holding the
models, emits no announce field, adds no HTTP route, adds no MCP tool
and sends fleetd nothing. The sample never leaves the process; the
scores never leave the box except as the operator's own terminal output.

**The sample is never a resumable state.** A `sampled` journal state
between C18's `staged` and `applied` was considered and rejected: a
journal state that means "a sample exists" is a standing invitation to
persist it, and it buys only the ability to resume a replay across a
process death — which is a thing that *should* fail, because the sample
would be stale traffic replayed against a decision made later. The
sample exists for one process lifetime and there is no state that could
hold it. If the command dies, the trial resumes without a replay and
says so.

---

## 4. How it relates to C8 and C18 — and how much it shrinks

### It is C18's `measured` step with a different sampler

[C18](c18-model-try.md) §6 already measures both sides with C8's
`modelprobe`, on the same box, both warm, both after the same reload,
under a lease, and prints what is not controlled for. Replay changes the
*sample*, from one canned deterministic prompt to n of the cell's own
recent requests, and adds a scorer that reads structure instead of
speed. Everything else in C18 §6 is reused verbatim, including the
refusal to print a ratio when the two sides reported different metrics.

The justification for the phase existing is a sentence C18 already
wrote about its own limits:

> **throughput is not quality.** A faster model that tool-calls worse is
> a worse model, and no probe in this repo measures that. The report
> says so in those words.

That is the hole. C25 fills exactly it and nothing else.

**So yes — the phase shrank on contact.** It is C18 plus a sampler plus
a structural scorer. It is not a benchmark harness, it has no corpus
management, no cross-cell aggregation, no `--since`, no store. The
backlog entry's own title (`vibe bench replay`, a top-level verb) is the
part that did not survive: it is delivered as a flag on the trial
sequence, and §9 records that the futures entry should be updated to say
so.

### The ordering, which §1b forces

One uninterrupted invocation, on the cell, holding a lease:

1. **harvest** — before anything is written. Walk
   `/api/metrics/activity` newest-first for rows with
   `has_capture: true` on the incumbent's id, `GET /api/captures/{id}`
   each, extract, hold in memory. This is the only step that reads
   private traffic and it is over before the config is touched.
2. **apply** — C18's existing step, unchanged. The reload evicts
   everything and empties the capture buffer, which no longer matters.
3. **warm the incumbent, replay the sample** — n requests, scored.
4. **warm the candidate** (evicting the incumbent), **replay the same
   sample** — n requests, scored.
5. **report** — paired, then C18's rollback story as before.

Step 1's placement is not a preference. Steps 3 and 4 are the only place
a sample could otherwise be taken, and by then it does not exist.

### What is reused, named

| from | what |
|---|---|
| `internal/vibe/usagemeter` | the `/api/metrics/activity` walk (newest-first, `limit` capped at 999, the page loop), C15 key handling, and `BasisFor` to keep chat captures and drop the rest |
| `internal/vibe/modelprobe` | `isResident` and the never-load refusal; the cooldown/daily-cap shape; `MetricDecode` vs `MetricE2E` never compared; `Config.ReadOnly` (added by C18 for this exact reason — replay reads the cell's probe state and writes none of it) |
| `internal/vibe/modeltry` | the journal, the lease, the hold, the apply, the rollback, the report renderer and its caveat block |
| `internal/vibe/fleetapi/swapauth.go` | the key-reading and `Authorization: Bearer` posture, unchanged. **Note the gap**: that file's endpoint list is one someone verified against a real v239 binary, and `/api/captures/{id}` is not on it. The claim in §1's table — that it sits on `apiChain` and so 401s without a key — is read off upstream *source*, not measured. It is one `curl` to promote, and the implementer should do that and extend swapauth.go's comment rather than inherit an unverified row |
| `internal/swaptest` | the double, extended with a synthetic `/api/captures/{id}` |

### C8's hardest rule, inherited with three teeth

> the measurement must never become an actuator

- **Replay never loads a model.** Same refusal-with-an-instruction as
  `probe_model`: warm it first, with the verb that exists for warming.
- **A bad replay score changes nothing.** No `degraded` mark, no
  unload, no alias move, no render exclusion, no notification. It prints
  and it stops. C8's §5 argument for why a degraded model is *not*
  withdrawn applies unchanged and more strongly, because replay's n is
  smaller and its sample is not deterministic.
- **Replay never turns captures on.** If a cell's `captureBuffer` is 0,
  replay refuses by name and prints the config key. It does not write
  the cell's llama-swap config. *How much private traffic a box retains
  is a declaration*, and a benchmark that quietly widens it has made an
  intent decision that belongs to the operator — ownership axis 2, in a
  place nobody expected to find it.

---

## 5. The divergence metric, honestly scoped

This is the part of the backlog entry most likely to be silly, and it
is: "divergence vs recorded prod responses" as text similarity is close
to meaningless here. Four compounding reasons, and then what survives.

**Sampling makes a model diverge from itself.** The captured request
body carries the *client's* own `temperature` and `top_p`. Real agentic
clients send temperature above zero. Replaying a captured request
through the very model that produced the recorded response yields
different text. The floor is not zero and is not knowable in advance.

**It cannot be fixed by injecting a seed.** llama.cpp accepts `seed`,
but real clients do not send it, and adding one edits the client's
request — the same objection C7a raised against injecting
`stream_options`, for the same reason: a sample you had to falsify is no
longer *your* workload, which was the entry's whole point.

**Streaming recorded responses are frames, not messages.** For an SSE
response `RespBody` is the raw buffered frame stream. Reassembling it to
"the text the client saw" is possible but llama.cpp's chunk boundaries
are not a stable contract, so it adds a second, silent source of skew to
a metric that already has one.

**Non-200 rows have no recorded response at all** (§1c). The most
interesting requests in the sample are the ones with nothing to compare
against.

### What survives, and why

**Nothing that needs the recorded response is a primary metric.** The
metrics that matter are computable from the *request* — which declares
the tools, the schema and the token budget — and the *candidate's own
response*. The recorded response is demoted to a control.

1. **Paired tok/s.** Both sides see the identical sample, so prompt
   length composition cancels exactly. Report the **median paired
   per-request ratio**, never the ratio of means: one 100k-token request
   in the sample otherwise decides the answer. Read from llama.cpp's
   `timings` block, C8's `MetricDecode`, with C8's rule that a side
   falling back to `MetricE2E` is not comparable and prints no ratio.
2. **Tool-call correctness**, the metric C18 named and could not
   measure, and the one the operator actually cares about. Four
   per-request booleans, all structural, all immune to temperature:
   did a response arrive with a tool call when the request declared
   `tools`; is the arguments JSON parseable; does the called name appear
   in *the request's own* `tools` array; do the arguments carry the
   declared required keys. A name that is not in the request's list is
   reported as `<undeclared>` and never echoed — a hallucinated tool
   name is model output, which is to say private traffic.
3. **Structural outcome distribution.** `finish_reason`, truncation at
   the request's own `max_tokens`, empty responses, output length. Each
   is a per-request category; over n they are a distribution, and a
   paired one.
4. **Schema conformance** where the request carried
   `response_format`/a grammar. Ground truth is again in the request.

### The recorded response, used correctly: as the noise floor

The one honest use of the recorded prod response is a **control run**:

> Replay the sample through the **incumbent** — the model that produced
> those recorded responses — and measure its structural disagreement
> with its own recorded output. That is the floor. The candidate's
> divergence is reported only as a delta above it, and when the
> candidate is not above the floor the report prints **no divergence
> claim at all**.

The incumbent replay in step 3 is already happening for tok/s, so the
floor costs nothing extra.

And divergence is **structural agreement, never text similarity**: did
the two responses agree on tool-call-vs-prose, on the tool name, on
`finish_reason`, on JSON validity. "Same tool, different arguments" is a
real signal. "Different wording" is not a signal and the report must not
render it. No edit distance, no embedding similarity, no LLM judge —
the last of these being a thing this operator has already measured as
the unreliable half of a prose pipeline.

### The n, and when to refuse a number

Every figure carries its n, and n is not the operator's to choose — it
is whatever survived the FIFO buffer. Refusals:

- `captureBuffer: 0` → refuse, name the key, write nothing.
- no rows with `has_capture` → refuse: "this cell has served nothing
  recently". **n = 0 is not a score of 0**, which is C7b's own rule
  about em dashes and measured zeros.
- n below the aggregate floor → print the per-request table, refuse the
  rate. A proportion on n = 3 is noise wearing a percent sign. The
  proposed floor is **n ≥ 20 for any rate**, and it is a judgement, not
  a derived constant: C8 wants five samples before a *paired scalar*
  means anything, and a proportion needs more than a scalar does. It
  should be a constant in the source with this paragraph next to it,
  not a flag.

### The accounting cost, named rather than hidden

A replay is n real requests through the cell's own llama-swap, so it
lands in C7a's ledger as ordinary traffic under whichever model id
served it — the trial id for the candidate side (C18 already says trial
tokens land under their own id), the **incumbent's** id for the control
side. C7a's poke classifier keys on one-token chat rows and will not
catch these. So a replay measurably inflates that cell-day, and the
report must say so with the number: *"this run added N requests and M
tokens to `<model>` on this cell-day"*. It also evicts older captures
from the buffer, since the replays are themselves captured — worth
knowing, and not a leak: the buffer is fixed-size either way.

---

## 6. Refusals, in one list

Every one is a refusal with an instruction, never a silent degradation.

| condition | answer |
|---|---|
| `captureBuffer: 0` on the cell | refuse; name the key; do not write the config |
| no `has_capture` rows for the incumbent id | refuse; "nothing recent to replay" |
| n below the rate floor | per-request table, no rate |
| the incumbent is not resident at step 3 | C8's refusal; warm it first |
| the candidate is not resident at step 4 | same |
| `--cell` is not this box | C18's `RefuseRemote`; a capture must not cross a box, and neither may a replay |
| the front cell | refused: the front's captures are peer traffic already counted on a cell, and its config resolves no aliases (C7a's attribution argument, unchanged) |
| an `mlx_server` or `cloud_peer` candidate | C18's existing refusals |
| a capture whose `req_path` is not a chat path | dropped by `BasisFor`, counted in the report as `skipped_basis` |
| a capture that 404s (evicted between the activity read and the fetch) | dropped, counted as `evicted`, never retried |
| a process death mid-run | the trial resumes without a replay and says why |

---

## 7. Acceptance gates

Each row is a mechanical run. Unit gates are a `go test -race -count=5`
claim; live gates say which physical fact they need.

### Unit

| # | gate | shape |
|---|---|---|
| U1 | `Report` and everything reachable from it can hold no body: no `[]byte`, no `map`, no open-set `string` | reflection walk, mutation-verified by adding a field |
| U2 | the replay package writes no file: `os.WriteFile`/`Create`/`OpenFile`/`MkdirAll` appear nowhere, and no writer is imported | AST/grep test over the package |
| U3 | a capture whose bodies are a marker string leaks it to no output — stdout, stderr, journal, log — on the success path and on every refusal path | end-to-end against the swaptest double |
| U4 | an undeclared tool name is reported as `<undeclared>` and never echoed | table test over responses calling a tool the request never declared |
| U5 | the harvest happens before the apply, and a run whose apply is forced first is refused rather than replaying an empty buffer | ordering test against the double |
| U6 | the noise floor suppresses the claim: a candidate whose structural disagreement does not exceed the incumbent's own is reported with **no** divergence figure | synthetic paired shapes |
| U7 | every refusal in §6 fires by name, and none of them writes a config | table test |
| U8 | `internal/swaptest/record_test.go` refuses `/api/captures/{id}` by name | test asserts the recorder's endpoint list and that the refusal is explicit, not incidental |
| U9 | no fixture under `internal/swaptest/fixtures/**` contains a capture | tree walk; the gate that keeps ground rule 3 true |
| U10 | paired tok/s is a median of per-request ratios, not a ratio of means, and prints no ratio when the two sides used different metrics | fixture with one dominating request |
| U11 | n = 0 is a refusal, never a zero; n below the floor prints the table and no rate | table test |
| U12 | a 404 on `GET /api/captures/{id}` mid-harvest is counted as `evicted` and never retried | double drops a capture between the two calls |
| U13 | the double serves `/api/captures/{id}` identically under the v239 and v247 wire fixtures | conformance, alongside the existing matrix |
| U14 | full inner loop, plus ground rule 9's adversarial pass as its own commit | |

### Live

| # | gate | needs |
|---|---|---|
| L1 | **the n gate.** On a real cell after a real day of agentic traffic, how many captures does a 10 MB buffer actually hold, and what is the token-length distribution? §1a's 125–160 is arithmetic, not a measurement, and the rate floor depends on it | **a time budget** — one day of ordinary use on the reference fleet, then one `/api/metrics/activity` walk. No hardware this box lacks |
| L2 | **the reload-wipe gate.** Against a real llama-swap with `-watch-config`: read a row's `has_capture`, touch the config, confirm the same row now reports `has_capture: false` and the fetch 404s | **a time budget** — `scripts/fleetlab`, once ports are offsettable (futures item 15) |
| L3 | **the leak gate on metal.** A full run against a real cell, with the operator's own traffic in the buffer, grepping the entire terminal transcript and every file the command's process touched (`strace -e trace=openat,write`) for any substring of any captured body | **a time budget**, and a willingness to run it against real traffic — which is the only way this gate means anything |
| L4 | **the magnitude gate.** A real GPU, a real candidate, and a tool-call rate difference a human agrees with. This phase's whole output is a judgement about a model, and CPU models tool-call differently than GPU ones | **metal** — the same qualification C18's L5 carries, and for the same reason |

---

## 8. Size

Small-to-medium; smaller than C8, materially smaller than C18.

| piece | estimate |
|---|---|
| `internal/vibe/benchreplay` — harvest, extract, score, report | ~450–550 lines |
| wiring into `internal/vibe/modeltry` (the ordering, the flag, the report block) | ~80–120 lines |
| `internal/swaptest` — synthetic `/api/captures/{id}` on the double, the recorder refusal | ~80 lines |
| tests (U1–U14) | ~700–900 lines |

No new dependency (§1c), no new HTTP route, no new MCP tool, no proto
change, no new store on fleetd, and an empty diff for
`internal/vibe/proxy`.

The largest single risk is not the scorer — it is U8/U9, because the
recorder refusal has to be written by someone who has internalised §1e,
and the failure mode is a fixture commit that publishes a real prompt.

---

## 9. For the reconciliation pass

This branch does not touch `AGENTS.md`,
`docs/design/fleet-control-plan/README.md` or
`docs/design/fleet-control.md`. Everything meant for them is below.
`docs/design/fleet-control-futures.md` **is** updated on this branch
(item 8 → SHIPPED-with-an-open-hole, verified clause by clause).

### For `AGENTS.md` — the operator-facing finding from §1d

This is the one item here that matters whether or not C25 is ever built,
because it is true of the fleet today. Suggested, under the router /
model-lifecycle section:

> **llama-swap retains recent request and response bodies in RAM by
> default.** `captureBuffer` defaults to 10 MB and vibe's renderer sets
> it nowhere, so every cell and the front hold a rolling FIFO window of
> verbatim prompts and completions, readable at
> `GET /api/captures/{id}` by anything holding the cell's llama-swap key
> and discoverable via `has_capture` on `/api/metrics/activity`. It is
> in-process memory, not the store: a restart or a `-watch-config`
> reload empties it. Set `captureBuffer: 0` through `--extras` to turn
> it off; it is a deliberate declaration either way, and no vibe verb
> may change it (C25 §4).

### For `docs/design/fleet-control-plan/README.md` — the status row

> | [C25](c25-bench-replay.md) | `vibe bench replay`: your own traffic as the benchmark | design only; ~700 lines estimated | C8, C18 (composition), C7a (the activity walk) | **DESIGN (2026-08-08)**; the backlog entry's `/api/captures` premise corrected against v239+v247; delivered as a C18 flag rather than a top-level verb; not implemented |

Plus a row in the owed-gates table for C25 L4, matching C18 L5's
wording: *needs metal, not a time budget.*

### For `docs/design/fleet-control.md` §9 — the rejected-alternatives table

> | **Live shadow routing at the front** (the front duplicates each model-dispatched request to a candidate as it flows, and scores the copy) | Rejected 2026-08-08 ([C25](fleet-control-plan/c25-bench-replay.md) §2). Ground rule 1 permits observing the data plane; a shadow is a second *emission*. On a single-GPU cell it contends for the GPU serving the request it is measuring, so there is no version of it that leaves hot-path latency unchanged; it must buffer the request body before forwarding, which changes flush timing; a shadow for a non-resident candidate JIT-loads it and evicts the model serving the user, which is C8's cardinal rule violated in its worst direction; and its failure modes are either backpressure on a user's request or a silently short n. It is also a residency decision taken by the front, which axis 3 gives to llama-swap. The replacement is offline replay of captures llama-swap already holds and is already going to evict. |

### For `docs/design/fleet-control-futures.md` item 11 — if C25 is built

Item 11's own text needs the §1 correction: `/api/captures` is
`GET /api/captures/{id}` out of a 10 MB in-RAM FIFO, there is no corpus
to sync and therefore no corpus rule to break, and the deliverable is a
flag on `vibe model try` rather than a `vibe bench` verb. Left unedited
on this branch so the entry still reads as written when the phase doc is
compared against it.

---

## 10. Open questions for whoever builds this

1. **Should the incumbent-side replay be optional?** It costs n real
   requests against the production model and inflates its ledger day
   (§5). Skipping it saves that but forfeits both the paired ratio and
   the noise floor — i.e. every number that survives §5. The tentative
   answer is no, it is mandatory, and the accounting cost is disclosed.
2. **Is `has_capture` trustworthy at the moment of fetch?**
   `overlayCaptureState` computes it on read, and FIFO eviction proceeds
   during the harvest, so a long harvest races its own buffer. §6 counts
   the 404s as `evicted`; whether a harvest that loses more than some
   fraction of its intended sample should refuse outright is unresolved.
3. **Does the front hold captures worth anything?** It does hold them,
   and they are the only place a cloud-peer request is visible — which
   is futures item 8's open bypass hole wearing a different hat. Reading
   them would mean scoring cloud responses, which is a different phase
   and a much worse privacy story. Named, not opened.
4. **Multi-turn.** A captured agentic request carries the whole prior
   conversation in its own body, so replay is single-shot by
   construction and that is fine. But it means the sample is biased
   toward long prompts, and the median paired ratio should probably be
   reported alongside a short-prompt subset. Unresolved.
