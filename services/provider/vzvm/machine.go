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
	"sync"
	"sync/atomic"
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
	// shim is the pid the attach verified with its start time, which a kill uses when a full socket queue refuses the dial that names it (SHARD-423).
	shim vz.Process
	// machineID is what the shim reported, which a record persists for every later boot of the disk.
	machineID string
	// control is the stream to shard-init, replaced when a dropped one is dialed again.
	control atomic.Pointer[supervisor.Control]
	// closed says this process let the shim go, so a stream that ends after it is not dialed again.
	closed atomic.Bool
	// swap orders a replacement against close, so no stream is put in after the shim was let go.
	swap   sync.Mutex
	link   io.Closer
	cancel context.CancelFunc
	// freezing, taken before swap, holds each freeze and thaw of the guest's root until the guest answers, so none lands inside another.
	freezing sync.Mutex
	// pausing, set under freezing and read bare, is a verb that froze the guest's root to pause the VM, until it stops the VM or runs it again.
	pausing atomic.Bool
	// resetBy is the verb whose save reset every vsock stream, so its runAgain dials the control stream again; only that verb's goroutine reads it.
	resetBy string
	// holder is the verb that froze the guest, until its runAgain ends; silence from the shim then is that verb at work.
	holder atomic.Pointer[string]
	// admit orders an exec's dial against a freeze, so no exec stream opens once a verb holds the VM.
	admit sync.RWMutex
	// logsRound ends the logs stream in use, so a stream the reset killed is dialed again.
	logsRound atomic.Pointer[context.CancelFunc]
	// execs holds each open exec stream, with the verb that cut it, or "" while it runs.
	execs   map[net.Conn]string
	execsMu sync.Mutex
	// events closes when the control connection ended, which is the guest gone.
	events chan struct{}

	// started is what the guest last said: the entrypoint forked, so the sandbox runs.
	started bool
	// gone is set by the event loop when the control connection ended, so a status needs no socket round trip.
	gone bool
	// silent is set when the shim missed the probe bound; only stop ends it, and an answer clears it (SHARD-421).
	silent bool
	// asking closes once the one state request out to the shim ends; nil when none is out.
	asking chan struct{}
	// lost is the first exit or restart event the loop could not persist; the files say nothing true after it.
	lost error
	// refusals logs the control lines the guest sent past the bound, which the reconnect otherwise hides.
	refusals *supervisor.Refusals
}

// dial is the supervisor's Dialer over the shim: one vsock connection per call.
func (m *machine) dial(ctx context.Context, port uint32) (net.Conn, error) {
	return m.client.Connect(ctx, port)
}

// lookup finds the sandbox's shim, held or adopted by its socket, and returns nil when none answers.
func (p *Provider) lookup(ctx context.Context, id, dir string, r record) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	silent, found := p.unadopted[id]
	p.mu.Unlock()
	if held {
		// A probe would queue behind the save and read the verb at work as a shim that does not answer.
		if m.holder.Load() != nil {
			return m, nil
		}
		p.probe(ctx, m, adoptBound)

		return m, nil
	}
	if found {
		p.probe(ctx, silent, adoptBound)
		if p.waiting(silent) {
			return silent, nil
		}
	}

	release, err := p.claim(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if m, settled := p.settled(id, silent); settled {
		return m, nil
	}
	socket := filepath.Join(dir, socketFile)
	began := time.Now()
	probe, cancel := context.WithTimeout(ctx, adoptBound)
	client, info, err := vz.Adopt(probe, socket)
	cancel()
	// A frozen shim's full socket queue refuses the dial as a dead shim's stale socket does, so the recorded pid tells the two apart (SHARD-423).
	if refused(err) {
		return p.unanswered(id, dir, socket)
	}
	if absent(err) {
		return nil, nil
	}
	// A shim silent for the whole bound may still thaw and give the same VM back, so it reads unresponsive and only stop kills it (SHARD-422).
	if err != nil && time.Since(began) >= adoptBound && ctx.Err() == nil {
		return p.unanswered(id, dir, socket)
	}
	if err != nil {
		return nil, err
	}
	if err := resumeCut(id, dir, client, info, &r); err != nil {
		return nil, err
	}

	return p.attachAdopted(ctx, id, dir, r, client, info)
}

