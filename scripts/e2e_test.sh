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
echo "== runtime_binary names the runtime of each provider and refuses the rest"
check "gvisor drives runsc" "$(runtime_binary gvisor 2>/dev/null)" "runsc"
check "sysbox drives sysbox-runc" "$(runtime_binary sysbox 2>/dev/null)" "sysbox-runc"
check "runc drives runc" "$(runtime_binary runc 2>/dev/null)" "runc"
(runtime_binary firecracker) >/dev/null 2>&1
check "a provider the daemon does not know" "$?" "1"
(runtime_binary "") >/dev/null 2>&1
check "an empty provider" "$?" "1"

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
echo "== a host net probe that cannot run fails, so the teardown never reads it as nothing"
ss() { return 1; }
(proxy_listeners) >/dev/null 2>&1
check "a failed ss" "$?" "1"
unset -f ss

echo
echo "== teardown gives the host back on the failure path"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
STUB_MOUNTS=""

shard() { printf '%s\n' "$*" >>"${SHARD_CALLS}"; }
ip() { printf '%s\n' "$*" >>"${IP_CALLS}"; }
# The self-test never reads the host, and a port on the bridge keeps the host net sweep, which has its own section, out of these calls.
STUB_PORTS="shardv1"
STUB_LISTENERS=""
STUB_PROBE_FAILS=""
bridge_ports() { [ "${STUB_PROBE_FAILS}" != ports ] || return 1; [ -z "${STUB_PORTS}" ] || printf '%s\n' "${STUB_PORTS}"; }
proxy_listeners() { [ "${STUB_PROBE_FAILS}" != listeners ] || return 1; printf '%s' "${STUB_LISTENERS}"; }

SHARD_ROOT=$(mktemp -d)
touch "${SHARD_ROOT}/sandbox.json"
ID="tidy-otter-0102"
LINK="shardv2"

teardown

check "the sandbox it removed" "$(cat "${SHARD_CALLS}")" "remove --force tidy-otter-0102"
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
	"remove --force e2e-fork-0304,remove --force tidy-otter-0102,"
check "both namespaces and both links" "$(tr '\n' ',' <"${IP_CALLS}")" \
	"netns delete e2e-fork-0304,netns delete tidy-otter-0102,link delete shardv3,link delete shardv2,"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"
FORK_ID=""
FORK_LINK=""

echo
echo "== teardown removes the seeded sandboxes, then the fork, then the source"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
SHARD_ROOT=$(mktemp -d)
FORK_ID="e2e-fork-0304"
FORK_LINK="shardv3"
SEEDED_IDS=" e2e-seeded-0506 e2e-seeded-0708"
SEEDED_LINKS=" shardv4 shardv5"

teardown

check "the seeded sandboxes first, then the fork, then the source" "$(tr '\n' ',' <"${SHARD_CALLS}")" \
	"remove --force e2e-seeded-0506,remove --force e2e-seeded-0708,remove --force e2e-fork-0304,remove --force tidy-otter-0102,"
check "every link" "$(grep -c 'link delete' "${IP_CALLS}")" "4"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"
FORK_ID=""
FORK_LINK=""
SEEDED_IDS=""
SEEDED_LINKS=""

echo
echo "== teardown stops the daemon it started, after the sandboxes and before the root goes"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
SHARD_ROOT=$(mktemp -d)
DAEMON_LOG=$(mktemp)
sleep 600 &
DAEMON_PID=$!

teardown

check "the daemon process" "$(kill -0 "${DAEMON_PID}" 2>/dev/null && echo alive || echo gone)" "gone"
check "the pid it kept" "${DAEMON_PID}" ""
check "the daemon log" "$([ -e "${DAEMON_LOG}" ] && echo present || echo gone)" "gone"
check "the sandbox it removed first" "$(cat "${SHARD_CALLS}")" "remove --force tidy-otter-0102"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"
DAEMON_LOG=""

