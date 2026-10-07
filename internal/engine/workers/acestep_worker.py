# mlxlab's ACE-Step 1.5 worker: keeps the DiT (MLX) and the 5 Hz LM (mlx-lm) loaded and writes songs.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   request: {"model": "ace-step-1.5-turbo" | "ace-step-1.5-xl-turbo",
#             "dirs": {"main": "/dir", "xl-turbo": "/dir", "lm": "/dir"},
#             "prompt": "…", "lyrics": "[verse]\n…", "instrumental": false, "duration": 30, "steps": 0, "seed": 1,
#             "output": "/path/out.wav",
#             optional: "lm": true (use the 5 Hz LM to plan the song; default true), "language": "en", "bpm": 120,
#                       "key": "C major", "shift": 3.0, "guidance": 7.0 (base/xl-base only),
#                       "lm_temperature": 0.85 (the planner's sampling temperature), "cot_caption": true (let the
#                       planner rewrite the caption the DiT gets; false keeps the caption as written),
#                       "lora": {"file": "name.safetensors" (default adapter_model.safetensors), "scale": 1.0} (a PEFT
#                       LoRA in dirs.lora — a trained style or a community LoRA — merged into the MLX DiT's weights
#                       per request, without reloading the model; see apply_style),
#                       studio tasks: "task": "text2music" (default) | "cover" | "repaint" | "extend" | "lego" | "extract",
#                       "src_audio": "/abs/song.wav|mp3" (all tasks but text2music), "start"/"end": seconds (repaint,
#                       lego; end -1 = to the end), "strength": 0..1 (cover, default 0.8), "track": "vocals"|"drums"|…
#                       (lego, extract), "extend": seconds to add after src_audio (extend; optional "start" < source
#                       length regenerates the tail too)}
#   models:  turbo / xl-turbo do text2music, cover, repaint, extend; "ace-step-1.5-base" (CFG) also lego, extract;
#            "ace-step-1.5-xl-base" (the 4B base DiT, CFG) text2music, cover, repaint, extend
#   dirs:    main     = hf ACE-Step/Ace-Step1.5 (2B turbo DiT, VAE, Qwen3-Embedding-0.6B text encoder, 1.7B LM); always needed
#            xl-turbo = hf ACE-Step/acestep-v15-xl-turbo (4B DiT); only for "ace-step-1.5-xl-turbo"
#            base     = hf ACE-Step/acestep-v15-base (2B base DiT); only for "ace-step-1.5-base"
#            xl-base  = hf ACE-Step/acestep-v15-xl-base (4B base DiT); only for "ace-step-1.5-xl-base"
#            lora     = a PEFT LoRA repository (with "lora" in the request)
#            lm       = optional other LM repo (hf ACE-Step/acestep-5Hz-lm-0.6B or -4B; "lm_name" names it in the
#                       status); default is the 1.7B LM in main
#   events:  {"type": "status"|"progress"|"log"|"result"|"error", …}
#
# The worker never downloads: it builds a private "checkpoints" folder of symlinks to the given dirs (the layout
# ACE-Step expects) and runs with HF_HUB_OFFLINE=1.
import atexit
import ctypes
import gc
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import traceback

events = os.fdopen(3, "w", buffering=1)
_emit_lock = threading.Lock()


def emit(**kw):
    with _emit_lock:
        events.write(json.dumps(kw) + "\n")
        events.flush()


emit(type="status", text="starting ACE-Step …")
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
os.environ.setdefault("ACESTEP_LM_BACKEND", "mlx")
os.environ.setdefault("ACESTEP_GENERATION_TIMEOUT", "7200")  # library default (600 s) is too short for long XL songs
# decode the audio in windows of 384 latent frames (~15 s): bounds the decode memory and keeps each Metal command
# buffer short (one long buffer can be killed by macOS with "Impacting Interactivity" when the GPU is busy)
os.environ.setdefault("ACESTEP_MLX_VAE_CHUNK", "384")
os.environ.pop("ACESTEP_CHECKPOINTS_DIR", None)
os.environ.pop("ACESTEP_VAE_CHECKPOINT", None)

import mlx.core as mx  # noqa: E402
from mlx.utils import tree_map  # noqa: E402
import numpy as np  # noqa: E402
import soundfile as sf  # noqa: E402
import torch  # noqa: E402

