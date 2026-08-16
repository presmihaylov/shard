// Command shard-init is PID 1 in every sandbox: it runs the entrypoint, reaps children and stays up.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -exit-file <path> -- <entrypoint> [args...]`

// errSupervisor marks a failure of our own bookkeeping, which the host reads back as an exit code.
var errSupervisor = errors.New("the supervisor failed")

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}

	fmt.Fprintln(os.Stderr, "shard-init:", err)
	os.Exit(exitCodeFor(err))
}

// The host reads this back with runsc wait, so a dead supervisor is diagnosable and not a mystery.
func exitCodeFor(err error) int {
	if errors.Is(err, errSupervisor) {
		return models.SupervisorFailedExitCode
	}

	return 1
}

func run(args []string) error {
	flags := flag.NewFlagSet("shard-init", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(flags.Output(), usage) }
	exitFile := flags.String("exit-file", "", "file the entrypoint exit status is written to, as JSON")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *exitFile == "" {
		return errors.New("-exit-file is required")
	}
	if flags.NArg() == 0 {
		return errors.New("no entrypoint given")
	}

	if err := supervise(flags.Args(), *exitFile); err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	return nil
}

// supervise never returns once the entrypoint starts: a sandbox outlives it, so only a kill ends us.
func supervise(entrypointArgv []string, exitFile string) error {
	// Two channels, so a burst of child deaths can never push a stop signal out of the buffer.
	childDeaths := make(chan os.Signal, 1)
	stopSignals := make(chan os.Signal, 4)
	signal.Notify(childDeaths, syscall.SIGCHLD)
	signal.Notify(stopSignals, syscall.SIGTERM, syscall.SIGINT)

	entrypointPID, err := startProcess(entrypointArgv)
	if err != nil {
		return fmt.Errorf("start entrypoint %q: %w", entrypointArgv[0], err)
	}

	entrypointRunning := true
	for {
		select {
		case <-childDeaths:
			entrypointExited, err := collectDeadChildren(entrypointPID, exitFile)
			if err != nil {
				return err
			}
			if entrypointExited {
				entrypointRunning = false
			}
		case received := <-stopSignals:
			if !entrypointRunning {
				continue
			}
			if err := forwardToEntrypoint(entrypointPID, received); err != nil {
				return err
			}
		}
	}
}

// PID 1 in a namespace has no default disposition, so a stop only works if we pass it on ourselves.
func forwardToEntrypoint(entrypointPID int, received os.Signal) error {
	unixSignal, ok := received.(syscall.Signal)
	if !ok {
		return fmt.Errorf("cannot forward signal %v to the entrypoint", received)
	}

	err := syscall.Kill(entrypointPID, unixSignal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("forward %s to the entrypoint: %w", unixSignal, err)
}

// It collects every dead child, not only the entrypoint: orphaned grandchildren land on PID 1.
func collectDeadChildren(entrypointPID int, exitFile string) (bool, error) {
	entrypointExited := false
	for {
		var waitStatus syscall.WaitStatus
		deadPID, err := syscall.Wait4(-1, &waitStatus, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return entrypointExited, nil
		}
		if err != nil {
			return entrypointExited, fmt.Errorf("wait for a child: %w", err)
		}
		// Zero means children are alive but none has died, so nothing is left to collect right now.
		if deadPID <= 0 {
			return entrypointExited, nil
		}
		if deadPID != entrypointPID {
			continue
		}

		if err := writeExitStatus(exitFile, exitStatusFrom(waitStatus)); err != nil {
			return true, err
		}
		entrypointExited = true
	}
}

// A signalled entrypoint has no exit code of its own, so report the 128+n that a shell reports.
func exitStatusFrom(waitStatus syscall.WaitStatus) models.ExitStatus {
	if waitStatus.Signaled() {
		return models.ExitStatus{Code: 128 + int(waitStatus.Signal()), Signal: waitStatus.Signal()}
	}

	return models.ExitStatus{Code: waitStatus.ExitStatus()}
}

// A full disk is usually transient, and losing a whole sandbox to one is worse than waiting it out.
const (
	exitFileAttempts = 5
	exitFileBackoff  = 200 * time.Millisecond
)

// Giving up here ends the sandbox, so retry first: only a lasting fault is worth dying for.
func writeExitStatus(path string, status models.ExitStatus) error {
	encoded, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal the exit status: %w", err)
	}

	var last error
	for attempt := range exitFileAttempts {
		if attempt > 0 {
			time.Sleep(exitFileBackoff)
		}

		last = writeFile(path, encoded)
		if last == nil {
			return nil
		}
	}

	return fmt.Errorf("write the exit status after %d attempts: %w", exitFileAttempts, last)
}

// Provider.Wait watches this file from the host, so it must never read a half-written one.
func writeFile(path string, encoded []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpPath, path, err)
	}

	return nil
}

// ForkExec, not os/exec: an os/exec Wait would race the wait4(-1) that collects every other child.
func startProcess(argv []string) (int, error) {
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return 0, fmt.Errorf("look up %q: %w", argv[0], err)
	}

	// The child gets our own stdio fds: shard streams them through, it does not proxy them.
	pid, err := syscall.ForkExec(binary, argv, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
	})
	if err != nil {
		return 0, fmt.Errorf("fork and exec %q: %w", binary, err)
	}

	return pid, nil
}
