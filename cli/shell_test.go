package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
	"github.com/presmihaylov/shard/services/serve"
)

// onTerminal gives the app a 24 by 80 terminal on stdin, as a shell typed at a prompt has.
func onTerminal(t *testing.T, app *App) {
	t.Helper()

	pair, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := pair.Close(); err != nil {
			t.Errorf("close the pair: %v", err)
		}
	})
	if err := pair.Resize(pty.Size{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("resize the pair: %v", err)
	}

	app.in = pair.Replica
}

// guestExecs answers each exec with the next of answers, and keeps every spec the daemon passed in.
type guestExecs struct {
	mu      sync.Mutex
	specs   []models.ExecSpec
	sizes   []pty.Size
	answers []error
	exit    models.ExitStatus
}

func (g *guestExecs) serve(spec models.ExecSpec) (models.ExitStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.specs = append(g.specs, spec)
	if spec.TTY {
		size, err := pty.SizeOf(spec.Stdin)
		if err != nil {
			return models.ExitStatus{}, err
		}
		g.sizes = append(g.sizes, size)
	}

	if len(g.answers) > 0 {
		answer := g.answers[0]
		g.answers = g.answers[1:]
		if answer != nil {
			return models.ExitStatus{}, answer
		}
	}
	spec.Report(42)

	return g.exit, nil
}

func (g *guestExecs) seen() []models.ExecSpec {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.specs)
}

func (g *guestExecs) windows() []pty.Size {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.sizes)
}

func newShellApp(t *testing.T, out *bytes.Buffer) (App, *fakeLifecycleProvider, *guestExecs) {
	t.Helper()

	app, d := newClientApp(t, out, running())
	onTerminal(t, &app)
	provider := d.providerSvc.(*fakeLifecycleProvider)
	guest := &guestExecs{}
	provider.serve = guest.serve

	return app, provider, guest
}

func notFound(reason string) *models.CommandNotStartedError {
	return &models.CommandNotStartedError{Sandbox: "sandbox1", Reason: reason, Code: models.CommandNotFoundExitCode}
}

func TestParseShellTakesTheExecFlagsAndOneSandbox(t *testing.T) {
	opts, err := parseShell([]string{"--workdir", "/work", "--user", "app", "agent"})
	if err != nil {
		t.Fatalf("parseShell: %v", err)
	}

	if want := (shellOptions{id: "agent", workDir: "/work", user: "app"}); opts != want {
		t.Errorf("parseShell = %+v, want %+v", opts, want)
	}
}

// A command after the sandbox is a job for exec, and the refusal says so.
func TestParseShellRefusesAnythingButOneSandbox(t *testing.T) {
	for name, args := range map[string][]string{
		"no sandbox":         {},
		"a command after":    {"agent", "bash"},
		"two sandboxes":      {"agent", "web"},
		"a command after --": {"agent", "--", "ls"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseShell(args)
			if err == nil || !strings.Contains(err.Error(), "shard exec") {
				t.Errorf("parseShell(%q) returned %v, want a refusal that points to shard exec", args, err)
			}
		})
	}
}

// An interactive shell on a pipe would print its prompt into a script, so shell refuses and points to exec.
func TestShellRefusesAStdinThatIsNotATerminal(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	plain, err := os.Create(filepath.Join(t.TempDir(), "not-a-terminal"))
	if err != nil {
		t.Fatalf("create the file: %v", err)
	}
	defer plain.Close()
	app.in = plain

	err = app.Run(t.Context(), []string{"shell", "sandbox1"})
	if err == nil || !strings.Contains(err.Error(), "terminal") || !strings.Contains(err.Error(), "shard exec") {
		t.Fatalf("shell on a plain file returned %v, want a refusal that names the terminal and shard exec", err)
	}
	if slices.Contains(d.providerSvc.(*fakeLifecycleProvider).r.seen(), "provider.Exec") {
		t.Error("shell reached the provider without a terminal")
	}
}

// The choice of shell is the sandbox's, so the one exec carries the script, the flags and this window.
func TestShellOpensAnInteractiveShellOnTheTerminal(t *testing.T) {
	var out bytes.Buffer

	app, _, guest := newShellApp(t, &out)

	if err := app.Run(t.Context(), []string{"shell", "--workdir", "/work", "--user", "app", "sandbox1"}); err != nil {
		t.Fatalf("shell: %v", err)
	}

	specs := guest.seen()
	if len(specs) != 1 {
		t.Fatalf("shell ran %d execs, want 1", len(specs))
	}
	spec := specs[0]
	if want := []string{"sh", "-c", shellScript}; !slices.Equal(spec.Argv, want) {
		t.Errorf("argv = %q, want %q", spec.Argv, want)
	}
	if !spec.TTY || spec.Stdin == nil {
		t.Errorf("tty = %v, stdin = %v, want a terminal to type on", spec.TTY, spec.Stdin)
	}
	if spec.WorkDir != "/work" || spec.User != "app" {
		t.Errorf("workDir = %q, user = %q, want /work and app", spec.WorkDir, spec.User)
	}
	if want := []pty.Size{{Rows: 24, Cols: 80}}; !slices.Equal(guest.windows(), want) {
		t.Errorf("the shell's window is %+v, want %+v", guest.windows(), want)
	}
}

