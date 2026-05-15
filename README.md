# vibe

A task-oriented launcher for local AI inference. Think `docker compose` for local AI workflows: define a profile that bundles a model configuration with a frontend, and one command brings up everything for a task.

```
vibe doctor            # verify the machine is set up to run vibe
vibe start code        # llama-server + opencode wired up for coding
vibe start research    # different model, different context, different frontend
vibe ps                # what's running
vibe stop              # tear it all down
```

## First run

`vibe doctor` is the one-shot diagnostic. It checks for `llama-server` and
the HuggingFace `hf` CLI on `$PATH`, ensures the XDG config/state/runtime
directories exist and are writable, probes the control-plane (`:9001`) and
proxy (`:9000`) ports, validates every profile under
`~/.config/vibe/profiles/`, counts MCP definitions, and — best-effort —
reports `nvidia-smi` output. Each line is tagged `[ OK ]`, `[WARN]`,
`[FAIL]`, or `[INFO]`; the command exits non-zero only when something
fails. Run it before your first `vibe start` and again whenever
something behaves unexpectedly.

`vibe doctor --install comfyui` switches doctor into install mode and
runs the ComfyUI bring-up steps idempotently: clones
[ComfyUI](https://github.com/comfyanonymous/ComfyUI) to `~/ComfyUI`,
creates `.venv`, runs `pip install -r requirements.txt`, optionally
downloads the SDXL-Turbo checkpoint (~7 GB), and drops a default
`comfyui.yaml` profile. Each step skips when already satisfied; pass
`--yes` to bypass the confirmation prompts (for automation).

`vibe doctor --install llama-cpp` does the same for
[llama.cpp](https://github.com/ggerganov/llama.cpp) itself, offering
three install methods: `[d]istro` (probes the local package manager —
`pacman` on Arch, `dnf` on Fedora — and prints the `sudo` install
command rather than running it for you), `[r]elease tarball` (downloads
the latest published Linux x86_64 build from GitHub, extracts to
`~/.local/share/vibe/llama-cpp/`, and symlinks `llama-server` into
`~/.local/bin/`), or `[s]ource build` (prints the canonical
`cmake -B build -DGGML_CUDA=ON` commands — too operator-specific to
run for you). Falls through automatically from the distro path to the
tarball when the package isn't in standard repos (the Ubuntu/Debian
case in Phase 1). Pass `--yes` to skip the menu and pick the tarball
path; pass `--cuda` to prefer a CUDA-flavoured asset.

## Why

Today running local AI looks like:

1. Launch `llama-server` with the right flags (model, context, parallel, GPU layers, cache types, jinja, ...).
2. Launch the frontend separately and configure it to match.
3. Hope the frontend's context size agrees with what llama-server actually loaded.
4. Repeat all of the above whenever you switch tasks.

`vibe` collapses that into one command and a versioned YAML profile.

## Architecture

- **Profile**: the unit of configuration. A YAML file bundling a backend spec, an optional frontend integration, and template variables that wire them together.
- **Backend kinds** (discriminated union under `backend:`; exactly one block must be set):
  - `llama_server` — supervises [`llama-server`](https://github.com/ggml-org/llama.cpp) for an OpenAI-compatible chat/completion API. See [`profiles/code.example.yaml`](profiles/code.example.yaml).
  - `comfyui` — supervises a [ComfyUI](https://github.com/comfyanonymous/ComfyUI) python process for image/video generation. ComfyUI ships its own UI, so these profiles have no `frontend:` block; vibe exposes the backend addr via `Status.BackendAddr` for tools like `vamp`. See [`profiles/comfyui.example.yaml`](profiles/comfyui.example.yaml).
- **MCP definitions**: one YAML file per Model Context Protocol server, dropped into `~/.config/vibe/mcp/` (e.g. `datadog.yaml`, `jira.yaml`). Profiles compose them by listing names: `frontend.mcps: [datadog, jira]`. Vibe injects a top-level `mcp` map into the rendered frontend config. Secrets stay in env vars (the frontend resolves `${env:...}` references); profiles never name them inline.
- **Daemon**: supervises the active backend (llama-server or ComfyUI) and the active frontend. Exposes a control plane over a Unix socket.
- **Frontend kinds** (only applicable to `backend.llama_server` profiles):
  - `external` — vibe renders a sidecar config file (e.g. an `opencode.json`) and surfaces the env vars to set when launching the tool. No process lifecycle.
  - `docker-compose` — vibe runs `docker compose up -d` against a user-supplied compose file on `vibe start`, polls any `wait_for` health endpoints, and runs `docker compose down` on `vibe stop`. Good fit for heavy stacks like Perplexica or Open WebUI that benefit from being lifecycle-coupled to a profile. See [`profiles/docker-compose.example.yaml`](profiles/docker-compose.example.yaml).
- **Proxy**: reverse-proxies frontends to the active llama-server so swapping models doesn't require reconfiguring the frontend.
- **CLI**: `start`, `stop`, `ps`, `logs`, `list`.

## Status

Phase 1 in progress: profile schema, llama-server supervision, proxy, CLI, opencode integration, docker-compose frontends. Single-host, local-only.

Not yet:

- managed-binary frontends (native processes supervised by vibe)
- TUI dashboard
- VRAM enforcement
- Remote (LAN) access from a laptop

## Multi-profile example

`vamp` is a sibling tool in this repo that drives `vibe` to run a
multi-stage pipeline where each stage can use a different profile (and
therefore a different model). See
[`examples/multi-profile-pipeline/`](examples/multi-profile-pipeline/)
for a runnable two-stage demo: a small 7B profile drafts an outline,
then `vibe` swaps to a larger profile to expand it. Both
`profiles/fast.example.yaml` and `profiles/code.example.yaml` are
referenced by that example.

## License

MIT — see [LICENSE](LICENSE).
