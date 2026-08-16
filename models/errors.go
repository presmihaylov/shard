package models

import (
	"errors"
	"fmt"
)

// ErrUnsupported is the sentinel behind every refused verb. Match it with errors.Is.
var ErrUnsupported = errors.New("verb not supported")

// UnsupportedError names the provider and the verb, because shard refuses rather than downgrades.
type UnsupportedError struct {
	Provider string
	Verb     string
}

func Unsupported(provider, verb string) error {
	return &UnsupportedError{Provider: provider, Verb: verb}
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("provider %s does not support %s on this host", e.Provider, e.Verb)
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }
