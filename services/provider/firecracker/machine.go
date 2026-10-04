package firecracker

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
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/pkg/pidpin"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// machine is one live vmm: its client and the control connection to shard-init.
type machine struct {
	id  string
	dir string
	// jail is the vmm's chroot, which goes once the vmm does; empty is a vmm spawned before the jail.
	jail   string
	client *fcapi.Client
	pid    int
	// pinned holds the vmm by a pin taken on a connection it answered, or never answered, so a stop kills that vmm alone.
	pinned *pidpin.Process
	// control is the stream to shard-init, replaced when a dropped one is dialed again.
	control atomic.Pointer[supervisor.Control]
	// closed says this process let the vmm go, so a stream that ends after it is not dialed again.
	closed atomic.Bool
	// swap orders a replacement against close, so no stream is put in after the vmm was let go.
	swap sync.Mutex
	// freezing, taken before swap, holds each freeze and thaw of the guest until the guest answers, so none lands inside another.
	freezing sync.Mutex
	// pausing, under freezing, is a pause that froze the guest and still means to snapshot it.
	pausing bool
	// resetBy is the verb whose snapshot create reset every vsock stream, so its runAgain dials the control stream again; only that verb's goroutine reads it.
	resetBy string
	// holder is the verb that froze the guest, until its runAgain ends; the vmm holds its API while it writes the snapshot, so silence then is that verb at work.
	holder atomic.Pointer[string]
	// logsRound ends the logs stream in use, so a stream the reset killed is dialed again.
	logsRound atomic.Pointer[context.CancelFunc]
	// execs holds each open exec stream, with the verb that cut it, or "" while it runs.
	execs   map[net.Conn]string
	execsMu sync.Mutex
	// freezesOverlay is what the guest said when attached: an older shard-init fails every freeze on the overlay root.
	freezesOverlay bool
	// wholeLog says the vmm's dirty-page log holds every page the guest wrote since this process booted or loaded it, so a Diff is whole (SHARD-458).
	wholeLog bool
	cancel   context.CancelFunc
	// followed is closed once follow has landed the guest's last event, so a stop that saw the vmm go reads all of them (SHARD-290).
	followed chan struct{}

	// started is what the guest last said: the entrypoint forked, so the sandbox runs.
	started bool
	// gone is set by the event loop once the vmm no longer runs the VM, so a status needs no socket round trip.
	gone bool
	// silent is set while the vmm has not answered a probe within its bound; only stop ends it (SHARD-392, SHARD-439).
	silent bool
	// asking closes once the one state request out to a silent vmm ends; nil when none is out.
	asking chan struct{}
	// lost is the first exit or restart event the loop could not persist; the files say nothing true after it.
	lost error
	// refusals logs the control lines the guest sent past the bound, which the reconnect otherwise hides.
	refusals *supervisor.Refusals
}

// dial is the supervisor's Dialer over the vmm: one vsock connection per call.
func (m *machine) dial(_ context.Context, port uint32) (net.Conn, error) {
	return m.client.Connect(port)
}

