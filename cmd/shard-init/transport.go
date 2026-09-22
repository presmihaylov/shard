package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
}

// capbsetEnv marks the re-exec, so the second image knows the bounding set is already shrunk and the disk is the root.
const capbsetEnv = "SHARD_INIT_CAPBSET"

// serveTransport is the whole of -transport: move onto the root disk, listen, and supervise until the stop.
func serveTransport(name, root string) error {
	listen, err := listenerFor(name)
	if err != nil {
		return err
	}
	// The re-exec in confine runs this again, over a root disk already moved onto.
	if root != "" && os.Getenv(capbsetEnv) == "" {
		if err := bootGuest(root); err != nil {
			return fmt.Errorf("%w: %w", errSupervisor, err)
		}
	}
	// The boundary is for the VM; a test runs the same mode unprivileged and is never PID 1.
	if os.Getpid() == 1 {
		if err := confine(); err != nil {
			return fmt.Errorf("%w: %w", errSupervisor, err)
		}
	}

	listeners := make([]net.Listener, 0, 4)
	for _, port := range []uint32{supervisor.ControlPort, supervisor.ExecPort, supervisor.LogsPort, supervisor.FilesPort} {
		l, err := listen(port)
		if err != nil {
			return fmt.Errorf("%w: %w", errSupervisor, err)
		}
		defer l.Close()
		listeners = append(listeners, l)
	}

	logs, err := newLogSink()
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	t := &transport{logs: logs, attached: make(chan struct{}, 1)}
	t.g = newGuest(t, restartPolicy{})
	// Only a VM has the bound; a test on a Linux host runs unconfined and would read its own cgroup.
	if root != "" {
		t.g.oomProbe, t.g.exempt = oomKilledGuest, true
	}
	go t.acceptControl(listeners[0])
	go t.acceptExec(listeners[1])
	go logs.accept(listeners[2])
	go t.acceptFiles(listeners[3])

	if err := t.g.supervise(); err != nil {
		return t.fail(fmt.Errorf("%w: %w", errSupervisor, err))
	}

	// The stop is done, so the VM has nothing left to run; a powered-off guest is what the host waits for.
	if err := powerOff(); err != nil {
		return t.fail(fmt.Errorf("%w: %w", errSupervisor, err))
	}

	return nil
}

// A host that dials while the guest boots is at most this far from attaching, so a death waits this long for it.
var failureGrace = 10 * time.Second

// fail carries the supervisor's own death to the host before the exit 125 halts the VM, where runsc wait would read the code on gVisor.
func (t *transport) fail(err error) error {
	deadline := time.After(failureGrace)
	report := supervisor.Message{Kind: supervisor.KindSupervisorFailed, Error: err.Error(), Exit: &models.ExitStatus{Code: models.SupervisorFailedExitCode}}
	for {
		heard, sendErr := t.tell(report)
		if heard && sendErr == nil {
			return err
		}
		if sendErr != nil {
			// The host went while the guest died; the next one gets the report.
			t.detach(nil)
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

// detach forgets the control connection, all of them for nil, so a report waits for the next host instead of a dead one.
func (t *transport) detach(conn net.Conn) {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	if conn == nil || t.control == conn {
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
		state := supervisor.Message{Kind: supervisor.KindState, Ready: t.g.started, Exit: t.g.lastExit, Restarts: &count, OOM: t.g.oom}
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

	return true, supervisor.WriteMessage(t.control, m)
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

func (t *transport) restarted(count models.RestartCount) error {
	return t.send(supervisor.Message{Kind: supervisor.KindRestarts, Restarts: &count})
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
		t.answer(conn, m.ID, t.handle(m))
	}
}

// answer carries the request's id back on the connection that asked; a host replaced meanwhile never sees another's reply.
func (t *transport) answer(conn net.Conn, id int, err error) {
	reply := supervisor.Message{Kind: supervisor.KindDone, ID: id}
	if err != nil {
		reply = supervisor.Message{Kind: supervisor.KindFailure, ID: id, Error: err.Error()}
	}

	t.controlMu.Lock()
	defer t.controlMu.Unlock()
	if t.control != conn {
		return
	}
	if err := supervisor.WriteMessage(conn, reply); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
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
	case supervisor.KindStop:
		// The stop takes the same path a Linux host's SIGTERM does, so one loop owns the grace.
		return syscall.Kill(os.Getpid(), syscall.SIGTERM)
	case supervisor.KindReaddress:
		if m.Address == nil {
			return errors.New("a readdress message names no address")
		}

		return applyAddress(*m.Address)
	default:
		return fmt.Errorf("the host sent a %q message, which the guest does not take", m.Kind)
	}
}

// launch starts the entrypoint the host resolved, once; its output is the log pipe from the first byte.
func (t *transport) launch(spec supervisor.RunSpec) error {
	if len(spec.Argv) == 0 {
		return errors.New("the run message has no entrypoint")
	}

	credential, err := credentialOf(spec.User, spec.Groups)
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
	ep := entrypoint{argv: spec.Argv, env: spec.Env, dir: spec.WorkDir, credential: credential, out: t.logs.pipe}
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

// logSink copies the entrypoint's pipe to the live logs connection, and waits with no host attached so no byte is lost.
type logSink struct {
	pipe *os.File
	mu   sync.Mutex
	cond *sync.Cond
	conn net.Conn
}

func newLogSink() (*logSink, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open the log pipe: %w", err)
	}

	s := &logSink{pipe: w}
	s.cond = sync.NewCond(&s.mu)
	go s.copy(r)

	return s, nil
}

func (s *logSink) accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: accept a logs connection:", err)

			return
		}

		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.conn = conn
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

// copy moves each chunk to the live connection, and what is left of it to the next one when a write fails midway.
func (s *logSink) copy(r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		s.write(buf[:n])
	}
}

func (s *logSink) write(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(chunk) > 0 {
		for s.conn == nil {
			s.cond.Wait()
		}
		n, err := s.conn.Write(chunk)
		chunk = chunk[n:]
		if err == nil {
			continue
		}
		_ = s.conn.Close()
		s.conn = nil
	}
}
