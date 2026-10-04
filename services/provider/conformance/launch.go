package conformance

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
)

// launchDir holds the files whose modes the refusals need, in the one sandbox RunLaunch drives.
const launchDir = "/launch"

type refusal struct {
	name string
	argv []string
	user string
	code int
}

// RunLaunch takes modeBindsNobody because providers give an exec different capabilities (SHARD-498).
func RunLaunch(t *testing.T, s Subject, modeBindsNobody bool) {
	t.Helper()

	spec := s.NewSpec(t)
	id := s.start(t, spec)
	setup := "mkdir -p " + launchDir + " && cd " + launchDir +
		" && printf '#!/bin/sh\\nexit 0\\n' > plain && chmod 0644 plain" +
		" && printf '#!/no/such/interpreter\\n' > orphan && chmod 0755 orphan" +
		" && printf '#!/bin/sh\\nexit 0\\n' > private && chmod 0700 private"
	if status, out := s.exec(t, id, models.ExecSpec{Argv: s.Shell(setup)}); status.Code != 0 {
		t.Fatalf("write the launch files: exited %d: %s", status.Code, out)
	}

	refusals := []refusal{
		{"APathThatIsNotThere", []string{"/no/such/binary"}, "", models.CommandNotFoundExitCode},
		{"ANameOnNoPATHEntry", []string{"no-such-command"}, "", models.CommandNotFoundExitCode},
		{"AFileWithNoExecuteBit", []string{launchDir + "/plain"}, "", models.CommandNotExecutableExitCode},
		{"AnInterpreterThatIsNotThere", []string{launchDir + "/orphan"}, "", models.CommandNotFoundExitCode},
		{"ADirectory", []string{launchDir}, "", models.CommandNotExecutableExitCode},
	}
	if modeBindsNobody {
		refusals = append(refusals, refusal{"ARootFile0700ToNobody", []string{launchDir + "/private"}, "nobody", models.CommandNotExecutableExitCode})
	}

	for _, tty := range []bool{false, true} {
		for _, r := range refusals {
			t.Run(terminalName(r.name, tty), func(t *testing.T) {
				reported := &reports{}
				_, _, err := s.launch(t, id, models.ExecSpec{Argv: r.argv, User: r.user, Report: reported.add}, tty)

				var refused *models.CommandNotStartedError
				if !errors.As(err, &refused) {
					t.Fatalf("Exec returned %v, want a CommandNotStartedError", err)
				}
				if refused.Code != r.code {
					t.Errorf("the refusal carries code %d, want %d", refused.Code, r.code)
				}
				for _, host := range []string{spec.RootFS, spec.StateDir} {
					if strings.Contains(err.Error(), host) {
						t.Errorf("the refusal %q names the host path %s", err, host)
					}
				}
				if pids := reported.all(); len(pids) != 0 {
					t.Errorf("a command that never started reported pids %v", pids)
				}
			})
		}

		t.Run(terminalName("NobodyLaunchesACommand", tty), func(t *testing.T) {
			s.launched(t, id, models.ExecSpec{Argv: []string{"/bin/true"}, User: "nobody"}, tty, 0)
		})

		t.Run(terminalName("ACommandThatEndsAtOnceLaunches", tty), func(t *testing.T) {
			s.launched(t, id, models.ExecSpec{Argv: s.Shell("exit 3")}, tty, 3)
		})
	}

	t.Run("ATerminalReachesTheCommand", func(t *testing.T) {
		s.launched(t, id, models.ExecSpec{Argv: s.Shell("test -t 0 && test -t 1 && exit 4")}, true, 4)
	})

	t.Run("TheCommandKeepsItsArgvEnvAndCwd", func(t *testing.T) {
		exec := models.ExecSpec{
			Argv:    []string{"/bin/sh", "-c", `printf '%s|' "$@" "$PWD" "$LAUNCH"`, "launch", "a b", "", "c"},
			Env:     []string{"LAUNCH=set"},
			WorkDir: "/tmp",
		}
		if out := s.launched(t, id, exec, false, 0); out != "a b||c|/tmp|set|" {
			t.Errorf("the command printed %q, want its argv, then /tmp, then set", out)
		}
	})

	// A runtime's own exec hands the command no blocked and no ignored signal, and the shim between must keep that.
	t.Run("TheCommandStartsWithNoSignalBlockedOrIgnored", func(t *testing.T) {
		out := s.launched(t, id, models.ExecSpec{Argv: []string{"/bin/cat", "/proc/self/status"}}, false, 0)
		if !strings.Contains(out, "SigBlk:") {
			t.Skip("gVisor /proc/self/status has no SigBlk or SigIgn")
		}
		for _, line := range []string{"SigBlk:\t0000000000000000\n", "SigIgn:\t0000000000000000\n"} {
			if !strings.Contains(out, line) {
				t.Errorf("the command's status has no %q line:\n%s", strings.TrimSpace(line), out)
			}
		}
	})

	t.Run("ASignalReachesTheLaunchedCommand", func(t *testing.T) {
		pids := make(chan int, 1)
		type result struct {
			status models.ExitStatus
			err    error
		}
		done := make(chan result, 1)
		go func() {
			status, err := s.Provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sleep", "60"}, Report: func(pid int) { pids <- pid }})
			done <- result{status, err}
		}()

		var pid int
		select {
		case pid = <-pids:
		case r := <-done:
			t.Fatalf("the exec ended before it reported a pid: %+v, %v", r.status, r.err)
		}
		if err := s.Provider.Signal(t.Context(), id, pid, "TERM"); err != nil {
			t.Fatalf("Signal: %v", err)
		}

		r := <-done
		if r.err != nil {
			t.Fatalf("Exec: %v", r.err)
		}
		if r.status.Code != 143 {
			t.Errorf("the command exited %d, want 143 from the TERM", r.status.Code)
		}
	})
}

