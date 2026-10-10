#!/usr/bin/env bash
# The local integration (docs/local.md): this repo's ai-connector server on 127.0.0.1, this Mac as its worker (own
# config and state, the model store shared with ~/.config/mlxlab), a vboard from a local checkout with its test login,
# and a vboard connector offering a local chat model and Claude Code in vboard's chat panel.
#
#   deploy/local/local.sh up | down | status | logs [server|worker|vboard|idp] | open
#
# State (data, logs, pids, password, tokens): $AIC_LOCAL_STATE (default ~/.local/share/ai-connector-local, 0700).
# Settings (environment): AIC_LOCAL_PORT (18484), VBOARD_SRC (~/dev/vboard), VBOARD_PORT (18480), VBOARD_IDP_PORT
# (18482), VBOARD_URL + VBOARD_TOKEN (use a vboard that runs elsewhere instead of starting one), LOCAL_CHAT_MODEL
# (the local chat agent; default qwen3.6-35b-a3b if on disk, else the first chat model on disk), LOCAL_USER (the
# person; default the first word of git's user.name, else $USER), ALLOW_DOWNLOAD=1 (accept a chat model that is
# not on disk yet: it is downloaded on its first use), REBUILD=1 (build everything again).
#
# Running work: before up restarts (or down stops) the server, the worker or vboard, it waits until no job runs or is
# queued and no Claude Code turn is busy (a restart ends a vboard chat with "The connection to vboard was lost").
# WAIT_JOBS (seconds, default 600) bounds the wait, FORCE=1 skips it, NO_RESTART=1 builds without restarting.
# LOCAL_FAKE_BUSY=<seconds> (tests) reports a made-up running job for that long, LOCAL_POLL the poll interval (2 s).
set -euo pipefail
umask 077

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
STATE="${AIC_LOCAL_STATE:-$HOME/.local/share/ai-connector-local}"
PORT="${AIC_LOCAL_PORT:-18484}"
VBOARD_SRC="${VBOARD_SRC:-$HOME/dev/vboard}"
VB_PORT="${VBOARD_PORT:-18480}"
IDP_PORT="${VBOARD_IDP_PORT:-18482}"
AIC_URL="http://127.0.0.1:$PORT"
CONSOLE_URL="http://localhost:$PORT/console/"
EXTERNAL_VB="${VBOARD_URL:-}"
VB_URL="${EXTERNAL_VB:-http://localhost:$VB_PORT}"
VB_URL="${VB_URL%/}"
IDP_URL="http://localhost:$IDP_PORT"
CONNECTOR_NAME="${CONNECTOR_NAME:-This Mac (local)}"

LOGS="$STATE/logs"
RUN="$STATE/run"
AIC_BIN="$REPO/bin/ai-connector"
SRV_BIN="$REPO/bin/ai-connector-server"
VB_DIR="$STATE/vboard"
VB_BIN="$VB_DIR/bin/vboard"

say() { printf '%s\n' "$*"; }
step() { printf '\033[1m→ %s\033[0m\n' "$*"; }
die() { printf 'local.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is needed ($2)"; }

user_name() {
  if [[ -n "${LOCAL_USER:-}" ]]; then printf '%s' "$LOCAL_USER"; return; fi
  local n
  n="$(git config user.name 2>/dev/null | awk '{print $1}')"
  printf '%s' "${n:-$USER}"
}
USER_NAME="$(user_name)"
USER_SUB="$(printf '%s' "$USER_NAME" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9._-')"
USER_FULL="$(git config user.name 2>/dev/null || true)"
USER_FULL="${USER_FULL:-$USER_NAME}"

# ---------- processes: pid files in $RUN, each checked against its command line ----------

pid_of() { # name → the pid if it runs and is ours
  local f="$RUN/$1.pid" pid
  [[ -f "$f" ]] || return 1
  pid="$(cat "$f")"
  [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || return 1
  ps -o command= -p "$pid" 2>/dev/null | grep -qF "$(cat "$RUN/$1.cmd" 2>/dev/null || echo "\0")" || return 1
  printf '%s' "$pid"
}

start_bg() { # name match-string command… (env set by the caller with env …)
  local name="$1" match="$2"; shift 2
  nohup "$@" </dev/null >>"$LOGS/$name.log" 2>&1 &
  local pid=$!
  printf '%s' "$pid" >"$RUN/$name.pid"
  printf '%s' "$match" >"$RUN/$name.cmd"
  cat "$(build_file "$name")" >"$RUN/$name.build" 2>/dev/null || true # the build it runs (see stale)
  printf '\n===== %s started (pid %s) =====\n' "$(date '+%F %T')" "$pid" >>"$LOGS/$name.log"
}

stop_one() { # name [seconds to wait]
  local name="$1" wait="${2:-10}" pid
  if pid="$(pid_of "$name")"; then
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 $((wait * 5))); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
    if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid" 2>/dev/null || true; say "  $name ($pid) killed"; else say "  $name ($pid) stopped"; fi
  fi
  rm -f "$RUN/$name.pid" "$RUN/$name.cmd" "$RUN/$name.build"
}

port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

wait_url() { # url [seconds]
  for _ in $(seq 1 $(( ${2:-30} * 5 ))); do curl -fsS -o /dev/null "$1" 2>/dev/null && return 0; sleep 0.2; done
  return 1
}

# ---------- ai-connector API helpers ----------

dev_token() { cat "$STATE/device-token" 2>/dev/null; }
aic() { # method path [json]
  local m="$1" p="$2"; shift 2
  curl -fsS -X "$m" -H "Authorization: Bearer $(dev_token)" -H 'Content-Type: application/json' "$@" "$AIC_URL$p"
}

# ---------- builds: which running process runs an older build ----------
# A build writes its id ("<version> <time>") to $STATE/build-{aic,vboard}.id; start_bg copies it to $RUN/<name>.build.
# A process whose .build differs runs an older build and needs a restart — also when an earlier up built but did not
# restart (it waited and gave up, Ctrl-C, NO_RESTART=1).

build_file() { case "$1" in server | worker) printf '%s' "$STATE/build-aic.id" ;; *) printf '%s' "$STATE/build-vboard.id" ;; esac; }

