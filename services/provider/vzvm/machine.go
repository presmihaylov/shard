package vzvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// machine is one live shim: its client, the control connection to shard-init and the link its frames ride.
type machine struct {
	id     string
	dir    string
	client *vz.Client
	pid    int
	// machineID is what the shim reported, which a record persists for every later boot of the disk.
	machineID string
	control   *supervisor.Control
	link      *netstack.Link
	cancel    context.CancelFunc
	// events closes when the control connection ended, which is the guest gone.
	events chan struct{}

	// started is what the guest last said: the entrypoint forked, so the sandbox runs.
	started bool
	// gone is set by the event loop when the control connection ended, so a status needs no socket round trip.
	gone bool
	// lost is the first exit or restart event the loop could not persist; the files say nothing true after it.
	lost error
}

// dial is the supervisor's Dialer over the shim: one vsock connection per call.
func (m *machine) dial(_ context.Context, port uint32) (net.Conn, error) {
	return m.client.Connect(port)
}

// lookup finds the sandbox's shim, held or adopted by its socket, and returns nil when none answers.
func (p *Provider) lookup(ctx context.Context, id, dir string, r record) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	p.mu.Unlock()
	if held {
		return m, nil
	}

	client, info, err := vz.Adopt(filepath.Join(dir, socketFile))
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return p.attach(ctx, id, dir, r, client, info)
}

// absent is a socket with no shim behind it: never made, or its owner exited and the path went with it.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// release lets a shim whose guest has gone finish exiting, and forgets it, so a new boot can claim the socket.
func (p *Provider) release(ctx context.Context, m *machine) error {
	if m == nil {
		return nil
	}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the shim of sandbox %s still answers %s after its guest went", m.id, killGrace)
	}
	p.forget(m)

	return m.close()
}

func (p *Provider) forget(m *machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.machines[m.id] == m {
		delete(p.machines, m.id)
	}
}

// boot starts a shim for the sandbox over its own disk, and attaches to the guest once it answers.
func (p *Provider) boot(ctx context.Context, id, dir string, r record, restore string) (*machine, error) {
	// The next run must not answer a wait, or a restart count, with what the last one left.
	for _, stale := range []string{exitFile, restartsFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	cfg := vz.Config{
		Kernel:    p.cfg.Kernel,
		Initrd:    p.initrd,
		Cmdline:   cmdline,
		CPUs:      uint(max(r.Resources.VCPUs, 0)),                            //nolint:gosec // negative is clamped just before
		Memory:    vz.Memory(uint64(max(bundle.MemoryBound(r.Resources), 0))), //nolint:gosec // negative is clamped just before
		Disk:      filepath.Join(dir, diskFile),
		Network:   r.Address != "",
		MachineID: r.MachineID,
		Restore:   restore,
		Socket:    filepath.Join(dir, socketFile),
		Console:   filepath.Join(dir, consoleFile),
	}
	client, info, err := vz.Start(ctx, p.cfg.Shim, cfg)
	if err != nil {
		return nil, fmt.Errorf("boot sandbox %s: %w", id, err)
	}

	m, err := p.attach(ctx, id, dir, r, client, info)
	if err != nil {
		return nil, errors.Join(err, endShim(client))
	}

	return m, nil
}

// attach puts the guest on the stack, opens the control connection and follows its events and its logs.
func (p *Provider) attach(ctx context.Context, id, dir string, r record, client *vz.Client, info vz.Info) (*machine, error) {
	m := &machine{id: id, dir: dir, client: client, pid: info.PID, machineID: info.MachineID, events: make(chan struct{})}

	if r.Address != "" && p.cfg.Stack == nil {
		return nil, fmt.Errorf("sandbox %s has an address and the provider no stack to carry it", id)
	}
	if r.Address != "" {
		link, err := p.linkOf(client, r)
		if err != nil {
			return nil, fmt.Errorf("attach the network of sandbox %s: %w", id, err)
		}
		m.link = link
	}

	connectCtx, cancel := context.WithTimeout(ctx, startGrace)
	defer cancel()
	control, err := supervisor.Connect(connectCtx, m.dial)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w", id, err), m.closeLink())
	}
	m.control = control

	state, err := control.Next()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: read the supervisor state: %w", id, err), m.close())
	}
	if state.Kind != supervisor.KindState {
		return nil, errors.Join(fmt.Errorf("sandbox %s: the supervisor opened with a %q message, not its state", id, state.Kind), m.close())
	}
	m.started = state.Ready

	// The guest holds the entrypoint's output until a logs connection is open, so it is open before any run.
	logs, err := m.dial(ctx, supervisor.LogsPort)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: open the logs connection: %w", id, err), m.close())
	}
	out, err := os.OpenFile(filepath.Join(dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: open the log: %w", id, err), logs.Close(), m.close())
	}

	pumpCtx, cancelPump := context.WithCancel(context.Background())
	m.cancel = cancelPump
	go p.follow(m)
	go m.followLogs(pumpCtx, logs, out)

	p.mu.Lock()
	p.machines[id] = m
	p.mu.Unlock()

	return m, nil
}

