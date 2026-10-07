package supervisor

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// fakeInit stands in for the provider's exec of `/.shard/init process`: it reads the request off stdin and answers as guest says.
func fakeInit(t *testing.T, guest func(m Message, spec models.ExecSpec) models.ExitStatus) ExecFunc {
	return func(_ context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		var m Message
		if err := ReadHeader(spec.Stdin, &m); err != nil {
			return models.ExitStatus{}, err
		}

		return guest(m, spec), nil
	}
}

func answer(t *testing.T, spec models.ExecSpec, reply Message) {
	t.Helper()
	if err := WriteMessage(spec.Stdout, reply); err != nil {
		t.Errorf("answer: %v", err)
	}
}

func TestRunProcessRunsTheProcessModeAsRoot(t *testing.T) {
	var got models.ExecSpec
	var sent Message
	run := fakeInit(t, func(m Message, spec models.ExecSpec) models.ExitStatus {
		got, sent = spec, m
		answer(t, spec, Message{Kind: KindDone})

		return models.ExitStatus{}
	})

	if err := RunProcess(t.Context(), run, RunSpec{Name: "web", Argv: []string{"/srv/web"}, Restart: models.RestartAlways}); err != nil {
		t.Fatalf("RunProcess: %v", err)
	}
	if strings.Join(got.Argv, " ") != "/.shard/init process" || got.User != "0:0" || got.WorkDir != "/" {
		t.Errorf("the exec ran %+v, want /.shard/init process as 0:0 in /", got)
	}
	if sent.Kind != KindRun || sent.Run == nil || sent.Run.Name != "web" || sent.Run.Restart != models.RestartAlways {
		t.Errorf("the guest read %+v, want the run of web", sent)
	}
}

// A stop waits out its grace in the guest, so the exec's bound covers the grace and the run's own.
func TestStopProcessSendsTheNameAndBoundsTheExecPastTheGrace(t *testing.T) {
	const grace = 2 * time.Minute
	var sent Message
	var left time.Duration
	run := func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		deadline, _ := ctx.Deadline()
		left = time.Until(deadline)

		return fakeInit(t, func(m Message, spec models.ExecSpec) models.ExitStatus {
			sent = m
			answer(t, spec, Message{Kind: KindDone})

			return models.ExitStatus{}
		})(ctx, spec)
	}

	if err := StopProcess(t.Context(), run, "web", grace); err != nil {
		t.Fatalf("StopProcess: %v", err)
	}
	if sent.Kind != KindStopProcess || sent.Name != "web" || sent.Grace != grace {
		t.Errorf("the guest read %+v, want the stop of web with its grace", sent)
	}
	if left <= grace {
		t.Errorf("the exec had %s, want more than the %s grace", left, grace)
	}
}

// The provider maps each guest answer through ProcessError, as it does a VM's.
func TestEachGuestAnswerMapsToItsError(t *testing.T) {
	cases := map[string]struct {
		reply Message
		check func(error) bool
	}{
		"a name that still runs": {
			reply: Message{Kind: KindFailure, Error: `"web": a process of that name still runs`, Taken: true},
			check: func(err error) bool { return errors.Is(err, models.ErrProcessRunning) },
		},
		"a command not found": {
			reply: Message{Kind: KindFailure, Error: `"/missing": no such file or directory`, Code: 127},
			check: func(err error) bool {
				refused, ok := errors.AsType[*models.CommandNotStartedError](err)
				return ok && refused.Code == 127 && refused.Sandbox == "sb" && strings.Contains(refused.Reason, "no such file")
			},
		},
		"a command that cannot run": {
			reply: Message{Kind: KindFailure, Error: `"/etc": permission denied`, Code: 126},
			check: func(err error) bool {
				refused, ok := errors.AsType[*models.CommandNotStartedError](err)
				return ok && refused.Code == 126
			},
		},
		"a PID 1 that predates the socket": {
			reply: Message{Kind: KindFailure, Error: "stop and start the sandbox", Outdated: true},
			check: func(err error) bool {
				old, ok := errors.AsType[*models.SupervisorTooOldError](err)
				return ok && old.Sandbox == "sb"
			},
		},
		"any other refusal carries the guest's words": {
			reply: Message{Kind: KindFailure, Error: "the sandbox is stopping"},
			check: func(err error) bool {
				_, old := errors.AsType[*models.SupervisorTooOldError](err)
				return !old && !errors.Is(err, models.ErrProcessRunning) && strings.Contains(err.Error(), "the sandbox is stopping")
			},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			run := fakeInit(t, func(_ Message, spec models.ExecSpec) models.ExitStatus {
				answer(t, spec, c.reply)

				return models.ExitStatus{}
			})

			err := ProcessError("sb", RunProcess(t.Context(), run, RunSpec{Name: "web", Argv: []string{"/missing"}}))
			if err == nil || !c.check(err) {
				t.Fatalf("RunProcess gave %v", err)
			}
		})
	}
}

