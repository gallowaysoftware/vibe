# rag-eval (Go DSL)

The Go-DSL port of [`examples/rag-eval-pipeline/`](../rag-eval-pipeline/).
Same DAG, same templates, same outputs — built with the
`github.com/gallowaysoftware/vibe/vamp` package's fluent builders
instead of YAML. Prompts and body templates ship inside the binary via
`embed.FS`, so the whole pipeline can be distributed as a single
self-contained Go binary or a git-cloneable Go module.

## What's in here

| File | Role |
|---|---|
| `main.go` | One-liner: `func main() { vamp.Main(Pipeline) }`. |
| `pipeline.go` | Fluent DSL construction — five stages, identical DAG to the YAML version. |
| `prompts/judge.tmpl`, `prompts/report.tmpl` | Embedded via `//go:embed prompts/*.tmpl`. |
| `body_templates/qdrant_search.json.tmpl` | Embedded via the same directive. |

## Run

```sh
go run . validate                                    # static check
go run . viz                                         # Mermaid DAG to stdout
go run . run --input collection=<qdrant-collection>  # live run
```

Or build a binary and ship it:

```sh
go build -o rag-eval .
./rag-eval validate
./rag-eval run --input collection=docs
```

`go run . --help` lists every subcommand the standalone `vamp` binary
exposes (run / validate / viz / render / list / logs / cache / jobs /
runs / capabilities / schema / confirm / diff / cancel) — the
pipeline binary IS a `vamp` binary, just one whose pipeline is
already built in.

## Prerequisites

Same as the YAML version — `vibe start rag` running, a populated
Qdrant collection, etc. See
[`examples/rag-eval-pipeline/README.md`](../rag-eval-pipeline/README.md)
for the full setup walkthrough.

## Why a Go pipeline?

A YAML pipeline lives in one file; a Go pipeline lives in a Go
module, with the prompts and templates bundled into the binary. That
unlocks:

- **Type-safe cross-stage refs.** `judge.After(retrieve)` is a Go
  compile error if `retrieve` is misspelled or undeclared. A YAML
  pipeline has to wait for `vamp validate` to flag it.
- **Variables, helpers, comments.** Pipeline logic that would be
  awkward in YAML (per-environment URL pinning, conditional stage
  inclusion, named retry policies reused across stages) becomes
  ordinary Go code.
- **Distribution.** `go install
  github.com/<you>/<your-pipeline>@latest` lands a working binary
  next to `vamp` itself, with the prompts already inside it. No
  filesystem layout to recreate.

The YAML form is still ergonomic for short, mostly-static pipelines
(profile smoke tests, one-off render hooks) — both forms run through
the same orchestrator, so the choice is mostly authoring preference.
