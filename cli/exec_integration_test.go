//go:build integration

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/pty"
)

// terminalReadBudget bounds the wait for what a command wrote to a terminal this test holds open.
const terminalReadBudget = 5 * time.Second

// TestExecReturnsTheCommandExitCode is the SHARD-22 acceptance criterion: a command that exits 7
// makes shard exec exit 7. Create prints an id and exits 0; exec is the opposite.
func TestExecReturnsTheCommandExitCode(t *testing.T) {
	app, id := runningSandbox(t)

	out, err := runExec(t, app, "exec", id, "/bin/sh", "-c", "echo seven; exit 7")

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("exec returned %v, want an ExitError", err)
	}
	if exit.Code != 7 {
		t.Errorf("exec exited %d, want 7", exit.Code)
	}
	if !strings.Contains(out, "seven") {
		t.Errorf("exec wrote %q, and the command printed seven", out)
	}
}

// The non-TTY path writes what the command wrote, and reports success as no error at all.
func TestExecWritesTheCommandOutput(t *testing.T) {
	app, id := runningSandbox(t)

	out, err := runExec(t, app, "exec", id, "/bin/echo", "hello from the sandbox")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if !strings.Contains(out, "hello from the sandbox") {
		t.Errorf("exec wrote %q, want what the command printed", out)
	}
}

// Two execs are two processes in one sandbox, so what the first writes the second reads.
func TestTwoExecsShareTheSandbox(t *testing.T) {
	app, id := runningSandbox(t)

	if _, err := runExec(t, app, "exec", id, "/bin/sh", "-c", "echo shared > /exec-state"); err != nil {
		t.Fatalf("the first exec: %v", err)
	}

	out, err := runExec(t, app, "exec", id, "/bin/cat", "/exec-state")
	if err != nil {
		t.Fatalf("the second exec: %v", err)
	}

	if !strings.Contains(out, "shared") {
		t.Errorf("the second exec read %q, want what the first wrote", out)
	}
}

// The env and the workdir belong to the exec'd process, and the entrypoint never sees them.
func TestExecAppliesItsOwnEnvAndWorkDir(t *testing.T) {
	app, id := runningSandbox(t)

	out, err := runExec(t, app, "exec", "--env", "SHARD_EXEC=set", "--workdir", "/tmp", id, "/bin/sh", "-c", "pwd; echo $SHARD_EXEC")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if !strings.Contains(out, "/tmp") || !strings.Contains(out, "set") {
		t.Errorf("exec wrote %q, want the workdir and the variable it was given", out)
	}
}

// An exec drops to any user, and the supervisor is untouched: PID 1 stays root.
func TestExecRunsAsTheUserItWasGiven(t *testing.T) {
	app, id := runningSandbox(t)

	out, err := runExec(t, app, "exec", "--user", "nobody", id, "/bin/sh", "-c", "id -u; cat /proc/1/status | grep '^Uid:'")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if !strings.Contains(out, "65534") {
		t.Errorf("exec wrote %q, and nobody is uid 65534 on this image", out)
	}
	if !strings.Contains(out, "Uid:\t0") {
		t.Errorf("exec wrote %q, and the supervisor must still be root", out)
	}
}

// An exec with no user of its own runs as the entrypoint does, or the confinement --user bought is
// gone for every command after it.
func TestExecInheritsTheUserTheEntrypointRunsAs(t *testing.T) {
	app, id := sandboxAs(t, "nobody")

	out, err := runExec(t, app, "exec", id, "/bin/sh", "-c", "id -u")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	if strings.TrimSpace(out) != "65534" {
		t.Errorf("exec ran as uid %q, want 65534, which is what the entrypoint runs as", strings.TrimSpace(out))
	}
}

// Refuse, never downgrade. An id shard does not hold is an error, and it never reaches the substrate.
func TestExecRefusesAnIdShardDoesNotHold(t *testing.T) {
	app, _ := runningSandbox(t)

	_, err := runExec(t, app, "exec", "quiet-otter-0000", "/bin/true")
	if err == nil {
		t.Fatal("exec accepted an id shard does not hold")
	}
	if !strings.Contains(err.Error(), "quiet-otter-0000") {
		t.Errorf("the refusal is %q, and it must name the sandbox", err)
	}
}

func TestExecRefusesASandboxThatIsStopped(t *testing.T) {
	app, id := runningSandbox(t)

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	_, err := runExec(t, app, "exec", id, "/bin/true")
	if err == nil {
		t.Fatal("exec ran a command in a sandbox that is stopped")
	}
	if !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("the refusal is %q, and it must name the sandbox and its state", err)
	}
}

// runsc says 128 for a command it never started, which is its own code and no shell's. A shell says
// 127 for a command it cannot find and 126 for one it found and may not run, and so does shard.
func TestExecAnswersACommandThatNeverRanWithAShellExitCode(t *testing.T) {
	app, id := runningSandbox(t)

	if _, err := runExec(t, app, "exec", id, "/bin/sh", "-c", "echo x > /not-executable"); err != nil {
		t.Fatalf("write the file the sandbox may not run: %v", err)
	}

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"a command that is not there", []string{"exec", id, "/bin/nope"}, 127},
		{"a workdir that is not there", []string{"exec", "--workdir", "/no/such/dir", id, "/bin/true"}, 127},
		{"a file the sandbox may not run", []string{"exec", id, "/not-executable"}, 126},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runExec(t, app, c.args...)

			var exit *ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("exec returned %v, want an ExitError", err)
			}
			if exit.Code != c.want {
				t.Errorf("exec exited %d, want %d", exit.Code, c.want)
			}
			if exit.Message == "" {
				t.Error("exec said nothing about a command that never ran")
			}
		})
	}
}

