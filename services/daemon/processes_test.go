package daemon

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// The tick builds the substrate only once a running record has processes, so a host without runsc keeps its daemon.
func TestProcessTickAsksForNoSubstrateWhileNoRunningRecordHasProcesses(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42}); err != nil {
		t.Fatalf("create the running record: %v", err)
	}
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateStopped, Processes: []models.Process{alwaysProcess("app")}}); err != nil {
		t.Fatalf("create the stopped record: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	task := processTick{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	if err := task.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a quiet end over a root where no running record has a process", err)
	}
}

func TestProcessTickFailsLoudWhenTheSubstrateIsGone(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Processes: []models.Process{alwaysProcess("app")}}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	_, want := d.lifecycle()
	if want == nil {
		t.Skip("this host holds a substrate")
	}

	task := processTick{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	err := task.Run(t.Context())
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Run = %v, want the layers' own refusal %v, so the supervisor logs it", err, want)
	}
}

func TestProcessTickCopiesTheTableAndLogsEachMoveOnce(t *testing.T) {
	var out bytes.Buffer
	d := &deps{cfg: Config{Root: t.TempDir(), Out: &out, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}

	app := models.Process{Name: "app", Command: []string{"/bin/false"}, Restart: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 1}, Status: models.ProcessStatus{State: models.ProcessRunning}}
	job := models.Process{Name: "job", Command: []string{"/bin/true"}, Restart: models.RestartSpec{Policy: models.RestartNo}, Status: models.ProcessStatus{State: models.ProcessRunning}}
	sb, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Processes: []models.Process{app, alwaysProcess("web"), job}})
	if err != nil {
		t.Fatalf("create the record: %v", err)
	}

	restarted := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p := &guestProvider{tables: map[string][]models.ProcessReport{sb.ID: {
		{Name: "app", ProcessStatus: models.ProcessStatus{State: models.ProcessGaveUp, Restarts: 2, Exit: &models.ExitStatus{Code: 1}}},
		{Name: "web", ProcessStatus: models.ProcessStatus{State: models.ProcessRunning, Restarts: 1, StartedAt: restarted}},
		{Name: "job", ProcessStatus: models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{}}},
	}}}
	task := processTick{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	// Ticks past the first copy read a table the record already holds, so they must log nothing more.
	awaitTicks(t, p, sb.ID, 5, done)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	got, err := repo.Get(sb.ID)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	states := map[string]models.ProcessStatus{}
	for _, proc := range got.Processes {
		states[proc.Name] = proc.Status
	}
	if s := states["app"]; s.State != models.ProcessGaveUp || s.Restarts != 2 || s.Exit == nil || s.Exit.Code != 1 {
		t.Errorf("the record holds app as %+v, want the give-up after 2 with code 1", s)
	}
	if s := states["web"]; s.State != models.ProcessRunning || s.Restarts != 1 || !s.StartedAt.Equal(restarted) {
		t.Errorf("the record holds web as %+v, want running after 1 start again at %s", s, restarted)
	}
	if s := states["job"]; s.State != models.ProcessExited || s.Exit == nil || s.Exit.Code != 0 {
		t.Errorf("the record holds job as %+v, want exited with code 0", s)
	}

	for _, line := range []string{
		"sandbox " + sb.ID + ": process app was started again, 2 of 2",
		"sandbox " + sb.ID + ": process app exited again and the 2 restarts its policy allows are spent",
		"sandbox " + sb.ID + ": process web was started again, 1\n",
		"sandbox " + sb.ID + ": process job is exited (code 0, signal 0)",
	} {
		if n := strings.Count(out.String(), line); n != 1 {
			t.Errorf("the daemon logged %q %d times, want once:\n%s", line, n, out.String())
		}
	}
}

// One sandbox whose table never reads must not hold back the copy of another's (SHARD-376).
func TestProcessTickGoesOnPastASandboxThatFailsEveryTick(t *testing.T) {
	var out bytes.Buffer
	d := &deps{cfg: Config{Root: t.TempDir(), Out: &out, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}
	broken, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Processes: []models.Process{alwaysProcess("app")}})
	if err != nil {
		t.Fatalf("create the broken record: %v", err)
	}
	good, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 43, Processes: []models.Process{alwaysProcess("app")}})
	if err != nil {
		t.Fatalf("create the record that reads: %v", err)
	}

	p := &guestProvider{broken: broken.ID, tables: map[string][]models.ProcessReport{good.ID: {
		{Name: "app", ProcessStatus: models.ProcessStatus{State: models.ProcessRunning, Restarts: 3}},
	}}}
	task := processTick{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	awaitTicks(t, p, broken.ID, 5, done)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	got, err := repo.Get(good.ID)
	if err != nil {
		t.Fatalf("read the record that reads: %v", err)
	}
	if s := got.Processes[0].Status; s.Restarts != 3 {
		t.Errorf("the record of %s holds %+v, want the table copied past %s", good.ID, s, broken.ID)
	}
	if n := strings.Count(out.String(), "the process table does not decode"); n != 1 {
		t.Fatalf("the error of %s was logged %d times, want once:\n%s", broken.ID, n, out.String())
	}
	if !strings.Contains(out.String(), "task processes: sandbox "+broken.ID) {
		t.Fatalf("the log does not name %s under the task:\n%s", broken.ID, out.String())
	}
}

func alwaysProcess(name string) models.Process {
	return models.Process{Name: name, Command: []string{"/bin/sleep", "600"}, Restart: models.RestartSpec{Policy: models.RestartAlways, Backoff: 1}, Status: models.ProcessStatus{State: models.ProcessRunning}}
}

// awaitTicks waits until the table of id was read n times, which proves the ticks before the last ran to their end.
func awaitTicks(t *testing.T, p *guestProvider, id string, n int, done <-chan error) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for p.readsOf(id) < n {
		select {
		case err := <-done:
			t.Fatalf("Run = %v after %d reads of %s, want the task to go on", err, p.readsOf(id), id)
		case <-deadline:
			t.Fatalf("the table of %s was read %d times within 5s, want %d", id, p.readsOf(id), n)
		case <-time.After(time.Millisecond):
		}
	}
}

// guestProvider answers shard-init's table from tables, fails every read of broken, and keeps each process it starts.
type guestProvider struct {
	models.Provider
	tables map[string][]models.ProcessReport
	broken string
	logs   string

	mu      sync.Mutex
	reads   map[string]int
	started []string
}

func (*guestProvider) Name() string { return "fake" }

func (p *guestProvider) Processes(_ context.Context, id string) ([]models.ProcessReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reads == nil {
		p.reads = map[string]int{}
	}
	p.reads[id]++
	if id == p.broken {
		return nil, errors.New("the process table does not decode")
	}

	return p.tables[id], nil
}

func (p *guestProvider) readsOf(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.reads[id]
}

func (p *guestProvider) StartProcess(_ context.Context, id string, spec models.ProcessSpec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = append(p.started, id+"/"+spec.Name)

	return nil
}

func (p *guestProvider) ProcessLogPath(id, name string) (string, error) {
	return filepath.Join(p.logs, id+"-"+name+".log"), nil
}

func (p *guestProvider) startedProcesses() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.started)
}
