// Package pidpintest skips a test that ends a process through its pin where the host refuses to pin a live child, as a seatbelt sandbox does.
package pidpintest

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/pidpin"
)

// Require skips t only when this process may pin itself and a live child of its own still fails to pin, so a pin that fails for any other reason fails t.
func Require(t testing.TB) {
	t.Helper()
	self, err := pidpin.Open(os.Getpid())
	if err != nil {
		t.Fatalf("pin this process: %v", err)
	}
	if err := self.Close(); err != nil {
		t.Fatalf("release the pin of this process: %v", err)
	}

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Error(err)
		}
	}()

	p, err := pidpin.Open(cmd.Process.Pid)
	if errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a live child reads as gone: %v", err)
	}
	if err != nil {
		t.Skipf("this host refuses to pin a live child, so no test can end one through its pin: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("release the pin of a live child: %v", err)
	}
}
