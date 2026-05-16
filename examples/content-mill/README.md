# content-mill

Full vamp pipeline stitching every stage type together: LLM scripting,
LLM JSON shot-list, ComfyUI image gen, Piper TTS, ffmpeg muxing, and a
Slack/Discord webhook on completion — plus a failure-path webhook that
fires only when an earlier stage explodes.

## Prereqs

- vibe daemon running (`vibe daemon &`)
- `code` and `comfyui` profiles in `~/.config/vibe/profiles/`
- `capabilities.yaml` maps `reasoning: code` and `image_gen: comfyui`
- `piper` on `$PATH` with `en_US-lessac-medium.onnx` under
  `~/.local/share/piper-voices/`
- `ffmpeg` on `$PATH`
- `VAMP_SLACK_WEBHOOK=https://hooks.slack.com/...` env var (or drop the
  `notify` and `notify_failure` stages if you don't want the announcement)

## Run

```
VAMP_SLACK_WEBHOOK=https://hooks.slack.com/... \
  vamp run examples/content-mill/pipeline.yaml \
  --input topic="bioluminescent forests at dusk"
```

## Stage graph

```
script -> cover -> render -.
                            \
                             assemble -> notify         (run_when: success, the default)
       voice ----------------/
       (any stage above failing) ---> notify_failure    (run_when: failure)
```

## How the two notify stages work

The pipeline ends in two webhook stages with different `run_when:`
qualifiers — together they give the operator try/finally semantics
without any branching logic in the YAML.

**`notify` (default, `run_when: success`)** — declares `inputs:
[assemble]` and so depends on the full success path. It fires only after
every prior stage succeeds; if anything upstream fails, the executor
skips it.

**`notify_failure` (`run_when: failure`)** — declares no `inputs:`, so
it isn't gated on any success-path stage finishing. The executor
schedules it only when the pipeline's overall status is `failed` (i.e.
some other stage hit a terminal error after exhausting its retries).
The template namespace gains two extra bindings for failure-path
stages:

- `{{ .pipeline_status }}` — `"failed"` here.
- `{{ .failure_summary }}` — the first failed stage's id and trimmed
  error message, e.g. `render: comfyui workflow returned http 500`.

The pair lets you wire a single pipeline that always notifies the same
channel — success messages on the happy path, failure messages with
useful context on the sad path — without dropping into a separate
incident-response runbook.

`run_when: always` is also supported (runs unconditionally regardless of
prior status) and is the right choice for cleanup stages that need to
run whether the pipeline succeeded or failed.

## Files

- `pipeline.yaml` — the seven-stage pipeline plus the failure-path
  notify stage.
- The cover-image workflow is reused from
  [`examples/comfyui-image-batch/workflows/sdxl_turbo.json`](../comfyui-image-batch/workflows/sdxl_turbo.json).
