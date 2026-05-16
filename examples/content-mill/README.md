# content-mill

Full vamp pipeline stitching every stage type together: LLM scripting,
LLM JSON shot-list, ComfyUI image gen, Piper TTS, ffmpeg muxing, and a
Slack/Discord webhook on completion.

Prereqs:
- vibe daemon running (`vibe daemon &`)
- `code` and `comfyui` profiles in `~/.config/vibe/profiles/`
- `capabilities.yaml` maps `reasoning: code` and `image_gen: comfyui`
- `piper` on `$PATH` with `en_US-lessac-medium.onnx` under
  `~/.local/share/piper-voices/`
- `ffmpeg` on `$PATH`
- `VAMP_SLACK_WEBHOOK=https://hooks.slack.com/...` env var (or drop
  the `notify` stage if you don't want the announcement)

Run:
```
VAMP_SLACK_WEBHOOK=https://hooks.slack.com/... \
  vamp run examples/content-mill/pipeline.yaml \
  --input topic="bioluminescent forests at dusk"
```
