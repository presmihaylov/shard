// Package setup is the one implementation of shard setup. The wizard and the flags answer the same
// questions through UI, so an automated run takes every path a person can.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
)

// DataDir is the one data directory setup provisions; it never asks for another.
const DataDir = "/var/lib/shard"

// Host is the machine setup inspects and changes. A test points Root at a temp dir and Releases at a fake server.
type Host struct {
	// Root prefixes every path setup reads or writes: "/" on a real host.
	Root string
	OS   string
	Arch string
	// Euid is the user setup runs as: 0 runs a privileged step as it is, any other runs it under sudo.
	Euid int
	// Executable is the running shard binary, and Version is its release tag.
	Executable string
	Version    string
	// Releases is the base URL of the release server.
	Releases string
	HTTP     *http.Client
	Env      func(string) string
	// Run runs one command as it is given; a step that needs root wraps it in sudo itself.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath finds a command on PATH, so a hint knows whether shard is reachable by name yet.
	LookPath func(string) (string, error)
}

// releases is where setup looks up and downloads a shard release.
const releases = "https://api.github.com/repos/presmihaylov/shard/releases"

// NewHost is this machine, as the running binary of version sees it.
func NewHost(version string) (Host, error) {
	executable, err := os.Executable()
	if err != nil {
		return Host{}, fmt.Errorf("find the running shard binary: %w", err)
	}

	return Host{
		Root:       "/",
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Euid:       os.Geteuid(),
		Executable: executable,
		Version:    version,
		Releases:   releases,
		HTTP:       newHTTPClient(idleTimeout),
		Env:        os.Getenv,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		LookPath: exec.LookPath,
	}, nil
}

// retryCommand is "shard" once it is on PATH, else the full path of the running binary, so the hint always works.
func (h Host) retryCommand() string {
	if h.LookPath != nil {
		if _, err := h.LookPath("shard"); err == nil {
			return "shard"
		}
	}
	if h.Executable == "" {
		return "shard"
	}

	return h.Executable
}

// Question names one choice, so the flags can answer it and a run without a terminal can name the flag it lacks.
type Question string

const (
	AskMode        Question = "mode"
	AskProvider    Question = "provider"
	AskStartAtBoot Question = "start-at-boot"
	AskConfirm     Question = "confirm"
	AskURL         Question = "url"
	AskHTTP        Question = "http"
	AskAPIKey      Question = "api-key"
	AskSave        Question = "save"
	AskSaved       Question = "saved"
	AskExisting    Question = "existing"
	AskRetry       Question = "retry"
	AskSwitch      Question = "switch"
)

// UI is where every answer comes from and every line goes: a person at a terminal, the flags, or a test.
type UI interface {
	Select(ctx context.Context, q Question, title string, options []term.Option) (int, error)
	// Confirm asks yes or no, and yes is the answer an empty reply takes.
	Confirm(ctx context.Context, q Question, text string, yes bool) (bool, error)
	// Text asks for one line that starts as initial, which Enter keeps.
	Text(ctx context.Context, q Question, prompt, initial string) (string, error)
	// Secret reads a value that never echoes and never lands in a log.
	Secret(ctx context.Context, q Question, prompt string) (string, error)
	Checklist(title string, steps []string) (Checklist, error)
	Print(lines ...string) error
}

// Checklist is the live list of one job's steps; term.Checklist is the one a terminal draws.
type Checklist interface {
	Start(i int) error
	Done(i int) error
	Attention(i int, detail ...string) error
	Fail(i int, detail ...string) error
	// Progress updates the running step's detail lines, so a long step like a download can show how far it is.
	Progress(i int, detail ...string) error
}

// Step is one line of a checklist and the change it makes. Do is safe to run again after a stop.
type Step struct {
	Title string
	Do    func(ctx context.Context) error
}

// StoppedError is a step that failed: the steps before it stay in place and a second run retries.
type StoppedError struct {
	Step string
	Err  error
}

func (e *StoppedError) Error() string { return fmt.Sprintf("%s: %v", e.Step, e.Err) }

func (e *StoppedError) Unwrap() error { return e.Err }

