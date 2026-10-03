package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLogReopenReopensOnEachHangup(t *testing.T) {
	reopened := make(chan string)
	task := logReopen{path: "/var/log/shard/daemon.log", limit: logCap, interval: time.Hour, hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(path string) error {
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
	task := logReopen{path: "/var/log/shard/daemon.log", limit: logCap, interval: time.Hour, hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(string) error { return refused }}
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

// Past its cap the log moves to the one overflow file, replacing the one before, and the daemon writes a fresh one.
func TestLogCapMovesALogPastItsCapAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	full := bytes.Repeat([]byte("x"), 32)
	writeFile(t, path, full)
	writeFile(t, path+LogOverflow, []byte("the one before\n"))

	reopened := make(chan string, 1)
	var out bytes.Buffer
	task := logReopen{path: path, limit: 16, interval: 10 * time.Millisecond, hangups: make(chan os.Signal, 1), out: &out, reopen: func(path string) error {
		reopened <- path
		return os.WriteFile(path, nil, 0o600)
	}}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	select {
	case got := <-reopened:
		if got != path {
			t.Errorf("reopened %s, want %s", got, path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a log past its cap was never moved aside")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the task ended with %v", err)
	}

	if got := readFile(t, path+LogOverflow); !bytes.Equal(got, full) {
		t.Errorf("the overflow holds %q, want the capped log", got)
	}
	if got := readFile(t, path); len(got) != 0 {
		t.Errorf("the log holds %q after the move, want a fresh one", got)
	}
	if want := "daemon moved " + path + " to " + path + LogOverflow + " at 32 bytes, past its cap of 16"; strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), want) {
		t.Errorf("logged %q, want one line with %q", out.String(), want)
	}
}

func TestLogCapLeavesALogItNeedNotMove(t *testing.T) {
	for name, size := range map[string]int{"under its cap": 15, "moved by newsyslog": -1} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			if size >= 0 {
				writeFile(t, path, bytes.Repeat([]byte("x"), size))
			}
			task := logReopen{path: path, limit: 16, hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(string) error {
				t.Error("reopened a log it need not move")
				return nil
			}}

			if err := task.capLog(); err != nil {
				t.Fatalf("capLog: %v", err)
			}
			if _, err := os.Stat(path + LogOverflow); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the overflow is there (%v), want none", err)
			}
		})
	}
}

// The daemon still writes into the moved file, so the restart must reopen the path without waiting for a rotation.
func TestLogCapKeepsAHangupWhenTheReopenFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, path, bytes.Repeat([]byte("x"), 32))
	refused := errors.New("permission denied")
	task := logReopen{path: path, limit: 16, hangups: make(chan os.Signal, 1), out: io.Discard, reopen: func(string) error { return refused }}

	if err := task.capLog(); !errors.Is(err, refused) {
		t.Fatalf("capLog = %v, want %v", err, refused)
	}
	select {
	case <-task.hangups:
	default:
		t.Error("no hangup is queued, so the restart leaves the daemon writing into the overflow")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return data
}