// launched runs a command that must start: it reports one pid, and exits code.
func (s Subject) launched(t *testing.T, id string, spec models.ExecSpec, tty bool, code int) string {
	t.Helper()

	reported := &reports{}
	spec.Report = reported.add
	status, out, err := s.launch(t, id, spec, tty)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if status.Code != code {
		t.Errorf("the command exited %d, want %d: %s", status.Code, code, out)
	}
	if pids := reported.all(); len(pids) != 1 || pids[0] <= 0 {
		t.Errorf("the command reported pids %v, want one", pids)
	}

	return out
}

// launch runs spec on a file, whose output it returns, or on a host pty replica as the daemon's TTY exec does.
func (s Subject) launch(t *testing.T, id string, spec models.ExecSpec, tty bool) (models.ExitStatus, string, error) {
	t.Helper()

	if !tty {
		out, err := os.CreateTemp(t.TempDir(), "exec-output")
		if err != nil {
			t.Fatalf("create a file for the exec output: %v", err)
		}
		defer func() {
			if err := out.Close(); err != nil {
				t.Errorf("close the exec output file: %v", err)
			}
		}()

		spec.Stdout, spec.Stderr = out, out
		status, err := s.Provider.Exec(t.Context(), id, spec)
		written, rerr := os.ReadFile(out.Name())
		if rerr != nil {
			t.Fatalf("read the exec output: %v", rerr)
		}

		return status, string(written), err
	}

	p, err := pty.Open()
	if err != nil {
		t.Fatalf("open a pty: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("close the pty: %v", err)
		}
	}()

	spec.TTY = true
	spec.Stdin, spec.Stdout, spec.Stderr = p.Replica, p.Replica, p.Replica
	status, err := s.Provider.Exec(t.Context(), id, spec)

	return status, "", err
}

func terminalName(name string, tty bool) string {
	if tty {
		return name + "OnATerminal"
	}

	return name
}

// reports collects what Report was called with, which a provider may call from any goroutine.
type reports struct {
	mu   sync.Mutex
	pids []int
}

func (r *reports) add(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pids = append(r.pids, pid)
}

func (r *reports) all() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]int(nil), r.pids...)
}
