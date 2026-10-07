# mlxlab's style trainer for ACE-Step 1.5: a LoRA of the DiT decoder from the user's own tracks.
#
# Runs in the acestep runtime (the pinned ACE-Step package brings PEFT and its training code) and imports
# acestep_worker.py (written next to it as mlxlab_acestep_worker.py) for the model loading, the style merge into the
# MLX DiT and the sample songs. Training itself runs on PyTorch/MPS (ACE-Step has no MLX trainer); measured on an
# M1 Max: ~2.5 s a step for a 30 s segment with the 2B turbo, ~7 GB.
#
# Protocol: the first line on stdin is the request; a later {"stop": true} line asks it to stop at the next step
# (a checkpoint is written, then the result). Events as JSON lines on fd 3, like every worker:
#   {"type": "status"|"log"|"progress"|"error"|"result", …}
#   {"type": "train", "step", "total", "loss", "lr", "eta", "sps"}      after every optimiser step
#   {"type": "checkpoint", "step", "dir"}                               a resumable checkpoint was written
#   {"type": "sample", "step", "file"}                                  a short song with the LoRA so far
#   {"type": "captions", "captions": {segment file name: caption}}      the auto-labels
# request: {"model": "ace-step-1.5-turbo"|…, "dirs": {…as acestep_worker…}, "style_dir": "/abs", "segments":
#           [{"file": "/abs/seg.wav", "caption": "" (auto), "lyrics": "" (instrumental), "seconds": 30}],
#           "trigger": "mystyle", "steps": 1000, "rank": 64, "alpha": 128, "lr": 1e-4, "warmup": 50,
#           "checkpoint_every": 50, "sample_every": 250, "sample_seconds": 12, "sample_prompt": "", "seed": 42,
#           "captions": true (auto-label segments that have none)}
# result: {"type": "result", "file": "<style_dir>/lora", "step", "stopped": bool, "seconds"}
#
# Layout under style_dir: work/captions.json, work/dataset.json, work/tensors/*.pt (VAE latents and text
# embeddings, kept for the next run), work/checkpoint/ (PEFT adapter + state.pt: optimiser, step, RNG), lora/ (the
# final adapter, PEFT format: adapter_config.json + adapter_model.safetensors), samples/step-NNNNNN.wav.
import json
import math
import os
import random
import shutil
import sys
import threading
import time
import traceback

import mlxlab_acestep_worker as W  # noqa: E402  (loads mlx, torch, ACE-Step; opens fd 3)

import torch  # noqa: E402

emit = W.emit
VARIANT = {"ace-step-1.5-turbo": "turbo", "ace-step-1.5-xl-turbo": "xl_turbo", "ace-step-1.5-base": "base",
           "ace-step-1.5-xl-base": "xl_base"}
stop = threading.Event()


def read_stdin(first):
    first.append(sys.stdin.readline())
    ready.set()
    for line in sys.stdin:
        try:
            if json.loads(line).get("stop"):
                stop.set()
        except ValueError:
            pass
    stop.set()  # stdin closed: mlxlab went away


ready = threading.Event()


def write_json(path, v):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(v, f, indent=1)
    os.replace(tmp, path)


def checkpoint_root(req):
    return W.loaded["root"] and os.path.join(W.loaded["root"], "checkpoints")


# ---------------------------------------------------------------- 1. captions (the 5 Hz LM listens to each segment)
def caption_all(req, work):
    path = os.path.join(work, "captions.json")
    caps = json.load(open(path)) if os.path.isfile(path) else {}
    todo = [s for s in req["segments"] if not (s.get("caption") or "").strip() and os.path.basename(s["file"]) not in caps]
    if todo and req.get("captions", True):
        dit, lm = W.models_for({"model": req["model"], "dirs": req["dirs"], "lm": True})
        for i, s in enumerate(todo):
            if stop.is_set():
                break
            W.status(f"listening to segment {i + 1} of {len(todo)} (captions) …")
            emit(type="progress", step=i, total=len(todo))
            codes = dit.convert_src_audio_to_codes(s["file"])
            if not codes or codes.startswith("❌"):
                emit(type="log", text=f"no codes for {os.path.basename(s['file'])}: {(codes or '')[:200]}")
                caps[os.path.basename(s["file"])] = {}
                continue
            meta, msg = lm.understand_audio_from_codes(audio_codes=codes, temperature=0.7, use_constrained_decoding=True)
            meta = meta or {}
            caps[os.path.basename(s["file"])] = {k: meta.get(k) for k in ("caption", "genres", "bpm", "keyscale",
                                                                         "timesignature", "lyrics", "vocal_language")}
            emit(type="log", text=f"{os.path.basename(s['file'])}: {meta.get('caption', '')[:160]}")
            write_json(path, caps)
        emit(type="captions", captions={k: (v or {}).get("caption", "") for k, v in caps.items()})
        # the LM is not needed again (the samples skip it)
        W.loaded.update(lm=None, lm_key=None)
        W.trim()
    return caps


