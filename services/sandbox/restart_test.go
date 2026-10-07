package sandbox_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestRunFillsTheRestartPolicy(t *testing.T) {
	cases := []struct {
		name string
		req  *models.RestartSpec
		want models.RestartSpec
	}{
		{"none named", nil, models.RestartSpec{Policy: models.RestartUnlessStopped, Backoff: sandbox.DefaultRestartBackoff}},
		{"no", &models.RestartSpec{Policy: models.RestartNo}, models.RestartSpec{Policy: models.RestartNo}},
		{"always", &models.RestartSpec{Policy: models.RestartAlways}, models.RestartSpec{Policy: models.RestartAlways, Backoff: sandbox.DefaultRestartBackoff}},
		{"on-failure as named", &models.RestartSpec{Policy: models.RestartOnFailure, Retries: 3, Backoff: 4}, models.RestartSpec{Policy: models.RestartOnFailure, Retries: 3, Backoff: 4}},
	}
	for _, c := range cases {
		svc, l := newService(t, &recorder{}, running())

		p, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"web"}, Restart: c.req})
		if err != nil {
			t.Fatalf("%s: run: %v", c.name, err)
		}

		if p.Restart != c.want || l.repo.sb.Processes[0].Restart != c.want || l.provider.specs[0].Restart != c.want {
			t.Errorf("%s: the process holds %+v and the guest got %+v, want %+v", c.name, p.Restart, l.provider.specs[0].Restart, c.want)
		}
	}
}

func TestRunRefusesARestartPolicyNoSupervisorTakes(t *testing.T) {
	cases := map[string]models.RestartSpec{
		"an unknown policy":         {Policy: "sometimes"},
		"retries under always":      {Policy: models.RestartAlways, Retries: 3},
		"retries under unless":      {Policy: models.RestartUnlessStopped, Retries: 3},
		"negative retries":          {Policy: models.RestartOnFailure, Retries: -1},
		"negative backoff":          {Policy: models.RestartOnFailure, Backoff: -1},
		"a backoff past the cap":    {Policy: models.RestartAlways, Backoff: models.RestartBackoffCap + 1},
		"a backoff that never runs": {Policy: models.RestartNo, Backoff: 2},
	}
	for name, spec := range cases {
		r := &recorder{}
		svc, _ := newService(t, r, running())

		_, err := svc.Run(t.Context(), "sandbox1", sandbox.RunRequest{Name: "web", Command: []string{"web"}, Restart: &spec})

		if !errors.As(err, new(*sandbox.RequestError)) {
			t.Errorf("%s: run returned %v, want a request error", name, err)
		}
		if len(keep(r.calls, "repo.Update", "provider.StartProcess")) != 0 {
			t.Errorf("%s: a refused run reached the record or the guest: %v", name, r.calls)
		}
	}
}

// tickLab drives the process tick over one record, and keeps what it reported.
type tickLab struct {
	svc     *sandbox.Service
	l       layers
	r       *recorder
	reports []string
}

func newTickLab(t *testing.T, sb models.Sandbox) *tickLab {
	t.Helper()

	lab := &tickLab{r: &recorder{}}
	lab.svc, lab.l = newService(t, lab.r, sb)

	return lab
}

func (lab *tickLab) tick(t *testing.T) {
	t.Helper()

	err := lab.svc.RecordProcesses(t.Context(), []models.Sandbox{lab.l.repo.sb}, func(line string) { lab.reports = append(lab.reports, line) })
	if err != nil {
		t.Fatalf("RecordProcesses: %v", err)
	}
}

// guest says what shard-init's table holds for one process from now on.
func (lab *tickLab) guest(name string, status models.ProcessStatus) {
	lab.l.provider.report(models.ProcessReport{Name: name, ProcessStatus: status})
}

func onFailure(name string, retries int) models.Sandbox {
	sb := running()
	p := proc(name, models.RestartOnFailure)
	p.Restart.Retries, p.Restart.Backoff = retries, 1
	sb.Processes = []models.Process{p}

	return sb
}

