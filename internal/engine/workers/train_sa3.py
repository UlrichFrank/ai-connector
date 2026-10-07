# mlxlab's style trainer for Stable Audio 3: a LoRA from the user's own tracks, trained by the official pure-MLX
# trainer of the runtime (stable-audio-3/optimized/mlx, scripts/lora_train_mlx.py: underfit's conventions — DoRA-rows
# adapters on the BASE rectified-flow checkpoint, random crops, tag prompts with a trigger word, CFG dropout).
# Measured on an M1 Max: Stable Audio 3 Medium, 30 s crops, rank 16, gradient checkpointing: ~1.1 s a step, ~12 GB.
#
#   python train_sa3.py <dir of stable-audio-3/optimized/mlx at the pinned commit>
#
# Protocol (as train_acestep.py): the first line on stdin is the request, a later {"stop": true} stops it (the
# trainer gets SIGINT: it writes a checkpoint and a last sample, then ends). Events on fd 3:
#   {"type": "status"|"log"|"progress"|"error"|"result", …}, {"type": "train", "step", "total", "loss", "lr", "eta",
#   "sps"}, {"type": "checkpoint", "step", "dir"}, {"type": "sample", "step", "file"}
# request: {"model": "medium" | "sm-music", "dirs": {"weights": "/dir of hf stable-audio-3-optimized"} (with MLX/
#           dit_<model>-base_f16.npz as well), "style_dir": "/abs", "tracks": [{"file": "/abs.wav", "name", "bpm"}],
#           "description": "tags for every track", "trigger": "word", "steps": 1000, "rank": 16, "alpha": 16,
#           "lr": 1e-4, "crop_seconds": 30, "checkpoint_every": 50, "sample_every": 250, "sample_seconds": 12,
#           "sample_prompt": "", "seed": 42}
# result: {"type": "result", "file": "<style_dir>/lora" (holding style.safetensors), "step", "stopped", "seconds"}
#
# Layout under style_dir: work/audio/<track>.wav (hard links of the tracks) + .json (the tag prompt: the description
# and the tempo, "…, 138 BPM"), work/latents/ (SAME latents, encoded again each run: seconds), work/runs/<name>/<session>/checkpoints/
# *.safetensors (resumable: the newest is the next run's --lora-ckpt-path), work/demos/, samples/step-N.mp3,
# lora/style.safetensors.
import glob
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import threading
import time
import traceback

events = os.fdopen(3, "w", buffering=1)
_lock = threading.Lock()


def emit(**kw):
    with _lock:
        events.write(json.dumps(kw) + "\n")
        events.flush()


CODE = os.path.abspath(sys.argv[1])
MODELS = {  # model -> (codec, DiT weights, base DiT weights)
    "medium": ("same-l", "dit_medium_f16.npz", "dit_medium-base_f16.npz"),
    "sm-music": ("same-s", "dit_sm-music_f16.npz", "dit_sm-music-base_f16.npz"),
}
SR, PER_LATENT = 44100, 4096
stop = threading.Event()
first = []
ready = threading.Event()


def read_stdin():
    first.append(sys.stdin.readline())
    ready.set()
    for line in sys.stdin:
        try:
            if json.loads(line).get("stop"):
                stop.set()
        except ValueError:
            pass
    stop.set()


def link_weights(req):
    """The runtime finds its weights under <CODE>/models/mlx (weights.ensure_local): link the pinned ones there."""
    codec, dit, base = MODELS[req["model"]]
    c = codec.replace("-", "_")
    src = os.path.join(req["dirs"]["weights"], "MLX")
    dst = os.path.join(CODE, "models", "mlx")
    os.makedirs(dst, exist_ok=True)
    for f in ("t5gemma_f16.npz", dit, base, f"{c}_decoder_f32.npz", f"{c}_encoder_f32.npz"):
        if not os.path.isfile(os.path.join(src, f)):
            raise FileNotFoundError(f"missing weights {os.path.join(src, f)}")
        p = os.path.join(dst, f)
        if os.path.islink(p) or os.path.exists(p):
            os.remove(p)
        os.symlink(os.path.join(src, f), p)
    return os.path.join(dst, base)


