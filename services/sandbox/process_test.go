package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// refusedWith fails the test unless err is a state refusal carrying code.
func refusedWith(t *testing.T, err error, code models.Code) {
	t.Helper()

	refused, ok := errors.AsType[*sandbox.StateError](err)
	if !ok || refused.Code != code {
		t.Errorf("got %v, want the refusal %s", err, code)
	}
}

func writeProcessLog(t *testing.T, l layers, name, text string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(l.provider.logDir, name+".log"), []byte(text), 0o600); err != nil {
		t.Fatalf("write the log of %s: %v", name, err)
	}
}

func running16(ended ...int) models.Sandbox {
	sb := running()
	for i := range models.MaxProcesses {
		p := proc(fmt.Sprintf("p%d", i), models.RestartNo)
		if slices.Contains(ended, i) {
			p.Status.State = models.ProcessExited
		}
		sb.Processes = append(sb.Processes, p)
	}

	return sb
}

func names(procs []models.Process) []string {
	var out []string
	for _, p := range procs {
		out = append(out, p.Name)
	}

	return out
}

// The record names the process before the guest starts it, so a daemon that dies between the two still owns the name.
func TestRunRecordsTheProcessThenHandsItToTheGuest(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, running())
	writeProcessLog(t, l, "web", "a run before\n")

	p, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"/srv/web", "--port", "80"}, Env: []string{"A=1"}, WorkDir: "/srv", User: "app"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := keep(r.calls, "repo.Update", "provider.StartProcess"); !slices.Equal(got, []string{"repo.Update", "provider.StartProcess"}) {
		t.Errorf("the run made %v, want the record written before the guest starts it", got)
	}
	want := models.ProcessSpec{Name: "web", Argv: []string{"/srv/web", "--port", "80"}, Env: []string{"A=1"}, WorkDir: "/srv", User: "app", Restart: p.Restart}
	if len(l.provider.specs) != 1 || !reflect.DeepEqual(l.provider.specs[0], want) {
		t.Errorf("the guest got %+v, want %+v", l.provider.specs, want)
	}
	rec := l.repo.sb
	if len(rec.Processes) != 1 || rec.Processes[0].Name != "web" || rec.Processes[0].Status.State != models.ProcessRunning || rec.Processes[0].Status.StartedAt.IsZero() {
		t.Errorf("the record holds %+v, want web running with its start time", rec.Processes)
	}
	// The log outlives each run, so this run's output begins where the last one's ended.
	if got := rec.LogStarts["web"]; got != int64(len("a run before\n")) {
		t.Errorf("the run begins at byte %d of its log, want the end of the run before", got)
	}
}

func TestRunNamesAProcessAfterItsCommand(t *testing.T) {
	svc, _ := newService(t, &recorder{}, running())

	p, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Command: []string{"/usr/bin/python3", "app.py"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if p.Name != "python3" {
		t.Errorf("the process is named %q, want python3", p.Name)
	}
}

func TestRunRefusesARequestNoGuestCouldStart(t *testing.T) {
	cases := map[string]sandbox.RunRequest{
		"no command":                   {Name: "web"},
		"an uppercase name":            {Name: "Web", Command: []string{"web"}},
		"a name too long":              {Name: strings.Repeat("a", models.MaxProcessName+1), Command: []string{"web"}},
		"a command with no name":       {Command: []string{"/"}},
		"an env that is no assignment": {Name: "web", Command: []string{"web"}, Env: []string{"FOO"}},
	}
	for name, req := range cases {
		r := &recorder{}
		svc, _ := newService(t, r, running())

		_, err := svc.Run(t.Context(), "sandbox1", req)

		if !errors.As(err, new(*sandbox.RequestError)) {
			t.Errorf("%s: run returned %v, want a request error", name, err)
		}
		if got := keep(r.calls, "repo.Update", "provider.StartProcess"); len(got) != 0 {
			t.Errorf("%s: a refused run made %v", name, got)
		}
	}
}

func TestRunRefusesANameThatStillRuns(t *testing.T) {
	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartNo)}
	r := &recorder{}
	svc, _ := newService(t, r, sb)

	_, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"web"}})

	refusedWith(t, err, models.CodeNameTaken)
	if got := keep(r.calls, "repo.Update", "provider.StartProcess"); len(got) != 0 {
		t.Errorf("a taken name made %v", got)
	}
}