echo
echo "== teardown frees a sandbox the root records and no step tracked"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
SHARD_ROOT=$(mktemp -d)
ID=""
LINK=""
mkdir -p "${SHARD_ROOT}/sandboxes/loose-heron-0910"
echo '{"id":"loose-heron-0910","host_interface":"shardv9"}' >"${SHARD_ROOT}/sandboxes/loose-heron-0910/sandbox.json"

teardown

check "the sandbox it removed" "$(cat "${SHARD_CALLS}")" "remove --force loose-heron-0910"
check "nothing else, because remove freed it" "$(cat "${IP_CALLS}")" ""
check "the root it removed" "$([ -e "${SHARD_ROOT}" ] && echo present || echo gone)" "gone"
rm -f "${SHARD_CALLS}" "${IP_CALLS}"

echo
echo "== teardown asks the runtime and the host directly when remove cannot free a recorded sandbox"
SHARD_CALLS=$(mktemp)
IP_CALLS=$(mktemp)
RUNTIME_CALLS=$(mktemp)
RMDIR_CALLS=$(mktemp)
ERR=$(mktemp)
SHARD_ROOT=$(mktemp -d)
RUNTIME=runsc
mkdir -p "${SHARD_ROOT}/sandboxes/loose-heron-0910"
echo '{"id":"loose-heron-0910","host_interface":"shardv9"}' >"${SHARD_ROOT}/sandboxes/loose-heron-0910/sandbox.json"

shard() { printf '%s\n' "$*" >>"${SHARD_CALLS}"; return 1; }
runsc() { printf '%s\n' "$*" >>"${RUNTIME_CALLS}"; }
rmdir() { printf '%s\n' "$*" >>"${RMDIR_CALLS}"; }
umount() { :; }

teardown 2>"${ERR}"

check "the remove it tried" "$(cat "${SHARD_CALLS}")" "remove --force loose-heron-0910"
check "the runtime it asked, over the state under the root" "$(cat "${RUNTIME_CALLS}")" "--root ${SHARD_ROOT}/runsc delete --force loose-heron-0910"
check "the cgroup it removed" "$(cat "${RMDIR_CALLS}")" "/sys/fs/cgroup/shard/loose-heron-0910"
check "the namespace and the link off the record" "$(tr '\n' ',' <"${IP_CALLS}")" "netns delete loose-heron-0910,link delete shardv9,"
check "what it said" "$(grep -c 'remove could not free sandbox loose-heron-0910' "${ERR}")" "1"
check "the root it removed" "$([ -e "${SHARD_ROOT}" ] && echo present || echo gone)" "gone"
rm -f "${SHARD_CALLS}" "${IP_CALLS}" "${RUNTIME_CALLS}" "${RMDIR_CALLS}" "${ERR}"
unset -f rmdir umount
shard() { printf '%s\n' "$*" >>"${SHARD_CALLS}"; }
RUNTIME=""

echo
echo "== wait_for_daemon waits for the socket and the proxy line, and names the cause on a miss"
SHARD_ROOT=$(mktemp -d)
DAEMON_LOG=$(mktemp)
# A daemon that is slow: nothing for 1 s, then the socket and the proxy line, well inside the bound.
(sleep 1; python3 -c "import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])" "${SHARD_ROOT}/shard.sock"; echo "proxy listening on 30080" >>"${DAEMON_LOG}"; sleep 30) &
DAEMON_PID=$!
BEFORE=$(date +%s)
DAEMON_START_BOUND=20 wait_for_daemon >/dev/null 2>&1
check "a slow daemon is waited for" "$?" "0"
check "the wait ended with the daemon, not with the bound" "$(( $(date +%s) - BEFORE < 10 ))" "1"
kill "${DAEMON_PID}" 2>/dev/null; wait "${DAEMON_PID}" 2>/dev/null

