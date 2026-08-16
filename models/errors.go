package models

import (
	"errors"
	"fmt"
)

// ErrUnsupported is the sentinel behind every refusal to run a verb. Match it
// with errors.Is; build one with Unsupported so the message names names.
var ErrUnsupported = errors.New("verb not supported")

// UnsupportedError says which provider refused which verb.
//
// Refuse, never downgrade: a provider that cannot pause fails here rather than
// falling back to a weaker mechanism, so the message is the whole explanation a
// user gets.
type UnsupportedError struct {
	Provider string
	Verb     string
}

// Unsupported builds the error a provider returns for a verb it cannot run.
func Unsupported(provider, verb string) error {
	return &UnsupportedError{Provider: provider, Verb: verb}
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("provider %s does not support %s on this host", e.Provider, e.Verb)
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }
