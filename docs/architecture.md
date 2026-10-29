# Architecture: hub, worker, clients

```
 iPhone app ─┐
 web UI ─────┼── HTTPS ──► hub (Raspberry Pi, k0s, mlxlab.example.org)  ◄── WebSocket+yamux ── worker (Mac)
 TUI ────────┘             sign-in, web UI, proxy                        dialled by the worker   models, jobs, history,
                                                                                                  files, prompts
```

* **worker** (`mlxlab worker`, Mac): the server as it was — job queue, history, outputs, prompts, models (Go MLX
  and the Python workers). It serves the API (`internal/web`, without auth) **only through the tunnel**: it dials
  the hub (`internal/tunnel`), so no port opens on the Mac. Media stays on the Mac. `mlxlab serve` still runs
  it standalone on 127.0.0.1 for development (no hub, no auth, local only).
* **hub** (`mlxlab-hub`, linux/arm64 image `pharndt/mlxlab-hub`, `cmd/mlxlab-hub`): does not import the engine.
  It serves the web UI (React build, `internal/webui`), signs people in, and forwards every other `/api/*`
  and `/files/*` request to the worker through the tunnel. While no worker is connected it answers those with
  `503 {"error":"offline","message":"The Mac is offline"}`.
* **clients**: the web UI (served by the hub), the TUI (`mlxlab tui`, a client of the hub API) and the iOS
  app, all over HTTPS to the hub.

## Hub API (its own endpoints; everything else is the worker's API, unchanged)

| | |
|---|---|
| `GET /healthz` (no auth) | `200 ok` (text/plain) while the server serves HTTP; no state, no worker needed (Kubernetes probes, see docs/deploy.md) |
| `GET /api/info` (no auth) | `{"app":"mlxlab","role":"hub","name":"mlxlab","signedIn":bool,"user":"…","owner":bool,"canInvite":bool,"worker":{"connected":bool,"id":"…","name":"…","since":"RFC3339"},"workers":[…]}` (`worker`: the primary Mac, see "Several Macs"; `workers`: the connected ones as in `GET /api/workers`) |
| `POST /api/login` (no auth) | body `{"password":"…","device":"Philipp's iPhone","user":"Philipp"}` (or `"invite"` instead of `"password"`, see below) → `{"token":"…","device":{…}}`; also sets cookie `mlxlab_session` (HttpOnly, Secure when TLS, SameSite=Lax, 1 year). Wrong password: 401 (and a short delay). |
| `POST /api/logout` | revokes the token used (cookie cleared) |
| `GET /api/devices` | `[{"id","name","user","created","lastSeen","current":bool,"owner":bool,"canInvite":bool,"invitedBy"}]` |
| `PATCH /api/devices/{id}` | owner devices only, see "Signing in with a QR code" |
| `POST /api/invites` | a QR code, see "Signing in with a QR code" |
| `DELETE /api/devices/{id}` | revokes that device's token |
| `GET /api/worker/connect` | the worker's tunnel (`Authorization: Bearer <worker token>`), see `internal/tunnel` |
| `GET /api/worker/events` | SSE: `data: {"connected":bool,"id":"…","name":"…","since":"…","workers":[…]}` at once and on every change (the old fields: the primary) |

**Auth**: every request except `/healthz`, `/api/info`, `/api/login`, `POST /api/link/start` and `/token`, `/pin`, the worker tunnel, `/tools/mlxlab/…` (a worker token
or a device token, see below) and the static UI needs a device
token: `Authorization: Bearer <token>`, the `mlxlab_session` cookie, or `?token=<token>` (for players and
`EventSource` that cannot set headers). Without one: `401 {"error":"signin"}`. Tokens are random 32 bytes
(base64url), stored hashed (SHA-256) in `<data>/devices.json`.

