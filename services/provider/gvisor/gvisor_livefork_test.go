package gvisor_test

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

const (
	liveSource = "amber-otter-1a2b"
	liveFork   = "brave-fox-3c4d"
)

// A live fork freezes the source, saves it with the sentry left running, copies its layer while it is frozen, and thaws it before any restore (SHARD-457).
func TestALiveForkCapturesTheSourceAndThawsItBeforeTheRestore(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")

	err := p.Fork(t.Context(), liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)})
	got := runscCalls(t, calls)
	pause, checkpoint, resume, restore := index(got, "pause "+liveSource), index(got, "checkpoint --leave-running"), index(got, "resume "+liveSource), index(got, "restore")
	if pause < 0 || checkpoint < pause || resume < checkpoint {
		t.Fatalf("the fork ran %q (Fork = %v), want pause, then checkpoint --leave-running, then resume of %s", got, err, liveSource)
	}
	if restore >= 0 && restore < resume {
		t.Fatalf("the fork restored before the source was thawed: %q", got)
	}
	if !strings.Contains(got[checkpoint], filepath.Join(dir, liveFork, "capture")) {
		t.Errorf("the checkpoint wrote to %q, want the fork's own capture directory", got[checkpoint])
	}
	if _, err := os.Stat(filepath.Join(dir, liveFork, "capture", "config.json")); err != nil {
		t.Errorf("the capture holds no copy of the source's bundle: %v", err)
	}
	assertThawed(t, p, dir)
}

// A checkpoint that fails still thaws the source, copies no layer under the frozen guest, and the fork restores from nothing (SHARD-457).
func TestAFailedCaptureThawsTheSourceAndRestoresNoFork(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", `echo "save failed: no space left on device" >&2; exit 1`)
	fakeCopy(t, `echo "$*" >> `+filepath.Join(dir, "copies")+`; exec /bin/cp "$@"`)

	err := p.Fork(t.Context(), liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)})
	if err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("Fork with a failed checkpoint = %v, want its reason", err)
	}
	got := runscCalls(t, calls)
	if index(got, "resume "+liveSource) < index(got, "checkpoint") {
		t.Fatalf("the failed capture ran %q, want a resume of the source after the checkpoint", got)
	}
	if index(got, "restore") >= 0 {
		t.Errorf("the failed capture ran %q, want no restore", got)
	}
	if _, err := os.Stat(filepath.Join(dir, liveFork, "capture")); err == nil {
		t.Error("the failed capture left its directory behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "copies")); err == nil {
		t.Error("the failed checkpoint still copied the source's layer")
	}
	assertThawed(t, p, dir)
}

// A layer copy that hangs is cut by the fork's context, so it cannot hold the source frozen, and the source is thawed (SHARD-457).
func TestAHungLayerCopyIsCutAndThawsTheSource(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")
	copying := filepath.Join(dir, "copying")
	fakeCopy(t, "touch "+copying+"; exec sleep 30")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The cut lands once the copy runs, so a slow host cannot spend it on the steps before.
	cut := make(chan time.Time, 1)
	go func() {
		for ; ctx.Err() == nil; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(copying); err == nil {
				cut <- time.Now()
				cancel()

				return
			}
		}
	}()

	if err := p.Fork(ctx, liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)}); err == nil {
		t.Fatal("Fork over a hung layer copy = nil, want the cut")
	}
	select {
	case at := <-cut:
		if took := time.Since(at); took > 10*time.Second {
			t.Errorf("the fork returned %s after its cut, want the hung copy ended with it", took)
		}
	default:
		t.Fatal("the fork failed before its layer copy began")
	}
	if index(runscCalls(t, calls), "restore") >= 0 {
		t.Errorf("the cut fork ran %q, want no restore", runscCalls(t, calls))
	}
	assertThawed(t, p, dir)
}

