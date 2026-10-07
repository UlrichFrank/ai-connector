# mlxlab's HeartMuLa worker: keeps the HeartMuLa 3B language model and the HeartCodec decoder loaded (PyTorch on MPS)
# and turns style tags + lyrics into a song.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "heartmula-3b",
#             "dirs": {"gen": "/HeartMuLa/HeartMuLaGen (tokenizer.json, gen_config.json)",
#                      "mula": "/HeartMuLa/HeartMuLa-oss-3B-happy-new-year",
#                      "codec": "/HeartMuLa/HeartCodec-oss-20260123"},
#             "prompt": "pop, piano, female vocal, happy", "lyrics": "[Verse]\n…", "instrumental": false,
#             "duration": 30, "steps": 0, "seed": 123, "output": "/abs/song.wav"}
#             optional: "topk" (50), "temperature" (1.0), "cfg_scale" (1.5), "device" ("mps"|"cpu")
#             "steps" = HeartCodec flow-matching steps (0 -> upstream default 10).
#   events:  {"type": "status"|"progress"|"log"|"result"|"error", …}
import json
import os
import sys
import time
import traceback

events = os.fdopen(3, "w", buffering=1)


def emit(**kw):
    events.write(json.dumps(kw) + "\n")
    events.flush()


emit(type="status", text="starting HeartMuLa …")
os.environ.setdefault("PYTORCH_ENABLE_MPS_FALLBACK", "1")  # ops missing on MPS run on the CPU instead of failing
os.environ.setdefault("HF_HUB_OFFLINE", "1")
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
import torch  # noqa: E402
import soundfile as sf  # noqa: E402
from tokenizers import Tokenizer  # noqa: E402
from heartlib import HeartMuLaGenPipeline  # noqa: E402
from heartlib.pipelines.music_generation import HeartMuLaGenConfig  # noqa: E402

import math  # noqa: E402
from heartlib.heartcodec.models import transformer as _codec_tf  # noqa: E402


def _timestep_embedding(self, timesteps, max_period=10000, scale=1000):
    # upstream casts with .type(timesteps.type()), which rejects 'torch.mps.FloatTensor'; same math with .to(dtype)
    half = self.flow_t_size // 2
    freqs = torch.exp(-math.log(max_period) * torch.arange(start=0, end=half, device=timesteps.device) / half)
    args = timesteps[:, None] * freqs.to(timesteps.dtype)[None] * scale
    embedding = torch.cat([torch.cos(args), torch.sin(args)], dim=-1)
    if self.flow_t_size % 2:
        embedding = torch.cat([embedding, torch.zeros_like(embedding[:, :1])], dim=-1)
    return embedding


_codec_tf.PixArtAlphaCombinedFlowEmbeddings.timestep_embedding = _timestep_embedding

FRAME_MS = 80  # one HeartMuLa frame = 8 codebook tokens = 80 ms of audio (12.5 Hz)
SAMPLE_RATE = 48000

loaded = {"key": None, "pipe": None}


def default_device():
    return "mps" if torch.backends.mps.is_available() else "cpu"


def peak_gb():
    if torch.backends.mps.is_available():
        return torch.mps.driver_allocated_memory() / 2**30
    return 0.0


def free():
    loaded["pipe"] = None
    import gc

    gc.collect()
    if torch.backends.mps.is_available():
        torch.mps.empty_cache()


def pipe_for(req):
    dirs = req["dirs"]
    dev = req.get("device") or default_device()
    key = (req.get("model"), dirs["gen"], dirs["mula"], dirs["codec"], dev)
    if loaded["key"] != key:
        free()
        emit(type="status", text=f"loading {req.get('model', 'heartmula-3b')} ({dev}) …")
        t0 = time.time()
        device = torch.device(dev)
        # built directly instead of from_pretrained so the three repos can live in separate directories;
        # lazy_load stays off (its unload path calls torch.cuda.*)
        pipe = HeartMuLaGenPipeline(
            heartmula_path=dirs["mula"],
            heartcodec_path=dirs["codec"],
            heartmula_device=device,
            heartcodec_device=device,
            heartmula_dtype=torch.bfloat16,
            heartcodec_dtype=torch.float32,
            lazy_load=False,
            muq_mulan=None,
            text_tokenizer=Tokenizer.from_file(os.path.join(dirs["gen"], "tokenizer.json")),
            config=HeartMuLaGenConfig.from_file(os.path.join(dirs["gen"], "gen_config.json")),
        )
        pipe.mula.eval()
        pipe.codec.eval()
        loaded["key"], loaded["pipe"] = key, pipe
        emit(type="log", text=f"loaded HeartMuLa + HeartCodec in {time.time() - t0:.1f} s")
    return loaded["pipe"]