// Three execs at once are three processes in one sandbox, and nothing about one is shared with another.
func TestConcurrentExecsAllSucceed(t *testing.T) {
	app, id := runningSandbox(t)

	failures := make(chan error, 3)
	dir := t.TempDir()
	for i := range 3 {
		go func() {
			failures <- runExecTo(app, filepath.Join(dir, fmt.Sprintf("out-%d", i)), "exec", id, "/bin/sh", "-c", "sleep 1; echo done")
		}()
	}

	for range 3 {
		if err := <-failures; err != nil {
			t.Errorf("a concurrent exec failed: %v", err)
		}
	}
}

// A terminal is the other half of the verb, and it must keep the exit code the non-TTY path keeps.
func TestExecOnATerminalKeepsTheExitCodeAndTheWindow(t *testing.T) {
	app, id := runningSandbox(t)

	terminal, chunks := sizedTerminal(t, pty.Size{Rows: 40, Cols: 120})
	app.Out, app.Err = terminal.Replica, terminal.Replica
	app.in = terminal.Replica

	runErr := app.Run(context.Background(), []string{"exec", "-it", id, "/bin/sh", "-c", "stty size; exit 7"})

	var exit *ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("exec on a terminal returned %v, want an ExitError", runErr)
	}
	if exit.Code != 7 {
		t.Errorf("exec on a terminal exited %d, want 7", exit.Code)
	}

	deadline := time.After(terminalReadBudget)
	var out string
	for !strings.Contains(out, "40 120") {
		select {
		case chunk := <-chunks:
			out += chunk
		case <-deadline:
			t.Fatalf("the command wrote %q to its terminal, want the window 40 120", strings.TrimSpace(out))
		}
	}
}

// SHARD-514: runc sized the guest's pty only after the command started, so a quick stty size read a 0x0 window.
func TestExecOnATerminalHasItsWindowBeforeTheCommandStarts(t *testing.T) {
	app, id := runningSandbox(t)

	for i := range 20 {
		terminal, chunks := sizedTerminal(t, pty.Size{Rows: 24, Cols: 80})
		app.Out, app.Err, app.in = terminal.Replica, terminal.Replica, terminal.Replica

		runErr := app.Run(context.Background(), []string{"exec", "-it", id, "stty", "size"})
		if line := firstLine(t, chunks); line != "24 80" {
			t.Fatalf("exec %d: stty size printed %q on a 24x80 terminal, want 24 80", i, line)
		}
		if runErr != nil {
			t.Fatalf("exec %d on a terminal: %v", i, runErr)
		}
	}
}

// sizedTerminal hands back chunks, not a read to the end: the master hangs up only once every replica copy is gone.
func sizedTerminal(t *testing.T, size pty.Size) (*pty.Pty, <-chan string) {
	t.Helper()

	terminal, err := pty.Open()
	if err != nil {
		t.Fatalf("open a terminal for the test: %v", err)
	}
	t.Cleanup(func() {
		if err := terminal.Close(); err != nil {
			t.Logf("close the test terminal: %v", err)
		}
	})
	if err := terminal.Resize(size); err != nil {
		t.Fatalf("size the test terminal: %v", err)
	}

	chunks := make(chan string, 16)
	go func() {
		for {
			buf := make([]byte, 4096)
			n, err := terminal.Master.Read(buf)
			if n > 0 {
				chunks <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	return terminal, chunks
}

// firstLine is the first line a command wrote to its terminal, without the line end.
func firstLine(t *testing.T, chunks <-chan string) string {
	t.Helper()

	deadline := time.After(terminalReadBudget)
	var out string
	for !strings.Contains(out, "\n") {
		select {
		case chunk := <-chunks:
			out += chunk
		case <-deadline:
			t.Fatalf("the command wrote %q to its terminal and no whole line", out)
		}
	}

	line, _, _ := strings.Cut(out, "\n")

	return strings.TrimSpace(line)
}

// runningSandbox creates one sandbox whose entrypoint has already exited, because a sandbox outlives
// it and an exec must still work.
func runningSandbox(t *testing.T) (App, string) {
	t.Helper()

	return sandboxAs(t, "")
}

// sandboxAs is the same, created as the given user, which is what an exec of its own inherits.
func sandboxAs(t *testing.T, user string) (App, string) {
	t.Helper()

	app, out := newCreateApp(t)

	var flags []string
	if user != "" {
		flags = append(flags, "--user", user)
	}

	id := runDetachedWith(t, app, out, append(flags, testImage, "/bin/true")...)
	t.Cleanup(func() { cleanUp(t, app, id) })

	awaitEntrypoint(t, app, id)

	return app, id
}

// runExec runs one shard exec on the real wiring and returns everything the command wrote.
func runExec(t *testing.T, app App, args ...string) (string, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "exec-output")

	err := runExecTo(app, path, args...)

	written, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read the exec output: %v", readErr)
	}

	return string(written), err
}

func runExecTo(app App, path string, args ...string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}

	app.Out, app.Err = out, out

	return errors.Join(app.Run(context.Background(), args), out.Close())
}
