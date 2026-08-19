package runsc_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/runsc"
)

// fake stands in for the binary: it records the argv it was called with, then prints what a test asked for.
func fake(t *testing.T, stdout, stderr string, exitCode int) (*runsc.Runner, string) {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	binary := filepath.Join(dir, "runsc")

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"printf '%s' '" + stdout + "'\n" +
		"printf '%s' '" + stderr + "' >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"

	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake runsc: %v", err)
	}

	r, err := runsc.New(filepath.Join(dir, "root"), runsc.WithBinary(binary))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return r, argvFile
}

func argv(t *testing.T, path string) []string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake runsc recorded no argv: %v", err)
	}

	return strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
}

func TestNewRefusesARelativeRoot(t *testing.T) {
	if _, err := runsc.New("var/lib/shard/runsc"); err == nil {
		t.Fatal("New accepted a relative runsc root")
	}
}

func TestNewRefusesABinaryThatIsNotThere(t *testing.T) {
	if _, err := runsc.New(t.TempDir(), runsc.WithBinary("runsc-that-does-not-exist")); err == nil {
		t.Fatal("New accepted a binary it cannot find")
	}
}

func TestStateParsesWhatRunscPrints(t *testing.T) {
	r, _ := fake(t, `{"id":"amber-otter-1a2b","status":"running","pid":4242,"bundle":"/var/lib/shard/sandboxes/x/bundle"}`, "", 0)

	state, err := r.State(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if state.Status != runsc.StatusRunning {
		t.Errorf("got status %q, want running", state.Status)
	}
	if state.PID != 4242 {
		t.Errorf("got pid %d, want 4242", state.PID)
	}
}

// The overlay flag is the one that matters: runsc's own filestore would throw the guest's writes away.
func TestEveryCommandCarriesTheGlobalFlags(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	if err := r.Start(t.Context(), "amber-otter-1a2b"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := argv(t, recorded)
	for _, want := range []string{"--root", r.Root(), "--network=none", "--overlay2=none", "start", "amber-otter-1a2b"} {
		if !slices.Contains(got, want) {
			t.Errorf("got argv %v, which is missing %q", got, want)
		}
	}
}

func TestKillAllReachesEveryProcess(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	if err := r.Kill(t.Context(), "amber-otter-1a2b", "KILL", true); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	want := []string{"kill", "--all", "amber-otter-1a2b", "KILL"}
	if got := argv(t, recorded); !strings.Contains(strings.Join(got, " "), strings.Join(want, " ")) {
		t.Errorf("got argv %v, want it to end with %v", got, want)
	}
}

func TestASandboxRunscDoesNotHoldIsNotFound(t *testing.T) {
	r, _ := fake(t, "", "FetchSpec failed: loading container: file does not exist", 1)

	if _, err := r.State(t.Context(), "amber-otter-1a2b"); !errors.Is(err, runsc.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A stop kills a sandbox that may already be dead, and that must not read as a failure.
func TestKillingADeadSandboxIsNotRunning(t *testing.T) {
	r, _ := fake(t, "", "sandbox is not running", 1)

	if err := r.Kill(t.Context(), "amber-otter-1a2b", "TERM", false); !errors.Is(err, runsc.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}

func TestAFailureKeepsWhatRunscSaid(t *testing.T) {
	r, _ := fake(t, "", "some new gvisor failure", 1)

	err := r.Delete(t.Context(), "amber-otter-1a2b", false)
	if err == nil {
		t.Fatal("Delete reported success on a failing runsc")
	}
	if !strings.Contains(err.Error(), "some new gvisor failure") {
		t.Errorf("got %q, want it to carry what runsc printed", err)
	}
}

func TestCreateRefusesWithNoBundle(t *testing.T) {
	r, _ := fake(t, "", "", 0)

	if err := r.Create(t.Context(), "amber-otter-1a2b", runsc.CreateOptions{}); err == nil {
		t.Fatal("Create accepted a container with no bundle")
	}
}
