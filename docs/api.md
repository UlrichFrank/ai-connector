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
