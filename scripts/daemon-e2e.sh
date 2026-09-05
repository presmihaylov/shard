#!/usr/bin/env bash
# SHARD-124: the daemon and the CLI over one root. It starts shard daemon in the background, creates
# a sandbox with the CLI, reads the three routes over the socket with curl, proves the socket mode is
# what the daemon logged, stops the daemon and proves the socket is gone.
#
#   sudo ./scripts/daemon-e2e.sh
#
# Same guards and environment as scripts/e2e.sh, whose helpers it sources. With a shard group on the
# host it expects 0660 root:shard, without one 0600 root.

set -euo pipefail

export PATH="${PATH}:/usr/sbin:/sbin"

SHARD_ROOT=${SHARD_ROOT:-/var/lib/shard-e2e}

HERE=$(cd "$(dirname "$0")" && pwd)
export E2E_LIB_ONLY=1
# shellcheck source=./e2e.sh
. "${HERE}/e2e.sh"
unset E2E_LIB_ONLY

SOCKET="${SHARD_ROOT}/shard.sock"
DAEMON_LOG=""
DAEMON_PID=""

# api reads one route into BODY and API_STATUS; never call it in a subshell, which keeps the status.
api() {
	API_STATUS=$(curl -sS --unix-socket "${SOCKET}" -o "${BODY}" -w '%{http_code}' "http://localhost$1")
}

body() { cat "${BODY}"; }

expect_status() { expect "${API_STATUS}" "$1" "$2"; }

stop_daemon() {
	[ -n "${DAEMON_PID}" ] || return 0
	kill "${DAEMON_PID}" >/dev/null 2>&1 || true
	wait "${DAEMON_PID}" >/dev/null 2>&1 || true
	DAEMON_PID=""
}

daemon_on_exit() {
	local status=$?

	trap - EXIT
	if [ "${status}" -ne 0 ]; then
		report "the command under this step exited non-zero"
		[ -z "${DAEMON_LOG}" ] || cat "${DAEMON_LOG}" >&2 || true
	fi

	stop_daemon
	teardown
	rm -f "${DAEMON_LOG:-/nonexistent}" "${BODY:-/nonexistent}"

	exit "${status}"
}

trap daemon_on_exit EXIT

step "check the host"
[ "$(id -u)" = "0" ] || fail "shard drives netns, nft and runsc, so this needs root"
for binary in runsc ip nft go curl; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
check_host_is_free
check_root
say "runsc, ip, nft, go and curl are on the host, and this run owns ${SHARD_ROOT}"

step "install shard and its guest supervisor"
if [ "${SKIP_INSTALL:-0}" = "1" ]; then
	say "skipped, running against the binaries already in ${PREFIX}"
else
	cd "${HERE}/.."
	BUILD=$(mktemp -d)
	go build -o "${BUILD}/shard" ./cmd/shard
	CGO_ENABLED=0 go build -o "${BUILD}/shard-init" ./cmd/shard-init
	install -m0755 "${BUILD}/shard" "${PREFIX}/shard"
	install -m0755 "${BUILD}/shard-init" "${PREFIX}/shard-init"
	rm -rf "${BUILD}"
	say "installed $("${PREFIX}/shard" version) into ${PREFIX}"
fi

wipe_root
mkdir -p "${SHARD_ROOT}"
DAEMON_LOG=$(mktemp)
BODY=$(mktemp)

step "start the daemon in the background"
"${PREFIX}/shard" --root "${SHARD_ROOT}" daemon >"${DAEMON_LOG}" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 50); do
	[ -S "${SOCKET}" ] && grep -q "api listening on" "${DAEMON_LOG}" && break
	sleep 0.1
done
[ -S "${SOCKET}" ] || fail "no socket at ${SOCKET} after 5s: $(cat "${DAEMON_LOG}")"
LISTEN_LINE=$(grep "api listening on" "${DAEMON_LOG}")
say "the daemon logged: ${LISTEN_LINE#* api }"

step "prove the socket mode is what the daemon claims"
if getent group shard >/dev/null; then
	WANT_MODE="0660"
	WANT_GROUP="shard"
	echo "${LISTEN_LINE}" | grep -q "mode 0660, group shard" || fail "the host has a shard group and the daemon logged '${LISTEN_LINE}'"
else
	WANT_MODE="0600"
	WANT_GROUP="root"
	echo "${LISTEN_LINE}" | grep -q "mode 0600, no shard group" || fail "the host has no shard group and the daemon logged '${LISTEN_LINE}'"
fi
expect "$(stat -c '%a %U:%G' "${SOCKET}")" "${WANT_MODE#0} root:${WANT_GROUP}" "the socket sits at ${WANT_MODE} root:${WANT_GROUP}, as logged"

step "read the version over the socket"
api /v0/version
expect_status 200 "GET /v0/version is 200"
expect "$(body)" "{\"version\":\"$("${PREFIX}/shard" version)\"}" "the daemon reports the version the CLI prints"

step "create a sandbox with the CLI"
ID=$(shard create --name e2e-daemon "${IMAGE}" -- /bin/sleep 600)
RECORD="${SHARD_ROOT}/sandboxes/${ID}/sandbox.json"
LINK=$(grep -o '"host_interface": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
[ "$(listed_state "${ID}")" = "running" ] || fail "shard ls does not list ${ID} running"
say "created ${ID}, named e2e-daemon"

step "list the sandbox over the socket"
api /v0/sandboxes
expect_status 200 "GET /v0/sandboxes is 200"
body | grep -q "\"id\":\"${ID}\"" || fail "the list does not hold ${ID}: $(body)"
body | grep -q '"warnings"' && fail "the list carries warnings: $(body)"
say "the daemon lists the sandbox the CLI created, with no warnings"

step "inspect the sandbox over the socket, by id and by name"
for REF in "${ID}" e2e-daemon; do
	api "/v0/sandboxes/${REF}"
	expect_status 200 "GET /v0/sandboxes/${REF} is 200"
	body | grep -q "\"id\":\"${ID}\"" || fail "GET /v0/sandboxes/${REF} answered $(body)"
	body | grep -q '"state":"running"' || fail "GET /v0/sandboxes/${REF} does not say running: $(body)"
done
say "both references answer the running record"

step "an unknown name is 404"
api /v0/sandboxes/no-such-sandbox
expect_status 404 "GET /v0/sandboxes/no-such-sandbox is 404"
body | grep -q '"error":' || fail "the 404 body is $(body), want an error object"
say "the 404 body is an error object"

step "stop and remove the sandbox"
shard stop --time "${GRACE}" "${ID}" >/dev/null
api /v0/sandboxes
body | grep -q "\"id\":\"${ID}\"" && fail "the stopped sandbox is still listed without all: $(body)"
api '/v0/sandboxes?all=true'
body | grep -q "\"id\":\"${ID}\"" || fail "the stopped sandbox is missing with all=true: $(body)"
say "a stopped sandbox is hidden unless all=true, as shard ls does it"
shard rm "${ID}" >/dev/null
ID=""
LINK=""

step "stop the daemon and prove the socket is gone"
stop_daemon
[ ! -e "${SOCKET}" ] || fail "the socket ${SOCKET} outlived the daemon"
say "the socket is gone"

step "clean up"
teardown
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
say "the run's own root is gone"

trap - EXIT
rm -f "${DAEMON_LOG}" "${BODY}"
echo
echo "daemon e2e PASSED: daemon up, socket ${WANT_MODE} root:${WANT_GROUP}, version, list, inspect, 404, and a socket gone with the daemon"
