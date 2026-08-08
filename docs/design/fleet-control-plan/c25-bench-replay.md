# C25 — `vibe bench replay`: your own traffic as the benchmark

Status: **BUILT (2026-08-08)**, off `c25-bench-replay` branched from
`main` at `899a414` (this doc's own merge). Delivered as
`vibe model try --replay` — a flag on [C18](c18-model-try.md)'s trial
sequence, not a `vibe bench` verb, for the reason §4 gives. See
[§11 Execution](#11-execution-2026-08-08) for what shipped, the gate
results and the two defects the phase's own tests found in it.

The design half below is unchanged from the DESIGN ONLY commit
(`c25-bench-replay-design`, branched from `main` at `cb8b336`), which
was this doc plus one small gate closure in `internal/vibe/usagemeter`
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
| auth | `apiChain` — 401 without the key, like every route but `/health`. ~~**Read off source, not measured** — see §4~~ **MEASURED 2026-08-08 on a real binary; see [§11](#the-auth-row-promoted-from-read-off-source-to-measured)** | same, measured |

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
| `internal/vibe/fleetapi/swapauth.go` | the key-reading and `Authorization: Bearer` posture, unchanged. **Note the gap**: that file's endpoint list is one someone verified against a real v239 binary, and `/api/captures/{id}` is not on it. The claim in §1's table — that it sits on `apiChain` and so 401s without a key — is read off upstream *source*, not measured. It is one `curl` to promote, and the implementer should do that and extend swapauth.go's comment rather than inherit an unverified row. **DONE — measured on real v239 and v247 on 2026-08-08 and written into `swapauth.go`; see [§11](#the-auth-row-promoted-from-read-off-source-to-measured)** |
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

---

## 11. Execution (2026-08-08)

Three commits, in the order the risk demanded: **the refusal first**,
then the feature, then the wiring. Shipping the swaptest capture refusal
before any code that could fetch a capture existed was not ceremony —
§8 names the recorder as the phase's largest risk, and the failure mode
is a fixture commit that publishes a real prompt to a public repository.

### What shipped

| piece | where | lines |
|---|---|---|
| the package: harvest, shape, replay, score, render | `internal/vibe/benchreplay/` (new) | 1744 (1103 non-comment) |
| the double's `GET /api/captures/{id}` + the recorder's refusal | `internal/swaptest/captures.go` (new), `swaptest.go`, `record_test.go` | 164 (+73 changed) |
| `Runner.Harvest`, `Measure(…, *benchreplay.Set)`, the report block | `internal/vibe/modeltry/` | +122 |
| `--replay`, the ordering, the early refusal | `internal/vibe/cli/cmd_model_try.go` | +84 |
| the measured auth row | `internal/vibe/fleetapi/swapauth.go` | +12 |
| `IncludeTests` / `Files` | `internal/astscan/astscan.go` | +26 |
| ten registry entries | `internal/mutation/mutation.go` | +130 |
| tests (U1–U13, plus the fake) | six `c25_test.go` / `fake_test.go` / `capture_contract_test.go` | 2088 |

**Against §8's estimate**: ~700 production lines predicted, **1262
non-comment production lines** shipped (2150 including comments), and
~800 test lines predicted against 2088. The overshoot is one thing:
§5's structural scorer needed a response reducer that handles BOTH a
JSON completion and a buffered SSE frame stream, because the reference
fleet's rows are all `text/event-stream` and without the streaming path
the noise floor would have been vacuous on every real box (see below).
`shape.go` alone is 409 lines. Everything else landed near estimate.

No new dependency, no new HTTP route, no new MCP tool, no proto change,
no new store on fleetd, and `git diff --stat origin/main..HEAD --
internal/vibe/proxy/` is **empty**.

### The privacy invariant, as built

All five mechanisms shipped, and four of the five are gated by a test
that goes red when the mechanism is removed:

1. **`Report` cannot carry a body.** A reflection walk over `Report` and
   everything reachable from it, against an explicit field allowlist
   (`reportFields`) plus a declared closed set for every string field
   (`closedSetStrings`). Rejects `[]byte`, `map`, `interface`, `chan`,
   `func`. `TestReportCannotCarryABody` +
   `TestClosedSetStringsAreActuallyClosed`, the second of which drives
   every string PRODUCER (including `normalizeFinish` against a
   4096-byte finish reason, and a model that named a tool after the
   operator's prompt) and checks the value is in the set.
   `TestSetCannotBeSerialized` covers the sample: every field of `Set`
   is unexported, `json.Marshal` of one is `{}`, and staticcheck's
   SA9005 fires on it — the linter agreeing with the assertion.
2. **The package cannot write a file.** `TestPackageWritesNoFile` parses
   every non-test file and fails on any `os.X` call outside
   `{ReadFile, Stat, Getenv, IsNotExist}`, and on any import of
   `os/exec`, `log`, `log/slog` or `bufio`. With an inertness floor.
3. **Bodies become shapes at the boundary, once.** `shape.go` is the
   only file that sees a `[]byte`; the scorer's inputs are `shape` and
   `requestFacts`. Held by the call graph, not by discipline.
4. **No capture text reaches any output.**
   `TestCaptureTextReachesNoOutput` puts a marker in every text field of
   a capture — prompt, system prompt, tool description, the recorded
   completion — and drives it through the success path and all five
   refusal paths, asserting the marker (and three sub-fragments of it,
   so a chopped-up leak cannot pass) is absent from the rendered report,
   the marshalled report, the marshalled `Set`, the computed caveats and
   every error string. It carries an explicit **control**: a
   caller-supplied warm error containing the marker MUST pass through,
   because otherwise the whole test would pass on a code path where
   nothing propagates at all. The journal half is
   `TestReplayScoresReachTheJournalAndCaptureTextDoesNot` in `modeltry`,
   which reads the trial journal back off disk and re-renders
   `vibe model try status` from it.
5. **Nothing crosses a box.** `TestNothingCrossesABox` fails on an
   import of `fleetapi`, `fleetmcp`, `fleetannounce`, `vibeclient`,
   `daemon` or `fleetnotify`.

**Stronger than the design in one place.** §3 permits `Report` to carry
"a tool name that §5 restricts to the request's own declared list". It
carries none. A tool name the request declared is still capture text, so
the per-request table reports the closed set `none` / `declared` /
`<undeclared>`, and the name lives only inside the unexported `shape`,
where it is used for agreement comparison and nothing else. This makes
U4 a stronger gate: the assertion is that the name appears in no field
and no byte of output, not that it was replaced with a marker.

### Harvest-before-apply: enforced by ordering, not documented

Three independent mechanisms, because this is the constraint that is
also the privacy invariant:

- **`modeltry.Runner.Harvest` refuses** when the journal says `applied`
  or `measured`. Journalled state, so a resumed process reads it too.
- **`Measure` takes the sample as a parameter**, and
  `benchreplay.Harvest` is its only producer. There is no path from
  inside the measurement to a buffer the apply has emptied.
- **The CLI calls it between the idle wait and `Apply`** — the freshest
  possible sample, still before any write.

`TestReplayHarvestsBeforeTheConfigIsWritten` asserts it by OBSERVATION
rather than by reading the source: it harvests, applies, then empties
the double's capture buffer the way a `-watch-config` reload does
(activity rows survive, captures do not), and checks the sample is still
in memory — then shows that a harvest at that point returns nothing
*even with the journal forced back to `staged`*, which is what makes the
refusal a description of reality rather than a convention.
`TestMeasureCannotObtainASampleOfItsOwn` is the type-level half.

A trial resumed past the apply measures **without** a replay and says
why, in one sentence naming the reload. §6's last row, as built.

### What the structural scorer compares — and refuses to claim

Everything primary is computable from the **request** (which declares
the tools, their required arguments, the token budget and any
`response_format`) and the **candidate's own response**. The recorded
production response is demoted to a control.

- **Paired tok/s**: median of per-request ratios, never a ratio of
  means. Read from llama.cpp's `timings` block (`decode_tok_s`), with
  wall-clock (`e2e_tok_s`) as a fallback tracked separately.
- **Tool-call correctness**: four per-request booleans, all structural,
  all immune to temperature — did a tool call arrive when the request
  declared `tools`; do the arguments parse; is the name in the request's
  own list; are the schema's required keys present.
- **Structural outcome distribution**: finish reason (normalised to a
  six-value enum, so a free string from the model reaches nothing),
  truncation against the request's own `max_tokens`, empty responses,
  output length.
- **Schema conformance** where the request carried `response_format`.
- **Divergence**: structural agreement only — tool-call-vs-prose, which
  tool, finish reason, JSON validity, emptiness. No edit distance, no
  embedding, no judge.

**Five things it refuses to claim**, each with a test:

| refusal | why | test |
|---|---|---|
| no divergence figure when the candidate is at or below the incumbent's own disagreement with its own recorded output | the captured request carries the CLIENT's temperature, so the floor is not zero and is not knowable in advance | `TestNoiseFloorSuppressesTheDivergenceClaim` |
| no divergence at all when the recorded response cannot be reduced | llama-swap stores no response body for a non-200, and those are the most interesting rows | `TestDivergenceIsNotMeasuredWhenTheRecordedResponseCannotBeReduced` |
| no ratio when the two sides used different metrics, or when either side mixed its own | `decode_tok_s` excludes queueing and `e2e_tok_s` does not | `TestNoRatioWhenTheTwoSidesUsedDifferentMetrics`, `TestASideThatMixedItsOwnMetricsIsNotComparable` |
| no proportion under n = 20; no paired scalar under n = 5 | every rate is an `observed.Value[float64]` and reads `unknown`, never `0%` | `TestBelowTheFloorThereIsATableAndNoRate`, `TestThePairedScalarHasItsOwnSmallerFloor` |
| no score at all at n = 0 | n = 0 is a refusal, never a score of zero | `TestZeroSamplesIsARefusalNotAScoreOfZero` |

And one it refuses structurally: there is **no way to obtain a one-sided
score**. `Set.Run` takes both sides and returns one `Report`, because an
API that could produce half of one is an API that invites somebody to
save it and trend it — which §2 says is the obvious next feature and is
wrong.

### Two defects the phase's own tests found in it

Both were caught by assertions written against the design's prose, in
the same session, before any review pass.

**D1 — a `0% faster` printed on n = 3.** §5 says the rate floor refuses
proportions, and the first cut applied that only to the proportions. The
paired median is a scalar and had no floor at all, so a three-request
sample rendered *"candidate is 0% faster than incumbent on the MEDIAN
paired request"*. §5's own sentence is the fix, and it distinguishes the
two cases: *"C8 wants five samples before a paired scalar means
anything, and a proportion needs more than a scalar does."* So there are
now **two floors**: `PairedFloor = 5` for the scalar and
`DefaultRateFloor = 20` for every proportion, each a constant with that
paragraph beside it. `TestThePairedScalarHasItsOwnSmallerFloor` pins
both at the boundary, in both directions.

**D2 — "the metrics differed" reported for a side that measured
nothing.** A candidate answering HTTP 500 to every request produces no
metric at all, and the first cut folded `metric == ""` into the
mixed-metrics branch — describing a comparison nobody could attempt as
one that was attempted and refused. It is `no-pairs` now.
`TestAFailedRequestIsAFailureNotAFastZero`.

Both are the absent-evidence class in a phase built to avoid it, which
is the argument for writing the assertions from the design's sentences
rather than from the code.

### The recorder refusal, and how it was proven

Four mechanisms, each mutation-verified — the mutation applied, the
named test watched to go RED by reading `go test`'s own exit status, the
file restored byte-identical:

| mutation | red |
|---|---|
| `RefuseCaptureEndpoint` always allows | `TestRefuseCaptureEndpoint_RefusesEveryFormOfTheRoute`, 9 findings |
| the guard dropped from `recordGET` | `TestRecorderFetchesOnlyThroughTheCaptureRefusal`, naming `recordGET` |
| `/api/captures/{id}` added to the recorder's endpoint list | `TestRecordedEndpointsNameNoCaptureRoute` |
| a capture-shaped fixture planted in `fixtures/v239/` | `TestNoFixtureContainsACapture`, on all four markers |

The first two are in `internal/mutation`'s registry and run in CI's
mutation job on every PR. The third and fourth are edits to DATA rather
than to a production line, so they were verified by hand here and are
recorded as such; the guards themselves run on every test invocation.

The refusal over-refuses by construction (prefix match on the
lower-cased path, with scheme, host, query and fragment stripped) and is
tested in **both** directions, so it cannot degenerate into "refuse
everything" — which would be a broken recorder, and a broken recorder
gets deleted rather than fixed.

**The double serves the route.** `GET /api/captures/{id}`, with
upstream's own 400/404/200 and base64 bodies. Two population rules:
`Cell.Request(...)` — the synthetic row driver, which has no body —
populates NOTHING, so the eviction case a harvest is most likely to get
wrong is what a test gets for free; `handleChat` records the caller's
own request and the double's own canned reply, because that is what
llama-swap does and because those bytes are synthetic by construction.

### Gates

#### Unit

| # | gate | result |
|---|---|---|
| U1 | `Report` can hold no body | **PASS** — `TestReportCannotCarryABody`, `TestClosedSetStringsAreActuallyClosed`, `TestSetCannotBeSerialized` |
| U2 | the package writes no file | **PASS** — `TestPackageWritesNoFile` (AST, with an inertness floor) |
| U3 | a marker leaks to no output, success path and every refusal path | **PASS** — `TestCaptureTextReachesNoOutput` (with a propagation control), `TestReplayScoresReachTheJournalAndCaptureTextDoesNot` (journal + `status`) |
| U4 | an undeclared tool name is marked and never echoed | **PASS** — `TestUndeclaredToolNameIsReportedAndNeverEchoed`; strengthened, see above |
| U5 | the harvest happens before the apply | **PASS** — `TestHarvestIsRefusedOnceTheTrialIsApplied`, `TestMeasureCannotObtainASampleOfItsOwn`, `TestReplayHarvestsBeforeTheConfigIsWritten` |
| U6 | the noise floor suppresses the claim | **PASS** — `TestNoiseFloorSuppressesTheDivergenceClaim` (at, below and above the floor), `TestDivergenceIsNotMeasuredWhenTheRecordedResponseCannotBeReduced` |
| U7 | every §6 refusal fires by name and writes no config | **PASS** — `TestEveryRefusalFiresByNameAndWritesNothing`, `TestReplayIsRefusedBeforeTheTwentyMinutePull`, `TestWithoutTheFlagNoCaptureIsEverRead` |
| U8 | the recorder refuses the captures endpoint by name | **PASS** — three tests, three mutations, see above |
| U9 | no fixture contains a capture | **PASS** — `TestNoFixtureContainsACapture`, walking the EMBEDDED tree (14 files), with a floor |
| U10 | median of ratios, not ratio of means; no ratio across metrics | **PASS** — `TestPairedRatioIsAMedianOfRatiosNotARatioOfMeans` (one dominating request, plus an assertion that the mean and the median genuinely differ on that fixture, so the test is a discrimination and not a coincidence), `TestNoRatioWhenTheTwoSidesUsedDifferentMetrics`, `TestASideThatMixedItsOwnMetricsIsNotComparable` |
| U11 | n = 0 refuses; under the floor, table and no rate | **PASS** — `TestZeroSamplesIsARefusalNotAScoreOfZero`, `TestBelowTheFloorThereIsATableAndNoRate`, `TestAtTheFloorTheRatesAppear`, `TestThePairedScalarHasItsOwnSmallerFloor` |
| U12 | a mid-harvest 404 is `evicted` and never retried | **PASS** — `TestAnEvictedCaptureIsCountedAndNeverRetried`, which asserts the double's READ COUNT is 1 rather than merely that the output is right |
| U13 | the route behaves identically under v239 and v247 | **PASS**, and further than asked — see below |
| U14 | full inner loop | **PASS** — `go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy` byte-clean, `golangci-lint run` **0 issues**, `go test -race ./...`, and `-race -count=5` over the eight touched packages |

**Ten production predicates are mutation-verified** and registered in
`internal/mutation`, so CI re-proves them on every PR rather than this
paragraph being the only record. The full harness reports **42/42 guards
mutation-verified in 40s**. The ten:

| mutation | red |
|---|---|
| the recorder stops refusing the captures endpoint | `TestRefuseCaptureEndpoint_RefusesEveryFormOfTheRoute` |
| the recorder's fetch guard moves off the one fetch path | `TestRecorderFetchesOnlyThroughTheCaptureRefusal` |
| the sample is harvested after the apply | `TestHarvestIsRefusedOnceTheTrialIsApplied` |
| an undeclared tool name is echoed instead of marked | `TestUndeclaredToolNameIsReportedAndNeverEchoed`, `TestClosedSetStringsAreActuallyClosed` |
| the divergence claim stops being gated on the noise floor | `TestNoiseFloorSuppressesTheDivergenceClaim` |
| a proportion is printed below the rate floor | `TestBelowTheFloorThereIsATableAndNoRate` |
| the paired ratio becomes a ratio of means | `TestPairedRatioIsAMedianOfRatiosNotARatioOfMeans` |
| an unreducible recorded response counts as agreement | `TestDivergenceIsNotMeasuredWhenTheRecordedResponseCannotBeReduced` |
| a replay loads a model that is not resident | `TestEveryRefusalFiresByNameAndWritesNothing` |
| the replay edits the client's own sampling (a `seed`, `temperature: 0`) | `TestReplayRewritesOnlyTheModelAndTheStreamFlag` |

#### U13, promoted: the capture contract measured against real binaries

U13 asked for the DOUBLE to behave the same under both wire fixtures.
What shipped is an invariant in the conformance suite —
`I7_a_capture_is_fetchable_by_activity_id` — which runs against both
wires of the double **and against a real llama-swap binary**, which is
what CI's conformance job supplies for v239 and v247. It drives one
request, reads `has_capture` off the activity row (the only enumeration
that exists), fetches the capture, and asserts the id is the ACTIVITY
row id, the payload shape, that `req_body` is valid JSON carrying the
messages that were sent, that none of the five redacted header names
survived, and that a capture that cannot exist answers 404.

It is the one place in this repository that deliberately fetches a
capture, and it is allowed to because the request it fetches is the one
the test issued three lines earlier — the prompt is `"hi"`. It asserts
structure and writes nothing.

**Executed here, 2026-08-08**, against binaries downloaded exactly as
CI's `conformance` job does (llama-swap v239 `dd81801` and v247
`40027d6`, llama.cpp `b10282`, `stories260K.gguf`):

- v239: `TestSwapContract/live/exec/I7_… PASS`, whole contract suite
  PASS, `TestSwapBehaviour` PASS (36.7 s).
- v247: `TestSwapContract/live/exec/I7_… PASS`, whole contract suite
  PASS.

#### The auth row, promoted from "read off source" to measured

§4's reuse table flagged one row: `swapauth.go`'s endpoint list was
verified against a real binary and `/api/captures/{id}` was not on it,
so §1's claim that it sits on `apiChain` was read off upstream *source*.
The design said the implementer should promote it with one `curl`.

**Measured 2026-08-08**, on a locally-started v239 and v247 with
`apiKeys:` set — never against the production `:9000`, and never
fetching a real capture:

| ask | v239 | v247 |
|---|---|---|
| `/health`, no key | 200 | 200 |
| `/api/metrics/activity`, no key | 401 | 401 |
| `/api/captures/1`, no key | **401** | **401** |
| `/api/captures/1`, wrong key | **401** (never 403) | **401** |
| `/api/captures/{missing}`, good key | 404 | 404 |
| `/api/captures/abc`, good key | 400 | 400 |
| an activity row's `has_capture` | `true` | `true` |
| capture keys | `id req_path req_headers req_body resp_headers resp_body` | identical |

`swapauth.go`'s comment now carries this, with the date and both
commits, and states the consequence §1d names: any holder of a cell's
llama-swap key can read that cell's recent prompts and completions
verbatim, because `captureBuffer` defaults to 10 MB and vibe's renderer
sets it nowhere.

#### Live

| # | gate | status |
|---|---|---|
| L1 | **the n gate** — how many captures a real 10 MB buffer holds after a day of agentic traffic, and the token-length distribution | **NOT RUN.** Needs a time budget: one day of ordinary use on the reference fleet, then one activity walk. §1a's 125–160 remains arithmetic. `DefaultMaxSample = 40` and `DefaultRateFloor = 20` are set against that arithmetic and should be re-examined once L1 has a real number. |
| L2 | **the reload-wipe gate** — `has_capture` flips false and the fetch 404s after a `-watch-config` touch | **PASS (2026-08-08), on real v239 and v247 binaries** — see the transcript below. It needed no lab at all: `scripts/fleetlab` was the wrong tool, because the gate is one llama-swap on a private port, and reaching for the shared rig was what made three earlier phases record it UNRUN. |
| L3 | **the leak gate on metal** — a full run against a real cell with the operator's own traffic in the buffer, `strace`-ing every file the process touches and grepping the whole terminal transcript | **NOT RUN.** Needs a willingness to run it against real traffic, which is the only way it means anything. U3 is its synthetic twin and covers the same surfaces — stdout, the journal, the marshalled report, the caveats, every refusal path — with a marker instead of a prompt. |
| L4 | **the magnitude gate** — a real GPU, a real candidate, and a tool-call rate difference a human agrees with | **NOT RUN — needs metal**, the same qualification C18's L5 carries and for the same reason. This phase's whole output is a judgement about a model, and CPU models tool-call differently than GPU ones; nothing in a CPU lab exercises it. |

**L2's transcript**, on a private port with a private config, driving one
completion through a real llama-swap started with `-watch-config`,
appending one comment line to its config, and waiting out the 2 s poll:

```
=== v239, -watch-config ===
  BEFORE  row id=1  "has_capture":true
  BEFORE  GET /api/captures/1 -> 200
  ... touched the config; waiting out the 2s poll
  AFTER   the same row still in the activity store? 1
  AFTER   "has_capture":false
  AFTER   GET /api/captures/1 -> 404
=== v247 — identical, line for line ===
```

That is §1b's whole claim, measured: the activity **store** is carried
across the reload and the capture **buffer** is not, `overlayCaptureState`
recomputes `has_capture` from the live buffer so the surviving row stops
advertising a capture rather than advertising one that 404s, and nothing
about the models changed — a comment appended to the config is a config
CHANGE as far as `-watch-config` is concerned, which is exactly what
C18's apply is. The harvest-before-apply ordering is therefore a
description of the binary's behaviour rather than of upstream's source.

**Not attempted and not possible are different**, and the difference is
recorded above: L1 and L3 need a time budget or a willingness this
session did not have; L4 needs hardware. Neither was attempted. L2 was
run and passed.

### §10's open questions, answered where the build had to decide

1. **Is the incumbent-side replay optional?** No, and it is not
   configurable: `Set.Run` takes both sides. The accounting cost is
   disclosed in the report with the number — *"this run added N real
   requests and M output tokens to this cell-day"*.
2. **Is `has_capture` trustworthy at fetch time?** No, and it does not
   need to be: a 404 is counted as `evicted`, never retried, and the
   loss is printed on the same screen as the number. Whether a harvest
   that loses most of its intended sample should refuse outright is
   **still unresolved** — it currently reports the loss and scores what
   survived.
3. **The front's captures.** Not opened. The front cell is refused by
   C18's existing rules before a harvest is reachable.
4. **Multi-turn.** Single-shot by construction, now stated as a caveat
   the report prints. The short-prompt subset is **not** built.

### One thing the design did not anticipate

**The recorded response is usually a buffered SSE frame stream, not a
JSON object.** §5 warns that reassembling streamed text is unsafe
because llama.cpp's chunk boundaries are not a stable contract, and it
is right — but on the reference fleet *every* recorded operator row is
`text/event-stream` (`fixtures/v239/activity-page.json`), so a reducer
that handled only JSON would have produced an unknown recorded shape for
every sample and a noise floor of n = 0 on every real box. The floor
would have been vacuous, and §5's whole "used correctly, as a control"
argument with it.

So `shapeOfRecorded` reduces SSE too, and the line it draws is exactly
§5's: **streamed text is never reassembled** — the reducer asks only
whether any content arrived at all — while tool-call ARGUMENT fragments
ARE concatenated, because that is the documented OpenAI streaming
contract and every client performs it. Schema conformance is the one
metric that needs the text, so on a streamed recorded response it is
DROPPED rather than answered "did not conform": an unanswerable question
must not become a failure nobody measured.
`TestSchemaConformanceIsGroundedInTheRequest` pins that direction.

---

## 12. For the reconciliation pass (execution addendum)

§9's three entries stand. Two corrections and one addition, from the
build.

**§9's `AGENTS.md` entry is now MEASURED**, not read off source. The
suggested paragraph should carry the numbers: on real v239 (`dd81801`)
and v247 (`40027d6`), `GET /api/captures/{id}` answers 401 without a
key, 401 with a wrong one, 404 for an evicted id and 400 for a
non-integer id, and the object is
`{id, req_path, req_headers, req_body, resp_headers, resp_body}` with
base64 bodies. Everything else in the §9 draft is unchanged and still
correct.

**§9's README status row** should now read:

> | [C25](c25-bench-replay.md) | `vibe model try --replay`: your own traffic as the benchmark | 1262 non-comment production lines + 2088 test | C8, C18 (composition), C7a (the activity walk) | **BUILT (2026-08-08)**; delivered as a C18 flag rather than a top-level verb; U1–U14 green, 10 predicates mutation-verified, the capture contract measured against real v239 and v247 binaries; L2 PASS on real v239+v247; L1, L3, L4 NOT RUN (L4 needs metal) |

Plus a row in the owed-gates table for **C25 L4**, matching C18 L5's
wording (*needs metal, not a time budget*), and rows for **C25 L1, L2's
remainder and L3** as time-budget gates.

**New, for `docs/design/fleet-control-futures.md` item 15**
(`scripts/fleetlab` port offsets, shipped as `FLEETLAB_PORT_BASE` in
C23): C25's L2 did **not** need it, and that is worth recording beside
the item. The gate is one llama-swap on a private port with a private
config; reaching for the shared four-cell rig is what made it look like a
lab gate, and three earlier phases recorded a live gate UNRUN for a
scheduling reason that the gate itself never had. Before the next phase
writes `scripts/fleetlab/gate-cNN.sh`, the question worth asking is
whether the invariant needs a FLEET or one process.

**Unchanged**: the `fleet-control.md` §9 rejection row for live shadow
routing at the front, verbatim as §9 drafts it. Nothing in the build
softened that argument — if anything the replay's own accounting caveat
("this run added N real requests to this cell-day", for traffic the
operator ASKED for) makes the shadow's silent version worse.
