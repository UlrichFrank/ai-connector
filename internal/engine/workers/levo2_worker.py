# mlxlab's LeVo 2 (SongGeneration v2) worker: lyrics + style -> song, through the LeVo2.cpp (ggml) command-line tools.
#
#   python -u levo2_worker.py <bin_dir>      (bin_dir holds levo-cli and levo-render, see levo2_build.sh)
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay free).
#   request: {"model": "levo2-large"|"levo2-medium", "dirs": {"levo2": "/dir/with/ggufs"}, "prompt": "female, pop",
#             "lyrics": "[verse]\n...\n[chorus]\n...", "instrumental": false, "duration": 30, "steps": 0, "seed": 1,
#             "output": "/abs/song.wav"}            optional: "cfg" (0 = 1.5), "backend" ("gpu"|"cpu", default gpu)
#   events:  {"type": "ready"|"status"|"progress"|"log"|"result"|"error", ...}
#
# Every request runs two processes: levo-cli (LeLM: lyrics/style -> [3,T] audio tokens, 25 per second) and
# levo-render (Flow + VAE: tokens -> 48 kHz stereo 32-bit float WAV). Both load their models per request.
#
# License: LeVo 2 / SongGeneration and LeVo2.cpp are for academic, research and education use only; no commercial use.
import json
import os
import queue
import re
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import threading
import time

events = os.fdopen(3, "w", buffering=1)
emit_lock = threading.Lock()


def emit(**kw):
    with emit_lock:
        events.write(json.dumps(kw) + "\n")
        events.flush()


BIN = sys.argv[1] if len(sys.argv) > 1 else os.path.dirname(os.path.abspath(__file__))
CLI = os.path.join(BIN, "levo-cli")
RENDER = os.path.join(BIN, "levo-render")

# model id -> LeLM file stem; the first quantization present in the directory wins.
MODELS = {"levo2-large": "LeVo2-v2-large", "levo2-medium": "LeVo2-v2-medium"}
LM_QUANTS = ["Q6_K", "Q8_0", "Q5_K_M", "Q4_K_M", "F16"]
FLOW_QUANTS = ["Q6_K", "Q8_0", "F32", "Q5_K_M", "Q4_K_M"]
VAE_QUANTS = ["F16", "F32"]
MAX_SECONDS = 270.0  # levo-cli's limit

# Structure tags the model knows (13 added tokens). verse/chorus/bridge carry lyrics; the others are instrumental.
VOCAL = {"verse", "chorus", "bridge"}
NON_VOCAL = {"intro-short", "intro-medium", "intro-long", "outro-short", "outro-medium", "outro-long",
             "inst-short", "inst-medium", "inst-long", "silence"}
ALIASES = {
    "intro": "intro-short", "outro": "outro-short", "inst": "inst-short", "instrumental": "inst-short",
    "solo": "inst-short", "break": "inst-short", "interlude": "inst-short", "end": "outro-short",
    "hook": "chorus", "refrain": "chorus", "post-chorus": "chorus", "pre-chorus": "verse", "prechorus": "verse",
    "rap": "verse", "spoken": "verse",
}

current = {"proc": None}
closing = threading.Event()


def pick(directory, stem, quants):
    for q in quants:
        path = os.path.join(directory, f"{stem}-{q}.gguf")
        if os.path.isfile(path):
            return path
    raise FileNotFoundError(f"no {stem}-{{{','.join(quants)}}}.gguf in {directory}")


def normalize_tag(raw):
    tag = raw.strip().lower().replace(" ", "-").replace("_", "-")
    tag = re.sub(r"-?\d+$", "", tag)  # [verse 2] / [chorus-1]
    if tag in VOCAL or tag in NON_VOCAL:
        return tag
    if tag in ALIASES:
        return ALIASES[tag]
    for prefix, mapped in (("intro", "intro-short"), ("outro", "outro-short"), ("inst", "inst-short"),
                           ("chorus", "chorus"), ("verse", "verse"), ("bridge", "bridge")):
        if tag.startswith(prefix):
            return mapped
    return "verse"


def clean_line(line):
    # The upstream SongGeneration front end strips punctuation; lines of a section are joined with "."
    line = re.sub(r"[^\w\s']", " ", line, flags=re.UNICODE)
    return re.sub(r"\s+", " ", line).strip()


