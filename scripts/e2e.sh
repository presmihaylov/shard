#!/usr/bin/env bash
# SHARD-17: the whole sandbox lifecycle on a no-KVM Linux box, from an install to a clean host.
# It installs the two binaries, starts shard daemon over the run's root (SHARD-124), creates a
# sandbox, execs into it twice over the same filesystem, forks it while it runs, pauses and resumes it (SHARD-36, SHARD-457),
# stops it, removes it, stops the daemon, and then proves the host holds nothing either left behind.
# One sandbox holds a secret and a policy, so it is fronted: the run starts an echo server in a
# netns of its own and proves the proxy puts the value in on the granted host only (SHARD-71).
#
#   sudo ./scripts/e2e.sh
#
# The run keeps its own state root, but the bridge, the subnet, the veth names and the echo's netns
# and link belong to the host, so it must not run beside live sandboxes from another root. It refuses
# one that has any, and a host that already holds the echo's netns or link.
#
# Environment:
#   PREFIX     where the binaries are installed, and where the daemon loads shard-init from (default /usr/local/bin)
#   SHARD_ROOT where this run keeps its state          (default /var/lib/shard-e2e)
#   IMAGE      the image the sandbox is built from     (default alpine:3.20)
#   PROVIDER   the substrate the daemon runs on: gvisor, sysbox or runc (default gvisor)
#   DIND_IMAGE the image the sysbox docker step runs dockerd from (default docker:27-dind)
#   SKIP_INSTALL=1 to run against the binaries already on the box
#
# On sysbox the checkpoint steps become their refusals: the provider claims no pause, resume or fork,
# and the run proves each one says so by name while the sandbox runs on. Sysbox then earns its slot:
# a second sandbox runs dockerd and a docker build inside it, which no other substrate here can.
# On runc the checkpoint steps are the same refusals, and the docker step is skipped: bare runc holds no dockerd.

set -euo pipefail

# ip and nft live in sbin, which a sudo that carries the caller's PATH does not have.
export PATH="${PATH}:/usr/sbin:/sbin"

PREFIX=${PREFIX:-/usr/local/bin}
SHARD_ROOT=${SHARD_ROOT:-/var/lib/shard-e2e}
IMAGE=${IMAGE:-alpine:3.20}
PROVIDER=${PROVIDER:-gvisor}
# Where the daemon pins each sandbox's user namespace on sysbox (pkg/netns.UsernsRunDir).
USERNS_DIR=/var/run/shard/userns
DIND_IMAGE=${DIND_IMAGE:-docker:27-dind}
# The two loopback ports the TCP front binds in this run: one over the daemon, one over nothing.
SERVE_PORT=${SERVE_PORT:-12376}
LONE_PORT=${LONE_PORT:-12377}
# The loopback port where socat terminates TLS in front of the first, as the proxy of a real setup does.
FRONT_TLS_PORT=${FRONT_TLS_PORT:-12378}

# The root the run must never delete, and the name every sandbox veth on the host starts with.
PRODUCTION_ROOT="/var/lib/shard"
HOST_LINK_PREFIX="shardv"
# The bridge the daemon makes, host-wide like its two policy tables, and its own name with no override: a wrong one would delete a bridge this run never made.
HOST_BRIDGE="shard0"
# Who held the bridge and the tables when the teardown had to keep them, and empty when it dropped them.
HOST_NET_KEPT=""
# The failed probe that made the teardown keep them without knowing, which fails the run.
HOST_NET_PROBE_ERROR=""

STEP="startup"
ID=""
LINK=""
FORK_ID=""
FORK_LINK=""
# The sandbox the reconcile step makes and removes itself, kept here so a failure halfway still frees it.
RECONCILE_ID=""
RECONCILE_LINK=""
# The seeded sandboxes are space separated lists: two come off one snapshot, and both must go on teardown.
SEEDED_IDS=""
SEEDED_LINKS=""
SNAPSHOT_ID=""
# The sandbox that is created unfronted and granted a secret later (SHARD-114).
GRANT_ID=""
GRANT_LINK=""
# The sandbox the sysbox docker step runs dockerd in (SHARD-90).
DIND_ID=""
DIND_LINK=""
# The sandboxes the stack-feature steps hold right now, so a step that fails mid-flight still gives them back.
FEATURE_IDS=""
# The daemon this run started, which list, inspect and version speak to; the trap stops it by pid.
DAEMON_PID=""
DAEMON_LOG=""
# The TCP fronts and the socat in front of one, by pid, and the directory holding the certificate, signing key and token.
SERVE_PIDS=""
SERVE_DIR=""
SIGNING_KEY=""
SERVE_TOKEN=""
SERVE_LOG=""
# The second front sits over a root no daemon owns, which is how a refusal is proved to dial nothing.
LONE_ROOT=""
LONE_LOG=""
REPORTED=0
# The two ports the daemon's proxy listens on, as pkg/proxy declares them.
PROXY_PLAIN_PORT=30080
PROXY_TLS_PORT=30443
# The echo is the upstream behind the proxy, and it lives for the whole run.
ECHO_PID=""
ECHO_DIR=""
# The proxy refuses a name that resolves to the host's own address (SHARD-291), so the echo holds a TEST-NET-2 address in a netns of its own.
ECHO_NETNS_NAME="shard-e2e-echo"
ECHO_LINK="e2eecho"
# Each is set only once this run made it, so a teardown never deletes one another run holds.
ECHO_NETNS=""
ECHO_VETH=""
ECHO_GATEWAY="198.51.100.1"
ECHO_ADDRESS="198.51.100.2"
# The echo names all resolve to its address through sslip.io: one is granted, one is only allowed, one is neither.
ECHO_HOST="api.${ECHO_ADDRESS//./-}.sslip.io"
OTHER_HOST="other.${ECHO_ADDRESS//./-}.sslip.io"
DENIED_HOST="deny.${ECHO_ADDRESS//./-}.sslip.io"

# report names the step, so a red run says what broke rather than where the shell gave up. It speaks
# once: a failure reaches it through fail and then again through the exit handler.
report() {
	[ "${REPORTED}" = "0" ] || return 0
	REPORTED=1

	echo >&2
	echo "e2e FAILED at step: ${STEP}" >&2
	if [ -n "${1:-}" ]; then
		echo "  ${1}" >&2
	fi
}

fail() {
	report "${1:-}"

	exit 1
}

step() {
	STEP="$1"
	echo
	echo "== ${STEP}"
}

# say reports one assertion that held, so the transcript proves what was checked.
say() { echo "   ok: $1"; }

shard() { "${PREFIX}/shard" --root "${SHARD_ROOT}" "$@"; }

# expect compares two strings and names both when they differ.
expect() {
	[ "$1" = "$2" ] || fail "got '$1', want '$2'"
	say "$3"
}

# expect_exec runs a command in the sandbox and compares what it wrote. The status is checked on its
# own, because a command substitution used as an argument throws the status of the call away.
expect_exec() { expect_exec_in "${ID}" "$@"; }

# expect_exec_in is expect_exec against a named sandbox, so a fork is checked without swapping ID.
expect_exec_in() {
	local id="$1" want="$2" note="$3"
	shift 3

	local got
	if ! got=$(shard exec "${id}" "$@"); then
		fail "shard exec $* wrote the right bytes and then exited non-zero"
	fi

	expect "${got}" "${want}" "${note}"
}

# holds matches what a verb wrote, read through $(...): grep -q on a pipe closes it at the first match, and under pipefail the verb's SIGPIPE reads as a miss.
holds() {
	local want="$1" out
	shift
	out=$("$@") || return 1
	grep -q -- "${want}" <<<"${out}"
}

# has_line holds when one line of stdin matches every pattern. It reads stdin to the end, so a producer never dies of SIGPIPE.
has_line() {
	local lines pattern
	lines=$(cat)
	for pattern in "$@"; do
		lines=$(grep -E -- "${pattern}" <<<"${lines}") || return 1
	done
}

# nap_alive reports the guest process of the background exec. The bracket keeps the probe off its own args.
nap_alive() { shard exec "${ID}" /bin/sh -c 'pgrep -f "[s]leep 313" >/dev/null' >/dev/null 2>&1; }

# listed_state reads the STATE column of shard list for one sandbox, so the check never matches the image.
listed_state() { shard list --all | awk -v id="$1" '$1 == id { print $4 }'; }

# id_of reads the id of a named sandbox through $(...), so the grep's early exit never SIGPIPEs inspect.
id_of() {
	local out
	out=$(shard inspect "$1") || return 1
	grep -om1 '"id": *"[^"]*"' <<<"${out}" | cut -d'"' -f4
}

# fronted reports the dnat that sends a sandbox's 80 to the proxy. The address is read fresh: a lease
# is re-allocated across a stop and a start, so one read at the create goes stale.
fronted() {
	local id="$1" address
	address=$(grep -o '"address": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${id}/sandbox.json" | cut -d'"' -f4)
	address="${address%%/*}"
	[ -n "${address}" ] || fail "the record of ${id} holds no address"

	# No pipe: grep -q closes one early, and under pipefail the producer's SIGPIPE reads as no match.
	local rules
	rules=$(nft list table inet shard)
	case "${rules}" in
	*"ip saddr ${address} tcp dport 80 dnat"*) return 0 ;;
	esac

	return 1
}

# entrypoint_clock reads the guest pid and start time of the entrypoint, which only a restore keeps.
entrypoint_clock() {
	local clock
	# Anchored to argv0: shard-init, PID 1, carries the entrypoint's command line mid-argv, so it never counts (SHARD-329).
	clock=$(shard exec "$1" /bin/sh -c 'p=$(pgrep -f "^/bin/sleep 600") && echo "$p $(cut -d" " -f22 /proc/$p/stat)"') ||
		fail "the guest runs no sleep 600 entrypoint to read a clock from"
	[[ "${clock}" =~ ^[0-9]+\ [0-9]+$ ]] || fail "the guest gave '${clock}' for the entrypoint, want one pid and its start time"
	printf '%s\n' "${clock}"
}

# expect_network fails when the guest does not hold its address or cannot get out through the NAT.
# The gateway itself drops what a guest sends it, so the probe goes past it.
expect_network() {
	local when="$1"
	expect_exec "${ADDRESS}" "the guest holds its address ${when}" \
		/bin/sh -c "ip -o -4 addr show eth0 | grep -o '${ADDRESS}'"
	expect_exec "reachable" "the guest gets out through the NAT ${when}" \
		/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
}

# expect_blocked fails when the guest can reach an address its policy denies. A drop answers nothing,
# so the probe waits two seconds and the check is that it gave up.
expect_blocked() {
	local id="$1" note="$2" got
	got=$(shard exec "${id}" /bin/sh -c 'ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1 && echo reachable || echo blocked')
	expect "${got}" "blocked" "${note}"
}

# seen is the sha256 the echo prints for one header value; shasum covers macOS, where make check runs the self-test.
seen() {
	if command -v sha256sum >/dev/null; then
		printf '%s' "$1" | sha256sum | cut -d' ' -f1
		return
	fi
	printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1
}

# fetch runs busybox wget in a sandbox against one echo name, over http or https, with the placeholder of
# each secret in a header, and prints the lines the echo answered with.
fetch() {
	local id="$1" scheme="$2" host="$3"
	shard exec "${id}" /bin/sh -c "wget -q -O - --header \"Authorization: Bearer \$E2E_TOKEN\" --header \"X-Shaped: \$E2E_SHAPED\" ${scheme}://${host}/"
}

# expect_fronted fails when a tls request from the sandbox to the granted host does not carry the real value,
# which proves the request went through the proxy and the proxy put the value in.
expect_fronted() {
	local id="$1" note="$2" got
	if ! got=$(fetch "${id}" https "${ECHO_HOST}"); then
		fail "the request to ${ECHO_HOST} from ${id} failed"
	fi
	grep -qx "authorization=$(seen "Bearer ${SECRET_VALUE}")" <<<"${got}" || fail "the echo saw '${got}', want the value in Authorization"
	say "${note}"
}

# basic_of prints the digest one echo host saw for an HTTP Basic header. busybox has no curl, so the
# guest builds the header the way every client does: base64 of "user:placeholder".
basic_of() {
	local id="$1" host="$2" got line
	got=$(shard exec "${id}" /bin/sh -c "wget -q -O - --header \"Authorization: Basic \$(printf '%s' \"api:\$E2E_TOKEN\" | base64)\" https://${host}/") ||
		fail "the basic auth request to ${host} failed"
	line=$(grep '^authorization=' <<<"${got}") || fail "the echo saw no authorization header: ${got}"
	printf '%s' "${line#authorization=}"
}

# basic is the digest of the Basic header for the user api and one password, as the proxy re-encodes it.
basic() { seen "Basic $(printf 'api:%s' "$1" | base64 | tr -d '\n')"; }

# expect_env_clean greps the guest's environment on the host: the value in an exec's argv would land in guest memory.
expect_env_clean() {
	local env
	env=$(shard exec "$1" /bin/sh -c env) || fail "env did not run in $1"
	grep -qF -- "${SECRET_VALUE}" <<<"${env}" && fail "the environment of $1 holds the value"
	say "the value is not in the guest's environment"
}

# start_echo builds the upstream and starts it on 80 and 443 of its own netns, behind a veth pair to the host.
start_echo() {
	[ ! -e "/run/netns/${ECHO_NETNS_NAME}" ] || fail "the netns ${ECHO_NETNS_NAME} is already on the host: another run holds the echo, or a crashed one left it"
	ip link show "${ECHO_LINK}0" >/dev/null 2>&1 && fail "the link ${ECHO_LINK}0 is already on the host: another run holds the echo, or a crashed one left it"

	ECHO_DIR=$(mktemp -d)
	go build -o "${ECHO_DIR}/echo" ./scripts/echo
	ip netns add "${ECHO_NETNS_NAME}"
	ECHO_NETNS="${ECHO_NETNS_NAME}"
	ip link add "${ECHO_LINK}0" type veth peer name "${ECHO_LINK}1" netns "${ECHO_NETNS}"
	ECHO_VETH="${ECHO_LINK}0"
	ip addr add "${ECHO_GATEWAY}/30" dev "${ECHO_LINK}0"
	ip link set "${ECHO_LINK}0" up
	ip -n "${ECHO_NETNS}" addr add "${ECHO_ADDRESS}/30" dev "${ECHO_LINK}1"
	ip -n "${ECHO_NETNS}" link set "${ECHO_LINK}1" up
	ip netns exec "${ECHO_NETNS}" "${ECHO_DIR}/echo" -address "${ECHO_ADDRESS}" -names "${ECHO_HOST},${OTHER_HOST}" -cert-out "${ECHO_DIR}/cert.pem" -ready "${ECHO_DIR}/ready" >"${ECHO_DIR}/log" 2>&1 &
	ECHO_PID=$!
	for _ in $(seq 1 50); do
		[ -f "${ECHO_DIR}/ready" ] && return 0
		kill -0 "${ECHO_PID}" 2>/dev/null || break
		sleep 0.1
	done
	fail "the echo did not come up: $(cat "${ECHO_DIR}/log")"
}

stop_echo() {
	if [ -n "${ECHO_PID}" ]; then
		kill "${ECHO_PID}" >/dev/null 2>&1 || true
		wait "${ECHO_PID}" >/dev/null 2>&1 || true
		ECHO_PID=""
	fi
	# The kernel frees a deleted netns lazily, so the pair goes first, and at once, for the next run's ip link add.
	if [ -n "${ECHO_VETH}" ]; then
		ip link delete "${ECHO_VETH}" >/dev/null 2>&1 || true
		ECHO_VETH=""
	fi
	[ -n "${ECHO_NETNS}" ] || return 0
	ip netns delete "${ECHO_NETNS}" >/dev/null 2>&1 || true
	ECHO_NETNS=""
}

# timed runs a command and prints how long it took, so the transcript carries the numbers SHARD-32 asks for.
# Never redirect a timed call: the redirect would swallow this line, so the wrappers below do it inside.
timed() {
	local note="$1" started ended
	shift

	started=$(date +%s.%N)
	"$@"
	ended=$(date +%s.%N)
	say "${note} took $(awk -v a="${started}" -v b="${ended}" 'BEGIN { printf "%.3f s", b - a }')"
}

pause_it() { shard pause "${ID}" >/dev/null; }
resume_it() { shard resume "${ID}" >/dev/null; }
fork_it() { FORK_ID=$(shard fork --name e2e-fork "${ID}"); }
snapshot_it() { SNAPSHOT_ID=$(shard snapshot create --name e2e-snapshot "${ID}" | tail -n 1); }
seed_it() { SEEDED_IDS="${SEEDED_IDS} $(shard create --name "$1" --secret E2E_TOKEN --policy e2e-policy --snapshot "${SNAPSHOT_ID}")"; }

# rss_kib reads the resident set of a host process, which for a sandbox is the sentry and its guest memory.
rss_kib() { ps -o rss= -p "$1" 2>/dev/null | tr -d ' ' || true; }

# absent fails when the pattern is still on the host, quoting what was found.
absent() {
	local what="$1" found="$2"
	[ -z "${found}" ] || fail "${what} is still on the host: ${found}"
	say "${what} is gone"
}

# normalise folds away '.', '..' and repeated or trailing slashes, so a guard can compare two paths
# rather than two spellings of one.
normalise() {
	local path="$1" part out=""
	local -a parts

	IFS=/ read -r -a parts <<<"${path}"
	for part in "${parts[@]}"; do
		case "${part}" in
		"" | .) ;;
		..) out="${out%/*}" ;;
		*) out="${out}/${part}" ;;
		esac
	done

	printf '%s\n' "${out:-/}"
}