// Problem is a step error worded for a person: apply prints its lines under the failed step.
type Problem struct {
	Lines []string
}

func (p *Problem) Error() string { return strings.Join(p.Lines, "; ") }

// ErrDeclined is a person who said no at the confirmation, which changed nothing.
var ErrDeclined = errors.New("setup was cancelled; nothing changed")

// Setup is one run of shard setup.
type Setup struct {
	Host Host
	UI   UI
	// RetrySuffix repeats this run's flags in the retry hint, so a no-TTY run that fails can be re-run without a terminal.
	RetrySuffix string
	// step reports progress on the running step; apply sets it per step, and it is nil outside apply.
	step func(detail ...string) error
}

// The first choice, in the order the wizard shows it.
const (
	modeLocal = iota
	modeRemote
)

// localOption is the first screen's choice to set up this machine; why says what no provider here can run.
func localOption(why []string) term.Option {
	return term.Option{Name: "local", Label: "Run sandboxes on this machine", Lines: why}
}

// Run asks the first question: the saved connection's menu when there is one, else local or remote.
func (s *Setup) Run(ctx context.Context) error {
	path, err := client.ConfigPath(s.Host.Env)
	if err != nil {
		return err
	}
	saved, err := client.LoadConfig(path)
	if err != nil {
		return err
	}
	why := noProvider(Providers(ctx, s.Host))
	if saved.Remote != "" {
		return s.saved(ctx, path, saved, why)
	}

	local := localOption(why)
	local.Default = why == nil
	mode, err := s.UI.Select(ctx, AskMode, "How do you want to use shard?", []term.Option{
		modeLocal:  local,
		modeRemote: {Name: "remote", Label: "Connect to a remote server", Default: why != nil},
	})
	if err != nil {
		return err
	}
	if mode == modeRemote {
		return s.connect(ctx, path, client.Config{})
	}

	return s.runLocal(ctx)
}

// runLocal offers the existing installation's menu when there is one, else a fresh local setup.
func (s *Setup) runLocal(ctx context.Context) error {
	inst, found, err := Detect(ctx, s.Host)
	if err != nil {
		return fmt.Errorf("look for an existing installation: %w", err)
	}
	if found {
		return s.existing(ctx, inst)
	}
	h, err := s.switchToLocal(ctx)
	if err != nil {
		return err
	}

	return s.local(ctx, h)
}

// switched runs a local job after the offer to drop a saved remote, and drops it only once the job succeeds; removal is the job's review line.
func (s *Setup) switched(ctx context.Context, job func(ctx context.Context, removal string) error) error {
	h, err := s.switchToLocal(ctx)
	if err != nil {
		return err
	}
	if err := job(ctx, h.review); err != nil {
		return err
	}
	lines, err := h.finish(ctx)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}

	return s.UI.Print(append([]string{""}, lines...)...)
}

// apply runs the steps under a live checklist, and on a failure marks the step, says what stays and stops.
func (s *Setup) apply(ctx context.Context, title string, steps []Step) error {
	titles := make([]string, 0, len(steps))
	for _, step := range steps {
		titles = append(titles, step.Title)
	}
	list, err := s.UI.Checklist(title, titles)
	if err != nil {
		return err
	}

	for i, step := range steps {
		if err := list.Start(i); err != nil {
			return err
		}
		s.step = func(detail ...string) error { return list.Progress(i, detail...) }
		err := step.Do(ctx)
		s.step = nil
		if err != nil {
			return errors.Join(
				list.Fail(i, problemLines(err)...),
				s.UI.Print("", "Setup stopped. Earlier completed steps remain in place.", fmt.Sprintf("Run %s setup%s again to retry.", s.Host.retryCommand(), s.RetrySuffix)),
				&StoppedError{Step: step.Title, Err: err},
			)
		}
		if err := list.Done(i); err != nil {
			return err
		}
	}

	return nil
}

// problemLines are the lines a Problem gives, or the error itself when the step returned none.
func problemLines(err error) []string {
	var problem *Problem
	if errors.As(err, &problem) {
		return problem.Lines
	}

	return []string{err.Error()}
}