def dataset_json(req, work, caps):
    samples = []
    for s in req["segments"]:
        c = caps.get(os.path.basename(s["file"])) or {}
        caption = (s.get("caption") or "").strip() or (c.get("caption") or "").strip() or req.get("sample_prompt") or "music"
        lyrics = (s.get("lyrics") or "").strip()
        if not lyrics and s.get("vocals") and (c.get("lyrics") or "").strip():
            lyrics = c["lyrics"].strip()
        bpm = c.get("bpm")
        try:
            bpm = int(bpm) if bpm not in (None, "", "N/A") else None
        except (TypeError, ValueError):
            bpm = None
        samples.append({"audio_path": s["file"], "filename": os.path.basename(s["file"]), "caption": caption,
                        "genre": c.get("genres") or "", "lyrics": lyrics or "[Instrumental]",
                        "is_instrumental": not lyrics, "bpm": bpm, "keyscale": c.get("keyscale") or "",
                        "timesignature": c.get("timesignature") or "", "duration": int(round(s.get("seconds") or 30))})
    data = {"metadata": {"custom_tag": req.get("trigger") or "", "tag_position": "prepend"}, "samples": samples}
    path = os.path.join(work, "dataset.json")
    write_json(path, data)
    return path, data


# ---------------------------------------------------------------- 2. tensors (VAE latents, text embeddings)
def preprocess(req, work, ds_path, data):
    from acestep.training_v2.preprocess import preprocess_audio_files

    out = os.path.join(work, "tensors")
    os.makedirs(out, exist_ok=True)
    # a segment whose caption changed is encoded again
    sig_path = os.path.join(out, "captions.json")
    sigs = json.load(open(sig_path)) if os.path.isfile(sig_path) else {}
    want = {}
    for s in data["samples"]:
        stem = os.path.splitext(s["filename"])[0]
        want[stem] = json.dumps([data["metadata"]["custom_tag"], s["caption"], s["lyrics"], s["bpm"], s["keyscale"]])
        if sigs.get(stem) != want[stem]:
            for f in (stem + ".pt", stem + ".tmp.pt"):
                if os.path.exists(os.path.join(out, f)):
                    os.remove(os.path.join(out, f))
    keep = {s + ".pt" for s in want}
    for f in os.listdir(out):  # segments of tracks that were removed
        if f.endswith(".pt") and f not in keep:
            os.remove(os.path.join(out, f))
    todo = [s for s in want if not os.path.exists(os.path.join(out, s + ".pt"))]
    if todo:
        W.status(f"encoding {len(todo)} segments (VAE, text encoder) …")
        res = preprocess_audio_files(None, out, checkpoint_root(req), variant=VARIANT[req["model"]],
                                     max_duration=float(max(s.get("seconds") or 30 for s in req["segments"])) + 1,
                                     dataset_json=ds_path, device="mps", precision="bf16",
                                     progress_callback=lambda i, n, msg: emit(type="progress", step=i, total=n),
                                     cancel_check=stop.is_set)
        emit(type="log", text=f"encoded: {res}")
    write_json(sig_path, want)
    files = sorted(os.path.join(out, s + ".pt") for s in want if os.path.exists(os.path.join(out, s + ".pt")))
    if not files and not stop.is_set():
        raise RuntimeError("no segment could be encoded (see the log)")
    return files


# ---------------------------------------------------------------- 3. training
def lr_at(step, total, warmup, lr):
    if step < warmup:
        return lr * (step + 1) / max(1, warmup)
    p = (step - warmup) / max(1, total - warmup)
    return lr * (0.1 + 0.9 * 0.5 * (1 + math.cos(math.pi * min(1.0, p))))


