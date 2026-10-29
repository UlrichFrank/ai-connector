# mlxlab's mflux worker (FLUX.2 klein, Z-Image Turbo, Qwen-Image, SeedVR2): keeps one mflux model loaded and answers
# requests.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "flux2-klein-4b" | "z-image-turbo" | "qwen-image" | "qwen-image-edit" | "qwen-image-2.1", "path": "/dir", "quantize": 8, "prompt": "…", "width": 1024, "height": 1024, "steps": 4,
#             "seed": 1, "guidance": 1.0, "images": ["ref.png"], "negativePrompt": "…", "output": "/path/out.png"}
#   upscale: {"model": "seedvr2-3b" | "seedvr2-7b", "upscale": true, "path": "/dir", "quantize": 0, "image": "in.png",
#             "scale": 2, "maxSide": 4096, "seed": 1, "output": "/path/out.png"}
#   events:  {"type": "status"|"progress"|"result"|"error", …}
import importlib
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


def load(spec):
    # "module:Class", as the model's manifest names it (runtimeArgs.loader / editLoader)
    module, _, name = spec.partition(":")
    return getattr(importlib.import_module(module), name)


def model_class(req, edit):
    # the class the model's manifest names for text to image (loader) and for editing with reference images
    # (editLoader); a model with only one does only that. None named: FLUX.2 klein's (Flux2Klein, Flux2KleinEdit).
    loader, edit_loader = req.get("loader"), req.get("editLoader")
    if not loader and not edit_loader:
        return Flux2KleinEdit if edit else Flux2Klein
    if edit and not edit_loader:
        raise ValueError(f"{req['model']} makes pictures from text only")
    if not edit and not loader:
        raise ValueError(f"{req['model']} needs at least one reference image")
    return load(edit_loader if edit else loader)


def seedvr2(name):
    # SeedVR2's 3B and 7B: the same class, the config says which transformer (and which weights file)
    from mflux.models.seedvr2 import SeedVR2

    config = ModelConfig.seedvr2_7b() if name == "seedvr2-7b" else ModelConfig.seedvr2_3b()
    return lambda model_config, **kw: SeedVR2(model_config=config, **kw)


def upscale_resolution(path, scale, max_side):
    # the shorter side's new length (mflux's resolution): scale times as long, less when the longer side would pass
    # max_side (as engine.UpscaleSize works it out); the size as mflux sees it (EXIF orientation applied)
    from mflux.utils.exif_orientation import open_oriented

    w, h = open_oriented(path).size
    short, long = min(w, h), max(w, h)
    if long * scale > max_side:
        return short * max_side // long
    return short * scale


def model_for(req):
    edit = bool(req.get("images"))
    cls = seedvr2(req["model"]) if req.get("upscale") else model_class(req, edit)
    key = (req["model"], req.get("path"), req.get("quantize"), edit)
    if loaded["key"] != key:
        loaded["key"], loaded["model"] = None, None
        mx.clear_cache()
        q = req.get("quantize")
        emit(type="status", text=f"loading {req['model']} ({'as stored' if not q else str(q) + '-bit'}) …")
        t0 = time.time()
        config = None if req.get("upscale") else ModelConfig.from_name(req["model"])
        model = cls(model_config=config, model_path=req.get("path"), quantize=q or None)
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
        if req.get("upscale"):
            # one step; mflux tiles the VAE (512 px tiles) on its own, so a large result fits in memory
            image = model.generate_image(
                seed=int(req["seed"]),
                image_path=req["image"],
                resolution=upscale_resolution(req["image"], int(req["scale"]), int(req["maxSide"])),
                softness=float(req.get("softness") or 0.0),
            )
            image.save(path=req["output"], export_json_metadata=False, overwrite=True)
            emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=mx.get_peak_memory() / 2**30)
            continue
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
