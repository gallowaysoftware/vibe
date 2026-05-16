# TODO

## Overnight progress (2026-05-15 → 16)

Five batches of four parallel agent-built features landed since the
user went to bed. Headline additions:

**vibe**: `doctor` (+ `--install comfyui|llama-cpp`), `tui`,
`profile init`, `frontend.kind: managed`, `backend.comfyui` as a
first-class backend supervised alongside llama-server, VRAM
pre-flight check (`--no-vram-check` bypass), HF download via the
`hf` CLI for gated repos, shell completion (bash/zsh/fish) with
dynamic profile-name suggestions, GitHub Actions CI gating
build / vet / `-race` / gofmt / mod tidy.

**vamp**: stage types `text`, `comfyui`, `audio` (Piper), `ffmpeg`,
`youtube`, `webhook` (Slack/Discord/Mattermost); DAG executor with
parallel waves + parallel foreach (configurable cap); ComfyUI
WebSocket progress (stdlib RFC 6455, polling fallback); video/gif
output handling; per-stage retry with exponential backoff;
`--resume <dir>` with snapshot-drift detection + `--resume-force`;
`--dry-run`; `runs ls/show/cleanup`; `viz` (Mermaid `flowchart TD`);
per-pipeline timing report + `pipeline_timing.json`; SSE streaming
for live tokens.

**Architecture**: Connect/protobuf control plane (unix socket + TCP);
`internal/vibeclient/` typed SDK; `internal/comfyui/` typed REST + WS
client; slog JSON daemon logs; profile schema migrated to
`backend.{llama_server,comfyui}` discriminated union.

~376 test functions across 104 Go files; CI green on main. Two
end-to-end smokes ran against the live GPU during the session: the
multi-stage Qwen3.6-27B coding pipeline (~2m) and the cross-backend
SDXL-Turbo image-batch pipeline (~40s) — both produced real artifacts.

## ~~Installation path~~ (done)

`vibe doctor` is the entry point. It verifies binaries (`llama-server`,
`hf`), HuggingFace auth state, XDG dirs, ports `:9000` / `:9001`, profile
validity, MCP definitions, daemon reachability, and GPU presence. Each
check prints OK / WARN / FAIL; the command exits non-zero only on FAIL.

The "drop an example profile" affordance from the original list is now
satisfied by `vibe profile init <kind> [--name <name>]`, which writes a
starter YAML (with `# REPLACE: ...` markers for the fields the user must
edit) to `$XDG_CONFIG_HOME/vibe/profiles/`. See
`internal/vibe/cli/cmd_profile.go`.

~~Still open from the original list: a one-shot binary install script
(`curl ... | sh` or `go install`).~~ Done — see `install.sh` (POSIX
shell, OS/arch detection, idempotent, `--dry-run`), `.goreleaser.yaml`,
and `.github/workflows/release.yaml`. The next operator step is to tag
the first `v*` release; the workflow takes care of the rest.

## ~~MCP composition for profiles~~ (done)

Implemented: one YAML file per MCP under `$XDG_CONFIG_HOME/vibe/mcp/`,
referenced from a profile via `frontend.mcps: [...]`. See
`profiles/mcp.example.yaml` and the Architecture section of the README.

## ~~docker-compose frontends~~ (done)

Implemented: `frontend.kind: docker-compose` brings up a compose stack as
part of profile activation and tears it down on `vibe stop`. See
`profiles/docker-compose.example.yaml` and the Architecture section of
the README.

## ~~Managed (native-binary) frontends~~ (done)

Implemented: `frontend.kind: managed` execs a native binary directly,
captures its PID, polls any configured `wait_for` URLs, and stops it on
`vibe stop` with the same SIGINT-then-SIGKILL contract the backend
supervisor uses for llama-server. See `profiles/managed.example.yaml`
and `internal/vibe/frontend/managed.go`.

## ~~`vibe doctor --install comfyui`~~ (done)

Implemented: `vibe doctor --install comfyui [--yes]` walks the install
path (git clone, venv, pip install, optional SDXL-Turbo checkpoint, drop
default profile) idempotently — each step skips when already satisfied.
See `internal/vibe/cli/install_comfyui.go`.

