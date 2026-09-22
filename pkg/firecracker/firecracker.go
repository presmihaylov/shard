// Package firecracker drives one firecracker process per microVM over its API socket, and knows no sandbox.
package firecracker

import (
	"errors"
	"time"
)

// ErrSocketInUse says a live firecracker answers on the API socket, so a second one must not replace it.
var ErrSocketInUse = errors.New("firecracker: a vmm already serves this socket")

// Config is what one microVM boots with. Every path is on the host.
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
	// Console is where the guest's serial console lands, which is firecracker's own stdout.
	Console string
	// Cgroup is the host cgroup the vmm joins before the guest is configured, so the guest's whole memory is charged to it; empty joins none.
	Cgroup string
}

// Snapshot is what one microVM comes back from: the two files a snapshot wrote, and the host things the new process owns instead of the old one's.
type Snapshot struct {
	// State and Memory are the files Client.Snapshot wrote; the vmm maps the memory private and read-only, so one file serves any number of restores.
	State  string
	Memory string
	// Tap replaces the host device of eth0; empty keeps the one in the snapshot, which two microVMs cannot both open.
	Tap string
	// Drives swap the host path of a drive the snapshot names, by id, after the load opened the snapshot's own; a drive not named keeps it.
	Drives  []Drive
	Vsock   string
	Socket  string
	Console string
	Cgroup  string
}

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

// A vmm that resets a call is exiting; a kill gives it this long to leave its socket, or to answer again.
const (
	resetGrace = time.Second
	resetPoll  = 50 * time.Millisecond
)
