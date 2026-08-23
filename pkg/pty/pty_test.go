package pty_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/presmihaylov/shard/pkg/pty"
)

// A pair is a kernel call, so a developer Mac must say it cannot rather than fail the suite.
func TestOpenGivesAPairAndRefusesOffLinux(t *testing.T) {
	pair, err := pty.Open()

	if runtime.GOOS != "linux" {
		if !errors.Is(err, pty.ErrNotLinux) {
			t.Fatalf("Open returned %v, want ErrNotLinux off Linux", err)
		}

		return
	}

	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := pair.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	want := pty.Size{Rows: 40, Cols: 120}
	if err := pair.Resize(want); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	got, err := pty.SizeOf(pair.Replica)
	if err != nil {
		t.Fatalf("SizeOf: %v", err)
	}
	if got != want {
		t.Errorf("the replica reports %+v, want %+v", got, want)
	}

	if !pty.IsTerminal(pair.Replica) {
		t.Error("the replica of a pair is not a terminal")
	}
}

func TestIsTerminalSaysNoToAPlainFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "not-a-terminal"))
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}
	defer f.Close()

	if pty.IsTerminal(f) {
		t.Error("a plain file reports as a terminal")
	}
	if pty.IsTerminal(nil) {
		t.Error("no file at all reports as a terminal")
	}
}
