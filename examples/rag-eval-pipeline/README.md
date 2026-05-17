# rag-eval — Tier 3 RAG quality pipeline

A vamp pipeline that exercises the [`rag`](../profiles/rag-with-qdrant/)
profile's retrieval stack end-to-end and produces a quality report.
Demonstrates "RAG as a pipeline" — treating retrieval quality as
something you measure and iterate on, not a config you set once and
hope for the best.

## What it does

For each query in an inline test suite:

1. **Embed** the query with TEI (`POST /v1/embeddings`).
2. **Retrieve** the top-5 chunks from Qdrant (`POST
   /collections/<name>/points/search`).
3. **Judge** retrieval quality with the chat LLM: scores
   precision-at-5, flags whether the top chunk is on-topic, comments
   on what went wrong.

Then a final stage **aggregates** all per-query judgements into a
single Markdown report (mean precision, failure patterns, suggested
adjustments).

## Prerequisites

1. **`vibe start rag` is running.** TEI on `:14002`, Qdrant on
   `:6333`, chat LLM on `:9000`. See the [`rag-with-qdrant`
   profile](../profiles/rag-with-qdrant/) for setup.
2. **A populated Qdrant collection.** Upload some PDFs / Markdown /
   text to Open WebUI's Knowledge UI (Workspace → Knowledge → New).
   The collection name shows up under Workspace; you'll pass it as a
   pipeline input.
3. **`vamp` is in `$PATH`** (installed by `vibe`'s install script or
   `go install ./cmd/vamp`).

## Run

```sh
vamp validate pipeline.yaml                            # always works
vamp run pipeline.yaml --input collection=<name>       # live run
```

**Note**: `vamp run --dry-run` doesn't traverse this pipeline cleanly.
The `judge` and `report` stages use `readFile` to consume upstream
webhook responses, but dry-run doesn't actually execute the
upstream stages so the response files don't exist. Use `validate`
for static checks; the live `run` works as expected.

Outputs land in `$XDG_STATE_HOME/vamp/runs/<timestamp>_rag-eval/`:

```
queries.json         # the test suite
embed_0.json         # TEI's embedding response for query 0
embed_1.json
embed_2.json
retrieve_0.json      # Qdrant's top-5 for query 0
retrieve_1.json
retrieve_2.json
judge_0.json         # per-query quality scores
judge_1.json
judge_2.json
report.md            # final aggregate report
```

Open `report.md` for the verdict.

## What's in here

| File | Role |
|---|---|
| `pipeline.yaml` | Five-stage DAG: queries → embed → retrieve → judge → report. |
| `body_templates/qdrant_search.json.tmpl` | Request body for Qdrant's search endpoint — uses `readFile`/`parseJSON`/`toJSON` to inline the embedding vector as a real JSON array. |
| `prompts/judge.tmpl` | Per-query judge prompt — asks the LLM to score retrieval. |
| `prompts/report.tmpl` | Aggregate-report prompt — combines per-query judgements into Markdown. |

## Editing the query suite

The current test queries live inline in the `queries` stage's prompt.
Replace them with your own — anything you'd actually ask the system —
to make the eval reflect your real use case. Keep the format as a flat
JSON array of strings; the foreach iterates each string under the
`{{.query}}` template variable.

For larger suites, swap the inline prompt for `prompt_file:
prompts/queries.tmpl` and put the list in its own file.

## What this pattern unlocks

The five-stage flow here is a regression test for retrieval quality
that you can run after any change:

- **Adjusted chunk size?** Re-run, compare the new `report.md` to the
  old one — if mean precision dropped, revert the chunk-size change.
- **Swapped embedding model?** Same loop — eval before / after,
  decide based on the report.
- **Updated the knowledge collection?** Re-run to make sure new
  content didn't degrade retrieval on existing queries.

The `judge`-with-LLM step is the interesting bit: it gives you
quality scores without manual labelling, at the cost of being only as
accurate as the judge model. For high-stakes evaluation, run the
pipeline against a held-out human-labelled set and compare the LLM's
scores to the human ones to calibrate.

## Going further

- **Hyperparameter sweep**: wrap this pipeline in a shell loop that
  varies `CHUNK_SIZE` / `RAG_TOP_K` in the rag profile's compose env,
  re-runs the pipeline, and diffs reports. `vamp diff <run-a>
  <run-b>` makes the comparison cheap.
- **Reranker on/off**: turn off `ENABLE_RAG_HYBRID_SEARCH` in the
  compose env, re-run, compare. Captures the reranker's actual lift
  on your corpus.
- **Multiple embedding models**: swap TEI's `--model-id` between
  runs (BGE-M3 vs Qwen3-Embedding vs nomic-embed-text). Same eval
  shape, different answers.