def generate_frames(pipe, inputs, max_frames, temperature, topk, cfg_scale):
    """HeartMuLaGenPipeline._forward with a progress event per frame (upstream only has tqdm)."""
    dev = pipe.mula_device
    tokens = inputs["tokens"].to(dev)
    tokens_mask = inputs["tokens_mask"].to(dev)
    muq = inputs["muq_embed"].to(dev)
    pos = inputs["pos"].to(dev)
    n = pipe._parallel_number
    pipe.mula.setup_caches(2 if cfg_scale != 1.0 else 1)
    # the 3B backbone's KV cache holds 8192 positions (prompt + frames)
    max_frames = min(max_frames, pipe.mula.backbone.max_seq_len - tokens.shape[1] - 1)
    emit(type="status", text="generating …")
    emit(type="progress", step=0, total=max_frames)
    with torch.autocast(device_type=dev.type, dtype=pipe.mula_dtype):
        cur = pipe.mula.generate_frame(tokens=tokens, tokens_mask=tokens_mask, input_pos=pos, temperature=temperature,
                                       topk=topk, cfg_scale=cfg_scale, continuous_segments=muq,
                                       starts=inputs["muq_idx"])
    frames = [cur[0:1]]
    last = time.time()
    for i in range(max_frames):
        tok = torch.full((cur.shape[0], n), pipe.config.empty_id, device=cur.device, dtype=torch.long)
        tok[:, :-1] = cur
        tok = tok.unsqueeze(1)
        mask = torch.ones_like(tok, dtype=torch.bool)
        mask[..., -1] = False
        with torch.autocast(device_type=dev.type, dtype=pipe.mula_dtype):
            cur = pipe.mula.generate_frame(tokens=tok, tokens_mask=mask, input_pos=pos[..., -1:] + i + 1,
                                           temperature=temperature, topk=topk, cfg_scale=cfg_scale,
                                           continuous_segments=None, starts=None)
        if torch.any(cur[0:1, :] >= pipe.config.audio_eos_id):
            emit(type="log", text=f"end of song after {i + 1} frames ({(i + 1) * FRAME_MS / 1000:.1f} s)")
            break
        frames.append(cur[0:1])
        now = time.time()
        if now - last >= 0.25 or i + 1 == max_frames:
            emit(type="progress", step=i + 1, total=max_frames)
            last = now
    emit(type="progress", step=max_frames, total=max_frames)
    return torch.stack(frames).permute(1, 2, 0).squeeze(0)  # [8 codebooks, frames]


def run(req):
    pipe = pipe_for(req)
    seed = int(req.get("seed") or 0)
    torch.manual_seed(seed)
    if torch.backends.mps.is_available():
        torch.mps.manual_seed(seed)
    lyrics = (req.get("lyrics") or "").strip()
    if req.get("instrumental") or not lyrics:
        lyrics = "[Instrumental]"
    cfg_scale = float(req.get("cfg_scale") or 1.5)
    duration = float(req.get("duration") or 30)
    steps = int(req.get("steps") or 0) or 10
    t0 = time.time()
    with torch.no_grad():
        inputs = pipe.preprocess({"tags": req.get("prompt") or "", "lyrics": lyrics}, cfg_scale=cfg_scale)
        frames = generate_frames(pipe, inputs, int(duration * 1000) // FRAME_MS,
                                 float(req.get("temperature") or 1.0), int(req.get("topk") or 50), cfg_scale)
        emit(type="status", text=f"decoding {frames.shape[-1] * FRAME_MS / 1000:.1f} s of audio …")
        wav = pipe.codec.detokenize(frames.to(pipe.codec_device), num_steps=steps, disable_progress=True)
    wav = wav.to(torch.float32).cpu()  # [channels, samples]
    out = req["output"]
    os.makedirs(os.path.dirname(os.path.abspath(out)), exist_ok=True)
    if out.lower().endswith(".wav"):
        sf.write(out, wav.numpy().T, SAMPLE_RATE, subtype="PCM_16")
    else:  # mp3/flac/…: write a wav first, ffmpeg converts
        import subprocess
        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
            sf.write(tmp.name, wav.numpy().T, SAMPLE_RATE, subtype="PCM_16")
        subprocess.run(["ffmpeg", "-y", "-loglevel", "error", "-i", tmp.name, out], check=True)
        os.unlink(tmp.name)
    emit(type="result", file=out, seconds=time.time() - t0, duration=wav.shape[-1] / SAMPLE_RATE, peakGB=peak_gb())


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        run(json.loads(line))
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
    finally:
        if torch.backends.mps.is_available():
            torch.mps.empty_cache()