# check_root refuses a root this run must not delete, and rewrites SHARD_ROOT to its normal form.
# A trailing slash is what completing a directory name appends, and it must not walk past the guard.
check_root() {
	case "${SHARD_ROOT}" in
	/*) ;;
	*) fail "SHARD_ROOT must be an absolute path, got '${SHARD_ROOT}'" ;;
	esac

	SHARD_ROOT=$(normalise "${SHARD_ROOT}")

	[ "${SHARD_ROOT}" != "/" ] || fail "SHARD_ROOT must not be the filesystem root"
	[ "${SHARD_ROOT}" != "$(normalise "${PRODUCTION_ROOT}")" ] ||
		fail "the e2e must not run against the production root ${PRODUCTION_ROOT}"
}

# check_host_is_free refuses a host that already carries a sandbox. The lease pool lives under this
# run's own root, so it would hand out an address another root holds and delete that sandbox's veth.
check_host_is_free() {
	local links
	links=$(ip -o link show 2>/dev/null | grep -o "${HOST_LINK_PREFIX}[0-9]\+" | sort -u | tr '\n' ' ' || true)

	[ -z "${links% }" ] || fail "the host already carries the sandbox links ${links% }: the e2e must not run beside live sandboxes"
}

# unmount_under drops every mount under a directory, the deepest first, so a later rm -rf cannot
# meet one and delete the record of a sandbox that is still up.
unmount_under() {
	local dir="$1" point

	[ -n "${dir}" ] || return 0

	for point in $(mount | awk -v root="${dir}/" 'index($3 "/", root) == 1 { print $3 }' | sort -r); do
		umount -l "${point}" >/dev/null 2>&1 || true
	done
}

# wipe_root gives the root back. The unmount comes first: runsc bind mounts a null-netns into its own
# root on the first create, and an overlay sits under every sandbox that is still up.
wipe_root() {
	unmount_under "${SHARD_ROOT}"
	rm -rf "${SHARD_ROOT}" || true
	clear_host_net
}

# bridge_ports lists the links on the host bridge, which on a veth or a tap is a sandbox of any root, and fails when it cannot read them.
bridge_ports() { [ ! -e "/sys/class/net/${HOST_BRIDGE}" ] || ls -A "/sys/class/net/${HOST_BRIDGE}/brif"; }

# proxy_listeners lists what serves the proxy ports, which every daemon binds whatever its root.
proxy_listeners() { ss -Hltnp "( sport = :${PROXY_PLAIN_PORT} or sport = :${PROXY_TLS_PORT} )"; }

# host_net_holder names what still uses the bridge and the tables, prints nothing once nothing does, and fails on a probe it could not run.
host_net_holder() {
	local ports listeners
	if ! ports=$(bridge_ports); then
		echo "the ports of the bridge ${HOST_BRIDGE} could not be listed"

		return 1
	fi
	if [ -n "${ports}" ]; then
		echo "the bridge still has the ports $(printf '%s' "${ports}" | tr '\n' ' ')"

		return
	fi
	if ! listeners=$(proxy_listeners); then
		echo "the listeners on the proxy ports could not be listed"

		return 1
	fi
	[ -z "${listeners}" ] || echo "a daemon still serves the proxy: ${listeners}"
}

# clear_host_net drops the bridge and the tables the daemon never drops (SHARD-272), unless a run on another root still holds them or a probe cannot tell.
clear_host_net() {
	local table
	HOST_NET_PROBE_ERROR=""
	if ! HOST_NET_KEPT=$(host_net_holder); then
		HOST_NET_PROBE_ERROR="${HOST_NET_KEPT}"
	fi
	if [ -n "${HOST_NET_KEPT}" ]; then
		echo "teardown: kept the bridge ${HOST_BRIDGE} and the shard nft tables, because ${HOST_NET_KEPT}" >&2

		return 0
	fi
	for table in inet bridge; do
		if nft list table "${table}" shard >/dev/null 2>&1; then
			nft delete table "${table}" shard
		fi
	done
	if ip link show "${HOST_BRIDGE}" >/dev/null 2>&1; then
		ip link del "${HOST_BRIDGE}"
	fi
}

# check_host_net_clear fails a run that left the bridge or a table nothing held, or kept them on a failed probe, and names the holder of one it had to keep.
check_host_net_clear() {
	[ -z "${HOST_NET_PROBE_ERROR}" ] || fail "the teardown could not tell whether another run holds the bridge ${HOST_BRIDGE}: ${HOST_NET_PROBE_ERROR}"
	if [ -n "${HOST_NET_KEPT}" ]; then
		say "the bridge ${HOST_BRIDGE} and the shard nft tables stay, because ${HOST_NET_KEPT}"

		return
	fi
	ip link show "${HOST_BRIDGE}" >/dev/null 2>&1 && fail "the bridge ${HOST_BRIDGE} is still on the host"
	nft list table inet shard >/dev/null 2>&1 && fail "the host still holds table inet shard"
	nft list table bridge shard >/dev/null 2>&1 && fail "the host still holds table bridge shard"
	say "the bridge ${HOST_BRIDGE} and both shard nft tables are gone"
}

# start_daemon runs shard daemon over the run's root in the background and waits for its socket line.
# It returns non-zero rather than failing, so a teardown can bring one back without ending the script.
start_daemon() {
	local busy
	busy=$(ss -Hltn "( sport = :${PROXY_PLAIN_PORT} or sport = :${PROXY_TLS_PORT} )" 2>/dev/null || true)
	[ -z "${busy}" ] || return 1

	[ -n "${DAEMON_LOG}" ] || DAEMON_LOG=$(mktemp)
	# The proxy verifies the echo like any upstream, so the daemon trusts the host roots plus that one certificate.
	local trust=""
	if [ -f "${ECHO_DIR}/cert.pem" ]; then
		cat /etc/ssl/certs/ca-certificates.crt "${ECHO_DIR}/cert.pem" >"${ECHO_DIR}/trust.pem"
		trust="${ECHO_DIR}/trust.pem"
	fi
	# Emptied before the fork, so a restart never waits on the last daemon's lines; the child only appends.
	: >"${DAEMON_LOG}"
	# The daemon binds the supervisor by this path into every sandbox, so a PREFIX run tests the shard-init it installed.
	SHARD_INIT_PATH="${PREFIX}/shard-init" SSL_CERT_FILE="${trust}" "${PREFIX}/shard" --root "${SHARD_ROOT}" daemon --provider "${PROVIDER}" >>"${DAEMON_LOG}" 2>&1 &
	DAEMON_PID=$!
	wait_for_daemon
}

# A cold daemon on a busy box can take well over 5 s to bind, so the bound is generous and never the signal.
DAEMON_START_BOUND=${DAEMON_START_BOUND:-60}

# wait_for_daemon answers once the daemon has bound its socket and its proxy, or as soon as it died,
# and prints what it logged on a miss so the failure names the cause and not the clock.
wait_for_daemon() {
	local deadline
	deadline=$(($(date +%s) + DAEMON_START_BOUND))
	while :; do
		daemon_ready && return 0
		if ! kill -0 "${DAEMON_PID}" 2>/dev/null; then
			echo "the daemon ${DAEMON_PID} exited before it listened; it logged:" >&2
			cat "${DAEMON_LOG}" >&2

			return 1
		fi
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			echo "the daemon ${DAEMON_PID} listened on nothing within ${DAEMON_START_BOUND}s; it logged:" >&2
			cat "${DAEMON_LOG}" >&2

			return 1
		fi
		sleep 0.1
	done
}

# The socket file appears when the API binds, so either it or the log line proves that half.
daemon_ready() {
	{ [ -S "${SHARD_ROOT}/shard.sock" ] || grep -q "api listening on" "${DAEMON_LOG}"; } &&
		grep -q "proxy listening on" "${DAEMON_LOG}"
}

# stop_daemon ends the daemon this run started, by its own pid, and answers only once the socket it
# served is gone. It never touches another daemon: the pid is the one start_daemon kept.
stop_daemon() {
	[ -n "${DAEMON_PID}" ] || return 0
	kill "${DAEMON_PID}" >/dev/null 2>&1 || true
	wait "${DAEMON_PID}" >/dev/null 2>&1 || true
	DAEMON_PID=""

	for _ in $(seq 1 50); do
		[ -e "${SHARD_ROOT}/shard.sock" ] || return 0
		sleep 0.1
	done

	return 1
}

# start_serve runs a TCP front over one root on one port with any further serve flags, waits for its listen line, and writes its pid.
start_serve() {
	local root="$1" port="$2" log="$3" pid
	shift 3

	"${PREFIX}/shard" --root "${root}" serve --listen "127.0.0.1:${port}" "$@" >"${log}" 2>&1 &
	pid=$!

	for _ in $(seq 1 50); do
		if grep -q "serve listening on" "${log}"; then
			echo "${pid}"

			return 0
		fi
		sleep 0.1
	done

	kill "${pid}" >/dev/null 2>&1 || true

	return 1
}

# start_front_tls runs socat as the TLS proxy in front of the front on upstream, waits for its port, and writes its pid.
start_front_tls() {
	local port="$1" upstream="$2" pid

	socat "OPENSSL-LISTEN:${port},bind=127.0.0.1,reuseaddr,fork,cert=${SERVE_DIR}/serve.crt,key=${SERVE_DIR}/serve.key,verify=0" \
		"TCP:127.0.0.1:${upstream}" >/dev/null 2>&1 &
	pid=$!

	for _ in $(seq 1 50); do
		if [ -n "$(ss -Hltn "sport = :${port}")" ]; then
			echo "${pid}"

			return 0
		fi
		sleep 0.1
	done

	kill "${pid}" >/dev/null 2>&1 || true

	return 1
}

# stop_serve ends every front and socat this run started, by pid. It never touches another one.
stop_serve() {
	local pid
	# shellcheck disable=SC2086 # the pid list is meant to split
	for pid in ${SERVE_PIDS}; do
		kill "${pid}" >/dev/null 2>&1 || true
		wait "${pid}" >/dev/null 2>&1 || true
	done
	SERVE_PIDS=""
}

# front_curl asks one front over plain http on its own port, with the token when one is given and none when it is empty.
front_curl() {
	local port="$1" token="$2" path="$3"
	shift 3

	if [ -n "${token}" ]; then
		set -- -H "Authorization: Bearer ${token}" "$@"
	fi

	curl -sS "$@" "http://127.0.0.1:${port}${path}"
}

# shard_front drives a verb through socat and the front rather than over the socket, which is what --remote is for.
shard_front() {
	"${PREFIX}/shard" --remote "https://127.0.0.1:${FRONT_TLS_PORT}" --token-file "${SERVE_TOKEN}" \
		--ca-file "${SERVE_DIR}/serve.crt" "$@"
}

# recorded_sandboxes prints the id of every sandbox the root still records, whether a step tracked it or not.
recorded_sandboxes() {
	local dir
	for dir in "${SHARD_ROOT}"/sandboxes/*/; do
		[ -d "${dir}" ] || continue
		basename "${dir}"
	done
}

# free_sandbox gives back a sandbox remove did not, before wipe_root removes the runtime state that names it.
free_sandbox() {
	local id="$1" link
	shard remove --force "${id}" >/dev/null 2>&1 && return 0

	echo "teardown: remove could not free sandbox ${id}, so the runtime and the host are asked directly" >&2
	[ -z "${RUNTIME:-}" ] || "${RUNTIME}" --root "${SHARD_ROOT}/${RUNTIME}" delete --force "${id}" >/dev/null 2>&1 || true
	rmdir "/sys/fs/cgroup/shard/${id}" >/dev/null 2>&1 || true
	ip netns delete "${id}" >/dev/null 2>&1 || true
	umount "${USERNS_DIR}/${id}" >/dev/null 2>&1 || true
	rm -f "${USERNS_DIR}/${id}"
	link=$(grep -o '"host_interface": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${id}/sandbox.json" 2>/dev/null | cut -d'"' -f4 || true)
	[ -z "${link}" ] || ip link delete "${link}" >/dev/null 2>&1 || true
}

# teardown gives the host back. A run that failed halfway must not leave a sandbox behind: the
# record is the only handle by which its mount, its namespace, its cgroup and its process can be found again.
teardown() {
	local id link
	# remove speaks to the daemon, so a run that broke while the daemon was down gets one back first.
	if [ -z "${DAEMON_PID}" ] && [ -x "${PREFIX}/shard" ] && [ -n "${ID}${FORK_ID}${SEEDED_IDS}${RECONCILE_ID}${GRANT_ID}${DIND_ID}${FEATURE_IDS}$(recorded_sandboxes)" ]; then
		start_daemon || echo "teardown: no daemon came up, so remove cannot run: $(cat "${DAEMON_LOG}")" >&2
	fi
	# shellcheck disable=SC2086 # the seeded lists are meant to split
	for id in ${SEEDED_IDS} ${FEATURE_IDS} "${GRANT_ID}" "${RECONCILE_ID}" "${FORK_ID}" "${DIND_ID}" "${ID}"; do
		[ -n "${id}" ] || continue
		shard remove --force "${id}" >/dev/null 2>&1 || true
		ip netns delete "${id}" >/dev/null 2>&1 || true
		# sysbox pins the userns next to the netns; a bind mount survives the rm of its file.
		umount "${USERNS_DIR}/${id}" >/dev/null 2>&1 || true
		rm -f "${USERNS_DIR}/${id}"
	done
	# shellcheck disable=SC2086
	for link in ${SEEDED_LINKS} "${GRANT_LINK}" "${RECONCILE_LINK}" "${FORK_LINK}" "${DIND_LINK}" "${LINK}"; do
		[ -n "${link}" ] || continue
		ip link delete "${link}" >/dev/null 2>&1 || true
	done
	# What the loops above did not free still has a record: a sandbox no step tracked, or one remove refused.
	for id in $(recorded_sandboxes); do
		free_sandbox "${id}"
	done

	stop_serve
	stop_daemon || echo "teardown: the socket ${SHARD_ROOT}/shard.sock outlived the daemon" >&2
	stop_echo
	wipe_root
	rm -rf "${ECHO_DIR:-/nonexistent}" "${SERVE_DIR:-/nonexistent}" "${LONE_ROOT:-/nonexistent}"
	rm -f "${DAEMON_LOG:-/nonexistent}" "${SERVE_LOG:-/nonexistent}" "${LONE_LOG:-/nonexistent}"
}

# on_exit is the one handler: it names the step that broke and then gives the host back. The step is
# named here rather than from an ERR trap, because bash runs this one first and it never returns.
on_exit() {
	local status=$?

	trap - EXIT
	if [ "${status}" -ne 0 ]; then
		report "the command under this step exited non-zero"
		[ -z "${DAEMON_LOG}" ] || cat "${DAEMON_LOG}" >&2 || true
	fi

	teardown

	exit "${status}"
}

# runtime_binary names the binary the provider drives, and refuses a provider the daemon does not know.
# The daemon would refuse it too, but only at the first create, after the install and the echo are up.
runtime_binary() {
	case "$1" in
	gvisor) printf 'runsc\n' ;;
	sysbox) printf 'sysbox-runc\n' ;;
	runc) printf 'runc\n' ;;
	*) fail "PROVIDER must be gvisor, sysbox or runc, got '$1'" ;;
	esac
}

# checkpoint_steps fork the running sandbox, then pause and resume it, which only a provider that holds checkpoints can do.
checkpoint_steps() {
	fork_steps

	step "pause the sandbox"
	# A restore keeps the guest's processes; a restart makes new ones. The entrypoint's pid and start
	# time tell the two apart from outside, and the file proves the layer went with the memory.
	shard exec "${ID}" /bin/sh -c 'echo before-the-pause > /root/at-pause' >/dev/null
	CLOCK_BEFORE=$(entrypoint_clock "${ID}")
	say "the entrypoint is guest pid and start time ${CLOCK_BEFORE} before the pause"
	PID=$(grep -o '"pid": *[0-9]*' "${RECORD}" | grep -o '[0-9]*$')
	RSS_BEFORE=$(rss_kib "${PID}")
	[ -n "${RSS_BEFORE}" ] || fail "the sandbox process ${PID} has no resident set to read"
	say "the sandbox process ${PID} holds ${RSS_BEFORE} KiB on the host before the pause"

	timed "pause" pause_it
	grep -q '"state": *"paused"' "${RECORD}" || fail "the record does not say paused"
	CHECKPOINT=$(grep -o '"checkpoint": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
	[ -f "${CHECKPOINT}/checkpoint.img" ] || fail "there is no checkpoint at ${CHECKPOINT}/checkpoint.img"
	say "the record says paused and the checkpoint is at ${CHECKPOINT}"
	# The checkpoint is the guest's memory after it sent the placeholder out, so the value must not be in it.
	absent "the value in the checkpoint" "$(grep -rl "${SECRET_VALUE}" "${CHECKPOINT}" 2>/dev/null || true)"

	# The whole point of a pause: the memory goes back to the host, so the sandbox process is gone.
	absent "the sandbox process ${PID} and its ${RSS_BEFORE} KiB" "$(rss_kib "${PID}")"
	case "${PROVIDER}" in
	# A Firecracker sandbox keeps its cgroup, empty, until remove, so the start that brings it back boots into it (SHARD-267).
	firecracker) absent "a process in the cgroup of the paused sandbox" "$(cat "/sys/fs/cgroup/shard/${ID}/cgroup.procs" 2>/dev/null || true)" ;;
	*) absent "the cgroup of the paused sandbox" "$([ -e "/sys/fs/cgroup/shard/${ID}" ] && echo "/sys/fs/cgroup/shard/${ID}" || true)" ;;
	esac
	absent "the rootfs mount of the paused sandbox" "$(mount | grep "${SHARD_ROOT}/sandboxes/${ID}" || true)"
	[ "$(listed_state "${ID}")" = "paused" ] || fail "shard list --all does not list the sandbox as paused"

	CODE=0
	REFUSAL=$(shard exec "${ID}" /bin/true 2>&1) || CODE=$?
	[ "${CODE}" != "0" ] || fail "exec ran in a paused sandbox"
	grep -q "shard resume ${ID}" <<<"${REFUSAL}" || fail "exec said '${REFUSAL}', want it to name the resume"
	say "exec refused the paused sandbox and named the resume"

	step "resume the sandbox"
	# A resume loads the checkpoint and leaves it, so a second resume could load it again.
	timed "resume" resume_it
	grep -q '"state": *"running"' "${RECORD}" || fail "the record does not say running after the resume"
	grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the resume changed the address"
	say "the record says running on the same address"

	expect "$(entrypoint_clock "${ID}")" "${CLOCK_BEFORE}" "the entrypoint is the same process with the same start time, so the resume was a restore"
	expect_exec "before-the-pause" "the file written before the pause is there after the resume" /bin/cat /root/at-pause
	# The restore brought up a new guest, and the host rules were applied again over it.
	expect_network "after the resume"
	expect_blocked "${ID}" "the policy holds after the resume"
	expect_fronted "${ID}" "the proxy fronts the sandbox after the resume"
}

