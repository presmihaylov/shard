#!/usr/bin/env bash
# SHARD-268: the whole sandbox lifecycle on Firecracker, from an install to a clean host.
# It needs /dev/kvm, which no CI runner and no cloud devbox has, so nothing runs it on its own.
# docs/provider.md holds the runbook, the environment it reads, and the rule that keeps it out of CI.

# The library reads these before its own defaults, so the run never lands on the gVisor root.
export SHARD_ROOT=${SHARD_ROOT:-/var/lib/shard-fc-e2e}
export PROVIDER=firecracker

HERE=$(cd "$(dirname "$0")" && pwd)
E2E_LIB_ONLY=1
# shellcheck source=./e2e.sh
. "${HERE}/e2e.sh"
unset E2E_LIB_ONLY

MEMORY=${MEMORY:-256}
# The least memory a microVM boots with, so the fill that outgrows it is short.
OOM_MEMORY=128
# What services/provider/firecracker/memory.go gives the vmm on top of the guest's memory.
VMM_OVERHEAD_MIB=64
# The image the daemon provisions beside the root (services/datadir). check_root normalises SHARD_ROOT first, so this waits for it.
DATA_IMAGE=""
ROOT_MARKER=""
# The sandboxes the feature steps hold, so a step that fails mid-flight still gives them back.
EXIT_ID=""
OOM_ID=""

# start_daemon is the library's over firecracker, and the kernel override goes through.
start_daemon() {
	local busy
	busy=$(ss -Hltn "( sport = :${PROXY_PLAIN_PORT} or sport = :${PROXY_TLS_PORT} )" 2>/dev/null || true)
	[ -z "${busy}" ] || return 1

	[ -n "${DAEMON_LOG}" ] || DAEMON_LOG=$(mktemp)
	local trust=""
	if [ -f "${ECHO_DIR}/cert.pem" ]; then
		cat /etc/ssl/certs/ca-certificates.crt "${ECHO_DIR}/cert.pem" >"${ECHO_DIR}/trust.pem"
		trust="${ECHO_DIR}/trust.pem"
	fi
	# Emptied before the fork, so a restart never waits on the last daemon's lines; the child only appends.
	: >"${DAEMON_LOG}"
	SHARD_INIT_PATH="${PREFIX}/shard-init" SSL_CERT_FILE="${trust}" \
		SHARD_KERNEL="${SHARD_KERNEL:-}" SHARD_KERNEL_SHA256="${SHARD_KERNEL_SHA256:-}" \
		"${PREFIX}/shard" --root "${SHARD_ROOT}" daemon --provider firecracker >>"${DAEMON_LOG}" 2>&1 &
	DAEMON_PID=$!
	wait_for_daemon
}

# fstab_line is what pkg/xfs appendFstab writes, byte for byte, so the teardown removes that line and no other.
fstab_line() { printf '%s %s xfs loop,nofail 0 0' "${DATA_IMAGE}" "${SHARD_ROOT}"; }

# forget_fstab drops the run's own line through a temp file in /etc, so an interrupt never leaves a half-written fstab.
forget_fstab() {
	local kept status tmp
	[ -e /etc/fstab ] || return 0
	grep -qxF -- "$(fstab_line)" /etc/fstab && status=0 || status=$?
	# Only exit 1 means the line is absent; anything above it is a read that failed, and a failed read must never pass for a clean file.
	[ "${status}" -le 1 ] || fail "read /etc/fstab: grep exited ${status}"
	[ "${status}" = "0" ] || return 0
	kept=$(grep -vxF -- "$(fstab_line)" /etc/fstab) && status=0 || status=$?
	[ "${status}" -le 1 ] || fail "read /etc/fstab: grep exited ${status}"
	tmp=$(mktemp /etc/fstab.e2e-fc.XXXXXX)
	chmod --reference=/etc/fstab "${tmp}"
	chown --reference=/etc/fstab "${tmp}"
	if [ -n "${kept}" ]; then
		printf '%s\n' "${kept}" >"${tmp}"
	fi
	mv "${tmp}" /etc/fstab
}

# own_root refuses anything wipe_root would delete and this run did not make, because check_root guards only / and the production root.
own_root() {
	if [ -f "${ROOT_MARKER}" ]; then
		return 0
	fi
	local leftover
	for leftover in "${DATA_IMAGE}" "${DATA_IMAGE}.part" "${DATA_IMAGE}.lock"; do
		if [ -e "${leftover}" ]; then
			fail "${leftover} is on this host and ${ROOT_MARKER} does not exist: this run deletes nothing it did not make, so name a root whose siblings are free"
		fi
	done
	[ -e "${SHARD_ROOT}" ] || return 0
	[ -d "${SHARD_ROOT}" ] || fail "${SHARD_ROOT} is not a directory: name a root of this suite's own"
	[ -z "$(/bin/ls -A "${SHARD_ROOT}")" ] || fail "${SHARD_ROOT} holds files and ${ROOT_MARKER} does not exist: this run deletes no directory it did not make, so name an empty or absent root"
}

# wipe_root is the library's plus what the firecracker daemon put beside the root and on the host: the mount over it, the image, its fstab line, the bridge and the policy tables.
wipe_root() {
	unmount_under "${SHARD_ROOT}"
	umount -l "${SHARD_ROOT}" >/dev/null 2>&1 || true
	rm -rf "${SHARD_ROOT}" || true
	rm -f "${DATA_IMAGE}" "${DATA_IMAGE}.part" "${DATA_IMAGE}.lock"
	forget_fstab
	clear_host_net
	drop_cgroup_parent
}

