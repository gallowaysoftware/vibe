# image-batch (ComfyUI smoke)

Demonstrates a cross-backend vamp pipeline:

1. `prompts` (text stage, `reasoning` capability → llama-server profile) generates a JSON array of scene descriptions and seeds.
2. `render` (comfyui stage, `image_gen` capability → ComfyUI profile) iterates per scene, mutates the SDXL-Turbo workflow's prompt and seed nodes, submits to ComfyUI, copies each PNG into the run dir at `assets/img_<i>.png`.

vibe swaps profiles between the two stages: the llama-server backend stops and ComfyUI starts (one GPU, one profile active at a time).

Prerequisites: see top-level `TODO.md` for the ComfyUI install path; for now, install manually:
```
git clone --depth 1 https://github.com/comfyanonymous/ComfyUI ~/ComfyUI
python3 -m venv ~/ComfyUI/.venv
~/ComfyUI/.venv/bin/pip install -r ~/ComfyUI/requirements.txt
hf download stabilityai/sdxl-turbo sd_xl_turbo_1.0_fp16.safetensors \
  --local-dir ~/ComfyUI/models/checkpoints/
```

Plus a vibe profile at `~/.config/vibe/profiles/comfyui.yaml` and a capability mapping `image_gen: comfyui` in `~/.config/vamp/capabilities.yaml`.

Run:
```
vamp run examples/comfyui-image-batch/pipeline.yaml --input theme="neon rainy alley"
```
