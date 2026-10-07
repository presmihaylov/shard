package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/vsock"
	"github.com/presmihaylov/shard/services/supervisor"
)

// transport is the vsock mode: the entrypoint comes over control, each exec is a connection, the output goes down logs.
type transport struct {
	g *guest
	// control is the host's live control connection; nil between two, and the state replays on the next.
	control   net.Conn
	controlMu sync.Mutex
	// attached wakes a death report waiting for its first host; one token, since a report reads the connection itself.
	attached chan struct{}
	logs     *logSink
	// rekey reseeds the guest crng; nil in a test on a Linux host, whose crng is the host's own.
	rekey func([]byte) error
	// setClock sets the guest wall clock; nil in a test on a Linux host, whose clock is the host's own.
	setClock func(int64) error
	// freezing puts one freeze and its answer before the next, so a freeze undone for want of a host never undoes a later one.
	freezing sync.Mutex
	// bound is the sandbox cgroup a freeze stops before it holds the root; nil off a VM.
	bound *os.File
	// root is the disk a freeze holds; nil off a VM.
	root *os.File
	// endSent says the host heard the policy end, which waits for its ack of the app's last output; under controlMu.
	endSent bool
	// forced marks a stop the grace outran, whose disk is left as the kill left it.
	forced atomic.Bool
}

// capbsetEnv marks the re-exec, so the second image knows the bounding set is already shrunk and the disk is the root.
const capbsetEnv = "SHARD_INIT_CAPBSET"

// serveTransport is the whole of -transport: move onto the root disk, listen, and supervise until the stop.
func serveTransport(name string, boot guestBoot) error {
	listen, err := listenerFor(name)
	if err != nil {
		return err
	}
	// The re-exec in confine runs this again, over a root disk already moved onto.
	if boot.set() && os.Getenv(capbsetEnv) == "" {
		if err := bootGuest(boot); err != nil {
			return failBoot(listen, fmt.Errorf("%w: %w", errSupervisor, err))
		}
	}
	// The boundary is for the VM; a test runs the same mode unprivileged and is never PID 1.
	var bound, root *os.File
	if os.Getpid() == 1 {
		bound, err = confine()
		if err != nil {
			return failBoot(listen, fmt.Errorf("%w: %w", errSupervisor, err))
		}
		root, err = rootDisk()
		if err != nil {
			return fmt.Errorf("%w: %w", errSupervisor, err)
		}
	}

	listeners := make([]net.Listener, 0, 4)
	for _, port := range []uint32{supervisor.ControlPort, supervisor.ExecPort, supervisor.LogsPort, supervisor.ForwardPort} {
		l, err := listen(port)
		if err != nil {
			return fmt.Errorf("%w: %w", errSupervisor, err)
		}
		r := &retrying{Listener: l}
		defer r.Close()
		listeners = append(listeners, r)
	}

	logs, err := newLogSink()
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	t := &transport{logs: logs, attached: make(chan struct{}, 1), bound: bound, root: root}
	t.g = newGuest(t, restartPolicy{})
	t.g.bound = bound
	// Only a VM has the bound and a crng of its own; a test on a Linux host runs unconfined and would read its own cgroup.
	if boot.set() {
		t.g.oomProbe = oomKilledGuest
		t.rekey = reseed
		t.setClock = setClock
	}
	go t.acceptControl(listeners[0])
	go t.acceptExec(listeners[1])
	go logs.accept(listeners[2])
	go acceptForward(listeners[3])

	if err := t.g.supervise(); err != nil {
		return t.fail(fmt.Errorf("%w: %w", errSupervisor, err))
	}

	if err := t.seal(freezeRoot); err != nil {
		return t.fail(fmt.Errorf("%w: %w", errSupervisor, err))
	}
	// The stop is done, so the VM has nothing left to run; a guest that went is what the host waits for.
	if err := powerOff(boot.Reboot); err != nil {
		return t.fail(fmt.Errorf("%w: %w", errSupervisor, err))
	}

	return nil
}

// A host that dials while the guest boots is at most this far from attaching, so a death waits this long for it.
var failureGrace = 10 * time.Second

// A freeze that outlasts this has a disk it cannot settle, and the stop goes on without it rather than hang.
var sealGrace = 5 * time.Second

