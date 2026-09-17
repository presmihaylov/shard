package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

type healthLab struct {
	svc     *sandbox.Service
	l       layers
	r       *recorder
	reports []string
}

func newHealthLab(t *testing.T, sb models.Sandbox) *healthLab {
	t.Helper()

	lab := &healthLab{r: &recorder{}}
	lab.svc, lab.l = newService(t, lab.r, sb)

	return lab
}

// tick runs one pass over the record as the daemon lists it, which may be older than what the store holds.
func (l *healthLab) tick(t *testing.T, listed models.Sandbox, now time.Time) error {
	t.Helper()

	return l.svc.CheckHealth(t.Context(), []models.Sandbox{listed}, now, func(line string) { l.reports = append(l.reports, line) })
}

// probed is a running sandbox with a command probe on its first run, as the create leaves it.
func probed() models.Sandbox {
	sb := running()
	sb.HealthCheck = &models.HealthCheck{Command: []string{"/bin/true"}, Interval: 30, Timeout: 10, Retries: 3}
	sb.Health = &models.Health{Status: models.HealthStarting}

	return sb
}

func TestHealthCheckMarksAPassingCommandHealthy(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	lab := newHealthLab(t, probed())

	if err := lab.tick(t, probed(), now); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	got := lab.l.repo.sb.Health
	if got.Status != models.HealthHealthy || got.Failures != 0 || !got.CheckedAt.Equal(now) {
		t.Errorf("the record says %+v, want healthy with no failures at %v", got, now)
	}
	if argv := lab.l.provider.execSpec.Argv; strings.Join(argv, " ") != "/bin/true" {
		t.Errorf("the probe ran %v, want the command the record names", argv)
	}
	if len(lab.reports) != 1 || lab.reports[0] != "sandbox sandbox1 is healthy" {
		t.Errorf("the pass reported %v, want one line for the change", lab.reports)
	}
}

func TestHealthCheckTurnsUnhealthyAfterTheRetries(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	lab := newHealthLab(t, probed())
	lab.l.provider.execExit = models.ExitStatus{Code: 1}

	for i := range 3 {
		if err := lab.tick(t, lab.l.repo.sb, now.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatalf("CheckHealth %d: %v", i+1, err)
		}
		if got := lab.l.repo.sb.Health; got.Failures != i+1 {
			t.Fatalf("after probe %d the record counts %d failures", i+1, got.Failures)
		}
		if got := lab.l.repo.sb.Health; i < 2 && got.Status != models.HealthStarting {
			t.Fatalf("after probe %d the record says %s, want starting until the retries are spent", i+1, got.Status)
		}
	}

	if got := lab.l.repo.sb.Health; got.Status != models.HealthUnhealthy {
		t.Errorf("after three failures the record says %s, want unhealthy", got.Status)
	}
	if len(lab.reports) != 1 || lab.reports[0] != "sandbox sandbox1 is unhealthy: /bin/true exited 1" {
		t.Errorf("the passes reported %v, want one line naming the exit", lab.reports)
	}
}

func TestHealthCheckComesBackHealthyOnOnePass(t *testing.T) {
	sb := probed()
	sb.Health = &models.Health{Status: models.HealthUnhealthy, Failures: 3}
	lab := newHealthLab(t, sb)

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	if got := lab.l.repo.sb.Health; got.Status != models.HealthHealthy || got.Failures != 0 {
		t.Errorf("the record says %+v, want healthy with the failures cleared", got)
	}
}

func TestHealthCheckWaitsOutTheInterval(t *testing.T) {
	now := time.Now()
	sb := probed()
	sb.Health = &models.Health{Status: models.HealthHealthy, CheckedAt: now.Add(-10 * time.Second)}
	lab := newHealthLab(t, sb)

	if err := lab.tick(t, sb, now); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	if calls := keep(lab.r.calls, "provider.Exec"); len(calls) != 0 {
		t.Errorf("a probe ran 10 s into a 30 s interval")
	}
}

func TestHealthCheckLeavesASandboxThatIsNotRunning(t *testing.T) {
	sb := probed()
	sb.State = models.StatePaused
	lab := newHealthLab(t, sb)

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	if calls := keep(lab.r.calls, "provider.Exec"); len(calls) != 0 {
		t.Errorf("a probe ran in a paused sandbox")
	}
}

func TestHealthCheckFailsAProbeThatDoesNotAnswerInTime(t *testing.T) {
	sb := probed()
	sb.HealthCheck.Timeout, sb.HealthCheck.Retries = 1, 1
	lab := newHealthLab(t, sb)
	lab.l.provider.execWaits = make(chan struct{})

	if err := lab.tick(t, sb, time.Now()); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	if got := lab.l.repo.sb.Health; got.Status != models.HealthUnhealthy {
		t.Errorf("a probe that hung left the record %+v, want unhealthy", got)
	}
	if len(lab.reports) != 1 || !strings.HasSuffix(lab.reports[0], "the probe did not answer within 1s") {
		t.Errorf("the pass reported %v, want the timeout named", lab.reports)
	}
}

func TestHealthCheckDropsTheResultOfARunThatEnded(t *testing.T) {
	sb := probed()
	lab := newHealthLab(t, sb)
	lab.l.provider.execBegan, lab.l.provider.execWaits = make(chan struct{}), make(chan struct{})

	done := make(chan error, 1)
	go func() { done <- lab.tick(t, sb, time.Now()) }()

	// A stop and a start land while the probe runs, so the record holds another run's pid.
	<-lab.l.provider.execBegan
	lab.l.repo.sb.PID = 99
	close(lab.l.provider.execWaits)

	if err := <-done; err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if got := lab.l.repo.sb.Health; got.Status != models.HealthStarting || !got.CheckedAt.IsZero() {
		t.Errorf("a probe of the old run wrote %+v onto the new one", got)
	}
}