# MLX keeps freed buffers in its cache; without a limit the VAE decode alone parks ~20 GB there
mx.set_cache_limit(int(float(os.environ.get("MLXLAB_MLX_CACHE_GB", "2")) * 2**30))

import acestep.core.generation.handler.mlx_vae_decode_native as _vae_decode_mod  # noqa: E402
import acestep.core.generation.handler.mlx_vae_encode_native as _vae_encode_mod  # noqa: E402
import acestep.llm_inference as _llm_mod  # noqa: E402
import acestep.models.mlx.dit_generate as _dit_mod  # noqa: E402
from acestep.constants import TASK_INSTRUCTIONS  # noqa: E402
from acestep.handler import AceStepHandler  # noqa: E402
from acestep.inference import GenerationConfig, GenerationParams, generate_music  # noqa: E402
from acestep.llm_inference import LLMHandler  # noqa: E402

# model id -> (checkpoint folder name ACE-Step knows, dirs key holding it, sub folder inside that dir, default steps)
MODELS = {
    "ace-step-1.5-turbo": ("acestep-v15-turbo", "main", "acestep-v15-turbo", 8),
    "ace-step-1.5-xl-turbo": ("acestep-v15-xl-turbo", "xl-turbo", "", 8),
    "ace-step-1.5-base": ("acestep-v15-base", "base", "", 32),  # needed for lego and extract
    "ace-step-1.5-xl-base": ("acestep-v15-xl-base", "xl-base", "", 32),
}
# per-model defaults the library does not apply itself: timestep shift (3.0 is the turbo recommendation, 1.0 the
# base/SFT default); the guidance scale is forced to 1.0 for turbo by the library, base uses CFG 7.0
SHIFT = {"ace-step-1.5-turbo": 3.0, "ace-step-1.5-xl-turbo": 3.0, "ace-step-1.5-base": 1.0, "ace-step-1.5-xl-base": 1.0}
TASKS = ("text2music", "cover", "repaint", "extend", "lego", "extract")
BASE_ONLY = ("lego", "extract")
TRACKS = ("vocals", "backing_vocals", "drums", "bass", "guitar", "keyboard", "percussion", "strings", "synth", "fx",
          "brass", "woodwinds")
LM_NAME = "acestep-5Hz-lm-1.7B"


# ---------------------------------------------------------------- progress: replace the libraries' tqdm bars
_last_status = [None]


def status(text):
    if text != _last_status[0]:
        _last_status[0] = text
        emit(type="status", text=text)


class _Bar:
    """Stand-in for tqdm that reports to mlxlab instead of drawing to the terminal."""

    def __init__(self, iterable=None, total=None, desc=None, phase=None, quiet=None, **_kw):
        self.iterable = iterable
        self.total = total if total is not None else (len(iterable) if hasattr(iterable, "__len__") else 0)
        self.n = 0
        self.last = 0.0
        self.quiet = quiet() if quiet else False
        if phase:
            status(phase)
        self._report(force=True)

    def _report(self, force=False):
        now = time.time()
        if self.total and not self.quiet and (force or self.n >= self.total or now - self.last >= 0.25):
            self.last = now
            emit(type="progress", step=int(min(self.n, self.total)), total=int(self.total))

    def __iter__(self):
        for item in self.iterable:
            yield item
            self.n += 1
            self._report()

    def update(self, n=1):
        self.n += n
        self._report()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()

    def close(self):
        pass

    def __getattr__(self, name):  # set_description, set_postfix, refresh, …
        return lambda *a, **k: None


def _bar_factory(phase, quiet=None):
    return lambda *a, **kw: _Bar(*a, phase=phase, quiet=quiet, **{k: v for k, v in kw.items() if k not in ("phase", "quiet")})


_dit_mod.tqdm = _bar_factory("diffusion …")
_vae_decode_mod.tqdm = _bar_factory("decoding audio …")
_vae_encode_mod.tqdm = _bar_factory("encoding source audio …")
# the LM bars count tokens up to max_new_tokens: useful for the audio codes (≈ 5 per second of audio), not for the
# metadata phase (a fixed 3500 cap of which ~150 are used)
_llm_mod.tqdm = _bar_factory(None, quiet=lambda: not (lib_progress.last or "").startswith("Phase 2"))

