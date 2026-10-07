package sandbox_test

import (
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

func withPorts(sb models.Sandbox, ports ...models.PortForward) models.Sandbox {
	sb.Ports = ports
	return sb
}

// owner is another sandbox on the record that forwards host port 8080, stopped, since its next start takes the port anyway.
func owner() models.Sandbox {
	return models.Sandbox{ID: "sandbox9", Name: "api", State: models.StateStopped, Ports: []models.PortForward{{HostPort: 8080, GuestPort: 80}}}
}

func TestAddPortOpensTheForwardOfARunningSandbox(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	row, err := svc.AddPort(t.Context(), "sandbox1", 8080, sandbox.PortRequest{GuestPort: 80})
	if err != nil {
		t.Fatalf("add port: %v", err)
	}

	want := []models.PortForward{{HostPort: 8080, GuestPort: 80}}
	if !slices.Equal(l.repo.sb.Ports, want) {
		t.Errorf("the record holds %+v, want %+v", l.repo.sb.Ports, want)
	}
	if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{8080}) {
		t.Errorf("the host listens on %v, want 8080", got)
	}
	if !row.Listening || row.Address != "127.0.0.1" || row.HostPort != 8080 || row.GuestPort != 80 {
		t.Errorf("the row is %+v, want 127.0.0.1:8080 to 80, listening", row)
	}
	if len(row.ReachableOn) != 1 || row.ReachableOn[0].Address != "127.0.0.1" {
		t.Errorf("a private forward is reachable on %+v, want the loopback alone", row.ReachableOn)
	}
}

func TestAddPortOnAStoppedSandboxWaitsForItsNextStart(t *testing.T) {
	svc, l := newService(t, &recorder{}, stopped())

	row, err := svc.AddPort(t.Context(), "web", 8080, sandbox.PortRequest{GuestPort: 80, Public: true})
	if err != nil {
		t.Fatalf("add port: %v", err)
	}
	if row.Listening || len(row.ReachableOn) != 0 || row.Address != "0.0.0.0" {
		t.Errorf("the row of a stopped sandbox is %+v, want 0.0.0.0 not listening and reachable nowhere", row)
	}
	if slices.ContainsFunc(l.ports.calls, func(c string) bool { return strings.HasPrefix(c, "Open") }) {
		t.Errorf("a stopped sandbox's forward was opened: %v", l.ports.calls)
	}

	if _, err := svc.Start(t.Context(), "web"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{8080}) {
		t.Errorf("after the start the host listens on %v, want 8080", got)
	}
}

func TestAddPortChangesTheForwardOnAHostPortInPlace(t *testing.T) {
	svc, l := newService(t, &recorder{}, withPorts(running(), models.PortForward{HostPort: 8080, GuestPort: 80}))

	row, err := svc.AddPort(t.Context(), "sandbox1", 8080, sandbox.PortRequest{GuestPort: 81, Public: true})
	if err != nil {
		t.Fatalf("add port: %v", err)
	}

	want := []models.PortForward{{HostPort: 8080, GuestPort: 81, Public: true}}
	if !slices.Equal(l.repo.sb.Ports, want) {
		t.Errorf("the record holds %+v, want the one forward changed to %+v", l.repo.sb.Ports, want)
	}
	if len(row.ReachableOn) != 2 {
		t.Errorf("a public forward is reachable on %+v, want every interface up", row.ReachableOn)
	}
}

func TestAddPortRefusesAHostPortAnotherSandboxForwards(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())
	l.repo.left = []models.Sandbox{running(), owner()}

	_, err := svc.AddPort(t.Context(), "sandbox1", 8080, sandbox.PortRequest{GuestPort: 80})

	var held *sandbox.HeldError
	if !errors.As(err, &held) || !slices.Equal(held.Users, []string{"api"}) {
		t.Fatalf("add port returned %v, want a held error naming sandbox api", err)
	}
	if !strings.Contains(err.Error(), "shard port remove api 8080") {
		t.Errorf("the refusal reads %q, want the fix in it", err)
	}
	if len(l.repo.sb.Ports) != 0 || len(l.ports.calls) != 0 {
		t.Errorf("a refused claim still wrote %+v and drove %v", l.repo.sb.Ports, l.ports.calls)
	}
}

