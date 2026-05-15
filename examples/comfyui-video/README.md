# comfyui-video (plumbing demo)

Demonstrates that vamp's ComfyUI executor can collect non-image outputs —
i.e. files produced by `SaveVideo` (modern), `VHS_VideoCombine`, or
`SaveAnimatedWEBP` nodes — and copy them into the run dir at the
`output:` template path.

**This example is not a runnable smoke against a real model out of the
box.** The referenced checkpoint (LTX-Video) is multi-GB and has to be
downloaded manually. The point of the example is to show the YAML / JSON
shape the ComfyUI client now supports; the executor-level plumbing is
covered by tests under `internal/comfyui/` and `internal/vamp/`. A real
end-to-end smoke against a downloaded model is tracked in the top-level
`TODO.md`.

## What's in here

- `pipeline.yaml` — one `comfyui` stage that templates the prompt and seed
  into `workflows/video_basic.json` and writes the rendered video to
  `<run-dir>/assets/video.mp4`.
- `workflows/video_basic.json` — minimal LTX-Video-style workflow:
  `CheckpointLoaderSimple` → two `CLIPTextEncode` nodes (positive +
  negative) → `EmptyHunyuanLatentVideo` → `KSampler` → `VAEDecode` →
  `SaveVideo`. The `SaveVideo` output emits files under the `videos`
  bucket that this PR teaches the client to recognise.

## To actually run it

You'd need:

1. ComfyUI installed (see top-level `TODO.md` step 1–3 of the install
   path).
2. The video-relevant custom nodes — `SaveVideo` ships in stock ComfyUI
   recent builds; for older builds, replace `SaveVideo` in the workflow
   with `VHS_VideoCombine` (from the ComfyUI-VideoHelperSuite custom node
   pack) and rewire `images: ["6", 0]`.
3. An LTX-Video checkpoint in `~/ComfyUI/models/checkpoints/`. The
   workflow references `ltx-video-2b-v0.9.safetensors` from the public
   Lightricks repo: https://huggingface.co/Lightricks/LTX-Video. Pull it
   with:

   ```
   hf download Lightricks/LTX-Video ltx-video-2b-v0.9.safetensors \
     --local-dir ~/ComfyUI/models/checkpoints/
   ```

   Heads up: this is several GB.

4. A vibe profile pointing at the ComfyUI install (same shape as the
   `comfyui-image-batch` example) and a capability mapping
   `image_gen: comfyui` in `~/.config/vamp/capabilities.yaml`.

Then:

```
vamp run examples/comfyui-video/pipeline.yaml \
  --input scene="a robot walking through a neon alley" \
  --input seed=42
```

The rendered MP4 lands at `<run-dir>/assets/video.mp4`.

## Where the plumbing lives

- `internal/comfyui/client.go` — `NodeOutputs` carries `Images`, `Videos`,
  and `Gifs` slices, mirroring the per-kind buckets ComfyUI emits in
  `/history`.
- `internal/vamp/comfyui_executor.go` — `collectOutputs` flattens every
  bucket into a single deterministic list; the existing "exactly 1 output
  per stage" guard still applies, with a richer error message when the
  files span more than one kind (e.g. an image + a video, which usually
  means an unintended `SaveImage` was left in the workflow).
