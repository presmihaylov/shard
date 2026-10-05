//go:build darwin && cgo

package pidpin

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdEnv turns the test binary into a child that holds memory, so its exit lasts long enough to pin it in the middle.
const holdEnv = "PIDPIN_HOLD"

func TestMain(m *testing.M) {
	if os.Getenv(holdEnv) == "1" {
		hold()

		return
	}
	os.Exit(m.Run())
}

func hold() {
	held := make([]byte, 256<<20)
	for i := 0; i < len(held); i += os.Getpagesize() {
		held[i] = 1
	}
	fmt.Println("held")
	time.Sleep(time.Minute)
	runtime.KeepAlive(held)
}

// A process in its exit is out of task_name_for_pid's reach and past any signal, so a pin of it reads gone (SHARD-530).
func TestOpenOfAProcessInItsExitSaysESRCH(t *testing.T) {
	requirePin(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), holdEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the child said %q, %v, want held", line, err)
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := Open(pid)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("Open of a process in its exit = %v, want ESRCH", err)
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the child is no zombie 5s after its kill")
		}
	}
}

func TestOpenOfAZombieSaysESRCH(t *testing.T) {
	pid, _ := child(t)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// exited reads gone already in the exit, where task_name_for_pid can still find the pid, so wait for the zombie itself.
	deadline := time.Now().Add(5 * time.Second)
	for !zombied(t, pid) {
		if time.Now().After(deadline) {
			t.Fatal("the child is no zombie 5s after its kill")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := Open(pid)
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Open of a zombie = %v, want ESRCH", err)
	}
}

func zombied(t *testing.T, pid int) bool {
	t.Helper()
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		t.Fatal(err)
	}

	return len(procs) == 1 && procs[0].Proc.P_stat == zombie
}

// A token whose pid version is not the live process's stands for one that held the pid before, which no signal may reach.
func TestASignalThroughAnEarlierHoldersTokenReachesNobody(t *testing.T) {
	requirePin(t)
	pid, _ := child(t)
	p := pinned(t, pid)
	p.handle.token[7]--

	if err := signal(p.handle, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("signal through an earlier token = %v, want ESRCH", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the live holder of the pid was hit: %v", err)
	}
	if exited(pid) {
		t.Fatal("the live holder of the pid was hit")
	}
}

func TestExitedSaysAnEarlierHoldersTokenIsGone(t *testing.T) {
	requirePin(t)
	pid, _ := child(t)
	p := pinned(t, pid)
	p.handle.token[pidVersion]--

	if done, err := p.Exited(); err != nil || !done {
		t.Fatalf("Exited through an earlier token = %v, %v, want true", done, err)
	}
}
