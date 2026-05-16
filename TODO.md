# TODO

## Open

- **Multi-GPU scheduling.** The current single-profile-at-a-time
  invariant assumes one GPU; supporting two or more cards needs (a) a
  GPU index in the profile schema, (b) per-GPU free-VRAM accounting in
  the pre-flight check, and (c) a vamp scheduler that can run two
  capabilities concurrently when they land on different cards.

## Recently shipped

- **Content-addressed cache** (`feat/content-cache`). Per-stage
  cache keys derived from rendered prompt/params/model (text),
  post-substitution workflow JSON (comfyui), rendered text + voice
  model size (audio), and rendered argv + per-input-file sha256
  (ffmpeg). Entries land under `$XDG_CACHE_HOME/vamp/sha256/<2>/<hash>/`
  with `meta.json`; foreach caches per item. Opt-out via
  pipeline-level `cache: false`, per-stage `cache: false`,
  `--no-cache`, or `VAMP_NO_CACHE=1`. Webhook + YouTube never cached.
  CLI surface: `vamp cache ls/size/prune/clean`.

- **Template-expression `run_when` + `type: confirm` stage** (2026-05-16).
  Stages can now gate on rendered template booleans
  (`run_when: '{{ contains .stages.cover.output "rainy" }}'`) on top of
  the existing success/failure/always keyword qualifier; the keyword
  gate runs first (template form is implicit `success`), then the
  template renders and must evaluate to one of true/yes/1 or
  false/no/0/"" (anything else is a pipeline error). The companion
  `type: confirm` stage blocks until an operator approves: TTY mode
  prompts on stdin, `--detach` mode writes a `<stage-id>.pending` marker
  cleared with `vamp confirm <run-dir> <stage-id> [--reject]`. Optional
  `timeout:` rejects automatically. Rejection is a sentinel error so
  downstream `run_when: failure`/`always` stages fire as expected.

## History (phases 1 + 2, done)

The bulk of the original phase-1 + phase-2 plan has shipped over the
overnight runs of 2026-05-15 → 16 (five batches of four parallel agents):

**vibe.** `doctor` (+ `--install comfyui|llama-cpp`), `tui`,
`profile init`, `frontend.kind: managed`, `backend.comfyui` as a
first-class backend supervised alongside llama-server, VRAM pre-flight
check with `--no-vram-check` bypass, HF download via the `hf` CLI for
gated repos, shell completion (bash/zsh/fish) with dynamic profile-name
suggestions, install script + `.goreleaser.yaml` + GitHub Actions CI
gating build / vet / `-race` / gofmt / mod tidy.

**vamp.** Stage types `text`, `comfyui`, `audio` (Piper), `ffmpeg`,
`youtube`, `webhook` (Slack/Discord/Mattermost); DAG executor with
parallel waves + parallel foreach (configurable cap); ComfyUI WebSocket
progress (stdlib RFC 6455, polling fallback); video/gif output
handling; per-stage retry with exponential backoff; `--resume <dir>`
with snapshot-drift detection + `--resume-force`; per-foreach-item
resume granularity; `--dry-run`; `--detach` with `vamp jobs ls/show/cancel`
and `vamp logs <id> [-f]`; `runs ls/show/cleanup`; `viz` (Mermaid
`flowchart TD`); `schema` (JSON Schema draft-07) for editor support;
per-pipeline timing report + `pipeline_timing.json`; SSE streaming for
live tokens; VRAM-aware capability fallback (ordered `candidates:`
list); `run_when: success|failure|always` stage qualifier with
`pipeline_status` + `failure_summary` template bindings.

**Architecture.** Connect/protobuf control plane (unix socket + TCP
with bearer-token auth on TCP); `internal/vibeclient/` typed SDK;
`internal/comfyui/` typed REST + WS client; slog JSON daemon logs;
profile schema migrated to `backend.{llama_server,comfyui}` discriminated
union.

**Smokes.** Two end-to-end runs against the live GPU validated the
machinery in addition to the unit tests: the multi-stage Qwen3.6-27B
coding pipeline (~2m) and the cross-backend SDXL-Turbo image-batch
pipeline (~40s). The LTX-Video 2B (0.9.8 distilled FP8) example
produced a valid 2.04s 512x320 H.264 MP4 in 7.3s, and the full
`examples/content-mill/pipeline.yaml` chained every stage type into a
9.31s H.264+AAC MP4 plus a webhook POST.

**Bugs fixed during the morning smokes.** Three issues surfaced during
the live runs and have all landed: the ffmpeg executor's tail-ring
buffer now consumes the full `Write()` instead of returning short
counts; `{{ .stages.X.output }}` references resolve to absolute paths
so subprocess stages no longer need the `{{ .runDir }}/` prefix
workaround; and a single-item foreach no longer requires a
`{{...}}`-templated output path.
