package daemon

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestGateOpensOnceAndANilGateIsANoOp(t *testing.T) {
	g := newGate()
	g.open()
	g.open()
	select {
	case <-g.ch:
	default:
		t.Fatal("the gate is still shut after open")
	}

	var none *gate
	none.open()
}

func TestAutostartReadyWaitsForEveryGate(t *testing.T) {
	proxy, firewall := newGate(), newGate()
	proxy.open()
	go func() {
		time.Sleep(10 * time.Millisecond)
		firewall.open()
	}()

	task := autostart{after: []*gate{proxy, firewall}, wait: time.Minute}
	if !task.ready(t.Context()) {
		t.Fatal("ready = false, want true once the last gate opened")
	}
}

func TestAutostartReadyGivesUpOnceTheWaitRunsOut(t *testing.T) {
	proxy := newGate()
	proxy.open()

	task := autostart{after: []*gate{proxy, newGate()}, wait: 10 * time.Millisecond}
	if task.ready(t.Context()) {
		t.Fatal("ready = true over a gate that never opened, want false once the wait ran out")
	}
}

func TestAutostartReadyEndsWithTheDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	task := autostart{after: []*gate{newGate()}, wait: time.Minute}
	if task.ready(ctx) {
		t.Fatal("ready = true after the daemon stopped, want false")
	}
}

// A daemon that stops while it waits on the network starts nothing and logs nothing.
func TestAutostartEndsQuietWhenTheDaemonStopsBeforeTheNetworkIsUp(t *testing.T) {
	d := noRunscDeps(t)
	var out bytes.Buffer
	d.cfg.Out = &out
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateStopped, Processes: []models.Process{alwaysProcess("app")}}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	task := autostart{deps: d, lifecycle: &lifecycle{deps: d}, after: []*gate{newGate()}, wait: time.Minute}
	if err := task.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a quiet end once the daemon stopped", err)
	}
	if out.Len() != 0 {
		t.Fatalf("Run logged %q, want nothing", out.String())
	}
}

// A network task that never comes up costs a sandbox its start no more than the wait.
func TestAutostartGoesOnWithoutTheNetworkOnceTheWaitRunsOut(t *testing.T) {
	d := noRunscDeps(t)
	var out bytes.Buffer
	d.cfg.Out = &out
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateStopped, Processes: []models.Process{alwaysProcess("app")}}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	_, want := d.lifecycle()
	if want == nil {
		t.Skip("this host holds a substrate")
	}

	task := autostart{deps: d, lifecycle: &lifecycle{deps: d}, after: []*gate{newGate()}, wait: time.Millisecond}
	err := task.Run(t.Context())
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Run = %v, want the layers' own refusal %v, which only a start past the wait reaches", err, want)
	}
	if !strings.Contains(out.String(), "task autostart: the proxy, the resolver or the host firewall is not up after 1ms; the sandboxes start without it") {
		t.Fatalf("Run logged %q, want the wait that ran out named", out.String())
	}
}

// A root that owes no start needs no substrate, so a host without runsc keeps its daemon.
func TestAutostartAsksForNoSubstrateWhenNothingIsOwed(t *testing.T) {
	d := noRunscDeps(t)

	unlessStopped := models.Process{Name: "app", Command: []string{"/bin/sleep", "600"}, Restart: models.RestartSpec{Policy: models.RestartUnlessStopped, Backoff: 1}}
	killed := unlessStopped
	killed.Killed = true
	never := models.Process{Name: "app", Command: []string{"/bin/true"}, Restart: models.RestartSpec{Policy: models.RestartNo}}
	for _, sb := range []models.Sandbox{
		{Image: "alpine", State: models.StateRunning, PID: 42},
		{Image: "alpine", State: models.StateStopped, Processes: []models.Process{never}},
		{Image: "alpine", State: models.StateStopped, StoppedByOperator: true, Processes: []models.Process{unlessStopped}},
		{Image: "alpine", State: models.StateRunning, PID: 43, Processes: []models.Process{killed}},
		{Image: "alpine", State: models.StateFailed, Processes: []models.Process{alwaysProcess("app")}},
	} {
		if _, err := d.repoSvc.Create(sb); err != nil {
			t.Fatalf("create the record: %v", err)
		}
	}

	up := newGate()
	up.open()
	task := autostart{deps: d, lifecycle: &lifecycle{deps: d}, after: []*gate{up}, wait: time.Minute}
	if err := task.Run(t.Context()); err != nil {
		t.Fatalf("Run = %v, want a quiet end over a root that owes no start", err)
	}
}

// Autostart starts what each owed sandbox owes, and one sandbox's error is logged while the rest still start (SHARD-376).
func TestAutostartStartsWhatTheOwedSandboxesOwe(t *testing.T) {
	var out bytes.Buffer
	d := &deps{cfg: Config{Root: t.TempDir(), Out: &out, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}

	killed := alwaysProcess("app")
	killed.Restart.Policy = models.RestartUnlessStopped
	killed.Killed = true
	records := map[string]models.Sandbox{
		"broken": {Image: "alpine", State: models.StateRunning, PID: 42, Processes: []models.Process{alwaysProcess("app")}},
		"owed":   {Image: "alpine", State: models.StateRunning, PID: 43, Processes: []models.Process{alwaysProcess("app")}},
		"known":  {Image: "alpine", State: models.StateRunning, PID: 44, Processes: []models.Process{alwaysProcess("app")}},
		"killed": {Image: "alpine", State: models.StateRunning, PID: 45, Processes: []models.Process{killed}},
	}
	ids := map[string]string{}
	for name, sb := range records {
		created, err := repo.Create(sb)
		if err != nil {
			t.Fatalf("create the %s record: %v", name, err)
		}
		ids[name] = created.ID
	}

	p := &guestProvider{broken: ids["broken"], logs: t.TempDir(), tables: map[string][]models.ProcessReport{
		ids["known"]: {{Name: "app", ProcessStatus: models.ProcessStatus{State: models.ProcessRunning}}},
	}}
	up := newGate()
	up.open()
	task := autostart{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, after: []*gate{up}, wait: time.Minute}
	if err := task.Run(t.Context()); err != nil {
		t.Fatalf("Run = %v, want a quiet end with the error of %s logged", err, ids["broken"])
	}

	if got, want := p.startedProcesses(), []string{ids["owed"] + "/app"}; !slices.Equal(got, want) {
		t.Errorf("Autostart started %v, want %v alone: the guest already runs the known one, and kill keeps the other down", got, want)
	}
	if !strings.Contains(out.String(), "task autostart: sandbox "+ids["broken"]+": ") || !strings.Contains(out.String(), "the process table does not decode") {
		t.Errorf("Run logged %q, want the error of %s named under the task", out.String(), ids["broken"])
	}
}
