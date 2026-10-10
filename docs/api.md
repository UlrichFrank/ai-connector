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
and chat (with tools) as vboard-ai does, through the workers. vboard is not changed for it.

| | |
|---|---|
| `GET /api/connectors` | `[<connector>]` in the order they were made (never the token) |
| `POST /api/connectors` | `{"kind"?:"vboard","name"?,"url","token","board"?,"enabled"?,"models"?:{"classify"?,"embed"?,"chat"?}}` → the connector (201) |
| `PATCH /api/connectors/{id}` | any of `name`, `url`, `token`, `board`, `enabled`, `models` → the connector (200) |
| `DELETE /api/connectors/{id}` | 204 (unknown id: 404 `{"error":"not found"}`) |
| `GET /api/connectors/events` | SSE: `data: [<connector>,…]` (the whole list) at once and on every change; `: ping` every 25 s |

A connector: `{"id","kind":"vboard","name","url","board","enabled","models":{"classify","embed","chat"},"owner",
"created","state":"connecting|connected|error|disabled","error","since"?,"user"}`.

* `name`: also the name vboard shows for the local AI (hello.name and `?name=`); blank → `ai-connector`; at most 80
  characters.
* `url`: the vboard address, `http(s)://host[:port][/path]` (a trailing slash is dropped); anything else → 400
  `{"error":"url","message"}`.
* `token`: required on POST; any text without spaces (vboard's personal tokens start with `vbp_`, the prefix is
  configurable there, board tokens work too) → else 400 `{"error":"token"}`. PATCH with a blank or missing token keeps
  the stored one.
* `board`: optional; with it the AI serves everyone on that board (the token needs "Can edit" there; a board token is
  bound to its board anyway).
* `models`: catalogue ids or names as in `/v1/*` (`""` = the server's default: `MLXLAB_CLASSIFY_MODEL`,
  `MLXLAB_EMBED_MODEL`, and for chat the first chat model a connected worker lists). PATCH replaces the fields given.
* `enabled`: default true. `owner`: the `user` of the device that made it (for routing by person later).
* `state`: `connecting` (with `error` saying why it waits or retries: no worker offers a model, a lost connection
  "… (retrying in 4s)"), `connected` (`since`: RFC 3339, `user`: who the token belongs to, from vboard's
  `/api/v1/me` and `welcome`), `error` (`error`: why; it stays stopped until the connector is edited — any PATCH
  dials again), `disabled`.
* Unknown kind → 400 `{"error":"kind"}`; a bad body → 400 `{"error":"bad request"}`. Client tokens (`aic_…`) get 401.

Stored in `<data>/connectors.json` (mode 0600; the token in clear, the server needs it to dial).

### How the vboard connector works (as built)

One goroutine per enabled connector, started with the server and on create/enable/edit, stopped on
disable/delete/shutdown (the requests running on it are canceled; a chat's job is canceled on its worker).

1. `GET <url>/api/v1/me` with the token (as vboard-ai does): 401 stops the connector (state `error`); a network
   error, 429 or 5xx is retried; other answers are left to the bridge.
2. `GET ws(s)://…/api/v1/classifier/bridge?board=&name=` with `Authorization: Bearer <token>`. 401, 403
   (`token.boardScope`, `token.scope`) and 404 (`board.notFound`) are final; 503 (`server.draining`), other statuses
   and network errors are retried after 1 s, 2 s, 4 s … at most 60 s (back to 1 s after a connection that worked).
3. `{"type":"hello","name","device":"ai-connector","model","embed","chat","instance","handover":true}` — the models
   the connected workers offer **now** (catalogue ids, cut to vboard's 60/80/80 characters; `""` = cannot), the
   instance `aic_…` one per runner. While no connected worker offers any of the three, the connector does not dial
   (`connecting`, "No connected worker offers …"); when that happens on an open connection it closes it.
4. vboard answers `{"type":"welcome","id","board","user"}` → `connected`.
5. Requests are answered in goroutines while one loop keeps reading (vboard pings every 30 s):
   * `classify` → the worker direct path (`/api/direct/classify`, a worker that lists the model, preferring one that
     has it loaded): each state serialised like Python's `json.dumps(state, ensure_ascii=False)`, the options read
     in their order (`null` = no description; a structured one becomes compact Python JSON, as vboard-ai does);
     `{"type":"answers","id","answers":[{"choice","probabilities","answer_confidence"}]}`, one per state.
   * `embed` → `/api/direct/embed` in batches of 256 texts; `{"type":"vectors","id","vectors"}`, L2-normalised.
   * `chat` → a chat job in a worker's queue (`"client":"vboard: <connector name>"`, thinking off as vboard-ai's
     built-in model, vboard's messages with their tool turns and vboard's tools; the calls are returned, never run).
     While the job waits or its model starts: `{"type":"chatDelta","id","status":"Waiting for Mac Studio (queued)"}`
     / `"Running on Mac Studio (<model>)"`; a thought (`<think>`) → one `{"thinking":true}` (the thought itself is
     not sent); the answer → `{"text"}` deltas; then `{"type":"chatDone","id","content","toolCalls":[{"id","name",
     "arguments"}]}` (`toolCalls` always a list, `arguments` a JSON string, `"{}"` when empty). Every 60 s without
     anything else sent, a keep-alive (the same status again, `thinking:true` again, or an empty `text`; vboard gives up
     after 3 minutes of silence). `chatCancel` cancels the job; nothing more is sent for it.
   * Failures → `{"type":"error","id","message"}` (at most 300 characters); an unknown request type with an id too.
     A request for something the connector cannot do now (no worker offers the model) is such an error.
   * An answer larger than vboard reads (8 MB) is replaced by an error for its id.
6. `{"type":"reconnect"}` (a vboard replica drains), and a change of what the workers offer (a worker with the chat
   model connects or goes; checked on every fleet change and every 30 s): a new connection with a fresh hello and
   the same `instance` is opened (retried 1 s … 10 s); new requests are answered there, the old connection is
   closed normally (1000 "handed over") once what runs on it is answered (and at least 2 s after the handover).

**Compatibility.** What the connector sends is understood by every vboard since the bridge exists; tested against a
fake vboard of each protocol stage (`internal/hub/vboard_test.go`), which reads the hello and the answers the way
that commit's `bridge.go` / `chat.go` do:

| vboard | what it has | result |
|---|---|---|
| `d508560` (2026-09-26) | hello `{type,name,device,model}`, classify → answers/error | ✓ classify (the other hello fields are ignored); it sends classify to any bridge, so without a classify model the connector answers it with an error |
| `0992b68` | + embed → vectors, hello.embed | ✓ classify, embed |
| `f914d44` (2026-09-27) | + chat/chatCancel → chatDelta/chatDone, hello.chat | ✓ chat with tools, thinking, cancel; a status-only `chatDelta` is an empty text there (not shown, it restarts the idle timer) |
| `175647c` → HEAD | + hello.instance/handover, `reconnect`, `chatDelta.status` | ✓ status shown, handover keeps the running chat |

## Console UI and host names (phase 6)

Two web apps from one server: the **console** (ai-connector: overview/queue, workers, models, clients, connectors,
"Connect a worker") under `/console/`, and the mlxlab app at `/` as before. On the host names in
`MLXLAB_CONSOLE_HOSTS` (comma list, e.g. `ai.example.org`) `/` redirects to `/console/`; every host serves both paths.
Both sign in through `POST /api/login` and use the same cookie.