def format_lyrics(text, instrumental):
    """'[verse]\\nline\\nline\\n\\n[chorus]\\n…' -> '[verse] line.line ; [chorus] … ' (the format LeVo was trained on)."""
    sections = []  # [tag, [lines]]
    for raw in (text or "").splitlines():
        line = raw.strip()
        if not line:
            continue
        m = re.match(r"^\[([^\]]+)\]\s*(.*)$", line)
        if m:
            sections.append([normalize_tag(m.group(1)), []])
            line = m.group(2)
        if line:
            if not sections:
                sections.append(["verse", []])
            cleaned = clean_line(line)
            if cleaned:
                sections[-1][1].append(cleaned)
    if instrumental:
        tags = [t for t, _ in sections if t in NON_VOCAL]
        return " ; ".join(f"[{t}]" for t in tags) if tags else "[intro-short] ; [inst-medium] ; [outro-short]"
    parts = []
    for tag, lines in sections:
        if tag in VOCAL:
            if lines:
                parts.append(f"[{tag}] " + ".".join(lines))
        else:
            parts.append(f"[{tag}]")
    if not any(p.split("]")[0][1:] in VOCAL for p in parts):
        raise ValueError("no lyrics (set instrumental for a song without vocals)")
    return " ; ".join(parts)


def wav_seconds(path):
    with open(path, "rb") as f:
        head = f.read(12)
        if head[:4] != b"RIFF" or head[8:12] != b"WAVE":
            raise ValueError(f"{path} is not a WAV file")
        rate = block = None
        while True:
            chunk = f.read(8)
            if len(chunk) < 8:
                break
            cid, size = struct.unpack("<4sI", chunk)
            if cid == b"fmt ":
                fmt = f.read(size)
                rate, = struct.unpack_from("<I", fmt, 4)
                block, = struct.unpack_from("<H", fmt, 12)
                size = 0
            elif cid == b"data" and rate:
                return size / block / rate
            f.seek(size + (size & 1), 1)
    raise ValueError(f"{path} has no audio data")


def run(cmd, on_json):
    """Runs one CLI, turning its JSON progress (stderr) into events and other output into log lines."""
    tail = []
    proc = subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, errors="replace", bufsize=1)
    current["proc"] = proc
    if closing.is_set():
        stop(proc)

    def pump_stdout():
        for line in proc.stdout:
            line = line.rstrip()
            if line:
                emit(type="log", text=line)

    t = threading.Thread(target=pump_stdout, daemon=True)
    t.start()
    for line in proc.stderr:
        line = line.rstrip()
        if not line:
            continue
        if line.startswith("{"):
            try:
                on_json(json.loads(line))
                continue
            except (ValueError, KeyError, TypeError):
                pass
        tail = (tail + [line])[-8:]
        # ggml's Metal init/pipeline chatter is noise; keep errors and everything else.
        if line.startswith(("ggml_metal_", "ggml_backend_metal")) and "error" not in line.lower():
            continue
        emit(type="log", text=line)
    proc.wait()
    t.join(timeout=5)
    current["proc"] = None
    if closing.is_set():
        raise RuntimeError("cancelled")
    if proc.returncode != 0:
        name = os.path.basename(cmd[0])
        raise RuntimeError(f"{name} exited with {proc.returncode}: " + " | ".join(tail[-4:]))


GEN_STATUS = {
    "initializing_backend": "starting the GPU backend …",
    "loading_model": "loading the language model …",
    "preparing_conditioning": "reading lyrics and style …",
    "prefilling": "reading lyrics and style …",
    "generating": "generating tokens …",
}
RENDER_STATUS = {
    "initializing_backend": "rendering: starting the GPU backend …",
    "loading_flow": "rendering: loading the flow model …",
    "generating_latents": "rendering audio (flow) …",
    "loading_vae": "rendering: loading the VAE …",
    "decoding_window": "decoding audio (VAE) …",
    "assembling_audio": "writing the song …",
}


