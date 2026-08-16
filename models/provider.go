package models

import (
	"context"
	"net/netip"
	"syscall"
)

// Provider runs sandboxes on one substrate: gVisor on a host without /dev/kvm,
// Firecracker on a host with one.
//
// It is one fat interface with every verb on it, not a small required interface
// plus optional ones behind type assertions. Capabilities is a method, so it
// reports what THIS HOST can do rather than what the code happens to contain:
// Firecracker on ext4 has no reflink and therefore returns Fork: false.
//
// A provider that cannot run a verb reports false from Capabilities AND returns
// an error matching ErrUnsupported from the verb. Nothing in the compiler links
// those two answers, so the conformance suite in services/provider/conformance
// checks that they agree.
//
// This is v0 and it is expected to change. SHARD-8 designed it on paper,
// SHARD-15 revises it once gVisor is real, SHARD-45 revises it once Firecracker
// is real. The failure to avoid is not designing it early, it is refusing to
// change it later.
type Provider interface {
	// Name is the substrate: "gvisor" or "firecracker". It goes into errors and
	// into shard info, so it is the word a user sees.
	Name() string

	// Capabilities reports the optional verbs this host can run. Compute it once
	// in the constructor, where probing may fail loudly, and return the stored
	// answer here.
	Capabilities() Capabilities

	// Create prepares a sandbox and leaves it in StateCreated. Nothing in the
	// guest runs yet.
	Create(ctx context.Context, spec SandboxSpec) (Runtime, error)

	// Start runs the entrypoint. The sandbox enters StateRunning.
	Start(ctx context.Context, id string) error

	// Wait blocks until the sandbox exits and reports how. It returns at once if
	// the sandbox already exited. A resumed sandbox runs again, so a Wait that
	// has returned must not be reused.
	Wait(ctx context.Context, id string) (ExitStatus, error)

	// Kill sends a signal to the sandbox entrypoint. shard stop sends SIGTERM
	// and then SIGKILL; the grace period between them is the caller's rule, not
	// the provider's.
	Kill(ctx context.Context, id string, sig syscall.Signal) error

	// Delete removes the substrate's own state for the sandbox. It does not
	// remove the shard state record, and it does not remove a snapshot.
	Delete(ctx context.Context, id string) error

	// Pause writes a snapshot into dir and frees the sandbox memory. The sandbox
	// enters StatePaused. Optional: see Capabilities.
	//
	// Freeing the memory is the point. A pause that still holds every byte buys
	// nothing on the small host this project exists for.
	Pause(ctx context.Context, id string, dir string) error

	// Resume restores the sandbox from the snapshot in dir and runs it again.
	//
	// A resume does NOT consume its snapshot. That is a promise to the layers
	// above, because pause once plus fork N is the warm pool primitive, and it
	// costs a provider work: Firecracker maps the memory file at restore, so it
	// must reflink or copy first. Optional: see Capabilities.
	Resume(ctx context.Context, id string, dir string) error

	// Fork creates a new sandbox from the snapshot in dir and starts it, so the
	// result is live and in StateRunning. The snapshot and its source sandbox
	// are left untouched.
	//
	// spec describes the new sandbox and carries its own ID, network and state
	// directory. fork --count N is N calls; the provider forks one at a time.
	// Optional: see Capabilities.
	Fork(ctx context.Context, dir string, spec SandboxSpec) (Runtime, error)
}

// Capabilities is one boolean per optional verb. Never pretend providers are
// equal: a false here is a refusal a user can see before making the call, in
// shard info and in GET /capabilities.
type Capabilities struct {
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
	Fork   bool `json:"fork"`
}

// SandboxSpec is everything a provider needs to create one sandbox. It is
// substrate-neutral on purpose: gVisor turns it into an OCI bundle, Firecracker
// turns it into an EROFS disk plus an initrd, and neither shape leaks into here.
type SandboxSpec struct {
	ID   string
	Name string

	// RootFS is the unpacked image directory. It is shared and read-only, so the
	// provider gives every sandbox its own writable layer over it. shard start on
	// a stopped sandbox must find the files the previous run wrote.
	RootFS string
	// StateDir is the per-sandbox directory the provider owns: bundle, writable
	// layer, logs, pid files. Snapshots are passed separately, per verb.
	StateDir string

	Entrypoint []string
	// Env is the environment for the entrypoint. Order is not significant, and a
	// value here is never a secret: the proxy substitutes those on the wire.
	Env     map[string]string
	WorkDir string
	User    string

	// CACert is the PEM the guest must trust so the intercepting proxy can see
	// HTTP and TLS traffic. Empty until chunk 4. The provider chooses where in
	// the guest it lands.
	CACert []byte

	Network   NetworkSpec
	Resources Resources
}

// NetworkSpec is what the shared layer allocates before Create. The provider
// joins the namespace and reports the host side back in Runtime, because a veth
// and a tap are not the same thing and only the provider knows which it made.
type NetworkSpec struct {
	NetnsPath string
	// Address is the sandbox address with its prefix length.
	Address netip.Prefix
	Gateway netip.Addr
}

// Resources bounds the sandbox. Firecracker requires both values to boot a
// guest; gVisor may apply them through cgroups or ignore them.
type Resources struct {
	MemoryMiB int64
	VCPUs     int
}

// Runtime is what only the provider knows once a sandbox exists: the host
// handles the state record keeps and the egress layer targets.
type Runtime struct {
	// PID supervises the sandbox on the host.
	PID int
	// HostInterface is the host end of the link, a veth or a tap. Host netfilter
	// is the policy of record, and this is the name it is written against.
	HostInterface string
}

// ExitStatus is how a sandbox ended. shard inspect reports it; shard logs says
// why it happened.
type ExitStatus struct {
	Code int `json:"code"`
	// Signal is the signal that killed the sandbox, or 0 if it exited on its own.
	Signal syscall.Signal `json:"signal"`
}
