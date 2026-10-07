//go:build integration

package gvisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// A Ctrl-C anywhere in a create leaves no process of the sandbox behind, and rm then frees the rest.
func TestACancelledCreateLeavesNothingRmCannotFree(t *testing.T) {
	h := newHarness(t)

	for _, after := range []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond, time.Second} {
		spec := h.newSpec(t)

		ctx, cancel := context.WithCancel(t.Context())
		timer := time.AfterFunc(after, cancel)
		err := h.provider.Create(ctx, spec)
		timer.Stop()
		cancel()
		t.Logf("a cancel after %s: Create returned %v", after, err)

		// A create that finished before the cancel is an ordinary sandbox, and only one that did not must have left nothing running.
		if err != nil {
			assertNoProcessNames(t, spec.ID)
		}

		if err := h.provider.Remove(context.Background(), spec.ID); err != nil {
			t.Fatalf("Remove after a cancel at %s: %v", after, err)
		}
		assertFreed(t, h, spec.ID)
	}
}

// A create killed before runsc saved its state leaves a sentry and a gofer runsc cannot name, and rm must still end them.
func TestRemoveEndsASandboxRunscNeverSaved(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t)

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	h.forgetTheSandbox(t, spec.ID)

	if len(namedProcesses(t, spec.ID)) == 0 {
		t.Fatalf("sandbox %s has no process left for runsc to lose", spec.ID)
	}

	if err := h.provider.Remove(t.Context(), spec.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	assertFreed(t, h, spec.ID)
}

// forgetTheSandbox leaves what a killed runsc create leaves: the lock but no state file, the processes alive, and the rootfs the failed create unmounted.
func (h *harness) forgetTheSandbox(t *testing.T, id string) {
	t.Helper()

	states, err := filepath.Glob(filepath.Join(h.runscRoot, "*_sandbox:"+id+".state"))
	if err != nil || len(states) != 1 {
		t.Fatalf("find the runsc state of %s: %v %v", id, states, err)
	}
	if err := os.Remove(states[0]); err != nil {
		t.Fatalf("drop the runsc state of %s: %v", id, err)
	}

	dir, err := h.stateDir(id)
	if err != nil {
		t.Fatalf("the state directory of %s: %v", id, err)
	}
	b, err := bundle.Open(dir)
	if err != nil {
		t.Fatalf("open the bundle of %s: %v", id, err)
	}
	if err := b.Unmount(); err != nil {
		t.Fatalf("unmount the rootfs of %s: %v", id, err)
	}
}

// assertFreed proves the host holds nothing of the sandbox: no runsc state, no process, no cgroup and no mount.
func assertFreed(t *testing.T, h *harness, id string) {
	t.Helper()

	status, err := h.provider.Status(t.Context(), id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Exists {
		t.Errorf("runsc still holds sandbox %s after Remove", id)
	}

	assertNoProcessNames(t, id)

	if _, err := cgroup.Procs(filepath.Join(cgroup.Root, bundle.CgroupsPath(id))); !errors.Is(err, cgroup.ErrNotFound) {
		t.Errorf("the cgroup of %s is still there after Remove: %v", id, err)
	}

	assertMounted(t, h, id, false)
}

// assertNoProcessNames waits out an exit a kill has already started, rather than race it.
func assertNoProcessNames(t *testing.T, id string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		left := namedProcesses(t, id)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes %v still name sandbox %s", left, id)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// namedProcesses lists every live process whose command line carries the id as one whole argument.
func namedProcesses(t *testing.T, id string) []string {
	t.Helper()

	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Fatalf("list /proc: %v", err)
	}

	var named []string
	for _, path := range cmdlines {
		raw, err := os.ReadFile(path)
		if gvisor.Vanished(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// A zombie's command line reads empty, so only a process that still runs can match.
		if slices.Contains(strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), id) {
			named = append(named, filepath.Base(filepath.Dir(path)))
		}
	}

	return named
}