func TestRunReplacesAnEndedProcessOfTheSameName(t *testing.T) {
	sb := running()
	sb.Processes = []models.Process{exitedProcess(), proc("web", models.RestartNo)}
	svc, l := newService(t, &recorder{}, sb)

	if _, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "job", Command: []string{"job"}}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := l.repo.sb.Processes
	if !slices.Equal(names(got), []string{"web", "job"}) || got[1].Status.State != models.ProcessRunning || got[1].Status.Exit != nil {
		t.Errorf("the record holds %+v, want web, then job running with no exit", got)
	}
}

func TestRunEvictsTheOldestEndedProcessWhenTheSandboxIsFull(t *testing.T) {
	svc, l := newService(t, &recorder{}, running16(3, 9))

	if _, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "new", Command: []string{"new"}}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := names(l.repo.sb.Processes)
	if len(got) != models.MaxProcesses || slices.Contains(got, "p3") || !slices.Contains(got, "p9") || got[len(got)-1] != "new" {
		t.Errorf("the record holds %v, want p3 gone, p9 kept and new last", got)
	}
}

func TestRunRefusesASandboxWhoseProcessesAllRun(t *testing.T) {
	r := &recorder{}
	svc, _ := newService(t, r, running16())

	_, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "new", Command: []string{"new"}})

	refusedWith(t, err, models.CodeProcessLimit)
	if got := keep(r.calls, "repo.Update", "provider.StartProcess"); len(got) != 0 {
		t.Errorf("a full sandbox made %v", got)
	}
}

func TestRunRefusesASandboxThatDoesNotRun(t *testing.T) {
	for _, sb := range []models.Sandbox{stopped(), pausedSandbox()} {
		svc, _ := newService(t, &recorder{}, sb)

		_, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"web"}})

		refusedWith(t, err, models.CodeSandboxNotRunning)
	}
}

// A command the guest could not start is an exit the record keeps, as an exec of it would answer.
func TestRunRecordsACommandThatDidNotStartAsExited(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.provider.startProcessErr = map[string]error{"web": &models.CommandNotStartedError{Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}}

	p, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"/srv/missing"}})

	refused, ok := errors.AsType[*models.CommandNotStartedError](err)
	if !ok || refused.Command != "/srv/missing" || refused.Sandbox != "sandbox1" {
		t.Errorf("run returned %v, want the command and the sandbox named", err)
	}
	for _, got := range []models.Process{p, l.repo.sb.Processes[0]} {
		if got.Status.State != models.ProcessExited || got.Status.Exit == nil || got.Status.Exit.Code != models.CommandNotFoundExitCode {
			t.Errorf("the process reads %+v, want exited 127", got.Status)
		}
	}
}

// A guest that kept the name or cannot run named processes started nothing, so the record goes back as it was.
func TestRunPutsTheRecordBackWhenTheGuestStartedNothing(t *testing.T) {
	cases := map[string]struct {
		err     error
		refused func(error) bool
	}{
		"a name the guest still runs": {fmt.Errorf("process web: %w", models.ErrProcessRunning), func(err error) bool {
			refused, ok := errors.AsType[*sandbox.StateError](err)
			return ok && refused.Code == models.CodeNameTaken
		}},
		"a guest too old": {fmt.Errorf("named processes: %w", models.ErrUnsupported), func(err error) bool { return errors.Is(err, models.ErrUnsupported) }},
	}
	for name, c := range cases {
		sb := running()
		ended := proc("web", models.RestartNo)
		ended.Status.State = models.ProcessExited
		sb.Processes = []models.Process{ended}
		sb.LogStarts = map[string]int64{"web": 4}
		svc, l := newService(t, &recorder{}, sb)
		writeProcessLog(t, l, "web", "a longer log\n")
		l.provider.startProcessErr = map[string]error{"": c.err}

		_, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"web"}})

		if !c.refused(err) {
			t.Errorf("%s: run returned %v, want the guest's refusal", name, err)
		}
		if !reflect.DeepEqual(l.repo.sb.Processes, sb.Processes) || !reflect.DeepEqual(l.repo.sb.LogStarts, sb.LogStarts) {
			t.Errorf("%s: the record holds %+v and %v, want them as before the run", name, l.repo.sb.Processes, l.repo.sb.LogStarts)
		}
	}
}