// lookup finds the sandbox's vmm, held or adopted by the socket its record names, and returns nil when none answers.
func (p *Provider) lookup(ctx context.Context, id, dir string, r record) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	silent, found := p.unadopted[id]
	p.mu.Unlock()
	if held {
		// A probe would queue behind the snapshot and read the verb at work as a vmm that does not answer.
		if m.holder.Load() != nil {
			return m, nil
		}
		bound := adoptBound
		// A held vmm already silent has its one request out, so a lookup waits only the floor on it.
		if p.waiting(m) {
			bound = probeFloor
		}
		p.probe(ctx, m, bound)

		return m, nil
	}
	// The one request to a silent vmm is already out, so a lookup waits only the floor on it, and a stop's opening probe stays short.
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
	m, settled, err := p.settled(id, silent)
	if settled || err != nil {
		return m, err
	}
	socket, vsock := r.sockets(dir)
	began := time.Now()
	probe, cancel := context.WithTimeout(ctx, adoptBound)
	client, info, pin, err := fcapi.AdoptPinned(probe, socket, vsock)
	cancel()
	if absent(err) {
		return nil, nil
	}
	// A vmm silent for the whole bound may still thaw and give the same VM back, so it reads unresponsive and only stop kills it (SHARD-392).
	if pin != nil && time.Since(began) >= adoptBound && ctx.Err() == nil && !p.spared(id) {
		return p.unanswered(id, dir, r.Jail, fcapi.Open(socket, vsock), pin)
	}
	if pin != nil {
		return nil, errors.Join(err, pin.Close())
	}
	if err != nil {
		return nil, err
	}
	// A vmm that booted and loaded nothing has no guest, so an attach would wait on it until every verb timed out (SHARD-295).
	if info.State == fcapi.StateNotStarted {
		return nil, p.endJudged(id, client, info.PID, r.Jail)
	}
	// A paused VM to adopt is a pause cut before it ended the vmm, or a fork cut before its resume.
	if info.State == fcapi.StatePaused {
		restoring, err := exists(filepath.Join(dir, restoringFile))
		if err != nil {
			return nil, fmt.Errorf("sandbox %s: read the restore marker: %w", id, err)
		}
		// A fork's restore was in flight, and its guest holds the source's address: end it, never resume it (SHARD-321).
		if restoring {
			return nil, p.endJudged(id, client, info.PID, r.Jail)
		}
		capturing, err := exists(filepath.Join(dir, captureFile))
		if err != nil {
			return nil, fmt.Errorf("sandbox %s: read the capture marker: %w", id, err)
		}
		frozen, err := p.installed(id)
		if err != nil {
			return nil, fmt.Errorf("sandbox %s: %w", id, err)
		}
		// A pause cut after its install left the guest frozen beside a complete snapshot, and a resume would run it past that; a capture's source runs on (SHARD-427, SHARD-462).
		if frozen && !capturing {
			return nil, p.endJudged(id, client, info.PID, r.Jail)
		}
		// A pause cut before its install leaves a paused VM with nothing to stand for it, and its stopped guest answers no handshake.
		if err := client.Resume(); err != nil {
			return nil, fmt.Errorf("sandbox %s: resume the vm a cut pause left paused: %w", id, err)
		}
	}

	m, err = p.attach(ctx, id, dir, r.Jail, client, info)
	if err != nil || m == nil {
		return m, err
	}
	// A daemon cut between a restore's attach and its reseed left the guest on the snapshot's key, and no other step gives it one.
	if err := m.reseed(ctx); err != nil {
		return nil, errors.Join(err, p.end(ctx, m))
	}
	// The attach thawed a guest the cut capture left frozen, so the source runs again and the marker is spent.
	if err := os.Remove(filepath.Join(dir, captureFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("sandbox %s: clear the capture marker: %w", id, err)
	}

	return m, nil
}

// unfreeze reseeds a guest a snapshot left frozen, then thaws it.
func (m *machine) unfreeze(ctx context.Context) error {
	if err := m.reseed(ctx); err != nil {
		return err
	}
	if err := m.control.Load().Thaw(ctx); err != nil {
		return fmt.Errorf("sandbox %s: thaw the guest: %w", m.id, err)
	}

	return nil
}

// reseed gives a restored guest a crng key of its own while its marker says it has none; every restore of one snapshot wakes with the same key, and the guest kernel has no vmgenid to rekey it (SHARD-266).
func (m *machine) reseed(ctx context.Context) error {
	marker := filepath.Join(m.dir, reseedFile)
	pending, err := exists(marker)
	if err != nil {
		return fmt.Errorf("sandbox %s: read the reseed marker: %w", m.id, err)
	}
	if !pending {
		return nil
	}
	if err := m.control.Load().Reseed(ctx); err != nil {
		return fmt.Errorf("sandbox %s: reseed the restored guest: %w", m.id, err)
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("sandbox %s: clear the reseed marker: %w", m.id, err)
	}

	return nil
}

// endJudged ends the vmm a read judged dead weight, by the pid it judged; one this process still spawns, or holds since, is left to it.
func (p *Provider) endJudged(id string, client *fcapi.Client, pid int, jail string) error {
	if p.spared(id) {
		return nil
	}

	// The pid is the vmm judged here: the socket may answer for one a spawn began since.
	if err := fcapi.KillPID(pid); err != nil {
		return fmt.Errorf("sandbox %s: end the vmm a read judged: %w", id, err)
	}

	if err := awaitEnded(&machine{id: id, client: client, pid: pid}); err != nil {
		return err
	}
	// A vmm a spawn began since answers from the same jail, which is then its own.
	probe, cancel := context.WithTimeout(context.Background(), probeFloor)
	defer cancel()
	if _, err := client.State(probe); !absent(err) {
		return nil
	}

	return removeJail(jail)
}

