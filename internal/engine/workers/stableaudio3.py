# mlxlab's Stable Audio 3 worker: the official pure-MLX runtime (github.com/Stability-AI/stable-audio-3,
# optimized/mlx; no PyTorch) with its models kept loaded between songs. Instrumental music only: the model has no
# lyrics input (lyrics in a request are ignored).
#
#   python stableaudio3.py <dir of stable-audio-3/optimized/mlx at the pinned commit>
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3.
#   request: {"model": "sm-music" | "medium", "dirs": {"weights": "/dir of hf stabilityai/stable-audio-3-optimized"},
#             "prompt": "…", "duration": 45, "seed": 1, "output": "/path/out.wav",
#             optional: "steps": 8, "cfg": 1.0, "negative": "…",
#                       "lora": {"file": "style.safetensors", "scale": 1.0} (a trained style or another underfit-format
#                       LoRA in dirs.lora, merged into the DiT and the seconds conditioner when the model loads),
#                       studio tasks: "task": "cover" | "repaint" | "extend", "src_audio": "/abs/song.wav|mp3",
#                       "strength": 0..1 (cover: how much of the source stays, default 0.8),
#                       "start"/"end": seconds (repaint; end ≤ 0 = to the end), "extend": seconds to add (extend;
#                       optional "start" < source length regenerates the tail too)}
#   events:  {"type": "status"|"progress"|"log"|"result"|"error"|"ready", …}
#
# Weights (all under MLX/ in the weights dir): t5gemma_f16.npz (text encoder, shared), dit_<model>_f16.npz,
# same_{s,l}_{decoder,encoder}_f32.npz (the codec; the encoder only for the studio tasks).
import contextlib
import ctypes
import gc
import importlib
import io
import json
import math
import os
import sys
import threading
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting Stable Audio 3 …")
CODE = os.path.abspath(sys.argv[1])
sys.path.insert(0, CODE)
sys.path.insert(0, os.path.join(CODE, "scripts"))

import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402

from models.defs.sa3_pipeline import (  # noqa: E402
    apply_prompt_padding,
    build_pingpong_schedule,
    load_conditioner_from_npz,
    patched_decode,
    sample_flow_pingpong,
)
from models.defs.t5gemma_mlx import T5Gemma  # noqa: E402
import sa3_mlx  # noqa: E402  (read_wav, save_wav, patch_audio, the decoder/encoder table)

SR = sa3_mlx.SAMPLE_RATE  # 44.1 kHz stereo
PER_LATENT = sa3_mlx.SAMPLES_PER_LATENT
# model -> (DiT module, DiT weights, codec, longest song in seconds)
MODELS = {
    "sm-music": ("models.defs.dit_mlx", "dit_sm-music_f16.npz", "same-s", 120.0),
    "medium": ("models.defs.dit_mlx_medium", "dit_medium_f16.npz", "same-l", 380.0),
}
CODEC = {  # codec -> (decoder module, chunk, overlap, decoder weights, encoder module, pad modulo, encoder weights)
    "same-s": ("models.defs.same_s_decoder", 8, 2, "same_s_decoder_f32.npz", "models.defs.same_s_encoder", 32, "same_s_encoder_f32.npz"),
    "same-l": ("models.defs.same_l_decoder", 128, 8, "same_l_decoder_f32.npz", "models.defs.same_l_encoder", 16, "same_l_encoder_f32.npz"),
}
TASKS = ("text2music", "cover", "repaint", "extend")
DTYPE = mx.float16  # the DiT (validated by upstream at ~50–57 dB PSNR vs fp32); the codec runs fp32


# ---------------------------------------------------------------- memory: peak physical footprint of this process
class _RUsageV4(ctypes.Structure):
    _fields_ = [("uuid", ctypes.c_uint8 * 16), ("v", ctypes.c_uint64 * 35)]


_libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib")


def footprint():
    ru = _RUsageV4()
    if _libc.proc_pid_rusage(os.getpid(), 4, ctypes.byref(ru)) != 0:
        return 0
    return ru.v[7]


