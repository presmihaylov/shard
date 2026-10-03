package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestLogReopenReopensOnEachHangup(t *testing.T) {
	reopened := make(chan string)
	task := logReopen{path: "/var/log/shard/daemon.log", hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(path string) error {
		reopened <- path
		return nil
	}}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	for range 2 {
		task.hangups <- syscall.SIGHUP
		select {
		case got := <-reopened:
			if got != task.path {
				t.Errorf("reopened %s, want %s", got, task.path)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a hangup reopened nothing")
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the task ended with %v", err)
	}
}

// A failed reopen leaves the daemon writing into the rotated file, so the restart must try again without a second rotation.
func TestLogReopenKeepsTheHangupItCouldNotServe(t *testing.T) {
	refused := errors.New("permission denied")
	task := logReopen{path: "/var/log/shard/daemon.log", hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(string) error { return refused }}
	task.hangups <- syscall.SIGHUP

	if err := task.Run(t.Context()); !errors.Is(err, refused) {
		t.Fatalf("Run = %v, want %v", err, refused)
	}
	select {
	case <-task.hangups:
	default:
		t.Error("the failed hangup is gone, so the restart waits for the next rotation")
	}
}