# fork_steps fork the running sandbox where the provider forks, and prove the refusal by name where it does not (SHARD-457).
fork_steps() {
	if [ "$(status_field fork)" != "true" ]; then
		step "fork is refused by name on ${PROVIDER}"
		CODE=0
		REFUSAL=$(shard fork --name e2e-fork "${ID}" 2>&1) || CODE=$?
		[ "${CODE}" != "0" ] || fail "fork ran on ${PROVIDER}, which claims no fork"
		grep -q "${PROVIDER}" <<<"${REFUSAL}" || fail "the fork refusal '${REFUSAL}' does not name ${PROVIDER}"
		[ "$(listed_state "${ID}")" = "running" ] || fail "the refused fork moved the source off running"
		say "fork is refused by name on ${PROVIDER}, and the source runs on"

		return 0
	fi

	step "fork the running sandbox into a second sandbox"
	# A fork captures the source as it runs, and the same runtime runs on: same host pid, same entrypoint (SHARD-457).
	shard exec "${ID}" /bin/sh -c 'echo before-the-fork > /root/at-fork' >/dev/null
	CLOCK_BEFORE_FORK=$(entrypoint_clock "${ID}")
	SOURCE_PID=$(grep -o '"pid": *[0-9]*' "${RECORD}" | grep -o '[0-9]*$')
	timed "fork" fork_it
	[ -n "${FORK_ID}" ] && [ "${FORK_ID}" != "${ID}" ] || fail "fork printed '${FORK_ID}', want a new id"
	FORK_RECORD="${SHARD_ROOT}/sandboxes/${FORK_ID}/sandbox.json"
	FORK_ADDRESS=$(grep -o '"address": *"[^"]*"' "${FORK_RECORD}" | cut -d'"' -f4)
	FORK_LINK=$(grep -o '"host_interface": *"[^"]*"' "${FORK_RECORD}" | cut -d'"' -f4)
	[ "${FORK_ADDRESS}" != "${ADDRESS}" ] || fail "the fork got the source's address ${ADDRESS}"
	say "the fork is ${FORK_ID} on its own address ${FORK_ADDRESS} and link ${FORK_LINK}"

	[ "$(listed_state "${FORK_ID}")" = "running" ] || fail "shard list does not list the fork running"
	[ "$(listed_state "${ID}")" = "running" ] || fail "the fork moved the source off running"
	grep -Eq "\"pid\": *${SOURCE_PID}([^0-9]|$)" "${RECORD}" || fail "the source is no longer host pid ${SOURCE_PID} after the fork"
	expect "$(entrypoint_clock "${ID}")" "${CLOCK_BEFORE_FORK}" "the source's entrypoint is the same process with the same start time, so the source ran on"
	say "list shows the fork running beside the source, which runs on as host pid ${SOURCE_PID}"

	holds '"E2E_TOKEN"' shard inspect "${FORK_ID}" || fail "the fork did not carry the grant"
	holds '"policy": "e2e-policy"' shard inspect "${FORK_ID}" || fail "the fork did not carry the policy"
	expect_blocked "${FORK_ID}" "the policy holds on the fork"
	expect_exec_in "${FORK_ID}" "mock-E2E_TOKEN" "the fork holds the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'
	expect_fronted "${FORK_ID}" "the proxy fronts the fork on its own address"

	expect_exec_in "${FORK_ID}" "before-the-fork" "the fork holds the file the source wrote before the fork" /bin/cat /root/at-fork
	expect_exec_in "${FORK_ID}" "${FORK_ADDRESS}" "the fork holds its own address" \
		/bin/sh -c "ip -o -4 addr show eth0 | grep -o '${FORK_ADDRESS}'"
	expect_exec_in "${FORK_ID}" "reachable" "the fork gets out through the NAT" \
		/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
	expect_exec_in "${FORK_ID}" "e2e-fork" "the fork carries its own hostname" /bin/hostname
	shard exec "${FORK_ID}" /bin/sh -c 'echo fork-only > /root/fork-only' >/dev/null
	CODE=0
	shard exec "${ID}" /bin/cat /root/fork-only >/dev/null 2>&1 || CODE=$?
	[ "${CODE}" != "0" ] || fail "the source sees the file the fork wrote"
	say "the source does not see what the fork wrote"

	step "stop and remove the fork"
	shard stop "${FORK_ID}" >/dev/null
	shard remove "${FORK_ID}" >/dev/null
	absent "the fork's record" "$([ -e "${SHARD_ROOT}/sandboxes/${FORK_ID}" ] && echo "${SHARD_ROOT}/sandboxes/${FORK_ID}" || true)"
	absent "the fork's link" "$(ip link show "${FORK_LINK}" 2>/dev/null || true)"
	FORK_ID=""
	FORK_LINK=""
	# The sandbox the reconcile step makes and removes itself, kept here so a failure halfway still frees it.
	RECONCILE_ID=""
	RECONCILE_LINK=""
	expect_exec "before-the-fork" "the source runs on after the fork is gone" /bin/cat /root/at-fork
}

# E2E_LIB_ONLY lets the self-test source the helpers above without driving a sandbox.
if [ -n "${E2E_LIB_ONLY:-}" ]; then
	return 0
fi

trap on_exit EXIT

step "check the host"
RUNTIME=$(runtime_binary "${PROVIDER}")
[ "$(id -u)" = "0" ] || fail "shard drives netns, nft and ${RUNTIME}, so this needs root"
for binary in "${RUNTIME}" ip ss nft go curl openssl socat; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm, which is the box this ticket targets"
fi
say "${RUNTIME}, ip, ss, nft, go, curl, openssl and socat are on the host, and the run is on ${PROVIDER}"

check_host_is_free
say "no other sandbox holds a link on this host"

# The whole run owns one root, and it is never the production one.
check_root
say "this run owns the root ${SHARD_ROOT}"

step "install shard and its guest supervisor"
cd "$(dirname "$0")/.."
if [ "${SKIP_INSTALL:-0}" = "1" ]; then
	say "skipped, running against the binaries already in ${PREFIX}"
else
	# Build outside the tree: this runs as root, and root-owned files in a checkout are a trap.
	BUILD=$(mktemp -d)
	go build -o "${BUILD}/shard" ./cmd/shard
	# The supervisor is PID 1 in the guest, so it is static: the image may be musl or have no libc.
	CGO_ENABLED=0 go build -o "${BUILD}/shard-init" ./cmd/shard-init
	install -m0755 "${BUILD}/shard" "${PREFIX}/shard"
	install -m0755 "${BUILD}/shard-init" "${PREFIX}/shard-init"
	rm -rf "${BUILD}"
	say "installed shard and shard-init into ${PREFIX}"
fi

wipe_root
mkdir -p "${SHARD_ROOT}"

step "refuse every read with no daemon up"
SOCKET="${SHARD_ROOT}/shard.sock"
CODE=0
REFUSAL=$(shard list 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "shard list answered with no daemon up"
expect "${REFUSAL}" "shard: cannot connect to shard daemon at ${SOCKET}: is it running? shard --root ${SHARD_ROOT} daemon" "list names the socket and this root's daemon, and nothing else"

step "start the echo the fronted sandbox talks to"
start_echo
say "the echo answers on ${ECHO_ADDRESS} in the netns ${ECHO_NETNS}, ports 80 and 443, as ${ECHO_HOST} and ${OTHER_HOST}"

step "start the daemon in the background"
start_daemon || fail "the daemon did not come up"
[ -S "${SOCKET}" ] || fail "no socket at ${SOCKET}"
LISTEN_LINE=$(grep "api listening on" "${DAEMON_LOG}")
say "the daemon logged: ${LISTEN_LINE#* api }"
say "the daemon logged: $(grep 'proxy listening on' "${DAEMON_LOG}" | sed 's/.*proxy/proxy/')"
say "the daemon logged: $(grep 'dns resolver listening on' "${DAEMON_LOG}" | sed 's/.*dns resolver/dns resolver/')"

step "prove the socket mode is what the daemon claims"
if getent group shard >/dev/null; then
	WANT_MODE="0660"
	WANT_GROUP="shard"
	grep -q "mode 0660, group shard" <<<"${LISTEN_LINE}" || fail "the host has a shard group and the daemon logged '${LISTEN_LINE}'"
else
	WANT_MODE="0600"
	WANT_GROUP="root"
	grep -q "mode 0600, no shard group" <<<"${LISTEN_LINE}" || fail "the host has no shard group and the daemon logged '${LISTEN_LINE}'"
fi
expect "$(stat -c '%a %U:%G' "${SOCKET}")" "${WANT_MODE#0} root:${WANT_GROUP}" "the socket sits at ${WANT_MODE} root:${WANT_GROUP}, as logged"

step "read both versions"
VERSION_OUT=$(shard version)
CLIENT_LINE=$(echo "${VERSION_OUT}" | sed -n 1p)
DAEMON_LINE=$(echo "${VERSION_OUT}" | sed -n 2p)
[ "${CLIENT_LINE#client }" != "${CLIENT_LINE}" ] || fail "the first line of shard version is '${CLIENT_LINE}', want 'client <v>'"
expect "${DAEMON_LINE}" "daemon ${CLIENT_LINE#client }" "version prints the client line and the daemon line, and both agree"

step "read the daemon status"
STATUS_OUT=$(shard daemon status)
# The status is read as name value pairs, one per line, so a field is checked by name and not by row.
status_field() { echo "${STATUS_OUT}" | awk -v name="$1" '$1 == name { print $2 }'; }
expect "$(status_field pid)" "${DAEMON_PID}" "the status names the pid of the daemon this run started"
expect "$(status_field socket)" "${SOCKET}" "the status names the socket the CLI speaks to"
expect "$(status_field provider)" "${PROVIDER}" "the status names the provider the daemon was started with"
expect "$(status_field version)" "${CLIENT_LINE#client }" "the status carries the version the daemon reports"
expect "$(status_field plain_port) $(status_field tls_port)" "30080 30443" "the status names the proxy ports"
if [ "${PROVIDER}" = "gvisor" ]; then
	expect "$(status_field pause) $(status_field resume) $(status_field fork)" "true true true" "gvisor claims every optional verb"
else
	expect "$(status_field pause) $(status_field resume) $(status_field fork)" "false false false" "${PROVIDER} claims no optional verb"
fi

step "read what the host picks"
# info asks the host, not the socket, so it says what a daemon started without --provider would run on.
info_field() { shard info | awk -v name="${FIELD}" '$1 == name { $1 = ""; sub(/^ +/, ""); print }'; }
WANT_PICK=gvisor
WANT_REASON="no /dev/kvm"
# The probe opens the node, so a /dev/kvm this host will not open leaves the pick at gvisor.
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
	WANT_PICK=firecracker
	WANT_REASON="/dev/kvm opens"
fi
expect "$(FIELD=provider info_field)" "${WANT_PICK}" "the host picks ${WANT_PICK} while the root holds no record"
expect "$(FIELD=reason info_field)" "${WANT_REASON}" "info names the reason it picked ${WANT_PICK}"
# --provider is the daemon's own flag, so before the verb it is refused with the command that takes it. No pipe: grep -q closes one early under pipefail.
CODE=0
REFUSAL=$(shard --provider sysbox info 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "info took --provider before the verb"
case "${REFUSAL}" in
*"shard daemon --provider sysbox"*) ;;
*) fail "info said '${REFUSAL}', want it to name shard daemon --provider sysbox" ;;
esac
say "info refuses --provider and names shard daemon --provider"

step "store a secret"
# The value is synthetic and unique to this run, so a grep of the root can prove where it is and is not.
SECRET_VALUE="e2e-secret-value-$$-$(date +%s)"
printf '%s\n' "${SECRET_VALUE}" | shard secret set --to "${ECHO_HOST}" E2E_TOKEN >/dev/null
# The second secret names its own placeholder, for an SDK that checks the shape of a key before it sends it.
SHAPED_PLACEHOLDER="sk_test_e2eplaceholder01"
SHAPED_VALUE="sk_live_e2e_$$_$(date +%s)"
CAUTION=$(shard secret set --to "${ECHO_HOST}" --placeholder "${SHAPED_PLACEHOLDER}" E2E_SHAPED "${SHAPED_VALUE}" 2>&1 >/dev/null)
grep -q "visible in the process list" <<<"${CAUTION}" || fail "a value on the command line printed no caution: '${CAUTION}'"
say "a value on the command line is stored, with a caution on stderr"
# The caution is about what ps saw, and a refusal does not un-see it.
CODE=0
REFUSED_CAUTION=$(shard secret set --to no-dot E2E_REFUSED "${SHAPED_VALUE}" 2>&1 >/dev/null) || CODE=$?
[ "${CODE}" != "0" ] || fail "secret set took a destination with no dot"
grep -q "caution" <<<"${REFUSED_CAUTION}" || fail "a refused set printed no caution: '${REFUSED_CAUTION}'"
say "a refused set still cautions about the value on the command line"
SHAPED_LS=$(shard secret list)
grep -q "${SHAPED_PLACEHOLDER}" <<<"${SHAPED_LS}" || fail "shard secret list does not print the chosen placeholder: ${SHAPED_LS}"
say "secret list prints the chosen placeholder"
SECRET_LS=$(shard secret list)
grep -q "E2E_TOKEN" <<<"${SECRET_LS}" || fail "shard secret list does not list E2E_TOKEN: ${SECRET_LS}"
grep -q "${SECRET_VALUE}" <<<"${SECRET_LS}" && fail "shard secret list printed the value"
say "secret list lists the name and the destination, and not the value"
SECRET_MODE=$(stat -c '%a' "${SHARD_ROOT}/secrets/E2E_TOKEN")
[ "${SECRET_MODE}" = "600" ] || fail "the secret file is mode ${SECRET_MODE}, want 600"
say "the secret file is mode 0600"

step "store an egress policy"
# The probe address is allowed on every protocol, so the ping the network checks use goes through. The
# other echo name is allowed by rule and not granted, so a request to it must keep the placeholder.
shard policy create --deny any e2e-deny-all >/dev/null
shard policy create --allow 1.1.1.1 --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
POLICY_LS=$(shard policy list)
grep -q "e2e-policy" <<<"${POLICY_LS}" || fail "shard policy list does not list e2e-policy: ${POLICY_LS}"
holds '"kind": "cidr"' shard policy show e2e-policy || fail "shard policy show does not print the rules"
say "policy list lists the policies and policy show prints the rules"
shard policy create --allow suffix:example.com --allow '*.example.com' e2e-web >/dev/null
shard policy remove e2e-web >/dev/null
say "policy create accepts a suffix rule and a wildcard rule, which the proxy matches"
CODE=0
shard policy create --allow 'api.example.com tcp:22' e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted a domain rule on a raw port"
CODE=0
shard policy create --allow 'api*.example.com' e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted a wildcard inside a label"
CODE=0
shard policy create --allow 'domain:api.example.com' e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted the old kind:value spelling"
CODE=0
shard policy create --allow private e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted a rule naming the private ranges"
say "policy create refuses a raw-port name rule, an in-label wildcard, the old spelling and private"

step "run a sandbox detached"
# The app speaks once, so logs has something to show, and then holds the sandbox up.
ID=$(shard run -d --secret E2E_TOKEN --secret E2E_SHAPED --policy e2e-policy "${IMAGE}" /bin/sh -c 'echo shard-e2e-entrypoint; exec /bin/sleep 600')
[ -n "${ID}" ] || fail "run -d printed no id"
say "run -d printed the id ${ID}"

