package cgroup_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

// fake stands in for a cgroup directory: the control files are ordinary files, and the driver only
// ever writes to one that already exists.
func fake(t *testing.T, contents string) string {
	t.Helper()

	dir := t.TempDir()
	for _, file := range []string{"memory.high", "memory.max", "memory.swap.max"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatalf("write the control file: %v", err)
		}
	}

	return dir
}

func TestSetAndReadTheThrottle(t *testing.T) {
	dir := fake(t, "max\n")

	if err := cgroup.SetMemoryHigh(dir, 134217728); err != nil {
		t.Fatalf("SetMemoryHigh: %v", err)
	}

	got, err := cgroup.MemoryHigh(dir)
	if err != nil {
		t.Fatalf("MemoryHigh: %v", err)
	}
	if got != 134217728 {
		t.Fatalf("MemoryHigh = %d, want 134217728", got)
	}
}

func TestMaxReadsAsNoThrottle(t *testing.T) {
	got, err := cgroup.MemoryHigh(fake(t, "max\n"))
	if err != nil {
		t.Fatalf("MemoryHigh: %v", err)
	}
	if got != -1 {
		t.Fatalf("MemoryHigh = %d, want -1 for max", got)
	}
}

func TestACgroupThatIsGoneIsNamedAsSuch(t *testing.T) {
	err := cgroup.SetMemoryHigh(filepath.Join(t.TempDir(), "nothing"), 1)
	if !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("SetMemoryHigh on a missing cgroup = %v, want ErrNotFound", err)
	}
}

func TestReadTheCeiling(t *testing.T) {
	got, err := cgroup.MemoryMax(fake(t, "201326592\n"))
	if err != nil {
		t.Fatalf("MemoryMax: %v", err)
	}
	if got != 201326592 {
		t.Fatalf("MemoryMax = %d, want 201326592", got)
	}
}

func TestMaxReadsAsNoCeiling(t *testing.T) {
	got, err := cgroup.MemoryMax(fake(t, "max\n"))
	if err != nil {
		t.Fatalf("MemoryMax: %v", err)
	}
	if got != -1 {
		t.Fatalf("MemoryMax = %d, want -1 for max", got)
	}
}

func TestACeilingThatIsNotANumberIsAFailure(t *testing.T) {
	if _, err := cgroup.MemoryMax(fake(t, "not a number\n")); err == nil {
		t.Fatal("MemoryMax on a control file that holds no number = nil, want an error")
	}
}

func TestPinningTheSwapToNone(t *testing.T) {
	dir := fake(t, "max\n")

	if err := cgroup.SetMemorySwapMax(dir, 0); err != nil {
		t.Fatalf("SetMemorySwapMax: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "memory.swap.max"))
	if err != nil {
		t.Fatalf("read memory.swap.max: %v", err)
	}
	if string(raw) != "0" {
		t.Fatalf("memory.swap.max = %q, want 0", raw)
	}
}

func TestMovingAProcessIntoTheCgroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), nil, 0o600); err != nil {
		t.Fatalf("write cgroup.procs: %v", err)
	}

	if err := cgroup.Add(dir, 4171); err != nil {
		t.Fatalf("Add: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		t.Fatalf("read cgroup.procs: %v", err)
	}
	if string(raw) != "4171" {
		t.Fatalf("cgroup.procs = %q, want 4171", raw)
	}
}

// The kernel takes one controller per write, with the sign in front, and answers the read as a plain list.
func TestDelegatingAControllerToTheChildren(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), nil, 0o600); err != nil {
		t.Fatalf("write cgroup.subtree_control: %v", err)
	}

	if err := cgroup.Delegate(dir, "memory"); err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		t.Fatalf("read cgroup.subtree_control: %v", err)
	}
	if string(raw) != "+memory" {
		t.Fatalf("cgroup.subtree_control = %q, want +memory", raw)
	}
}

