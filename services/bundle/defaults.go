package bundle

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
)

// defaultPath is what the OCI image spec says to use when the image config sets no PATH.
const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// defaultNoFile is the runc default. A sandbox that needs more says so, once a ticket owns rlimits.
const defaultNoFile = 1024

// defaultCapabilities is the runc default set. It is the ceiling, and a sandbox never gains more.
var defaultCapabilities = []string{
	"CAP_AUDIT_WRITE",
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_MKNOD",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_RAW",
	"CAP_SETFCAP",
	"CAP_SETGID",
	"CAP_SETPCAP",
	"CAP_SETUID",
	"CAP_SYS_CHROOT",
}

// maskedPaths hide host state that leaks through /proc even inside a sandbox.
var maskedPaths = []string{
	"/proc/acpi",
	"/proc/asound",
	"/proc/kcore",
	"/proc/keys",
	"/proc/latency_stats",
	"/proc/timer_list",
	"/proc/timer_stats",
	"/proc/sched_debug",
	"/proc/scsi",
	"/sys/firmware",
}

var readonlyPaths = []string{
	"/proc/bus",
	"/proc/fs",
	"/proc/irq",
	"/proc/sys",
	"/proc/sysrq-trigger",
}

// A tmpfs page is guest memory the sentry holds, so the host cgroup charges it against the sandbox's
// bound. A mount larger than the bound therefore lets a guest end its own sandbox with dd and no
// privilege at all, so /dev/shm never exceeds the bound and /dev is read-only.
const shmTmpfsMiB = 64

// shmSize caps /dev/shm at the Docker default, which is what a workload that uses shared memory
// expects, and a bound smaller than that wins.
func shmSize(r models.Resources) int64 {
	if r.MemoryMiB <= 0 {
		return shmTmpfsMiB
	}

	return min(shmTmpfsMiB, r.MemoryMiB)
}

func tmpfsSize(mib int64) string {
	return fmt.Sprintf("size=%dm", mib)
}

func mounts(shardDir, tmpDir, initPath string, r models.Resources) []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		// gVisor mounts its own devtmpfs here and drops our size=, so the only bound left is ro: a
		// writable /dev reports half the host's memory and a guest fills it to kill its own sandbox.
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "ro"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", tmpfsSize(shmSize(r))}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		// runsc puts an unsized tmpfs on an empty /tmp, and tmpfs is guest memory: 200 MB of dd killed a sandbox.
		{Destination: "/tmp", Type: "bind", Source: tmpDir, Options: []string{"rbind", "rw", "nosuid", "nodev"}},
		// The host side of this one is where shard-init writes the exit status the provider watches.
		{Destination: guestShardDir, Type: "bind", Source: shardDir, Options: []string{"rbind", "rw", "nosuid", "nodev"}},
		// Mounted after its parent, and read-only: the guest may run the supervisor and never replace it.
		{Destination: GuestInitPath, Type: "bind", Source: initPath, Options: []string{"rbind", "ro", "nosuid", "nodev"}},
	}
}

// namespaces gives the sandbox its own everything. An empty netns path means one the runtime creates.
func namespaces(netnsPath string) []specs.LinuxNamespace {
	return []specs.LinuxNamespace{
		{Type: specs.PIDNamespace},
		{Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace},
		{Type: specs.MountNamespace},
		{Type: specs.NetworkNamespace, Path: netnsPath},
	}
}
