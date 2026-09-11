#!/usr/bin/env bash
# SHARD-17: the whole sandbox lifecycle on a no-KVM Linux box, from an install to a clean host.
# It installs the two binaries, starts shard daemon over the run's root (SHARD-124), creates a
# sandbox, execs into it twice over the same filesystem, pauses, resumes and forks it (SHARD-36),
# stops it, removes it, stops the daemon, and then proves the host holds nothing either left behind.
# One sandbox holds a secret and a policy, so it is fronted: the run starts an echo server on the
# host's 80 and 443 and proves the proxy puts the value in on the granted host only (SHARD-71).
#
#   sudo ./scripts/e2e.sh
#
# The run keeps its own state root, but the bridge, the subnet, the veth names and the two echo
# ports belong to the host, so it must not run beside live sandboxes from another root. It refuses
# one that has any, and a host whose 80 or 443 is taken.
#
# Environment:
#   PREFIX     where the binaries are installed        (default /usr/local/bin)
#   SHARD_ROOT where this run keeps its state          (default /var/lib/shard-e2e)
#   IMAGE      the image the sandbox is built from     (default alpine:3.20)
#   SKIP_INSTALL=1 to run against the binaries already on the box

set -euo pipefail

# ip and nft live in sbin, which a sudo that carries the caller's PATH does not have.
export PATH="${PATH}:/usr/sbin:/sbin"

PREFIX=${PREFIX:-/usr/local/bin}
SHARD_ROOT=${SHARD_ROOT:-/var/lib/shard-e2e}
IMAGE=${IMAGE:-alpine:3.20}
GRACE=${GRACE:-5s}

# The root the run must never delete, and the name every sandbox veth on the host starts with.
PRODUCTION_ROOT="/var/lib/shard"
HOST_LINK_PREFIX="shardv"

STEP="startup"
ID=""
LINK=""
FORK_ID=""
FORK_LINK=""
# The sandbox the reconcile step makes and removes itself, kept here so a failure halfway still frees it.
RECONCILE_ID=""
RECONCILE_LINK=""
# The clones are space separated lists: two come off one stopped source, and both must go on teardown.
CLONE_IDS=""
CLONE_LINKS=""
# The sandbox that is created unfronted and granted a secret later (SHARD-114).
GRANT_ID=""
GRANT_LINK=""
# The daemon this run started, which ls, inspect and version speak to; the trap stops it by pid.
DAEMON_PID=""
DAEMON_LOG=""
REPORTED=0
# The two ports the daemon's proxy listens on, as pkg/proxy declares them.
PROXY_PLAIN_PORT=30080
PROXY_TLS_PORT=30443
# The echo is the upstream behind the proxy, and it lives for the whole run.
ECHO_PID=""
ECHO_DIR=""
# The echo names all resolve to this host through sslip.io: one is granted, one is only allowed, one is neither.
ECHO_HOST=""
OTHER_HOST=""
DENIED_HOST=""

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
	if ! got=$(shard exec "${id}" -- "$@"); then
		fail "shard exec $* wrote the right bytes and then exited non-zero"
	fi

	expect "${got}" "${want}" "${note}"
}

# nap_alive reports the guest process of the background exec. The bracket keeps the probe off its own args.
nap_alive() { shard exec "${ID}" -- /bin/sh -c 'pgrep -f "[s]leep 313" >/dev/null' >/dev/null 2>&1; }

# listed_state reads the STATE column of shard ls for one sandbox, so the check never matches the image.
listed_state() { shard ls --all | awk -v id="$1" '$1 == id { print $4 }'; }

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
	shard exec "$1" -- /bin/sh -c 'p=$(pgrep -x sleep); echo "$p $(cut -d" " -f22 /proc/$p/stat)"'
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
	got=$(shard exec "${id}" -- /bin/sh -c 'ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1 && echo reachable || echo blocked')
	expect "${got}" "blocked" "${note}"
}

# fetch runs busybox wget in a sandbox against one echo name, over http or https, with the placeholder of
# each secret in a header, and prints the lines the echo answered with.
fetch() {
	local id="$1" scheme="$2" host="$3"
	shard exec "${id}" -- /bin/sh -c "wget -q -O - --header \"Authorization: Bearer \$E2E_TOKEN\" --header \"X-Shaped: \$E2E_SHAPED\" ${scheme}://${host}/"
}

# expect_fronted fails when a request from the sandbox to the granted host does not carry the real value,
# which proves the request went through the proxy and the proxy put the value in.
expect_fronted() {
	local id="$1" note="$2" got
	if ! got=$(fetch "${id}" http "${ECHO_HOST}"); then
		fail "the request to ${ECHO_HOST} from ${id} failed"
	fi
	echo "${got}" | grep -qx "authorization=Bearer ${SECRET_VALUE}" || fail "the echo saw '${got}', want the value in Authorization"
	say "${note}"
}

# basic_of prints what one echo host saw inside an HTTP Basic header, decoded. busybox has no curl, so the
# guest builds the header the way every client does: base64 of "user:placeholder".
basic_of() {
	local id="$1" host="$2" got line
	got=$(shard exec "${id}" -- /bin/sh -c "wget -q -O - --header \"Authorization: Basic \$(printf '%s' \"api:\$E2E_TOKEN\" | base64)\" http://${host}/") ||
		fail "the basic auth request to ${host} failed"
	line=$(grep '^authorization=Basic ' <<<"${got}") || fail "the echo saw no basic header: ${got}"
	printf '%s' "${line#authorization=Basic }" | base64 -d
}

# start_echo builds and starts the upstream on the host's 80 and 443. Both must be free: a server already
# there would answer the guest instead, and the run would prove nothing.
start_echo() {
	local busy
	busy=$(ss -Hltn '( sport = :80 or sport = :443 )' 2>/dev/null || true)
	[ -z "${busy}" ] || fail "port 80 or 443 is taken on this host, and the echo needs both: ${busy}"

	ECHO_DIR=$(mktemp -d)
	go build -o "${ECHO_DIR}/echo" ./scripts/echo
	"${ECHO_DIR}/echo" -address "${HOST_IPV4}" -names "${ECHO_HOST},${OTHER_HOST}" -cert-out "${ECHO_DIR}/cert.pem" -ready "${ECHO_DIR}/ready" >"${ECHO_DIR}/log" 2>&1 &
	ECHO_PID=$!
	for _ in $(seq 1 50); do
		[ -f "${ECHO_DIR}/ready" ] && return 0
		kill -0 "${ECHO_PID}" 2>/dev/null || break
		sleep 0.1
	done
	fail "the echo did not come up: $(cat "${ECHO_DIR}/log")"
}