_PHASES = {
    "Phase 1": "LM: planning the song (bpm, key, caption) …",
    "Phase 2": "LM: writing audio codes …",
    "Preparing inputs": "preparing inputs …",
    "Generating music": "encoding text, lyrics and codes …",
    "Decoding audio": "decoding audio …",
    "Preparing audio data": "writing audio …",
}


def lib_progress(*args, desc=None, **_kw):
    """ACE-Step's gradio-style progress(value, desc=…): only the phase text is useful (the value is an estimate)."""
    if desc is None and len(args) > 1 and isinstance(args[1], str):
        desc = args[1]
    if not desc or desc == lib_progress.last:
        return
    lib_progress.last = desc
    status(next((v for k, v in _PHASES.items() if desc.startswith(k)), desc))


lib_progress.last = None


# ---------------------------------------------------------------- memory: peak physical footprint of this process
class _RUsageV4(ctypes.Structure):
    _fields_ = [("uuid", ctypes.c_uint8 * 16), ("v", ctypes.c_uint64 * 35)]


_libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib")


def footprint():
    ru = _RUsageV4()
    if _libc.proc_pid_rusage(os.getpid(), 4, ctypes.byref(ru)) != 0:
        return 0
    return ru.v[7]  # ri_phys_footprint (what Activity Monitor calls "Memory"; includes Metal buffers)


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


# ---------------------------------------------------------------- loading
loaded = {"dit_key": None, "dit": None, "lm_key": None, "lm": None, "root": None}


def link(src, dst):
    if not os.path.isdir(src):
        raise FileNotFoundError(f"missing model folder {src}")
    os.symlink(src, dst)


def link_dit(src, dst):
    # a real folder with the weights symlinked: ACE-Step copies its own model code (.py) into it, which must not
    # touch the downloaded files
    if not os.path.isdir(src):
        raise FileNotFoundError(f"missing model folder {src}")
    os.makedirs(dst)
    for f in os.listdir(src):
        if f.startswith("."):
            continue
        if f.endswith(".py"):
            shutil.copy(os.path.join(src, f), os.path.join(dst, f))
        else:
            os.symlink(os.path.join(src, f), os.path.join(dst, f))


def build_root(req):
    """Make <tmp>/checkpoints/{acestep-v15-turbo, [acestep-v15-xl-turbo], vae, Qwen3-Embedding-0.6B, LMs}."""
    dirs = req.get("dirs") or {}
    main = dirs.get("main")
    if not main:
        raise ValueError('dirs.main (ACE-Step/Ace-Step1.5) is required')
    root = tempfile.mkdtemp(prefix="mlxlab-acestep-")
    ck = os.path.join(root, "checkpoints")
    os.makedirs(ck)
    link(os.path.join(main, "vae"), os.path.join(ck, "vae"))
    link(os.path.join(main, "Qwen3-Embedding-0.6B"), os.path.join(ck, "Qwen3-Embedding-0.6B"))
    link(os.path.join(main, LM_NAME), os.path.join(ck, LM_NAME))
    link_dit(os.path.join(main, "acestep-v15-turbo"), os.path.join(ck, "acestep-v15-turbo"))
    name, key, sub, _ = MODELS[req["model"]]
    if name != "acestep-v15-turbo":
        if not dirs.get(key):
            raise ValueError(f"dirs.{key} is required for {req['model']}")
        link_dit(os.path.join(dirs[key], sub), os.path.join(ck, name))
    return root


def trim():
    gc.collect()
    mx.clear_cache()
    if torch.backends.mps.is_available():
        torch.mps.synchronize()
        torch.mps.empty_cache()


def free():
    loaded.update(dit=None, lm=None, dit_key=None, lm_key=None)
    trim()
    if loaded["root"]:
        shutil.rmtree(loaded["root"], ignore_errors=True)
        loaded["root"] = None


