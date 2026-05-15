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

## `vibe doctor --install` (ComfyUI etc.)

`vibe doctor` diagnoses; it doesn't install. From hands-on bring-up of
ComfyUI on this box, the install path is mechanical and worth codifying:

  1. `git clone --depth 1 https://github.com/comfyanonymous/ComfyUI ~/ComfyUI`
  2. `python3 -m venv ~/ComfyUI/.venv`
  3. `~/ComfyUI/.venv/bin/pip install -r ~/ComfyUI/requirements.txt`
     (pulls torch + CUDA libs; multi-GB; slow)
  4. Download a checkpoint into `~/ComfyUI/models/checkpoints/`. For a
     small public option:
     `hf download stabilityai/sdxl-turbo sd_xl_turbo_1.0_fp16.safetensors --local-dir ~/ComfyUI/models/checkpoints/`
  5. Drop a vibe profile in `~/.config/vibe/profiles/comfyui.yaml`
     pointing at the install (see `profiles/comfyui.example.yaml`).

A `vibe doctor --install comfyui` (or `vibe install comfyui`) command
should do steps 1–4 with confirmation prompts at each big-disk step and
pick a sensible default model. Step 5 is profile drop-in; could be a
separate `vibe profile init <kind>`.

Same shape for llama.cpp itself (current install assumes
`llama-server` already on `$PATH`).