def run(cmd, on_line, cwd):
    """Runs a runtime script, passing every line (tqdm's \\r updates too) to on_line; SIGINT on stop."""
    p = subprocess.Popen(cmd, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                         env=dict(os.environ, PYTHONUNBUFFERED="1"))
    stopped = [False]

    def watch():
        stop.wait()
        if p.poll() is None:
            stopped[0] = True
            p.send_signal(signal.SIGINT)

    threading.Thread(target=watch, daemon=True).start()
    buf = b""
    while True:
        ch = p.stdout.read1(4096) if hasattr(p.stdout, "read1") else p.stdout.read(1)
        if not ch:
            break
        buf += ch
        parts = re.split(rb"[\r\n]", buf)
        buf = parts.pop()
        for part in parts:
            line = part.decode("utf-8", "replace").strip()
            if line:
                on_line(line)
    if buf.strip():
        on_line(buf.decode("utf-8", "replace").strip())
    return p.wait(), stopped[0]


def prepare(req, work):
    """Hard links of the tracks with their tags; then SAME latents of all of them."""
    audio = os.path.join(work, "audio")
    shutil.rmtree(audio, ignore_errors=True)
    os.makedirs(audio)
    desc = (req.get("description") or "").strip()
    for t in req["tracks"]:
        stem = os.path.splitext(os.path.basename(t["file"]))[0]
        dst = os.path.join(audio, stem + ".wav")
        try:
            os.link(t["file"], dst)
        except OSError:
            shutil.copyfile(t["file"], dst)
        # one tag, written the way songs ask for it ("…, 138 BPM", as the song backend's withBPM)
        prompt = desc.rstrip(",. ")
        if t.get("bpm"):
            prompt = (prompt + ", " if prompt else "") + f"{int(round(float(t['bpm'])))} BPM"
        tags = {"prompt": prompt or "instrumental"}
        with open(os.path.join(audio, stem + ".json"), "w") as f:
            json.dump(tags, f)
    latents = os.path.join(work, "latents")
    shutil.rmtree(latents, ignore_errors=True)
    emit(type="status", text=f"encoding {len(req['tracks'])} tracks (SAME codec) …")
    t0 = time.time()
    code, _ = run([sys.executable, os.path.join(CODE, "scripts", "pre_encode_mlx.py"), "--audio-dir", audio,
                   "--output-dir", latents, "--codec", MODELS[req["model"]][0]],
                  lambda l: emit(type="log", text=l), CODE)
    n = len(glob.glob(os.path.join(latents, "**", "*.npy"), recursive=True))
    if code != 0 or n == 0:
        raise RuntimeError(f"encoding the tracks failed (exit {code}, {n} encoded; see the log)")
    emit(type="log", text=f"encoded {n} tracks in {time.time() - t0:.0f} s")
    return latents


def latest_checkpoint(runs):
    best, step = None, -1
    for f in glob.glob(os.path.join(runs, "**", "checkpoints", "*.safetensors"), recursive=True):
        m = re.search(r"step=(\d+)", os.path.basename(f))
        if m and int(m.group(1)) > step:
            best, step = f, int(m.group(1))
    return best, step


STEP = re.compile(r"^Step (\d+), Epoch \d+.*train/loss=([0-9.eE+-]+).*train/lr=([0-9.eE+-]+)")
CKPT = re.compile(r"checkpoint (\S+-step=(\d+)-epoch=\d+\.safetensors)")
DEMO = re.compile(r"(demo_0_(\d{8})\.mp3)")


