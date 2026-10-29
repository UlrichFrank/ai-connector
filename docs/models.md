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

What is covered so far: the image models of mflux (`backend: "mflux"`: FLUX.2 [klein], Z-Image Turbo, Qwen-Image,
SeedVR2). The other families follow; until then they stay in Go.

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
| `kind` | required | `chat`, `image`, `video`, `music`, `speech`, `avatar`, `sound`, `embed`, `classify` (so far only `image`) |
| `backend` | required | the runtime that runs it (so far only `mflux`) |
| `order` | | the place in the list among the models of its kind, small first (the pickers' order); the same order: by id. A machine's own models come after the catalogue's of their kind, in their order |
| `licence` | required | the weights' licence as the pickers show it; `nonCommercial: true` when it forbids commercial use |
| `gated` | | Hugging Face wants a token and an accepted licence |
| `size` | required | what a download fetches: bytes (`5905580032`) or `"5.5 GB"`, `"501 MB"` (GB = 2³⁰, MB = 2²⁰) |
| `tasks` | | an image model: `generate` (text to image), `edit` (needs reference images), `upscale`, `cutout`; none: generate and edit |
| `source.repo`, `source.rev` | required | the Hugging Face repository and the commit (40 hex digits: pinned, a new revision is a new download) |
| `source.files` | | the file patterns it needs (a folder ends in `/`); none: the whole repository but `source.ignore` |
| `here` | required | the file patterns that must be there for the model to count as downloaded: in the store each with its `.sha256` (verified), in the Hugging Face cache as they are |
| `sharedFolder` | | the repository's folder holds more than this model (SeedVR2 3B and 7B): its size stays `size`, not the folder's |
| `defaults` | | the request fields the UIs start with (`width`, `height`, `steps`, `guidance`, `quantize`, `scale`, …; docs/api.md) |
| `runtimeArgs` | | what the runtime needs to know of the model; mflux: `mflux` (mflux's name for it) |

Unknown fields are an error, so a typo does not go unnoticed.

## Checks

`go test ./internal/engine` loads every manifest of the catalogue: they parse, ids are unique, kinds and backends
exist, revisions are pinned. `TestCatalogGolden` (testdata/catalog.golden.json) pins what users get of every model —
the API, the files and folders, the runtime's arguments, the recommendations; a model moved from Go into a manifest
must leave it as it is.
