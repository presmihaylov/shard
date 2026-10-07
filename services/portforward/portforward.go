// Package portforward listens on the host ports an operator forwarded and carries each connection to its port inside a sandbox.
package portforward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/splice"
)

// Dial opens one stream to a port on the loopback of a running sandbox; models.Provider.DialPort is one.
type Dial func(ctx context.Context, id string, port uint16) (net.Conn, error)

// dialBudget bounds the reach into the sandbox, which the guest bounds again at 10 s; a refusal comes back at once.
const dialBudget = 15 * time.Second

// acceptBackoff spaces the accepts after one the host refused, as for a full fd table, so the loop does not spin.
const acceptBackoff = 100 * time.Millisecond

// Address is where a forward listens: the loopback, or every interface for a public one.
func Address(public bool) string {
	if public {
		return "0.0.0.0"
	}

	return "127.0.0.1"
}

// Status is what the host does with one host port now.
type Status struct {
	// Sandbox is the one the listener carries to, empty when no listener was ever asked for.
	Sandbox   string
	Listening bool
	// Error is why the port is not listening, or else why the last connection could not reach the sandbox, in words a public route may show.
	Error string
}

// Forwarder owns every forward listener of the daemon.
type Forwarder struct {
	dial   Dial
	report func(string)
	// hidden is the interface a public forward never counts as reachable on: the bridge the sandboxes sit behind.
	hidden string

	mu    sync.Mutex
	ports map[uint16]*forward
}

type forward struct {
	id       string
	spec     models.PortForward
	listener net.Listener
	// reason is why listener is nil, or why the last dial failed while it is up; the daemon log has the cause.
	reason string
	// ctx ends every dial still in flight when the forward goes.
	ctx    context.Context
	cancel context.CancelFunc
	// conns holds both ends of every connection, so a drop frees a guest read that a half close left waiting.
	conns map[net.Conn]struct{}
}

// New answers a forwarder with no listener; report takes what a connection broke mid-stream.
func New(dial Dial, report func(string), hidden string) *Forwarder {
	return &Forwarder{dial: dial, report: report, hidden: hidden, ports: map[uint16]*forward{}}
}

// Open puts up one forward of a running sandbox: a forward it already has changes in place, and one it lacks binds.
func (f *Forwarder) Open(id string, spec models.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.open(id, spec)
}

func (f *Forwarder) open(id string, spec models.PortForward) error {
	fw, ok := f.ports[spec.HostPort]
	if ok && fw.id != id {
		if err := f.drop(spec.HostPort); err != nil {
			return err
		}
		ok = false
	}
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		fw = &forward{id: id, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
		f.ports[spec.HostPort] = fw
	}

	// A new guest port takes the next connection; one already carried stays where it went.
	rebind := fw.listener == nil || fw.spec.Public != spec.Public
	fw.spec = spec
	if !rebind {
		return nil
	}

	// The loopback and the wildcard bind of one port conflict, so the old listener goes first; its connections stay.
	if fw.listener != nil {
		err := fw.listener.Close()
		fw.listener = nil
		if err != nil && !errors.Is(err, net.ErrClosed) {
			fw.reason = fmt.Sprintf("the host did not let go of port %d; the daemon log has why", spec.HostPort)

			return fmt.Errorf("close the listener on host port %d: %w", spec.HostPort, err)
		}
	}

	l, err := net.Listen("tcp4", net.JoinHostPort(Address(spec.Public), strconv.Itoa(int(spec.HostPort))))
	if err != nil {
		bind := &BindError{Port: spec.HostPort, Err: err}
		fw.reason = bind.Public()

		return bind
	}
	fw.listener, fw.reason = l, ""
	go f.accept(fw, l)

	return nil
}

