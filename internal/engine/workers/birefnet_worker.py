# mlxlab's cutout worker (an image → the same image with its background transparent): keeps one BiRefNet model
# loaded (PyTorch: CUDA, else Apple's MPS, else the CPU) and answers requests.
#
# BiRefNet (ZhengPeng7, MIT) predicts a soft foreground mask; the mask becomes the alpha channel as it is (sigmoid,
# resized back to the image's size, never thresholded): fine hair and soft edges stay half transparent. BiRefNet
# drops what only tints the background (a glow, a soft shadow); on a plain background those come back half
# transparent by colour to alpha (glow) — against a smooth field fitted to the backdrop (its gradient and vignette
# included) and only near the object, so the backdrop leaves no veil.
# The colours at the edge are estimated anew (blur fusion, github.com/Photoroom/fast-foreground-estimation): a pixel
# half covered by the object no longer carries the old background into a picture placed on another one (no light
# grey fringe on a dark slide). Nothing is downloaded here: the snapshot (birefnet.py, BiRefNet_config.py,
# config.json, model.safetensors) comes in as path.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "birefnet", "path": "/snapshot", "size": 1024, "image": "/in.png", "output": "/out.png",
#             "refine": true, "glow": true, "maxBytes": 15728640}
#             size: the side the model sees (1024; the HR models 2048); refine: estimate the edge colours (default
#             true); glow: on a plain background keep what only tints it — a glow, a soft shadow — half transparent
#             (default true); maxBytes: a PNG larger than this is written as a WebP with alpha instead (0: no limit)
#   events:  {"type": "ready"|"status"|"progress"|"log"|"result"|"error", …}
#   result:  {"type": "result", "file": the PNG (or WebP), "seconds": the cutout's time (with writing the file), "width", "height",
#             "peakGB": the GPU's memory}
import json
import os
import sys
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting BiRefNet …")
os.environ.setdefault("PYTORCH_ENABLE_MPS_FALLBACK", "1")  # ops missing on MPS (deform_conv2d) run on the CPU
os.environ.setdefault("HF_HUB_OFFLINE", "1")
# the model's own code (birefnet.py, trust_remote_code) is copied here, not into ~/.cache/huggingface/modules
os.environ.setdefault("HF_MODULES_CACHE", os.path.join(os.path.dirname(os.path.abspath(__file__)), "hf-modules"))
import numpy as np  # noqa: E402
import torch  # noqa: E402
import torch.nn.functional as F  # noqa: E402
from PIL import Image, ImageOps  # noqa: E402
from transformers import AutoModelForImageSegmentation  # noqa: E402

torch.set_float32_matmul_precision("high")

MEAN = torch.tensor([0.485, 0.456, 0.406]).view(1, 3, 1, 1)
STD = torch.tensor([0.229, 0.224, 0.225]).view(1, 3, 1, 1)

loaded = {"key": None, "model": None}


def device():
    if torch.cuda.is_available():
        return "cuda"
    if torch.backends.mps.is_available():
        return "mps"
    return "cpu"


def sync(dev):
    if dev == "cuda":
        torch.cuda.synchronize()
    elif dev == "mps":
        torch.mps.synchronize()


def peak_gb(dev):
    if dev == "cuda":
        return torch.cuda.max_memory_allocated() / 2**30
    if dev == "mps":
        return torch.mps.driver_allocated_memory() / 2**30
    return 0.0


def model_for(req):
    dev = req.get("device") or device()
    key = (req.get("model"), req["path"], dev)
    if loaded["key"] != key:
        loaded["key"], loaded["model"] = None, None
        if dev == "mps":
            torch.mps.empty_cache()
        emit(type="status", text=f"loading {req.get('model', 'BiRefNet')} ({dev}) …")
        t0 = time.time()
        model = AutoModelForImageSegmentation.from_pretrained(req["path"], trust_remote_code=True, local_files_only=True)
        # half precision on CUDA (upstream's recommendation: the same masks at half the memory and time); float32
        # elsewhere — some checkpoints are stored in fp16 (the HR matting one), which MPS and the CPU run poorly
        model.to(dev, dtype=torch.float16 if dev == "cuda" else torch.float32).eval()
        loaded["key"], loaded["model"] = key, model
        emit(type="log", text=f"loaded {req.get('model')} on {dev} in {time.time() - t0:.1f} s")
    return loaded["model"], dev