func TestProcessesLaysTheLatestReportOverTheRecord(t *testing.T) {
	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartAlways), exitedProcess()}
	svc, l := newService(t, &recorder{}, sb)
	l.provider.table = []models.ProcessReport{
		{Name: "web", Seq: 3, ProcessStatus: models.ProcessStatus{State: models.ProcessRestarting, Restarts: 1}},
		{Name: "web", Seq: 2, ProcessStatus: models.ProcessStatus{State: models.ProcessRunning}},
		{Name: "forged", Seq: 4, ProcessStatus: models.ProcessStatus{State: models.ProcessRunning}},
	}

	got, err := svc.Processes(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("processes: %v", err)
	}

	if !slices.Equal(names(got), []string{"web", "job"}) || got[0].Status.State != models.ProcessRestarting || got[1].Status.State != models.ProcessExited {
		t.Errorf("processes answered %+v, want web restarting by its latest report, job as recorded, and nothing forged", got)
	}
}

// A sandbox that does not run has no table, so the record is the answer.
func TestProcessesOfAStoppedSandboxReadsTheRecordAlone(t *testing.T) {
	svc, l := newService(t, &recorder{}, stopped())
	reads := 0
	l.provider.onTable = func() { reads++ }

	got, err := svc.Processes(t.Context(), "web")
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if reads != 0 || !reflect.DeepEqual(got, stopped().Processes) {
		t.Errorf("processes read the table %d times and answered %+v, want the record alone", reads, got)
	}
}

func TestProcessRefusesANameTheSandboxNeverRan(t *testing.T) {
	svc, _ := newService(t, &recorder{}, running())

	_, err := svc.Process(t.Context(), "sandbox1", "web")

	refusedWith(t, err, models.CodeNoProcess)
}

// killable is a running sandbox whose guest runs web and once ran job.
func killable(t *testing.T, r *recorder) (*sandbox.Service, layers) {
	t.Helper()

	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartAlways), exitedProcess()}
	svc, l := newService(t, r, sb)
	l.provider.report(models.ProcessReport{Name: "web", ProcessStatus: models.ProcessStatus{State: models.ProcessRunning}})
	l.provider.report(models.ProcessReport{Name: "job", ProcessStatus: exitedProcess().Status})

	return svc, l
}

func TestKillEndsTheProcessAndMarksIt(t *testing.T) {
	for force, grace := range map[bool]string{false: "web:30s", true: "web:0s"} {
		svc, l := killable(t, &recorder{})

		p, err := svc.Kill(t.Context(), "sandbox1", "web", force)
		if err != nil {
			t.Fatalf("kill: %v", err)
		}

		if !slices.Equal(l.provider.killed, []string{grace}) {
			t.Errorf("force %t stopped %v, want %s", force, l.provider.killed, grace)
		}
		rec := l.repo.sb.Processes[0]
		if !p.Killed || p.Status.State != models.ProcessKilled || !rec.Killed || rec.Status.State != models.ProcessKilled {
			t.Errorf("force %t: the kill answered %+v and recorded %+v, want both killed and marked", force, p, rec)
		}
		if l.repo.sb.State != models.StateRunning {
			t.Errorf("the sandbox reads %s after a kill, want running", l.repo.sb.State)
		}
	}
}

// A process its policy already ended keeps its own end, and only takes the mark.
func TestKillOfAnEndedProcessKeepsItsEnd(t *testing.T) {
	svc, l := killable(t, &recorder{})

	p, err := svc.Kill(t.Context(), "sandbox1", "job", false)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	for _, got := range []models.Process{p, l.repo.sb.Processes[1]} {
		if !got.Killed || got.Status.State != models.ProcessExited || got.Status.Exit == nil || got.Status.Exit.Code != 3 {
			t.Errorf("job reads %+v after the kill, want it marked and still exited 3", got)
		}
	}
}

