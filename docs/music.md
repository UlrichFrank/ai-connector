# Music

Song models (`internal/engine/music.go`, kind `music`): a style (and lyrics, for the models that sing) in, a WAV out.
Each runtime is a Python worker in its own pinned uv environment (`internal/engine/workers/*.py`), one at a time;
it keeps its model loaded between songs.

| id | model | runtime | vocals | longest | licence |
|---|---|---|---|---|---|
| `ace-step-1.5-turbo` | ACE-Step 1.5, 2B turbo DiT (8 steps) | `acestep` (MLX DiT + MLX planner LM) | yes, 50+ languages | — | MIT |
| `ace-step-1.5-xl-turbo` | ACE-Step 1.5, 4B XL turbo DiT (8 steps) | `acestep` | yes | — | MIT |
| `ace-step-1.5-base` | ACE-Step 1.5, 2B base DiT (50 steps, CFG 7.5); also lego, extract | `acestep` | yes | — | MIT |
| `ace-step-1.5-xl-base` | ACE-Step 1.5, 4B XL base DiT (50 steps, CFG 7.5) | `acestep` | yes | — | MIT |
| `ace-step-1.5-xl-turbo-techno` | XL turbo + RyanOnTheInside's techno LoRA (trigger word added) | `acestep` | yes | — | MIT base; the LoRA states none |
| `stable-audio-3-small` | Stable Audio 3 Small-Music (433M, 8 steps) | `stableaudio3` (pure MLX) | **no** (instrumental only) | 120 s | Stability AI Community |
| `stable-audio-3-medium` | Stable Audio 3 Medium (1.4B, 8 steps) | `stableaudio3` | **no** | 380 s | Stability AI Community |
| `minimax-music3-8bit` | MiniMax Music 3 (8B song LM + 0.6B depth LM + 2.4B flow DiT), 8-bit, 30 flow steps | `minimax3` (pure MLX) | yes | 300 s (a ceiling) | MiniMax-Music3 Community (name shown in a commercial UI) |
| `minimax-music3-4bit` | the same, LM and DiT in 4 bits (smaller Macs) | `minimax3` | yes | 300 s | MiniMax-Music3 Community |
| `levo2-large`, `levo2-medium` | LeVo 2 (ggml/Metal) | `levo2` | yes | — | research only |
| `heartmula-3b` | HeartMuLa (PyTorch/MPS) | `heartmula` | yes | — | Apache-2.0 |
| `yue2-3b` | YuE2 (MLX, 8-bit) | `yue2` | yes | — | CC BY-NC 4.0 |

Studio tasks (`Model.Tasks`): ACE-Step `cover` (remix), `repaint`, `extend`; the 2B base also `lego`, `extract`;
Stable Audio 3 `cover`, `repaint`, `extend`; YuE2 `restyle`.

## Request fields that matter for music

* `prompt` — the style. A tempo in it (`"138 bpm"`, or `"BPM: 120"` / `"bpm is 92"` as MiniMax Music 3's
  structured captions write it; case and spacing free, 30–300) is used as the tempo.