def save_adapter(mod, path):
    """The PEFT adapter into path, atomically (a folder written aside, then renamed)."""
    tmp = path + ".tmp"
    shutil.rmtree(tmp, ignore_errors=True)
    mod.model.decoder.save_pretrained(tmp)
    old = path + ".old"
    shutil.rmtree(old, ignore_errors=True)
    if os.path.exists(path):
        os.rename(path, old)
    os.rename(tmp, path)
    shutil.rmtree(old, ignore_errors=True)


def train(req, work, files):
    from peft import set_peft_model_state_dict
    from safetensors.torch import load_file
    from acestep.training.data_module import collate_preprocessed_batch
    from acestep.training_v2.configs import LoRAConfigV2, TrainingConfigV2
    from acestep.training_v2.fixed_lora_module import FixedLoRAModule
    from acestep.training_v2.model_loader import load_decoder_for_training, read_model_config
    from acestep.training_v2.trainer_helpers import configure_memory_features

    total = int(req.get("steps") or 1000)
    rank, alpha = int(req.get("rank") or 64), int(req.get("alpha") or 128)
    lr, warmup = float(req.get("lr") or 1e-4), int(req.get("warmup") or 50)
    every_ckpt, every_sample = int(req.get("checkpoint_every") or 50), int(req.get("sample_every") or 0)
    seed = int(req.get("seed") or 42)
    ck = checkpoint_root(req)
    variant = VARIANT[req["model"]]
    sig = {"model": req["model"], "rank": rank, "alpha": alpha}
    ckpt = os.path.join(work, "checkpoint")
    state = None
    if os.path.isfile(os.path.join(ckpt, "state.pt")):
        state = torch.load(os.path.join(ckpt, "state.pt"), weights_only=False)
        if state.get("sig") != sig:
            emit(type="log", text=f"the checkpoint was made with other settings ({state.get('sig')}): starting over")
            state = None

    W.status(f"loading the {variant} DiT for training …")
    t0 = time.time()
    model = load_decoder_for_training(ck, variant, "mps", "bf16")
    mc = read_model_config(ck, variant)
    cfg = TrainingConfigV2(dataset_dir=os.path.join(work, "tensors"), device="mps", precision="bf16",
                           model_variant=variant, checkpoint_dir=ck, output_dir=work, num_workers=0,
                           timestep_mu=mc.get("timestep_mu", -0.4), timestep_sigma=mc.get("timestep_sigma", 1.0),
                           data_proportion=mc.get("data_proportion", 0.5), cfg_ratio=0.15)
    mod = FixedLoRAModule(model, LoRAConfigV2(r=rank, alpha=alpha, dropout=0.1), cfg, torch.device("mps"), torch.bfloat16)
    ok = configure_memory_features(mod.model.decoder)
    mod.force_input_grads_for_checkpointing = ok[0]
    params = [p for p in mod.model.parameters() if p.requires_grad]
    for p in params:  # the LoRA weights (and AdamW's state) in float32; the frozen model stays bf16
        p.data = p.data.float()
    opt = torch.optim.AdamW(params, lr=lr, weight_decay=0.01)
    step = 0
    rng = random.Random(seed)
    torch.manual_seed(seed)
    if state is not None:
        set_peft_model_state_dict(mod.model.decoder, load_file(os.path.join(ckpt, "adapter_model.safetensors")))
        for p in params:
            p.data = p.data.float()
        opt.load_state_dict(state["optimizer"])
        step = int(state["step"])
        rng.setstate(state["rng"])
        torch.set_rng_state(state["torch_rng"])
        emit(type="log", text=f"resumed at step {step}")
    emit(type="log", text=f"training {sum(p.numel() for p in params):,} parameters (rank {rank}, alpha {alpha}) on "
                          f"{len(files)} segments; model loaded in {time.time() - t0:.0f} s")
    if step >= total:
        return mod, step, False

    data = [torch.load(f, map_location="cpu", weights_only=True) for f in files]
    order = []

    def save_checkpoint():
        save_adapter(mod, ckpt)
        torch.save({"sig": sig, "step": step, "optimizer": opt.state_dict(), "rng": rng.getstate(),
                    "torch_rng": torch.get_rng_state()}, os.path.join(ckpt, "state.pt.tmp"))
        os.replace(os.path.join(ckpt, "state.pt.tmp"), os.path.join(ckpt, "state.pt"))
        emit(type="checkpoint", step=step, dir=ckpt)

    mod.model.decoder.train()
    times = []
    W.status(f"training: step {step} of {total} …")
    while step < total:
        if stop.is_set():
            save_checkpoint()
            return mod, step, True
        if not order:
            order = list(range(len(data)))
            rng.shuffle(order)
        batch = collate_preprocessed_batch([data[order.pop()]])
        ts = time.time()
        for g in opt.param_groups:
            g["lr"] = lr_at(step, total, warmup, lr)
        loss = mod.training_step(batch)
        loss.backward()
        torch.nn.utils.clip_grad_norm_(params, 1.0)
        opt.step()
        opt.zero_grad(set_to_none=True)
        step += 1
        value = float(loss.detach())
        del loss
        times.append(time.time() - ts)
        times = times[-20:]
        sps = sum(times) / len(times)
        emit(type="train", step=step, total=total, loss=value, lr=opt.param_groups[0]["lr"], sps=sps,
             eta=sps * (total - step))
        emit(type="progress", step=step, total=total)
        W.status(f"training: step {step} of {total}, loss {value:.3f}")
        if step % every_ckpt == 0 or step == total:
            save_checkpoint()
        if every_sample and step % every_sample == 0 and step < total:
            if step % every_ckpt:
                save_checkpoint()
            sample(req, step, ckpt)
            mod.model.decoder.train()
    return mod, step, False


