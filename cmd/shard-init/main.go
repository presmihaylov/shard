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
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -exit-file <path> -- <entrypoint> [args...]`

// errSupervisor marks a failure of our own bookkeeping, which the host reads back as an exit code.
var errSupervisor = errors.New("the supervisor failed")

// errNoEntrypoint marks a broken image, not a broken supervisor, so the two do not share an exit code.
var errNoEntrypoint = errors.New("the entrypoint did not start")

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
	if errors.Is(err, errNoEntrypoint) {
		return models.EntrypointNotStartedExitCode
	}
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
	// This also rejects -exit-file --, which the flag package otherwise takes as the value.
	if !filepath.IsAbs(*exitFile) {
		return fmt.Errorf("-exit-file must be an absolute path, got %q", *exitFile)
	}
	if flags.NArg() == 0 {
		return errors.New("no entrypoint given")
	}

	err := supervise(flags.Args(), *exitFile)
	if errors.Is(err, errNoEntrypoint) {
		return err
	}
	if err != nil {
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
		return fmt.Errorf("%w: %q: %w", errNoEntrypoint, entrypointArgv[0], err)
	}

	for {
		select {
		case <-childDeaths:
			// The guest PID space wraps at 65536, so stop watching the PID once it has been reaped.
			if collectDeadChildren(entrypointPID, exitFile) {
				entrypointPID = 0
			}
		case received := <-stopSignals:
			if entrypointPID == 0 {
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
func collectDeadChildren(entrypointPID int, exitFile string) bool {
	entrypointExited := false
	for {
		var waitStatus syscall.WaitStatus
		deadPID, err := syscall.Wait4(-1, &waitStatus, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return entrypointExited
		}
		// PID 1 must survive, so an unexpected wait error is reported and the next SIGCHLD tries again.
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: wait for a child:", err)

			return entrypointExited
		}
		// Zero means children are alive but none has died, so nothing is left to collect right now.
		if deadPID <= 0 {
			return entrypointExited
		}
		if entrypointPID == 0 || deadPID != entrypointPID {
			continue
		}

		// A sandbox outlives its entrypoint, so a lost exit status is reported and never fatal (AGENTS.md).
		if err := writeExitStatus(exitFile, exitStatusFrom(waitStatus)); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init:", err)
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

// A full disk is usually transient, so the budget is tens of seconds and not the length of one hiccup.
const (
	exitFileAttempts = 60
	exitFileBackoff  = 500 * time.Millisecond
)

// permanentErrnos names the faults no amount of waiting clears, so retrying them only delays the message.
var permanentErrnos = []syscall.Errno{
	syscall.EROFS, syscall.EACCES, syscall.EPERM, syscall.ENOENT, syscall.ENOTDIR, syscall.ENOTEMPTY,
}

// Retry first: a transient full disk must not cost the exit status of an otherwise healthy sandbox.
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

		// pkg/store lands it through a random temp name and an fsync, so no planted path and no lost write.
		last = store.WriteFile(path, encoded, 0o600)
		if last == nil {
			return nil
		}
		if slices.ContainsFunc(permanentErrnos, func(code syscall.Errno) bool { return errors.Is(last, code) }) {
			return fmt.Errorf("write the exit status: %w", last)
		}
	}

	return fmt.Errorf("write the exit status after %d attempts: %w", exitFileAttempts, last)
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