func TestShellExitsWithTheShellsCode(t *testing.T) {
	var out bytes.Buffer

	app, _, guest := newShellApp(t, &out)
	guest.exit = models.ExitStatus{Code: 3}

	err := app.Run(t.Context(), []string{"shell", "sandbox1"})

	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 || exit.Message != "" {
		t.Fatalf("shell returned %v, want the shell's exit code 3 and no message", err)
	}
}

// A sandbox with neither bash nor sh fails the probe from / as well, and the error names the sandbox.
func TestShellNamesASandboxWithNoShell(t *testing.T) {
	var out bytes.Buffer

	app, _, guest := newShellApp(t, &out)
	guest.answers = []error{notFound("failed to load sh: no such file or directory"), notFound("failed to load sh: no such file or directory")}

	err := app.Run(t.Context(), []string{"shell", "sandbox1"})

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("shell returned %v, want an ExitError", err)
	}
	if exit.Code != models.CommandNotFoundExitCode || !strings.Contains(exit.Message, "found no shell in sandbox sandbox1") {
		t.Errorf("shell exited %d with %q, want 127 and the sandbox with no shell named", exit.Code, exit.Message)
	}
	specs := guest.seen()
	if len(specs) != 2 || specs[1].WorkDir != "/" || specs[1].TTY {
		t.Errorf("the probe was %+v, want a second exec from / with no terminal", specs)
	}
}

// A missing --workdir refuses with the same 127, so the probe that passes keeps the reason the shell gave.
func TestShellKeepsTheReasonWhenTheSandboxHasAShell(t *testing.T) {
	var out bytes.Buffer

	app, _, guest := newShellApp(t, &out)
	reason := `failed to find initial working directory "/missing": no such file or directory`
	guest.answers = []error{notFound(reason)}

	err := app.Run(t.Context(), []string{"shell", "--workdir", "/missing", "--user", "app", "sandbox1"})

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("shell returned %v, want an ExitError", err)
	}
	if exit.Code != models.CommandNotFoundExitCode || !strings.Contains(exit.Message, reason) || strings.Contains(exit.Message, "no shell") {
		t.Errorf("shell exited %d with %q, want 127 and the missing directory named", exit.Code, exit.Message)
	}
	specs := guest.seen()
	if len(specs) != 2 || specs[1].WorkDir != "/" || specs[1].User != "app" {
		t.Errorf("the probe was %+v, want a second exec from / as the same user", specs)
	}
}

// shell rides the exec routes, so a token of the exec scope alone opens one through the front and a read-only one does not.
func TestShellThroughTheFrontNeedsTheExecScopeAlone(t *testing.T) {
	for name, scopes := range map[string][]string{
		"exec":         {models.ScopeExec},
		"sandbox:read": {models.ScopeSandboxRead},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer

			app, provider, guest := newShellApp(t, &out)
			f := scopedFront(t, app, scopes)

			err := app.Run(t.Context(), append(f.use(t), "shell", "sandbox1"))

			reached := slices.Contains(provider.r.seen(), "provider.Exec")
			if name == "exec" && (err != nil || !reached || len(guest.seen()) != 1) {
				t.Fatalf("shell with an exec token returned %v, reached the guest %v", err, reached)
			}
			if name != "exec" && (err == nil || reached) {
				t.Fatalf("shell with a %s token returned %v, reached the guest %v, want a refusal", name, err, reached)
			}
		})
	}
}

// scopedFront serves a front over the app's daemon with a key of only the given scopes.
func scopedFront(t *testing.T, app App, scopes []string) front {
	t.Helper()

	secret := filepath.Join(t.TempDir(), "signing-key")
	if err := os.WriteFile(secret, []byte(frontSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write the signing key file: %v", err)
	}

	minted, err := serve.IssueToken([]byte(frontSecret), serve.TokensPath(secret), "cli", scopes, time.Hour)
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	address, cert := startFront(t, serve.Config{Listen: "127.0.0.1:0", SigningKeyFile: secret, Root: app.Root, Out: io.Discard}, true)

	return front{url: "https://" + address, key: minted.Token, ca: cert}
}
