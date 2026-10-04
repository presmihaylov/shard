package runc_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/pkg/runc"
)

// fake stands in for the binary: it records the argv it was called with, then prints what a test asked for.
func fake(t *testing.T, stdout, stderr string, exitCode int) (*runc.Runner, string) {
	t.Helper()

	return fakeBinary(t, "printf '%s' '"+stdout+"'\n"+
		"printf '%s' '"+stderr+"' >&2\n"+
		"exit "+strconv.Itoa(exitCode)+"\n")
}

// fakeBinary writes a fake runc that records its argv and then runs body. Body is what a test varies.
func fakeBinary(t *testing.T, body string, opts ...runc.Option) (*runc.Runner, string) {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	binary := filepath.Join(dir, "runc")

	// A body that has to signal the test writes beside the argv file, so $argv is the path it needs.
	script := "#!/bin/sh\nargv=" + argvFile + "\nprintf '%s\\n' \"$@\" > \"$argv\"\n" + body

	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake runc: %v", err)
	}

	r, err := runc.New(filepath.Join(dir, "root"), append([]runc.Option{runc.WithBinary(binary)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return r, argvFile
}

// waitFor blocks until the fake creates path, or gives up: a fixed sleep races a loaded machine.
func waitFor(path string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func argv(t *testing.T, path string) []string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake runc recorded no argv: %v", err)
	}

	return strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
}

func TestNewRefusesARelativeRoot(t *testing.T) {
	if _, err := runc.New("var/lib/shard/runc"); err == nil {
		t.Fatal("New accepted a relative root")
	}
}

func TestNewRefusesABinaryThatIsNotThere(t *testing.T) {
	if _, err := runc.New(t.TempDir(), runc.WithBinary("runc-that-does-not-exist")); err == nil {
		t.Fatal("New accepted a binary it cannot find")
	}
}

func TestStateParsesWhatRuncPrints(t *testing.T) {
	r, _ := fake(t, `{"id":"amber-otter-1a2b","status":"running","pid":4242,"bundle":"/var/lib/shard/sandboxes/x/bundle"}`, "", 0)

	state, err := r.State(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if state.Status != runc.StatusRunning {
		t.Errorf("got status %q, want running", state.Status)
	}
	if state.PID != 4242 {
		t.Errorf("got pid %d, want 4242", state.PID)
	}
}

// The root is the one global flag: without it runc keeps state under its own default and a
// second daemon on the host would see, and could delete, containers that are not its own.
func TestEveryCommandCarriesTheRoot(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	if err := r.Start(t.Context(), "amber-otter-1a2b"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := argv(t, recorded); !slices.Equal(got, []string{"--root", r.Root(), "start", "amber-otter-1a2b"}) {
		t.Errorf("got argv %v, want the root then the verb", got)
	}
}

func TestTheVerbsSpellTheirFlags(t *testing.T) {
	cases := []struct {
		verb string
		call func(r *runc.Runner) error
		want []string
	}{
		{"pause", func(r *runc.Runner) error { return r.Pause(t.Context(), "amber-otter-1a2b") }, []string{"pause", "amber-otter-1a2b"}},
		{"resume", func(r *runc.Runner) error { return r.Resume(t.Context(), "amber-otter-1a2b") }, []string{"resume", "amber-otter-1a2b"}},
		{"delete", func(r *runc.Runner) error { return r.Delete(t.Context(), "amber-otter-1a2b", true) }, []string{"delete", "--force", "amber-otter-1a2b"}},
		{"kill", func(r *runc.Runner) error { return r.Kill(t.Context(), "amber-otter-1a2b", "TERM", false) }, []string{"kill", "amber-otter-1a2b", "TERM"}},
		{"kill --all", func(r *runc.Runner) error { return r.Kill(t.Context(), "amber-otter-1a2b", "KILL", true) }, []string{"kill", "--all", "amber-otter-1a2b", "KILL"}},
	}

	for _, tc := range cases {
		r, recorded := fake(t, "", "", 0)

		if err := tc.call(r); err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}

		if got := argv(t, recorded); !strings.HasSuffix(strings.Join(got, " "), strings.Join(tc.want, " ")) {
			t.Errorf("%s got argv %v, want it to end with %v", tc.verb, got, tc.want)
		}
	}
}

func TestAContainerRuncDoesNotHoldIsNotFound(t *testing.T) {
	r, _ := fake(t, "", `container "amber-otter-1a2b" does not exist`, 1)

	if _, err := r.State(t.Context(), "amber-otter-1a2b"); !errors.Is(err, runc.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A stop kills a container that may already be dead, and that must not read as a failure.
func TestKillingADeadContainerIsNotRunning(t *testing.T) {
	r, _ := fake(t, "", "container not running", 1)

	if err := r.Kill(t.Context(), "amber-otter-1a2b", "TERM", false); !errors.Is(err, runc.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}

func TestAFailureKeepsWhatRuncSaid(t *testing.T) {
	r, _ := fake(t, "", "some new runc failure", 1)

	err := r.Delete(t.Context(), "amber-otter-1a2b", false)
	if err == nil {
		t.Fatal("Delete reported success on a failing runc")
	}
	if !strings.Contains(err.Error(), "some new runc failure") {
		t.Errorf("got %q, want it to carry what runc printed", err)
	}
}

func TestCreateRefusesWithNoBundle(t *testing.T) {
	r, _ := fake(t, "", "", 0)

	if err := r.Create(t.Context(), "amber-otter-1a2b", runc.CreateOptions{}); err == nil {
		t.Fatal("Create accepted a container with no bundle")
	}
}

// The caller deletes the state directory when a create fails, so a message that only named the log
// would send the operator to a file that is already gone.
func TestCreateQuotesItsDiagnostics(t *testing.T) {
	r, _ := fake(t, "", "container_linux.go: starting container process caused: mount /tmp/absent: no such file or directory", 1)

	f := openLog(t, "output from an earlier run\n")

	err := r.Create(t.Context(), "amber-otter-1a2b", runc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if err == nil {
		t.Fatal("Create reported success on a failing runc")
	}

	if !strings.Contains(err.Error(), "mount /tmp/absent: no such file or directory") {
		t.Errorf("got %q, want it to carry what runc printed", err)
	}
	if strings.Contains(err.Error(), "output from an earlier run") {
		t.Errorf("got %q, want only this create's own output", err)
	}
}

func TestCreateReportsThatRuncPrintedNothing(t *testing.T) {
	r, _ := fake(t, "", "", 1)

	f := openLog(t, "")

	err := r.Create(t.Context(), "amber-otter-1a2b", runc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if err == nil {
		t.Fatal("Create reported success on a failing runc")
	}

	if !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("got %q, want it to say runc left nothing behind", err)
	}
}

func TestCreateNamesOurOwnCancellation(t *testing.T) {
	r, _ := fake(t, "", "", 1)

	f := openLog(t, "")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := r.Create(ctx, "amber-otter-1a2b", runc.CreateOptions{Bundle: t.TempDir(), Stdout: f, Stderr: f})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create returned %v, want it to name the cancellation", err)
	}

	if strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("got %q, want no diagnostic about output we cut short ourselves", err)
	}
}

// openLog is the guest's output file, appended to the way the provider opens it, holding what it holds.
func openLog(t *testing.T, earlier string) *os.File {
	t.Helper()

	log := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(log, []byte(earlier), 0o600); err != nil {
		t.Fatalf("write the log: %v", err)
	}

	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close the log: %v", err)
		}
	})

	return f
}

// SHARD-158: a restarted daemon sweeps the scratch the last one left, and it looks under its own root alone.
func TestExecKeepsItsScratchUnderTheExecDirAndRemovesIt(t *testing.T) {
	execDir := filepath.Join(t.TempDir(), "exec")
	r, argvFile := fakeBinary(t, "", runc.WithExecDir(execDir))

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/true"}, Bundle: bundle(t)}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	got := argv(t, argvFile)
	at := slices.Index(got, "--pid-file")
	if at < 0 || at+1 >= len(got) {
		t.Fatalf("the argv %q names no pid file", got)
	}
	if pidFile := got[at+1]; filepath.Dir(filepath.Dir(pidFile)) != execDir {
		t.Errorf("the pid file is %s, want it in a directory under %s", pidFile, execDir)
	}

	entries, err := os.ReadDir(execDir)
	if err != nil {
		t.Fatalf("read the exec directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Exec left %d entries under %s, want none", len(entries), execDir)
	}
}

// runc reads the whole process from the file and merges nothing, so the file is what runc's flags would have made.
func TestExecHandsRuncTheBundleProcessWithTheExecOverrides(t *testing.T) {
	r, argvFile := fakeBinary(t, savingProcess())

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{
		Bundle:  bundle(t),
		Argv:    []string{"/bin/sh", "-c", "echo hi"},
		Env:     []string{"A=1", "B=2"},
		WorkDir: "/srv",
		User:    "65534:65533",
		Groups:  []uint32{65534, 10},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	got := argv(t, argvFile)
	at := slices.Index(got, "exec")
	if at < 0 || len(got) != at+6 || got[at+1] != "--pid-file" || got[at+3] != "--process" || got[at+5] != "amber-otter-1a2b" {
		t.Fatalf("the argv is %q, want exec --pid-file P --process F and the id last", got)
	}

	process := savedProcess(t, argvFile)
	if !slices.Equal(process.Args, []string{"/bin/sh", "-c", "echo hi"}) {
		t.Errorf("the args are %q, want the argv Exec was given", process.Args)
	}
	if !slices.Equal(process.Env, []string{"PATH=/bin", "A=1", "B=2"}) {
		t.Errorf("the env is %q, want the bundle's with the exec's appended", process.Env)
	}
	if process.Cwd != "/srv" {
		t.Errorf("the cwd is %q, want /srv", process.Cwd)
	}
	if process.User.UID != 65534 || process.User.GID != 65533 || !slices.Equal(process.User.AdditionalGids, []uint32{4, 65534, 10}) {
		t.Errorf("the user is %+v, want 65534:65533 with the groups appended to the bundle's", process.User)
	}
	if process.Terminal || process.ConsoleSize != nil {
		t.Errorf("a pipe exec asked for a terminal: terminal %t, window %+v", process.Terminal, process.ConsoleSize)
	}
	if process.Capabilities == nil || !slices.Equal(process.Capabilities.Bounding, []string{"CAP_KILL"}) || !process.NoNewPrivileges {
		t.Errorf("the process lost what the bundle confines it with: %+v", process)
	}
}

// SHARD-514: --tty alone sizes the pty after the command starts, so a quick stty size read 0x0.
func TestATerminalExecHandsRuncTheWindowBeforeTheCommandStarts(t *testing.T) {
	terminal, err := pty.Open()
	if err != nil {
		t.Fatalf("open a pty: %v", err)
	}
	t.Cleanup(func() {
		if err := terminal.Close(); err != nil {
			t.Errorf("close the pty: %v", err)
		}
	})
	if err := terminal.Resize(pty.Size{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("resize the pty: %v", err)
	}

	r, argvFile := fakeBinary(t, savingProcess())
	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{
		Bundle: bundle(t),
		Argv:   []string{"/bin/sh"},
		TTY:    true,
		Stdin:  terminal.Replica, Stdout: terminal.Replica, Stderr: terminal.Replica,
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	process := savedProcess(t, argvFile)
	if !process.Terminal || process.ConsoleSize == nil || *process.ConsoleSize != (specs.Box{Height: 24, Width: 80}) {
		t.Errorf("terminal %t, window %+v, want a terminal of 24x80", process.Terminal, process.ConsoleSize)
	}
}

func TestExecRefusesWithNoBundle(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/true"}}); err == nil {
		t.Fatal("Exec ran with no bundle to take the process from")
	}

	if _, err := os.Stat(recorded); err == nil {
		t.Error("a refusal still ran runc")
	}
}

// bundle is a directory whose config.json holds what create would have started PID 1 with.
func bundle(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	blob, err := json.Marshal(specs.Spec{Process: &specs.Process{
		Args:            []string{"/sbin/shard-init"},
		Env:             []string{"PATH=/bin"},
		Cwd:             "/",
		User:            specs.User{AdditionalGids: []uint32{4}},
		Capabilities:    &specs.LinuxCapabilities{Bounding: []string{"CAP_KILL"}},
		NoNewPrivileges: true,
	}})
	if err != nil {
		t.Fatalf("encode the bundle config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), blob, 0o600); err != nil {
		t.Fatalf("write the bundle config: %v", err)
	}

	return dir
}

// savingProcess keeps a copy of the process file, because Exec removes the original when runc returns.
func savingProcess() string {
	return `prev=
for arg in "$@"; do
	if [ "$prev" = "--process" ]; then cp "$arg" "$argv.process"; fi
	prev=$arg
done
`
}

func savedProcess(t *testing.T, argvFile string) specs.Process {
	t.Helper()

	blob, err := os.ReadFile(argvFile + ".process")
	if err != nil {
		t.Fatalf("the fake runc saved no process file: %v", err)
	}

	var process specs.Process
	if err := json.Unmarshal(blob, &process); err != nil {
		t.Fatalf("decode the process file: %v", err)
	}

	return process
}

// The point of exec: a command that exits 7 is an answer, not a failure of the driver.
func TestExecReturnsTheCommandExitCode(t *testing.T) {
	r, _ := fake(t, "", "", 7)

	code, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/false"}, Bundle: bundle(t)})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if code != 7 {
		t.Errorf("Exec returned %d, want 7", code)
	}
}

func TestExecRefusesACommandThatIsEmpty(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{}); err == nil {
		t.Fatal("Exec accepted a spec with no command")
	}

	if _, err := os.Stat(recorded); err == nil {
		t.Error("a refusal still ran runc")
	}
}

// A driver the host killed reports no exit code at all, and -1 is not one a command chose.
func TestExecRefusesADriverThatWasSignalled(t *testing.T) {
	r, _ := fakeBinary(t, "kill -9 $$\n")

	code, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/true"}, Bundle: bundle(t)})
	if err == nil {
		t.Fatalf("Exec reported code %d and no error for a driver a signal ended", code)
	}
	if code != 0 {
		t.Errorf("Exec returned code %d beside its error, want 0", code)
	}
	if !strings.Contains(err.Error(), "amber-otter-1a2b") || !strings.Contains(err.Error(), "signal") {
		t.Errorf("the error is %q, and it must name the container and the signal", err)
	}
}

// A cancelled exec must end the one guest process, because only Delete ends a container. The pid file
// holds a host pid, so the fake writes its own and the kill that lands on it is the proof.
func TestACancelledExecKillsTheGuestProcessAlone(t *testing.T) {
	r, argvFile := fakeBinary(t, writingOwnPID()+`: > "$argv.ready"
sleep 30
echo survived > "$argv.survived"
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// A cancel before the pid file lands tests the fallback, not the kill this test is about.
		defer cancel()

		waitFor(argvFile + ".ready")
	}()

	_, err := r.Exec(ctx, "amber-otter-1a2b", runc.ExecOptions{Argv: []string{"/bin/sleep", "30"}, Bundle: bundle(t)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Exec returned %v, want it to name the cancellation", err)
	}

	if _, err := os.Stat(argvFile + ".survived"); err == nil {
		t.Error("the guest process outlived the cancellation")
	}
}

// runc exec prints a command it cannot start only to the guest's stderr and exits 1, which is
// indistinguishable from the command's own 1. The host-side lookup against the rootfs is what tells them apart.
func TestExecLooksTheCommandUpBeforeItRuns(t *testing.T) {
	r, recorded := fake(t, "", "", 0)

	rootfs := t.TempDir()
	writeExecutable(t, filepath.Join(rootfs, "bin", "true"))

	_, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{
		Argv: []string{"nosuch"}, Env: []string{"PATH=/bin"}, RootFS: rootfs, Bundle: bundle(t),
	})

	var lookup *runc.LookupError
	if !errors.As(err, &lookup) {
		t.Fatalf("Exec returned %v, want a LookupError", err)
	}
	if lookup.Reason != "nosuch: not found" {
		t.Errorf("the reason is %q, want the shell's", lookup.Reason)
	}
	if _, err := os.Stat(recorded); err == nil {
		t.Error("a command that is not there still ran runc")
	}

	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{
		Argv: []string{"true"}, Env: []string{"PATH=/bin"}, RootFS: rootfs, Bundle: bundle(t),
	}); err != nil {
		t.Fatalf("Exec refused a command that is on the guest's PATH: %v", err)
	}

	// No PATH in the env means the OCI default, which is what runc would resolve against.
	if _, err := r.Exec(t.Context(), "amber-otter-1a2b", runc.ExecOptions{
		Argv: []string{"true"}, RootFS: rootfs, Bundle: bundle(t),
	}); err != nil {
		t.Fatalf("Exec refused a command on the default PATH when the env named none: %v", err)
	}
}

// writingOwnPID is a fake whose "guest process" is the fake itself, so a kill on the pid it wrote is safe.
func writingOwnPID() string {
	return `prev=
for arg in "$@"; do
	if [ "$prev" = "--pid-file" ]; then echo $$ > "$arg"; fi
	prev=$arg
done
`
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
