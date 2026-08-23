package models

import (
	"errors"
	"fmt"
)

// ErrUnsupported is the sentinel behind every refused verb. Match it with errors.Is.
var ErrUnsupported = errors.New("verb not supported")

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
