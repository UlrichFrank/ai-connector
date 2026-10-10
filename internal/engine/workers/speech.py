# mlxlab's speech worker: text to speech and speech to text with mlx-audio (github.com/Blaizzy/mlx-audio, MIT).
# Keeps one TTS model (and the ASR model, when asked to transcribe) loaded.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines).
#   tts:  {"op": "tts", "family": "qwen3-tts"|"chatterbox"|"voxcpm2"|"higgs"|"voxtral"|"kokoro",
#          "dirs": {"model": "/dir", …}  (the model's folder; chatterbox also "conds" and "s3tok"),
#          "voice": {"mode": "preset"|"clone"|"design"|"default", "preset": "Ryan", "ref_audio": "/x.wav",
#                    "ref_text": "…", "description": "…"},
#          "instruct": "…", "language": "de", "seed": 1, "exaggeration": 0.5 (chatterbox),
#          "chunks": [{"text": "…", "pause": 0.3}], "output": "/out.wav"}
#        → progress per chunk; result {"file", "seconds" (generation), "duration" (audio), "peakGB",
#          "words": [{"text", "start", "end"}] (Kokoro, English: when each word is said, in seconds)}
#   asr:  {"op": "asr", "dirs": {"asr": "/dir"}, "audio": "/x.wav", "language": "de"} → result {"text"}
#   unload_asr: frees the ASR model.
#
# The worker never downloads (HF_HUB_OFFLINE=1): mlxlab fetches the pinned repositories and passes their folders.
import ctypes
import gc
import json
import os
import shutil
import sys
import tempfile
import threading
import time
import traceback
from pathlib import Path

events = os.fdopen(3, "w", buffering=1)
_emit_lock = threading.Lock()


def emit(**kw):
    with _emit_lock:
        events.write(json.dumps(kw) + "\n")
        events.flush()


emit(type="status", text="starting mlx-audio …")
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")

import huggingface_hub  # noqa: E402
import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402
from mlx_audio.audio_io import write as audio_write  # noqa: E402

mx.set_cache_limit(int(float(os.environ.get("MLXLAB_MLX_CACHE_GB", "2")) * 2**30))

# repositories the libraries fetch by themselves (chatterbox: the S3 tokenizer): mapped to the folders mlxlab passes
_local_repos = {}
_snapshot_download = huggingface_hub.snapshot_download


def _offline_snapshot(repo_id=None, *a, **kw):
    repo = repo_id if repo_id is not None else kw.get("repo_id")
    if repo in _local_repos:
        return _local_repos[repo]
    return _snapshot_download(repo_id, *a, **kw)


huggingface_hub.snapshot_download = _offline_snapshot

MODEL_TYPES = {"qwen3-tts": "qwen3_tts", "chatterbox": "chatterbox", "voxcpm2": "voxcpm2", "higgs": "higgs_audio_v3",
               "voxtral": "voxtral_tts", "kokoro": "kokoro"}

LANG_NAMES = {"de": "german", "en": "english", "zh": "chinese", "fr": "french", "es": "spanish", "it": "italian",
              "pt": "portuguese", "ja": "japanese", "ko": "korean", "ru": "russian", "nl": "dutch", "ar": "arabic",
              "hi": "hindi", "pl": "polish", "tr": "turkish", "sv": "swedish", "da": "danish", "fi": "finnish",
              "el": "greek", "he": "hebrew", "ms": "malay", "no": "norwegian", "sw": "swahili"}


# ---------------------------------------------------------------- memory: peak physical footprint of this process
class _RUsageV4(ctypes.Structure):
    _fields_ = [("uuid", ctypes.c_uint8 * 16), ("v", ctypes.c_uint64 * 35)]


_libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib")


def footprint():
    ru = _RUsageV4()
    if _libc.proc_pid_rusage(os.getpid(), 4, ctypes.byref(ru)) != 0:
        return 0
    return ru.v[7]  # ri_phys_footprint (Activity Monitor's "Memory"; includes Metal buffers)


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
loaded = {"key": None, "model": None, "tmp": None, "asr_key": None, "asr": None}


def trim():
    gc.collect()
    mx.clear_cache()


def free_tts():
    loaded.update(key=None, model=None)
    trim()
    if loaded["tmp"]:
        shutil.rmtree(loaded["tmp"], ignore_errors=True)
        loaded["tmp"] = None