// Set makes one sandbox's forwards exactly want; a port the host refuses keeps why in its status and is retried next Set.
func (f *Forwarder) Set(id string, want []models.PortForward) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	wanted := map[uint16]bool{}
	for _, spec := range want {
		wanted[spec.HostPort] = true
	}

	var errs []error
	for port, fw := range f.ports {
		if fw.id == id && !wanted[port] {
			errs = append(errs, f.drop(port))
		}
	}
	for _, spec := range want {
		errs = append(errs, f.openOrNote(id, spec))
	}

	return errors.Join(errs...)
}

// openOrNote opens one forward for Set; a port the host refuses is no error, and its reason reports only when it changes.
func (f *Forwarder) openOrNote(id string, spec models.PortForward) error {
	was := f.reason(spec.HostPort)
	err := f.open(id, spec)
	_, refused := errors.AsType[*BindError](err)
	if refused && f.reason(spec.HostPort) != was {
		f.report(fmt.Sprintf("sandbox %s: %v", id, err))
	}
	if refused || err == nil {
		return nil
	}

	return fmt.Errorf("sandbox %s: %w", id, err)
}

func (f *Forwarder) reason(hostPort uint16) string {
	fw, ok := f.ports[hostPort]
	if !ok {
		return ""
	}

	return fw.reason
}

// Probe binds the address spec asks for and lets it go, so a create refuses a host port the host would not give it before anything is built.
func (f *Forwarder) Probe(spec models.PortForward) error {
	l, err := net.Listen("tcp4", net.JoinHostPort(Address(spec.Public), strconv.Itoa(int(spec.HostPort))))
	if err != nil {
		return &BindError{Port: spec.HostPort, Err: err}
	}
	if err := l.Close(); err != nil {
		return fmt.Errorf("let go of host port %d after a probe: %w", spec.HostPort, err)
	}

	return nil
}

// Close ends the forward on one host port of sandbox id, its listener and every connection it carries; another sandbox's is not touched.
func (f *Forwarder) Close(id string, hostPort uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	fw, ok := f.ports[hostPort]
	if !ok || fw.id != id {
		return nil
	}

	return f.drop(hostPort)
}

// CloseSandbox ends every forward of one sandbox, so a stop or a pause leaves no listener that reaches nothing.
func (f *Forwarder) CloseSandbox(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var errs []error
	for port, fw := range f.ports {
		if fw.id == id {
			errs = append(errs, f.drop(port))
		}
	}

	return errors.Join(errs...)
}

// Sandboxes names every sandbox with a forward, a refused one included, so a sync reaches one whose record is gone.
func (f *Forwarder) Sandboxes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var ids []string
	for _, fw := range f.ports {
		if !slices.Contains(ids, fw.id) {
			ids = append(ids, fw.id)
		}
	}
	slices.Sort(ids)

	return ids
}

// Status says whether hostPort listens and for which sandbox, and why not or what the last connection hit.
func (f *Forwarder) Status(hostPort uint16) Status {
	f.mu.Lock()
	defer f.mu.Unlock()

	fw, ok := f.ports[hostPort]
	if !ok {
		return Status{}
	}

	return Status{Sandbox: fw.id, Listening: fw.listener != nil, Error: fw.reason}
}

// drop takes the forward off the map whatever its close answers, so a retry finds nothing left to close.
func (f *Forwarder) drop(hostPort uint16) error {
	fw := f.ports[hostPort]
	delete(f.ports, hostPort)
	fw.cancel()

	var errs []error
	if fw.listener != nil {
		errs = append(errs, quiet(fw.listener.Close()))
	}
	for conn := range fw.conns {
		errs = append(errs, quiet(conn.Close()))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("close the forward on host port %d: %w", hostPort, err)
	}

	return nil
}

func (f *Forwarder) accept(fw *forward, l net.Listener) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			f.acceptFailed(fw, err)
			time.Sleep(acceptBackoff)

			continue
		}

		guest, ok := f.track(fw, l, conn)
		if !ok {
			f.reject(fw, conn)

			return
		}
		go f.serve(fw, conn, guest)
	}
}

