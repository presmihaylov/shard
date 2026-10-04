package vz

import (
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/pidpin/pidpintest"
)

func TestOnlyTheArgumentsStartGivesAShimServeItsSocket(t *testing.T) {
	const socket = "/s/a/shim.sock"
	config, err := json.Marshal(Config{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	other, err := json.Marshal(Config{Socket: "/s/b/shim.sock"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"the shim of the socket", []string{"shard-vz-shim", "-config", string(config)}, true},
		{"the shim of another socket", []string{"shard-vz-shim", "-config", string(other)}, false},
		{"another flag", []string{"shard-vz-shim", "-restore", string(config)}, false},
		{"one argument more", []string{"shard-vz-shim", "-config", string(config), "-v"}, false},
		{"a config that is no json", []string{"shard-vz-shim", "-config", socket}, false},
		{"no arguments", nil, false},
	}
	for _, c := range cases {
		if got := serves(c.args, socket); got != c.want {
			t.Errorf("%s: serves = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestKillOfAShimInTheDaemonsGroupSignalsItAlone(t *testing.T) {
	pidpintest.Require(t)
	shim, waitShim := child(t, exec.Command("sleep", "60"), nil)
	sibling, _ := child(t, exec.Command("sleep", "60"), nil)
	pgid, err := syscall.Getpgid(shim)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != syscall.Getpgrp() {
		t.Fatalf("the shim's group is %d, want this test's own %d", pgid, syscall.Getpgrp())
	}
	p, err := Identify(shim)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Kill(); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	reaped(t, waitShim)
	// A signal to the group would have reached the sibling by now, and Identify reads a zombie as gone.
	time.Sleep(200 * time.Millisecond)
	if _, err := Identify(sibling); err != nil {
		t.Errorf("a process in the test's own group after Kill: %v", err)
	}
}

// A check and a kill that read the pid apart would end whatever holds it by the kill, so the kill goes through the pin the check judged.
func TestKillSignalsThePinnedShimWhenItsPidNamesAnotherAfterTheCheck(t *testing.T) {
	pidpintest.Require(t)
	shim, waitShim := child(t, exec.Command("sleep", "60"), nil)
	innocent, _ := child(t, exec.Command("sleep", "60"), nil)
	p, err := Identify(shim)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.kill(func(q *Process) { q.PID = innocent }); err != nil {
		t.Fatalf("kill = %v", err)
	}
	reaped(t, waitShim)
	if _, err := Identify(innocent); err != nil {
		t.Fatalf("the process on the pid after the check was hit: %v", err)
	}
}

func TestAliveRefusesAZombieAndAPidWithAnotherStartTime(t *testing.T) {
	pid, wait := child(t, exec.Command("sleep", "60"), nil)
	p, err := Identify(pid)
	if err != nil {
		t.Fatal(err)
	}
	if alive, err := p.Alive(); err != nil || !alive {
		t.Fatalf("Alive of a running child = %t, %v", alive, err)
	}
	if alive, err := (Process{PID: pid, Start: p.Start + 1}).Alive(); err != nil || alive {
		t.Errorf("Alive with another start time = %t, %v; want false", alive, err)
	}

	// Nothing reaps the child before reaped, so it stays a zombie in between.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		alive, err := p.Alive()
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a killed child still reads alive after 5s")
		}
	}
	if err := p.Kill(); err != nil {
		t.Errorf("Kill of a zombie = %v, want nothing done", err)
	}
	reaped(t, wait)
	if _, err := Identify(pid); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Identify of a reaped pid = %v, want ESRCH", err)
	}
}

// child runs cmd and kills it when the test ends; nothing reaps it before the wait it returns.
func child(t *testing.T, cmd *exec.Cmd, attr *syscall.SysProcAttr) (int, func() error) {
	t.Helper()
	cmd.SysProcAttr = attr
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

// reaped collects the exit of a killed child, which must come within 5s.
func reaped(t *testing.T, wait func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child still runs 5s after its kill")
	}
}
