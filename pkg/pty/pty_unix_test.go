//go:build linux || darwin

package pty

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A pair that cannot be allocated must say why. The close that follows it succeeds, and an error
// nobody made is not part of the reason.
func TestOpenReportsOnlyTheFailureThatHappened(t *testing.T) {
	notAMultiplexer := filepath.Join(t.TempDir(), "ptmx")
	if err := os.WriteFile(notAMultiplexer, nil, 0o600); err != nil {
		t.Fatalf("write the stand-in for the multiplexer: %v", err)
	}

	previous := ptmx
	ptmx = notAMultiplexer
	t.Cleanup(func() { ptmx = previous })

	pair, err := Open()
	if err == nil {
		t.Fatalf("Open gave a pair %+v from a file that is no multiplexer", pair)
	}

	if strings.Contains(err.Error(), "%!") {
		t.Errorf("Open reported %q, and a formatting verb with nothing to print is not a reason", err)
	}
}

// openEchoing is a pair whose replica echoes, the way a shell leaves a terminal for the next command.
func openEchoing(t *testing.T) *Pty {
	t.Helper()

	pair, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := pair.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if !echoes(t, pair.Replica) {
		t.Fatal("a fresh replica does not echo")
	}

	return pair
}

func echoes(t *testing.T, f *os.File) bool {
	t.Helper()

	settings, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	if err != nil {
		t.Fatalf("read the terminal settings: %v", err)
	}

	return settings.Lflag&unix.ECHO != 0
}

func TestReadPasswordReadsOneLineAndGivesTheEchoBack(t *testing.T) {
	pair := openEchoing(t)

	if _, err := pair.Master.WriteString("s3cr3t-value\n"); err != nil {
		t.Fatalf("type the line: %v", err)
	}

	line, err := ReadPassword(t.Context(), pair.Replica)
	if err != nil || string(line) != "s3cr3t-value" {
		t.Fatalf("ReadPassword = %q, %v, want the line without its newline", line, err)
	}
	if !echoes(t, pair.Replica) {
		t.Error("the echo stayed off after the line was read")
	}
}

// A stop signal at the prompt cancels ctx; the read must end on it, drop the half-typed line and leave the terminal echoing.
func TestReadPasswordEndsOnCancelAndGivesTheEchoBack(t *testing.T) {
	pair := openEchoing(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := ReadPassword(ctx, pair.Replica)
		done <- err
	}()

	for deadline := time.Now().Add(2 * time.Second); echoes(t, pair.Replica); {
		if time.Now().After(deadline) {
			t.Fatal("the prompt never turned the echo off")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := pair.Master.WriteString("synthetic-partial"); err != nil {
		t.Fatalf("type half a line: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled prompt returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled prompt still waits for a line")
	}
	if !echoes(t, pair.Replica) {
		t.Error("a cancelled prompt left the echo off")
	}

	if _, err := pair.Master.WriteString("next\n"); err != nil {
		t.Fatalf("type the next line: %v", err)
	}
	next, cancelNext := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelNext()
	line, err := ReadPassword(next, pair.Replica)
	if err != nil || string(line) != "next" {
		t.Errorf("the next reader got %q, %v, want only the next line", line, err)
	}
}