**Configuration** (environment): `MLXLAB_PASSWORD` (sign-in password), `MLXLAB_WORKER_TOKEN` (optional: the
legacy shared worker token, see below), `MLXLAB_DATA` (default `/data`: devices.json, workers.json),
`MLXLAB_ADDR` (default `:8080`), `MLXLAB_TOOLS` (default `/tools`: the Mac program), `MLXLAB_CONSOLE_HOSTS`,
`MLXLAB_PUBLIC_URL` (the address for people and apps, e.g. `https://ai.example.org`; default: each request's
origin), `MLXLAB_MDNS` (Bonjour, default on; `0`/`false`/`off` turns it off) and `MLXLAB_MDNS_NAME` (default
`AI Connector`); see docs/api.md. In k8s: sops secret.

## Worker

`mlxlab worker [--hub https://mlxlab.example.org]` — the hub URL and the worker token come from flags, the
environment (`MLXLAB_HUB`, `MLXLAB_WORKER_TOKEN`) or `~/.config/mlxlab/worker.json`
(`{"hub":"…","token":"…"}`; written by `mlxlab worker --hub URL` with `--token T` or `MLXLAB_WORKER_TOKEN`).
It reconnects with backoff and prints its state; a refused token (revoked) stops it. `mlxlab version` prints the
build. The TUI is no longer part of the worker.

**Settings in `worker.json`** besides hub and token (kept when the file is written again):

| Key | | Environment (wins; also for `serve`) |
|---|---|---|
| `idleUnload` | a model with no job for this long leaves memory: `"10m"` (the default), `"1h"`, `"0"` never | `MLXLAB_IDLE_UNLOAD` |
| `pin` | models that stay loaded however long they wait, e.g. `["qwen3.6-35b-a3b"]` (the chat model without a cold start) | `MLXLAB_PIN` (comma list) |
| `allowLocalRuntimes` | read `<config dir>/runtimes.d`: Python runtimes of this machine's own (docs/models.md) | `MLXLAB_ALLOW_LOCAL_RUNTIMES=1` |

Unloading after idle time (`jobs/idle.go`): the worker looks every quarter of the idle time (at most every 30 s);
a backend whose model has had no job for that long is unloaded — its Python worker or server ends, the memory goes
back to the machine — unless the model is pinned or the queue's running job is on it.

**Starts at login** (`mlxlab worker --install`; `--uninstall` stops and removes it): hub and token as above (flags,
environment or the file; none → "--install needs a hub and a token …") are written to the config file (0600), and
the service runs `ai-connector worker` with no token on its command line or in its file — only `--config FILE` and
`--state DIR` when they were given (absolute). macOS: launchd agent
`~/Library/LaunchAgents/de.rnd7.ai-connector.worker.plist` (RunAtLoad, KeepAlive, log
`~/Library/Logs/ai-connector-worker.log`); Linux: systemd user unit `ai-connector-worker.service` (log `journalctl
--user -u ai-connector-worker`); Windows: scheduled task "ai-connector worker", else the Run key (started now too). The
kept environment and the rest are `ai-connector vboard --install`'s (docs/api.md, "Starts at login"; install.go,
`serviceKind`). A config file other than the default one gets names of its own from its file name (`workerSuffix`:
lower case, a leading `worker-` dropped; `worker-local.json` → `de.rnd7.ai-connector.worker.local`,
`ai-connector-worker-local.service`, "ai-connector worker local", `ai-connector-worker-local.log`; a name that says
nothing, e.g. `worker.json` elsewhere → the first 8 hex digits of the SHA-256 of its absolute path), so two workers
on one Mac have two services. **One per config file:** a worker holds an OS lock on the file's `.lock` sibling
(`worker.json` → `worker.lock`, PID inside, instance.go); a second one with the same file says "another mlxlab
worker runs (pid N) with …" and waits, so a terminal copy and the service never dial with the same token;
`--install` stops the running copy (SIGTERM, up to 10 s) before the service starts.

**Config file and state dir (several workers on one Mac).** `--config <file>` / `MLXLAB_WORKER_CONFIG` names the file
with hub and token (default `<config dir>/worker.json`). `--state <dir>` / `MLXLAB_STATE` (also for `serve` and `run`)
is where the process keeps what is its own; the rest of the config dir (`MLXLAB_HOME`, `~/.config/mlxlab`) is shared:

| own (`--state`, default: the config dir) | shared (the config dir) |
|---|---|
| `history.json`, `outputs/` (incl. uploads) — the job manager's dir | `models/` (and `verified.json`), `envs/`, `lib/`, `tools/`, `bin/` |
| `prompts.json`, `approvals.json` | `mcp.json`, `logs/` (the MCP servers' logs) |
| `agents/` (Claude Code sessions, `resume.json`), `agents.json` | `voices/`, `styles/` (the libraries; `web.Server.Home`) |

A Claude Code session of such a worker may read neither its state dir's files nor the shared config dir's
(`agents.Manager.Shared`, guarded like `Home`: the other worker's `worker.json` holds its token). Two workers on one
Mac share the memory: each loads its own copy of a model it runs. The web sandbox's VMs have fixed names: only one of
the workers should run web-search chats at a time.

## TUI

`mlxlab tui [--server https://mlxlab.example.org]` — a client of the hub API (`internal/client`). The first time it
asks for the password and keeps the device token in `~/.config/mlxlab/client.json`
(`{"server":"…","token":"…"}`). The server is `--server`, else the saved one, else `$MLXLAB_SERVER`; there is no
built-in default. `mlxlab tui --server http://127.0.0.1:8484` works against `mlxlab serve` (no sign-in needed there).

## Connecting a Mac from the web UI (like vboard's local AI)

Each Mac (worker) gets **its own token**, made in the web UI (Devices → "Connect a Mac"); the shared
`MLXLAB_WORKER_TOKEN` still works (the first Mac, set up before this existed) but is optional.

Hub API (device sign-in required):

| | |
|---|---|
| `GET /api/workers` | `[{"id","name","created","lastSeen","route","connected":bool,…}]` (`connected`: on the tunnel now; more fields in "Several Macs") |
| `POST /api/workers` | body `{"name":"Mac Studio"}` → `{"worker":{…},"token":"…"}` — the token is shown once (stored hashed in `<data>/workers.json`, like devices) |
| `DELETE /api/workers/{id}` | revokes it; if it is connected, the tunnel is closed |

The tunnel (`/api/worker/connect`) accepts an enrolled worker's token (or the legacy shared one) and knows which
worker is connected (`/api/info` and `/api/worker/events` gain `"id"` next to `name`; `name` is the enrolled
name, the Mac's host name for the legacy token, whose id is `"legacy"`). Connecting updates the worker's `lastSeen`.
Every worker stays connected at once (see "Several Macs"); a newer connection of the *same* worker replaces its
older one.

The program: `GET /tools/mlxlab/{platform}` serves the worker binary of the same build as the server for
darwin-arm64, linux-amd64, linux-arm64 and windows-amd64 (cross-compiled without cgo in the `Dockerfile`, kept at
`/tools/mlxlab/<GOOS-GOARCH>` in the image, `MLXLAB_TOOLS` = the folder). `{platform}` may be Go's name or what
`uname -s`-`uname -m` prints (`Darwin-arm64`, `Linux-x86_64`, `Linux-aarch64`; Windows `Windows-AMD64`).
It needs `Authorization: Bearer <worker token>` (enrolled or legacy) or a device token (bearer, cookie, `?token=`);
without: 401. Other platforms → 404 with a message (whatever the token).

The command the web UI shows (one paste; the token in the environment, never as an argument):

```sh
mkdir -p ~/.config/mlxlab/bin \
  && curl -fsSL -H 'Authorization: Bearer <token>' -o ~/.config/mlxlab/bin/mlxlab '<hub>/tools/mlxlab/'"$(uname -s)-$(uname -m)" \
  && chmod +x ~/.config/mlxlab/bin/mlxlab \
  && MLXLAB_WORKER_TOKEN='<token>' ~/.config/mlxlab/bin/mlxlab worker --hub '<hub>'
```

`mlxlab worker --hub URL` with the token from `MLXLAB_WORKER_TOKEN` saves both to `~/.config/mlxlab/worker.json`,
so later `~/.config/mlxlab/bin/mlxlab worker` is enough, and `… worker --install` starts it at every login.

## Signing in with a QR code, users

Every sign-in asks for a **user name** (who uses this device: "Philipp", "Anna"); devices carry it and the
Devices page groups by it. Two ways in:

1. **Password** (as before) — `POST /api/login {"password","device","user"}`. Such a device is an *owner* device.
2. **QR code** — a signed-in device that may invite shows a QR code; a new iPhone scans it and is in, no password.

Invites:

* `POST /api/invites` (owner devices, or devices with `canInvite`) → `{"code":"…","url":"mlxlab://signin?hub=<hub
  url>&code=<code>","expires":"RFC3339"}` — a one-time code (≥ 128 bits, base64url), valid 10 minutes, stored hashed.
  The QR code holds `url`. The iPhone app registers the `mlxlab://` scheme (the Camera app opens it) and also has a
  scanner on its sign-in screen.
* `POST /api/login {"invite":"<code>","device","user"}` → `{token, device}` like a password sign-in; the code is used
  up. Wrong/expired/used → 401 `{"error":"invite"}` (rate-limited like passwords).
* Devices: `{"id","name","user","created","lastSeen","current","owner":bool,"canInvite":bool,"invitedBy":"<device id>"}`.
  `PATCH /api/devices/{id} {"canInvite":bool,"user"?:"…"}` — owner devices only; this is the **toggle "may sign in
  other iPhones"** (off by default for invited devices; owners can always invite).
* `GET /api/info` gains `"user"`, `"owner"`, `"canInvite"` for the asking device (`""`/`false` when signed out;
  `canInvite` is what the device may do, so always `true` for an owner).

Details (as built):

* `user` is **required** on every sign-in: trimmed, cut to 40 characters; missing or blank → `400 {"error":"user"}`
  (checked before the password or the code, so a missing name does not use an invite up). There is no grace
  period for clients without `user`: the web UI and the iOS app send it from this release on, and so must the TUI
  (`internal/client`).
* An invite is checked when it is used: it must exist, not be expired or used, and the device that made it must
  still exist and still be allowed to invite (turning `canInvite` off, or revoking the device, voids its pending
  codes). Invites are kept in memory only (hashed, SHA-256): a hub restart drops pending codes, which costs a new
  QR code at most. Expired ones are purged whenever an invite is made or used; at most 100 are pending
  (more → 429). Password and invite sign-ins share one rate limit per client IP (10 a minute, then 429
  `{"error":"rate"}`); a wrong code waits like a wrong password.
* `url`'s `hub` is the hub's public origin as the client sees it: `X-Forwarded-Proto` and `X-Forwarded-Host` behind
  traefik (first entry of each), else the request's own scheme and `Host`; it is URL-encoded
  (`mlxlab://signin?hub=https%3A%2F%2Fmlxlab.example.org&code=…`).
* `POST /api/invites` from a device that may not invite → `403 {"error":"forbidden"}`; `PATCH /api/devices/{id}`
  from a non-owner (its own device too) → `403 {"error":"forbidden"}`, unknown id → 404, blank `user` → 400
  `{"error":"user"}`; it answers with the changed device. Both fields are optional. `canInvite:false` on an owner
  device is ignored (owners always invite; owner devices report `canInvite: true`).
* Revoking a device leaves the devices it invited alone (their `invitedBy` then names a device that is gone);
  only its pending invites stop working.
* `devices.json` from before: entries without `"owner"` all came from the password, so they become owners
  (`owner`/`canInvite` true, `user` `""`, `invitedBy` `""`); the file is rewritten in the new shape on start.
  An owner can set their `user` with `PATCH`.

## Several Macs (workers) at once

The hub keeps **every** enrolled worker connected (no more "a newer one replaces the older"), learns what each can
do, and sends each job to the best one. Each Mac keeps its own jobs, files, history — the hub merges them.

**Specs** — the worker's `GET /api/info` (through the tunnel) adds
`{"specs":{"chip":"Apple M2 Ultra","ramGB":64,"gpuCores":60,"macOS":"27.0.1","os":"macOS","osVersion":"27.0.1",
"arch":"arm64","gpu":"","vramGB":0,"version":"…"},
"state":{"running":"<job id>"|null,"queued":3,"loaded":{"<backend>":"<model id>"}}}`; the hub reads it on connect
and keeps the state current from the worker's own event stream (job state changes) plus a refresh every 30 s.

(`os` is macOS, Linux or Windows; `gpu`/`vramGB` a discrete NVIDIA GPU through `nvidia-smi`; `macOS` stays for
older UIs. Linux reads `/proc` and `/etc/os-release`, Windows the registry and `GlobalMemoryStatusEx`.)

**Workers differ in what they run** (Linux and Windows have no MLX; a local runtime's models are its own machine's):
a model whose id some worker lists goes only to the workers that list it, and `GET /api/models` names only those
under `workers`.

**Job ids and files are unique across Macs**: on connect the hub tells the worker its short route id (header
`X-Mlxlab-Route: <rid>` on every proxied request, e.g. `m1`); the worker then answers with job ids `<rid>.<id>` and
file urls `/files/<rid>/…`, and strips its own prefix from incoming ids/paths (requests without the header —
`mlxlab serve` — stay unprefixed). Every job carries `"worker":{"id","name"}`.

**Routing** (hub):

* `GET /api/jobs`, `/api/events` (SSE): merged from all connected workers.
* `GET /api/models`: the union; each model gets `"workers":[{"id","name","here":bool,"loaded":bool,"fits":bool}]`
  and is listed if any worker has it. `fits`: the worker's RAM is enough for the model (`size` × 1.3 + 6 GB
  headroom; music/video/avatar runtimes: their documented peak where known).
* `POST /api/jobs`: `request.worker` pins a worker; a follow-up (`parent`) and studio tasks on a song (`source`,
  `parent`) go to the worker holding it; otherwise the hub picks among connected workers that fit: model loaded
  > on disk > needs a download; then idle > fewer queued; then more RAM. None fits → 409 `{"error":"no-worker",
  "message":"No connected Mac has enough memory for <model> (needs ~N GB)"}`; none connected → 503 offline.
* Everything addressed to a job (`/api/jobs/{id}/…`, `DELETE`, transcript, approve, cover, star …) and
  `/files/<rid>/…` goes to the worker in the prefix.
* Per-Mac libraries (prompts, voices, approvals, tools, MCP): served from the **primary** worker (the one with
  the most RAM among the connected; `PUT /api/workers/{id}/primary` pins one); writes go to all connected workers
  (prompts, approvals) so they stay alike. Voices stay on the Mac that made them (listed per worker).
* `GET /api/workers` adds `specs`, `state`, `primary`; `/api/worker/events` sends the list of connected workers.

As built:

* **Tunnel** (`internal/tunnel`): one yamux session per worker id (enrolled id, or `legacy` for the shared token);
  `Connected()` lists them, `Disconnect(id)` closes one, `Transport()` dials the worker named by the URL host
  `tunnel.Host(id)`.
* **Route ids** are given at enrollment and kept in `workers.json` (`"route":"m1"`, `m2`, …; entries from before get
  one on start, in the order they were made), so links stay valid across restarts; the legacy worker is `m0`. The
  hub sends `X-Mlxlab-Route: <rid>`, `X-Mlxlab-Worker-Id` and `X-Mlxlab-Worker-Name` (URL-encoded) on every request
  to a worker that understands routes (a client's own such headers are dropped).
* **Worker side** (`internal/web/route.go`, one layer around every handler): the path (`/api/jobs/<rid>.<id>/…`,
  `/files/<rid>/…`) and a JSON body (`/files/<rid>/` file urls, `parent`/`coverFor` ids) lose the prefix;
  a JSON answer and every `data:` line of an event stream get it: a job's `id`, `request.parent`,
  `request.coverFor`, an update's `jobId`, and the **file urls** — the strings of fields that hold them (`files`,
  `file` (a `file` event), `preview`, `cover`, `images`, `image`, `audio`, `source`, `url`, `sample`, `video`,
  `poster`; also in lists, e.g. a tool's `files`) and any other string that is a `/files/` url as a whole — plus
  `"worker":{"id","name"}` on every job. **Free text is never rewritten**: `text` (a chat's answer, `text` events),
  messages' `content`, `prompt`, `logs`, `status`, `error`, a tool's `arguments`, `summary` and the like, and
  everything inside them, stay as written — a chat answer that mentions `/files/x` reaches an OpenAI client as
  `/files/x`. mlxlab's web app adds the job's route to `/files/` links in an answer itself (`web/src/lib/files.ts`). `/api/info` adds `"routes":true` (the capability), `specs` (sysctl `machdep.cpu.brand_string` and
  `hw.memsize`, `ioreg` `gpu-core-count`, `sw_vers`; asked once) and `state` (`running` with the prefix).
  `MLXLAB_FAKE_RAM_GB` / `MLXLAB_FAKE_CHIP` pretend other specs, for trying the routing on one Mac.
* **An older worker** (no `"routes":true`, or no `/api/info`): alone it gets everything unchanged, as before; next to
  a newer one it is left out (listed with `"routes":false`).
* **State**: the hub probes `/api/info` and `/api/models` on connect, follows each worker's `/api/events` (one stream
  per worker, shared by every client; reconnected after a second), probes again ~0.3 s after a job update and
  every 30 s.
* **Merged `/api/events`**: when the set of connected Macs changes, the stream ends, so clients connect again and
  reload the lists (with the new Mac's jobs, without the gone one's).
* **`fits`**: RAM ≥ need, need = (`size` × 1.3, or the known peak) + 6 GB. Known peaks (hub `route.go`): LTX-2.5 q4
  and the LTX avatar ~16 GB, LTX q8 ~45 GB, LTX HQ q8 ~34 GB (measured 30 GB at 512×320) and q4 ~26 GB, LongCat q8 ~37 GB, ACE-Step ~10 GB, FLUX.2 klein 4B ~15 GB, Qwen-Image
  2512 ~42 GB (Edit 2511 ~44, 2.1 ~30), Qwen3.6 35B ~19 GB. A worker that reports no RAM fits everything.
* **`POST /api/jobs`** order: what the job works on first — `parent`, `coverFor`, `source` (its Mac; offline → 503)
  and a library `voice` (the Mac that has it) — then `request.worker` (a worker id; removed before forwarding),
  then the score: model loaded > on disk > download; holds the request's `images`/`audio`; idle; fewer queued;
  more RAM; the primary. `images`/`audio` on another Mac than the chosen one are copied there (download from the
  one, `/api/upload` to the other) and the request names the copy.
* **Libraries**: prompts, approvals, tools and anything else not listed here go to the primary; `POST /api/prompts`,
  `DELETE /api/prompts/{id}`, `PUT`/`DELETE /api/approvals/{tool}` go to the primary, then to the others (a new
  prompt with the id the primary gave it); the primary's answer is returned. `POST /api/folders/rename` and
  `POST /api/unload` go to all (`moved` summed). `GET /api/voices` lists every Mac's voices (each with `worker`) and
  the primary's presets; `PATCH`/`DELETE /api/voices/{id}` go to the Mac that has it; `POST /api/voices` and
  `POST /api/upload` to `?worker=<id>`, else the primary.
* **Primary**: `PUT /api/workers/{id}/primary` pins (kept in `<data>/primary.json`), `DELETE
  /api/workers/{id}/primary` goes back to automatic (most RAM, then the longest connected). `GET /api/workers`
  items: `{"id","name","created","lastSeen","route","connected","legacy"?,"since"?,"primary","pinned"?,"routes",
  "specs"?,"state"?}`; the legacy worker is listed while it is connected.

## Train your own style (music LoRAs)

Upload tracks you own the rights to (your own productions), train a **LoRA** of a song model on them, then pick it
as a style when making songs. Trainable: **Stable Audio 3 Medium** (first choice: the best songs) and ACE-Step 1.5
(turbo, XL turbo, base, XL base); every other music model refuses a style with a clear error (a style also only works
with the model it was trained for).

**How it works** (feasibility measured on an M1 Max, 64 GB):

* **Stable Audio 3 Medium** trains with the runtime's own pure-MLX trainer (`optimized/mlx/scripts/
  lora_train_mlx.py` at the pinned commit — underfit's conventions: DoRA-rows rank 16, alpha = rank, on the **base**
  rectified-flow DiT `MLX/dit_medium-base_f16.npz` of the same pinned repository, fetched by the store on the first
  training, 2.9 GB; random crops of each track, CFG dropout 0.1, AdamW 1e-4, grad-norm clip 1.0, gradient
  checkpointing). `workers/train_sa3.py` drives it in its own runtime `sa3train` (the song runtime's packages +
  `soundfile`, `tqdm`), parses its progress, sends SIGINT for a pause (it writes a checkpoint and a last sample) and
  resumes from the newest checkpoint. SA3 has no captioner: every track's caption is the style's **description**
  (tags the user writes) plus its tempo, measured once with the tempo helper (librosa) — `"<description>, 138 BPM"`,
  the trigger word in front 80 % of the time. Samples are ARC renders (the shipped model with the LoRA merged, 8
  steps) at step 0 ("before"), every N steps and at the end. A step of a 30 s crop: **~1.1 s**, ~12 GB peak; encoding
  2 min of audio ~6 s; a 1000-step style ≈ 20 min. At inference the song worker (`workers/stableaudio3.py`) loads the
  DiT with the LoRA merged (`load_dit(lora_paths=…)`) and applies its seconds-conditioner delta; another style or
  strength loads the DiT again (a few seconds).
* **ACE-Step training** runs ACE-Step's own LoRA code from the pinned package (`acestep.training_v2`: PEFT LoRA on the DiT
  decoder's q/k/v/o projections, flow-matching loss with logit-normal timesteps and CFG dropout) on **PyTorch/MPS**
  in the existing `acestep` runtime — ACE-Step has no MLX trainer. bf16 model, float32 LoRA weights and AdamW
  state, gradient checkpointing. A step on one 30 s segment: ~1.9 s for the 2B turbo with the GPU to itself (2.4 s
  while other jobs share it), ~7 GB peak; the 4B XL is estimated at ~2× (not measured end to end). 1000 steps
  ≈ 35 min for the turbo.
* **Captions**: each segment is auto-labelled by the 5 Hz LM (`understand_audio_from_codes`, ~10–15 s a segment);
  tracks marked "with vocals" get the LM's lyric transcription, the rest train as `[Instrumental]`. The captions
  start with the style's **trigger word** (a made-up token like `deep-trance-x7k2`).
* **Using an ACE-Step style**: the pinned ACE-Step applies LoRAs to its PyTorch decoder only; mlxlab diffuses on the MLX copy,
  so the worker **merges** the PEFT LoRA into the MLX DiT's weights (`W += scale·alpha/r·B·A`, in float32, cast
  back to bf16) and keeps the originals of the touched layers: another style, another strength or none restores
  them first, without reloading the model (~0.5 s; a song with and without a style at the same seed is identical
  after the restore). Request shape: `dirs.lora` = the LoRA folder, `"lora": {"file", "scale"}` (the same shape
  the community techno LoRA model uses). Community LoRAs load the same way (tested: ryanontheinside's
  techno-acestep1.5-xl-v1 on the XL turbo, trigger `roti-t3kn0`).

**Library** (`internal/styles`): `<config dir>/styles/<id>/` — `style.json` (name, base model, trigger, strength,
tracks with hash/name/length, settings, status, progress with losses and samples, captions), `tracks/<hash>.wav`
(the uploads converted to 48 kHz stereo — the only copy), `work/segments/` (tracks cut into segments: full
`segmentSeconds` windows, plus one ending at the track's end when ≥ a third is left; ≤ 30 per track; ACE-Step
only — Stable Audio 3 takes random `segmentSeconds` crops of the whole tracks: `work/audio`, `work/latents`,
`work/runs/…/checkpoints`, `lora/style.safetensors`),
`work/captions.json`, `work/tensors/*.pt` (VAE latents and text embeddings, reused), `work/checkpoint/` (adapter +
`state.pt`: optimiser, step, RNG), `samples/step-N.wav`, `lora/` (PEFT `adapter_config.json` +
`adapter_model.safetensors`). Defaults: 1000 steps, rank 64 / alpha 128, lr 1e-4 (cosine, 50 warmup steps),
30 s segments, a checkpoint every 50 steps, a 12 s sample every 250 and at the end (Stable Audio 3: rank 16 /
alpha 16, no warmup). A style has a `description` (PATCH; required before a Stable Audio 3 training) and each track
its measured `bpm`.

**Training job**: a music job, `{"model": <base>, "task": "train-style", "style": <id>}` (heavy: one at a time, it
blocks the queue; other jobs wait). It cuts the segments (ffmpeg), stops the song workers, and runs
`workers/train_acestep.py` (it imports `acestep_worker.py` for loading, the merge and the samples). Events:
`train` (step, total, loss, lr, s/step, eta), `checkpoint`, `sample`, `captions`; the backend writes them into
`style.json` and the job's progress. **Cancel = pause**: the trainer gets `{"stop": true}`, writes a checkpoint at
the next step and ends (killed after 3 min); the style is `paused`, the next training resumes from the checkpoint
(same rank/alpha/model; else it starts over). A worker that stopped mid-training shows the style as `paused`.

**API** (worker; the hub merges `GET /api/styles` over every Mac — each style with its `worker` — and sends
everything addressed to a style, and every job with `style`, to the Mac that has it; `POST /api/styles` goes to
`?worker=<id>`, else the primary):

| | |
|---|---|
| `GET /api/styles` | `{"styles":[style],"models":[{"id","name","here","stepSeconds","described","defaults"}],"defaults":settings,"community":[…]}` (`described`: its styles need a description; `community`: the curated Hugging Face LoRAs, docs/music.md) |
| `POST /api/styles` | `{"name","baseModel"}` → style (`baseModel` must have `"styles":true` in `/api/models`); `{"repo","rev"?,"trigger"?,"baseModel"?,"name"?}` → a community style (docs/music.md) |
| `GET /api/community-styles` | the curated list; `?repo=owner/name[@rev]` what that LoRA repository says about itself (hub: `?worker=<id>`, else the primary) |
| `GET`/`PATCH`/`DELETE /api/styles/{id}` | PATCH `{"name"?,"description"?,"strength"? (0..2),"settings"?}`; settings, tracks and delete refused (409) while it trains |
| `POST /api/styles/{id}/tracks` | multipart, one or more `file` (+ `vocals=1`): converted, de-duplicated by hash |
| `DELETE /api/styles/{id}/tracks/{track}` | |
| `POST /api/styles/{id}/train` | `{"steps"?, "settings"?, "fresh"?}` → `{"style","job"}` (resumes unless `fresh`) |

A style as served: the fields of `style.json` plus `seconds`, `segments`, `estimate` (seconds a training — or what
is left of it — takes), `tracks[].url`, `samples[{step,url}]`, `losses[{step,loss}]` (≤ 2000 points), `jobId`,
`running` (the job while queued/running). Audio is under `/files/styles/<id>/…` (audio files only, not the weights).
Song requests take `style` and `styleStrength` (0: the style's strength); the trigger word is added to the prompt.
A **community style** (a LoRA from Hugging Face, `source` set) is a style too, with no tracks and no training (train
and tracks refused): see docs/music.md, "Styles: trained and community".

**Web UI**: Music → the palette button ("Your styles", `#/music/styles/<id>`): create, drop tracks (with the note
that only music you own or are licensed to use belongs there), segments and length, steps and advanced settings
with a time estimate, live progress (step, loss, s/step, ETA, the GPU-busy note), loss chart, sample players,
pause/resume/start over, rename, delete, default strength. New song: a "Your style" picker with strength for a
model that takes styles.