func TestKillRefusesANameTheSandboxNeverRan(t *testing.T) {
	r := &recorder{}
	svc, _ := killable(t, r)

	_, err := svc.Kill(t.Context(), "sandbox1", "nope", false)

	refusedWith(t, err, models.CodeNoProcess)
	if got := keep(r.calls, "provider.StopProcess", "repo.Update"); len(got) != 0 {
		t.Errorf("a kill of no process made %v", got)
	}
}

// Nothing runs in a stopped sandbox, so the kill is the mark, which keeps an unless-stopped process down on the next start.
func TestKillOfAStoppedSandboxKeepsTheProcessDownOnTheNextStart(t *testing.T) {
	sb := stopped()
	web := proc("web", models.RestartUnlessStopped)
	web.Status.State = models.ProcessStopped
	sb.Processes = []models.Process{web}
	r := &recorder{}
	svc, l := newService(t, r, sb)

	p, err := svc.Kill(t.Context(), "web", "web", false)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !p.Killed || p.Status.State != models.ProcessStopped || len(keep(r.calls, "provider.StopProcess")) != 0 {
		t.Errorf("the kill answered %+v after %v, want the mark alone", p, r.calls)
	}

	if _, err := svc.Start(t.Context(), "web"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(l.provider.specs) != 0 {
		t.Errorf("the start ran %+v, want the killed process kept down", l.provider.specs)
	}
}

// attachableProcess is a running sandbox whose web wrote a run before this one, which the attach must not replay.
func attachableProcess(t *testing.T) (*sandbox.Service, layers) {
	t.Helper()

	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartNo)}
	sb.LogStarts = map[string]int64{"web": int64(len("old run\n"))}
	svc, l := newService(t, &recorder{}, sb)
	writeProcessLog(t, l, "web", "old run\nthis run\n")

	return svc, l
}

func TestAttachProcessCopiesThisRunAndAnswersItsEnd(t *testing.T) {
	svc, l := attachableProcess(t)
	l.provider.report(models.ProcessReport{Name: "web", ProcessStatus: models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{Code: 4}}})
	var out bytes.Buffer

	p, err := svc.AttachProcess(t.Context(), "sandbox1", "web", func() (io.Writer, error) { return &out, nil })
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	if out.String() != "this run\n" {
		t.Errorf("the attach copied %q, want this run's output alone", out.String())
	}
	if p.Status.State != models.ProcessExited || p.Status.Exit == nil || p.Status.Exit.Code != 4 {
		t.Errorf("the attach answered %+v, want exited 4", p.Status)
	}
	// A ps right after the attach reads the record, so the end is written before the answer.
	if got := l.repo.sb.Processes[0].Status.State; got != models.ProcessExited {
		t.Errorf("the record reads %s after the attach, want exited", got)
	}
}

func TestAttachProcessRefusesBeforeItOpens(t *testing.T) {
	svc, _ := attachableProcess(t)
	opened := false

	_, err := svc.AttachProcess(t.Context(), "sandbox1", "nope", func() (io.Writer, error) {
		opened = true

		return io.Discard, nil
	})

	refusedWith(t, err, models.CodeNoProcess)
	if opened {
		t.Error("the attach opened its stream before the refusal")
	}
}

func TestAttachProcessEndsWithTheSandbox(t *testing.T) {
	svc, l := attachableProcess(t)
	l.provider.exits = func() {}

	_, err := svc.AttachProcess(t.Context(), "sandbox1", "web", func() (io.Writer, error) { return io.Discard, nil })

	refusedWith(t, err, models.CodeSandboxNotRunning)
}

func TestAttachProcessLeavesOnAnInterrupt(t *testing.T) {
	svc, _ := attachableProcess(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := svc.AttachProcess(ctx, "sandbox1", "web", func() (io.Writer, error) { return io.Discard, nil })

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the attach returned %v, want the interrupt", err)
	}
}
