package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/term"
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
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(in.Close(), w.Close()); err != nil {
			t.Error(err)
		}
	})

	return &answers{opts: opts, t: term.New(in, &bytes.Buffer{}, func(string) string { return "" })}
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
	_, provider := ui.Select(t.Context(), setup.AskProvider, "Provider", providers)
	_, key := ui.Secret(t.Context(), setup.AskAPIKey, "API key")
	_, retry := ui.Select(t.Context(), setup.AskRetry, "Retry", providers)

	for got, want := range map[error]string{
		provider: "no terminal to ask provider: pass --provider",
		key:      "no terminal to read the API key: set SHARD_API_KEY",
		retry:    "no terminal to ask retry: run shard setup in a terminal",
	} {
		if got == nil || got.Error() != want {
			t.Errorf("got %v, want %q", got, want)
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
