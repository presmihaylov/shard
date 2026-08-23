#!/usr/bin/env bash
# SHARD-17: the whole sandbox lifecycle on a no-KVM Linux box, from an install to a clean host.
# It installs the two binaries, creates a sandbox, execs into it twice over the same filesystem,
# stops it, removes it, and then proves the host holds nothing the sandbox left behind.
#
#   sudo ./scripts/e2e.sh
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

STEP="startup"
ID=""

# fail names the step, so a red run says what broke rather than where the shell gave up.
fail() {
	echo >&2
	echo "e2e FAILED at step: ${STEP}" >&2
	if [ -n "${1:-}" ]; then
		echo "  ${1}" >&2
	fi

	exit 1
}

trap 'fail "the command under this step exited non-zero"' ERR

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

# absent fails when the pattern is still on the host, quoting what was found.
absent() {
	local what="$1" found="$2"
	[ -z "${found}" ] || fail "${what} is still on the host: ${found}"
	say "${what} is gone"
}

step "check the host"
[ "$(id -u)" = "0" ] || fail "shard drives netns, nft and runsc, so this needs root"
for binary in runsc ip nft go; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
if [ ! -e /dev/kvm ]; then
	say "no /dev/kvm, which is the box this ticket targets"
fi
say "runsc, ip, nft and go are on the host"

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

# The whole run owns one root, and it is never the production one.
[ "${SHARD_ROOT}" != "/var/lib/shard" ] || fail "the e2e must not run against the production root"
rm -rf "${SHARD_ROOT}"

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

step "exec a command in the sandbox"
expect "$(shard exec "${ID}" -- /bin/sh -c 'echo shard-e2e > /tmp/marker; cat /tmp/marker')" \
	"shard-e2e" "the command ran and wrote a file"

step "exec again into the same filesystem state"
expect "$(shard exec "${ID}" -- /bin/cat /tmp/marker)" "shard-e2e" \
	"the second exec read what the first one wrote"

step "propagate the exit code of a command that failed"
# The || keeps the failure a condition rather than an error, which the trap above would report.
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

step "remove the sandbox a second time"
shard rm "${ID}" >/dev/null 2>&1
say "a second rm is idempotent"

step "clean up"
# runsc bind mounts a null-netns into its own root on the first create, and it belongs to no sandbox.
umount -l "${SHARD_ROOT}/runsc/null-netns" 2>/dev/null || true
rm -rf "${SHARD_ROOT}"
say "the run's own root is gone"

trap - ERR
echo
echo "e2e PASSED: install, create, exec, exec again, stop, rm, and a clean host"
