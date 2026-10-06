package supervisor_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/supervisor"
)

// guestDialer hands Exec one end of a pipe, and the guest end to serve once it has read the exec header; that end stays open until the test ends (SHARD-471).
func guestDialer(t *testing.T, serve func(net.Conn) error) supervisor.Dialer {
	t.Helper()

	return func(context.Context, uint32) (net.Conn, error) {
		host, guest := net.Pipe()
		// closing silences the teardown's own close, and done makes the cleanup wait so no assertion runs after the test returns (SHARD-572).
		closing, done := make(chan struct{}), make(chan struct{})
		t.Cleanup(func() {
			close(closing)
			guest.Close()
			<-done
		})
		fail := func(format string, err error) {
			select {
			case <-closing:
			default:
				t.Errorf(format, err)
			}
		}
		go func() {
			defer close(done)
			var header supervisor.ExecHeader
			if err := supervisor.ReadHeader(guest, &header); err != nil {
				fail("read the header: %v", err)

				return
			}
			if err := serve(guest); err != nil {
				fail("serve the exec: %v", err)
			}
		}()

		return host, nil
	}
}

// A guest that took the connection and never the exec is the listener SHARD-354 lost: the exec fails by name, not as running forever.
func TestExecFailsByNameWhenTheGuestNeverStartsIt(t *testing.T) {
	supervisor.SetStartTimeout(t, 100*time.Millisecond)
	dial := guestDialer(t, func(guest net.Conn) error {
		_, err := io.Copy(io.Discard, guest)

		return err
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"true"}}, models.ExecSpec{})
	if !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "the guest did not start it within 100ms") {
		t.Fatalf("exec gave %v, want the start bound by name", err)
	}
}

// The bound is on the start alone: a command that runs past it after its started frame still reports its exit.
func TestExecOutlivesTheStartBoundOnceStarted(t *testing.T) {
	supervisor.SetStartTimeout(t, 100*time.Millisecond)
	dial := guestDialer(t, func(guest net.Conn) error {
		go func() { _, _ = io.Copy(io.Discard, guest) }()
		if err := supervisor.WriteJSONFrame(guest, supervisor.StreamStarted, supervisor.StartedFrame{PID: 42}); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)

		return supervisor.WriteJSONFrame(guest, supervisor.StreamExit, supervisor.ExitFrame{Code: 7})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	exit, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"sleep"}}, models.ExecSpec{})
	if err != nil || exit.Code != 7 {
		t.Fatalf("exec gave %+v, %v, want code 7", exit, err)
	}
}

// A guest that took the connection and never reads the header holds the write, which the start bound ends by name too.
func TestExecFailsByNameWhenTheGuestNeverReadsTheHeader(t *testing.T) {
	supervisor.SetStartTimeout(t, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := supervisor.Exec(ctx, unreadDialer(t), "sb", supervisor.ExecHeader{Argv: []string{"true"}}, models.ExecSpec{})
	if !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "the guest did not start it within 100ms") {
		t.Fatalf("exec gave %v, want the start bound by name", err)
	}
}

// The caller's deadline ends a header write no guest reads, well before the start bound would.
func TestExecEndsByItsContextWhileTheHeaderWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := supervisor.Exec(ctx, unreadDialer(t), "sb", supervisor.ExecHeader{Argv: []string{"true"}}, models.ExecSpec{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec gave %v, want the caller's deadline", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("exec took %s on a deadline of 100ms", took)
	}
}

// unreadDialer hands Exec one end of a pipe whose guest end nobody reads, so the header write blocks.
func unreadDialer(t *testing.T) supervisor.Dialer {
	t.Helper()

	return func(context.Context, uint32) (net.Conn, error) {
		host, guest := net.Pipe()
		t.Cleanup(func() { guest.Close() })

		return host, nil
	}
}

