#!/usr/bin/env bash
# The self-test for scripts/e2e.sh. It sources the helpers and drives the guards the real run cannot
# be asked to prove: a wrong answer from one of them costs the production root, a live sandbox on the
# host, or a green run over a command that failed.
#
#   ./scripts/e2e_test.sh
#
# It needs no root, no runsc and no network, so make check runs it on any host.

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)

FAILURES=0

# check compares one answer and keeps going, so a red run names every guard that broke.
check() {
	local note="$1" got="$2" want="$3"

	if [ "${got}" = "${want}" ]; then
		echo "   ok: ${note}"

		return
	fi

	echo "  BAD: ${note}: got '${got}', want '${want}'" >&2
	FAILURES=$((FAILURES + 1))
}

export E2E_LIB_ONLY=1
# shellcheck source=./e2e.sh
. "${HERE}/e2e.sh"
unset E2E_LIB_ONLY

# The script runs under set -e; the test does not, because it reads the status of every subject.
set +e

echo "== normalise folds a path to one spelling"
check "a trailing slash" "$(normalise /var/lib/shard/)" "/var/lib/shard"
check "a dot segment" "$(normalise /var/lib/./shard)" "/var/lib/shard"
check "a parent segment" "$(normalise /var/lib/shard/../shard)" "/var/lib/shard"
check "a repeated slash" "$(normalise //var//lib//shard)" "/var/lib/shard"
check "the filesystem root" "$(normalise /)" "/"

# rootVerdict answers 'refused' when check_root rejected the root, and the normal form when it took it.
rootVerdict() {
	local out status

	out=$(
		SHARD_ROOT="$1"
		check_root >/dev/null 2>&1
		printf '%s' "${SHARD_ROOT}"
	)
	status=$?

	if [ "${status}" -ne 0 ]; then
		printf 'refused\n'

		return
	fi

	printf '%s\n' "${out}"
}

echo
echo "== check_root refuses every spelling of the production root"
check "the production root" "$(rootVerdict /var/lib/shard)" "refused"
check "the production root with a trailing slash" "$(rootVerdict /var/lib/shard/)" "refused"
check "the production root through a dot" "$(rootVerdict /var/lib/./shard)" "refused"
check "the production root through a parent" "$(rootVerdict /var/lib/shard/../shard)" "refused"
check "the filesystem root" "$(rootVerdict /)" "refused"
check "an empty root" "$(rootVerdict '')" "refused"
check "a relative root" "$(rootVerdict shard-e2e)" "refused"
check "the run's own root" "$(rootVerdict /var/lib/shard-e2e)" "/var/lib/shard-e2e"
check "the run's own root with a trailing slash" "$(rootVerdict /var/lib/shard-e2e/)" "/var/lib/shard-e2e"

echo
echo "== check_host_is_free refuses a host that already carries a sandbox"
STUB_LINKS=""
ip() { printf '%s\n' "${STUB_LINKS}"; }

STUB_LINKS='1: lo: <LOOPBACK,UP> mtu 65536
2: eth0: <BROADCAST,MULTICAST,UP> mtu 1500'
(check_host_is_free) >/dev/null 2>&1
check "a host with no sandbox link" "$?" "0"

STUB_LINKS='1: lo: <LOOPBACK,UP> mtu 65536
7: shardv2@if6: <BROADCAST,MULTICAST,UP> mtu 1500'
(check_host_is_free) >/dev/null 2>&1
check "a host that already holds shardv2" "$?" "1"

echo
echo "== unmount_under drops every mount under the root, the deepest first"
STUB_MOUNTS='sysfs on /sys type sysfs (rw)
tmpfs on /run/e2e-other type tmpfs (rw)
tmpfs on /run/e2e/a type tmpfs (rw)
tmpfs on /run/e2e/a/b type tmpfs (rw)
none on /run/e2e/runsc/null-netns type nsfs (rw)'
UMOUNTED=$(mktemp)

mount() { printf '%s\n' "${STUB_MOUNTS}"; }
umount() { printf '%s\n' "$*" >>"${UMOUNTED}"; }

