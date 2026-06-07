# AGENTS.md

Operating notes for agents (Claude Code, Aider, Codex, Cursor, etc.) working
in this repo. The user-facing model lives in `README.md`; this file
captures the conventions and invariants an agent needs to make changes
that fit.

## Repo at a glance

Two binaries from one Go module (`github.com/gallowaysoftware/vibe`):

- **`vibe`** (`cmd/vibe`, `internal/vibe/`): task launcher. One YAML
  profile activates a backend (`llama_server` | `comfyui` | `http_server` |
  `tabby_api`) and an optional frontend (`external` | `docker-compose` |
  `managed`).
  The daemon owns a Connect/protobuf control plane on a unix socket
  plus optional `127.0.0.1:9001` (bearer-token-authed). The supervisor
  auto-respawns a backend that exits unexpectedly mid-life (up to 60
  restarts per 30 min) so a flaky CUDA kernel mid-foreach doesn't kill
  a long pipeline — see `internal/vibe/daemon/daemon.go:watchBackendForRespawn`.
- **`vamp`** (`cmd/vamp`, `internal/vamp/`): pipeline orchestrator that
  drives `vibe`. A YAML pipeline declares stages (`text`, `comfyui`,
  `audio`, `ffmpeg`, `youtube`, `webhook`, `confirm`, `render`,
  `compact`, `pandoc`) with a DAG of inputs; capability → profile
  mapping lives in `$XDG_CONFIG_HOME/vamp/capabilities.yaml`.

**`render` stage type.** Pure template → text without LLM invocation.
Does not activate a vibe profile. Use for deterministic data
transformation: enumerating directories, building JSON arrays, etc.
Validated in `pipeline.go:Validate`, executor in
`vision_executor.go:renderExecutor`.

Generated code: `proto/vibe/v1/control.pb.go` and
`proto/vibe/v1/vibev1connect/`. Regenerate with `buf generate` (see
`buf.gen.yaml`).

## Inner loop

```
go build ./...
go vet ./...
go test -race ./...
gofmt -l .          # CI fails if this prints anything
go mod tidy         # CI fails if this dirties go.mod/go.sum
```

The CI workflow (`.github/workflows/ci.yml`) gates exactly these. Run
them before pushing.

## Conventions agents tend to violate

- **Stdlib first.** Reach for stdlib before adding a dep. Current
  third-party set is small and intentional (cobra, yaml.v3, bubbletea,
  lipgloss, connectrpc, protobuf, isatty); justify any addition.
- **Modern Go.** `log/slog` for logging (not `log`), `errors.Join` /
  `errors.Is` / `errors.As`, `any` over `interface{}`, `embed.FS` for
  bundled assets. Go 1.26+ — `go.mod`'s `go` directive is the floor.
- **No emojis** in code, comments, commit messages, or generated docs
  unless the user explicitly asks.
- **Comments explain WHY, not WHAT.** Identifiers carry the what.
  Prefer no comment to a comment that restates the code. When you do
  comment, justify the surprising choice or the hidden constraint.
- **Don't narrate the current task.** No `// added for issue #123`,
  `// used by X`, or `// removed Y`. Those rot.
- **No documentation files unless asked.** Don't create README.md or
  similar in subdirectories without a request.

## vibe profile schema rules

- Backend is a **discriminated union by sub-block presence** — exactly
  one of `backend.llama_server`, `backend.comfyui`,
  `backend.http_server`, or `backend.tabby_api` must be set. We
  deliberately do NOT use a `kind:` field; the sub-block IS the
  discriminator. If you add a fifth backend, follow the same pattern.
- **`http_server` backend.** Wraps any HTTP-serving inference engine
  (TTS daemons, embedding servers, third-party inference). Two
  modes, mutually exclusive: docker (`image:` + optional `volumes`,
  `gpu`, `container_port`) or bare binary (`binary:`). Common
  fields: `port` (required, daemon proxies here), `args`, `env`,
  `health_path` (defaults to `/health`). The daemon synthesizes a
  `docker run --rm --name vibe-<profile> -p 127.0.0.1:N:M ...`
  invocation in docker mode. Used today for Kokoro-FastAPI TTS
  serving the audio stage's `capability: tts` capability. Frontend
  block is rejected — the HTTP server IS the deliverable.
