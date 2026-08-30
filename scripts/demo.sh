#!/usr/bin/env bash
# SHARD-36: pause, resume and fork on a box with no /dev/kvm, in under 90 seconds, as a cast.
#
#   sudo asciinema rec -c ./scripts/demo.sh docs/demo.cast
#
# It keeps its own root and its image cache across takes, and only the two sandboxes come and go.

set -euo pipefail

export PATH="${PATH}:/usr/sbin:/sbin"

PREFIX=${PREFIX:-/usr/local/bin}
SHARD_ROOT=${SHARD_ROOT:-/var/lib/shard-demo}
IMAGE=${IMAGE:-alpine:3.20}

shard() { "${PREFIX}/shard" --root "${SHARD_ROOT}" "$@"; }

# show prints the command the way a person would type it, then runs it, so the cast reads as a session.
show() {
	printf '\n$ %s\n' "$*"
	"$@"
}

timed() {
	local started ended
	started=$(date +%s.%N)
	show "$@"
	ended=$(date +%s.%N)
	awk -v a="${started}" -v b="${ended}" 'BEGIN { printf "  -> %.3f s\n", b - a }'
}

pid_of() { shard inspect "$1" | grep -o '"pid": *[0-9]*' | grep -o '[0-9]*$'; }

# rss prints the resident set of a host pid, or "gone" once runsc holds nothing of the sandbox.
rss() {
	local kib
	kib=$(ps -o rss= -p "$1" 2>/dev/null | tr -d ' ' || true)
	printf '%s' "${kib:-gone}"
}

mem_available_kib() { awk '/^MemAvailable:/ { print $2 }' /proc/meminfo; }

cleanup() {
	for name in web-2 web; do
		shard rm --force "${name}" >/dev/null 2>&1 || true
	done
}
trap cleanup EXIT

[ "$(id -u)" = "0" ] || { echo "this needs root" >&2; exit 1; }
case "${SHARD_ROOT}" in
/ | /var/lib/shard | /var/lib/shard/) echo "SHARD_ROOT must not be ${SHARD_ROOT}" >&2; exit 1 ;;
/*) ;;
*) echo "SHARD_ROOT must be an absolute path, got '${SHARD_ROOT}'" >&2; exit 1 ;;
esac

STARTED=$(date +%s.%N)
cleanup
shard pull "${IMAGE}" >/dev/null

echo "# shard on a box with no /dev/kvm: pause, resume and fork on gVisor"

show shard create --name web "${IMAGE}" -- /bin/sh -c 'i=0; while true; do i=$((i+1)); echo tick $i; sleep 1; done'
sleep 2
show shard exec web -- /bin/sh -c 'echo hello > /root/state'
show shard logs web
PID=$(pid_of web)
RSS_BEFORE=$(rss "${PID}")
FREE_BEFORE=$(mem_available_kib)
echo "  host RSS of sandbox web: ${RSS_BEFORE} KiB (pid ${PID})"

timed shard pause web
echo "  the sandbox process is gone, its memory is back on the host:"
echo "  host RSS of pid ${PID}: ${RSS_BEFORE} KiB before, $(rss "${PID}") after"
CGROUP="/sys/fs/cgroup/shard/$(shard inspect web | grep -o '"id": *"[^"]*"' | cut -d'"' -f4)/memory.current"
echo "  ${CGROUP}: $([ -e "${CGROUP}" ] && echo present || echo gone)"
echo "  host MemAvailable: +$(( $(mem_available_kib) - FREE_BEFORE )) KiB"
show ls -la "$(shard inspect web | grep -o '"snapshot": *"[^"]*"' | cut -d'"' -f4)"

timed shard resume web
sleep 2
show shard exec web -- cat /root/state
echo "  the loop went on from where the pause froze it:"
show shard logs web

timed shard fork --name web-2 web
show shard ls
show shard exec web-2 -- cat /root/state
show shard exec web-2 -- hostname

echo
echo "# E2B quotes ~4 s/GiB to pause and ~1 s to resume, with a cloud round trip inside those numbers."
awk -v a="${STARTED}" -v b="$(date +%s.%N)" 'BEGIN { printf "# total: %.1f s\n", b - a }'