# A daemon that wrote both lines and never a socket file counts as up too.
: >"${DAEMON_LOG}"
rm -f "${SHARD_ROOT}/shard.sock"
printf 'api listening on %s\nproxy listening on 30080\n' "${SHARD_ROOT}/shard.sock" >"${DAEMON_LOG}"
sleep 30 &
DAEMON_PID=$!
DAEMON_START_BOUND=5 wait_for_daemon >/dev/null 2>&1
check "the two log lines are enough" "$?" "0"
kill "${DAEMON_PID}" 2>/dev/null; wait "${DAEMON_PID}" 2>/dev/null

# A daemon that died is reported at once, with its log, not after the bound.
echo "bind: address already in use" >"${DAEMON_LOG}"
true &
DAEMON_PID=$!
wait "${DAEMON_PID}" 2>/dev/null
BEFORE=$(date +%s)
MISS=$(DAEMON_START_BOUND=20 wait_for_daemon 2>&1 >/dev/null)
check "a dead daemon is a miss" "$?" "1"
check "the miss came at once" "$(( $(date +%s) - BEFORE < 5 ))" "1"
check "the miss names the exit" "$(echo "${MISS}" | grep -c 'exited before it listened')" "1"
check "the miss prints the log" "$(echo "${MISS}" | grep -c 'address already in use')" "1"

# A daemon that hangs is given the bound and then reported with its log.
echo "still loading" >"${DAEMON_LOG}"
sleep 30 &
DAEMON_PID=$!
MISS=$(DAEMON_START_BOUND=1 wait_for_daemon 2>&1 >/dev/null)
check "a silent daemon is a miss at the bound" "$?" "1"
check "the miss names the bound" "$(echo "${MISS}" | grep -c 'within 1s')" "1"
check "the miss prints the log" "$(echo "${MISS}" | grep -c 'still loading')" "1"
kill "${DAEMON_PID}" 2>/dev/null; wait "${DAEMON_PID}" 2>/dev/null
DAEMON_PID=""
rm -rf "${SHARD_ROOT}" "${DAEMON_LOG}"
DAEMON_LOG=""

echo
echo "== start_daemon empties the log in the parent and the daemon only appends, so a restart never waits on the last daemon's lines"
for script in e2e.sh e2e-fc.sh; do
	# A truncation in the child's own redirect can lose the race to the wait, so only the line order proves this.
	order=$(awk 'index($0, "start_daemon() {") == 1 {p = 1}
		p && index($0, ": >\"${DAEMON_LOG}\"") {e = NR}
		p && /daemon --provider .*&$/ {f = NR; a = (index($0, ">>\"${DAEMON_LOG}\" 2>&1 &") > 0)}
		p && /^}/ {exit}
		END {print (e && f && e < f ? "emptied first" : "not emptied first") ", " (a ? "appends" : "truncates")}' "${HERE}/${script}")
	check "${script}: the parent empties the log, then the daemon appends" "${order}" "emptied first, appends"
done

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
check "the sandbox the failure path removed" "$(cat "${SHARD_CALLS}")" "remove --force tidy-otter-0102"
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
echo "== the echo's digests match on the host, so the guest never holds the value"
check "seen is sha256sum in hex" "$(seen abc)" "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
check "basic is the digest of the header the proxy re-encodes" "$(basic value)" "$(seen "Basic YXBpOnZhbHVl")"

echo
echo "== process_clock takes one pid and its start time, and nothing else"
STUB_CLOCK="42 1234"
shard() { printf '%s\n' "${STUB_CLOCK}"; }
check "a pid and a start time" "$(process_clock tidy-otter-0102 2>/dev/null)" "42 1234"
# What pgrep -x sleep gave: no pid, so cut read /proc/stat.
STUB_CLOCK=$'  \n\n0'
(process_clock tidy-otter-0102) >/dev/null 2>&1
check "blank lines and a 0" "$?" "1"
STUB_CLOCK=$'42 1234\n43 1235'
(process_clock tidy-otter-0102) >/dev/null 2>&1
check "two processes" "$?" "1"
shard() { return 1; }
(process_clock tidy-otter-0102) >/dev/null 2>&1
check "no process" "$?" "1"

