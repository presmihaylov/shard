package models

import (
	"context"
	"net/netip"
	"os"
	"time"
)

// Provider runs sandboxes on one substrate. It is v0: SHARD-45 will change it again.
type Provider interface {
	// Name is the substrate, "gvisor" or "firecracker". It appears in errors and in shard info.
	Name() string
	// Capabilities reports the optional verbs this host can run. Probe once in the constructor.
	Capabilities() Capabilities

	// Create prepares a sandbox in StateCreated. Nothing in the guest runs yet.
	Create(ctx context.Context, spec SandboxSpec) error
	// Start runs the entrypoint. Create prepared the sandbox and nothing in the guest ran before this.
	// A provider may refuse a start after a stop; the orchestrator then re-creates over the same
	// writable layer (SHARD-24).
	Start(ctx context.Context, id string) error
	// Stop ends the sandbox, and nothing else does. It signals, waits out grace, then kills.
	Stop(ctx context.Context, id string, grace time.Duration) error
	// Remove deletes the substrate's own state, not the shard record and not a snapshot.
	Remove(ctx context.Context, id string) error

	// Exec runs a command in a sandbox that already runs and returns how that command ended. It is
	// never the entrypoint: it has no supervisor, and its exit ends nothing. ExitStatus.Signal is
	// always 0, because a substrate reports an exec's exit code and nothing else.
	Exec(ctx context.Context, id string, spec ExecSpec) (ExitStatus, error)

	// Wait blocks until the entrypoint exits. The sandbox stays up, so the caller may exec again.
	// It reports ErrNoExitStatus for a sandbox a stop had to kill, which recorded no exit.
	Wait(ctx context.Context, id string) (ExitStatus, error)
	// Status asks the substrate, because a record saying running can outlive a shard restart.
	Status(ctx context.Context, id string) (Status, error)
	// LogPath names the file the guest's output lands in. SHARD-23 turns it into shard logs.
	LogPath(id string) (string, error)

	// Pause writes a snapshot into dir and frees the memory. Optional, see Capabilities.
	Pause(ctx context.Context, id string, dir string) error
	// Resume restores from the snapshot in dir and does not consume it. Optional.
	Resume(ctx context.Context, id string, dir string) error
	// Fork starts a new sandbox from the snapshot in dir and leaves the source alone. Optional.
	Fork(ctx context.Context, dir string, spec SandboxSpec) error
}

// Capabilities is one boolean per optional verb. Never pretend providers are equal.
type Capabilities struct {
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
	Fork   bool `json:"fork"`
}

// Status is what the substrate says now, never what the record says.
type Status struct {
	// Exists is false for an id the substrate never held, and for one it has already forgotten.
	Exists bool
	State  State
	// PID is the sandbox process on the host, never the entrypoint, which has no host pid.
	PID int
	// OOMKilled says the host ended this sandbox for holding too much memory. It is only ever set on
	// a sandbox that is not alive, because the provider reads it from what the dead one left behind.
	OOMKilled bool
}

// Alive is the assertion the keep-alive default rests on: only Stop takes a sandbox out of it.
func (s Status) Alive() bool { return s.Exists && s.State != StateStopped }

// SandboxSpec is substrate-neutral: gVisor builds an OCI bundle from it, Firecracker an EROFS disk.
type SandboxSpec struct {
	ID   string
	Name string

	// RootFS is the shared read-only image tree; the provider derives its own writable form from it.
	RootFS string
	// StateDir is the per-sandbox directory whose whole layout belongs to the provider.
	StateDir string

	// Entrypoint is the supervisor's argv: it runs it as its child, so its exit does not end the sandbox.
	Entrypoint []string
	// Env is KEY=VALUE, resolved against the image by Resolve. It never carries a secret value.
	Env     []string
	WorkDir string
	User    string

	Network   NetworkSpec
	Resources Resources
}

// ExecSpec is one process in a sandbox that already runs. It is never the entrypoint.
type ExecSpec struct {
	Argv []string
	// Env overrides what the entrypoint runs with, which the provider reads back from the sandbox.
	Env     []string
	WorkDir string
	// User is an image-style name or id, resolved by the provider against the sandbox's own rootfs.
	// Empty is the user the entrypoint runs as, which the provider reads back from the sandbox.
	User string
	// TTY says the three files below are one pty replica the caller allocated on the host. A terminal
	// carries one stream, so Stderr is then the same file as Stdout.
	TTY bool
	// The fds the guest process gets. They are files, not pipes, so a pty replica passes straight
	// through; a nil one is /dev/null.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

// ImageConfig is the part of an OCI image config a sandbox is built from. The spec overrides it.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkDir    string
	User       string
}

// NetworkSpec is allocated before Create, so the provider joins a namespace it did not build.
type NetworkSpec struct {
	NetnsPath string
	Address   netip.Prefix
	Gateway   netip.Addr
	// HostInterface is the veth or tap on the host side of the link. Netfilter rules target it.
	HostInterface string
	// Nameservers is what the guest resolver reads. Neither substrate resolves a name itself.
	Nameservers []netip.Addr
}

// Resources bounds the sandbox. Firecracker needs both to boot; gVisor may ignore them.
type Resources struct {
	MemoryMiB int64 `json:"memory_mib"`
	VCPUs     int   `json:"vcpus"`
}

// ExitStatus is how the entrypoint ended. A sandbox outlives it and has no exit status of its own.
type ExitStatus struct {
	Code int `json:"code"`
	// Signal is a guest signal number, so it is an int: this package must not import syscall.
	Signal int `json:"signal"`
}

// SupervisorFailedExitCode is shard-init's own exit code when it cannot record the entrypoint exit.
const SupervisorFailedExitCode = 125

// EntrypointNotStartedExitCode is what a shell reports for a command it cannot run, and so is a broken image.
const EntrypointNotStartedExitCode = 127

// The two codes a shell answers for a command that never ran. A substrate's own code is never one
// of these: runsc says 128 for both, which no shell means anything by.
const (
	CommandNotFoundExitCode      = 127
	CommandNotExecutableExitCode = 126
)
