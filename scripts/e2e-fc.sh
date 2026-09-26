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
# The bridge and the two policy tables the daemon makes are host-wide, not per root, so two runs on one box collide over them.
# The name is the daemon's own and takes no override: a wrong one here would delete a bridge this run never made.
HOST_BRIDGE="shard0"
# The image the daemon provisions beside the root (services/datadir). check_root normalises SHARD_ROOT first, so this waits for it.
DATA_IMAGE=""
ROOT_MARKER=""
# The sandboxes the feature steps hold, so a step that fails mid-flight still gives them back.
EXIT_ID=""

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
	SHARD_INIT_PATH="${PREFIX}/shard-init" SSL_CERT_FILE="${trust}" \
		SHARD_KERNEL="${SHARD_KERNEL:-}" SHARD_KERNEL_SHA256="${SHARD_KERNEL_SHA256:-}" \
		"${PREFIX}/shard" --root "${SHARD_ROOT}" --provider firecracker daemon >"${DAEMON_LOG}" 2>&1 &
	DAEMON_PID=$!
	wait_for_daemon
}

# fstab_line is what services/datadir appends, byte for byte, so the teardown removes that line and no other.
fstab_line() { printf '%s %s xfs loop 0 0' "${DATA_IMAGE}" "${SHARD_ROOT}"; }

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

# clear_host_net drops the host-wide network state the daemon leaves: a run that keeps it collides with the next run and with the other suites.
clear_host_net() {
	local table
	for table in inet bridge; do
		if nft list table "${table}" shard >/dev/null 2>&1; then
			nft delete table "${table}" shard
		fi
	done
	if ip link show "${HOST_BRIDGE}" >/dev/null 2>&1; then
		ip link del "${HOST_BRIDGE}"
	fi
}

# wipe_root is the library's plus what the firecracker daemon put beside the root and on the host: the mount over it, the image, its fstab line, the bridge and the policy tables.
wipe_root() {
	unmount_under "${SHARD_ROOT}"
	umount -l "${SHARD_ROOT}" >/dev/null 2>&1 || true
	rm -rf "${SHARD_ROOT}" || true
	rm -f "${DATA_IMAGE}" "${DATA_IMAGE}.part" "${DATA_IMAGE}.lock"
	forget_fstab
	clear_host_net
}

# vmm_pids lists every firecracker process driving a socket under this root, and no other root's.
vmm_pids() { pgrep -f -- "^firecracker --api-sock ${SHARD_ROOT}/" || true; }

# record_field reads one string field of a sandbox record.
record_field() { grep -o "\"$2\": *\"[^\"]*\"" "${SHARD_ROOT}/sandboxes/$1/sandbox.json" | cut -d'"' -f4; }

# record_pid reads the pid the record holds, which on firecracker is the vmm.
record_pid() { grep -o '"pid": *[0-9]*' "${SHARD_ROOT}/sandboxes/$1/sandbox.json" | grep -o '[0-9]*$'; }

step "check the host"
[ "$(id -u)" = "0" ] || fail "shard drives /dev/kvm, a tap and nft, so this needs root"
[ -e /dev/kvm ] || fail "no /dev/kvm on this host: firecracker needs bare metal, rent one and run this there"
for binary in firecracker mkfs.erofs mkfs.xfs ip ss nft iptables go curl openssl; do
	command -v "${binary}" >/dev/null || fail "no ${binary} on this host"
done
say "/dev/kvm, firecracker, mkfs.erofs, mkfs.xfs, ip, ss, nft, iptables, go, curl and openssl are on the host"
# The guest reaches the resolver and the proxy over the bridge, and a host firewall that drops INPUT eats them before shard sees them.
if iptables -S INPUT 2>/dev/null | grep -qx -- "-P INPUT DROP" && ! iptables -C INPUT -i shard0 -j ACCEPT 2>/dev/null; then
	fail "the host firewall drops INPUT: run 'iptables -I INPUT -i shard0 -j ACCEPT' (ufw hosts: 'ufw allow in on shard0') and run this again"
