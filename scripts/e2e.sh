#!/usr/bin/env bash
# SHARD-17: the whole sandbox lifecycle on a no-KVM Linux box, from an install to a clean host.
# It installs the two binaries, creates a sandbox, execs into it twice over the same filesystem,
# pauses, resumes and forks it (SHARD-36), stops it, removes it, and then proves the host holds
# nothing the sandbox left behind.
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
FORK_ID=""
FORK_LINK=""
# The clones are space separated lists: two come off one stopped source, and both must go on teardown.
CLONE_IDS=""
CLONE_LINKS=""
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

# listed_state reads the STATE column of shard ls for one sandbox, so the check never matches the image.
listed_state() { shard ls --all | awk -v id="$1" '$1 == id { print $4 }'; }

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

# teardown gives the host back. A run that failed halfway must not leave a sandbox behind: the
# record is the only handle by which its mount and its namespace can be found again.
teardown() {
	local id link
	# shellcheck disable=SC2086 # the clone lists are meant to split
	for id in ${CLONE_IDS} "${FORK_ID}" "${ID}"; do
		[ -n "${id}" ] || continue
		shard rm --force "${id}" >/dev/null 2>&1 || true
		ip netns delete "${id}" >/dev/null 2>&1 || true
	done
	# shellcheck disable=SC2086
	for link in ${CLONE_LINKS} "${FORK_LINK}" "${LINK}"; do
		[ -n "${link}" ] || continue
		ip link delete "${link}" >/dev/null 2>&1 || true
	done

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
# The entrypoint speaks once, so logs has something to show, and then holds the sandbox up.
ID=$(shard create "${IMAGE}" -- /bin/sh -c 'echo shard-e2e-entrypoint; exec /bin/sleep 600')
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

step "reach the network from the sandbox"
expect_network "after the create"

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

step "stop the started sandbox"
shard stop --time "${GRACE}" "${ID}" >/dev/null
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
say "the record says stopped"

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
absent "the cgroup" "$([ -e "/sys/fs/cgroup/shard/${ID}" ] && echo "/sys/fs/cgroup/shard/${ID}" || true)"

step "prune the image nothing references any more"
shard image prune | grep -q "${IMAGE%%:*}" || fail "image prune did not remove the image"
shard image ls | grep -q "${IMAGE%%:*}" && fail "image ls still lists the pruned image"
say "image prune removed the image once no sandbox referenced it"

step "remove the sandbox a second time"
shard rm "${ID}" >/dev/null 2>&1
say "a second rm is idempotent"

step "clean up"
teardown
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
say "the run's own root is gone"

trap - EXIT
echo
echo "e2e PASSED: install, create, exec, exec again, pause, resume, fork, stop, inspect, start, rm, prune, and a clean host"
