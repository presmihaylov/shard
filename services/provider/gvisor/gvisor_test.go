package gvisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestOnlyForkIsNotClaimedYet(t *testing.T) {
	if got, want := newProvider(t).Capabilities(), (models.Capabilities{Pause: true, Resume: true}); got != want {
		t.Errorf("got capabilities %+v, want %+v", got, want)
	}
}

func TestTheOptionalVerbsRefuseRatherThanDowngrade(t *testing.T) {
	p := newProvider(t)

	verbs := map[string]error{
		models.VerbFork: p.Fork(t.Context(), t.TempDir(), models.SandboxSpec{}),
	}

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

// A snapshot that failed must leave the sandbox running and the snapshot it had, even after a Ctrl-C.
func TestAFailedCheckpointThawsTheSandboxAndKeepsTheOldSnapshot(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	p := newProviderOver(t, `echo "$*" >> `+calls+`
case "$*" in *checkpoint*) sleep 5;; esac
echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The checkpoint outlives the context, which is what a Ctrl-C in the middle of one looks like.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	if err := p.Pause(ctx, "amber-otter-1a2b", dir); err == nil {
		t.Fatal("Pause returned no error for a checkpoint that was cut short")
	}

	if got := unitFile(t, calls); !strings.Contains(got, "resume amber-otter-1a2b") {
		t.Errorf("the sandbox was not thawed after the failed checkpoint: %s", got)
	}
	if got := unitFile(t, filepath.Join(dir, "checkpoint.img")); got != "old" {
		t.Errorf("the old snapshot is %q after a failed pause, want it kept", got)
	}
	if _, err := os.Stat(dir + ".tmp"); err == nil {
		t.Error("the failed checkpoint left its temporary directory behind")
	}
}

// Pause is the one verb that deletes a sandbox from runsc, so it must never take one it did not see running.
func TestPauseTakesOnlyARunningSandbox(t *testing.T) {
	cases := map[string]string{
		"stopped": `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`,
		"created": `echo '{"id":"amber-otter-1a2b","status":"created","pid":42}'`,
		"gone":    "echo 'FetchSpec failed: loading container: file does not exist' >&2; exit 1",
	}

	for state, script := range cases {
		p := newProviderOver(t, script)
		dir := filepath.Join(t.TempDir(), "snap")

		err := p.Pause(t.Context(), "amber-otter-1a2b", dir)
		if err == nil || !strings.Contains(err.Error(), "amber-otter-1a2b") {
			t.Errorf("Pause of a %s sandbox returned %v, want a refusal that names it", state, err)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("Pause of a %s sandbox made the snapshot directory", state)
		}
	}
}

func TestResumeTakesOnlyASnapshotOfAPausedSandbox(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	err := p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Errorf("Resume from an empty directory returned %v, want a refusal that says there is no snapshot", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// A running sandbox is one the pause never deleted, and a restore over it would fail late inside runsc.
	err = p.Resume(t.Context(), "amber-otter-1a2b", dir)
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("Resume of a running sandbox returned %v, want a refusal that names the state", err)
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

func unitFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}