func TestRecordProcessesCopiesTheTableAndReportsEachMove(t *testing.T) {
	lab := newTickLab(t, onFailure("web", 2))
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	lab.guest("web", models.ProcessStatus{State: models.ProcessRunning, Restarts: 1, Exit: &models.ExitStatus{Code: 1}, StartedAt: at})
	lab.tick(t)
	if got := lab.l.repo.sb.Processes[0].Status; got.Restarts != 1 || !got.StartedAt.Equal(at) {
		t.Errorf("the record reads %+v, want 1 start again at %s", got, at)
	}
	if want := []string{"sandbox sandbox1: process web was started again, 1 of 2"}; !slices.Equal(lab.reports, want) {
		t.Errorf("the tick reported %v, want %v", lab.reports, want)
	}

	// The same table again is no news, and the record is not written for it.
	lab.reports = nil
	updates := len(keep(lab.r.calls, "repo.Update"))
	lab.tick(t)
	if len(lab.reports) != 0 || len(keep(lab.r.calls, "repo.Update")) != updates {
		t.Errorf("a tick with nothing new reported %v and wrote the record", lab.reports)
	}

	lab.guest("web", models.ProcessStatus{State: models.ProcessGaveUp, Restarts: 2, Exit: &models.ExitStatus{Code: 1}, StartedAt: at.Add(time.Minute)})
	lab.tick(t)
	want := []string{
		"sandbox sandbox1: process web was started again, 2 of 2",
		"sandbox sandbox1: process web exited again and the 2 restarts its policy allows are spent",
	}
	if !slices.Equal(lab.reports, want) || lab.l.repo.sb.Processes[0].Status.State != models.ProcessGaveUp {
		t.Errorf("the give-up reported %v and recorded %+v, want %v and the give-up", lab.reports, lab.l.repo.sb.Processes[0].Status, want)
	}
}

// The reset window can zero the count, so a start that lands on the same number is told by its fresh start time.
func TestRecordProcessesReportsAFreshStartTheCountDoesNotShow(t *testing.T) {
	t1 := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	sb := running()
	p := proc("web", models.RestartAlways)
	p.Status.Restarts, p.Status.StartedAt = 1, t1
	sb.Processes = []models.Process{p}
	lab := newTickLab(t, sb)

	t2 := t1.Add(time.Minute)
	lab.guest("web", models.ProcessStatus{State: models.ProcessRunning, Restarts: 1, StartedAt: t2})
	lab.tick(t)

	if got := lab.l.repo.sb.Processes[0].Status; !got.StartedAt.Equal(t2) {
		t.Errorf("the record reads %+v, want the start stamped at the fresh %s", got, t2)
	}
	// always has no limit, so the line names none.
	if want := []string{"sandbox sandbox1: process web was started again, 1"}; !slices.Equal(lab.reports, want) {
		t.Errorf("a fresh start the count did not move reported %v, want %v", lab.reports, want)
	}
}

func TestRecordProcessesReportsAnEndOnce(t *testing.T) {
	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartNo)}
	lab := newTickLab(t, sb)

	lab.guest("web", models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{Code: 7}})
	lab.tick(t)
	lab.tick(t)

	if got := lab.l.repo.sb; got.State != models.StateRunning || got.Processes[0].Status.State != models.ProcessExited {
		t.Errorf("the record says %s with %+v, want running with web exited", got.State, got.Processes[0].Status)
	}
	if want := []string{"sandbox sandbox1: process web is exited (code 7, signal 0)"}; !slices.Equal(lab.reports, want) {
		t.Errorf("two ticks reported %v, want %v", lab.reports, want)
	}
}

// Guest root can write any name into the table, and only a name the record ran may land on it.
func TestRecordProcessesDropsANameTheRecordNeverRan(t *testing.T) {
	sb := running()
	sb.Processes = []models.Process{proc("web", models.RestartNo)}
	lab := newTickLab(t, sb)

	lab.guest("forged", models.ProcessStatus{State: models.ProcessRunning})
	lab.tick(t)

	if got := lab.l.repo.sb.Processes; len(got) != 1 || got[0].Name != "web" {
		t.Errorf("the record holds %+v, want web alone", got)
	}
}

func TestRecordProcessesLeavesARecordThatRunsNothingOrIsNotRunning(t *testing.T) {
	for name, sb := range map[string]models.Sandbox{"no process": running(), "stopped": stopped()} {
		lab := newTickLab(t, sb)
		reads := 0
		lab.l.provider.onTable = func() { reads++ }

		lab.tick(t)

		if reads != 0 || len(keep(lab.r.calls, "repo.Update")) != 0 {
			t.Errorf("%s: the tick read the table %d times and wrote %v", name, reads, lab.r.calls)
		}
	}
}