def train(req, work, latents, base):
    total = int(req.get("steps") or 1000)
    rank = int(req.get("rank") or 16)
    alpha = float(req.get("alpha") or rank)
    every_sample = int(req.get("sample_every") or 0)
    crop = max(64, int(round(float(req.get("crop_seconds") or 30) * SR / PER_LATENT)))
    runs = os.path.join(work, "runs")
    ckpt, done = latest_checkpoint(runs)
    done = max(done, 0)
    trigger = (req.get("trigger") or "").strip()
    prompt_cfg = os.path.join(work, "prompt.json")
    with open(prompt_cfg, "w") as f:  # underfit's tag prompts: the description and tempo, the trigger in 80 %
        json.dump({"use_tags": True, "tag_keys": ["prompt"], "hide_tag_names": True, "trigger": trigger,
                   "trigger_pct": 80}, f)
    cmd = [sys.executable, os.path.join(CODE, "scripts", "lora_train_mlx.py"), "--dit", req["model"],
           "--dit-weights", base, "--latents-dir", latents, "--lr", str(float(req.get("lr") or 1e-4)),
           "--name", "style", "--save-dir", runs, "--max-steps", str(total),
           "--checkpoint-every", str(int(req.get("checkpoint_every") or 50)), "--rank", str(rank), "--alpha", str(alpha),
           "--latent-crop-length", str(crop), "--prompt-config", prompt_cfg, "--seed", str(int(req.get("seed") or 42)),
           "--grad-checkpoint", "--gradient-clip-val", "1.0"]
    demos = os.path.join(work, "demos")
    if every_sample > 0:
        prompt = (req.get("sample_prompt") or "").strip() or (req.get("description") or "").strip() or "instrumental"
        if trigger and trigger.lower() not in prompt.lower():
            prompt = f"{trigger}, {prompt}"
        cfg = os.path.join(work, "demos.json")
        with open(cfg, "w") as f:  # the shipped (ARC) model with the LoRA merged: what a song with the style sounds like
            json.dump([{"prompt": prompt, "cfg": 1, "seed": int(req.get("seed") or 42), "steps": 8, "arc": True,
                        "duration": float(req.get("sample_seconds") or 12)}], f)
        cmd += ["--demo-every", str(every_sample), "--demo-config", cfg, "--demo-dir", demos]
    if ckpt:
        cmd += ["--lora-ckpt-path", ckpt]
        emit(type="log", text=f"resuming from step {done} ({os.path.basename(ckpt)})")
    if done >= total:
        return ckpt, done, False

    state = {"step": done, "times": [], "last": time.time()}
    samples = os.path.join(req["style_dir"], "samples")
    os.makedirs(samples, exist_ok=True)

    def on_line(line):
        m = STEP.match(line)
        if m:
            step = int(m.group(1))
            if step > state["step"]:
                now = time.time()
                if state["step"] > done:  # (the first step includes compiling)
                    state["times"] = (state["times"] + [(now - state["last"]) / (step - state["step"])])[-20:]
                state["step"], state["last"] = step, now
                sps = sum(state["times"]) / len(state["times"]) if state["times"] else 0
                emit(type="train", step=step, total=total, loss=float(m.group(2)), lr=float(m.group(3)), sps=sps,
                     eta=sps * (total - step))
                emit(type="progress", step=step, total=total)
                emit(type="status", text=f"training: step {step} of {total}, loss {float(m.group(2)):.3f}")
            return
        m = CKPT.search(line)
        if m:
            emit(type="checkpoint", step=int(m.group(2)), dir=m.group(1))
            return
        m = DEMO.search(line)
        if m:
            src, step = os.path.join(demos, m.group(1)), int(m.group(2))
            # (the writer thread prints this line once the mp3 is on disk)
            dst = os.path.join(samples, f"step-{step:06d}.mp3")
            if os.path.isfile(src):
                shutil.copyfile(src, dst)
                emit(type="sample", step=step, file=dst)
            emit(type="log", text=line)
            return
        if "loading ARC model" in line:
            emit(type="status", text=f"step {state['step']}: rendering a sample …")
        if not line.startswith("Step ") and "demo 0:" not in line:
            emit(type="log", text=line)

    emit(type="status", text=f"loading Stable Audio 3 {req['model']} (base) for training …")
    code, stopped = run(cmd, on_line, work)
    ckpt, step = latest_checkpoint(runs)
    if code != 0 and not stopped:
        raise RuntimeError(f"the trainer stopped with exit code {code} (see the log)")
    return ckpt, step, stopped or step < total


def main():
    threading.Thread(target=read_stdin, daemon=True).start()
    emit(type="ready")
    ready.wait()
    req = json.loads(first[0])
    t0 = time.time()
    if req["model"] not in MODELS:
        raise ValueError(f"{req['model']} cannot be trained (known: {', '.join(MODELS)})")
    work = os.path.join(req["style_dir"], "work")
    os.makedirs(work, exist_ok=True)
    base = link_weights(req)
    latents = prepare(req, work)
    ckpt, step, stopped = (None, 0, True) if stop.is_set() else train(req, work, latents, base)
    final = os.path.join(req["style_dir"], "lora")
    if not stopped and ckpt:
        os.makedirs(final, exist_ok=True)
        shutil.copyfile(ckpt, os.path.join(final, "style.safetensors.tmp"))
        os.replace(os.path.join(final, "style.safetensors.tmp"), os.path.join(final, "style.safetensors"))
        with open(os.path.join(final, "mlxlab.json"), "w") as f:
            json.dump({"model": req["model"], "trigger": req.get("trigger") or "", "steps": step,
                       "checkpoint": os.path.basename(ckpt)}, f)
    emit(type="result", file=final if not stopped else "", step=step, stopped=stopped, seconds=time.time() - t0)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
