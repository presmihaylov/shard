package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// machine is one live vmm: its client and the control connection to shard-init.
type machine struct {
	id     string
	dir    string
	client *fcapi.Client
	pid    int
	// control is the stream to shard-init, replaced when a dropped one is dialed again.
	control atomic.Pointer[supervisor.Control]
	// closed says this process let the vmm go, so a stream that ends after it is not dialed again.
	closed atomic.Bool
	// swap orders a replacement against close, so no stream is put in after the vmm was let go.
	swap   sync.Mutex
	cancel context.CancelFunc

	// started is what the guest last said: the entrypoint forked, so the sandbox runs.
	started bool
	// gone is set by the event loop once the vmm no longer runs the VM, so a status needs no socket round trip.
	gone bool
	// lost is the first exit or restart event the loop could not persist; the files say nothing true after it.
	lost error
}

// dial is the supervisor's Dialer over the vmm: one vsock connection per call.
func (m *machine) dial(_ context.Context, port uint32) (net.Conn, error) {
	return m.client.Connect(port)
}

// lookup finds the sandbox's vmm, held or adopted by its socket, and returns nil when none answers.
func (p *Provider) lookup(ctx context.Context, id, dir string) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	p.mu.Unlock()
	if held {
		return m, nil
	}

	client, info, err := fcapi.Adopt(filepath.Join(dir, socketFile), filepath.Join(dir, vsockFile))
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return p.attach(ctx, id, dir, client, info)
}

// absent is a socket with no vmm behind it: never made, or its owner exited and the path stayed.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// release lets a vmm whose guest has gone finish exiting, and forgets it, so a new boot can claim the socket.
func (p *Provider) release(ctx context.Context, m *machine) error {
	if m == nil {
		return nil
	}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the vmm of sandbox %s still answers %s after its guest went", m.id, killGrace)
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

// boot starts a vmm for the sandbox over its image and its own overlay, and attaches to the guest once it answers.
func (p *Provider) boot(ctx context.Context, id, dir string, r record) (*machine, error) {
	// The next run must not answer a wait, or a restart count, with what the last one left.
	for _, stale := range []string{exitFile, restartsFile, oomFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	cfg := fcapi.Config{
		Kernel:    p.cfg.Kernel,
		Initrd:    p.initrd,
		Cmdline:   cmdline,
		VCPUs:     vcpus(r.Resources.VCPUs),
		MemoryMiB: r.Resources.MemoryMiB,
		Drives: []fcapi.Drive{
			{ID: "base", Path: r.BaseDisk, ReadOnly: true},
			{ID: "overlay", Path: filepath.Join(dir, bundle.OverlayDiskFile)},
		},
		Vsock:   filepath.Join(dir, vsockFile),
		Socket:  filepath.Join(dir, socketFile),
		Console: filepath.Join(dir, consoleFile),
	}
	client, info, err := fcapi.Start(ctx, p.cfg.Binary, cfg)
	if err != nil {
		return nil, fmt.Errorf("boot sandbox %s: %w", id, err)
	}

	m, err := p.attach(ctx, id, dir, client, info)
	if err != nil {
		return nil, errors.Join(err, endVMM(id, client))
	}
	if m == nil {
		return nil, fmt.Errorf("sandbox %s: the guest was killed by its memory bound before it ran", id)
	}

	return m, nil
}

// attach opens the control connection to the guest and follows its events and its logs.
func (p *Provider) attach(ctx context.Context, id, dir string, client *fcapi.Client, info fcapi.Info) (*machine, error) {
	m := &machine{id: id, dir: dir, client: client, pid: info.PID}

	connectCtx, cancel := context.WithTimeout(ctx, startGrace)
	defer cancel()
	control, err := supervisor.Connect(connectCtx, m.dial)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	m.control.Store(control)

	state, err := control.Next()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: read the supervisor state: %w", id, err), m.close())
	}
	if state.Kind != supervisor.KindState {
		return nil, errors.Join(fmt.Errorf("sandbox %s: the supervisor opened with a %q message, not its state", id, state.Kind), m.close())
	}
	if err := p.reconcile(m, state); err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: record the supervisor state: %w", id, err), m.close())
	}
	if state.OOM {
		// The guest kept a kill no host heard; the marker is on disk and it is going, so there is nothing to follow.
		return nil, p.release(ctx, m)
	}

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
	go p.followLogs(pumpCtx, m, logs, out)

	p.mu.Lock()
	p.machines[id] = m
	p.mu.Unlock()

	return m, nil
}

// follow lands every event the guest sends where the file readers look, until the VM is gone.
func (p *Provider) follow(m *machine) {
	for {
		event, err := m.control.Load().Next()
		if err != nil {
			again, err := p.reconnect(m)
			p.keep(m, err)
			if again {
				continue
			}
			p.mu.Lock()
			m.gone = true
			p.mu.Unlock()

			return
		}
		p.keep(m, p.record(m, event))
	}
}

// keep holds the first error the event loop met, which is what a later verb reports.
func (p *Provider) keep(m *machine, err error) {
	if err == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.lost == nil {
		m.lost = err
	}
}

func (p *Provider) record(m *machine, event supervisor.Message) error {
	switch event.Kind {
	case supervisor.KindReady, supervisor.KindState:
		return p.reconcile(m, event)
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
	case supervisor.KindOOM:
		return m.markOOM()
	}

	return nil
}

