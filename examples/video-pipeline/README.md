# video-pipeline (ffmpeg final-assembly smoke)

Demonstrates a complete vamp pipeline that combines every backend type into a
single MP4:

1. `script` (text stage, `reasoning` capability -> llama-server profile)
   drafts a 2-sentence narration about the supplied `topic` input.
2. `voiceover` (audio stage) renders the script through
   [Piper TTS](https://github.com/rhasspy/piper), writing `voiceover.wav`.
3. `render` (comfyui stage, `image_gen` capability -> ComfyUI profile)
   generates a single still illustrating the topic, writing `cover.png`.
4. `assemble` (ffmpeg stage) loops the still for the audio duration and muxes
   in the voiceover, writing `final.mp4`.

The `assemble` stage uses `type: ffmpeg`. Like audio stages, it shells out to
a local binary and does **not** activate a vibe profile, so the scheduler
groups it on its own without trying to bind a capability mapping. The
executor appends `-y <output>` (under the run dir) after the user-supplied
args, so the YAML doesn't manage the destination path.

## Prerequisites

This example exercises three independent runtimes; you need all of them
installed for an end-to-end run. Each piece is covered by its own example
smoke pipeline if you want to verify it in isolation first.

1. **Piper TTS** (audio stage). See
   [`examples/voiceover-pipeline`](../voiceover-pipeline/README.md). The
   short version:
   ```
   pipx install piper-tts
   mkdir -p ~/.local/share/piper-voices
   cd ~/.local/share/piper-voices
   curl -L -O https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx
   curl -L -O https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json
   ```
2. **ComfyUI + SDXL-Turbo** (image_gen stage). See
   [`examples/comfyui-image-batch`](../comfyui-image-batch/README.md). The
   short version:
   ```
   git clone --depth 1 https://github.com/comfyanonymous/ComfyUI ~/ComfyUI
   python3 -m venv ~/ComfyUI/.venv
   ~/ComfyUI/.venv/bin/pip install -r ~/ComfyUI/requirements.txt
   hf download stabilityai/sdxl-turbo sd_xl_turbo_1.0_fp16.safetensors \
     --local-dir ~/ComfyUI/models/checkpoints/
   ```
3. **ffmpeg** (assemble stage). The binary just needs to be reachable on
   `$PATH` as `ffmpeg`; override with the stage's `binary:` field if you
   have it elsewhere.
   ```
   # debian / ubuntu
   sudo apt install ffmpeg
   # arch / cachyos
   sudo pacman -S ffmpeg
   # macos
   brew install ffmpeg
   ```
4. **vibe profiles + capabilities**. Two profiles must exist, one exposing
   `reasoning` (any llama-server-backed profile) and one exposing `image_gen`
   (a ComfyUI-backed profile). Map them in
   `~/.config/vamp/capabilities.yaml`:
   ```yaml
   capabilities:
     reasoning: <your-llama-profile>
     image_gen: <your-comfyui-profile>
   ```

## Run

```
vamp run examples/video-pipeline/pipeline.yaml --input topic="why robots dream"
```

vibe will swap profiles between the text and comfyui stages (one GPU, one
profile active at a time). The audio and ffmpeg stages do not need a profile
and run as ordinary subprocesses. When complete, the run dir contains:

- `script.txt` (the narration)
- `voiceover.wav` (the synthesized audio)
- `cover.png` (the rendered still)
- `final.mp4` (audio + image combined)

Play it with any video player:

```
mpv $(vamp runs latest)/final.mp4
```
