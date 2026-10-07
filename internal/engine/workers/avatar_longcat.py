# mlxlab's LongCat-Video-Avatar 1.5 worker (github.com/xocialize/longcat-avatar-mlx, MIT; weights
# mlx-community/LongCat-Video-Avatar-1.5-*-dmd-merged): a portrait + speech -> a talking clip (head and body motion).
# Keeps one pipeline loaded and answers requests. The repository's own script has its demo inputs hard-coded
# (load_demo_inputs), so this is a thin driver around its pipeline.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3.
#   request: {"model": "/weights", "audio": "clip.wav", "image": "portrait.png", "prompt": "…", "width": 256,
#             "height": 432, "frames": 29, "seed": 1, "ffmpeg": "/opt/homebrew/bin/ffmpeg", "output": "/path/clip.mp4"}
#   events:  {"type": "status"|"progress"|"log"|"result"|"error", …}
# The video is written without sound at 25 fps (the model's audio feature rate); mlxlab muxes the original wav.
import io
import json
import os
import pathlib
import subprocess
import sys
import tarfile
import time
import traceback
import urllib.request

events = os.fdopen(3, "w", buffering=1)

FPS = 25
COMMIT = sys.argv[1] if len(sys.argv) > 1 else ""


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


def ensure_avatar_package():
    # the package's setuptools list leaves out longcat_video_avatar.models.avatar (the DiT): put it in place from
    # the same pinned commit
    import importlib.util

    if importlib.util.find_spec("longcat_video_avatar.models.avatar") is not None:
        return
    import longcat_video_avatar.models as pkg

    dst = pathlib.Path(pkg.__file__).parent / "avatar"
    url = f"https://codeload.github.com/xocialize/longcat-avatar-mlx/tar.gz/{COMMIT}"
    emit(type="log", text=f"fetching longcat_video_avatar/models/avatar from {url}")
    data = urllib.request.urlopen(url, timeout=120).read()
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as tf:
        for m in tf.getmembers():
            parts = m.name.split("/", 1)
            if len(parts) == 2 and parts[1].startswith("longcat_video_avatar/models/avatar/") and m.isfile():
                out = dst / parts[1].split("longcat_video_avatar/models/avatar/", 1)[1]
                out.parent.mkdir(parents=True, exist_ok=True)
                out.write_bytes(tf.extractfile(m).read())
    if not (dst / "__init__.py").exists():
        raise RuntimeError("the longcat-avatar-mlx archive has no longcat_video_avatar/models/avatar")


emit(type="status", text="starting LongCat-Video-Avatar …")
import mlx.core as mx  # noqa: E402
import mlx.nn as nn  # noqa: E402
import numpy as np  # noqa: E402

ensure_avatar_package()

from longcat_video_avatar.audio_process import loudness_normalize  # noqa: E402
from longcat_video_avatar.models.autoencoder_kl_wan import AutoencoderKLWan  # noqa: E402
from longcat_video_avatar.models.avatar.longcat_video_dit_avatar import (  # noqa: E402
    LongCatVideoAvatarTransformer3DModel,
)
from longcat_video_avatar.models.umt5 import UMT5EncoderModel  # noqa: E402
from longcat_video_avatar.models.whisper import WhisperEncoder  # noqa: E402
from longcat_video_avatar.pipeline_mlx import LongCatAvatarPipeline, PipelineConfig  # noqa: E402


class Pipeline(LongCatAvatarPipeline):
    """Reports a step per CFG forward (one per sampling step)."""

    step = 0

    def _cfg_forward(self, *a, **kw):
        out = super()._cfg_forward(*a, **kw)
        mx.eval(out)
        self.step += 1
        emit(type="progress", step=self.step, total=self.config.num_sampling_steps)
        return out


def load_json(p):
    return json.loads(pathlib.Path(p).read_text())


def build(d: pathlib.Path):
    # as scripts/run_inference.py's build_pipeline (the quantised DiT is quantised before its weights load)
    vae = AutoencoderKLWan.from_config(load_json(d / "vae" / "config.json"))
    vae.load_weights(str(d / "vae" / "diffusion_pytorch_model.safetensors"), strict=False)
    umt5 = UMT5EncoderModel.from_config(load_json(d / "text_encoder" / "config.json"))
    for shard in sorted(set(load_json(d / "text_encoder" / "model.safetensors.index.json")["weight_map"].values())):
        umt5.load_weights(str(d / "text_encoder" / shard), strict=False)
    whisper = WhisperEncoder.from_config(load_json(d / "audio_encoder" / "config.json"))
    whisper.load_weights(str(d / "audio_encoder" / "model.safetensors"), strict=False)
    dit_cfg = load_json(d / "dit" / "config.json")
    dit = LongCatVideoAvatarTransformer3DModel.from_config(dit_cfg)
    q = dit_cfg.get("quantization")
    if q is not None:
        skip = q.get("skip_patterns", ["final_layer.linear", "t_embedder.", "y_embedder.", "adaLN_modulation.",
                                       "audio_adaLN_modulation."])

        def pred(path, module):
            return isinstance(module, nn.Linear) and not any(s in path for s in skip)

        nn.quantize(dit, group_size=int(q.get("group_size", 64)), bits=int(q["bits"]), class_predicate=pred)
    for shard in sorted(set(load_json(d / "dit" / "diffusion_pytorch_model.safetensors.index.json")["weight_map"].values())):
        dit.load_weights(str(d / "dit" / shard), strict=False)
    mx.eval(vae.parameters(), umt5.parameters(), whisper.parameters(), dit.parameters())
    return Pipeline(vae=vae, text_encoder=umt5, audio_encoder=whisper, dit=dit, config=PipelineConfig())


