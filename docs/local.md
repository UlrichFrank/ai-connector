# Local integration: vboard + ai-connector on this Mac

One command starts everything vboard's chat panel needs to offer several local AIs on this Mac: the ai-connector
server, this Mac as its worker, a vboard built from your checkout, and a vboard connector with its agents — a local
chat model (Qwen on this Mac) and Claude Code with your own subscription.

```bash
make local-up       # build what changed, start, connect, print the URLs and the console password
make local-status   # processes, worker, connector and the agents vboard is offered
make local-logs     # tail the logs (deploy/local/local.sh logs server|worker|vboard|idp for one)
make local-open     # the console and the board in the browser
make local-down     # stop what local-up started; the data stays
```

`make local-up` is idempotent: run it again after a change or a reboot — it rebuilds what changed (ai-connector at
another commit or with local changes, the vboard checkout at another commit), restarts what was rebuilt and starts
what does not run. Tokens, the worker's enrollment, the vboard token and the board are made once and reused.

### Running chats are not cut off

Restarting the server, the worker or vboard ends a chat that is running in vboard ("The connection to vboard was
lost"; a Claude Code turn can take minutes). So before `local-up` restarts anything — and before `local-down` stops
anything — it checks for running work: jobs on the worker that run or are queued (`GET /api/jobs`) and Claude Code
turns that are busy (`GET /api/agents/sessions`). When there is some, it lists it (model, client e.g.
`vboard: This Mac (local)`, how long) and waits, checking every 2 s with a progress line, until nothing runs; then
it restarts (or stops).

```bash
make local-status                            # "work  1 running (longest 2:10: claude-code/sonnet for vboard: …)"
make local-up                                # builds first, then waits (up to 10 min) and restarts what changed
WAIT_JOBS=1800 make local-up                 # wait up to 30 min (seconds)
FORCE=1 make local-up                        # restart now, ending what runs (also FORCE=1 make local-down)
NO_RESTART=1 make local-up                   # build only: says what runs an older build, restarts nothing
```

- The builds run first, the wait comes after them; only what was rebuilt is restarted. When nothing would be
  restarted there is no check at all, and when nothing runs the check is one quick request.
- Ctrl-C during the wait aborts: nothing was restarted or stopped. When the wait times out it stops the same way
  and says how to force it.
- A build that was not restarted (timeout, Ctrl-C, `NO_RESTART=1`) is remembered: the next `local-up` restarts the
  processes that still run the older build (`<state>/build-*.id` against `<state>/run/<name>.build`).
- Tests: `deploy/local/local_test.sh` (the check and the wait, with the API faked). `LOCAL_FAKE_BUSY=<seconds>`
  makes the script see a made-up running job for that long (to try the wait), `LOCAL_POLL` sets the poll interval.

## What runs

| | address | started as |
|---|---|---|
| ai-connector server (console, API) | http://localhost:18484/console/ (`/` redirects there) | `bin/ai-connector-server`, `MLXLAB_CONSOLE_HOSTS=localhost`, data in the state dir |
| this Mac as worker | — (dials the server) | `bin/ai-connector worker --config <state>/worker.json --state <state>/worker` |
| vboard | http://localhost:18480 | `vboard` built from `~/dev/vboard` (`backend/cmd/vboard`, its web app copied next to it), SQLite |
| vboard's test login | http://localhost:18482 | `vboard test-idp`, offering one person: you |

Everything listens on 127.0.0.1 only. The ports: `AIC_LOCAL_PORT`, `VBOARD_PORT`, `VBOARD_IDP_PORT`.

**The person.** The first word of `git config user.name` (else `$USER`), e.g. `Philipp` — `LOCAL_USER=…` for
another. The script signs a device in to the server with that user name, so the worker it enrolls, the connector
and the Claude Code jobs are owned by that person (Claude Code runs only on its owner's workers, with their
subscription). vboard's test login offers exactly that person (full name from git), as admin.

**The worker** keeps its hub and token in `<state>/worker.json` and its own jobs, outputs and Claude Code sessions in
`<state>/worker` (`--config`, `--state`: README "Worker and hub"). The model store stays `~/.config/mlxlab` — shared
with the mlxlab worker that serves the hub on the Pi, nothing is downloaded twice; `~/.config/mlxlab/worker.json`
is neither read nor written. Both workers can run at the same time; each loads its own copy of a model it runs
(memory).

**The connector** (console → Connectors, "This Mac (local)") dials vboard with your vboard token and offers, in
this order:

1. the local chat model — `qwen3.6-35b-a3b` when it is on this Mac (LM Studio's copy counts), else the first chat
   model `bin/ai-connector list` shows as here; `LOCAL_CHAT_MODEL=…` picks another (a model that is not on the Mac
   needs `ALLOW_DOWNLOAD=1`: nothing large is downloaded silently);
2. `lmstudio/qwen3.6-35b-a3b` — the same Qwen with **images** (screenshots in the chat), through LM Studio: the
   built-in MLX engine runs Qwen text-only. `up` starts LM Studio's server when it is off (`down` stops it again only
   then); LM Studio loads the model on the first image question (~20 GB more memory next to the built-in copy, freed
   by LM Studio when idle). `LMSTUDIO_VISION_MODEL=<LM Studio id of a vlm>` picks another, `LMSTUDIO_VISION=0` leaves
   it out. LM Studio ignores "thinking off", so this agent always thinks first;
3. `claude-code/opus`, `claude-code/sonnet` and `claude-code/haiku`, when `claude` is installed — they work on the board themselves
   through vboard's MCP, and read images too.

## Using it

1. `make local-up`; it prints the console URL with the password and the board's URL.
2. Open the board, sign in with **Local login** → your name.
3. The board's AI menu → **Chat with your local AI** (⇧I); pick the agent at the top of the panel (Qwen, Claude Code
   (Sonnet), Claude Code (Haiku)).

