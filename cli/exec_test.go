package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func TestParseExecTheGoalCommand(t *testing.T) {
	opts, err := parseExec([]string{"bold-comet-9ed5", "--", "echo", "hello"})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}

	if opts.id != "bold-comet-9ed5" {
		t.Errorf("id = %q, want bold-comet-9ed5", opts.id)
	}
	if want := []string{"echo", "hello"}; !slices.Equal(opts.argv, want) {
		t.Errorf("argv = %v, want %v", opts.argv, want)
	}
}

func TestParseExecTakesEveryFlagBeforeTheID(t *testing.T) {
	args := []string{"-i", "--env", "A=1", "--env", "B=2", "--workdir", "/srv", "--user", "app", "one-two-0000", "--", "sh"}

	opts, err := parseExec(args)
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}

	if !opts.interactive {
		t.Error("-i did not set interactive")
	}
	if want := []string{"A=1", "B=2"}; !slices.Equal(opts.env, want) {
		t.Errorf("env = %v, want %v", opts.env, want)
	}
	if opts.workDir != "/srv" {
		t.Errorf("workDir = %q, want /srv", opts.workDir)
	}
	if opts.user != "app" {
		t.Errorf("user = %q, want app", opts.user)
	}
}

// -it is one word to everyone who has typed it, and Go's flag package reads it as a flag named it.
func TestParseExecReadsTheBundledFlags(t *testing.T) {
	for _, bundle := range []string{"-it", "-ti"} {
		opts, err := parseExec([]string{bundle, "one-two-0000", "--", "sh"})
		if err != nil {
			t.Fatalf("parseExec %s: %v", bundle, err)
		}

		if !opts.interactive || !opts.tty {
			t.Errorf("%s set interactive=%v tty=%v, want both", bundle, opts.interactive, opts.tty)
		}
	}
}

// A flag of the command is the command's, and shard must never read it as one of its own.
func TestParseExecLeavesTheCommandFlagsAlone(t *testing.T) {
	opts, err := parseExec([]string{"one-two-0000", "--", "ls", "-i", "--user", "root"})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}

	if opts.interactive || opts.user != "" {
		t.Errorf("shard read the command's flags: interactive=%v user=%q", opts.interactive, opts.user)
	}
	if want := []string{"ls", "-i", "--user", "root"}; !slices.Equal(opts.argv, want) {
		t.Errorf("argv = %v, want %v", opts.argv, want)
	}
}