// unanswered keeps a shim an adopt found silent, by the pid behind its socket, so each later lookup waits on its one request.
func (p *Provider) unanswered(id, dir, socket string) (*machine, error) {
	client := vz.Open(socket)
	shim, err := p.silentShim(client, dir)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: read the pid of its silent shim: %w", id, err)
	}
	if shim.PID == 0 {
		return nil, nil
	}
	m := &machine{id: id, dir: dir, client: client, shim: shim, silent: true}
	p.mu.Lock()
	defer p.mu.Unlock()
	if kept, found := p.unadopted[id]; found {
		return kept, nil
	}
	p.unadopted[id] = m

	return m, nil
}

// silentShim is the shim behind the socket, or the one the last attach recorded while it runs, as a full socket queue refuses the dial; zero is none.
func (p *Provider) silentShim(client *vz.Client, dir string) (vz.Process, error) {
	pid, err := client.PID()
	if refused(err) {
		return p.recordedShim(dir)
	}
	if absent(err) {
		return vz.Process{}, nil
	}
	if err != nil {
		return vz.Process{}, err
	}
	shim, err := vz.Identify(pid)
	if errors.Is(err, syscall.ESRCH) {
		return vz.Process{}, nil
	}

	return shim, err
}

// recordedShim is the shim the last attach recorded while it still runs, and zero once it does not.
func (p *Provider) recordedShim(dir string) (vz.Process, error) {
	shim, err := p.readShim(dir)
	if err != nil {
		return vz.Process{}, err
	}
	alive, err := shim.Alive()
	if err != nil || !alive {
		return vz.Process{}, err
	}

	return shim, nil
}

// waiting says the one request to an unadopted shim is still out past its bound; any other end lets a fresh adopt decide.
func (p *Provider) waiting(m *machine) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return m.silent && m.asking != nil
}

// lookupToStop is lookup whose adoption ends by the grace; a shim too frozen to answer is cut by its socket at once (SHARD-349).
func (p *Provider) lookupToStop(ctx context.Context, id, dir string, r record, grace time.Duration) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	silent, found := p.unadopted[id]
	p.mu.Unlock()
	// A shim that missed its probe gets one short probe more, and the stop kills it by its pid with no grace if that is silent too (SHARD-421).
	if held && m.status(p).State == models.StateUnresponsive {
		p.probe(ctx, m, probeFloor)
	}
	if held {
		return m, nil
	}
	if found {
		p.probe(ctx, silent, probeFloor)
		if p.waiting(silent) {
			return silent, nil
		}
	}

	release, err := p.claim(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if m, settled := p.settled(id, silent); settled {
		return m, nil
	}
	socket := filepath.Join(dir, socketFile)
	probe, cancel := context.WithTimeout(ctx, max(grace, probeFloor))
	client, info, err := vz.Adopt(probe, socket)
	cancel()
	if refused(err) {
		return p.unanswered(id, dir, socket)
	}
	if absent(err) {
		return nil, nil
	}
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("stop sandbox %s: %w", id, ctx.Err())
	}
	if err != nil {
		shim, err := p.readShim(dir)
		if err != nil {
			return nil, err
		}

		return nil, p.end(ctx, &machine{id: id, dir: dir, client: vz.Open(socket), shim: shim})
	}
	// A VM that will not run again cannot take the guest's stop either, so it is cut by its socket.
	if err := resumeCut(id, dir, client, info, &r); err != nil {
		shim, err := vz.Identify(info.PID)
		if err != nil {
			return nil, fmt.Errorf("sandbox %s: %w", id, err)
		}

		return nil, p.end(ctx, &machine{id: id, dir: dir, client: client, shim: shim})
	}

	return p.attachAdopted(ctx, id, dir, r, client, info)
}

