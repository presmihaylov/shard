package models

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrUnsupported is the sentinel behind every refused verb. Match it with errors.Is.
var ErrUnsupported = errors.New("verb not supported")

// ErrNoExitStatus is what a sandbox that was killed leaves behind: the supervisor died before it
// could record how the entrypoint ended. It is a normal outcome of a stop, not a failure.
var ErrNoExitStatus = errors.New("the sandbox ended before its entrypoint exited")

// ErrExitFileTooLarge is an exit file past any record shard-init writes, which only a guest that reached the file can make.
var ErrExitFileTooLarge = errors.New("the exit file is larger than any exit record")

// ErrExitChannelReplaced is a PID 1 whose fd 0 is no longer the sealed channel create gave it, which only guest root can do.
var ErrExitChannelReplaced = errors.New("exit channel replaced")

// ErrExecLost is a command that started while the substrate lost its wait on it, so how it ended is unknown.
var ErrExecLost = errors.New("the substrate lost its wait on the command")

// ErrImageGone is an image whose files left the host while a sandbox still stacks over them.
var ErrImageGone = errors.New("its image is gone from the host")

// ErrLostState is a run whose state files say nothing true, since the substrate could not land one of its events.
var ErrLostState = errors.New("lost its lifecycle state")

// CommandNotStartedError is a command a sandbox refused to start, which is no exit code of that
// command: it never ran. Code is what a shell answers for the same refusal.
type CommandNotStartedError struct {
	Sandbox string
	// Command is the program as the caller named it, argv[0]; empty where only the reason reached this side.
	Command string
	// Reason is the substrate's own words for why the command did not start.
	Reason string
	Code   int
}

func (e *CommandNotStartedError) Error() string {
	command := "the command"
	if e.Command != "" {
		command = strconv.Quote(e.Command)
	}

	return fmt.Sprintf("sandbox %s could not run %s: %s", e.Sandbox, command, e.Reason)
}

// Public answers the whole text, which names the caller's command and the kernel's or runtime's reason, never a host path.
func (e *CommandNotStartedError) Public() string { return e.Error() }

// The optional verbs, spelled once here so a refusal and the conformance suite cannot drift apart.
const (
	VerbPause  = "pause"
	VerbResume = "resume"
	VerbFork   = "fork"
	VerbPort   = "port"
	VerbSwap   = "swap"
)

// UnsupportedError names the provider and the verb, because shard refuses rather than downgrades.
type UnsupportedError struct {
	Provider string
	// Verb is one of the Verb constants above, never the Go method name.
	Verb string
}

func Unsupported(provider, verb string) error {
	return &UnsupportedError{Provider: provider, Verb: verb}
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("provider %s does not support %s on this host", e.Provider, e.Verb)
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

func (e *UnsupportedError) Public() string {
	return fmt.Sprintf("%s; use a server that supports %s", e.Error(), e.Verb)
}

// NotFoundError marks a lookup miss whose text names only what the caller asked for, so a public route may answer it.
type NotFoundError struct {
	Err error
}

// NotFound words a miss as one sentence, while errors.Is still finds sentinel.
func NotFound(sentinel error, text string) error {
	return &NotFoundError{Err: worded{text: text, cause: sentinel}}
}

func (e *NotFoundError) Error() string { return e.Err.Error() }

func (e *NotFoundError) Unwrap() error { return e.Err }

func (e *NotFoundError) Public() string { return e.Err.Error() }

type worded struct {
	text  string
	cause error
}

func (w worded) Error() string { return w.text }

func (w worded) Unwrap() error { return w.cause }

// UnresponsiveError is a verb the substrate refused because the process behind the sandbox missed its probe bound.
type UnresponsiveError struct {
	Sandbox  string
	Provider string
	Verb     string
	// Reason is the words Status.Reason carries for the silent process, so a caller records them without asking again.
	Reason string
}

func (e *UnresponsiveError) Error() string {
	return fmt.Sprintf("sandbox %s is %s on %s: %s takes a running sandbox: %s", e.Sandbox, StateUnresponsive, e.Provider, e.Verb, e.Reason)
}

// LostError is a verb that failed after the substrate had already ended the sandbox, so its record ends failed.
type LostError struct {
	Sandbox string
	Err     error
}

func (e *LostError) Error() string {
	return fmt.Sprintf("sandbox %s is lost: %v", e.Sandbox, e.Err)
}

func (e *LostError) Unwrap() error { return e.Err }

func (e *LostError) Public() string {
	return fmt.Sprintf("sandbox %s is lost; remove it and create another sandbox", e.Sandbox)
}

// EntrypointNotStartedError is a sandbox whose entrypoint never ran; Err quotes the sandbox log, which can name a host path.
type EntrypointNotStartedError struct {
	Sandbox string
	Err     error
}

func (e *EntrypointNotStartedError) Error() string {
	return fmt.Sprintf("the entrypoint of sandbox %s did not start: %v", e.Sandbox, e.Err)
}

func (e *EntrypointNotStartedError) Unwrap() error { return e.Err }

func (e *EntrypointNotStartedError) Public() string {
	return fmt.Sprintf("the entrypoint of sandbox %s did not start; the daemon log has the cause", e.Sandbox)
}
