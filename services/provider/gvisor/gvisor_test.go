package gvisor_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// newProvider wires a provider over a runsc that is never called: these tests refuse before they run one.
func newProvider(t *testing.T) *gvisor.Provider {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "runsc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the fake runsc: %v", err)
	}

	runner, err := runsc.New(filepath.Join(dir, "root"), runsc.WithBinary(binary))
	if err != nil {
		t.Fatalf("open the runsc runner: %v", err)
	}

	bundles, err := bundle.New("/usr/local/bin/shard-init")
	if err != nil {
		t.Fatalf("open the bundle service: %v", err)
	}

	p, err := gvisor.New(runner, bundles, func(id string) (string, error) { return filepath.Join(dir, id), nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := gvisor.New(nil, nil, nil); err == nil {
		t.Fatal("New accepted a provider with nothing to drive")
	}
}

func TestTheProviderNamesItsSubstrate(t *testing.T) {
	if got := newProvider(t).Name(); got != "gvisor" {
		t.Errorf("got name %q, want gvisor", got)
	}
}

// Chunk 3 flips these. Until then the flags and the verbs must agree, which is what conformance asserts.
func TestTheOptionalVerbsAreNotClaimedYet(t *testing.T) {
	if got := newProvider(t).Capabilities(); got != (models.Capabilities{}) {
		t.Errorf("got capabilities %+v, want none of them", got)
	}
}

func TestTheOptionalVerbsRefuseRatherThanDowngrade(t *testing.T) {
	p := newProvider(t)

	verbs := map[string]error{
		"pause":  p.Pause(t.Context(), "amber-otter-1a2b", t.TempDir()),
		"resume": p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir()),
	}

	_, verbs["fork"] = p.Fork(t.Context(), t.TempDir(), models.SandboxSpec{})

	for verb, err := range verbs {
		if !errors.Is(err, models.ErrUnsupported) {
			t.Errorf("%s returned %v, want ErrUnsupported", verb, err)
		}
	}
}
