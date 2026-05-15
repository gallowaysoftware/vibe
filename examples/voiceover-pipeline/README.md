# voiceover-pipeline (audio smoke)

Demonstrates a vamp pipeline that mixes a text stage with the new `type: audio`
stage:

1. `script` (text stage, `reasoning` capability -> llama-server profile) drafts
   a short narration about the supplied `topic` input.
2. `voiceover` (audio stage) renders the script through [Piper
   TTS](https://github.com/rhasspy/piper), writing
   `voiceover.wav` to the run dir.

Audio stages do not activate a vibe profile -- they shell out to a local
`piper` binary, so they run as ordinary subprocesses outside the
backend-swapping machinery the text/comfyui stages use.

## Prerequisites

1. Install Piper. The simplest path is `pipx`:
   ```
   pipx install piper-tts
   ```
   Or use your distro package, or follow the
   [upstream install guide](https://github.com/rhasspy/piper#installation).
   The binary just needs to be reachable on `$PATH` as `piper`; override with
   the stage's `binary:` field if you have it elsewhere.
2. Download a voice file (the `en_US-lessac-medium` voice referenced by the
   example is a good default) into `~/.local/share/piper-voices`:
   ```
   mkdir -p ~/.local/share/piper-voices
   cd ~/.local/share/piper-voices
   curl -L -O https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx
   curl -L -O https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx.json
   ```
   Browse the full catalog at
   [huggingface.co/rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices).
   Override the directory per-stage with `voices_dir:`.
3. A vibe profile exposing a `reasoning` capability (any llama-server-backed
   profile works) and the matching capability mapping in
   `~/.config/vamp/capabilities.yaml`:
   ```
   capabilities:
     reasoning: <your-llama-profile>
   ```

## Run

```
vamp run examples/voiceover-pipeline/pipeline.yaml --input topic="why robots dream"
```

The run dir will contain `script.txt` (the narration) and `voiceover.wav`
(the synthesized audio). Play it with any WAV-capable player:

```
aplay $(vamp runs latest)/voiceover.wav
```
