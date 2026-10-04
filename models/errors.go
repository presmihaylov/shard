package models

import (
	"errors"
	"fmt"
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

// CommandNotStartedError is a command a sandbox refused to start, which is no exit code of that
// command: it never ran. Code is what a shell answers for the same refusal.
type CommandNotStartedError struct {
	Sandbox string
	// Reason is the substrate's own words for why the command did not start.
	Reason string
	Code   int
}

func (e *CommandNotStartedError) Error() string {
	return fmt.Sprintf("sandbox %s could not run the command: %s", e.Sandbox, e.Reason)
}

// The optional verbs, spelled once here so a refusal and the conformance suite cannot drift apart.
const (
	VerbPause  = "pause"
	VerbResume = "resume"
	VerbFork   = "fork"
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