// seal skips a forced stop, which never freezes, so its dirty disk is one a grow refuses by name (shard ruling 790da96b).
func (t *transport) seal(freeze func(*os.File) error) error {
	if t.forced.Load() {
		return nil
	}

	return sealRoot(t.root, freeze)
}

// sealRoot flushes, then freezes the root last of all, so a clean stop leaves a disk with no journal to replay that a host can grow (SHARD-476).
func sealRoot(root *os.File, freeze func(*os.File) error) error {
	if err := syncDisk(); err != nil {
		return err
	}
	frozen := make(chan error, 1)
	go func() { frozen <- freeze(root) }()
	select {
	case err := <-frozen:
		return err
	case <-time.After(sealGrace):
		return fmt.Errorf("freeze the root: no answer within %s", sealGrace)
	}
}

// fail carries the supervisor's own death to the host before the exit 125 halts the VM, where runsc wait would read the code on gVisor.
func (t *transport) fail(err error) error {
	deadline := time.After(failureGrace)
	report := supervisor.Message{Kind: supervisor.KindSupervisorFailed, Error: err.Error(), Exit: &models.ExitStatus{Code: models.SupervisorFailedExitCode}}
	for {
		// A failed write already forgot that host; the next one gets the report.
		if heard, sendErr := t.tell(report); heard && sendErr == nil {
			return err
		}
		select {
		case <-t.attached:
		// The guest loop is over, so a late attach replays the state here or blocks forever.
		case command := <-t.g.commands:
			command()
		case <-deadline:
			return fmt.Errorf("%w; no host attached to hear it", err)
		}
	}
}

// failBoot carries a death from before the listeners exist to the first host that dials, as the message its state would open with (SHARD-416).
func failBoot(listen func(uint32) (net.Listener, error), err error) error {
	l, listenErr := listen(supervisor.ControlPort)
	if listenErr != nil {
		return fmt.Errorf("%w; listen for a host to hear it: %w", err, listenErr)
	}
	type accepted struct {
		conn net.Conn
		err  error
	}
	got := make(chan accepted, 1)
	go func() {
		conn, err := l.Accept()
		got <- accepted{conn: conn, err: err}
	}()

	var a accepted
	select {
	case a = <-got:
	case <-time.After(failureGrace):
		return errors.Join(fmt.Errorf("%w; no host attached to hear it", err), l.Close())
	}
	if a.err != nil {
		return errors.Join(fmt.Errorf("%w; accept a host to hear it: %w", err, a.err), l.Close())
	}
	report := supervisor.Message{Kind: supervisor.KindSupervisorFailed, Error: err.Error(), Exit: &models.ExitStatus{Code: models.SupervisorFailedExitCode}}
	if writeErr := supervisor.WriteMessage(a.conn, report); writeErr != nil {
		return errors.Join(fmt.Errorf("%w; tell the host: %w", err, writeErr), a.conn.Close(), l.Close())
	}
	// The halt follows the exit at once and can drop bytes still in flight, so the host hanging up is what says it read them.
	if deadlineErr := a.conn.SetReadDeadline(time.Now().Add(failureGrace)); deadlineErr != nil {
		return errors.Join(fmt.Errorf("%w; wait for the host to hang up: %w", err, deadlineErr), a.conn.Close(), l.Close())
	}
	if _, readErr := io.Copy(io.Discard, a.conn); readErr != nil {
		return errors.Join(fmt.Errorf("%w; wait for the host to hang up: %w", err, readErr), a.conn.Close(), l.Close())
	}

	return errors.Join(err, a.conn.Close(), l.Close())
}

// detach forgets the control connection, all of them for nil, so a report waits for the next host instead of a dead one.
func (t *transport) detach(conn net.Conn) {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	if t.control == conn {
		t.control = nil
	}
}

// listenerFor picks the socket family: vsock in a VM, unix sockets under a directory in a test.
func listenerFor(name string) (func(uint32) (net.Listener, error), error) {
	if name == "vsock" {
		return vsock.Listen, nil
	}
	if dir, ok := strings.CutPrefix(name, "unix:"); ok {
		return func(port uint32) (net.Listener, error) {
			return net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
		}, nil
	}

	return nil, fmt.Errorf("-transport must be vsock or unix:<dir>, got %q", name)
}

