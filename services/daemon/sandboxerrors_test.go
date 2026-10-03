package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestSandboxErrorsLogsAnErrorOnceWhileItLasts(t *testing.T) {
	var out bytes.Buffer
	failures := sandboxErrors{logger: log.New(&out, "", 0), task: "liveness"}
	a, b := errors.New("sandbox a: broken"), errors.New("sandbox b: broken")

	lineA, lineB := "task liveness: sandbox a: broken; the task goes on\n", "task liveness: sandbox b: broken; the task goes on\n"

	ticks := []struct {
		err  error
		want string
	}{
		{errors.Join(a, b), lineA + lineB},
		{errors.Join(a, b), ""},
		{errors.Join(a), ""},
		{nil, ""},
		{errors.Join(a), lineA},
	}
	for i, tick := range ticks {
		out.Reset()
		failures.tick(t.Context(), tick.err)
		if out.String() != tick.want {
			t.Fatalf("tick %d logged %q, want %q", i, out.String(), tick.want)
		}
	}
}

func TestSandboxErrorsKeepsAJoinInsideOneSandboxUnderItsWrapper(t *testing.T) {
	inner := errors.Join(errors.New("dial failed"), errors.New("kill failed"))
	a, b := fmt.Errorf("ask gvisor about sandbox a: %w", inner), fmt.Errorf("ask gvisor about sandbox b: %w", inner)

	ticks := []struct {
		err  error
		want []string
	}{
		{errors.Join(a, b), []string{"sandbox a", "sandbox b"}},
		{a, []string{"sandbox a"}},
	}
	for i, tick := range ticks {
		var out bytes.Buffer
		failures := sandboxErrors{logger: log.New(&out, "", 0), task: "liveness"}
		failures.tick(t.Context(), tick.err)

		if got := strings.Count(out.String(), "the task goes on"); got != len(tick.want) {
			t.Fatalf("tick %d logged %d errors, want one per sandbox: %q", i, got, out.String())
		}
		for _, id := range tick.want {
			if !strings.Contains(out.String(), id) {
				t.Errorf("tick %d: the log %q lost %s", i, out.String(), id)
			}
		}
	}
}

func TestSandboxErrorsKeepsQuietOnTheWayDown(t *testing.T) {
	var out bytes.Buffer
	failures := sandboxErrors{logger: log.New(&out, "", 0), task: "liveness"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	failures.tick(ctx, errors.New("sandbox a: context canceled"))

	if out.Len() != 0 {
		t.Fatalf("a tick the shutdown cut short logged %q, want nothing", out.String())
	}
}

func TestSandboxErrorsLogsEachSandboxsProbeErrorOnceWhileItLasts(t *testing.T) {
	var out bytes.Buffer
	failures := sandboxErrors{logger: log.New(&out, "", 0), task: "health-check"}
	broken := errors.New("sandbox a: broken")
	line := "task health-check: sandbox a: broken; the task goes on\n"

	probes := []struct {
		id   string
		err  error
		want string
	}{
		{"a", broken, line},
		{"a", broken, ""},
		{"b", nil, ""},
		{"a", nil, ""},
		{"a", broken, line},
	}
	for i, probe := range probes {
		out.Reset()
		failures.probe(t.Context(), probe.id, probe.err)
		if out.String() != probe.want {
			t.Fatalf("probe %d logged %q, want %q", i, out.String(), probe.want)
		}
	}

	failures.forget([]models.Sandbox{{ID: "a", State: models.StateStopped}}, probed)
	out.Reset()
	failures.probe(t.Context(), "a", broken)
	if out.String() != line {
		t.Fatalf("a run after one the task stopped probing logged %q, want %q", out.String(), line)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	failures.probe(ctx, "c", errors.New("sandbox c: context canceled"))
	if out.Len() != 0 {
		t.Fatalf("a probe the shutdown cut short logged %q, want nothing", out.String())
	}
}

// One sandbox whose record never writes ended the task on its first probe, so no other sandbox was probed again (SHARD-376).
func TestHealthCheckGoesOnPastASandboxThatFailsEveryProbe(t *testing.T) {
	var out bytes.Buffer
	d := &deps{cfg: Config{Root: t.TempDir(), Out: &out, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}
	starting := &models.Health{Status: models.HealthStarting}
	check := &models.HealthCheck{Command: []string{"false"}, Interval: 1, Timeout: 1, Retries: 2}
	broken, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Health: starting, HealthCheck: check})
	if err != nil {
		t.Fatalf("create the broken record: %v", err)
	}
	fails, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 43, Health: starting, HealthCheck: check})
	if err != nil {
		t.Fatalf("create the record whose probe fails: %v", err)
	}

	broke := &brokenRecordRepo{Repository: repo, broken: broken.ID}
	task := healthCheck{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: broke, Provider: &hangProvider{}})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	// The second failure comes one interval after the first, so it lands only if the task outlived the broken record's first error.
	deadline := time.After(5 * time.Second)
	for {
		sb, err := repo.Get(fails.ID)
		if err != nil {
			t.Fatalf("read the record whose probe fails: %v", err)
		}
		if sb.Health.Status == models.HealthUnhealthy && broke.updates.Load() >= 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run = %v before %s turned unhealthy, want the task to go on past %s", err, fails.ID, broken.ID)
		case <-deadline:
			t.Fatalf("%s is %+v after 5s, want unhealthy within two intervals while %s fails every probe", fails.ID, sb.Health, broken.ID)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	if got := strings.Count(out.String(), "the record does not write"); got != 1 {
		t.Fatalf("the error of %s was logged %d times over %d probes, want once:\n%s", broken.ID, got, broke.updates.Load(), out.String())
	}
	if !strings.Contains(out.String(), broken.ID) {
		t.Fatalf("the log does not name %s:\n%s", broken.ID, out.String())
	}
}

