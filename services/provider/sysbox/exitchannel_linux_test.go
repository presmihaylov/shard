package sysbox_test

import (
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

func writeTable(t *testing.T, f *os.File, reports ...models.ProcessReport) {
	t.Helper()

	page := make([]byte, models.ExitChannelSize)
	copy(page, tableLine(t, reports...))
	if _, err := f.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
}

// hasProcess reports a table of exactly one process, name, in state.
func hasProcess(rows []models.ProcessReport, name string, state models.ProcessState) bool {
	return len(rows) == 1 && rows[0].Name == name && rows[0].State == state
}

// After a restart the daemon finds the page through PID 1's fd 0 and copies its table into the exit file.
func TestAReopenReadsTheSealedPageCreateRecorded(t *testing.T) {
	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))
	writeTable(t, page, reportOf("web", models.ProcessRunning, 0))

	rows, err := lab.processes(t)
	if err != nil || !hasProcess(rows, "web", models.ProcessRunning) {
		t.Fatalf("Processes returned %+v, %v, want web running", rows, err)
	}

	// A guest write that is no table fails the read and nothing else, and the next table reads again.
	if _, err := page.WriteAt([]byte("not a table\n"), 0); err != nil {
		t.Fatal(err)
	}
	if rows, err := lab.processes(t); err == nil {
		t.Errorf("Processes after a guest write returned %+v, want the forged page refused", rows)
	}

	writeTable(t, page, reportOf("web", models.ProcessGaveUp, 2))
	if rows, err := lab.processes(t); err != nil || !hasProcess(rows, "web", models.ProcessGaveUp) {
		t.Errorf("Processes after the next table returned %+v, %v, want web gave up", rows, err)
	}
	rows, err = bundle.ReadProcessTable(lab.b.ExitFile)
	if err != nil || !hasProcess(rows, "web", models.ProcessGaveUp) {
		t.Errorf("the exit file reads %+v, %v, want web gave up", rows, err)
	}
}

// Guest root can make its own memfd with the same seals and size; only the inode create recorded tells it apart.
func TestAReopenNamesTheGuestsOwnSealedPageReplaced(t *testing.T) {
	lab := newChannelLab(t)
	ours := sealedPage(t)
	theirs := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", theirs.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", ours.Fd())))

	_, err := lab.processes(t)
	if !errors.Is(err, models.ErrExitChannelReplaced) {
		t.Errorf("Processes over the guest's own memfd returned %v, want %v", err, models.ErrExitChannelReplaced)
	}
}

// PID 1 writes its last table and exits inside Stop, so Stop copies it before it returns: a daemon that dies next keeps it.
func TestAStopCopiesTheTablePidOneLeftBeforeItReturns(t *testing.T) {
	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))

	dir := t.TempDir()
	report := filepath.Join(dir, "table")
	if err := os.WriteFile(report, tableLine(t, reportOf("web", models.ProcessExited, 0)), 0o600); err != nil {
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

	rows, err := bundle.ReadProcessTable(lab.b.ExitFile)
	if err != nil || !hasProcess(rows, "web", models.ProcessExited) {
		t.Errorf("after Stop the exit file reads %+v, %v, want web exited", rows, err)
	}
}

// Guest root that replaced fd 0 loses its process table, never the stop that only the host may give.
func TestAStopEndsASandboxWhoseFdZeroWasReplaced(t *testing.T) {
	for name, target := range map[string]func(*testing.T) string{
		"a FIFO": func(t *testing.T) string {
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}

			return fifo
		},
		"a file the daemon cannot open": unopenable,
	} {
		t.Run(name, func(t *testing.T) {
			lab := newChannelLab(t)
			path := target(t)
			lab.pointFd0(t, path)
			lab.record(t, inodeOf(t, path))
			killed := filepath.Join(t.TempDir(), "killed")
			lab.script(t, fmt.Sprintf(`case "$*" in
*" kill "*) touch %s ;;
*" state "*) if [ -e %s ]; then echo '{"id":%q,"status":"stopped","pid":0}'; else echo '{"id":%q,"status":"running","pid":%d}'; fi ;;
esac`, killed, killed, labID, labID, labPid))

			if err := lab.p.Stop(t.Context(), labID, time.Second); err != nil {
				t.Errorf("Stop over %s on fd 0 returned %v, want the sandbox stopped", name, err)
			}
		})
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

// The grace bounds the stop and is never a wait: a shard-init that exits on TERM ends it at once (SHARD-460).
func TestAStopReturnsOnceShardInitExitsOnTerm(t *testing.T) {
	lab, work := stopLab(t, true)

	started := time.Now()
	if err := lab.p.Stop(t.Context(), labID, models.StopGrace); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("Stop took %s of the %s grace, so it waited past a sandbox that exited on TERM", took, models.StopGrace)
	}
	if _, err := os.Stat(filepath.Join(work, "killed")); err == nil {
		t.Error("Stop sent KILL to a sandbox that exited on TERM")
	}
}

func TestAStopKillsASandboxThatIgnoresTermOnceTheGraceRunsOut(t *testing.T) {
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
		t.Errorf("Stop never sent KILL to a sandbox that ignored TERM: %v", err)
	}
}