// unanswered keeps a vmm an adopt found silent, by the pin on the peer that took its dial, so each later lookup waits on its one request.
func (p *Provider) unanswered(id, dir, jail string, client *fcapi.Client, pin *pidpin.Process) (*machine, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kept, found := p.unadopted[id]; found {
		return kept, pin.Close()
	}
	m := &machine{id: id, dir: dir, jail: jail, client: client, pid: pin.PID(), pinned: pin, silent: true}
	p.unadopted[id] = m

	return m, nil
}

// lookupToStop is lookup for a stop: a held vmm that already missed its probe gets one short probe more, and any other held vmm gets its grace (SHARD-439).
func (p *Provider) lookupToStop(ctx context.Context, id, dir string, r record) (*machine, error) {
	p.mu.Lock()
	m, held := p.machines[id]
	p.mu.Unlock()
	if !held {
		return p.lookup(ctx, id, dir, r)
	}
	if m.status(p).State == models.StateUnresponsive {
		p.probe(ctx, m, probeFloor)
	}

	return m, nil
}

// waiting says a silent vmm has still not answered; an answer, or no vmm left on the socket, lets a fresh adopt decide.
func (p *Provider) waiting(m *machine) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return m.silent
}

// probe waits the bound on the one state request out to the vmm, and starts it when none is: silence marks it unresponsive, and nothing here kills it.
func (p *Provider) probe(ctx context.Context, m *machine, bound time.Duration) {
	p.mu.Lock()
	if m.gone || m.closed.Load() {
		p.mu.Unlock()

		return
	}
	// A frozen vmm accepts nothing, and every dial waits in its socket queue, so one request at a time keeps that queue from filling.
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

// ask puts the one state request to a silent vmm and holds it past the caller; a read that times out leaves it silent for the next probe.
func (p *Provider) ask(ctx context.Context, m *machine, asking chan struct{}) {
	_, err := m.client.State(ctx)
	p.mu.Lock()
	m.asking = nil
	if err == nil || absent(err) {
		m.silent = false
	}
	p.mu.Unlock()
	close(asking)
}

// claim makes this lookup the one that adopts the sandbox's vmm once any other adopt of it ends, so a thawed vmm is attached once.
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
			return nil, fmt.Errorf("sandbox %s: wait for another adopt of its vmm: %w", id, ctx.Err())
		}
	}
}

// settled is the vmm another lookup adopted, or found silent, while this one waited for the claim; seen is let go, as it answered.
func (p *Provider) settled(id string, seen *machine) (*machine, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, held := p.machines[id]; held {
		return m, true, nil
	}
	m, found := p.unadopted[id]
	if !found {
		return nil, false, nil
	}
	if m != seen {
		return m, true, nil
	}
	delete(p.unadopted, id)

	return nil, false, m.close()
}

// spared is a vmm this process still spawns, or holds since, which a read leaves to its spawn.
func (p *Provider) spared(id string) bool {
	// One look under the lock sees the spawn mark or the machine, whichever side of the attach the spawn is on.
	p.mu.Lock()
	defer p.mu.Unlock()
	_, held := p.machines[id]

	return held || p.spawning[id]
}

// installed says a complete snapshot is where the sandbox's pause writes; the pause verb removes the old one first, so a VM frozen beside it is a pause past its install or a restore before its vCPUs ran.
func (p *Provider) installed(id string) (bool, error) {
	dir, err := p.cfg.Snapshots(id)
	if err != nil {
		return false, fmt.Errorf("find the checkpoint directory: %w", err)
	}
	path := filepath.Join(dir, checkpointFile)
	_, err = os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat the checkpoint %s: %w", path, err)
	}

	return true, nil
}

// absent is a socket with no vmm behind it: never made, or its owner exited and the path stayed.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// exists reports whether a stat found the path; a missing path is no error, any other stat failure is.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	return false, err
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

	return p.settle(ctx, m)
}

// settle lets the vmm go once follow has landed what the guest sent before it went; a boot that never followed has nothing to wait for.
func (p *Provider) settle(ctx context.Context, m *machine) error {
	if m.followed != nil {
		select {
		case <-m.followed:
		case <-ctx.Done():
			return fmt.Errorf("wait for the last events of sandbox %s: %w", m.id, ctx.Err())
		case <-time.After(killGrace):
			return fmt.Errorf("the last events of sandbox %s still land %s after its vmm went", m.id, killGrace)
		}
	}
	p.forget(m)
	if err := errors.Join(m.close(), removeJail(m.jail)); err != nil {
		return err
	}

	return p.lost(m.id)
}

