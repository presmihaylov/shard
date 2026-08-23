package bundle

import specs "github.com/opencontainers/runtime-spec/specs-go"

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

func mounts(shardDir, initPath string) []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
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