stop_echo() {
	[ -n "${ECHO_PID}" ] || return 0
	kill "${ECHO_PID}" >/dev/null 2>&1 || true
	wait "${ECHO_PID}" >/dev/null 2>&1 || true
	ECHO_PID=""
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
clone_it() { CLONE_IDS="${CLONE_IDS} $(shard clone --name "$1" "${ID}")"; }

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
	SSL_CERT_FILE="${trust}" "${PREFIX}/shard" --root "${SHARD_ROOT}" daemon >"${DAEMON_LOG}" 2>&1 &
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

# teardown gives the host back. A run that failed halfway must not leave a sandbox behind: the
# record is the only handle by which its mount and its namespace can be found again.
teardown() {
	local id link
	# rm speaks to the daemon, so a run that broke while the daemon was down gets one back first.
	if [ -z "${DAEMON_PID}" ] && [ -x "${PREFIX}/shard" ] && [ -n "${ID}${FORK_ID}${CLONE_IDS}${RECONCILE_ID}${GRANT_ID}" ]; then
		start_daemon || echo "teardown: no daemon came up, so rm cannot run: $(cat "${DAEMON_LOG}")" >&2
	fi
	# shellcheck disable=SC2086 # the clone lists are meant to split
	for id in ${CLONE_IDS} "${GRANT_ID}" "${RECONCILE_ID}" "${FORK_ID}" "${ID}"; do
		[ -n "${id}" ] || continue
		shard rm --force "${id}" >/dev/null 2>&1 || true
		ip netns delete "${id}" >/dev/null 2>&1 || true
	done
	# shellcheck disable=SC2086
	for link in ${CLONE_LINKS} "${GRANT_LINK}" "${RECONCILE_LINK}" "${FORK_LINK}" "${LINK}"; do
		[ -n "${link}" ] || continue
		ip link delete "${link}" >/dev/null 2>&1 || true
	done

	stop_daemon || echo "teardown: the socket ${SHARD_ROOT}/shard.sock outlived the daemon" >&2
	stop_echo
	wipe_root
	rm -rf "${ECHO_DIR:-/nonexistent}" "${DAEMON_LOG:-/nonexistent}"
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

# E2E_LIB_ONLY lets the self-test source the helpers above without driving a sandbox.
if [ -n "${E2E_LIB_ONLY:-}" ]; then
	return 0
fi

trap on_exit EXIT

step "check the host"
[ "$(id -u)" = "0" ] || fail "shard drives netns, nft and runsc, so this needs root"
for binary in runsc ip ss nft go; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm, which is the box this ticket targets"
fi
say "runsc, ip, ss, nft and go are on the host"

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
REFUSAL=$(shard ls 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "shard ls answered with no daemon up"
expect "${REFUSAL}" "shard: cannot connect to shard daemon at ${SOCKET}: is it running? systemctl status shard" "ls names the socket and the daemon, and nothing else"

step "start the echo the fronted sandbox talks to"
# The echo answers on this host's own address, and sslip.io turns that address into three names.
HOST_IPV4=$(ip route get 1.1.1.1 | grep -o 'src [0-9.]*' | cut -d' ' -f2)
[ -n "${HOST_IPV4}" ] || fail "this host has no route to 1.1.1.1 to read its address from"
ECHO_HOST="api.${HOST_IPV4//./-}.sslip.io"
OTHER_HOST="other.${HOST_IPV4//./-}.sslip.io"
DENIED_HOST="deny.${HOST_IPV4//./-}.sslip.io"
start_echo
say "the echo answers on ${HOST_IPV4}, ports 80 and 443, as ${ECHO_HOST} and ${OTHER_HOST}"

step "start the daemon in the background"
start_daemon || fail "the daemon did not come up"
[ -S "${SOCKET}" ] || fail "no socket at ${SOCKET}"
LISTEN_LINE=$(grep "api listening on" "${DAEMON_LOG}")
say "the daemon logged: ${LISTEN_LINE#* api }"
say "the daemon logged: $(grep 'proxy listening on' "${DAEMON_LOG}" | sed 's/.*proxy/proxy/')"

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

step "read both versions"
VERSION_OUT=$(shard version)
CLIENT_LINE=$(echo "${VERSION_OUT}" | sed -n 1p)
DAEMON_LINE=$(echo "${VERSION_OUT}" | sed -n 2p)
[ "${CLIENT_LINE#client }" != "${CLIENT_LINE}" ] || fail "the first line of shard version is '${CLIENT_LINE}', want 'client <v>'"
expect "${DAEMON_LINE}" "daemon ${CLIENT_LINE#client }" "version prints the client line and the daemon line, and both agree"

step "store a secret"
# The value is synthetic and unique to this run, so a grep of the root can prove where it is and is not.
SECRET_VALUE="e2e-secret-value-$$-$(date +%s)"
printf '%s\n' "${SECRET_VALUE}" | shard secret set --to "${ECHO_HOST}" E2E_TOKEN >/dev/null
# The second secret names its own placeholder, for an SDK that checks the shape of a key before it sends it.
SHAPED_PLACEHOLDER="sk_test_e2eplaceholder01"
SHAPED_VALUE="sk_live_e2e_$$_$(date +%s)"
CAUTION=$(shard secret set --to "${ECHO_HOST}" --placeholder "${SHAPED_PLACEHOLDER}" E2E_SHAPED "${SHAPED_VALUE}" 2>&1 >/dev/null)
echo "${CAUTION}" | grep -q "visible in the process list" || fail "a value on the command line printed no caution: '${CAUTION}'"
say "a value on the command line is stored, with a caution on stderr"
# The caution is about what ps saw, and a refusal does not un-see it.
CODE=0
REFUSED_CAUTION=$(shard secret set --to no-dot E2E_REFUSED "${SHAPED_VALUE}" 2>&1 >/dev/null) || CODE=$?
[ "${CODE}" != "0" ] || fail "secret set took a destination with no dot"
echo "${REFUSED_CAUTION}" | grep -q "caution" || fail "a refused set printed no caution: '${REFUSED_CAUTION}'"
say "a refused set still cautions about the value on the command line"
SHAPED_LS=$(shard secret ls)
echo "${SHAPED_LS}" | grep -q "${SHAPED_PLACEHOLDER}" || fail "shard secret ls does not print the chosen placeholder: ${SHAPED_LS}"
say "secret ls prints the chosen placeholder"
SECRET_LS=$(shard secret ls)
echo "${SECRET_LS}" | grep -q "E2E_TOKEN" || fail "shard secret ls does not list E2E_TOKEN: ${SECRET_LS}"
echo "${SECRET_LS}" | grep -q "${SECRET_VALUE}" && fail "shard secret ls printed the value"
say "secret ls lists the name and the destination, and not the value"
SECRET_MODE=$(stat -c '%a' "${SHARD_ROOT}/secrets/E2E_TOKEN")
[ "${SECRET_MODE}" = "600" ] || fail "the secret file is mode ${SECRET_MODE}, want 600"
say "the secret file is mode 0600"

step "store an egress policy"
# The probe address is allowed on every protocol, so the ping the network checks use goes through. The
# other echo name is allowed by rule and not granted, so a request to it must keep the placeholder.
shard policy create --deny any e2e-deny-all >/dev/null
shard policy create --allow 1.1.1.1 --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
POLICY_LS=$(shard policy ls)
echo "${POLICY_LS}" | grep -q "e2e-policy" || fail "shard policy ls does not list e2e-policy: ${POLICY_LS}"
shard policy show e2e-policy | grep -q '"kind": "cidr"' || fail "shard policy show does not print the rules"
say "policy ls lists the policies and policy show prints the rules"
shard policy create --allow suffix:example.com --allow '*.example.com' e2e-web >/dev/null
shard policy rm e2e-web >/dev/null
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

step "create a sandbox"
# The entrypoint speaks once, so logs has something to show, and then holds the sandbox up.
ID=$(shard create --secret E2E_TOKEN --secret E2E_SHAPED --policy e2e-policy "${IMAGE}" -- /bin/sh -c 'echo shard-e2e-entrypoint; exec /bin/sleep 600')
[ -n "${ID}" ] || fail "create printed no id"
say "create printed the id ${ID}"

RECORD="${SHARD_ROOT}/sandboxes/${ID}/sandbox.json"
[ -f "${RECORD}" ] || fail "there is no record at ${RECORD}"
ADDRESS=$(grep -o '"address": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
LINK=$(grep -o '"host_interface": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
say "the record holds the address ${ADDRESS} on the link ${LINK}"

ip netns list | grep -q "^${ID}" || fail "there is no namespace named ${ID}"
ip link show "${LINK}" >/dev/null || fail "there is no link named ${LINK}"
say "the namespace and the link are up"

step "list the sandbox"
LISTED=$(shard ls | grep "^${ID}" || true)
[ -n "${LISTED}" ] || fail "shard ls does not list ${ID}"
echo "${LISTED}" | grep -q "${ADDRESS%%/*}" || fail "shard ls listed '${LISTED}', want the address ${ADDRESS%%/*} on it"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard ls listed '${LISTED}', want it running"
say "ls shows the sandbox running on its address"

step "restart the daemon and prove the sandbox and an exec in flight outlive it"
SANDBOX_PID=$(grep -o '"pid": *[0-9]*' "${RECORD}" | grep -o '[0-9]*$')
[ -n "${SANDBOX_PID}" ] || fail "the record holds no pid"
# The exec is in flight when the daemon goes: its client dies with the stream, its guest process must not.
(shard exec "${ID}" -- /bin/sleep 313 >/dev/null 2>&1 || true) &
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
[ "$(listed_state "${ID}")" = "running" ] || fail "shard ls does not list ${ID} running after the daemon restart"
expect_exec "restarted" "an exec answers after the daemon restart" /bin/echo restarted
# grep -c reads to the end, so nft never takes a SIGPIPE that pipefail would count as a miss.
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK} after the daemon restart"
say "the host still holds the egress chain of the sandbox"
expect_exec "alive" "the guest process of the exec in flight outlived the daemon" \
	/bin/sh -c 'pgrep -f "[s]leep 313" >/dev/null && echo alive'
# The pause step reads the entrypoint by name, so the nap must be gone before it: nothing reattaches to it.
shard exec "${ID}" -- /bin/sh -c 'pkill -f "[s]leep 313"' >/dev/null
wait "${EXEC_CLIENT_PID}" 2>/dev/null || true
say "the client of that exec is gone, and no verb reattaches to it"

step "reconcile a sandbox the host lost while the daemon was down"
RECONCILE_ID=$(shard create --name e2e-lost "${IMAGE}" -- /bin/sleep 600)
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
shard ls --all | grep "^${RECONCILE_ID}" | grep -q "daemon restarted and found no process" \
	|| fail "shard ls gives no reason for ${RECONCILE_ID}"
say "ls gives the reason: daemon restarted and found no process"
grep -q "${RECONCILE_ID}" "${DAEMON_LOG}" || fail "the daemon logged no line for the record it corrected"
say "the daemon logged the record it corrected"
shard rm --force "${RECONCILE_ID}" >/dev/null || fail "rm did not free the sandbox the host lost"
ip link delete "${RECONCILE_LINK}" >/dev/null 2>&1 || true
RECONCILE_ID=""
RECONCILE_LINK=""
say "rm freed what it left on the host"

step "read the output of the entrypoint"
# The line lands when the guest gets to it, which is after create returned.
for _ in $(seq 1 50); do
	shard logs "${ID}" | grep -q "shard-e2e-entrypoint" && break
	sleep 0.2
done
shard logs "${ID}" | grep -q "shard-e2e-entrypoint" || fail "shard logs does not show what the entrypoint wrote"
say "logs shows what the entrypoint wrote"

step "exec a command in the sandbox"
expect_exec "shard-e2e" "the command ran and wrote a file" \
	/bin/sh -c 'echo shard-e2e > /tmp/marker; cat /tmp/marker'

step "exec again into the same filesystem state"
expect_exec "shard-e2e" "the second exec read what the first one wrote" /bin/cat /tmp/marker

step "hold the placeholder and never the value"
expect_exec "mock-E2E_TOKEN" "the guest sees the placeholder as \$E2E_TOKEN" /bin/sh -c 'echo "$E2E_TOKEN"'
expect_exec "${SHAPED_PLACEHOLDER}" "the guest sees the chosen placeholder as \$E2E_SHAPED" /bin/sh -c 'echo "$E2E_SHAPED"'
printf '%s\n' "${SHAPED_VALUE}" | shard secret set --placeholder sk_test_othershape01 E2E_SHAPED >/dev/null 2>"${SHARD_ROOT}/moved.err" && fail "secret set moved a placeholder the sandbox holds"
grep -q "${ID}" "${SHARD_ROOT}/moved.err" || fail "the refusal does not name the sandbox: $(cat "${SHARD_ROOT}/moved.err")"
say "secret set refuses to move a placeholder the sandbox holds, and names it"
# The store file is the one place the value is written; nothing under the sandbox tree or anywhere else holds it.
absent "the value outside the store" "$(grep -rl --exclude-dir=secrets "${SECRET_VALUE}" "${SHARD_ROOT}" 2>/dev/null || true)"
shard inspect "${ID}" | grep -q '"E2E_TOKEN"' || fail "inspect does not name the grant"
say "inspect names the secret and holds no value"
shard secret rm E2E_TOKEN >/dev/null 2>&1 && fail "secret rm removed a secret a sandbox holds"
say "secret rm refuses while the sandbox holds the secret"

step "front the sandbox through the proxy"
# grep -c reads to the end, so nft never takes a SIGPIPE that pipefail would count as a miss.
nft list table inet shard | grep -c "dnat ip to .*:30080" >/dev/null || fail "the host holds no dnat to the proxy for ${LINK}"
say "the host turns the sandbox's 80 and 443 to the proxy"
# The guest trusts the proxy CA beside the image's own roots, at the path the image already reads.
CA_LINE=$(sed -n 2p "${SHARD_ROOT}/proxy/ca.crt")
GUEST_BUNDLE=$(shard exec "${ID}" -- /bin/sh -c 'cat "$SSL_CERT_FILE"')
echo "${GUEST_BUNDLE}" | grep -q "${CA_LINE}" || fail "the guest's \$SSL_CERT_FILE does not hold the proxy CA"
[ "$(echo "${GUEST_BUNDLE}" | grep -c 'BEGIN CERTIFICATE')" -gt 1 ] || fail "the guest's bundle holds the proxy CA alone"
say "the guest trusts the proxy CA and still trusts the image's roots"

step "a grant opens nothing: the policy alone decides the host"
# The policy so far names 1.1.1.1 and the other host, so the granted host falls to the catch-all.
DENIED=$(shard exec "${ID}" -- /bin/sh -c "wget -S -O - --header \"Authorization: Bearer \$E2E_TOKEN\" http://${ECHO_HOST}/ 2>&1" || true)
grep -q "403 Forbidden" <<<"${DENIED}" || fail "the granted host the policy does not allow answered '${DENIED}'"
grep -q "authorization=" <<<"${DENIED}" && fail "the echo answered a request the proxy should have denied"
say "a granted host the policy does not allow gets a 403, and the echo never sees the request"

DECISIONS=$(shard logs --egress "${ID}")
PROXY_DENIES=$(grep '"source":"proxy"' <<<"${DECISIONS}" | grep '"verdict":"deny"' || true)
grep -q "\"host\":\"${ECHO_HOST}\"" <<<"${PROXY_DENIES}" || fail "the egress log holds no proxy deny for ${ECHO_HOST}"
grep -qE '"rule_text":"deny (any|0\.0\.0\.0/0)"' <<<"${PROXY_DENIES}" || fail "the deny does not name the catch-all: ${PROXY_DENIES}"
say "the egress log names the catch-all that denied it, with source proxy"

step "a secret opens no DNS either"
# Only address rules, so nothing implies port 53. The address is not a nameserver: allowing one would
# open DNS by address and prove nothing.
shard policy create --allow 1.0.0.1 --deny any e2e-policy >/dev/null
expect_exec "unresolved" "a policy of addresses only leaves the granted host unresolvable" \
	/bin/sh -c "timeout 5 nslookup ${ECHO_HOST} >/dev/null 2>&1 && echo resolved || echo unresolved"

NOTE=$(shard policy create --allow 1.0.0.1 --deny any e2e-note 2>&1 >/dev/null)
echo "${NOTE}" | grep -q "this policy opens no DNS" || fail "policy create said '${NOTE}' over a policy that opens no DNS"
shard policy rm e2e-note >/dev/null
say "policy create notes a policy that opens no DNS, and exits 0"

# The lookup the address-only policy dropped is in the log, as a host drop on port 53 to a nameserver.
DNS_DROP=""
for _ in $(seq 1 20); do
	DNS_DROP=$(shard logs --egress "${ID}" | grep '"source":"host"' | grep '"port":53' || true)
	[ -n "${DNS_DROP}" ] && break
	sleep 0.1
done
[ -n "${DNS_DROP}" ] || fail "the egress log holds no host drop on port 53 for the lookup the policy closed"
say "the dropped lookup is in the egress log, as a host drop on port 53"

step "allow dns opens the lookup the address rules left shut"
shard policy create --allow 1.0.0.1 --allow dns --allow "${ECHO_HOST}" --deny any e2e-policy >/dev/null
expect_exec "resolved" "the same policy with --allow dns resolves the echo name" \
	/bin/sh -c "timeout 5 nslookup ${ECHO_HOST} >/dev/null 2>&1 && echo resolved || echo unresolved"
shard policy show e2e-policy | grep -q '"dns": "open"' || fail "policy show does not say dns is open"
shard inspect "${ID}" | grep -q '"implied": "dns rule"' || fail "inspect does not name the dns rule that opened 53"
say "policy show says dns is open and inspect names the rule that opened 53"

CODE=0
shard policy create --deny dns e2e-bad >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy create accepted a deny dns rule"
say "policy create refuses deny dns: dns is closed until a rule opens it"

# From here the policy allows the granted host, which is what every step below needs.
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null

expect_fronted "${ID}" "a request to the granted host carries the value, and the guest only ever sent the placeholder"
GOT=$(fetch "${ID}" https "${ECHO_HOST}") || fail "the https request to ${ECHO_HOST} failed"
echo "${GOT}" | grep -qx "authorization=Bearer ${SECRET_VALUE}" || fail "the echo saw '${GOT}' over tls, want the value in Authorization"
echo "${GOT}" | grep -qx "x-shaped=${SHAPED_VALUE}" || fail "the echo saw '${GOT}' over tls, want the value under the chosen placeholder"
say "the same holds over tls, and the chosen placeholder carries its own value"

GOT=$(fetch "${ID}" http "${OTHER_HOST}") || fail "the http request to ${OTHER_HOST} failed"
echo "${GOT}" | grep -qx "authorization=Bearer mock-E2E_TOKEN" || fail "the echo saw '${GOT}' from the other host, want the placeholder untouched"
echo "${GOT}" | grep -qx "x-shaped=${SHAPED_PLACEHOLDER}" || fail "the echo saw '${GOT}' from the other host, want the chosen placeholder untouched"
say "a request to a host the policy allows but the grant does not keeps both placeholders"

step "a client that encodes the placeholder still gets the value"
expect "$(basic_of "${ID}" "${ECHO_HOST}")" "api:${SECRET_VALUE}" "basic auth to the granted host carries the value, decoded and re-encoded"
expect "$(basic_of "${ID}" "${OTHER_HOST}")" "api:mock-E2E_TOKEN" "basic auth to an ungranted host keeps the placeholder"

expect_exec "403 Forbidden" "a request to a host no rule allows gets a 403 from the proxy" \
	/bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
step "a policy deny closes a granted host"
# The three names share the host's address, so 80 and 443 go to the proxy and the proxy is the only judge.
shard policy create --deny "${ECHO_HOST}" --allow 1.1.1.1 --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_exec "403 Forbidden" "a request to the granted host the policy denies gets a 403" \
	/bin/sh -c "wget -S -O /dev/null --header \"Authorization: Bearer \$E2E_TOKEN\" http://${ECHO_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_fronted "${ID}" "the same grant passes again once the policy allows the host"

expect_exec "" "the value is not in the guest's environment" /bin/sh -c "env | grep -F '${SECRET_VALUE}' || true"
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
shard exec "${ID}" -- /bin/sh -c 'ping -6 -c 1 -W 2 ff02::1%eth0 || ping6 -c 1 -W 2 ff02::1%eth0' >/dev/null 2>&1 || true
say "the guest sent an IPv6 packet, which the port must drop"
shard inspect "${ID}" | grep -q '"policy": "e2e-policy"' || fail "inspect does not name the policy"
shard inspect "${ID}" | grep -q '"egress"' || fail "inspect does not print what the host enforces"
say "inspect names the policy and what the host enforces"

# A policy change reaches a live sandbox at once, and never waits for the next start.
shard policy create --deny any e2e-policy >/dev/null
BLOCKED=$(shard exec "${ID}" -- /bin/sh -c 'ping -c 1 -W 2 1.1.1.1 >/dev/null 2>&1 && echo reachable || echo blocked')
expect "${BLOCKED}" "blocked" "a deny-all policy blocks the probe the moment it is stored"
shard policy create --allow 1.1.1.1 --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_network "after the policy was put back"

CODE=0
REFUSAL=$(shard policy rm e2e-policy 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy rm removed a policy a sandbox holds"
echo "${REFUSAL}" | grep -q "${ID}" || fail "policy rm said '${REFUSAL}', want it to name the sandbox"
say "policy rm refused it and named the sandbox"

step "read the egress decision log"
# The daemon tails the host's half out of the kernel ring, so the drop is a moment behind the probe.
EGRESS=""
for _ in $(seq 1 20); do
	EGRESS=$(shard logs --egress "${ID}")
	echo "${EGRESS}" | grep '"source":"host"' | grep -q '"rule":"ipv6"' && break
	sleep 0.1
done

named_rule() {
	echo "${EGRESS}" | grep "$1" | grep "$2" | grep -qE '"rule":"[^"]+"' || fail "the egress log holds no $3 with the rule that decided it"
	say "the egress log holds $3 with the rule that decided it"
}

named_rule "\"host\":\"${ECHO_HOST}\"" '"verdict":"allow"' "the proxy's allow"
named_rule "\"host\":\"${DENIED_HOST}\"" '"verdict":"deny"' "the proxy's deny"
named_rule '"source":"host"' '"verdict":"deny"' "the host's drop"
echo "${EGRESS}" | grep '"source":"host"' | grep -q '"rule":"local"' || fail "the egress log holds no drop of a packet aimed at the host's own address"
say "the egress log holds the drop of a packet aimed at the host's own address, on rule local"
echo "${EGRESS}" | grep '"source":"host"' | grep -q '"rule":"ipv6"' || fail "the egress log holds no drop of an IPv6 packet"
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
shard logs --egress "${ID}" | grep -q '"source":"host"' || fail "the host drop did not outlive the daemon that wrote it"
say "the host drop is still in the log after a daemon restart"

for _ in $(seq 1 20); do
	shard logs --egress "${ID}" | grep -q '"rule":"e2e-catchup"' && break
	sleep 0.1
done
shard logs --egress "${ID}" | grep -q '"rule":"e2e-catchup"' || fail "the drop that landed while the daemon was down never reached the log"
say "a drop that landed while the daemon was down is written at catch-up"

# A follow is a tail of the one file, so both halves of the log reach it live.
FOLLOW_LOG=$(mktemp)
shard logs -f --egress "${ID}" >"${FOLLOW_LOG}" 2>&1 &
FOLLOW_PID=$!

shard exec "${ID}" -- /bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ >/dev/null 2>&1" >/dev/null 2>&1 || true
shard exec "${ID}" -- /bin/sh -c 'ping -c 1 -W 2 8.8.8.8 >/dev/null 2>&1' >/dev/null 2>&1 || true

for _ in $(seq 1 20); do
	grep -q '"verdict":"deny"' "${FOLLOW_LOG}" && grep -q '"source":"host"' "${FOLLOW_LOG}" && break
	sleep 0.1
done

kill "${FOLLOW_PID}" 2>/dev/null || true
wait "${FOLLOW_PID}" 2>/dev/null || true

grep -q '"verdict":"deny"' "${FOLLOW_LOG}" || fail "the follow never printed the proxy's deny: $(cat "${FOLLOW_LOG}")"
grep -q '"source":"host"' "${FOLLOW_LOG}" || fail "the follow never printed the host's drop: $(cat "${FOLLOW_LOG}")"
say "logs -f --egress prints both halves as they happen"

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
shard exec "${ID}" -- /bin/sh -c 'exit 7' >/dev/null 2>&1 || CODE=$?
expect "${CODE}" "7" "a non-zero exit inside the sandbox reached this shell"

step "pause the sandbox"
# A restore keeps the guest's processes; a restart makes new ones. The entrypoint's pid and start
# time tell the two apart from outside, and the file proves the layer went with the memory.
shard exec "${ID}" -- /bin/sh -c 'echo before-the-pause > /root/at-pause' >/dev/null
CLOCK_BEFORE=$(entrypoint_clock "${ID}")
[ -n "${CLOCK_BEFORE}" ] || fail "the guest has no entrypoint to read a clock from"
say "the entrypoint is guest pid and start time ${CLOCK_BEFORE} before the pause"
PID=$(grep -o '"pid": *[0-9]*' "${RECORD}" | grep -o '[0-9]*$')
RSS_BEFORE=$(rss_kib "${PID}")
[ -n "${RSS_BEFORE}" ] || fail "the sandbox process ${PID} has no resident set to read"
say "the sandbox process ${PID} holds ${RSS_BEFORE} KiB on the host before the pause"

timed "pause" pause_it
grep -q '"state": *"paused"' "${RECORD}" || fail "the record does not say paused"
SNAPSHOT=$(grep -o '"snapshot": *"[^"]*"' "${RECORD}" | cut -d'"' -f4)
[ -f "${SNAPSHOT}/checkpoint.img" ] || fail "there is no checkpoint at ${SNAPSHOT}/checkpoint.img"
say "the record says paused and the snapshot is at ${SNAPSHOT}"
# The snapshot is the guest's memory after it sent the placeholder out, so the value must not be in it.
absent "the value in the memory snapshot" "$(grep -rl "${SECRET_VALUE}" "${SNAPSHOT}" 2>/dev/null || true)"

# The whole point of a pause: the memory goes back to the host. runsc holds nothing, so the process is gone.
absent "the sandbox process ${PID} and its ${RSS_BEFORE} KiB" "$(rss_kib "${PID}")"
absent "the cgroup of the paused sandbox" "$([ -e "/sys/fs/cgroup/shard/${ID}" ] && echo "/sys/fs/cgroup/shard/${ID}" || true)"
absent "the rootfs mount of the paused sandbox" "$(mount | grep "${SHARD_ROOT}/sandboxes/${ID}" || true)"
[ "$(listed_state "${ID}")" = "paused" ] || fail "shard ls --all does not list the sandbox as paused"

CODE=0
REFUSAL=$(shard exec "${ID}" -- /bin/true 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "exec ran in a paused sandbox"
echo "${REFUSAL}" | grep -q "shard resume ${ID}" || fail "exec said '${REFUSAL}', want it to name the resume"
say "exec refused the paused sandbox and named the resume"

step "resume the sandbox"
timed "resume" resume_it
grep -q '"state": *"running"' "${RECORD}" || fail "the record does not say running after the resume"
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the resume changed the address"
say "the record says running on the same address"

expect "$(entrypoint_clock "${ID}")" "${CLOCK_BEFORE}" "the entrypoint is the same process with the same start time, so the resume was a restore"
expect_exec "before-the-pause" "the file written before the pause is there after the resume" /bin/cat /root/at-pause
# The restore rebuilt the guest over a new namespace, and the host rules were applied again over it.
expect_network "after the resume"
expect_blocked "${ID}" "the policy holds after the resume"
expect_fronted "${ID}" "the proxy fronts the sandbox after the resume"

step "fork the paused snapshot into a second sandbox"
# A fork reads the snapshot, so the source may run on: the fork is the sandbox as it was at the pause.
timed "fork" fork_it
[ -n "${FORK_ID}" ] && [ "${FORK_ID}" != "${ID}" ] || fail "fork printed '${FORK_ID}', want a new id"
FORK_RECORD="${SHARD_ROOT}/sandboxes/${FORK_ID}/sandbox.json"
FORK_ADDRESS=$(grep -o '"address": *"[^"]*"' "${FORK_RECORD}" | cut -d'"' -f4)
FORK_LINK=$(grep -o '"host_interface": *"[^"]*"' "${FORK_RECORD}" | cut -d'"' -f4)
[ "${FORK_ADDRESS}" != "${ADDRESS}" ] || fail "the fork got the source's address ${ADDRESS}"
say "the fork is ${FORK_ID} on its own address ${FORK_ADDRESS} and link ${FORK_LINK}"

[ "$(listed_state "${FORK_ID}")" = "running" ] || fail "shard ls does not list the fork running"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard ls no longer lists the source running"
say "ls shows the source and the fork running side by side"

shard inspect "${FORK_ID}" | grep -q '"E2E_TOKEN"' || fail "the fork did not carry the grant"
shard inspect "${FORK_ID}" | grep -q '"policy": "e2e-policy"' || fail "the fork did not carry the policy"
expect_blocked "${FORK_ID}" "the policy holds on the fork"
expect_exec_in "${FORK_ID}" "mock-E2E_TOKEN" "the fork holds the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'
expect_fronted "${FORK_ID}" "the proxy fronts the fork on its own address"

expect_exec_in "${FORK_ID}" "before-the-pause" "the fork holds the file the source wrote before the pause" /bin/cat /root/at-pause
expect_exec_in "${FORK_ID}" "${FORK_ADDRESS}" "the fork holds its own address" \
	/bin/sh -c "ip -o -4 addr show eth0 | grep -o '${FORK_ADDRESS}'"
expect_exec_in "${FORK_ID}" "reachable" "the fork gets out through the NAT" \
	/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
expect_exec_in "${FORK_ID}" "e2e-fork" "the fork carries its own hostname" /bin/hostname
shard exec "${FORK_ID}" -- /bin/sh -c 'echo fork-only > /root/fork-only' >/dev/null

CODE=0
shard exec "${ID}" -- /bin/cat /root/fork-only >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "the source sees the file the fork wrote"
say "the source does not see what the fork wrote"

step "stop and remove the fork"
shard stop --time "${GRACE}" "${FORK_ID}" >/dev/null
shard rm "${FORK_ID}" >/dev/null
absent "the fork's record" "$([ -e "${SHARD_ROOT}/sandboxes/${FORK_ID}" ] && echo "${SHARD_ROOT}/sandboxes/${FORK_ID}" || true)"
absent "the fork's link" "$(ip link show "${FORK_LINK}" 2>/dev/null || true)"
FORK_ID=""
FORK_LINK=""
# The sandbox the reconcile step makes and removes itself, kept here so a failure halfway still frees it.
RECONCILE_ID=""
RECONCILE_LINK=""
expect_exec "before-the-pause" "the source runs on after the fork is gone" /bin/cat /root/at-pause

step "refuse to remove a sandbox that is still up"
CODE=0
REFUSAL=$(shard rm "${ID}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "rm removed a running sandbox"
echo "${REFUSAL}" | grep -q "shard stop ${ID}" || fail "rm said '${REFUSAL}', want it to say to stop it first"
say "rm refused it and named the stop"

step "stop the sandbox"
shard stop --time "${GRACE}" "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
say "the record says stopped"

# This is the boundary the ticket names: a stop keeps everything a later start needs.
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the stop dropped the address"
ip netns list | grep -q "^${ID}" || fail "the stop dropped the namespace"
ip link show "${LINK}" >/dev/null || fail "the stop dropped the link"
# The lease is a file named by the address, and it holds the id of the sandbox that took it.
LEASE="${SHARD_ROOT}/network/leases/${ADDRESS%%/*}"
grep -qx "${ID}" "${LEASE}" || fail "the stop dropped the address lease"
say "the record, the address, the lease, the namespace and the link all survived the stop"

shard ls | grep -q "^${ID}" && fail "shard ls still lists the stopped sandbox"
shard ls --all | grep "^${ID}" | grep -q "stopped" || fail "shard ls --all does not list the sandbox as stopped"
say "ls hides the stopped sandbox and ls --all shows it stopped"

# -f ends on its own once the sandbox is stopped, so a hang here is a failure, not a wait.
timeout 10 "${PREFIX}/shard" --root "${SHARD_ROOT}" logs -f "${ID}" | grep -q "shard-e2e-entrypoint" || fail "shard logs -f on a stopped sandbox did not print its output and end"
say "logs still reads a stopped sandbox, and -f ends on its own"

step "stop the sandbox a second time"
shard stop "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the second stop changed the state"
say "a second stop is idempotent"

step "inspect the stopped sandbox"
shard inspect "${ID}" | grep -q '"state": "stopped"' || fail "shard inspect does not say stopped"
shard inspect "${ID}" | grep -q '"exit_status"' || fail "shard inspect holds no exit status after the stop"
say "inspect prints the record with its state and its exit status"
CODE=0
REFUSAL=$(shard inspect no-such-sandbox 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "inspect answered for a sandbox nothing holds"
expect "${REFUSAL}" "shard: no sandbox no-such-sandbox" "inspect of a name nothing holds is one line"

step "clone the stopped sandbox twice"
# A clone copies the files a stop kept and runs the entrypoint again under a new id: no memory, no snapshot.
timed "clone" clone_it e2e-clone-1
timed "clone" clone_it e2e-clone-2
# shellcheck disable=SC2086 # the clone list is meant to split
set -- ${CLONE_IDS}
[ "$#" = "2" ] && [ "$1" != "$2" ] && [ "$1" != "${ID}" ] && [ "$2" != "${ID}" ] || fail "clone printed '${CLONE_IDS}', want two new ids"
N=0
for CLONE_ID in "$@"; do
	N=$((N + 1))
	CLONE_RECORD="${SHARD_ROOT}/sandboxes/${CLONE_ID}/sandbox.json"
	CLONE_ADDRESS=$(grep -o '"address": *"[^"]*"' "${CLONE_RECORD}" | cut -d'"' -f4)
	CLONE_LINKS="${CLONE_LINKS} $(grep -o '"host_interface": *"[^"]*"' "${CLONE_RECORD}" | cut -d'"' -f4)"
	[ "${CLONE_ADDRESS}" != "${ADDRESS}" ] || fail "clone ${CLONE_ID} got the source's address ${ADDRESS}"
	[ "$(listed_state "${CLONE_ID}")" = "running" ] || fail "shard ls does not list clone ${CLONE_ID} running"
	grep -q '"exit_status"' "${CLONE_RECORD}" && fail "clone ${CLONE_ID} carries the source's exit status"
	grep -q '"snapshot": *"[^"]' "${CLONE_RECORD}" && fail "clone ${CLONE_ID} names a snapshot"
	# A fresh run prints the banner once in the clone's own log, and never the source's earlier lines.
	for _ in $(seq 1 50); do
		[ "$(shard logs "${CLONE_ID}" | grep -c "shard-e2e-entrypoint")" -ge 1 ] && break
		sleep 0.2
	done
	[ "$(shard logs "${CLONE_ID}" | grep -c "shard-e2e-entrypoint")" = "1" ] || fail "clone ${CLONE_ID} printed the banner $(shard logs "${CLONE_ID}" | grep -c "shard-e2e-entrypoint") times, want once"
	expect_exec_in "${CLONE_ID}" "kept" "clone ${CLONE_ID} holds the file the source wrote before the stop" /bin/cat /root/kept
	expect_exec_in "${CLONE_ID}" "${CLONE_ADDRESS}" "clone ${CLONE_ID} holds its own address" \
		/bin/sh -c "ip -o -4 addr show eth0 | grep -o '${CLONE_ADDRESS}'"
	expect_exec_in "${CLONE_ID}" "reachable" "clone ${CLONE_ID} gets out through the NAT" \
		/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
	expect_exec_in "${CLONE_ID}" "e2e-clone-${N}" "clone ${CLONE_ID} carries its own hostname" /bin/hostname
	expect_exec_in "${CLONE_ID}" "mock-E2E_TOKEN" "clone ${CLONE_ID} holds the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'
	expect_blocked "${CLONE_ID}" "the policy holds on clone ${CLONE_ID}"
	expect_fronted "${CLONE_ID}" "the proxy fronts clone ${CLONE_ID}"
	[ -d "/sys/fs/cgroup/shard/${CLONE_ID}" ] || fail "clone ${CLONE_ID} has no cgroup under the shard parent"
done
say "both clones run the entrypoint again over the source's files, each on its own address"

shard exec "$1" -- /bin/sh -c 'echo clone-only > /root/clone-only' >/dev/null
CODE=0
shard exec "$2" -- /bin/cat /root/clone-only >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "clone $2 sees the file clone $1 wrote"
[ ! -e "${SHARD_ROOT}/sandboxes/${ID}/overlay/upper/root/clone-only" ] || fail "the source's layer holds what a clone wrote"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the clones changed the source's state"
say "the clones share nothing with each other or with the source, which is still stopped"

step "stop and remove the clones"
for CLONE_ID in "$@"; do
	shard stop --time "${GRACE}" "${CLONE_ID}" >/dev/null
	shard rm "${CLONE_ID}" >/dev/null
	absent "the record of clone ${CLONE_ID}" "$([ -e "${SHARD_ROOT}/sandboxes/${CLONE_ID}" ] && echo "${SHARD_ROOT}/sandboxes/${CLONE_ID}" || true)"
done
for CLONE_LINK in ${CLONE_LINKS}; do
	absent "the link ${CLONE_LINK} of a clone" "$(ip link show "${CLONE_LINK}" 2>/dev/null || true)"
done
CLONE_IDS=""
CLONE_LINKS=""
set --

step "refuse to remove the image a stopped sandbox references"
CODE=0
REFUSAL=$(shard image rm "${IMAGE}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "image rm removed the image under a stopped sandbox"
echo "${REFUSAL}" | grep -q "${ID}" || fail "image rm said '${REFUSAL}', want it to name the sandbox"
shard image ls | grep -q "${IMAGE%%:*}" || fail "image ls no longer lists the image"
say "image rm refused it and named the sandbox"

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
shard stop --time "${GRACE}" "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
say "the record says stopped"

step "remove the sandbox"
shard rm "${ID}" >/dev/null
say "rm returned"

step "grant a secret to a sandbox that was created without one"
# This sandbox is created unfronted, so the grant is what plants the placeholder, the CA and the dnat.
GRANT_ID=$(shard create "${IMAGE}" -- /bin/sh -c 'exec /bin/sleep 600')
GRANT_LINK=$(grep -o '"host_interface": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${GRANT_ID}/sandbox.json" | cut -d'"' -f4)
expect_exec_in "${GRANT_ID}" "" "the guest holds no placeholder before the grant" /bin/sh -c 'echo "$E2E_TOKEN"'
shard secret grant "${GRANT_ID}" E2E_TOKEN >/dev/null 2>&1 && fail "secret grant took a running sandbox"
say "secret grant refuses a running sandbox"

shard stop --time "${GRACE}" "${GRANT_ID}" >/dev/null
shard secret grant "${GRANT_ID}" E2E_TOKEN >/dev/null
shard inspect "${GRANT_ID}" | grep -q '"E2E_TOKEN"' || fail "inspect does not name the grant"
shard start "${GRANT_ID}" >/dev/null
expect_exec_in "${GRANT_ID}" "mock-E2E_TOKEN" "the granted guest sees the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'

GRANT_BUNDLE=$(shard exec "${GRANT_ID}" -- /bin/sh -c 'cat "$SSL_CERT_FILE"')
echo "${GRANT_BUNDLE}" | grep -q "${CA_LINE}" || fail "the late grant did not plant the proxy CA"
[ "$(echo "${GRANT_BUNDLE}" | grep -c 'BEGIN CERTIFICATE')" -gt 1 ] || fail "the late bundle holds the proxy CA alone"
say "the grant planted the proxy CA beside the image's roots"
fronted "${GRANT_ID}" || fail "the host holds no dnat to the proxy for ${GRANT_LINK}"
say "the grant turns the sandbox's 80 and 443 to the proxy"
expect_fronted "${GRANT_ID}" "the grant fronts the sandbox, and the proxy puts the value in"

step "ungrant the secret and prove the placeholder is gone"
shard stop --time "${GRACE}" "${GRANT_ID}" >/dev/null
shard secret ungrant "${GRANT_ID}" E2E_TOKEN >/dev/null
shard inspect "${GRANT_ID}" | grep -q '"E2E_TOKEN"' && fail "inspect still names the grant"
shard start "${GRANT_ID}" >/dev/null
expect_exec_in "${GRANT_ID}" "" "the guest holds no placeholder after the ungrant" /bin/sh -c 'echo "$E2E_TOKEN"'
say "ungrant took the grant and the placeholder back"

step "attach a policy to a sandbox that was created without one"
# The sandbox holds neither a policy nor a secret here, so nothing fronts it and the attach is what does.
# The dnat is the whole of fronting; the decision log is history and still holds what the grant sent.
fronted "${GRANT_ID}" && fail "an unfronted sandbox holds a dnat to the proxy"
say "a sandbox with no policy and no secret is not fronted"

# The policy names a host, which is what opens DNS: a deny-all would stop the lookup before the proxy.
shard policy create --deny "${ECHO_HOST}" --deny any e2e-attach >/dev/null

CODE=0
REFUSAL=$(shard policy attach "${GRANT_ID}" e2e-attach 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy attach took a running sandbox"
echo "${REFUSAL}" | grep -q "stop it first" || fail "policy attach said '${REFUSAL}', want the fix"
say "policy attach refuses a running sandbox"

shard stop --time "${GRACE}" "${GRANT_ID}" >/dev/null
shard policy attach "${GRANT_ID}" e2e-attach >/dev/null
shard start "${GRANT_ID}" >/dev/null
fronted "${GRANT_ID}" || fail "the attach did not turn the sandbox's 80 to the proxy"
say "the attach fronts the sandbox"

expect_exec_in "${GRANT_ID}" "403 Forbidden" "the attached policy denies the request with a 403" \
	/bin/sh -c "wget -S -O /dev/null http://${ECHO_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
DECISIONS=$(shard logs --egress "${GRANT_ID}")
PROXY_DECISIONS=$(grep '"source":"proxy"' <<<"${DECISIONS}" || true)
grep -q '"verdict":"deny"' <<<"${PROXY_DECISIONS}" || fail "the egress log holds no proxy deny for ${GRANT_ID}"
say "the deny is in the egress decision log with source proxy"

shard ls --all | awk -v id="${GRANT_ID}" '$1 == id { print $NF }' | grep -qx "e2e-attach" || fail "shard ls does not show the attached policy"
say "shard ls shows the attached policy"

CODE=0
REFUSAL=$(shard policy rm e2e-attach 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "policy rm removed a policy an attach put on a sandbox"
echo "${REFUSAL}" | grep -q "${GRANT_ID}" || fail "policy rm said '${REFUSAL}', want it to name the sandbox"
say "policy rm refuses the attached policy and names the sandbox"

step "detach the policy and prove the sandbox is not fronted any more"
shard stop --time "${GRACE}" "${GRANT_ID}" >/dev/null
shard policy detach "${GRANT_ID}" >/dev/null
shard start "${GRANT_ID}" >/dev/null
fronted "${GRANT_ID}" && fail "the detached sandbox still holds a dnat to the proxy"
shard ls --all | awk -v id="${GRANT_ID}" '$1 == id { print $NF }' | grep -qx "-" || fail "shard ls still shows a policy after the detach"
expect_exec_in "${GRANT_ID}" "reachable" "the detached guest still gets out through the NAT" \
	/bin/sh -c 'ping -c 1 -W 3 1.1.1.1 >/dev/null && echo reachable'
say "detach leaves the sandbox with no policy and takes the fronting with it"

CODE=0
shard policy attach "${GRANT_ID}" e2e-missing >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "policy attach took a policy the host does not hold"
shard inspect "${GRANT_ID}" | grep -q '"policy"' && fail "the refused attach wrote the record"
say "policy attach refuses a policy the host does not hold and writes nothing"


shard stop --time "${GRACE}" "${GRANT_ID}" >/dev/null
GRANT_ADDRESS=$(grep -o '"address": *"[^"]*"' "${SHARD_ROOT}/sandboxes/${GRANT_ID}/sandbox.json" | cut -d'"' -f4)
GRANT_ADDRESS="${GRANT_ADDRESS%%/*}"
shard rm "${GRANT_ID}" >/dev/null
# The rules are keyed by the address, so rules left here would front whoever takes that address next.
for _ in $(seq 1 10); do
	RULES=$(nft list ruleset)
	echo "${RULES}" | grep -q "${GRANT_ADDRESS}" || break
	sleep 0.1
done
echo "${RULES}" | grep -q "${GRANT_ADDRESS}" && fail "rm left the host rules of ${GRANT_ADDRESS}: $(echo "${RULES}" | grep "${GRANT_ADDRESS}")"
echo "${RULES}" | grep -q "chain egress_${GRANT_LINK}" && fail "rm left the egress chain of ${GRANT_LINK}"
say "rm took the host rules of the sandbox with it"
ip link delete "${GRANT_LINK}" >/dev/null 2>&1 || true
GRANT_ID=""
GRANT_LINK=""
say "the granted sandbox is gone"

step "remove the secret nothing holds any more"
shard secret rm E2E_TOKEN >/dev/null
shard secret rm E2E_SHAPED >/dev/null
absent "the secret file" "$([ -e "${SHARD_ROOT}/secrets/E2E_TOKEN" ] && echo "${SHARD_ROOT}/secrets/E2E_TOKEN" || true)"
say "secret rm removed the secret"

step "remove the policies nothing holds any more"
shard policy rm e2e-policy >/dev/null
shard policy rm e2e-deny-all >/dev/null
shard policy rm e2e-attach >/dev/null
absent "the policy file" "$([ -e "${SHARD_ROOT}/policies/e2e-policy.json" ] && echo "${SHARD_ROOT}/policies/e2e-policy.json" || true)"
say "policy rm removed the policies"

step "prove the host holds nothing the sandbox left"
absent "the record" "$([ -e "${SHARD_ROOT}/sandboxes/${ID}" ] && echo "${SHARD_ROOT}/sandboxes/${ID}" || true)"
absent "the address lease" "$([ -e "${LEASE}" ] && echo "${LEASE}" || true)"
absent "the namespace" "$(ip netns list | grep "^${ID}" || true)"
absent "the link" "$(ip link show "${LINK}" 2>/dev/null || true)"
absent "the address" "$(ip -o addr | grep "${ADDRESS%%/*}" || true)"
absent "the rootfs mount" "$(mount | grep "${SHARD_ROOT}/sandboxes" || true)"
absent "the runsc container" "$(ls "${SHARD_ROOT}/runsc" 2>/dev/null | grep "^${ID}" || true)"
absent "the ls --all line" "$(shard ls --all | grep "^${ID}" || true)"
absent "the cgroup" "$([ -e "/sys/fs/cgroup/shard/${ID}" ] && echo "/sys/fs/cgroup/shard/${ID}" || true)"

step "prune the image nothing references any more"
shard image prune | grep -q "${IMAGE%%:*}" || fail "image prune did not remove the image"
shard image ls | grep -q "${IMAGE%%:*}" && fail "image ls still lists the pruned image"
say "image prune removed the image once no sandbox referenced it"

step "remove the sandbox a second time"
shard rm "${ID}" >/dev/null 2>&1
say "a second rm is idempotent"

step "stop the daemon and prove the socket is gone"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
say "the socket is gone"
CODE=0
REFUSAL=$(shard ls 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "shard ls answered with the daemon stopped"
expect "${REFUSAL}" "shard: cannot connect to shard daemon at ${SOCKET}: is it running? systemctl status shard" "ls fails fast once the daemon is gone"

step "clean up"
teardown
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
say "the run's own root is gone"

trap - EXIT
echo
echo "e2e PASSED: install, daemon up, version, create, daemon restart, proxy, exec, exec again, pause, resume, fork, stop, inspect, start, grant, ungrant, rm, attach, detach, prune, daemon down, and a clean host"
