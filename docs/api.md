# ai-connector — API contract (phases 3–6)

The contract the server, the workers, the console UI and other apps (vboard, docscanner, scripts) build against.
`docs/architecture.md` describes the mlxlab-compatible API that already exists (`/api/*`, `/files/*`, devices,
workers, the tunnel); this file adds what other applications use. Wire names stay mlxlab's (`MLXLAB_*`, the
`mlxlab_session` cookie).

## Who may call what

| Credential | How it is sent | May use |
|---|---|---|
| device token (a person: web UI, TUI, iOS) | `Authorization: Bearer`, cookie `mlxlab_session`, `?token=` | everything below and the whole mlxlab API |
| **client token** (an application: `aic_…`) | `Authorization: Bearer aic_…` | the client APIs (OpenAI, TEI, Laya) and the native jobs API (below) — not devices, workers, clients, connectors |
| worker token | `Authorization: Bearer` | the tunnel `/api/worker/connect`, `/tools/mlxlab/{platform}` |

Without a valid credential: client API paths (`/v1/*`, `/embed`) answer `401 {"error":{"message":"…","type":"invalid_request_error","code":"invalid_api_key"}}`; `/api/*` answers `401 {"error":"signin"}` as before.

## Client tokens (device sign-in required)

Stored like worker tokens: `<data>/clients.json`, SHA-256 hashes only; the token is shown once.

| | |
|---|---|
| `GET /api/clients` | `[{"id","name","created","lastSeen"}]` (`lastSeen`: last use, written at most once a minute; absent until the first use) |
| `POST /api/clients` | `{"name":"docscanner"}` → `{"client":{…},"token":"aic_<43 base64url chars>"}`; blank name → 400 `{"error":"name"}` |
| `DELETE /api/clients/{id}` | revokes it (204; unknown id 404) |

## OpenAI-compatible API (client or device token)

Model ids are the catalogue ids the workers list (`GET /api/models`): built-in ones (`qwen3.6-35b-a3b`,
`qwen3-embedding-0.6b`, `laya-typed-decisions`, …) and runtime ones (`ollama/qwen3:8b`, `lmstudio/…`). A model is
also found by a unique prefix of its id, and by the last path segment of a Hugging Face name, case-insensitive
(`Qwen/Qwen3-Embedding-0.6B` → `qwen3-embedding-0.6b`; `typed-decisions` → `laya-typed-decisions`).

Errors are OpenAI-shaped: `{"error":{"message","type","code"}}` — 400 `invalid_request_error`; 401
`invalid_api_key`; 404 `model_not_found` (no connected worker lists the model); 503 `no_worker` (no worker
connected, or none that has the model is online); 409 `no_worker` (none has the memory); 502 `worker_error`.
As implemented: `type` is `invalid_request_error` for 400/401/404 and `server_error` for 409/502/503; `code` is the
name above (`invalid_api_key`, `model_not_found`, `no_worker`, `worker_error`; `unknown_url` for a path that is no
endpoint) and `null` for a plain 400. A model of another kind (an embedder for a chat) is a 400.

Limits: a request body of at most 32 MB; at most 2048 inputs per embedding call (OpenAI and TEI); no cap on
`max_tokens` beyond the worker's (when unset: 4096, thinking 32768 on the MLX chat models; a runtime's own default for `ollama/…` and the like).

### `GET /v1/models`

`{"object":"list","data":[{"id","object":"model","created":0,"owned_by":"ai-connector","kind":"chat|embed|classify","workers":["Mac Studio",…]}]}` — the chat, embed and classify models of the connected workers (an empty list while none is connected).

### `POST /v1/embeddings`

Request `{"model"?: "…", "input": "text" | ["text", …], "encoding_format"?: "float"|"base64"}` (`model` default:
`MLXLAB_EMBED_MODEL`, else `qwen3-embedding-0.6b`; token arrays → 400; at most 2048 inputs).
Response `{"object":"list","model":"<id>","data":[{"object":"embedding","index":i,"embedding":[…] | "<base64 of little-endian float32>"}],"usage":{"prompt_tokens":0,"total_tokens":0}}`.
Vectors are L2-normalised (Qwen3-Embedding: 1024 dimensions). The caller adds any instruction prefix itself.
Runs on the worker's **direct path** (below): not queued behind long jobs, not in the job history.

### `POST /v1/chat/completions`

