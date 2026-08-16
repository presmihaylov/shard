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

	"github.com/presmihaylov/shard/models"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -exit-file <path> -- <entrypoint> [args...]`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("shard-init", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), usage) }
	exitFile := fs.String("exit-file", "", "file the entrypoint exit status is written to, as JSON")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *exitFile == "" {
		return errors.New("-exit-file is required")
	}
	if fs.NArg() == 0 {
		return errors.New("no entrypoint given")
	}

	return supervise(fs.Args(), *exitFile)
}

// supervise never returns once the entrypoint starts: a sandbox outlives it, so only a kill ends us.
func supervise(argv []string, exitFile string) error {
	// Two channels, so a burst of SIGCHLD can never push a forwardable signal out of the buffer.
	chld := make(chan os.Signal, 1)
	fwd := make(chan os.Signal, 4)
	signal.Notify(chld, syscall.SIGCHLD)
	signal.Notify(fwd, syscall.SIGTERM, syscall.SIGINT)

	pid, err := spawn(argv)
	if err != nil {
		return fmt.Errorf("start entrypoint %q: %w", argv[0], err)
	}

	running := true
	for {
		select {
		case <-chld:
			exited, err := reap(pid, exitFile)
			if err != nil {
				return err
			}
			if exited {
				running = false
			}
		case sig := <-fwd:
			if !running {
				continue
			}
			if err := forward(pid, sig); err != nil {
				return err
			}
		}
	}
}

// PID 1 in a namespace has no default disposition, so a stop only works if we pass it on ourselves.
func forward(pid int, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("cannot forward signal %v to the entrypoint", sig)
	}

	err := syscall.Kill(pid, s)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("forward %s to the entrypoint: %w", s, err)
}

// reap drains every exited child, not only the entrypoint: orphaned grandchildren land on PID 1.
func reap(entrypoint int, exitFile string) (bool, error) {
	exited := false
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return exited, nil
		}
		if err != nil {
			return exited, fmt.Errorf("wait for a child: %w", err)
		}
		if pid <= 0 {
			return exited, nil
		}
		if pid != entrypoint {
			continue
		}

		if err := writeExitStatus(exitFile, statusOf(ws)); err != nil {
			return true, err
		}
		exited = true
	}
}

// A signalled entrypoint has no exit code of its own, so report the 128+n that a shell reports.
func statusOf(ws syscall.WaitStatus) models.ExitStatus {
	if ws.Signaled() {
		return models.ExitStatus{Code: 128 + int(ws.Signal()), Signal: ws.Signal()}
	}

	return models.ExitStatus{Code: ws.ExitStatus()}
}

// Provider.Wait watches this file from the host, so it must never read a half-written one.
func writeExitStatus(path string, status models.ExitStatus) error {
	blob, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal the exit status: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}

	return nil
}

// ForkExec, not os/exec: an os/exec Wait would race the wait4(-1) that reaps everything else.
func spawn(argv []string) (int, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return 0, fmt.Errorf("look up %q: %w", argv[0], err)
	}

	// The child gets our own stdio fds: shard streams them through, it does not proxy them.
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
	})
	if err != nil {
		return 0, fmt.Errorf("fork and exec %q: %w", path, err)
	}

	return pid, nil
}
