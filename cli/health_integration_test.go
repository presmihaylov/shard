//go:build integration

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// healthBudget covers a 1 s interval, a probe of a few seconds and the retries, with room for a slow box.
const healthBudget = 90 * time.Second

func TestTheDaemonProbesASandboxWithACommand(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	args := []string{"create", "--health-command", "test ! -e /sick", "--health-interval", "1s", "--health-retries", "2", testImage, "--", "/bin/sh", "-c", "while true; do sleep 1; done"}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := strings.TrimSpace(out.String())
	out.Reset()
	t.Cleanup(func() { cleanUp(t, app, id) })

	sb := awaitHealth(t, app, id, models.HealthHealthy)
	if sb.Health.Failures != 0 || sb.Health.CheckedAt.IsZero() {
		t.Errorf("the healthy record holds %+v, want no failures and the last probe named", sb.Health)
	}

	if _, err := runExec(t, app, "exec", id, "--", "/bin/touch", "/sick"); err != nil {
		t.Fatalf("exec touch: %v", err)
	}

	sb = awaitHealth(t, app, id, models.HealthUnhealthy)
	if sb.Health.Failures != 2 {
		t.Errorf("the unhealthy record counts %d failures, want the 2 retries", sb.Health.Failures)
	}
	if !strings.Contains(daemonUnderTest.logged(), "sandbox "+id+" is unhealthy: /bin/sh -c test ! -e /sick exited 1") {
		t.Error("the daemon logged no line for the change")
	}

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(out.String(), "unhealthy 2/2") {
		t.Errorf("ls printed %q, want the health column", out.String())
	}
}

func TestTheDaemonProbesASandboxOverHTTP(t *testing.T) {
	app, out := newCreateApp(t)
	t.Parallel()

	// busybox httpd serves /www in the foreground, and the second command ends it on request.
	script := "mkdir -p /www && echo ok > /www/healthz && httpd -f -p 8080 -h /www"
	args := []string{"create", "--health-http", "8080/healthz", "--health-interval", "1s", "--health-retries", "1", testImage, "--", "/bin/sh", "-c", script}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := strings.TrimSpace(out.String())
	out.Reset()
	t.Cleanup(func() { cleanUp(t, app, id) })

	awaitHealth(t, app, id, models.HealthHealthy)

	if _, err := runExec(t, app, "exec", id, "--", "/bin/sh", "-c", "pkill httpd"); err != nil {
		t.Fatalf("exec pkill: %v", err)
	}

	awaitHealth(t, app, id, models.HealthUnhealthy)
	if !strings.Contains(daemonUnderTest.logged(), "sandbox "+id+" is unhealthy: GET http://") {
		t.Error("the daemon logged no line naming the request that failed")
	}
}

// awaitHealth polls the daemon until the record reads the status, and names what it last saw if never.
func awaitHealth(t *testing.T, app App, id string, want models.HealthStatus) models.Sandbox {
	t.Helper()

	deadline := time.Now().Add(healthBudget)
	for {
		sb := record(t, app, id)
		if sb.Health != nil && sb.Health.Status == want {
			return sb
		}
		if time.Now().After(deadline) {
			t.Fatalf("the record of %s never read %s in %s, last %s with health %+v", id, want, healthBudget, sb.State, sb.Health)
		}

		time.Sleep(time.Second)
	}
}
