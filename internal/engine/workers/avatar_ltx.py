# mlxlab's talking-avatar worker for LTX-2.5 (runs in the pinned ltx-2-mlx runtime): a portrait as the first frame,
# speech as a frozen audio latent, the video generated around it.
#
# The recipe that makes lip-sync with custom audio work on LTX-2.5 (huggingface.co/Lightricks/LTX-2.5/discussions/44):
# the supplied audio is encoded with the audio VAE and kept frozen (denoise mask 0) in both stages, plain Euler
# sampling (not the ancestral sampler the 2.5 distilled stage 1 uses by default), no CFG / modality guidance (the
# distilled pipeline has none), and the original wav is muxed back at the end (mlxlab does that; this worker writes
# the video only). ltx-2-mlx's own `a2v` needs the dev transformer + distilled LoRA (and uses modality guidance 3.0),
# so this subclasses the distilled pipeline instead.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3.
#   request: {"model": "/pack", "audio": "clip.wav", "image": "first.png", "prompt": "…", "width": 512, "height": 512,
#             "frames": 121, "fps": 24, "seed": 1, "lowRam": false, "output": "/path/clip.mp4"}
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


emit(type="status", text="starting ltx-2-mlx …")
import mlx.core as mx  # noqa: E402
from ltx_core_mlx.conditioning.types.latent_cond import LatentState  # noqa: E402
from ltx_core_mlx.model.audio_vae import encode_audio  # noqa: E402
from ltx_core_mlx.utils.audio import load_audio  # noqa: E402
from ltx_core_mlx.utils.positions import compute_audio_token_count  # noqa: E402
from ltx_pipelines_mlx.distilled import DistilledPipeline  # noqa: E402
from ltx_pipelines_mlx.utils.args import ImageConditioningInput  # noqa: E402


class AvatarPipeline(DistilledPipeline):
    """The distilled two-stage pipeline with the audio stream frozen to the given tokens, Euler in both stages."""

    a2v_tokens = None
    stage = 0

    def _run_denoise_loop(self, *, audio_state, ancestral, sigmas, on_step=None, **kw):
        if self.a2v_tokens is not None:
            t = self.a2v_tokens
            audio_state = LatentState(
                latent=t,
                clean_latent=t,
                denoise_mask=mx.zeros((1, t.shape[1], 1), dtype=mx.bfloat16),
                positions=audio_state.positions,
                frozen=True,
            )
        self.stage += 1
        stage, n = self.stage, len(sigmas) - 1
        emit(type="status", text=f"denoising, stage {stage}/2 …")
        emit(type="progress", step=0, total=n)

        def step(i, total, x0, sigma):
            emit(type="progress", step=i + 1, total=total)
            if on_step is not None:
                on_step(i, total, x0, sigma)

        return super()._run_denoise_loop(audio_state=audio_state, ancestral=False, sigmas=sigmas, on_step=step, **kw)


def run(req):
    frames, fps = int(req["frames"]), float(req["fps"])
    pipe = AvatarPipeline(model_dir=req["model"], low_memory=True, low_ram_streaming=bool(req.get("lowRam")))
    pipe.generate_audio = False  # the original wav goes back in (mlxlab muxes it)

    emit(type="status", text="encoding the speech …")
    pipe._load_audio_encoder()
    audio = load_audio(req["audio"], target_sample_rate=16000, max_duration=frames / fps)
    if audio is None:
        raise ValueError(f"no audio in {req['audio']}")
    latent = encode_audio(audio.waveform, audio.sample_rate, pipe.audio_encoder, pipe.audio_processor)
    need = compute_audio_token_count(frames, fps)
    if latent.shape[2] < need:
        raise ValueError(f"the audio is too short for {frames} frames: {latent.shape[2]} of {need} latent frames")
    tokens, _ = pipe.audio_patchifier.patchify(latent[:, :, :need, :])
    mx.eval(tokens)
    pipe.audio_conditioner.free()
    pipe.a2v_tokens = tokens

    images = [ImageConditioningInput(path=req["image"], frame_idx=0, strength=1.0)] if req.get("image") else None
    pipe.generate_and_save(
        prompt=req["prompt"],
        output_path=req["output"],
        height=int(req["height"]),
        width=int(req["width"]),
        num_frames=frames,
        frame_rate=fps,
        seed=int(req["seed"]),
        images=images,
    )


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        mx.reset_peak_memory()
        t0 = time.time()
        run(req)
        mx.clear_cache()
        emit(type="result", file=req["output"], seconds=time.time() - t0, peakGB=mx.get_peak_memory() / 2**30)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