## ~~`vibe doctor --install llama-cpp`~~ (done)

Implemented: `vibe doctor --install llama-cpp [--yes] [--cuda]` offers
three install methods (distro package metadata probe, GitHub release
tarball, or printed source-build commands), falling through to the
tarball path whenever a distro package isn't available. Each step is
idempotent; the release path picks `llama-<ver>-bin-ubuntu-x64.tar.gz`
(or a CUDA variant when one exists upstream and `--cuda` is set),
extracts under `~/.local/share/vibe/llama-cpp/`, and symlinks into
`~/.local/bin/`. See `internal/vibe/cli/install_llama_cpp.go`.

## ~~ComfyUI video smoke~~ (done)

LTX-Video 2B (0.9.8 distilled FP8) ran end-to-end on the live machine
via `vamp run examples/comfyui-video/pipeline.yaml` and produced a
valid 2.04s 512x320 H.264 MP4 in 7.3s. The video/gif output collection
path is verified against a real workflow, not just unit tests.

## ~~End-to-end content-mill smoke~~ (done)

`examples/content-mill/pipeline.yaml` chains every stage type together:
text → text(JSON) → ComfyUI image → Piper TTS → ffmpeg muxing → webhook
(Slack/Discord/Mattermost). Verified against the live machine; produces
a 9.31s H.264+AAC MP4 and POSTs the run details to the configured
webhook URL.

## Bugs found during the morning smokes

- **vamp ffmpeg executor: tail-ring buffer returns short writes.**
  When ffmpeg's default verbose output exceeds the executor's stderr
  tail-ring buffer capacity, the Write() call returns a short count,
  which ffmpeg surfaces as a non-zero exit "short write" error even
  though the output file is valid. Workaround in the example pipeline
  is `-hide_banner -loglevel error`; real fix is to make the tail-ring
  always consume the full Write (drop excess, never return short).
- **{{ .stages.X.output }} returns paths relative to RunDir.** Stages
  invoked as subprocesses (audio/ffmpeg) run from the daemon's CWD, so
  the relative path doesn't resolve. Workaround: prefix templates with
  `{{ .runDir }}/`. Future polish: pre-resolve stage outputs to
  absolute paths before exposing them in the template namespace.
- **foreach with a single item still requires a `{{...}}`-templated
  output path.** Cosmetic; the safety check for cross-item path
  collisions could short-circuit when the array length is 1.

## Larger nice-to-haves still on the radar

- VRAM-aware *scheduling* (today's pre-flight rejects when free VRAM is
  insufficient for the chosen profile; smarter scheduling could pick a
  smaller capable profile that fits the budget).
- vamp daemon mode: background runs with job IDs, `vamp logs <id>` to
  follow live, `vamp jobs ls` for the queue.
- Per-foreach-item resume (today resume granularity is whole-stage; a
  failed item in a foreach causes the whole stage to rerun on resume).
- Real LAN access from a laptop: HTTP control plane is loopback-bound
  today. Needs auth (the deferred token-based-auth decision from earlier).
- Multi-GPU scheduling: single-profile-at-a-time invariant assumes one
  GPU.
- ~~vamp pipeline JSON-schema export for editor IDE support
  (yaml-language-server etc.).~~ Done — see `vamp schema` and the
  README's "Editor schema" section. Schema is generated by
  `internal/vamp/schema.go` (draft-07, stdlib only) and round-trip-
  validated against every example pipeline in `internal/vamp/schema_test.go`.
- ~~Webhook-on-failure: today the webhook stage runs as a normal pipeline
  stage; firing it from a `defer` on pipeline failure (so users get a
  notification even when stage 3 of 5 explodes) is a separate feature.~~
  Done — see the `run_when: success|failure|always` stage qualifier
  (`internal/vamp/pipeline.go` + scheduler changes in `exec.go`).
  Failure/always stages get `{{ .pipeline_status }}` and
  `{{ .failure_summary }}` template bindings.