new_build_id() { printf '%s %s' "$2" "$(date +%s)" >"$STATE/build-$1.id"; }

# adopt_builds: processes started before build ids existed run the build that is recorded as built.
adopt_builds() {
  local c n
  for c in aic vboard; do
    [[ -f "$STATE/build-$c.id" || ! -f "$STATE/built-$c" ]] || new_build_id "$c" "$(cat "$STATE/built-$c")"
  done
  for n in server worker vboard idp; do
    if pid_of "$n" >/dev/null && [[ ! -f "$RUN/$n.build" ]]; then cat "$(build_file "$n")" >"$RUN/$n.build" 2>/dev/null || true; fi
  done
}

stale() { # name → true when it runs an older build
  pid_of "$1" >/dev/null && [[ "$(cat "$RUN/$1.build" 2>/dev/null)" != "$(cat "$(build_file "$1")" 2>/dev/null)" ]]
}

stale_list() { # the processes to restart: "server worker vboard" or fewer
  local out=()
  stale server && out+=(server)
  stale worker && out+=(worker)
  if [[ -z "$EXTERNAL_VB" ]] && { stale vboard || stale idp; }; then out+=(vboard); fi
  printf '%s' "${out[*]:-}"
}

RESTART="" # what up restarts (after the wait)
restarting() { [[ " $RESTART " == *" $1 "* ]]; }

# ---------- running work: jobs on the worker, busy Claude Code turns ----------

