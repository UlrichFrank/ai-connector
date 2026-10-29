# A generic runtime's worker (docs/models.md, "Runtimes"): image restoration models through spandrel.
# request: {"model": "<weights file>", "path": "<the model's folder>", "images": ["<one image>"],
#           "params": {"maxSide": 4096}, "output": "<png>"}
import json
import os
import sys
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")


emit(type="status", text="starting spandrel …")

import numpy as np  # noqa: E402
import torch  # noqa: E402
from PIL import Image  # noqa: E402
from spandrel import ModelLoader  # noqa: E402

device = "cuda" if torch.cuda.is_available() else ("mps" if torch.backends.mps.is_available() else "cpu")
loaded = {"key": None, "model": None}


def model_for(req):
    key = os.path.join(req["path"], req["model"])
    if loaded["key"] != key:
        emit(type="status", text=f"loading {req['model']} on {device} …")
        loaded["model"] = ModelLoader().load_from_file(key).to(device).eval()
        loaded["key"] = key
    return loaded["model"]


def peak_gb():
    if device == "cuda":
        return torch.cuda.max_memory_allocated() / 2**30
    if device == "mps":
        return torch.mps.driver_allocated_memory() / 2**30
    return 0.0


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        images = req.get("images") or []
        if len(images) != 1:
            raise ValueError("an upscaler takes exactly one image")
        model = model_for(req)
        t0 = time.time()
        img = Image.open(images[0]).convert("RGB")
        x = torch.from_numpy(np.array(img)).permute(2, 0, 1).float().div(255).unsqueeze(0).to(device)
        emit(type="progress", step=0, total=1)
        with torch.no_grad():
            y = model(x).clamp(0, 1)
        out = Image.fromarray((y[0].permute(1, 2, 0).cpu().numpy() * 255).round().astype(np.uint8))
        max_side = int(req.get("params", {}).get("maxSide", 4096))
        if max(out.size) > max_side:
            f = max_side / max(out.size)
            out = out.resize((round(out.width * f), round(out.height * f)), Image.LANCZOS)
        out.save(req["output"])
        emit(type="progress", step=1, total=1)
        emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=peak_gb(), width=out.width, height=out.height)
    except Exception as e:
        traceback.print_exc()
        emit(type="error", text=str(e))