func TestParseExecRefusals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"--", "sh"}, "one sandbox id"},
		{"no separator", []string{"one-two-0000", "sh"}, "after --"},
		{"nothing after the separator", []string{"one-two-0000", "--"}, "nothing followed it"},
		{"a second argument before the separator", []string{"one-two-0000", "sh", "--", "sh"}, "unexpected argument"},
		{"a terminal with no input", []string{"-t", "one-two-0000", "--", "sh"}, "-t needs -i"},
		{"an env entry that is no assignment", []string{"--env", "BROKEN", "one-two-0000", "--", "sh"}, "KEY=VALUE"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseExec(c.args)
			if err == nil {
				t.Fatalf("parseExec accepted %v", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("parseExec failed with %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// The exit code is the whole reason exec is useful, so it must reach the process that ran shard.
func TestExecReportsTheCommandExitCodeAsAnExitError(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.providerSvc.(*fakeLifecycleProvider).execExit = models.ExitStatus{Code: 7}

	err := app.Run(t.Context(), []string{"exec", "sandbox1", "--", "sh", "-c", "exit 7"})

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Run returned %v, want an ExitError", err)
	}
	if exit.Code != 7 {
		t.Errorf("exit code = %d, want 7", exit.Code)
	}
}

func TestExecReportsNothingForACommandThatSucceeded(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.providerSvc.(*fakeLifecycleProvider).execOut = "hello\n"

	if err := app.Run(t.Context(), []string{"exec", "sandbox1", "--", "echo", "hello"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.String() != "hello\n" {
		t.Errorf("exec printed %q, want what the command wrote", out.String())
	}
}

// The flags belong to the exec'd process alone, and nothing else in the sandbox sees them.
func TestExecPassesItsFlagsToTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	provider := d.providerSvc.(*fakeLifecycleProvider)

	args := []string{"exec", "--env", "A=1", "--workdir", "/srv", "--user", "app", "sandbox1", "--", "env"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if want := []string{"env"}; !slices.Equal(provider.execSpec.Argv, want) {
		t.Errorf("argv = %v, want %v", provider.execSpec.Argv, want)
	}
	if want := []string{"A=1"}; !slices.Equal(provider.execSpec.Env, want) {
		t.Errorf("env = %v, want %v", provider.execSpec.Env, want)
	}
	if provider.execSpec.WorkDir != "/srv" || provider.execSpec.User != "app" {
		t.Errorf("workDir = %q, user = %q, want /srv and app", provider.execSpec.WorkDir, provider.execSpec.User)
	}
	if provider.execSpec.TTY {
		t.Error("the exec asked for a terminal, and nobody asked for one")
	}
}

// Without -i the command reads nothing, so the daemon must give it no stdin at all.
func TestExecWithoutInteractiveGivesTheCommandNoStdin(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"exec", "sandbox1", "--", "true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if d.providerSvc.(*fakeLifecycleProvider).execSpec.Stdin != nil {
		t.Error("the command was given stdin without -i")
	}
}

func TestExecRefusesASandboxTheDaemonDoesNotHold(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"exec", "gone-away-0000", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "gone-away-0000") {
		t.Fatalf("exec returned %v, want the id named", err)
	}
	if slices.Contains(d.providerSvc.(*fakeLifecycleProvider).r.calls, "provider.Exec") {
		t.Error("exec reached the provider for a sandbox shard does not hold")
	}
}

// runsc says 128 for a command that never ran, and a shell says 127 for one it cannot find.
func TestExecTurnsARefusedCommandIntoAShellExitCode(t *testing.T) {
	cases := map[int]*models.CommandNotStartedError{
		127: {Sandbox: "sandbox1", Reason: "failed to load /bin/nope: no such file or directory", Code: 127},
		126: {Sandbox: "sandbox1", Reason: "failed to load /tmp/data: permission denied", Code: 126},
	}

	for want, refusal := range cases {
		var out bytes.Buffer

		app, d := newClientApp(t, &out, running())
		d.providerSvc.(*fakeLifecycleProvider).execErr = refusal

		err := app.Run(t.Context(), []string{"exec", "sandbox1", "--", "/bin/nope"})

		var exit *ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("Run returned %v, want an ExitError", err)
		}
		if exit.Code != want {
			t.Errorf("exit code = %d, want %d", exit.Code, want)
		}
		if !strings.Contains(exit.Message, refusal.Reason) {
			t.Errorf("the message is %q, and it must say why the command never ran", exit.Message)
		}
	}
}

// A terminal cannot be faked with a pipe, so -t must refuse rather than hang on one.
func TestExecRefusesATerminalWhenStdinIsNotOne(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, running())

	err := app.Run(t.Context(), []string{"exec", "-it", "sandbox1", "--", "sh"})
	if err == nil {
		t.Fatal("-t was accepted on stdin that is not a terminal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("the refusal is %q, and it must say the terminal is missing", err)
	}
}

// A daemon nothing answers on is the one failure every verb prints the same way.
func TestExecReportsADaemonThatIsNotThere(t *testing.T) {
	var out bytes.Buffer

	app := App{Version: "test", Root: shortRoot(t), Out: &out}

	err := app.Run(t.Context(), []string{"exec", "sandbox1", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "cannot connect to shard daemon") {
		t.Fatalf("exec with no daemon returned %v", err)
	}
}

// A window that changes reaches the forwarder, and its stop waits for the goroutine that carries it.
func TestTheResizeForwarderEndsWithItsStop(t *testing.T) {
	warnings := &syncBuffer{}
	app := App{Version: "test", Root: shortRoot(t), Out: warnings, Err: warnings}

	// A plain file is no terminal, so the forwarder warns instead of resizing anything.
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-terminal"))
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}
	defer plain.Close()

	forwarder := forwardResize(t.Context(), app, "sandbox1", plain)
	forwarder.named("1a2b3c4d5e6f7a8b")

	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("raise SIGWINCH: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(warnings.String(), "terminal") {
		if time.Now().After(deadline) {
			t.Fatalf("the forwarder said %q about a window it cannot read", warnings.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	forwarder.stop()
}