def box_blur(x, r):
    # cv2.blur: the mean of an r×r window (edges repeated), on a [1, C, H, W] tensor
    a, b = r // 2, r - 1 - r // 2
    return F.avg_pool2d(F.pad(x, (a, b, a, b), mode="replicate"), r, stride=1)


def fuse(image, fg, bg, alpha, r):
    blurred_alpha = box_blur(alpha, r)
    blurred_fg = box_blur(fg * alpha, r) / (blurred_alpha + 1e-5)
    blurred_bg = box_blur(bg * (1 - alpha), r) / ((1 - blurred_alpha) + 1e-5)
    fg = blurred_fg + alpha * (image - alpha * blurred_fg - (1 - alpha) * blurred_bg)
    return fg.clamp(0, 1), blurred_bg


def foreground(image, alpha):
    # blur fusion foreground estimation, twice (a wide pass, then a narrow one), as BiRefNet's own refine_foreground;
    # the radii follow the picture's size (90 and 6 at 1024 px)
    scale = max(image.shape[-2:]) / 1024
    fg, bg = fuse(image, image, image, alpha, max(3, round(90 * scale)))
    return fuse(image, fg, bg, alpha, max(3, round(6 * scale)))[0]


def plain_background(image, alpha):
    # the background when it is plain (a studio backdrop, a generated picture's "on a light grey background"): a smooth
    # field — a quadratic in x and y per channel, so a backdrop's gradient and vignette fit too — fitted to the pixels
    # far from the object (the mask leaves them out and they lie outside its reach, so a glow does not count), and how
    # far the pixels stray from it (95th percentile of the largest channel difference). None: too little background,
    # or it is not plain.
    h, w = image.shape[-2:]
    subject = (alpha > 0.5).float()
    reach = near(subject, h, w)
    far = (alpha[0, 0] < 0.05) & (reach[0, 0] < 0.01)
    if far.float().mean() < 0.15:
        return None
    ys, xs = torch.meshgrid(torch.linspace(-1, 1, h, device=image.device), torch.linspace(-1, 1, w, device=image.device), indexing="ij")
    basis = torch.stack([torch.ones_like(xs), xs, ys, xs * xs, xs * ys, ys * ys], dim=-1)  # [h, w, 6]
    pts = basis[far]
    px = image[0].permute(1, 2, 0)[far]  # [n, 3]
    if pts.shape[0] > 40000:
        pick = torch.randperm(pts.shape[0], device=pts.device)[:40000]
        pts, px = pts[pick], px[pick]
    coef = torch.linalg.lstsq(pts.cpu(), px.cpu()).solution.to(image.device)  # [6, 3]
    field = (basis @ coef).permute(2, 0, 1).unsqueeze(0).clamp(1e-3, 1 - 1e-3)
    spread = torch.quantile((image[0].permute(1, 2, 0)[far] - field[0].permute(1, 2, 0)[far]).abs().amax(dim=1).float().cpu()[:200000], 0.95).item()
    if spread > 0.06:
        return None
    return field, spread, reach


