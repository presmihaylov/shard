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

// One sandbox whose status never decodes failed the whole tick, so the task backed off up to a minute and every other sandbox waited (SHARD-376).
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
	dies, err := repo.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 43})
	if err != nil {
		t.Fatalf("create the record that dies: %v", err)
	}

	p := &dyingProvider{broken: broken.ID, dies: dies.ID}
	task := liveness{deps: d, lifecycle: &lifecycle{deps: d, svc: sandbox.New(sandbox.Config{Repo: repo, Provider: p})}, interval: time.Millisecond}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		sb, err := repo.Get(dies.ID)
		if err != nil {
			t.Fatalf("read the record that dies: %v", err)
		}
		if sb.State == models.StateStopped {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run = %v before the death of %s was recorded, want the task to go on past %s", err, dies.ID, broken.ID)
		case <-deadline:
			t.Fatalf("the death of %s was not recorded within 5s", dies.ID)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want a quiet end", err)
	}

	if got := strings.Count(out.String(), "status report does not decode"); got != 1 {
		t.Fatalf("the error of %s was logged %d times over %d ticks, want once:\n%s", broken.ID, got, p.reads.Load(), out.String())
	}
	if !strings.Contains(out.String(), broken.ID) {
		t.Fatalf("the log does not name %s:\n%s", broken.ID, out.String())
	}
}

// dyingProvider fails every status read of broken, and reports dies gone once that failure has repeated.
type dyingProvider struct {
	models.Provider
	broken, dies string
	reads        atomic.Int32
}

func (*dyingProvider) Name() string { return "fake" }

func (p *dyingProvider) Status(_ context.Context, id string) (models.Status, error) {
	if id == p.broken {
		p.reads.Add(1)

		return models.Status{}, errors.New("the status report does not decode")
	}
	// On main the first failed tick ended the task, so a death that lands after the second one went unseen for up to a minute.
	if id == p.dies && p.reads.Load() >= 2 {
		return models.Status{}, nil
	}

	return models.Status{Exists: true, State: models.StateRunning, PID: 42}, nil
}
