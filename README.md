# ai-connector — one server for local AI on your own machines

ai-connector generalises mlxlab, vboard-ai and docscan-ai into one application: a **server** (web UI, sign-in, queue,
APIs for other apps) and **workers** (Macs, Linux and Windows PCs) that dial out to it and run the models.
It starts as mlxlab (copied from `local-ai-tests` at e4606bf, module `github.com/philipparndt/ai-connector`); the
plan and its phases are on the vboard board "AI Connector — Architecture".

| Binary | Was | Runs on |
|---|---|---|
| `ai-connector-server` (`cmd/ai-connector-server`) | `mlxlab-hub` | the server (Raspberry Pi, linux/arm64 image `pharndt/ai-connector-server`) |
| `ai-connector` (`cmd/ai-connector`) | `mlxlab` | each worker; also `serve`, the TUI and `run` |

The wire names stay mlxlab's for now (`/api/*`, the `mlxlab_session` cookie, `X-Mlxlab-*` headers, `MLXLAB_*`
environment, `~/.config/mlxlab`), so mlxlab's web UI, TUI and iOS app keep working against the new server.

### What a worker runs, by platform

| | Apple silicon Mac | Linux (x86-64, arm64) | Windows (x86-64) |
|---|---|---|---|
| embedder (Qwen3-Embedding-0.6B), classifier (Laya) — pure Go | ✓ Accelerate / Metal | ✓ OpenBLAS if installed, else Go | ✓ Go |
| chat on MLX, with tools (home / web / files zones) | ✓ | – | – |
| image, video, music, speech, sound, avatar (Python on MLX) | ✓ | – | – |
| chat and embedding models of local runtimes (below) | ✓ | ✓ | ✓ |

**Local runtimes**: the worker finds the models of OpenAI-compatible servers on its machine — Ollama (:11434),
LM Studio (:1234), llama.cpp (:8080), vLLM (:8000) — at start and every 30 s, and offers them as `<runtime>/<model>`
(e.g. `ollama/qwen3:8b`): chat (streamed, thinking as `<think>`; no tool zones) and embeddings. `MLXLAB_RUNTIMES`
picks them: unset = those four on 127.0.0.1, `none` = off, or a list like `ollama,gpu=http://10.0.0.5:8000#key`
(name=URL, a key after `#`).

