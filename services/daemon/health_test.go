package daemon

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// The tick builds the substrate only once a running record has a probe, so a host without runsc keeps its daemon.
func TestHealthCheckAsksForNoSubstrateWhileNothingIsProbed(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	task := healthCheck{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	if err := task.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a quiet end over a root where no record asks for a probe", err)
	}
}

func TestHealthCheckFailsLoudWhenTheSubstrateIsGone(t *testing.T) {
	d := noRunscDeps(t)
	sb := models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, HealthCheck: &models.HealthCheck{Command: []string{"true"}, Interval: 1, Timeout: 1, Retries: 1}}
	if _, err := d.repoSvc.Create(sb); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	_, want := d.lifecycle()
	if want == nil {
		t.Skip("this host holds a substrate")
	}

	task := healthCheck{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	err := task.Run(t.Context())
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Run = %v, want the layers' own refusal %v, so the supervisor logs it", err, want)
	}
}

// One probe that never answered held the tick, so no other sandbox was probed again until its timeout ran out (SHARD-363).
func TestHealthCheckGoesOnPastAProbeThatHangs(t *testing.T) {
	d := &deps{cfg: Config{Root: t.TempDir(), Out: io.Discard, Provider: "gvisor"}}
	repo, err := d.repo()
	if err != nil {
		t.Fatalf("build the repository: %v", err)
	}
	starting := &models.Health{Status: models.HealthStarting}
	hangs, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Health: starting,
		HealthCheck: &models.HealthCheck{Command: []string{"sleep", "infinity"}, Interval: 1, Timeout: sandbox.MaxHealthTimeout, Retries: 1}})
	if err != nil {
		t.Fatalf("create the record whose probe hangs: %v", err)
	}
	fails, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 43, Health: starting,
		HealthCheck: &models.HealthCheck{Command: []string{"false"}, Interval: 1, Timeout: 1, Retries: 2}})
	if err != nil {
		t.Fatalf("create the record whose probe fails: %v", err)
	}

	p := &hangProvider{hangs: hangs.ID}
	task := healthCheck{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	// The second failure comes one interval after the first, so it lands only if the hung probe holds nothing back.
	deadline := time.After(5 * time.Second)
	for {
		sb, err := repo.Get(fails.ID)
		if err != nil {
			t.Fatalf("read the record whose probe fails: %v", err)
		}
		if sb.Health.Status == models.HealthUnhealthy {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run = %v before %s turned unhealthy", err, fails.ID)
		case <-deadline:
			t.Fatalf("%s is %+v after 5s, want unhealthy within two intervals while the probe of %s hangs", fails.ID, sb.Health, hangs.ID)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	if got := p.hung.Load(); got != 1 {
		t.Errorf("the probe of %s ran %d times while its first one hung, want once", hangs.ID, got)
	}
}

// hangProvider fails every probe at once, except one sandbox's, which answers only when its context ends.
type hangProvider struct {
	models.Provider
	hangs string
	hung  atomic.Int32
}

func (*hangProvider) Name() string { return "fake" }

func (p *hangProvider) Exec(ctx context.Context, id string, _ models.ExecSpec) (models.ExitStatus, error) {
	if id != p.hangs {
		return models.ExitStatus{Code: 1}, nil
	}
	p.hung.Add(1)
	<-ctx.Done()

	return models.ExitStatus{}, ctx.Err()
}