// forget drops the machine and keeps what its loop could not land, which the files would otherwise answer for.
func (p *Provider) forget(m *machine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unadopted[m.id] == m {
		delete(p.unadopted, m.id)
	}
	if p.machines[m.id] != m {
		return
	}
	delete(p.machines, m.id)
	if m.lost != nil {
		p.lostRuns[m.id] = m.lost
	}
}

// spawn marks the sandbox as one this process brings a vmm up for, until the returned done.
func (p *Provider) spawn(id string) (done func()) {
	p.mu.Lock()
	p.spawning[id] = true
	p.mu.Unlock()

	return func() {
		p.mu.Lock()
		delete(p.spawning, id)
		p.mu.Unlock()
	}
}

// boot starts a vmm for the sandbox over its image and its own overlay, and attaches to the guest once it answers.
func (p *Provider) boot(ctx context.Context, id, dir string, r record) (*machine, error) {
	// The next run must not answer a wait, or a restart count, with what the last one left.
	for _, stale := range []string{exitFile, restartsFile, oomFile, supervisorFailedFile, cursorFile} {
		if err := os.Remove(filepath.Join(dir, stale)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("clear %s: %w", stale, err)
		}
	}

	device, err := r.device()
	if err != nil {
		return nil, fmt.Errorf("boot sandbox %s: %w", id, err)
	}
	if err := p.bound(id, r.Resources); err != nil {
		return nil, fmt.Errorf("boot sandbox %s: %w", id, err)
	}
	done := p.spawn(id)
	defer done()
	jail, err := p.jail(id, dir, &r, "")
	if err != nil {
		return nil, fmt.Errorf("boot sandbox %s: %w", id, err)
	}
	cfg := fcapi.Config{
		Kernel:    jailKernel,
		Initrd:    jailInitrd,
		Cmdline:   cmdline,
		VCPUs:     vcpus(r.Resources.VCPUs),
		MemoryMiB: r.Resources.MemoryMiB,
		Drives: []fcapi.Drive{
			{ID: baseDrive, Path: jailBase, ReadOnly: true},
			{ID: overlayDrive, Path: jailOverlay},
		},
		Network: device,
		Vsock:   jailVsock,
		Socket:  apiSocket,
		Console: filepath.Join(dir, consoleFile),
	}
	client, info, err := fcapi.Start(ctx, jail, cfg)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("boot sandbox %s: %w", id, err), removeJail(r.Jail))
	}

	return p.up(ctx, id, dir, r.Jail, client, info)
}

// up attaches to a vmm this provider just spawned, and ends it when there is no guest to attach to.
func (p *Provider) up(ctx context.Context, id, dir, jail string, client *fcapi.Client, info fcapi.Info) (*machine, error) {
	m, err := p.attach(ctx, id, dir, jail, client, info)
	if err != nil {
		// A vmm the kill did not end keeps its jail, so a later lookup can still find it and end it.
		if endErr := endVMM(id, client); endErr != nil {
			return nil, errors.Join(err, endErr)
		}

		return nil, errors.Join(err, removeJail(jail))
	}
	if m == nil {
		return nil, fmt.Errorf("sandbox %s: the guest was killed by its memory bound before it ran", id)
	}
	m.wholeLog = true

	return m, nil
}

// device is the virtio-net device the vmm attaches over the tap the record names; no tap is no device.
func (r record) device() (fcapi.Network, error) {
	if r.Tap == "" {
		return fcapi.Network{}, nil
	}
	prefix, err := netip.ParsePrefix(r.Address)
	if err != nil {
		return fcapi.Network{}, fmt.Errorf("parse the recorded address: %w", err)
	}

	return fcapi.Network{Tap: r.Tap, MAC: guestMAC(prefix.Addr())}, nil
}

