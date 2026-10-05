package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/setup"
)

func TestSetupRefusesWhatItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"setup", "now"}, "setup takes no arguments"},
		{[]string{"setup", "--remote", "https://shard.example.com", "--provider", "gvisor"}, "--remote connects to a server"},
		{[]string{"setup", "--remote", "https://shard.example.com", "--local"}, "--remote connects to a server"},
		{[]string{"setup", "--save", "--start-at-boot=false"}, "--save saves a remote connection"},
		{[]string{"setup", "--start-at-boot=yes"}, "want true or false"},
	} {
		err := (App{Version: "test", Root: t.TempDir(), Out: &bytes.Buffer{}}).run(t.Context(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("shard %s: %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// noTerminal is the answers of these flags with stdin a pipe, the way a script runs setup.
func noTerminal(t *testing.T, opts setupFlags) *answers {
	t.Helper()
	ui, _ := noTerminalOut(t, opts)

	return ui
}

// noTerminalOut is noTerminal with the buffer its lines go to.
func noTerminalOut(t *testing.T, opts setupFlags) (*answers, *bytes.Buffer) {
	t.Helper()
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(in.Close(), w.Close()); err != nil {
			t.Error(err)
		}
	})

	none := func(string) string { return "" }
	out := &bytes.Buffer{}

	return &answers{opts: opts, t: term.New(in, out, none), env: none}, out
}

var providers = []term.Option{
	{Name: "firecracker", Label: "Firecracker", Unavailable: []string{"/dev/kvm is missing"}},
	{Name: "gvisor", Label: "gVisor"},
}

func TestAFlagPicksItsOptionOrSaysWhyNot(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     int
		err      string
	}{
		{"gvisor", 1, ""},
		{"firecracker", 0, "--provider firecracker: Firecracker is unavailable: /dev/kvm is missing"},
		{"kata", 0, `--provider "kata": want firecracker or gvisor`},
	} {
		chosen, err := noTerminal(t, setupFlags{provider: tc.provider}).Select(t.Context(), setup.AskProvider, "Provider", providers)
		if tc.err == "" && (err != nil || chosen != tc.want) {
			t.Errorf("--provider %s chose %d, %v", tc.provider, chosen, err)
		}
		if tc.err != "" && (err == nil || err.Error() != tc.err) {
			t.Errorf("--provider %s: %v, want %q", tc.provider, err, tc.err)
		}
	}
}

func TestWithoutATerminalTheErrorNamesWhatAnswers(t *testing.T) {
	ui := noTerminal(t, setupFlags{})
	_, mode := ui.Select(t.Context(), setup.AskMode, "How do you want to use Shard?", providers)
	_, provider := ui.Select(t.Context(), setup.AskProvider, "Provider", providers)
	_, confirm := ui.Confirm(t.Context(), setup.AskConfirm, "Continue?", true)
	_, key := ui.Secret(t.Context(), setup.AskAPIKey, "API key")
	_, existing := ui.Select(t.Context(), setup.AskExisting, "What would you like to do?", providers)

	// Each question is worded for a person and keeps the option that answers it (SHARD-740).
	for got, want := range map[error]string{
		mode:     "no terminal to choose local or remote setup: pass --local or --remote <url>",
		provider: "no terminal to choose a provider: pass --provider",
		confirm:  "no terminal to confirm the changes: pass -y",
		key:      "no terminal to read the API key: set SHARD_API_KEY",
		existing: "no terminal to choose what to do with the existing installation: run shard setup in a terminal",
	} {
		if got == nil || got.Error() != want {
			t.Errorf("got %v, want %q", got, want)
		}
	}
}

func TestEveryQuestionIsWorded(t *testing.T) {
	for _, q := range []setup.Question{
		setup.AskMode, setup.AskProvider, setup.AskStartAtBoot, setup.AskConfirm, setup.AskURL, setup.AskHTTP,
		setup.AskAPIKey, setup.AskSave, setup.AskSaved, setup.AskExisting, setup.AskRetry, setup.AskSwitch,
	} {
		if setupQuestion[q].ask == "" {
			t.Errorf("question %s has no wording", q)
		}
	}
}

func TestALocalRunWithoutATerminalRefusesBeforeAnyCheck(t *testing.T) {
	boot := boolChoice{set: true, value: true}
	for _, tc := range []struct {
		opts setupFlags
		want string
	}{
		{setupFlags{local: true}, "no terminal to choose a provider, choose whether shard starts at boot and confirm the changes: pass --provider, --start-at-boot and -y"},
		{setupFlags{provider: "gvisor", startAtBoot: boot}, "no terminal to confirm the changes: pass -y"},
		{setupFlags{local: true, startAtBoot: boot, yes: true}, "no terminal to choose a provider: pass --provider"},
		{setupFlags{provider: "gvisor", startAtBoot: boot, yes: true}, ""},
		{setupFlags{remote: "https://shard.example.com"}, ""},
		{setupFlags{}, ""},
	} {
		err := noTerminal(t, tc.opts).unattended()
		if tc.want == "" && err != nil {
			t.Errorf("%+v refused: %v", tc.opts, err)
		}
		if tc.want != "" && (err == nil || err.Error() != tc.want) {
			t.Errorf("%+v: %v, want %q", tc.opts, err, tc.want)
		}
	}
}

func TestYesConfirmsButMakesNoChoice(t *testing.T) {
	ui := noTerminal(t, setupFlags{local: true, yes: true})
	for q, want := range map[setup.Question]bool{setup.AskConfirm: true, setup.AskHTTP: true, setup.AskSave: false, setup.AskSwitch: false} {
		got, err := ui.Confirm(t.Context(), q, "?", true)
		if err != nil || got != want {
			t.Errorf("-y answered %s with %v, %v; want %v", q, got, err, want)
		}
	}
	if mode, ok := ui.flagged(setup.AskMode); !ok || mode != "local" {
		t.Errorf("--local picked %q", mode)
	}
}

// A flag that answers yes still prints the warning above the question (SHARD-739).
func TestASaveFlagStillPrintsThePlainTextWarning(t *testing.T) {
	ui, out := noTerminalOut(t, setupFlags{remote: "https://shard.example.com", save: true})
	if save, err := ui.Confirm(t.Context(), setup.AskSave, "The key is plain text.\nSave?", true); err != nil || !save {
		t.Fatalf("--save answered %v, %v; want a save", save, err)
	}
	if got := out.String(); got != "The key is plain text.\n" {
		t.Errorf("--save printed %q, want the warning alone", got)
	}
}

func TestTheURLIsTheFlagOrTheEnvironmentOnlyOnce(t *testing.T) {
	for _, tc := range []struct {
		flag, env, want string
	}{
		{"https://flag.example.com", "https://env.example.com", "https://flag.example.com"},
		{"", "https://env.example.com", "https://env.example.com"},
	} {
		ui := noTerminal(t, setupFlags{remote: tc.flag})
		ui.env = func(name string) string { return map[string]string{client.RemoteEnv: tc.env}[name] }
		if url, err := ui.Text(t.Context(), setup.AskURL, "Shard server URL:", ""); err != nil || url != tc.want {
			t.Errorf("the URL is %q, %v; want %q", url, err, tc.want)
		}
		if _, err := ui.Text(t.Context(), setup.AskURL, "Shard server URL:", ""); err == nil || err.Error() != "no terminal to read the server URL: pass --remote" {
			t.Errorf("an edit after a failed check answered %v, want the person asked", err)
		}
	}
}

func TestARemoteFlagReplacesASavedConnectionAndAFailedCheckExits(t *testing.T) {
	ui := noTerminal(t, setupFlags{remote: "https://shard.example.com", save: true, yes: true})
	saved := []term.Option{{Name: "check"}, {Name: "replace"}, {Name: "remove"}, {Name: "local"}, {Name: "exit"}}
	if chosen, err := ui.Select(t.Context(), setup.AskSaved, "What would you like to do?", saved); err != nil || saved[chosen].Name != "replace" {
		t.Errorf("--remote chose %d, %v; want replace", chosen, err)
	}
	retry := []term.Option{{Name: "retry"}, {Name: "edit"}, {Name: "exit"}}
	if chosen, err := ui.Select(t.Context(), setup.AskRetry, "What would you like to do?", retry); err != nil || retry[chosen].Name != "exit" {
		t.Errorf("a failed check without a terminal chose %d, %v; want exit", chosen, err)
	}
	// A local option picks local setup from the saved connection's menu (SHARD-741).
	if chosen, err := noTerminal(t, setupFlags{provider: "gvisor"}).Select(t.Context(), setup.AskSaved, "What would you like to do?", saved); err != nil || saved[chosen].Name != "local" {
		t.Errorf("--provider chose %d, %v; want local", chosen, err)
	}
}

func TestSetupExitsOnceItSaidWhy(t *testing.T) {
	refused := errors.New("--provider kata: want firecracker or gvisor")
	for _, tc := range []struct {
		err     error
		code    int
		message string
	}{
		{&setup.StoppedError{Step: "Install gVisor", Err: errors.New("timed out")}, 1, ""},
		{&setup.StoppedError{Step: "Install gVisor", Err: context.Canceled}, InterruptedExitCode, ""},
		{fmt.Errorf("select mode: %w", term.ErrInterrupted), InterruptedExitCode, "setup interrupted"},
		{fmt.Errorf("read the terminal: %w", context.Canceled), InterruptedExitCode, "setup interrupted"},
	} {
		var exit *ExitError
		if !errors.As(setupExit(tc.err), &exit) || exit.Code != tc.code || exit.Message != tc.message {
			t.Errorf("%v exits %+v, want code %d and %q", tc.err, exit, tc.code, tc.message)
		}
	}
	if err := setupExit(refused); !errors.Is(err, refused) {
		t.Errorf("a refusal became %v", err)
	}
	if err := setupExit(nil); err != nil {
		t.Errorf("a clean run exits with %v", err)
	}
}
