//go:build darwin && cgo

package pidpin

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestOpenOfAZombieSaysESRCH(t *testing.T) {
	pid, _ := child(t)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !exited(pid) {
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