// acceptControl serves one host at a time: a new connection replaces the old one and hears the state first.
func (t *transport) acceptControl(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a control connection:", err)

			return
		}
		if err := t.attach(conn); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: replay the state:", err)
			_ = conn.Close()

			continue
		}
		go t.serveControl(conn)
	}
}

// attach swaps the host in and replays the state on the guest's goroutine, so no event can land between the two.
func (t *transport) attach(conn net.Conn) error {
	var err error
	t.g.run(func() {
		t.controlMu.Lock()
		defer t.controlMu.Unlock()

		if t.control != nil {
			_ = t.control.Close()
		}
		count := t.g.count
		count.Ended = count.Ended && t.endSent
		state := supervisor.Message{Kind: supervisor.KindState, Ready: t.g.started, Exit: t.g.lastExit, Restarts: &count, OOM: t.g.oom, Frozen: t.g.frozen.Load() != nil, Logs: supervisor.LogsVersion, FreezesOverlay: true, Ports: true}
		err = supervisor.WriteMessage(conn, state)
		if err != nil {
			t.control = nil

			return
		}
		t.control = conn
		select {
		case t.attached <- struct{}{}:
		default:
		}
	})

	return err
}

// send writes one message to the host, or drops it when no host is attached: the state replays on the next.
func (t *transport) send(m supervisor.Message) error {
	_, err := t.tell(m)

	return err
}

// tell writes one message to the attached host, and says whether there was one to write to.
func (t *transport) tell(m supervisor.Message) (bool, error) {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	if t.control == nil {
		return false, nil
	}
	err := supervisor.WriteMessage(t.control, m)
	// Forgotten under the same lock, so a host that attached meanwhile is never the one cleared.
	if err != nil {
		t.control = nil
	}

	return true, err
}

func (t *transport) ready() error { return t.send(supervisor.Message{Kind: supervisor.KindReady}) }

func (t *transport) exited(exit models.ExitStatus) error {
	return t.send(supervisor.Message{Kind: supervisor.KindExit, Exit: &exit})
}

// oomKilled is the one report that must land, so it says when nobody is attached instead of dropping the message.
func (t *transport) oomKilled() error {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	if t.control == nil {
		return errNoHost
	}
	if err := supervisor.WriteMessage(t.control, supervisor.Message{Kind: supervisor.KindOOM}); err != nil {
		return fmt.Errorf("%w: %w", errNoHost, err)
	}

	return nil
}

// restarted holds the end back until the host acks the app's last output, so a run that reads the end has read every byte.
func (t *transport) restarted(count models.RestartCount) error {
	m := supervisor.Message{Kind: supervisor.KindRestarts, Restarts: &count}
	if !count.Ended {
		return t.send(m)
	}
	mark, err := t.logs.end()
	if err != nil {
		return err
	}
	go func() {
		t.logs.landed(mark)
		t.controlMu.Lock()
		defer t.controlMu.Unlock()
		t.endSent = true
		if t.control == nil {
			return
		}
		// A host that missed the end reads it in the replay, so a failed write is reported and never fatal (AGENTS.md).
		if err := supervisor.WriteMessage(t.control, m); err != nil {
			t.control = nil
			fmt.Fprintln(os.Stderr, "shard-init: report the end of the app:", err)
		}
	}()

	return nil
}

// serveControl takes the host's messages until it hangs up, and answers each with done or failure.
func (t *transport) serveControl(conn net.Conn) {
	defer t.detach(conn)
	r := bufio.NewReader(conn)
	for {
		var m supervisor.Message
		err := supervisor.ReadMessage(r, &m)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: control:", err)

			return
		}
		if m.Kind == supervisor.KindFreeze {
			t.freeze(conn, m.ID, m.Verb)

			continue
		}
		if m.Kind == supervisor.KindKill {
			t.forceStop(conn, m.ID)

			continue
		}
		if m.Kind == supervisor.KindStop {
			t.stop(conn, m.ID)

			continue
		}
		t.answer(conn, m.ID, t.handle(m))
	}
}