# drop_cgroup_parent removes the shard cgroup parent when this run made it and no sandbox of any root is under it.
drop_cgroup_parent() {
	[ "${CGROUP_PARENT_BEFORE:-yes}" = no ] && [ -d "${CGROUP_PARENT}" ] && [ -z "$(shard_cgroups)" ] || return 0
	rmdir "${CGROUP_PARENT}" || echo "teardown: could not remove the cgroup parent ${CGROUP_PARENT}" >&2
}

# vmm_pids lists every firecracker process jailed under this root by the pid file and inode of its jail, since the jailer's own mount namespace makes /proc/<pid>/root read /.
vmm_pids() {
	local jail ino pid jails=" "
	{
		for jail in "${SHARD_ROOT}"/jail/firecracker/*/root; do
			[ -d "${jail}" ] || continue
			ino="$(stat -c %d:%i "${jail}" 2>/dev/null || true)"
			if [ -n "${ino}" ]; then jails+="${ino} "; fi
			pid="$(cat "${jail}/firecracker.pid" 2>/dev/null || true)"
			if [ -n "${pid}" ] && [ "$(cat "/proc/${pid}/comm" 2>/dev/null || true)" = firecracker ]; then echo "${pid}"; fi
		done
		for pid in $(pgrep -x firecracker || true); do
			ino="$(stat -L -c %d:%i "/proc/${pid}/root/" 2>/dev/null || true)"
			if [ -n "${ino}" ] && [[ "${jails}" == *" ${ino} "* ]]; then echo "${pid}"; fi
		done
	} | sort -u
}

# started_vmms lists every firecracker process the host did not run when this run began, which a vmm whose jail is gone still is.
started_vmms() {
	local pid
	for pid in $(pgrep -x firecracker || true); do
		if [[ "${FIRECRACKERS_BEFORE}" != *" ${pid} "* ]]; then echo "${pid}"; fi
	done
}

# vmm_status reads one field of the vmm's /proc status.
vmm_status() { awk -v field="$2:" '$1 == field { print $2 }' "/proc/$1/status"; }

CGROUP_PARENT=/sys/fs/cgroup/shard

# shard_cgroups lists the sandbox cgroups under the shard parent, which the daemon of every root on the host shares.
shard_cgroups() { find "${CGROUP_PARENT}" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort || true; }

# run_cgroups lists the sandbox cgroups this run added to the ones the host held before it.
run_cgroups() { comm -13 <(printf '%s\n' "${CGROUPS_BEFORE}") <(shard_cgroups) | tr '\n' ' '; }

# record_field reads one string field of a sandbox record.
record_field() { grep -o "\"$2\": *\"[^\"]*\"" "${SHARD_ROOT}/sandboxes/$1/sandbox.json" | cut -d'"' -f4; }

# record_pid reads the pid the record holds, which on firecracker is the vmm.
record_pid() { grep -o '"pid": *[0-9]*' "${SHARD_ROOT}/sandboxes/$1/sandbox.json" | grep -o '[0-9]*$'; }

step "check the host"
[ "$(id -u)" = "0" ] || fail "shard drives /dev/kvm, a tap and nft, so this needs root"
[ -e /dev/kvm ] || fail "no /dev/kvm on this host: firecracker needs bare metal, rent one and run this there"
for binary in firecracker jailer mkfs.erofs mkfs.xfs ip ss nft go curl openssl; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
say "/dev/kvm, firecracker, jailer, mkfs.erofs, mkfs.xfs, ip, ss, nft, go, curl and openssl are on the host"
if [ -n "${SHARD_KERNEL:-}" ]; then
	[ -f "${SHARD_KERNEL}" ] || fail "SHARD_KERNEL names ${SHARD_KERNEL}, which is not a file"
	[ -n "${SHARD_KERNEL_SHA256:-}" ] || fail "SHARD_KERNEL is set and SHARD_KERNEL_SHA256 is not: the daemon refuses one without the other"
	say "the daemon boots the guest kernel at ${SHARD_KERNEL}"
else
	say "the daemon fetches the guest kernel release by checksum under its root"
fi

# The teardown deletes host-wide network state, so a daemon on another root would lose its bridge and its policy to this run.
daemons=$(pgrep -x shard || true)
[ -z "${daemons}" ] || fail "a shard daemon is already running on this host (pid ${daemons}): it shares the bridge ${HOST_BRIDGE} and the shard nft tables with this run, so stop it or wait for it"
say "no shard daemon holds the host-wide bridge and tables"

check_host_is_free
say "no other sandbox holds a link on this host"
[ -z "$(vmm_pids)" ] || fail "a firecracker process already drives ${SHARD_ROOT}: $(vmm_pids)"
FIRECRACKERS_BEFORE=" $(pgrep -x firecracker | tr '\n' ' ' || true)"
CGROUPS_BEFORE=$(shard_cgroups)
CGROUP_PARENT_BEFORE=$([ -d "${CGROUP_PARENT}" ] && echo yes || echo no)

check_root
DATA_IMAGE="${SHARD_ROOT}.xfs"
ROOT_MARKER="${SHARD_ROOT}.e2e-owned"
own_root
say "this run owns the root ${SHARD_ROOT} and the image ${DATA_IMAGE}"

# The marker outlives a crashed run, so the next one recognises its own root; only now may the teardown delete anything.
: >"${ROOT_MARKER}"
trap on_exit EXIT

step "install shard and its guest supervisor"
cd "${HERE}/.."
if [ "${SKIP_INSTALL:-0}" = "1" ]; then
	say "skipped, running against the binaries already in ${PREFIX}"
else
	BUILD=$(mktemp -d)
	go build -o "${BUILD}/shard" ./cmd/shard
	CGO_ENABLED=0 go build -o "${BUILD}/shard-init" ./cmd/shard-init
	install -m0755 "${BUILD}/shard" "${PREFIX}/shard"
	install -m0755 "${BUILD}/shard-init" "${PREFIX}/shard-init"
	rm -rf "${BUILD}"
	say "installed shard and shard-init into ${PREFIX}"
fi

wipe_root
mkdir -p "${SHARD_ROOT}"

step "start the echo the fronted sandbox talks to"
start_echo
say "the echo answers on ${ECHO_ADDRESS} in the netns ${ECHO_NETNS}, ports 80 and 443, as ${ECHO_HOST} and ${OTHER_HOST}"

step "start the daemon over a root it turns into an xfs image"
SOCKET="${SHARD_ROOT}/shard.sock"
start_daemon || fail "the daemon did not come up"
[ -S "${SOCKET}" ] || fail "no socket at ${SOCKET}"
say "the daemon logged: $(grep 'api listening on' "${DAEMON_LOG}" | sed 's/.*api/api/')"
expect "$(findmnt -no FSTYPE "${SHARD_ROOT}")" "xfs" "the root is an xfs mount"
findmnt -no SOURCE "${SHARD_ROOT}" | has_line '^/dev/loop' || fail "the root is mounted from $(findmnt -no SOURCE "${SHARD_ROOT}"), want a loop device"
[ -f "${DATA_IMAGE}" ] || fail "there is no image at ${DATA_IMAGE}"
grep -qxF -- "$(fstab_line)" /etc/fstab || fail "/etc/fstab holds no line for ${SHARD_ROOT}"
grep -q "with reflink" "${DAEMON_LOG}" || fail "the daemon did not log the data dir bootstrap"
say "the daemon provisioned ${DATA_IMAGE} over ${SHARD_ROOT}, with reflink, and a line in /etc/fstab"

step "read the daemon status"
STATUS_OUT=$(shard daemon status)
status_field() { echo "${STATUS_OUT}" | awk -v name="$1" '$1 == name { print $2 }'; }
expect "$(status_field pid)" "${DAEMON_PID}" "the status names the pid of the daemon this run started"
expect "$(status_field provider)" "firecracker" "the status names firecracker"
expect "$(status_field pause) $(status_field resume) $(status_field fork)" "true true true" "firecracker pauses, resumes and forks a running sandbox (SHARD-462)"

step "store a secret and a policy"
SECRET_VALUE="fc-e2e-secret-value-$$-$(date +%s)"
printf '%s\n' "${SECRET_VALUE}" | shard secret set --destination "${ECHO_HOST}" E2E_TOKEN >/dev/null
SHAPED_PLACEHOLDER="sk_test_e2eplaceholder01"
SHAPED_VALUE="sk_live_e2e_$$_$(date +%s)"
shard secret set --destination "${ECHO_HOST}" --placeholder "${SHAPED_PLACEHOLDER}" E2E_SHAPED "${SHAPED_VALUE}" >/dev/null 2>&1
holds "E2E_TOKEN" shard secret list || fail "shard secret list does not list E2E_TOKEN"
holds "${SECRET_VALUE}" shard secret list && fail "shard secret list printed the value"
say "secret list lists the name and not the value"
# The probe address is allowed on every protocol, so the ping the network checks use goes through.
shard policy create --allow 1.1.1.1 --allow dns --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
holds "e2e-policy" shard policy list || fail "shard policy list does not list e2e-policy"
say "the policy allows the probe, dns and the two echo names, and denies the rest"

step "size a create with no memory"
# A microVM has no unbounded memory, so the daemon gives a create with no --memory 512 MiB (SHARD-761).
DEFAULT_ID=$(shard create "${IMAGE}")
[ -n "${DEFAULT_ID}" ] || fail "create with no --memory printed no id"
expect "$(grep -o '"memory_mib": *[0-9]*' "${SHARD_ROOT}/sandboxes/${DEFAULT_ID}/sandbox.json" | grep -o '[0-9]*$')" "512" "the record of a create with no --memory holds 512 MiB"
shard remove --force "${DEFAULT_ID}" >/dev/null

step "run a microVM detached"
create_it() { ID=$(shard run -d --memory "${MEMORY}MiB" --secret E2E_TOKEN --secret E2E_SHAPED --policy e2e-policy "${IMAGE}" /bin/sh -c 'echo shard-e2e-entrypoint; exec /bin/sleep 600'); }
timed "run -d" create_it
[ -n "${ID}" ] || fail "run -d printed no id"
say "run -d printed the id ${ID}"
RECORD="${SHARD_ROOT}/sandboxes/${ID}/sandbox.json"
[ -f "${RECORD}" ] || fail "there is no record at ${RECORD}"
ADDRESS=$(record_field "${ID}" address)
LINK=$(record_field "${ID}" host_interface)
VMM_PID=$(record_pid "${ID}")
say "the record holds the address ${ADDRESS} on the link ${LINK}, driven by pid ${VMM_PID}"
expect "$(ps -o comm= -p "${VMM_PID}" | tr -d ' ')" "firecracker" "the pid in the record is a firecracker process"
JAIL="${SHARD_ROOT}/jail/firecracker/${ID}/root"
expect "$(stat -L -c %d:%i "/proc/${VMM_PID}/root/")" "$(stat -c %d:%i "${JAIL}")" "the vmm runs chrooted in its jail"
expect "$(cat "${JAIL}/firecracker.pid")" "${VMM_PID}" "the pid file in the jail names the vmm"
VMM_UID=$(vmm_status "${VMM_PID}" Uid)
[ "${VMM_UID}" -ge 1879048192 ] || fail "the vmm runs as uid ${VMM_UID}, want one from 0x70000000 up"
expect "$(vmm_status "${VMM_PID}" CapEff) $(vmm_status "${VMM_PID}" Seccomp)" "0000000000000000 2" "the vmm runs as uid ${VMM_UID}, with no capability, under its seccomp filter"
# The vmm opens its tap inside the sandbox's netns, so neither of them is on the host (SHARD-431).
[ -e "/run/netns/${ID}" ] || fail "there is no namespace named ${ID} for the vmm to join"
VMM_NETNS=$(stat -L -c %d:%i "/proc/${VMM_PID}/ns/net")
expect "${VMM_NETNS}" "$(stat -L -c %d:%i "/run/netns/${ID}")" "the vmm runs in the namespace ${ID}"
[ "${VMM_NETNS}" != "$(stat -L -c %d:%i /proc/1/ns/net)" ] || fail "the vmm ${VMM_PID} runs in the host's network namespace"
HOST_PORT=$(ip -details -o link show "${LINK}")
grep -q "veth" <<<"${HOST_PORT}" || fail "the host link ${LINK} is not a veth: ${HOST_PORT}"
grep -q "master ${HOST_BRIDGE}" <<<"${HOST_PORT}" || fail "the host link ${LINK} is not a port of ${HOST_BRIDGE}: ${HOST_PORT}"
grep -q "tun type tap" <<<"${HOST_PORT}" && fail "the host link ${LINK} is a tap: ${HOST_PORT}"
NETNS_TAP=$(ip -netns "${ID}" -details -o link show "${LINK}" 2>&1 || true)
grep -q "tun type tap" <<<"${NETNS_TAP}" || fail "the namespace ${ID} holds no tap ${LINK}: ${NETNS_TAP}"
grep -q "master br0" <<<"${NETNS_TAP}" || fail "the tap ${LINK} is not a port of br0 in ${ID}: ${NETNS_TAP}"
say "the host link ${LINK} is a veth port of ${HOST_BRIDGE}, and the vmm and its tap are in the namespace ${ID}"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard list lists the sandbox $(listed_state "${ID}"), want running"
holds "${IMAGE%%:*}" shard image list || fail "image list does not list ${IMAGE}"
say "list shows the sandbox running, and the image is cached"

step "bound the vmm in a host cgroup of its own"
CGROUP="/sys/fs/cgroup/shard/${ID}"
[ -d "${CGROUP}" ] || fail "there is no cgroup at ${CGROUP}"
expect "$(cat "${CGROUP}/memory.max")" "$(((MEMORY + VMM_OVERHEAD_MIB) * 1048576))" "memory.max is the guest's ${MEMORY} MiB plus ${VMM_OVERHEAD_MIB} MiB for the vmm"
expect "$(cat "${CGROUP}/memory.swap.max")" "0" "memory.swap.max is 0, so the host never swaps the guest out"
expect "$(cat "${CGROUP}/memory.oom.group")" "1" "memory.oom.group is 1, so a host OOM kill takes the whole vmm"
grep -qx "${VMM_PID}" "${CGROUP}/cgroup.procs" || fail "the vmm ${VMM_PID} is not in ${CGROUP}, which holds: $(cat "${CGROUP}/cgroup.procs")"
say "the vmm ${VMM_PID} runs in ${CGROUP}"

step "read the output of the entrypoint"
for _ in $(seq 1 50); do
	holds "shard-e2e-entrypoint" shard logs "${ID}" && break
	sleep 0.2
done
holds "shard-e2e-entrypoint" shard logs "${ID}" || fail "shard logs does not show what the entrypoint wrote"
say "logs shows what the entrypoint wrote"

step "exec a command in the microVM"
expect_exec "shard-e2e" "the command ran and wrote a file" /bin/sh -c 'echo shard-e2e > /tmp/marker; cat /tmp/marker'
expect_exec "shard-e2e" "the second exec read what the first one wrote" /bin/cat /tmp/marker
expect_exec "pong" "a listener on 127.0.0.1 answers, so lo is up" \
	/bin/sh -c '(echo pong | nc -l -p 7077 -s 127.0.0.1 -w 3 &); sleep 1; nc -w 3 127.0.0.1 7077 </dev/null'
expect_exec "kept" "a file lands on the overlay disk, which a stop keeps" /bin/sh -c 'echo kept > /root/kept; cat /root/kept'
CODE=0
shard exec "${ID}" /bin/sh -c 'exit 7' >/dev/null 2>&1 || CODE=$?
expect "${CODE}" "7" "exec carries the guest's exit code"
expect "$(printf 'over vsock\n' | shard exec -i "${ID}" /bin/cat)" "over vsock" "exec carries stdin in"
expect_exec "1" "the entrypoint is a child of PID 1, which is shard-init" /bin/sh -c 'awk '"'"'$2 == "(sleep)" { print $4 }'"'"' /proc/[0-9]*/stat'

step "run exits with the app's code, and the microVM outlives the app"
CODE=0
OUT=$(shard run --memory "${MEMORY}MiB" --name e2e-exited "${IMAGE}" /bin/sh -c 'echo e2e-run-out; exit 3') || CODE=$?
EXIT_ID=$(id_of e2e-exited)
expect "${CODE}" "3" "run exits with the app's code"
expect "${OUT}" "e2e-run-out" "run prints the app output once"
# The liveness tick writes the exit into the record, seconds after run returns (SHARD-479).
for _ in $(seq 1 50); do
	grep -q '"exit_status"' "${SHARD_ROOT}/sandboxes/${EXIT_ID}/sandbox.json" && break
	sleep 0.2
done
grep -q '"exit_status"' "${SHARD_ROOT}/sandboxes/${EXIT_ID}/sandbox.json" || fail "the record of ${EXIT_ID} never took the app's exit"
expect "$(listed_state "${EXIT_ID}")" "running" "the sandbox is running after its app exited 3"
expect_exec_in "${EXIT_ID}" "still-up" "an exec answers in a sandbox whose entrypoint is gone" /bin/echo still-up
shard stop "${EXIT_ID}" >/dev/null
shard remove "${EXIT_ID}" >/dev/null
EXIT_ID=""
say "only stop ended it"

step "a microVM that outgrows its memory stops with its reason, and nothing starts it again"
# Only the first boot fills: the marker is on the overlay disk, and the sync keeps it through the stop that follows the OOM.
OOM_ID=$(shard run -d --memory "${OOM_MEMORY}MiB" --name e2e-oom "${IMAGE}" /bin/sh -c \
	'if [ ! -e /root/ran ]; then touch /root/ran && sync && mount -o remount,size=1G /dev/shm && dd if=/dev/zero of=/dev/shm/fill bs=1M; fi; echo e2e-oom-settled; while true; do sleep 1; done')
OOM_RECORD="${SHARD_ROOT}/sandboxes/${OOM_ID}/sandbox.json"
for _ in $(seq 1 120); do
	grep -q '"state": *"stopped"' "${OOM_RECORD}" && break
	sleep 1
done
grep -q '"state": *"stopped"' "${OOM_RECORD}" || fail "${OOM_ID} never stopped after its OOM: $(cat "${OOM_RECORD}")"
grep -q '"stopped_reason": *"ran out of memory and the host ended it"' "${OOM_RECORD}" || fail "the stop of ${OOM_ID} names no OOM: $(cat "${OOM_RECORD}")"
grep -q '"pid": *0' "${OOM_RECORD}" || fail "the stopped ${OOM_ID} still names a pid: $(cat "${OOM_RECORD}")"
grep -q "sandbox ${OOM_ID}: ran out of memory and the host ended it, the record now says stopped$" "${DAEMON_LOG}" || fail "the daemon log holds no OOM stop of ${OOM_ID}"
# Two liveness ticks pass, and the record still says stopped.
sleep 11
grep -q '"state": *"stopped"' "${OOM_RECORD}" || fail "something started ${OOM_ID} again after its OOM: $(cat "${OOM_RECORD}")"
say "the daemon read the end as an OOM, not a crash, and left the microVM stopped"

step "start brings the microVM back over its kept files"
shard start "${OOM_ID}" >/dev/null
for _ in $(seq 1 50); do
	holds "e2e-oom-settled" shard logs "${OOM_ID}" && break
	sleep 0.2
done
holds "e2e-oom-settled" shard logs "${OOM_ID}" || fail "the second boot of ${OOM_ID} did not get past the fill: $(shard logs "${OOM_ID}")"
expect_exec_in "${OOM_ID}" "alive" "an exec answers in the microVM a start brought back" /bin/echo alive
shard remove --force "${OOM_ID}" >/dev/null
OOM_ID=""
say "one OOM, one stop, and the boot a start brought back skipped the fill"

step "reach the network from the microVM"
expect_network "after the create"
expect_exec "resolved" "the guest resolves a name through the daemon's resolver" \
	/bin/sh -c "timeout 5 nslookup ${OTHER_HOST} >/dev/null 2>&1 && echo resolved"

step "enforce the egress policy on the link"
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK}"
nft list table bridge shard | grep -c "iifname \"${LINK}\"" >/dev/null || fail "the host does not pin the address of ${LINK}"
say "the host holds a chain for the link and pins its address"
expect_blocked "${ID}" "the guest cannot reach an address the policy denies"
shard policy create --allow 1.1.1.1 --allow 8.8.8.8 --allow dns --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_exec "reachable" "the same address answers once a rule allows it" \
	/bin/sh -c 'ping -c 1 -W 3 8.8.8.8 >/dev/null && echo reachable'
shard policy create --allow 1.1.1.1 --allow dns --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
expect_blocked "${ID}" "the deny holds again the moment the policy is stored"
expect_exec "blocked" "the floor holds under the policy: the metadata address is dropped" \
	/bin/sh -c 'ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 && echo reachable || echo blocked'
EGRESS=""
for _ in $(seq 1 30); do
	EGRESS=$(shard policy logs "${ID}")
	has_line '"source":"host"' '"verdict":"deny"' <<<"${EGRESS}" && break
	sleep 0.2
done
has_line '"source":"host"' '"verdict":"deny"' <<<"${EGRESS}" || fail "the egress log holds no host deny: ${EGRESS}"
say "the egress log carries the host's deny"

step "front the microVM through the proxy"
expect_exec "mock-E2E_TOKEN" "the guest sees the placeholder as \$E2E_TOKEN" /bin/sh -c 'echo "$E2E_TOKEN"'
expect_exec "${SHAPED_PLACEHOLDER}" "the guest sees the chosen placeholder as \$E2E_SHAPED" /bin/sh -c 'echo "$E2E_SHAPED"'
fronted "${ID}" || fail "the host holds no dnat to the proxy for ${ADDRESS}"
say "the host turns the guest's 80 and 443 to the proxy"
expect_fronted "${ID}" "an https request to the granted host carries the value, so the guest trusts the proxy CA"
if ! GOT=$(fetch "${ID}" http "${ECHO_HOST}"); then
	fail "the http request to ${ECHO_HOST} failed"
fi
grep -qx "authorization=$(seen "Bearer mock-E2E_TOKEN")" <<<"${GOT}" || fail "the echo saw '${GOT}' over plain http, want the placeholder, never the value in cleartext"
say "plain http to the granted host keeps the placeholder: the value goes out over tls alone"
if ! GOT=$(fetch "${ID}" https "${OTHER_HOST}"); then
	fail "the request to ${OTHER_HOST} failed"
fi
grep -qx "authorization=$(seen "Bearer mock-E2E_TOKEN")" <<<"${GOT}" || fail "the echo saw '${GOT}' on the ungranted host, want the placeholder"
say "a request to a host the policy allows but the grant does not keeps the placeholder"
expect_exec "403 Forbidden" "a request to a host no rule allows is refused at the proxy" \
	/bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
expect_env_clean "${ID}"
absent "the value in the daemon log" "$(grep -l "${SECRET_VALUE}" "${DAEMON_LOG}" || true)"
absent "the value outside the store" "$(grep -rl --exclude-dir=secrets "${SECRET_VALUE}" "${SHARD_ROOT}" 2>/dev/null || true)"

step "restart the daemon and prove the microVM and the guest's own processes outlive it"
# An attached exec dies with the daemon here: its stream is the vsock, and shard-init reaps the child when it closes.
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
kill -0 "${VMM_PID}" 2>/dev/null || fail "the vmm ${VMM_PID} died with the daemon"
say "the daemon is down and the vmm ${VMM_PID} is still up"
start_daemon || fail "the daemon did not come up"
expect "$(listed_state "${ID}")" "running" "the new daemon adopted the vmm by its socket"
expect "$(record_pid "${ID}")" "${VMM_PID}" "the record still names the same vmm"
expect_exec "restarted" "an exec answers after the daemon restart" /bin/echo restarted
expect_exec "alive" "the entrypoint process in the guest outlived the daemon" \
	/bin/sh -c 'pgrep -f "[s]leep 600" >/dev/null && echo alive'
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK} after the daemon restart"
expect_fronted "${ID}" "the proxy fronts the sandbox after the daemon restart"
grep -q "with reflink" "${DAEMON_LOG}" && fail "the second daemon provisioned the data dir again"
say "the second daemon found the xfs mount and provisioned nothing"