# ts: a Go time (RFC 3339 with fractions and an offset) in epoch seconds; age: seconds since then.
RUNNING_JQ='
def ts: if (. // "") == "" or startswith("0001") then null else
  (capture("^(?<d>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\\.[0-9]+)?(?<z>Z|[+-][0-9]{2}:[0-9]{2})$") // null) as $c
  | if $c == null then null else ($c.d + "Z" | fromdateiso8601) -
      (if $c.z == "Z" then 0 else ($c.z[1:3] | tonumber) * 3600 + ($c.z[4:6] | tonumber) * 60
         | if $c.z[0:1] == "-" then -. else . end end) end end;
def age: (ts // now) | now - . | floor | if . < 0 then 0 else . end;
(.[0] // []) as $jobs | (.[1] // []) as $sess
| ($jobs | map(select(.state == "running" or .state == "queued"))) as $run
| ($run[] | [.state, .model, (.request.client // "-"), ((.started // .created) | age)] | @tsv),
  ($sess[] | select(.state == "busy")
   | select(.driver == "tty" or ($run | any(.model | startswith("claude-code/")) | not))
   | ["turn", (.model | if startswith("claude-code/") then . else "claude-code/" + . end),
      (if .driver == "tty" then "terminal session" else "Claude Code session" end), (.lastUsed | age)] | @tsv)'

# running_work prints one line per running or queued job and busy Claude Code turn: state, model, client, seconds
# (tab-separated). Nothing when nothing runs, when the server does not run or no worker is connected.
running_work() {
  if [[ -n "${LOCAL_FAKE_BUSY:-}" ]]; then
    local el=$(( $(date +%s) - ${FAKE_T0:-$(date +%s)} ))
    (( el >= LOCAL_FAKE_BUSY )) || printf 'running\tfake-model\tvboard: fake (LOCAL_FAKE_BUSY)\t%s\n' "$el"
  fi
  [[ -s "$STATE/device-token" ]] || return 0
  curl -fsS -m 2 -o /dev/null "$AIC_URL/healthz" 2>/dev/null || return 0
  { aic GET /api/jobs -m 5 2>/dev/null || printf '[]'; printf '\n'
    aic GET /api/agents/sessions -m 5 2>/dev/null || printf '[]'; } | jq -rs "$RUNNING_JQ" 2>/dev/null || true
}

dur() { # seconds → 0:42, 12:05, 1:02:05
  local s="$1"
  if (( s >= 3600 )); then printf '%d:%02d:%02d' $((s / 3600)) $((s % 3600 / 60)) $((s % 60)); else printf '%d:%02d' $((s / 60)) $((s % 60)); fi
}

work_summary() { # running_work's lines → "2 running (longest 3:12: qwen… for vboard: …)"
  local n longest m c s
  n="$(grep -c . <<<"$1")"
  longest="$(sort -t $'\t' -k4,4nr <<<"$1" | head -n1)"
  IFS=$'\t' read -r _ m c s <<<"$longest"
  printf '%s running (longest %s: %s for %s)' "$n" "$(dur "$s")" "$m" "$c"
}

print_work() {
  local st m c s
  while IFS=$'\t' read -r st m c s; do
    printf '  %-8s %-28s %-36s %s\n' "$st" "$m" "$c" "$(dur "$s")"
  done <<<"$1"
}

# wait_idle waits until no work runs before "what" (e.g. "restarting server worker"): at once when nothing runs or
# FORCE=1; polls every LOCAL_POLL (2) s up to WAIT_JOBS (600) s. 1 when the work still runs then. Ctrl-C exits (130)
# before anything was restarted or stopped.
wait_idle() {
  local what="$1" retry="$2" work
  if [[ "${FORCE:-0}" == 1 ]]; then say "  FORCE=1: $what without checking for running work"; return 0; fi
  FAKE_T0="${FAKE_T0:-$(date +%s)}"
  work="$(running_work)"
  [[ -n "$work" ]] || return 0
  local limit="${WAIT_JOBS:-600}" poll="${LOCAL_POLL:-2}" t0 el n=0 tty=0
  [[ -t 1 ]] && tty=1
  step "work is running: waiting before $what"
  print_work "$work"
  say "  Ctrl-C aborts (nothing is restarted or stopped); FORCE=1 $retry does it now and ends the work"
  trap 'printf "\n"; say "aborted: nothing was restarted or stopped"; exit 130' INT
  t0=$(date +%s)
  while :; do
    el=$(( $(date +%s) - t0 ))
    if (( el >= limit )); then
      (( tty )) && printf '\n'
      trap - INT
      say "still running after $(dur "$el"): $(work_summary "$work")"
      say "nothing was restarted or stopped. Wait and run it again, WAIT_JOBS=<seconds> to wait longer, or"
      say "FORCE=1 $retry to do it now (ends the running work)."
      return 1
    fi
    if (( tty )); then
      printf '\r\033[K  waiting %s (up to %s): %s' "$(dur "$el")" "$(dur "$limit")" "$(work_summary "$work")"
    elif (( n % 15 == 0 )); then
      say "  waiting $(dur "$el") (up to $(dur "$limit")): $(work_summary "$work")"
    fi
    n=$((n + 1))
    sleep "$poll"
    work="$(running_work)"
    if [[ -z "$work" ]]; then
      (( tty )) && printf '\n'
      trap - INT
      say "  done after $(dur $(( $(date +%s) - t0 ))): nothing runs any more"
      return 0
    fi
  done
}

# ---------- up ----------

build_aic() {
  local v; v="$(git -C "$REPO" describe --always --dirty 2>/dev/null || echo dev)"
  if [[ "${REBUILD:-0}" != 1 && -x "$AIC_BIN" && -x "$SRV_BIN" && "$(cat "$STATE/built-aic" 2>/dev/null)" == "$v" && "$v" != *-dirty ]]; then
    say "  ai-connector $v already built"
    return
  fi
  step "building ai-connector $v (make build server)"
  make -C "$REPO" build server >"$LOGS/build-aic.log" 2>&1 || { tail -30 "$LOGS/build-aic.log"; die "build failed (log: $LOGS/build-aic.log)"; }
  printf '%s' "$v" >"$STATE/built-aic"
  new_build_id aic "$v" # running server and worker are restarted with the new build (stale)
}

build_vboard() {
  [[ -d "$VBOARD_SRC/backend" ]] || die "no vboard checkout at $VBOARD_SRC (VBOARD_SRC=…)"
  local v
  v="$(git -C "$VBOARD_SRC" rev-parse --short HEAD 2>/dev/null || echo dev)"
  [[ -z "$(git -C "$VBOARD_SRC" status --porcelain --untracked-files=no 2>/dev/null)" ]] || v="$v-dirty"
  if [[ "${REBUILD:-0}" != 1 && -x "$VB_BIN" && -f "$VB_DIR/web/index.html" && "$(cat "$STATE/built-vboard" 2>/dev/null)" == "$v" && "$v" != *-dirty ]]; then
    say "  vboard $v already built"
    return
  fi
  step "building vboard $v from $VBOARD_SRC (web app and server)"
  # the checkout is only read and built (frontend/dist, node_modules): no git command changes it
  (cd "$VBOARD_SRC/frontend" && pnpm install --frozen-lockfile && pnpm build) >"$LOGS/build-vboard.log" 2>&1 ||
    { tail -30 "$LOGS/build-vboard.log"; die "vboard web app build failed (log: $LOGS/build-vboard.log)"; }
  mkdir -p "$VB_DIR/bin"
  # the version package's import path comes from vboard's own go.mod
  local mod
  mod="$(go -C "$VBOARD_SRC/backend" list -m 2>>"$LOGS/build-vboard.log")" && [[ -n "$mod" ]] ||
    { tail -30 "$LOGS/build-vboard.log"; die "vboard: no Go module in $VBOARD_SRC/backend (log: $LOGS/build-vboard.log)"; }
  CGO_ENABLED=0 go -C "$VBOARD_SRC/backend" build -trimpath -buildvcs=false \
    -ldflags "-X $mod/internal/version.Version=local -X $mod/internal/version.Commit=$v" \
    -o "$VB_BIN" ./cmd/vboard >>"$LOGS/build-vboard.log" 2>&1 ||
    { tail -30 "$LOGS/build-vboard.log"; die "vboard build failed (log: $LOGS/build-vboard.log)"; }
  # the web app copied next to the binary: a later build in the checkout does not change the running one
  rm -rf "$VB_DIR/web.new" && cp -R "$VBOARD_SRC/frontend/dist" "$VB_DIR/web.new" && rm -rf "$VB_DIR/web" && mv "$VB_DIR/web.new" "$VB_DIR/web"
  mkdir -p "$VB_DIR/manual"
  if [[ -f "$VBOARD_SRC/manual/dist/index.html" ]]; then rm -rf "$VB_DIR/manual" && cp -R "$VBOARD_SRC/manual/dist" "$VB_DIR/manual"; fi
  printf '%s' "$v" >"$STATE/built-vboard"
  new_build_id vboard "$v"
}

start_server() {
  if restarting server; then say "  restarting the server (new build)"; stop_one server 10; fi
  if pid_of server >/dev/null && curl -fsS -o /dev/null "$AIC_URL/healthz" 2>/dev/null; then
    say "  ai-connector server runs ($AIC_URL)"
    return
  fi
  port_busy "$PORT" && die "port $PORT is used by another program (AIC_LOCAL_PORT=…)"
  [[ -f "$STATE/password" ]] || openssl rand -base64 18 | tr -d '/+=' >"$STATE/password"
  mkdir -p "$STATE/server/data" "$STATE/server/tools"
  step "starting the ai-connector server on $AIC_URL"
  start_bg server "$SRV_BIN" env MLXLAB_PASSWORD="$(cat "$STATE/password")" MLXLAB_DATA="$STATE/server/data" \
    MLXLAB_ADDR="127.0.0.1:$PORT" MLXLAB_TOOLS="$STATE/server/tools" MLXLAB_CONSOLE_HOSTS=localhost "$SRV_BIN"
  wait_url "$AIC_URL/healthz" 30 || { tail -20 "$LOGS/server.log"; die "the server did not come up"; }
}

sign_in() {
  if [[ -s "$STATE/device-token" ]] && aic GET /api/workers -o /dev/null 2>/dev/null; then
    return
  fi
  step "signing in a device for $USER_NAME"
  local body tok
  body="$(jq -n --arg p "$(cat "$STATE/password")" --arg u "$USER_NAME" '{password:$p, device:"local.sh", user:$u}')"
  tok="$(curl -fsS -H 'Content-Type: application/json' -d "$body" "$AIC_URL/api/login" | jq -r .token)"
  [[ -n "$tok" && "$tok" != null ]] || die "sign-in failed"
  printf '%s' "$tok" >"$STATE/device-token"
}

start_worker() {
  mkdir -p "$STATE/worker"
  if restarting worker; then say "  restarting the worker (new build)"; stop_one worker 30; fi
  if pid_of worker >/dev/null; then
    say "  worker runs"
    return
  fi
  local wid; wid="$(cat "$STATE/worker-id" 2>/dev/null || true)"
  if [[ ! -s "$STATE/worker.json" || -z "$wid" ]] || ! aic GET /api/workers | jq -e --arg id "$wid" 'any(.[]; .id == $id)' >/dev/null; then
    step "enrolling this Mac as a worker (owner $USER_NAME)"
    local res tok
    res="$(aic POST /api/workers -d "$(jq -n --arg n "$(scutil --get ComputerName 2>/dev/null || hostname -s) (local)" '{name:$n}')")"
    tok="$(jq -r .token <<<"$res")"
    jq -r .worker.id <<<"$res" >"$STATE/worker-id"
    jq -n --arg h "$AIC_URL" --arg t "$tok" '{hub:$h, token:$t}' >"$STATE/worker.json"
    chmod 600 "$STATE/worker.json"
  fi
  step "starting the worker (config $STATE/worker.json, state $STATE/worker, models shared from ~/.config/mlxlab)"
  # MLXLAB_HOME stays unset: the config dir (models, envs, lib) is the one the other mlxlab worker uses, read and shared
  start_bg worker "$AIC_BIN worker --config $STATE/worker.json" \
    env -u MLXLAB_HUB -u MLXLAB_WORKER_TOKEN -u MLXLAB_HOME "$AIC_BIN" worker --config "$STATE/worker.json" --state "$STATE/worker"
  for _ in $(seq 1 75); do
    aic GET /api/workers 2>/dev/null | jq -e --arg id "$(cat "$STATE/worker-id")" 'any(.[]; .id == $id and .connected)' >/dev/null && return
    sleep 0.2
  done
  tail -20 "$LOGS/worker.log"
  die "the worker did not connect"
}

start_vboard() {
  if [[ -n "$EXTERNAL_VB" ]]; then
    say "  vboard: $VB_URL (VBOARD_URL; not started here)"
    return
  fi
  if restarting vboard; then say "  restarting vboard (new build)"; stop_one vboard 5 >/dev/null; stop_one idp 2 >/dev/null; fi
  if pid_of vboard >/dev/null && curl -fsS -o /dev/null "$VB_URL/healthz" 2>/dev/null; then
    pid_of idp >/dev/null || start_idp # the test login alone: a running vboard (and its chats) stays
    say "  vboard runs ($VB_URL)"
    return
  fi
  stop_one vboard 5 >/dev/null; stop_one idp 2 >/dev/null
  port_busy "$VB_PORT" && die "port $VB_PORT is used by another program (VBOARD_PORT=…)"
  mkdir -p "$VB_DIR/data"
  step "starting vboard on $VB_URL (its test login offers $USER_FULL)"
  start_idp
  start_bg vboard "$VB_BIN" env -u DATABASE_URL -u BLOB_STORE \
    HOST=127.0.0.1 PORT="$VB_PORT" DATA_DIR="$VB_DIR/data" STATIC_DIR="$VB_DIR/web" MANUAL_DIR="$VB_DIR/manual" \
    AUTH_MODE=oidc AUTH_OIDC_ISSUER="$IDP_URL" AUTH_OIDC_CLIENT_ID=vboard AUTH_OIDC_CLIENT_SECRET=local \
    AUTH_OIDC_NAME="Local login" AUTH_ADMIN_GROUPS=admins AUTH_BASE_URL="$VB_URL" LOG_LEVEL=info "$VB_BIN"
  wait_url "$VB_URL/healthz" 60 || { tail -20 "$LOGS/vboard.log"; die "vboard did not come up"; }
}

start_idp() {
  port_busy "$IDP_PORT" && die "port $IDP_PORT is used by another program (VBOARD_IDP_PORT=…)"
  local users
  users="$(jq -cn --arg s "$USER_SUB" --arg n "$USER_FULL" '[{sub:$s, name:$n, email:($s+"@localhost"), groups:["admins"], note:"this Mac", color:"#6E56CF"}]')"
  start_bg idp "$VB_BIN test-idp" env TEST_IDP_HOST=127.0.0.1 TEST_IDP_PORT="$IDP_PORT" TEST_IDP_USERS="$users" "$VB_BIN" test-idp
  wait_url "$IDP_URL/.well-known/openid-configuration" 20 || { tail -20 "$LOGS/idp.log"; die "vboard's test login did not come up"; }
}

vb() { curl -fsS -H "Authorization: Bearer $(cat "$STATE/vboard-token")" -H 'Content-Type: application/json' "$@"; }

vboard_token() {
  if [[ -n "$EXTERNAL_VB" ]]; then
    [[ -n "${VBOARD_TOKEN:-}" ]] || [[ -s "$STATE/vboard-token" ]] ||
      die "VBOARD_URL needs VBOARD_TOKEN: create a personal access token (write) in that vboard (Settings → Access tokens)"
    [[ -n "${VBOARD_TOKEN:-}" ]] && printf '%s' "$VBOARD_TOKEN" >"$STATE/vboard-token"
    vb -o /dev/null "$VB_URL/api/v1/me" || die "vboard refuses the token"
    return
  fi
  if [[ -s "$STATE/vboard-token" ]] && vb -o /dev/null "$VB_URL/api/v1/me" 2>/dev/null; then
    return
  fi
  step "creating a vboard access token for $USER_FULL (through the test login)"
  local jar="$RUN/vboard-cookies" page authz req tok
  rm -f "$jar"
  page="$(curl -sS -c "$jar" -b "$jar" -L -w '\n%{url_effective}' "$VB_URL/auth/login/oidc")"
  authz="$(printf '%s' "$page" | tail -n1)"
  req="$(printf '%s' "$page" | sed -n 's/.*name="req" value="\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$req" ]] || die "vboard login: no login page (got $authz)"
  curl -sS -o /dev/null -c "$jar" -b "$jar" -L --data-urlencode "req=$req" --data-urlencode "user=$USER_SUB" "${authz%%\?*}"
  tok="$(curl -fsS -b "$jar" -H 'Content-Type: application/json' \
    -d '{"name":"ai-connector (local)","scope":"write","expiresInDays":365}' "$VB_URL/api/v1/me/tokens" | jq -r .token)"
  rm -f "$jar"
  [[ -n "$tok" && "$tok" != null ]] || die "vboard token creation failed"
  printf '%s' "$tok" >"$STATE/vboard-token"
}

vboard_board() {
  local id; id="$(cat "$STATE/vboard-board" 2>/dev/null || true)"
  if [[ -n "$id" ]] && vb -o /dev/null "$VB_URL/api/v1/boards/$id" 2>/dev/null; then return; fi
  id="$(vb -d '{"name":"Local AI"}' "$VB_URL/api/v1/boards" | jq -r .id)"
  [[ -n "$id" && "$id" != null ]] || die "board creation failed"
  printf '%s' "$id" >"$STATE/vboard-board"
}

chat_model() { # the local chat agent's model: on disk, never a silent download
  local lst
  lst="$(MLXLAB_STATE="$STATE/list-state" env -u MLXLAB_HOME "$AIC_BIN" list 2>/dev/null)"
  if [[ -n "${LOCAL_CHAT_MODEL:-}" ]]; then
    if awk -v m="$LOCAL_CHAT_MODEL" '$1==m && $0 !~ /↓/' <<<"$lst" | grep -q . || [[ "${ALLOW_DOWNLOAD:-0}" == 1 ]]; then
      printf '%s' "$LOCAL_CHAT_MODEL"; return
    fi
    die "$LOCAL_CHAT_MODEL is not on this Mac (ALLOW_DOWNLOAD=1 accepts a download on its first use)"
  fi
  local here
  here="$(awk 'NR>1 && $2=="chat" && $3!="claude-code" && $0 !~ /↓/ {print $1}' <<<"$lst")"
  if grep -qx 'qwen3.6-35b-a3b' <<<"$here"; then printf 'qwen3.6-35b-a3b'; return; fi
  head -n1 <<<"$here"
}

# ---------- LM Studio: Qwen with images ----------
# The built-in MLX engine runs Qwen 3.6 text-only (it skips the vision tower); LM Studio runs the same weights with
# vision. up starts LM Studio's server when it is off (and down stops it again only then); the worker finds its models
# by itself (MLXLAB_RUNTIMES unset: LM Studio on :1234). LMSTUDIO_VISION=0 leaves it out, LMSTUDIO_VISION_MODEL picks
# the model (an LM Studio id of type vlm; default qwen3.6-35b-a3b, else the first vlm).
LMS="${LMS:-$HOME/.lmstudio/bin/lms}"
LMS_URL="http://127.0.0.1:1234"

start_lmstudio() {
  [[ "${LMSTUDIO_VISION:-1}" != 0 && -x "$LMS" ]] || return 0
  if ! curl -fsS -m 2 "$LMS_URL/v1/models" >/dev/null 2>&1; then
    step "starting LM Studio's server (for Qwen with images)"
    "$LMS" server start >>"$LOGS/lmstudio.log" 2>&1 || { say "  LM Studio's server did not start (see $LOGS/lmstudio.log): no vision agent"; return 0; }
    touch "$RUN/lmstudio-started"
    wait_url "$LMS_URL/v1/models" 20 || true
  fi
}

# vision_model prints LM Studio's id of the vision model to offer ("" when there is none).
vision_model() {
  [[ "${LMSTUDIO_VISION:-1}" != 0 ]] || return 0
  local vlms want="${LMSTUDIO_VISION_MODEL:-qwen3.6-35b-a3b}"
  vlms="$(curl -fsS -m 3 "$LMS_URL/api/v0/models" 2>/dev/null | jq -r '.data[] | select(.type == "vlm") | .id' 2>/dev/null)" || return 0
  if grep -qx "$want" <<<"$vlms"; then printf '%s' "$want"; else head -n1 <<<"$vlms"; fi
}

# wait_model waits until a worker offers the model (the worker asks its runtimes every 30 s; the server's refresh
# asks at once).
wait_model() {
  for _ in $(seq 1 40); do
    aic GET '/api/models?refresh=1' 2>/dev/null | jq -e --arg m "$1" 'any(.models[]; .id == $m)' >/dev/null && return 0
    sleep 1
  done
  return 1
}

connector() {
  local model agents body cid vlm
  model="$(chat_model)"
  agents='[]'
  [[ -n "$model" ]] && agents="$(jq -cn --arg m "$model" '[{model:$m}]')"
  vlm="$(vision_model)"
  if [[ -n "$model" && -n "$vlm" ]]; then
    local vid; vid="$(tr '[:upper:]' '[:lower:]' <<<"lmstudio/$vlm")"
    if wait_model "$vid"; then
      agents="$(jq -c --arg m "$vid" --arg n "$vlm · vision (LM Studio)" '. + [{model:$m, name:$n}]' <<<"$agents")"
    else
      say "  the worker does not offer $vid yet: no vision agent this time (local.sh up again adds it)"
    fi
  fi
  if command -v claude >/dev/null 2>&1; then
    agents="$(jq -c '. + [{model:"claude-code/opus", name:"Claude Code (Opus)"}, {model:"claude-code/sonnet", name:"Claude Code (Sonnet)"}, {model:"claude-code/haiku", name:"Claude Code (Haiku)"}]' <<<"$agents")"
  fi
  [[ -n "$model" ]] || say "  no local chat model on this Mac: the connector offers the server's default (LOCAL_CHAT_MODEL=… ALLOW_DOWNLOAD=1 to pick one)"
  if [[ -z "$model" ]]; then agents='[]'; fi # the first agent must be a model
  cid="$(aic GET /api/connectors | jq -r --arg u "$VB_URL" '[.[] | select(.url == $u)][0].id // empty')"
  body="$(jq -n --arg n "$CONNECTOR_NAME" --arg u "$VB_URL" --arg t "$(cat "$STATE/vboard-token")" --argjson a "$agents" \
    '{name:$n, url:$u, token:$t, agents:$a, enabled:true}')"
  if [[ -n "$cid" ]]; then
    step "updating the vboard connector (agents: $(jq -r 'map(.model) | join(", ")' <<<"$agents"))"
    aic PATCH "/api/connectors/$cid" -d "$body" -o /dev/null
  else
    step "creating the vboard connector (agents: $(jq -r 'map(.model) | join(", ")' <<<"$agents"))"
    aic POST /api/connectors -d "$body" -o /dev/null
  fi
  for _ in $(seq 1 50); do
    [[ "$(aic GET /api/connectors | jq -r --arg u "$VB_URL" '[.[] | select(.url == $u)][0].state')" == connected ]] && return
    sleep 0.2
  done
  say "  the connector is not connected yet (local.sh status shows why)"
}

cmd_up() {
  need jq "brew install jq"; need curl ""; need go "Go"; need pnpm "pnpm"; need openssl ""
  mkdir -p "$STATE" "$LOGS" "$RUN"
  chmod 700 "$STATE"
  adopt_builds
  build_aic
  [[ -n "$EXTERNAL_VB" ]] || build_vboard
  # restarted after the builds and only after the work that runs is done (a restart ends a running vboard chat)
  local stale; stale="$(stale_list)"
  if [[ -n "$stale" && "${NO_RESTART:-0}" == 1 ]]; then
    say "  NO_RESTART=1: built, not restarted: ${stale// /, } (an older build runs; make local-up restarts it)"
  elif [[ -n "$stale" ]]; then
    wait_idle "restarting ${stale// /, }" "make local-up" || exit 1
    RESTART="$stale"
  fi
  start_server
  sign_in
  start_lmstudio
  start_worker
  start_vboard
  vboard_token
  vboard_board
  connector
  say ""
  cmd_status
  say ""
  say "Console:  $CONSOLE_URL   (password: $(cat "$STATE/password"))"
  say "vboard:   $VB_URL/b/$(cat "$STATE/vboard-board")"
  [[ -n "$EXTERNAL_VB" ]] || say "          sign in: \"Local login\" → $USER_FULL"
  say "          then the board's AI menu → \"Chat with your local AI\" (⇧I), and pick the agent at the top of the panel"
  say "Stop:     make local-down (the data stays in $STATE)"
}

# ---------- down, status, logs, open ----------

cmd_down() {
  local n any=0
  for n in server worker vboard idp; do pid_of "$n" >/dev/null && any=1; done
  if (( any )); then wait_idle "stopping" "make local-down" || exit 1; fi
  say "stopping the local integration (data stays in $STATE)"
  stop_one worker 30   # it unloads its models and ends its Claude Code sessions first
  stop_one vboard 15
  stop_one idp 3
  stop_one server 10
  if [[ -f "$RUN/lmstudio-started" && -x "$LMS" ]]; then # only when up started it
    say "stopping LM Studio's server (started by local.sh up)"
    "$LMS" unload --all >>"$LOGS/lmstudio.log" 2>&1 || true
    "$LMS" server stop >>"$LOGS/lmstudio.log" 2>&1 || true
    rm -f "$RUN/lmstudio-started"
  fi
}

cmd_status() {
  local n pid
  for n in server worker vboard idp; do
    if pid="$(pid_of "$n")"; then printf '  %-7s running (pid %s)\n' "$n" "$pid"; else printf '  %-7s stopped\n' "$n"; fi
  done
  if curl -fsS -o /dev/null "$AIC_URL/healthz" 2>/dev/null && [[ -s "$STATE/device-token" ]]; then
    say "  server   $AIC_URL ($CONSOLE_URL)"
    aic GET /api/workers 2>/dev/null | jq -r '.[] | "  worker   \(.name)  owner=\(.owner // "-")  connected=\(.connected)"' || true
    local work; work="$(running_work)"
    if [[ -n "$work" ]]; then say "  work     $(work_summary "$work")"; print_work "$work" | sed 's/^/         /'
    else say "  work     nothing running (up restarts and down stops at once)"; fi
    aic GET /api/connectors 2>/dev/null | jq -r '.[] | "  connector \(.name) → \(.url)  state=\(.state)\(if (.error // "") != "" then " (\(.error))" else "" end)  user=\(.user // "-")",
      "            agents: \([.agents[] | .model] | join(", "))",
      "            offered now: \([.advertised[]? | "\(.id) (\(.kind) on \(.worker))"] | join(", "))"' || true
  else
    say "  server   not reachable on $AIC_URL"
  fi
  if curl -fsS -o /dev/null "$VB_URL/healthz" 2>/dev/null; then say "  vboard   $VB_URL"; else say "  vboard   not reachable on $VB_URL"; fi
  if curl -fsS -m 2 -o /dev/null "$LMS_URL/v1/models" 2>/dev/null; then
    local by="already running"; [[ -f "$RUN/lmstudio-started" ]] && by="started by local.sh"
    say "  lmstudio $LMS_URL ($by; vision model: $(vision_model || true))"
  else
    say "  lmstudio not running (no Qwen with images)"
  fi
  # the advertised agents and whether they take images
  aic GET /api/connectors 2>/dev/null | jq -r '.[] | "            images: \([.advertised[]? | select(.images == true) | .id] | join(", "))"' 2>/dev/null || true
}

cmd_logs() {
  local which="${1:-}"
  if [[ -n "$which" ]]; then tail -n 60 -F "$LOGS/$which.log"; else tail -n 20 -F "$LOGS"/{server,worker,vboard,idp}.log; fi
}

cmd_open() {
  open "$CONSOLE_URL"
  [[ -s "$STATE/vboard-board" ]] && open "$VB_URL/b/$(cat "$STATE/vboard-board")" || open "$VB_URL"
}

[[ "${BASH_SOURCE[0]}" == "$0" ]] || return 0 # sourced (deploy/local/local_test.sh): the functions only

case "${1:-}" in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  logs) shift; cmd_logs "${1:-}" ;;
  open) cmd_open ;;
  *) say "usage: $0 up|down|status|logs [server|worker|vboard|idp]|open"; exit 2 ;;
esac
