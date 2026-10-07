package sandbox_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// withProcesses is sb running the given processes, each stopped when sb is.
func withProcesses(sb models.Sandbox, procs ...models.Process) models.Sandbox {
	sb.Processes = procs
	for i := range sb.Processes {
		if sb.State != models.StateRunning {
			sb.Processes[i].Status.State = models.ProcessStopped
		}
	}

	return sb
}

func TestOwedNamesASandboxADaemonStartHasWorkIn(t *testing.T) {
	byOperator := withProcesses(stopped(), proc("web", models.RestartUnlessStopped))
	byOperator.StoppedByOperator = true
	paused := withProcesses(pausedSandbox(), proc("web", models.RestartAlways))
	created := withProcesses(models.Sandbox{ID: "sandbox1", State: models.StateCreated}, proc("web", models.RestartAlways))

	cases := map[string]struct {
		sb   models.Sandbox
		want bool
	}{
		"stopped under always":                     {withProcesses(stopped(), proc("web", models.RestartAlways)), true},
		"stopped by the host under unless-stopped": {withProcesses(stopped(), proc("web", models.RestartUnlessStopped)), true},
		"stopped by the operator":                  {byOperator, false},
		"running under unless-stopped":             {withProcesses(running(), proc("web", models.RestartUnlessStopped)), true},
		"stopped under on-failure and no":          {withProcesses(stopped(), proc("api", models.RestartOnFailure), proc("job", models.RestartNo)), false},
		"paused":                                   {paused, false},
		"created":                                  {created, false},
	}
	for name, c := range cases {
		if got := sandbox.Owed(c.sb); got != c.want {
			t.Errorf("%s: owed %t, want %t", name, got, c.want)
		}
	}
}

func TestAutostartStartsAStoppedSandboxAndWhatItOwes(t *testing.T) {
	sb := withProcesses(stopped(), proc("web", models.RestartAlways), proc("job", models.RestartNo))
	r := &recorder{}
	svc, l := newService(t, r, sb)
	var reports []string

	if err := svc.Autostart(t.Context(), []models.Sandbox{sb}, func(line string) { reports = append(reports, line) }); err != nil {
		t.Fatalf("autostart: %v", err)
	}

	if len(keep(r.calls, "provider.Start")) != 1 || l.repo.sb.State != models.StateRunning {
		t.Errorf("the autostart left the sandbox %s after %v, want it started once", l.repo.sb.State, r.calls)
	}
	if got := specNames(l.provider.specs); !slices.Equal(got, []string{"web"}) {
		t.Errorf("the autostart ran %v, want web alone", got)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "started as the daemon came up") {
		t.Errorf("the autostart reported %v, want one line on the start", reports)
	}
}

func TestAutostartLeavesASandboxTheOperatorStopped(t *testing.T) {
	sb := withProcesses(stopped(), proc("web", models.RestartUnlessStopped))
	sb.StoppedByOperator = true
	r := &recorder{}
	svc, _ := newService(t, r, sb)

	if err := svc.Autostart(t.Context(), []models.Sandbox{sb}, func(string) {}); err != nil {
		t.Fatalf("autostart: %v", err)
	}
	if got := keep(r.calls, "provider.Start", "provider.StartProcess"); len(got) != 0 {
		t.Errorf("the autostart made %v, want nothing", got)
	}
}

// The guest starts again what it knows, so only a run the daemon died before handing over is started here.
func TestAutostartHandsARunningSandboxOnlyWhatItsGuestLacks(t *testing.T) {
	sb := withProcesses(running(), proc("web", models.RestartAlways), proc("api", models.RestartAlways))
	r := &recorder{}
	svc, l := newService(t, r, sb)
	l.provider.report(models.ProcessReport{Name: "web", ProcessStatus: models.ProcessStatus{State: models.ProcessRestarting}})

	if err := svc.Autostart(t.Context(), []models.Sandbox{sb}, func(string) {}); err != nil {
		t.Fatalf("autostart: %v", err)
	}

	if got := specNames(l.provider.specs); !slices.Equal(got, []string{"api"}) {
		t.Errorf("the autostart ran %v, want api alone", got)
	}
	if len(keep(r.calls, "provider.Start")) != 0 {
		t.Errorf("the autostart started a running sandbox: %v", r.calls)
	}
}

// A kill ends the guest's entry, and only the policy then decides: always comes back, unless-stopped stays down.
func TestAutostartBringsBackAKilledAlwaysProcessInARunningSandbox(t *testing.T) {
	web, keeper := killed(proc("web", models.RestartAlways)), killed(proc("keeper", models.RestartUnlessStopped))
	sb := withProcesses(running(), web, keeper)
	svc, l := newService(t, &recorder{}, sb)
	l.provider.report(models.ProcessReport{Name: "web", ProcessStatus: web.Status})
	l.provider.report(models.ProcessReport{Name: "keeper", ProcessStatus: keeper.Status})

	if err := svc.Autostart(t.Context(), []models.Sandbox{sb}, func(string) {}); err != nil {
		t.Fatalf("autostart: %v", err)
	}
	if got := specNames(l.provider.specs); !slices.Equal(got, []string{"web"}) {
		t.Errorf("the autostart ran %v, want web alone", got)
	}
}

// Guest root can break the table, and a start of every name then would run a second copy of what still runs.
func TestAutostartStartsNothingInASandboxWhoseTableIsBroken(t *testing.T) {
	sb := withProcesses(running(), proc("web", models.RestartAlways))
	svc, l := newService(t, &recorder{}, sb)
	l.provider.tableErr = fmt.Errorf("fd 0 of PID 1 is not a regular file: %w", models.ErrExitChannelReplaced)

	err := svc.Autostart(t.Context(), []models.Sandbox{sb}, func(string) {})

	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("autostart returned %v, want the broken table named", err)
	}
	if len(l.provider.specs) != 0 {
		t.Errorf("the autostart ran %+v under a broken table", l.provider.specs)
	}
}

// killed is p as shard kill leaves it.
func killed(p models.Process) models.Process {
	p.Killed = true
	p.Status.State = models.ProcessKilled

	return p
}

func specNames(specs []models.ProcessSpec) []string {
	var out []string
	for _, spec := range specs {
		out = append(out, spec.Name)
	}

	return out
}
