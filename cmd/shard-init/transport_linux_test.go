package main

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The S2 of SHARD-354 in one process: drop the supervisor's descriptor limit under what it holds, exec, and give the limit back.
func TestTransportExecAnswersAfterAFailedAccept(t *testing.T) {
	cmd, dial := startTransport(t)
	ctx := testContext(t)
	c, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if err := c.Run(t.Context(), supervisor.RunSpec{Argv: childArgv("sleep:60000")}); err != nil {
		t.Fatalf("run: %v", err)
	}

	pid := cmd.Process.Pid
	var held unix.Rlimit
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, nil, &held); err != nil {
		t.Fatalf("read the supervisor's limit: %v", err)
	}
	// fd 0 is already open, so every new descriptor fails with EMFILE.
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 1, Max: held.Max}, nil); err != nil {
		t.Fatalf("drop the supervisor's limit: %v", err)
	}

	say := supervisor.ExecHeader{Argv: childArgv("say:still-here"), Env: os.Environ()}
	first := make(chan error, 1)
	go func() {
		execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := supervisor.Exec(execCtx, dial, "sb", say, models.ExecSpec{})
		first <- err
	}()
	// The dial waits in the backlog while the guest's Accept fails on it.
	time.Sleep(200 * time.Millisecond)
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &held, nil); err != nil {
		t.Fatalf("give the supervisor its limit back: %v", err)
	}

	if err := <-first; err != nil {
		t.Fatalf("the exec dialled while Accept failed gave %v", err)
	}
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	exit, err := supervisor.Exec(execCtx, dial, "sb", say, models.ExecSpec{})
	if err != nil || exit.Code != 0 {
		t.Fatalf("the next exec gave %+v, %v", exit, err)
	}
}