// guestMAC derives a locally administered MAC from the leased IPv4, so the bridge sees the same address on every boot.
func guestMAC(address netip.Addr) string {
	v4 := address.As4()

	return fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// readdress gives the guest the address the record names, once the control stream is up; a fork's memory holds the source's, MAC included.
func (m *machine) readdress(ctx context.Context, r record) error {
	if r.Address == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(r.Address)
	if err != nil {
		return fmt.Errorf("parse the recorded address: %w", err)
	}
	address := supervisor.Address{
		Interface: "eth0", MAC: guestMAC(prefix.Addr()), IP: prefix.Addr().String(), Prefix: prefix.Bits(), Gateway: r.Gateway,
		Nameservers: r.Nameservers, Hostname: r.Hostname,
	}
	if err := m.control.Load().Readdress(ctx, address); err != nil {
		return fmt.Errorf("sandbox %s: address the guest: %w", m.id, err)
	}

	return nil
}

// attach opens the control connection to the guest and follows its events and its logs.
func (p *Provider) attach(ctx context.Context, id, dir, jail string, client *fcapi.Client, info fcapi.Info) (*machine, error) {
	// The pin is taken while the vmm answers, so a stop after a later freeze kills this vmm alone (SHARD-439).
	answered, pin, err := client.StatePinned(ctx)
	if err != nil {
		if pin != nil {
			err = errors.Join(err, pin.Close())
		}
		return nil, fmt.Errorf("sandbox %s: pin its vmm: %w", id, err)
	}
	m := &machine{id: id, dir: dir, jail: jail, client: client, pid: info.PID, pinned: pin, refusals: supervisor.NewRefusals(p.cfg.Log, id)}
	if answered.PID != info.PID {
		return nil, errors.Join(fmt.Errorf("sandbox %s: its socket answers for pid %d, not its vmm %d", id, answered.PID, info.PID), m.close())
	}

	connectCtx, cancel := context.WithTimeout(ctx, startGrace)
	defer cancel()
	control, err := supervisor.Connect(connectCtx, m.dial)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: %w", id, err), m.close())
	}
	m.control.Store(control)

	state, err := control.Next()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: read the supervisor state: %w", id, err), m.close())
	}
	if state.Kind == supervisor.KindSupervisorFailed {
		return nil, errors.Join(m.failedAtBoot(state), m.close())
	}
	if state.Kind != supervisor.KindState {
		return nil, errors.Join(fmt.Errorf("sandbox %s: the supervisor opened with a %q message, not its state", id, state.Kind), m.close())
	}
	m.freezesOverlay = state.FreezesOverlay
	// The guest answered, so a fork's restore resumed; clear its marker, or a later pause would read as a cut fork (SHARD-321).
	if err := os.Remove(filepath.Join(dir, restoringFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.Join(fmt.Errorf("sandbox %s: clear the restore marker: %w", id, err), m.close())
	}
	if err := p.reconcile(m, state); err != nil {
		return nil, errors.Join(fmt.Errorf("sandbox %s: record the supervisor state: %w", id, err), m.close())
	}
	if state.OOM {
		// The guest kept a kill no host heard; the marker is on disk and it is going, so there is nothing to follow.
		return nil, p.release(ctx, m)
	}
	// A snapshot holds the guest frozen, so it runs nothing on the saved crng key until the reseed is in and the thaw follows (SHARD-409).
	if state.Frozen {
		if err := m.unfreeze(ctx); err != nil {
			// A guest left frozen never runs again, and an adopter that kept its vmm would retry this on every verb.
			return nil, errors.Join(err, m.close(), endVMM(id, client))
		}
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
	m.followed = make(chan struct{})
	go p.follow(m)
	go p.followLogs(pumpCtx, m, logs, &supervisor.FileLog{File: out, Cursor: filepath.Join(dir, cursorFile), Max: supervisor.MaxLog}, state.Logs)

	p.mu.Lock()
	p.machines[id] = m
	p.mu.Unlock()

	return m, nil
}

// follow lands every event the guest sends where the file readers look, until the VM is gone.
func (p *Provider) follow(m *machine) {
	defer close(m.followed)
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

// markSupervisorFailed lands shard-init's own death as the sandbox exit, with its reason, before the halt takes the guest.
func (m *machine) markSupervisorFailed(event supervisor.Message) error {
	if event.Exit == nil {
		return errors.New("a supervisor-failed event carries no status")
	}
	if err := os.WriteFile(filepath.Join(m.dir, supervisorFailedFile), []byte(supervisor.OneLine(event.Error)), 0o600); err != nil {
		return fmt.Errorf("record why the supervisor of sandbox %s failed: %w", m.id, err)
	}

	return supervisor.WriteExit(filepath.Join(m.dir, exitFile), *event.Exit)
}

// failedAtBoot lands a death from before the guest listened, and makes its reason the answer to the start (SHARD-416).
func (m *machine) failedAtBoot(event supervisor.Message) error {
	if err := m.markSupervisorFailed(event); err != nil {
		return fmt.Errorf("sandbox %s: %w", m.id, err)
	}

	return fmt.Errorf("sandbox %s: shard-init failed at boot with exit %d: %s", m.id, event.Exit.Code, supervisor.OneLine(event.Error))
}

// reconnect dials the control stream again after dropped ends, for as long as the vmm runs the VM: a lost stream is not a dead guest.
func (p *Provider) reconnect(m *machine, dropped *supervisor.Control) (bool, error) {
	for {
		m.freezing.Lock()
		// The snapshot's runAgain put the next stream in, so the follower moves to it and never dials beside it.
		if m.control.Load() != dropped {
			m.freezing.Unlock()

			return true, nil
		}
		pausing := m.pausing
		if !pausing && m.alive() {
			adopted, err := p.dialAgain(m, startGrace)
			m.freezing.Unlock()
			if adopted {
				return true, err
			}
			// A dial the vmm answers can still land on a transport mid-reset, so a short read is one more try; a refusal waits longer.
			time.Sleep(max(pollInterval, m.refusals.Note(err)))

			continue
		}
		m.freezing.Unlock()
		// A snapshot in flight dials once it runs the VM again; a vmm that no longer answers has nothing left to follow.
		if !pausing || m.vmState() == "" {
			return false, nil
		}
		time.Sleep(pollInterval)
	}
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
	// A guest frozen with no pause in flight is a freeze whose answer the drop lost, or a snapshot run again, and nothing else would thaw it.
	if state.Frozen && !m.pausing {
		if err := control.Thaw(context.Background()); err != nil {
			thawed = fmt.Errorf("sandbox %s: thaw the guest: %w", m.id, err)
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

// alive says the vmm still answers with a running VM and this process has not let it go.
func (m *machine) alive() bool {
	return m.vmState() == fcapi.StateRunning
}

// vmState is the state the vmm answers with, or "" once it does not answer or this process let it go.
func (m *machine) vmState() fcapi.State {
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
			fmt.Fprintf(os.Stderr, "firecracker: sandbox %s: %v\n", m.id, err)
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

// close ends what this process holds of the vmm; the vmm itself, and its VM, are the stop's business.
func (m *machine) close() error {
	m.swap.Lock()
	defer m.swap.Unlock()
	m.closed.Store(true)
	if m.cancel != nil {
		m.cancel()
	}
	var errs []error
	if m.pinned != nil {
		errs = append(errs, m.pinned.Close())
	}
	if control := m.control.Load(); control != nil {
		errs = append(errs, closeControl(control))
	}

	return errors.Join(errs...)
}

// endVMM kills the vmm of a boot the provider could not finish; firecracker has no stop verb, so the kill is the only end.
func endVMM(id string, client *fcapi.Client) error {
	if err := client.Kill(); err != nil {
		return fmt.Errorf("sandbox %s: end the vmm after a failed boot: %w", id, err)
	}

	return awaitEnded(&machine{id: id, client: client})
}

// awaitEnded waits out a vmm just killed on its own clock: the verb's context may already be canceled, and the vmm must go either way.
func awaitEnded(m *machine) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*killGrace)
	defer cancel()
	ended, err := m.awaitGone(ctx, killGrace)
	if err != nil {
		return err
	}
	if !ended {
		return fmt.Errorf("the vmm of sandbox %s still answers %s after a kill", m.id, killGrace)
	}

	return nil
}

// awaitGone polls the vmm until it stops answering, which is the VM powered off and firecracker exited.
func (m *machine) awaitGone(ctx context.Context, grace time.Duration) (bool, error) {
	deadline := time.Now().Add(grace)
	for {
		// Each read ends with the wait, so a vmm that takes the dial and never answers costs the grace and not callTimeout (SHARD-388).
		probe, cancel := context.WithTimeout(ctx, max(time.Until(deadline), probeFloor))
		info, err := m.client.State(probe)
		cancel()
		// Another pid on the socket is a vmm begun since, so the one this machine names is gone.
		if absent(err) || (err == nil && m.pid != 0 && info.PID != m.pid) {
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
	// An unadopted vmm has no stream to the guest, so it reads unresponsive until an adopt attaches it, even past an answer.
	if (m.silent && m.holder.Load() == nil) || m.control.Load() == nil {
		return models.Status{Exists: true, State: models.StateUnresponsive, PID: m.pid, Reason: fmt.Sprintf("its vmm (pid %d) did not answer within %s", m.pid, adoptBound)}
	}
	state := models.StateCreated
	if m.started {
		state = models.StateRunning
	}

	return models.Status{Exists: true, State: state, PID: m.pid}
}

// because is what made a sandbox unresponsive, appended to the error of a verb it refuses.
func because(status models.Status) string {
	if status.Reason == "" {
		return ""
	}

	return ": " + status.Reason
}
