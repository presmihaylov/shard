package models

import (
	"context"
	"net/netip"
	"syscall"
	"time"
)

// Provider runs sandboxes on one substrate. It is v0: SHARD-15 and SHARD-45 will change it.
type Provider interface {
	// Name is the substrate, "gvisor" or "firecracker". It appears in errors and in shard info.
	Name() string
	// Capabilities reports the optional verbs this host can run. Probe once in the constructor.
	Capabilities() Capabilities

	// Create prepares a sandbox in StateCreated. Nothing in the guest runs yet.
	Create(ctx context.Context, spec SandboxSpec) (Runtime, error)
	// Start runs the entrypoint under the supervisor that is already PID 1.
	Start(ctx context.Context, id string) error
	// Stop ends the sandbox, and nothing else does. It signals, waits out grace, then kills.
	Stop(ctx context.Context, id string, grace time.Duration) error
	// Remove deletes the substrate's own state, not the shard record and not a snapshot.
	Remove(ctx context.Context, id string) error

	// Wait blocks until the entrypoint exits. The sandbox stays up, so the caller may exec again.
	Wait(ctx context.Context, id string) (ExitStatus, error)
	// Alive asks the substrate, because a record saying running can outlive a shard restart.
	Alive(ctx context.Context, id string) (bool, error)

	// Pause writes a snapshot into dir and frees the memory. Optional, see Capabilities.
	Pause(ctx context.Context, id string, dir string) error
	// Resume restores from the snapshot in dir and does not consume it. Optional.
	Resume(ctx context.Context, id string, dir string) error
	// Fork starts a new sandbox from the snapshot in dir and leaves the source alone. Optional.
	Fork(ctx context.Context, dir string, spec SandboxSpec) (Runtime, error)
}

// Capabilities is one boolean per optional verb. Never pretend providers are equal.
type Capabilities struct {
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
	Fork   bool `json:"fork"`
}

// SandboxSpec is substrate-neutral: gVisor builds an OCI bundle from it, Firecracker an EROFS disk.
type SandboxSpec struct {
	ID   string
	Name string

	// RootFS is shared and read-only, so the provider adds a per-sandbox writable layer.
	RootFS string
	// StateDir is the per-sandbox directory the provider owns. Snapshots are passed per verb.
	StateDir string

	// Entrypoint is the supervisor's argv, so it is PID 2 and its exit does not end the sandbox.
	Entrypoint []string
	// Env never carries a secret value; the proxy substitutes those on the wire.
	Env     map[string]string
	WorkDir string
	User    string
	// CACert is the PEM the guest must trust for the chunk 4 proxy. Empty until then.
	CACert []byte

	Network   NetworkSpec
	Resources Resources
}

// NetworkSpec is allocated before Create. The provider joins it and reports the host side back.
type NetworkSpec struct {
	NetnsPath string
	Address   netip.Prefix
	Gateway   netip.Addr
}

// Resources bounds the sandbox. Firecracker needs both to boot; gVisor may ignore them.
type Resources struct {
	MemoryMiB int64
	VCPUs     int
}

// Runtime is what only the provider knows once a sandbox exists.
type Runtime struct {
	// PID is the sandbox process on the host, never the entrypoint, which has no host pid.
	PID int
	// HostInterface is the veth or tap that host netfilter rules target.
	HostInterface string
}

// ExitStatus is how the entrypoint ended. A sandbox outlives it and has no exit status of its own.
type ExitStatus struct {
	Code int `json:"code"`
	// Signal is what killed the entrypoint, or 0 if it exited on its own.
	Signal syscall.Signal `json:"signal"`
}

// SupervisorFailedExitCode is shard-init's own exit code when it cannot record the entrypoint exit.
const SupervisorFailedExitCode = 125
