# A generic runtime's worker (docs/models.md, "Runtimes"): open-vocabulary detection with SAM 3.1 through mlx-vlm.
# request: {"path": "<the model's folder>", "images": ["<image>", …], "texts": ["ball", …] (or "prompt"),
#           "params": {"threshold": 0.5, "maxBoxes": 20}, "output": "<json>"}
# result (the output file, and the job's result): {"images": [{"image": 0, "width": …, "height": …,
#   "detections": [{"text": "ball", "box": [x1, y1, x2, y2], "score": 0.93}, …]}, …]} — boxes in the image's pixels
import json
import os
import sys
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")


emit(type="status", text="starting SAM 3.1 …")

import mlx.core as mx  # noqa: E402
from PIL import Image  # noqa: E402
from mlx_vlm.models.sam3.generate import Sam3Predictor  # noqa: E402
from mlx_vlm.models.sam3.processing_sam3 import Sam3Processor  # noqa: E402
from mlx_vlm.models.sam3_1.generate import predict_multi  # noqa: E402
from mlx_vlm.utils import load_model  # noqa: E402
from pathlib import Path  # noqa: E402

loaded = {"path": None, "predictor": None}


def predictor_for(req):
    if loaded["path"] != req["path"]:
        emit(type="status", text="loading SAM 3.1 …")
        model = load_model(Path(req["path"]))
        loaded["predictor"] = Sam3Predictor(model, Sam3Processor.from_pretrained(req["path"]))
        loaded["path"] = req["path"]
    return loaded["predictor"]


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        images = req.get("images") or []
        texts = req.get("texts") or ([req["prompt"]] if req.get("prompt") else [])
        if not images:
            raise ValueError("detect needs at least one image")
        if not texts:
            raise ValueError("detect needs what to find: texts (or a prompt)")
        params = req.get("params", {})
        threshold = float(params.get("threshold", 0.5))
        max_boxes = int(params.get("maxBoxes", 20))
        pred = predictor_for(req)
        t0 = time.time()
        out = []
        for i, path in enumerate(images):
            emit(type="progress", step=i, total=len(images))
            img = Image.open(path).convert("RGB")
            dets = []
            for text in texts:
                r = predict_multi(pred, img, [text], score_threshold=max(threshold, 1e-3))  # 0 would mean "the default"
                for b, s in zip(r.boxes, r.scores):
                    if float(s) >= threshold:
                        dets.append({"text": text, "box": [round(float(x), 1) for x in b], "score": round(float(s), 3)})
            dets.sort(key=lambda d: -d["score"])
            out.append({"image": i, "width": img.width, "height": img.height, "detections": dets[:max_boxes]})
        with open(req["output"], "w") as f:
            json.dump({"images": out}, f)
        emit(type="progress", step=len(images), total=len(images))
        emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=mx.get_peak_memory() / 2**30)
    except Exception as e:
        traceback.print_exc()
        emit(type="error", text=str(e))