**Agents (Claude Code)**: a worker where Claude Code is installed offers `claude-code/sonnet`, `/opus` and `/haiku`
— chats with Claude Code as a long-running agent (one session per conversation, kept warm between turns, with its own
folder, tools and MCP servers; see [docs/api.md](docs/api.md), "Agents (Claude Code)"). To enable it on a worker:
install Claude Code there (`claude` on the PATH of the worker process) and **log in with your own subscription on
that machine** (`claude`, then `/login`) — the worker runs Claude Code as you, so the server sends Claude Code jobs
only to workers **you** enrolled (a worker's `owner` is the user who connected it). Optionally limit what sessions may
do in `~/.config/mlxlab/agents.json`, e.g. `{"allowedTools":["Read(./**)","Grep(./**)","WebSearch"],"model":"sonnet",
"idleMinutes":30,"maxSessions":4}` (the defaults read only the session's own folder and the web; nothing that edits
files or runs commands is allowed unless listed; the worker's secrets are denied to sessions — see docs/api.md,
"Security"). Sessions can also run in a terminal you attach to from the browser (driver `tty`; macOS and
Linux).

A worker offers only what it can run, and the server routes a job only to a worker that lists its model. The
Linux worker needs glibc (it loads OpenBLAS at run time; Ubuntu, Debian, Fedora — not Alpine/musl). Windows workers
build and pass vet but have not run on Windows yet.

**Images in chats**: chat models that can see (`images: true` in the model lists: Claude Code; LM Studio vision
models such as the Qwen3.6 MLX checkpoints; Ollama models with the `vision` capability) take images — OpenAI
`image_url` parts with data URLs, or screenshots pasted into vboard's chat; others refuse them (docs/api.md, "Images
in chats").

### Using it from other apps

An application gets its own token: `POST /api/clients {"name":"docscanner"}` while signed in (or in the console: Clients) answers `{"client":{…},"token":"aic_…"}`, shown once. With it the server speaks the OpenAI API
(`/v1/models`, `/v1/embeddings`, `/v1/chat/completions`), TEI's `POST /embed` and Laya's `POST /v1/systemone`, and
the native jobs API; chats become jobs in a worker's queue, embedding and classifying run beside it. Model names
are the catalogue ids (`GET /v1/models`); a Hugging Face name like `Qwen/Qwen3-Embedding-0.6B` works too. The whole
contract: [docs/api.md](docs/api.md).

```bash
curl https://ai.example.org/v1/chat/completions -H "Authorization: Bearer $AIC_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.6-35b-a3b","messages":[{"role":"user","content":"Explain MoE in two sentences."}]}'
curl https://ai.example.org/v1/embeddings -H "Authorization: Bearer $AIC_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-embedding-0.6b","input":["a red fox","a brown dog"]}'
```

```python
import os
from openai import OpenAI

client = OpenAI(base_url="https://ai.example.org/v1", api_key=os.environ["AIC_TOKEN"])
vecs = client.embeddings.create(model="Qwen/Qwen3-Embedding-0.6B", input=["a red fox", "a brown dog"])
for chunk in client.chat.completions.create(model="qwen3.6-35b-a3b", stream=True,
                                            messages=[{"role": "user", "content": "Hello"}]):
    print(chunk.choices[0].delta.content or "", end="")
```

Thinking is off unless asked for (`reasoning_effort` other than `"none"`); the thought then streams as
`reasoning_content`. `MLXLAB_EMBED_MODEL` and `MLXLAB_CLASSIFY_MODEL` set the server's default models.

### vboard without vboard-ai

vboard's "local AI" (classify, "Group by theme", the board chat with its tools) can come from the workers without
anyone running vboard-ai. Two ways:

1. **A connector** (the server dials vboard, as vboard-ai does): in vboard, *Set up AI* (a board's AI menu or the user
   menu) → *Create token* (a personal token `vbp_…`, or a board token for one board). In the console, *Connectors* →
   *Add vboard*: the vboard address, the token, optionally a board id (then everyone on that board uses it; the token
   needs "Can edit" there) and the models (blank: the server's defaults, `MLXLAB_CLASSIFY_MODEL`,
   `MLXLAB_EMBED_MODEL`, the first chat model a worker offers that is not Claude Code). Under *Chat agents* choose
   what people can pick from in vboard's chat panel, in order: chat models and **Claude Code** (`claude-code/*`), which
   works as an agent — it changes the board itself through vboard's MCP server, in a session kept warm per
   conversation — on the workers of the person who added the connector, with their Claude subscription, and is offered
   only while such a worker is connected. The first entry is the default and must be a chat model (older vboards and
   search use it). Without a list: the chat model alone. The connector shows `connected` with the vboard user
   the token belongs to. It tells vboard what the connected workers offer and says it again when that changes; chats
   become jobs in a worker's queue (`client` "vboard: <name>"), classify and embed run beside the queue.
   Or by API: `POST /api/connectors {"url":"https://vboard.example.com","token":"vbp_…"}` (docs/api.md, "Connectors").
2. **Configuration only** (vboard's own server-side AI settings, no bridge): make a client token (*Clients*) and
   point vboard's environment at this server — `EMBED_URL`/`EMBED_TOKEN` (TEI `/embed`), `CLASSIFIER_URL`/
   `CLASSIFIER_TOKEN` (Laya `/v1/systemone`) and, for the board chat, vboard's cloud AI: `CLOUD_AI_BASE_URL=<server>/v1`
   and `CLOUD_AI_API_KEY` in its environment, then on vboard's admin page *Cloud AI* the provider
   "OpenAI-compatible", that base URL, a model and *enabled*; the *Clients* page shows the snippet filled in. That serves everybody on that vboard, and the chat runs as vboard's "cloud AI" (its consent rules
   apply), not as a person's local AI.

Compatibility: the connector speaks the bridge protocol of every vboard since the bridge exists — tested against
each stage: `d508560` (classify), `0992b68` (+ embed), `f914d44` (+ chat with tools), `175647c` → today (+ handover on
rolling updates, chat status), and vboard's `ai-agents` branch (+ several agents per bridge, which the agent list
needs; older vboards see the default chat model only).