// A cancel that lands while the freeze is in flight still thaws the source: the freeze may have landed though its answer did not (SHARD-457).
func TestACancelledForkStillThawsTheSource(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "sleep 2", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The cancel lands once the freeze has, while its answer is still out.
	go func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "frozen-"+liveSource)); err == nil {
				cancel()

				return
			}
		}
	}()

	if err := p.Fork(ctx, liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)}); err == nil {
		t.Fatal("Fork cut inside its freeze = nil, want the cancel")
	}
	got := runscCalls(t, calls)
	if index(got, "resume "+liveSource) < 0 {
		t.Fatalf("the cut fork ran %q, want the source resumed", got)
	}
	if index(got, "restore") >= 0 {
		t.Errorf("the cut fork ran %q, want no restore", got)
	}
	assertThawed(t, p, dir)
}

// A daemon cut inside a live fork leaves the source frozen with its mark, and the next read of it thaws it (SHARD-457).
func TestTheReadAfterADaemonCutInALiveForkThawsTheSource(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")
	if err := os.WriteFile(filepath.Join(dir, "frozen-"+liveSource), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, liveSource, "fork-frozen"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := p.Status(t.Context(), liveSource)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status of a source a cut fork left frozen = %+v, %v, want it thawed and running", status, err)
	}
	if index(runscCalls(t, calls), "resume "+liveSource) < 0 {
		t.Errorf("the read ran %q, want a resume of the source", runscCalls(t, calls))
	}
	assertThawed(t, p, dir)
}

// A cut after the resume leaves the mark on a running source, and the next read drops it, so a later real pause is never taken for a cut fork (SHARD-457).
func TestAReadDropsTheMarkACutForkLeftOnARunningSource(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")
	if err := os.WriteFile(filepath.Join(dir, liveSource, "fork-frozen"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := p.Status(t.Context(), liveSource)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status of a running source with a stale mark = %+v, %v, want running", status, err)
	}
	if _, err := os.Stat(filepath.Join(dir, liveSource, "fork-frozen")); err == nil {
		t.Fatal("the read left the stale mark on the running source")
	}

	// A real pause freezes the source while it checkpoints, and a read in that window must leave it frozen.
	sourceCgroup(t, p)
	if err := os.WriteFile(filepath.Join(dir, "frozen-"+liveSource), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := p.Status(t.Context(), liveSource); err != nil || status.State != models.StatePaused {
		t.Errorf("Status during a real pause = %+v, %v, want paused", status, err)
	}
	if got := runscCalls(t, calls); index(got, "resume") >= 0 {
		t.Errorf("the reads ran %q, want no resume", got)
	}
}

// A read of the source while this process captures it sees it frozen and leaves it so, or a thaw would run the guest under the checkpoint (SHARD-457).
func TestAReadDuringTheCaptureLeavesTheSourceFrozen(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "touch "+dir+"/checkpointing; sleep 1")
	sourceCgroup(t, p)
	read := make(chan models.Status, 1)
	go func() {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "checkpointing")); err == nil {
				status, err := p.Status(t.Context(), liveSource)
				if err != nil {
					t.Error(err)
				}
				read <- status

				return
			}
		}
		read <- models.Status{}
	}()

	err := p.Fork(t.Context(), liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)})
	if status := <-read; status.State != models.StatePaused {
		t.Fatalf("a read during the capture = %+v, want the source still frozen", status)
	}
	got := runscCalls(t, calls)
	if resume, checkpoint := index(got, "resume "+liveSource), index(got, "checkpoint"); resume < checkpoint {
		t.Fatalf("the fork ran %q (Fork = %v), want the one resume after the checkpoint", got, err)
	}
	assertThawed(t, p, dir)
}

// A fork over an id that runs would unmount the rootfs that one runs on, so it is refused before the source is frozen (SHARD-457).
func TestAForkRefusesAnIdThatRuns(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")

	err := p.Fork(t.Context(), liveSource, models.SandboxSpec{ID: "calm-owl-5e6f", StateDir: filepath.Join(dir, "calm-owl-5e6f")})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Fork over a running id = %v, want a refusal that says it exists", err)
	}
	if got := runscCalls(t, calls); index(got, "pause") >= 0 {
		t.Errorf("the refused fork ran %q, want no pause", got)
	}
}

