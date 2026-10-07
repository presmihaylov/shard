package gvisor

import (
	"context"
	"net"
	"os"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
)

// BoundMemory drives what create does to the cgroup runsc just made. A test cannot reach it through
// Create, which needs root and a real rootfs mount, so it reaches it here over a directory it owns.
func BoundMemory(root string, spec models.SandboxSpec) error {
	return boundMemory(root, spec)
}

// BoundPids drives the pids cap create writes on the cgroup, reachable without runsc or root.
func BoundPids(root, id string) error {
	return boundPids(root, id)
}

// SetCgroupRoot points a provider at a directory a test owns, so the reason a dead sandbox died can
// be read without a real cgroup hierarchy and without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// SetProcRoot points a provider at a directory that stands in for /proc, so a reclaim reads command lines a test wrote.
func (p *Provider) SetProcRoot(root string) {
	p.procRoot = root
}

// ProcRoot is the stand-in /proc a test writes into, so it can add a command line beside the stat the provider already made.
func (p *Provider) ProcRoot() string {
	return p.procRoot
}

// SetKill replaces the SIGKILL a teardown sends, so a test records the pids instead of killing anything; it still rechecks the id the way the pinned kill does.
func (p *Provider) SetKill(kill func(pid int) error) {
	p.killPinned = func(pid int, still func() (bool, error)) error {
		ok, err := still()
		if err != nil || !ok {
			return err
		}

		return kill(pid)
	}
}

// SetKillPinned replaces the pinned SIGKILL a restore gets, so a test can change the process between the scan and the kill.
func (p *Provider) SetKillPinned(kill func(pid int, still func() (bool, error)) error) {
	p.killPinned = kill
}

// PidfdKill is the pinned SIGKILL itself, reachable over a child process the test owns.
func PidfdKill(pid int, still func() (bool, error)) error {
	return pidfdKill(pid, still)
}

// LastRestore is the file a fork or resume records its restore in, under the sandbox's state directory.
const LastRestore = lastRestore

// Restore is the launch fork and resume share: it records the restore, then runs runsc.
func (p *Provider) Restore(ctx context.Context, id string, opts runsc.RestoreOptions) error {
	return p.restore(ctx, id, opts)
}

// RestoreArgs is the command line the provider's runner gives a restore, which the record keeps.
func (p *Provider) RestoreArgs(id string, opts runsc.RestoreOptions) []string {
	return p.runsc.RestoreArgs(id, opts)
}

// StateDir is where the provider looks for a sandbox's own files.
func (p *Provider) StateDir(id string) (string, error) {
	return p.dirs(id)
}

// Sweep is the kill Remove runs on what a cut-short create left in the cgroup, reachable without runsc.
func (p *Provider) Sweep(ctx context.Context, id string) error {
	return p.sweep(ctx, id)
}

// BringUp is the launch create, fork and resume share, with the runsc verb a test scripts.
func (p *Provider) BringUp(ctx context.Context, spec models.SandboxSpec, exitFile string, up func(out, exit *os.File) error) error {
	return p.bringUp(ctx, spec, exitFile, up)
}

// RunscExecutable is the binary a restore's /proc/<pid>/exe must name for the kill to touch it.
func (p *Provider) RunscExecutable() string {
	return p.runsc.Executable()
}

// KillRestores is the kill Remove runs first on a runsc restore of the sandbox, reachable without runsc.
func (p *Provider) KillRestores(ctx context.Context, id string) error {
	return p.killRestores(ctx, id)
}

// RemoveCgroup is the cgroup rmdir a teardown runs, reachable without runsc.
func RemoveCgroup(root, id string) error {
	return cgroup.Remove(cgroupDir(root, id))
}

// ZombieStat is the /proc stat parse Status runs on a live answer, reachable with a fixture line.
func ZombieStat(stat string) bool {
	return zombieStat(stat)
}

// ExecFailure is the name Exec gives what runsc reports as its own 128, reachable with a driver error.
func ExecFailure(id string, err error) error {
	return execFailure(id, err)
}

// Vanished is the classification stale runs on a read of /proc that failed, reachable with an error.
func Vanished(err error) bool {
	return vanished(err)
}

// SafeDelete is the SHARD-440 teardown: it sweeps the sandbox's own cgroup members through a pinned kill, then forgets runsc's state, and never force-deletes.
func (p *Provider) SafeDelete(ctx context.Context, id string) error {
	return p.safeDelete(ctx, id)
}

// SetRunsc swaps the runsc surface the provider drives, so a teardown test records what it forgot without a real runner.
func (p *Provider) SetRunsc(r runscCtl) {
	p.runsc = r
}

// RunscStub answers every runscCtl method with a zero value, so a test embeds it and overrides only the methods under test.
type RunscStub struct{}

func (RunscStub) Create(ctx context.Context, id string, opts runsc.CreateOptions) error { return nil }
func (RunscStub) Exec(ctx context.Context, id string, opts runsc.ExecOptions) (int, error) {
	return 0, nil
}
func (RunscStub) Signal(ctx context.Context, id string, pid int, signal string) error { return nil }
func (RunscStub) Start(ctx context.Context, id string) error                          { return nil }
func (RunscStub) Pause(ctx context.Context, id string) error                          { return nil }
func (RunscStub) Resume(ctx context.Context, id string) error                         { return nil }
func (RunscStub) Checkpoint(ctx context.Context, id, dir string) error                { return nil }
func (RunscStub) CheckpointRunning(ctx context.Context, id, dir string) error         { return nil }
func (RunscStub) Restore(ctx context.Context, id string, opts runsc.RestoreOptions) error {
	return nil
}
func (RunscStub) RestoreArgs(id string, opts runsc.RestoreOptions) []string   { return nil }
func (RunscStub) Kill(ctx context.Context, id, signal string, all bool) error { return nil }
func (RunscStub) State(ctx context.Context, id string) (runsc.State, error) {
	return runsc.State{}, nil
}
func (RunscStub) Forget(id string) error { return nil }
func (RunscStub) Executable() string     { return "" }
func (RunscStub) DropNullNetns() error   { return nil }
func (RunscStub) PortForward(ctx context.Context, id string, port uint16) (net.Conn, error) {
	return nil, nil
}

// ExecOptions is the process an exec hands runsc, reachable without a running sandbox.
func ExecOptions(b bundle.Bundle, spec models.ExecSpec) (runsc.ExecOptions, error) {
	return execOptions(b, spec)
}
