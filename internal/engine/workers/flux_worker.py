# mlxlab's mflux worker (FLUX.2 klein, Qwen-Image): keeps one mflux model loaded and answers requests.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "flux2-klein-4b" | "qwen-image" | "qwen-image-edit" | "qwen-image-2.1", "path": "/dir", "quantize": 8, "prompt": "…", "width": 1024, "height": 1024, "steps": 4,
#             "seed": 1, "guidance": 1.0, "images": ["ref.png"], "negativePrompt": "…", "output": "/path/out.png"}
#   events:  {"type": "status"|"progress"|"result"|"error", …}
import json
import os
import sys
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting mflux …")
import mlx.core as mx  # noqa: E402
from mflux.models.common.config.model_config import ModelConfig  # noqa: E402
from mflux.models.flux2.variants import Flux2Klein, Flux2KleinEdit  # noqa: E402

QWEN = ("qwen-image", "qwen-image-edit", "qwen-image-2.1")

loaded = {"key": None, "model": None}


class Progress:
    def __init__(self):
        self.total = 0

    def call_before_loop(self, seed, prompt, latents, config, **kw):
        self.total = config.num_inference_steps
        emit(type="status", text="denoising …")
        emit(type="progress", step=0, total=self.total)

    def call_in_loop(self, t, seed, prompt, latents, config, time_steps, **kw):
        emit(type="progress", step=t + 1, total=config.num_inference_steps)

    def call_after_loop(self, seed, prompt, latents, config, **kw):
        emit(type="status", text="decoding …")


def model_class(name, edit):
    # FLUX.2 klein: Flux2Klein, with reference images Flux2KleinEdit (it needs at least one). Qwen-Image: one class
    # per model, the edit model only edits and the others only draw.
    if name == "qwen-image":
        if edit:
            raise ValueError("Qwen-Image makes pictures from text only (editing: qwen-image-edit)")
        from mflux.models.qwen.variants.txt2img.qwen_image import QwenImage

        return QwenImage
    if name == "qwen-image-edit":
        if not edit:
            raise ValueError("Qwen-Image-Edit needs at least one reference image")
        from mflux.models.qwen.variants.edit.qwen_image_edit import QwenImageEdit

        return QwenImageEdit
    if name == "qwen-image-2.1":
        if edit:
            raise ValueError("Qwen-Image 2.1 makes pictures from text only")
        from mflux.models.qwen21.variants.txt2img.qwen_image_21 import QwenImage21

        return QwenImage21
    return Flux2KleinEdit if edit else Flux2Klein


def model_for(req):
    edit = bool(req.get("images"))
    cls = model_class(req["model"], edit)
    key = (req["model"], req.get("path"), req.get("quantize"), edit)
    if loaded["key"] != key:
        loaded["key"], loaded["model"] = None, None
        mx.clear_cache()
        q = req.get("quantize")
        emit(type="status", text=f"loading {req['model']} ({'bf16' if not q else str(q) + '-bit'}) …")
        t0 = time.time()
        model = cls(model_config=ModelConfig.from_name(req["model"]), model_path=req.get("path"), quantize=q or None)
        model.callbacks.register(Progress())
        loaded["key"], loaded["model"] = key, model
        emit(type="log", text=f"loaded {req['model']} in {time.time() - t0:.1f} s")
    return loaded["model"]


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        model = model_for(req)
        mx.reset_peak_memory()
        t0 = time.time()
        args = dict(
            seed=int(req["seed"]),
            prompt=req["prompt"],
            num_inference_steps=int(req["steps"]),
            width=int(req["width"]),
            height=int(req["height"]),
            guidance=float(req.get("guidance") or 1.0),
        )
        if req.get("images"):
            args["image_paths"] = req["images"]
        if req["model"] in QWEN:
            # true CFG against the negative prompt (Qwen-Image: " " when empty; 2.1 skips the pass without one)
            args["negative_prompt"] = req.get("negativePrompt") or None
        image = model.generate_image(**args)
        image.save(path=req["output"], export_json_metadata=False, overwrite=True)
        emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=mx.get_peak_memory() / 2**30)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
