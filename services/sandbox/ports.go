package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/portforward"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// Ports is the part of portforward.Forwarder the verbs drive: the host listeners of the running sandboxes' forwards.
type Ports interface {
	Open(id string, spec models.PortForward) error
	Set(id string, want []models.PortForward) error
	Close(id string, hostPort uint16) error
	CloseSandbox(id string) error
	Sandboxes() []string
	Probe(spec models.PortForward) error
	Status(hostPort uint16) portforward.Status
	Reachable(public bool) ([]models.HostAddress, error)
}

// PortRequest is the body of a port PUT: the guest port the host port carries to, and whether it listens on every interface.
type PortRequest struct {
	GuestPort uint16 `json:"guest_port" minimum:"1" maximum:"65535"`
	Public    bool   `json:"public,omitempty" doc:"Listen on 0.0.0.0, every interface of the host, instead of 127.0.0.1."`
}

// PortNotFoundError is a host port the sandbox does not forward.
type PortNotFoundError struct {
	Sandbox  string
	HostPort uint16
}

func (e *PortNotFoundError) Error() string {
	return fmt.Sprintf("sandbox %s forwards no host port %d: shard port list %s lists its forwards", e.Sandbox, e.HostPort, e.Sandbox)
}

func (e *PortNotFoundError) Public() string { return e.Error() }

// PortInUseError is a host port another process on the host listens on.
type PortInUseError struct {
	Err *portforward.BindError
}

func (e *PortInUseError) Error() string { return e.Err.Error() }

func (e *PortInUseError) Unwrap() error { return e.Err }

func (e *PortInUseError) Public() string { return e.Err.Public() }

// AddPort upserts a forward by host port; a running sandbox takes it at once, any other on its next start.
func (s *Service) AddPort(ctx context.Context, ref string, hostPort uint16, req PortRequest) (models.Port, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbPort); err != nil {
		return models.Port{}, err
	}
	spec := models.PortForward{HostPort: hostPort, GuestPort: req.GuestPort, Public: req.Public}
	if err := validPort(spec); err != nil {
		return models.Port{}, err
	}

	id, sb, unlock, err := s.holdForPorts(ctx, ref)
	if err != nil {
		return models.Port{}, err
	}
	defer unlock()

	// One step with the owner check, so two sandboxes never both claim a host port.
	s.portsMu.Lock()
	defer s.portsMu.Unlock()
	if err := s.portUnclaimed(id, hostPort); err != nil {
		return models.Port{}, err
	}

	was := sb.Ports
	if err := s.writePorts(id, upsertPort(was, spec)); err != nil {
		return models.Port{}, err
	}
	if sb.State == models.StateRunning {
		if err := s.cfg.Ports.Open(id, spec); err != nil {
			return models.Port{}, s.refusedPort(id, was, hostPort, err)
		}
	}

	sb, err = s.record(id)
	if err != nil {
		return models.Port{}, err
	}

	return s.portRow(id, sb, spec)
}

// refusedPort puts the record and the host back as they were, since a record must not name a forward the host would not take.
func (s *Service) refusedPort(id string, was []models.PortForward, hostPort uint16, err error) error {
	restore := s.cfg.Ports.Close(id, hostPort)
	if i := slices.IndexFunc(was, func(p models.PortForward) bool { return p.HostPort == hostPort }); i >= 0 {
		restore = s.cfg.Ports.Open(id, was[i])
	}
	if revert := errors.Join(s.writePorts(id, was), restore); revert != nil {
		return fmt.Errorf("the host did not take the forward and the sandbox did not go back as it was: %w", errors.Join(portRefused(err), revert))
	}

	return portRefused(err)
}

// portRefused makes the request's fault a port another process holds or one only root may bind; the host breaking is not.
func portRefused(err error) error {
	bind, ok := errors.AsType[*portforward.BindError](err)
	if !ok {
		return err
	}
	if errors.Is(bind, syscall.EADDRINUSE) {
		return &PortInUseError{Err: bind}
	}
	if errors.Is(bind, syscall.EACCES) {
		return &RequestError{Err: bind, Text: bind.Public()}
	}

	return bind
}

