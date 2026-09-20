package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// A reporter that keeps what the guest said, so a test reads the decision without a file or a socket.
type memoryReporter struct {
	exits chan models.ExitStatus
	ooms  chan struct{}
	// deaf is a reporter with no host attached, which the OOM report must say rather than drop.
	deaf bool
}

func (r memoryReporter) ready() error                        { return nil }
func (r memoryReporter) exited(exit models.ExitStatus) error { r.exits <- exit; return nil }
func (memoryReporter) restarted(models.RestartCount) error   { return nil }
func (r memoryReporter) oomKilled() error {
	r.ooms <- struct{}{}
	if r.deaf {
		return errNoHost
	}

	return nil
}

func superviseBounded(t *testing.T, oom, deaf bool) (*guest, memoryReporter, chan error) {
	t.Helper()

	report := memoryReporter{exits: make(chan models.ExitStatus, 1), ooms: make(chan struct{}, 1), deaf: deaf}
	g := newGuest(report, restartPolicy{policy: models.RestartNo})
	g.oomProbe = func() (bool, error) { return oom, nil }
	if err := g.launch(entrypoint{argv: []string{os.Args[0], childPrefix + "sigkill:0"}, env: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.supervise() }()

	return g, report, done
}

// A SIGKILL the memory bound made is reported with no exit record, and the host's stop, once it holds the reason, ends the guest.
func TestAKillUnderTheMemoryBoundEndsTheGuestOnTheHostsStop(t *testing.T) {
	g, report, done := superviseBounded(t, true, false)

	select {
	case <-report.ooms:
	case <-time.After(5 * time.Second):
		t.Fatal("no OOM report within 5s")
	}
	select {
	case err := <-done:
		t.Fatalf("the guest ended before the host acknowledged the kill: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	g.stopSignals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest kept running after the host's stop")
	}
	if len(report.exits) != 0 {
		t.Fatal("an exit was recorded for a kill the bound made")
	}
}

// A SIGKILL from anywhere else is an exit like any other: the record lands and the sandbox outlives it.
func TestAKillOutsideTheMemoryBoundIsAnExit(t *testing.T) {
	g, report, done := superviseBounded(t, false, false)

	select {
	case exit := <-report.exits:
		if exit.Signal != 9 {
			t.Fatalf("exit = %+v, want signal 9", exit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit within 5s")
	}
	select {
	case err := <-done:
		t.Fatalf("the guest ended on a plain kill: %v", errors.Join(err))
	case <-time.After(200 * time.Millisecond):
	}
	if len(report.ooms) != 0 {
		t.Fatal("a plain kill was reported as an OOM")
	}
	// Every guest in this process hears SIGCHLD, so one left running would reap the next test's child.
	g.stopSignals <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest did not end on a stop with nothing to forward to")
	}
}

// A kill nobody heard keeps the guest up, so the reason survives until a host connects and reads it in the replay.
func TestAKillWithNoHostAttachedWaitsForTheReplay(t *testing.T) {
	g, report, done := superviseBounded(t, true, true)

	select {
	case <-report.ooms:
	case <-time.After(5 * time.Second):
		t.Fatal("no OOM report within 5s")
	}
	select {
	case err := <-done:
		t.Fatalf("the guest ended with no host to tell: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	var oom bool
	g.run(func() { oom = g.oom })
	if !oom {
		t.Fatal("the guest forgot the kill")
	}
	g.stopSignals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest kept running after the host's stop")
	}
}
