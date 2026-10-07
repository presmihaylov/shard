package main

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// superviseBounded runs one process that SIGKILLs itself, under a guest whose memory bound the probe says was or was not hit.
func superviseBounded(t *testing.T, oom, deaf bool) (*testGuest, *recordReporter) {
	t.Helper()

	report := newRecordReporter(t)
	report.deaf = deaf
	g := startGuestOver(t, report)
	g.run(func() { g.oomProbe = func() (bool, error) { return oom, nil } })
	spec := named("victim", "sigkill:0")
	spec.Restart = models.RestartAlways
	mustRun(t, g, spec)

	return g, report
}

func (r *recordReporter) oomCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.ooms
}

// A SIGKILL the memory bound made is reported with no exit and no start again, and the host's stop, once it holds the reason, ends the guest.
func TestAKillUnderTheMemoryBoundEndsTheGuestOnTheHostsStop(t *testing.T) {
	g, report := superviseBounded(t, true, false)

	waitFor(t, 5*time.Second, "the OOM report", func() bool { return report.oomCount() == 1 })
	if ended, err := g.awaitEnd(200 * time.Millisecond); ended {
		t.Fatalf("the guest ended before the host acknowledged the kill: %v", err)
	}
	for _, p := range report.of("victim") {
		if p.State != models.ProcessRunning {
			t.Fatalf("a kill the bound made was reported as %+v", p)
		}
	}
	if err := g.runSpec(named("after", "say:hi")); err == nil {
		t.Fatal("a run started after the bound took every process")
	}
	g.stopSignals <- syscall.SIGTERM
	if ended, err := g.awaitEnd(5 * time.Second); !ended || err != nil {
		t.Fatalf("the guest did not end on the host's stop: ended %t, %v", ended, err)
	}
}

// A SIGKILL from anywhere else is an exit like any other: the status lands, the policy applies, and the sandbox outlives it.
func TestAKillOutsideTheMemoryBoundIsAnExit(t *testing.T) {
	g, report := superviseBounded(t, false, false)

	restarting := report.await(t, "victim", models.ProcessRestarting)
	if restarting.Exit == nil || restarting.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("victim = %+v, want an exit on signal 9", restarting)
	}
	if ended, err := g.awaitEnd(200 * time.Millisecond); ended {
		t.Fatalf("the guest ended on a plain kill: %v", err)
	}
	if n := report.oomCount(); n != 0 {
		t.Fatalf("a plain kill was reported as %d OOMs", n)
	}
}

// A kill nobody heard keeps the guest up, so the reason survives until a host connects and reads it in the replay.
func TestAKillWithNoHostAttachedWaitsForTheReplay(t *testing.T) {
	g, report := superviseBounded(t, true, true)

	waitFor(t, 5*time.Second, "the OOM report", func() bool { return report.oomCount() == 1 })
	if ended, err := g.awaitEnd(200 * time.Millisecond); ended {
		t.Fatalf("the guest ended with no host to tell: %v", err)
	}
	var oom bool
	g.run(func() { oom = g.oom })
	if !oom {
		t.Fatal("the guest forgot the kill")
	}
}

// An exec's SIGKILL under the bound still reaches its session, so the host reads 137 and not a hang.
func TestAKillUnderTheMemoryBoundStillAnswersAnExec(t *testing.T) {
	report := newRecordReporter(t)
	g := startGuestOver(t, report)
	g.run(func() { g.oomProbe = func() (bool, error) { return true, nil } })

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := devNull.Close(); err != nil {
			t.Error(err)
		}
	})
	_, exit, err := g.spawn(spawnSpec{argv: childArgv("sigkill:0"), env: os.Environ()}, []*os.File{devNull, os.Stdout, os.Stderr}, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-exit:
		if status.Signal != int(syscall.SIGKILL) {
			t.Fatalf("the exec ended with %+v, want signal 9", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the exec never heard its own exit")
	}
	if ended, err := g.awaitEnd(200 * time.Millisecond); ended {
		t.Fatalf("the guest ended on an exec's kill: %v", err)
	}
}
