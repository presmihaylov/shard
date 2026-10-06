//go:build linux

package vzvm_test

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// awaitStopped waits for the kernel to report child pid stopped, which it does only once every thread stopped; kill returns before that (SHARD-609).
func awaitStopped(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(stopGrace); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WSTOPPED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
			t.Fatalf("wait for process %d to stop: %v", pid, err)
		}
		if info.Signo == int32(unix.SIGCHLD) {
			return
		}
	}
	t.Fatalf("process %d did not stop within %s of its SIGSTOP", pid, stopGrace)
}
