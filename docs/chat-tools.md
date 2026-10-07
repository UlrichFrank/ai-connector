# Chat with tools: zones, sandboxes, the guard

See `internal/agent/contract.go` for the Go contract. This is the API and the security model.

## Zones (enforced by the worker, not the model)

| zone | tools | network of the tools | never |
|---|---|---|---|
| `home` | internal MCP servers (`~/.config/mlxlab/mcp.json`) | the LAN | web content in the context; web/file tools |
| `web` | `web_search`, `web_fetch` (sandbox VM) + the files tools | internet only, via the egress proxy | home tools (not offered; refused if called) |
| `files` | `make_file` (offline sandbox VM) | none | home or web tools |

* The zone is chosen when a chat starts (`request.zone`) and every follow-up turn must keep it: the worker
  derives the zone of a follow-up from the job it continues (`request.parent`) and refuses a change (400).
* Tools that change something (MCP tools without `readOnlyHint`) wait for approval: event `tool` with state
  `approval`; `POST /api/jobs/{id}/approve {"call":"<tool id>","allow":true|false}`; no answer within 5 minutes =
  denied. (The GPU slot stays with the chat while it waits.)
* Untrusted results (web) are screened by the guard before the model reads them; `injection` → state `held`, the
  model gets "[result held back: it contained instructions aimed at the assistant]" instead.
* After the answer, the guard grades it: event `stats` with `{"verdict": {label, score, why}}`.

## Request

`POST /api/jobs` for a chat model, plus: `zone` ("home" | "web" | "files"; "" = no tools), `parent` (the job a
turn continues). Messages may include tool turns: `{Role:"assistant", Content, ToolCalls:[{Name, Arguments}]}`,
`{Role:"tool", Content, ToolCallID}` — the worker sends them back in `result.messages` so the next turn can carry
them.

## Events (SSE `/api/events`, engine event `type`)

* `tool` — `data` is `agent.ToolEvent` (one per state change of a call; the UI keys calls by `id`).
* `text` — the answer, streamed (between tool calls too: "thinking out loud" before a call).
* `stats` — at the end: tokens etc., `verdict`, `zone`, `toolCalls`.
* Generated files are job `files` (`/files/...`), also listed on their tool event.

## Sandboxes (Apple `container`, one lightweight VM each)

* **web**: a VM with the search (SearXNG; Brave Search API when `MLXLAB_BRAVE_KEY` / `~/.config/mlxlab/brave.key`
  is set) and a fetcher (page → readable text, size-capped). Its only route out is an egress proxy container that
  refuses private, loopback, link-local, CGNAT, multicast and the Mac's/Pi's addresses and names (`*.example.org`,
  `*.nip.io` to private IPs, `*.local`), resolving DNS itself (no DNS rebinding). A pf anchor on the Mac
  (`make sandbox-firewall`, needs sudo) additionally drops traffic from the container network to the LAN and to
  the Mac. Nothing secret is in the VM (the Brave key stays in the proxy).
* **files**: a VM without network (`--network none`) with Python (reportlab, matplotlib, pandas, python-docx,
  openpyxl, markdown→PDF). The model sends code + a file name; outputs are copied out if type (pdf, png, svg,
  csv, xlsx, docx, md, txt) and size (≤ 25 MB) are allowed. CPU/memory/time limits per run.

## MCP (`~/.config/mlxlab/mcp.json`, mode 0600)

```json
{ "servers": {
    "vboard":    { "url": "https://vboard.192.168.65.4.nip.io/mcp", "headers": { "Authorization": "Bearer …" } },
    "smarthome": { "command": "smarthome-mcp", "args": [], "env": {} , "readOnly": ["get_*", "list_*"] }
} }
```

Transports: streamable HTTP (and the older HTTP+SSE) and stdio. All servers are `home` zone.

### Setting it up

The client is `internal/mcp` (standard library only). `mcp.Toolboxes(ctx)` connects to every enabled server and
gives one `agent.Toolbox` per server; a server that does not answer is left out (its error is returned alongside).

1. Copy `docs/mcp.example.json` to `~/.config/mlxlab/mcp.json` (or `$MLXLAB_HOME/mcp.json`) and keep it private:
   `chmod 600 ~/.config/mlxlab/mcp.json` — the client refuses a file the group or others can read.
