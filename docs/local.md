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
2. `claude-code/opus`, `claude-code/sonnet` and `claude-code/haiku`, when `claude` is installed — they work on the board themselves
   through vboard's MCP.

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
- `local-down` stops the worker first (it unloads its models and closes its Claude Code sessions), then vboard and
  the server.