RECORD="${SHARD_ROOT}/sandboxes/${ID}/sandbox.json"
[ -f "${RECORD}" ] || fail "there is no record at ${RECORD}"
ADDRESS=$(grep -o '"address": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
LINK=$(grep -o '"host_interface": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
say "the record holds the address ${ADDRESS} on the link ${LINK}"

ip netns list | has_line "^${ID}" || fail "there is no namespace named ${ID}"
ip link show "${LINK}" >/dev/null || fail "there is no link named ${LINK}"
say "the namespace and the link are up"

step "list the sandbox"
LISTED=$(shard list | grep "^${ID}" || true)
[ -n "${LISTED}" ] || fail "shard list does not list ${ID}"
grep -q "${ADDRESS%%/*}" <<<"${LISTED}" || fail "shard list listed '${LISTED}', want the address ${ADDRESS%%/*} on it"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard list listed '${LISTED}', want it running"
say "list shows the sandbox running on its address"

# SHARD-46: a root that holds records keeps what made them, so an upgrade on a KVM host switches nothing.
expect "$(FIELD=provider info_field)" "${PROVIDER}" "the root now keeps ${PROVIDER}, which made its records"

step "restart the daemon and prove the sandbox and an exec in flight outlive it"
SANDBOX_PID=$(grep -o '"pid": *[0-9]*' "${RECORD}" | grep -o '[0-9]*$')
[ -n "${SANDBOX_PID}" ] || fail "the record holds no pid"
# The driver's scratch lives under the root and the driver dies with the daemon, so /tmp must stay as it was.
TMP_EXECS_BEFORE=$(find /tmp -maxdepth 1 -name 'shard-exec-*' | wc -l)
# The exec is in flight when the daemon goes: its client dies with the stream, its guest process must not.
(shard exec "${ID}" /bin/sleep 313 >/dev/null 2>&1 || true) &
EXEC_CLIENT_PID=$!
for _ in $(seq 1 50); do
	nap_alive && break
	sleep 0.2
done
nap_alive || fail "the background exec never started in the guest"
say "an exec runs /bin/sleep 313 in the guest"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
kill -0 "${SANDBOX_PID}" 2>/dev/null || fail "the sandbox process ${SANDBOX_PID} died with the daemon"
say "the daemon is down and the sandbox process ${SANDBOX_PID} is still up"
start_daemon || fail "the daemon did not come up"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard list does not list ${ID} running after the daemon restart"
expect_exec "restarted" "an exec answers after the daemon restart" /bin/echo restarted
expect "$(find /tmp -maxdepth 1 -name 'shard-exec-*' | wc -l)" "${TMP_EXECS_BEFORE}" "the restart left no shard-exec directory under /tmp"
expect "$(find "${SHARD_ROOT}/exec" -mindepth 1 -maxdepth 1 | wc -l)" "0" "the new daemon swept the exec scratch under its root"
# The driver of the exec in flight dies with the daemon, so no runsc exec of this root is left on init.
for _ in $(seq 1 20); do
	ORPHANS=$(ps -eo ppid=,args= | awk -v root="${SHARD_ROOT}" '$1 == 1 && $2 ~ /(runsc|sysbox-runc|runc)$/ && index($0, root) && / exec /' | wc -l)
	[ "${ORPHANS}" = "0" ] && break
	sleep 0.1
done
expect "${ORPHANS}" "0" "no exec driver of this root is orphaned on init"
# An exec is a record now, so a curl GET names it and its exit after the shard exec that ran it returns.
EXECS=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${ID}/exec")
EXEC_ID=$(echo "${EXECS}" | grep -o '"exec": *"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "${EXEC_ID}" ] || fail "the sandbox lists no exec after a shard exec returned: ${EXECS}"
RECORD_JSON=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${ID}/exec/${EXEC_ID}")
grep -q '"state": *"exited"' <<<"${RECORD_JSON}" || fail "the exec record ${EXEC_ID} is not exited: ${RECORD_JSON}"
say "a curl GET answers the exited exec record ${EXEC_ID} that shard exec left behind"
# grep -c reads to the end, so nft never takes a SIGPIPE that pipefail would count as a miss.
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK} after the daemon restart"
say "the host still holds the egress chain of the sandbox"
expect_exec "alive" "the guest process of the exec in flight outlived the daemon" \
	/bin/sh -c 'pgrep -f "[s]leep 313" >/dev/null && echo alive'
# The pause step reads the entrypoint by name, so the nap must be gone before it: nothing reattaches to it.
shard exec "${ID}" /bin/sh -c 'pkill -f "[s]leep 313"' >/dev/null
wait "${EXEC_CLIENT_PID}" 2>/dev/null || true
say "the client of that exec is gone, and no verb reattaches to it"

step "reconcile a sandbox the host lost while the daemon was down"
RECONCILE_ID=$(shard run -d --name e2e-lost "${IMAGE}" /bin/sleep 600)
RECONCILE_RECORD="${SHARD_ROOT}/sandboxes/${RECONCILE_ID}/sandbox.json"
RECONCILE_LINK=$(grep -o '"host_interface": *"[^"]*"' "${RECONCILE_RECORD}" | cut -d'"' -f4)
RECONCILE_PID=$(grep -o '"pid": *[0-9]*' "${RECONCILE_RECORD}" | grep -o '[0-9]*$')
[ -n "${RECONCILE_PID}" ] || fail "the record of ${RECONCILE_ID} holds no pid"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
kill -9 "${RECONCILE_PID}" 2>/dev/null || true
for _ in $(seq 1 50); do
	kill -0 "${RECONCILE_PID}" 2>/dev/null || break
	sleep 0.1
done
kill -0 "${RECONCILE_PID}" 2>/dev/null && fail "the sandbox process ${RECONCILE_PID} survived the kill"
say "the sandbox process ${RECONCILE_PID} is gone and the record still says running"
start_daemon || fail "the daemon did not come up"
expect "$(listed_state "${RECONCILE_ID}")" "stopped" "the daemon corrected the record of the sandbox it lost"
holds "^${RECONCILE_ID}.*daemon restarted and found no process" shard list --all || fail "shard list gives no reason for ${RECONCILE_ID}"
say "list gives the reason: daemon restarted and found no process"
grep -q "${RECONCILE_ID}" "${DAEMON_LOG}" || fail "the daemon logged no line for the record it corrected"
say "the daemon logged the record it corrected"
# Two records exist here, one running and one stopped, which is enough to page the list over the socket.
PAGE=$(curl -sS --unix-socket "${SOCKET}" 'http://shard/v0/sandboxes?all=true&limit=1')
NEXT=$(echo "${PAGE}" | grep -o '"next": *"[^"]*"' | cut -d'"' -f4)
[ -n "${NEXT}" ] || fail "the first page of two sandboxes carries no next: ${PAGE}"
expect "$(echo "${PAGE}" | grep -o '"id": *"[^"]*"' | wc -l | tr -d ' ')" "1" "limit=1 answers one sandbox and a next"
PAGE=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes?all=true&limit=1&cursor=${NEXT}")
expect "$(echo "${PAGE}" | grep -o '"id": *"[^"]*"' | wc -l | tr -d ' ')" "1" "the cursor answers the other sandbox"
expect "$(echo "${PAGE}" | grep -c '"next": *null')" "1" "the last page carries a null next"
CODE=$(curl -sS -o /dev/null -w '%{http_code}' --unix-socket "${SOCKET}" 'http://shard/v0/sandboxes?cursor=not/an-id')
expect "${CODE}" "400" "a malformed cursor is refused"
shard remove --force "${RECONCILE_ID}" >/dev/null || fail "remove did not free the sandbox the host lost"
ip link delete "${RECONCILE_LINK}" >/dev/null 2>&1 || true
RECONCILE_ID=""
RECONCILE_LINK=""
say "remove freed what it left on the host"

step "read the output of the entrypoint"
# The line lands when the guest gets to it, which is after create returned.
for _ in $(seq 1 50); do
	holds "shard-e2e-entrypoint" shard logs "${ID}" && break
	sleep 0.2
done
holds "shard-e2e-entrypoint" shard logs "${ID}" || fail "shard logs does not show what the entrypoint wrote"
say "logs shows what the entrypoint wrote"

step "exec a command in the sandbox"
expect_exec "shard-e2e" "the command ran and wrote a file" \
	/bin/sh -c 'echo shard-e2e > /tmp/marker; cat /tmp/marker'

step "exec again into the same filesystem state"
expect_exec "shard-e2e" "the second exec read what the first one wrote" /bin/cat /tmp/marker
expect_exec "pong" "a listener on 127.0.0.1 answers, so lo is up" \
	/bin/sh -c '(echo pong | nc -l -p 7077 -s 127.0.0.1 -w 3 &); sleep 1; nc -w 3 127.0.0.1 7077 </dev/null'

step "reach the daemon through the tcp front"
SERVE_DIR=$(mktemp -d /tmp/shard-e2e-serve.XXXXXX)
SIGNING_KEY="${SERVE_DIR}/signing-key"
SERVE_TOKEN="${SERVE_DIR}/serve.token"
SERVE_LOG="${SERVE_DIR}/serve.log"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
	-subj "/CN=127.0.0.1" -addext "subjectAltName=IP:127.0.0.1" \
	-keyout "${SERVE_DIR}/serve.key" -out "${SERVE_DIR}/serve.crt" >/dev/null 2>&1 ||
	fail "openssl did not make a self-signed pair"
openssl rand -hex 32 >"${SIGNING_KEY}"
chmod 0600 "${SIGNING_KEY}"
# The front verifies a JWT; tokens mint writes the JSON record to SERVE_TOKEN, read whole by --token-file.
"${PREFIX}/shard" tokens mint --name shard-e2e --signing-key-file "${SIGNING_KEY}" >"${SERVE_TOKEN}" ||
	fail "tokens mint did not print a token"
chmod 0600 "${SERVE_TOKEN}"
say "the run made a certificate for socat and its own signing key, and minted a token"

SERVE_PIDS="${SERVE_PIDS} $(start_serve "${SHARD_ROOT}" "${SERVE_PORT}" "${SERVE_LOG}" --signing-key-file "${SIGNING_KEY}")" ||
	fail "the front did not come up: $(cat "${SERVE_LOG}")"
say "shard serve is listening on 127.0.0.1:${SERVE_PORT}"
SERVE_PIDS="${SERVE_PIDS} $(start_front_tls "${FRONT_TLS_PORT}" "${SERVE_PORT}")" ||
	fail "socat did not listen on 127.0.0.1:${FRONT_TLS_PORT}"
say "socat terminates TLS on 127.0.0.1:${FRONT_TLS_PORT} in front of it"
curl -sSk "https://127.0.0.1:${SERVE_PORT}/v0/sandboxes" >/dev/null 2>&1 && fail "the front answered a TLS handshake"
say "the front itself speaks plain http, and no TLS"

# The bearer header and the log check need the bare jwt, so pull it from the record with jq.
TOKEN=$(jq -r .token "${SERVE_TOKEN}")
OVER_TCP=$(front_curl "${SERVE_PORT}" "${TOKEN}" /v0/sandboxes)
OVER_SOCKET=$(curl -sS --unix-socket "${SHARD_ROOT}/shard.sock" http://shard/v0/sandboxes)
expect "${OVER_TCP}" "${OVER_SOCKET}" "the front answers a request byte for byte as the socket does"

CODE=$(front_curl "${SERVE_PORT}" "wrong-${TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "a wrong token is refused"
BODY=$(front_curl "${SERVE_PORT}" "wrong-${TOKEN}" /v0/sandboxes)
expect "${BODY}" '{"error":{"code":"unauthorized","message":"the request carries no valid bearer token"}}' \
	"the refusal carries a code like every other error body"
CODE=$(front_curl "${SERVE_PORT}" "" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "no token at all is refused"

# A token the signing key signed but whose lifetime has passed is refused, so the front checks expiry.
EXPIRED=$("${PREFIX}/shard" tokens mint --name shard-e2e --duration 1s --signing-key-file "${SIGNING_KEY}" | jq -r .token) ||
	fail "tokens mint did not print a short-lived token"
sleep 2
CODE=$(front_curl "${SERVE_PORT}" "${EXPIRED}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "an expired token is refused"

# A token another key signed is refused, so the front checks the signature against its own signing key.
OTHER_KEY="${SERVE_DIR}/other-signing-key"
openssl rand -hex 32 >"${OTHER_KEY}"
chmod 0600 "${OTHER_KEY}"
OTHER_TOKEN=$("${PREFIX}/shard" tokens mint --name shard-e2e --signing-key-file "${OTHER_KEY}" | jq -r .token) ||
	fail "tokens mint did not print a token from the other signing key"
CODE=$(front_curl "${SERVE_PORT}" "${OTHER_TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "a token signed with a different signing key is refused"

# A front over a root no daemon owns cannot answer anything but the refusal, so a 401 here proves
# the check runs before the dial: only the request that carried the token reached a socket at all.
LONE_ROOT=$(mktemp -d /tmp/shard-e2e-lone.XXXXXX)
LONE_LOG="${SERVE_DIR}/lone.log"
SERVE_PIDS="${SERVE_PIDS} $(start_serve "${LONE_ROOT}" "${LONE_PORT}" "${LONE_LOG}" --signing-key-file "${SIGNING_KEY}")" ||
	fail "the second front did not come up: $(cat "${LONE_LOG}")"
CODE=$(front_curl "${LONE_PORT}" "wrong-${TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "a wrong token is refused by a front that fronts nothing"
CODE=$(front_curl "${LONE_PORT}" "${EXPIRED}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "an expired token is refused by a front that fronts nothing"
CODE=$(front_curl "${LONE_PORT}" "${OTHER_TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "a token from a different signing key is refused by a front that fronts nothing"
CODE=$(front_curl "${LONE_PORT}" "${TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "502" "the same front cannot reach a daemon that is not there"
expect "$(grep -c 'dial the daemon socket' "${LONE_LOG}")" "1" \
	"only the authorized request was dialed, and the refused ones were not"

grep -q "${TOKEN}" "${SERVE_LOG}" "${LONE_LOG}" "${DAEMON_LOG}" && fail "a log holds the token value"
say "no log holds the token value"

step "drive a verb and an exec through the front"
holds "${ID}" shard_front list --all || fail "list over the front does not list the sandbox"
say "list over the front lists the sandbox"
holds "daemon" shard_front version || fail "version over the front does not name the daemon"
say "version over the front reaches the daemon"
GOT=$(shard_front exec "${ID}" /bin/cat /tmp/marker) || fail "exec over the front failed"
expect "${GOT}" "shard-e2e" "exec over the front read what the first exec wrote"
GOT=$(printf 'over-tls\n' | shard_front exec -i "${ID}" /bin/cat) || fail "exec with stdin over the front failed"
expect "${GOT}" "over-tls" "the websocket of an exec passes through the front both ways"

step "a read-only token reads through the front but is refused a write"
# The front maps the request line to a capability and refuses a write the token's scopes do not reach.
READONLY=$("${PREFIX}/shard" tokens mint --name shard-e2e --scopes sandbox:read --signing-key-file "${SIGNING_KEY}" | jq -r .token) ||
	fail "tokens mint did not print a read-only token"
CODE=$(front_curl "${SERVE_PORT}" "${READONLY}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "200" "a sandbox:read token lists the sandboxes"
CODE=$(front_curl "${SERVE_PORT}" "${READONLY}" /v0/sandboxes -X POST -o /dev/null -w '%{http_code}')
expect "${CODE}" "403" "a sandbox:read token is refused a create"
BODY=$(front_curl "${SERVE_PORT}" "${READONLY}" /v0/sandboxes -X POST)
expect "${BODY}" '{"error":{"code":"forbidden","message":"the token does not carry a scope for this route"}}' \
	"the refusal carries the forbidden code"

step "a revoked token is refused on the next request, with no restart"
# The front reloads the ledger per request, so revoke takes effect at once with no restart.
REVOKE_TOKEN=$("${PREFIX}/shard" tokens mint --name shard-e2e-revoke --signing-key-file "${SIGNING_KEY}" | jq -r .token) ||
	fail "tokens mint did not print a token to revoke"
CODE=$(front_curl "${SERVE_PORT}" "${REVOKE_TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "200" "the token reads before it is revoked"
JTI=$("${PREFIX}/shard" tokens list --signing-key-file "${SIGNING_KEY}" | awk '$2=="shard-e2e-revoke"{print $1}')
[ -n "${JTI}" ] || fail "tokens list did not list the minted token"
"${PREFIX}/shard" tokens revoke --signing-key-file "${SIGNING_KEY}" "${JTI}" || fail "tokens revoke failed"
CODE=$(front_curl "${SERVE_PORT}" "${REVOKE_TOKEN}" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "401" "the revoked token is refused on the next request"
say "a revoked token is refused at once, with no restart"

stop_serve
rm -rf "${LONE_ROOT}"
LONE_ROOT=""
say "both fronts and socat are down"

step "mint and serve create the default signing key on first use, and share it"
# The daemon has run over this root since the start, so an absent auth dir proves its startup never made one.
[ ! -e "${SHARD_ROOT}/auth" ] || fail "the daemon made ${SHARD_ROOT}/auth, which only tokens mint and serve create"
DEFAULT_TOKEN="${SERVE_DIR}/default.token"
DEFAULT_LOG="${SERVE_DIR}/default.log"
shard tokens mint --name shard-e2e-default >"${DEFAULT_TOKEN}" || fail "tokens mint with no key flag did not print a token"
chmod 0600 "${DEFAULT_TOKEN}"
expect "$(stat -c '%a' "${SHARD_ROOT}/auth")" "700" "tokens mint made the auth dir at 0700"
expect "$(stat -c '%a' "${SHARD_ROOT}/auth/signing-key")" "600" "tokens mint made the signing key at 0600"
SERVE_PIDS="${SERVE_PIDS} $(start_serve "${SHARD_ROOT}" "${SERVE_PORT}" "${DEFAULT_LOG}")" ||
	fail "the front with no key flag did not come up: $(cat "${DEFAULT_LOG}")"
CODE=$(front_curl "${SERVE_PORT}" "$(jq -r .token "${DEFAULT_TOKEN}")" /v0/sandboxes -o /dev/null -w '%{http_code}')
expect "${CODE}" "200" "the front with no key flag accepts the token minted with no key flag"
DEFAULT_JTI=$(shard tokens list | awk '$2=="shard-e2e-default"{print $1}')
[ -n "${DEFAULT_JTI}" ] || fail "tokens list with no flags did not list the token minted with no flags"
say "tokens list with no flags reads the ledger beside the default signing key"
stop_serve
say "the front over the default signing key is down"

step "hold the placeholder and never the value"
expect_exec "mock-E2E_TOKEN" "the guest sees the placeholder as \$E2E_TOKEN" /bin/sh -c 'echo "$E2E_TOKEN"'
expect_exec "${SHAPED_PLACEHOLDER}" "the guest sees the chosen placeholder as \$E2E_SHAPED" /bin/sh -c 'echo "$E2E_SHAPED"'
printf '%s\n' "${SHAPED_VALUE}" | shard secret set --placeholder sk_test_othershape01 E2E_SHAPED >/dev/null 2>"${SHARD_ROOT}/moved.err" && fail "secret set moved a placeholder the sandbox holds"
grep -q "${ID}" "${SHARD_ROOT}/moved.err" || fail "the refusal does not name the sandbox: $(cat "${SHARD_ROOT}/moved.err")"
say "secret set refuses to move a placeholder the sandbox holds, and names it"
# The store file is the one place the value is written; nothing under the sandbox tree or anywhere else holds it.
absent "the value outside the store" "$(grep -rl --exclude-dir=secrets "${SECRET_VALUE}" "${SHARD_ROOT}" 2>/dev/null || true)"
holds '"E2E_TOKEN"' shard inspect "${ID}" || fail "inspect does not name the grant"
say "inspect names the secret and holds no value"
shard secret remove E2E_TOKEN >/dev/null 2>&1 && fail "secret remove removed a secret a sandbox holds"
say "secret remove refuses while the sandbox holds the secret"

step "front the sandbox through the proxy"
# grep -c reads to the end, so nft never takes a SIGPIPE that pipefail would count as a miss.
nft list table inet shard | grep -c "dnat ip to .*:30080" >/dev/null || fail "the host holds no dnat to the proxy for ${LINK}"
say "the host turns the sandbox's 80 and 443 to the proxy"
# The guest trusts the proxy CA beside the image's own roots, at the path the image already reads.
CA_LINE=$(sed -n 2p "${SHARD_ROOT}/proxy/ca.crt")
GUEST_BUNDLE=$(shard exec "${ID}" /bin/sh -c 'cat "$SSL_CERT_FILE"')
grep -qF "${CA_LINE}" <<<"${GUEST_BUNDLE}" || fail "the guest's \$SSL_CERT_FILE does not hold the proxy CA"
[ "$(echo "${GUEST_BUNDLE}" | grep -c 'BEGIN CERTIFICATE')" -gt 1 ] || fail "the guest's bundle holds the proxy CA alone"
say "the guest trusts the proxy CA and still trusts the image's roots"

step "a grant opens nothing: the policy alone decides the host"
# The policy so far names 1.1.1.1 and the other host, so the granted host falls to the catch-all at the resolver.
DENIED=$(shard exec "${ID}" /bin/sh -c "wget -S -O - --header \"Authorization: Bearer \$E2E_TOKEN\" http://${ECHO_HOST}/ 2>&1" || true)
grep -q "bad address" <<<"${DENIED}" || fail "the granted host the policy does not allow answered '${DENIED}'"
grep -q "authorization=" <<<"${DENIED}" && fail "the echo answered a request the resolver should have refused"
say "a granted host the policy does not allow does not resolve, and the echo never sees the request"

# The answer and the log line are two writes, so the log is read until the line lands.
DNS_DENY=""
for _ in $(seq 1 20); do
	DNS_DENY=$(shard policy logs "${ID}" | grep '"source":"dns"' | grep '"verdict":"deny"' | grep "\"host\":\"${ECHO_HOST}\"" || true)
	[ -n "${DNS_DENY}" ] && break
	sleep 0.1
done
[ -n "${DNS_DENY}" ] || fail "the egress log holds no dns deny for ${ECHO_HOST}"
grep -qE '"rule_text":"deny (any|0\.0\.0\.0/0)"' <<<"${DNS_DENY}" || fail "the deny does not name the catch-all: ${DNS_DENY}"
say "the egress log names the catch-all that denied it, with source dns"

step "a secret opens no DNS either"
# Only address rules, so nothing implies port 53. The address is not a nameserver: allowing one would
# open DNS by address and prove nothing.
shard policy create --allow 1.0.0.1 --deny any e2e-policy >/dev/null
# The grant step already logged a deny for the name, so only a line past that count proves this lookup.
DNS_DENIES_BEFORE=$(shard policy logs "${ID}" | grep '"source":"dns"' | grep '"verdict":"deny"' | grep -c "\"host\":\"${ECHO_HOST}\"" || true)
expect_exec "unresolved" "a policy of addresses only leaves the granted host unresolvable" \
	/bin/sh -c "timeout 5 nslookup ${ECHO_HOST} >/dev/null 2>&1 && echo resolved || echo unresolved"

NOTE=$(shard policy create --allow 1.0.0.1 --deny any e2e-note 2>&1 >/dev/null)
grep -q "this policy opens no DNS" <<<"${NOTE}" || fail "policy create said '${NOTE}' over a policy that opens no DNS"
shard policy remove e2e-note >/dev/null
say "policy create notes a policy that opens no DNS, and exits 0"

# The lookup the address-only policy refused is in the log, as a dns deny for the name the guest asked.
DNS_DENIES=0
for _ in $(seq 1 20); do
	DNS_DENIES=$(shard policy logs "${ID}" | grep '"source":"dns"' | grep '"verdict":"deny"' | grep -c "\"host\":\"${ECHO_HOST}\"" || true)
	[ "${DNS_DENIES}" -gt "${DNS_DENIES_BEFORE}" ] && break
	sleep 0.1
done
[ "${DNS_DENIES}" -gt "${DNS_DENIES_BEFORE}" ] || fail "the egress log holds no dns deny for the lookup of ${ECHO_HOST} the policy closed"
say "the refused lookup is in the egress log, as a dns deny for ${ECHO_HOST}"

step "allow dns opens the lookup the address rules left shut"
shard policy create --allow 1.0.0.1 --allow dns --allow "${ECHO_HOST}" --deny any e2e-policy >/dev/null
expect_exec "resolved" "the same policy with --allow dns resolves the echo name" \
	/bin/sh -c "timeout 5 nslookup ${ECHO_HOST} >/dev/null 2>&1 && echo resolved || echo unresolved"
holds '"dns": "open"' shard policy show e2e-policy || fail "policy show does not say dns is open"
holds '"implied": "dns rule"' shard inspect "${ID}" || fail "inspect does not name the dns rule that opened 53"
say "policy show says dns is open and inspect names the rule that opened 53"

CODE=0
shard policy create --deny dns e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted a deny dns rule"
say "policy create refuses deny dns: dns is closed until a rule opens it"

# From here the policy allows the granted host, which is what every step below needs.
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null

expect_fronted "${ID}" "a request to the granted host carries the value, and the guest only ever sent the placeholder"
GOT=$(fetch "${ID}" https "${ECHO_HOST}") || fail "the https request to ${ECHO_HOST} failed"
grep -qx "x-shaped=$(seen "${SHAPED_VALUE}")" <<<"${GOT}" || fail "the echo saw '${GOT}' over tls, want the value under the chosen placeholder"
say "the chosen placeholder carries its own value"

GOT=$(fetch "${ID}" http "${ECHO_HOST}") || fail "the http request to ${ECHO_HOST} failed"
grep -qx "authorization=$(seen "Bearer mock-E2E_TOKEN")" <<<"${GOT}" || fail "the echo saw '${GOT}' over plain http, want the placeholder, never the value in cleartext"
grep -qx "x-shaped=$(seen "${SHAPED_PLACEHOLDER}")" <<<"${GOT}" || fail "the echo saw '${GOT}' over plain http, want the chosen placeholder untouched"
say "plain http to the granted host keeps both placeholders: the value goes out over tls alone"

GOT=$(fetch "${ID}" https "${OTHER_HOST}") || fail "the https request to ${OTHER_HOST} failed"
grep -qx "authorization=$(seen "Bearer mock-E2E_TOKEN")" <<<"${GOT}" || fail "the echo saw '${GOT}' from the other host, want the placeholder untouched"
grep -qx "x-shaped=$(seen "${SHAPED_PLACEHOLDER}")" <<<"${GOT}" || fail "the echo saw '${GOT}' from the other host, want the chosen placeholder untouched"
say "a request to a host the policy allows but the grant does not keeps both placeholders"

step "a client that encodes the placeholder still gets the value"
expect "$(basic_of "${ID}" "${ECHO_HOST}")" "$(basic "${SECRET_VALUE}")" "basic auth to the granted host carries the value, decoded and re-encoded"
expect "$(basic_of "${ID}" "${OTHER_HOST}")" "$(basic mock-E2E_TOKEN)" "basic auth to an ungranted host keeps the placeholder"

expect_exec "bad address" "a request to a host no rule allows is refused at the resolver" \
	/bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ 2>&1 | grep -o 'bad address' | head -1"
step "a policy deny closes a granted host"
# allow dns first, so the name resolves and the deny is the proxy's: the grant does not open what the policy closes.
shard policy create --allow dns --deny "${ECHO_HOST}" --allow 1.1.1.1 --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_exec "403 Forbidden" "a request to the granted host the policy denies gets a 403" \
	/bin/sh -c "wget -S -O /dev/null --header \"Authorization: Bearer \$E2E_TOKEN\" http://${ECHO_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_fronted "${ID}" "the same grant passes again once the policy allows the host"

expect_env_clean "${ID}"
absent "the value in the daemon log" "$(grep -l "${SECRET_VALUE}" "${DAEMON_LOG}" || true)"
absent "the value in the sandbox tree" "$(grep -rl "${SECRET_VALUE}" "${SHARD_ROOT}/sandboxes/${ID}" 2>/dev/null || true)"

step "reach the network from the sandbox"
expect_network "after the create"

step "enforce the egress policy"
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK}"
nft list table bridge shard | grep -c "iifname \"${LINK}\"" >/dev/null || fail "the host does not pin the address of ${LINK}"
say "the host holds a chain for the sandbox and pins its address"
expect_blocked "${ID}" "the guest cannot reach an address the policy denies"
# The probe is only proof once the same address answers when a rule allows it.
shard policy create --allow 1.1.1.1 --allow 8.8.8.8 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_exec "reachable" "the same address answers once a rule allows it" \
	/bin/sh -c 'ping -c 1 -W 3 8.8.8.8 >/dev/null && echo reachable'
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_exec "blocked" "the floor holds under the policy: the metadata address is dropped" \
	/bin/sh -c 'ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 && echo reachable || echo blocked'
expect_exec "blocked" "the floor holds under the policy: the gateway is dropped" \
	/bin/sh -c 'ping -c 1 -W 2 10.87.0.1 >/dev/null 2>&1 && echo reachable || echo blocked'
# The host keeps the proxy ports on its own address and drops the rest, and that drop is logged too.
expect_exec "blocked" "the floor holds under the policy: a non-web port on the host is dropped" \
	/bin/sh -c 'wget -T 2 -q -O /dev/null http://10.87.0.1:5432/ >/dev/null 2>&1 && echo reachable || echo blocked'
# IPv6 matches no rule in the forward path, so the port it came in on drops it and logs the drop.
shard exec "${ID}" /bin/sh -c 'ping -6 -c 1 -W 2 ff02::1%eth0 || ping6 -c 1 -W 2 ff02::1%eth0' >/dev/null 2>&1 || true
say "the guest sent an IPv6 packet, which the port must drop"
holds '"policy": "e2e-policy"' shard inspect "${ID}" || fail "inspect does not name the policy"
holds '"egress"' shard inspect "${ID}" || fail "inspect does not print what the host enforces"
say "inspect names the policy and what the host enforces"

# A policy change reaches a live sandbox at once, and never waits for the next start.
shard policy create --deny any e2e-policy >/dev/null
BLOCKED=$(shard exec "${ID}" /bin/sh -c 'ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1 && echo reachable || echo blocked')
expect "${BLOCKED}" "blocked" "a deny-all policy blocks the probe the moment it is stored"
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_network "after the policy was put back"

CODE=0
REFUSAL=$(shard policy remove e2e-policy 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy remove removed a policy a sandbox holds"
grep -q "${ID}" <<<"${REFUSAL}" || fail "policy remove said '${REFUSAL}', want it to name the sandbox"
say "policy remove refused it and named the sandbox"

step "read the egress decision log"
# The daemon tails the host's half out of the kernel ring, so the drop is a moment behind the probe.
EGRESS=""
for _ in $(seq 1 20); do
	EGRESS=$(shard policy logs "${ID}")
	has_line '"source":"host"' '"rule":"ipv6"' <<<"${EGRESS}" && break
	sleep 0.1
done

named_rule() {
	has_line "$1" "$2" '"rule":"[^"]+"' <<<"${EGRESS}" || fail "the egress log holds no $3 with the rule that decided it"
	say "the egress log holds $3 with the rule that decided it"
}

named_rule "\"host\":\"${ECHO_HOST}\"" '"verdict":"allow"' "the proxy's allow"
named_rule "\"host\":\"${DENIED_HOST}\"" '"verdict":"deny"' "the resolver's deny"
named_rule '"source":"host"' '"verdict":"deny"' "the host's drop"
has_line '"source":"host"' '"rule":"local"' <<<"${EGRESS}" || fail "the egress log holds no drop of a packet aimed at the host's own address"
say "the egress log holds the drop of a packet aimed at the host's own address, on rule local"
has_line '"source":"host"' '"rule":"ipv6"' <<<"${EGRESS}" || fail "the egress log holds no drop of an IPv6 packet"
say "the egress log holds the drop of an IPv6 packet, on rule ipv6"

# The ring is shared and short, so a drop only ever read from it is gone within minutes. It is in the
# sandbox's own file, which outlives the daemon that wrote it.
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"

# A drop that lands while the daemon is down is still in the ring when it comes back, so catch-up
# writes it. The kernel is asked for the line directly: no guest can be driven without a daemon.
SANDBOX_ADDRESS=$(grep -o '"address": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${ID}/sandbox.json" | cut -d'"' -f4)
SANDBOX_ADDRESS="${SANDBOX_ADDRESS%%/*}"
for _ in 1 2; do
	echo "<4>shard-egress rule=e2e-catchup SRC=${SANDBOX_ADDRESS} DST=203.0.113.9 PROTO=TCP DPT=25 " >/dev/kmsg
done

start_daemon || fail "the daemon did not come up"
EGRESS=$(shard policy logs "${ID}") || fail "policy logs failed after the restart with status $?"
grep -q '"source":"host"' <<<"${EGRESS}" || fail "the host drop did not outlive the daemon that wrote it: $(printf '%s' "${EGRESS}" | head -c 400)"
say "the host drop is still in the log after a daemon restart"

EGRESS=""
for _ in $(seq 1 20); do
	EGRESS=$(shard policy logs "${ID}") || fail "policy logs failed at catch-up with status $?"
	grep -q '"rule":"e2e-catchup"' <<<"${EGRESS}" && break
	sleep 0.1
done
grep -q '"rule":"e2e-catchup"' <<<"${EGRESS}" || fail "the drop that landed while the daemon was down never reached the log: $(printf '%s' "${EGRESS}" | head -c 400)"
say "a drop that landed while the daemon was down is written at catch-up"

# A follow is a tail of the one file, so both halves of the log reach it live.
FOLLOW_LOG=$(mktemp)
# The binary, so the kill below ends the client and not only a subshell around it.
"${PREFIX}/shard" --root "${SHARD_ROOT}" policy logs -f "${ID}" >"${FOLLOW_LOG}" 2>&1 &
FOLLOW_PID=$!

shard exec "${ID}" /bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ >/dev/null 2>&1" >/dev/null 2>&1 || true
shard exec "${ID}" /bin/sh -c 'ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1' >/dev/null 2>&1 || true

for _ in $(seq 1 20); do
	grep -q '"verdict":"deny"' "${FOLLOW_LOG}" && grep -q '"source":"host"' "${FOLLOW_LOG}" && break
	sleep 0.1
done

kill "${FOLLOW_PID}" 2>/dev/null || true
wait "${FOLLOW_PID}" 2>/dev/null || true

grep -q '"verdict":"deny"' "${FOLLOW_LOG}" || fail "the follow never printed the resolver's deny: $(cat "${FOLLOW_LOG}")"
grep -q '"source":"host"' "${FOLLOW_LOG}" || fail "the follow never printed the host's drop: $(cat "${FOLLOW_LOG}")"
say "policy logs -f prints both halves as they happen"

# The same follow without a WebSocket is one JSON record per line, so curl -N reads it live too.
DENIES_BEFORE=$(shard policy logs "${ID}" | grep -c '"verdict":"deny"' || true)
NDJSON_LOG=$(mktemp)
NDJSON_HEADERS=$(mktemp)
curl -sN -D "${NDJSON_HEADERS}" --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${ID}/egress-log?follow=true" >"${NDJSON_LOG}" 2>&1 &
NDJSON_PID=$!

shard exec "${ID}" /bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ >/dev/null 2>&1" >/dev/null 2>&1 || true

for _ in $(seq 1 30); do
	[ "$(grep -c '"verdict":"deny"' "${NDJSON_LOG}" || true)" -gt "${DENIES_BEFORE}" ] && break
	sleep 0.1
done

kill "${NDJSON_PID}" 2>/dev/null || true
wait "${NDJSON_PID}" 2>/dev/null || true

grep -qi '^content-type: application/x-ndjson' "${NDJSON_HEADERS}" || fail "curl -N on the egress log got '$(cat "${NDJSON_HEADERS}")', want application/x-ndjson"
[ "$(grep -c '"verdict":"deny"' "${NDJSON_LOG}" || true)" -gt "${DENIES_BEFORE}" ] || fail "curl -N never printed the deny that landed after it opened: $(cat "${NDJSON_LOG}")"
grep -q '"source":"host"' "${NDJSON_LOG}" || fail "curl -N never printed the host's drop: $(cat "${NDJSON_LOG}")"
grep -qv '^{' "${NDJSON_LOG}" && fail "curl -N printed a line that is no JSON record: $(cat "${NDJSON_LOG}")"
say "curl -N on egress-log?follow=true prints one JSON record per line as it lands"

step "write a file into the writable layer"
expect_exec "kept" "the file is in the image layer, which a start after a stop must keep" \
	/bin/sh -c 'echo kept > /root/kept; cat /root/kept'

step "prove the cgroup sits under the shard parent"
[ -d "/sys/fs/cgroup/shard/${ID}" ] || fail "there is no cgroup at /sys/fs/cgroup/shard/${ID}"
[ ! -e "/sys/fs/cgroup/${ID}" ] || fail "a cgroup landed at the root, /sys/fs/cgroup/${ID}"
say "the cgroup is /sys/fs/cgroup/shard/${ID} and nothing is at the root"

step "propagate the exit code of a command that failed"
# The || keeps the failure a condition rather than an error, which the exit handler would report.
CODE=0
shard exec "${ID}" /bin/sh -c 'exit 7' >/dev/null 2>&1 || CODE=$?
expect "${CODE}" "7" "a non-zero exit inside the sandbox reached this shell"

step "carry stdin into a command"
GOT=$(printf 'from-stdin\n' | shard exec -i "${ID}" /bin/cat)
expect "${GOT}" "from-stdin" "what this shell piped in came back out of the sandbox"

# These steps reach the create verbs the CLI blocks past: pending, failed, the exec cap, OOM, the policy and a live follow.
# Each is one function that makes its own sandboxes over the socket, asserts, and removes them.

# track_sandbox and untrack_sandbox keep FEATURE_IDS current, so teardown sweeps a sandbox a failed step left.
track_sandbox() { FEATURE_IDS="${FEATURE_IDS} $1"; }
untrack_sandbox() {
	local kept="" held
	for held in ${FEATURE_IDS}; do [ "${held}" = "$1" ] || kept="${kept} ${held}"; done
	FEATURE_IDS="${kept}"
}

# drop_sandbox removes a sandbox a step made and stops tracking it, so the next step starts from a clean host.
drop_sandbox() {
	shard remove --force "$1" >/dev/null 2>&1 || fail "remove did not free the feature sandbox $1"
	untrack_sandbox "$1"
}

# api_create posts a create body and prints the new record; the query is empty for at once, or ?wait=true.
api_create() {
	curl -sS --unix-socket "${SOCKET}" -X POST -H 'Content-Type: application/json' -d "$2" "http://shard/v0/sandboxes$1"
}

# api_call runs one request and sets REPLY_CODE and REPLY_BODY, so a refusal step reads its status and its body.
api_call() {
	local reply
	reply=$(curl -sS -w $'\n%{http_code}' --unix-socket "${SOCKET}" -X "$1" -H 'Content-Type: application/json' -d "$3" "http://shard$2")
	REPLY_CODE="${reply##*$'\n'}"
	REPLY_BODY="${reply%$'\n'*}"
}

# json_field prints the first value of a top-level JSON string field read from stdin.
json_field() { grep -om1 "\"$1\": *\"[^\"]*\"" | cut -d'"' -f4; }

# rec_of names the record file of a sandbox, which the daemon tasks write and a step reads.
rec_of() { printf '%s\n' "${SHARD_ROOT}/sandboxes/$1/sandbox.json"; }

# pending_and_failed_steps drives the async create: a record that is pending before it runs, and a failed one (SHARD-166).
pending_and_failed_steps() {
	local body id

	step "create over the API and see it pending, then running"
	# An uncached image makes the create async, so the record is pending before the pull and the start run.
	body=$(api_create "" "{\"image\":\"alpine:3.19\",\"command\":[\"/bin/sleep\",\"600\"]}")
	id=$(json_field id <<<"${body}")
	[ -n "${id}" ] || fail "the create over the API named no sandbox: ${body}"
	track_sandbox "${id}"
	grep -q '"state": *"pending"' <<<"${body}" || fail "an uncached create did not answer pending: ${body}"
	say "an uncached create answers 201 with a pending record ${id}"
	body=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}?wait=true")
	grep -q '"state": *"running"' <<<"${body}" || fail "the pending sandbox never reached running: ${body}"
	say "a get with wait=true blocks until the pending sandbox is running"
	drop_sandbox "${id}"

	step "wait on a pending create with wait=true"
	# A second uncached tag makes the create block through its own pull, so wait=true answers running, not pending.
	body=$(api_create "?wait=true" "{\"image\":\"alpine:3.18\",\"command\":[\"/bin/sleep\",\"600\"]}")
	id=$(json_field id <<<"${body}")
	[ -n "${id}" ] || fail "the create with wait=true named no sandbox: ${body}"
	track_sandbox "${id}"
	grep -q '"state": *"running"' <<<"${body}" || fail "a create with wait=true did not answer running: ${body}"
	say "a create with wait=true blocks through the pull and answers running ${id}"
	drop_sandbox "${id}"

	step "a bad image lands the sandbox failed with a reason"
	body=$(api_create "" "{\"image\":\"alpine:e2e-no-such-tag\"}")
	id=$(json_field id <<<"${body}")
	[ -n "${id}" ] || fail "the bad-image create named no sandbox: ${body}"
	track_sandbox "${id}"
	body=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}?wait=true")
	grep -q '"state": *"failed"' <<<"${body}" || fail "a bad image did not land the sandbox failed: ${body}"
	grep -q '"failed_reason": *"[^"]' <<<"${body}" || fail "the failed sandbox carries no reason: ${body}"
	say "a bad image lands the sandbox failed with a reason"

	step "refuse every verb on a failed sandbox with 409 sandbox_failed"
	api_call POST "/v0/sandboxes/${id}/start" '{}'
	[ "${REPLY_CODE}" = "409" ] || fail "start on a failed sandbox answered ${REPLY_CODE}, want 409"
	grep -q '"code": *"sandbox_failed"' <<<"${REPLY_BODY}" || fail "start on a failed sandbox gave no sandbox_failed: ${REPLY_BODY}"
	api_call POST "/v0/sandboxes/${id}/exec" '{"command":["/bin/true"]}'
	[ "${REPLY_CODE}" = "409" ] || fail "exec on a failed sandbox answered ${REPLY_CODE}, want 409"
	grep -q '"code": *"sandbox_failed"' <<<"${REPLY_BODY}" || fail "exec on a failed sandbox gave no sandbox_failed: ${REPLY_BODY}"
	# A follow log used to answer 200 with an empty stream instead of the refusal (SHARD-205).
	api_call GET "/v0/sandboxes/${id}/logs?follow=true" ''
	[ "${REPLY_CODE}" = "409" ] || fail "logs?follow=true on a failed sandbox answered ${REPLY_CODE}, want 409"
	grep -q '"code": *"sandbox_failed"' <<<"${REPLY_BODY}" || fail "logs?follow=true on a failed sandbox gave no sandbox_failed: ${REPLY_BODY}"
	say "a failed sandbox refuses start, exec and a follow log with 409 sandbox_failed"

	step "remove a failed sandbox"
	shard remove "${id}" >/dev/null || fail "remove did not remove the failed sandbox ${id}"
	untrack_sandbox "${id}"
	[ ! -e "${SHARD_ROOT}/sandboxes/${id}" ] || fail "the failed sandbox's record survived the remove"
	say "remove removes a failed sandbox and its record is gone"
}

# alone_in proves the guest runs shard-init as PID 1 with nothing after its --, and no other process but the exec that looks.
alone_in() {
	local id="$1" when="$2"
	# Every user process but this exec, by pid and argv0; a kernel thread has no cmdline, so it never counts.
	expect_exec_in "${id}" "1 /.shard/init" "${when}, the guest holds shard-init as PID 1 and nothing else" /bin/sh -c '
		for p in /proc/[0-9]*; do
			p=${p#/proc/}
			if [ "$p" = "$$" ]; then continue; fi
			a=$(tr "\000" "\n" < "/proc/$p/cmdline" 2>/dev/null | head -n 1)
			if [ -n "$a" ]; then echo "$p $a"; fi
		done'
	expect_exec_in "${id}" "--" "${when}, nothing follows the -- of PID 1" /bin/sh -c 'tr "\000" "\n" < /proc/1/cmdline | tail -n 1'
	holds '"exit_status"' shard inspect "${id}" && fail "${when}, sandbox ${id} recorded an exit: $(shard inspect "${id}")"
	say "${when}, inspect holds no exit status, because nothing ran to exit"
}

# no_command_steps proves the image's own ENTRYPOINT and CMD never run: with no command only shard-init does (SHARD-453).
no_command_steps() {
	local id seeded snapshot code refusal policy started took

	step "create with no command runs only shard-init and stays up"
	id=$(shard create "${IMAGE}")
	track_sandbox "${id}"
	[ "$(listed_state "${id}")" = "running" ] || fail "the sandbox with no command is not running: $(shard list --all)"
	say "a create with no command reaches running"
	expect_exec_in "${id}" "alive" "an exec answers in a sandbox with no command" /bin/echo alive
	alone_in "${id}" "after the create"

	step "stop a sandbox with no command and find no exit"
	started=$(date +%s.%N)
	shard stop "${id}" >/dev/null
	took=$(awk -v a="${started}" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }')
	# Nothing runs under shard-init, so the stop never spends the 30 s grace (SHARD-460).
	awk -v t="${took}" 'BEGIN { exit !(t < 10) }' || fail "the stop of a sandbox with no command took ${took} s"
	[ "$(listed_state "${id}")" = "stopped" ] || fail "shard list --all does not list the sandbox with no command stopped"
	# The image's CMD would have exited by now and its status would land at the stop, so none proves it never ran.
	holds '"exit_status"' shard inspect "${id}" && fail "the stop recorded an exit for a sandbox that ran nothing: $(shard inspect "${id}")"
	say "the stop ends shard-init alone and records no exit status"

	step "start and snapshot a sandbox with no command, and still run nothing"
	shard start "${id}" >/dev/null
	[ "$(listed_state "${id}")" = "running" ] || fail "the sandbox with no command is not running after the start"
	alone_in "${id}" "after a start"
	shard stop "${id}" >/dev/null
	snapshot=$(shard snapshot create "${id}" | tail -n 1)
	seeded=$(shard create --snapshot "${snapshot}")
	track_sandbox "${seeded}"
	[ "$(listed_state "${seeded}")" = "running" ] || fail "the sandbox from the snapshot of the sandbox with no command is not running"
	alone_in "${seeded}" "in the sandbox from the snapshot"
	drop_sandbox "${seeded}"
	drop_sandbox "${id}"
	shard snapshot remove "${snapshot}" >/dev/null

	step "create refuses a command, and the API refuses restart.policy with no command"
	code=0
	refusal=$(shard create "${IMAGE}" /bin/true 2>&1) || code=$?
	[ "${code}" != "0" ] || fail "create took a command: ${refusal}"
	grep -q 'create takes no command: shard run' <<<"${refusal}" || fail "the refusal of a command does not point at run: ${refusal}"
	api_call POST "/v0/sandboxes" "{\"image\":\"${IMAGE}\",\"restart\":{\"policy\":\"always\"}}"
	[ "${REPLY_CODE}" = "400" ] || fail "a restart policy with no command answered ${REPLY_CODE}, want 400"
	grep -q 'restart.policy needs a command' <<<"${REPLY_BODY}" || fail "the refusal does not name restart.policy: ${REPLY_BODY}"
	say "create refuses a command, naming run, and the API refuses restart.policy with no command, 400"
}

# run_steps prove SHARD-454: run waits on its app and exits with its last code, -d returns once the sandbox is up, and Ctrl+C stops the app while the sandbox runs on.
run_steps() {
	local id out err code count pid flag before

	step "run prints the app output once and exits with its code"
	code=0
	out=$(shard run --name e2e-run-exit "${IMAGE}" /bin/sh -c 'echo e2e-run-out; exit 3' 2>/dev/null) || code=$?
	id=$(id_of e2e-run-exit)
	track_sandbox "${id}"
	expect "${code}" "3" "run exits with the app's code"
	expect "${out}" "e2e-run-out" "run prints the app output once"
	[ "$(listed_state "${id}")" = "running" ] || fail "the sandbox did not outlive the app of the run: $(shard list --all)"
	drop_sandbox "${id}"

	step "run under on-failure exits with the last code once the policy gives up"
	code=0
	out=$(shard run --name e2e-run-retries --restart on-failure --restart-retries 2 "${IMAGE}" /bin/sh -c 'echo e2e-run-try; exit 4' 2>/dev/null) || code=$?
	id=$(id_of e2e-run-retries)
	track_sandbox "${id}"
	expect "${code}" "4" "run exits with the code of the last run"
	count=$(grep -c e2e-run-try <<<"${out}" || true)
	expect "${count}" "3" "the app ran three times: the first run and its 2 retries"
	holds '"count": *2' shard inspect "${id}" || fail "inspect holds no restart count of 2 when run returns: $(shard inspect "${id}")"
	holds '"ended": *true' shard inspect "${id}" || fail "inspect does not hold the end of the policy when run returns: $(shard inspect "${id}")"
	say "inspect holds count 2 and ended true as soon as run returns"
	drop_sandbox "${id}"

	step "run -d returns the id once the sandbox is up"
	id=$(shard run -d "${IMAGE}" /bin/sleep 600)
	track_sandbox "${id}"
	[ "$(listed_state "${id}")" = "running" ] || fail "run -d returned before the sandbox was running: $(shard list --all)"
	say "run -d printed ${id}, and the sandbox is running"
	drop_sandbox "${id}"

	step "run refuses no command, and create and exec refuse the restart flags"
	code=0
	err=$(shard run "${IMAGE}" 2>&1) || code=$?
	expect "${code}" "125" "run with no command exits 125"
	grep -q 'run needs a command after the image' <<<"${err}" || fail "the refusal does not say run needs a command: ${err}"
	id=$(shard create "${IMAGE}")
	track_sandbox "${id}"
	for flag in "--restart always" "--restart-retries 2" "--restart-backoff 2s"; do
		code=0
		# shellcheck disable=SC2086 # the flag and its value are meant to split
		err=$(shard create ${flag} "${IMAGE}" 2>&1) || code=$?
		[ "${code}" != "0" ] || fail "create took ${flag}: ${err}"
		grep -q -- "${flag% *} is a run flag" <<<"${err}" || fail "create's refusal of ${flag} does not name it: ${err}"
		code=0
		# shellcheck disable=SC2086 # the flag and its value are meant to split
		err=$(shard exec ${flag} "${id}" /bin/true 2>&1) || code=$?
		[ "${code}" != "0" ] || fail "exec took ${flag}: ${err}"
		grep -q -- "${flag% *} is a run flag" <<<"${err}" || fail "exec's refusal of ${flag} does not name it: ${err}"
	done
	say "create and exec refuse each restart flag by name"
	drop_sandbox "${id}"

	step "Ctrl+C on run --restart always stops the app and leaves the sandbox running"
	out=$(mktemp)
	err=$(mktemp)
	# The binary, not the shard function: $! of a backgrounded function is a subshell, which ignores SIGINT.
	"${PREFIX}/shard" --root "${SHARD_ROOT}" run --name e2e-run-int --restart always "${IMAGE}" /bin/sh -c 'while true; do echo e2e-tick; sleep 1; done' >"${out}" 2>"${err}" &
	pid=$!
	for _ in $(seq 1 100); do
		grep -q e2e-tick "${out}" && break
		sleep 0.2
	done
	grep -q e2e-tick "${out}" || fail "the attached run printed no tick: $(cat "${err}")"
	id=$(id_of e2e-run-int)
	track_sandbox "${id}"
	# Go un-ignores SIGINT when it registers for it, so a background job of this shell still takes the signal.
	kill -INT "${pid}"
	for _ in $(seq 1 150); do
		kill -0 "${pid}" 2>/dev/null || break
		sleep 0.2
	done
	if kill -0 "${pid}" 2>/dev/null; then
		kill -KILL "${pid}" 2>/dev/null || true
		fail "run did not exit within 30 s of one Ctrl+C: $(cat "${err}")"
	fi
	code=0
	wait "${pid}" || code=$?
	expect "${code}" "143" "run exits with the app's TERM death after one Ctrl+C"
	grep -q 'shard: stopping the app' "${err}" || fail "run did not say it stops the app: $(cat "${err}")"
	before=$(shard logs "${id}" | grep -c e2e-tick || true)
	sleep 3
	expect "$(shard logs "${id}" | grep -c e2e-tick || true)" "${before}" "the ticks stopped: no restart followed the stop"
	[ "$(listed_state "${id}")" = "running" ] || fail "the Ctrl+C stopped the sandbox, want it running: $(shard list --all)"
	expect_exec_in "${id}" "1 /.shard/init" "after the Ctrl+C, the guest holds shard-init alone" /bin/sh -c '
		for p in /proc/[0-9]*; do
			p=${p#/proc/}
			if [ "$p" = "$$" ]; then continue; fi
			a=$(tr "\000" "\n" < "/proc/$p/cmdline" 2>/dev/null | head -n 1)
			if [ -n "$a" ]; then echo "$p $a"; fi
		done'
	rm -f "${out}" "${err}"
	drop_sandbox "${id}"
}

# exec_cap_steps proves the sandbox keeps at most 32 exited execs and never evicts a running one (SHARD-163).
exec_cap_steps() {
	local body id long first exec_id n code

	body=$(api_create "?wait=true" "{\"image\":\"${IMAGE}\",\"command\":[\"/bin/sleep\",\"600\"]}")
	id=$(json_field id <<<"${body}")
	[ -n "${id}" ] || fail "the exec-cap sandbox was not created: ${body}"
	track_sandbox "${id}"

	step "cap the retained execs at 32 and evict the oldest"
	# A long exec is created first, so the cap that evicts the exited ones must keep this running one.
	body=$(curl -sS --unix-socket "${SOCKET}" -X POST -H 'Content-Type: application/json' -d '{"command":["/bin/sleep","600"]}' "http://shard/v0/sandboxes/${id}/exec")
	long=$(json_field exec <<<"${body}")
	[ -n "${long}" ] || fail "the long exec was not created: ${body}"
	first=""
	for _ in $(seq 1 33); do
		body=$(curl -sS --unix-socket "${SOCKET}" -X POST -H 'Content-Type: application/json' -d '{"command":["/bin/true"]}' "http://shard/v0/sandboxes/${id}/exec")
		exec_id=$(json_field exec <<<"${body}")
		[ -n "${exec_id}" ] || fail "one of the short execs was not created: ${body}"
		[ -n "${first}" ] || first="${exec_id}"
		curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/exec/${exec_id}?wait=true" >/dev/null
	done
	# The cap runs when the last exec exits, so the count settles at 33 a moment after the wait returns.
	n=0
	for _ in $(seq 1 50); do
		n=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/exec" | grep -o '"exec": *"[^"]*"' | wc -l | tr -d ' ')
		[ "${n}" = "33" ] && break
		sleep 0.1
	done
	# 32 exited plus the one still running is 33, and the oldest exited is the one the cap evicted.
	expect "${n}" "33" "the sandbox retains 32 exited execs and the one still running"
	code=$(curl -sS -o /dev/null -w '%{http_code}' --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/exec/${first}")
	expect "${code}" "404" "the oldest exited exec was evicted and answers 404"
	say "the exec cap keeps 32 exited execs and evicts the oldest"

	step "a running exec survives the cap"
	code=$(curl -sS -o /dev/null -w '%{http_code}' --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/exec/${long}")
	expect "${code}" "200" "the running exec is still held after the cap evicted an exited one"
	body=$(curl -sS --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/exec/${long}")
	grep -q '"state": *"running"' <<<"${body}" || fail "the surviving exec is not running: ${body}"
	say "a running exec is never evicted by the cap"
	drop_sandbox "${id}"
}

# restart_policy_steps drives the supervisor policy: on-failure, always, a clean exit, a bare outlive, and refusals (SHARD-55).
restart_policy_steps() {
	local id rec

	step "on-failure restarts on a nonzero exit"
	id=$(api_create "?wait=true" "{\"image\":\"${IMAGE}\",\"command\":[\"/bin/sh\",\"-c\",\"exit 1\"],\"restart\":{\"policy\":\"on-failure\",\"retries\":2}}" | json_field id)
	[ -n "${id}" ] || fail "the on-failure sandbox was not created"
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	for _ in $(seq 1 100); do
		grep -q '"count": *2' "${rec}" && grep -q '"gave_up": *true' "${rec}" && break
		sleep 0.2
	done
	grep -q '"count": *2' "${rec}" || fail "on-failure did not restart the entrypoint to its 2 retries: $(cat "${rec}")"
	grep -q '"gave_up": *true' "${rec}" || fail "on-failure did not give up after its retries: $(cat "${rec}")"
	say "on-failure restarts the entrypoint on a nonzero exit and gives up at the retries"
	drop_sandbox "${id}"

	step "always restarts on a clean exit"
	id=$(api_create "?wait=true" "{\"image\":\"${IMAGE}\",\"command\":[\"/bin/sh\",\"-c\",\"exit 0\"],\"restart\":{\"policy\":\"always\"}}" | json_field id)
	[ -n "${id}" ] || fail "the always sandbox was not created"
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	for _ in $(seq 1 100); do
		grep -q '"count": *[2-9]' "${rec}" && break
		sleep 0.2
	done
	grep -q '"count": *[2-9]' "${rec}" || fail "always did not restart the entrypoint on a clean exit: $(cat "${rec}")"
	grep -q '"gave_up": *true' "${rec}" && fail "always gave up, but it never does: $(cat "${rec}")"
	say "always restarts the entrypoint on a clean exit and never gives up"
	drop_sandbox "${id}"

	step "on-failure ignores a clean exit"
	id=$(api_create "?wait=true" "{\"image\":\"${IMAGE}\",\"command\":[\"/bin/sh\",\"-c\",\"exit 0\"],\"restart\":{\"policy\":\"on-failure\",\"retries\":2}}" | json_field id)
	[ -n "${id}" ] || fail "the clean-exit sandbox was not created"
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	sleep 3
	grep -q '"count": *0' "${rec}" || fail "on-failure restarted a clean exit: $(cat "${rec}")"
	grep -q '"state": *"running"' "${rec}" || fail "the sandbox did not outlive its clean entrypoint: $(cat "${rec}")"
	expect_exec_in "${id}" "alive" "an exec answers after the clean exit" /bin/echo alive
	say "on-failure ignores a clean exit and the sandbox outlives its entrypoint"
	drop_sandbox "${id}"

	step "a sandbox outlives its entrypoint"
	id=$(api_create "?wait=true" "{\"image\":\"${IMAGE}\",\"command\":[\"/bin/sh\",\"-c\",\"echo done\"]}" | json_field id)
	[ -n "${id}" ] || fail "the short-entrypoint sandbox was not created"
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	for _ in $(seq 1 100); do
		grep -q '"exit_status"' "${rec}" && break
		sleep 0.2
	done
	grep -q '"state": *"running"' "${rec}" || fail "the sandbox did not stay running after its entrypoint exited: $(cat "${rec}")"
	grep -q '"exit_status"' "${rec}" || fail "the record noted no exit for the entrypoint that ended: $(cat "${rec}")"
	expect_exec_in "${id}" "alive" "an exec answers after the entrypoint exited" /bin/echo alive
	say "a sandbox outlives its entrypoint and still runs an exec"
	drop_sandbox "${id}"

	step "refuse an unknown policy or retries with policy no"
	api_call POST "/v0/sandboxes" "{\"image\":\"${IMAGE}\",\"restart\":{\"policy\":\"sometimes\"}}"
	[ "${REPLY_CODE}" = "400" ] || fail "an unknown restart policy answered ${REPLY_CODE}, want 400"
	grep -q 'restart.policy is no, on-failure or always' <<<"${REPLY_BODY}" || fail "the refusal does not name the policies: ${REPLY_BODY}"
	api_call POST "/v0/sandboxes" "{\"image\":\"${IMAGE}\",\"restart\":{\"policy\":\"no\",\"retries\":2}}"
	[ "${REPLY_CODE}" = "400" ] || fail "retries under policy no answered ${REPLY_CODE}, want 400"
	grep -q 'need a policy that starts again' <<<"${REPLY_BODY}" || fail "the refusal does not name the missing policy: ${REPLY_BODY}"
	say "the API refuses an unknown policy and retries under policy no, 400 each"
}

# http_follow_steps proves a live log follow over plain HTTP, and that a dropped follow client is no failure (SHARD-164).
http_follow_steps() {
	local id body follow_log follow_headers second_log follow_pid second_pid base target

	step "follow the entrypoint logs live over HTTP"
	# A looping entrypoint prints a new line several times a second, so a later line proves a live stream, not a replay.
	body='{"image":"IMAGEREF","command":["/bin/sh","-c","i=0; while true; do echo tick-$i; i=$((i+1)); sleep 0.3; done"]}'
	id=$(api_create "?wait=true" "${body/IMAGEREF/${IMAGE}}" | json_field id)
	[ -n "${id}" ] || fail "the log-follow sandbox was not created"
	track_sandbox "${id}"
	follow_log=$(mktemp)
	follow_headers=$(mktemp)
	curl -sN -D "${follow_headers}" --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/logs?follow=true" >"${follow_log}" 2>&1 &
	follow_pid=$!
	# Read a tick that is already out, then wait for one five ticks later, which only a live stream delivers.
	base=""
	for _ in $(seq 1 50); do
		base=$(grep -om1 'tick-[0-9]*' "${follow_log}" 2>/dev/null | cut -d- -f2 || true)
		[ -n "${base}" ] && break
		sleep 0.1
	done
	[ -n "${base}" ] || fail "the log follow printed no tick while the sandbox ran: $(cat "${follow_log}")"
	target=$((base + 5))
	for _ in $(seq 1 50); do
		grep -q "tick-${target}" "${follow_log}" && break
		sleep 0.1
	done
	kill "${follow_pid}" 2>/dev/null || true
	wait "${follow_pid}" 2>/dev/null || true
	grep -q "tick-${target}" "${follow_log}" || fail "the log follow did not stream past tick-${base}: $(cat "${follow_log}")"
	grep -qi '^content-type: text/plain' "${follow_headers}" || fail "the log follow got '$(cat "${follow_headers}")', want text/plain"
	say "logs?follow=true streams the entrypoint output live over HTTP"
	drop_sandbox "${id}"

	step "a dropped follow client is not a failure"
	# A client that hangs up mid-follow ends its own read and nothing else, so the daemon logs no failure for it.
	body='{"image":"IMAGEREF","command":["/bin/sh","-c","echo dropped-client-alive; exec /bin/sleep 600"]}'
	id=$(api_create "?wait=true" "${body/IMAGEREF/${IMAGE}}" | json_field id)
	[ -n "${id}" ] || fail "the dropped-client sandbox was not created"
	track_sandbox "${id}"
	follow_log=$(mktemp)
	curl -sN --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/logs?follow=true" >"${follow_log}" 2>&1 &
	follow_pid=$!
	for _ in $(seq 1 50); do
		grep -q 'dropped-client-alive' "${follow_log}" && break
		sleep 0.1
	done
	grep -q 'dropped-client-alive' "${follow_log}" || fail "the follow printed nothing before the client dropped: $(cat "${follow_log}")"
	kill -9 "${follow_pid}" 2>/dev/null || true
	wait "${follow_pid}" 2>/dev/null || true
	# Give the daemon a tick to notice the hangup, then prove it logged no failure for this follow.
	sleep 1
	grep -q "logs of sandbox ${id}" "${DAEMON_LOG}" && fail "the daemon logged a failure for a client that only hung up: $(grep "logs of sandbox ${id}" "${DAEMON_LOG}")"
	# A second follow still reads the sandbox, so the dropped client left it whole.
	second_log=$(mktemp)
	curl -sN --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${id}/logs?follow=true" >"${second_log}" 2>&1 &
	second_pid=$!
	for _ in $(seq 1 50); do
		grep -q 'dropped-client-alive' "${second_log}" && break
		sleep 0.1
	done
	kill "${second_pid}" 2>/dev/null || true
	wait "${second_pid}" 2>/dev/null || true
	grep -q 'dropped-client-alive' "${second_log}" || fail "a second follow after the drop read nothing: $(cat "${second_log}")"
	say "a dropped follow client is not a failure and the sandbox stays whole"
	drop_sandbox "${id}"
}

# OOM_BOMB overruns a 64 MiB bound in 32 tasks; memory.high throttles each task to ~128 KiB/s.
OOM_BOMB='i=0; while [ $i -lt 32 ]; do awk '\''BEGIN { s = "x"; while (1) s = s s }'\'' & i=$((i+1)); done; wait'
# OOM_POLLS bounds the wait for one kill at one 5 s tick, like the integration test's budget.
OOM_POLLS="${OOM_POLLS:-360}"

# oom_stop_steps prove SHARD-461: an OOM stops the sandbox with its reason, nothing starts it again, and start brings it back. It runs on every provider (SHARD-191).
oom_stop_steps() {
	local id rec

	step "an OOM stops the sandbox with its reason, and nothing starts it again"
	# The bomb overruns the bound on the first run only, so the run a start brings back sleeps and can be used.
	id=$(shard run -d --memory 64MiB "${IMAGE}" /bin/sh -c "if [ ! -e /ran ]; then touch /ran; ${OOM_BOMB}; fi; while true; do sleep 1; done")
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	for _ in $(seq 1 "${OOM_POLLS}"); do
		grep -q '"state": *"stopped"' "${rec}" && break
		sleep 1
	done
	grep -q '"state": *"stopped"' "${rec}" || fail "the OOM sandbox never stopped on ${PROVIDER}: $(cat "${rec}")"
	grep -q '"stopped_reason": *"ran out of memory and the host ended it"' "${rec}" || fail "the stop names no OOM on ${PROVIDER}: $(cat "${rec}")"
	grep -q '"pid": *0' "${rec}" || fail "the stopped OOM sandbox still names a pid: $(cat "${rec}")"
	grep -q "sandbox ${id}: ran out of memory and the host ended it, the record now says stopped" "${DAEMON_LOG}" || fail "the daemon never reported the OOM stop of ${id}"
	# Two liveness ticks pass, and the record still says stopped.
	sleep 11
	grep -q '"state": *"stopped"' "${rec}" || fail "something started the OOM sandbox again on ${PROVIDER}: $(cat "${rec}")"
	say "on ${PROVIDER} an OOM stops the sandbox with its reason, and nothing starts it again"

	step "start brings the OOM sandbox back over its kept files"
	shard start "${id}" >/dev/null
	expect_exec_in "${id}" "/ran" "the sandbox a start brought back kept the file of its first run" /bin/ls /ran
	drop_sandbox "${id}"
}

# disk_bound_steps prove SHARD-173: a guest write past --disk fails with ENOSPC and the host holds no more than the bound.
disk_bound_steps() {
	local id seeded snapshot rec image_mib state_mib disk_mount
	# Each fill asks for three times the bound. A full disk takes no status file, so the count goes through a pipe and the fill is freed after.
	local fill_tmp='dd if=/dev/zero of=/tmp/fill bs=1M count=192 2>&1 | grep -c "No space left on device"; rm -f /tmp/fill'
	local fill_root='dd if=/dev/zero of=/fill bs=1M count=192 2>&1 | grep -c "No space left on device"; rm -f /fill'

	step "refuse a negative disk bound"
	api_call POST "/v0/sandboxes" "{\"image\":\"${IMAGE}\",\"resources\":{\"disk_mib\":-1}}"
	[ "${REPLY_CODE}" = "400" ] || fail "a negative disk bound answered ${REPLY_CODE}, want 400"
	grep -q 'the disk bound is in MiB and cannot be negative' <<<"${REPLY_BODY}" || fail "the refusal does not name the bound: ${REPLY_BODY}"
	say "the API refuses a negative disk bound, 400"

	step "a write past the disk bound fails in the guest and stops on the host"
	id=$(shard run -d --disk 64MiB "${IMAGE}" /bin/sleep 600)
	track_sandbox "${id}"
	rec=$(rec_of "${id}")
	grep -q '"disk_mib": *64' "${rec}" || fail "the record does not carry the disk bound: $(cat "${rec}")"
	say "the record carries disk_mib 64"
	expect_exec_in "${id}" "1" "a fill of /tmp past the bound fails with ENOSPC" /bin/sh -c "${fill_tmp}"
	expect_exec_in "${id}" "1" "a fill of the root past the bound fails with ENOSPC" /bin/sh -c "${fill_root}"
	expect_exec_in "${id}" "alive" "the sandbox lives on after ENOSPC" /bin/echo alive
	# -x keeps du off the loop and overlay mounts, so this is what the host directory itself holds.
	image_mib=$(du -m "${SHARD_ROOT}/sandboxes/${id}/disk.img" | cut -f1)
	state_mib=$(du -sxm "${SHARD_ROOT}/sandboxes/${id}" | cut -f1)
	[ "${image_mib}" -le 65 ] || fail "the disk image holds ${image_mib} MiB on the host, want at most the bound"
	[ "${state_mib}" -le 70 ] || fail "the state directory holds ${state_mib} MiB on the host, want at most the bound"
	say "the host holds ${image_mib} MiB of image and ${state_mib} MiB of state, under the bound"

	step "the disk survives a stop and a start"
	shard exec "${id}" /bin/sh -c 'echo before-the-stop > /root/marker' >/dev/null
	shard stop "${id}" >/dev/null
	# sysbox-runc holds a stopped sandbox, and sysbox-mgr chowns its upper layer back at delete, so the disk stays up until then.
	disk_mount=$(mount | grep " on ${SHARD_ROOT}/sandboxes/${id}/disk " || true)
	case "${PROVIDER}" in
	gvisor | runc) absent "the disk mount of the stopped sandbox" "${disk_mount}" ;;
	sysbox) [ -n "${disk_mount}" ] || fail "the disk of the stopped sandbox is not mounted, and sysbox-mgr walks its upper layer at the next start" ;;
	esac
	shard start "${id}" >/dev/null
	expect_exec_in "${id}" "before-the-stop" "the marker survives the stop and start" /bin/cat /root/marker

	step "a sandbox from a snapshot is bounded the way its source was"
	shard stop "${id}" >/dev/null
	snapshot=$(shard snapshot create "${id}" | tail -n 1)
	seeded=$(shard create --name e2e-disk-seeded --snapshot "${snapshot}")
	track_sandbox "${seeded}"
	grep -q '"disk_mib": *64' "$(rec_of "${seeded}")" || fail "the seeded record does not carry the disk bound: $(cat "$(rec_of "${seeded}")")"
	expect_exec_in "${seeded}" "before-the-stop" "the seeded sandbox holds the source's layer" /bin/cat /root/marker
	expect_exec_in "${seeded}" "1" "a fill past the bound fails in the seeded sandbox too" /bin/sh -c "${fill_root}"
	say "the sandbox from the snapshot carries disk_mib 64 and its own disk bounds it"
	drop_sandbox "${seeded}"
	drop_sandbox "${id}"
	shard snapshot remove "${snapshot}" >/dev/null
}

# stop_grace_steps prove SHARD-460: the grace is fixed at 30 s, a stop ends with an entrypoint that exits on SIGTERM, and kills one that ignores it at 30 s.
stop_grace_steps() {
	local id started took

	step "a stop ends as soon as the entrypoint exits on SIGTERM"
	id=$(shard run -d "${IMAGE}" /bin/sleep 600)
	track_sandbox "${id}"
	started=$(date +%s.%N)
	shard stop "${id}" >/dev/null
	took=$(awk -v a="${started}" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }')
	awk -v t="${took}" 'BEGIN { exit !(t < 10) }' || fail "the stop took ${took} s, and the entrypoint exits on SIGTERM"
	grep -q '"signal": *15' "$(rec_of "${id}")" || fail "the record holds no SIGTERM exit: $(cat "$(rec_of "${id}")")"
	say "on ${PROVIDER} the stop took ${took} s of the 30 s grace, and the record holds the SIGTERM exit"

	step "remove --force stops a running sandbox, then deletes it"
	shard start "${id}" >/dev/null
	shard remove --force "${id}" >/dev/null
	untrack_sandbox "${id}"
	absent "the record of the sandbox remove --force stopped" "$([ -e "${SHARD_ROOT}/sandboxes/${id}" ] && echo "${SHARD_ROOT}/sandboxes/${id}" || true)"

	# The kill costs the full 30 s, so gvisor proves it end to end and the provider unit tests cover the rest.
	[ "${PROVIDER}" = "gvisor" ] || return 0
	step "a stop kills an entrypoint that ignores SIGTERM at 30 s"
	id=$(shard run -d "${IMAGE}" /bin/sh -c "trap '' TERM; echo e2e-ignores-term; while true; do sleep 1; done")
	track_sandbox "${id}"
	for _ in $(seq 1 50); do
		holds "e2e-ignores-term" shard logs "${id}" && break
		sleep 0.2
	done
	holds "e2e-ignores-term" shard logs "${id}" || fail "the entrypoint never said it ignores SIGTERM"
	started=$(date +%s.%N)
	shard stop "${id}" >/dev/null
	took=$(awk -v a="${started}" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }')
	# The 15 s past the grace covers the kill and the settle after it.
	awk -v t="${took}" 'BEGIN { exit !(t >= 30 && t < 45) }' || fail "the stop took ${took} s, want the 30 s grace and then the kill"
	expect "$(listed_state "${id}")" "stopped" "on ${PROVIDER} the stop took ${took} s: the 30 s grace, then the kill"
	drop_sandbox "${id}"
}

pending_and_failed_steps
no_command_steps
run_steps
exec_cap_steps
restart_policy_steps
http_follow_steps
oom_stop_steps
disk_bound_steps
stop_grace_steps

# checkpoint_refusals prove a provider without checkpoints refuses each verb by name and leaves the sandbox running.
checkpoint_refusals() {
	local verb refusal code
	for verb in pause resume; do
		step "refuse to ${verb} on ${PROVIDER}"
		code=0
		refusal=$(shard "${verb}" "${ID}" 2>&1) || code=$?
		[ "${code}" != "0" ] || fail "shard ${verb} exited 0 on ${PROVIDER}, which holds no checkpoints"
		expect "${refusal}" "shard: provider ${PROVIDER} does not support ${verb} on this host" "${verb} names the provider and the verb"
		[ "$(listed_state "${ID}")" = "running" ] || fail "the refused ${verb} left the sandbox $(listed_state "${ID}")"
	done

	step "refuse to fork on ${PROVIDER}"
	code=0
	refusal=$(shard fork --name e2e-fork "${ID}" 2>&1) || code=$?
	[ "${code}" != "0" ] || fail "shard fork exited 0 on ${PROVIDER}, which holds no checkpoints"
	expect "${refusal}" "shard: provider ${PROVIDER} does not support fork on this host" "fork names the provider and the verb"
	absent "a sandbox named e2e-fork" "$(shard list --all | grep e2e-fork || true)"
	expect_exec "still-running" "the source runs on after the refusals" /bin/echo still-running
}

# docker_steps run dockerd in a second sandbox and a docker build inside it: the workload Sysbox is
# here for. The image's own entrypoint sets up cgroups and iptables before dockerd, so it is bypassed
# on purpose: the sandbox already holds a cgroup of its own, and the daemon is what is under test.
docker_steps() {
	step "run dockerd inside a sandbox on ${PROVIDER}"
	DIND_ID=$(shard run -d --name e2e-dind "${DIND_IMAGE}" /usr/local/bin/dockerd)
	[ -n "${DIND_ID}" ] || fail "run -d printed no id for the dockerd sandbox"
	DIND_LINK=$(grep -o '"host_interface": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${DIND_ID}/sandbox.json" | cut -d'"' -f4)
	say "the dockerd sandbox is ${DIND_ID} on the link ${DIND_LINK}"

	# dockerd takes a few seconds to open its socket; the log names the failure when it never does.
	local ready=0
	for _ in $(seq 1 150); do
		shard exec "${DIND_ID}" docker info >/dev/null 2>&1 && ready=1 && break
		sleep 0.2
	done
	[ "${ready}" = "1" ] || fail "docker info never answered inside ${DIND_ID}: $(shard logs "${DIND_ID}" | tail -n 20)"
	say "docker info answers inside the sandbox"

	step "docker build and run an image inside the sandbox"
	expect_exec_in "${DIND_ID}" "built-inside" "an image built by the nested dockerd runs and reads its own layer" \
		/bin/sh -c 'mkdir -p /tmp/e2e && printf "FROM alpine:3.20\nRUN echo built-inside > /built\n" > /tmp/e2e/Dockerfile && docker build -q -t e2e-nested /tmp/e2e >/dev/null && docker run --rm e2e-nested cat /built'
	# The nested image lives in the sandbox's layer, so the host's image store never sees it.
	absent "the nested image in the host store" "$(shard image list | grep e2e-nested || true)"

	step "stop and remove the dockerd sandbox"
	shard stop "${DIND_ID}" >/dev/null
	[ "$(listed_state "${DIND_ID}")" = "stopped" ] || fail "shard list --all does not list the dockerd sandbox stopped"
	shard remove "${DIND_ID}" >/dev/null
	absent "the dockerd sandbox's record" "$([ -e "${SHARD_ROOT}/sandboxes/${DIND_ID}" ] && echo "${SHARD_ROOT}/sandboxes/${DIND_ID}" || true)"
	absent "the dockerd sandbox's link" "$(ip link show "${DIND_LINK}" 2>/dev/null || true)"
	absent "the dockerd sandbox's cgroup" "$([ -e "/sys/fs/cgroup/shard/${DIND_ID}" ] && echo "/sys/fs/cgroup/shard/${DIND_ID}" || true)"
	DIND_ID=""
	DIND_LINK=""
	expect_exec "still-running" "the first sandbox runs on beside the docker steps" /bin/echo still-running
}

# The docker step is Sysbox's alone: bare runc holds no dockerd.
case "${PROVIDER}" in
gvisor) checkpoint_steps ;;
sysbox) checkpoint_refusals; docker_steps ;;
runc) checkpoint_refusals ;;
esac

step "refuse to remove a sandbox that is still up"
CODE=0
REFUSAL=$(shard remove "${ID}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "remove removed a running sandbox"
grep -q "shard stop ${ID}" <<<"${REFUSAL}" || fail "remove said '${REFUSAL}', want it to say to stop it first"
say "remove refused it and named the stop"

# curl -N holds the log open through the stop, and the body must end on its own once the sandbox is stopped.
PLAIN_LOG=$(mktemp)
PLAIN_HEADERS=$(mktemp)
curl -sN -D "${PLAIN_HEADERS}" --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${ID}/logs?follow=true" >"${PLAIN_LOG}" 2>&1 &
PLAIN_PID=$!
for _ in $(seq 1 50); do
	grep -q "shard-e2e-entrypoint" "${PLAIN_LOG}" && break
	sleep 0.1
done
grep -q "shard-e2e-entrypoint" "${PLAIN_LOG}" || fail "curl -N on logs?follow=true printed nothing while the sandbox ran"

step "stop the sandbox"
shard stop "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
say "the record says stopped"

for _ in $(seq 1 100); do
	kill -0 "${PLAIN_PID}" 2>/dev/null || break
	sleep 0.1
done
kill -0 "${PLAIN_PID}" 2>/dev/null && fail "curl -N on logs?follow=true is still open 10 s after the stop"
wait "${PLAIN_PID}" || fail "curl -N on logs?follow=true ended with a failure on the stop"
grep -qi '^content-type: text/plain' "${PLAIN_HEADERS}" || fail "curl -N on the logs got '$(cat "${PLAIN_HEADERS}")', want text/plain"
say "curl -N on logs?follow=true streams text/plain and ends on the stop"

# This is the boundary the ticket names: a stop keeps everything a later start needs.
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the stop dropped the address"
ip netns list | has_line "^${ID}" || fail "the stop dropped the namespace"
ip link show "${LINK}" >/dev/null || fail "the stop dropped the link"
# The lease is a file named by the address, and it holds the id of the sandbox that took it.
LEASE="${SHARD_ROOT}/network/leases/${ADDRESS%%/*}"
grep -qx "${ID}" "${LEASE}" || fail "the stop dropped the address lease"
say "the record, the address, the lease, the namespace and the link all survived the stop"

holds "^${ID}" shard list && fail "shard list still lists the stopped sandbox"
[ "$(listed_state "${ID}")" = "stopped" ] || fail "shard list --all does not list the sandbox as stopped"
say "list hides the stopped sandbox and list --all shows it stopped"

# -f ends on its own once the sandbox is stopped, so a hang here is a failure, not a wait.
holds "shard-e2e-entrypoint" timeout 10 "${PREFIX}/shard" --root "${SHARD_ROOT}" logs -f "${ID}" || fail "shard logs -f on a stopped sandbox did not print its output and end"
say "logs still reads a stopped sandbox, and -f ends on its own"

# The egress log outlives the stop, so a plain follow of a stopped sandbox prints it and ends by itself.
STOPPED_NDJSON=$(mktemp)
timeout 10 curl -sN --unix-socket "${SOCKET}" "http://shard/v0/sandboxes/${ID}/egress-log?follow=true" >"${STOPPED_NDJSON}" || fail "curl -N on egress-log?follow=true of a stopped sandbox did not end on its own"
grep -q '"verdict":"deny"' "${STOPPED_NDJSON}" || fail "curl -N on the egress log of a stopped sandbox printed no record: $(cat "${STOPPED_NDJSON}")"
say "curl -N on egress-log?follow=true of a stopped sandbox prints the log and ends on its own"

step "stop the sandbox a second time"
shard stop "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the second stop changed the state"
say "a second stop is idempotent"

step "inspect the stopped sandbox"
holds '"state": "stopped"' shard inspect "${ID}" || fail "shard inspect does not say stopped"
holds '"exit_status"' shard inspect "${ID}" || fail "shard inspect holds no exit status after the stop"
say "inspect prints the record with its state and its exit status"
CODE=0
REFUSAL=$(shard inspect no-such-sandbox 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "inspect answered for a sandbox nothing holds"
expect "${REFUSAL}" "shard: no sandbox no-such-sandbox" "inspect of a name nothing holds is one line"

step "snapshot the stopped sandbox"
# A snapshot copies the files a stop kept, never the memory, and outlives its source.
timed "snapshot create" snapshot_it
[ -n "${SNAPSHOT_ID}" ] || fail "snapshot create printed no id"
SNAPSHOT_DIR="${SHARD_ROOT}/snapshots/${SNAPSHOT_ID}"
[ -f "${SNAPSHOT_DIR}/snapshot.json" ] || fail "there is no snapshot record at ${SNAPSHOT_DIR}/snapshot.json"
holds "e2e-snapshot" shard snapshot list || fail "snapshot list does not list e2e-snapshot"
holds "\"source\": \"${ID}\"" shard snapshot inspect e2e-snapshot || fail "snapshot inspect does not name the source: $(shard snapshot inspect e2e-snapshot)"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the snapshot changed the source's state"
say "snapshot create printed ${SNAPSHOT_ID}, and list and inspect find it by name"

step "create two sandboxes from the snapshot"
timed "create --snapshot" seed_it e2e-seeded-1
timed "create --snapshot" seed_it e2e-seeded-2
# shellcheck disable=SC2086 # the seeded list is meant to split
set -- ${SEEDED_IDS}
[ "$#" = "2" ] && [ "$1" != "$2" ] && [ "$1" != "${ID}" ] && [ "$2" != "${ID}" ] || fail "create --snapshot printed '${SEEDED_IDS}', want two new ids"
N=0
for SEEDED_ID in "$@"; do
	N=$((N + 1))
	SEEDED_RECORD="${SHARD_ROOT}/sandboxes/${SEEDED_ID}/sandbox.json"
	SEEDED_ADDRESS=$(grep -o '"address": *"[^"]*"' "${SEEDED_RECORD}" | cut -d'"' -f4)
	SEEDED_LINKS="${SEEDED_LINKS} $(grep -o '"host_interface": *"[^"]*"' "${SEEDED_RECORD}" | cut -d'"' -f4)"
	[ "${SEEDED_ADDRESS}" != "${ADDRESS}" ] || fail "sandbox ${SEEDED_ID} got the source's address ${ADDRESS}"
	[ "$(listed_state "${SEEDED_ID}")" = "running" ] || fail "shard list does not list sandbox ${SEEDED_ID} running"
	grep -q "\"snapshot\": *\"${SNAPSHOT_ID}\"" "${SEEDED_RECORD}" || fail "sandbox ${SEEDED_ID} does not name snapshot ${SNAPSHOT_ID}: $(cat "${SEEDED_RECORD}")"
	grep -q '"checkpoint": *"[^"]' "${SEEDED_RECORD}" && fail "sandbox ${SEEDED_ID} names a checkpoint"
	# A snapshot runs shard-init alone, so the source's app and its log never come along.
	alone_in "${SEEDED_ID}" "in sandbox ${SEEDED_ID}"
	[ "$(shard logs "${SEEDED_ID}" | grep -c "shard-e2e-entrypoint")" = "0" ] || fail "sandbox ${SEEDED_ID} printed the source's banner"
	expect_exec_in "${SEEDED_ID}" "kept" "sandbox ${SEEDED_ID} holds the file the source wrote before the stop" /bin/cat /root/kept
	expect_exec_in "${SEEDED_ID}" "${SEEDED_ADDRESS}" "sandbox ${SEEDED_ID} holds its own address" \
		/bin/sh -c "ip -o -4 addr show eth0 | grep -o '${SEEDED_ADDRESS}'"
	expect_exec_in "${SEEDED_ID}" "reachable" "sandbox ${SEEDED_ID} gets out through the NAT" \
		/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
	expect_exec_in "${SEEDED_ID}" "e2e-seeded-${N}" "sandbox ${SEEDED_ID} carries its own hostname" /bin/hostname
	expect_exec_in "${SEEDED_ID}" "mock-E2E_TOKEN" "sandbox ${SEEDED_ID} holds the placeholder its create granted" /bin/sh -c 'echo "$E2E_TOKEN"'
	expect_blocked "${SEEDED_ID}" "the policy holds on sandbox ${SEEDED_ID}"
	expect_fronted "${SEEDED_ID}" "the proxy fronts sandbox ${SEEDED_ID}"
	[ -d "/sys/fs/cgroup/shard/${SEEDED_ID}" ] || fail "sandbox ${SEEDED_ID} has no cgroup under the shard parent"
done
say "both sandboxes run shard-init alone over the source's files, each on its own address"

shard exec "$1" /bin/sh -c 'echo seeded-only > /root/seeded-only' >/dev/null
CODE=0
shard exec "$2" /bin/cat /root/seeded-only >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "sandbox $2 sees the file sandbox $1 wrote"
[ ! -e "${SHARD_ROOT}/sandboxes/${ID}/overlay/upper/root/seeded-only" ] || fail "the source's layer holds what a seeded sandbox wrote"
absent "what a seeded sandbox wrote, in the snapshot" "$(find "${SNAPSHOT_DIR}" -name seeded-only)"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the seeded sandboxes changed the source's state"
say "the seeded sandboxes share nothing with each other, the snapshot or the source, which is still stopped"

step "refuse to snapshot a running sandbox"
CODE=0
REFUSAL=$(shard snapshot create "$1" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "snapshot create copied the running sandbox $1"
grep -q "stop it first" <<<"${REFUSAL}" || fail "snapshot create said '${REFUSAL}', want it to say stop it first"
say "snapshot create refused the running sandbox and named the stop"

step "stop and remove the seeded sandboxes and the snapshot"
for SEEDED_ID in "$@"; do
	shard stop "${SEEDED_ID}" >/dev/null
	shard remove "${SEEDED_ID}" >/dev/null
	absent "the record of sandbox ${SEEDED_ID}" "$([ -e "${SHARD_ROOT}/sandboxes/${SEEDED_ID}" ] && echo "${SHARD_ROOT}/sandboxes/${SEEDED_ID}" || true)"
done
for SEEDED_LINK in ${SEEDED_LINKS}; do
	absent "the link ${SEEDED_LINK} of a seeded sandbox" "$(ip link show "${SEEDED_LINK}" 2>/dev/null || true)"
done
SEEDED_IDS=""
SEEDED_LINKS=""
set --
shard snapshot remove e2e-snapshot >/dev/null
absent "the snapshot ${SNAPSHOT_ID}" "$([ -e "${SNAPSHOT_DIR}" ] && echo "${SNAPSHOT_DIR}" || true)"
absent "e2e-snapshot in snapshot list" "$(shard snapshot list | grep e2e-snapshot || true)"
SNAPSHOT_ID=""
say "the seeded sandboxes and the snapshot are gone"

step "refuse to remove the image a stopped sandbox references"
CODE=0
REFUSAL=$(shard image remove "${IMAGE}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "image remove removed the image under a stopped sandbox"
grep -q "${ID}" <<<"${REFUSAL}" || fail "image remove said '${REFUSAL}', want it to name the sandbox"
holds "${IMAGE%%:*}" shard image list || fail "image list no longer lists the image"
say "image remove refused it and named the sandbox"

step "start the sandbox again"
shard start "${ID}" >/dev/null
grep -q '"state": *"running"' "${RECORD}" || fail "the record does not say running after the start"
grep -q '"exit_status"' "${RECORD}" && fail "the record still holds the old exit status after the start"
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the start changed the address"
say "the record says running, without the old exit, on the same address"

# The entrypoint runs from the beginning, so its line lands a second time.
for _ in $(seq 1 50); do
	[ "$(shard logs "${ID}" | grep -c "shard-e2e-entrypoint")" -ge 2 ] && break
	sleep 0.2
done
[ "$(shard logs "${ID}" | grep -c "shard-e2e-entrypoint")" -ge 2 ] || fail "the entrypoint did not run again from the beginning"
say "the entrypoint ran again from the beginning"

expect_exec "kept" "the file written before the stop is there after the start" /bin/cat /root/kept
[ -d "/sys/fs/cgroup/shard/${ID}" ] || fail "the started sandbox has no cgroup under the shard parent"
# gVisor took the address at the first create, so this proves the start built the netns again.
expect_network "after the start"
expect_blocked "${ID}" "the policy holds after the start"
expect_fronted "${ID}" "the proxy fronts the sandbox after the start"

step "stop the started sandbox"
shard stop "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
say "the record says stopped"

step "remove the sandbox"
shard remove "${ID}" >/dev/null
say "remove returned"

step "grant a secret to a sandbox that was created without one"
# This sandbox is created unfronted, so the grant is what plants the placeholder, the CA and the dnat.
GRANT_ID=$(shard run -d "${IMAGE}" /bin/sh -c 'exec /bin/sleep 600')
GRANT_LINK=$(grep -o '"host_interface": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${GRANT_ID}/sandbox.json" | cut -d'"' -f4)
expect_exec_in "${GRANT_ID}" "" "the guest holds no placeholder before the grant" /bin/sh -c 'echo "$E2E_TOKEN"'
shard secret grant "${GRANT_ID}" E2E_TOKEN >/dev/null 2>&1 && fail "secret grant took a running sandbox"
say "secret grant refuses a running sandbox"

shard stop "${GRANT_ID}" >/dev/null
shard secret grant "${GRANT_ID}" E2E_TOKEN >/dev/null
holds '"E2E_TOKEN"' shard inspect "${GRANT_ID}" || fail "inspect does not name the grant"
shard start "${GRANT_ID}" >/dev/null
expect_exec_in "${GRANT_ID}" "mock-E2E_TOKEN" "the granted guest sees the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'

GRANT_BUNDLE=$(shard exec "${GRANT_ID}" /bin/sh -c 'cat "$SSL_CERT_FILE"')
grep -qF "${CA_LINE}" <<<"${GRANT_BUNDLE}" || fail "the late grant did not plant the proxy CA"
[ "$(echo "${GRANT_BUNDLE}" | grep -c 'BEGIN CERTIFICATE')" -gt 1 ] || fail "the late bundle holds the proxy CA alone"
say "the grant planted the proxy CA beside the image's roots"
fronted "${GRANT_ID}" || fail "the host holds no dnat to the proxy for ${GRANT_LINK}"
say "the grant turns the sandbox's 80 and 443 to the proxy"
expect_fronted "${GRANT_ID}" "the grant fronts the sandbox, and the proxy puts the value in"

step "ungrant the secret and prove the placeholder is gone"
shard stop "${GRANT_ID}" >/dev/null
shard secret ungrant "${GRANT_ID}" E2E_TOKEN >/dev/null
holds '"E2E_TOKEN"' shard inspect "${GRANT_ID}" && fail "inspect still names the grant"
shard start "${GRANT_ID}" >/dev/null
expect_exec_in "${GRANT_ID}" "" "the guest holds no placeholder after the ungrant" /bin/sh -c 'echo "$E2E_TOKEN"'
say "ungrant took the grant and the placeholder back"

step "attach a policy to a sandbox that was created without one"
# The sandbox holds neither a policy nor a secret here, so nothing fronts it and the attach is what does.
# The dnat is the whole of fronting; the decision log is history and still holds what the grant sent.
fronted "${GRANT_ID}" && fail "an unfronted sandbox holds a dnat to the proxy"
say "a sandbox with no policy and no secret is not fronted"

# allow dns first, so the name resolves and the deny is the proxy's, which is what proves the attach fronts the sandbox.
shard policy create --allow dns --deny "${ECHO_HOST}" --deny any e2e-attach >/dev/null

CODE=0
REFUSAL=$(shard policy attach "${GRANT_ID}" e2e-attach 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy attach took a running sandbox"
grep -q "stop it first" <<<"${REFUSAL}" || fail "policy attach said '${REFUSAL}', want the fix"
say "policy attach refuses a running sandbox"

shard stop "${GRANT_ID}" >/dev/null
shard policy attach "${GRANT_ID}" e2e-attach >/dev/null
shard start "${GRANT_ID}" >/dev/null
fronted "${GRANT_ID}" || fail "the attach did not turn the sandbox's 80 to the proxy"
say "the attach fronts the sandbox"

expect_exec_in "${GRANT_ID}" "403 Forbidden" "the attached policy denies the request with a 403" \
	/bin/sh -c "wget -S -O /dev/null http://${ECHO_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
DECISIONS=$(shard policy logs "${GRANT_ID}")
PROXY_DECISIONS=$(grep '"source":"proxy"' <<<"${DECISIONS}" || true)
grep -q '"verdict":"deny"' <<<"${PROXY_DECISIONS}" || fail "the egress log holds no proxy deny for ${GRANT_ID}"
say "the deny is in the egress decision log with source proxy"

[ "$(shard list --all | awk -v id="${GRANT_ID}" '$1 == id { print $NF }')" = "e2e-attach" ] || fail "shard list does not show the attached policy"
say "shard list shows the attached policy"

CODE=0
REFUSAL=$(shard policy remove e2e-attach 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy remove removed a policy an attach put on a sandbox"
grep -q "${GRANT_ID}" <<<"${REFUSAL}" || fail "policy remove said '${REFUSAL}', want it to name the sandbox"
say "policy remove refuses the attached policy and names the sandbox"

step "detach the policy and prove the sandbox is not fronted any more"
shard stop "${GRANT_ID}" >/dev/null
shard policy detach "${GRANT_ID}" >/dev/null
shard start "${GRANT_ID}" >/dev/null
fronted "${GRANT_ID}" && fail "the detached sandbox still holds a dnat to the proxy"
[ "$(shard list --all | awk -v id="${GRANT_ID}" '$1 == id { print $NF }')" = "-" ] || fail "shard list still shows a policy after the detach"
expect_exec_in "${GRANT_ID}" "reachable" "the detached guest still gets out through the NAT" \
	/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
say "detach leaves the sandbox with no policy and takes the fronting with it"

CODE=0
shard policy attach "${GRANT_ID}" e2e-missing >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy attach took a policy the host does not hold"
holds '"policy"' shard inspect "${GRANT_ID}" && fail "the refused attach wrote the record"
say "policy attach refuses a policy the host does not hold and writes nothing"


shard stop "${GRANT_ID}" >/dev/null
GRANT_ADDRESS=$(grep -o '"address": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${GRANT_ID}/sandbox.json" | cut -d'"' -f4)
GRANT_ADDRESS="${GRANT_ADDRESS%%/*}"
shard remove "${GRANT_ID}" >/dev/null
# The rules are keyed by the address, so rules left here would front whoever takes that address next.
for _ in $(seq 1 10); do
	RULES=$(nft list ruleset)
	grep -q "${GRANT_ADDRESS}" <<<"${RULES}" || break
	sleep 0.1
done
grep -q "${GRANT_ADDRESS}" <<<"${RULES}" && fail "remove left the host rules of ${GRANT_ADDRESS}: $(echo "${RULES}" | grep "${GRANT_ADDRESS}")"
grep -q "chain egress_${GRANT_LINK}" <<<"${RULES}" && fail "remove left the egress chain of ${GRANT_LINK}"
say "remove took the host rules of the sandbox with it"
ip link delete "${GRANT_LINK}" >/dev/null 2>&1 || true
GRANT_ID=""
GRANT_LINK=""
say "the granted sandbox is gone"

step "remove the secret nothing holds any more"
shard secret remove E2E_TOKEN >/dev/null
shard secret remove E2E_SHAPED >/dev/null
absent "the secret file" "$([ -e "${SHARD_ROOT}/secrets/E2E_TOKEN" ] && echo "${SHARD_ROOT}/secrets/E2E_TOKEN" || true)"
say "secret remove removed the secret"

step "remove the policies nothing holds any more"
shard policy remove e2e-policy >/dev/null
shard policy remove e2e-deny-all >/dev/null
shard policy remove e2e-attach >/dev/null
absent "the policy file" "$([ -e "${SHARD_ROOT}/policies/e2e-policy.json" ] && echo "${SHARD_ROOT}/policies/e2e-policy.json" || true)"
say "policy remove removed the policies"

step "prove the host holds nothing the sandbox left"
absent "the record" "$([ -e "${SHARD_ROOT}/sandboxes/${ID}" ] && echo "${SHARD_ROOT}/sandboxes/${ID}" || true)"
absent "the address lease" "$([ -e "${LEASE}" ] && echo "${LEASE}" || true)"
absent "the namespace" "$(ip netns list | grep "^${ID}" || true)"
absent "the user namespace pin" "$([ -e "${USERNS_DIR}/${ID}" ] && echo "${USERNS_DIR}/${ID}" || true)"
absent "the link" "$(ip link show "${LINK}" 2>/dev/null || true)"
absent "the address" "$(ip -o addr | grep "${ADDRESS%%/*}" || true)"
absent "the rootfs mount" "$(mount | grep "${SHARD_ROOT}/sandboxes" || true)"
absent "the runsc container" "$(ls "${SHARD_ROOT}/runsc" 2>/dev/null | grep "^${ID}" || true)"
absent "the list --all line" "$(shard list --all | grep "^${ID}" || true)"
absent "the cgroup" "$([ -e "/sys/fs/cgroup/shard/${ID}" ] && echo "/sys/fs/cgroup/shard/${ID}" || true)"

step "prune the image nothing references any more"
holds "${IMAGE%%:*}" shard image prune || fail "image prune did not remove the image"
holds "${IMAGE%%:*}" shard image list && fail "image list still lists the pruned image"
say "image prune removed the image once no sandbox referenced it"

step "remove the sandbox a second time"
CODE=0
REFUSAL=$(shard remove "${ID}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "a second remove exited 0 on an id that does not exist"
expect "${REFUSAL}" "shard: no sandbox ${ID}" "a second remove fails on the id it no longer finds"
expect "$(shard remove --force "${ID}" 2>&1)" "shard: warning: sandbox ${ID} does not exist, so there is nothing to remove" "a second remove --force only warns, so a teardown stays idempotent"

step "stop the daemon and prove the socket is gone"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
say "the socket is gone"
CODE=0
REFUSAL=$(shard list 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "shard list answered with the daemon stopped"
expect "${REFUSAL}" "shard: cannot connect to shard daemon at ${SOCKET}: is it running? shard --root ${SHARD_ROOT} daemon" "list fails fast once the daemon is gone"

step "clean up"
teardown
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
[ ! -e "/run/netns/${ECHO_NETNS_NAME}" ] || fail "the echo's netns ${ECHO_NETNS_NAME} is still on the host"
ip link show "${ECHO_LINK}0" >/dev/null 2>&1 && fail "the echo's link ${ECHO_LINK}0 is still on the host"
say "the run's own root, the echo's netns and its link are gone"
check_host_net_clear

trap - EXIT
echo
case "${PROVIDER}" in
gvisor) CHECKPOINT_STEPS="pause, fork, resume" ;;
sysbox) CHECKPOINT_STEPS="refused pause, resume and fork, docker build inside" ;;
runc) CHECKPOINT_STEPS="refused pause, resume and fork" ;;
esac
echo "e2e PASSED on ${PROVIDER}: install, daemon up, version, what the host picks, create, daemon restart, proxy, exec, exec again, the tcp front, the disk bound, ${CHECKPOINT_STEPS}, stop, inspect, start, grant, ungrant, remove, attach, detach, prune, daemon down, and a clean host"
