package gvisor_test

import (
	"context"
	"errors"
	"fmt"
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

// The sentry exits after any checkpoint, so a failed one loses the sandbox: nothing to thaw, the old checkpoint kept.
func TestAFailedCheckpointLosesTheSandboxAndKeepsTheOldCheckpoint(t *testing.T) {
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
	if strings.Contains(got, "resume") || strings.Contains(got, "delete") {
		t.Errorf("the failed checkpoint ran %q, want no thaw and no runsc delete (SHARD-440)", got)
	}
	if got := unitFile(t, filepath.Join(dir, "checkpoint.img")); got != "old" {
		t.Errorf("the old checkpoint is %q after a failed pause, want it kept", got)
	}
	if _, err := os.Stat(dir + ".tmp"); err == nil {
		t.Error("the failed checkpoint left its temporary directory behind")
	}
}

// The service bounds a pause the client let go of, and the sweep after a good checkpoint must keep that bound.
func TestAWedgedSweepAfterACheckpointEndsAtTheDeadline(t *testing.T) {
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
	p := newProviderIn(t, dir, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	cgroups := t.TempDir()
	p.SetCgroupRoot(cgroups)
	cg := filepath.Join(cgroups, bundle.CgroupsPath("amber-otter-1a2b"))
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The sentry names the sandbox, so the sweep pins and kills it; the kill never lands, so the cgroup never empties and the wait hits the deadline (SHARD-440).
	if err := os.WriteFile(filepath.Join(p.ProcRoot(), "42", "cmdline"), []byte("runsc-sandbox\x00boot\x00amber-otter-1a2b\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The deadline starts at the sweep, so a loaded host that spends it on the fake runsc never sends the pause through lose and its fresh kill grace (SHARD-511).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var swept time.Time
	p.SetKillPinned(func(int, func() (bool, error)) error {
		if swept.IsZero() {
			swept = time.Now()
			time.AfterFunc(500*time.Millisecond, cancel)
		}
		return nil
	})

	err := p.Pause(ctx, "amber-otter-1a2b", filepath.Join(t.TempDir(), "snap"))
	if err == nil {
		t.Fatal("Pause returned nil, want the wedged sweep cut at the deadline")
	}
	if swept.IsZero() {
		t.Fatalf("Pause = %v before the sweep, want it to reach the wedged sweep", err)
	}
	if took := time.Since(swept); took > 5*time.Second {
		t.Errorf("Pause took %s over a wedged sweep, want it ended near the 500ms deadline", took)
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

// frozenSentry lays the proc and cgroup entries of a sentry a pause froze, and a checkpoint beside it, and answers that checkpoint.
func frozenSentry(t *testing.T, p *gvisor.Provider, cgroups string) string {
	t.Helper()

	proc := t.TempDir()
	p.SetProcRoot(proc)
	p.SetCgroupRoot(cgroups)
	if err := os.MkdirAll(filepath.Join(proc, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "stat"), []byte("42 (runsc-sandbox) S 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The frozen sentry names the sandbox, so the sweep kills it by cgroup rather than mistaking it for a reused pid (SHARD-440).
	if err := os.WriteFile(filepath.Join(proc, "42", "cmdline"), []byte("runsc-sandbox\x00boot\x00amber-otter-1a2b\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	cg := filepath.Join(cgroups, bundle.CgroupsPath("amber-otter-1a2b"))
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

// sweepKill stands in for the pinned SIGKILL: it records the pid and removes the sandbox cgroup, the way the kernel reclaims one once its last process dies.
func sweepKill(p *gvisor.Provider, cg string, killed *[]int) {
	p.SetKillPinned(func(pid int, still func() (bool, error)) error {
		ok, err := still()
		if err != nil || !ok {
			return err
		}
		*killed = append(*killed, pid)

		return os.RemoveAll(cg)
	})
}

// A cut pause leaves the sentry frozen past its checkpoint, so the release must sweep it and never thaw it (SHARD-366); the sweep kills by cgroup, never a runsc force delete (SHARD-440).
func TestReleaseEndsAFrozenSandboxWithoutAThawOrAForceDelete(t *testing.T) {
	work := t.TempDir()
	calls := filepath.Join(work, "calls")
	cgroups := t.TempDir()
	cg := filepath.Join(cgroups, bundle.CgroupsPath("amber-otter-1a2b"))
	p := newProviderOver(t, `echo "$*" >> `+calls+`
echo '{"id":"amber-otter-1a2b","status":"paused","pid":42}'`)
	dir := frozenSentry(t, p, cgroups)
	var killed []int
	sweepKill(p, cg, &killed)

	err := p.Release(t.Context(), "amber-otter-1a2b", dir)
	// Only Linux has the overlayfs the unmount after it needs.
	if err != nil && runtime.GOOS == "linux" {
		t.Errorf("Release of a frozen sandbox: %v", err)
	}

	if len(killed) != 1 || killed[0] != 42 {
		t.Errorf("release pinned and killed %v, want the frozen sentry 42", killed)
	}
	got := unitFile(t, calls)
	if strings.Contains(got, "resume") || strings.Contains(got, "kill") || strings.Contains(got, "delete") {
		t.Errorf("release of a frozen sandbox ran %q, want no thaw, no signal, and no runsc delete (SHARD-440)", got)
	}
}

// runsc no longer holds the sandbox, yet the cgroup still names the frozen sentry, so the release must sweep it and drop the view (SHARD-366).
func TestReleaseFreesASandboxRunscNoLongerHolds(t *testing.T) {
	work := t.TempDir()
	calls := filepath.Join(work, "calls")
	cgroups := t.TempDir()
	cg := filepath.Join(cgroups, bundle.CgroupsPath("amber-otter-1a2b"))
	p := newProviderOver(t, `echo "$*" >> `+calls+`
case "$*" in *state*) echo 'FetchSpec failed: loading container: file does not exist' >&2; exit 1;; esac`)
	dir := frozenSentry(t, p, cgroups)
	var killed []int
	sweepKill(p, cg, &killed)

	err := p.Release(t.Context(), "amber-otter-1a2b", dir)
	// Only Linux has the overlayfs the unmount after it needs.
	if err != nil && runtime.GOOS == "linux" {
		t.Errorf("Release of a sandbox runsc no longer holds: %v", err)
	}

	if len(killed) != 1 || killed[0] != 42 {
		t.Errorf("release pinned and killed %v, want the frozen sentry 42 the cgroup still named", killed)
	}
	if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cgroup %s is still there (%v), want it swept before the unmount", cg, err)
	}
	if got := unitFile(t, calls); strings.Contains(got, "resume") || strings.Contains(got, "kill") || strings.Contains(got, "delete") {
		t.Errorf("release of a sandbox runsc no longer holds ran %q, want no thaw, no signal, and no runsc delete", got)
	}
}

// Release ends a sandbox outright, so it must refuse one that runs.
func TestReleaseRefusesARunningSandbox(t *testing.T) {
	work := t.TempDir()
	calls := filepath.Join(work, "calls")
	p := newProviderOver(t, `echo "$*" >> `+calls+`
echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	dir := frozenSentry(t, p, t.TempDir())

	err := p.Release(t.Context(), "amber-otter-1a2b", dir)
	if err == nil || !strings.Contains(err.Error(), "amber-otter-1a2b") {
		t.Errorf("Release of a running sandbox returned %v, want a refusal that names it", err)
	}
	if got := unitFile(t, calls); strings.Contains(got, "delete") {
		t.Errorf("release of a running sandbox ran %q, want no delete", got)
	}
}

// The callers hand Release a context with no deadline, so its own bound must cover the probe before the delete.
func TestReleaseEndsAWedgedProbeAtItsOwnBound(t *testing.T) {
	p := newProviderOver(t, `case "$*" in *state*) exec sleep 60;; esac`)
	dir := frozenSentry(t, p, t.TempDir())

	start := time.Now()
	if err := p.Release(t.Context(), "amber-otter-1a2b", dir); err == nil {
		t.Fatal("Release returned nil over a runsc that never answered")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("Release took %s over a wedged probe, want it ended at its own bound", took)
	}
}

// Pause is the one verb that ends a sandbox in runsc, so it must never take one it did not see running.
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
			t.Errorf("Pause of a %s sandbox made the checkpoint directory", state)
		}
	}
}

func TestResumeTakesOnlyACheckpointOfAPausedSandbox(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)

	err := p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no checkpoint") {
		t.Errorf("Resume from an empty directory returned %v, want a refusal that says there is no checkpoint", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// A running sandbox is one the pause never ended, and a restore over it would fail late inside runsc.
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

// A source runsc never held has no layers to copy, and the refusal comes before any write.
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

// A cut pause leaves dir+".tmp" that resume never reads, so AdoptStaging drops it at daemon start (SHARD-404).
func TestAdoptStagingDropsACutPauseStage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
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

// stopRunsc is a runsc whose sentry 42 runs until a KILL ends it, and until a TERM does too when honoursTerm says so.
func stopRunsc(t *testing.T, honoursTerm bool) (*gvisor.Provider, string) {
	t.Helper()

	work := t.TempDir()
	onTerm := ":"
	if honoursTerm {
		onTerm = "touch " + work + "/ended"
	}
	p := newProviderOver(t, `case "$*" in
*" state "*) if [ -e `+work+`/ended ]; then echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'; else echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'; fi ;;
*" kill "*KILL) touch `+work+`/killed `+work+`/ended ;;
*" kill "*TERM) `+onTerm+` ;;
esac`)
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "42"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "42", "stat"), []byte("42 (runsc-sandbox) S 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.SetProcRoot(proc)
	p.SetCgroupRoot(t.TempDir())

	return p, work
}

// The grace bounds the stop and is never a wait: an entrypoint that exits on TERM ends it at once (SHARD-460).
func TestStopReturnsOnceTheEntrypointExitsOnTerm(t *testing.T) {
	p, work := stopRunsc(t, true)

	started := time.Now()
	err := p.Stop(t.Context(), "amber-otter-1a2b", models.StopGrace)
	// Only Linux has the overlayfs the unmount after the stop needs, so elsewhere the stop ends on that refusal.
	if err != nil && (runtime.GOOS == "linux" || !strings.Contains(err.Error(), "overlayfs")) {
		t.Fatalf("Stop: %v", err)
	}

	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("Stop took %s of the %s grace, so it waited past an entrypoint that exited on TERM", took, models.StopGrace)
	}
	if _, err := os.Stat(filepath.Join(work, "ended")); err != nil {
		t.Errorf("Stop never sent TERM: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "killed")); err == nil {
		t.Error("Stop sent KILL to an entrypoint that exited on TERM")
	}
}

func TestStopKillsAnEntrypointThatIgnoresTermOnceTheGraceRunsOut(t *testing.T) {
	p, work := stopRunsc(t, false)

	grace := 500 * time.Millisecond
	started := time.Now()
	err := p.Stop(t.Context(), "amber-otter-1a2b", grace)
	if err != nil && (runtime.GOOS == "linux" || !strings.Contains(err.Error(), "overlayfs")) {
		t.Fatalf("Stop: %v", err)
	}

	took := time.Since(started)
	if took < grace || took > grace+3*time.Second {
		t.Errorf("Stop took %s, want the %s grace and then the kill", took, grace)
	}
	if _, err := os.Stat(filepath.Join(work, "killed")); err != nil {
		t.Errorf("Stop never sent KILL to an entrypoint that ignored TERM: %v", err)
	}
}

// A wait runsc lost is the one sentinel a pause can claim, and it keeps runsc's words beside it (SHARD-486).
func TestALostWaitIsTheExecLostSentinel(t *testing.T) {
	err := gvisor.ExecFailure("amber-otter-1a2b", fmt.Errorf("runsc exec amber-otter-1a2b: %w", &runsc.ExecLostError{Reason: "waiting on pid 7: EOF"}))

	if !errors.Is(err, models.ErrExecLost) || !strings.Contains(err.Error(), "waiting on pid 7: EOF") {
		t.Errorf("ExecFailure returned %v, want models.ErrExecLost with runsc's words", err)
	}
	if _, ok := errors.AsType[*models.CommandNotStartedError](err); ok {
		t.Errorf("ExecFailure returned %v, want no command that never started", err)
	}
}