// attachAdopted attaches to a shim met by its socket; one whose guest does not attach would fail every later lookup, stop and rm the same way, so it ends here (SHARD-577).
func (p *Provider) attachAdopted(ctx context.Context, id, dir string, r record, client *vz.Client, info vz.Info) (*machine, error) {
	m, err := p.attach(ctx, id, dir, r, client, info, false, adoptBound)
	// A lookup cut short may be what failed the attach, so its guest is not judged.
	if (errors.Is(err, errNoGuest) || errors.Is(err, errBootFailed)) && ctx.Err() == nil {
		return nil, p.endUnattached(ctx, id, dir, err)
	}

	return m, err
}

// endUnattached ends an adopted shim whose guest does not attach and puts why on file, so the sandbox reads stopped with its reason.
func (p *Provider) endUnattached(ctx context.Context, id, dir string, cause error) error {
	// The shim the attach identified, so a kill never reaches a process on its pid since.
	shim, err := p.readShim(dir)
	if err != nil {
		return errors.Join(cause, err)
	}
	if err := p.end(ctx, &machine{id: id, dir: dir, client: vz.Open(filepath.Join(dir, socketFile)), shim: shim}); err != nil {
		return errors.Join(cause, fmt.Errorf("sandbox %s: end the shim whose guest does not attach: %w", id, err))
	}
	// A boot failure put its own reason and exit on file.
	if errors.Is(cause, errBootFailed) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, supervisorFailedFile), []byte(supervisor.OneLine(cause.Error())), 0o600); err != nil {
		return fmt.Errorf("sandbox %s: record why its shim ended: %w", id, err)
	}

	return nil
}

// claim makes this lookup the one that adopts the sandbox's shim once any other adopt of it ends, so the shim is attached once (SHARD-422).
func (p *Provider) claim(ctx context.Context, id string) (func(), error) {
	for {
		p.mu.Lock()
		busy, taken := p.adopting[id]
		if !taken {
			done := make(chan struct{})
			p.adopting[id] = done
			p.mu.Unlock()

			return func() {
				p.mu.Lock()
				delete(p.adopting, id)
				p.mu.Unlock()
				close(done)
			}, nil
		}
		p.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, fmt.Errorf("sandbox %s: wait for another adopt of its shim: %w", id, ctx.Err())
		}
	}
}

// settled is the shim another lookup adopted, or found silent, while this one waited for the claim; seen is let go, as its request ended.
func (p *Provider) settled(id string, seen *machine) (*machine, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, held := p.machines[id]; held {
		return m, true
	}
	m, found := p.unadopted[id]
	if !found {
		return nil, false
	}
	if m != seen {
		return m, true
	}
	delete(p.unadopted, id)

	return nil, false
}

// resumeCut runs a VM a daemon killed inside a pause left paused, before or after its record said so, and attach then thaws its root (SHARD-375, SHARD-402).
func resumeCut(id, dir string, client *vz.Client, info vz.Info, r *record) error {
	if info.State != vz.StatePaused {
		return nil
	}
	// The pause never returned, so the service still says running; Pauses stays, so the next save outranks one the swap installed.
	if r.Paused {
		r.Paused = false
		if err := writeRecord(dir, *r); err != nil {
			return fmt.Errorf("sandbox %s: undo the paused record a cut pause left: %w", id, err)
		}
	}
	if _, err := client.Resume(); err != nil {
		return fmt.Errorf("resume sandbox %s, which a cut pause left paused: %w", id, err)
	}

	return nil
}

// absent is a socket that takes no dial: never made, its owner exited and the path went with it, or a frozen shim's queue is full.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || refused(err) || errors.Is(err, syscall.ENOENT)
}

// refused is a socket file that takes no dial: a shim died and left it, or a frozen shim's queue is full (SHARD-423).
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
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
	closeDown(m)

	return nil
}

// probe waits the bound for the shim's answer: silence marks it unresponsive, an answer clears that, and neither kills it (SHARD-421).
func (p *Provider) probe(ctx context.Context, m *machine, bound time.Duration) {
	p.mu.Lock()
	if m.gone || m.closed.Load() {
		p.mu.Unlock()

		return
	}
	// A frozen shim accepts nothing, and a full socket queue refuses a dial as if no shim were there, so one request waits for it.
	asking := m.asking
	if asking == nil {
		asking = make(chan struct{})
		m.asking = asking
		go p.ask(context.WithoutCancel(ctx), m, asking)
	}
	p.mu.Unlock()

	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	// The request cleared the mark on an answer; a failed one is the shim gone, which the event loop reports.
	case <-asking:
	case <-ctx.Done():
	case <-timer.C:
		p.mu.Lock()
		if m.asking == asking {
			m.silent = true
		}
		p.mu.Unlock()
	}
}

