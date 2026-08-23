package runsc_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/runsc"
)

// fake stands in for the binary: it records the argv it was called with, then prints what a test asked for.
func fake(t *testing.T, stdout, stderr string, exitCode int) (*runsc.Runner, string) {
	t.Helper()

	return fakeBinary(t, "printf '%s' '"+stdout+"'\n"+
		"printf '%s' '"+stderr+"' >&2\n"+
		"exit "+strconv.Itoa(exitCode)+"\n")
}

// fakeBinary writes a fake runsc that records its argv and then runs body. Body is what a test varies.
func fakeBinary(t *testing.T, body string) (*runsc.Runner, string) {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	binary := filepath.Join(dir, "runsc")

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" + body

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

// TestCreateQuotesItsDiagnostics: the caller deletes the state directory when a create fails, so a
// message that only named the log would send the operator to a file that is already gone.
func TestCreateQuotesItsDiagnostics(t *testing.T) {
	r, _ := fake(t, "", "creating container: mount source /tmp/absent does not exist", 1)

	log := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(log, []byte("output from an earlier run\n"), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close the log: %v", err)
		}
	}()

	err = r.Create(t.Context(), "amber-otter-1a2b", runsc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if err == nil {
		t.Fatal("Create reported success on a failing runsc")
	}

	if !strings.Contains(err.Error(), "mount source /tmp/absent does not exist") {
		t.Errorf("got %q, want it to carry what runsc printed", err)
	}
	if strings.Contains(err.Error(), "output from an earlier run") {
		t.Errorf("got %q, want only this create's own output", err)
	}
}

// TestCreateReportsThatRunscPrintedNothing: a failure that left no output still has to read as one.
func TestCreateReportsThatRunscPrintedNothing(t *testing.T) {
	r, _ := fake(t, "", "", 1)

	log := filepath.Join(t.TempDir(), "output.log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close the log: %v", err)
		}
	}()

	err = r.Create(t.Context(), "amber-otter-1a2b", runsc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if err == nil {
		t.Fatal("Create reported success on a failing runsc")
	}

	if !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("got %q, want it to say runsc left nothing behind", err)
	}
}

// TestCreateNamesOurOwnCancellation: an interrupt kills runsc before it prints, and reporting that
// silence as a diagnostic reads as a runsc that crashed for no reason.
func TestCreateNamesOurOwnCancellation(t *testing.T) {
	r, _ := fake(t, "", "", 1)

	log := filepath.Join(t.TempDir(), "output.log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close the log: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = r.Create(ctx, "amber-otter-1a2b", runsc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create returned %v, want it to name the cancellation", err)
	}

	if strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("got %q, want no diagnostic about output we cut short ourselves", err)
	}
}

func TestExecPutsTheFlagsBeforeTheIDAndTheCommandAfter(t *testing.T) {
	r, argvFile := fake(t, "", "", 0)

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runsc.ExecOptions{
		Argv:    []string{"/bin/sh", "-c", "echo hi"},
		Env:     []string{"A=1", "B=2"},
		WorkDir: "/srv",
		User:    "65534:65534",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	got := argv(t, argvFile)

	id := slices.Index(got, "amber-otter-1a2b")
	if id < 0 {
		t.Fatalf("the argv %q names no sandbox", got)
	}

	// Everything after the id is the guest's own command, and runsc reads no flag past it.
	if command := got[id+1:]; !slices.Equal(command, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Errorf("the command is %q, want the argv Exec was given", command)
	}

	flags := pairs(got[:id])
	for _, want := range []string{"--cwd /srv", "--user 65534:65534", "--env A=1", "--env B=2"} {
		if !slices.Contains(flags, want) {
			t.Errorf("the flags before the id are %q, want %q in them", got[:id], want)
		}
	}
}

// pairs reads a flag list as the flag-value pairs it is, so a repeated flag is matched by its value.
func pairs(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		out = append(out, args[i]+" "+args[i+1])
	}

	return out
}

// The point of exec: a command that exits 7 is an answer, not a failure of the driver.
func TestExecReturnsTheCommandExitCode(t *testing.T) {
	r, _ := fake(t, "", "", 7)

	code, err := r.Exec(t.Context(), "amber-otter-1a2b", runsc.ExecOptions{Argv: []string{"/bin/false"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 7 {
		t.Errorf("Exec returned %d, want 7", code)
	}
}

func TestExecRefusesACommandThatIsEmpty(t *testing.T) {
	r, _ := fake(t, "", "", 0)

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runsc.ExecOptions{}); err == nil {
		t.Fatal("Exec accepted a spec with no command")
	}
}

// A driver the host killed reports no exit code at all, and -1 is not one a command chose.
func TestExecRefusesADriverThatWasSignalled(t *testing.T) {
	r, _ := fakeBinary(t, "kill -9 $$\n")

	code, err := r.Exec(t.Context(), "amber-otter-1a2b", runsc.ExecOptions{Argv: []string{"/bin/true"}})
	if err == nil {
		t.Fatalf("Exec reported code %d and no error for a driver a signal ended", code)
	}
	if code != 0 {
		t.Errorf("Exec returned code %d beside its error, want 0", code)
	}
	if !strings.Contains(err.Error(), "amber-otter-1a2b") || !strings.Contains(err.Error(), "signal") {
		t.Errorf("the error is %q, and it must name the sandbox and the signal", err)
	}
}

// A cancelled exec must end the one guest process, because only Stop ends a sandbox.
func TestACancelledExecKillsTheGuestProcessAlone(t *testing.T) {
	// The fake writes the pid runsc would have written, then hangs the way an exec that runs does.
	r, argvFile := fakeBinary(t, `prev=
for arg in "$@"; do
	if [ "$prev" = "--internal-pid-file" ]; then echo 4242 > "$arg"; fi
	prev=$arg
done
case " $* " in *" exec "*) sleep 30 ;; esac
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	if _, err := r.Exec(ctx, "amber-otter-1a2b", runsc.ExecOptions{Argv: []string{"/bin/sleep", "30"}}); err == nil {
		t.Fatal("a cancelled Exec reported success")
	}

	// The kill ran last, so the recorded argv is its own.
	got := argv(t, argvFile)
	want := []string{"kill", "--pid", "4242", "amber-otter-1a2b", "KILL"}
	if at := slices.Index(got, "kill"); at < 0 || !slices.Equal(got[at:], want) {
		t.Errorf("the cancellation ran %q, want %q at its end", got, want)
	}
}
