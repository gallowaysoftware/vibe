# TODO

## ~~Installation path~~ (done)

`vibe doctor` is the entry point. It verifies binaries (`llama-server`,
`hf`), HuggingFace auth state, XDG dirs, ports `:9000` / `:9001`, profile
validity, MCP definitions, daemon reachability, and GPU presence. Each
check prints OK / WARN / FAIL; the command exits non-zero only on FAIL.

Still open from the original list: a one-shot binary install script
(`curl ... | sh` or `go install`) and a first-run "drop an example
profile in `~/.config/vibe/profiles/`" affordance.

## ~~MCP composition for profiles~~ (done)

Implemented: one YAML file per MCP under `$XDG_CONFIG_HOME/vibe/mcp/`,
referenced from a profile via `frontend.mcps: [...]`. See
`profiles/mcp.example.yaml` and the Architecture section of the README.

## ~~docker-compose frontends~~ (done)

Implemented: `frontend.kind: docker-compose` brings up a compose stack as
part of profile activation and tears it down on `vibe stop`. See
`profiles/docker-compose.example.yaml` and the Architecture section of
the README.

## Managed (native-binary) frontends

Still pending. Vibe currently supports two frontend kinds: `external`
(write a sidecar config, user launches the tool) and `docker-compose`
(vibe runs `docker compose up -d`/`down`). The third planned kind,
`managed`, would supervise a native binary directly — same lifecycle
coupling as docker-compose but without the container dependency. Useful
for tools that ship as a single executable and want to be started/stopped
with the model.

## ~~`vibe doctor --install comfyui`~~ (done)

Implemented: `vibe doctor --install comfyui [--yes]` walks the install
path (git clone, venv, pip install, optional SDXL-Turbo checkpoint, drop
default profile) idempotently — each step skips when already satisfied.
See `internal/vibe/cli/install_comfyui.go`.

## `vibe doctor --install llama-cpp`

Same shape for llama.cpp itself (current install assumes
`llama-server` already on `$PATH`). The dispatcher in
`internal/vibe/cli/cmd_doctor.go` (`runInstall`) is structured so a new
installer plugs in next to `comfyui` without touching the diagnostic
path.