def chatterbox_dir(dirs):
    """The multilingual v3 weights plus the default voice (conds.safetensors of the English checkpoint) in one
    folder: mlx-audio reads the default conditionals from the model folder."""
    tmp = tempfile.mkdtemp(prefix="mlxlab-chatterbox-")
    for f in os.listdir(dirs["model"]):
        if not f.startswith("."):
            os.symlink(os.path.join(dirs["model"], f), os.path.join(tmp, f))
    conds = os.path.join(dirs.get("conds") or "", "conds.safetensors")
    if os.path.isfile(conds):
        os.symlink(conds, os.path.join(tmp, "conds.safetensors"))
    if dirs.get("s3tok"):
        _local_repos["mlx-community/S3TokenizerV2"] = dirs["s3tok"]
    return tmp


def tts_model(req):
    from mlx_audio.tts.utils import load_model

    dirs = req.get("dirs") or {}
    if not dirs.get("model"):
        raise ValueError("dirs.model is required")
    key = (req["family"], json.dumps(dirs, sort_keys=True))
    if loaded["key"] != key:
        free_tts()
        path = dirs["model"]
        if req["family"] == "chatterbox":
            loaded["tmp"] = path = chatterbox_dir(dirs)
        emit(type="status", text=f"loading {os.path.basename(os.path.dirname(dirs['model'].rstrip('/')))} …")
        t0 = time.time()
        # the model type is given: mlx-audio would guess it from the folder's name parts (and gets Higgs wrong)
        loaded["model"] = load_model(Path(path), model_type=MODEL_TYPES[req["family"]], model_name_parts=[])
        loaded["key"] = key
        trim()
        emit(type="log", text=f"loaded in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    return loaded["model"]


def asr_model(req):
    from mlx_audio.stt.utils import load_model

    d = (req.get("dirs") or {}).get("asr")
    if not d:
        raise ValueError("dirs.asr is required")
    if loaded["asr_key"] != d:
        loaded["asr"] = None
        trim()
        emit(type="status", text="loading Qwen3-ASR …")
        loaded["asr"] = load_model(Path(d), model_type="qwen3_asr", model_name_parts=[])
        loaded["asr_key"] = d
    return loaded["asr"]


# ---------------------------------------------------------------- text to speech
HIGGS_TAGS = [  # instruct words → Higgs Audio v3 sentence-level control tags (PROMPTING.md of the model)
    (("whisper",), "<|style:whispering|>"), (("shout", "yell", "scream"), "<|style:shouting|>"),
    (("sing",), "<|style:singing|>"),
    (("very slow",), "<|prosody:speed_very_slow|>"), (("slow",), "<|prosody:speed_slow|>"),
    (("very fast",), "<|prosody:speed_very_fast|>"), (("fast", "quick", "hurried"), "<|prosody:speed_fast|>"),
    (("low voice", "deep", "low pitch"), "<|prosody:pitch_low|>"), (("high pitch", "high voice"), "<|prosody:pitch_high|>"),
    (("expressive", "dramatic", "lively"), "<|prosody:expressive_high|>"), (("monotone", "flat"), "<|prosody:expressive_low|>"),
    (("amused", "amusement", "funny", "laugh"), "<|emotion:amusement|>"), (("angry", "anger", "furious"), "<|emotion:anger|>"),
    (("sad", "sadness", "melanchol"), "<|emotion:sadness|>"), (("afraid", "fear", "scared"), "<|emotion:fear|>"),
    (("excited", "enthusias"), "<|emotion:enthusiasm|>"), (("happy", "joy", "elat", "thrilled"), "<|emotion:elation|>"),
    (("warm", "tender", "loving", "affection"), "<|emotion:affection|>"), (("calm", "content", "relaxed"), "<|emotion:contentment|>"),
    (("surprise",), "<|emotion:surprise|>"), (("proud", "pride"), "<|emotion:pride|>"), (("thought", "contempla", "pensive"), "<|emotion:contemplation|>"),
    (("determin", "resolute", "firm"), "<|emotion:determination|>"), (("confus",), "<|emotion:confusion|>"),
    (("relie",), "<|emotion:relief|>"), (("awe",), "<|emotion:awe|>"), (("disgust",), "<|emotion:disgust|>"),
    (("bitter",), "<|emotion:bitterness|>"), (("longing", "yearn"), "<|emotion:longing|>"),
]


def higgs_prefix(instruct, speed):
    instruct = (instruct or "").strip()
    if instruct.startswith("<|"):
        return instruct  # tags given as they are
    low, tags, seen = instruct.lower(), [], set()
    for words, tag in HIGGS_TAGS:
        cat = tag.split(":")[0]
        if cat in seen and cat != "<|prosody":
            continue
        if any(w in low for w in words):
            if "speed" in tag and any("speed" in t for t in tags):
                continue
            tags.append(tag)
            seen.add(cat)
    if speed and not any("speed" in t for t in tags):
        if speed <= 0.7:
            tags.append("<|prosody:speed_very_slow|>")
        elif speed <= 0.88:
            tags.append("<|prosody:speed_slow|>")
        elif speed >= 1.4:
            tags.append("<|prosody:speed_very_fast|>")
        elif speed >= 1.12:
            tags.append("<|prosody:speed_fast|>")
    return "".join(tags)


def gen_kwargs(req, model):
    """The keyword arguments of model.generate for this request (the same for every chunk)."""
    fam, v = req["family"], req.get("voice") or {}
    mode, lang, instruct = v.get("mode") or "default", (req.get("language") or "").lower(), req.get("instruct") or ""
    kw = {}
    if fam == "qwen3-tts":
        kw["lang_code"] = LANG_NAMES.get(lang, "auto")
        mt = getattr(model.config, "tts_model_type", "base")
        if mode == "clone":
            if mt != "base":
                raise ValueError("cloning needs the Base checkpoint")
            kw.update(ref_audio=v["ref_audio"], ref_text=v.get("ref_text") or None)
        elif mode == "design":
            if mt != "voice_design":
                raise ValueError("a designed voice needs the VoiceDesign checkpoint")
            kw["instruct"] = v["description"] + (". " + instruct if instruct else "")
        else:
            if mt != "custom_voice":
                raise ValueError("preset voices need the CustomVoice checkpoint")
            kw["voice"] = v.get("preset") or "Ryan"
            if instruct:
                kw["instruct"] = instruct
    elif fam == "chatterbox":
        kw["lang_code"] = lang or "en"
        kw["exaggeration"] = float(req.get("exaggeration") or 0.5)
        if mode == "clone":
            kw["ref_audio"] = v["ref_audio"]
    elif fam == "voxcpm2":
        if mode == "clone":
            kw["ref_audio"] = v["ref_audio"]
            if v.get("ref_text"):  # with the transcript the sample also continues into the text: closer to the voice
                kw.update(prompt_audio=v["ref_audio"], prompt_text=v["ref_text"].strip() + " ")
        desc = v.get("description") if mode == "design" else ""
        style = ", ".join(x for x in (desc, instruct) if x)
        if style:
            kw["instruct"] = style
    elif fam == "higgs":
        if mode == "clone":
            kw.update(ref_audio=v["ref_audio"], ref_text=v.get("ref_text") or None)
        kw["max_new_tokens"] = 2048
    elif fam == "voxtral":
        kw["voice"] = v.get("preset") or "neutral_female"
    else:
        raise ValueError(f"unknown family {fam!r}")
    return kw


def short_espeak_data():
    """espeak-ng (inside espeakng-loader's wheel) keeps its data path in 160 bytes: when the environment is deep
    (a long MLXLAB_HOME) its data is copied to the temp folder once and used from there."""
    try:
        import espeakng_loader
        from importlib.metadata import version
    except ImportError:
        return
    data = os.path.realpath(espeakng_loader.get_data_path())
    if len(data.encode()) < 150:
        return
    short = os.path.join(tempfile.gettempdir(), "mlxlab-espeak-ng-data-" + version("espeakng-loader"))
    if not os.path.isfile(os.path.join(short, "phontab")):
        tmp = f"{short}.{os.getpid()}"
        shutil.copytree(data, tmp, dirs_exist_ok=True)
        try:
            os.rename(tmp, short)
        except OSError:  # another worker was quicker
            shutil.rmtree(tmp, ignore_errors=True)
    import misaki.espeak  # noqa: F401  (it sets the long path when imported)
    from phonemizer.backend.espeak.wrapper import EspeakWrapper

    EspeakWrapper.set_data_path(short)
    emit(type="log", text=f"espeak-ng's data from {short} (its own path is too long for it)")


# Kokoro's pipelines by language (misaki G2P; American/British English by the voice's first letter)
KOKORO_CODES = {"es": "e", "fr": "f", "hi": "h", "it": "i", "pt": "p", "zh": "z"}


def kokoro_tts(req, model):
    """Kokoro through its pipeline (model.generate does not hand out the tokens): the voice from the weights' voices
    folder (nothing is fetched), the words' times from the tokens (English only: the other languages are phonemized
    by espeak-ng or misaki's zh without tokens)."""
    voice = (req.get("voice") or {}).get("preset") or "af_heart"
    lang = (req.get("language") or "en").lower()
    code = ("b" if voice.startswith("b") else "a") if lang == "en" else KOKORO_CODES.get(lang, "a")
    path = os.path.join(req["dirs"]["model"], "voices", voice + ".safetensors")
    if not os.path.isfile(path):
        raise ValueError(f"no Kokoro voice {voice!r}")
    if code not in model._pipelines:
        emit(type="status", text=f"preparing the {lang} text processing …")
        short_espeak_data()
    pipe = model._get_pipeline(code)
    speed = float(req.get("speed") or 1.0)
    sr = int(model.sample_rate)
    chunks = req.get("chunks") or []
    if not chunks:
        raise ValueError("no text")
    parts, words, at, t0 = [], [], 0, time.time()
    for i, ch in enumerate(chunks):
        emit(type="status", text=f"sentence {i + 1} of {len(chunks)} …")
        emit(type="progress", step=i, total=len(chunks))
        got = False
        for res in pipe(ch["text"], voice=path, speed=speed, split_pattern=None):
            if res.audio is None:
                continue
            a = np.asarray(res.audio, dtype=np.float32).reshape(-1)
            for t in res.tokens or []:
                start, end = getattr(t, "start_ts", None), getattr(t, "end_ts", None)
                if start is None or end is None or not any(c.isalnum() for c in t.text):
                    continue
                words.append({"text": t.text, "start": round(at / sr + start, 3), "end": round(at / sr + end, 3)})
            parts.append(a)
            at += len(a)
            got = True
        if not got:
            raise RuntimeError(f"no audio for: {ch['text'][:60]}")
        pause = float(ch.get("pause") or 0)
        if pause > 0 and i < len(chunks) - 1:
            parts.append(np.zeros(int(pause * sr), dtype=np.float32))
            at += int(pause * sr)
        mx.clear_cache()
    emit(type="progress", step=len(chunks), total=len(chunks))
    wav = np.clip(np.concatenate(parts), -1.0, 1.0)
    seconds = time.time() - t0
    os.makedirs(os.path.dirname(os.path.abspath(req["output"])), exist_ok=True)
    audio_write(req["output"], wav, sr)
    return seconds, len(wav) / sr, words


def tts(req):
    model = tts_model(req)
    fam = req["family"]
    if fam == "kokoro":
        return kokoro_tts(req, model)
    kw = gen_kwargs(req, model)
    prefix = higgs_prefix(req.get("instruct"), req.get("speed")) if fam == "higgs" else ""
    seed = req.get("seed")
    if seed:
        mx.random.seed(int(seed))
        np.random.seed(int(seed) % 2**32)
    chunks = req.get("chunks") or []
    if not chunks:
        raise ValueError("no text")
    sr = int(getattr(model, "sample_rate", 0) or 24000)
    parts, t0 = [], time.time()
    for i, ch in enumerate(chunks):
        emit(type="status", text=f"sentence {i + 1} of {len(chunks)} …")
        emit(type="progress", step=i, total=len(chunks))
        text = prefix + ch["text"] if prefix else ch["text"]
        kwi = dict(kw)
        if fam == "higgs" and seed:
            kwi["seed"] = int(seed) + i
        audio = []
        for res in model.generate(text=text, verbose=False, **kwi):
            audio.append(np.asarray(res.audio, dtype=np.float32).reshape(-1))
            sr = int(getattr(res, "sample_rate", 0) or sr)
        if not audio:
            raise RuntimeError(f"no audio for: {ch['text'][:60]}")
        parts.append(np.concatenate(audio))
        pause = float(ch.get("pause") or 0)
        if pause > 0 and i < len(chunks) - 1:
            parts.append(np.zeros(int(pause * sr), dtype=np.float32))
        mx.clear_cache()
    emit(type="progress", step=len(chunks), total=len(chunks))
    wav = np.clip(np.concatenate(parts), -1.0, 1.0)
    seconds = time.time() - t0
    os.makedirs(os.path.dirname(os.path.abspath(req["output"])), exist_ok=True)
    audio_write(req["output"], wav, sr)
    return seconds, len(wav) / sr, []


def asr(req):
    model = asr_model(req)
    lang = (req.get("language") or "").lower()
    emit(type="status", text="transcribing …")
    t0 = time.time()
    out = model.generate(req["audio"], language=LANG_NAMES.get(lang) if lang else None, verbose=False)
    text = (getattr(out, "text", None) or "").strip()
    return text, time.time() - t0


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        op = req.get("op") or "tts"
        if op == "unload_asr":
            loaded.update(asr=None, asr_key=None)
            trim()
            emit(type="result")
            continue
        sampler = PeakSampler()
        try:
            if op == "asr":
                text, seconds = asr(req)
            else:
                seconds, duration, words = tts(req)
        finally:
            peak = sampler.done()
        if op == "asr":
            emit(type="result", text=text, seconds=seconds, peakGB=peak)
        else:
            res = dict(file=req["output"], seconds=seconds, duration=duration, peakGB=peak)
            if words:
                res["words"] = words
            emit(type="result", **res)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
