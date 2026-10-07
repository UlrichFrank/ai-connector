# mlxlab's YuE2-3B worker (lyrics to song): keeps one YuE2 MLX pipeline loaded and answers requests.
#
# Uses the community pure-MLX port ahmadw/YuE2-3B-MLX (CC BY-NC 4.0). The repo snapshot is passed in as dirs.yue2: its
# generate.py / yue2_model.py / yue2_vae.py are imported from there, the weights come from its 8bit/, 4bit/ or bf16/ folder.
# Nothing is downloaded here.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "yue2-3b-8bit", "dirs": {"yue2": "/snapshot"}, "prompt": "style tags", "lyrics": "[Verse]\n…",
#             "instrumental": false, "duration": 30, "steps": 0, "seed": 1, "output": "/path/song.wav",
#             optional: "cot": "full"|"melody"|"off", "cfg": 1.0}
#   events:  {"type": "status"|"progress"|"log"|"result"|"error", …}
# The 48 kHz stereo WAV goes to "output"; with cot melody/full the generated ABC score is written next to it
# (song.wav → song.abc) and its path is reported as "abc" in the result. "abc_text" in a request skips the planning
# and sings that score (mlxlab's "restyle": an earlier song's melody in a new style).
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


emit(type="status", text="starting YuE2 …")
import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402,F401
import tiktoken  # noqa: E402,F401

VARIANTS = {"yue2-3b-8bit": "8bit", "yue2-3b-4bit": "4bit", "yue2-3b-bf16": "bf16"}
FPS = 25  # semantic codec frames per second (1920 samples at 48 kHz)
# no documented instrumental mode upstream: wordless sections (as in the upstream example) + an "instrumental" style tag.
# Best effort: the ABC plan may still contain a vocal melody line.
INSTRUMENTAL_LYRICS = "[Intro]\n\n[Interlude]\n\n[Outro]"
FADE_SECONDS = 4

loaded = {"key": None, "pipe": None, "gen": None}


def gen_module(root):
    # generate.py imports yue2_model / yue2_vae as top-level modules, so the snapshot root goes on sys.path
    if loaded["gen"] is None:
        sys.path.insert(0, str(root))
        import generate  # noqa: PLC0415

        loaded["gen"] = generate
    return loaded["gen"]


def weights_dir(req):
    variant = VARIANTS.get(req["model"])
    if variant is None:
        raise ValueError(f"unknown model {req['model']!r} (supported: {', '.join(VARIANTS)})")
    root = Path((req.get("dirs") or {}).get("yue2") or "")
    if not (root / "generate.py").is_file():
        raise FileNotFoundError(f"no YuE2-3B-MLX snapshot (generate.py) in {root}")
    path = root / variant
    if not (path / "model.safetensors").is_file():
        raise FileNotFoundError(f"{path} has no model.safetensors (download the {variant}/ folder)")
    return root, path


def pipeline_for(req):
    root, path = weights_dir(req)
    gen = gen_module(root)
    key = str(path)
    if loaded["key"] != key:
        loaded["key"], loaded["pipe"] = None, None
        mx.clear_cache()
        emit(type="status", text=f"loading {req['model']} …")
        t0 = time.time()
        loaded["pipe"] = gen.Yue2Pipeline(path, log=lambda msg: print(msg, file=sys.stderr, flush=True))
        loaded["key"] = key
        emit(type="log", text=f"loaded {req['model']} in {time.time() - t0:.1f} s")
    return gen, loaded["pipe"]


def token_progress(total, every):
    count = [0]

    def on_token(_phase, _token):
        count[0] += 1
        if count[0] % every == 0:
            emit(type="progress", step=min(count[0], total), total=total)

    return on_token