// answer carries the request's id back on the connection that asked, and says whether it went; a host replaced meanwhile never sees another's reply.
func (t *transport) answer(conn net.Conn, id int, err error) bool {
	reply := supervisor.Message{Kind: supervisor.KindDone, ID: id}
	if err != nil {
		reply = supervisor.Message{Kind: supervisor.KindFailure, ID: id, Error: err.Error()}
	}

	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if t.control != conn {
		return false
	}
	if err := supervisor.WriteMessage(conn, reply); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return false
	}

	return true
}

// freeze holds the root for a pause; a host replaced before the answer may have read the root unfrozen off its replay, so the freeze is undone.
func (t *transport) freeze(conn net.Conn, id int, verb string) {
	t.freezing.Lock()
	defer t.freezing.Unlock()

	err := t.hold(verb, func() error { return freezeGuest(t.bound, t.root) })
	if t.answer(conn, id, err) || err != nil {
		return
	}
	if err := t.thaw(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: thaw a freeze no host heard:", err)
	}
}

// handle does what one control message asks, on the guest's goroutine where it touches its state.
func (t *transport) handle(m supervisor.Message) error {
	switch m.Kind {
	case supervisor.KindRun:
		if m.Run == nil {
			return errors.New("a run message names no entrypoint")
		}

		return t.launch(*m.Run)
	case supervisor.KindSignal:
		sig, err := signalOf(m.Signal)
		if err != nil {
			return err
		}

		return t.g.signal(m.PID, sig)
	case supervisor.KindStopApp:
		var err error
		t.g.run(func() { err = t.g.stopApp(m.Force) })

		return err
	case supervisor.KindReaddress:
		if m.Address == nil {
			return errors.New("a readdress message names no address")
		}

		return applyAddress(*m.Address)
	case supervisor.KindReseed:
		if len(m.Seed) < supervisor.SeedSize {
			return fmt.Errorf("a reseed message carries %d bytes, under the %d a crng key takes", len(m.Seed), supervisor.SeedSize)
		}

		if m.Now <= 0 {
			return errors.New("a reseed message carries no host time")
		}
		if t.setClock != nil {
			if err := t.setClock(m.Now); err != nil {
				return err
			}
		}
		if t.rekey == nil {
			return nil
		}

		return t.rekey(m.Seed)
	case supervisor.KindThaw:
		// A host thaws a kill whose cut was lost, so the guest runs on and its next clean stop seals.
		t.forced.Store(false)

		return t.thaw()
	default:
		return fmt.Errorf("the host sent a %q message, which the guest does not take", m.Kind)
	}
}

// stop answers before the guest acts on it: with nothing to forward to the guest goes at once, and the host would read only its EOF (SHARD-483).
func (t *transport) stop(conn net.Conn, id int) {
	// A kill an earlier stop's lost cut left behind is not this stop's, so only a kill within this one skips the seal.
	t.forced.Store(false)
	// A frozen root would hold the entrypoint's last writes.
	t.answer(conn, id, t.thaw())
	// The stop takes the same path a Linux host's SIGTERM does, so one loop owns the grace.
	t.g.stopSignals <- syscall.SIGTERM
}

// freezeGuest stops the guest's processes, then holds the root: a writer the root held first would sleep where no cgroup freeze reaches it.
func freezeGuest(bound, root *os.File) error {
	if err := freezeBound(bound); err != nil {
		return err
	}
	if err := freezeRoot(root); err != nil {
		return errors.Join(err, thawBound(bound))
	}

	return nil
}

// forceStop ends a stop the grace outran: it kills the entrypoint, freezes the rest and flushes, so the host's cut loses nothing.
// A host replaced before the answer may have read the guest unfrozen off its replay, so the freeze is undone, as a pause's is.
func (t *transport) forceStop(conn net.Conn, id int) {
	t.forced.Store(true)
	t.freezing.Lock()
	defer t.freezing.Unlock()

	err := t.killAndFreeze()
	if t.answer(conn, id, err) || err != nil {
		return
	}
	if err := t.thaw(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init: thaw a kill no host heard:", err)
	}
}