// RemovePort ends the forward on one host port of the sandbox, the connections it carries included.
func (s *Service) RemovePort(ctx context.Context, ref string, hostPort uint16) error {
	id, sb, unlock, err := s.holdForPorts(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()

	kept := slices.DeleteFunc(slices.Clone(sb.Ports), func(p models.PortForward) bool { return p.HostPort == hostPort })
	if len(kept) == len(sb.Ports) {
		return &PortNotFoundError{Sandbox: nameOf(id, sb), HostPort: hostPort}
	}
	if err := s.writePorts(id, kept); err != nil {
		return err
	}
	if err := s.cfg.Ports.Close(id, hostPort); err != nil {
		return fmt.Errorf("the forward on host port %d is off the record, but its listener did not close: %w", hostPort, err)
	}

	return nil
}

// ListPorts answers the forwards of one sandbox, or of every sandbox when ref is empty, as the host serves them now.
func (s *Service) ListPorts(_ context.Context, ref string) ([]models.Port, error) {
	sandboxes, err := s.portSandboxes(ref)
	if err != nil {
		return nil, err
	}

	rows := []models.Port{}
	for _, sb := range sandboxes {
		own, err := s.portRows(sb)
		if err != nil {
			return nil, err
		}
		rows = append(rows, own...)
	}
	// Host ports are unique across sandboxes, so this is the order a list pages in.
	slices.SortFunc(rows, func(a, b models.Port) int { return cmp.Compare(a.HostPort, b.HostPort) })

	return rows, nil
}

func (s *Service) portSandboxes(ref string) ([]models.Sandbox, error) {
	if ref != "" {
		sb, err := Get(s.cfg.Repo, ref)
		if err != nil {
			return nil, err
		}
		if err := FailedGuard(sb.ID, sb); err != nil {
			return nil, err
		}

		return []models.Sandbox{sb}, nil
	}

	return sandboxstate.ListReadable(s.cfg.Repo, nil)
}

// SyncPorts makes the host's listeners match the records: each running sandbox's forwards, and nothing for one that is not running or is gone.
func (s *Service) SyncPorts(sandboxes []models.Sandbox) error {
	ids := s.cfg.Ports.Sandboxes()
	for _, sb := range sandboxes {
		if len(sb.Ports) != 0 && !slices.Contains(ids, sb.ID) {
			ids = append(ids, sb.ID)
		}
	}

	var errs []error
	for _, id := range ids {
		errs = append(errs, s.syncPorts(id))
	}

	return errors.Join(errs...)
}

// syncPorts reads the record under the sandbox's lock, so a create or an rm in flight is never undone; one a verb holds is that verb's to open or close.
func (s *Service) syncPorts(id string) error {
	unlock, ok := s.tryLock(id)
	if !ok {
		return nil
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return s.closePorts(id)
	}
	if err != nil {
		return err
	}
	if sb.State != models.StateRunning {
		return s.closePorts(id)
	}

	return s.cfg.Ports.Set(id, sb.Ports)
}

// openPorts puts up the forwards of a sandbox that just reached running; a host port the host refuses keeps why for port list, and the ports task retries it.
func (s *Service) openPorts(id string) error {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}
	if err := s.cfg.Ports.Set(id, sb.Ports); err != nil {
		return fmt.Errorf("sandbox %s is running, but its forwards did not open: %w", id, err)
	}

	return nil
}

// closePorts ends every forward of a sandbox on its way down, and the connections they carry, which reach nothing past this.
func (s *Service) closePorts(id string) error {
	if err := s.cfg.Ports.CloseSandbox(id); err != nil {
		return fmt.Errorf("close the forwards of sandbox %s: %w", id, err)
	}

	return nil
}

// claimPorts refuses a create whose host ports another sandbox forwards, or the host would not give it.
func (s *Service) claimPorts(ports []models.PortForward) error {
	for _, spec := range ports {
		if err := s.portUnclaimed("", spec.HostPort); err != nil {
			return err
		}
		if err := s.cfg.Ports.Probe(spec); err != nil {
			return portRefused(err)
		}
	}

	return nil
}

// portUnclaimed refuses a host port a sandbox other than id forwards, whatever state that one is in, since its next start takes the port.
func (s *Service) portUnclaimed(id string, hostPort uint16) error {
	sandboxes, err := sandboxstate.ListReadable(s.cfg.Repo, nil)
	if err != nil {
		return err
	}
	for _, sb := range sandboxes {
		if sb.ID == id || !slices.ContainsFunc(sb.Ports, func(p models.PortForward) bool { return p.HostPort == hostPort }) {
			continue
		}
		owner := nameOf(sb.ID, sb)

		return &HeldError{Subject: fmt.Sprintf("host port %d", hostPort), Verb: "forwarded to", Noun: "sandbox", Users: []string{owner},
			Fix: fmt.Sprintf("remove that forward first with shard port remove %s %d", owner, hostPort)}
	}

	return nil
}

