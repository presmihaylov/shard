package gvisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// newProvider wires a provider over a runsc that is never called: these tests refuse before they run one.
func newProvider(t *testing.T) *gvisor.Provider {
	t.Helper()

	return newProviderOver(t, "exit 1")
}

// newProviderOver stands a fake runsc up from a shell script, so a hang and a state are both scriptable.
func newProviderOver(t *testing.T, script string) *gvisor.Provider {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "runsc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
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
		models.VerbPause:  p.Pause(t.Context(), "amber-otter-1a2b", t.TempDir()),
		models.VerbResume: p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir()),
	}

	verbs[models.VerbFork] = p.Fork(t.Context(), t.TempDir(), models.SandboxSpec{})

	// The conformance suite asserts the provider and the verb a refusal carries, so assert both here too.
	for verb, err := range verbs {
		var refusal *models.UnsupportedError
		if !errors.As(err, &refusal) {
			t.Errorf("%s returned %v, want an UnsupportedError", verb, err)
			continue
		}
		if refusal.Provider != "gvisor" {
			t.Errorf("the %s refusal names provider %q, want gvisor", verb, refusal.Provider)
		}
		if refusal.Verb != verb {
			t.Errorf("the refusal names verb %q, want %q", refusal.Verb, verb)
		}
	}
}

// A runsc that never answers must not hang a verb forever. Only the context bounds one, so a wedged
// sentry has to surface as an error rather than as a caller that never returns.
func TestAWedgedRunscFailsWithTheContextRatherThanHanging(t *testing.T) {
	p := newProviderOver(t, "sleep 60")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := p.Status(ctx, "amber-otter-1a2b"); err == nil {
		t.Fatal("Status answered for a runsc that never replied")
	}

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Status took %s, so the context did not cut the runsc call short", elapsed)
	}
}

// Wait polls for a file that may never arrive, so its context is the only thing that ends it.
func TestWaitGivesUpWithItsContext(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	// The sandbox reads as alive and never writes an exit status, which is the case that would spin.
	_, err := p.Wait(ctx, "amber-otter-1a2b")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait returned %v, want the context deadline", err)
	}
}
