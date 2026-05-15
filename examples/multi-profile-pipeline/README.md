# multi-profile-pipeline

A minimal example that demonstrates `vamp`'s reason for existing: running
each pipeline stage on a *different* vibe profile (and therefore a
different loaded model), on a single GPU, in a single run.

## What it shows

Two stages, two profiles:

1. **`outline`** — capability `fast_drafting`, mapped to the small
   `fast` profile (Qwen2.5-Coder-7B Q4_K_M, ~5 GB). Cheap and quick,
   appropriate for drafting a short bullet list.
2. **`expand`** — capability `careful_reasoning`, mapped to the larger
   `code` profile (Qwen3.6-27B Q6_K, ~26 GB VRAM in the reference
   setup). Heavier, used for polished prose.

Between stages the executor calls `vibe`'s control plane to swap the
active profile; vibe enforces one active profile at a time, so this
unloads the 7B model and loads the 27B model before stage 2 runs.

It also exercises both prompt forms: stage 1 inlines `prompt`, stage 2
uses `prompt_file` (`prompts/expand.tmpl`).

## Prerequisites

- Both profiles installed:
  - `~/.config/vibe/profiles/fast.yaml` (see `profiles/fast.example.yaml`)
  - `~/.config/vibe/profiles/code.yaml` (see `profiles/code.example.yaml`)
- The corresponding GGUF files pulled: `vibe pull fast` and `vibe pull code`.
- `~/.config/vamp/capabilities.yaml` containing at least:

  ```yaml
  capabilities:
    fast_drafting: fast
    careful_reasoning: code
  ```

- A running daemon: `vibe daemon &`.

## Run it

```sh
vamp run examples/multi-profile-pipeline/pipeline.yaml \
  --input topic="how prompt caching works in LLM serving"
```

Outputs land under `~/.local/state/vamp/runs/<timestamp>-multi-profile-demo/`:

- `outline.md` — the bullet outline from the 7B model.
- `article.md` — the expanded article from the 27B model.
- `inputs.json` — the inputs you passed on the CLI.

Watch the executor log — you should see two distinct lines of the form
`activating profile "fast"` and `activating profile "code"`, with the
swap happening between them.
