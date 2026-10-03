//go:build integration && darwin

package hostclean

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A shim carries no mark but the socket in its config, so that is what a sweep goes by, and a shim under another root stays.
func TestSweepKillsTheShimsUnderARootOfOursAndNoOther(t *testing.T) {
	const name = "shard-hostclean-darwin-"
	ours, err := os.MkdirTemp("", name) //nolint:usetesting // the root must sit under the prefix the sweep is given
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(ours); err != nil {
			t.Error(err)
		}
	})
	mine, mineExited := standIn(t, filepath.Join(ours, "s", "vm-1", "shim.sock"))
	theirs, _ := standIn(t, filepath.Join(t.TempDir(), "s", "vm-1", "shim.sock"))
	prefix := filepath.Join(os.TempDir(), name)

	left, err := Find(prefix)
	if err != nil {
		t.Fatal(err)
	}
	named := make([]string, 0, len(left))
	for _, l := range left {
		named = append(named, l.String())
	}
	if want := []string{"the vm shim " + strconv.Itoa(mine), "the temp root " + ours}; !slices.Equal(named, want) {
		t.Fatalf("Find = %v, want %v", named, want)
	}

	if err := Sweep(prefix); err != nil {
		t.Fatal(err)
	}
	select {
	case <-mineExited:
	case <-time.After(shimGrace):
		t.Fatalf("the shim %d under our root still runs after the sweep", mine)
	}
	if err := syscall.Kill(theirs, 0); err != nil {
		t.Errorf("the shim %d under another root: %v", theirs, err)
	}
	if _, err := os.Stat(ours); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the root %s outlived the sweep: %v", ours, err)
	}
}

// standIn is a process whose arguments carry a shim config with this socket, reaped as pkg/vz reaps a shim.
func standIn(t *testing.T, socket string) (int, <-chan error) {
	t.Helper()

	config, err := json.Marshal(map[string]string{"socket": socket})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done", string(config))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
	})

	return cmd.Process.Pid, exited
}
