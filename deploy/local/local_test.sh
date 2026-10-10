#!/usr/bin/env bash
# Tests of local.sh's wait for running work (no server, no worker: the API and the check are faked).
#
#   deploy/local/local_test.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/local-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
export AIC_LOCAL_STATE="$TMP/state" AIC_LOCAL_PORT=1 LOCAL_POLL=0.2
mkdir -p "$AIC_LOCAL_STATE/run"

# shellcheck source=deploy/local/local.sh
source "$HERE/local.sh"
set +e

fails=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fails=$((fails + 1)); }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got [$2], want [$3]"; fi; }

# ---------- dur ----------
check "dur 42" "$(dur 42)" "0:42"
check "dur 725" "$(dur 725)" "12:05"
check "dur 3725" "$(dur 3725)" "1:02:05"

# ---------- running_work: the API's answers → the lines ----------
printf 'tok' >"$STATE/device-token"
curl() { return 0; } # the server's healthz
ago() { date -u -r $(( $(date +%s) - $1 )) '+%Y-%m-%dT%H:%M:%S.123456Z'; }
ago_local() { date -r $(( $(date +%s) - $1 )) '+%Y-%m-%dT%H:%M:%S.5%z' | sed -E 's/([+-][0-9]{2})([0-9]{2})$/\1:\2/'; }
JOBS="[
  {\"id\":\"a\",\"model\":\"qwen3.6-35b-a3b\",\"state\":\"running\",\"created\":\"$(ago 100)\",\"started\":\"$(ago 90)\",\"request\":{\"client\":\"vboard: This Mac (local)\"}},
  {\"id\":\"b\",\"model\":\"flux2-klein-4b\",\"state\":\"queued\",\"created\":\"$(ago_local 30)\",\"request\":{}},
  {\"id\":\"c\",\"model\":\"qwen3.6-35b-a3b\",\"state\":\"done\",\"created\":\"$(ago 999)\",\"started\":\"$(ago 999)\",\"request\":{}}]"
SESSIONS="[
  {\"id\":\"s1\",\"model\":\"sonnet\",\"driver\":\"json\",\"state\":\"busy\",\"lastUsed\":\"$(ago 20)\"},
  {\"id\":\"s2\",\"model\":\"opus\",\"driver\":\"tty\",\"state\":\"idle\",\"lastUsed\":\"$(ago 20)\"}]"
aic() { case "$2" in /api/jobs) printf '%s' "$JOBS" ;; /api/agents/sessions) printf '%s' "$SESSIONS" ;; esac; }
want=$'running\tqwen3.6-35b-a3b\tvboard: This Mac (local)\t90\nqueued\tflux2-klein-4b\t-\t30\nturn\tclaude-code/sonnet\tClaude Code session\t20'
check "running_work: running and queued jobs, the busy turn" "$(running_work)" "$want"
JOBS='[{"id":"c","model":"claude-code/sonnet","state":"running","created":"2026-10-10T10:00:00Z","request":{"client":"vboard: x"}}]'
check "running_work: a turn that is a job is listed once" "$(running_work | cut -f1,2 | tr '\t\n' ' ;')" "running claude-code/sonnet;"
JOBS='[]' SESSIONS='[]'
check "running_work: nothing" "$(running_work)" ""
aic() { return 22; } # 502: no worker connected
check "running_work: no worker" "$(running_work)" ""
curl() { return 7; } # the server does not run
check "running_work: no server" "$(running_work)" ""
unset -f curl aic

# ---------- wait_idle: the poll loop ----------
# fake running_work: busy for the first $BUSY_CALLS calls
running_work() {
  local n; n="$(cat "$TMP/calls" 2>/dev/null || echo 0)"; echo $((n + 1)) >"$TMP/calls"
  (( n < BUSY_CALLS )) && printf 'running\tqwen\tvboard: test\t%s\n' "$n"
  return 0
}
calls() { cat "$TMP/calls" 2>/dev/null || echo 0; }

rm -f "$TMP/calls"; BUSY_CALLS=0
t0=$(date +%s); out="$(wait_idle "restarting server" "make local-up")"; rc=$?
check "idle: returns at once" "$rc/$(calls)/$(( $(date +%s) - t0 < 2 ))" "0/1/1"
check "idle: says nothing" "$out" ""

rm -f "$TMP/calls"; BUSY_CALLS=3
out="$(wait_idle "restarting server" "make local-up")"; rc=$?
check "busy, then done: waits and returns 0" "$rc/$(calls)" "0/4"
grep -q 'done after' <<<"$out" && ok "busy, then done: says so" || bad "busy, then done: $out"

rm -f "$TMP/calls"; BUSY_CALLS=1000
out="$(WAIT_JOBS=1 wait_idle "restarting server" "make local-up")"; rc=$?
check "busy past WAIT_JOBS: returns 1" "$rc" "1"
grep -q 'FORCE=1 make local-up' <<<"$out" && ok "busy past WAIT_JOBS: tells how to force" || bad "timeout message: $out"

rm -f "$TMP/calls"
out="$(FORCE=1 wait_idle "restarting server" "make local-up")"; rc=$?
check "FORCE=1: returns 0 without checking" "$rc/$(calls)" "0/0"

# Ctrl-C: SIGINT during the wait ends the script with 130 before it goes on (job control: a background job gets SIGINT)
rm -f "$TMP/calls"
set -m
( BUSY_CALLS=1000; wait_idle "restarting server" "make local-up"; echo "went on" ) >"$TMP/int.out" 2>&1 &
pid=$!
sleep 1
kill -INT "$pid"
wait "$pid"; rc=$?
set +m
check "Ctrl-C: exit 130" "$rc" "130"
grep -q 'went on' "$TMP/int.out" && bad "Ctrl-C: went on" || ok "Ctrl-C: did not go on"
grep -q 'aborted: nothing was restarted' "$TMP/int.out" && ok "Ctrl-C: says so" || bad "Ctrl-C: $(cat "$TMP/int.out")"

# ---------- LOCAL_FAKE_BUSY ----------
unset -f running_work
source "$HERE/local.sh"; set +e
rm -f "$STATE/device-token"
unset FAKE_T0
out="$(LOCAL_FAKE_BUSY=1 wait_idle "restarting server" "make local-up")"; rc=$?
check "LOCAL_FAKE_BUSY=1: waits about a second, then 0" "$rc" "0"
grep -q 'fake-model' <<<"$out" && ok "LOCAL_FAKE_BUSY: shows the fake job" || bad "LOCAL_FAKE_BUSY: $out"

echo
if (( fails )); then echo "$fails failed"; exit 1; fi
echo "all passed"
