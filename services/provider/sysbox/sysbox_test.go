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
	"github.com/presmihaylov/shard/pkg/runc"
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

	runner, err := runc.New(filepath.Join(dir, "root"), runc.WithBinary(binary))
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

// A snapshot copies the layer, so a source that still writes it is refused before anything is copied.
func TestSnapshotRefusesASourceThatIsLive(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	dir := t.TempDir()

	err := p.Snapshot(t.Context(), "amber-otter-1a2b", dir)
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("Snapshot of a running source returned %v, want a refusal that names the stop", err)
	}
	if entries := readDir(t, dir); len(entries) != 0 {
		t.Errorf("Snapshot of a running source wrote into the snapshot: %v", entries)
	}
}

// A source sysbox-runc never held has no layers to copy, and the refusal comes before any write.
func TestSnapshotRefusesASourceThatWasNeverBuilt(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
	dir := t.TempDir()

	if err := p.Snapshot(t.Context(), "amber-otter-1a2b", dir); err == nil {
		t.Error("Snapshot of a source with no bundle returned no error")
	}
	if entries := readDir(t, dir); len(entries) != 0 {
		t.Errorf("Snapshot of an empty source wrote into the snapshot: %v", entries)
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

// A nested container that hits its own bound counts only in the hierarchical memory.events, so a later clean exit is no OOM (SHARD-364).
func TestOnlyTheSandboxsOwnBoundIsAnOOM(t *testing.T) {
	const id = "amber-otter-1a2b"

	for name, tc := range map[string]struct {
		local string
		want  bool
	}{
		"a nested container's OOM": {local: "oom 0\noom_kill 0\n", want: false},
		"the sandbox's own OOM":    {local: "oom 1\noom_kill 1\n", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, bundle.CgroupsPath(id))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for file, body := range map[string]string{"memory.events": "oom 1\noom_kill 1\n", "memory.events.local": tc.local} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
			p.SetCgroupRoot(root)

			status, err := p.Status(t.Context(), id)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.OOMKilled != tc.want {
				t.Errorf("OOMKilled = %v, want %v", status.OOMKilled, tc.want)
			}
		})
	}
}