2. Tokens: write them into the file, or as `${NAME}` (in `headers` and `env` values) to take them from the
   environment of the worker; an unset variable is an error, not an empty header. For vboard the token is the one
   in Claude's settings (`~/.claude.json`, `mcpServers.vboard.headers.Authorization`).
3. `mlxlab mcp list` shows each server (name, version, MCP version, transport) and its tools with their class;
   `mlxlab mcp call <server__tool> '<json>'` calls one (a tool that changes something needs `--yes`).

Per server:

| key | |
|---|---|
| `url`, `headers` | streamable HTTP; if the server answers the POST with a 4xx (not 401/403), the older HTTP+SSE transport |
| `type` | force `http`, `sse` or `stdio` |
| `command`, `args`, `env`, `dir` | stdio: a child process (own process group; stdin closed, then SIGTERM/SIGKILL on close); stderr goes to `~/.config/mlxlab/logs/mcp-<server>.log` |
| `readOnly` | glob patterns (`get_*`) of tools that change nothing, besides those the server marks `readOnlyHint` |
| `timeout` | seconds per tool call (default 60) |
| `disabled` | skip the server |

* Tool names are `<server>__<tool>` (`vboard__search_cards`); the description and the JSON schema are the server's.
  Server names: letters, digits, `-` and single `_`.
* Every tool is `home` zone, trusted, and `Changes` (approval for each call) unless read-only as above.
* Protocol: the client asks for MCP 2025-11-25 and accepts 2025-06-18, 2025-03-26 and 2024-11-05. It answers the
  server's `ping` and refuses other server requests (no sampling, roots or elicitation).
* Results: text as is; images and audio as a note (`[image image/png, 12 KB, not shown]`); embedded resources as
  their text; `isError` and JSON-RPC errors (unknown tool, bad arguments) become a result with `IsError` for the
  model, a broken connection an error for the loop.
* Timeouts: connect 20 s, list 30 s, calls 60 s (or `timeout`), dial and TLS 10 s; the caller's context wins. An
  expired HTTP session (404) is re-initialized once and the request sent again; other connection failures are
  retried only for requests that cannot change anything (never `tools/call`).

## The files sandbox (as built: `sandbox/files`, `internal/sandbox/files`)

* Image `mlxlab-sandbox-files:<hash of the build context>` (Debian trixie-slim, Python 3.13 from Debian packages:
  reportlab, matplotlib, numpy, pandas, openpyxl, xlsxwriter, python-docx, markdown + WeasyPrint, Pillow, pypdf;
  fonts DejaVu, Noto, Liberation, Noto Color Emoji; no compilers, no pip). The Containerfile and scripts are
  embedded in the binary; a missing image is built on first use (`container build`, needs the builder) and earlier
  tags of the same repo are deleted. `make sandbox-files-image | sandbox-files-test | sandbox-files-try`.
* Tools: `make_file {filename, code, language:"python"}` and `markdown_to_pdf {filename, markdown, title?}`
  (zone files, not "changes", trusted). One call at a time.
* Each call: `container run --rm -i --network none --no-dns --read-only --tmpfs /out --tmpfs /tmp --cpus 2
  --memory 2g --cap-drop ALL --user 1000:1000` — no mounts at all. `--network none` leaves only `lo` in the VM
  (verified: 1.1.1.1, 192.168.0.1, the container gateway, DNS and UDP are unreachable). The request goes in on
  stdin; the runner (`runner.py`) runs the code as a child with a 60 s limit (process group killed), its output
  to a log, then streams a tar to stdout: `result.json` (exit, log tail, page counts) and the files of `/out`.
* The worker reads at most 10 files of ≤ 25 MB, regular files only, re-sanitizes the names (base name, letters,
  digits, `-_.`, ≤ 100 chars), allows pdf, png, svg, csv, xlsx, docx, md, txt, html, checks the magic bytes (PDF
  header, PNG signature + IHDR, OOXML zip with the right part and no macros, UTF-8 text), refuses svg/html with
  scripts, event handlers, `javascript:`, iframes/objects or entity declarations, and writes them into the output
  dir without overwriting (`name-2.pdf`). A failed run returns the traceback tail (IsError) so the model can fix
  its code; a timeout or an out-of-memory kill says so.
