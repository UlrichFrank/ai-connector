# Models as data

A model of a runtime the worker already has is a file, not code: one JSON manifest per model. The worker reads the
manifests when it starts and lists their models in `/api/models` like every other; the server, the connectors and the
UIs see no difference.

| Where | What it is for |
|---|---|
| `internal/engine/catalog/<id>.json` | the catalogue: embedded in the binary, every worker has it after a release |
| `<config dir>/models.d/<id>.json` (`~/.config/mlxlab/models.d/`) | trying a model on one machine, no build: read when the worker starts; only this worker lists the model, so the server sends its jobs only here |

The same file works in both places: try it in `models.d/`, then move it to the catalogue unchanged. A file in
`models.d/` whose id the catalogue has already is left out (the worker says so when it starts); a file that does not
parse or does not pass the checks below is left out with the reason.

Every model of the catalogue is a manifest but the chat models, the embedder and the classifier: their files are
pinned one by one with checksums (`internal/llm`, `internal/models`) and their engine is Go itself.

| `backend` | `kind` | the models | `runtimeArgs` it needs |
|---|---|---|---|
| `mflux` | `image` | FLUX.2 [klein], Z-Image Turbo, Qwen-Image, SeedVR2 | `mflux`: mflux's name for the model |
| `cutout` | `image` | BiRefNet | `side`: the side the model sees |
| `ltx` | `video` | LTX-2.5 | `hq`: the two-stage HQ pipeline of the dev transformer |
| `avatar` | `avatar` | LTX-2.5 avatar, LongCat Avatar | `runtime`: `ltx-a2v` or `longcat` |
| `music` | `music` | ACE-Step, Stable Audio 3, MiniMax Music 3, LeVo 2, HeartMuLa, YuE2 | `runtime` (the song runtime), `workerModel`; `maxSeconds`, `lora` {`file`, `trigger`, `scale`} |
| `speech` | `speech` | Kokoro, Qwen3-TTS, Chatterbox, VoxCPM2, Higgs, Voxtral, Qwen3-ASR, Qwen3-ForcedAligner | `workerModel` (the family; its tasks, voices and languages come from it) |
| `sound` | `sound` | MOSS-SoundEffect | `workerModel` |

## The manifest

```json
{
  "id": "z-image-turbo",
  "name": "Z-Image Turbo",
  "kind": "image",
  "backend": "mflux",
  "order": 40,
  "about": "Z-Image Turbo, 6B, Apache 2.0: text to image in 9 steps …",
  "licence": "Apache-2.0",
  "size": "5.5 GB",
  "tasks": ["generate"],
  "source": {
    "repo": "filipstrand/Z-Image-Turbo-mflux-4bit",
    "rev": "b3a8f31115a11f2f9e2fa0bfbc8d78dcc3e6568b",
    "files": ["transformer/", "text_encoder/", "vae/", "tokenizer/"]
  },
  "here": ["transformer/*.safetensors", "vae/*.safetensors", "text_encoder/*.safetensors"],
  "defaults": {"width": 1024, "height": 1024, "steps": 9, "quantize": -1},
  "runtimeArgs": {"mflux": "z-image-turbo"}
}
```

