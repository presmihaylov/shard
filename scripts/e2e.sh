#!/usr/bin/env bash
# SHARD-17: the whole sandbox lifecycle on a no-KVM Linux box, from an install to a clean host.
# It installs the two binaries, creates a sandbox, execs into it twice over the same filesystem,
# stops it, removes it, and then proves the host holds nothing the sandbox left behind.
#
#   sudo ./scripts/e2e.sh
#
# The run keeps its own state root, but the bridge, the subnet and the veth names belong to the
# host, so it must not run beside live sandboxes from another root. It refuses one that has any.
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
REPORTED=0

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
expect_exec() {
	local want="$1" note="$2"
	shift 2

	local got
	if ! got=$(shard exec "${ID}" -- "$@"); then
		fail "shard exec $* wrote the right bytes and then exited non-zero"
	fi

	expect "${got}" "${want}" "${note}"
}

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

# teardown gives the host back. A run that failed halfway must not leave a sandbox behind: the
# record is the only handle by which its mount and its namespace can be found again.
teardown() {
	if [ -n "${ID}" ]; then
		shard rm --force "${ID}" >/dev/null 2>&1 || true
		ip netns delete "${ID}" >/dev/null 2>&1 || true
	fi
	if [ -n "${LINK}" ]; then
		ip link delete "${LINK}" >/dev/null 2>&1 || true
	fi

	wipe_root
}

# on_exit is the one handler: it names the step that broke and then gives the host back. The step is
# named here rather than from an ERR trap, because bash runs this one first and it never returns.
on_exit() {
	local status=$?

	trap - EXIT
	if [ "${status}" -ne 0 ]; then
		report "the command under this step exited non-zero"
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
for binary in runsc ip nft go; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm, which is the box this ticket targets"
fi
say "runsc, ip, nft and go are on the host"

check_host_is_free
say "no other sandbox holds a link on this host"

# The whole run owns one root, and it is never the production one.
check_root
say "this run owns the root ${SHARD_ROOT}"

step "install shard and its guest supervisor"
if [ "${SKIP_INSTALL:-0}" = "1" ]; then
	say "skipped, running against the binaries already in ${PREFIX}"
else
	cd "$(dirname "$0")/.."
	# Build outside the tree: this runs as root, and root-owned files in a checkout are a trap.
	BUILD=$(mktemp -d)
	go build -o "${BUILD}/shard" ./cmd/shard
	# The supervisor is PID 1 in the guest, so it is static: the image may be musl or have no libc.
	CGO_ENABLED=0 go build -o "${BUILD}/shard-init" ./cmd/shard-init
	install -m0755 "${BUILD}/shard" "${PREFIX}/shard"
	install -m0755 "${BUILD}/shard-init" "${PREFIX}/shard-init"
	rm -rf "${BUILD}"
	say "installed $("${PREFIX}/shard" version) into ${PREFIX}"
fi

wipe_root

step "create a sandbox"
ID=$(shard create "${IMAGE}" -- /bin/sleep 600)
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
echo "${LISTED}" | grep -q "running" || fail "shard ls listed '${LISTED}', want it running"
say "ls shows the sandbox running on its address"

step "exec a command in the sandbox"
expect_exec "shard-e2e" "the command ran and wrote a file" \
	/bin/sh -c 'echo shard-e2e > /tmp/marker; cat /tmp/marker'

step "exec again into the same filesystem state"
expect_exec "shard-e2e" "the second exec read what the first one wrote" /bin/cat /tmp/marker

step "propagate the exit code of a command that failed"
# The || keeps the failure a condition rather than an error, which the exit handler would report.
CODE=0
shard exec "${ID}" -- /bin/sh -c 'exit 7' >/dev/null 2>&1 || CODE=$?
expect "${CODE}" "7" "a non-zero exit inside the sandbox reached this shell"

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

step "stop the sandbox a second time"
shard stop "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the second stop changed the state"
say "a second stop is idempotent"

step "remove the sandbox"
shard rm "${ID}" >/dev/null
say "rm returned"

step "prove the host holds nothing the sandbox left"
absent "the record" "$([ -e "${SHARD_ROOT}/sandboxes/${ID}" ] && echo "${SHARD_ROOT}/sandboxes/${ID}" || true)"
absent "the address lease" "$([ -e "${LEASE}" ] && echo "${LEASE}" || true)"
absent "the namespace" "$(ip netns list | grep "^${ID}" || true)"
absent "the link" "$(ip link show "${LINK}" 2>/dev/null || true)"
absent "the address" "$(ip -o addr | grep "${ADDRESS%%/*}" || true)"
absent "the rootfs mount" "$(mount | grep "${SHARD_ROOT}/sandboxes" || true)"
absent "the runsc container" "$(ls "${SHARD_ROOT}/runsc" 2>/dev/null | grep "^${ID}" || true)"
absent "the ls --all line" "$(shard ls --all | grep "^${ID}" || true)"

step "remove the sandbox a second time"
shard rm "${ID}" >/dev/null 2>&1
say "a second rm is idempotent"

step "clean up"
teardown
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
say "the run's own root is gone"

trap - EXIT
echo
echo "e2e PASSED: install, create, exec, exec again, stop, rm, and a clean host"