// ask puts the one state request to the shim and holds it past the caller until the shim answers or dies; stop kills one that never answers.
func (p *Provider) ask(ctx context.Context, m *machine, asking chan struct{}) {
	_, err := m.client.Await(ctx)
	frozen := refused(err) && m.lingers()
	p.mu.Lock()
	m.asking = nil
	if err == nil {
		m.silent = false
	}
	// A full socket queue refuses the dial as a dead shim's socket does, so a shim that still runs by its pid stays silent (SHARD-423).
	if frozen {
		m.silent = true
	}
	p.mu.Unlock()
	close(asking)
}

// lingers says the shim the attach verified still runs; a pid the kernel will not read is not proven gone, and a stop's kill reports why.
func (m *machine) lingers() bool {
	alive, err := m.shim.Alive()

	return alive || err != nil
}

func (p *Provider) forget(m *machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.machines[m.id] == m {
		delete(p.machines, m.id)
	}
	if p.unadopted[m.id] == m {
		delete(p.unadopted, m.id)
	}
}

// settle waits for the last events of a guest that went, so a death it reported on the way down is on disk before the stop returns.
func (p *Provider) settle(ctx context.Context, m *machine) error {
	if m.events != nil {
		select {
		case <-m.events:
		case <-ctx.Done():
			return fmt.Errorf("wait for the last events of sandbox %s: %w", m.id, ctx.Err())
		case <-time.After(killGrace):
			return fmt.Errorf("the last events of sandbox %s still land %s after its guest went", m.id, killGrace)
		}
	}
	lost := p.lost(m.id)
	p.forget(m)
	closeDown(m)

	return lost
}