fi
say "the host firewall lets the bridge reach the daemon"
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
HOST_IPV4=$(ip route get 1.1.1.1 | grep -o 'src [0-9.]*' | cut -d' ' -f2)
[ -n "${HOST_IPV4}" ] || fail "this host has no route to 1.1.1.1 to read its address from"
case "${HOST_IPV4}" in
10.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | 192.168.* | 169.254.* | 127.* | 100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*)
	fail "this host's address ${HOST_IPV4} is inside the egress floor, which every sandbox is denied: run the suite on a host with a public address" ;;
esac
ECHO_HOST="api.${HOST_IPV4//./-}.sslip.io"
OTHER_HOST="other.${HOST_IPV4//./-}.sslip.io"
DENIED_HOST="deny.${HOST_IPV4//./-}.sslip.io"
start_echo
say "the echo answers on ${HOST_IPV4}, ports 80 and 443, as ${ECHO_HOST} and ${OTHER_HOST}"

step "start the daemon over a root it turns into an xfs image"
SOCKET="${SHARD_ROOT}/shard.sock"
start_daemon || fail "the daemon did not come up"
[ -S "${SOCKET}" ] || fail "no socket at ${SOCKET}"
say "the daemon logged: $(grep 'api listening on' "${DAEMON_LOG}" | sed 's/.*api/api/')"
expect "$(findmnt -no FSTYPE "${SHARD_ROOT}")" "xfs" "the root is an xfs mount"
findmnt -no SOURCE "${SHARD_ROOT}" | grep -q '^/dev/loop' || fail "the root is mounted from $(findmnt -no SOURCE "${SHARD_ROOT}"), want a loop device"
[ -f "${DATA_IMAGE}" ] || fail "there is no image at ${DATA_IMAGE}"
grep -qxF -- "$(fstab_line)" /etc/fstab || fail "/etc/fstab holds no line for ${SHARD_ROOT}"
grep -q "with reflink" "${DAEMON_LOG}" || fail "the daemon did not log the data dir bootstrap"
say "the daemon provisioned ${DATA_IMAGE} over ${SHARD_ROOT}, with reflink, and a line in /etc/fstab"

step "read the daemon status"
STATUS_OUT=$(shard daemon status)
status_field() { echo "${STATUS_OUT}" | awk -v name="$1" '$1 == name { print $2 }'; }
expect "$(status_field pid)" "${DAEMON_PID}" "the status names the pid of the daemon this run started"
expect "$(status_field provider)" "firecracker" "the status names firecracker"
expect "$(status_field pause) $(status_field resume) $(status_field fork)" "false false false" "firecracker claims no snapshot verb until SHARD-44"

step "store a secret and a policy"
SECRET_VALUE="fc-e2e-secret-value-$$-$(date +%s)"
printf '%s\n' "${SECRET_VALUE}" | shard secret set --to "${ECHO_HOST}" E2E_TOKEN >/dev/null
SHAPED_PLACEHOLDER="sk_test_e2eplaceholder01"
SHAPED_VALUE="sk_live_e2e_$$_$(date +%s)"
shard secret set --to "${ECHO_HOST}" --placeholder "${SHAPED_PLACEHOLDER}" E2E_SHAPED "${SHAPED_VALUE}" >/dev/null 2>&1
holds "E2E_TOKEN" shard secret ls || fail "shard secret ls does not list E2E_TOKEN"
holds "${SECRET_VALUE}" shard secret ls && fail "shard secret ls printed the value"
say "secret ls lists the name and not the value"
# The probe address is allowed on every protocol, so the ping the network checks use goes through.
shard policy create --allow 1.1.1.1 --allow dns --allow "${ECHO_HOST}" --allow "${OTHER_HOST}" --deny any e2e-policy >/dev/null
holds "e2e-policy" shard policy ls || fail "shard policy ls does not list e2e-policy"
say "the policy allows the probe, dns and the two echo names, and denies the rest"