The first question to Qwen loads the model (a few seconds from disk) and reads vboard's prompt with its tools
(~14k tokens, about 35 s on an M2 Ultra; later turns of the conversation are cached). Claude Code's first turn
starts a session (~15–20 s).

The console (password printed by `local-up`, also in `<state>/password`) shows the worker (owner = you), the queue
and the connector's state and agents.

## Another vboard

`VBOARD_URL=https://vboard.example.com VBOARD_TOKEN=vbp_… make local-up` uses a vboard that runs elsewhere instead
of starting one: create a personal access token (scope write) there under Settings → Access tokens. The token is
kept in the state dir; later runs need only `VBOARD_URL`. (A vboard before the multi-agent bridge — `ai-agents`,
merged 2026-10-10 — shows the default chat model only.)

## State

`~/.local/share/ai-connector-local/` (`AIC_LOCAL_STATE`), mode 0700, nothing of it in the repo:

| | |
|---|---|
| `password`, `device-token` | the server's sign-in password (generated) and the script's device token |
| `server/data/` | the server's data (devices, workers, connectors with the vboard token, …) |
| `worker.json`, `worker-id`, `worker/` | the worker's hub and token; its jobs, outputs and Claude Code sessions |
| `vboard/` | the vboard binary, its web app, its SQLite data (`vboard/data`) |
| `vboard-token`, `vboard-board` | the vboard access token of the connector (365 days, renewed when refused), the board |
| `logs/` | `server.log`, `worker.log`, `vboard.log`, `idp.log`, the build logs |
| `run/` | pid files (`down` stops only the processes recorded there, checked against their command line) |

Start over: `make local-down && rm -rf ~/.local/share/ai-connector-local`. `REBUILD=1 make local-up` builds
everything again (a checkout with local changes is rebuilt on every `up`).

## Notes

- The script builds vboard from the checkout as it is (`pnpm install --frozen-lockfile && pnpm build` in
  `frontend/`, `go build` of the backend); it never runs git commands that change the checkout.
- vboard's `make dev` instance on :8080 (if one runs) is a different instance with its own data; the local
  integration does not touch it.
- A second instance (e.g. to try a change of the script) runs next to the first with its own state and ports:
  `AIC_LOCAL_STATE=/tmp/aic2 AIC_LOCAL_PORT=28484 VBOARD_PORT=28480 VBOARD_IDP_PORT=28482 LMSTUDIO_VISION=0
  CONNECTOR_NAME=test deploy/local/local.sh up` (its worker has its own state; the model store is shared).
- `local-down` waits for running work (see above), then stops the worker first (it unloads its models and closes its Claude Code sessions), then vboard and
  the server.
