# mlxlab's sound effects worker (a description → a sound): keeps one MOSS-SoundEffect pipeline loaded and answers
# requests.
#
# Runtime: mlx-speech (github.com/appautomaton/mlx-speech, MIT), pure MLX: MOSS-SoundEffect (OpenMOSS, Apache-2.0;
# the MossTTSDelay architecture on a Qwen3-8B backbone, 4-bit) predicts 16 RVQ codebooks at 12.5 frames per second,
# the MOSS audio tokenizer (int8) decodes them to 24 kHz mono. Nothing is downloaded here: the snapshots come in as
# dirs.model (…/mlx-4bit inside) and dirs.codec (…/mlx-int8 inside).
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "moss-soundeffect", "dirs": {"model": "/snap", "codec": "/snap"}, "prompt": "rain on a tin
#             roof", "duration": 8, "seed": 1, "output": "/path/sound.wav"}
#   events:  {"type": "ready"|"status"|"progress"|"log"|"result"|"error", …}
#   result:  {"type": "result", "file": output, "seconds": generation time, "duration": seconds of audio,
#             "sampleRate": 24000, "peakGB": MLX peak memory, "loadSeconds": model loading of this request}
import json
import os
import sys
import time
import traceback
from pathlib import Path

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting the sound effects runtime …")
import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402
import soundfile as sf  # noqa: E402

import mlx_speech.generation.moss_delay as gen  # noqa: E402
from mlx_speech.models.moss_audio_tokenizer import load_moss_audio_tokenizer_model  # noqa: E402
from mlx_speech.models.moss_delay import MossTTSDelayProcessor, load_moss_sound_effect_model  # noqa: E402
from mlx_speech.models.moss_delay.sound_effect import (  # noqa: E402
    SOUND_EFFECT_DEFAULT_AUDIO_REPETITION_PENALTY,
    SOUND_EFFECT_DEFAULT_AUDIO_TEMPERATURE,
    SOUND_EFFECT_DEFAULT_AUDIO_TOP_K,
    SOUND_EFFECT_DEFAULT_AUDIO_TOP_P,
    build_sound_effect_conversation,
)

MODELS = {"moss-soundeffect": ("mlx-4bit", "mlx-int8")}
# The duration is a hint in the prompt (expected tokens), not a hard length. The row budget leaves room for the
# model to finish (and the 16-row delay pattern), but stops a run-away generation.
MIN_SECONDS, MAX_SECONDS = 1.0, 30.0

loaded = {"key": None, "model": None, "processor": None}

# progress: the generation loop updates its delay state once per row; count the calls
progress = {"step": 0, "total": 0}
_update = gen._update_delay_state


def _counting_update(*args, **kw):
    progress["step"] += 1
    s, n = progress["step"], progress["total"]
    if n and (s % 10 == 0):
        emit(type="progress", step=min(s, n), total=n)
    return _update(*args, **kw)


gen._update_delay_state = _counting_update


def dirs_for(req):
    sub = MODELS.get(req.get("model"))
    if sub is None:
        raise ValueError(f"unknown model {req.get('model')!r} (supported: {', '.join(MODELS)})")
    dirs = req.get("dirs") or {}
    model = Path(dirs.get("model") or "") / sub[0]
    codec = Path(dirs.get("codec") or "") / sub[1]
    if not (model / "model.safetensors").is_file():
        raise FileNotFoundError(f"no MOSS-SoundEffect weights in {model}")
    if not (codec / "model.safetensors").is_file():
        raise FileNotFoundError(f"no MOSS audio tokenizer weights in {codec}")
    return model, codec


def pipeline_for(req):
    model_dir, codec_dir = dirs_for(req)
    key = (str(model_dir), str(codec_dir))
    if loaded["key"] == key:
        return 0.0
    loaded["key"] = loaded["model"] = loaded["processor"] = None
    mx.clear_cache()
    emit(type="status", text=f"loading {req['model']} …")
    t0 = time.time()
    lm = load_moss_sound_effect_model(model_dir)
    codec = load_moss_audio_tokenizer_model(codec_dir)
    loaded["processor"] = MossTTSDelayProcessor.from_path(lm.model_dir, audio_tokenizer=codec.model)
    loaded["model"], loaded["key"] = lm.model, key
    took = time.time() - t0
    emit(type="log", text=f"loaded {req['model']} in {took:.1f} s")
    return took


def generate(req):
    prompt = (req.get("prompt") or "").strip()
    if not prompt:
        raise ValueError("describe the sound")
    duration = min(max(float(req.get("duration") or 8.0), MIN_SECONDS), MAX_SECONDS)
    load = pipeline_for(req)
    processor = loaded["processor"]
    seed = int(req.get("seed") or 0)
    mx.random.seed(seed % (2**32))

    conversation, expected = build_sound_effect_conversation(processor, ambient_sound=prompt, duration_seconds=duration)
    budget = int(expected * 2) + 64
    progress["step"], progress["total"] = 0, expected + 16
    config = gen.MossTTSDelayGenerationConfig(
        max_new_tokens=budget,
        audio_temperature=SOUND_EFFECT_DEFAULT_AUDIO_TEMPERATURE,
        audio_top_p=SOUND_EFFECT_DEFAULT_AUDIO_TOP_P,
        audio_top_k=SOUND_EFFECT_DEFAULT_AUDIO_TOP_K,
        audio_repetition_penalty=SOUND_EFFECT_DEFAULT_AUDIO_REPETITION_PENALTY,
    )
    emit(type="status", text=f"generating {duration:g} s of sound …")
    t0 = time.time()
    result = gen.synthesize_moss_tts_delay_conversations(
        loaded["model"], processor, conversations=conversation, mode="generation", config=config)
    out = result.outputs[0]
    wav = np.asarray(out.waveform.astype(mx.float32)).reshape(-1)
    if wav.size == 0:
        raise RuntimeError("the model made no audio (try another seed or description)")
    peak = float(np.max(np.abs(wav)))
    if peak > 1.0:  # the codec can overshoot a little: keep it from clipping
        wav = wav / peak * 0.98
    rate = int(out.sample_rate)
    Path(req["output"]).parent.mkdir(parents=True, exist_ok=True)
    sf.write(req["output"], wav, rate, subtype="PCM_16")
    seconds = time.time() - t0
    if not result.generation.stop_reached:
        emit(type="log", text=f"the model did not finish within {budget} rows: the sound is cut")
    emit(type="progress", step=progress["total"], total=progress["total"])
    mlx_peak = mx.get_peak_memory() / 2**30
    mx.clear_cache()  # the buffer cache would keep several GB between sounds
    emit(type="result", file=req["output"], seconds=seconds, duration=wav.size / rate, sampleRate=rate,
         peakGB=mlx_peak, loadSeconds=load, expectedTokens=expected)


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        mx.reset_peak_memory()
        generate(req)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