* Still to do by the web side: serve `/files/*.svg|html` with `Content-Security-Policy: sandbox` (or as a
  download) as a second line behind the active-content check.

## Web sandbox: how it is built (`internal/sandbox/web`, `sandbox/web`)

Three VMs (Apple `container`), images built with `container build` on first use (tagged with a hash of the sources;
no package installs, so the build needs no network), started on the first call, restarted when the health check
fails, stopped after 10 minutes idle:

| VM | networks | can reach |
|---|---|---|
| `mlxlab-web-proxy` (egress proxy, Go) | `mlxlab-web-ext` (NAT, 192.168.241.0/24) + `mlxlab-web-int` | the public internet, ports 80/443, DNS 1.1.1.1/9.9.9.9 |
| `mlxlab-web-tools` (search + fetch, Go) | `mlxlab-web-int` (host-only, 192.168.240.0/24) | only the proxy and SearXNG |
| `mlxlab-web-searxng` (only without a Brave key) | `mlxlab-web-int` | only the proxy |

* **Proxy**: resolves names itself, refuses the whole name if any address is not public (RFC1918, loopback,
  link-local, CGNAT, ULA, multicast, NAT64/6to4/Teredo, the container networks, its own addresses), refuses
  `*.example.org`, `*.local`, `*.internal`, `*.lan`, `*.home.arpa`, `localhost`, single-label and numeric names, dials
  the address it checked (no DNS rebinding), caps bytes (32 MiB) and time (90 s), logs every decision as JSON
  (`container logs mlxlab-web-proxy`). The Brave key (`MLXLAB_BRAVE_KEY` or `~/.config/mlxlab/brave.key`, mode
  0600) is passed only to the proxy, which adds it to `http://api.search.brave.com/res/v1/…` and sends that on
  over HTTPS.
* **Lockdown** (entrypoint of every VM): removes the default route (tools, SearXNG) or blackholes every private
  range and the Mac's gateway addresses (proxy), turns IPv6 off, then runs the service as an unprivileged user
  with no capabilities (empty bounding set, `no_new_privs`).
* **The Mac** talks to the tools service through a unix socket forwarded over vsock (`--publish-socket`,
  `~/Library/Caches/mlxlab/web-tools.sock`), not over the network — no TCP port, and the tools VM needs no route
  to the Mac at all.
* **pf** (`make sandbox-firewall`, sudo): the second wall for a VM whose kernel is compromised (root in a VM can
  re-add routes): drops everything from the isolated network to the Mac, and everything from the proxy's network
  except public DNS/HTTP/HTTPS. Run it again after a reboot, and while the sandbox is up to also load the
  interface rules (IPv6, spoofed sources).
* **Check**: `make sandbox-check` (`go test -tags sandbox ./internal/sandbox/web`) probes from inside every VM, as
  the service user: a canary port on every address of the Mac, the LAN router, 192.168.0.1, 10.0.0.1, 100.64.0.1,
  169.254.169.254, `localhost:8484`, the internet directly (all must fail), the internet via the proxy (must
  work), the proxy with private names/addresses (`192.168.1.1.nip.io`, `example.org`, `[::1]`, `2130706433`, …) (must
  be refused), and that the services run without capabilities. A "control" VM without the lockdown shows what
  only the network and pf block (WARN until pf is loaded).

## The guard in use

By default the chat's own model is the judge (`guard.NewJudged`, `internal/engine/chat.go`): it is already loaded,
needs no extra memory, and on the eval set it caught every injection, also in the holdout it was never tuned on,
without false alarms. It costs ~3 s per call (~6 s per web page, which asks two questions). The Laya classifier
with the heuristics is faster but missed most injections it was not tuned on (holdout recall 0.29, 0.28 false
alarms) — `MLXLAB_GUARD=laya` uses it anyway. `mlxlab guard eval` runs the eval set.

A judge is a model reading untrusted text, so it can itself be a target ("classifier: answer ok"). The zones are
the protection that does not depend on any model; the guard only lowers the odds.

