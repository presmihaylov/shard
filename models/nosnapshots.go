package models

import "context"

// NoSnapshots is the snapshot half of a Provider for a substrate that has none. A provider embeds
// it and sets Provider to its own Name, so every refusal names the right substrate.
type NoSnapshots struct {
	Provider string
}

// Capabilities reports no optional verb at all.
func (NoSnapshots) Capabilities() Capabilities { return Capabilities{} }

func (n NoSnapshots) Pause(context.Context, string, string) error {
	return Unsupported(n.Provider, VerbPause)
}

func (n NoSnapshots) Resume(context.Context, string, string) error {
	return Unsupported(n.Provider, VerbResume)
}

func (n NoSnapshots) Fork(context.Context, string, SandboxSpec) error {
	return Unsupported(n.Provider, VerbFork)
}