def generate(req):
    model = req.get("model") or "levo2-large"
    if model not in MODELS:
        raise ValueError(f"unknown model {model!r} (known: {', '.join(MODELS)})")
    directory = (req.get("dirs") or {}).get("levo2") or req.get("path")
    if not directory:
        raise ValueError("dirs.levo2 (the directory with the GGUF files) is missing")
    lm = pick(directory, MODELS[model], LM_QUANTS)
    flow = pick(directory, "LeVo2-v2-flow", FLOW_QUANTS)
    vae = pick(directory, "LeVo2-v2-vae", VAE_QUANTS)
    for exe in (CLI, RENDER):
        if not os.access(exe, os.X_OK):
            raise FileNotFoundError(f"{exe} is missing (build LeVo2.cpp with levo2_build.sh)")

    duration = float(req.get("duration") or 30)
    duration = min(max(duration, 1.0), MAX_SECONDS)
    steps = int(req.get("steps") or 0) or 50
    cfg = float(req.get("cfg") or req.get("guidance") or 0) or 1.5
    seed = int(req.get("seed") or 0)
    backend = req.get("backend") or os.environ.get("LEVO2_BACKEND") or "gpu"
    output = os.path.abspath(req["output"])
    lyrics = format_lyrics(req.get("lyrics") or "", bool(req.get("instrumental")))
    prompt = (req.get("prompt") or "").strip()

    t0 = time.time()
    work = tempfile.mkdtemp(prefix="levo2-")
    try:
        lyrics_file = os.path.join(work, "lyrics.txt")
        with open(lyrics_file, "w", encoding="utf-8") as f:
            f.write(lyrics)
        emit(type="log", text=f"{os.path.basename(lm)} + {os.path.basename(flow)} + {os.path.basename(vae)}, "
                               f"backend {backend}, lyrics: {lyrics}")

        tokens = os.path.join(work, "tokens.npy")
        state = {"stage": None}

        def on_gen(ev):
            stage = ev["stage"]
            if stage != state["stage"]:
                state["stage"] = stage
                if stage in GEN_STATUS:
                    emit(type="status", text=GEN_STATUS[stage])
            if stage == "generating" and ev.get("total_steps"):
                emit(type="progress", step=int(ev["completed_steps"]), total=int(ev["total_steps"]))

        emit(type="status", text=f"generating tokens ({model}) …")
        cmd = [CLI, "--model", lm, "--lyrics", lyrics_file, "--duration", f"{duration:g}", "--output", tokens,
               "--backend", backend, "--seed", str(seed), "--progress", "json", "--progress-interval", "1"]
        if prompt:
            cmd += ["--prompt", prompt]
        run(cmd, on_gen)
        t_tokens = time.time() - t0

        rendered = os.path.join(work, "song.wav")
        state["stage"] = None

        def on_render(ev):
            stage = ev["stage"]
            windows = int(ev.get("total_windows") or 0)
            if stage != state["stage"]:
                state["stage"] = stage
                if stage in RENDER_STATUS:
                    emit(type="status", text=RENDER_STATUS[stage])
            if stage == "generating_latents" and ev.get("total_steps") and windows:
                total = windows * int(ev["total_steps"])
                done = (max(int(ev.get("current_window") or 1), 1) - 1) * int(ev["total_steps"]) + int(ev["completed_steps"])
                emit(type="progress", step=min(done, total), total=total)
            elif stage == "decoding_window" and windows:
                emit(type="progress", step=int(ev.get("completed_windows") or 0), total=windows)

        emit(type="status", text="rendering audio …")
        run([RENDER, tokens, "--flow-model", flow, "--vae-model", vae, "--output", rendered, "--backend", backend,
             "--steps", str(steps), "--cfg", f"{cfg:g}", "--seed", str(seed),
             "--progress", "json", "--progress-interval", "1"], on_render)

        os.makedirs(os.path.dirname(output), exist_ok=True)
        shutil.move(rendered, output)
        seconds = time.time() - t0
        emit(type="log", text=f"tokens {t_tokens:.1f} s, render {seconds - t_tokens:.1f} s")
        emit(type="result", file=output, seconds=seconds, duration=wav_seconds(output),
             tokenSeconds=t_tokens, renderSeconds=seconds - t_tokens)
    finally:
        shutil.rmtree(work, ignore_errors=True)


def stop(proc):
    if proc and proc.poll() is None:
        try:
            proc.send_signal(signal.SIGTERM)  # both CLIs stop cooperatively (exit 130)
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        except OSError:
            pass


requests = queue.Queue()


def read_stdin():
    for line in sys.stdin:
        requests.put(line)
    closing.set()
    requests.put(None)
    stop(current["proc"])


threading.Thread(target=read_stdin, daemon=True).start()
emit(type="ready")
while True:
    line = requests.get()
    if line is None:
        break
    line = line.strip()
    if not line:
        continue
    try:
        generate(json.loads(line))
    except Exception as e:  # noqa: BLE001
        if closing.is_set():
            break
        emit(type="error", text=f"{e}" if isinstance(e, (RuntimeError, ValueError, FileNotFoundError))
             else f"{type(e).__name__}: {e}")