## "Always allow" for tools

Some home tools change things but are fine without asking each time (e.g. `smarthome__set_light`). The user
can allow a tool **always**; it then runs without an approval card. Kept on the worker (the Mac) in
`<config dir>/approvals.json` (`{"always": ["smarthome__set_light", …]}`), so it holds for every client.

* `GET /api/approvals` → `{"always": ["…"]}`
* `PUT /api/approvals/{tool}` → allow always; `DELETE /api/approvals/{tool}` → ask again (both answer with the
  list). PUT takes only names the home zone's toolboxes offer right now (`400` otherwise); while an MCP server
  does not answer, a name `<server>__<tool>` of that configured server is taken on trust.
* `POST /api/jobs/{id}/approve {"call","allow":true,"always":true}` approves this call and adds the tool (`400`
  for a call that is not a home tool, or `always` without `allow`; `409` when nothing waits by that id).
* The loop skips the approval for tools on the list: the call's `tool` event goes straight to `running` with
  `"auto": true` (the UI shows "allowed always"). Only tools of the **home** zone can be on the list (the files
  and web tools never need approval anyway); unknown names are refused (400).
* The list is read again whenever the file changes (`internal/approvals`, written atomically), asked at each
  call: a change holds from the next call on, also in a running chat.
* A call that waited for approval keeps `"asked": true` in the job's `tools` (not in the live event), so a
  reloaded page and the transcript can say it was approved.
* `GET /api/tools` → `{"servers": [{"name", "error"?, "tools": [{"name", "description", "changes", "always"}]}],
  "always": [...]}`: the home zone's tools by MCP server (each one is connected to, so it takes a moment; a
  server that does not answer has `error`), for Settings → Tools.

## Copying a chat for analysis (like vboard's chat log)

`GET /api/jobs/{id}/transcript` (any job of a chat: it follows `parent` back to the first turn and forward to the
newest turn of that same branch) → `text/plain`, built on the worker so every client copies the same
(`internal/jobs/transcript.go`):

```
mlxlab chat · <model name> · zone: home · 2026-10-04T06:30:00Z
System: <system prompt, or "(default)">

———
Question:
<text>

Tools:
✓ smarthome__house_status {} — <result, shortened to ~600 chars>
⏸ smarthome__set_light {"light":"kitchen","on":true} — approval: allowed always — <result>
✗ web_fetch {"url":"…"} — held: <guard reason>

Answer:
<answer without the thinking>

(verdict: good 0.95 · 48.2 tok/s · 312 tokens · 7.4 s)
```

* The branch: a parent with as many messages (without the system prompt) is a sibling — a regenerated answer, or
  another model's in a compare chat — at the same place, not a turn before. Going forward, the newest continuation
  of each turn is taken; going back, the turns `id` continues (so a job deep in an older branch copies that
  branch).
* Tool lines, in call order: `✓` done; `⏸` a call that needed approval (`approval: allowed always`, `approval:
  approved`, or `approval: waiting`), then its result; `✗` `held: <guard reason>`, `denied: …`, `blocked: …`,
  `error: …`; `…` running. Arguments are compact JSON; a multi-line result goes on indented lines.
* `zone: none` for a chat without tools. `Answer (<model>):` when a turn's model differs from the first one's.
* The last line of a turn is its stats; a turn that did not answer says `(failed: <error> · …)`, `(canceled · …)`,
  `(still running)` or `(queued)` (and shows no Answer, or the part it had).