- Frontends use an explicit `frontend.kind` enum
  (`external | docker-compose | managed`) because frontends share many
  fields; the sub-block-presence trick doesn't fit.
- Path fields (`backend.*.path`, `backend.*.dir`,
  `backend.comfyui.python`, `backend.llama_server.binary`,
  `backend.llama_server.mmproj`, `backend.llama_server.draft_model`,
  `backend.tabby_api.model_dir`,
  `backend.tabby_api.venv`, `backend.tabby_api.repo`,
  `backend.tabby_api.draft_model_dir`, `frontend.workdir`,
  `frontend.binary`, `frontend.write_file`, `frontend.compose_file`)
  are tilde-expanded in `internal/vibe/profile/profile.go:Load`. Add
  new path fields to that list.
- **Vision models (mmproj).** `backend.llama_server.mmproj` is the
  path to the multimodal projector GGUF that llama-server loads via
  `--mmproj`. Required to enable image input on vision-capable
  models (Gemma 3, Qwen2.5-VL, LLaVA, etc.) — without it,
  llama-server rejects image content parts with "image input is not
  supported". When `huggingface.mmproj_file` is set, vibe pulls a
  second file from the same repo/revision into the mmproj path.
  Validation rules (in `validateLlamaServer`): mmproj path must
  exist on disk unless an HF mmproj_file is provided; setting
  mmproj_file without an mmproj target is rejected. The HF pull
  flow in `daemon.Pull` is a helper closure (`pullOne`) called
  once per file — weights, mmproj, and draft model — all
  streaming download progress over the same RPC stream.
- **Speculative draft models (Gemma 4 MTP).**
  `backend.llama_server.draft_model` points at a draft GGUF loaded via
  `--model-draft`; vibe also emits `--spec-type` (`spec_type`, default
  `draft-mtp`) and `--spec-draft-n-max` (`spec_draft_n_max`, default 4).
  Gemma 4's MTP head ships as a separate ~0.4B "assistant" drafter
  (unlike Qwen MTP, which is in-weights and needs no draft file).
  `huggingface.draft_file` pulls it from the same repo into the
  draft_model path (same `pullOne` flow as mmproj). `validateLlamaServer`
  mirrors the mmproj rules and additionally **rejects a quantized
  `cache_type_k/v` when `spec_type` is `draft-mtp`** — quantized KV gives
  ~0% draft acceptance, so the speedup would silently vanish.

## vamp stage rules

- Adding a stage type? Touch all of: `Stage` struct in
  `internal/vamp/pipeline.go`, the type switch in
  `pipeline.go:Validate`, the executor in
  `internal/vamp/<kind>_executor.go` implementing `StageExecutor`,
  `stageCacheable` in `cache_key.go` if it should be cacheable, and
  `schema.go`'s stage-properties block.
- **Cache invariants.** `stageCacheable` (in
  `internal/vamp/cache_key.go`) is the single source of truth for "can
  this stage type be cached?". Today it returns true for `text`,
  `comfyui`, `audio`, `ffmpeg`, `render`, `compact`, `pandoc`, `mix`,
  `short` and false for everything else (`youtube`, `confirm`).
  `webhook` is non-cacheable by default but opt-in cacheable via
  `cache: true` (for idempotent reads). Side-
  effect stages must not be cached — replaying a "success" would skip
  the side effect that gave the pipeline its reason for existing.
- **`.stages.X.output` semantics depend on stage type.** For text /
  render / webhook stages (including their foreach variants — the
  per-item content is `\n\n`-joined) it renders the **content**
  produced by the stage, so templates can inline it directly:
  `{{ .stages.merge_lessons.output }}` drops the merged JSON into
  the next prompt verbatim. For binary stages (comfyui / audio /
  ffmpeg / youtube) it renders the **absolute path(s)** to the
  output file, since those bytes are not text. When a downstream
  stage needs a field out of a text-stage's JSON, use `readFile`
  only if the path-shaped form is needed; otherwise pipe directly:
  `{{ .stages.X.output | parseJSON | <accessor> | toJSON }}`.
  See `examples/rag-eval-pipeline/` for the canonical chain.