| Field | | |
|---|---|---|
| `id` | required | lower case, digits, `.`, `_`, `-`; unique; the name in every API (changing it breaks clients) |
| `name`, `about` | required | as the pickers show them |
| `kind` | required | the backend's (table above) |
| `backend` | required | the runtime that runs it (table above) |
| `order` | | the place in the list among the models of its kind, small first (the pickers' order); the same order: by id. A machine's own models come after the catalogue's of their kind, in their order |
| `licence` | required | the weights' licence as the pickers show it; `nonCommercial: true` when it forbids commercial use |
| `gated` | | Hugging Face wants a token and an accepted licence |
| `size` | required | what a download fetches: bytes (`5905580032`) or `"5.5 GB"`, `"501 MB"` (GB = 2³⁰, MB = 2²⁰) |
| `memory` | | the GB its runtime takes at its peak, where the size misleads (streaming, offloading, quantising on load); the server routes with it (else size × 1.3), plus 6 GB for the rest of the machine |
| `tasks` | | mflux: `generate` (text to image), `edit` (needs reference images), `upscale`; none: generate and edit · cutout: `cutout` · music: `cover`, `repaint`, `extend`, `lego`, `extract`, `restyle` · speech: none, its family says them |
| `source.repo`, `source.rev` | one of `source` / `sources` | one Hugging Face repository and the commit (40 hex digits: pinned, a new revision is a new download) |
| `source.files` | | the file patterns it needs (a folder ends in `/`); none: the whole repository but `source.ignore` |
| `sources` | one of `source` / `sources` | several repositories, each `{key, repo, rev, include, optional}`: the runtime gets each by its key; `optional`: fetched when first used (Qwen3-TTS's Base and VoiceDesign); the model is here when the store has every file of the others |
| `here` | with `source` | the file patterns that must be there for the model to count as downloaded: in the store each with its `.sha256` (verified), in the Hugging Face cache as they are |
| `hereCache` | | other patterns for the Hugging Face cache; `[]`: the runtime does not take the model from there (LTX, the avatars) |
| `declaredSize` | | a downloaded model's size stays `size`, not the folder's (SeedVR2's folder holds both models; BiRefNet's what it loads) |
| `defaults` | | the request fields the UIs start with (`width`, `height`, `steps`, `guidance`, `quantize`, `scale`, …; docs/api.md) |
| `runtimeArgs` | | what the runtime needs to know of the model (table above); mflux also `loader` and `editLoader`: the mflux classes (`"module:Class"`) for text to image and for editing — a model with only one does only that · a generic runtime: `workerModel` |
| `params` | | a generic runtime's model: what it takes besides the request's fields ("Params" below) |

Unknown fields are an error, so a typo does not go unnoticed.

## Checks

`go test ./internal/engine` loads every manifest of the catalogue: they parse, ids are unique, kinds and backends
exist, revisions are pinned. `TestCatalogGolden` (testdata/catalog.golden.json) pins what users get of every model —
the API, the files and folders, the runtime's arguments, the recommendations; a model moved from Go into a manifest
must leave it as it is.

## Runtimes

A Python runtime is a manifest too: `internal/engine/runtimes/<name>.json` — its worker script (in
`internal/engine/workers/`), an optional build step, and one environment per platform. The packages differ by
platform (PyTorch's CUDA or ROCm wheels, MLX only on Apple silicon), so a worker offers a runtime's models only where
the runtime has an environment for its platform; the others are not listed (and the server routes their jobs to the
workers that have them).

```json
{
  "name": "cutout",
  "script": "birefnet_worker.py",
  "envs": {
    "darwin-arm64": {"python": "3.12", "packages": ["torch==2.14.1", "…"]},
    "linux-cuda":   {"python": "3.12", "packages": ["torch==2.14.1", "…"]},
    "linux-rocm":   {"python": "3.12", "packages": ["torch==2.14.1", "…"], "index": "https://download.pytorch.org/whl/rocm6.4"}
  }
}
```

| Platform | is |
|---|---|
| `darwin-arm64` | a Mac with Apple silicon (MLX, PyTorch on MPS) |
| `linux-cuda`, `linux-rocm`, `linux-cpu` | Linux with an NVIDIA GPU (`nvidia-smi`), with AMD's ROCm (`rocminfo`, `/opt/rocm`), with neither |
| `windows-cuda`, `windows-rocm`, `windows-cpu` | the same on Windows (ROCm: the HIP SDK's `HIP_PATH`) |

`MLXLAB_GPU=cuda|rocm|cpu` overrides what the worker finds. `index`: a package index besides PyPI, preferred where it
has a package (PyTorch's wheels for a GPU). An environment is installed once, by uv, into
`<config dir>/envs/<name>-<hash>`; the hash is of the Python version, the packages and the index, so a changed
environment is a new directory.

Today: every runtime runs on Apple silicon; BiRefNet also on Linux (CUDA, the CPU, and ROCm — not tried on a Radeon
yet). No Python runtime on Windows yet: the job protocol's events come on file descriptor 3, which Windows does not
pass to a child process. ComfyUI (an HTTP API) and the local servers are not affected.

### Generic runtimes: a new library without Go

A runtime with `"generic": true` needs no Go of its own: its models (`"backend": "<runtime>"`) run in the generic
backend, which speaks the job protocol with the worker script.

| Field | |
|---|---|
| `generic`, `kinds`, `output` | the kinds its models may be, and the extension of what it makes (`png`, `wav`, `mp4`, …) |
| `heavy` | its model takes so much memory that the queue unloads the other heavy ones first |

The script reads one JSON request per line on stdin and writes events as JSON lines on file descriptor 3 (`status`,
`progress` with `step`/`total`, `log`, `result` with `file`, `seconds`, `peakGB`, `width`, `height`, or `error` with
`text`; `ready` once it has started). The request is the job's request with the model's `defaults` under it, and:

| Key | |
|---|---|
| `model` | the model's `runtimeArgs.workerModel` (else its id) |
| `path` | the folder of the model's `source` (downloaded and verified) |
| `dirs` | the folders of its `sources`, by key |
| `params` | the model's params, checked and with their defaults |
| `output` | where to write the result |

### Params

A model of a generic runtime says what it takes besides the request's fields:

```json
"params": {"maxSide": {"type": "integer", "default": 4096, "minimum": 256, "maximum": 8192,
                       "description": "the longest side of the result"}}
```

`type` is `integer`, `number`, `string` or `boolean`; `minimum`, `maximum`, `enum`, `default` and `description` as in
JSON Schema. `/api/models` lists them (`params`), so a UI can make its form from them. A job's `params` are checked
when it is submitted — an unknown one, a wrong type or a value out of range is a 400 that names it, before anything is
loaded — and the defaults are filled in.

### A machine's own runtimes

`<config dir>/runtimes.d/<name>/runtime.json` with its script beside it adds a generic runtime to one machine,
without a release. It runs code nobody reviewed, so the worker reads `runtimes.d` only when it is allowed:
`"allowLocalRuntimes": true` in its `worker.json` (or `MLXLAB_ALLOW_LOCAL_RUNTIMES=1` for `ai-connector serve`). A
runtime the catalogue has already is left out.

`docs/examples/` has one that was tried: spandrel with Real-ESRGAN x4 (`runtimes.d/spandrel/`, `models.d/realesrgan-x4.json`)
— copy both into the config dir, allow local runtimes, and `realesrgan-x4` upscales images (512² to 2048² in about
5 s on an M2 Pro, after uv has installed the environment once).