func (s *Service) holdForPorts(ctx context.Context, ref string) (string, models.Sandbox, func(), error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return "", models.Sandbox{}, nil, err
	}
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return "", models.Sandbox{}, nil, err
	}
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}
	if err := FailedGuard(id, sb); err != nil {
		unlock()

		return "", models.Sandbox{}, nil, err
	}

	return id, sb, unlock, nil
}

func (s *Service) writePorts(id string, ports []models.PortForward) error {
	return s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.Ports = ports

		return nil
	})
}

// portRows are the forwards of one sandbox as the host serves them.
func (s *Service) portRows(sb models.Sandbox) ([]models.Port, error) {
	rows := make([]models.Port, 0, len(sb.Ports))
	for _, spec := range sb.Ports {
		row, err := s.portRow(sb.ID, sb, spec)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}

	return rows, nil
}

// portRow is one forward as the host serves it: a sandbox that is not running listens on nothing.
func (s *Service) portRow(id string, sb models.Sandbox, spec models.PortForward) (models.Port, error) {
	row := models.Port{Sandbox: id, SandboxName: sb.Name, HostPort: spec.HostPort, GuestPort: spec.GuestPort, Public: spec.Public,
		Address: portforward.Address(spec.Public), ReachableOn: []models.HostAddress{}}
	status := s.cfg.Ports.Status(spec.HostPort)
	if sb.State != models.StateRunning || status.Sandbox != id {
		return row, nil
	}
	row.Listening, row.Error = status.Listening, status.Error
	if !row.Listening {
		return row, nil
	}
	on, err := s.cfg.Ports.Reachable(spec.Public)
	if err != nil {
		return models.Port{}, err
	}
	row.ReachableOn = on

	return row, nil
}

// upsertPort replaces the forward on spec's host port or adds it.
func upsertPort(ports []models.PortForward, spec models.PortForward) []models.PortForward {
	kept := slices.DeleteFunc(slices.Clone(ports), func(p models.PortForward) bool { return p.HostPort == spec.HostPort })

	return byHostPort(append(kept, spec))
}

// byHostPort is the order a record keeps its forwards in, which ls lists them in.
func byHostPort(ports []models.PortForward) []models.PortForward {
	out := slices.Clone(ports)
	slices.SortFunc(out, func(a, b models.PortForward) int { return cmp.Compare(a.HostPort, b.HostPort) })

	return out
}

func validPort(spec models.PortForward) error {
	if spec.HostPort == 0 {
		return &RequestError{Err: errors.New("the host port must be 1 to 65535, got 0")}
	}
	if spec.GuestPort == 0 {
		return &RequestError{Err: errors.New("the guest port must be 1 to 65535, got 0")}
	}

	return nil
}

// validPorts refuses a create that names one host port twice, which no two forwards can share.
func validPorts(ports []models.PortForward) error {
	seen := map[uint16]bool{}
	for _, spec := range ports {
		if err := validPort(spec); err != nil {
			return err
		}
		if seen[spec.HostPort] {
			return &RequestError{Err: fmt.Errorf("host port %d is forwarded twice; a host port carries to one guest port", spec.HostPort)}
		}
		seen[spec.HostPort] = true
	}

	return nil
}

// noPorts is the forwarder of a service a test builds without one: no verb puts up a listener.
type noPorts struct{}

func (noPorts) Open(string, models.PortForward) error        { return nil }
func (noPorts) Set(string, []models.PortForward) error       { return nil }
func (noPorts) Close(string, uint16) error                   { return nil }
func (noPorts) CloseSandbox(string) error                    { return nil }
func (noPorts) Sandboxes() []string                          { return nil }
func (noPorts) Probe(models.PortForward) error               { return nil }
func (noPorts) Status(uint16) portforward.Status             { return portforward.Status{} }
func (noPorts) Reachable(bool) ([]models.HostAddress, error) { return []models.HostAddress{}, nil }