func (p *Provider) linkOf(client *vz.Client, r record) (*netstack.Link, error) {
	prefix, err := netip.ParsePrefix(r.Address)
	if err != nil {
		return nil, fmt.Errorf("parse the recorded address: %w", err)
	}
	frames, err := client.Network()
	if err != nil {
		return nil, err
	}
	link, err := p.cfg.Stack.Attach(frames, prefix.Addr())
	if err != nil {
		return nil, errors.Join(err, frames.Close())
	}

	return link, nil
}

// readdress gives the guest the address the record names, so a fresh boot and a restored fork both take it.
func (m *machine) readdress(r record) error {
	if r.Address == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(r.Address)
	if err != nil {
		return fmt.Errorf("parse the recorded address: %w", err)
	}
	address := supervisor.Address{
		Interface: "eth0", IP: prefix.Addr().String(), Prefix: prefix.Bits(), Gateway: r.Gateway,
		Nameservers: r.Nameservers, Hostname: r.Hostname,
	}
	if err := m.control.Readdress(address); err != nil {
		return fmt.Errorf("sandbox %s: address the guest: %w", m.id, err)
	}

	return nil
}

// follow lands every event the guest sends where the file readers look, until the control connection ends.
func (p *Provider) follow(m *machine) {
	defer close(m.events)
	for {
		event, err := m.control.Next()
		if err != nil {
			p.mu.Lock()
			m.gone = true
			p.mu.Unlock()

			return
		}
		if err := p.record(m, event); err != nil {
			p.mu.Lock()
			if m.lost == nil {
				m.lost = err
			}
			p.mu.Unlock()
		}
	}
}

func (p *Provider) record(m *machine, event supervisor.Message) error {
	switch event.Kind {
	case supervisor.KindReady, supervisor.KindState:
		p.mu.Lock()
		m.started = m.started || event.Ready
		p.mu.Unlock()
	case supervisor.KindExit:
		if event.Exit == nil {
			return errors.New("an exit event carries no status")
		}

		return supervisor.AppendExit(filepath.Join(m.dir, exitFile), *event.Exit)
	case supervisor.KindRestarts:
		if event.Restarts == nil {
			return errors.New("a restarts event carries no count")
		}

		return supervisor.WriteRestarts(filepath.Join(m.dir, restartsFile), *event.Restarts)
	}

	return nil
}

// followLogs appends what the open logs connection carries to the log file, until the guest ends it.
func (m *machine) followLogs(ctx context.Context, logs net.Conn, out *os.File) {
	defer out.Close()
	opened := func(context.Context, uint32) (net.Conn, error) { return logs, nil }
	if err := supervisor.Logs(ctx, opened, out); err != nil && !errors.Is(err, io.EOF) {
		// The guest ends the connection when it powers off, which is the normal end of a log.
		fmt.Fprintf(os.Stderr, "vz: sandbox %s: %v\n", m.id, err)
	}
}

// close ends what this process holds of the shim; the shim itself, and its VM, are the stop's business.
func (m *machine) close() error {
	if m.cancel != nil {
		m.cancel()
	}
	var err error
	if m.control != nil {
		err = m.control.Close()
	}

	return errors.Join(err, m.closeLink())
}

func (m *machine) closeLink() error {
	if m.link == nil {
		return nil
	}
	link := m.link
	m.link = nil

	return link.Close()
}

// endShim force-stops the VM behind a client that answered, for a boot the provider could not finish.
func endShim(client *vz.Client) error {
	if _, err := client.Stop(); err != nil && !absent(err) {
		return fmt.Errorf("stop the vm after a failed boot: %w", err)
	}

	return nil
}

// awaitGone polls the shim until it stops answering, which is the VM powered off and the shim exited.
func (m *machine) awaitGone(ctx context.Context, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		_, err := m.client.State()
		if absent(err) {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for sandbox %s to power off: %w", m.id, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// status is what the shim and the guest say now: created until the entrypoint forked, running after.
func (m *machine) status(p *Provider) models.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.gone {
		return models.Status{Exists: true, State: models.StateStopped}
	}
	state := models.StateCreated
	if m.started {
		state = models.StateRunning
	}

	return models.Status{Exists: true, State: state, PID: m.pid}
}