// killAndFreeze kills the entrypoint, holds every exec and child so none dirties the disk, then flushes it before the cut.
func (t *transport) killAndFreeze() error {
	err := t.hold("stop", func() error {
		// A gone entrypoint ends nothing here: the published freeze, not a power off, is what a lost cut recovers from.
		if _, err := t.g.stop(syscall.SIGKILL); err != nil {
			return err
		}

		return freezeBound(t.bound)
	})
	if err != nil {
		return err
	}

	return syncDisk()
}

// hold freezes on the guest's goroutine and marks the verb there, so no child start lands between the two and blocks it.
func (t *transport) hold(verb string, freeze func() error) error {
	var err error
	t.g.run(func() {
		err = freeze()
		if err == nil {
			t.g.frozen.Store(&verb)
		}
	})

	return err
}

// thaw lets the root take writes before the guest's processes run again, so none wakes into a held write.
func (t *transport) thaw() error {
	if err := errors.Join(thawRoot(t.root), thawBound(t.bound)); err != nil {
		return err
	}
	t.g.frozen.Store(nil)

	return nil
}

// launch starts the entrypoint the host resolved, once; its output is the log pipe from the first byte.
func (t *transport) launch(spec supervisor.RunSpec) error {
	credential, err := credentialOf(spec.User, spec.Groups)
	if err != nil {
		return err
	}
	env, err := withHome("/", spec.Env, credential)
	if err != nil {
		return err
	}

	restart, err := parseRestart(string(nonEmpty(spec.Restart, models.RestartNo)), spec.Retries, nonEmpty(spec.Backoff, defaultBackoff), nonEmpty(spec.Reset, defaultReset))
	if err != nil {
		return err
	}

	if spec.Trust != nil {
		if err := writeTrustIn("/", *spec.Trust); err != nil {
			return err
		}
	}
	ep := entrypoint{argv: spec.Argv, env: env, dir: spec.WorkDir, credential: credential, out: t.logs.pipe}
	t.g.run(func() {
		if t.g.started {
			err = errors.New("the entrypoint already runs")

			return
		}
		t.g.restart = restart
		err = t.g.launch(ep)
	})

	return err
}

func nonEmpty[T comparable](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}

	return value
}

// credentialOf takes the ids the host resolved; an empty user keeps the supervisor's own, root.
func credentialOf(user string, groups []uint32) (*syscall.Credential, error) {
	if user == "" {
		return nil, nil
	}

	credential, err := parseCredential(user, "")
	if err != nil {
		return nil, err
	}
	credential.Groups = groups

	return credential, nil
}

// signalOf takes the two names the API allows, so a guest pid gets nothing the host would not send.
func signalOf(name string) (syscall.Signal, error) {
	switch name {
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	default:
		return 0, fmt.Errorf("the signal %q is not one of TERM or KILL", name)
	}
}

// logHold is the most output the guest keeps for a host that has not acked it; a full hold blocks the entrypoint on its pipe.
const logHold = 1 << 20

// logSink keeps the entrypoint's output until a host acks it, so a host that comes back resumes where its log file ends.
type logSink struct {
	pipe *os.File
	// read is the pipe's read end, which only copy reads, under mu, so every byte is in the pipe or in held.
	read syscall.RawConn
	mu   sync.Mutex
	cond *sync.Cond
	// held is the output no host has acked yet, and its first byte is output byte from.
	held []byte
	from uint64
	conn net.Conn
	// stopped is a host whose log refused the output, so until the next host nothing waits for an ack.
	stopped bool
}

func newLogSink() (*logSink, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open the log pipe: %w", err)
	}

	read, err := r.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("open the log pipe: %w", err)
	}
	s := &logSink{pipe: w, read: read, held: make([]byte, 0, logHold)}
	s.cond = sync.NewCond(&s.mu)
	go s.copy()

	return s, nil
}

func (s *logSink) accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a logs connection:", err)

			return
		}

		// An ack still in flight on the old connection is dropped with it, so what the new host is offered holds until it answers.
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.conn = conn
		s.stopped = false
		from, to := s.from, s.from+uint64(len(s.held))
		s.cond.Broadcast()
		s.mu.Unlock()
		go s.serve(conn, from, to)
	}
}

