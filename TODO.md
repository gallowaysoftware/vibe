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

## ComfyUI video smoke

The ComfyUI client and the vamp executor now collect non-image outputs
(`videos`, `gifs`) from a workflow's `/history` response, and an example
sits at `examples/comfyui-video/`. The plumbing is covered by unit tests
against a fake ComfyUI server, but we haven't yet run the pipeline against
a real video model (LTX-Video, HunyuanVideo, Wan2.2, etc.). Open follow-up:
download a public video checkpoint, run the example end-to-end, and
confirm the MP4 lands at `<run-dir>/assets/video.mp4` with sensible
contents. May also need to add a `VHS_VideoCombine`-flavoured variant if
that turns out to be the more common community node in practice.

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
- vamp pipeline JSON-schema export for editor IDE support
  (yaml-language-server etc.).
- Webhook-on-failure: today the webhook stage runs as a normal pipeline
  stage; firing it from a `defer` on pipeline failure (so users get a
  notification even when stage 3 of 5 explodes) is a separate feature.
