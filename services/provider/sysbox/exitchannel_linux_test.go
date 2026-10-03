package sysbox_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/memfd"
	"github.com/presmihaylov/shard/services/bundle"
)

func sealedPage(t *testing.T) *os.File {
	t.Helper()

	f, err := memfd.Create("shard-exit", models.ExitChannelSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})

	return f
}

// script replaces what the fake sysbox-runc answers.
func (l *channelLab) script(t *testing.T, body string) {
	t.Helper()

	if err := os.WriteFile(l.runtime, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // the fake runtime the test executes
		t.Fatalf("write the fake sysbox-runc: %v", err)
	}
}

func record(t *testing.T, exit models.ExitStatus) []byte {
	t.Helper()

	encoded, err := json.Marshal(models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal})
	if err != nil {
		t.Fatal(err)
	}

	return []byte("\n" + string(encoded) + "\n")
}

func writeRecord(t *testing.T, f *os.File, exit models.ExitStatus) {
	t.Helper()

	page := make([]byte, models.ExitChannelSize)
	copy(page, record(t, exit))
	if _, err := f.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
}

// After a restart the daemon finds the page through PID 1's fd 0 and copies its record into the exit file.
func TestAReopenReadsTheSealedPageCreateRecorded(t *testing.T) {
	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))
	writeRecord(t, page, models.ExitStatus{Code: 5})

	exit, err := lab.exitStatus(t)
	if err != nil || exit == nil || *exit != (models.ExitStatus{Code: 5}) {
		t.Fatalf("ExitStatus returned %+v, %v, want {code:5}", exit, err)
	}

	// A guest write that is no record leaves the last one standing.
	if _, err := page.WriteAt([]byte("not a record\n"), 0); err != nil {
		t.Fatal(err)
	}
	exit, err = lab.exitStatus(t)
	if err != nil || exit == nil || *exit != (models.ExitStatus{Code: 5}) {
		t.Errorf("ExitStatus after a guest write returned %+v, %v, want {code:5} still", exit, err)
	}

	writeRecord(t, page, models.ExitStatus{Code: 7})
	if exit, err := lab.exitStatus(t); err != nil || exit == nil || *exit != (models.ExitStatus{Code: 7}) {
		t.Errorf("ExitStatus after the next exit returned %+v, %v, want {code:7}", exit, err)
	}
	got, found, err := bundle.ReadExitStatus(lab.b.ExitFile)
	if err != nil || !found || got != (models.ExitStatus{Code: 7}) {
		t.Errorf("the exit file reads %+v (found %v, %v), want {code:7}", got, found, err)
	}
}

// Guest root can make its own memfd with the same seals and size; only the inode create recorded tells it apart.
func TestAReopenNamesTheGuestsOwnSealedPageReplaced(t *testing.T) {
	lab := newChannelLab(t)
	ours := sealedPage(t)
	theirs := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", theirs.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", ours.Fd())))

	_, err := lab.exitStatus(t)
	if !errors.Is(err, models.ErrExitChannelReplaced) {
		t.Errorf("ExitStatus over the guest's own memfd returned %v, want %v", err, models.ErrExitChannelReplaced)
	}
}

// PID 1 writes its record and exits inside Stop, so Stop copies it before it returns: a daemon that dies next keeps it.
func TestAStopCopiesTheRecordPidOneLeftBeforeItReturns(t *testing.T) {
	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))

	dir := t.TempDir()
	report := filepath.Join(dir, "record")
	if err := os.WriteFile(report, record(t, models.ExitStatus{Code: 7}), 0o600); err != nil {
		t.Fatal(err)
	}
	killed := filepath.Join(dir, "killed")
	// The TERM is where shard-init writes the page and exits; dd with notrunc never shrinks the sealed page.
	lab.script(t, fmt.Sprintf(`case "$*" in
*" kill "*) dd if=%s of=/proc/%d/fd/%d conv=notrunc 2>/dev/null && touch %s ;;
*" state "*) if [ -e %s ]; then echo '{"id":%q,"status":"stopped","pid":0}'; else echo '{"id":%q,"status":"running","pid":%d}'; fi ;;
esac`, report, os.Getpid(), page.Fd(), killed, killed, labID, labID, labPid))

	if err := lab.p.Stop(t.Context(), labID, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got, found, err := bundle.ReadExitStatus(lab.b.ExitFile)
	if err != nil || !found || got != (models.ExitStatus{Code: 7}) {
		t.Errorf("after Stop the exit file reads %+v (found %v, %v), want {code:7}", got, found, err)
	}
}

// Guest root that replaced fd 0 loses its exit record, never the stop that only the host may give.
func TestAStopEndsASandboxWhoseFdZeroWasReplaced(t *testing.T) {
	lab := newChannelLab(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lab.pointFd0(t, fifo)
	lab.record(t, inodeOf(t, fifo))
	killed := filepath.Join(t.TempDir(), "killed")
	lab.script(t, fmt.Sprintf(`case "$*" in
*" kill "*) touch %s ;;
*" state "*) if [ -e %s ]; then echo '{"id":%q,"status":"stopped","pid":0}'; else echo '{"id":%q,"status":"running","pid":%d}'; fi ;;
esac`, killed, killed, labID, labID, labPid))

	if err := lab.p.Stop(t.Context(), labID, time.Second); err != nil {
		t.Errorf("Stop over a replaced fd 0 returned %v, want the sandbox stopped", err)
	}
}

// stopLab is a sandbox whose sysbox-runc runs it until a KILL ends it, and until a TERM does too when honoursTerm says so.
func stopLab(t *testing.T, honoursTerm bool) (*channelLab, string) {
	t.Helper()

	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))
	work := t.TempDir()
	ended := filepath.Join(work, "ended")
	onTerm := ":"
	if honoursTerm {
		onTerm = "touch " + ended
	}
	lab.script(t, fmt.Sprintf(`case "$*" in
*" kill "*KILL) touch %s %s ;;
*" kill "*TERM) %s ;;
*" state "*) if [ -e %s ]; then echo '{"id":%q,"status":"stopped","pid":0}'; else echo '{"id":%q,"status":"running","pid":%d}'; fi ;;
esac`, filepath.Join(work, "killed"), ended, onTerm, ended, labID, labID, labPid))

	return lab, work
}

// The grace bounds the stop and is never a wait: an entrypoint that exits on TERM ends it at once (SHARD-460).
func TestAStopReturnsOnceTheEntrypointExitsOnTerm(t *testing.T) {
	lab, work := stopLab(t, true)

	started := time.Now()
	if err := lab.p.Stop(t.Context(), labID, models.StopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("Stop took %s of the %s grace, so it waited past an entrypoint that exited on TERM", took, models.StopGrace)
	}
	if _, err := os.Stat(filepath.Join(work, "killed")); err == nil {
		t.Error("Stop sent KILL to an entrypoint that exited on TERM")
	}
}

func TestAStopKillsAnEntrypointThatIgnoresTermOnceTheGraceRunsOut(t *testing.T) {
	lab, work := stopLab(t, false)

	grace := 500 * time.Millisecond
	started := time.Now()
	if err := lab.p.Stop(t.Context(), labID, grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if took := time.Since(started); took < grace || took > grace+3*time.Second {
		t.Errorf("Stop took %s, want the %s grace and then the kill", took, grace)
	}
	if _, err := os.Stat(filepath.Join(work, "killed")); err != nil {
		t.Errorf("Stop never sent KILL to an entrypoint that ignored TERM: %v", err)
	}
}