step "reconcile a microVM the host lost while the daemon was down"
RECONCILE_ID=$(shard run -d --memory "${MEMORY}MiB" --name e2e-lost "${IMAGE}" /bin/sleep 600)
RECONCILE_LINK=$(record_field "${RECONCILE_ID}" host_interface)
RECONCILE_PID=$(record_pid "${RECONCILE_ID}")
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
kill -9 "${RECONCILE_PID}" 2>/dev/null || true
for _ in $(seq 1 50); do
	kill -0 "${RECONCILE_PID}" 2>/dev/null || break
	sleep 0.1
done
kill -0 "${RECONCILE_PID}" 2>/dev/null && fail "the vmm ${RECONCILE_PID} survived the kill"
start_daemon || fail "the daemon did not come up"
expect "$(listed_state "${RECONCILE_ID}")" "stopped" "the daemon corrected the record of the microVM it lost"
holds "^${RECONCILE_ID}.*daemon restarted and found no process" shard list --all || fail "shard list gives no reason for ${RECONCILE_ID}"
shard remove --force "${RECONCILE_ID}" >/dev/null || fail "remove did not free the microVM the host lost"
ip link delete "${RECONCILE_LINK}" >/dev/null 2>&1 || true
RECONCILE_ID=""
RECONCILE_LINK=""
say "list gives the reason, and remove freed what it left on the host"

