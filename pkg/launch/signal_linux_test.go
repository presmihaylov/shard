//go:build linux

package launch

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSignalReachesThePinnedCommand(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			r := start(t, middleRole, []string{"/bin/sleep", "30"})
			if _, err := r.await(t, r.pid); err != nil {
				t.Fatal(err)
			}
			if err := r.ch.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := r.exit(t); code != 128+int(sig) {
				t.Fatalf("the command exited %d, want signal %s", code, sig)
			}
			if err := r.ch.Signal(sig); !errors.Is(err, unix.ESRCH) {
				t.Fatalf("the reaped command returned %v, want ESRCH", err)
			}
		})
	}
}

func TestSignalAndUnpinSpareAReusedDescriptor(t *testing.T) {
	r := start(t, middleRole, []string{"/bin/sleep", "30"})
	if _, err := r.await(t, r.pid); err != nil {
		t.Fatal(err)
	}
	r.ch.mu.Lock()
	oldFD := r.ch.pidfd
	ownPin, err := unix.FcntlInt(uintptr(oldFD), unix.F_DUPFD_CLOEXEC, 0)
	r.ch.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.PidfdSendSignal(ownPin, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			t.Error(err)
		}
		if err := unix.Close(ownPin); err != nil {
			t.Error(err)
		}
	})
	bystander := exec.Command("/bin/sleep", "30")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bystander.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := bystander.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	})
	// The pin can close while a signal already holds a reference to the channel.
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		for range 1000 {
			if err := r.ch.Signal(0); err != nil && !errors.Is(err, os.ErrProcessDone) {
				done <- err
				return
			}
		}
		done <- nil
	}()
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	<-started
	if err := r.ch.unpin(); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(bystander.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fd != oldFD {
		if err := unix.Dup3(fd, oldFD, unix.O_CLOEXEC); err != nil {
			t.Fatal(errors.Join(err, unix.Close(fd)))
		}
		if err := unix.Close(fd); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := unix.Close(oldFD); err != nil {
			t.Error(err)
		}
	})
	for range 100 {
		if err := r.ch.Signal(syscall.SIGKILL); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("a released channel returned %v, want an ended exec", err)
		}
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the reused descriptor received the signal: %v", err)
	}
	if err := unix.PidfdSendSignal(ownPin, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	r.exit(t)
}