Request (OpenAI): `model` (required), `messages` (`system|user|assistant|tool`; `content` a string or an array of
`{"type":"text","text"}` parts — image parts → 400), `stream`, `temperature`, `top_p`, `presence_penalty`,
`max_tokens` / `max_completion_tokens`, `tools` (`[{"type":"function","function":{"name","description","parameters"}}]`),
assistant `tool_calls`, tool `tool_call_id`. Thinking is off unless asked: `reasoning_effort` other than `"none"`,
or `"think": true`, or `"chat_template_kwargs":{"enable_thinking":true}`. Other fields are ignored.

It becomes a **chat job** in the chosen worker's queue (visible in the job lists, `"client":"<client name>"`). The
model may call the given tools; the calls are returned, never run (`finish_reason:"tool_calls"`).

* `stream:false` → `{"id":"<job id>","object":"chat.completion","created","model","choices":[{"index":0,"message":{"role":"assistant","content":"…","reasoning_content"?: "…","tool_calls"?: [{"id","type":"function","function":{"name","arguments":"<JSON string>"}}]},"finish_reason":"stop|tool_calls|length"}],"usage":{"prompt_tokens","completion_tokens","total_tokens"}}`
* `stream:true` → SSE `data: {"id","object":"chat.completion.chunk","created","model","choices":[{"index":0,"delta":{…},"finish_reason":null}]}` … — the first delta has `role:"assistant"`; thinking comes as `delta.reasoning_content`, the answer as `delta.content`, tool calls in the last chunk as `delta.tool_calls` (with `index`); a final chunk with `finish_reason` and `usage`; then `data: [DONE]`. While the job waits in the queue or loads its model, `: ping` comments keep the stream open. A failure after the stream started is sent as `data: {"error":{…}}` before `[DONE]`.
* The client going away cancels the job.

Details of the implementation:

* `model` in the answers is the resolved catalogue id (`qwen3.6` → `qwen3.6-35b-a3b`), never the caller's alias
  (the same for `/v1/embeddings`).
* A streamed answer opens at once: the SSE headers and a first `: ping` go out before the job is submitted, then a
  `: ping` every 5 s while it waits or runs. A failure from then on (also the submission's: a worker that refuses
  the job) is the same `{"error":{"message","type","code"}}` object as a `data:` line, then `data: [DONE]`.
* `finish_reason` is `length` when the answer used its whole token limit (`max_completion_tokens`, else
  `max_tokens`, else the worker's default).
* `role:"developer"` is taken as `system`; text parts of a content array are joined with a newline. `"think"` may
  also be Ollama's level string (`"low"` … : on). `tool_choice:"none"` drops the tools; other `tool_choice`
  values are not enforced (the model chooses).
* A non-streamed answer with tool calls has `content: null` when the model wrote nothing besides the calls.
* The streamed text comes from the workers' event stream; the job as the worker has it at the end is
  authoritative (what the stream missed at the end is sent from it).
* A chat job without `zone` and `parent` may carry the caller's own tool turns in its history (assistant
  `tool_calls`, `tool` results): nothing runs tools in such a chat and a follow-up of it can never take a zone, so
  the zone rule ("tool results only from the chat that made them") still holds for zone chats.

## Endpoints vboard's sidecar settings point at (client token)

### TEI — `POST /embed` (vboard `EMBED_URL`)

Request `{"inputs": "text" | ["text", …], "normalize"?: true, "truncate"?: true}` → `[[float, …], …]` with
`MLXLAB_EMBED_MODEL` (default `qwen3-embedding-0.6b`). `normalize` and `truncate` are accepted and ignored: the
vectors are always normalised, the embedder truncates long texts itself. Errors: TEI-style `{"error":"…","error_type":"…"}` with the
status codes above.

### Laya — `POST /v1/systemone` (vboard `CLASSIFIER_URL`)

Request `{"state":{…},"model"?: "typed-decisions","questions":{"<qid>":{"type":"choice","instructions":"…","criteria":{"<label>":"<description>",…}}}}`
→ `{"answers":{"<qid>":{"choice":"<label>","probabilities":{"<label>":p,…},"answer_confidence":p}}}`.
The state is serialised the way Python's `json.dumps(state, ensure_ascii=False)` does (`internal/pyjson`), as
vboard-ai does; criteria keep their order (a label sent twice keeps its first place and its last description, as a
Python dict). `type` must be `choice` (or absent); a criterion's description is a text or `null`. Each question is
one classify call on the same worker. Errors are OpenAI-shaped (the endpoint is under `/v1/`). Model default
`MLXLAB_CLASSIFY_MODEL`, else `laya-typed-decisions`.