class PeakSampler:
    def __init__(self):
        self.peak = footprint()
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def _run(self):
        while not self.stop.wait(0.2):
            self.peak = max(self.peak, footprint())

    def done(self):
        self.stop.set()
        self.thread.join()
        return max(self.peak, footprint()) / 2**30


# ---------------------------------------------------------------- loading (kept between requests)
loaded = {"key": None, "t5": None, "dit": None, "cond": None, "dec": None, "enc": None}


def weight(dirs, name):
    p = os.path.join(dirs["weights"], "MLX", name)
    if not os.path.isfile(p):
        raise FileNotFoundError(f"missing weights {p}")
    return p


def quiet(fn, *a, **kw):
    with contextlib.redirect_stdout(io.StringIO()):
        return fn(*a, **kw)


def models_for(req):
    model = req["model"]
    if model not in MODELS:
        raise ValueError(f"unknown model {model!r} (known: {', '.join(MODELS)})")
    dirs = req.get("dirs") or {}
    if not dirs.get("weights"):
        raise ValueError("dirs.weights (stabilityai/stable-audio-3-optimized) is required")
    lora = lora_of(req)
    key = (model, dirs["weights"], lora)
    if loaded["key"] != key:
        loaded.update(key=None, dit=None, cond=None, dec=None, enc=None)
        gc.collect()
        mx.clear_cache()
        t0 = time.time()
        if loaded["t5"] is None:
            emit(type="status", text="loading the text encoder (T5Gemma) …")
            loaded["t5"] = T5Gemma.from_npz(weight(dirs, "t5gemma_f16.npz"))
        mod, dit_file, codec, _ = MODELS[model]
        emit(type="status", text=f"loading {model} (DiT, decoder) …")
        dit_path = weight(dirs, dit_file)
        # T_lat only sizes a buffer of zeros the model replaces at the input's length, so one load serves any length
        lora_kw = {}
        if lora:
            emit(type="status", text=f"loading {model} with the style {os.path.basename(lora[0])} …")
            lora_kw = dict(lora_paths=[lora[0]], lora_strength=lora[2], lora_log=lambda m: emit(type="log", text=m))
        loaded["dit"] = quiet(importlib.import_module(mod).load_dit, dit_path, T_lat=320, dtype=DTYPE, compile_=False, **lora_kw)
        loaded["cond"] = load_conditioner_from_npz(dit_path, prefix="cond.")
        if lora:  # the seconds conditioner is loaded from the npz on its own: its LoRA delta goes in here
            from models.defs.lora_merge import apply_conditioner_lora
            secs = loaded["cond"][1]
            secs.W, _ = apply_conditioner_lora(secs.W, [{"path": lora[0], "strength": lora[2], "steps": None}])
        c = CODEC[codec]
        loaded["dec"] = quiet(importlib.import_module(c[0]).load_model, weights_path=weight(dirs, c[3]), dtype=mx.float32, compile_=False)
        loaded["key"] = key
        emit(type="log", text=f"loaded {model} in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    return loaded


def lora_of(req):
    """The request's LoRA as (file, mtime, scale), or None (a changed file or scale loads the DiT again)."""
    spec = req.get("lora")
    if not spec:
        return None
    d = (req.get("dirs") or {}).get("lora")
    if not d:
        raise ValueError("dirs.lora is required with a lora")
    f = os.path.join(d, spec.get("file") or "style.safetensors")
    if not os.path.isfile(f):
        raise FileNotFoundError(f"missing LoRA {f}")
    return (f, os.path.getmtime(f), float(spec.get("scale") or 1.0))


def encoder(req):
    if loaded["enc"] is None:
        c = CODEC[MODELS[req["model"]][2]]
        emit(type="status", text="loading the audio encoder …")
        loaded["enc"] = quiet(importlib.import_module(c[4]).load_model, weights_path=weight(req["dirs"], c[6]), dtype=mx.float32, compile_=False)
    return loaded["enc"], CODEC[MODELS[req["model"]][2]][5]


# ---------------------------------------------------------------- generation
def audio_seconds(path):
    return sa3_mlx.read_wav(path).shape[-1] / SR


def encode_audio(req, audio, T_lat):
    """(2, T) float32 audio → latents (1, 256, T_lat): trimmed or zero-padded to the song's length."""
    enc, pad_mod = encoder(req)
    enc_T = T_lat
    if (T_lat * 16) % pad_mod:
        enc_T = math.ceil(T_lat * 16 / pad_mod) * pad_mod // 16
    n = enc_T * PER_LATENT
    audio = audio[:, :n] if audio.shape[-1] >= n else np.pad(audio, ((0, 0), (0, n - audio.shape[-1])))
    patches = sa3_mlx.patch_audio(audio[None, ...], patch_size=256)
    status("encoding the source …")
    lat = enc(mx.array(patches))[..., :T_lat]
    mx.eval(lat)
    return lat.astype(DTYPE)


_last = [None]


def status(text):
    if text != _last[0]:
        _last[0] = text
        emit(type="status", text=text)


def plan(req):
    """The request's task as (seconds, init audio or None, σmax, inpaint range in seconds or None)."""
    task = req.get("task") or "text2music"
    if task not in TASKS:
        raise ValueError(f"unknown task {task!r} (known: {', '.join(TASKS)})")
    longest = MODELS[req["model"]][3]
    if task == "text2music":
        return min(float(req.get("duration") or 30), longest), None, 1.0, None
    src = req.get("src_audio")
    if not src or not os.path.isfile(src):
        raise ValueError(f"task {task} needs src_audio (an existing audio file), got {src!r}")
    audio = sa3_mlx.read_wav(src)
    src_len = audio.shape[-1] / SR
    if task == "cover":
        # strength = how much of the source stays; σmax 0.4–0.8 is upstream's range for a variation
        strength = float(req["strength"]) if req.get("strength") is not None else 0.8
        sigma = min(1.0, max(0.3, 1.0 - 0.6 * max(0.0, min(1.0, strength))))
        return min(src_len, longest), audio, sigma, None
    if task == "repaint":
        seconds = min(src_len, longest)
        start = max(0.0, float(req.get("start") or 0.0))
        end = float(req["end"]) if req.get("end") is not None else -1.0
        end = seconds if end <= 0 or end > seconds else end
        if end <= start:
            raise ValueError(f"repaint: an empty range {start}–{end} s")
        return seconds, audio, 1.0, (start, end)
    add = float(req.get("extend") or 30)
    start = float(req["start"]) if req.get("start") is not None else src_len
    seconds = min(src_len + add, longest)
    if start >= seconds:
        raise ValueError(f"extend: the song is already {src_len:.0f} s (this model makes at most {longest:.0f} s)")
    return seconds, audio, 1.0, (max(0.0, start), seconds)


def generate(req, m):
    seconds, init_audio, sigma_max, inpaint = plan(req)
    steps = int(req.get("steps") or 8)
    cfg = float(req.get("cfg") or 1.0)
    seed = int(req.get("seed") or 0) % 2**31
    prompt = req.get("prompt") or ""
    T_lat = max(1, math.ceil(seconds * SR / PER_LATENT))

    status("encoding the prompt …")
    pad_emb, secs = m["cond"]
    emb, mask = m["t5"].encode([prompt], max_len=256)
    emb = apply_prompt_padding(emb.astype(DTYPE), mask, pad_emb.astype(DTYPE))
    sec_emb = secs(seconds).astype(DTYPE)
    cross = mx.concatenate([emb, sec_emb], axis=1)
    gcond = sec_emb[:, 0, :]
    null_cross = None
    if cfg != 1.0:
        if req.get("negative"):
            ne, nm = m["t5"].encode([req["negative"]], max_len=256)
            null_cross = mx.concatenate([apply_prompt_padding(ne.astype(DTYPE), nm, pad_emb.astype(DTYPE)), sec_emb], axis=1)
        else:
            null_cross = mx.zeros_like(cross)
    mx.eval(cross, gcond)

    init_lat = encode_audio(req, init_audio, T_lat) if init_audio is not None else None
    noise = mx.random.normal((1, 256, T_lat), dtype=DTYPE, key=mx.random.key(seed))
    if init_lat is not None and inpaint is None:
        noise = init_lat * (1.0 - sigma_max) + noise * sigma_max
    local, paste = None, None
    if inpaint is not None:
        s0 = max(0, int(round(inpaint[0] * SR / PER_LATENT)))
        s1 = min(T_lat, int(round(inpaint[1] * SR / PER_LATENT)))
        mk = np.ones((1, 1, T_lat), dtype=np.float32)
        mk[:, :, s0:s1] = 0.0
        mk = mx.array(mk)
        local = mx.concatenate([mk, init_lat.astype(mx.float32) * mk], axis=1).transpose(0, 2, 1).astype(DTYPE)
        paste = (init_lat, mk)
    mx.eval(noise)

    dit = m["dit"]

    def model_fn(x, t):  # as sa3_mlx.py: CFG with adaptive projected guidance (apg 1.0)
        if null_cross is None:
            return dit(x, t, cross, gcond, local_add_cond=local)
        v = dit(mx.concatenate([x, x]), mx.concatenate([t, t]), mx.concatenate([cross, null_cross]),
                mx.concatenate([gcond, gcond]), local_add_cond=None if local is None else mx.concatenate([local, local]))
        cv, uv = mx.split(v, 2, axis=0)
        s = t.reshape(-1, 1, 1).astype(mx.float32)
        xf = x.astype(mx.float32)
        cd, ud = xf - cv.astype(mx.float32) * s, xf - uv.astype(mx.float32) * s
        diff = cd - ud
        unit = cd / mx.maximum(mx.sqrt((cd * cd).sum(axis=(-2, -1), keepdims=True)), 1e-8)
        diff = diff - (diff * unit).sum(axis=(-2, -1), keepdims=True) * unit
        return ((xf - (cd + (cfg - 1.0) * diff)) / s).astype(x.dtype)

    status("diffusion …")
    sigmas = build_pingpong_schedule(steps, sigma_max=sigma_max, use_logsnr_shift=True)
    lat = sample_flow_pingpong(model_fn, noise, sigmas, seed=seed + 1, paste_back=paste,
                               on_step=lambda i, n: emit(type="progress", step=i, total=n))

    status("decoding audio …")
    codec = CODEC[MODELS[req["model"]][2]]
    chunk, ovl = codec[1], codec[2]
    lat = lat.astype(mx.float32)
    dec, dmod = m["dec"], importlib.import_module(codec[0])
    if T_lat > chunk + 2 * ovl:
        patches = dmod.decode_chunked(dec, lat, chunk, ovl)
    elif T_lat % 2 == 0:
        patches = dec(lat)
    elif T_lat > 6:
        patches = dmod.decode_chunked(dec, lat, 2, 2)
    else:
        patches = dec(mx.concatenate([lat, lat[..., -1:]], axis=-1))[..., : T_lat * 16]
    audio = patched_decode(patches, patch_size=256, channels=2)
    mx.eval(audio)
    wav = np.array(audio.astype(mx.float32))[0][..., : int(round(seconds * SR))]
    os.makedirs(os.path.dirname(os.path.abspath(req["output"])), exist_ok=True)
    sa3_mlx.save_wav(req["output"], wav)
    return wav.shape[-1] / SR


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        sampler = PeakSampler()
        try:
            t0 = time.time()
            m = models_for(req)
            load = time.time() - t0
            mx.reset_peak_memory()
            _last[0] = None
            t0 = time.time()
            duration = generate(req, m)
            seconds = time.time() - t0
        finally:
            m = None
            peak = sampler.done()
            mx.clear_cache()
        emit(type="result", file=req["output"], seconds=seconds, duration=duration, peakGB=peak, loadSeconds=load,
             mlxPeakGB=mx.get_peak_memory() / 2**30)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