checkpoint_steps
# The resume brought the microVM up in a fresh vmm, so the stop and the start below read that one.
VMM_PID=$(record_pid "${ID}")
expect "$(ps -o comm= -p "${VMM_PID}" | tr -d ' ')" "firecracker" "a fresh vmm ${VMM_PID} drives the resumed microVM"

step "stop the microVM"
stop_it() { shard stop "${ID}" >/dev/null; }
timed "stop" stop_it
grep -q '"state": *"stopped"' "${RECORD}" || fail "the record does not say stopped"
for _ in $(seq 1 50); do
	kill -0 "${VMM_PID}" 2>/dev/null || break
	sleep 0.1
done
absent "the vmm ${VMM_PID}" "$(kill -0 "${VMM_PID}" 2>/dev/null && echo "${VMM_PID}" || true)"
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the stop dropped the address"
LEASE="${SHARD_ROOT}/network/leases/${ADDRESS%%/*}"
grep -qx "${ID}" "${LEASE}" || fail "the stop dropped the address lease"
say "the record says stopped and keeps the address and its lease"
[ -d "${CGROUP}" ] || fail "the stop removed ${CGROUP}, which the next start boots into"
expect "$(cat "${CGROUP}/cgroup.procs")" "" "the cgroup stays, empty, for the next start"
holds "^${ID}" shard list && fail "shard list still lists the stopped sandbox"
expect "$(listed_state "${ID}")" "stopped" "list hides the stopped sandbox and list --all shows it stopped"
holds "shard-e2e-entrypoint" timeout 10 "${PREFIX}/shard" --root "${SHARD_ROOT}" logs -f "${ID}" || fail "shard logs -f on a stopped sandbox did not print its output and end"
say "logs still reads a stopped sandbox, and -f ends on its own"
shard stop "${ID}" >/dev/null
say "a second stop is idempotent"