func TestAddPortPutsEverythingBackWhenTheHostRefuses(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		is    func(error) bool
	}{
		{"in use", syscall.EADDRINUSE, func(err error) bool { _, ok := errors.AsType[*sandbox.PortInUseError](err); return ok }},
		{"privileged", syscall.EACCES, func(err error) bool { _, ok := errors.AsType[*sandbox.RequestError](err); return ok }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			was := models.PortForward{HostPort: 8080, GuestPort: 80}
			svc, l := newService(t, &recorder{}, withPorts(running(), was))
			if err := l.ports.Open("sandbox1", was); err != nil {
				t.Fatalf("open the forward the record holds: %v", err)
			}
			l.ports.refuse["0.0.0.0:8080"] = tc.cause

			_, err := svc.AddPort(t.Context(), "sandbox1", 8080, sandbox.PortRequest{GuestPort: 80, Public: true})
			if !tc.is(err) {
				t.Fatalf("add port returned %v, want the refusal typed", err)
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("the refusal %v lost its cause", err)
			}

			if !slices.Equal(l.repo.sb.Ports, []models.PortForward{was}) {
				t.Errorf("the record holds %+v, want the forward it had", l.repo.sb.Ports)
			}
			if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{8080}) || l.ports.specs[8080] != was {
				t.Errorf("the host serves %v as %+v, want the old forward back up", got, l.ports.specs[8080])
			}
		})
	}
}

func TestAddPortRefusesPortZero(t *testing.T) {
	svc, l := newService(t, &recorder{}, running())

	for _, tc := range []struct {
		host  uint16
		guest uint16
	}{{0, 80}, {8080, 0}} {
		_, err := svc.AddPort(t.Context(), "sandbox1", tc.host, sandbox.PortRequest{GuestPort: tc.guest})
		if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
			t.Errorf("add port %d:%d returned %v, want a request error", tc.host, tc.guest, err)
		}
	}
	if len(l.repo.sb.Ports) != 0 {
		t.Errorf("a refused port reached the record: %+v", l.repo.sb.Ports)
	}
}

func TestRemovePortClosesTheForwardAndDropsItFromTheRecord(t *testing.T) {
	ports := []models.PortForward{{HostPort: 8080, GuestPort: 80}, {HostPort: 9090, GuestPort: 90}}
	svc, l := newService(t, &recorder{}, withPorts(running(), ports...))
	if err := l.ports.Set("sandbox1", ports); err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := svc.RemovePort(t.Context(), "sandbox1", 8080); err != nil {
		t.Fatalf("remove port: %v", err)
	}

	if !slices.Equal(l.repo.sb.Ports, ports[1:]) {
		t.Errorf("the record holds %+v, want %+v", l.repo.sb.Ports, ports[1:])
	}
	if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{9090}) {
		t.Errorf("the host listens on %v, want 9090 alone", got)
	}
}

func TestRemovePortOfAHostPortTheSandboxDoesNotForwardIsNotFound(t *testing.T) {
	svc, _ := newService(t, &recorder{}, withPorts(stopped(), models.PortForward{HostPort: 9090, GuestPort: 90}))

	err := svc.RemovePort(t.Context(), "web", 8080)

	var missing *sandbox.PortNotFoundError
	if !errors.As(err, &missing) || missing.Sandbox != "web" || missing.HostPort != 8080 {
		t.Fatalf("remove port returned %v, want port 8080 of web not found", err)
	}
}

