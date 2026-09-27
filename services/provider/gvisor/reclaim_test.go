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
