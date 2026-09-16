package sysbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/sysboxrunc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/sysbox"
)

// newProvider wires a provider over a sysbox-runc that is never called: these tests refuse before they run one.
func newProvider(t *testing.T) *sysbox.Provider {
	t.Helper()

	return newProviderOver(t, "exit 1")
}

// newProviderOver stands a fake sysbox-runc up from a shell script, so a hang and a state are both scriptable.
func newProviderOver(t *testing.T, script string) *sysbox.Provider {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "sysbox-runc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write the fake sysbox-runc: %v", err)
	}

	runner, err := sysboxrunc.New(filepath.Join(dir, "root"), sysboxrunc.WithBinary(binary))
	if err != nil {
		t.Fatalf("open the sysbox-runc runner: %v", err)
	}

	bundles, err := bundle.New("/usr/local/bin/shard-init")
	if err != nil {
		t.Fatalf("open the bundle service: %v", err)
	}

	p, err := sysbox.New(runner, bundles, func(id string) (string, error) { return filepath.Join(dir, id), nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := sysbox.New(nil, nil, nil); err == nil {
		t.Fatal("New accepted a provider with nothing to drive")
	}
}

func TestTheProviderNamesItsSubstrate(t *testing.T) {
	if got := newProvider(t).Name(); got != "sysbox" {
		t.Errorf("got name %q, want sysbox", got)
	}
}

func TestNoOptionalVerbIsClaimed(t *testing.T) {
	if got := newProvider(t).Capabilities(); got != (models.Capabilities{}) {
		t.Errorf("got capabilities %+v, want none", got)
	}
}

// The snapshot verbs refuse by name before sysbox-runc runs, so the cli can say which provider lacks what.
func TestEveryOptionalVerbRefusesByName(t *testing.T) {
	p := newProvider(t)
	spec := models.SandboxSpec{ID: "amber-otter-2c3d", StateDir: t.TempDir()}

	verbs := map[string]error{
		models.VerbPause:  p.Pause(t.Context(), "amber-otter-1a2b", t.TempDir()),
		models.VerbResume: p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir()),
		models.VerbFork:   p.Fork(t.Context(), "amber-otter-1a2b", spec),
	}

	for verb, err := range verbs {
		var unsupported *models.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Errorf("%s returned %v, want an UnsupportedError", verb, err)
			continue
		}
		if unsupported.Provider != "sysbox" || unsupported.Verb != verb {
			t.Errorf("%s refused as %+v, want provider sysbox and verb %s", verb, unsupported, verb)
		}
	}
}

func TestAWedgedRuncFailsWithTheContextRatherThanHanging(t *testing.T) {
	p := newProviderOver(t, "sleep 60")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := p.Status(ctx, "amber-otter-1a2b"); err == nil {
		t.Fatal("Status answered for a sysbox-runc that never replied")
	}

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Status took %s, so the context did not cut the sysbox-runc call short", elapsed)
	}
}

// Wait polls for a file that may never arrive, so its context is the only thing that ends it.
func TestWaitGivesUpWithItsContext(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, err := p.Wait(ctx, "amber-otter-1a2b")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait returned %v, want the context deadline", err)
	}
}

// A sandbox sysbox-runc never heard of reads as absent, not as an error, so a stale record can be read past.
func TestStatusOfAnUnknownSandboxIsAbsent(t *testing.T) {
	p := newProviderOver(t, `echo 'container "amber-otter-1a2b" does not exist' >&2; exit 1`)

	status, err := p.Status(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Exists || status.Alive() {
		t.Errorf("got %+v, want a sandbox that does not exist", status)
	}
}

// Exec refuses anything but a running sandbox, because runc reports its own startup failures as exit 1.
func TestExecTakesOnlyARunningSandbox(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)

	_, err := p.Exec(t.Context(), "amber-otter-1a2b", models.ExecSpec{Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Exec in a stopped sandbox returned %v, want a refusal that names the state", err)
	}

	_, err = p.Exec(t.Context(), "amber-otter-1a2b", models.ExecSpec{})
	if err == nil || !strings.Contains(err.Error(), "no command") {
		t.Errorf("Exec with no argv returned %v, want a refusal", err)
	}
}

// A clone copies the layer, so a source that still writes it is refused before anything is laid out.
func TestCloneRefusesASourceThatIsLive(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	spec := models.SandboxSpec{ID: "amber-otter-2c3d", StateDir: t.TempDir()}

	err := p.Clone(t.Context(), "amber-otter-1a2b", spec)
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("Clone of a running source returned %v, want a refusal that names the stop", err)
	}
	if entries := readDir(t, spec.StateDir); len(entries) != 0 {
		t.Errorf("Clone of a running source wrote into its state directory: %v", entries)
	}
}

// A source sysbox-runc never held has no config.json to run again, and the refusal comes before any write.
func TestCloneRefusesASourceThatWasNeverBuilt(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
	spec := models.SandboxSpec{ID: "amber-otter-2c3d", StateDir: t.TempDir()}

	if err := p.Clone(t.Context(), "amber-otter-1a2b", spec); err == nil {
		t.Error("Clone of a source with no bundle returned no error")
	}
	if entries := readDir(t, spec.StateDir); len(entries) != 0 {
		t.Errorf("Clone of an empty source wrote into its state directory: %v", entries)
	}
}

func readDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	return entries
}
