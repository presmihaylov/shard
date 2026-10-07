package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// The tick builds the substrate only once a record forwards a port, so a host without runsc keeps its daemon.
func TestHostPortsAsksForNoSubstrateWhileNothingForwards(t *testing.T) {
	d := noRunscDeps(t)
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	task := hostPorts{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	if err := task.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want a quiet end over a root where no record forwards a port", err)
	}
}

func TestHostPortsFailsLoudWhenTheSubstrateIsGone(t *testing.T) {
	d := noRunscDeps(t)
	ports := []models.PortForward{{HostPort: 8080, GuestPort: 80}}
	if _, err := d.repoSvc.Create(models.Sandbox{Image: "alpine", State: models.StateRunning, PID: 42, Ports: ports}); err != nil {
		t.Fatalf("create the record: %v", err)
	}

	_, want := d.lifecycle()
	if want == nil {
		t.Skip("this host holds a substrate")
	}

	task := hostPorts{deps: d, lifecycle: &lifecycle{deps: d}, interval: time.Millisecond}
	err := task.Run(t.Context())
	if err == nil || err.Error() != want.Error() {
		t.Fatalf("Run = %v, want the layers' own refusal %v, so the supervisor logs it", err, want)
	}
}
