package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/setup"
)

// setupFlags are the choices a command line gives shard setup ahead of the wizard.
type setupFlags struct {
	local       bool
	remote      string
	provider    string
	startAtBoot boolChoice
	save        bool
	yes         bool
}

// boolChoice is a flag that takes true or false as its value, so a bare --start-at-boot is refused.
type boolChoice struct {
	set   bool
	value bool
}

func (b *boolChoice) String() string {
	if !b.set {
		return ""
	}

	return fmt.Sprint(b.value)
}

func (b *boolChoice) Set(value string) error {
	switch value {
	case "true":
		b.set, b.value = true, true
	case "false":
		b.set, b.value = true, false
	default:
		return fmt.Errorf("want true or false, got %q", value)
	}

	return nil
}

func (a App) setup(ctx context.Context, args []string) error {
	var opts setupFlags
	flags := newFlags("setup")
	flags.BoolVar(&opts.local, "local", false, "")
	flags.StringVar(&opts.remote, "remote", "", "")
	flags.StringVar(&opts.provider, "provider", "", "")
	flags.Var(&opts.startAtBoot, "start-at-boot", "")
	flags.BoolVar(&opts.save, "save", false, "")
	flags.BoolVar(&opts.yes, "y", false, "")
	flags.BoolVar(&opts.yes, "yes", false, "")
	if err := parseVerb(flags, args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("setup takes no arguments, got %s", gotArgs(flags.Args()))
	}
	if err := opts.check(); err != nil {
		return err
	}

	host, err := setup.NewHost(a.Version)
	if err != nil {
		return err
	}
	ui := &answers{opts: opts, t: term.New(a.stdin(), a.Out, os.Getenv), env: os.Getenv}
	if err := ui.unattended(); err != nil {
		return err
	}
	run := setup.Setup{Host: host, UI: ui}

	return setupExit(run.Run(ctx))
}

// setupExit is the exit a setup error takes: a stopped step already said why under its checklist, and an interrupt leaves as SIGINT does.
func setupExit(err error) error {
	interrupted := errors.Is(err, term.ErrInterrupted) || errors.Is(err, context.Canceled)
	var stopped *setup.StoppedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &stopped) && interrupted:
		return &ExitError{Code: InterruptedExitCode}
	case errors.As(err, &stopped):
		return &ExitError{Code: 1}
	case interrupted:
		return &ExitError{Code: InterruptedExitCode, Message: "setup interrupted"}
	}

	return err
}

// wantsLocal is a flag that only local setup takes.
func (o setupFlags) wantsLocal() bool { return o.local || o.provider != "" || o.startAtBoot.set }

// check refuses a local choice beside a remote one, so neither half guesses which was meant.
func (o setupFlags) check() error {
	if o.remote != "" && o.wantsLocal() {
		return errors.New("--remote connects to a server; --local, --provider and --start-at-boot set up this machine")
	}
	if o.save && o.wantsLocal() {
		return errors.New("--save saves a remote connection; it does not apply to --local")
	}

	return nil
}

// answers puts the flags in front of the terminal: a flag answers its question, else a person does, else the error names the flag.
type answers struct {
	opts setupFlags
	t    *term.Terminal
	env  func(string) string
	// urlAsked is set once the URL was answered, so an edit after a failed check asks the person and never loops on the flag.
	urlAsked bool
}

// flagged is the option name a flag picks for a question, and whether one does.
func (a *answers) flagged(q setup.Question) (string, bool) {
	switch q {
	case setup.AskMode:
		if a.opts.remote != "" || a.opts.save {
			return "remote", true
		}
		if a.opts.wantsLocal() {
			return "local", true
		}
	case setup.AskProvider:
		return a.opts.provider, a.opts.provider != ""
	case setup.AskStartAtBoot:
		return a.opts.startAtBoot.String(), a.opts.startAtBoot.set
	case setup.AskSaved:
		if a.opts.remote != "" {
			return "replace", true
		}
		if a.opts.wantsLocal() {
			return "local", true
		}
	case setup.AskRetry:
		// A failed check with nobody to ask leaves with its reason rather than an error about the terminal.
		return "exit", !a.t.Interactive()
	}

	return "", false
}

func (a *answers) Select(ctx context.Context, q setup.Question, title string, options []term.Option) (int, error) {
	if name, ok := a.flagged(q); ok {
		return pick(q, options, name)
	}
	chosen, err := a.t.Select(ctx, title, options)

	return chosen, need(q, err)
}

// pick is the option a flag names; an unavailable one is refused with its reason, never swapped for another.
func pick(q setup.Question, options []term.Option, name string) (int, error) {
	var names []string
	for i, o := range options {
		if o.Name != name {
			names = append(names, o.Name)
			continue
		}
		if len(o.Unavailable) > 0 {
			return 0, fmt.Errorf("%s %s: %s is unavailable: %s", setupQuestion[q].flag, name, o.Label, o.Unavailable[0])
		}

		return i, nil
	}

	return 0, fmt.Errorf("%s %q: want %s", setupQuestion[q].flag, name, orList(names))
}

