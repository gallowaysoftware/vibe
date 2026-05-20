# AGENTS.md

Operating notes for agents (Claude Code, Aider, Codex, Cursor, etc.) working
in this repo. The user-facing model lives in `README.md`; this file
captures the conventions and invariants an agent needs to make changes
that fit.

## Repo at a glance

Two binaries from one Go module (`github.com/gallowaysoftware/vibe`):

- **`vibe`** (`cmd/vibe`, `internal/vibe/`): task launcher. One YAML
  profile activates a backend (`llama_server` | `comfyui`) and an
  optional frontend (`external` | `docker-compose` | `managed`). The
  daemon owns a Connect/protobuf control plane on a unix socket plus
  optional `127.0.0.1:9001` (bearer-token-authed).
- **`vamp`** (`cmd/vamp`, `internal/vamp/`): pipeline orchestrator that
  drives `vibe`. A YAML pipeline declares stages (`text`, `comfyui`,
  `audio`, `ffmpeg`, `youtube`, `webhook`, `confirm`, `render`) with a
  DAG of inputs; capability → profile mapping lives in
  `$XDG_CONFIG_HOME/vamp/capabilities.yaml`.

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
  one of `backend.llama_server` or `backend.comfyui` must be set. We
  deliberately do NOT use a `kind:` field; the sub-block IS the
  discriminator. If you add a third backend, follow the same pattern.
- Frontends use an explicit `frontend.kind` enum
  (`external | docker-compose | managed`) because frontends share many
  fields; the sub-block-presence trick doesn't fit.
- Path fields (`backend.*.path`, `backend.*.dir`,
  `backend.comfyui.python`, `backend.llama_server.binary`,
  `backend.llama_server.mmproj`, `frontend.workdir`,
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
  twice — once for the weights, once for the mmproj — both
  streaming download progress over the same RPC stream.

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
  `comfyui`, `audio`, `ffmpeg` and false for everything else
  (`webhook`, `youtube`, `confirm`). Side-effect stages must not be
  cached — replaying a "success" would skip the side effect that gave
  the pipeline its reason for existing.
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
- **Vision (image_dir).** `Stage.ImageDir` on a `type: text` stage
  instructs the executor to scan the directory for image files
  (png/jpg/jpeg/gif/webp/bmp), base64-encode them, and send via
  `CallMultimodal`. SVGs in the directory are rasterized through
  `rsvg-convert` into a content-addressed PNG cache under
  `$XDG_CACHE_HOME/vamp/svg-rasterized/` and attached as the resulting
  PNG — mmproj-backed vision models read pixels, not vector markup, so
  the stage would otherwise drop the diagrams the lesson author
  authored as SVG. The field renders as a template so foreach stages
  can set per-item directories. Requires `rsvg-convert` on `$PATH`
  when SVGs are present, and a vision-capable backend (Gemma 3 +
  mmproj) to actually consume the images.
- **Foreach items run independently.** A failing item no longer cancels
  sibling items via the per-stage context. Each item completes or fails
  on its own; the stage aggregates partial output from successes and
  reports joined errors. See `exec_test.go:TestExecutor_ParallelForeach_IndependentItems`.
- **Template functions.** Registered in `exec.go:makeTemplate`:
  `readFile` (tilde-expanded), `readFiles(pattern)` (glob, 200KB max
  per file, sorted), `readLessons(path, batch, total)` (paginated
  lesson reading), `enumerateLessons(glob)` (JSON array of lesson dirs,
  filters files >200KB), `mergeJSON(ndjson)` (newline-delimited JSON
  merge), `joinPath(parts...)` (filepath.Join).
- **Concat WAVs.** `Stage.ConcatWavs` on an `ffmpeg` stage auto-globs
  all `*.wav` files, creates a concat list, and merges into the output
  MP3. Implemented in `ffmpeg_executor.go:executeConcatWavs`.
- **InputSpec requires struct form.** Bare strings like
  `lesson_root: "~/path"` are rejected. Use `lesson_root: {default: "~/path"}`.

## Detach / job lifecycle

- `vamp run --detach` re-execs the current binary with the hidden
  `--internal-run-job` flag in a fresh session (`Setsid`), with stdin
  redirected to `/dev/null`.
- `os.Stdin` in the detached worker is `/dev/null`, which IS a
  character device. Do not use `info.Mode() & os.ModeCharDevice` to
  detect a TTY — use `isatty.IsTerminal(os.Stdin.Fd())`. (Bug fixed in
  86db9b8 because of this trap.)

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