func TestRecordProcessesReportsATableItCannotRead(t *testing.T) {
	sb := onFailure("web", 2)
	svc, l := newService(t, &recorder{}, sb)
	l.provider.tableErr = errors.New("read the process table: input/output error")

	if err := svc.RecordProcesses(t.Context(), []models.Sandbox{sb}, func(string) {}); err == nil {
		t.Fatal("a table that cannot be read returned no error")
	}
	if !reflect.DeepEqual(l.repo.sb.Processes, sb.Processes) {
		t.Errorf("the record holds %+v after a failed read", l.repo.sb.Processes)
	}
}

// Guest root can put its own file on PID 1's fd 0; inspect names it once, and a later good read clears it (SHARD-419).
func TestRecordProcessesNamesAReplacedExitChannelOnceAndClearsIt(t *testing.T) {
	lab := newTickLab(t, onFailure("web", 2))
	lab.l.provider.tableErr = fmt.Errorf("fd 0 of PID 1 is not a regular file: %w", models.ErrExitChannelReplaced)

	lab.tick(t)
	lab.tick(t)
	if got := lab.l.repo.sb; got.State != models.StateRunning || !strings.Contains(got.ExitChannel, "exit channel replaced") {
		t.Errorf("the record says %s with exit channel %q, want running and the channel named replaced", got.State, got.ExitChannel)
	}
	if len(lab.reports) != 1 {
		t.Errorf("two ticks reported %v, want one line", lab.reports)
	}

	lab.l.provider.tableErr = nil
	lab.tick(t)
	if got := lab.l.repo.sb.ExitChannel; got != "" {
		t.Errorf("a good read left exit channel %q on the record", got)
	}
}

// everyPolicy is a stopped sandbox an operator stopped, with one process under each policy and a kill on two.
func everyPolicy() models.Sandbox {
	sb := stopped()
	sb.StoppedByOperator = true
	killedAlways := proc("always-killed", models.RestartAlways)
	killedAlways.Killed = true
	killedUnless := proc("unless-killed", models.RestartUnlessStopped)
	killedUnless.Killed = true
	sb.Processes = []models.Process{
		proc("always", models.RestartAlways), killedAlways,
		proc("unless", models.RestartUnlessStopped), killedUnless,
		proc("on-failure", models.RestartOnFailure), proc("no", models.RestartNo),
	}
	for i := range sb.Processes {
		sb.Processes[i].Status.State = models.ProcessStopped
	}

	return sb
}

// An operator's start brings back always and unless-stopped, by Docker's rules, and a kill keeps unless-stopped down.
func TestStartBringsBackWhatThePoliciesSay(t *testing.T) {
	svc, l := newService(t, &recorder{}, everyPolicy())

	sb, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	started := specNames(l.provider.specs)
	if want := []string{"always", "always-killed", "unless"}; !slices.Equal(started, want) {
		t.Errorf("the start ran %v, want %v", started, want)
	}
	if sb.StoppedByOperator {
		t.Error("an operator's start kept the mark of the operator's stop")
	}
	for _, p := range sb.Processes {
		want := models.ProcessStopped
		if slices.Contains(started, p.Name) {
			want = models.ProcessRunning
		}
		if p.Status.State != want {
			t.Errorf("%s reads %s after the start, want %s", p.Name, p.Status.State, want)
		}
	}
}

// The sandbox runs whatever its processes do, so one that did not start is recorded and reported, and the rest still start.
func TestStartRecordsAProcessThatDidNotStartAndRunsOn(t *testing.T) {
	sb := stopped()
	sb.Processes = []models.Process{proc("api", models.RestartAlways), proc("web", models.RestartAlways)}
	var reports []string
	svc, l := newService(t, &recorder{}, sb, func(cfg *sandbox.Config) {
		cfg.Report = func(line string) { reports = append(reports, line) }
	})
	l.provider.startProcessErr = map[string]error{"api": &models.CommandNotStartedError{Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}}

	got, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	api, web := got.Processes[0].Status, got.Processes[1].Status
	if got.State != models.StateRunning || api.State != models.ProcessExited || api.Exit == nil || api.Exit.Code != models.CommandNotFoundExitCode || web.State != models.ProcessRunning {
		t.Errorf("the record says %s with api %+v and web %+v, want running, api exited 127 and web running", got.State, api, web)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "process api did not start again") {
		t.Errorf("the start reported %v, want one line on api", reports)
	}
}
