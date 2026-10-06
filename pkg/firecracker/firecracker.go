// Package firecracker drives one firecracker process per microVM over its API socket, and knows no sandbox.
package firecracker

import (
	"errors"
	"path/filepath"
	"time"
)

// ErrSocketInUse says a live firecracker answers on the API socket, so a second one must not replace it.
var ErrSocketInUse = errors.New("firecracker: a vmm already serves this socket")

// ErrExiting says the vmm that took the dial has begun its exit, so it is as gone as one that refuses the dial.
var ErrExiting = errors.New("firecracker: the vmm behind this socket is exiting")

// Jail is where the jailer puts one vmm: a chroot under Base, a uid and gid of its own, and the cgroup it joins before the vmm exists.
type Jail struct {
	// Jailer and Exec are host paths; the jailer copies Exec into the chroot and runs it there, as UID.
	Jailer string
	Exec   string
	// ID names the chroot, which the jailer takes as at most 64 letters, digits and hyphens.
	ID   string
	UID  int
	Base string
	// Cgroup is the v2 cgroup, relative to the hierarchy's root, the jailer moves itself into before the clone, so the vmm's whole memory is charged to it.
	Cgroup string
	// Netns is the network namespace the jailer joins before the clone, so the vmm opens its tap there; empty stays in the jailer's own.
	Netns string
}

// Root is the chroot the jailer makes, the vmm's "/"; it names Exec by its base name, so Exec must be no symlink.
func (j Jail) Root() string {
	return filepath.Join(j.Base, filepath.Base(j.Exec), j.ID, "root")
}

// Host is where a path inside the jail is on the host.
func (j Jail) Host(path string) string {
	return filepath.Join(j.Root(), path)
}

// Config is what one microVM boots with. Every path is inside the jail, but Console.
type Config struct {
	Kernel string
	Initrd string
	// Cmdline is the kernel command line; firecracker adds nothing to it, so root= and console= are the caller's.
	Cmdline string
	// VCPUs is 1 to 32, and MemoryMiB is the guest's whole memory; neither has a default.
	VCPUs     int64
	MemoryMiB int64
	// Drives attach in order, so the first is /dev/vda in the guest.
	Drives []Drive
	// Network is the one virtio-net device, eth0 in the guest; an empty tap attaches none.
	Network Network
	// Vsock is the unix socket the host dials to reach a guest port; empty attaches no vsock device.
	Vsock string
	// Socket is the API socket, which firecracker creates and refuses to find already there.
	Socket string
	// Console is the host file the guest's serial console lands in, which is the jailer's and then firecracker's own stdout.
	Console string
}

// Snapshot is what one microVM comes back from: the two files a snapshot wrote, and the things the new process owns instead of the old one's. Every path is inside the jail, but Console.
type Snapshot struct {
	// State and Memory are the files Client.Snapshot wrote; the vmm maps the memory private and read-only, so one file serves any number of restores.
	State  string
	Memory string
	// Tap replaces the host device of eth0; empty keeps the one in the snapshot, which two microVMs cannot both open.
	Tap     string
	Vsock   string
	Socket  string
	Console string
}

// SnapshotType is how much of the guest's memory a snapshot writes: a Full every page, a Diff the pages the dirty-page log holds, which the snapshot then clears.
type SnapshotType string

const (
	SnapshotFull SnapshotType = "Full"
	SnapshotDiff SnapshotType = "Diff"
)

// Drive is one virtio block device.
type Drive struct {
	ID       string
	Path     string
	ReadOnly bool
}

// Network is the tap the vmm opens on the host, and the MAC the guest's device answers to.
type Network struct {
	Tap string
	MAC string
}

// State is the microVM's state as firecracker reports it, in its own words.
type State string

const (
	StateNotStarted State = "Not started"
	StateRunning    State = "Running"
	StatePaused     State = "Paused"
)

// Info is what every verb reports back: the microVM's state and the pid of the firecracker behind the socket.
type Info struct {
	State State
	PID   int
}

// GuestCID is the vsock address of every guest; each microVM has its own vmm, so they never meet.
const GuestCID = 3

// guestInterface is the device's id on the API, and the name the guest kernel gives its only network device.
const guestInterface = "eth0"

// The API answers once the process is up; a spawn that takes longer than this is a failure to report.
const startTimeout = 30 * time.Second

// Every call, dial to reply, is bounded, so a vmm that accepts and never answers cannot hold the daemon.
const callTimeout = 30 * time.Second

// A dial that fails and is not refused is a vmm on its way out; a kill gives it this long to leave its socket, or to take the dial.
const (
	resetGrace = time.Second
	resetPoll  = 50 * time.Millisecond
)
