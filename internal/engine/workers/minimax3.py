# mlxlab's MiniMax Music 3 worker: the pure-MLX port github.com/mikolaj92/minimax-music3-mlx (Apache-2.0, pinned
# in the uv environment; no PyTorch) with the model kept loaded between songs. The weights are that port's
# quantised conversions of MiniMaxAI/MiniMax-Music3 (mikoy92/MiniMax-Music3-MLX-8bit, -4bit).
#
#   python minimax3.py
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3.
#   request: {"model": "8bit" | "4bit", "dirs": {"weights": "/dir of the converted MLX weights"},
#             "prompt": "the music description", "lyrics": "[verse]\n…\n[chorus]\n…" (tags on their own lines),
#             "duration": 30 (a ceiling: the model may end the song earlier), "steps": 30, "seed": 1,
#             "output": "/path/out.wav"}
#   events:  {"type": "status"|"progress"|"log"|"result"|"error"|"ready", …}
#
# The pipeline: the 8B global LM (Qwen3) samples one semantic code per 25 Hz frame, the 0.6B depth decoder the seven
# residual codes, their hidden states condition the 2.4B flow-matching DiT (200-frame windows, 100-frame hop), the
# vocoder decodes each window; output 44.1 kHz stereo (the port's vocoder rate).
import contextlib
import ctypes
import gc
import io
import json
import os
import sys
import threading
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting MiniMax Music 3 …")

import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402
import soundfile as sf  # noqa: E402

from minimax_music3_mlx.pipeline import chunk_starts, generate_audio, load_modules  # noqa: E402

FRAME_RATE = 25.0
LONGEST = 300.0  # seconds: MiniMax's "up to five minutes" (9,000 frames is the hard limit)


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
loaded = {"key": None, "modules": None}


def quiet(fn, *a, **kw):
    with contextlib.redirect_stdout(io.StringIO()):
        return fn(*a, **kw)


def modules_for(req):
    weights = (req.get("dirs") or {}).get("weights")
    if not weights or not os.path.isdir(weights):
        raise ValueError(f"dirs.weights (the converted MiniMax Music 3 weights) is required, got {weights!r}")
    for part in ("language_model", "rvq_depth_decoder", "condition_encoder", "transformer", "vocoder", "tokenizer"):
        if not os.path.isdir(os.path.join(weights, part)):
            raise FileNotFoundError(f"missing {part}/ in {weights}")
    if loaded["key"] != weights:
        loaded.update(key=None, modules=None)
        gc.collect()
        mx.clear_cache()
        t0 = time.time()
        emit(type="status", text=f"loading MiniMax Music 3 ({req.get('model', '?')}) …")
        modules = quiet(load_modules, weights)
        # the port decodes every flow window lazily and evaluates all of them at the end: the vocoder graphs of all
        # windows then live at once (45.5 GB peak for 30 s on the M1 Max). Evaluated window by window: 20.4 GB,
        # same speed, bit-identical samples (the same computation, only evaluated earlier).
        vocoder = modules.vocoder

        def vocode(latents):
            wave = vocoder(latents)
            mx.eval(wave)
            mx.clear_cache()
            return wave

        modules.vocoder = vocode
        loaded["modules"] = modules
        loaded["key"] = weights
        emit(type="log", text=f"loaded in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    return loaded["modules"]


# ---------------------------------------------------------------- generation
def generate(req, modules):
    seconds = min(float(req.get("duration") or 30), LONGEST)
    steps = int(req.get("steps") or 30)
    seed = int(req.get("seed") or 0) % 2**31
    prompt = (req.get("prompt") or "").strip()
    lyrics = (req.get("lyrics") or "").strip()
    if not prompt:
        raise ValueError("a music description (the style) is required")
    if not lyrics:
        lyrics = "[intro]\n(instrumental)\n[instrumental]\n(instrumental)\n[outro]\n(instrumental)"
    frames = max(1, int(seconds * FRAME_RATE))
    windows = len(chunk_starts(frames))
    state = {"stage": None}

    def progress(stage, i, n):
        if stage != state["stage"]:
            state["stage"] = stage
            emit(type="status", text="singing (language model) …" if stage == "ar" else "rendering the audio (flow matching) …")
        if stage == "ar":
            if i == 1 or i % 25 == 0 or i == n:
                emit(type="progress", step=i, total=n)
        else:
            emit(type="progress", step=i, total=n)

    emit(type="status", text=f"singing (language model, up to {frames} frames) …")
    state["stage"] = "ar"
    wave, rate = quiet(generate_audio, modules, lyrics=lyrics, prompt=prompt, audio_duration=seconds,
                       num_inference_steps=steps, seed=seed, progress_callback=progress)
    emit(type="log", text=f"{wave.shape[0] / rate:.1f} s of audio at {rate} Hz (asked for at most {seconds:.0f} s, "
                          f"{windows} flow windows planned)")
    out = os.path.abspath(req["output"])
    os.makedirs(os.path.dirname(out), exist_ok=True)
    sf.write(out, np.ascontiguousarray(wave), rate, subtype="PCM_16")
    return wave.shape[0] / rate


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
            m = modules_for(req)
            load = time.time() - t0
            mx.reset_peak_memory()
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
