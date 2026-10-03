package gvisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
func newProviderOver(t *testing.T, script string, opts ...runsc.Option) *gvisor.Provider {
	t.Helper()

	return newProviderIn(t, t.TempDir(), script, opts...)
}

// newProviderIn is newProviderOver with the state directory of sandbox id at dir/id, for a test that lays a bundle there.
func newProviderIn(t *testing.T, dir, script string, opts ...runsc.Option) *gvisor.Provider {
	t.Helper()

	binary := filepath.Join(dir, "runsc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write the fake runsc: %v", err)
	}

	runner, err := runsc.New(filepath.Join(dir, "root"), append(opts, runsc.WithBinary(binary))...)
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

	// The fake runsc answers pid 42, so a stand-in /proc keeps that pid alive.
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "42"), 0o755); err != nil {
		t.Fatalf("make the stand-in /proc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "stat"), []byte("42 (runsc-sandbox) S 1 42 42 0 -1 4194560"), 0o600); err != nil {
		t.Fatalf("write the stat of pid 42: %v", err)
	}
	p.SetProcRoot(proc)

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

func TestEveryOptionalVerbIsClaimed(t *testing.T) {
	if got, want := newProvider(t).Capabilities(), (models.Capabilities{Pause: true, Resume: true, Fork: true}); got != want {
		t.Errorf("got capabilities %+v, want %+v", got, want)
	}
}

func TestForkTakesOnlyASnapshotAndAFreeId(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	spec := models.SandboxSpec{ID: "amber-otter-1a2b", StateDir: t.TempDir()}

	err := p.Fork(t.Context(), t.TempDir(), spec)
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Errorf("Fork from an empty directory returned %v, want a refusal that says there is no snapshot", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// A live id must not be forked over: the rollback would unmount the rootfs the first one runs on.
	err = p.Fork(t.Context(), dir, spec)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Fork over a running id returned %v, want a refusal that says it exists", err)
	}
	if entries := readDir(t, spec.StateDir); len(entries) != 0 {
		t.Errorf("Fork over a running id wrote into its state directory: %v", entries)
	}
}

// The sentry exits after any checkpoint, so a failed one loses the sandbox: nothing to thaw, the old snapshot kept.
func TestAFailedCheckpointLosesTheSandboxAndKeepsTheOldSnapshot(t *testing.T) {
	work := t.TempDir()
	calls := filepath.Join(work, "calls")
	p := newProviderOver(t, `echo "$*" >> `+calls+`
case "$*" in *checkpoint*) echo "save failed: no space left on device" >&2; exit 1;; esac
echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	p.SetCgroupRoot(t.TempDir())

	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.Pause(t.Context(), "amber-otter-1a2b", dir)
	var lost *models.LostError
	if !errors.As(err, &lost) || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Pause returned %v, want the lost sandbox with the checkpoint's reason", err)
	}

	got := unitFile(t, calls)
	if strings.Contains(got, "resume") || !strings.Contains(got, "delete --force amber-otter-1a2b") {
		t.Errorf("the failed checkpoint ran %q, want a delete and no thaw", got)
	}
	if got := unitFile(t, filepath.Join(dir, "checkpoint.img")); got != "old" {
		t.Errorf("the old snapshot is %q after a failed pause, want it kept", got)
	}
	if _, err := os.Stat(dir + ".tmp"); err == nil {
		t.Error("the failed checkpoint left its temporary directory behind")
	}
}

// The service bounds a pause the client let go of, and the delete after a good checkpoint must keep that bound.
func TestAWedgedDeleteAfterACheckpointEndsAtTheDeadline(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "amber-otter-1a2b")
	for _, layer := range []string{"bundle", "disk/upper", "disk/tmp", "disk/shard"} {
		if err := os.MkdirAll(filepath.Join(stateDir, layer), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "bundle", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newProviderIn(t, dir, `case "$*" in *delete*) exec sleep 60;; esac
echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	p.SetCgroupRoot(t.TempDir())

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Pause(ctx, "amber-otter-1a2b", filepath.Join(t.TempDir(), "snap"))
	if err == nil {
		t.Fatal("Pause returned nil, want the delete cut at the deadline")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Pause took %s over a wedged delete, want it ended near the 500ms deadline", took)
	}
}

// runsc never probes a paused sandbox, so a sentry gone after a checkpoint must read as stopped, not paused.
func TestStatusReadsAPausedSandboxWhoseSentryIsGoneAsStopped(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"paused","pid":42}'`)
	cgroupRoot, proc := t.TempDir(), t.TempDir()
	p.SetCgroupRoot(cgroupRoot)
	p.SetProcRoot(proc)

	status, err := p.Status(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Alive() || !status.Exists || status.PID != 0 {
		t.Errorf("Status of a paused sandbox with no sentry is %+v, want stopped and still held by runsc", status)
	}

	// A frozen sentry is still there, in its own cgroup, and only a resume brings it back.
	if err := os.MkdirAll(filepath.Join(proc, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "stat"), []byte("42 (runsc-sandbox) S 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cgroupDir := filepath.Join(cgroupRoot, bundle.CgroupsPath("amber-otter-1a2b"))
	if err := os.MkdirAll(cgroupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.procs"), []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err = p.Status(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != models.StatePaused || status.PID != 42 {
		t.Errorf("Status of a frozen sandbox is %+v, want paused with its pid", status)
	}
}

// runsc refuses to signal or thaw a paused sandbox whose sentry is gone, so stop must ask it for neither.
func TestStopEndsAPausedSandboxWhoseSentryIsGone(t *testing.T) {
	work := t.TempDir()
	calls := filepath.Join(work, "calls")
	p := newProviderOver(t, `echo "$*" >> `+calls+`
echo '{"id":"amber-otter-1a2b","status":"paused","pid":42}'`)
	p.SetCgroupRoot(t.TempDir())
	p.SetProcRoot(t.TempDir())

	err := p.Stop(t.Context(), "amber-otter-1a2b", time.Second)
	// Only Linux has the overlayfs the unmount after it needs.
	if err != nil && runtime.GOOS == "linux" {
		t.Errorf("Stop of a paused sandbox with no sentry: %v", err)
	}

	if got := unitFile(t, calls); strings.Contains(got, "resume") || strings.Contains(got, "kill") {
		t.Errorf("stop of a paused sandbox with no sentry ran %q, want neither a thaw nor a signal", got)
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

// A source runsc never held has no config.json to run again, and the refusal comes before any write.
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

// A cut pause leaves dir+".tmp" that resume never reads, so AdoptStaging drops it at daemon start (SHARD-404).
func TestAdoptStagingDropsACutPauseStage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snapshot")
	tmp := dir + ".tmp"
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatalf("stage a cut pause: %v", err)
	}

	if err := (&gvisor.Provider{}).AdoptStaging(dir); err != nil {
		t.Fatalf("AdoptStaging: %v", err)
	}

	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging %s survived adopt, want it dropped (err %v)", tmp, err)
	}
}