// boot starts a shim for the sandbox over its own disk, and attaches to the guest once it answers.
func (p *Provider) boot(ctx context.Context, id, dir string, r record, restore string) (*machine, error) {
	// The next run must not answer a wait, or a restart count, with what the last one left.
	stales := []string{exitFile, restartsFile, oomFile, supervisorFailedFile}
	// A restored guest still holds the output its cursor places; a fresh one starts its output again.
	if restore == "" {
		stales = append(stales, cursorFile)
	}
	for _, stale := range stales {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	cfg := vz.Config{
		Kernel:    p.cfg.Kernel,
		Initrd:    p.initrd,
		Cmdline:   cmdline,
		CPUs:      uint(max(r.Resources.VCPUs, 0)),                 //nolint:gosec // negative is clamped just before
		Memory:    uint64(max(bundle.MemoryBound(r.Resources), 0)), //nolint:gosec // negative is clamped just before
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

	m, err := p.attach(ctx, id, dir, r, client, info, restore != "", startGrace)
	if err != nil {
		return nil, errors.Join(err, endShim(id, client, info.PID))
	}
	if m == nil {
		return nil, fmt.Errorf("sandbox %s: the restored guest was killed by its memory bound", id)
	}

	if restore == "" {
		return m, nil
	}
	// Only a running sandbox is ever paused, so what a checkpoint brings back is running and Status says so.
	p.mu.Lock()
	m.started = true
	p.mu.Unlock()

	return m, nil
}

// errNoGuest marks an attach the guest itself failed, which no later attach to the same shim gets past.
var errNoGuest = errors.New("its guest does not attach")

// errBootFailed marks a guest whose shard-init died at boot, with its death on file.
var errBootFailed = errors.New("shard-init failed at boot")

// attach puts the guest on the stack, opens the control connection within grace and follows its events and its logs.
func (p *Provider) attach(ctx context.Context, id, dir string, r record, client *vz.Client, info vz.Info, restored bool, grace time.Duration) (*machine, error) {
	shim, err := vz.Identify(info.PID)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	if err := writeJSON(filepath.Join(dir, shimFile), shim); err != nil {
		return nil, err
	}
	m := &machine{id: id, dir: dir, client: client, shim: shim, machineID: info.MachineID, events: make(chan struct{}), refusals: supervisor.NewRefusals(p.cfg.Log, id)}

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

	connectCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	control, err := supervisor.Connect(connectCtx, m.dial)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w: %w", id, errNoGuest, err), m.closeLink())
	}
	m.control.Store(control)

	// A connected guest can still stay silent past the create deadline.
	closed := make(chan error, 1)
	stopRead := context.AfterFunc(connectCtx, func() { closed <- closeControl(control) })
	state, err := control.Next()
	if !stopRead() {
		err = errors.Join(context.Cause(connectCtx), err, <-closed)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w: read the supervisor state: %w", id, errNoGuest, err), m.close())
	}
	if state.Kind == supervisor.KindSupervisorFailed {
		return nil, errors.Join(m.failedAtBoot(state), m.close())
	}
	if state.Kind != supervisor.KindState {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w: the supervisor opened with a %q message, not its state", id, errNoGuest, state.Kind), m.close())
	}
	if err := p.reconcile(m, state); err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: record the supervisor state: %w", id, err), m.close())
	}
	if state.OOM {
		// The guest kept a kill no host heard; the marker is on disk and it is going, so there is nothing to follow.
		return nil, p.release(ctx, m)
	}
	// Every restore of one save wakes with the same crng key and VZ has no vmgenid; an older guest restores unfrozen and still needs it.
	if restored || state.Frozen {
		if err := control.Reseed(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("sandbox %s: reseed the restored guest: %w", id, err), m.close())
		}
	}
	// A guest restored from a pause, or left by a daemon that died mid-pause, holds its processes frozen until a host thaws it, after the reseed.
	if state.Frozen {
		if err := control.Thaw(ctx); err != nil {
			return nil, errors.Join(fmt.Errorf("sandbox %s: thaw the guest's root: %w", id, err), m.close())
		}
	}

	// The guest holds the entrypoint's output until a logs connection is open, so it is open before any run.
	logs, err := m.dial(ctx, supervisor.LogsPort)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w: open the logs connection: %w", id, errNoGuest, err), m.close())
	}
	out, err := os.OpenFile(filepath.Join(dir, logFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: open the log: %w", id, err), logs.Close(), m.close())
	}

	pumpCtx, cancelPump := context.WithCancel(context.Background())
	m.cancel = cancelPump
	go p.follow(m)
	go p.followLogs(pumpCtx, m, logs, &supervisor.FileLog{File: out, Cursor: filepath.Join(dir, cursorFile), Max: supervisor.MaxLog}, state.Logs)

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
func (m *machine) readdress(ctx context.Context, r record) error {
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
	if err := m.control.Load().Readdress(ctx, address); err != nil {
		return fmt.Errorf("sandbox %s: address the guest: %w", m.id, err)
	}

	return nil
}

// follow lands every event the guest sends where the file readers look, until the guest is gone.
func (p *Provider) follow(m *machine) {
	defer close(m.events)
	for {
		control := m.control.Load()
		event, err := control.Next()
		if err != nil {
			// A refused stream waits before the redial, so a guest that floods every stream cannot keep the daemon dialing (SHARD-408).
			time.Sleep(m.refusals.Note(err))
			again, err := p.reconnect(m, control)
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

		return supervisor.WriteExit(filepath.Join(m.dir, exitFile), *event.Exit)
	case supervisor.KindRestarts:
		if event.Restarts == nil {
			return errors.New("a restarts event carries no count")
		}

		return supervisor.WriteRestarts(filepath.Join(m.dir, restartsFile), *event.Restarts)
	case supervisor.KindOOM:
		return m.markOOM()
	case supervisor.KindSupervisorFailed:
		return m.markSupervisorFailed(event)
	}

	return nil
}

// markOOM puts the reason on disk and only then tells the guest to go: a host that dies first hears the kill again in the replay.
func (m *machine) markOOM() error {
	if err := os.WriteFile(filepath.Join(m.dir, oomFile), nil, 0o600); err != nil {
		return fmt.Errorf("mark sandbox %s killed by its memory bound: %w", m.id, err)
	}
	if err := m.control.Load().Stop(context.Background()); err != nil {
		return fmt.Errorf("end sandbox %s after its memory bound: %w", m.id, err)
	}

	return nil
}

// markSupervisorFailed lands shard-init's own death as the sandbox exit, with the reason it gave.
func (m *machine) markSupervisorFailed(event supervisor.Message) error {
	if event.Exit == nil {
		return errors.New("a supervisor-failed event carries no status")
	}
	if err := os.WriteFile(filepath.Join(m.dir, supervisorFailedFile), []byte(supervisor.OneLine(event.Error)), 0o600); err != nil {
		return fmt.Errorf("record why the supervisor of sandbox %s failed: %w", m.id, err)
	}

	return supervisor.WriteExit(filepath.Join(m.dir, exitFile), *event.Exit)
}

// failedAtBoot lands a death from before the guest listened as the sandbox exit, and makes its reason the answer to the start (SHARD-418).
func (m *machine) failedAtBoot(event supervisor.Message) error {
	if err := m.markSupervisorFailed(event); err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
	}

	return fmt.Errorf("sandbox %s: %w with exit %d: %s", m.id, errBootFailed, event.Exit.Code, supervisor.OneLine(event.Error))
}

// reconnect dials the control stream again after dropped ends, which a sleep of the host can cause, while the shim says the VM runs.
func (p *Provider) reconnect(m *machine, dropped *supervisor.Control) (bool, error) {
	deadline := time.Now().Add(startGrace)
	for {
		next, err := p.reconnectOnce(m, dropped, deadline)
		switch next {
		case reconnectAdopted:
			return true, err
		case reconnectGone:
			return false, nil
		case reconnectSaving:
			// The VM is paused for the save, so the grace runs from its end.
			deadline = time.Now().Add(startGrace)
			time.Sleep(pollInterval)
		case reconnectRetry:
			// A dial the shim answers can still land on a transport mid-reset, so a short read is one more try; a refusal waits longer.
			time.Sleep(max(pollInterval, m.refusals.Note(err)))
		}
	}
}

// reconnectStep is what one try of reconnect leaves the loop to do.
type reconnectStep int

const (
	reconnectAdopted reconnectStep = iota
	reconnectGone
	reconnectSaving
	reconnectRetry
)

// reconnectOnce is one try of reconnect under freezing, before deadline.
func (p *Provider) reconnectOnce(m *machine, dropped *supervisor.Control, deadline time.Time) (reconnectStep, error) {
	m.freezing.Lock()
	defer m.freezing.Unlock()
	// The save's runAgain put the next stream in, so the follower moves to it and never dials beside it.
	if m.control.Load() != dropped {
		return reconnectAdopted, nil
	}
	state := m.vmState()
	// A pause still in flight leaves the root frozen on the stream this puts in, since adopt thaws none under it.
	if state == vz.StateRunning && time.Now().Before(deadline) {
		adopted, err := p.dialAgain(m, time.Until(deadline))
		if adopted {
			return reconnectAdopted, err
		}

		return reconnectRetry, err
	}
	// A paused guest answers no dial, so a save in flight dials once it runs the VM again; anything else has nothing left to follow.
	if m.pausing.Load() && state == vz.StatePaused {
		return reconnectSaving, nil
	}

	return reconnectGone, nil
}

// dialAgain dials the control stream once and takes it in once the guest replays its state there within bound; the caller holds freezing.
func (p *Provider) dialAgain(m *machine, bound time.Duration) (bool, error) {
	conn, err := m.dial(context.Background(), supervisor.ControlPort)
	if err != nil {
		return false, fmt.Errorf("sandbox %s: dial the control stream: %w", m.id, err)
	}
	control := supervisor.ControlOver(conn)
	type replay struct {
		state supervisor.Message
		err   error
	}
	replayed := make(chan replay, 1)
	go func() {
		state, err := control.Next()
		replayed <- replay{state, err}
	}()
	select {
	case <-time.After(bound):
		return false, errors.Join(fmt.Errorf("sandbox %s: the guest replayed no state within %s", m.id, bound), control.Close())
	case r := <-replayed:
		if r.err != nil {
			return false, errors.Join(fmt.Errorf("sandbox %s: read the replayed state: %w", m.id, r.err), control.Close())
		}
		if r.state.Kind != supervisor.KindState {
			return false, errors.Join(fmt.Errorf("sandbox %s: the guest opened with a %q message, not its state", m.id, r.state.Kind), control.Close())
		}

		return p.adopt(m, control, r.state)
	}
}

// adopt makes control the machine's stream before the replay is reconciled, so a stop the replay calls for goes down the live one; the caller holds freezing.
func (p *Provider) adopt(m *machine, control *supervisor.Control, state supervisor.Message) (bool, error) {
	m.swap.Lock()
	if m.closed.Load() {
		m.swap.Unlock()

		return false, control.Close()
	}
	dropped := m.control.Swap(control)
	m.swap.Unlock()

	var thawed error
	// A root frozen with no pause in flight is a freeze whose answer the drop lost, or a save run again, and nothing else would thaw it.
	if state.Frozen && !m.pausing.Load() {
		if p.recovering != nil {
			p.recovering()
		}
		if err := control.Thaw(context.Background()); err != nil {
			thawed = fmt.Errorf("sandbox %s: thaw the guest's root: %w", m.id, err)
		}
	}

	return true, errors.Join(thawed, p.reconcile(m, state), closeControl(dropped))
}

// closeControl ends a control stream that a redial which ran out may have ended already.
func closeControl(control *supervisor.Control) error {
	if err := control.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}

	return nil
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
			if err := supervisor.WriteExit(path, *state.Exit); err != nil {
				return err
			}
		}
	}
	if state.Restarts != nil {
		return supervisor.WriteRestarts(filepath.Join(m.dir, restartsFile), *state.Restarts)
	}

	return nil
}

// alive says the shim still answers with a running VM and this process has not let it go.
func (m *machine) alive() bool {
	return m.vmState() == vz.StateRunning
}

// vmState is the state the shim answers with, or "" once it does not answer or this process let it go.
func (m *machine) vmState() vz.State {
	if m.closed.Load() {
		return ""
	}
	info, err := m.client.State(context.Background())
	if err != nil {
		return ""
	}

	return info.State
}

// followLogs appends what the logs connection carries to the log file, in the protocol the guest's state named, and opens it again after a drop while the VM runs.
func (p *Provider) followLogs(ctx context.Context, m *machine, logs net.Conn, out *supervisor.FileLog, version int) {
	defer out.Close()
	opened := func(context.Context, uint32) (net.Conn, error) { return logs, nil }
	for {
		round, end := context.WithCancel(ctx)
		m.logsRound.Store(&end)
		err := supervisor.Logs(round, opened, out, version)
		end()
		if ctx.Err() != nil {
			return
		}
		// A file that refuses the log blocks the guest on its output pipe, so every read of the sandbox says so; a redial would not help.
		if out.Err != nil {
			p.keep(m, fmt.Errorf("the log stopped: %w", out.Err))

			return
		}
		if errors.Is(err, supervisor.ErrLogsVersion) {
			p.keep(m, fmt.Errorf("the log stopped: %w", err))

			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			// The guest ends the connection when it powers off, which is the normal end of a log.
			fmt.Fprintf(os.Stderr, "vz: sandbox %s: %v\n", m.id, err)
		}
		// A save the fork holds paused can drop the stream, and the VM runs again once it ends.
		for m.holder.Load() != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval):
			}
		}
		if !m.alive() {
			return
		}
		opened = m.dial
		time.Sleep(pollInterval)
	}
}

// kickLogs ends the logs stream in use, which a host that only reads would wait on for good once a reset killed it.
func (m *machine) kickLogs() {
	if end := m.logsRound.Load(); end != nil {
		(*end)()
	}
}

// close ends what this process holds of the shim; the shim itself, and its VM, are the stop's business.
func (m *machine) close() error {
	m.swap.Lock()
	m.closed.Store(true)
	if m.cancel != nil {
		m.cancel()
	}
	var err error
	if control := m.control.Load(); control != nil {
		err = closeControl(control)
	}
	m.swap.Unlock()

	return errors.Join(err, m.closeLink())
}

// closeDown closes a machine whose VM is down: a fault is logged, never a failed stop, so the record says stopped (SHARD-389).
func closeDown(m *machine) {
	if err := m.close(); err != nil {
		// Log and continue, decided by Pres on 2026-10-03: the VM is already down, so failing the stop would only strand the record at running.
		fmt.Fprintf(os.Stderr, "vz: sandbox %s stopped, and closing what the daemon held of it failed: %v\n", m.id, err)
	}
}

func (m *machine) closeLink() error {
	if m.link == nil {
		return nil
	}
	link := m.link
	m.link = nil

	return link.Close()
}

// endShim force-stops the VM of a boot the provider could not finish and sees its shim out; one that stays past the grace is killed by pid.
func endShim(id string, client *vz.Client, pid int) error {
	// The create's context may already be canceled, and the shim must go either way, so the cleanup runs on its own clock.
	ctx, cancel := context.WithTimeout(context.Background(), 3*killGrace)
	defer cancel()
	// The identity comes before the stop, so a refused dial after it reads the shim gone only once this pid is (SHARD-423).
	shim, err := vz.Identify(pid)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("end the shim of sandbox %s after a failed boot: %w", id, err)
	}
	var stopErr error
	if _, err := client.Stop(ctx); err != nil && !absent(err) {
		stopErr = fmt.Errorf("stop the vm after a failed boot: %w", err)
	}
	m := &machine{id: id, client: client, shim: shim}
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return errors.Join(stopErr, err)
	}
	if ended {
		return stopErr
	}
	if err := shim.Kill(); err != nil {
		return errors.Join(stopErr, fmt.Errorf("kill the shim of sandbox %s after a failed boot: %w", id, err))
	}
	ended, err = m.awaitGone(ctx, killGrace)
	if err != nil {
		return errors.Join(stopErr, err)
	}
	if !ended {
		return errors.Join(stopErr, fmt.Errorf("the shim %d of sandbox %s still answers %s after a kill", pid, id, killGrace))
	}

	return stopErr
}

// awaitGone polls the shim until it stops answering, which is the VM powered off and the shim exited.
func (m *machine) awaitGone(ctx context.Context, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		// Each read ends with the wait, so a shim that takes the dial and never answers costs the grace and not callTimeout (SHARD-349).
		probeEnd := deadline
		if floor := time.Now().Add(probeFloor); floor.After(probeEnd) {
			probeEnd = floor
		}
		probe, cancel := context.WithDeadline(ctx, probeEnd)
		_, err := m.client.State(probe)
		cancel()
		if absent(err) && !refused(err) {
			return true, nil
		}
		// A full socket queue refuses the dial too, so a refused shim is gone only once its pid is (SHARD-423).
		if refused(err) {
			alive, err := m.shim.Alive()
			if err != nil {
				return false, fmt.Errorf("sandbox %s: %w", m.id, err)
			}
			if !alive {
				return true, nil
			}
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
		return models.Status{Exists: true, State: models.StateStopped, OOMKilled: oomKilled(m.dir)}
	}
	// An unadopted shim has no stream to the guest, so it reads unresponsive until an adopt attaches it, even past an answer.
	if (m.silent && m.holder.Load() == nil) || m.control.Load() == nil {
		return models.Status{Exists: true, State: models.StateUnresponsive, PID: m.shim.PID, Reason: fmt.Sprintf("its shim (pid %d) did not answer within %s", m.shim.PID, adoptBound)}
	}
	state := models.StateCreated
	if m.started {
		state = models.StateRunning
	}

	return models.Status{Exists: true, State: state, PID: m.shim.PID}
}

// because is what made a sandbox unresponsive, appended to the error of a verb it refuses.
func because(status models.Status) string {
	if status.Reason == "" {
		return ""
	}

	return ": " + status.Reason
}