# ---------------------------------------------------------------- samples: a short song with the LoRA so far
def sample(req, step, lora_dir):
    out = os.path.join(req["style_dir"], "samples", f"step-{step:06d}.wav")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    W.status(f"step {step}: rendering a sample …")
    prompt = (req.get("sample_prompt") or "").strip() or "instrumental"
    trig = (req.get("trigger") or "").strip()
    if trig and trig.lower() not in prompt.lower():
        prompt = f"{trig}, {prompt}"
    t0 = time.time()
    r = {"model": req["model"], "dirs": {**req["dirs"], "lora": lora_dir}, "prompt": prompt, "instrumental": True,
         "lm": False, "duration": float(req.get("sample_seconds") or 12), "seed": int(req.get("seed") or 42),
         "output": out, "lora": {"file": "adapter_model.safetensors", "scale": 1.0}}
    try:
        dit, _ = W.models_for(r)
        W.generate(r, dit, None)
    except Exception as e:  # noqa: BLE001 (a failed sample does not stop the training)
        traceback.print_exc()
        emit(type="log", text=f"sample at step {step} failed: {type(e).__name__}: {e}")
        return
    finally:
        dit = None
        W.trim()
    emit(type="sample", step=step, file=out)
    emit(type="log", text=f"sample at step {step} in {time.time() - t0:.0f} s")


def main():
    first = []
    threading.Thread(target=read_stdin, args=(first,), daemon=True).start()
    emit(type="ready")
    ready.wait()
    req = json.loads(first[0])
    t0 = time.time()
    if req["model"] not in VARIANT:
        raise ValueError(f"{req['model']} cannot be trained (known: {', '.join(VARIANT)})")
    work = os.path.join(req["style_dir"], "work")
    os.makedirs(work, exist_ok=True)
    from acestep.training.path_safety import set_safe_root
    set_safe_root(req["style_dir"])
    # the checkpoint folder of symlinks the loaders expect (and the DiT handler for captions and samples)
    if not W.loaded["root"]:
        W.loaded["root"] = W.build_root(req)
        os.environ["ACESTEP_PROJECT_ROOT"] = W.loaded["root"]
    caps = caption_all(req, work)
    ds_path, data = dataset_json(req, work, caps)
    if not (req.get("sample_prompt") or "").strip() and data["samples"]:
        req["sample_prompt"] = data["samples"][0]["caption"]  # the samples sound like the first segment
    files = [] if stop.is_set() else preprocess(req, work, ds_path, data)
    mod, step, stopped = (None, 0, True) if stop.is_set() else train(req, work, files)
    final = os.path.join(req["style_dir"], "lora")
    if not stopped and mod is not None:
        save_adapter(mod, final)
        with open(os.path.join(final, "mlxlab.json"), "w") as f:
            json.dump({"model": req["model"], "trigger": req.get("trigger") or "", "steps": step}, f)
        del mod
        W.trim()
        if int(req.get("sample_every") or 0) > 0:
            sample(req, step, final)  # how the finished style sounds
    emit(type="result", file=final if not stopped else "", step=step, stopped=stopped, seconds=time.time() - t0)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
