#!/usr/bin/env bash
# Daemon e2e: the one-shot proxy dies, shard daemon takes over; the daemon dies, the world keeps working.
set -euo pipefail
cd "$(dirname "$0")/.."
E2E_LIB_ONLY=1 source ./scripts/e2e.sh

step "host is free"
pgrep -af "[r]unsc" && fail "runsc still runs" || true
ip netns list 2>/dev/null | grep . && fail "netns left over" || true
ss -ltn | grep -E ':80 ' && fail "something on :80" || true
pgrep -af "[s]hard daemon" && fail "a daemon already runs" || true
say "clean"

rm -rf "${SHARD_ROOT}"

PUBLIC_IP=$(curl -fs -4 https://ifconfig.me)
ALLOWED_HOST="${PUBLIC_IP}.nip.io"
say "allowed host ${ALLOWED_HOST}"

start_echo

DAEMON_PID=""
cleanup() {
	local status=$?
	set +e
	[ -n "${DAEMON_PID}" ] && kill "${DAEMON_PID}" 2>/dev/null
	[ -n "${ID:-}" ] && shard stop "${ID}" >/dev/null 2>&1 && shard rm "${ID}" >/dev/null 2>&1
	[ -n "${ID2:-}" ] && shard stop "${ID2}" >/dev/null 2>&1 && shard rm "${ID2}" >/dev/null 2>&1
	P=$(cat "${SHARD_ROOT}/proxy/pid" 2>/dev/null || true)
	[ -n "${P}" ] && kill "${P}" 2>/dev/null
	[ -n "${ECHO_PID:-}" ] && kill "${ECHO_PID}" 2>/dev/null
	sleep 1
	rm -rf "${SHARD_ROOT}"
	exit "${status}"
}
trap cleanup EXIT

proxy_pid() { cat "${SHARD_ROOT}/proxy/pid" 2>/dev/null || true; }

wait_proxy_pid_is() {
	local want="$1" what="$2"
	# The takeover rides the task backoff, which may have grown toward its one-minute cap by then.
	for _ in $(seq 1 450); do
		[ "$(proxy_pid)" = "${want}" ] && kill -0 "${want}" 2>/dev/null && return 0
		sleep 0.2
	done
	fail "${what}: proxy pid is '$(proxy_pid)', want ${want}"
}

step "a fronted create starts a one-shot proxy"
shard policy create --allow "*.nip.io" --deny any daemon-policy
ID=$(shard create --policy daemon-policy alpine:3.20 -- /bin/sleep 600)
ONESHOT_PID=$(proxy_pid)
[ -n "${ONESHOT_PID}" ] && kill -0 "${ONESHOT_PID}" 2>/dev/null || fail "no proxy after the create"
expect "$(fetch "${ID}" "http://${ALLOWED_HOST}/")" "auth= path=/" "the web answers through the one-shot proxy"

step "the daemon waits behind the one-shot proxy"
"${PREFIX}/shard" --root "${SHARD_ROOT}" daemon >"${SHARD_ROOT}/daemon.out" 2>&1 &
DAEMON_PID=$!
sleep 2
kill -0 "${DAEMON_PID}" 2>/dev/null || { cat "${SHARD_ROOT}/daemon.out"; fail "the daemon died instead of waiting"; }
[ "$(proxy_pid)" = "${ONESHOT_PID}" ] || fail "the daemon stole the proxy while the one-shot lives"
say "the daemon runs (pid ${DAEMON_PID}) and the one-shot proxy keeps the lock"

step "the daemon takes over when the one-shot proxy dies"
kill -9 "${ONESHOT_PID}"
wait_proxy_pid_is "${DAEMON_PID}" "the takeover"
say "proxy pid is now the daemon's own pid ${DAEMON_PID}"
expect "$(fetch "${ID}" "http://${ALLOWED_HOST}/")" "auth= path=/" "the web answers through the daemon's proxy"

step "the daemon restarts its proxy after a crash"
# There is no separate process to kill: the proxy is a task inside the daemon. Kill it and start another.
kill -9 "${DAEMON_PID}"
DAEMON_PID=""
sleep 1
"${PREFIX}/shard" --root "${SHARD_ROOT}" daemon >"${SHARD_ROOT}/daemon2.out" 2>&1 &
DAEMON_PID=$!
wait_proxy_pid_is "${DAEMON_PID}" "the restart"
expect "$(fetch "${ID}" "http://${ALLOWED_HOST}/")" "auth= path=/" "the web answers again after the daemon came back"

step "the daemon answers the API on its socket"
SOCK="${SHARD_ROOT}/shard.sock"
api() { curl -s --unix-socket "${SOCK}" -o "${SHARD_ROOT}/api.out" -w '%{http_code}' "http://shard$1"; }
for _ in $(seq 1 50); do
	[ -S "${SOCK}" ] && break
	sleep 0.1
done
[ -S "${SOCK}" ] || { cat "${SHARD_ROOT}/daemon2.out"; fail "no socket at ${SOCK}"; }
MODE=$(stat -c '%a %G' "${SOCK}")
case "${MODE}" in
"660 shard" | "600 root") say "the socket is mode ${MODE}" ;;
*) fail "the socket is mode ${MODE}, want 660 shard or 600 root" ;;
esac
expect "$(api /v0/sandboxes)" "200" "GET /v0/sandboxes answers 200"
jq -e --arg id "${ID}" 'map(.id) | index($id) != null' "${SHARD_ROOT}/api.out" >/dev/null || fail "the list does not hold ${ID}: $(cat "${SHARD_ROOT}/api.out")"
say "the list holds the sandbox the CLI created"
expect "$(api "/v0/sandboxes/${ID}")" "200" "GET /v0/sandboxes/${ID} answers 200"
expect "$(jq -r .state "${SHARD_ROOT}/api.out")" "running" "the sandbox is running over the socket"
expect "$(api /v0/sandboxes/no-such-sandbox)" "404" "an unknown sandbox is a 404"
expect "$(api /v0/version)" "200" "GET /v0/version answers 200"
expect "$(jq -r .version "${SHARD_ROOT}/api.out")" "$(shard version)" "the daemon reports the installed version"

step "verbs work with the daemon dead, and a create refronts"
kill -9 "${DAEMON_PID}"
DAEMON_PID=""
sleep 1
shard ls >/dev/null || fail "ls fails with the daemon dead"
shard inspect "${ID}" >/dev/null || fail "inspect fails with the daemon dead"
DEAD_OUT=$(fetch "${ID}" "http://${ALLOWED_HOST}/")
[ "${DEAD_OUT}" != "auth= path=/" ] || fail "the web still answers with no proxy: fail closed is broken"
say "the proxy is dead, so the sandbox is closed, not open (${DEAD_OUT})"
ID2=$(shard create --policy daemon-policy alpine:3.20 -- /bin/sleep 600)
NEWPID=$(proxy_pid)
[ -n "${NEWPID}" ] && kill -0 "${NEWPID}" 2>/dev/null || fail "the create did not start a one-shot proxy"
expect "$(fetch "${ID2}" "http://${ALLOWED_HOST}/")" "auth= path=/" "a fresh create fronts itself with a one-shot proxy again"
expect "$(fetch "${ID}" "http://${ALLOWED_HOST}/")" "auth= path=/" "the older sandbox rides the new proxy too"

echo
echo "DAEMON-RECOVERY-PASS"