def models_for(req):
    if req["model"] not in MODELS:
        raise ValueError(f"unknown model {req['model']!r} (known: {', '.join(MODELS)})")
    dirs = req.get("dirs") or {}
    # a LoRA (dirs.lora) is merged into the loaded DiT per request (apply_style): it does not reload the model
    dit_key = (req["model"], json.dumps({k: v for k, v in dirs.items() if k not in ("lm", "lora")}, sort_keys=True))
    # the LM only plans text2music; ACE-Step skips it for the audio-to-audio tasks, so don't load it for them
    use_lm = req.get("lm", True) is not False and (req.get("task") or "text2music") == "text2music"
    if loaded["dit_key"] != dit_key:
        free()
        loaded["root"] = build_root(req)
        os.environ["ACESTEP_PROJECT_ROOT"] = loaded["root"]
        emit(type="status", text=f"loading {req['model']} (DiT, VAE, text encoder) …")
        t0 = time.time()
        dit = AceStepHandler()
        # Load the PyTorch DiT on the CPU: its decoder only feeds the MLX conversion and is then dropped, the rest
        # moves to MPS afterwards. Loaded straight onto MPS, the freed decoder stays parked in torch's MPS allocator
        # (~14 GB for the XL).
        load_main = dit._load_main_model_from_checkpoint
        dit._load_main_model_from_checkpoint = lambda **kw: load_main(**{**kw, "device": "cpu"})
        msg, ok = dit.initialize_service(
            project_root=loaded["root"],
            config_path=MODELS[req["model"]][0],
            device="mps",
            use_mlx_dit=os.environ.get("MLXLAB_ACESTEP_MLX_DIT", "1") != "0",  # 0: PyTorch/MPS diffusion (debugging)
            prefer_source="huggingface",
        )
        del dit._load_main_model_from_checkpoint
        print(msg, file=sys.stderr)
        if not ok:
            raise RuntimeError(msg.splitlines()[0] if msg else "DiT initialisation failed")
        mlx_dit = getattr(dit, "use_mlx_dit", False) and getattr(dit, "mlx_decoder", None) is not None
        # lego/extract come out wrong from ACE-Step's MLX diffusion (the result is close to the input mix; the
        # PyTorch loop gives real stems), so the base model keeps its PyTorch decoder for those two tasks
        keep_torch = not mlx_dit or req["model"] == "ace-step-1.5-base"
        dit._mlxlab_mlx_dit = mlx_dit
        dit._mlxlab_model = req["model"]
        if mlx_dit:
            if not keep_torch:
                # the diffusion runs on the MLX copy of the decoder; the float32 PyTorch original only backs the
                # library's fallback path and its lyric-timestamp tools, so give its memory back
                dit.model.decoder.to("meta")
            if os.environ.get("MLXLAB_ACESTEP_DIT_DTYPE", "bfloat16") == "bfloat16":
                # the checkpoints are bf16-trained (the 2B turbo ships as bf16, so this is lossless there; the XL
                # ships float32 master weights and runs in bf16 on CUDA too); halves the DiT's memory
                dit.mlx_decoder.update(tree_map(
                    lambda v: v.astype(mx.bfloat16) if isinstance(v, mx.array) and v.dtype == mx.float32 else v,
                    dit.mlx_decoder.parameters()))
                mx.eval(dit.mlx_decoder.parameters())
        else:
            emit(type="log", text="MLX DiT unavailable, diffusion runs on PyTorch/MPS (slow)")
        gc.collect()
        decoder, dit.model.decoder = dit.model.decoder, torch.nn.Identity()
        dit.model = dit.model.to(dit.device)
        dit.model.decoder = decoder.to(dit.device) if keep_torch else decoder
        dit.silence_latent = dit.silence_latent.to(dit.device)
        trim()
        loaded["dit"], loaded["dit_key"] = dit, dit_key
        emit(type="log", text=f"loaded {req['model']} in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    lm_key = (dit_key, dirs.get("lm") or LM_NAME) if use_lm else None
    if use_lm and loaded["lm_key"] != lm_key:
        loaded["lm"] = None
        gc.collect()
        mx.clear_cache()
        lm_dir = LM_NAME
        if dirs.get("lm"):  # another LM: linked next to the others (the DiT's checkpoint folder stays as it is)
            lm_dir = "lm-" + hashlib.sha1(dirs["lm"].encode()).hexdigest()[:10]
            if not os.path.exists(os.path.join(loaded["root"], "checkpoints", lm_dir)):
                link(dirs["lm"], os.path.join(loaded["root"], "checkpoints", lm_dir))
        emit(type="status", text=f"loading 5 Hz LM ({req.get('lm_name') or lm_dir}) …")
        t0 = time.time()
        lm = LLMHandler()
        msg, ok = lm.initialize(
            checkpoint_dir=os.path.join(loaded["root"], "checkpoints"),
            lm_model_path=lm_dir,
            backend="mlx",
            device="mps",
        )
        print(msg, file=sys.stderr)
        if not ok:
            raise RuntimeError(msg.splitlines()[0] if msg else "LM initialisation failed")
        loaded["lm"], loaded["lm_key"] = lm, lm_key
        trim()
        emit(type="log", text=f"loaded LM in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    return loaded["dit"], (loaded["lm"] if use_lm else None)


# ---------------------------------------------------------------- styles: LoRA weights merged into the MLX DiT
# A style is a PEFT LoRA of the DiT decoder (q/k/v/o projections; trained by train_acestep.py, or a community one).
# ACE-Step applies LoRAs to its PyTorch decoder only; the diffusion here runs on the MLX copy, whose parameters carry
# the PyTorch names, so the LoRA is merged into those weights: W += scale · alpha/r · B·A. The original weights of
# every touched layer are kept, so another style (or none) restores them first.
def read_lora(path, file=None):
    """A PEFT LoRA folder (adapter_config.json + *.safetensors; file: which one) → ({mlx param name: (A, B)}, alpha/r)."""
    cfg_file = os.path.join(path, "adapter_config.json")
    if not os.path.isfile(cfg_file):
        raise ValueError(f"no LoRA in {path} (adapter_config.json missing)")
    cfg = json.load(open(cfg_file))
    if str(cfg.get("peft_type", "LORA")).upper() != "LORA":
        raise ValueError(f"only LoRA adapters are supported, this is {cfg.get('peft_type')}")
    files = sorted(f for f in os.listdir(path) if f.endswith(".safetensors"))
    if not files:
        raise ValueError(f"no .safetensors weights in {path}")
    pairs = {}
    if file and file not in files:
        raise ValueError(f"no {file} in {path}")
    weights = mx.load(os.path.join(path, file or ("adapter_model.safetensors" if "adapter_model.safetensors" in files else files[0])))
    for k, v in weights.items():
        for part in (".lora_A.", ".lora_B."):
            if part in k:
                name = k.split(part)[0].removeprefix("base_model.model.") + ".weight"
                pairs.setdefault(name, [None, None])[part == ".lora_B."] = v.astype(mx.float32)
    r = int(cfg.get("r") or 8)
    alpha = float(cfg.get("lora_alpha") or r)
    return {k: v for k, v in pairs.items() if v[0] is not None and v[1] is not None}, (
        alpha / r**0.5 if cfg.get("use_rslora") else alpha / r)


def merge_style_lora(dit, pairs, factor, key=None):
    """Merges LoRA pairs ({name: (A, B)}, factor = scale · alpha/r) into the MLX decoder; pairs None restores the
    base weights. key names what is merged (the same key again: nothing to do)."""
    from mlx.utils import tree_flatten
    if pairs is None and not getattr(dit, "_mlxlab_lora_base", None):
        return  # no style on, none wanted
    if pairs is not None and key is not None and getattr(dit, "_mlxlab_lora_key", None) == key:
        return
    base = getattr(dit, "_mlxlab_lora_base", None) or {}
    if base:  # back to the original weights first
        dit.mlx_decoder.load_weights(list(base.items()), strict=False)
        base = {}
    dit._mlxlab_lora_base, dit._mlxlab_lora_key = base, None
    if pairs:
        params = dict(tree_flatten(dit.mlx_decoder.parameters()))
        missing = [k for k in pairs if k not in params]
        if missing:
            raise ValueError(f"this style does not fit {dit._mlxlab_model}: the model has no {missing[0]} (trained for another model?)")
        new = []
        for name, (a, b) in pairs.items():
            w = params[name]
            if tuple(w.shape) != (b.shape[0], a.shape[1]):
                raise ValueError(f"this style does not fit {dit._mlxlab_model}: {name} is {tuple(w.shape)}, the LoRA makes "
                                 f"{(b.shape[0], a.shape[1])} (trained for another model?)")
            base[name] = w
            delta = (b @ a) * factor
            new.append((name, (w.astype(mx.float32) + delta).astype(w.dtype)))
        dit.mlx_decoder.load_weights(new, strict=False)
        mx.eval(dit.mlx_decoder.parameters())
        dit._mlxlab_lora_key = key
        emit(type="log", text=f"style merged: {len(new)} layers, factor {factor:.3g}")
    trim()


def apply_style(dit, req):
    """The request's LoRA on the DiT, or none: "lora": {"file": "x.safetensors" (optional), "scale": 0..2} with the
    folder in dirs.lora (a trained style's lora/, or a LoRA repository); "path" in "lora" names the folder too."""
    lora = req.get("lora") or None
    if not lora:
        merge_style_lora(dit, None, 0.0)
        return
    if not dit.use_mlx_dit:
        raise ValueError("a LoRA needs the MLX diffusion; this task runs on PyTorch (lego, extract)")
    path = lora.get("path") or (req.get("dirs") or {}).get("lora")
    if not path:
        raise ValueError("dirs.lora is required with a lora")
    scale, file = float(lora.get("scale") or 1.0), lora.get("file")
    weights = os.path.join(path, file) if file else path
    key = (os.path.realpath(path), file, os.path.getmtime(weights), scale)
    if getattr(dit, "_mlxlab_lora_key", None) == key:
        return
    pairs, factor = read_lora(path, file)
    merge_style_lora(dit, pairs, factor * scale, key)


# ---------------------------------------------------------------- generation
def audio_seconds(path):
    try:
        return sf.info(path).duration
    except Exception:  # noqa: BLE001 (formats libsndfile can't read)
        out = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path],
                             capture_output=True, text=True, check=True).stdout
        return float(out.strip())


