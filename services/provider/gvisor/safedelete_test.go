package gvisor_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// fakeRunsc records whether safeDelete dropped runsc's saved state; it holds no force delete to record.
type fakeRunsc struct {
	gvisor.RunscStub
	forgot bool
}

func (f *fakeRunsc) Forget(id string) error {
	f.forgot = true

	return nil
}

// TestSafeDeleteSweepsTheSandboxMembersThenForgets is the teardown: kill the sandbox's own sentry and gofer by cgroup, then drop the state.
func TestSafeDeleteSweepsTheSandboxMembersThenForgets(t *testing.T) {
	h := newHost(t)
	own := bundle.CgroupsPath(sandboxID)
	h.process(1101, own, "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)
	h.process(1102, own, "runsc-gofer", "gofer", "--bundle", bundleDir, sandboxID)

	p := h.provider()
	f := &fakeRunsc{}
	p.SetRunsc(f)

	if err := p.SafeDelete(context.Background(), sandboxID); err != nil {
		t.Fatalf("SafeDelete: %v", err)
	}

	if want := []int{1101, 1102}; !slices.Equal(h.killed, want) {
		t.Fatalf("SafeDelete killed %v, want the sentry and the gofer of %s: %v", h.killed, sandboxID, want)
	}
	if !f.forgot {
		t.Fatal("SafeDelete did not drop runsc's saved state after the sweep")
	}
}

// TestSafeDeleteNeverTouchesAReusedPidInAnotherCgroup is SHARD-440: the gofer exited and its pid now names a host process in another cgroup, which the sweep never enumerates.
func TestSafeDeleteNeverTouchesAReusedPidInAnotherCgroup(t *testing.T) {
	h := newHost(t)
	h.process(1101, bundle.CgroupsPath(sandboxID), "runsc-sandbox", "boot", "--bundle="+bundleDir, sandboxID)
	// pid 1102 was the gofer; a host process reused it and lives in its own cgroup.
	h.process(1102, "system.slice/cron.service", "sleep", "600")

	p := h.provider()
	f := &fakeRunsc{}
	p.SetRunsc(f)

	if err := p.SafeDelete(context.Background(), sandboxID); err != nil {
		t.Fatalf("SafeDelete: %v", err)
	}

	if want := []int{1101}; !slices.Equal(h.killed, want) {
		t.Fatalf("SafeDelete killed %v, want only the sandbox's own sentry 1101", h.killed)
	}
	if !f.forgot {
		t.Fatal("SafeDelete did not drop runsc's saved state")
	}
}

// TestSafeDeleteForgetsWhenTheCgroupIsEmptyOrGone covers a sandbox whose processes already exited, or one runsc never started: nothing to kill, drop the state.
func TestSafeDeleteForgetsWhenTheCgroupIsEmptyOrGone(t *testing.T) {
	h := newHost(t)

	p := h.provider()
	f := &fakeRunsc{}
	p.SetRunsc(f)

	if err := p.SafeDelete(context.Background(), sandboxID); err != nil {
		t.Fatalf("SafeDelete: %v", err)
	}

	if len(h.killed) != 0 {
		t.Fatalf("SafeDelete killed %v with nothing of the sandbox to kill", h.killed)
	}
	if !f.forgot {
		t.Fatal("SafeDelete did not drop the state of an already empty sandbox")
	}
}

// TestSafeDeleteKeepsTheStateWhenASweepRefuses: a stranger holds the cgroup and names nothing, so the sweep refuses and the state must survive for a retry.
func TestSafeDeleteKeepsTheStateWhenASweepRefuses(t *testing.T) {
	h := newHost(t)
	h.process(1103, bundle.CgroupsPath(sandboxID), "bash")

	p := h.provider()
	f := &fakeRunsc{}
	p.SetRunsc(f)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := p.SafeDelete(ctx, sandboxID); err == nil {
		t.Fatal("SafeDelete passed a cgroup a stranger still holds")
	}

	if len(h.killed) != 0 {
		t.Fatalf("SafeDelete killed %v, a process that does not name %s", h.killed, sandboxID)
	}
	if f.forgot {
		t.Fatal("SafeDelete dropped the state although the sweep refused the cgroup")
	}
}