func TestListPortsSaysWhatTheHostServesAndWhyNot(t *testing.T) {
	ports := []models.PortForward{{HostPort: 8080, GuestPort: 80}, {HostPort: 9090, GuestPort: 90, Public: true}}
	svc, l := newService(t, &recorder{}, withPorts(running(), ports...))
	l.ports.refuse["0.0.0.0:9090"] = syscall.EADDRINUSE
	if err := l.ports.Set("sandbox1", ports); err != nil {
		t.Fatalf("set: %v", err)
	}
	l.repo.left = []models.Sandbox{l.repo.sb, withPorts(owner(), models.PortForward{HostPort: 7070, GuestPort: 70})}

	rows, err := svc.ListPorts(t.Context(), "")
	if err != nil {
		t.Fatalf("list ports: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("listed %+v, want the three forwards on record", rows)
	}

	// Every sandbox's forwards list in one host port order, which the API pages by.
	if rows[0].Sandbox != "sandbox9" || rows[0].SandboxName != "api" || rows[0].Listening || rows[0].Error != "" {
		t.Errorf("the stopped sandbox's forward reads %+v, want it first, on record and not listening", rows[0])
	}
	if !rows[1].Listening || rows[1].Error != "" {
		t.Errorf("the open forward reads %+v, want listening", rows[1])
	}
	if rows[2].Listening || !strings.Contains(rows[2].Error, "in use on the host") || len(rows[2].ReachableOn) != 0 {
		t.Errorf("the refused forward reads %+v, want why and no address", rows[2])
	}

	one, err := svc.ListPorts(t.Context(), "sandbox1")
	if err != nil {
		t.Fatalf("list the ports of one sandbox: %v", err)
	}
	if len(one) != 2 {
		t.Errorf("listed %+v for sandbox1, want its two forwards", one)
	}
}

func TestListPortsOfAFailedSandboxIsRefused(t *testing.T) {
	failed := withPorts(running(), models.PortForward{HostPort: 8080, GuestPort: 80})
	failed.State, failed.FailedReason = models.StateFailed, "the guest kernel did not boot"
	svc, _ := newService(t, &recorder{}, failed)

	_, err := svc.ListPorts(t.Context(), "sandbox1")

	var state *sandbox.StateError
	if !errors.As(err, &state) || state.Code != models.CodeSandboxFailed {
		t.Fatalf("list ports returned %v, want sandbox_failed", err)
	}
}

func TestCreateWithPortsOpensThemOnceItRuns(t *testing.T) {
	svc, l := newService(t, &recorder{}, models.Sandbox{})
	req := alpine()
	req.Ports = []models.PortForward{{HostPort: 9090, GuestPort: 90}, {HostPort: 8080, GuestPort: 80}}

	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Fatalf("create: %v", err)
	}

	if !slices.Equal(l.repo.created.Ports, []models.PortForward{req.Ports[1], req.Ports[0]}) {
		t.Errorf("the record holds %+v, want the forwards by host port", l.repo.created.Ports)
	}
	if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{8080, 9090}) {
		t.Errorf("the host listens on %v, want both ports", got)
	}
	if i := slices.Index(l.ports.calls, "Probe 8080"); i < 0 || i > slices.Index(l.ports.calls, "Set sandbox1 2") {
		t.Errorf("the forwarder saw %v, want each port probed before the forwards open", l.ports.calls)
	}
}

func TestCreateRefusesAHostPortItCannotHaveBeforeBuildingAnything(t *testing.T) {
	cases := []struct {
		name  string
		setup func(l layers)
		is    func(error) bool
	}{
		{"forwarded by another sandbox", func(l layers) { l.repo.left = []models.Sandbox{owner()} },
			func(err error) bool { _, ok := errors.AsType[*sandbox.HeldError](err); return ok }},
		{"in use on the host", func(l layers) { l.ports.refuse["127.0.0.1:8080"] = syscall.EADDRINUSE },
			func(err error) bool { _, ok := errors.AsType[*sandbox.PortInUseError](err); return ok }},
		{"forwarded twice", func(layers) {},
			func(err error) bool { _, ok := errors.AsType[*sandbox.RequestError](err); return ok }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, models.Sandbox{})
			tc.setup(l)
			req := alpine()
			req.Ports = []models.PortForward{{HostPort: 8080, GuestPort: 80}}
			if tc.name == "forwarded twice" {
				req.Ports = append(req.Ports, models.PortForward{HostPort: 8080, GuestPort: 81})
			}

			_, err := svc.Create(t.Context(), req)
			if !tc.is(err) {
				t.Fatalf("create returned %v, want the refusal typed", err)
			}
			if built := keep(r.calls, "repo.Create", "net.Allocate", "provider.Create"); len(built) != 0 {
				t.Errorf("a refused create still ran %v", built)
			}
		})
	}
}