### Console

The server has two web apps: the **console** of ai-connector under `/console/` (`console/`, embedded from
`internal/consoleui`) and the mlxlab app at `/` (`web/`) as before. Both sign in with the server's password
(`POST /api/login`) and share the session cookie. The console has:

- **Overview** — the connected workers and the merged queue of every worker, live (`/api/events`,
  `/api/worker/events`): model, kind, state, worker, client, age; queued and running jobs can be cancelled.
- **Workers** — specs (OS, chip/CPU, RAM, GPU/VRAM), load, models in memory, primary, revoke, and **Connect a
  worker**: one paste for macOS/Linux (Terminal) or Windows (PowerShell) with the worker's own token, and a live
  "waiting → connected".
- **Models** — every model the workers offer, by kind and backend, and on which worker it is loaded, on disk or
  too big.
- **Clients** — API tokens for applications (`aic_…`, shown once) and copy-paste snippets with this server filled
  in: curl and Python (OpenAI SDK) for `/v1/chat/completions` and `/v1/embeddings`, vboard's sidecar environment,
  docscanner's.
- **Connectors** — vboard connectors (the server dials vboard's classifier bridge): add, edit, enable/disable,
  delete, with their live state; the chat agents in order, and which of them vboard is offered now.

`MLXLAB_CONSOLE_HOSTS` (comma list of host names, e.g. `ai.example.org`) makes `GET /` on those hosts redirect to
`/console/` (matched against `X-Forwarded-Host` behind traefik, else `Host`; port and case ignored). Every host
serves both apps at their paths. Pages of the console whose API the server does not have yet say so instead of
failing.

Build and develop: `make console` (pnpm, into `internal/consoleui/dist`; `make build`/`make server` run it),
`make console-dev` (Vite on http://localhost:5173/console/, proxying `/api`, `/files` and `/v1` to `MLXLAB_URL`,
default `http://127.0.0.1:8484`), `cd console && pnpm test` (the pure logic: install commands, snippets, specs).

### Deploying

The server ships as the image `pharndt/ai-connector-server` (linux/arm64 + amd64; `make image`, released by CI on
`server-v*` tags) with the Helm chart [`deploy/helm/ai-connector`](deploy/helm/ai-connector): one replica, a PVC for
`/data`, an Ingress for the console host and the app host, probes on `GET /healthz`. On the Raspberry Pi it runs as
its own release next to the mlxlab hub (`make deploy`). First install, values, upgrading, workers and backups:
[docs/deploy.md](docs/deploy.md).

### Locally, with vboard

`make local-up` runs the server, this Mac as its worker (own config and state, the model store shared with
`~/.config/mlxlab`), a vboard from `~/dev/vboard` and a connector whose agents (a local Qwen, Claude Code) appear in
vboard's chat panel; `make local-down` stops it, the data stays in `~/.local/share/ai-connector-local`.
[docs/local.md](docs/local.md).

---

# mlxlab — local AI models on a Mac

`mlxlab` tries local models on Apple silicon: chat models, an embedder and a classifier (taken over from vboard-ai,
run in Go), **FLUX.2 [klein]** and **Qwen-Image** for images and **LTX-2.5** for video with sound. The Mac is the **worker**: it runs
the models and the job queue and dials the **hub** on the Raspberry Pi, which signs people in and serves the web app.
The web app, the terminal UI and the iOS app all talk to the hub, so a job started in one shows up in the others
(see [docs/architecture.md](docs/architecture.md)).

```bash
make                    # → bin/ai-connector
bin/ai-connector worker --hub https://mlxlab.example.org --token …   # serve this Mac to the hub (remembered; make worker)
bin/ai-connector serve        # standalone on http://127.0.0.1:8484, no hub (development)
bin/ai-connector              # the TUI, a client of the hub (mlxlab tui --server URL)
bin/ai-connector list         # the models, and which are already on this Mac
bin/ai-connector run qwen3.6-35b "Explain MoE in two sentences."
bin/ai-connector run --size 768x768 --seed 7 flux2-klein-4b "a red fox in fresh snow, golden hour"
bin/ai-connector run --frames 49 ltx-2.5-q4 "a heavy wooden door creaks open onto a sunlit garden"
bin/ai-connector run --frames 49 --size 512x320 ltx-2.5-hq-q8 "a red fox trots through fresh snow"   # dev model + CFG
```

## Models

| kind | model | runs on | size |
|---|---|---|---|
| chat | Qwen3.5 4B / 9B, Qwen3.6 35B-A3B / 27B (vboard's catalogue), plus any Qwen3.x MLX model in LM Studio or the HF cache | MLX from Go (vboard-ai's engine, no Python) | 3–19 GB |
| embed | Qwen3-Embedding-0.6B: cosine similarities between texts | Go (Accelerate / Metal) | 1.1 GB |
| classify | Laya typed-decisions: a choice question about every text | Go (Accelerate / Metal) | 0.8 GB |
| image | FLUX.2 [klein] 4B (Apache 2.0), 9B (non-commercial, gated), 4B base; Qwen-Image 2512 (text to image) and Qwen-Image-Edit 2511 (edits reference images only; both Apache 2.0, 20B), Qwen-Image 2.1 (Qwen Research License, non-commercial) | [mflux](https://github.com/filipstrand/mflux) 0.20.0 | FLUX 16 / 35 GB, Qwen-Image 54 GB (2.1: 31 GB) download; transformer quantised to 8-bit when loaded (Qwen's text encoder stays bf16) |
| video | LTX-2.5 22B audio+video, int4 / int8: distilled two-stage pipeline (fast), and **LTX-2.5 HQ** (the open dev model with CFG, two-stage HQ pipeline; see below) | [ltx-2-mlx](https://github.com/dgrauet/ltx-2-mlx) @ `e421363` | about 30 / 45 GB; HQ 47 / 65 GB (the same folder plus the dev transformer and the distilled LoRA) |
| music | ACE-Step 1.5 (turbo, XL turbo, base, XL base; MIT), Stable Audio 3 Small / Medium (instrumental only, Stability AI Community License), LeVo 2, HeartMuLa, YuE2 — BPM, tempo lock, best of N: [docs/music.md](docs/music.md) | Python workers (MLX, ggml, PyTorch/MPS) in pinned uv environments | 2–30 GB |

Chat models already on the Mac (LM Studio's folders and the Hugging Face cache) are used where they are. A catalogue
model is checked once against its pinned SHA-256 sums. Everything else downloads on first use.

**LTX-2.5 HQ** (`ltx-2.5-hq-q4` / `ltx-2.5-hq-q8`) is the closest local thing to Lightricks' *LTX-2.5 Pro*, which
is their hosted API tier with no weights. It runs the open dev (non-distilled) transformer through ltx-2-mlx's
`--two-stages-hq` pipeline. Stage 1 works at half size with the res_2s sampler and real CFG: 15 steps by default
(`--steps`), CFG 3 and ltx-2-mlx's negative prompt. Stage 2 fuses the distilled LoRA in and refines at full size in
3 steps. A first frame (image to video) works as with the distilled models. On a 64 GB M-series Mac, the q8 model made a 512×320, 49-frame clip in
341 s at a ~30 GB peak. The distilled q8 took 60 s for the same clip and seed, so HQ is about 6× slower. In return
it gives a cleaner subject and follows the prompt more closely. Both models of a quantisation share one folder, so
HQ downloads only `transformer-dev.safetensors` and the distilled LoRA (q8: 19 + 8 GB). The default size is
640×384.

**FLUX.2 [klein] 9B** and the **LTX-2.5** packs (`dgrauet/ltx-2.5-mlx-q4` / `-q8`) are gated on Hugging Face. Accept
the licence on their model page, then log in with `uvx hf auth login` or `export HF_TOKEN=…`.

## How it runs

- **In Go:** the chat models run on MLX through the mlx-c dylib, embedded in the binary and loaded with purego (no
  cgo). The embedder and the classifier run in Go too. All of this comes from vboard-ai (`internal/mlx`, `llm`,
  `qwen3`, `laya`, `tensor`, `tokenizer`, `models`).
- **In Python, managed by mlxlab:** FLUX.2, Qwen-Image and LTX-2.5 run in Python MLX ports. On first use mlxlab creates their
  environments with `uv` (`~/.config/mlxlab/envs/`, pinned versions; needs `brew install uv`).
  - mflux runs as a worker that stays alive, so the model stays loaded between images.
  - ltx-2-mlx runs as one process per video, since it loads its stages one after another anyway. It needs `ffmpeg`.
- **Memory:** jobs run one at a time, and only one large model (chat, image or video) stays loaded. Starting a job of
  another kind unloads the previous one. `u` in the TUI (or *Unload* in the web app) frees everything.

Files live in `~/.config/mlxlab` (`$MLXLAB_HOME`): `outputs/` (images, videos, uploads), `history.json`, `models/`
(the pinned Go models), `envs/`, and `lib/` (the unpacked MLX runtime). Hugging Face downloads go to the usual HF
cache.

## TUI

`mlxlab` (or `mlxlab tui --server URL`) is a client of the hub: it asks for the password the first time and keeps
the device token in `~/.config/mlxlab/client.json`. `--server http://127.0.0.1:8484` uses `mlxlab serve` instead.

`↑↓` choose a model · `enter`/`tab` type · `e` (or `ctrl+e` while typing) next example prompt · `esc` back · `o` open
the newest file · `L` logs · `c` clear the chat · `x` cancel · `u` unload · `w` open the web app · `q` quit.

Image and video prompts take options at the end: `a fox in snow --size 768x768 --steps 4 --seed 7 --q 4 --image ref.png`
(video: `--frames 97 --fps 24 --lowram --noaudio`). The embedder takes one text per line (`alt+enter`). The classifier
takes a question, then `- label: description` options, then one text per line.

## Web app

The web app has a tab for each kind, each with an *Examples…* menu of ready-made prompts (they live in
`internal/engine/examples.go` and are shared with the TUI).

- **Chat:** pick several models and each gets its own column. A message goes to all of them, which makes it easy to
  compare answers and tokens/s.
- **Image:** reference images for editing, and *Edit* / *Animate* on a result to send it to the image or video tab.
- **Video, Embed (similarity heatmap), Classify,** and a **Jobs** list with logs.

The hub serves it (after sign-in). `mlxlab serve` serves it on this Mac alone, for development: no hub, no sign-in,
and so only on a loopback address (`--addr`, default `127.0.0.1:8484`).

## Worker and hub

`mlxlab worker` dials the hub (WebSocket + yamux, `internal/tunnel`) and serves the API through that connection, so
no port opens on the Mac and the files stay on it. The hub URL and the worker token come from `--hub`/`--token`,
`$MLXLAB_HUB`/`$MLXLAB_WORKER_TOKEN`, or `~/.config/mlxlab/worker.json` (written when both flags are given). The
token is the hub's `MLXLAB_WORKER_TOKEN`. The worker prints its state, reconnects with backoff when the connection
drops, and stops on a token the hub refuses. While it is offline the hub answers "The Mac is offline".

**A second worker on the same Mac** (e.g. one for a local ai-connector server while another serves the hub on the
Pi): `--config <file>` (`$MLXLAB_WORKER_CONFIG`) keeps its hub and token in another file than `worker.json`, and
`--state <dir>` (`$MLXLAB_STATE`) its own jobs (`history.json`, `outputs/`), prompts, `approvals.json` and Claude Code
sessions (`agents/`, `agents.json`). The models, `envs/`, `lib/`, `tools/`, `mcp.json`, the voices and trained styles
stay shared in `~/.config/mlxlab` — nothing is downloaded twice. Defaults: `worker.json` and the config dir itself, as
before. `make local-up` runs such a second worker ([docs/local.md](docs/local.md)).

## iOS app

`ios/` holds the iPhone/iPad app (SwiftUI, generated with XcodeGen like yacal; see `ios/README.md`). It talks to
the hub and signs in there with the password once; the hub keeps one token per device.

## Licences

MLX and mlx-c are MIT (`internal/mlx/darwin_arm64/LICENSE-MLX.txt`). The models keep their own licences:
FLUX.2 [klein] 4B is Apache 2.0, 9B is FLUX non-commercial, Qwen-Image 2512 and Edit 2511 are Apache 2.0, Qwen-Image
2.1 is the Qwen Research License (non-commercial), LTX-2.x is the LTX-2 Community License, and Qwen is
Apache 2.0.