- **Stage executors take injectable runners.** Every executor accepts
  a runner / httpDoer / process spawner that tests can swap. Don't
  hard-code `exec.Command` or `http.DefaultClient` at the executor
  level.
- **Webhook assertions.** `webhook` stages take an optional
  `assert:` block with `status_code` / `body_contains` /
  `body_not_contains` / `min_body_length` checks, exercised in
  `webhook_executor.go:runWebhookAsserts`. Designed for smoke-test
  pipelines that probe a stack from the outside. Setting
  `assert.status_code` overrides the executor's default "2xx
  required" so tests can verify a 401/4xx. GET/DELETE webhooks may
  omit `body:` / `body_template_file:` (POST/PUT/PATCH still
  require one to avoid silent empty notifications). The
  `examples/profiles/chat-with-search/smoke.yaml` pipeline is the
  canonical use.
- **Vision (image_dir / image_files).** Two ways to attach images to
  a `type: text` stage: `image_dir` (scan a directory, glob all
  supported files) or `image_files` (explicit templated list, one
  rendered path per entry, single-image-per-iteration fan-out).
  Mutually exclusive; same downstream encoding. SVGs get rasterised
  via `rsvg-convert` into a content-addressed PNG cache under
  `$XDG_CACHE_HOME/vamp/svg-rasterized/`. Rasterisation fits the
  output within 896×896 (`--width 896 --height 896 --keep-aspect-ratio`)
  so the result is a single Gemma 3 vision tile (~256 image tokens);
  exceeding 896 in either dimension triggers pan-and-scan and balloons
  token count. Requires `rsvg-convert` on `$PATH` when SVGs are present,
  and a vision-capable backend (Gemma 3 + mmproj) to actually consume
  the images.
- **Foreach items run independently.** A failing item no longer cancels
  sibling items via the per-stage context. Each item completes or fails
  on its own; the stage aggregates partial output from successes and
  reports joined errors. See `exec_test.go:TestExecutor_ParallelForeach_IndependentItems`.
- **Template functions.** Registered in `exec.go:templateFuncs`:
  `readFile` (tilde-expanded), `readFiles(pattern)` (glob, 200KB max
  per file, sorted, errors on no-match), `readFilesOrEmpty(pattern)`
  (same but returns "" on no-match — for foreach prompts that may
  have empty per-item globs), `readLessons(path, batch, total)`
  (paginated lesson reading), `enumerateLessons(glob)` (JSON array of
  lesson dirs, filters files >200KB), `enumerateImagePairs(root, lessonsJSON)`
  (flatten lesson list to per-image `{lesson, image, image_path}`
  entries), `enumerateUniqueImages(root, lessonsJSON)`
  (content-hash-deduped variant returning `{hash, path, ext}`),
  `imageDescriptionsForLesson(runDir, root, lesson)` (per-lesson
  reverse-lookup against `runDir/image_desc/<hash>.json` files),
  `extractSVGText(path)` (parse SVG XML, return `<text>` labels
  joined by `|` — sidecar for vision prompts so the model sees
  ground-truth strings even when small fonts in the raster blur),
  `mergeJSON(ndjson)`, `parseJSON`, `toJSON`, `urlencode`,
  `stripDataURIs`, `truncate`, `flattenItems`, `uniqueByKey`,
  `joinPath(parts...)`, `wordCount(text)` (returns int — for prompts
  that need authoritative word counts instead of model self-estimates,
  e.g., mode-switched edit passes), `mulInt(n, mult)` (int × float →
  int, for derived numeric targets in prompt prose), `addInt(a, b)`
  (int arithmetic across nested template ranges; Go templates lack
  native arithmetic), `splitSentences(text, maxChars)` (JSON array of
  greedy-packed sentence chunks under maxChars — TTS-friendly chop
  for long paragraphs that engines like Kokoro otherwise rush).