def portrait(path, width, height):
    # center-crop to the aspect, then resize (the original script stretches)
    from PIL import Image

    img = Image.open(path).convert("RGB")
    w, h = img.size
    want = width / height
    if w / h > want:
        nw = int(h * want)
        img = img.crop(((w - nw) // 2, 0, (w - nw) // 2 + nw, h))
    else:
        nh = int(w / want)
        img = img.crop((0, (h - nh) // 2, w, (h - nh) // 2 + nh))
    img = img.resize((width, height), Image.BICUBIC)
    arr = np.asarray(img, dtype=np.float32) / 127.5 - 1.0
    return mx.array(arr.transpose(2, 0, 1)[None, :, None, :, :])


def mel(path):
    import librosa
    from transformers import WhisperFeatureExtractor

    audio, sr = librosa.load(str(path), sr=16000)
    audio = loudness_normalize(audio.astype(np.float64), 16000).astype(np.float32)
    fe = WhisperFeatureExtractor(feature_size=128)  # whisper-large-v3's (no download)
    return mx.array(fe(audio, sampling_rate=16000, return_tensors="np").input_features.astype(np.float32))


def encode_text(pipe, d, prompt):
    from transformers import T5TokenizerFast

    tok = T5TokenizerFast.from_pretrained(str(d / "tokenizer"))
    enc = tok(prompt, return_tensors="np", padding="max_length", max_length=512, truncation=True)
    ids, mask = mx.array(enc.input_ids), mx.array(enc.attention_mask)
    text = pipe.text_encoder(ids, mask=mask)[:, None, :, :]
    empty_ids, empty_mask = mx.zeros_like(ids), mx.zeros_like(mask)
    uncond = pipe.text_encoder(empty_ids, mask=empty_mask)[:, None, :, :]
    mx.eval(text, uncond)
    return text, mask[:, None, None, :], uncond, empty_mask[:, None, None, :]


def save(video, out, ffmpeg):
    arr = (np.asarray(video.astype(mx.float32)).transpose(0, 2, 3, 4, 1)[0] * 127.5 + 127.5).clip(0, 255).astype(np.uint8)
    t, h, w, _ = arr.shape
    p = subprocess.Popen([ffmpeg, "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{w}x{h}", "-r", str(FPS),
                          "-i", "-", "-c:v", "libx264", "-crf", "16", "-pix_fmt", "yuv420p", out], stdin=subprocess.PIPE)
    p.stdin.write(arr.tobytes())
    p.stdin.close()
    if p.wait() != 0:
        raise RuntimeError("ffmpeg could not write the video")
    return t


loaded = {"dir": None, "pipe": None}

emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        d = pathlib.Path(req["model"])
        mx.reset_peak_memory()
        t0 = time.time()
        if loaded["dir"] != str(d):
            loaded["pipe"] = None
            mx.clear_cache()
            emit(type="status", text="loading LongCat-Video-Avatar …")
            loaded["pipe"] = build(d)
            loaded["dir"] = str(d)
            emit(type="log", text=f"loaded {d.name} in {time.time() - t0:.1f} s")
        pipe = loaded["pipe"]
        width, height, frames = int(req["width"]), int(req["height"]), int(req["frames"])
        emit(type="status", text="encoding the prompt and the speech …")
        text, text_mask, uncond, uncond_mask = encode_text(pipe, d, req.get("prompt") or "")
        image = portrait(req["image"], width, height)
        audio_mel = mel(req["audio"])
        emit(type="status", text="denoising …")
        pipe.step = 0
        emit(type="progress", step=0, total=pipe.config.num_sampling_steps)
        video = pipe(image=image, audio_mel=audio_mel, text_embeds=text, text_mask=text_mask, uncond_embeds=uncond,
                     uncond_mask=uncond_mask, height=height, width=width, num_frames=frames, seed=int(req["seed"]))
        mx.eval(video)
        emit(type="status", text="writing the video …")
        n = save(video, req["output"], req.get("ffmpeg") or "ffmpeg")
        mx.clear_cache()
        emit(type="log", text=f"{n} frames at {FPS} fps")
        emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=mx.get_peak_memory() / 2**30)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