echo
echo "== expect_env_clean greps the guest's environment on the host"
SECRET_VALUE="e2e-self-test-value"
shard() { printf 'PATH=/bin\nE2E_TOKEN=mock-E2E_TOKEN\n'; }
(expect_env_clean tidy-otter-0102) >/dev/null 2>&1
check "an environment with the placeholder" "$?" "0"
shard() { printf 'PATH=/bin\nE2E_TOKEN=%s\n' "${SECRET_VALUE}"; }
(expect_env_clean tidy-otter-0102) >/dev/null 2>&1
check "an environment with the value" "$?" "1"
shard() { printf '%s\n' "$*" >"${SHARD_CALLS}"; }
SHARD_CALLS=$(mktemp)
(expect_env_clean tidy-otter-0102) >/dev/null 2>&1
check "the exec carries no value" "$(grep -c "${SECRET_VALUE}" "${SHARD_CALLS}")" "0"
rm -f "${SHARD_CALLS}"

echo
echo "== the teardown drops the bridge and the tables only once no run holds them"
NET_CALLS=$(mktemp)
FW_CALLS=$(mktemp)
nft() { printf 'nft %s\n' "$*" >>"${NET_CALLS}"; }
ip() { printf 'ip %s\n' "$*" >>"${NET_CALLS}"; }
STUB_FW_RULES=$'-P INPUT DROP\n-A INPUT -i lo -j ACCEPT\n-A INPUT -p tcp -m tcp --dport 22 -m comment --comment shard -j ACCEPT\n-A FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT'
iptables() {
	if [ "$2" = -S ]; then
		printf '%s\n' "${STUB_FW_RULES}"
		return
	fi
	printf 'iptables %s\n' "$*" >>"${FW_CALLS}"
}
# firewalld runs only when STUB_FIREWALLD is set, with an admin's own zone "shard" and shard's policy.
firewall-cmd() {
	case "$*" in
	--state) [ -n "${STUB_FIREWALLD:-}" ] || return 252 ;;
	"--permanent --get-policies") echo "allow-host-ipv6 shard-forwarding" ;;
	"--permanent --get-zones") echo "public shard" ;;
	"--permanent --policy=shard-forwarding --get-description") echo "managed-by-shard" ;;
	"--permanent --zone=shard --get-description") echo "the admin's own" ;;
	*) printf 'firewall-cmd %s\n' "$*" >>"${FW_CALLS}" ;;
	esac
}

STUB_PORTS=$'shardv3\nshardv4'
clear_host_net 2>/dev/null
check "a sandbox port keeps them" "${HOST_NET_KEPT}" "the bridge still has the ports shardv3 shardv4"
check "a sandbox port deletes nothing" "$(cat "${NET_CALLS}")" ""

STUB_PORTS=""
STUB_LISTENERS='LISTEN 0 4096 *:30080 *:* users:(("shard",pid=4242,fd=9))'
clear_host_net 2>/dev/null
check "a live daemon keeps them" "$(printf '%s' "${HOST_NET_KEPT}" | grep -c 'a daemon still serves the proxy: .*pid=4242')" "1"
check "a live daemon deletes nothing" "$(cat "${NET_CALLS}")" ""
(check_host_net_clear) >/dev/null 2>&1
check "the end check takes what it had to keep" "$?" "0"

STUB_LISTENERS=""
for STUB_PROBE_FAILS in ports listeners; do
	clear_host_net 2>/dev/null
	check "a failed ${STUB_PROBE_FAILS} probe keeps them" "$(printf '%s' "${HOST_NET_PROBE_ERROR}" | grep -c 'could not be listed')" "1"
	check "a failed ${STUB_PROBE_FAILS} probe deletes nothing" "$(cat "${NET_CALLS}")" ""
	(check_host_net_clear) >/dev/null 2>&1
	check "the end check fails a keep on a failed ${STUB_PROBE_FAILS} probe" "$?" "1"
done
STUB_PROBE_FAILS=""

