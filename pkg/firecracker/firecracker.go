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
	// Vsock is the unix socket the host dials to reach a guest port; empty attaches no vsock device.
	Vsock string
	// Socket is the API socket, which firecracker creates and refuses to find already there.
	Socket string
	// Console is where the guest's serial console lands, which is firecracker's own stdout.
	Console string
}

// Drive is one virtio block device.
type Drive struct {
	ID       string
	Path     string
	ReadOnly bool
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

// The API answers once the process is up; a spawn that takes longer than this is a failure to report.
const startTimeout = 30 * time.Second

// Every call, dial to reply, is bounded, so a vmm that accepts and never answers cannot hold the daemon.
const callTimeout = 30 * time.Second

// A vmm that resets a call is exiting; a kill gives it this long to leave its socket, or to answer again.
const (
	resetGrace = time.Second
	resetPoll  = 50 * time.Millisecond
)