// A cancel the guest never reads leaves the command running there, so the exec says so rather than end as a plain cancel (SHARD-562).
func TestACancelTheGuestNeverReadsIsReported(t *testing.T) {
	dial := guestDialer(t, func(guest net.Conn) error {
		return supervisor.WriteJSONFrame(guest, supervisor.StreamStarted, supervisor.StartedFrame{PID: 42})
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	began := time.Now()
	_, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"sleep"}}, models.ExecSpec{Report: func(int) { cancel() }})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "the command may run on") {
		t.Fatalf("exec gave %v, want the cancel's own failure beside the context's", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("exec took %s to give up a cancel bounded by a second", took)
	}
}

// A command whose input the host could not read may still exit 0, so the exec answers with the read that cut it short.
func TestAStdinTheHostCannotReadFailsTheExec(t *testing.T) {
	stdin, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	dial := guestDialer(t, func(guest net.Conn) error {
		if err := supervisor.WriteJSONFrame(guest, supervisor.StreamStarted, supervisor.StartedFrame{PID: 42}); err != nil {
			return err
		}
		for {
			kind, _, err := supervisor.ReadFrame(guest)
			if err != nil {
				return err
			}
			if kind == supervisor.StreamStdinClose {
				return supervisor.WriteJSONFrame(guest, supervisor.StreamExit, supervisor.ExitFrame{Code: 0})
			}
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"cat"}}, models.ExecSpec{Stdin: stdin})
	if err == nil || !strings.Contains(err.Error(), "read the exec's stdin") {
		t.Fatalf("exec gave %v, want the failed stdin read", err)
	}
}

// The guest's terminal does the line discipline, so the host replica passes every key on as typed and echoes none of them (SHARD-760).
func TestATerminalExecRelaysEveryKeyUntouched(t *testing.T) {
	pair, err := pty.Open()
	if errors.Is(err, pty.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pair.Close(); err != nil {
			t.Error(err)
		}
	})

	// Ctrl-C, Tab, arrow-up and Ctrl-R, with no Enter: a cooked replica eats Ctrl-C and holds the rest for a line that never ends.
	const keys, shown = "ab\x03\t\x1b[A\x12\r", "done"
	ready, got := make(chan struct{}), make(chan string, 1)
	dial := guestDialer(t, func(guest net.Conn) error {
		close(ready)
		if err := supervisor.WriteJSONFrame(guest, supervisor.StreamStarted, supervisor.StartedFrame{PID: 42}); err != nil {
			return err
		}
		var seen []byte
		for len(seen) < len(keys) {
			kind, payload, err := supervisor.ReadFrame(guest)
			if err != nil {
				return fmt.Errorf("the guest got %q of %q: %w", seen, keys, err)
			}
			if kind == supervisor.StreamStdin {
				seen = append(seen, payload...)
			}
		}
		got <- string(seen)
		if err := supervisor.WriteFrame(guest, supervisor.StreamStdout, []byte(shown)); err != nil {
			return err
		}

		return supervisor.WriteJSONFrame(guest, supervisor.StreamExit, supervisor.ExitFrame{Code: 3})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type result struct {
		exit models.ExitStatus
		err  error
	}
	done := make(chan result, 1)
	go func() {
		spec := models.ExecSpec{TTY: true, Stdin: pair.Replica, Stdout: pair.Replica, Stderr: pair.Replica}
		exit, err := supervisor.Exec(ctx, dial, "sb", supervisor.ExecHeader{Argv: []string{"bash"}, TTY: true}, spec)
		done <- result{exit, err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("the exec never reached the guest")
	}
	if _, err := pair.Master.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
	screen := make(chan result, 1)
	var text [len(shown)]byte
	go func() {
		_, err := io.ReadFull(pair.Master, text[:])
		screen <- result{err: err}
	}()

	select {
	case seen := <-got:
		if seen != keys {
			t.Fatalf("the guest got %q, want %q", seen, keys)
		}
	case <-ctx.Done():
		t.Fatal("the keys never reached the guest whole")
	}
	if r := <-done; r.err != nil || r.exit.Code != 3 {
		t.Fatalf("exec gave %+v, %v, want code 3", r.exit, r.err)
	}
	select {
	case r := <-screen:
		if r.err != nil || string(text[:]) != shown {
			t.Fatalf("the terminal shows %q, %v, want only the guest's %q", text, r.err, shown)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the guest's output never reached the terminal")
	}
}