func (a *answers) Confirm(ctx context.Context, q setup.Question, text string, yes bool) (bool, error) {
	switch {
	case slices.Contains(confirmations, q) && a.opts.yes:
		return true, a.note(text)
	case q == setup.AskSave && a.opts.save:
		return true, a.note(text)
	case q == setup.AskSave && (a.opts.yes || !a.t.Interactive()):
		return false, nil
	case !slices.Contains(confirmations, q) && !a.t.Interactive():
		// A choice no flag names keeps things as they are when nobody is there to ask.
		return false, nil
	}
	answer, err := a.t.Confirm(ctx, text, yes)

	return answer, need(q, err)
}

// note prints the lines above a question a flag answered yes, so a warning there reaches the reader all the same.
func (a *answers) note(text string) error {
	lines := strings.Split(text, "\n")

	return a.t.Print(lines[:len(lines)-1]...)
}

func (a *answers) Text(ctx context.Context, q setup.Question, prompt, initial string) (string, error) {
	if q == setup.AskURL && !a.urlAsked {
		a.urlAsked = true
		if url := cmp.Or(a.opts.remote, a.env(client.RemoteEnv)); url != "" {
			return url, nil
		}
	}
	answer, err := a.t.Text(ctx, prompt, initial)

	return answer, need(q, err)
}

func (a *answers) Secret(ctx context.Context, q setup.Question, prompt string) (string, error) {
	answer, err := a.t.Secret(ctx, prompt)

	return answer, need(q, err)
}

func (a *answers) Checklist(title string, steps []string) (setup.Checklist, error) {
	list, err := a.t.Checklist(title, steps)
	if err != nil {
		return nil, err
	}

	return list, nil
}

func (a *answers) Print(lines ...string) error { return a.t.Print(lines...) }

// confirmations are the questions -y answers; the rest are choices it never makes.
var confirmations = []setup.Question{setup.AskConfirm, setup.AskHTTP}

// setupQuestion words each question for a person, with the option that answers it, which a run without a terminal must name.
var setupQuestion = map[setup.Question]struct{ ask, flag string }{
	setup.AskMode:        {"choose local or remote setup", "--local or --remote <url>"},
	setup.AskProvider:    {"choose a provider", "--provider"},
	setup.AskStartAtBoot: {"choose whether shard starts at boot", "--start-at-boot"},
	setup.AskConfirm:     {"confirm the changes", "-y"},
	setup.AskHTTP:        {"confirm a connection over HTTP", "-y"},
	setup.AskURL:         {"read the server URL", "--remote"},
	setup.AskAPIKey:      {"read the API key", client.APIKeyEnv},
	setup.AskSave:        {"confirm the save of the connection", "--save"},
	setup.AskSaved:       {"choose what to do with the saved connection", "--local or --remote <url>"},
	setup.AskExisting:    {"choose what to do with the existing installation", ""},
	setup.AskRetry:       {"choose what to do after the failed check", ""},
	setup.AskSwitch:      {"confirm the removal of the saved connection", ""},
}

// need words a question asked without a terminal as the option that answers it.
func need(q setup.Question, err error) error {
	if !errors.Is(err, term.ErrNotTerminal) {
		return err
	}
	question := setupQuestion[q]
	switch {
	case q == setup.AskAPIKey:
		return fmt.Errorf("no terminal to %s: set %s", question.ask, question.flag)
	case question.flag != "":
		return fmt.Errorf("no terminal to %s: pass %s", question.ask, question.flag)
	}

	return fmt.Errorf("no terminal to %s: run shard setup in a terminal", question.ask)
}

// unattended refuses a local run without a terminal before any check, when an answer it will need has no option.
func (a *answers) unattended() error {
	if a.t.Interactive() || !a.opts.wantsLocal() {
		return nil
	}
	var missing []setup.Question
	if a.opts.provider == "" {
		missing = append(missing, setup.AskProvider)
	}
	if !a.opts.startAtBoot.set {
		missing = append(missing, setup.AskStartAtBoot)
	}
	if !a.opts.yes {
		missing = append(missing, setup.AskConfirm)
	}
	if len(missing) == 0 {
		return nil
	}
	asks := make([]string, 0, len(missing))
	flags := make([]string, 0, len(missing))
	for _, q := range missing {
		asks = append(asks, setupQuestion[q].ask)
		flags = append(flags, setupQuestion[q].flag)
	}

	return fmt.Errorf("no terminal to %s: pass %s", andList(asks), andList(flags))
}
