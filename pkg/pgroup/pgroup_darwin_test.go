package pgroup_test

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/pgroup"
)

// zombie is SZOMB in sys/proc.h, which x/sys does not name.
const zombie = 5

// A group whose one member exited unreaped is the state a cleanup meets between a child's exit and its parent's wait (SHARD-513).
func TestAGroupOfOnlyZombiesIsGone(t *testing.T) {
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	defer signal.Stop(exits)
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// The kernel marks a child a zombie before it sends its parent SIGCHLD, so each SIGCHLD is the moment to look again.
	for !isZombie(t, pid) {
		<-exits
	}

	if err := syscall.Kill(-pid, syscall.SIGKILL); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill of a group of one zombie = %v, want the EPERM this package answers for", err)
	}
	if err := pgroup.Kill(pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Kill of a group of one zombie = %v, want ESRCH", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reap the zombie: %v", err)
	}
}

func isZombie(t *testing.T, pid int) bool {
	t.Helper()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		t.Fatalf("read the state of %d: %v", pid, err)
	}
	if len(procs) == 0 {
		t.Fatalf("pid %d is gone before its parent reaped it", pid)
	}

	return procs[0].Proc.P_stat == zombie
}