## Native jobs API (client or device token)

The mlxlab job API, for applications too: `GET /api/models`, `POST /api/jobs`, `GET /api/jobs`,
`GET /api/jobs/{id}`, `POST /api/jobs/{id}/cancel`, `GET /api/events` (SSE), `GET /files/…`
(see `docs/architecture.md`). A job submitted with a client token carries `"client":"<client name>"`.

`engine.Request` gains:

* `tools`: `[{"name","description","parameters":<JSON schema>}]` — functions a chat model may call; one round:
  the job ends with the model's calls in `result.toolCalls` (`[{"id","name","arguments":"<JSON string>"}]`),
  never run. Not with `zone` (the zones' tools run on the worker).
* `client`: set by the server from the client token's name (a value sent by the caller is replaced).

## Worker: direct path (through the tunnel; the server calls it)

Embedding and classifying are quick and run beside the queue (a video rendering does not hold a search up):

* `POST /api/direct/embed` `{"model","texts":[…]}` → `{"model","dim","vectors":[[…]]}`
* `POST /api/direct/classify` `{"model","states":[…],"instructions","options":[{"label","description"}]}` →
  `{"model","answers":[{"choice","probabilities":{…},"confidence"}]}`

Unknown model 404 `{"error":"model"}`, a model of another kind 400, a failure 500 `{"error":"run","message"}`;
a bad body 400 `{"error":"request"}` (at most 4096 texts or states per call). The worker L2-normalises the vectors
(a runtime's embeddings need not be), and its route mapping (job ids, `/files/` urls) is not applied to this path:
the texts are the caller's.
The server picks a worker that lists the model, preferring one that has it loaded.

## Connectors (phase 4; device sign-in required)

Outbound connections the server makes to applications that expect a local AI to dial in. Kind `vboard`: the server
dials `wss://<vboard>/api/v1/classifier/bridge` with a vboard personal or board token and answers classify, embed
and chat (with tools) as vboard-ai does, through the workers.

| | |
|---|---|
| `GET /api/connectors` | `[{"id","kind":"vboard","name","url","board","enabled","state":"connected|connecting|error|disabled","error","since","user","models":{"classify","embed","chat"}}]` (never the token) |
| `POST /api/connectors` | `{"kind":"vboard","name","url","token","board"?,"models"?:{"classify"?,"embed"?,"chat"?}}` → the connector (201) |
| `PATCH /api/connectors/{id}` | any of `name`, `url`, `token`, `board`, `enabled`, `models` → the connector |
| `DELETE /api/connectors/{id}` | 204 |
| `GET /api/connectors/events` | SSE: `data: [<connector>,…]` at once and on every change |

Stored in `<data>/connectors.json` (mode 0600; the token in clear, the server needs it to dial).

## Console UI and host names (phase 6)

Two web apps from one server: the **console** (ai-connector: overview/queue, workers, models, clients, connectors,
"Connect a worker") under `/console/`, and the mlxlab app at `/` as before. On the host names in
`MLXLAB_CONSOLE_HOSTS` (comma list, e.g. `ai.example.org`) `/` redirects to `/console/`; every host serves both paths.
Both sign in through `POST /api/login` and use the same cookie.

## Agents (Claude Code) (phase 8a)

Chat consumers (the OpenAI-compatible API, the vboard connector, the console) can chat with **Claude Code** running
on a worker, as a long-running agent. Code: `internal/agents` (sessions, drivers), `internal/engine/claudecode.go`
(the backend), `internal/web/agents.go` (worker API), `internal/hub/agents.go` (ownership, routing, proxy).

### Models and jobs

A worker where `claude --version` works lists `claude-code/sonnet`, `claude-code/opus` and `claude-code/haiku`
(kind `chat`, backend `claude-code`; asked again every minute, so installing Claude Code needs no restart). A chat
job of such a model is **one turn** in a Claude Code session. `engine.Request` fields (camelCase):

| field | |
|---|---|
| `session` | the conversation key (e.g. `vboard:<board>:<user>`): turns with the same owner and key share one session. Absent: a one-off session, closed (and its folder removed) after the turn; earlier messages are then sent as a transcript in front of the last one |
| `driver` | `json` (default) or `tty` (below) |
| `mcp` | `[{"name","url","type"?: "http"\|"sse","headers"?: {…}}]` — the MCP servers the session may use (only these: `--strict-mcp-config`); their tools are allowed as `mcp__<name>` |
| `owner` | **set by the server** from the signed-in device's user (a caller's value is dropped); later a connector or client sets its own owner |

With a `session`, only the last user message is sent: the session keeps its own history. `system` messages become
`--append-system-prompt` when the session starts. `zone` is refused (Claude Code has its own tools).

Events of the job: `text` (the answer, streamed as Claude Code writes it with the json driver), `status`, `tool`
(the job's `tools` list, upserted by id: `{"id","name","state":"running|done|error","arguments":{…},"summary":"<path,
command, URL …>","result":"<shortened>","zone":""}`), and `stats`: `{"session","driver","claudeSession","seconds",
"turns"}` plus, with the json driver, `{"promptTokens","tokens","cacheReadTokens","costUSD","firstTokenSeconds"}`
(`costUSD` is this turn's; Claude Code reports the session's total, the worker takes the difference).

Claude Code jobs **do not wait in the GPU queue**: a backend that implements `engine.Parallel` (`Parallel() bool`)
runs its jobs at once beside the queue (`jobs.Manager.Beside`), several at a time; they are still in the history,
cancellable, and not counted in the worker's `state.running/queued`. Cancel interrupts the turn (below); the session
stays.

### Sessions and drivers

Sessions are **long-running**, one per (owner, conversation key). Measured on the development Mac (sonnet, one session, Claude Code 2.1.288):
1.7 s to the first text on turn 1 (process start included), 1.2 s on turn 2, 0.7 s on turn 3; a fresh `claude -p`
per message is ≈ 2 s every time. A session is closed after `idleMinutes` without use (default 30; never while
someone is attached to its terminal) or when `maxSessions` (default 4) are running and a new one needs room (the one
idle longest goes; all busy → the job fails with "too many Claude Code sessions are busy"). Claude Code's own session
id is kept per conversation (`<config>/agents/resume.json`): a closed, stopped or crashed session continues with
`--resume` on its next turn, also after a worker restart. Changing the model, driver or MCP servers of a
conversation restarts its process (resumed).

* **json** (default; for the APIs) — `claude -p --input-format stream-json --output-format stream-json --verbose
  --include-partial-messages --model <m> --strict-mcp-config --mcp-config <file> --allowedTools <list>
  --permission-prompts none [--append-system-prompt …] [--resume <id>]`, kept running. A turn is a line
  `{"type":"user","message":{"role":"user","content":"…"}}` on its stdin; the answer is read from stdout
  (`stream_event` `content_block_delta`/`text_delta` → text; `tool_use` blocks of `assistant` messages and
  `tool_result` blocks of `user` messages → tools; the `result` line ends the turn with usage and cost). **Cancel**
  writes the control request `{"type":"control_request","request_id":"…","request":{"subtype":"interrupt"}}`: Claude
  Code stops the turn, answers `control_response` and a `result` with `subtype:"error_during_execution"`, and the
  process stays for the next turn (SIGINT if the request cannot be written; the process is killed if the turn has not
  ended 10 s later — the next turn then resumes the conversation). `--permission-prompts none` (used when `claude
  --help` lists it): a tool outside the allow-list is refused at once instead of waiting for an answer nobody gives.
* **tty** — an interactive `claude` in a pseudo terminal (unix: `github.com/creack/pty`; **not on Windows yet**:
  the job fails with "tty sessions are not supported on Windows yet"), kept running and **attachable from the
  browser**. API turns are typed into it (a bracketed paste, then Enter); the answer comes from Claude Code's hooks:
  the session's own `--settings` file has `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse` and `Stop`
  hooks that append their JSON input to `<config>/agents/.ctl/<id>/hooks.jsonl`, which the worker follows. The
  `Stop` hook's `last_assistant_message` is the answer (else the transcript's last assistant text); a `Stop` counts
  only after the `UserPromptSubmit` of this turn's prompt (a person typing meanwhile does not end it). No token
  streaming; no usage or cost. Cancel sends Esc. Claude Code asks whether to trust a new folder (default "No"): for
  the session's own folder the worker answers "Yes" (`autoTrust`, default true; Claude Code records that in its own
  config like any trust answer).

Each session works in **its own folder** `<config>/agents/<session id>/` (the same id for the same owner and key,
so `--resume` finds the conversation, which Claude Code keeps per folder); its control files (`mcp.json`,
`settings.json`, `hooks.jsonl`, mode 0600) are in `<config>/agents/.ctl/<session id>/`, outside that folder.
`<config>` is `~/.config/mlxlab` (`$MLXLAB_HOME`).

### The allow-list: `<config>/agents.json` (per worker)

```json
{"allowedTools": ["Read", "Glob", "Grep", "LS", "WebSearch", "WebFetch", "TodoWrite"],
 "model": "sonnet", "idleMinutes": 30, "maxSessions": 4, "autoTrust": true}
```

All fields optional; the values above are the defaults (read-mostly: nothing that edits files or runs commands).
`allowedTools` takes Claude Code's rule syntax (`"Bash(git status)"`, `"Edit"`); the `mcp__<name>` of the
session's own MCP servers are always added. `model` is used when a request names none.

### Ownership (everyone uses their own Claude subscription)

Claude Code runs with the subscription logged in on the worker, so it runs only for the worker's owner:

* `Worker.owner` — the user (`Device.user`) of the device that enrolled it (`POST /api/workers`). The legacy
  shared-token worker and workers from before have none; an owner device sets it: `PATCH /api/workers/{id}
  {"owner":"Anna"}` (owner devices only, like `PATCH /api/devices`; 403 otherwise, 404 unknown) → the worker.
  `GET /api/workers` items carry `owner`.
* `POST /api/jobs` with a `claude-code/*` model (by id or a prefix a worker resolves) goes **only to a connected
  worker whose owner is the signed-in user** and that lists the model (`request.worker` may pin one of the user's
  own workers; someone else's → 403). The server sets `owner`. None connected → **503**
  `{"error":"offline","message":"<user>'s Claude Code worker is offline"}`; the user owns no worker → **409**
  `{"error":"no-worker","message":"<user> has no worker with Claude Code: …"}`; all their workers are connected but
  none has Claude Code → 409 too. A device without a user name → 403. Other models route as before.

### Endpoints

Hub (device sign-in; only the sessions of workers the user owns):

| | |
|---|---|
| `GET /api/agents/sessions` | `[{"id":"<rid>.<id>","conversation","model","driver":"json\|tty","state":"starting\|idle\|busy","created","lastUsed","cwd","owner","viewers","turns","claudeSession","worker":{"id","name"}}]` — merged over the user's connected workers |
| `POST /api/agents/sessions[?worker=<id>]` | `{"model"?: "opus" \| "claude-code/opus","cwd"?: "<absolute folder on the worker>"}` → 201 the session: a **tty** session for a person to use interactively (default: their first connected worker with Claude Code; `owner` is set by the server) |
| `DELETE /api/agents/sessions/{id}` | stops it (204); the conversation resumes with its next turn |
| `GET /api/agents/sessions/{id}/tty` | **WebSocket**: binary frames are the terminal's bytes both ways (the viewer first gets the scrollback, ≤ 256 KB), text frames `{"type":"resize","cols","rows"}`; several viewers may attach; closed (1000) when the session ends. A json session → 409 |

Someone else's session → 403; its worker offline → 503. The worker serves the same paths (without ownership: it
trusts the tunnel); `mlxlab serve` keeps the WebSocket's same-origin check. The hub's reverse proxy passes the
WebSocket upgrade through the tunnel (tested in `internal/hub/agents_test.go`).

**Session ids** are route-safe: 13 characters, a letter (`s` a conversation, `o` a one-off, `p` a person's tty
session) and 12 hex digits. Through the hub they carry the worker's route like job ids — `m1.s0a580f134345` — the
worker's route layer adds the prefix to every session object it answers (`id`, plus `worker`) and strips it from
`/api/agents/sessions/<rid>.<id>` paths; the hub routes by it.

### vboard: agents in the bridge protocol (phase 8b; vboard implements its side)

Additive to vboard's bridge (`/api/v1/classifier/bridge`):

* The client's `hello` may carry `"agents":[{"id","name","kind":"model"|"agent","worker"?}]` — what the user can pick
  to chat with: plain chat models (`kind:"model"`) and agents like Claude Code (`kind:"agent"`, `worker`: where it
  runs). When the list changes the client sends `{"type":"agents","agents":[…]}`.
* vboard's `chat` request may carry `"agent":"<id>"` (absent: the default `hello.chat` model, as before) and, for a
  `kind:"agent"`, `"context":{"boardId","boardName"}`.
* An agent answers with `chatDelta` text and `status` lines as it works, then a `chatDone` with its final text and
  **no `toolCalls`**: it changed the board itself, through **vboard's MCP server** (the connector passes that server
  in the job's `mcp`, with a token for the board, and uses a session key per board and user).