step "snapshot the stopped microVM, by reflink"
timed "snapshot create" snapshot_it
[ -n "${SNAPSHOT_ID}" ] || fail "snapshot create printed no id"
SNAPSHOT_DIR="${SHARD_ROOT}/snapshots/${SNAPSHOT_ID}"
[ -f "${SNAPSHOT_DIR}/files/overlay.raw" ] || fail "the snapshot holds no overlay disk at ${SNAPSHOT_DIR}/files/overlay.raw"
holds "e2e-snapshot" shard snapshot list || fail "snapshot list does not list e2e-snapshot"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the snapshot changed the source's state"
say "snapshot create printed ${SNAPSHOT_ID}, and the snapshot holds the overlay disk"

step "create two microVMs from the snapshot"
timed "create --snapshot" seed_it e2e-seeded-1
timed "create --snapshot" seed_it e2e-seeded-2
# shellcheck disable=SC2086 # the seeded list is meant to split
set -- ${SEEDED_IDS}
[ "$#" = "2" ] && [ "$1" != "$2" ] && [ "$1" != "${ID}" ] && [ "$2" != "${ID}" ] || fail "create --snapshot printed '${SEEDED_IDS}', want two new ids"
N=0
for SEEDED_ID in "$@"; do
	N=$((N + 1))
	SEEDED_ADDRESS=$(record_field "${SEEDED_ID}" address)
	SEEDED_LINKS="${SEEDED_LINKS} $(record_field "${SEEDED_ID}" host_interface)"
	[ "${SEEDED_ADDRESS}" != "${ADDRESS}" ] || fail "sandbox ${SEEDED_ID} got the source's address ${ADDRESS}"
	expect "$(listed_state "${SEEDED_ID}")" "running" "sandbox ${SEEDED_ID} runs on its own address ${SEEDED_ADDRESS}"
	expect "$(record_field "${SEEDED_ID}" snapshot)" "${SNAPSHOT_ID}" "sandbox ${SEEDED_ID} names the snapshot it came from"
	# A snapshot runs shard-init alone, so the source's app never prints its banner again.
	expect "$(shard logs "${SEEDED_ID}" | grep -c "shard-e2e-entrypoint")" "0" "sandbox ${SEEDED_ID} ran no entrypoint"
	holds '"exit_status"' shard inspect "${SEEDED_ID}" && fail "sandbox ${SEEDED_ID} carries the source's exit status"
	expect_exec_in "${SEEDED_ID}" "kept" "sandbox ${SEEDED_ID} holds the file the source wrote before the stop" /bin/cat /root/kept
	expect_exec_in "${SEEDED_ID}" "e2e-seeded-${N}" "sandbox ${SEEDED_ID} carries its own hostname" /bin/hostname
	expect_exec_in "${SEEDED_ID}" "mock-E2E_TOKEN" "sandbox ${SEEDED_ID} holds the placeholder its create granted" /bin/sh -c 'echo "$E2E_TOKEN"'
	expect_blocked "${SEEDED_ID}" "the policy holds on sandbox ${SEEDED_ID}"
	expect_fronted "${SEEDED_ID}" "the proxy fronts sandbox ${SEEDED_ID}"
