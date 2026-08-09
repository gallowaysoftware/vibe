# vibe

![CI](https://github.com/gallowaysoftware/vibe/actions/workflows/ci.yml/badge.svg)

`vibe` runs local AI models on the machines you own.

It started as a task launcher — a profile bundles a model with a frontend
(llama-server + opencode, llama-server + Open WebUI, raw ComfyUI, ...) and
one command brings the whole task up — and that is still the fastest way
to get a model serving on one box. It has since grown a **fleet control
plane**: a workstation, a laptop, an always-on mini-PC and a cloud API can
present as one catalog behind one URL, with models loading on demand and
boxes sleeping when nobody is using them.

`vamp` is a pipeline orchestrator built on top of it. Pipelines chain
stages across model swaps and non-LLM backends (ComfyUI image/video, Piper
TTS, ffmpeg, webhooks, YouTube upload) into a single DAG with parallel
waves, per-foreach-item resume, and try/finally semantics.

## Which of these are you?

The three deployments are a gradient, not alternatives — each adds a file
to the one before it, and you can stop at any of them.

| | You want | Read |
| --- | --- | --- |
| **1. One box** | a model plus a frontend, brought up by one command. vibe supervises `llama-server` itself. | [Hello world](#hello-world) |
| **2. One box, llama-swap** | several models on one GPU, loaded on first request and unloaded when idle. | [Serving through llama-swap](#serving-through-llama-swap) |
| **3. A fleet** | several boxes behind one URL, with sleep schedules, alarms and a catalog that follows what is actually up. | [The fleet control plane](#the-fleet-control-plane) |

Nothing in (1) is deprecated by (2) or (3). A fresh install with a plain
profile still gets vibe's own supervisor and its own proxy.

## Quick install

```
curl -sSL https://raw.githubusercontent.com/gallowaysoftware/vibe/main/install.sh | sh
```

Drops `vibe` and `vamp` in `~/.local/bin/` and runs `vibe doctor`. Pin a
version with `VIBE_VERSION=v1.2.3`, redirect with `INSTALL_DIR=...`, or
pass `--dry-run` to preview. To build from source: `go install
./cmd/vibe ./cmd/vamp` from a checkout of this repo.

The archive's sha256 is checked against the release's `checksums.txt`.
An archive it **cannot** verify is refused by default —
`VIBE_INSECURE_SKIP_CHECKSUM=1` installs anyway if you accept that. A
checksum **mismatch** is always fatal and that knob does not override
it: an unverifiable archive is unknown, a mismatched one is known to be
wrong.

## Hello world

```
vibe doctor                               # verify the machine is set up
vibe profile new code --kind llama-server # drop a starter profile
# edit ~/.config/vibe/profiles/code.yaml: fill in the REPLACE-marked lines
vibe start code                           # llama-server + opencode wired up
```

The profile is written to `$XDG_CONFIG_HOME/vibe/profiles/code.yaml` with
`REPLACE-...` markers on fields you must fill in (model path, alias).
`vibe start` refuses to launch while any `REPLACE-` placeholder remains,
then validates, runs the VRAM pre-flight, launches both backend and
frontend, and exits with the env vars to set in the calling shell.

## How it works

**Profiles.** A YAML file under `$XDG_CONFIG_HOME/vibe/profiles/` bundling
a backend spec, an optional frontend integration, and template variables
that wire them together. `vibe profile new <name> --kind <kind>` drops a
starter; the kinds are `llama-server`, `mlx-server`, `comfyui`,
`docker-compose`, `managed`, `tabby-api`, `http-service`,
`llama-embed-service` and `cloud-peer`. Pass `--hf <repo>[:<file>]` on the
`llama-server` kind to inject a `huggingface:` block so `vibe pull` can
fetch the weights. `vibe profile show <name>` loads and validates one
(it doubles as a lint), and `vibe profile schema` emits a JSON Schema for
editor completion.

**Reusable backends.** A backend (the model-server spec) can live on its own
under `$XDG_CONFIG_HOME/vibe/backends/<name>.yaml` instead of being inlined in
a profile. A profile then references it with `backend_ref: <name>` rather than
an inline `backend:` block, so one model definition (e.g. `qwen3.6-27b`) is
shared across many frontends. `vamp` capabilities resolve to a **backend name**
(activated with no frontend — the model is the deliverable), so a pipeline
depends on a model, not on a specific frontend-bearing profile. A capability
that names a profile instead still works (backward-compatible fallback).
`vibe backend list` shows every def with the profiles that reference it;
`vibe backend start <name>` activates one with no frontend at all.

These same files under `backends/` are what the fleet's router renders
from — a backend def is the one place a model is described, whether vibe
supervises it or llama-swap does.

**Backends.** A discriminated union under `backend:` — exactly one
sub-block must be set:

- `llama_server` — supervises [`llama-server`](https://github.com/ggml-org/llama.cpp)
  for an OpenAI-compatible chat/completion API. Set
  `backend.llama_server.mmproj` (and optionally
  `huggingface.mmproj_file`) to enable image input on vision-capable
  models (Gemma 3, Qwen2.5-VL, LLaVA, etc.). See the
  `examples/profiles/chat-with-search/` profile for a full multimodal
  example. For speculative decoding, set `backend.llama_server.draft_model`
  (and optionally `huggingface.draft_file`) to a draft GGUF — e.g. a
  Gemma 4 MTP assistant; vibe adds `--model-draft` + `--spec-type draft-mtp`
  + `--spec-draft-n-max` (quantized KV with draft-mtp needs a llama.cpp
  build with PR #23398; older builds need f16 KV).
  Set `backend.external: true` and vibe launches nothing — the def
  describes a model that **llama-swap** owns (see below).
- `mlx_server` — supervises `mlx_lm.server` on Apple silicon, where MLX
  is measurably faster than llama.cpp for the same weights. `mlx_lm.server`
  has neither a context flag nor an alias flag, so `context:` is
  advertised metadata only and vibe's proxy rewrites alias ↔ model dir on
  the way through.
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
  loops on stages that set only `temperature` + `max_tokens`.
- `cloud_peer` — names a remote OpenAI/Anthropic-compatible API
  (`base_url`, `api_key_env`, `models`, `formats`, `context`) so the
  router can serve a hosted model from the same catalog as the local
  ones. vibe supervises nothing; the peer's catalog ids are its
  `cloud_peer.models` entries, **never** the def name.

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

**Frontends.** Applicable to `backend.llama_server`, `backend.tabby_api`,
`backend.mlx_server` and `backend.cloud_peer` profiles (ComfyUI and
http_server reject a frontend block):

- `external` — vibe renders one or more sidecar configs (`write_file`
  or `write_files`, e.g. `opencode.json`) and surfaces the env vars to
  set when launching the tool. No process lifecycle.
- `docker-compose` — `docker compose up -d` against a user-supplied
  compose file on `vibe start`, `down` on `vibe stop`. Polls any
  `wait_for` health endpoints. Good fit for Perplexica, Open WebUI.
- `managed` — execs a native binary with the configured args/env/workdir,
  polls `wait_for` URLs, stops it with SIGINT → SIGKILL (10s grace).
  `vibe run <profile>` is the foreground form: it starts the profile,
  execs the frontend, and stops the profile when the frontend exits.

**MCP composition.** One YAML file per MCP server under
`$XDG_CONFIG_HOME/vibe/mcp/` (`datadog.yaml`, `jira.yaml`, ...). Profiles
compose them by name: `frontend.mcps: [datadog, jira]`. Secrets stay in
env vars (`${env:...}` references in the MCP file); profiles never name
them inline. Frontends that render multiple config files
(`frontend.write_files`) can list `mcps` per file — each entry's MCP
servers are merged into that rendered file the same way.

**Proxy and control plane.** The daemon reverse-proxies frontends to the
active llama-server on `:9000` so swapping models doesn't require
reconfiguring the frontend. The same `:9000` port also routes by model
id: requests whose body names the model alias of a service-mode
`llama_server` profile are forwarded to that sidecar, so one proxy port
serves multiple concurrently-loaded models. (Other service kinds don't
advertise an OpenAI model id and are addressed by their own port.)
Profiles can declare a top-level `services:` list of service-mode
sidecars co-started best-effort on `vibe start` and stopped
(best-effort) when the profile stops — manually-started services and
other profiles' sidecars are left running, docker-compose-style up/down.
The Connect/protobuf control plane listens
on `$XDG_RUNTIME_DIR/vibe/vibe.sock` (0600) and, optionally,
`127.0.0.1:9001`.

**VRAM pre-flight: warn, don't block.** `vibe start` (and `vibe run`,
and `vibe backend start`) compares the profile's `estimated_vram_gb`
against the machine. It **refuses only when the estimate exceeds the
card's total capacity** — a start that could never fit. When it merely
exceeds what is *free right now* it warns and loads anyway, because free
VRAM is not a stable number: the same profile on the same laptop read
15.2 GiB and 23.5 GiB free minutes apart, purely from page cache, and a
hard stop on that number refuses starts that would have worked.
`--no-vram-check` bypasses even the hard stop.

A caller that *has somewhere else to go* can ask for the strict form,
which refuses on the free reading too. `vamp`'s capability fallback
(below) is the one in tree: it asks strictly for every candidate that
still has a smaller one behind it — so "another model is resident" moves
to the next candidate instead of thrashing — and asks for the last
candidate the ordinary way, because there is nothing to fall back to.
Refusals carry a machine-readable reason
(`REASON_VRAM_EXCEEDS_CAPACITY` / `REASON_VRAM_INSUFFICIENT_FREE`) as a
typed error detail rather than a string clients match on.

## Serving through llama-swap

One GPU can hold one big model at a time, but a day's work wants several.
[llama-swap](https://github.com/mostlygeek/llama-swap) is a small proxy
that owns a port, starts the right `llama-server` on the first request
that names a model, and unloads it after a TTL. vibe hands `:9000` to it
and becomes the thing that *writes its config*.

```
# 1. tell vibe not to bind :9000 — ~/.config/vibe/config.yaml
disable_proxy: true

# 2. describe each model once, in ~/.config/vibe/backends/<name>.yaml
name: qwen3.6-27b
estimated_vram_gb: 26
lifecycle:
  ttl: 2h              # unload after this much idle; 0 = never
backend:
  external: true       # llama-swap owns this process, not vibe
  llama_server:
    path: ~/models/qwen3.6-27b-q5_k_xl.gguf
    alias: qwen3.6-27b
    context: 65536
```

```
vibe router render                       # writes ~/.config/llama-swap/config.yaml
systemctl --user restart llama-swap      # render never restarts it for you
```

`vibe router render` prints a unified diff of what it is about to write
and writes atomically. It **never** restarts llama-swap — the command
prints the `systemctl` line instead, so applying a config is always your
decision. `--check` turns it into a drift gate (exit 1 if the rendered
output differs from what is on disk, write nothing), `--stdout` prints
instead of writing, and `--extras <file>` merges an operator-owned YAML
file in verbatim for the keys vibe does not generate.

From here there is no "start the model" step. **A request is the start
verb**: the first completion naming `qwen3.6-27b` loads it, and a
one-token completion is the cheap way to warm one deliberately.

## The fleet control plane

A second box makes every client-side URL a decision. The fleet control
plane exists so it isn't: one address, forever, and the catalog behind it
changes as boxes come and go.

```
                 clients  →  http://front:9000/v1     (never changes)
                                    │
                              ┌─────┴─────┐
                        llama-swap "front", peers-only
                              │     │     │
              ┌───────────────┘     │     └──────────────┐
        workstation :9000      laptop :9000        cloud API
        (opportunistic)        (roaming)           (cloud_peer)

        fleetd on :9001  —  reads the fleet, requests changes, never serves
```

**Cell.** One llama-swap instance = one eviction domain. Usually one box.
A cell decides which models are resident on it.

**The front.** A llama-swap in *peers-only* mode on the box that is never
off — no GPU, no local models, just the catalog. It is the one URL every
client is configured with. Cells appearing and disappearing changes the
catalog, never the address. Reference deployment in
[`deploy/front/`](deploy/front/) (digest-pinned image, `-watch-config`).

**fleetd** is a deployment, not a binary. An ordinary `vibe daemon` with
`fleet_registry: true` **is** fleetd. It listens on `:9001` and serves
`/api/fleet/state`, an SSE `/api/fleet/events`, the intent/wake/announce
endpoints, a `/ui/fleet` page and an MCP facade on `/mcp`. Reference
deployment in [`deploy/fleetd/`](deploy/fleetd/), whose README is the
best copy-pasteable config reference in the repo.

The load-bearing property: **fleetd is read-and-request-only.** If it
dies, inference is unaffected — clients talk to the front, not to
fleetd — and `vibe cell status` degrades to probing cells directly.

### Membership: `hosts.yaml`

```yaml
# ~/.config/vibe/hosts.yaml
fleetd_url: "http://front.lan:9001"
cells:
  front:
    url:   "http://front.lan:9000"
    class: always_on
  workstation:
    url:        "http://ws.lan:9000"
    class:      opportunistic          # always_on | opportunistic | roaming
    host_probe: "ws.lan:22"            # a plain TCP dial: is the HOST up?
    daemon_url: "http://ws.lan:9001"   # optional: enables drain/resume/suspend
  laptop:
    url:        "http://laptop.lan:9000"
    class:      roaming
```

A cell named `front` is required whenever `cells:` is present. Decoding is
strict — a typo'd key is a load failure, not a silent degrade. The file
being **absent** is the single-box case, not an error.

`class` is what stops the fleet crying wolf: an `always_on` cell that goes
missing is an alarm, an `opportunistic` one is Tuesday, and a `roaming`
one left the house. It also decides whether the catalog *holds* or
*prunes* that cell's model ids while it is away.

`host_probe` is a TCP dial and nothing more — it distinguishes "host up,
cell down" from "host down". **There is no SSH in the control plane**, no
keys in containers and no docker socket mounts; SSH's port is just a
convenient thing to dial.

### Adding a box

```
# on the new box: llama-swap on :9000, listening on the LAN, plus either a
# full vibe daemon or the slim announcer:
vibe fleet announce --cell workstation --registry http://front.lan:9001 \
                    --token-file ~/.config/vibe/tokens/fleetd

# on the front:
#   1. add the cell to hosts.yaml (announce REFUSES a cell that isn't there)
#   2. give its backend defs `cell: workstation`
vibe router render --cell workstation --out /path/to/that/cells/config.yaml
vibe router render --cell front              # the peers-only catalog
vibe cell status                             # who is up, what is resident
vibe fleet doctor                            # credentials, versions, disks, certs
```

Cells **dial out**. Commissioning is "install, add the `hosts.yaml` entry,
point it at the registry" — no inbound port on the cell is required.

### What you get once it is up

| | |
| --- | --- |
| `vibe cell status` | every cell's derived state, resident models, declared intent, last-seen. Falls back to direct probes and prints `DEGRADED` when fleetd is unreachable. |
| `vibe cell await <cell>` | block until a cell is up / down / warm (`--ready`) / quiet (`--idle 10m`), optionally taking an advisory `--lease`. The scriptable primitive. |
| `vibe cell drain <cell>` | reclaim a box — stop its serving stack, with a pre-drain report. `--wait 60s` lets in-flight generations finish first; `--until-exit -- <cmd>` drains, runs the command (a game), and resumes on exit. |
| `vibe cell suspend` / `wake` | the whole box sleeps; `wake` sends a Wake-on-LAN packet. Always explicit, never automatic. `suspend` refuses without proof the cell is idle unless `--force`. |
| `vibe cell hold <cell> <model>` | stop fleetd's warm policy evicting a model while you evaluate it. Not a pin — llama-swap may still unload it. |
| `vibe model try <hf-repo>` | pull a candidate, wait for the cell to go quiet, apply it, and measure it against the incumbent. `--replay` scores both against this cell's *own* recent traffic. `status` / `end` resume or roll it back. |
| `vibe fleet notify` | one webhook for the alarms worth waking up for: an `always_on` cell absent with no declared intent, a persisted config-fingerprint mismatch, a drain landing on a cell that still holds leases. `away`/`home` gates delivery only — alarms keep firing and stay visible. |
| `vibe fleet mirror` | archive fleetd's state, config and the front's rendered config somewhere off-host. `verify` re-checks every hash; `restore` places it on a standby box and refuses while the original still answers. |
| `vibe fleet doctor` | read-only wiring audit, safe mid-incident. Four levels, and `UNKNOWN` is its own level: a check that could not be evaluated never reports as a pass. Exit 0/1/2/3. |

Sleep schedules, warm targets and periodic probes are declared in
`config.yaml` (`sleep_schedule`, `warm_targets`, `warm_schedule`,
`probe_targets`) and evaluated by fleetd. A declared suspend that arrives
while somebody is mid-session **defers** and re-evaluates, up to a
`max_defer` after which it is abandoned rather than forced.

### Trying it without hardware

[`scripts/fleetlab/lab.sh`](scripts/fleetlab/) stands up a real four-cell
fleet on one box — four real llama-swap processes, a real fleetd, both
announcer shapes — on scratch XDG directories, with derived ports that
refuse to overlap a production range.

```
./scripts/fleetlab/lab.sh up
./scripts/fleetlab/lab.sh status
./scripts/fleetlab/lab.sh down
```

Be honest about what it proves: CPU models are not GPU models, and one
box is not a fleet. It does not exercise a real S3 suspend, a magic
packet on a real NIC, a laptop that leaves the LAN, or a second physical
box taking over the front's address.

### Credentials

Three, with three different blast radii:

1. **The control-plane bearer** (`$XDG_STATE_HOME/vibe/token`) — every
   verb, and it is every cell's voice: announce authenticates the
   *connection*, never the cell name. Distribute it like cell-root.
2. **The guest read-only bearer** (`fleet.guest_token_file`) — exactly
   `GET /api/fleet/state` and `/api/fleet/events`, nothing else.
3. **Per-cell `swap_key_file`** — the API key that cell's *llama-swap*
   demands. Deliberately not the daemon token.

Keep `:9000` and `:9001` on a LAN or VPN. `:9000` has no auth of its own
unless you configure llama-swap's `apiKeys`, and llama-swap retains recent
request and response bodies in RAM by default, readable over its own API.

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
| `mix`     | Assembles a structured-script JSON (ordered voice segments + optional intro/outro music, chapters, cover) into one audiobook/podcast file via a single ffmpeg invocation: concat, loudnorm to -16 LUFS, encode as `.m4b`/`.m4a` (with embedded cover) or `.mp3`. |
| `short`   | Assembles a structured-script JSON of shots (clip + voiceover + optional caption) into one vertical short-form MP4: per shot fits the clip to the voiceover duration, scales/crops to vertical, burns the caption, then concats, loudnorms, and optionally ducks a background-music bed under the voice. |
| `pandoc`  | Document conversion via pandoc (docker `pandoc/core` by default, override with `binary:`). Used for markdown → EPUB study guides with `cover_image:`. |
| `youtube` | Uploads a finished video via the YouTube Data API (OAuth token cache under XDG). |
| `webhook` | Slack/Discord/Mattermost-style JSON POST. Honors `run_when: failure` so a failed pipeline still pings. |
| `confirm` | Human-in-the-loop gate: prompts on stdin (TTY) or writes a marker file the operator clears with `vamp confirm <id-or-prefix> <stage-id> [--reject]` (detach mode). Its `timeout:` is the one that auto-REJECTS rather than erroring (see below). |

**Per-attempt timeouts.** `timeout: 5m` is valid on **every** stage kind
and bounds one ATTEMPT, not the stage: it composes with `retry:` the way
an HTTP client's timeout composes with a retry budget (three attempts of
five minutes, not one five-minute total the second attempt can never fit
inside), and each foreach item is its own attempt. Absent means no bound,
which is what every pipeline written before this has. It fires as
`context.DeadlineExceeded`, which the retry classifier already counts as
both `timeout` and `transient`. It used to be confirm-only — so the one
thing a pipeline author could bound was the wait for a HUMAN, while a
stalled model server or an ffmpeg wedged on a corrupt input ran until
somebody noticed. `confirm` keeps its own meaning: its timeout is a
decision (auto-reject), not an error.

**Resumes.** `vamp run --resume <run-dir>` re-uses outputs already on
disk, including per-foreach-item granularity (a failed item in an
otherwise-successful foreach stage re-runs only that item). An output
counts as done only if it exists, is non-empty, **and** — for
`output_format: json` stages — still parses: a run killed mid-write
leaves a truncated body that a resume must re-do rather than hand
downstream. Snapshot drift aborts unless `--resume-force` is set.

**Content-addressed cache.** Cacheable stages (`text`, `comfyui`,
`audio`, `ffmpeg`, `render`, `compact`, `pandoc`, `mix`, `short`) hash their full
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
`VAMP_NO_CACHE=1`. `webhook` is uncached by default (opt in with
`cache: true` for idempotent reads); `youtube` is never cached (network
side effects). Inspect with `vamp cache {ls,size,prune,clean,info}`.

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

`vibe` — everyday:

| Command | Purpose |
| --- | --- |
| `vibe doctor` | One-shot diagnostic; `--install comfyui\|llama-cpp` runs the bring-up steps, `--pipeline <binary>` preflights a vamp pipeline binary. Non-zero exit on any FAIL. |
| `vibe start <profile>` | Activate backend + frontend; pulls missing weights; `--no-vram-check` to bypass. Service-mode profiles run concurrently with each other and one active profile. |
| `vibe run <profile> [-- <args>]` | Start a profile, exec its `managed` frontend in the foreground, stop the profile on exit; `--session <id>` resumes a pi/opencode session; everything after `--` is forwarded verbatim. **The frontend's exit status is this command's exit status** — the wrapper does not swallow it. |
| `vibe stop [name]` | Stop the active profile (no arg), a specific service (`vibe stop searxng`), or everything (`--all`). |
| `vibe ps` | Show the active profile + every running service; `--json` for the machine-readable form. |
| `vibe list` | List profiles (alias of `vibe profile list`; no daemon needed). |
| `vibe logs [name]` | Tail backend logs from the active profile (no arg) or a named service; `-f` to follow, `-n` for the last N lines. |
| `vibe tui` | Bubbletea dashboard (start/stop, status, logs). Deliberately does not auto-spawn the daemon. |
| `vibe env` | Print export lines for the active profile's frontend env vars — for `eval "$(vibe env)"`. |
| `vibe pull <profile>` | Fetch the HF weights for a profile (auto-invoked by `start`). |
| `vibe token` | Print the bearer token; `--regenerate` rotates, `--guest` targets the read-only token. |
| `vibe shutdown` | Stop the active profile and shut down the daemon. |
| `vibe daemon` | Run the daemon in the foreground (normally auto-spawned). |
| `vibe completion bash\|zsh\|fish\|powershell` | Shell completion with dynamic completers. |

`vibe profile` / `vibe backend` / `vibe router` — definitions:

| Command | Purpose |
| --- | --- |
| `vibe profile new <name> --kind <kind>` | Drop a starter YAML; `--frontend external\|docker-compose\|managed` (llama-server only), `--hf <repo>[:<file>]`, `--force`. |
| `vibe profile list` | Every profile YAML with name + description + backend + mode. |
| `vibe profile show <name>` | Print the loaded + validated profile (doubles as a lint). |
| `vibe profile schema` | Emit the profile JSON Schema (draft-07); `--out <file>` to write. |
| `vibe backend list` | Every backend def with name + mode + kind + the profiles referencing it. Reads disk; never spawns the daemon. |
| `vibe backend show <name>` | Print the loaded + validated backend def. |
| `vibe backend start <name>` | Activate a backend with no frontend; `--no-vram-check`. |
| `vibe router render` | Render `~/.config/llama-swap/config.yaml` from the backend defs. `--cell <name>` (`front` renders the peers-only catalog), `--out`, `--extras`, `--llama-server`, `--check` (drift gate, exit 1), `--stdout`. Never restarts llama-swap. |

`vibe cell` — the boxes:

| Command | Purpose |
| --- | --- |
| `vibe cell status` | Every cell's derived state, resident models, intent, last-seen. `--json` emits the same document `/api/fleet/state` serves. |
| `vibe cell await <cell>` | Block until `--up` / `--down` / `--ready` (with `--model`) / `--idle <dur>`; `--lease <holder>` takes an advisory lease on success; `--timeout`, `--interval`, `--notify`. |
| `vibe cell drain [cell]` | Stop the cell's serving stack with a pre-drain report. `--wait <dur>` for in-flight requests, `--until-exit -- <cmd>`, `--reason`, `--eta`, `--yes`. |
| `vibe cell resume [cell]` | Resume a drained cell; JIT service returns on the next request. |
| `vibe cell suspend [cell]` | Suspend the whole box. Refuses without idle proof unless `--force`; `--reason` is recorded as intent. |
| `vibe cell wake <cell>` | Send a Wake-on-LAN packet. Always explicit, never automatic. |
| `vibe cell hold <cell> <model>` | Suspend fleetd's warm policy while you evaluate. `--for <dur>` (default 4h, max 24h), `--note`, `--release`. |

`vibe model` / `vibe fleet` — the fleet:

| Command | Purpose |
| --- | --- |
| `vibe model try <hf-repo>` | Pull a candidate, apply it when the cell is quiet, measure it against the incumbent. `--like <def>`, `--idle`, `--wait`, `--now`, `--replay <n>`, `--dry-run`. |
| `vibe model try status` | The in-flight trial on this cell, and how to end it. |
| `vibe model try end` | Roll the trial back and re-render; `--purge` also deletes the weights. |
| `vibe fleet doctor` | Audit credentials, versions, disks, certs, hygiene. Read-only. `--json`, `--problems-only`, `--exit-zero`. Exit 0 clear / 1 FAIL / 2 WARN / 3 only UNKNOWNs. |
| `vibe fleet announce` | Foreground heartbeat loop for slim cells. `--cell` and `--registry` required; `--token-file`, `--llama-swap`, `--llama-server`. |
| `vibe fleet notify status\|test\|away\|home` | The notifier's scope, live alarms and counters; one real delivery; withhold or resume delivery (`--reason`, `--until`). |
| `vibe fleet mirror` | Archive fleet state off-host. `--out` required; `--keep N`, `--no-secrets`, `--include`. |
| `vibe fleet mirror verify <archive>` | Re-check every claim the manifest makes, hash by hash. |
| `vibe fleet mirror restore <archive>` | Place state + config on a standby box. Each `--*-dir` left unset is skipped, never guessed. `--dry-run`, `--overwrite`, `--force`. |
| `vibe fleet prices show [model]` | What the vendored price table says a model costs, as of a `--day`. |
| `vibe fleet prices vendor` | Refresh the vendored table from models.dev + LiteLLM. Dev tooling; needs network and a checkout. |

`vamp`:

| Command | Purpose |
| --- | --- |
| `vamp run <pipeline.yaml>` | Execute. Flags: `--detach`, `--resume <dir>`, `--resume-force`, `--dry-run`, `--no-cache`, `--no-ensure-services`, `--input k=v`. By default each `RequireService` URL is probed pre-run and auto-started via `vibe start <name>` when the setup hint matches that shape. |
| `vamp validate <pipeline.yaml>` | Parse + schema-check without running. |
| `vamp render <pipeline.yaml> <stage_id>` | Render a single stage's prompt template against inputs and prior outputs (no LLM call). |
| `vamp lint <pipeline.yaml>` | Advisory checks layered on validate: webhook URL → matching `RequireService`, `output_format: json` → `Retry.RetryOn` includes `"invalid_output"`, trivial Retry blocks, capabilities missing a `CapabilityModelHints` entry. Findings only — exit 0. |
| `vamp doctor` | Report which required profiles are up and which are missing, with each service's setup hint inline. |
| `vamp list` | List pipelines under `$XDG_CONFIG_HOME/vamp/pipelines/`. |
| `vamp capabilities` | Print the resolved capability table. |
| `vamp runs ls/show/cancel/cleanup` | One noun for everything a run leaves behind: history + live detached jobs. `ls` has a `STATE` column (running/finished/crashed); `show` reports live pid/state; `cancel` SIGTERMs a running detached run. (`vamp jobs` is a hidden deprecated alias.) |
| `vamp diff <run-a> <run-b>` | Side-by-side comparison of two runs: pipeline YAML, inputs, per-stage prompt / output / status / duration. `--json` for a machine-readable shape, `--stage <id>` to narrow, `--no-content` for metadata only. Honors `NO_COLOR`. |
| `vamp logs <id> [-f]` | Cat or follow a run's worker log (id-or-prefix). |
| `vamp cancel <id>` | SIGTERM a detached worker (alias of `runs cancel`). |
| `vamp confirm <id> <stage-id>` | Clear a `confirm` stage's gate in detach mode; `--reject` to fail it. |
| `vamp viz <pipeline.yaml>` | Mermaid `flowchart TD` of the DAG; `--show-inputs` for the input block. |
| `vamp schema` | Emit the pipeline JSON Schema (draft-07); `--out <file>` to write. |
| `vamp cache ls/size/prune/clean/info` | Inspect and manage the content-addressed cache. `info <run-dir>` reports per-stage cache hit/miss for one run. |
| `vamp completion bash\|zsh\|fish\|powershell` | Shell completion (`vamp run <TAB>` lists pipelines). |

Pipeline binaries built with `vamp.BuildRoot` inherit the pipeline-bound
and pipeline-independent subcommands above with the pipeline already in
memory — so they take no `<pipeline.yaml>` argument — plus three that
exist only there:

| Command | Purpose |
| --- | --- |
| `<pipeline> requirements` | Report the runtime resources this pipeline needs (capabilities, services, inputs, hardware hints). Takes no arguments; `--format json` for the machine-readable form consumed by `vibe doctor --pipeline`. |
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

To make the box a **cell** the front can peer with rather than a daemon
you drive by hand, `proxy_bind_all: true` is the corresponding knob for
its `:9000`.

## Examples

`vibe` profile starters under `examples/profiles/`:

| Profile | What it shows |
| --- | --- |
| [`chat-with-search`](examples/profiles/chat-with-search/) | Local LLM + Open WebUI + SearXNG sidecar for web search + Tier-1 RAG (BGE-M3 + reranker + hybrid). Copy-and-adapt. |
| [`rag-with-qdrant`](examples/profiles/rag-with-qdrant/) | Tier-2 RAG: local LLM + Open WebUI + TEI (BGE-M3) + Qdrant. Dedicated embedding service + observable vector store. |

Minimal per-kind starters live in [`profiles/`](profiles/) as
`*.example.yaml` (one per backend kind, plus an MCP definition); they are
loaded by the test suite on every CI run, so they cannot rot into
descriptions of something that no longer works.

Deployment references under `deploy/`:

| | |
| --- | --- |
| [`deploy/front/`](deploy/front/) | The peers-only llama-swap front: compose file, digest pin, `-watch-config` wiring. |
| [`deploy/fleetd/`](deploy/fleetd/) | fleetd as a container, with a fully commented `config.yaml` reference and the state paths that must survive a recreate. |
| [`deploy/cell/`](deploy/cell/) | The reclaim wrapper (drop into Steam launch options) and the systemd drop-in that records `unit_stopped` / `unit_started` as intent. |

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
| [`rag-eval-pipeline-go`](examples/rag-eval-pipeline-go/) | Go-DSL twin of rag-eval-pipeline: same DAG built with the vamp package's fluent builders, templates embedded via embed.FS into a single binary. |
| [`bench-formats-go`](examples/bench-formats-go/) | Go-DSL pipeline (vamp.Main + fluent builders) benchmarking GGUF+MTP vs EXL3 backends across context depths via vamp's per-stage throughput capture. |

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
alongside its `pipeline.yaml` to show the wiring. `vibe profile schema`
does the same job for profile YAML.

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

The single-box launcher and the vamp DAG executor are complete and in
daily use: profile schema, six backend kinds, ComfyUI supervision, the
proxy, detach + per-item resume + `run_when` qualifiers, JSON-Schema
editor wiring, LAN access with bearer-token auth.

The fleet control plane has shipped through phase C27 — cells, the
rendered router, drain/resume/suspend/wake, holds and leases, alarms,
the state mirror, `fleet doctor`, `model try`, the usage ledger. It runs
the author's own fleet.

Be precise about what "shipped" means here. Every phase is gated by unit
tests, a mutation harness (`internal/mutation`) and a conformance suite
run against two pinned real llama-swap builds — but the end-to-end live
gates ran on `scripts/fleetlab`, four llama-swap processes on **one box**.
CPU models are not GPU models and one box is not a fleet. The design docs
under [`docs/design/`](docs/design/) say which gates ran, which were
simulated, and which are still owed; where a claim was never mechanically
re-verified, they say that too rather than implying it passed.
[`TODO.md`](TODO.md) is what is open.

## Contributing

PRs welcome. `./scripts/check.sh` is the gate — it runs exactly what CI
runs, in CI's order. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
mechanical setup and [AGENTS.md](AGENTS.md) for the project conventions.

## License

MIT — see [LICENSE](LICENSE).
