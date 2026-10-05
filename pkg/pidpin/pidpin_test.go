//go:build linux || (darwin && cgo)

package pidpin

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestOpenRefusesAPidThatNamesNoProcessToEnd(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if _, err := Open(pid); err == nil {
			t.Errorf("Open(%d) = nil, want a refusal", pid)
		}
	}
}

func TestKillEndsTheProcessItHolds(t *testing.T) {
	requirePin(t)
	pid, wait := child(t)
	p := pinned(t, pid)

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	if got := killedBy(t, wait); got != syscall.SIGKILL {
		t.Fatalf("the child ended by %v, want SIGKILL", got)
	}
}

func TestKillOfAProcessReapedSinceIsNoError(t *testing.T) {
	requirePin(t)
	pid, wait := child(t)
	p := pinned(t, pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killedBy(t, wait)

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill after the reap = %v, want nil", err)
	}
}

func TestOpenOfAReapedPidSaysESRCH(t *testing.T) {
	pid, wait := child(t)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killedBy(t, wait)

	_, err := Open(pid)
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Open of a reaped pid = %v, want ESRCH", err)
	}
}

func child(t *testing.T) (int, func() error) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait := sync.OnceValue(cmd.Wait)
	t.Cleanup(func() {
		if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})

	return cmd.Process.Pid, wait
}

func pinned(t *testing.T, pid int) *Process {
	t.Helper()
	p, err := Open(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})

	return p
}

// requirePin is pidpintest.Require for this package's own tests, which that package cannot serve without an import cycle.
func requirePin(t *testing.T) {
	t.Helper()
	pinned(t, os.Getpid())
	pid, _ := child(t)
	p, err := Open(pid)
	if errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a live child reads as gone: %v", err)
	}
	if err != nil {
		t.Skipf("this host refuses to pin a live child, so no test can end one through its pin: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// killedBy reaps the child, which must end within 5s, and names the signal that ended it.
func killedBy(t *testing.T, wait func() error) syscall.Signal {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the child exited by %v, want a signal", err)
		}
		ws, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() {
			t.Fatalf("the child exited by %v, want a signal", err)
		}
		return ws.Signal()
	case <-time.After(5 * time.Second):
		t.Fatal("the child still runs 5s after its kill")
	}

	return 0
}

func TestKillAfterCloseSignalsNothing(t *testing.T) {
	requirePin(t)
	pid, _ := child(t)
	p, err := Open(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("a second Close = %v, want nil", err)
	}

	if err := p.Kill(); err == nil {
		t.Fatal("Kill after Close = nil, want a refusal")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the child was hit by a Kill after Close: %v", err)
	}
}

func TestExitedTurnsTrueOnceTheHeldProcessEnds(t *testing.T) {
	requirePin(t)
	pid, wait := child(t)
	p := pinned(t, pid)
	if done, err := p.Exited(); err != nil || done {
		t.Fatalf("Exited of a live child = %v, %v, want false", done, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// A zombie has exited, so the probe says so before the reap.
	deadline := time.Now().Add(5 * time.Second)
	for {
		done, err := p.Exited()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Exited still says false 5s after the kill")
		}
		time.Sleep(10 * time.Millisecond)
	}
	killedBy(t, wait)
	if done, err := p.Exited(); err != nil || !done {
		t.Fatalf("Exited after the reap = %v, %v, want true", done, err)
	}
}

func TestExitedAfterCloseIsAnError(t *testing.T) {
	requirePin(t)
	pid, _ := child(t)
	p, err := Open(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Exited(); err == nil {
		t.Fatal("Exited after Close = nil, want an error")
	}
}