done
shard exec "$1" /bin/sh -c 'echo seeded-only > /root/seeded-only' >/dev/null
CODE=0
shard exec "$2" /bin/cat /root/seeded-only >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "sandbox $2 sees the file sandbox $1 wrote"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the seeded microVMs changed the source's state"
say "the seeded microVMs share nothing with each other or with the source, which is still stopped"

step "grow a larger --disk from the snapshot, and refuse a smaller one by name"
SNAPSHOT_DISK=$(shard snapshot inspect "${SNAPSHOT_ID}" | grep -o '"disk_mib": *[0-9]*' | grep -o '[0-9]*$')
CODE=0
REFUSAL=$(shard create --disk "$((SNAPSHOT_DISK - 128))MiB" --snapshot "${SNAPSHOT_ID}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "create --snapshot took a --disk under the snapshot's disk"
grep -q -- "a disk only grows" <<<"${REFUSAL}" || fail "create --snapshot --disk said '${REFUSAL}', want it to say a disk only grows"
say "firecracker refused the smaller disk by name"
GROWN_DISK=$((SNAPSHOT_DISK + 1024))
GROWN_ID=$(shard create --name e2e-seeded-grown --disk "${GROWN_DISK}MiB" --snapshot "${SNAPSHOT_ID}")
SEEDED_IDS="${SEEDED_IDS} ${GROWN_ID}"
SEEDED_LINKS="${SEEDED_LINKS} $(record_field "${GROWN_ID}" host_interface)"
expect "$(grep -o '"disk_mib": *[0-9]*' "${SHARD_ROOT}/sandboxes/${GROWN_ID}/sandbox.json" | grep -o '[0-9]*$')" "${GROWN_DISK}" "the record carries the ${GROWN_DISK} MiB disk"
expect_exec_in "${GROWN_ID}" "kept" "the grown disk holds the file the source wrote before the stop" /bin/cat /root/kept
GUEST_MIB=$(shard exec "${GROWN_ID}" /bin/df -m /root | awk 'NR == 2 { print $2 }')
# The filesystem's own metadata takes a few percent of the disk.
[ "$((GUEST_MIB * 10))" -ge "$((GROWN_DISK * 9))" ] || fail "the guest sees ${GUEST_MIB} MiB on /root, want most of ${GROWN_DISK}"
# shellcheck disable=SC2086 # the seeded list is meant to split
set -- ${SEEDED_IDS}
say "the guest sees ${GUEST_MIB} MiB on a disk the snapshot made at ${SNAPSHOT_DISK} MiB"

step "stop and remove the seeded microVMs and the snapshot"
for SEEDED_ID in "$@"; do
	shard stop "${SEEDED_ID}" >/dev/null
	shard remove "${SEEDED_ID}" >/dev/null
done
SEEDED_IDS=""
SEEDED_LINKS=""
absent "a seeded record" "$(shard list --all | grep e2e-seeded || true)"
shard snapshot remove e2e-snapshot >/dev/null
absent "the snapshot ${SNAPSHOT_ID}" "$([ -e "${SNAPSHOT_DIR}" ] && echo "${SNAPSHOT_DIR}" || true)"
SNAPSHOT_ID=""

step "start the microVM again"
start_it() { shard start "${ID}" >/dev/null; }
timed "start" start_it
expect "$(listed_state "${ID}")" "running" "the record says running again"
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the start changed the address"
NEW_VMM_PID=$(record_pid "${ID}")
[ "${NEW_VMM_PID}" != "${VMM_PID}" ] || fail "the start reused the pid of the stopped vmm"
expect "$(ps -o comm= -p "${NEW_VMM_PID}" | tr -d ' ')" "firecracker" "a new vmm ${NEW_VMM_PID} drives the same address"
grep -qx "${NEW_VMM_PID}" "${CGROUP}/cgroup.procs" || fail "the new vmm ${NEW_VMM_PID} is not in ${CGROUP}, which holds: $(cat "${CGROUP}/cgroup.procs")"
say "the new vmm runs in the cgroup the stop kept"
expect_exec "kept" "the file written before the stop is there after the start" /bin/cat /root/kept
expect_network "after the start"
expect_fronted "${ID}" "the proxy fronts the sandbox after the start"
shard stop "${ID}" >/dev/null
say "stopped again"

step "remove the microVM"
shard remove "${ID}" >/dev/null
absent "the record" "$([ -e "${SHARD_ROOT}/sandboxes/${ID}" ] && echo "${SHARD_ROOT}/sandboxes/${ID}" || true)"
absent "the address lease" "$([ -e "${LEASE}" ] && echo "${LEASE}" || true)"
absent "the host link" "$(ip link show "${LINK}" 2>/dev/null || true)"
absent "the namespace" "$([ -e "/run/netns/${ID}" ] && echo "/run/netns/${ID}" || true)"
absent "the list --all line" "$(shard list --all | grep "^${ID}" || true)"
absent "the egress chain" "$(nft list table inet shard | grep "chain egress_${LINK}" || true)"
absent "the cgroup" "$([ -e "${CGROUP}" ] && echo "${CGROUP}" || true)"

step "remove the secrets and the policy nothing holds any more"
shard secret remove E2E_TOKEN >/dev/null
shard secret remove E2E_SHAPED >/dev/null
shard policy remove e2e-policy >/dev/null
say "secret remove and policy remove accept what no sandbox holds"

step "prune the image nothing references any more"
holds "${IMAGE%%:*}" shard image prune || fail "image prune did not remove the image"
say "image prune removed the image once no sandbox referenced it"

step "prove the host holds nothing the run left"
absent "a link of this run" "$(ip -o link show | grep -o "${HOST_LINK_PREFIX}[0-9]\+" | sort -u | tr '\n' ' ' || true)"
absent "a vmm of this root" "$(vmm_pids)"
absent "a vmm this run started" "$(started_vmms)"
absent "a jail of this root" "$(find "${SHARD_ROOT}/jail/firecracker" -mindepth 1 -maxdepth 1 2>/dev/null || true)"
absent "a sandbox mount under the root" "$(mount | grep "${SHARD_ROOT}/sandboxes" || true)"
absent "a cgroup of this run" "$(run_cgroups)"
say "the link, the namespace, the vmm, the jail, the cgroup and the mount are gone; the bridge and the policy tables go with the teardown below"

step "stop the daemon and prove the socket is gone"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
say "the socket is gone"

step "clean up"
teardown
rm -f "${ROOT_MARKER}"
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
[ ! -e "${DATA_IMAGE}" ] || fail "the run's own image ${DATA_IMAGE} is still on the host"
grep -qxF -- "$(fstab_line)" /etc/fstab && fail "/etc/fstab still holds the line for ${SHARD_ROOT}"
check_host_net_clear
[ -z "$(run_cgroups)" ] || fail "the host still holds a cgroup of this run: $(run_cgroups)"
[ "${CGROUP_PARENT_BEFORE}" = yes ] || [ ! -d "${CGROUP_PARENT}" ] || fail "the cgroup parent ${CGROUP_PARENT} this run made is still on the host"
[ ! -e "/run/netns/${ECHO_NETNS_NAME}" ] || fail "the echo's netns ${ECHO_NETNS_NAME} is still on the host"
ip link show "${ECHO_LINK}0" >/dev/null 2>&1 && fail "the echo's link ${ECHO_LINK}0 is still on the host"
say "the root, the image, the fstab line, every cgroup of the run and the parent it made, the echo's netns and its link are gone"

trap - EXIT
echo
echo "e2e PASSED on firecracker: install, xfs bootstrap, daemon up, create, the vmm's host cgroup, logs, exec, an entrypoint exit, an OOM stop and start, network, policy, proxy, daemon restart, reconcile, a live fork, pause, resume, stop, snapshot, create twice from it, start, remove, prune, daemon down, and a host with no cgroup, no bridge and no policy table left"