def run(req, gen, pipe):
    tok = pipe.tokenizer
    style = (req.get("prompt") or "").strip()
    lyrics = (req.get("lyrics") or "").replace("\r\n", "\n").strip()
    if req.get("instrumental"):
        lyrics = INSTRUMENTAL_LYRICS
        if "instrumental" not in style.lower():
            style = f"{style}, instrumental" if style else "instrumental"
    if not style:
        raise ValueError("a style prompt is required")
    if not lyrics:
        raise ValueError("lyrics are required (or set instrumental)")
    # a song needs an ending: without an outro section the plan just stops where the lyrics do
    if "[outro" not in lyrics.lower():
        lyrics += "\n\n[Outro]"
    cot = req.get("cot") or "full"
    if cot not in gen.INSTRUCTIONS:
        raise ValueError("cot must be off, melody or full")
    seed = int(req.get("seed") or 0)
    steps = int(req.get("steps") or 0) or pipe.ode_steps
    semantic = pipe.semantic_sampling
    # the lyrics decide the length (the score follows them, not the clock: verse + chorus + outro came out at 51 s for
    # a 30 s target and ended there by itself), so the length is only a target with plenty of room; a song that runs
    # past the room fades out at the cap
    duration = float(req.get("duration") or 0)
    if duration > 0:
        cap = max(1, round(max(duration * 2, duration + 60) * FPS))
        semantic = gen.Sampling(**{**semantic.__dict__, "max_tokens": cap, "min_tokens": min(semantic.min_tokens, cap)})

    # 1. symbolic plan (ABC score) — or the score of an earlier song ("abc_text": restyle the same melody)
    abc_ids, abc_text = [], None
    given = (req.get("abc_text") or "").strip()
    if given:
        if cot == "off":
            cot = "full"
        abc_ids, abc_text = tok.encode(given), given
        emit(type="log", text=f"ABC score given: {len(abc_ids)} tokens (the melody stays, the style is new)")
    elif cot != "off":
        emit(type="status", text="planning ABC score …")
        t = time.time()
        count = [0]

        def on_abc(_phase, _token):
            count[0] += 1
            if count[0] % 100 == 0:
                emit(type="log", text=f"ABC score: {count[0]} tokens")

        abc_ids, truncated = gen.generate_tokens(pipe.model, gen.token_prefix(tok, style, lyrics, cot),
                                                 pipe.abc_sampling, seed, "abc", on_token=on_abc)
        abc_text = tok.decode(abc_ids)
        emit(type="log", text=f"ABC score: {len(abc_ids)} tokens in {time.time() - t:.1f} s"
                              + (" (hit max_tokens)" if truncated else ""))
    prefix = gen.token_prefix(tok, style, lyrics, cot, abc_ids)

    # 2. semantic codec tokens (25 per second of audio)
    guidance = req.get("cfg")
    guidance = (1.01 if cot == "off" else 1.0) if guidance in (None, 0) else float(guidance)
    negative = gen.negative_prefix(tok, cot, abc_ids) if guidance != 1 else None
    emit(type="status", text="generating semantic tokens …")
    emit(type="progress", step=0, total=semantic.max_tokens)
    t = time.time()
    ids, truncated = gen.generate_tokens(pipe.model, prefix, semantic, seed, "semantic", negative, guidance,
                                         legacy_off=cot == "off", on_token=token_progress(semantic.max_tokens, FPS))
    codec = [i - gen.CODEC_OFFSET for i in ids]
    if not codec:
        raise RuntimeError("the semantic stage produced no codec tokens")
    emit(type="log", text=f"semantic: {len(codec)} frames ({len(codec) / FPS:.1f} s of audio) in {time.time() - t:.1f} s, "
                          f"cfg {guidance}" + (" (hit the length cap)" if truncated else " (song ended)"))

    # 3. acoustic latents (flow matching)
    emit(type="status", text="NAR flow matching …")
    emit(type="progress", step=0, total=steps)
    latents = gen.synthesize(pipe.model, prefix, codec, seed, steps,
                             on_progress=lambda i, n: emit(type="progress", step=i, total=n))

    # 4. waveform
    emit(type="status", text="VAE decode …")
    audio = pipe.decode(latents)
    if truncated:
        # cut off at the cap: fade the last seconds out rather than stopping mid-note
        audio = np.array(audio, dtype=np.float32)
        n = min(len(audio), int(FADE_SECONDS * gen.SAMPLE_RATE))
        audio[-n:] *= np.linspace(1, 0, n, dtype=np.float32)[:, None]
        emit(type="log", text=f"the song ran past the length cap: faded out over the last {FADE_SECONDS:.0f} s")
    out = Path(req["output"])
    out.parent.mkdir(parents=True, exist_ok=True)
    gen.write_wav(out, audio)
    abc_path = None
    if abc_text is not None:
        abc_path = out.with_suffix(".abc")
        abc_path.write_text(abc_text)
    return out, audio.shape[0] / gen.SAMPLE_RATE, abc_path


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        gen, pipe = pipeline_for(req)
        mx.reset_peak_memory()
        t0 = time.time()
        out, seconds, abc_path = run(req, gen, pipe)
        result = dict(type="result", file=str(out), seconds=time.time() - t0, duration=seconds,
                      peakGB=mx.get_peak_memory() / 2**30)
        if abc_path is not None:
            result["abc"] = str(abc_path)
        emit(**result)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
    finally:
        mx.clear_cache()
