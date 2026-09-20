package main

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// A reporter that keeps what the guest said, so a test reads the decision without a file or a socket.
type memoryReporter struct {
	exits chan models.ExitStatus
	ooms  chan struct{}
}

func (r memoryReporter) ready() error                        { return nil }
func (r memoryReporter) exited(exit models.ExitStatus) error { r.exits <- exit; return nil }
func (memoryReporter) restarted(models.RestartCount) error   { return nil }
func (r memoryReporter) oomKilled() error                    { r.ooms <- struct{}{}; return nil }

func superviseBounded(t *testing.T, oom bool) (memoryReporter, chan error) {
	t.Helper()

	report := memoryReporter{exits: make(chan models.ExitStatus, 1), ooms: make(chan struct{}, 1)}
	g := newGuest(report, restartPolicy{policy: models.RestartNo})
	g.oomProbe = func() (bool, error) { return oom, nil }
	if err := g.launch(entrypoint{argv: []string{os.Args[0], childPrefix + "sigkill:0"}, env: os.Environ()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.supervise() }()

	return report, done
}

// A SIGKILL the memory bound made ends the guest with the OOM report and no exit record, as a whole Linux sandbox dies.
func TestAKillUnderTheMemoryBoundEndsTheGuest(t *testing.T) {
	report, done := superviseBounded(t, true)

	select {
	case <-report.ooms:
	case <-time.After(5 * time.Second):
		t.Fatal("no OOM report within 5s")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest kept running after the OOM report")
	}
	if len(report.exits) != 0 {
		t.Fatal("an exit was recorded for a kill the bound made")
	}
}

// A SIGKILL from anywhere else is an exit like any other: the record lands and the sandbox outlives it.
func TestAKillOutsideTheMemoryBoundIsAnExit(t *testing.T) {
	report, done := superviseBounded(t, false)

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
}