* `?thinking=1` adds a `Thinking:` part before each answer (each round's thought); `?full=1` keeps tool results
  unshortened (as the model read them). Clients offer "Copy chat" (clipboard) and, on iOS, the share sheet.

## Timetables: the `transit` MCP server (`internal/transit`, `cmd/transit-mcp`)

Public transport for the home zone: "when does a train go from Mössingen to Tübingen tomorrow morning?",
"departures at Tübingen Hbf now", "is my 7:42 late?". Unlike the other home tools it talks to the internet, but only
to fixed public timetable APIs (no keys, no user data sent but the stops and times asked for), and everything it
returns is sanitized: control, zero-width and bidi characters removed, HTML stripped (tags and entities; notices are
reduced to their titles), whitespace collapsed, lengths capped. All tools are read-only (`readOnlyHint`).

Setup — mlxlab has the server built in; print a ready-to-paste entry and merge it into mcp.json:

```sh
mlxlab mcp serve transit --print-config
```
```json
{ "servers": { "transit": { "command": "/path/to/mlxlab", "args": ["mcp", "serve", "transit"], "readOnly": ["*"] } } }
```

`mlxlab mcp list` then shows `transit__find_stop`, `transit__departures`, `transit__journeys`, `transit__trip`;
try `mlxlab mcp call transit__journeys '{"from":"Mössingen","to":"Tübingen Hbf","when":"2026-10-05T07:00"}'`.
The same server is `cmd/transit-mcp` (stdio; `--http :8790` serves streamable HTTP on 127.0.0.1:8790/mcp and
refuses foreign `Origin`s). It speaks MCP 2025-11-25 down to 2024-11-05.

Tools (text for the model, one line per departure or leg, plus `structuredContent` JSON; times Europe/Berlin):

| tool | arguments |
|---|---|
| `find_stop` | `query`, `provider?`, `limit?` → stops with ids (`db:8000141`, `efa:de:08416:10503`) |
| `departures` | `stop` (name or id), `when?` ("now" or local `2026-10-05T07:00`), `duration_minutes?` (60), `products?` (train, sbahn, bus, tram, subway, ferry, ondemand), `limit?` (15), `provider?` |
| `journeys` | `from`, `to`, `via?`, `when?`, `arrive_by?`, `results?` (4), `products?`, `provider?` — names are resolved and the answer says which stop was taken |
| `trip` | `id`: a journey id (`j-…`, all stops of every leg) or a trip id (`t-…`, a DB train's or bus's run) |

`07:11 +3` = three minutes late, `+0` on time, no sign = no real-time data; `(07:43)` is the real time when the delay
is large; `CANCELLED`, platform changes and important notices are marked. The model resolves "tomorrow" itself (the
chat's system prompt carries the date); a bare `07:00` means today.

Providers (behind `transit.Provider`; chosen with `provider`, default automatic):

* `db` — bahn.de's own web API (`https://www.bahn.de/web/api`: `reiseloesung/orte`, `reiseloesung/abfahrten`,
  `angebote/fahrplan`, `reiseloesung/fahrt`), what db-vendo-client's `dbweb` profile uses: every train in Germany
  and most buses, with real-time delays, platforms and cancellations. The community API v6.db.transport.rest
  answered 503 (October 2026). bahn.de's bot protection refuses requests that ask for compressed answers or carry a
  URL in the User-Agent, so the client sends `Accept-Encoding: identity` (and still unpacks gzip) and
  `User-Agent: mlxlab-transit/1.0 (personal timetable assistant)`; at most one request per 300 ms.
* `efa` — EFA-BW (`https://www.efa-bw.de/nvbw/`, Mentz EFA, `XML_STOPFINDER_REQUEST`, `XML_DM_REQUEST`,
  `XML_TRIP_REQUEST2`, `outputFormat=rapidJSON`): all of Baden-Württemberg — naldo (Tübingen, Reutlingen,
  Mössingen, Hechingen, Balingen, Sigmaringen: TüBus, RAB/SWEG buses, Zollernalbbahn) and VVS (Stuttgart) buses,
  trams and regional trains, with real-time data where the operators send it.
* `naldo` — `https://www.efa-bw.de/naldo/`, the EFA-BW instance naldo.de's journey planner uses (its page proxies
  to it). Same backend and same data as `efa` (checked: identical stops, departures and real-time), offered so the
  model can ask "naldo" explicitly.

Automatic choice: DB first; EFA first when only buses/trams/subways are asked for; an id (`efa:…`) goes to its
provider. When DB fails (blocked, down, timeout) or finds nothing, EFA answers and a `Note:` line says so.
Caches: stop searches 24 h, departures 30 s, journeys 60 s; journey and trip ids stay valid for 6 h. Requests time
out after 15 s, a tool call after 45 s. Tests run against recorded answers (`internal/transit/transittest`), and
an MCP round trip with `internal/mcp`'s client over stdio and HTTP.