* `bpm` — the tempo explicitly (wins over the style's). ACE-Step gets it as its `bpm` (the planner then keeps it);
  Stable Audio 3 and MiniMax Music 3 get `", N BPM"` added to the prompt when the prompt has no tempo yet.
* `instrumental` — ACE-Step sings `[Instrumental]` (also when there are no lyrics); MiniMax Music 3 gets wordless
  sections with an `(instrumental)` line and "instrumental, no vocals" in the description.
* `planner` (ACE-Step) — `""` the 1.7B 5 Hz planner LM, `"4b"` the 4B one (`ACE-Step/acestep-5Hz-lm-4B`, 7.8 GB,
  fetched the first time it is asked for), `"off"` no planner (the DiT alone). CLI: `mlxlab run … --planner 4b`.
* `steps`, `guidance` — default per model (`Defaults`): base / XL base 50 steps, CFG 7.5; turbo 8 steps (CFG fixed
  at 1 by ACE-Step); Stable Audio 3 8 steps (its sampler is distilled for 8); MiniMax Music 3 30 flow steps (its
  CFG is fixed: 1.5 for the LMs, 1.7 for the DiT, as upstream).
* `duration` — clamped to a model's longest (`maxSeconds`), with a log line.
* `lockTempo` (bool, default **on** when the song has a tempo) and `takes` (1–3, default 1): see below.

## Sticking to the tempo: tempo lock and best of N (`tempo.go`, `workers/tempo.py`)

Every model, after a new song (`task` generate) with a tempo (`bpm`, or "NNN bpm" in the style):

1. **Measure.** `workers/tempo.py` (its own pinned runtime `tempo`: librosa 1.0.0) finds the strongest pulse within
   ±12 % of the tempo asked for — or of half / double it, scaled back — in the autocorrelation of the onset envelope,
   with a sub-frame peak fit (precise to about 0.1 bpm), plus its salience (strength relative to the strongest
   pulse between 50 and 300 bpm; below 0.5, or a peak on the window's edge, counts as "no clear pulse"). librosa's
   beat tracker alone is not used: it locks onto 2/3 of the tempo for drum and bass and quantises to 1–2 bpm.
2. **Best of N** (`takes` 2–3): the job makes that many takes (seeds seed, seed+1, …), keeps the one closest to the
   tempo (renamed to the job's file; its seed becomes the job's `seed` stat) and **deletes the others** (their
   tempos stay in `takeBPMs`). It multiplies the generation time by N.
3. **Tempo lock** (`lockTempo`): a song off by at most 12 % (and more than 0.6 %, about the measurement's noise) is time-stretched to the exact
   tempo with its pitch kept — ffmpeg's `rubberband` filter when the installed ffmpeg has it, otherwise `atempo`
   (Homebrew's ffmpeg has no rubberband; atempo is a WSOLA stretch, clean for a few percent). The stretched song is
   the job's first file; the song as the model made it is kept as the second file (`…-original.wav`). The stretched
   song is measured again for `bpmFinal`. A song further off, or without a clear pulse, is left alone and the stats
   say `tempoMissed: "tempo missed: got N bpm"`.

Stats: `bpmAsked`, `bpmMeasured`, `bpmFinal`, `stretched` (+ `stretchFactor`, `stretchedWith`), `takes`,
`takeBPMs`, `tempoMissed`, `tempoError` (a failed measurement never loses the song). CLI: `--bpm N`; the lock is on
by default.

## Ending a cut song (`internal/audio/fade.go`)

A song made to a length often does not end: the model is still mid-song when the length runs out and the audio just
stops (HeartMuLa 3B, 150 s asked with ~700 characters of lyrics: −18…−20 dB in every 5 s window up to the last).
After every model's song (`generate`, `cover`, `extend`, `restyle`; not `repaint` / `lego`, which keep the source's
end, nor `extract`, a stem to mix) and after the tempo lock, before anything is encoded, `audio.FadeCut` looks at the
WAV:

* **A cut** is a last second that is still music (above −60 dBFS) and within **10 dB** of the median of the song's
  1 s windows (RMS of all channels; windows below −60 dBFS — silence before or between parts — left out of the
  median). A song that fades out, rings out or ends in silence by itself is far below that and is left as made.
* **The fade** covers the last **3 s** (a clip under 9 s: its last third), rewritten in place; every frame before it
  is untouched. Its curve is exponential: the gain falls by the same number of dB every moment (40 dB over the
  fade, ~13 dB/s), shifted to start at exactly 1 and land on exactly 0 — the ear hears it as an even fade, unlike a
  linear ramp (hangs on, then drops at the end) or an equal-power quarter-sine (made for crossfades: still −3 dB at
  half-time, then a fast drop).
* Stats: `faded` (true / false; absent for the other tasks); vboard's `generateDone` gets `faded: true`. A failed
  fade is logged and the song goes out as made.

Checked on ACE-Step 1.5 turbo (20 s asked with lyrics for a minute, seed 7; the same seed is bit-identical with and
without the fade, so the comparison is exact): last second −14.2 dBFS (song −12.4 LUFS) → faded, last second
−48.5 dBFS, last 100 ms −85 dBFS, every byte before the fade the same. Stable Audio 3 Small (12 s) ended by itself
(last second −67 dBFS) and was left alone.

## ACE-Step: what mlxlab sends (and why)

ACE-Step 1.5 plans a song with a 5 Hz language model first (metadata: bpm, key, time signature, and by default a new
caption), then the DiT renders it.

* **Tempo (fixed).** Without an explicit `bpm` the planner picks one itself: 130 for a "138 bpm" trance style, 125
  for 124, 176 for 174 (measured, all seeds). mlxlab now always passes the tempo it finds in the style (or `bpm`) —
  the planner keeps a given value, and the DiT is conditioned on it: 8/8 songs within 1 % (was 6/8; trance 138 came
  out at 130 every time).
* **Caption rewriting (tried, not shipped).** With `use_cot_caption` (the default) the planner replaces the style
  with its own caption. Keeping the caption as written (`cot_caption: false`) with planner temperature 0.65 — the
  community advice for EDM — measured no better: XL turbo 36 % vs 37 % percussive energy, pulse clarity 0.203 vs
  0.230; base 6/8 vs 8/8 songs on tempo. So the planner keeps its defaults; the worker still takes `cot_caption` and
  `lm_temperature` (benchmark configs `*-nocot`).
* **The 4B planner** (`planner: "4b"`) kept the tempo as well, but gave less percussive energy (30–32 % vs 36–39 %)
  at twice the time: opt-in only.

The base models (2B, 4B XL) now run 50 steps at CFG 7.5 (ACE-Step's docs: 32–64 steps, CFG 7–9 for base/SFT).
ACE-Step's `use_adg` (adaptive dual guidance) exists only in its PyTorch loop; the MLX diffusion mlxlab uses applies
plain CFG (with the CFG interval), so it is not set.

**LoRAs.** The pinned ACE-Step (`ca1e85f`) loads a LoRA with PEFT onto the PyTorch DiT (`add_lora`), but its MLX
DiT is converted from the PyTorch weights once, at initialisation, and never again — on the MLX path a LoRA loaded
the official way is silently ignored. The worker therefore merges a plain PEFT LoRA into the PyTorch weights
(`W += scale·alpha/r·B·A`) and converts the DiT to MLX again before generating. One is in the catalogue:
`ryanontheinside/techno-acestep1.5-xl-v1` (XL turbo, rank 64 on q/k/v/o, trigger `roti-t3kn0`, which mlxlab puts in
front of the style). Note its own card: trained on 15 synthetic tracks, no licence stated. Measured on the techno
prompt (2 seeds): 38 % percussive energy, 18.8 % in the kick band, tempo exact — about the same as XL turbo without
it (25 % and 93 % for the same two seeds: the seed matters more); worth a listen, not a clear win.

## Styles: trained and community

A **style** (`internal/styles`, API in docs/architecture.md, "Train your own style") is a LoRA a song picks with
`style` (and `styleStrength`, 0..2; 0 the style's own strength). Its trigger word is put in front of the prompt when
the prompt does not have it. Two kinds:

* **Trained** here, on your own tracks (ACE-Step 1.5, Stable Audio 3 Medium): `lora/` in the style's folder.
* **Community**: a PEFT LoRA someone shared on Hugging Face, **pinned to a commit** (`source: {repo, rev, url,
  author, licence, baseText, size, rank, alpha}`, `weightsFile`). It is not trained here: no tracks, no training
  (refused), but it can be renamed, given another default strength, and removed. Removing it deletes the style, not
  the downloaded weights, which stay in the model store.

**Adding a community style** (`POST /api/styles {"repo": "owner/name[@rev]"}`, or a huggingface.co link; the Styles
page's curated "Add" and "Add from Hugging Face"; on iOS "Community styles…" under the song form's style picker):

1. The commit: the curated list's pin, else the one the revision (default `main`) names now (`/api/models/<repo>`).
   A gated repository gives the usual "accept its licence" error.
2. `adapter_config.json` at the top must be a PEFT `LORA` (not DoRA), since the worker merges plain LoRAs only. The
   machine-readable sidecar `<name>.metadata.json` (schema v1, as RyanOnTheInside's LoRAs ship it) is read if
   there is one: `inference.primary_trigger_word`, `recommended_strength`, `recommended_steps`,
   `recommended_guidance`, `recommended_shift`, `example_prompts[{caption,bpm,key}]`,
   `compatibility.base_model_required`, `files.weights` (with its SHA-256 checked against the listing). Without a
   sidecar the user gives the trigger word and the base model (`trigger`, `baseModel`). The weights file is the
   sidecar's, else `adapter_model.safetensors`, else the one `.safetensors` there is.
3. The base model is mapped to the catalogue: "AceStep v1.5 Turbo (2B)" or a checkpoint path like
   `…/acestep-v15-turbo` → `ace-step-1.5-turbo`; XL/4B → `ace-step-1.5-xl-…`; base/SFT → `…-base`. Only ACE-Step 1.5
   takes community LoRAs (Stable Audio 3 styles have another format). If the request names a model other than the
   one the repository names, it is refused (a 2B LoRA does not fit the 4B DiT).
4. Only the small files are fetched when adding. The weights come through the model store (pinned, size and SHA-256
   checked, linked from the Hugging Face cache when it has them) with the **first song** that uses the style. That
   song shows the download like any model's.

`GET /api/community-styles` lists the curated LoRAs; `?repo=…` returns what the repository says about itself
(commit, trigger, base, recommended settings, examples, licence) without adding it. With several Macs the hub sends
both, like `POST /api/styles`, to `?worker=<id>` (else the primary). A community style stays on the Mac it was added
to, like a trained one, and songs with it run there.

**Recommended settings.** When a song uses a style with `recommended` settings and leaves `steps`, `guidance` or
`shift` on Auto (0), the recommended values are used, before the model's defaults. The request's own values always
win, and a log line names what was applied. `shift` is a request field now (ACE-Step's timestep shift; 0: the worker's
3.0 for turbo, 1.0 for base). Note that turbo models ignore `guidance` (ACE-Step fixes CFG at 1 for them), so a
recommended guidance only matters on base models.

**Licence.** A LoRA whose repository and metadata state no licence is shown as "licence not stated" in the web UI
and on iOS, with a note to check its model card before publishing songs made with it.

**Curated list** (`engine.CommunityStyles`):

| LoRA | base | trigger | notes |
|---|---|---|---|
| `ryanontheinside/deep_house-acestep1.5-v1` @ `746cf45` | ACE-Step 1.5 turbo (2B) | `roti-d33ph0us` | rank 64 / alpha 128 on q/k/v/o, 88 MB, 15 synthetic tracks; recommended strength 1.0 (tested 0.3–1.0), 8 steps, guidance 7, shift 3; licence not stated. Its own MFCC evaluation says "negative" (−0.004 vs baseline) |

**Measured** (ACE-Step 1.5 turbo, the deep house LoRA, example prompt 1, 122 BPM, 30 s, seed 4242, tempo lock off,
`TestCommunityStyleLive`, build tag `communitylive`). The worker merged 192 layers (factor alpha/r·scale = 2). The
three runs were the style, the trigger word alone without the LoRA, and neither:

| | centroid | sub < 60 Hz | 60–250 Hz | 250 Hz–2 kHz | 2–6 kHz | > 6 kHz | RMS |
|---|---|---|---|---|---|---|---|
| with the style | 278 Hz | 46.8 % | 41.8 % | 8.8 % | 1.7 % | 1.0 % | −14.3 dB |
| trigger word only | 177 Hz | 50.9 % | 43.9 % | 3.7 % | 1.0 % | 0.5 % | −14.5 dB |
| without | 206 Hz | 33.6 % | 56.3 % | 8.8 % | 0.9 % | 0.5 % | −15.1 dB |

The LoRA alone (style vs trigger only, the same prompt and seed) changes the spectrum by 12.4 dB log-spectral
distance (waveform correlation 0.74), with more energy in the mids and highs (keys, vocals). The trigger word alone
changes the arrangement (correlation 0.08) but hardly the spectrum (3.4 dB). Each song took ~14 s; the first one with
the style took 82 s, including the model load and the 88 MB download.

## Stable Audio 3

Stability AI's Stable Audio 3 (May 2026) runs on its official pure-MLX runtime: the `optimized/mlx` folder of
github.com/Stability-AI/stable-audio-3, fetched once at the pinned commit (`stableaudio3.go`, into
`~/.config/mlxlab/tools/stable-audio-3-3a82c80/`) and imported by `workers/stableaudio3.py`. Weights: the MLX `.npz`
files of `stabilityai/stable-audio-3-optimized` (not gated), pinned: Small 1.8 GB (DiT 0.9 GB, SAME-S codec, T5Gemma
text encoder 0.57 GB), Medium 6.4 GB (DiT 2.9 GB, SAME-L codec 2×1.7 GB, T5Gemma). Output: 44.1 kHz stereo.

* **Instrumental only.** There is no lyrics input; lyrics in a request are ignored (logged). The About text says so.
* **Tempo** goes into the prompt (`"124 BPM"`); the model follows it closely.
* **Studio tasks:** `cover` → audio-to-audio (`init_audio`, σmax = 1 − 0.6·strength, clamped to 0.3–1; strength 0.8
  → 0.52, inside upstream's 0.4–0.8), `repaint` → inpainting of [start, end] (end ≤ 0: to the end), `extend` →
  continuation (the source padded to source + extend seconds, the new part inpainted).
* **Licence:** Stability AI Community License — free for individuals and organisations below $1M annual revenue,
  an enterprise licence above; the T5Gemma encoder also carries Google's Gemma terms. The catalogue shows it as the
  model's `licence` (not `nonCommercial`), and the About text calls it restricted.
* Speed on this Mac (64 GB): Small about 1.5 s, Medium about 4 s for a 45 s song; the Medium worker peaks at about 10 GB (its footprint, all models loaded).

## MiniMax Music 3

MiniMax's MiniMax Music 3 (`MiniMaxAI/MiniMax-Music3`, rev `fbdf52f`, 57 GB in PyTorch): an 8B global LM
(initialised from Qwen3-8B) predicts the semantic RVQ code of each 25 Hz frame, a 0.6B local LM the seven acoustic
codes, the hidden states of both condition a 2.4B flow-matching DiT (200-frame windows, 100-frame hop) and a
Flow-VAE decoder makes the audio. Songs with vocals up to five minutes; lyrics plus a music description.

**Runtime (`minimax3`, `minimax3.go`, `workers/minimax3.py`).** Upstream supports only CUDA (SGLang-Omni,
diffusers, ComfyUI). Three MLX ports existed in October 2026, all young (August 2026, 1–7 stars):

| port | weights | notes |
|---|---|---|
| **github.com/mikolaj92/minimax-music3-mlx** (Apache-2.0) — **used** | `mikoy92/MiniMax-Music3-MLX` bf16 21.9 GB, `-8bit` 12.4 GB, `-6bit`, `-4bit` 7.4 GB | uses mlx-lm's Qwen3 with a KV cache; works with mlx 0.32.3 / mlx-lm 0.32.0; the 8-bit and 4-bit conversions cover the smaller Macs |
| github.com/vanch007/mlx-minimax-music3 (MiniMax licence) | `vanch007/MiniMax-Music3-MLX-8bit` 13.2 GB | component parity against the PyTorch reference published; pins mlx < 0.32; 8-bit only; its speed-up PR is unmerged (155 s for 10 s on a loaded M3 Max before it) |
| github.com/appautomaton/mlx-minimax-music3 (MIT, PyPI alpha) | `appautomaton/MiniMax-Music3-MLX` dense 26.6 GB | Python ≥ 3.13; dense only (18 min for 3 min on an M5 Max) |

All three implement the same pipeline (prompt `<|caption_start|>…<|lyrics_start|>[start]\n…`, CFG with
`<|audio_cfg|>`-masked text, top-k 50, Euler flow with the window overlap). The port is installed into the uv
environment from git at the pinned commit `b7699bc` (its own dependencies pinned beside it). The port's vocoder
writes **44.1 kHz** stereo (the official serving profile resamples to 32 kHz).

**Memory fix in the worker.** The port decodes every flow window lazily and evaluates all of them at the end, so the
vocoder graphs of all windows live at once: 45.5 GB peak for a 30 s song. The worker wraps the vocoder to evaluate
each window as it is made: **20.4 GB**, the same time and bit-identical samples.

**What mlxlab sends.** Lyrics as in the table below (lowercase official tags, no blank lines, directions to the
description); the tempo into the description (`", 120 BPM"` unless it names one); an instrumental gets
`[intro]/[instrumental]/[outro]`, each with an `(instrumental)` line (tags alone condition it weakly, as the
appautomaton port warns). The description should be the structured caption MiniMax recommends: *Global Metadata*
(genre, BPM, key, mood, production), *Vocal Details* (gender, timbre, delivery, harmonies), *Arrangement*
(instruments per section, groove). The length is a ceiling (`max_new_tokens` = 25 × seconds): the model ends the
song with its end token when it is done; mlxlab caps it at 300 s.

**Licence.** MiniMax-Music3 Community License: commercial use is allowed if "MiniMax-Music3" is displayed in the
UI of the commercial product; above USD 20M yearly revenue only with MiniMax's written authorization; plus an
acceptable use policy. So `licence` is set, not `nonCommercial`, and the About text names the condition.

**Measured** (64 GB M1 Max, 2026-10-05, 8-bit, 30 s, 30 steps, seed 7, upbeat synth-pop caption with "BPM: 120"):
347 s generation (about 11.6× real time; the LM decodes 750 frames, then 7 flow windows × 30 steps × 2 for CFG),
20.4 GB peak footprint, a 30.0 s stereo WAV at 44.1 kHz, RMS −16.2 dBFS, no silent second, 2 samples at full scale
(the port clips to ±1). Tempo: 122.6 bpm measured, locked to 120.2. The weights load lazily (first use: the
download, 12.4 GB). The 4-bit model was not run; its routing peak (16 GB) is estimated.

## EDM benchmark

`internal/engine/edm_bench_test.go` (build tag `edmbench`): four instrumental prompts (uplifting trance 138, techno
130, progressive house 124, drum and bass 174 — tag style, with explicit kick words), 45 s, seeds 11 and 22, through
several models and settings; the files stay in `MLXLAB_BENCH_OUT/<config>/`. Measured with librosa (scratch script):
tempo by beat tracking (½× and 2× count), the salience of the requested tempo (autocorrelation of the onset envelope
at the target, ½× or 2×, relative to the strongest periodicity between 50 and 300 bpm; 1.0 = the requested tempo is
the dominant pulse), percussive energy share (HPSS), kick-band share (percussive energy below 150 Hz as a share of
all energy) and pulse clarity (mean PLP).

Tempo is measured with mlxlab's own helper (`workers/tempo.py`); "held" = within 1 %. Measured on a 64 GB M1 Max
(2026-10-04, 8 songs per row: 4 prompts × 2 seeds; times are medians and partly slowed by other jobs on the GPU):

| configuration | tempo held | mean tempo error | percussive | kick band | pulse clarity | s per 45 s song |
|---|---|---|---|---|---|---|
| ACE-Step base, **old** (32 steps, CFG 7, no BPM) | 6/8 | 1.6 % | 44 % | 20.8 % | 0.207 | 43 |
| ACE-Step base, **new** (BPM, 50 steps, CFG 7.5) | 8/8 | 0.3 % | 35 % | 13.8 % | 0.219 | ≈ 47–68 |
| ACE-Step base, new + caption kept, LM temp 0.65 | 6/8 | 0.5 % | 39 % | 17.2 % | 0.195 | 47 |
| ACE-Step base, as above + 4B planner | 7/8 | 0.2 % | 32 % | 13.0 % | 0.165 | 85 |
| ACE-Step XL turbo, **old** (no BPM) | 6/8 | 1.6 % | 47 % | 22.1 % | 0.223 | 30 |
| ACE-Step XL turbo, **new** (BPM) | 8/8 | 0.3 % | 37 % | 15.2 % | 0.230 | ≈ 28–48 |
| ACE-Step XL turbo, new + caption kept, LM temp 0.65 | 7/8 | 0.4 % | 36 % | 16.7 % | 0.203 | 28 |
| ACE-Step XL turbo, as above + 4B planner | 7/8 | 0.2 % | 30 % | 9.8 % | 0.176 | 95 |
| ACE-Step XL turbo, new, best of 3 | 8/8 | 0.0 % | 32 % | 13.1 % | 0.220 | 3 × |
| ACE-Step base, **old + tempo lock** | 8/8 | 0.1 % | 44 % | 20.7 % | 0.207 | + 2–4 s |
| ACE-Step XL turbo, **old + tempo lock** | 8/8 | 0.1 % | 47 % | 21.7 % | 0.223 | + 2–4 s |
| Stable Audio 3 Small | 8/8 | 0.0 % | 29 % | 12.8 % | 0.181 | 2 |
| Stable Audio 3 Medium | 8/8 | 0.0 % | 25 % | 12.8 % | 0.197 | 4–6 |
| Stable Audio 3 Small / Medium + AudioSparx tags | 8/8, 8/8 | 0.0 % | 27 %, 23 % | 11.9 %, 10.1 % | 0.186, 0.195 | |
| Stable Audio 3 Small / Medium, CFG 3 + negative "beatless, no drums, acoustic" | 8/8, 8/8 | 0.0 % | 24 %, 21 % | 8.1 %, 9.0 % | 0.163, 0.175 | |

What it shows:

* **Tempo is solved.** Passing the BPM (ACE-Step) or naming it in the prompt (Stable Audio 3) keeps every song within
  1 %; the tempo lock catches the rest (the old settings' 130-bpm trance songs were stretched by 6.2 % to 138.0, the
  new settings' 124.8/129 bpm songs by < 1 %). Best of 3 gives the exact tempo too, at three times the cost — the lock
  is the cheaper way; best of N is for picking among takes.
* **The beat is not solved by settings.** Percussive energy varies far more between seeds (9–96 % for the same
  prompt) than between settings; no setting raised it on average. The new ACE-Step settings even measure a little
  lower than the old (35–37 % vs 44–47 %) — within seed noise for 8 songs, but not an improvement. Listen to the
  page: seeds matter more than knobs; "best of N" by tempo does not pick for punch.
* **Stable Audio 3** is fast (2–6 s for 45 s) and always on tempo, but measurably lighter on drums (25–29 %) than
  ACE-Step; its prompting guide's tags and CFG with a negative prompt made it lighter still.
* The techno LoRA and the 4B planner gave no measurable gain.

The benchmark's listening page (`index.html`, one player per song) and `results.jsonl` live in the bench's output
folder; `TestEDMLockOffline` applies the tempo lock to songs already made (`<config>-lock/`).

## Prompting electronic music

* Tag style, genre first, the tempo as `NNN bpm`, then the rhythm section in words: "four-on-the-floor kick drum,
  punchy kick, driving hi-hats" (house, techno, trance), "fast breakbeat drums, punchy kick and snare" (drum and bass).
* Instrumental: switch *Instrumental* on (ACE-Step then sings `[Instrumental]`), or use Stable Audio 3.
* Keep acoustic words out of an EDM style ("piano", "acoustic", "live drums") unless they are meant.

## Lyrics format

Every song model reads lyrics differently, and each sings whatever it does not understand: HeartMuLa sang
"(Heavy bass drop, mechanical breathing)" as words. mlxlab therefore takes lyrics in **one canonical format** and
translates them for the model right before the job is sent to its worker (`internal/engine/lyrics.go`). The job's
stats record what was sent: `lyricsSent` and `styleSent` ("As sent to <model>").

### The canonical format

```
[intro - heavy bass drop, mechanical breathing]
Yeah.
Black. Pink.
Watch your step.

[verse 2]
Chrome on the dance floor, we rise (we rise)

[drop]
(distorted synth stabs)
```

- **Section tags** on their own line, case-insensitive: `[intro]`, `[verse]`, `[pre-chorus]`, `[chorus]`, `[bridge]`,
  `[drop]`, `[breakdown]`, `[build-up]`, `[instrumental]`, `[outro]` — with an optional number (`[verse 2]`) and an
  optional description after ` - ` or `:` (`[drop - heavy bass]`, `[bridge: whispered]`). Common aliases work too
  (hook, refrain, prechorus, build, interlude, guitar solo, break, ending, fade out …).
- **Sung lines**: everything else. Parentheses inside a line are backing vocals: `we rise (we rise)`.
- **Directions**: a line that is entirely in `( )`, `[ ]` (and not a tag) or `* *` — `(Heavy bass drop)`,
  `[sound of rain]`, `*whispered*` — and inline `*…*` / `[…]`. They are never sung. A line of only vocables in
  parentheses, like `(ooh, ooh)`, or a shout ending in `!`, like `(1, 2, 3, go!)`, is a backing vocal, not a direction.

The built-in "Song from an idea" prompt asks the chat model for exactly this: directions go into the tag's
description or the style, never on their own line.

### Per model

| Model | Tags sent | Directions | Backing vocals `( )` | Source |
|---|---|---|---|---|
| ACE-Step 1.5 | `[Intro]` `[Verse 2]` `[Pre-Chorus]` `[Chorus]` `[Bridge]` `[Build]` `[Drop]` `[Breakdown]` `[Instrumental]` (or its own name: `[Break]`, `[Guitar Solo]`) `[Outro]` | folded into the tag: `[Intro - heavy bass drop, mechanical breathing]` (at most two short phrases, the rest to the caption) | kept (ACE-Step sings them as backing vocals) | ACE-Step 1.5 `docs/en/Tutorial.md`, "About Lyrics" |
| YuE2 | `[Intro]` `[Verse]` `[Pre-Chorus]` `[Chorus]` `[Bridge]` `[Interlude]` `[Outro]`, blank lines between sections; an `[Outro]` is added if missing | removed, appended to the style | brackets removed, words sung | YuE2 `docs/generation.md`, `examples/song.json`, `skills/yue2-music/SKILL.md` ("Keep implementation notes out of lyrics") |
| HeartMuLa | `[Intro]` `[Verse]` `[Prechorus]` `[Chorus]` `[Bridge]` `[Interlude]` `[Outro]` | removed, appended to the tags | brackets removed, words sung (it lowercases and sings every character) | heartlib `README.md`, `pipelines/music_generation.py` (`preprocess`) |
| LeVo 2 | `[intro-short]` `[verse]` `[chorus]` `[bridge]` `[inst-short/medium]` `[outro-short]`; words in an intro/outro/drop move to a sung section | removed, appended to the description | brackets removed | SongGeneration README (gt_lyric format); the worker joins lines with `.` and sections with ` ; ` |
| MiniMax Music 3 | `[intro]` `[verse]` `[pre-chorus]` `[chorus]` `[post-chorus]` `[bridge]` `[instrumental]` `[solo]` `[outro]`, lowercase, no numbers, no blank lines | removed (tag descriptions too), appended to the music description | kept | model card ("section tags on their own lines"), its reference script `scripts/end_to_end/minimax_ttm_test.py`; the port's `normalize_lyrics` lowercases tags and drops text after a tag on its line |

Sections a model has no tag for are mapped by whether they carry words: a wordless `[drop]` becomes the model's
instrumental section, a `[drop]` with words a chorus, a `[build-up]` with words a pre-chorus, a `[breakdown]` with
words a bridge. Directions appended to a style are lowercased, split at commas, deduplicated against the style (whole words),
shortened ("heavy industrial bass drop with vocal chops" → "heavy industrial bass drop") and capped at six
phrases.

`TestLyricsLive` (`MLXLAB_LYRICS_LIVE=heartmula-3b`, optional `MLXLAB_LYRICS_FILE`, `MLXLAB_LYRICS_STYLE`,
`MLXLAB_LYRICS_SEEDS`, `MLXLAB_LYRICS_ONLY=before|after`) sings a song with the raw and the translated lyrics and
transcribes both with Qwen3-ASR.
