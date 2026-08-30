package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

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
	provider := &fakeExecProvider{status: models.ExitStatus{Code: 7}}
	app := execApp(provider)

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "sh", "-c", "exit 7"})

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("Run returned %v, want an ExitError", err)
	}
	if exit.Code != 7 {
		t.Errorf("exit code = %d, want 7", exit.Code)
	}
}

func TestExecReportsNothingForACommandThatSucceeded(t *testing.T) {
	provider := &fakeExecProvider{}
	app := execApp(provider)

	if err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// The flags belong to the exec'd process alone, and nothing else in the sandbox sees them.
func TestExecPassesItsFlagsToTheProvider(t *testing.T) {
	provider := &fakeExecProvider{}
	app := execApp(provider)

	args := []string{"exec", "-i", "--env", "A=1", "--workdir", "/srv", "--user", "app", "one-two-0000", "--", "env"}
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.id != "one-two-0000" {
		t.Errorf("the provider was given id %q, want one-two-0000", provider.id)
	}
	if want := []string{"env"}; !slices.Equal(provider.spec.Argv, want) {
		t.Errorf("argv = %v, want %v", provider.spec.Argv, want)
	}
	if want := []string{"A=1"}; !slices.Equal(provider.spec.Env, want) {
		t.Errorf("env = %v, want %v", provider.spec.Env, want)
	}
	if provider.spec.WorkDir != "/srv" || provider.spec.User != "app" {
		t.Errorf("workDir = %q, user = %q, want /srv and app", provider.spec.WorkDir, provider.spec.User)
	}
	if provider.spec.TTY {
		t.Error("the exec asked for a terminal, and nobody asked for one")
	}
	if provider.spec.Stdin == nil {
		t.Error("-i gave the command no stdin")
	}
}

// Without -i the command reads nothing, so it must not be handed this process's keyboard.
func TestExecWithoutInteractiveGivesTheCommandNoStdin(t *testing.T) {
	provider := &fakeExecProvider{}
	app := execApp(provider)

	if err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.spec.Stdin != nil {
		t.Error("the command was given stdin without -i")
	}
}

func TestExecRefusesASandboxTheRecordDoesNotHold(t *testing.T) {
	provider := &fakeExecProvider{}
	app := execApp(provider)
	app.newDeps = func(App) *deps {
		return &deps{repoSvc: missingRepo{}, providerSvc: provider}
	}

	err := app.Run(context.Background(), []string{"exec", "gone-away-0000", "--", "true"})
	if err == nil {
		t.Fatal("exec accepted an id shard does not hold")
	}
	if !strings.Contains(err.Error(), "gone-away-0000") {
		t.Errorf("the refusal is %q, and it must name the sandbox", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a sandbox shard does not hold")
	}
}

// The keep-alive rule: the entrypoint exits, the sandbox stays running, and exec still works on it.
func TestExecRunsInASandboxWhoseEntrypointHasExited(t *testing.T) {
	provider := &fakeExecProvider{state: models.StateRunning}
	app := execApp(provider)

	if err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if provider.calls != 1 {
		t.Errorf("the provider ran %d commands, want 1", provider.calls)
	}
}

func TestExecRefusesAStoppedSandboxWithoutTheProvider(t *testing.T) {
	provider := &fakeExecProvider{state: models.StateStopped}
	app := execApp(provider)

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"})
	if err == nil {
		t.Fatal("exec accepted a stopped sandbox")
	}
	if !strings.Contains(err.Error(), "one-two-0000") || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("the refusal is %q, and it must name the sandbox and its state", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a stopped sandbox")
	}
}

// The provider holds nothing of a paused sandbox, so the refusal must point at resume and not at gone.
func TestExecRefusesAPausedSandboxWithTheResumeHint(t *testing.T) {
	provider := &fakeExecProvider{forgotten: true}
	app := execApp(provider)
	app.newDeps = func(App) *deps {
		return &deps{repoSvc: pausedRepo{}, providerSvc: provider}
	}

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "shard resume one-two-0000") {
		t.Fatalf("exec of a paused sandbox returned %v, want a refusal that names the resume", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a paused sandbox")
	}
}

// A record outlives a shard restart, so the substrate is the one that answers for the state.
func TestExecRefusesASandboxTheProviderNoLongerHolds(t *testing.T) {
	provider := &fakeExecProvider{forgotten: true}
	app := execApp(provider)

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"})
	if err == nil {
		t.Fatal("exec accepted a sandbox the provider has forgotten")
	}
	if !strings.Contains(err.Error(), "one-two-0000") {
		t.Errorf("the refusal is %q, and it must name the sandbox", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a sandbox it does not hold")
	}
}

// runsc says 128 for a command that never ran, and a shell says 127 for one it cannot find.
func TestExecTurnsARefusedCommandIntoAShellExitCode(t *testing.T) {
	cases := map[int]*models.CommandNotStartedError{
		127: {Sandbox: "one-two-0000", Reason: "failed to load /bin/nope: no such file or directory", Code: 127},
		126: {Sandbox: "one-two-0000", Reason: "failed to load /tmp/data: permission denied", Code: 126},
	}

	for want, refusal := range cases {
		provider := &fakeExecProvider{err: refusal}
		app := execApp(provider)

		err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "/bin/nope"})

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
	app := execApp(&fakeExecProvider{})

	err := app.Run(context.Background(), []string{"exec", "-it", "one-two-0000", "--", "sh"})
	if err == nil {
		t.Fatal("-t was accepted on stdin that is not a terminal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("the refusal is %q, and it must say the terminal is missing", err)
	}
}

// execApp wires exec onto fakes and onto a stdin that is a pipe, which is what a test process holds.
func execApp(provider *fakeExecProvider) App {
	app := App{Version: "test", Out: os.Stdout}
	app.newDeps = func(App) *deps {
		return &deps{repoSvc: presentRepo{}, providerSvc: provider}
	}

	return app
}

type fakeExecProvider struct {
	models.Provider

	status models.ExitStatus
	err    error
	// state is what the substrate says the sandbox is doing, and the empty one is running.
	state models.State
	// forgotten is a sandbox the substrate does not hold at all, which a stale record still names.
	forgotten bool
	// oomKilled is a sandbox the host ended for holding too much memory.
	oomKilled bool

	calls int
	id    string
	spec  models.ExecSpec
}

func (f *fakeExecProvider) Name() string { return "fake" }

func (f *fakeExecProvider) Status(context.Context, string) (models.Status, error) {
	if f.forgotten {
		return models.Status{OOMKilled: f.oomKilled}, nil
	}

	state := f.state
	if state == "" {
		state = models.StateRunning
	}

	return models.Status{Exists: true, State: state, OOMKilled: f.oomKilled}, nil
}

func (f *fakeExecProvider) Exec(_ context.Context, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	f.calls++
	f.id, f.spec = id, spec

	return f.status, f.err
}

type presentRepo struct{ sandboxRepo }

func (presentRepo) Get(id string) (models.Sandbox, error) {
	return models.Sandbox{ID: id, State: models.StateRunning}, nil
}

func (presentRepo) Resolve(ref string) (string, error) { return ref, nil }

type pausedRepo struct{ presentRepo }

func (pausedRepo) Get(id string) (models.Sandbox, error) {
	return models.Sandbox{ID: id, State: models.StatePaused, Snapshot: "/snapshots/" + id}, nil
}

// stoppedRepo holds a record that stop wrote, which only stop ever does.
type stoppedRepo struct{ presentRepo }

func (stoppedRepo) Get(id string) (models.Sandbox, error) {
	return models.Sandbox{ID: id, State: models.StateStopped}, nil
}

type missingRepo struct{ sandboxRepo }

func (missingRepo) Get(id string) (models.Sandbox, error) {
	return models.Sandbox{}, errors.New("sandbox " + id + ": sandbox not found")
}

func (missingRepo) Resolve(ref string) (string, error) { return ref, nil }

// TestExecRefusesAStoppedSandboxAsStoppedWhateverItsCgroupCounted pins the record over the cgroup: a
// stop leaves the cgroup and its oom count behind, and that count says nothing about who ended it.
func TestExecRefusesAStoppedSandboxAsStoppedWhateverItsCgroupCounted(t *testing.T) {
	provider := &fakeExecProvider{state: models.StateStopped, oomKilled: true}
	app := execApp(provider)
	app.newDeps = func(App) *deps {
		return &deps{repoSvc: stoppedRepo{}, providerSvc: provider}
	}

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"})
	if err == nil {
		t.Fatal("exec accepted a stopped sandbox")
	}
	if !strings.Contains(err.Error(), "stopped") || strings.Contains(err.Error(), "memory") {
		t.Errorf("the refusal is %q, and the user stopped this sandbox", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a stopped sandbox")
	}
}

// TestExecNamesTheMemoryTheSandboxRanOutOf keeps the one thing an operator cannot find out any other
// way: the exit file records a 137 for this, and so does a plain kill -9.
func TestExecNamesTheMemoryTheSandboxRanOutOf(t *testing.T) {
	provider := &fakeExecProvider{state: models.StateStopped, oomKilled: true}
	app := execApp(provider)

	err := app.Run(context.Background(), []string{"exec", "one-two-0000", "--", "true"})
	if err == nil {
		t.Fatal("exec accepted a sandbox the host ended for its memory")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("the refusal is %q, and it never says the sandbox ran out of memory", err)
	}
	if provider.calls != 0 {
		t.Error("exec reached the provider for a sandbox that is gone")
	}
}