def task_params(req):
    """Map the request's studio fields onto ACE-Step's task parameters."""
    task = req.get("task") or "text2music"
    if task not in TASKS:
        raise ValueError(f"unknown task {task!r} (known: {', '.join(TASKS)})")
    if task in BASE_ONLY and req["model"] != "ace-step-1.5-base":
        raise ValueError(f"task {task} needs the base model (ace-step-1.5-base); turbo models only do "
                         "text2music, cover, repaint and extend")
    if task == "text2music":
        return dict(task_type="text2music")
    src = req.get("src_audio")
    if not src or not os.path.isfile(src):
        raise ValueError(f"task {task} needs src_audio (an existing audio file), got {src!r}")
    p = dict(task_type=task, src_audio=src, enable_normalization=False)  # keep the source's level
    if task == "cover":
        p["audio_cover_strength"] = float(req["strength"]) if req.get("strength") is not None else 0.8
    elif task in ("repaint", "extend"):
        if task == "extend":
            # ACE-Step outpaints when the repaint window reaches past the source: the source is padded with
            # silence up to `end` and only [start, end] is generated
            src_len = audio_seconds(src)
            add = float(req.get("extend") or req.get("duration") or 30)
            start = float(req["start"]) if req.get("start") is not None else src_len
            p.update(task_type="repaint", repainting_start=start, repainting_end=src_len + add)
        else:
            p.update(repainting_start=float(req.get("start") or 0.0),
                     repainting_end=float(req["end"]) if req.get("end") is not None else -1.0)
        p["chunk_mask_mode"] = "explicit"  # what ACE-Step's own UI uses for repaint
    if task in ("lego", "extract"):
        track = (req.get("track") or "").strip().lower() or None
        if track and track not in TRACKS:
            raise ValueError(f"unknown track {track!r} (known: {', '.join(TRACKS)})")
        p["instruction"] = (TASK_INSTRUCTIONS[task].format(TRACK_NAME=track.upper()) if track
                            else TASK_INSTRUCTIONS[task + "_default"])  # as AceStepHandler.generate_instruction
        if task == "lego":
            p.update(repainting_start=float(req.get("start") or 0.0),
                     repainting_end=float(req["end"]) if req.get("end") is not None else -1.0)
    return p


