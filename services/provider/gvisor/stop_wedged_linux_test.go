//go:build linux

package gvisor_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// A cut-paused sentry whose bare resume succeeds exits on its own and empties its cgroup, so the following TERM dials a gone sentry; Stop must take the empty cgroup as ended, not refuse it (SHARD-411).
func TestStopReturnsNilWhenResumeEmptiesTheCgroupAndKillIsUnreachable(t *testing.T) {
	const id = "amber-otter-1a2b"
	const pid = 4242

	cgroupRoot, procRoot := t.TempDir(), t.TempDir()

	cgroupDir := filepath.Join(cgroupRoot, bundle.CgroupsPath(id))
	if err := os.MkdirAll(cgroupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	procs := filepath.Join(cgroupDir, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(statDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live, non-zombie sentry: the state field after the comm is S, so Status reads the sandbox as paused.
	if err := os.WriteFile(filepath.Join(statDir, "stat"), []byte(strconv.Itoa(pid)+" (gvisor_sentry) S 1 "+strconv.Itoa(pid)+" 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The fake runsc resumes with rc 0 and removes the cgroup, the way a real empty cgroup rmdirs after the sandbox exits, then the TERM refuses.
	p := newProviderOver(t, `case "$*" in
*'overlay2=none state'*) echo '{"id":"`+id+`","status":"paused","pid":`+strconv.Itoa(pid)+`}' ;;
*'overlay2=none resume'*) rm -rf '`+cgroupDir+`' ;;
*'overlay2=none kill'*) echo 'connecting to control server: connection refused' >&2; exit 1 ;;
*) exit 0 ;;
esac`)
	p.SetCgroupRoot(cgroupRoot)
	p.SetProcRoot(procRoot)
	p.SetKill(func(int) error { return nil })

	if err := p.Stop(t.Context(), id, time.Second); err != nil {
		t.Fatalf("Stop of a wedged sentry whose resume emptied its cgroup returned %v, want nil", err)
	}
}

// A mid-exit sentry still sits in the cgroup after the resume but its cmdline already reads empty, so named matches nothing; the following TERM is unreachable, which is the narrow race the empty-cgroup fix missed (SHARD-411).
func midExitSentry(t *testing.T, id string, pid int) (*gvisor.Provider, string) {
	t.Helper()

	cgroupRoot, procRoot := t.TempDir(), t.TempDir()

	cgroupDir := filepath.Join(cgroupRoot, bundle.CgroupsPath(id))
	if err := os.MkdirAll(cgroupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	procs := filepath.Join(cgroupDir, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	statDir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(statDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statDir, "stat"), []byte(strconv.Itoa(pid)+" (gvisor_sentry) S 1 "+strconv.Itoa(pid)+" 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An empty cmdline is the mid-exit signature: the kernel clears it before the process leaves its cgroup.
	if err := os.WriteFile(filepath.Join(statDir, "cmdline"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The fake runsc resumes with rc 0 but leaves the sentry pid in the cgroup, then the TERM refuses: endWedged sweeps a pid that names nothing.
	p := newProviderOver(t, `case "$*" in
*'overlay2=none state'*) echo '{"id":"`+id+`","status":"paused","pid":`+strconv.Itoa(pid)+`}' ;;
*'overlay2=none kill'*) echo 'connecting to control server: connection refused' >&2; exit 1 ;;
*) exit 0 ;;
esac`)
	p.SetCgroupRoot(cgroupRoot)
	p.SetProcRoot(procRoot)
	p.SetKill(func(int) error { return nil })

	return p, procs
}

// A mid-exit sentry leaves its cgroup a short delay after the resume, so Stop waits it out and ends clean rather than refusing on the empty name (SHARD-411).
func TestStopWaitsOutAMidExitSentryAndReturnsNil(t *testing.T) {
	const id = "amber-otter-1a2b"
	p, procs := midExitSentry(t, id, 4242)

	// The sentry leaves the cgroup after the resume, the way a real exit trails it and the empty cgroup then rmdirs; the wait must catch that.
	left := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		// Rename then remove, so the poller reads the cgroup as either whole or gone, never mid-delete.
		dir := filepath.Dir(procs)
		if err := os.Rename(dir, dir+".gone"); err != nil {
			left <- err
			return
		}
		left <- os.RemoveAll(dir + ".gone")
	}()

	if err := p.Stop(t.Context(), id, time.Second); err != nil {
		t.Fatalf("Stop of a mid-exit sentry returned %v, want nil", err)
	}
	if err := <-left; err != nil {
		t.Fatalf("empty the cgroup: %v", err)
	}
}

// A pid that names nothing and never leaves is foreign, so Stop waits the grace, then refuses by name rather than kill it (SHARD-411).
func TestStopRefusesAPidThatNamesNothingAndStays(t *testing.T) {
	const id = "amber-otter-1a2b"
	p, _ := midExitSentry(t, id, 4242)

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()

	err := p.Stop(ctx, id, time.Second)
	if err == nil || !strings.Contains(err.Error(), "so none was killed") {
		t.Fatalf("Stop of a stuck foreign pid returned %v, want the named refusal", err)
	}
}
