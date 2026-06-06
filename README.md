# vibe

![CI](https://github.com/gallowaysoftware/vibe/actions/workflows/ci.yml/badge.svg)

`vibe` is a task launcher for local AI: a profile bundles a model with a
frontend (llama-server + opencode, llama-server + Open WebUI, raw ComfyUI,
...) and one command brings the whole task up. `vamp` is a pipeline
orchestrator built on top of it — pipelines chain stages across model
swaps and non-LLM backends (ComfyUI image/video, Piper TTS, ffmpeg,
webhooks, YouTube upload) into a single DAG with parallel waves,
per-foreach-item resume, and try/finally semantics.

## Quick install

```
curl -sSL https://raw.githubusercontent.com/gallowaysoftware/vibe/main/install.sh | sh
```

Drops `vibe` and `vamp` in `~/.local/bin/` and runs `vibe doctor`. Pin a
version with `VIBE_VERSION=v1.2.3`, redirect with `INSTALL_DIR=...`, or
pass `--dry-run` to preview. To build from source: `go install
./cmd/vibe ./cmd/vamp` from a checkout of this repo.

## Hello world

```
vibe doctor                              # verify the machine is set up
vibe profile new code --kind llama-server # drop a starter profile
# edit ~/.config/vibe/profiles/code.yaml: fill in the REPLACE-marked lines
vibe start code                          # llama-server + opencode wired up
```

The profile is written to `$XDG_CONFIG_HOME/vibe/profiles/code.yaml` with
`REPLACE-...` markers on fields you must fill in (model path, alias).
`vibe start` refuses to launch while any `REPLACE-` placeholder remains,
then validates, runs the pre-flight VRAM check, launches both backend and
frontend, and exits with the env vars to set in the calling shell.

## How it works

**Profiles.** A YAML file under `$XDG_CONFIG_HOME/vibe/profiles/` bundling
a backend spec, an optional frontend integration, and template variables
that wire them together. `vibe profile new <name> --kind <kind>` drops a
starter; run it with `--help` for the full kind list (`llama-server`,
`comfyui`, `docker-compose`, `managed`, `tabby-api`, `http-service`,
`llama-embed-service`). Pass `--hf <repo>[:<file>]` on the `llama-server`
kind to inject a `huggingface:` block so `vibe pull` can fetch the weights.

**Backends.** A discriminated union under `backend:` — exactly one
sub-block must be set:

- `llama_server` — supervises [`llama-server`](https://github.com/ggml-org/llama.cpp)
  for an OpenAI-compatible chat/completion API. Set
  `backend.llama_server.mmproj` (and optionally
  `huggingface.mmproj_file`) to enable image input on vision-capable
  models (Gemma 3, Qwen2.5-VL, LLaVA, etc.). See the
  `examples/profiles/chat-with-search/` profile for a full multimodal
  example.
- `comfyui` — supervises a [ComfyUI](https://github.com/comfyanonymous/ComfyUI)
  python process for image/video generation. ComfyUI ships its own UI,
  so these profiles carry no `frontend:` block.
- `http_server` — wraps any HTTP-serving inference engine vibe doesn't
  have a first-class backend for: TTS daemons, embedding servers,
  third-party inference. Two modes (mutually exclusive): docker
  (`image:` + optional `volumes`, `gpu`, `container_port`) or bare
  binary (`binary:`). Vibe synthesizes the launch invocation, polls
  `health_path` until ready, and proxies traffic through `:9000`.
  Used today for Kokoro-FastAPI TTS behind a vamp `capability: tts`.
  No `frontend:` block — the HTTP server is the deliverable.
- `tabby_api` — supervises [tabbyAPI](https://github.com/theroyallab/tabbyAPI)
  for EXL3/EXL2 models on NVIDIA, giving an OpenAI-compatible API for
  ExLlamaV3 quants. Ships a `vibe_defaults` sampler preset (`min_p`,
  `repetition_penalty`) so EXL3 backends don't degenerate into repetition
  loops on stages that set only `temperature` + `max_tokens`. See
  `vibe profile new <name> --kind tabby-api`.

Service-mode profiles (`mode: service`) run as concurrent sidecars
addressed by name rather than as the single "active" profile — used for
embedding servers, TTS engines, and SearXNG that a pipeline depends on
while a different model holds the foreground.

**Auto-respawn.** The supervisor watches each backend after it
reaches ready; an unexpected mid-life exit (e.g. a flaky CUDA kernel
SIGABRT'ing under load) triggers a same-port re-launch with the same
spec. Budget: 60 respawns per 30 min before the daemon clears
`d.active` and lets the next `EnsureActive` start fresh. Tune in
`internal/vibe/daemon/daemon.go:maxBackendRespawns` if you regularly
hit it on stable hardware.

**Frontends.** Only applicable to `backend.llama_server` profiles:

- `external` — vibe renders a sidecar config (e.g. `opencode.json`) and
  surfaces the env vars to set when launching the tool. No process
  lifecycle.
- `docker-compose` — `docker compose up -d` against a user-supplied
  compose file on `vibe start`, `down` on `vibe stop`. Polls any
  `wait_for` health endpoints. Good fit for Perplexica, Open WebUI.
- `managed` — execs a native binary with the configured args/env/workdir,
  polls `wait_for` URLs, stops it with SIGINT → SIGKILL (10s grace).

**MCP composition.** One YAML file per MCP server under
`$XDG_CONFIG_HOME/vibe/mcp/` (`datadog.yaml`, `jira.yaml`, ...). Profiles
compose them by name: `frontend.mcps: [datadog, jira]`. Secrets stay in
env vars (`${env:...}` references in the MCP file); profiles never name
them inline.

**Proxy and control plane.** The daemon reverse-proxies frontends to the
active llama-server on `:9000` so swapping models doesn't require
reconfiguring the frontend. The Connect/protobuf control plane listens
on `$XDG_RUNTIME_DIR/vibe/vibe.sock` (0600) and, optionally,
`127.0.0.1:9001`.

**VRAM check.** `vibe start` runs a pre-flight check against the
profile's `estimated_vram_gb` and refuses to launch when free VRAM is
short. Pass `--no-vram-check` to bypass; pair with `vamp`'s capability
fallback (below) for graceful degradation.

## vamp pipelines

A vamp pipeline is a YAML file declaring stages and their data
dependencies. The executor builds a DAG, runs independent stages in
parallel waves, and resolves each stage's `capability:` to a vibe profile
on the fly. A foreach stage forks one task per item in a JSON array and
runs them in parallel up to a configurable cap. Per-stage retry with
exponential backoff handles transient errors; `run_when:
success|failure|always` gives you try/finally semantics for cleanup and
failure-path notifications, and a template-form `run_when:
'{{ contains .stages.cover.output "rainy" }}'` makes stages conditional
on upstream content (renders to one of true/yes/1 or false/no/0/"" —
anything else is a pipeline error).

| Stage type | What it does |
| --- | --- |
| `text`    | LLM chat completion. `output_format: json` validates the model's output. SSE-streamed when the frontend asks for it. Multimodal via `image_dir:` (scan a directory) or `image_files:` (explicit list, one image per foreach item). |
| `render`  | Pure template render — no LLM, no profile activation. For enumerating directories, building JSON arrays, transforming prior outputs. |
| `compact` | LLM-summarised, length-targeted compaction. Chunks the source, summarises each, concatenates; recurses if still over target. |
| `comfyui` | Runs a ComfyUI workflow JSON. WebSocket progress (RFC 6455, polling fallback); collects images, videos, and gifs. `free_memory_after: true` issues POST /free after a successful workflow so SDXL/Flux weights don't squat in VRAM between runs. |
| `audio`   | TTS. `engine: piper` runs Piper on a voice ONNX from `~/.local/share/piper-voices/`; `engine: kokoro` POSTs to a Kokoro-FastAPI server (declare `capability: tts` to let vibe manage its lifecycle). |
| `ffmpeg`  | Shells out to `ffmpeg` with templated args; tail-rings stderr for diagnostics. Three modes: explicit `ffmpeg_args:`, auto `concat_wavs:`, or M4B chapterised audiobook via `m4b_from:` / `m4b_file:` / `m4b_chapter:` (+ optional `cover_image:`). |
| `pandoc`  | Document conversion via pandoc (docker `pandoc/core` by default, override with `binary:`). Used for markdown → EPUB study guides with `cover_image:`. |
| `youtube` | Uploads a finished video via the YouTube Data API (OAuth token cache under XDG). |
| `webhook` | Slack/Discord/Mattermost-style JSON POST. Honors `run_when: failure` so a failed pipeline still pings. |
| `confirm` | Human-in-the-loop gate: prompts on stdin (TTY) or writes a marker file the operator clears with `vamp confirm <id-or-prefix> <stage-id> [--reject]` (detach mode). Optional `timeout: 30m` auto-rejects. |

**Resumes.** `vamp run --resume <run-dir>` re-uses outputs already on
disk, including per-foreach-item granularity (a failed item in an
otherwise-successful foreach stage re-runs only that item). Snapshot
drift aborts unless `--resume-force` is set.

**Content-addressed cache.** Cacheable stages (`text`, `comfyui`,
`audio`, `ffmpeg`, `render`, `compact`, `pandoc`) hash their full
input — prompt/params/model for text, post-substitution workflow
JSON for ComfyUI, rendered text + voice-model size for audio,
rendered argv + per-input-file sha256 for ffmpeg, full template
output for render, source + target length for compact, source bytes
+ pandoc flags for pandoc — and store outputs under
`$XDG_CACHE_HOME/vamp/sha256/<2>/<full>`.
Across runs, unchanged stages short-circuit to a cache hit; a single
tweaked prompt reruns only that stage (plus everything downstream).
Foreach is per-item: changing 3 items of a 50-item foreach reuses 47.
Disable with `cache: false` on the pipeline or stage, `--no-cache`, or
`VAMP_NO_CACHE=1`. `webhook` and `youtube` are never cached (network
side effects). Inspect with `vamp cache {ls,size,prune,clean}`.

**Detach.** `vamp run --detach` forks a setsid'd worker, writes
`vamp.pid` + `vamp.log` into the run dir, and returns the run id (plus
copy-pasteable `follow` / `cancel` hints). Drive it with `vamp runs
ls/show/cancel` and `vamp logs <id> [-f]` — a detached run shows up as
`running` in the `STATE` column of `vamp runs ls`.

**Capability fallback.** `$XDG_CONFIG_HOME/vamp/capabilities.yaml` maps
each capability to a profile, or to an ordered list of candidates
(biggest first). When the daemon's VRAM precondition fails, vamp falls
back to the next candidate and logs the skip; other errors still abort.

```yaml
capabilities:
  reasoning:
    candidates: [code, code_small]   # tries code first, falls back
  creative_writing: chat              # shorthand still works
```

## CLI reference

`vibe`:

| Command | Purpose |
| --- | --- |
| `vibe doctor` | One-shot diagnostic; `--install comfyui\|llama-cpp` runs the bring-up steps. |
| `vibe profile new <name> --kind <kind>` | Drop a starter YAML (run with `--help` for the kind list); `--hf <repo>[:<file>]` for gated llama-server. |
| `vibe start <profile>` | Activate backend + frontend; pulls missing weights; `--no-vram-check` to bypass. Service-mode profiles run concurrently with each other and one active profile. |
| `vibe stop [name]` | Stop the active profile (no arg), a specific service (`vibe stop searxng`), or everything (`vibe stop --all`). |
| `vibe ps` | Show the active profile + every running service. |
| `vibe list` | List profiles (alias of `vibe profile list`; no daemon needed). |
| `vibe logs [name]` | Tail backend logs from the active profile (no arg) or a named service. |
| `vibe tui` | Bubbletea dashboard (start/stop, status, logs). |
| `vibe token` | Print the bearer token; `--regenerate` rotates. |
| `vibe env` | Print env vars for the active profile's frontend. |
| `vibe pull <profile>` | Fetch the HF weights for a profile (auto-invoked by `start`). |
| `vibe shutdown` | Stop the daemon. |
| `vibe daemon` | Run the daemon in the foreground (normally auto-spawned). |

`vamp`:

| Command | Purpose |
| --- | --- |
| `vamp run <pipeline.yaml>` | Execute. Flags: `--detach`, `--resume <dir>`, `--resume-force`, `--dry-run`, `--no-cache`, `--no-ensure-services`, `--input k=v`. By default each `RequireService` URL is probed pre-run and auto-started via `vibe start <name>` when the setup hint matches that shape. |
| `vamp validate <pipeline.yaml>` | Parse + schema-check without running. |
| `vamp lint <pipeline.yaml>` | Advisory checks layered on validate: webhook URL → matching `RequireService`, `output_format: json` → `Retry.RetryOn` includes `"invalid_output"`. Findings only — exit 0. |
| `vamp list` | List pipelines under `$XDG_CONFIG_HOME/vamp/pipelines/`. |
| `vamp capabilities` | Print the resolved capability table. |
| `vamp runs ls/show/cancel/cleanup` | One noun for everything a run leaves behind: history + live detached jobs. `ls` has a `STATE` column (running/finished/crashed); `show` reports live pid/state; `cancel` SIGTERMs a running detached run. (`vamp jobs` is a hidden deprecated alias.) |
| `vamp diff <run-a> <run-b>` | Side-by-side comparison of two runs: pipeline YAML, inputs, per-stage prompt / output / status / duration. `--json` for a machine-readable shape, `--stage <id>` to narrow, `--no-content` for metadata only. Honors `NO_COLOR`. |
| `vamp logs <id> [-f]` | Cat or follow a run's worker log (id-or-prefix). |
| `vamp cancel <id>` | SIGTERM a detached worker (alias of `runs cancel`). |
| `vamp viz <pipeline.yaml>` | Mermaid `flowchart TD` of the DAG; `--show-inputs` for the input block. |
| `vamp schema` | Emit the pipeline JSON Schema (draft-07); `--out <file>` to write. |
| `vamp cache ls/size/prune/clean/info` | Inspect and manage the content-addressed cache. `info <run-dir>` reports per-stage cache hit/miss for one run. |

Pipeline binaries built with `vamp.BuildRoot` automatically inherit
the vamp subcommands above plus two pipeline-aware lifecycle helpers:

| Command | Purpose |
| --- | --- |
| `<pipeline> activate` | Read the pipeline's `RequireProfile` + `RequireService` declarations and bring up every required profile via vibe; health-check each declared URL. `--skip-active` for sidecars-only. |
| `<pipeline> doctor` | Read-only — report which required profiles are up, which are missing, with each service's setup hint inline. Exits non-zero so CI can use it as a gate. |

## Remote access

By default the TCP control plane binds `127.0.0.1:9001` and the unix
socket is the preferred local path. To drive a daemon from another
machine on your LAN:

1. On the dev box, edit `~/.config/vibe/config.yaml`:

   ```yaml
   bind_all: true
   ```

   (or `http_addr: "0.0.0.0:9001"`). Then `vibe shutdown` so the next
   command spawns a daemon with the new bind.

2. On the dev box, `vibe token` prints
   `$XDG_STATE_HOME/vibe/token` (mode 0600, generated on first daemon
   start). Copy it to your laptop.

3. On the laptop:

   ```
   export VIBE_TOKEN=<paste>
   export VIBE_API=http://devbox.local:9001
   vibe ps
   ```

`vibeclient` reads `$VIBE_TOKEN` first, falls back to
`$XDG_STATE_HOME/vibe/token`. Requests over the unix socket are never
authed (gated by filesystem perms instead). If the token leaks, run
`vibe token --regenerate` on the dev box, restart the daemon, re-copy.

## Examples

`vibe` profile starters under `examples/profiles/`:

| Profile | What it shows |
| --- | --- |
| [`chat-with-search`](examples/profiles/chat-with-search/) | Local LLM + Open WebUI + SearXNG sidecar for web search + Tier-1 RAG (BGE-M3 + reranker + hybrid). Copy-and-adapt. |
| [`rag-with-qdrant`](examples/profiles/rag-with-qdrant/) | Tier-2 RAG: local LLM + Open WebUI + TEI (BGE-M3) + Qdrant. Dedicated embedding service + observable vector store. |

`vamp` pipelines under `examples/`:

| Pipeline | What it shows |
| --- | --- |
| [`multi-profile-pipeline`](examples/multi-profile-pipeline/) | Two-stage demo: small profile drafts, larger profile expands. |
| [`comfyui-image-batch`](examples/comfyui-image-batch/) | SDXL-Turbo image batch via the ComfyUI WS client. |
| [`comfyui-video`](examples/comfyui-video/) | LTX-Video 2B end-to-end with video output collection. |
| [`video-pipeline`](examples/video-pipeline/) | Cross-backend image-to-video chain. |
| [`voiceover-pipeline`](examples/voiceover-pipeline/) | Piper TTS + ffmpeg over a still image. |
| [`notify-pipeline`](examples/notify-pipeline/) | Minimal webhook stage demo. |
| [`youtube-upload`](examples/youtube-upload/) | OAuth-driven YouTube Data API upload. |
| [`content-mill`](examples/content-mill/) | Every stage type stitched end-to-end with a failure-path webhook. |
| [`rag-eval-pipeline`](examples/rag-eval-pipeline/) | Tier-3 RAG: embed query suite via TEI, retrieve from Qdrant, judge quality with the LLM, aggregate report. Demonstrates `readFile`/`parseJSON`/`toJSON` chaining. |

## Editor support

`vamp schema` emits a JSON Schema (draft-07) document for `pipeline.yaml`
so editors with the `yaml-language-server` integration (VS Code's RedHat
YAML extension, Helix, vim's coc, IntelliJ) provide validation and
autocomplete:

```
vamp schema --out vamp.schema.json
```

Point editors at the rendered file with a directive at the top of your
pipeline YAML:

```yaml
# yaml-language-server: $schema=./vamp.schema.json
name: my-pipeline
stages:
  - ...
```

`examples/multi-profile-pipeline/` ships a checked-in `vamp.schema.json`
alongside its `pipeline.yaml` to show the wiring.

Both `vibe` and `vamp` ship Cobra-generated shell completion with
dynamic completers (`vibe start <TAB>` lists profiles; `vamp run <TAB>`
lists pipelines). Install once per shell:

```
vibe completion bash > /etc/bash_completion.d/vibe        # bash
vibe completion zsh  > "${fpath[1]}/_vibe"                # zsh
vibe completion fish > ~/.config/fish/completions/vibe.fish # fish
```

Same flags work on `vamp completion`.

## Status

Phase 1 and most of phase 2 have shipped: profile schema, llama-server
and ComfyUI supervision, proxy, full vamp DAG executor with detach +
per-item resume + `run_when` qualifiers, JSON-Schema editor wiring,
LAN access with bearer-token auth. See [`TODO.md`](TODO.md) for what's
open — the headline item still on the radar is multi-GPU scheduling
(today's single-profile-at-a-time invariant assumes one GPU).

## Contributing

PRs welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the mechanical
setup and [AGENTS.md](AGENTS.md) for the project conventions.

## License

MIT — see [LICENSE](LICENSE).
