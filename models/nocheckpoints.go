package models

import "context"

// NoCheckpoints refuses every checkpoint verb in the name of the provider that embeds it and sets Provider.
type NoCheckpoints struct {
	Provider string
}

func (n NoCheckpoints) Pause(context.Context, string, string) error {
	return Unsupported(n.Provider, VerbPause)
}

func (n NoCheckpoints) Resume(context.Context, string, string) error {
	return Unsupported(n.Provider, VerbResume)
}

func (n NoCheckpoints) Fork(context.Context, string, SandboxSpec) error {
	return Unsupported(n.Provider, VerbFork)
}

// AdoptStaging settles nothing: a substrate with no pause stages no checkpoint, so there is never a leftover to drop.
func (NoCheckpoints) AdoptStaging(string) error { return nil }