// copy holds each chunk of the pipe for the host, and waits for its acks while the hold is full.
func (s *logSink) copy() {
	for {
		s.mu.Lock()
		for len(s.held) == cap(s.held) {
			s.cond.Wait()
		}
		s.mu.Unlock()

		var n int
		var readErr error
		err := s.read.Read(func(fd uintptr) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			n, readErr = syscall.Read(int(fd), s.held[len(s.held):cap(s.held)])
			if errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, syscall.EINTR) {
				return false
			}
			if readErr == nil {
				s.held = s.held[:len(s.held)+n]
				s.cond.Broadcast()
			}

			return true
		})
		// shard-init holds the write end, so the pipe never ends and a failed read is the last thing it reports.
		if err := errors.Join(err, readErr); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: read the log pipe:", err)

			return
		}
		if n == 0 {
			return
		}
	}
}

// end is the output byte after the last one written so far: what the hold has, and what the pipe still holds.
func (s *logSink) end() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var queued int
	var ioctlErr error
	err := s.read.Control(func(fd uintptr) { queued, ioctlErr = unix.IoctlGetInt(int(fd), fionread) })
	if err := errors.Join(err, ioctlErr); err != nil {
		return 0, fmt.Errorf("measure the log pipe: %w", err)
	}

	return s.from + uint64(len(s.held)) + uint64(queued), nil //nolint:gosec // a byte count is never negative
}

// landed waits until a host acks the output before byte mark, which its log file then holds, or says its log stopped.
func (s *logSink) landed(mark uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.from < mark && !s.stopped {
		s.cond.Wait()
	}
}

// serve offers the host the output bytes [from, to), sends on from the one it answers, and lets go of what it acks.
func (s *logSink) serve(conn net.Conn, from, to uint64) {
	defer s.drop(conn)

	if _, err := conn.Write(supervisor.LogsHeader(from, to)); err != nil {
		return
	}
	var at uint64
	if err := binary.Read(conn, binary.BigEndian, &at); err != nil {
		return
	}
	if at == supervisor.LogsStopped {
		s.stop(conn)

		return
	}
	if at < from || at > to {
		fmt.Fprintf(os.Stderr, "shard-init: the host resumes the logs at %d, outside the held %d..%d\n", at, from, to)

		return
	}
	// The host's file already holds what it skips, which is an ack no write will send.
	if err := s.release(conn, at); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return
	}
	go s.acks(conn)

	buf := make([]byte, 32<<10)
	for {
		chunk, ok := s.next(conn, at, buf)
		if !ok {
			return
		}
		if _, err := conn.Write(chunk); err != nil {
			return
		}
		at += uint64(len(chunk))
	}
}

// next copies the held output from byte at into buf once there is some, and says false once conn is no longer the live one.
func (s *logSink) next(conn net.Conn, at uint64, buf []byte) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.conn == conn && at == s.from+uint64(len(s.held)) {
		s.cond.Wait()
	}
	// A host that acked past what it was sent has nothing left here to send from.
	if s.conn != conn || at < s.from {
		return nil, false
	}

	return buf[:copy(buf, s.held[at-s.from:])], true
}

func (s *logSink) acks(conn net.Conn) {
	defer s.drop(conn)

	for {
		var ack uint64
		if err := binary.Read(conn, binary.BigEndian, &ack); err != nil {
			return
		}
		if ack == supervisor.LogsStopped {
			s.stop(conn)

			return
		}
		if err := s.release(conn, ack); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init:", err)

			return
		}
	}
}

// release lets go of the output before byte ack, which the host's log file now holds.
func (s *logSink) release(conn net.Conn, ack uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != conn {
		return nil
	}
	if ack < s.from || ack > s.from+uint64(len(s.held)) {
		return fmt.Errorf("the host acked the logs at %d, outside the held %d..%d", ack, s.from, s.from+uint64(len(s.held)))
	}
	s.held = s.held[:copy(s.held, s.held[ack-s.from:])]
	s.from = ack
	s.cond.Broadcast()

	return nil
}

// stop lets the end go without the ack of a host whose log refused the output.
func (s *logSink) stop(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != conn {
		return
	}
	s.stopped = true
	s.cond.Broadcast()
}

// drop ends conn, unless a newer host already took its place.
func (s *logSink) drop(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != conn {
		return
	}
	_ = conn.Close()
	s.conn = nil
	s.cond.Broadcast()
}