// brokenRecordRepo is the real repository, except that every write to one record fails.
type brokenRecordRepo struct {
	sandbox.Repository
	broken  string
	updates atomic.Int32
}

func (r *brokenRecordRepo) Update(id string, mutate func(*models.Sandbox) error) error {
	if id == r.broken {
		r.updates.Add(1)

		return errors.New("the record does not write")
	}

	return r.Repository.Update(id, mutate)
}

// One sandbox whose exit never decodes failed the whole tick, so the task backed off up to a minute and every other sandbox waited (SHARD-376).
func TestLivenessGoesOnPastASandboxThatFailsEveryTick(t *testing.T) {
	var out bytes.Buffer
	d := &deps{cfg: Config{Root: t.TempDir(), Out: &out, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}
	broken, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42})
	if err != nil {
		t.Fatalf("create the broken record: %v", err)
	}
	exits, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 43})
	if err != nil {
		t.Fatalf("create the record that exits: %v", err)
	}

	p := &exitProvider{broken: broken.ID, exits: exits.ID}
	task := liveness{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		sb, err := repo.Get(exits.ID)
		if err != nil {
			t.Fatalf("read the record that exits: %v", err)
		}
		if sb.ExitStatus != nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run = %v before the exit of %s was recorded, want the task to go on past %s", err, exits.ID, broken.ID)
		case <-deadline:
			t.Fatalf("the exit of %s was not recorded within 5s", exits.ID)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	if got := strings.Count(out.String(), "exit report does not decode"); got != 1 {
		t.Fatalf("the error of %s was logged %d times over %d ticks, want once:\n%s", broken.ID, got, p.reads.Load(), out.String())
	}
	if !strings.Contains(out.String(), broken.ID) {
		t.Fatalf("the log does not name %s:\n%s", broken.ID, out.String())
	}
}

// exitProvider runs every sandbox: one fails its exit read every tick, and the other exits once that failure has repeated.
type exitProvider struct {
	models.Provider
	broken, exits string
	reads         atomic.Int32
}

func (*exitProvider) Name() string { return "fake" }

func (*exitProvider) Status(context.Context, string) (models.Status, error) {
	return models.Status{Exists: true, State: models.StateRunning, PID: 42}, nil
}

func (p *exitProvider) ExitStatus(_ context.Context, id string) (*models.ExitStatus, error) {
	if id == p.broken {
		p.reads.Add(1)

		return nil, errors.New("the exit report does not decode")
	}
	// On main the first failed tick ended the task, so an exit that lands after the second one went unseen for up to a minute.
	if id == p.exits && p.reads.Load() >= 2 {
		return &models.ExitStatus{Code: 3}, nil
	}

	return nil, nil
}