- **Concat WAVs.** `Stage.ConcatWavs` on an `ffmpeg` stage auto-globs
  all `*.wav` files, creates a concat list, and merges into the output
  MP3. Implemented in `ffmpeg_executor.go:executeConcatWavs`.
- **M4B audiobook mode.** `Stage.M4BFrom` / `M4BVar` / `M4BFile` /
  `M4BChapter` on an `ffmpeg` stage drive a chapterised audiobook
  build: read the upstream JSON-array stage to determine chapter
  order, ffprobe each per-chapter MP3 for duration, write an
  FFMETADATA chapter table + concat list, and run one ffmpeg
  invocation producing an Apple-Books-readable `.m4b` with embedded
  chapter navigation. `CoverImage` (also valid on `pandoc` stages)
  embeds the audiobook art / EPUB cover. Cache key folds in the
  chapter file template, chapter titles, and cover bytes so an M4B
  with empty FFmpegArgs doesn't collide with a concat_wavs entry.
- **Pandoc stage.** `type: pandoc` shells out to pandoc (docker
  `pandoc/core` image by default, override with `binary:`). Fields:
  `source_file`, `pandoc_from`, `pandoc_to`, `pandoc_metadata`
  (map of `--metadata key=value`), `pandoc_args` (raw extra args),
  `cover_image` (rendered as `--epub-cover-image`). Used today for
  markdown → EPUB study-guide generation.
- **Mix stage.** `type: mix` reads a structured-script JSON
  (`script_file`) of ordered voice-segment paths plus optional
  `intro_music` / `outro_music` / `cover_image` / chapters, and runs
  one ffmpeg invocation to concat the segments, loudnorm to -16 LUFS
  (override with `loudness_target`), and encode an audiobook/podcast
  file: `.m4b`/`.m4a` (AAC + attached-pic cover + faststart) or `.mp3`
  (libmp3lame, no cover). `metadata` keys become container tags.
- **Short stage.** `type: short` is the video analog of `mix`: a
  `script_file` JSON of shots (clip + voiceover + optional caption)
  becomes one vertical MP4 via a single ffmpeg invocation. Per shot it
  fits the clip to the voiceover duration (freeze-last-frame `tpad` by
  default, or `short_stretch_video` to time-stretch), scales/crops to
  the vertical target (`short_width`/`short_height`/`short_fps`,
  default 1080×1920@30), burns the caption via drawtext `textfile=`,
  then concats every shot, loudnorms, and optionally ducks an optional
  `background_music` bed under the voice.
- **InputSpec requires struct form.** Bare strings like
  `lesson_root: "~/path"` are rejected. Use `lesson_root: {default: "~/path"}`.
- **`chat_template_kwargs` passthrough.** Text-stage `params` accepts
  `chat_template_kwargs` (typically `{enable_thinking: false}`),
  forwarded by tabbyAPI / vLLM / SGLang to the model's chat template;
  ignored by llama-server. Use to silence Qwen3's verbose CoT preamble
  on strict-JSON stages — the model otherwise eats `max_tokens` before
  emitting a single brace. Keep CoT *on* for stages whose quality
  benefits (planning / editing). The allowlist is in
  `pipeline.go:knownTextParamKeys`.
- **`free_memory_after` on ComfyUI stages.** Set on the DSL with
  `.FreeMemoryAfter()` (Go) or `free_memory_after: true` (YAML) to
  POST /free after a successful workflow. Best-effort + non-fatal —
  weights unload + VRAM reclaim happens before the next pipeline run.
  Use on pipelines that issue a single image_gen stage per run and
  need the GPU back for a downstream LLM activation (e.g. a cover-image
  stage freeing VRAM before the next long-form text stage).
- **Auto-ensure RequireService.** `vamp run` (and any binary that
  mounts the same runCmd) probes every declared `RequireService` URL
  pre-run and auto-runs `vibe start <name>` for any unreachable
  service whose setup_hint matches that exact shape. Disable with
  `--no-ensure-services`; the legacy 3-second-retry-then-fail-with-
  hint path still works behind the flag.
