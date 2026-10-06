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
		{[]string{"setup", "--remote", "https://shard.example.com", "--provider", "gvisor"}, "--remote cannot go with --local, --provider, --start-at-boot, --storage-size, --http-api, --listen or --replace-api-key"},
		{[]string{"setup", "--remote", "https://shard.example.com", "--local"}, "--remote cannot go with --local, --provider, --start-at-boot, --storage-size, --http-api, --listen or --replace-api-key"},
		{[]string{"setup", "--remote", "https://shard.example.com", "--storage-size", "50GiB"}, "--remote cannot go with --local, --provider, --start-at-boot, --storage-size, --http-api, --listen or --replace-api-key"},
		{[]string{"setup", "--remote", "https://shard.example.com", "--http-api", "true"}, "--remote cannot go with --local, --provider, --start-at-boot, --storage-size, --http-api, --listen or --replace-api-key"},
		{[]string{"setup", "--save", "--start-at-boot=false"}, "--save applies only to --remote"},
		{[]string{"setup", "--save", "--listen", "127.0.0.1:7850"}, "--save applies only to --remote"},
		{[]string{"setup", "--save", "--storage-size", "50GiB"}, "drop --local, --provider, --start-at-boot, --storage-size, --http-api, --listen and --replace-api-key"},
		{[]string{"setup", "--storage-size", "50XB"}, `invalid value "50XB" for --storage-size: unknown unit "XB"; want KiB, MiB, GiB, KB, MB or GB`},
		{[]string{"setup", "--storage-size", "1.5GiB"}, "want a whole number; a fraction is never rounded"},
		{[]string{"setup", "--storage-size", "50"}, "want a unit, such as 50MiB or 2GiB"},
		{[]string{"setup", "--start-at-boot=yes"}, "want true or false"},
		{[]string{"setup", "--http-api"}, "--http-api needs a value"},
		{[]string{"setup", "--listen", "127.0.0.1:7850"}, "--listen and --replace-api-key apply only to --http-api true"},
		{[]string{"setup", "--http-api", "false", "--replace-api-key"}, "--listen and --replace-api-key apply only to --http-api true"},
		{[]string{"setup", "--http-api", "true", "--start-at-boot", "false"}, "--http-api true needs --start-at-boot true: the HTTP API runs as a background service"},
		{[]string{"setup", "--http-api", "true", "--listen", "0.0.0.0:7850"}, "--listen: 0.0.0.0:7850 listens on every network"},
		{[]string{"setup", "--http-api", "true", "--listen", "localhost:7850"}, `--listen: "localhost:7850" is not an IP address and port`},
	} {
		err := (&App{Version: "test", Root: t.TempDir(), Out: &bytes.Buffer{}}).run(t.Context(), tc.args)
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
	_, mode := ui.Select(t.Context(), setup.AskMode, "How do you want to use shard?", providers)
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

func TestWithoutATerminalTheStorageSizeIsTheDefault(t *testing.T) {
	ui, out := noTerminalOut(t, setupFlags{})
	got, err := ui.Text(t.Context(), setup.AskStorage, "How much space should shard reserve?", "50GiB")
	if got != "50GiB" || err != nil {
		t.Fatalf("got %q, %v; want the default 50GiB", got, err)
	}
	if want := "How much space should shard reserve? 50GiB, the default, since there is no terminal to ask."; !strings.Contains(out.String(), want) {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

func TestEveryQuestionIsWorded(t *testing.T) {
	for _, q := range []setup.Question{
		setup.AskMode, setup.AskProvider, setup.AskStartAtBoot, setup.AskConfirm, setup.AskURL, setup.AskHTTP,
		setup.AskAPIKey, setup.AskSave, setup.AskSaved, setup.AskExisting, setup.AskRetry, setup.AskSwitch, setup.AskStorage,
		setup.AskHTTPAPI, setup.AskListen, setup.AskReplaceKey,
	} {
		if setupQuestion[q].ask == "" {
			t.Errorf("question %s has no wording", q)
		}
	}
}

func TestALocalRunWithoutATerminalRefusesBeforeAnyCheck(t *testing.T) {
	boot := boolChoice{set: true, value: true}
	fifty := int64(50 << 10)
	for _, tc := range []struct {
		opts setupFlags
		want string
	}{
		{setupFlags{local: true}, "no terminal to choose a provider, choose whether shard starts at boot and confirm the changes: pass --provider, --start-at-boot and -y"},
		{setupFlags{provider: "gvisor", startAtBoot: boot}, "no terminal to confirm the changes: pass -y"},
		{setupFlags{local: true, startAtBoot: boot, yes: true}, "no terminal to choose a provider: pass --provider"},
		{setupFlags{provider: "gvisor", startAtBoot: boot, yes: true}, ""},
		{setupFlags{provider: "gvisor", httpAPI: boot, yes: true}, ""},
		{setupFlags{httpAPI: boot, yes: true}, "no terminal to choose a provider: pass --provider"},
		{setupFlags{storageSize: &fifty}, "no terminal to choose a provider, choose whether shard starts at boot and confirm the changes: pass --provider, --start-at-boot and -y"},
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
		if url, err := ui.Text(t.Context(), setup.AskURL, "URL of the shard server:", ""); err != nil || url != tc.want {
			t.Errorf("the URL is %q, %v; want %q", url, err, tc.want)
		}
		if _, err := ui.Text(t.Context(), setup.AskURL, "URL of the shard server:", ""); err == nil || err.Error() != "no terminal to read the server URL: pass --remote" {
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

var httpAPIOptions = []term.Option{{Name: "true", Label: "Yes"}, {Name: "false", Label: "No", Default: true}}

func TestTheHTTPAPIFlags(t *testing.T) {
	yes := boolChoice{set: true, value: true}
	for _, tc := range []struct {
		name string
		opts setupFlags
		want string
	}{
		{"is No without a terminal or a flag", setupFlags{provider: "gvisor", startAtBoot: yes, yes: true}, "false"},
		{"is the flag", setupFlags{provider: "gvisor", httpAPI: yes, yes: true}, "true"},
		{"is the flag when it says No", setupFlags{provider: "gvisor", startAtBoot: yes, httpAPI: boolChoice{set: true}, yes: true}, "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chosen, err := noTerminal(t, tc.opts).Select(t.Context(), setup.AskHTTPAPI, "Set up the HTTP API?", httpAPIOptions)
			if err != nil || httpAPIOptions[chosen].Name != tc.want {
				t.Fatalf("chose %d, %v; want %s", chosen, err, tc.want)
			}
		})
	}

	ui := noTerminal(t, setupFlags{provider: "gvisor", httpAPI: yes, yes: true})
	if boot, ok := ui.flagged(setup.AskStartAtBoot); !ok || boot != "true" {
		t.Errorf("--http-api true answered the start at boot with %q, %v; want true", boot, ok)
	}
	if replace, err := ui.Confirm(t.Context(), setup.AskReplaceKey, "Replace the API key?", false); err != nil || replace {
		t.Errorf("-y alone replaced the key: %v, %v", replace, err)
	}
	if replace, err := noTerminal(t, setupFlags{httpAPI: yes, replaceKey: true}).Confirm(t.Context(), setup.AskReplaceKey, "Replace the API key?", false); err != nil || !replace {
		t.Errorf("--replace-api-key answered %v, %v; want a replacement", replace, err)
	}
}

func TestTheAddressIsTheFlagOrTheOfferOnlyOnce(t *testing.T) {
	yes := boolChoice{set: true, value: true}
	for _, tc := range []struct {
		listen, want string
	}{
		{"100.64.0.5:7850", "100.64.0.5:7850"},
		{"", "127.0.0.1:9000"},
	} {
		ui := noTerminal(t, setupFlags{httpAPI: yes, listen: tc.listen})
		if address, err := ui.Text(t.Context(), setup.AskListen, "Listen address for the HTTP API:", "127.0.0.1:9000"); err != nil || address != tc.want {
			t.Errorf("the address is %q, %v; want %q", address, err, tc.want)
		}
		if _, err := ui.Text(t.Context(), setup.AskListen, "Listen address for the HTTP API:", "127.0.0.1:9000"); err == nil || err.Error() != "no terminal to read the HTTP API address: pass --listen" {
			t.Errorf("a second ask answered %v, want the person asked", err)
		}
	}
}

func TestHTTPAPITrueSetsUpTheAPIOverAnInstallation(t *testing.T) {
	menu := []term.Option{{Name: "repair"}, {Name: "upgrade"}, {Name: setup.ExistingHTTPAPI, Label: "Set up the HTTP API"}, {Name: "uninstall"}, {Name: "exit"}}
	ui := noTerminal(t, setupFlags{provider: "gvisor", httpAPI: boolChoice{set: true, value: true}, yes: true})
	if chosen, err := ui.Select(t.Context(), setup.AskExisting, "What would you like to do?", menu); err != nil || menu[chosen].Name != setup.ExistingHTTPAPI {
		t.Errorf("--http-api true chose %d, %v; want the HTTP API row", chosen, err)
	}
	menu[2].Unavailable = []string{"this installation does not start shard automatically."}
	if _, err := ui.Select(t.Context(), setup.AskExisting, "What would you like to do?", menu); err == nil || err.Error() != "--http-api true: Set up the HTTP API is unavailable: this installation does not start shard automatically." {
		t.Errorf("an unavailable row gave %v", err)
	}
	if _, err := noTerminal(t, setupFlags{provider: "gvisor", httpAPI: boolChoice{set: true}, yes: true}).Select(t.Context(), setup.AskExisting, "What would you like to do?", menu); err == nil || !strings.HasPrefix(err.Error(), "no terminal to choose what to do with the existing installation") {
		t.Errorf("--http-api false picked a row: %v", err)
	}
}