// Only a running source is forked, so a frozen one is refused before anything is frozen again (SHARD-457).
func TestAForkRefusesASourceThatDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	p, calls := liveForkProvider(t, dir, "", "")
	p.SetCgroupRoot(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "frozen-"+liveSource), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := p.Fork(t.Context(), liveSource, models.SandboxSpec{ID: liveFork, StateDir: filepath.Join(dir, liveFork)})
	if err == nil || !strings.Contains(err.Error(), "fork takes a running sandbox") {
		t.Fatalf("Fork of a source that does not run = %v, want a refusal", err)
	}
	if got := runscCalls(t, calls); index(got, "pause") >= 0 || index(got, "checkpoint") >= 0 {
		t.Errorf("the refused fork ran %q, want no pause and no checkpoint", got)
	}
}

// liveForkProvider is a provider over a fake runsc that keeps a frozen file per sandbox; it holds the fork id stopped, so no mount check stands before the capture; pause runs beforePause first, and a checkpoint runs onCheckpoint.
func liveForkProvider(t *testing.T, dir, beforePause, onCheckpoint string) (*gvisor.Provider, string) {
	t.Helper()
	calls := filepath.Join(dir, "calls")
	for _, layer := range []string{"bundle", "disk/upper", "disk/tmp", "disk/shard"} {
		if err := os.MkdirAll(filepath.Join(dir, liveSource, layer), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, liveSource, "bundle", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, liveFork), 0o700); err != nil {
		t.Fatal(err)
	}
	script := `echo "$*" >> ` + calls + `
for last; do :; done
case "$*" in
*" pause "*) touch ` + dir + `/frozen-$last; ` + beforePause + ` ;;
*" resume "*) rm -f ` + dir + `/frozen-$last ;;
*" checkpoint "*) ` + cmp.Or(onCheckpoint, ":") + ` ;;
*" state "*)
  if [ "$last" = ` + liveFork + ` ]; then echo '{"id":"'$last'","status":"stopped","pid":0}'; exit 0; fi
  if [ -e ` + dir + `/frozen-$last ]; then echo '{"id":"'$last'","status":"paused","pid":42}'; exit 0; fi
  echo '{"id":"'$last'","status":"running","pid":42}' ;;
esac`

	return newProviderIn(t, dir, script), calls
}

// sourceCgroup keeps the source's sentry in its cgroup, which is what lets a read tell a frozen one from a pid Linux reused.
func sourceCgroup(t *testing.T, p *gvisor.Provider) {
	t.Helper()
	cgroups := t.TempDir()
	p.SetCgroupRoot(cgroups)
	cg := filepath.Join(cgroups, bundle.CgroupsPath(liveSource))
	if err := os.MkdirAll(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeCopy puts a cp that runs body on PATH, ahead of the real one.
func fakeCopy(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { // #nosec G306
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// assertThawed proves the source runs again and carries no fork mark.
func assertThawed(t *testing.T, p *gvisor.Provider, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "frozen-"+liveSource)); err == nil {
		t.Error("the source is still frozen")
	}
	if _, err := os.Stat(filepath.Join(dir, liveSource, "fork-frozen")); err == nil {
		t.Error("the source still carries its fork mark")
	}
	status, err := p.Status(t.Context(), liveSource)
	if err != nil || status.State != models.StateRunning {
		t.Errorf("Status of the source = %+v, %v, want running", status, err)
	}
}

func runscCalls(t *testing.T, path string) []string {
	t.Helper()
	read, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(read)), "\n")
}

// index is the first call that holds part, or -1.
func index(calls []string, part string) int {
	for i, call := range calls {
		if strings.Contains(call, part) {
			return i
		}
	}

	return -1
}