// A shard-init from before named processes takes "process" for a bad flag and exits 1 before it reads a byte, even of a request past the pipe buffer.
func TestAnInitThatPrintsNoAnswerIsTooOld(t *testing.T) {
	run := func(_ context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		if _, err := spec.Stderr.WriteString("shard-init: -ready-file is required\n"); err != nil {
			t.Errorf("write stderr: %v", err)
		}

		return models.ExitStatus{Code: 1}, nil
	}
	env := []string{"BIG=" + strings.Repeat("x", 512<<10)}

	err := ProcessError("sb", RunProcess(t.Context(), run, RunSpec{Name: "web", Argv: []string{"/srv/web"}, Env: env}))
	if old, ok := errors.AsType[*models.SupervisorTooOldError](err); !ok || old.Sandbox != "sb" {
		t.Fatalf("RunProcess gave %v, want SupervisorTooOldError", err)
	}
}

func TestAnExecThatFailsOrAnswersNothingIsAnError(t *testing.T) {
	cases := map[string]ExecFunc{
		"the exec does not start": func(context.Context, models.ExecSpec) (models.ExitStatus, error) {
			return models.ExitStatus{}, errors.New("the sandbox is not running")
		},
		"a clean exit with no answer": fakeInit(t, func(Message, models.ExecSpec) models.ExitStatus {
			return models.ExitStatus{}
		}),
		"an answer that is not JSON": fakeInit(t, func(_ Message, spec models.ExecSpec) models.ExitStatus {
			if _, err := spec.Stdout.WriteString("not json\n"); err != nil {
				t.Errorf("write stdout: %v", err)
			}

			return models.ExitStatus{}
		}),
		"an answer of another kind": fakeInit(t, func(_ Message, spec models.ExecSpec) models.ExitStatus {
			answer(t, spec, Message{Kind: KindProcess})

			return models.ExitStatus{}
		}),
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err := RunProcess(t.Context(), run, RunSpec{Name: "web", Argv: []string{"/srv/web"}})
			if err == nil {
				t.Fatal("RunProcess succeeded")
			}
			if _, refused := errors.AsType[*Refusal](err); refused {
				t.Errorf("RunProcess gave the refusal %v, want a plain error", err)
			}
		})
	}
}

// A stuck guest must not hold the verb: the bound ends the exec, and a guest process that kept the pipes ends the reads.
func TestAStuckGuestEndsAtTheBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	cases := map[string]ExecFunc{
		"an exec that never returns until killed": func(ctx context.Context, _ models.ExecSpec) (models.ExitStatus, error) {
			<-ctx.Done()

			return models.ExitStatus{Signal: int(syscall.SIGKILL)}, ctx.Err()
		},
		"a guest process that keeps stdout open": fakeInit(t, func(_ Message, spec models.ExecSpec) models.ExitStatus {
			kept := dupFile(t, spec.Stdout)
			t.Cleanup(func() {
				if err := kept.Close(); err != nil {
					t.Errorf("close the kept stdout: %v", err)
				}
			})

			return models.ExitStatus{}
		}),
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := requestProcess(t.Context(), run, Message{Kind: KindRun, Run: &RunSpec{Name: "web"}}, bound)
			if err == nil {
				t.Fatal("the request succeeded")
			}
			if took := time.Since(start); took > 10*bound {
				t.Errorf("the request took %s, want it ended near the %s bound", took, bound)
			}
		})
	}
}

// A guest that floods stdout costs a line's bound of memory, and its drained exec ends.
func TestAFloodOnStdoutIsAnError(t *testing.T) {
	run := fakeInit(t, func(_ Message, spec models.ExecSpec) models.ExitStatus {
		if _, err := spec.Stdout.WriteString(strings.Repeat("x", 4*MaxPayload)); err != nil {
			t.Errorf("flood stdout: %v", err)
		}

		return models.ExitStatus{}
	})

	err := RunProcess(t.Context(), run, RunSpec{Name: "web", Argv: []string{"/srv/web"}})
	if err == nil || !strings.Contains(err.Error(), "answered") {
		t.Fatalf("RunProcess gave %v, want the answer refused", err)
	}
}

func dupFile(t *testing.T, f *os.File) *os.File {
	t.Helper()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}

	return os.NewFile(uintptr(fd), f.Name())
}