func TestCreateWithPortsOnAProviderWithoutThemIsRefused(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	l.provider.noPort = true
	req := alpine()
	req.Ports = []models.PortForward{{HostPort: 8080, GuestPort: 80}}

	_, err := svc.Create(t.Context(), req)
	if !errors.Is(err, models.ErrUnsupported) {
		t.Fatalf("create returned %v, want ErrUnsupported", err)
	}
	if built := keep(r.calls, "repo.Create", "net.Allocate", "provider.Create"); len(built) != 0 {
		t.Errorf("a refused create still ran %v", built)
	}
}

// Every verb that takes a sandbox off running closes its forwards, and every one that brings it back opens them.
func TestTheLifecycleOpensAndClosesTheForwards(t *testing.T) {
	forward := models.PortForward{HostPort: 8080, GuestPort: 80}
	cases := []struct {
		name string
		sb   models.Sandbox
		verb func(*sandbox.Service) error
		up   bool
	}{
		{"stop", running(), func(svc *sandbox.Service) error { _, err := svc.Stop(t.Context(), "sandbox1"); return err }, false},
		{"pause", running(), func(svc *sandbox.Service) error { _, err := svc.Pause(t.Context(), "sandbox1"); return err }, false},
		{"remove", stopped(), func(svc *sandbox.Service) error { return svc.Remove(t.Context(), "sandbox1", false) }, false},
		{"resume", pausedSandbox(), func(svc *sandbox.Service) error { _, err := svc.Resume(t.Context(), "sandbox1"); return err }, true},
		{"start", stopped(), func(svc *sandbox.Service) error { _, err := svc.Start(t.Context(), "sandbox1"); return err }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, l := newService(t, &recorder{}, withPorts(tc.sb, forward))
			openUnlessUp(t, l.ports, forward, tc.up)

			if err := tc.verb(svc); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			if got := len(l.ports.listening("sandbox1")) != 0; got != tc.up {
				t.Errorf("after %s the forward is up: %t, want %t (%v)", tc.name, got, tc.up, l.ports.calls)
			}
		})
	}
}

// openUnlessUp puts the forward up before a verb that must take it down.
func openUnlessUp(t *testing.T, ports *fakePorts, forward models.PortForward, up bool) {
	t.Helper()
	if up {
		return
	}
	if err := ports.Open("sandbox1", forward); err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestSyncPortsMatchesTheListenersToTheRecords(t *testing.T) {
	forward := models.PortForward{HostPort: 8080, GuestPort: 80}
	svc, l := newService(t, &recorder{}, withPorts(running(), forward))
	if err := l.ports.Open("gone", models.PortForward{HostPort: 7070, GuestPort: 70}); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := svc.SyncPorts([]models.Sandbox{l.repo.sb}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := l.ports.listening("sandbox1"); !slices.Equal(got, []uint16{8080}) {
		t.Errorf("sandbox1 listens on %v, want its record's 8080", got)
	}
	if got := l.ports.listening("gone"); len(got) != 0 {
		t.Errorf("a sandbox with no record still listens on %v", got)
	}

	l.repo.sb.State = models.StateStopped
	if err := svc.SyncPorts([]models.Sandbox{l.repo.sb}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := l.ports.listening("sandbox1"); len(got) != 0 {
		t.Errorf("a stopped sandbox still listens on %v", got)
	}
}
