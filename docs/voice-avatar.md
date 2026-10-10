# Voice, avatar, sound

Three new kinds (`internal/engine/engine.go`): `speech`, `avatar`, `sound`. Models carry `licence` and
`nonCommercial` (the pickers show a "non-commercial" badge).

## Speech (`speech.go`, worker `workers/speech.py`, runtime: mlx-audio pinned in a uv venv)

Models (ids as the catalog shows them; weights from mlx-community where there is a conversion):

| id | model | licence |
|---|---|---|
| `kokoro-82m` | Kokoro 82M (bf16, 339 MB): 49 preset voices in English (US, UK), Spanish, French, Italian, Portuguese, Hindi, Chinese — no German, no Japanese (below); speed; word timings for English | Apache-2.0 |
| `qwen3-tts-1.7b` | Qwen3-TTS 12Hz 1.7B (Base: cloning; CustomVoice: presets + instruct; VoiceDesign) | Apache-2.0 |
| `qwen3-tts-0.6b` | the small one | Apache-2.0 |
| `chatterbox-v3` | Chatterbox Multilingual (emotion exaggeration) | MIT |
| `voxcpm2` | VoxCPM2 (48 kHz) | Apache-2.0 |
| `higgs-audio-v3` | Higgs Audio v3 TTS 4B (inline emotion tags, whisper/shout, effects) | research, **non-commercial** |
| `voxtral-tts` | Voxtral 4B TTS | CC BY-NC 4.0, **non-commercial** |

Request: `text`, `voice` (library id or preset name), `instruct`, `language`, `speed`, `seed`. Output: one wav
(job file); stats: seconds, rtf, sampleRate; result: `text`, `language`, `voice` (`{id, name, language}` of the
preset or library voice; absent: the model's own) and `words` (`[{text, start, end}]`, seconds; Kokoro in English).
Long texts are split at sentences and joined. A model's presets, languages and limits are in the catalogue's
`audio` (`engine.AudioCaps`; vboard's generators read it).

**Kokoro** runs through mlx-audio's Kokoro pipeline (the worker calls it directly: `model.generate` does not hand
out the tokens' times), the voices read from the weights' `voices/` folder (pinned `mlx-community/Kokoro-82M-bf16`:
`config.json`, `kokoro-v1_0.safetensors`, `voices/*.safetensors`). The voice's first letter is its language
(a American, b British English, e es, f fr, h hi, i it, p pt-br, z zh); a language without a voice gets its default
(af_heart, ef_dora, ff_siwis, hf_alpha, if_sara, pf_dora, zf_xiaobei). Text processing (G2P) is misaki 0.9.4 in the
speech environment: English with spaCy's `en_core_web_sm` (pinned as a wheel, so nothing is fetched at run time)
and espeak-ng for unknown words, Chinese with jieba/pypinyin, Spanish, French, Italian, Portuguese and Hindi through
espeak-ng — which comes as a library inside `espeakng-loader`'s wheel: **nothing to install on the system**. (That
espeak-ng keeps its data path in 160 bytes and ends the process when it cannot find its data; with a deep
`MLXLAB_HOME` the worker copies the data, 19 MB, to the temp folder once and uses it from there.) Japanese is left out: misaki[ja] builds
pyopenjtalk from source and needs unidic's 500 MB dictionary (`python -m unidic download`). Measured on an M1 Max:
6 s of English in 2 s on first use (the pipeline loads), then 5 s in 0.3 s; 2.5–2.8 GB peak.

Voice library API (worker; proxied by the hub):

* `GET /api/voices` → `[Voice]` (sample as /files/ url) + `presets: {model: [names]}`
* `POST /api/voices` multipart `name`, `sample` (audio; converted to 24 kHz mono wav, trimmed to ≤ 20 s),
  optional `transcript` (else transcribed with Qwen3-ASR in the speech worker) → Voice
* `POST /api/voices` JSON `{name, description, language}` → a designed voice
* `PATCH /api/voices/{id}` `{name?, transcript?, description?}`, `DELETE /api/voices/{id}`
* Transcription is a job too: `kind speech`, model `qwen3-asr`, request `{audio}` → `text`. (Used for samples.)

## Avatar (`avatar.go`)

Request: `images[0]` portrait, `audio` or (`text` + `voice` + `speechModel`), `prompt` (scene/motion), `width`,
`height`, `seed`. If there is text, the backend first calls `Speaker.Speak` (in-process), then animates.

| id | model | notes |
|---|---|---|
| `ltx-2.5-avatar` | LTX-2.5 (the `ltx-2.5-q4` pack) with the portrait as first frame and the speech as a frozen audio latent (worker `workers/avatar_ltx.py`: ltx-2-mlx's distilled pipeline, plain Euler in both stages; its own `a2v` needs the dev transformer) | LTX-2 Community License (gated pack) |
| `longcat-avatar-1.5-q8` / `-q4` | LongCat-Video-Avatar 1.5 MLX (worker `workers/avatar_longcat.py`, runtime `longcat-avatar` pinned to xocialize/longcat-avatar-mlx@e2e1e87); experimental, very slow | MIT; bf16 left out (~50 GB peak) |

Sizes: the request's width/height (the web app sends multiples of 32 around 640²) are fitted per model:
LTX snaps to multiples of 64 and scales down to ≈640×640 pixels at most (default 512×640); LongCat snaps to
multiples of 16 and scales down to the area of 256×432 (its default; larger is far too slow). `frames` is the longest
clip (LTX default 161 at 24 fps ≈ 6.5 s, 8k+1; LongCat 93 at 25 fps, 4k+1). Longer speech is split at pauses
(ffmpeg silencedetect) and chained: an LTX clip starts from the last frame of the one before (that frame is dropped
at the seam); LongCat takes the portrait as the reference of every clip.

Statuses: `speaking …` (the Speaker), `animating …` / `animating i/n …`, the worker's own, `muxing the speech …`.
Output: the mp4 (first job file) with the original speech muxed as its sound track, then the spoken wav (text
requests only). Stats: `duration` (video seconds), `seconds` (wall clock), `speechSeconds`, `clips`, `peakGB`,
`clipsDir` (the work folder, removed with the job).

## Sound (`sound.go`)

`moss-soundeffect` — MOSS-SoundEffect MLX 4-bit (Apache-2.0; 24 kHz mono, the codec's rate; 1–30 s). Request: `prompt`, `duration`, `seed` → wav.