clear_host_net 2>/dev/null
check "nothing holds them" "${HOST_NET_KEPT}" ""
check "both tables and the bridge go" "$(cat "${NET_CALLS}")" "$(printf '%s\n' "nft list table inet shard" "nft delete table inet shard" "nft list table bridge shard" "nft delete table bridge shard" "ip link show shard0" "ip link del shard0")"
check "only the marked firewall rule goes" "$(cat "${FW_CALLS}")" "iptables -w -D FORWARD -i shard0 -m comment --comment managed-by-shard -j ACCEPT"
: >"${FW_CALLS}"
STUB_FIREWALLD=yes close_host_firewall
check "only the marked firewalld policy goes" "$(grep firewall-cmd "${FW_CALLS}")" "$(printf '%s\n' "firewall-cmd --permanent --delete-policy=shard-forwarding" "firewall-cmd --reload")"
(check_host_net_clear) >/dev/null 2>&1
check "the end check fails a bridge still there" "$?" "1"
ip() { return 1; }
nft() { return 1; }
(check_host_net_clear) >/dev/null 2>&1
check "the end check fails a shard firewall rule still there" "$?" "1"
STUB_FW_RULES=$'-P INPUT DROP\n-A INPUT -i lo -j ACCEPT\n-A INPUT -p tcp -m tcp --dport 22 -m comment --comment shard -j ACCEPT'
(check_host_net_clear) >/dev/null 2>&1
check "the end check takes a host with neither" "$?" "0"
unset -f iptables firewall-cmd
rm -f "${NET_CALLS}" "${FW_CALLS}"

echo "== has_line reads all of stdin, so a match early in it cuts no stage (SHARD-456)"
STUB_GREP_DIR=$(mktemp -d)
export STUB_REAL_GREP
STUB_REAL_GREP=$(command -v grep)
# The stub writes as GNU grep does into a pipe: 4096 bytes, then the rest once a grep -q has quit on them.
cat >"${STUB_GREP_DIR}/grep" <<'EOF'
#!/usr/bin/env bash
for arg in "$@"; do
	case "${arg}" in
	--) break ;;
	-*[qc]*) exec "${STUB_REAL_GREP}" "$@" ;;
	esac
done
out="$(dirname "$0")/out.$$"
"${STUB_REAL_GREP}" "$@" >"${out}"
status=$?
head -c 4096 "${out}"
sleep 0.2
tail -c +4097 "${out}" || exit $?
exit "${status}"
EOF
chmod +x "${STUB_GREP_DIR}/grep"

# Each run of lines outgrows the first write, and the line each check wants sits inside it.
STUB_EGRESS=$(
	for i in $(seq 1 40); do
		rule="e2e-floor"
		[ "${i}" -ne 1 ] || rule="local"
		[ "${i}" -ne 2 ] || rule="ipv6"
		printf '{"time":"2026-10-04T00:00:%02dZ","source":"host","verdict":"deny","rule":"%s","dst":"203.0.113.%d:443","proto":"tcp"}\n' "${i}" "${rule}" "${i}"
	done
	for i in $(seq 1 40); do
		printf '{"time":"2026-10-04T00:01:%02dZ","source":"proxy","host":"api.example.test","verdict":"allow","rule":"e2e-policy","method":"GET","path":"/%d"}\n' "${i}" "${i}"
	done
)

# stubbed_has_line runs has_line over the synthetic log with the stub first on PATH, under pipefail as e2e.sh runs.
stubbed_has_line() { (PATH="${STUB_GREP_DIR}:${PATH}" && has_line "$@" <<<"${STUB_EGRESS}"); }

stubbed_has_line '"source":"host"' '"rule":"ipv6"'
check "the host drop of an IPv6 packet" "$?" "0"
stubbed_has_line '"source":"host"' '"rule":"local"'
check "the host drop of a packet aimed at its own address" "$?" "0"
stubbed_has_line '"host":"api.example.test"' '"verdict":"allow"' '"rule":"[^"]+"'
check "the proxy's allow with the rule that decided it" "$?" "0"
stubbed_has_line '"source":"host"' '"rule":"e2e-catchup"'
check "a rule no line holds is a miss" "$?" "1"
stubbed_has_line '"source":"host"' '"verdict":"allow"'
check "two patterns on two different lines are a miss" "$?" "1"
printf '/dev/loop7\n' | has_line '^/dev/loop'
check "one pattern over a command's output" "$?" "0"