unmount_under /run/e2e
check "what it unmounted, deepest first" "$(tr '\n' ',' <"${UMOUNTED}")" \
	"-l /run/e2e/runsc/null-netns,-l /run/e2e/a/b,-l /run/e2e/a,"
rm -f "${UMOUNTED}"

echo
echo "== teardown gives the host back on the failure path"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
STUB_MOUNTS=""

shard() { printf '%s\n' "$*" >>"${SHARD_CALLS}"; }
ip() { printf '%s\n' "$*" >>"${IP_CALLS}"; }

SHARD_ROOT=$(mktemp -d)
touch "${SHARD_ROOT}/sandbox.json"
ID="tidy-otter-0102"
LINK="shardv2"

teardown

check "the sandbox it removed" "$(cat "${SHARD_CALLS}")" "rm --force tidy-otter-0102"
check "the namespace and the link it deleted" "$(tr '\n' ',' <"${IP_CALLS}")" \
	"netns delete tidy-otter-0102,link delete shardv2,"
check "the root it removed" "$([ -e "${SHARD_ROOT}" ] && echo present || echo gone)" "gone"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"

echo
echo "== teardown removes the fork before its source, and both links"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
SHARD_ROOT=$(mktemp -d)
FORK_ID="e2e-fork-0304"
FORK_LINK="shardv3"

teardown

check "the fork first, then the source" "$(tr '\n' ',' <"${SHARD_CALLS}")" \
	"rm --force e2e-fork-0304,rm --force tidy-otter-0102,"
check "both namespaces and both links" "$(tr '\n' ',' <"${IP_CALLS}")" \
	"netns delete e2e-fork-0304,netns delete tidy-otter-0102,link delete shardv3,link delete shardv2,"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"
FORK_ID=""
FORK_LINK=""

echo
echo "== the run never swaps ID for the fork, so a failure in the fork section still removes the source"
check "no line assigns FORK_ID to ID" "$(grep -c '^ID="\${FORK_ID}"' "${HERE}/e2e.sh")" "0"

echo
echo "== timed prints its line, and no call redirects it away"
check "the line timed prints" "$(timed "probe" true | grep -c 'probe took')" "1"
check "no timed call under a redirect" "$(grep -c 'timed .*>/dev/null' "${HERE}/e2e.sh")" "0"

echo
echo "== a step that fails names the step and still gives the host back"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
STUB_MOUNTS=""
PROBE_ROOT=$(mktemp -d)

shard() { printf '%s\n' "$*" >>"${SHARD_CALLS}"; }
ip() { printf '%s\n' "$*" >>"${IP_CALLS}"; }

REPORT=$(
	(
		set -e
		SHARD_ROOT="${PROBE_ROOT}"
		STEP="stop the sandbox"
		REPORTED=0
		ID="tidy-otter-0102"
		LINK="shardv2"
		trap on_exit EXIT
		false
	) 2>&1
)

check "the step the failure named" "$(printf '%s' "${REPORT}" | grep -c 'e2e FAILED at step: stop the sandbox')" "1"
check "the sandbox the failure path removed" "$(cat "${SHARD_CALLS}")" "rm --force tidy-otter-0102"
check "the root the failure path removed" "$([ -e "${PROBE_ROOT}" ] && echo present || echo gone)" "gone"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"

echo
echo "== expect_exec fails a command that wrote the right bytes and then failed"
shard() {
	printf 'shard-e2e\n'

	return "${STUB_EXEC_CODE}"
}

STUB_EXEC_CODE=0
(expect_exec "shard-e2e" "the output matched" /bin/true) >/dev/null 2>&1
check "an exec that matched and exited 0" "$?" "0"

STUB_EXEC_CODE=3
(expect_exec "shard-e2e" "the output matched" /bin/true) >/dev/null 2>&1
check "an exec that matched and exited 3" "$?" "1"

echo
if [ "${FAILURES}" -ne 0 ]; then
	echo "e2e self-test FAILED: ${FAILURES} guards broke" >&2

	exit 1
fi

echo "e2e self-test PASSED: the root guard, the host guard, the unmount, the teardown, the timer, the failure report and the exec status"