def near(subject, h, w):
    # how close each pixel is to the object, 1 next to it fading to 0 at about a quarter of the picture's longer side:
    # a glow or a shadow belongs to the object, a stain in a corner of the backdrop does not
    r = max(3, round(max(h, w) / 8)) | 1
    grown = F.max_pool2d(subject, r, stride=1, padding=r // 2)
    return box_blur(grown, r)


def glow(image, field, spread, reach):
    # colour to alpha against the background field: how much of each pixel is not background (the largest channel's
    # share of the way from the background's value to black or white) and the colour it then has. A soft glow, a
    # shadow come out half transparent in their own colour; the noise floor (the backdrop's own spread, at least 4 %)
    # stays 0, and the alpha is smoothed a little (no blotches from a compressed picture's noise) and fades out away
    # from the object.
    d = image - field
    a = torch.where(d > 0, d / (1 - field), -d / field).amax(dim=1, keepdim=True).clamp(0, 1)
    colour = (field + d / a.clamp_min(1e-3)).clamp(0, 1)
    floor = max(0.04, 2.5 * spread)
    a = ((a - floor) / (1 - floor)).clamp(0, 1)
    h, w = image.shape[-2:]
    a = box_blur(a, max(1, round(max(h, w) / 400)))
    return a * reach.clamp(0, 1), colour


def cutout(req):
    model, dev = model_for(req)
    size = int(req.get("size") or 1024)
    src = ImageOps.exif_transpose(Image.open(req["image"]))
    rgb = src.convert("RGB")
    w, h = rgb.size
    emit(type="status", text="cutting out …")
    emit(type="progress", step=0, total=2)
    t0 = time.time()
    image = torch.from_numpy(np.asarray(rgb, dtype=np.float32) / 255.0).permute(2, 0, 1).unsqueeze(0)
    x = F.interpolate(image, size=(size, size), mode="bilinear", align_corners=False, antialias=True)
    x = ((x - MEAN) / STD).to(dev)
    if dev == "cuda":
        x = x.half()
    with torch.inference_mode():
        pred = model(x)[-1].sigmoid().float()  # the finest of the decoder's outputs
    # the soft mask back to the image's size: the alpha as predicted, never cut to 0 / 1
    alpha = F.interpolate(pred, size=(h, w), mode="bilinear", align_corners=False).clamp(0, 1)
    if src.mode in ("RGBA", "LA") or (src.mode == "P" and "transparency" in src.info):
        # a picture that was transparent already stays so where it was
        had = torch.from_numpy(np.asarray(src.convert("RGBA"), dtype=np.float32)[..., 3] / 255.0)
        alpha = alpha * had.to(alpha.device).view(1, 1, h, w)
    emit(type="progress", step=1, total=2)
    image = image.to(alpha.device)
    if req.get("refine", True):
        fg = foreground(image, alpha)
    else:
        fg = image
    if req.get("glow", True) and (plain := plain_background(image, alpha)) is not None:
        # BiRefNet keeps the object and drops what only tints the background (a glow, a soft shadow): on a plain
        # background those come back half transparent wherever they are more than the mask
        ga, colour = glow(image, *plain)
        more = ga > alpha
        alpha, fg = torch.where(more, ga, alpha), torch.where(more, colour, fg)
        emit(type="log", text=f"plain background (spread {plain[1]:.3f}): glows and shadows near the object kept")
    sync(dev)
    rgba = torch.cat([fg, alpha], dim=1)[0].permute(1, 2, 0).mul(255).round().byte().cpu().numpy()
    out = req["output"]
    Image.fromarray(rgba, "RGBA").save(out, optimize=False, compress_level=6)
    limit = int(req.get("maxBytes") or 0)
    if limit and os.path.getsize(out) > limit:
        # too large for the board as a PNG: a WebP keeps the alpha (a JPEG would lose it)
        webp = os.path.splitext(out)[0] + ".webp"
        Image.fromarray(rgba, "RGBA").save(webp, quality=92, alpha_quality=100, method=4)
        emit(type="log", text=f"the PNG is {os.path.getsize(out) / 2**20:.1f} MB: written as a WebP of {os.path.getsize(webp) / 2**20:.1f} MB")
        os.remove(out)
        out = webp
    emit(type="progress", step=2, total=2)
    emit(type="result", file=out, seconds=time.time() - t0, width=w, height=h, peakGB=peak_gb(dev))


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        cutout(json.loads(line))
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
