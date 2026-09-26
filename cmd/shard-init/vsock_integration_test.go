//go:build integration

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/pkg/vsock"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The guest ports over a real AF_VSOCK, dialed back through the kernel's loopback; a host without vsock_loopback skips.
func TestTransportServesOverLiveVsock(t *testing.T) {
	probe, err := vsock.Listen(supervisor.FilesPort + 100)
	if err != nil {
		t.Skipf("no vsock device to listen on: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close the probe: %v", err)
	}
	loop, err := vsock.Dial(vsock.LocalCID, supervisor.ControlPort)
	if err == nil {
		_ = loop.Close()
		t.Fatalf("something already listens on vsock port %d; the guest ports must be free", supervisor.ControlPort)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ECONNRESET) {
		t.Skipf("no vsock loopback to dial through: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	cmd := exec.Command(exe, "-transport", "vsock")
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the supervisor: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the supervisor: %v", err)
		}
		_ = cmd.Wait()
	})
	dial := func(_ context.Context, port uint32) (net.Conn, error) { return vsock.Dial(vsock.LocalCID, port) }

	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect over vsock: %v", err)
	}
	defer c.Close()

	path := filepath.Join(t.TempDir(), "hello")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	stat, err := supervisor.Stat(ctx, dial, path)
	if err != nil {
		t.Fatalf("stat over vsock: %v", err)
	}
	if stat.Size != 5 {
		t.Fatalf("stat = %+v, want 5 bytes", stat)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the supervisor ended with %v, want a clean exit", err)
	}
}
