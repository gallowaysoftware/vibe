# TODO

## Open

- **Router + model lifecycle (adopt llama-swap).** Design at
  `docs/design/router-lifecycle.md` (2026-07-12): llama-swap per cell
  (localmodel / spark-pair / llamaloft) federated via peers, front instance takes
  over :9000, per-model TTL idle-unload + JIT autostart with SSE
  loading-state keepalives, matrix eviction, Anthropic as an apiKey
  peer, vamp streaming warm + lease heartbeat. Supersedes fleet.md's
  router/health/slot half; A0-A8 roadmap in the doc. **A1+A2+A7+A8a
  DONE 2026-07-12** (llama-swap v239 on :9000 with a RENDERED config,
  all LLM defs + ComfyUI external, cloud_peer anthropic, canonical
  capability ids, vamp typed errors/lease/--warm, fleet state+events
  API, 420s six-client gate + simulated peer-hop gate passed — §15-§17).
  Remaining needs hardware: A3/A4 (llamaloft), real A5/A6 (Sparks), A8b
  (proxy.go deletion once bge-embed moves to llamaloft).

- **hum/riff bring-up (in progress 2026-07-13).** Design at
  `docs/design/topology.md`; stacks at `deploy/hum` (front router, infra)
  and `deploy/riff` (persistent OWUI + SearXNG, application) — split
  deliberately so infra and app upgrade independently. Status:
  - DONE: localmodel's llama-swap cell now listens on `0.0.0.0:9000`
    (LAN-reachable at `192.168.13.117:9000`). hum's front config staged
    at unraid `/mnt/user/appdata/hum/front/config.yaml` (localmodel peer,
    all 9 model ids; llamaloft/spark/anthropic commented — anthropic
    key is NOT required, cloud peer ships disabled). riff's `.env`
    generated (`HUM_URL`, `RIFF_IPV4=172.16.3.212`, `WEBUI_SECRET_KEY`).
    Both compose stacks are UP on unraid (macvlan: hum-front `.211`,
    riff-webui `.212`; fixed a missing top-level `networks:` block in
    riff's compose, b2a4256). Model library seed: `~/models` (251GB)
    rsyncing to the new unraid `models` share in the background
    (`--partial --inplace`, survives interruption; re-run the same
    rsync command to resume/verify — do not assume it finished without
    checking).
  - BLOCKED on a UDM Pro firewall rule: hum-front (`172.16.3.211`,
    services VLAN) times out dialing localmodel (`192.168.13.117:9000`,
    main LAN) — `peer proxy error: dial tcp ...: i/o timeout` on any
    real completion through hum, even though catalog listing and the
    reverse direction (localmodel curling hum) both work. Needs an
    Allow rule, LAN-to-LAN/inter-VLAN, source `172.16.3.211` → dest
    `192.168.13.117:9000` tcp, placed above any existing block-inter-VLAN
    rule. Once added: re-verify with a real chat completion through
    `http://172.16.3.211:9000/v1/chat/completions` (model
    `qwen2.5-coder-7b` is the cheap JIT-load test) — the earlier attempt
    hung and 502'd, this is the exact repro.
  - NOT YET DONE: NPM proxy host `chat.<domain>` → `172.16.3.212:8080`
    with Authelia forward-auth + header-strip (block in
    `deploy/riff/README.md`); PWA install on phone; repointing coding
    harnesses at hum's address once it's confirmed end-to-end.

- **Fleet provisioning (multi-host + cloud keys).** Design at
  `docs/design/fleet.md` (2026-07-12): agentless SSH provisioning of
  2x DGX Spark + a 3080 Ti utility box, hosts.yaml converge, model
  distribution, doctor, Spark commissioning. The router half is
  superseded by router-lifecycle.md; the provisioning half stands and
  is a dependency of it (A2/A4 reuse fleet P2/P4 verbatim).

- **Multi-GPU scheduling.** The current single-profile-at-a-time
  invariant assumes one GPU; supporting two or more cards needs (a) a
  GPU index in the profile schema, (b) per-GPU free-VRAM accounting in
  the pre-flight check, and (c) a vamp scheduler that can run two
  capabilities concurrently when they land on different cards.

## Recently shipped

- **Proxy model-id routing + `services:` co-start** (2026-06-30, b688968).
  The reverse proxy grew per-model routes (AddRoute/RemoveRoute): POSTs
  get their JSON `model` field peeked and routed to a matching upstream,
  falling through to the active profile otherwise, with `/v1/models`
  aggregated across upstreams. An llama_server service auto-registers its
  alias as a route on start and deregisters on stop / respawn-giveup. A
  profile's new `services:` field co-starts (and stops) service-mode
  sidecars best-effort — dormant plumbing until a profile declares one.

- **vamp EnsureProfile / EnsureCapability Go API** (2026-06-09, 14a86b9).
  Public primitive for "stand up the environment, then drive it from Go":
  declare a vibe profile (or capability), call `vamp.EnsureProfile`, and
  talk plain OpenAI HTTP to the returned Endpoint (BaseURL + Model). It
  probes /v1/chat/completions until the model actually generates, so a
  backend still loading weights fails with a clear timeout instead of
  hanging the caller's first request. `StopProfile` releases the slot.

- **Profile lifecycle hooks (`pre_start` / `post_stop`)** (2026-06-09,
  b9634bf). A `hooks:` block runs shell commands around a profile's
  lifecycle: `pre_start` after the VRAM pre-flight and before the
  backend/frontend come up (non-zero exit aborts), `post_stop` after
  teardown, best-effort. Lets a managed-frontend profile bring its own
  sidecar services up and down with the session without a wrapper
  launcher.

- **Reusable backends; vamp capabilities target backends** (2026-06-07,
  e90e795). A profile's model spec decomposes into a named backend under
  `backends/<name>.yaml`, referenced via `backend_ref:` (resolved at
  Load), so one model definition is shared across frontends.
  `StartRequest.backend` activates a backend with no frontend — the
  active identity is the backend name, so repeated activations are no-op
  reuse. vamp capability candidates resolve to backend names via
  `EnsureBackendActive`, falling back to profiles for pre-backend
  capabilities.yaml files.

- **Speculative draft models (Gemma 4 MTP)** (2026-06-07, 8d72ee1).
  `backend.llama_server.draft_model` + `spec_type` + `spec_draft_n_max`
  and `huggingface.draft_file`, mirroring the mmproj plumbing
  (tilde-expansion, HF pull, validation, schema, launch flags). Gemma 4's
  MTP head ships as a separate ~0.4B drafter, unlike Qwen's in-weights
  MTP. A quantized `cache_type_k/v` with `spec_type: draft-mtp` draws a
  warning (originally a rejection; softened in 344fac4) — quantized KV
  gives ~0% draft acceptance, silently killing the speedup.

- **Video content-mill primitives** (2026-05-30). Three vamp features +
  a cache fix to drive image-to-video pipelines:
  - comfyui `input_images` map ("<node>.<input>" -> templated source path):
    uploads an upstream still to ComfyUI (`POST /upload/image`) and binds the
    returned filename to a LoadImage node — the i2v / ref-edit seam. Cache key
    folds the source image sha256 (gated; parameters-only stages keep their key).
  - ffmpeg `concat_video` mode: ordered N-clip concat from an upstream JSON
    array, re-encoded through a normalizing scale/pad/setsar/fps filtergraph.
  - `short` stage type (video analog of `mix`): assembly-script JSON of shots
    (clip + voiceover + caption) -> one vertical MP4; per-shot freeze/trim to
    the voiceover duration, scale/crop to vertical, captions burned via
    drawtext `textfile=` (apostrophe-safe), concat, loudnorm, optional ducked
    music. Real `computeStageCacheKey` branch.
  - fix: `mix` was cacheable but had no cache-key branch (empty key / never
    cached) — added a real branch. Added pandoc/mix/short to the schema enum.
  - capability `video_gen -> comfyui`. Validated end-to-end on a 5090 with
    Qwen-Image + Wan2.2-TI2V-5B i2v.

- **CLI usability pass (good → great)** (2026-05-29). No new features —
  streamlining and consistency across both binaries.
  - vamp: merged `jobs` into `runs` (one noun; `runs ls` gains a `STATE`
    column, `runs show` reports live pid/state, `runs cancel` added;
    `jobs` is now a hidden deprecated alias). Every run-targeting command
    (`runs show/cancel`, `logs`, `confirm`, `diff`, `cancel`) takes an
    `<id-or-prefix>` with tab-completion (`completeRunIDs`) and shares
    `renderLookupErr`; not-found errors point at `vamp runs ls`. `confirm`
    no longer needs a full run-dir path. `viz` gained `filepath.Abs` +
    completion and lost the dead `--format dot` flag and "Phase 1"
    jargon. `--detach` prints copy-pasteable follow/cancel hints. Shared
    `--input` usage string.
  - vibe: `vibe stop --all` flag replaces the magic positional `all`
    (kept as a deprecated alias). `vibe list` is a no-daemon alias of
    `vibe profile list` (shared `renderProfileList`). `vibe profile new`
    is canonical and its `--kind` now covers all 7 bundled templates
    (derived from the embed FS); `profile init` is a hidden alias.
    Read-only `ps`/`list`/`env` no longer auto-spawn the daemon.
    `start`/`run` share the `--no-vram-check` string, sort env output,
    and guard the empty `proxy:` line.
  - errors: `profile.Validate` rejects unedited starters (any surviving
    `REPLACE-` value) with one clear message; ffmpeg / docker missing-
    binary failures now carry install hints instead of a raw
    `exec: ... not found`.

- **Four new template helpers: `wordCount`, `mulInt`, `addInt`,
  `splitSentences`** (2026-05-25). Mechanical-guarantee helpers for
  pipelines that need deterministic shape from upstream LLM output.
  `wordCount(text)` returns int — drives mode-switched stages that
  branch on prior-stage length (e.g., a long-form prose pipeline's
  EXPAND / EXTEND / POLISH / TRIM edit dispatch). `mulInt(n, mult)` /
  `addInt(a, b)` give Go templates the arithmetic the stdlib omits
  (sequential counters across nested ranges, derived numeric
  targets in prompt prose). `splitSentences(text, maxChars)` chops
  long prose at sentence boundaries, greedy-packed under maxChars —
  a prose-to-audio pipeline uses it to cap Kokoro segments at 300
  chars (Kokoro rushes calls over that and elides interior comma
  pauses). All four
  registered in `exec.go:templateFuncs` and documented in
  `AGENTS.md`. Underlying pattern: when an LLM pipeline needs a
  mechanical guarantee (mode dispatch, length cap, segment chopping,
  distribution targets), do it at template-render time; models
  treat prompt rules as advisory even when phrased as hard
  requirements. Three real failures in a production pipeline run
  validated this.

- **EXL3 + tabbyAPI integration** (2026-05-24). New `tabby_api` backend
  for vibe profiles, supervised alongside llama-server / comfyui /
  http_server. Ships a `vibe_defaults` sampler preset (`min_p 0.05`,
  `repetition_penalty 1.05`) so EXL3 backends don't degenerate into
  repetition loops on stages that only set `temperature` +
  `max_tokens`. The `chat_template_kwargs` passthrough on text stages
  lets pipelines toggle Qwen3's verbose CoT off on strict-JSON stages
  (`enable_thinking: false`) without forking the chat template.
  Validated on a long-form episode → m4b run on an EXL3 long-form
  profile + Qwen3.6-27B-6.0bpw.

- **`free_memory_after` on ComfyUI stages** + **auto-ensure-services
  preflight** + **`vamp lint`** (2026-05-24). Three small DX wins from
  one runway. ComfyUI stages can now `POST /free` after a successful
  workflow so SDXL/Flux weights don't squat in VRAM between
  episodes of an episodic pipeline. `vamp run` auto-runs
  `vibe start <name>` for any
  unreachable `RequireService` URL with a parseable setup hint, so
  "first run of the day after reboot" just works without learning
  `<pipeline> activate`. `vamp lint` checks webhook → RequireService
  pairing and JSON-output retry coverage; dogfood caught a missing
  retry policy on a document-to-audiobook pipeline's `extract_topics`.

- **Per-capability resolution summary + pipeline_timing.json
  `capabilities` field** (2026-05-24, v0.6.0+v0.6.1). At end of run
  vamp prints one line per capability naming the profile that
  actually answered, with a `[fallback]` tag when the 1st-choice
  candidate was skipped (typically VRAM rejection). v0.6.1 also
  records the per-capability resolution in `pipeline_timing.json`'s
  new `capabilities` map so downstream tools (e.g. a per-pipeline
  `timings --summary`) can answer "which profile did this run land on?"
  without grepping the live log.

- **Two more `vamp lint` checks** (2026-05-24, v0.6.1). Trivial
  Retry blocks (`MaxAttempts < 2` — a retry-block no-op) and
  capabilities referenced but absent from `CapabilityModelHints`
  (so `<pipeline> doctor` can sanity-check the resolved profile
  against expected `min_params` / `min_context`). Brings the total
  to four checks; dogfooding against the local pipeline catalog
  flagged real gaps on dag-smoke + function-pipeline.

- **End-to-end validation: 12 episodic runs on EXL3** (2026-05-24).
  FreeMemoryAfter + auto-ensure-services + per-capability resolution
  all confirmed in production across 12 consecutive runs with zero
  VRAM-fallback regressions. Wall-clock comparison: EXL3 mean 8m44s
  vs GGUF 5m15s on this workload — EXL3's per-token speedup gets
  eaten by Qwen3.6's verbose CoT on long-form stages. CoT-off via
  `chat_template_kwargs` is the speed lever for strict-output stages;
  long-form stages keep CoT on for quality.

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