echo "== no grep -q in the e2e scripts reads a pipe (SHARD-456)"
# shell_pipelines prints file:line, a tab and each pipeline, with quoted text cut to Q, comments dropped and continued lines joined.
shell_pipelines() {
	awk '
	function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
	function emit(cmd, line,   cmds, nc, c, p) {
		gsub(/\$\{[^}]*\}/, "V", cmd)
		gsub(/[0-9]*(>&|<&)[0-9-]*|&>/, " R ", cmd)
		gsub(/\|\||&&|[;&(){}`]/, "\n", cmd)
		gsub(/[ \t]+/, " ", cmd)
		nc = split(cmd, cmds, "\n")
		for (c = 1; c <= nc; c++) {
			p = trim(cmds[c])
			if (p != "") printf "%s:%d\t%s\n", FILENAME, line, p
		}
	}
	BEGIN { sq = sprintf("%c", 39) }
	FNR == 1 { quote = ""; cmd = ""; start = 0 }
	{
		s = $0
		n = length(s)
		cont = 0
		for (i = 1; i <= n; i++) {
			ch = substr(s, i, 1)
			if (quote == sq && ch == sq) { quote = ""; continue }
			if (quote == sq) continue
			if (quote == "\"" && ch == "\\") { i++; continue }
			if (quote == "\"" && ch == "\"") { quote = ""; continue }
			if (quote == "\"") continue
			if (ch == "\\" && i == n) cont = 1
			if (ch == "\\") { i++; continue }
			if (ch == "#" && (i == 1 || substr(s, i - 1, 1) ~ /[ \t;(]/)) break
			if (ch == sq || ch == "\"") { quote = ch; ch = "Q" }
			if (ch !~ /[ \t]/ && cmd !~ /[^ \t]/) start = FNR
			cmd = cmd ch
		}
		if (quote != "" || cont || cmd ~ /(\||&&)[ \t]*$/) { cmd = cmd " "; next }
		emit(cmd, start)
		cmd = ""
	}' "$@"
}

# quiet_grep_hazards prints file:line for each one: an early match kills the stage before it, and pipefail reads that as a miss.
quiet_grep_hazards() {
	shell_pipelines "$@" | awk '
	function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
	function bare(s) {
		s = trim(s)
		while (match(s, /^(if|then|elif|else|while|until|do|!|time)([ \t]+|$)/)) s = trim(substr(s, RLENGTH + 1))
		return s
	}
	function quiet_grep(s,   w, n, k) {
		s = bare(s)
		if (s !~ /^grep([ \t]|$)/) return 0
		n = split(s, w, /[ \t]+/)
		for (k = 2; k <= n; k++) {
			if (w[k] == "--") return 0
			if (w[k] ~ /^--(quiet|silent)$/ || w[k] ~ /^-[A-Za-z]*q/) return 1
		}
		return 0
	}
	BEGIN { FS = "\t" }
	{
		ns = split($2, st, "|")
		for (k = 2; k <= ns; k++) {
			if (!quiet_grep(st[k])) continue
			print $1
		}
	}'
}

TOKEN_FIXTURE="${STUB_GREP_DIR}/tokens.sh"
cat >"${TOKEN_FIXTURE}" <<'EOF'
echo "a | b" | grep -q x # c | d
if a && b; then c; fi
x |
	y
f '
| g
' | h
z >&2 | w ${X}
EOF
check "the tokenizer cuts quotes and comments, splits commands and joins lines" "$(shell_pipelines "${TOKEN_FIXTURE}" | awk -F '\t' '{ sub(/.*:/, "", $1); print $1 " " $2 }')" "$(printf '%s\n' '1 echo Q | grep -q x' '2 if a' '2 b' '2 then c' '2 fi' '3 x | y' '5 f Q | h' '8 z R | w V')"

SCAN_FIXTURE="${STUB_GREP_DIR}/fixture.sh"
cat >"${SCAN_FIXTURE}" <<'EOF'
echo "${X}" | grep -q y
printf '%s' "${X}" | grep -qx y
grep -q y "${FILE}"
ip netns list | has_line "^${ID}"
echo "${X}" | grep a | grep -c b >/dev/null
echo "${X}" 2>&1 | grep -qE y
shard exec "${ID}" /bin/sh -c 'ip a | grep a | grep -q y'
# echo "${X}" | grep a | grep -q y
echo "${X}" | grep a | grep -q y
ip netns list | grep -q "^${ID}"
grep a <<<"${X}" | grep -q y
if iptables -S 2>/dev/null | grep -qx -- "-P INPUT DROP" && true; then :; fi
echo "${X}" |
	grep a |
	grep --quiet y
for _ in 1 2; do echo "${X}" | grep a | grep -Eq y && break; done
shard exec "${ID}" /bin/sh -c '
	ip a | grep a | grep -q y
'
ip link | grep -q shard0
EOF
check "the scan names each one in a fixture" "$(quiet_grep_hazards "${SCAN_FIXTURE}" | sed 's/.*://' | tr '\n' ' ')" "1 2 6 9 10 11 12 13 16 20 "
# The needle is on the first line, so grep -q quits after one read and the echo dies writing the rest.
BIG_FIXTURE="${STUB_GREP_DIR}/big.sh"
cat >"${BIG_FIXTURE}" <<'EOF'
set -o pipefail
BIG=$(printf 'needle\n'; head -c 1048576 /dev/zero | tr '\0' x)
echo "${BIG}" 2>/dev/null | grep -q needle
EOF
bash "${BIG_FIXTURE}"
BIG_STATUS=$?
check "an echo of 1 MiB into grep -q is a miss though the needle is there" "$([ "${BIG_STATUS}" -ne 0 ] && echo miss)" "miss"
check "the scan names that echo" "$(quiet_grep_hazards "${BIG_FIXTURE}" | sed 's/.*://')" "3"
HERE_FIXTURE="${STUB_GREP_DIR}/here.sh"
sed '3s/.*/grep -q needle <<<"${BIG}"/' "${BIG_FIXTURE}" >"${HERE_FIXTURE}"
bash "${HERE_FIXTURE}"
check "the same body in a here-string is a match" "$?" "0"
check "the scan passes the here-string" "$(quiet_grep_hazards "${HERE_FIXTURE}")" ""
check "the scan finds none in e2e.sh or e2e-fc.sh" "$(quiet_grep_hazards "${HERE}/e2e.sh" "${HERE}/e2e-fc.sh")" ""
rm -rf "${STUB_GREP_DIR}"

echo "== public_address reads the first address port add --public named beyond loopback"
check "loopback, then two interfaces" "$(printf 'lo     127.0.0.1:12379\neth0   192.0.2.20:12379\neth1   198.51.100.7:12379\n' | public_address)" "192.0.2.20"
check "loopback only" "$(printf 'lo     127.0.0.1:12379\n' | public_address)" ""
check "no output" "$(public_address </dev/null)" ""

echo
echo "== both scripts parse"
bash -n "${HERE}/e2e.sh"
check "bash -n e2e.sh" "$?" "0"
bash -n "${HERE}/e2e-fc.sh"
check "bash -n e2e-fc.sh" "$?" "0"

echo
if [ "${FAILURES}" -ne 0 ]; then
	echo "e2e self-test FAILED: ${FAILURES} guards broke" >&2

	exit 1
fi

echo "e2e self-test PASSED: the root guard, the provider guard, the host guard, the unmount, the teardown, the daemon wait, the timer, the failure report, the exec status, the echo digests, the process clock, the env check, the host net sweep, the line match, the grep -q scan and the public address"