def generate(req, dit, lm):
    model = req["model"]
    tp = task_params(req)
    task = tp["task_type"]
    steps = int(req.get("steps") or 0) or MODELS[model][3]
    seed = req.get("seed")
    seed = -1 if seed is None or int(seed) < 0 else int(seed)
    instrumental = bool(req.get("instrumental"))
    lyrics = (req.get("lyrics") or "").strip()
    if instrumental or (not lyrics and task == "text2music"):
        lyrics = "[Instrumental]"
        instrumental = True
    if task != "text2music":
        lm = None  # (not loaded for these tasks anyway)
    dit.use_mlx_dit = dit._mlxlab_mlx_dit and task not in BASE_ONLY
    if dit._mlxlab_mlx_dit and not dit.use_mlx_dit:
        emit(type="log", text=f"{task}: diffusion on PyTorch/MPS (ACE-Step's MLX path gets this task wrong)")
    apply_style(dit, req)
    params = GenerationParams(
        caption=req.get("prompt") or "",
        lyrics=lyrics,
        instrumental=instrumental,
        vocal_language=req.get("language") or "unknown",
        bpm=int(req["bpm"]) if req.get("bpm") else None,
        keyscale=req.get("key") or "",
        duration=float(req.get("duration") or 30) if task == "text2music" else -1.0,
        inference_steps=steps,
        seed=seed,
        shift=float(req.get("shift") or SHIFT[model]),
        thinking=lm is not None,
        use_cot_metas=lm is not None,
        use_cot_caption=lm is not None and req.get("cot_caption", True) is not False,
        use_cot_language=lm is not None,
        **tp,
    )
    if req.get("lm_temperature"):
        params.lm_temperature = float(req["lm_temperature"])
    if req.get("guidance"):
        params.guidance_scale = float(req["guidance"])
    config = GenerationConfig(
        batch_size=1,
        use_random_seed=seed < 0,
        seeds=[seed] if seed >= 0 else None,
    )
    lib_progress.last = None
    _last_status[0] = None
    if seed >= 0:  # the LM samples with MLX's global generator; the DiT gets the seed explicitly
        mx.random.seed(seed)
        torch.manual_seed(seed)
    result = generate_music(dit, lm, params, config, save_dir=None, progress=lib_progress)
    if not result.success or not result.audios:
        raise RuntimeError(result.error or result.status_message or "generation failed")
    audio = result.audios[0]
    wav = audio["tensor"].detach().to("cpu", torch.float32).numpy()  # [channels, samples]
    sr = int(audio.get("sample_rate") or 48000)
    if wav.ndim == 1:
        wav = wav[None, :]
    if wav.shape[0] == 1:
        wav = np.repeat(wav, 2, axis=0)
    os.makedirs(os.path.dirname(os.path.abspath(req["output"])), exist_ok=True)
    sf.write(req["output"], np.clip(wav.T, -1.0, 1.0), sr, subtype="PCM_16", format="WAV")
    meta = (result.extra_outputs or {}).get("lm_metadata") or {}
    if meta:
        emit(type="log", text="LM: " + ", ".join(f"{k}={meta[k]}" for k in ("bpm", "keyscale", "timesignature", "duration", "vocal_language") if meta.get(k)))
    return wav.shape[1] / sr


atexit.register(lambda: loaded["root"] and shutil.rmtree(loaded["root"], ignore_errors=True))


def serve():
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
                dit, lm = models_for(req)
                load = time.time() - t0
                mx.reset_peak_memory()
                t0 = time.time()
                duration = generate(req, dit, lm)
                seconds = time.time() - t0
            finally:
                dit = lm = None  # only `loaded` may hold the models, or a model switch could not free them
                peak = sampler.done()
            # seconds: generation only; loadSeconds: model loading done for this request (0 when already loaded);
            # peakGB: peak physical footprint of the process (torch MPS + MLX + CPU) during the request
            emit(type="result", file=req["output"], seconds=seconds, duration=duration, peakGB=peak,
                 loadSeconds=load, mlxPeakGB=mx.get_peak_memory() / 2**30)
        except Exception as e:  # noqa: BLE001
            traceback.print_exc()
            emit(type="error", text=f"{type(e).__name__}: {e}")


if __name__ == "__main__":  # (train_acestep.py imports this file for its loading, styles and sample songs)
    serve()
