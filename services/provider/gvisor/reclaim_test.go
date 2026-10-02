package gvisor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// host stands in for the cgroup tree and /proc; a kill drops the process from its cgroup the way an exit does.
type host struct {
	t          *testing.T
	cgroupRoot string
	procRoot   string
	placed     map[int]string
	cgroups    map[string]bool
	gone       map[int]bool
	killed     []int
}

func newHost(t *testing.T) *host {
	t.Helper()

	return &host{t: t, cgroupRoot: t.TempDir(), procRoot: t.TempDir(), placed: map[int]string{}, cgroups: map[string]bool{}, gone: map[int]bool{}}
}

// cgroup makes a cgroup exist, empty until a process is placed in it.
func (h *host) cgroup(path string) {
	h.t.Helper()

	h.cgroups[path] = true
	h.render()
}

// process places a pid in a cgroup with the argv the kernel would publish for it.
func (h *host) process(pid int, cgroup string, argv ...string) {
	h.t.Helper()

	dir := filepath.Join(h.procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("make the fake /proc entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
		h.t.Fatalf("write the fake command line: %v", err)
	}

	h.placed[pid] = cgroup
	h.cgroups[cgroup] = true
	h.render()
}

// render writes every cgroup.procs from the placement, so a kill shows up as the process leaving its cgroup.
func (h *host) render() {
	h.t.Helper()

	for path := range h.cgroups {
		var pids []string
		for pid, placed := range h.placed {
			if placed == path {
				pids = append(pids, strconv.Itoa(pid))
			}
		}
		slices.Sort(pids)

		dir := filepath.Join(h.cgroupRoot, path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.t.Fatalf("make the fake cgroup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strings.Join(pids, "\n")+"\n"), 0o600); err != nil {
			h.t.Fatalf("write cgroup.procs: %v", err)
		}
	}
}

// kill ends a process; one marked gone went between the list and the kill, so it answers ESRCH and is gone from its cgroup too.
func (h *host) kill(pid int) error {
	h.killed = append(h.killed, pid)
	delete(h.placed, pid)
	h.render()
	// A killed process reads an empty command line until it is reaped.
	if err := os.WriteFile(filepath.Join(h.procRoot, strconv.Itoa(pid), "cmdline"), nil, 0o600); err != nil {
		h.t.Fatalf("empty the fake command line: %v", err)
	}
	if h.gone[pid] {
		return syscall.ESRCH
	}

	return nil
}

func (h *host) provider() *gvisor.Provider {
	p := &gvisor.Provider{}
	p.SetCgroupRoot(h.cgroupRoot)
	p.SetProcRoot(h.procRoot)
	p.SetKill(h.kill)

	return p
}

const (
	sandboxID = "amber-otter-1a2b"
	bundleDir = "/var/lib/shard/sandboxes/amber-otter-1a2b/bundle"
)

// The kill lands on the sandbox's own sentry and gofer alone, never on a sibling sharing the id prefix or a foreign runsc.
func TestReclaimKillsTheSandboxProcessesAndNoOthers(t *testing.T) {
	h := newHost(t)
	own := bundle.CgroupsPath(sandboxID)
	h.process(1101, own, "runsc-sandbox", "--root=/var/lib/shard/runsc", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1102, own, "runsc-gofer", "--root=/var/lib/shard/runsc", "gofer", "--bundle", bundleDir, sandboxID)
	h.process(2201, bundle.CgroupsPath(sandboxID+"-2"), "runsc-sandbox", "boot", "--bundle="+bundleDir+"-2", sandboxID+"-2")
	h.process(3301, "docker/7f3a", "runsc-sandbox", "--root=/run/docker/runtime-runsc", "boot", "--bundle=/run/containerd/moby/7f3a", "7f3a")

	if err := h.provider().Reclaim(t.Context(), sandboxID); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(h.killed, want) {
		t.Errorf("Reclaim killed %v, want the sentry and the gofer of %s alone: %v", h.killed, sandboxID, want)
	}
}

// A process in the cgroup that does not name the sandbox is left alone, and the reclaim then says who is left.
func TestReclaimLeavesAProcessThatDoesNotNameTheSandbox(t *testing.T) {
	h := newHost(t)
	h.process(1101, bundle.CgroupsPath(sandboxID), "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1103, bundle.CgroupsPath(sandboxID), "bash")

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := h.provider().Reclaim(ctx, sandboxID)
	if err == nil || !strings.Contains(err.Error(), "still holds processes [1103]") {
		t.Fatalf("Reclaim returned %v, want it to name the process it left", err)
	}
	if want := []int{1101}; !slices.Equal(h.killed, want) {
		t.Errorf("Reclaim killed %v, want the sentry alone: %v", h.killed, want)
	}
}

// A process that went between the list and the kill is what the kill wanted, so ESRCH does not fail the reclaim.
func TestReclaimTakesAProcessThatWentBeforeTheKillAsDone(t *testing.T) {
	h := newHost(t)
	h.process(1101, bundle.CgroupsPath(sandboxID), "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1102, bundle.CgroupsPath(sandboxID), "runsc-gofer", "gofer", "--bundle", bundleDir, sandboxID)
	h.gone[1102] = true

	if err := h.provider().Reclaim(t.Context(), sandboxID); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(h.killed, want) {
		t.Errorf("Reclaim killed %v, want %v", h.killed, want)
	}
}

// A cgroup that is gone or holds nothing is not a wedge a kill can fix, so the reclaim says so instead of pretending.
func TestReclaimRefusesASandboxWithNoProcessToKill(t *testing.T) {
	h := newHost(t)

	err := h.provider().Reclaim(t.Context(), sandboxID)
	if err == nil || !strings.Contains(err.Error(), "no cgroup left to reclaim from") {
		t.Errorf("Reclaim with no cgroup returned %v, want it named", err)
	}

	h.cgroup(bundle.CgroupsPath(sandboxID))
	err = h.provider().Reclaim(t.Context(), sandboxID)
	if err == nil || !strings.Contains(err.Error(), "holds no process to kill") {
		t.Errorf("Reclaim of an empty cgroup returned %v, want it named", err)
	}

	h.process(1103, bundle.CgroupsPath(sandboxID), "bash")
	err = h.provider().Reclaim(t.Context(), sandboxID)
	if err == nil || !strings.Contains(err.Error(), "so none was killed") {
		t.Errorf("Reclaim over a stranger alone returned %v, want it named", err)
	}
	if len(h.killed) != 0 {
		t.Errorf("Reclaim killed %v with nothing of the sandbox to kill", h.killed)
	}
}

// A create killed before runsc saved its state leaves a sentry and a gofer only the cgroup still names, and rm must end them.
func TestSweepKillsWhatACutShortCreateLeft(t *testing.T) {
	h := newHost(t)
	own := bundle.CgroupsPath(sandboxID)
	h.process(1101, own, "runsc-sandbox", "--root=/var/lib/shard/runsc", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1102, own, "runsc-gofer", "--root=/var/lib/shard/runsc", "gofer", "--bundle", bundleDir, sandboxID)
	h.process(2201, bundle.CgroupsPath(sandboxID+"-2"), "runsc-sandbox", "boot", "--bundle="+bundleDir+"-2", sandboxID+"-2")

	if err := h.provider().Sweep(t.Context(), sandboxID); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(h.killed, want) {
		t.Errorf("Sweep killed %v, want the sentry and the gofer of %s alone: %v", h.killed, sandboxID, want)
	}
}

// A sandbox runsc deleted, or one that never got as far as a process, is the ordinary rm and has nothing to sweep.
func TestSweepPassesACgroupThatIsGoneOrEmpty(t *testing.T) {
	h := newHost(t)

	if err := h.provider().Sweep(t.Context(), sandboxID); err != nil {
		t.Errorf("Sweep with no cgroup returned %v, want nothing to do", err)
	}

	h.cgroup(bundle.CgroupsPath(sandboxID))
	if err := h.provider().Sweep(t.Context(), sandboxID); err != nil {
		t.Errorf("Sweep of an empty cgroup returned %v, want nothing to do", err)
	}
	if len(h.killed) != 0 {
		t.Errorf("Sweep killed %v with nothing in the cgroup", h.killed)
	}
}

// A process that does not name the sandbox is never killed, and the cgroup it holds up is refused by name.
func TestSweepRefusesAStranger(t *testing.T) {
	h := newHost(t)
	h.process(1103, bundle.CgroupsPath(sandboxID), "bash")

	err := h.provider().Sweep(t.Context(), sandboxID)
	if err == nil || !strings.Contains(err.Error(), "so none was killed") {
		t.Errorf("Sweep over a stranger alone returned %v, want it named", err)
	}
	if len(h.killed) != 0 {
		t.Errorf("Sweep killed %v, a process that does not name %s", h.killed, sandboxID)
	}
}

// A fork or resume cut short past the grace leaves a sentry and a gofer runsc never saved, so the bring-up kills them itself.
func TestACancelledBringUpSweepsWhatItForked(t *testing.T) {
	h := newHost(t)
	h.process(1101, bundle.CgroupsPath(sandboxID), "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1102, bundle.CgroupsPath(sandboxID), "runsc-gofer", "gofer", "--bundle", bundleDir, sandboxID)
	h.process(2201, bundle.CgroupsPath(sandboxID+"-2"), "runsc-sandbox", "boot", "--bundle="+bundleDir+"-2", sandboxID+"-2")

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	err := h.provider().BringUp(ctx, models.SandboxSpec{ID: sandboxID, StateDir: dir}, filepath.Join(dir, "exit"), func(*os.File, *os.File) error {
		cancel()
		return fmt.Errorf("runsc restore %s: %w", sandboxID, ctx.Err())
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BringUp returned %v, want the cancel", err)
	}
	if want := []int{1101, 1102}; !slices.Equal(h.killed, want) {
		t.Errorf("BringUp killed %v, want the sentry and the gofer of %s alone: %v", h.killed, sandboxID, want)
	}
}

// A failure runsc returns itself is one runsc already cleaned up, so the bring-up kills nothing.
func TestAFailedBringUpKillsNothing(t *testing.T) {
	h := newHost(t)
	h.process(1101, bundle.CgroupsPath(sandboxID), "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)

	dir := t.TempDir()
	err := h.provider().BringUp(t.Context(), models.SandboxSpec{ID: sandboxID, StateDir: dir}, filepath.Join(dir, "exit"), func(*os.File, *os.File) error {
		return errors.New("runsc restore: exit status 1")
	})
	if err == nil {
		t.Fatal("BringUp returned no error for a runsc that failed")
	}
	if len(h.killed) != 0 {
		t.Errorf("BringUp killed %v after a failure runsc returned itself", h.killed)
	}
}

// restorer is a provider over a fake runsc on this host, so a restore can be recorded the way a fork records it.
func (h *host) restorer(script string) *gvisor.Provider {
	h.t.Helper()

	p := newProviderOver(h.t, script, runsc.WithNetwork(runsc.NetworkSandbox))
	p.SetCgroupRoot(h.cgroupRoot)
	p.SetProcRoot(h.procRoot)
	p.SetKill(h.kill)

	return p
}

// launch records a restore of id through the provider, as a fork does before runsc runs, and returns its command line.
func (h *host) launch(p *gvisor.Provider, id string) []string {
	h.t.Helper()

	dir, err := p.StateDir(id)
	if err != nil {
		h.t.Fatalf("StateDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatalf("make the state directory: %v", err)
	}

	opts := runsc.RestoreOptions{Bundle: "/var/lib/shard/sandboxes/" + id + "/bundle", Image: "/var/lib/shard/sandboxes/" + id + "/checkpoint"}
	if err := p.Restore(h.t.Context(), id, opts); err == nil {
		h.t.Fatal("the fake runsc restore passed, want the exit 1 it scripts")
	}

	return p.RestoreArgs(id, opts)
}

// binary names the file a process runs, the link the kernel publishes as /proc/<pid>/exe.
func (h *host) binary(pid int, exe string) {
	h.t.Helper()

	if err := os.Symlink(exe, filepath.Join(h.procRoot, strconv.Itoa(pid), "exe")); err != nil {
		h.t.Fatalf("link the fake binary: %v", err)
	}
}

// restore is a process with a restore's command line, from a daemon now dead, so it sits in that daemon's cgroup.
func (h *host) restore(pid int, exe string, args []string) {
	h.t.Helper()

	h.process(pid, "system.slice/shard.service", args...)
	h.binary(pid, exe)
	h.owner(pid, os.Getuid())
}

// owner names the user that started a process, the real uid the kernel publishes in /proc/<pid>/status.
func (h *host) owner(pid, uid int) {
	h.t.Helper()

	status := fmt.Sprintf("Name:\trunsc\nUid:\t%d\t0\t0\t0\n", uid)
	if err := os.WriteFile(filepath.Join(h.procRoot, strconv.Itoa(pid), "status"), []byte(status), 0o600); err != nil {
		h.t.Fatalf("write the fake status: %v", err)
	}
}

// A restore that has not forked the sandbox yet is outside its cgroup, so the teardown finds it by binary and command line, and only it.
func TestKillRestoresEndsARestoreOutsideTheSandboxCgroup(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	bin, args := p.RunscExecutable(), h.launch(p, sandboxID)
	h.restore(4401, bin, args)
	h.restore(4402, bin, h.launch(p, sandboxID+"-2"))
	other := slices.Clone(args)
	other[2] = "/run/other/runsc"
	h.restore(4403, bin, other)
	h.restore(4404, bin, append(slices.Clone(args[:4]), "delete", "--force", sandboxID))
	// Every argument a restore has, but the verb is exec and the restore is only its command.
	h.restore(4405, bin, append(append(slices.Clone(args[:4]), "exec", sandboxID), args[4:]...))
	// The restore's exact command line on a binary that is not runsc must never be killed.
	h.restore(4406, "/usr/bin/python3", args)
	// A kernel thread links no binary at all.
	h.process(4407, "kthreadd", "")

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if want := []int{4401}; !slices.Equal(h.killed, want) {
		t.Errorf("KillRestores killed %v, want the restore of %s alone: %v", h.killed, sandboxID, want)
	}
}

// The record names the binary the restore ran, so neither a runsc replaced on disk nor a repointed link hides it.
func TestKillRestoresEndsARestoreOfARunscSinceReplaced(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	args := h.launch(p, sandboxID)
	h.restore(4401, p.RunscExecutable()+" (deleted)", args)

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if want := []int{4401}; !slices.Equal(h.killed, want) {
		t.Errorf("KillRestores killed %v, want the restore whose runsc was replaced: %v", h.killed, want)
	}

	// A daemon started on a repointed runsc reads the old binary from the record, not from its own runner.
	h = newHost(t)
	p = h.restorer("exit 1")
	dir, err := p.StateDir(sandboxID)
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make the state directory: %v", err)
	}
	old := `{"executable":"/opt/gvisor/20260701/runsc","args":["runsc","--root","/run/shard/runsc","restore","--detach","--bundle","/b","--image-path","/i","` + sandboxID + `"]}`
	if err := os.WriteFile(filepath.Join(dir, gvisor.LastRestore), []byte(old), 0o600); err != nil {
		t.Fatalf("write the record of the old daemon: %v", err)
	}
	h.restore(4501, "/opt/gvisor/20260701/runsc", []string{"runsc", "--root", "/run/shard/runsc", "restore", "--detach", "--bundle", "/b", "--image-path", "/i", sandboxID})

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if want := []int{4501}; !slices.Equal(h.killed, want) {
		t.Errorf("KillRestores killed %v, want the restore on the runsc the record names: %v", h.killed, want)
	}
}

// A pid whose process changes between the scan and the kill is checked again once pinned, and the stranger lives.
func TestKillRestoresSparesAPidReusedBeforeTheKill(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	h.restore(4401, p.RunscExecutable(), h.launch(p, sandboxID))
	p.SetKillPinned(func(pid int, still func() (bool, error)) error {
		if err := os.RemoveAll(filepath.Join(h.procRoot, strconv.Itoa(pid))); err != nil {
			t.Fatalf("end the fake restore: %v", err)
		}
		h.process(pid, "user.slice", "/usr/bin/python3", "-m", "http.server")
		h.binary(pid, "/usr/bin/python3")

		ok, err := still()
		if err != nil || !ok {
			return err
		}

		return h.kill(pid)
	})

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if len(h.killed) != 0 {
		t.Errorf("KillRestores killed %v, a process that took the pid of the restore", h.killed)
	}
}

// A restore a daemon from before restore.json launched has no record, so a process the daemon's user started with
// the restore's command line on the sandbox's own bundle names it, whatever runsc the deploy points at now.
func TestKillRestoresEndsARestoreAnOlderDaemonLaunched(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	dir, err := p.StateDir(sandboxID)
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	bin, bundle := p.RunscExecutable(), filepath.Join(dir, "bundle")
	old := "/opt/gvisor/20260701/runsc"
	if old == bin {
		t.Fatalf("the old runsc %s must differ from the one this daemon resolves", old)
	}
	args := p.RestoreArgs(sandboxID, runsc.RestoreOptions{Bundle: bundle, Image: "/var/lib/shard/snapshots/source"})
	h.restore(4401, old, args)
	h.restore(4402, old+" (deleted)", p.RestoreArgs(sandboxID, runsc.RestoreOptions{Bundle: bundle, Image: "/var/lib/shard/snapshots/other"}))
	h.restore(4403, bin, args)
	h.restore(4404, old, p.RestoreArgs(sandboxID, runsc.RestoreOptions{Bundle: "/var/lib/shard/sandboxes/other/bundle", Image: "/i"}))
	h.restore(4405, old, p.RestoreArgs(sandboxID+"-2", runsc.RestoreOptions{Bundle: bundle, Image: "/i"}))
	h.restore(4406, old, append(slices.Clone(args), "--extra"))
	// The exact command line, started by another user, as a setuid binary run by that user would be.
	h.restore(4407, "/usr/bin/python3", args)
	h.owner(4407, os.Getuid()+1)
	// A kernel thread links no binary and publishes no status.
	h.process(4408, "kthreadd", args...)

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if want := []int{4401, 4402, 4403}; !slices.Equal(h.killed, want) {
		t.Errorf("KillRestores killed %v, want the unrecorded restores on the bundle of %s alone: %v", h.killed, sandboxID, want)
	}
}

// A sandbox no fork or resume brought up has no restore on its bundle, so the teardown kills nothing.
func TestKillRestoresLeavesASandboxNeverRestored(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	h.restore(4401, p.RunscExecutable(), p.RestoreArgs(sandboxID, runsc.RestoreOptions{Bundle: "/b", Image: "/i"}))

	if err := p.KillRestores(t.Context(), sandboxID); err != nil {
		t.Fatalf("KillRestores: %v", err)
	}
	if len(h.killed) != 0 {
		t.Errorf("KillRestores killed %v for a sandbox with no restore on its bundle", h.killed)
	}
}

// A restore still there after the SIGKILL fails the teardown, so the record stays for the next start to try again.
func TestKillRestoresFailsWhileTheRestoreStillRuns(t *testing.T) {
	h := newHost(t)
	p := h.restorer("exit 1")
	h.restore(4401, p.RunscExecutable(), h.launch(p, sandboxID))
	p.SetKill(func(int) error { return nil })

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := p.KillRestores(ctx, sandboxID)
	if err == nil || !strings.Contains(err.Error(), "the runsc restore [4401] of sandbox "+sandboxID+" still runs") {
		t.Fatalf("KillRestores returned %v, want it to name the restore that survived", err)
	}
}

// Remove kills the restore before any runsc call, so the restore cannot bring the sandbox up after the teardown.
func TestRemoveKillsAnInFlightRestoreBeforeItRunsRunsc(t *testing.T) {
	h := newHost(t)
	ran := filepath.Join(t.TempDir(), "runsc-ran")
	p := h.restorer("touch " + ran + "; exit 1")
	h.restore(4401, p.RunscExecutable(), h.launch(p, sandboxID))
	if err := os.Remove(ran); err != nil {
		t.Fatalf("forget the restore the launch ran: %v", err)
	}

	var runscFirst []int
	p.SetKill(func(pid int) error {
		if _, err := os.Stat(ran); err == nil {
			runscFirst = append(runscFirst, pid)
		}

		return h.kill(pid)
	})

	if err := p.Remove(t.Context(), sandboxID); err == nil {
		t.Fatal("Remove passed over a runsc delete that failed")
	}
	if want := []int{4401}; !slices.Equal(h.killed, want) {
		t.Errorf("Remove killed %v, want the in-flight restore %v", h.killed, want)
	}
	if len(runscFirst) > 0 {
		t.Errorf("Remove ran runsc before it killed the restore %v", runscFirst)
	}
}