- **`vamp lint` is the advisory layer.** Two checks today: webhook
  URLs on loopback hosts must have a matching `RequireService`
  declaration; text stages with `output_format: json` must include
  `"invalid_output"` in their `Retry.RetryOn` list. Exit code 0
  regardless of findings — lint is editorial, not gating. New checks
  go in `internal/vamp/cli/cmd_lint.go` next to the existing two.

## Detach / job lifecycle

- `vamp run --detach` re-execs the current binary with the hidden
  `--internal-run-job` flag in a fresh session (`Setsid`), with stdin
  redirected to `/dev/null`.
- `os.Stdin` in the detached worker is `/dev/null`, which IS a
  character device. Do not use `info.Mode() & os.ModeCharDevice` to
  detect a TTY — use `isatty.IsTerminal(os.Stdin.Fd())`. (Bug fixed in
  86db9b8 because of this trap.)
- **`runs` is the single run noun.** History and live detached jobs are
  one surface: `vamp runs {ls,show,cancel,cleanup}`. `runs ls` derives a
  `STATE` column per dir from the pid file (`vamp.JobStatus`); `runs
  show` overlays live pid/state via `FindJobByPrefix`; `runs cancel`
  reuses `runCancel`. `vamp jobs` is a hidden deprecated alias whose
  subcommands delegate to the same `runs*Cmd` constructors — don't add
  new behavior under `jobs`. The data layer (`jobs.go`: `ListJobs`,
  `FindJobByPrefix`, `JobStatus`, `JobState`) is unchanged.
- **Run-targeting commands take an `<id-or-prefix>`.** `runs show`,
  `runs cancel`, `logs`, `confirm`, `diff`, and top-level `cancel` all
  resolve via `FindRunByPrefix`/`FindJobByPrefix` (path-shaped args work
  too) and render lookup failures through the shared `renderLookupErr`;
  they tab-complete via `completeRunIDs`. Keep new run-targeting
  commands on that path rather than taking a raw run-dir.

## Profile authoring / first-run guards

- **`vibe profile new` is canonical** (name positional, `--kind` via
  flag with completion over every bundled template, `--frontend` sugar
  for llama-server). `vibe profile init` is a hidden deprecated alias.
  `--kind` values are derived from `profile_templates/*.yaml` via
  `profileKinds()` — add a template file and it shows up automatically.
- **`REPLACE-` is a hard gate.** `profile.Validate` re-marshals the
  parsed profile (dropping `# REPLACE:` comments) and rejects any
  surviving `REPLACE-` value, so an unedited starter fails to load with
  a clear message instead of a downstream file-not-found. Template
  placeholder VALUES must use the `REPLACE-...` form (with hyphen);
  explanatory comments use `# REPLACE: ...` and are exempt.
- **Read-only commands never spawn the daemon.** `vibe list` reads the
  profiles dir directly; `vibe ps` / `vibe env` ping the daemon and
  report "not running" rather than `ensureDaemon`. Only `start` / `run`
  / `pull` / `stop` / `logs` may auto-spawn.

## Things to never do

- Don't add `--no-verify`, `--no-gpg-sign`, or any hook-bypass flag to
  git commands unless the user explicitly asks.
- Don't `git add .` or `git add -A` — the repo has historically pulled
  in `.claude/worktrees/` as submodule entries when this happened.
  Stage files by name.
- Don't commit `dist/`, `*.pid`, `*.log`, `*.sock` (already in
  `.gitignore` but worth saying).
- Don't bump `go.mod`'s `go` directive without also bumping the
  `golang:X.Y.Z-alpine` line in any Dockerfile that ships from this
  repo.

## Where to look

- Architecture deep-dive: `README.md` "How it works" section.
- Open work + recent history: `TODO.md`.
- Examples (real, runnable pipelines): `examples/`.
- Wire-level smoke commands: scan recent commit messages — every
  feature merge includes the smoke that verified it.
