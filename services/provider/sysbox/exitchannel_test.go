package sysbox_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/sysbox"
)

const (
	labID  = "amber-otter-1a2b"
	labPid = 4242
)

// channelLab is a sandbox a restarted daemon finds running: no channel held, PID 1 in its cgroup, fd 0 under a fake /proc.
type channelLab struct {
	p       *sysbox.Provider
	b       bundle.Bundle
	fd0     string
	cg      string
	runtime string
}

func newChannelLab(t *testing.T) *channelLab {
	t.Helper()

	dir := t.TempDir()
	binary := filepath.Join(dir, "sysbox-runc")
	script := fmt.Sprintf("#!/bin/sh\necho '{\"id\":%q,\"status\":\"running\",\"pid\":%d}'\n", labID, labPid)
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil { //nolint:gosec // the fake runtime the test executes
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

	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	p, err := sysbox.New(runner, bundles, func(string) (string, error) { return stateDir, nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := bundle.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	cgroupRoot := filepath.Join(dir, "cgroup")
	lab := &channelLab{p: p, b: b, cg: filepath.Join(cgroupRoot, bundle.CgroupsPath(labID)), runtime: binary}
	if err := os.MkdirAll(lab.cg, 0o750); err != nil {
		t.Fatal(err)
	}
	lab.placePid(t, labPid)
	p.SetCgroupRoot(cgroupRoot)

	procRoot := filepath.Join(dir, "proc")
	fdDir := filepath.Join(procRoot, strconv.Itoa(labPid), "fd")
	if err := os.MkdirAll(fdDir, 0o750); err != nil {
		t.Fatal(err)
	}
	lab.fd0 = filepath.Join(fdDir, "0")
	p.SetProcRoot(procRoot)

	return lab
}

func (l *channelLab) placePid(t *testing.T, pids ...int) {
	t.Helper()

	var procs []byte
	for _, pid := range pids {
		procs = strconv.AppendInt(procs, int64(pid), 10)
		procs = append(procs, '\n')
	}
	if err := os.WriteFile(filepath.Join(l.cg, "cgroup.procs"), procs, 0o600); err != nil {
		t.Fatal(err)
	}
}

// pointFd0 makes PID 1's fd 0 resolve to target, as /proc/<pid>/fd/0 does.
func (l *channelLab) pointFd0(t *testing.T, target string) {
	t.Helper()

	if err := os.Symlink(target, l.fd0); err != nil {
		t.Fatal(err)
	}
}

func (l *channelLab) record(t *testing.T, inode uint64) {
	t.Helper()

	if err := l.b.RecordExitChannel(bundle.ExitChannel{Inode: inode}); err != nil {
		t.Fatal(err)
	}
}

// exitStatus fails the test rather than hang, because a reopen must never block on what the guest put on fd 0.
func (l *channelLab) exitStatus(t *testing.T) (*models.ExitStatus, error) {
	t.Helper()

	type answer struct {
		exit *models.ExitStatus
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		exit, err := l.p.ExitStatus(t.Context(), labID)
		done <- answer{exit, err}
	}()
	select {
	case got := <-done:
		return got.exit, got.err
	case <-time.After(10 * time.Second):
		t.Fatal("ExitStatus blocked on fd 0 of PID 1")
		return nil, nil
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s has no inode", path)
	}

	return st.Ino
}

// Guest root can ptrace PID 1 and dup2 a FIFO onto fd 0; a reopen names it and never opens it (SHARD-419).
func TestAReopenNamesAFifoOnFdZeroReplacedWithoutOpeningIt(t *testing.T) {
	lab := newChannelLab(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lab.pointFd0(t, fifo)
	lab.record(t, inodeOf(t, fifo))

	_, err := lab.exitStatus(t)
	if !errors.Is(err, models.ErrExitChannelReplaced) {
		t.Errorf("ExitStatus over a FIFO on fd 0 returned %v, want %v", err, models.ErrExitChannelReplaced)
	}
}

// A regular file the guest put there fails one of the size, inode and seal checks, even when the others pass.
func TestAReopenNamesARegularFileThatIsNotThePageReplaced(t *testing.T) {
	cases := []struct {
		name  string
		size  int64
		shift uint64
	}{
		{"the wrong size", 64 << 10, 0},
		{"another inode", models.ExitChannelSize, 1},
		{"no seals", models.ExitChannelSize, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newChannelLab(t)
			path := filepath.Join(t.TempDir(), "page")
			if err := os.WriteFile(path, make([]byte, tc.size), 0o600); err != nil {
				t.Fatal(err)
			}
			lab.pointFd0(t, path)
			lab.record(t, inodeOf(t, path)+tc.shift)

			_, err := lab.exitStatus(t)
			if !errors.Is(err, models.ErrExitChannelReplaced) {
				t.Errorf("ExitStatus over a regular file with %s returned %v, want %v", tc.name, err, models.ErrExitChannelReplaced)
			}
		})
	}
}

// A pid runc names that is not in the sandbox cgroup is not its PID 1, so nothing of it is opened.
func TestAReopenOpensNothingOfAPidOutsideTheCgroup(t *testing.T) {
	lab := newChannelLab(t)
	lab.placePid(t, labPid+1)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lab.pointFd0(t, fifo)
	lab.record(t, inodeOf(t, fifo))

	exit, err := lab.exitStatus(t)
	if err != nil || exit != nil {
		t.Errorf("ExitStatus of a foreign pid returned %+v, %v, want no exit and no error", exit, err)
	}
}

// A sandbox created before the sealed channel still reports into the exit file, so the reader keeps reading it.
func TestASandboxFromBeforeTheChannelReadsItsExitFile(t *testing.T) {
	lab := newChannelLab(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	lab.pointFd0(t, fifo)
	if err := bundle.WriteExitStatus(lab.b.ExitFile, models.ExitStatus{Code: 9}); err != nil {
		t.Fatal(err)
	}

	exit, err := lab.exitStatus(t)
	if err != nil || exit == nil || *exit != (models.ExitStatus{Code: 9}) {
		t.Errorf("ExitStatus returned %+v, %v, want {code:9} from the exit file", exit, err)
	}
}