// markOOM puts the reason on disk and only then tells the guest to go: a host that dies first hears the kill again in the replay.
func (m *machine) markOOM() error {
	if err := os.WriteFile(filepath.Join(m.dir, oomFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark sandbox %s killed by its memory bound: %w", m.id, err)
	}
	if err := m.control.Load().Stop(); err != nil {
		return fmt.Errorf("end sandbox %s after its memory bound: %w", m.id, err)
	}

	return nil
}

// reconnect dials the control stream again after a drop, for as long as the vmm runs the VM: a lost stream is not a dead guest.
func (p *Provider) reconnect(m *machine) (bool, error) {
	for m.alive() {
		conn, err := m.dial(context.Background(), supervisor.ControlPort)
		if err != nil {
			time.Sleep(pollInterval)

			continue
		}
		control := supervisor.ControlOver(conn)
		state, err := control.Next()
		if err != nil || state.Kind != supervisor.KindState {
			// A dial the vmm answers can still land on a transport mid-reset, so a short read is one more try.
			p.keep(m, control.Close())
			time.Sleep(pollInterval)

			continue
		}
		return p.adopt(m, control, state)
	}

	return false, nil
}

// adopt makes control the machine's stream before the replay is reconciled, so a stop the replay calls for goes down the live one.
func (p *Provider) adopt(m *machine, control *supervisor.Control, state supervisor.Message) (bool, error) {
	m.swap.Lock()
	if m.closed.Load() {
		m.swap.Unlock()

		return false, control.Close()
	}
	dropped := m.control.Swap(control)
	m.swap.Unlock()

	return true, errors.Join(p.reconcile(m, state), dropped.Close())
}

// reconcile lands what the replayed state says happened while no stream was open, so no reader waits for an event that is gone.
func (p *Provider) reconcile(m *machine, state supervisor.Message) error {
	p.mu.Lock()
	m.started = m.started || state.Ready
	p.mu.Unlock()
	if state.OOM {
		if err := m.markOOM(); err != nil {
			return err
		}
	}
	if state.Exit != nil {
		path := filepath.Join(m.dir, exitFile)
		last, found, err := bundle.ReadExitStatus(path)
		if err != nil {
			return err
		}
		if !found || last != *state.Exit {
			if err := supervisor.AppendExit(path, *state.Exit); err != nil {
				return err
			}
		}
	}
	if state.Restarts != nil {
		return supervisor.WriteRestarts(filepath.Join(m.dir, restartsFile), *state.Restarts)
	}

	return nil
}

// alive says the vmm still answers with a running VM and this process has not let it go.
func (m *machine) alive() bool {
	if m.closed.Load() {
		return false
	}
	info, err := m.client.State()

	return err == nil && info.State == fcapi.StateRunning
}

// followLogs appends what the logs connection carries to the log file, and opens it again after a drop while the VM runs.
func (p *Provider) followLogs(ctx context.Context, m *machine, logs net.Conn, out io.WriteCloser) {
	defer out.Close()
	sink := &logSink{w: out}
	opened := func(context.Context, uint32) (net.Conn, error) { return logs, nil }
	for {
		err := supervisor.Logs(ctx, opened, sink)
		if ctx.Err() != nil {
			return
		}
		// A file that refuses the log blocks the guest on its output pipe, so every read of the sandbox says so; a redial would not help.
		if sink.err != nil {
			p.keep(m, fmt.Errorf("the log stopped: %w", sink.err))

			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			// The guest ends the connection when it powers off, which is the normal end of a log.
			fmt.Fprintf(os.Stderr, "firecracker: sandbox %s: %v\n", m.id, err)
		}
		if !m.alive() {
			return
		}
		opened = m.dial
		time.Sleep(pollInterval)
	}
}

// logSink keeps the first write failure of the log file, which the connection's own errors would otherwise hide.
type logSink struct {
	w   io.Writer
	err error
}

func (s *logSink) Write(b []byte) (int, error) {
	n, err := s.w.Write(b)
	if err != nil && s.err == nil {
		s.err = err
	}

	return n, err
}

// close ends what this process holds of the vmm; the vmm itself, and its VM, are the stop's business.
func (m *machine) close() error {
	m.swap.Lock()
	defer m.swap.Unlock()
	m.closed.Store(true)
	if m.cancel != nil {
		m.cancel()
	}
	if control := m.control.Load(); control != nil {
		return control.Close()
	}

	return nil
}

// endVMM kills the vmm of a boot the provider could not finish; firecracker has no stop verb, so the kill is the only end.
func endVMM(id string, client *fcapi.Client) error {
	// The create's context may already be canceled, and the vmm must go either way, so the cleanup runs on its own clock.
	ctx, cancel := context.WithTimeout(context.Background(), 2*killGrace)
	defer cancel()
	if err := client.Kill(); err != nil {
		return fmt.Errorf("sandbox %s: end the vmm after a failed boot: %w", id, err)
	}
	m := &machine{id: id, client: client}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the vmm of sandbox %s still answers %s after a kill", id, killGrace)
	}

	return nil
}

// awaitGone polls the vmm until it stops answering, which is the VM powered off and firecracker exited.
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

// status is what the vmm and the guest say now: created until the entrypoint forked, running after.
func (m *machine) status(p *Provider) models.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.gone {
		return models.Status{Exists: true, State: models.StateStopped, OOMKilled: oomKilled(m.dir)}
	}
	state := models.StateCreated
	if m.started {
		state = models.StateRunning
	}

	return models.Status{Exists: true, State: state, PID: m.pid}
}