step "refuse a create with no memory"
# --memory 0 is the default, which means unbounded on Linux; a microVM has no unbounded and refuses it by name.
CODE=0
REFUSAL=$(shard create "${IMAGE}" -- /bin/sleep 600 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "create ran a microVM with no --memory"
holds "memory" echo "${REFUSAL}" || fail "create said '${REFUSAL}', want it to name --memory"
say "create refuses a microVM with no --memory: ${REFUSAL#shard: }"

step "create a microVM"
create_it() { ID=$(shard create --memory "${MEMORY}" --secret E2E_TOKEN --secret E2E_SHAPED --policy e2e-policy "${IMAGE}" -- /bin/sh -c 'echo shard-e2e-entrypoint; exec /bin/sleep 600'); }
timed "create" create_it
[ -n "${ID}" ] || fail "create printed no id"
say "create printed the id ${ID}"
RECORD="${SHARD_ROOT}/sandboxes/${ID}/sandbox.json"
[ -f "${RECORD}" ] || fail "there is no record at ${RECORD}"
ADDRESS=$(record_field "${ID}" address)
LINK=$(record_field "${ID}" host_interface)
VMM_PID=$(record_pid "${ID}")
say "the record holds the address ${ADDRESS} on the tap ${LINK}, driven by pid ${VMM_PID}"
expect "$(ps -o comm= -p "${VMM_PID}" | tr -d ' ')" "firecracker" "the pid in the record is a firecracker process"
ip -o link show "${LINK}" | grep -q "master shard0" || fail "the tap ${LINK} is not a port of the bridge"
absent "a namespace named ${ID}" "$(ip netns list | grep "^${ID}" || true)"
say "the tap is a bridge port and the guest has no namespace on the host"
[ "$(listed_state "${ID}")" = "running" ] || fail "shard ls lists the sandbox $(listed_state "${ID}"), want running"
holds "${IMAGE%%:*}" shard image ls || fail "image ls does not list ${IMAGE}"
say "ls shows the sandbox running, and the image is cached"

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
expect_exec "kept" "a file lands on the overlay disk, which a stop keeps" /bin/sh -c 'echo kept > /root/kept; cat /root/kept'
CODE=0
shard exec "${ID}" -- /bin/sh -c 'exit 7' >/dev/null 2>&1 || CODE=$?
expect "${CODE}" "7" "exec carries the guest's exit code"
expect "$(printf 'over vsock\n' | shard exec -i "${ID}" -- /bin/cat)" "over vsock" "exec carries stdin in"
expect_exec "1" "the entrypoint is a child of PID 1, which is shard-init" /bin/sh -c 'awk '"'"'$2 == "(sleep)" { print $4 }'"'"' /proc/[0-9]*/stat'

step "a microVM outlives its entrypoint"
EXIT_ID=$(shard create --memory "${MEMORY}" --name e2e-exited "${IMAGE}" -- /bin/sh -c 'exit 3')
for _ in $(seq 1 50); do
	grep -q '"exit_status"' "${SHARD_ROOT}/sandboxes/${EXIT_ID}/sandbox.json" && break
	sleep 0.2
done
grep -q '"exit_status"' "${SHARD_ROOT}/sandboxes/${EXIT_ID}/sandbox.json" || fail "the record of ${EXIT_ID} never took the entrypoint's exit"
expect "$(listed_state "${EXIT_ID}")" "running" "the sandbox is running after its entrypoint exited 3"
expect_exec_in "${EXIT_ID}" "still-up" "an exec answers in a sandbox whose entrypoint is gone" /bin/echo still-up
shard stop --time "${GRACE}" "${EXIT_ID}" >/dev/null
shard rm "${EXIT_ID}" >/dev/null
EXIT_ID=""
say "only stop ended it"

step "reach the network from the microVM"
expect_network "after the create"
expect_exec "resolved" "the guest resolves a name through the daemon's resolver" \
	/bin/sh -c "timeout 5 nslookup ${OTHER_HOST} >/dev/null 2>&1 && echo resolved"

step "enforce the egress policy on the tap"
nft list table inet shard | grep -c "chain egress_${LINK}" >/dev/null || fail "the host holds no chain for ${LINK}"
nft list table bridge shard | grep -c "iifname \"${LINK}\"" >/dev/null || fail "the host does not pin the address of ${LINK}"
say "the host holds a chain for the tap and pins its address"
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
	EGRESS=$(shard logs --egress "${ID}")
	grep '"source":"host"' <<<"${EGRESS}" | grep -q '"verdict":"deny"' && break
	sleep 0.2
done
grep '"source":"host"' <<<"${EGRESS}" | grep -q '"verdict":"deny"' || fail "the egress log holds no host deny: ${EGRESS}"
say "the egress log carries the host's deny"

step "front the microVM through the proxy"
expect_exec "mock-E2E_TOKEN" "the guest sees the placeholder as \$E2E_TOKEN" /bin/sh -c 'echo "$E2E_TOKEN"'
expect_exec "${SHAPED_PLACEHOLDER}" "the guest sees the chosen placeholder as \$E2E_SHAPED" /bin/sh -c 'echo "$E2E_SHAPED"'
fronted "${ID}" || fail "the host holds no dnat to the proxy for ${ADDRESS}"
say "the host turns the guest's 80 and 443 to the proxy"
expect_fronted "${ID}" "a request to the granted host carries the value"
if ! GOT=$(fetch "${ID}" https "${ECHO_HOST}"); then
	fail "the https request to ${ECHO_HOST} failed"
fi
grep -qx "authorization=Bearer ${SECRET_VALUE}" <<<"${GOT}" || fail "the echo saw '${GOT}' over https, want the value in Authorization"
say "the same holds over https, so the guest trusts the proxy CA"
if ! GOT=$(fetch "${ID}" http "${OTHER_HOST}"); then
	fail "the request to ${OTHER_HOST} failed"
fi
grep -qx "authorization=Bearer mock-E2E_TOKEN" <<<"${GOT}" || fail "the echo saw '${GOT}' on the ungranted host, want the placeholder"
say "a request to a host the policy allows but the grant does not keeps the placeholder"
expect_exec "403 Forbidden" "a request to a host no rule allows is refused at the proxy" \
	/bin/sh -c "wget -S -O /dev/null http://${DENIED_HOST}/ 2>&1 | grep -o '403 Forbidden' | head -1"
expect_exec "" "the value is not in the guest's environment" /bin/sh -c "env | grep -F '${SECRET_VALUE}' || true"
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
RECONCILE_ID=$(shard create --memory "${MEMORY}" --name e2e-lost "${IMAGE}" -- /bin/sleep 600)
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
holds "^${RECONCILE_ID}.*daemon restarted and found no process" shard ls --all || fail "shard ls gives no reason for ${RECONCILE_ID}"
shard rm --force "${RECONCILE_ID}" >/dev/null || fail "rm did not free the microVM the host lost"
ip link delete "${RECONCILE_LINK}" >/dev/null 2>&1 || true
RECONCILE_ID=""
RECONCILE_LINK=""
say "ls gives the reason, and rm freed what it left on the host"

# The shared library returns before its own copy of this, so the refusals live here until SHARD-44 lands the snapshot.
for verb in pause resume; do
	step "refuse to ${verb} on firecracker"
	CODE=0
	REFUSAL=$(shard "${verb}" "${ID}" 2>&1) || CODE=$?
	[ "${CODE}" != "0" ] || fail "shard ${verb} exited 0 on firecracker, which holds no snapshots"
	expect "${REFUSAL}" "shard: provider firecracker does not support ${verb} on this host" "${verb} names the provider and the verb"
	expect "$(listed_state "${ID}")" "running" "the refused ${verb} left the microVM running"
done

step "refuse to fork on firecracker"
CODE=0
REFUSAL=$(shard fork --name e2e-fork "${ID}" 2>&1) || CODE=$?
[ "${CODE}" != "0" ] || fail "shard fork exited 0 on firecracker, which holds no snapshots"
expect "${REFUSAL}" "shard: provider firecracker does not support fork on this host" "fork names the provider and the verb"
absent "a sandbox named e2e-fork" "$(shard ls --all | grep e2e-fork || true)"
expect_exec "still-running" "the microVM runs on after the refusals" /bin/echo still-running

step "stop the microVM"
stop_it() { shard stop --time "${GRACE}" "${ID}" >/dev/null; }
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
holds "^${ID}" shard ls && fail "shard ls still lists the stopped sandbox"
expect "$(listed_state "${ID}")" "stopped" "ls hides the stopped sandbox and ls --all shows it stopped"
holds "shard-e2e-entrypoint" timeout 10 "${PREFIX}/shard" --root "${SHARD_ROOT}" logs -f "${ID}" || fail "shard logs -f on a stopped sandbox did not print its output and end"
say "logs still reads a stopped sandbox, and -f ends on its own"
shard stop "${ID}" >/dev/null
say "a second stop is idempotent"

step "clone the stopped microVM twice, by reflink"
timed "clone" clone_it e2e-clone-1
timed "clone" clone_it e2e-clone-2
# shellcheck disable=SC2086 # the clone list is meant to split
set -- ${CLONE_IDS}
[ "$#" = "2" ] && [ "$1" != "$2" ] && [ "$1" != "${ID}" ] && [ "$2" != "${ID}" ] || fail "clone printed '${CLONE_IDS}', want two new ids"
N=0
for CLONE_ID in "$@"; do
	N=$((N + 1))
	CLONE_ADDRESS=$(record_field "${CLONE_ID}" address)
	CLONE_LINKS="${CLONE_LINKS} $(record_field "${CLONE_ID}" host_interface)"
	[ "${CLONE_ADDRESS}" != "${ADDRESS}" ] || fail "clone ${CLONE_ID} got the source's address ${ADDRESS}"
	expect "$(listed_state "${CLONE_ID}")" "running" "clone ${CLONE_ID} runs on its own address ${CLONE_ADDRESS}"
	for _ in $(seq 1 50); do
		[ "$(shard logs "${CLONE_ID}" | grep -c "shard-e2e-entrypoint")" -ge 1 ] && break
		sleep 0.2
	done
	expect "$(shard logs "${CLONE_ID}" | grep -c "shard-e2e-entrypoint")" "1" "clone ${CLONE_ID} ran the entrypoint again, once"
	expect_exec_in "${CLONE_ID}" "kept" "clone ${CLONE_ID} holds the file the source wrote before the stop" /bin/cat /root/kept
	expect_exec_in "${CLONE_ID}" "e2e-clone-${N}" "clone ${CLONE_ID} carries its own hostname" /bin/hostname
	expect_exec_in "${CLONE_ID}" "mock-E2E_TOKEN" "clone ${CLONE_ID} holds the placeholder" /bin/sh -c 'echo "$E2E_TOKEN"'
	expect_blocked "${CLONE_ID}" "the policy holds on clone ${CLONE_ID}"
	expect_fronted "${CLONE_ID}" "the proxy fronts clone ${CLONE_ID}"
done
shard exec "$1" -- /bin/sh -c 'echo clone-only > /root/clone-only' >/dev/null
CODE=0
shard exec "$2" -- /bin/cat /root/clone-only >/dev/null 2>&1 || CODE=$?
[ "${CODE}" != "0" ] || fail "clone $2 sees the file clone $1 wrote"
grep -q '"state": *"stopped"' "${RECORD}" || fail "the clones changed the source's state"
say "the clones share nothing with each other or with the source, which is still stopped"

step "stop and remove the clones"
for CLONE_ID in "$@"; do
	shard stop --time "${GRACE}" "${CLONE_ID}" >/dev/null
	shard rm "${CLONE_ID}" >/dev/null
done
CLONE_IDS=""
CLONE_LINKS=""
absent "a clone record" "$(shard ls --all | grep e2e-clone || true)"

step "start the microVM again"
start_it() { shard start "${ID}" >/dev/null; }
timed "start" start_it
expect "$(listed_state "${ID}")" "running" "the record says running again"
grep -q "\"address\": *\"${ADDRESS}\"" "${RECORD}" || fail "the start changed the address"
NEW_VMM_PID=$(record_pid "${ID}")
[ "${NEW_VMM_PID}" != "${VMM_PID}" ] || fail "the start reused the pid of the stopped vmm"
expect "$(ps -o comm= -p "${NEW_VMM_PID}" | tr -d ' ')" "firecracker" "a new vmm ${NEW_VMM_PID} drives the same address"
expect_exec "kept" "the file written before the stop is there after the start" /bin/cat /root/kept
expect_network "after the start"
expect_fronted "${ID}" "the proxy fronts the sandbox after the start"
shard stop --time "${GRACE}" "${ID}" >/dev/null
say "stopped again"

step "remove the microVM"
shard rm "${ID}" >/dev/null
absent "the record" "$([ -e "${SHARD_ROOT}/sandboxes/${ID}" ] && echo "${SHARD_ROOT}/sandboxes/${ID}" || true)"
absent "the address lease" "$([ -e "${LEASE}" ] && echo "${LEASE}" || true)"
absent "the tap" "$(ip link show "${LINK}" 2>/dev/null || true)"
absent "the ls --all line" "$(shard ls --all | grep "^${ID}" || true)"
absent "the egress chain" "$(nft list table inet shard | grep "chain egress_${LINK}" || true)"

step "remove the secrets and the policy nothing holds any more"
shard secret rm E2E_TOKEN >/dev/null
shard secret rm E2E_SHAPED >/dev/null
shard policy rm e2e-policy >/dev/null
say "secret rm and policy rm accept what no sandbox holds"

step "prune the image nothing references any more"
holds "${IMAGE%%:*}" shard image prune || fail "image prune did not remove the image"
say "image prune removed the image once no sandbox referenced it"

step "prove the host holds nothing the run left"
absent "a tap of this run" "$(ip -o link show | grep -o "${HOST_LINK_PREFIX}[0-9]\+" | sort -u | tr '\n' ' ' || true)"
absent "a vmm of this root" "$(vmm_pids)"
absent "a sandbox mount under the root" "$(mount | grep "${SHARD_ROOT}/sandboxes" || true)"
say "the tap, the vmm and the mount are gone; the bridge and the policy tables go with the teardown below"

step "stop the daemon and prove the socket is gone"
stop_daemon || fail "the socket ${SOCKET} outlived the daemon"
say "the socket is gone"

step "clean up"
teardown
rm -f "${ROOT_MARKER}"
[ ! -e "${SHARD_ROOT}" ] || fail "the run's own root ${SHARD_ROOT} is still on the host"
[ ! -e "${DATA_IMAGE}" ] || fail "the run's own image ${DATA_IMAGE} is still on the host"
grep -qxF -- "$(fstab_line)" /etc/fstab && fail "/etc/fstab still holds the line for ${SHARD_ROOT}"
ip link show "${HOST_BRIDGE}" >/dev/null 2>&1 && fail "the bridge ${HOST_BRIDGE} is still on the host"
nft list table inet shard >/dev/null 2>&1 && fail "the host still holds table inet shard"
nft list table bridge shard >/dev/null 2>&1 && fail "the host still holds table bridge shard"
say "the root, the image, the fstab line, the bridge ${HOST_BRIDGE} and both shard nft tables are gone"

trap - EXIT
echo
echo "e2e PASSED on firecracker: install, xfs bootstrap, daemon up, create, logs, exec, an entrypoint exit, network, policy, proxy, daemon restart, reconcile, refused pause, resume and fork, stop, clone twice, start, rm, prune, daemon down, and a host with no bridge and no policy table left"
