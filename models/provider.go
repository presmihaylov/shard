package models

import (
	"context"
	"net/netip"
	"os"
	"time"
)

// Provider runs sandboxes on one substrate. It is v0: SHARD-45 will change it again.
type Provider interface {
	// Name is the substrate, "gvisor" or "sysbox". It appears in errors.
	Name() string
	// Capabilities reports the optional verbs this host can run. Probe once in the constructor.
	Capabilities() Capabilities

	// CheckResources refuses a bound this substrate cannot run under, before the orchestrator writes a
	// record: a refusal here leaves nothing behind. Create checks the same bounds again on its spec.
	CheckResources(res Resources) error
	// Create prepares a sandbox in StateCreated. Nothing in the guest runs yet.
	Create(ctx context.Context, spec SandboxSpec) error
	// Start runs the entrypoint. Create prepared the sandbox and nothing in the guest ran before this.
	// After a stop it runs the entrypoint again over the writable layer the stop kept, with the
	// address and the netns the orchestrator rebuilt first (SHARD-96).
	Start(ctx context.Context, id string) error
	// Stop ends the sandbox, and nothing else does. It signals, waits out grace, then kills.
	Stop(ctx context.Context, id string, grace time.Duration) error
	// Remove deletes the substrate's own state, not the shard record and not a snapshot.
	Remove(ctx context.Context, id string) error
	// Clone starts a new sandbox over a copy of the files sourceID kept and runs its entrypoint from the
	// beginning. It refuses a source that is alive, and it writes nothing of the source.
	Clone(ctx context.Context, sourceID string, spec SandboxSpec) error

	// Exec runs a command in a sandbox that already runs and returns how that command ended. It is
	// never the entrypoint: it has no supervisor, and its exit ends nothing. ExitStatus.Signal is
	// always 0, because a substrate reports an exec's exit code and nothing else.
	Exec(ctx context.Context, id string, spec ExecSpec) (ExitStatus, error)

	// Signal sends one signal to a running exec by the pid ExecSpec.Report gave for it. The pid is
	// whatever handle that provider signals by, so a caller only ever passes back what Report reported.
	Signal(ctx context.Context, id string, pid int, signal string) error

	// Wait blocks until the entrypoint exits. The sandbox stays up, so the caller may exec again.
	// Under a restart policy it returns the first exit of the run; once the sandbox is stopped, the last.
	// It reports ErrNoExitStatus for a sandbox a stop had to kill, which recorded no exit.
	Wait(ctx context.Context, id string) (ExitStatus, error)
	// ExitStatus reads how the entrypoint ended so far, nil while it still runs. It is a file read, not
	// a substrate call, like Restarts, so the liveness task polls it every tick; Wait would block.
	ExitStatus(ctx context.Context, id string) (*ExitStatus, error)
	// Status asks the substrate, because a record saying running can outlive a shard restart.
	Status(ctx context.Context, id string) (Status, error)
	// Restarts reads what the supervisor keeps beside the exit file: how often it started the
	// entrypoint again on this run, and whether it gave up. It is zero for a run that never did.
	Restarts(ctx context.Context, id string) (RestartCount, error)
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
	// A stop leaves the same leftovers, so a record that says stopped outranks it.
	OOMKilled bool
}

// Alive is the assertion the keep-alive default rests on: only Stop and Pause take a sandbox out of it.
func (s Status) Alive() bool { return s.Exists && s.State != StateStopped }

// UserNamespace is a user namespace a sandbox joins, pinned on the host like the netns it owns.
type UserNamespace struct {
	Path string
	// HostID is where guest uid and gid 0 land on the host; Size is how many ids follow it.
	HostID uint32
	Size   uint32
}

// Set reports whether the sandbox joins a user namespace of its own.
func (u UserNamespace) Set() bool { return u.Path != "" }

// SandboxSpec is substrate-neutral: gVisor builds an OCI bundle from it, Firecracker an EROFS disk.
type SandboxSpec struct {
	ID   string
	Name string

	// RootFS is the shared read-only image tree; the provider derives its own writable form from it.
	RootFS string
	// RootDisk is the same image as one ext4 file, for a provider that boots a VM; empty when the image service keeps none.
	RootDisk string
	// BaseDisk is the same image as one read-only EROFS file, for a provider that boots a microVM over an overlay; empty when the image service keeps none.
	BaseDisk string
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
	// Restart is what the supervisor is told about starting the entrypoint again; the zero value is never.
	Restart RestartSpec

	// ProxyCA is the PEM certificate a fronted sandbox must trust, so the proxy can terminate its TLS; nil fronts nothing.
	ProxyCA []byte
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
	// Report is called once with the guest process id, so the caller can Signal the exec while it runs.
	Report func(pid int)
	// Resizes carries every later window of the terminal, for a provider whose guest has a pty of its own; nil for one that shares the replica.
	Resizes <-chan TerminalSize
}

// TerminalSize is a terminal window in character cells.
type TerminalSize struct {
	Rows uint16
	Cols uint16
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
	// Userns is the user namespace that owns the netns, when the substrate runs the guest in one of
	// its own. The zero value is the host's, which is what gVisor joins.
	Userns  UserNamespace
	Address netip.Prefix
	Gateway netip.Addr
	// HostInterface is the veth or tap on the host side of the link. Netfilter rules target it.
	HostInterface string
	// Nameservers is what the guest resolver reads. Neither substrate resolves a name itself.
	Nameservers []netip.Addr
}

// Resources bounds the sandbox. Firecracker needs both to boot; gVisor may ignore them.
type Resources struct {
	MemoryMiB int64 `json:"memory_mib"`
	VCPUs     int   `json:"vcpus"`
	// DiskMiB bounds the writable layer and /tmp together, as one sparse image the guest fills before the host; 0 takes the default.
	DiskMiB int64 `json:"disk_mib"`
}

// ExitStatus is how the entrypoint ended. A sandbox outlives it and has no exit status of its own.
type ExitStatus struct {
	Code int `json:"code"`
	// Signal is a guest signal number, so it is an int: this package must not import syscall.
	Signal int `json:"signal"`
}

// ExitReport is shard-init's newline-framed exit record; Kind lets a reader reject a torn or foreign line.
type ExitReport struct {
	Kind   string `json:"kind"`
	Code   int    `json:"code"`
	Signal int    `json:"signal"`
}

// ExitReportKind is the only Kind an exit report carries, so a reader rejects anything else.
const ExitReportKind = "exit"

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

// Environment is the guest environment of a created or stopped sandbox, which a grant, an ungrant and an attach rewrite for the next start.
type Environment interface {
	// CanSetEnv answers what SetEnv would refuse and writes nothing, so a grant can check before it plants.
	CanSetEnv(name string) error
	// SetEnv adds one variable, and refuses a name the guest already holds.
	SetEnv(name, value string) error
	// RemoveEnv drops every entry of that name. An environment that holds none is the outcome asked for.
	RemoveEnv(name string) error
	// TrustProxy merges the proxy CA into the image roots, so the sandbox trusts the proxy from its next start.
	TrustProxy(proxyCA []byte) error
}