func (f *Forwarder) acceptFailed(fw *forward, err error) {
	if f.note(fw, fmt.Sprintf("the host refused a connection to port %d; the daemon log has why", fw.spec.HostPort)) {
		f.report(fmt.Sprintf("sandbox %s: accept on a forwarded host port: %v", fw.id, err))
	}
}

// reject closes a connection that reached a forward after it ended.
func (f *Forwarder) reject(fw *forward, conn net.Conn) {
	if err := quiet(conn.Close()); err != nil {
		f.report(fmt.Sprintf("sandbox %s: close a connection to a forward that ended: %v", fw.id, err))
	}
}

// track holds conn under its forward and answers the guest port it goes to, unless the listener was closed meanwhile.
func (f *Forwarder) track(fw *forward, l net.Listener, conn net.Conn) (uint16, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if fw.listener != l {
		return 0, false
	}
	fw.conns[conn] = struct{}{}

	return fw.spec.GuestPort, true
}

// hold tracks the guest end of a connection, unless the forward went while its dial was in flight; drop cancels ctx under f.mu.
func (f *Forwarder) hold(fw *forward, upstream net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if fw.ctx.Err() != nil {
		return false
	}
	fw.conns[upstream] = struct{}{}

	return true
}

func (f *Forwarder) untrack(fw *forward, conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(fw.conns, conn)
}

// note keeps reason as the forward's, empty once a connection gets through, and says whether it changed, so a client retrying logs one line.
func (f *Forwarder) note(fw *forward, reason string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if fw.listener == nil || fw.reason == reason {
		return false
	}
	fw.reason = reason

	return true
}

func (f *Forwarder) serve(fw *forward, client net.Conn, guest uint16) {
	defer f.untrack(fw, client)

	ctx, cancel := context.WithTimeout(fw.ctx, dialBudget)
	upstream, err := f.dial(ctx, fw.id, guest)
	cancel()
	if err != nil {
		if f.note(fw, unreached(guest, err)) {
			f.report(fmt.Sprintf("sandbox %s: a connection did not reach port %d: %v", fw.id, guest, err))
		}
		if err := quiet(client.Close()); err != nil {
			f.report(fmt.Sprintf("sandbox %s: close a connection the sandbox did not take: %v", fw.id, err))
		}

		return
	}
	// The drop that ended the forward closed the client already.
	if !f.hold(fw, upstream) {
		f.reject(fw, upstream)

		return
	}
	defer f.untrack(fw, upstream)
	f.note(fw, "")

	if err := splice.Conns(client, upstream); err != nil {
		f.report(fmt.Sprintf("sandbox %s: a connection to port %d broke: %v", fw.id, guest, err))
	}
}

// unreached says why a dial into the sandbox failed without the host paths and the substrate's text behind it.
func unreached(guest uint16, err error) string {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("nothing listens on 127.0.0.1:%d in the sandbox", guest)
	}
	if errors.Is(err, models.ErrUnsupported) {
		return "the sandbox runs a shard-init from before port forwards: stop and start it"
	}

	return fmt.Sprintf("the last connection did not reach port %d in the sandbox; the daemon log has why", guest)
}

func quiet(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}

// BindError is a host port the host would not listen on: another process holds it, or it is privileged and the daemon is not root.
type BindError struct {
	Port uint16
	Err  error
}

func (e *BindError) Error() string {
	return fmt.Sprintf("listen on host port %d: %v", e.Port, e.Err)
}

func (e *BindError) Unwrap() error { return e.Err }

// Public names the port and what to do, never the host's own wording.
func (e *BindError) Public() string {
	if errors.Is(e.Err, syscall.EADDRINUSE) {
		return fmt.Sprintf("host port %d is in use on the host: pick another host port", e.Port)
	}
	if errors.Is(e.Err, syscall.EACCES) {
		return fmt.Sprintf("host port %d is privileged and the daemon does not run as root: pick a host port of 1024 or more", e.Port)
	}

	return fmt.Sprintf("the host did not listen on port %d; the daemon log has why", e.Port)
}