// A sysbox guest's runc walks the shard parent as an unprivileged host uid, so a missing one is made 0755 and a stricter one opened.
func TestEnsureParentLeavesTheCgroupAt0755(t *testing.T) {
	for name, existing := range map[string]os.FileMode{"missing": 0, "0750": 0o750, "0700": 0o700} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shard")
			if existing != 0 {
				if err := os.Mkdir(dir, existing); err != nil {
					t.Fatalf("make the cgroup: %v", err)
				}
			}

			if err := cgroup.EnsureParent(dir); err != nil {
				t.Fatalf("EnsureParent: %v", err)
			}

			info, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat the cgroup: %v", err)
			}
			if mode := info.Mode().Perm(); mode != 0o755 {
				t.Fatalf("the cgroup is %#o, want 0755", mode)
			}
		})
	}
}

// A sandbox's own cgroup holds its pids and usage, so no other host user may read them.
func TestEnsureKeepsASandboxCgroupPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shard", "sandbox")
	if err := cgroup.Ensure(dir); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the cgroup: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o007 != 0 {
		t.Fatalf("the sandbox cgroup is %#o, want nothing for other users", mode)
	}
}

// TestReadingTheEventCounters pins the parse against the real file's shape, which is one key and one
// count per line, with keys this driver does not use mixed in.
func TestReadingTheEventCounters(t *testing.T) {
	dir := t.TempDir()
	body := "low 0\nhigh 355\nmax 12\noom 3\noom_kill 1\noom_group_kill 0\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(body), 0o600); err != nil {
		t.Fatalf("write memory.events: %v", err)
	}

	got, err := cgroup.MemoryEvents(dir)
	if err != nil {
		t.Fatalf("MemoryEvents: %v", err)
	}
	if got != (cgroup.Events{OOM: 3, OOMKill: 1, High: 355}) {
		t.Fatalf("MemoryEvents = %+v, want {OOM:3 OOMKill:1 High:355}", got)
	}
}

// TestTheLocalEventCountersReadTheirOwnFile keeps a descendant's OOM out of the answer: only the local file says what hit this bound.
func TestTheLocalEventCountersReadTheirOwnFile(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"memory.events": "oom 3\noom_kill 3\n", "memory.events.local": "oom 1\noom_kill 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := cgroup.LocalMemoryEvents(dir)
	if err != nil {
		t.Fatalf("LocalMemoryEvents: %v", err)
	}
	if got != (cgroup.Events{OOM: 1}) {
		t.Fatalf("LocalMemoryEvents = %+v, want {OOM:1 OOMKill:0}", got)
	}
}

// TestTheEventsOfACgroupThatIsGone is the ordinary case for a sandbox that stopped cleanly: runsc
// removed the cgroup, so there is nothing to read and nothing to report.
func TestTheEventsOfACgroupThatIsGone(t *testing.T) {
	_, err := cgroup.MemoryEvents(filepath.Join(t.TempDir(), "never-made-2b3c"))

	if !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("MemoryEvents of a missing cgroup = %v, want ErrNotFound", err)
	}
}

func TestPopulatedReadsTheEventsField(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "cgroup.events")
	write := func(body string) {
		if err := os.WriteFile(events, []byte(body), 0o600); err != nil {
			t.Fatalf("write cgroup.events: %v", err)
		}
	}

	write("populated 1\nfrozen 0\n")
	if pop, err := cgroup.Populated(dir); err != nil || !pop {
		t.Fatalf("Populated over a live cgroup = %v, %v, want true", pop, err)
	}

	write("populated 0\nfrozen 0\n")
	if pop, err := cgroup.Populated(dir); err != nil || pop {
		t.Fatalf("Populated over an idle cgroup = %v, %v, want false", pop, err)
	}
}

func TestPopulatedOfAGoneCgroupIsNotFound(t *testing.T) {
	if _, err := cgroup.Populated(filepath.Join(t.TempDir(), "gone")); !errors.Is(err, cgroup.ErrNotFound) {
		t.Fatalf("Populated of a gone cgroup = %v, want ErrNotFound", err)
	}
}

// A cgroup with no cgroup.events, which only a test fake is, reads as idle so a teardown can remove it.
func TestPopulatedWithoutAnEventsFileIsIdle(t *testing.T) {
	if pop, err := cgroup.Populated(t.TempDir()); err != nil || pop {
		t.Fatalf("Populated with no events file = %v, %v, want false, nil", pop, err)
	}
}
