package supervisor

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/pty"
)

// ErrEntrypointNotStarted is a run the guest refused, with the guest's own words for why behind it.
var ErrEntrypointNotStarted = errors.New("the entrypoint did not start")

// The events a guest can queue ahead of Next, by count and by line bytes: the queue is daemon memory, outside the sandbox's bound (SHARD-550).
const (
	maxQueuedEvents = 1024
	maxQueuedBytes  = 4 * MaxPayload
)

// ErrEventFlood ends a stream whose guest queued events past one of those bounds before the host read them.
var ErrEventFlood = fmt.Errorf("the guest queued more than %d events or %d bytes the host has not read", maxQueuedEvents, maxQueuedBytes)

// ErrGone is a request whose stream ended before the guest answered it, which a stream dialed again can carry.
var ErrGone = errors.New("the guest went away")

// Dialer opens one connection to a guest port. pkg/vz's Client.Connect is one, over the shim socket.
type Dialer func(ctx context.Context, port uint32) (net.Conn, error)

// The guest's listener comes up a moment after the kernel boots, so a refused dial is retried at this pace.
const dialInterval = 50 * time.Millisecond

// requestTimeout bounds a request on top of its caller's context, so a guest that never answers frees the verb (SHARD-339).
const requestTimeout = 30 * time.Second

// startTimeout bounds the wait for an exec's first frame, so an exec no guest listener took fails instead of running forever (SHARD-354).
var startTimeout = requestTimeout

// cancelBudget bounds the cancel frame of an exec, so a guest that stopped reading never holds the close.
const cancelBudget = time.Second

// Control is the host end of the control connection. A request waits for the guest's answer; the events between them queue for Next.
type Control struct {
	conn net.Conn
	// mu orders the writes, and guards the request counter and torn with them.
	mu     sync.Mutex
	nextID int
	// torn is the write that failed, which may have left half a frame on the stream for the guest to read.
	torn error

	pending   map[int]chan Message
	pendingMu sync.Mutex
	// ended is the read error once the reader is gone, under pendingMu so a request registers or is refused, never lost.
	ended error

	// events never blocks the reader, so a host that reads Next late never stalls the guest's answers behind them.
	events []queuedEvent
	// queued is the line bytes of events, which maxQueuedBytes bounds.
	queued   int
	eventsMu sync.Mutex
	arrived  *sync.Cond
	// readErr is why the reader stopped; io.EOF when the guest went away.
	readErr error
}

// Connect opens the control connection, retrying while the guest still boots, until ctx ends.
func Connect(ctx context.Context, dial Dialer) (*Control, error) {
	for {
		conn, err := dial(ctx, ControlPort)
		if err == nil {
			return ControlOver(conn), nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to the guest supervisor: %w, last: %w", ctx.Err(), err)
		case <-time.After(dialInterval):
		}
	}
}

// ControlOver speaks the control protocol over a connection the caller already holds.
func ControlOver(conn net.Conn) *Control {
	c := &Control{conn: conn, pending: map[int]chan Message{}}
	c.arrived = sync.NewCond(&c.eventsMu)
	go c.read()

	return c
}

// read routes each message: an answer to the request that carries its id, anything else to Next.
func (c *Control) read() {
	r := bufio.NewReader(c.conn)
	for {
		var m Message
		size, err := readMessage(r, &m)
		if err != nil {
			c.end(err)

			return
		}
		if m.ID != 0 {
			c.answer(m)

			continue
		}
		if !c.push(m, size) {
			// The close fails the guest's next write at once, and the redial gets a fresh stream.
			c.end(errors.Join(ErrEventFlood, c.conn.Close()))

			return
		}
	}
}

// answer hands a reply to the request that carries its id; one whose request is gone is dropped.
func (c *Control) answer(m Message) {
	c.pendingMu.Lock()
	reply, ok := c.pending[m.ID]
	delete(c.pending, m.ID)
	c.pendingMu.Unlock()
	if ok {
		reply <- m
	}
}

// end wakes every waiter with the read error, so no request outlives the connection, and refuses the ones after it.
func (c *Control) end(err error) {
	c.pendingMu.Lock()
	c.ended = err
	for id, reply := range c.pending {
		delete(c.pending, id)
		close(reply)
	}
	c.pendingMu.Unlock()

	c.eventsMu.Lock()
	c.readErr = err
	c.eventsMu.Unlock()
	c.arrived.Broadcast()
}

// push queues an event of size line bytes, or answers false when it would take the queue past a bound.
func (c *Control) push(m Message, size int) bool {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if len(c.events) >= maxQueuedEvents || c.queued+size > maxQueuedBytes {
		return false
	}
	c.events = append(c.events, queuedEvent{message: m, size: size})
	c.queued += size
	c.arrived.Broadcast()

	return true
}

// Next blocks for the guest's next event. A guest that went away reads as io.EOF once the events before it are out.
func (c *Control) Next() (Message, error) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()

	for len(c.events) == 0 && c.readErr == nil {
		c.arrived.Wait()
	}
	if len(c.events) == 0 {
		return Message{}, c.readErr
	}
	next := c.events[0]
	// The cleared slot lets the collector take the message while the rest still wait behind it.
	c.events[0] = queuedEvent{}
	c.events = c.events[1:]
	c.queued -= next.size

	return next.message, nil
}

// queuedEvent is one event Next has not taken yet, with the bytes of its line.
type queuedEvent struct {
	message Message
	size    int
}

// Run sends the entrypoint and waits until the guest says it forked, or says why it could not.
func (c *Control) Run(ctx context.Context, spec RunSpec) error {
	if err := c.request(ctx, Message{Kind: KindRun, Run: &spec}); err != nil {
		return fmt.Errorf("%w: %w", ErrEntrypointNotStarted, err)
	}

	return nil
}

// Signal sends one signal to a process shard-init started, the entrypoint or an exec, by its guest pid.
func (c *Control) Signal(ctx context.Context, pid int, signal string) error {
	return c.request(ctx, Message{Kind: KindSignal, PID: pid, Signal: signal})
}

// Stop asks shard-init to forward the stop to the entrypoint; the caller waits out the grace and kills the VM.
func (c *Control) Stop(ctx context.Context) error { return c.request(ctx, Message{Kind: KindStop}) }

// StopApp cancels every start again of the entrypoint and terms it, or kills it with force; the guest stays up.
func (c *Control) StopApp(ctx context.Context, force bool) error {
	return c.request(ctx, Message{Kind: KindStopApp, Force: force})
}

// Readdress moves a restored guest onto its own address, and returns once it answers there and nowhere else.
func (c *Control) Readdress(ctx context.Context, a Address) error {
	return c.request(ctx, Message{Kind: KindReaddress, Address: &a})
}

// SeedSize is the host entropy one reseed carries, the size of the kernel's crng key.
const SeedSize = 32

// Reseed gives a restored guest fresh host entropy and rekeys its crng from it, so two restores of one save draw different bytes, and sets its wall clock to the host's.
func (c *Control) Reseed(ctx context.Context) error {
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("draw the seed: %w", err)
	}

	return c.request(ctx, Message{Kind: KindReseed, Seed: seed, Now: time.Now().UnixNano()})
}

// Freeze flushes the guest's root and holds every write to it, so a disk copied while the VM is paused is whole; verb is what holds it.
func (c *Control) Freeze(ctx context.Context, verb string) error {
	return c.request(ctx, Message{Kind: KindFreeze, Verb: verb})
}

// Thaw lets the guest's root take writes again; a root that is not frozen is already thawed.
func (c *Control) Thaw(ctx context.Context) error { return c.request(ctx, Message{Kind: KindThaw}) }

// Kill ends a stop the grace outran: the guest kills the entrypoint and flushes the disk, so the VM the host then cuts loses nothing it wrote.
func (c *Control) Kill(ctx context.Context) error { return c.request(ctx, Message{Kind: KindKill}) }

func (c *Control) Close() error { return c.conn.Close() }

// request sends one message and waits for the guest's done, its failure as an error, or the end of ctx.
func (c *Control) request(ctx context.Context, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	reply := make(chan Message, 1)

	c.mu.Lock()
	if c.torn != nil {
		c.mu.Unlock()

		return fmt.Errorf("%s: an earlier write broke the stream: %w", m.Kind, c.torn)
	}
	c.nextID++
	m.ID = c.nextID
	// Registered under the lock end takes, so a reader already gone cannot leave the reply unanswered.
	c.pendingMu.Lock()
	ended := c.ended
	if ended == nil {
		c.pending[m.ID] = reply
	}
	c.pendingMu.Unlock()
	if ended != nil {
		c.mu.Unlock()

		return fmt.Errorf("%s: %w: %w", m.Kind, ErrGone, ended)
	}
	err := c.send(ctx, m)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, m.ID)
		c.pendingMu.Unlock()
		if closed(err) {
			return fmt.Errorf("%s: %w: %w", m.Kind, ErrGone, err)
		}

		return fmt.Errorf("%s: %w", m.Kind, err)
	}

	var answer Message
	var ok bool
	select {
	case answer, ok = <-reply:
	case <-ctx.Done():
		// The reader drops an answer whose id is no longer pending, so a late one cannot reach the next request.
		c.pendingMu.Lock()
		delete(c.pending, m.ID)
		c.pendingMu.Unlock()

		return fmt.Errorf("%s: the guest did not answer: %w", m.Kind, ctx.Err())
	}
	if !ok {
		return fmt.Errorf("%s: %w before it answered", m.Kind, ErrGone)
	}
	if answer.Kind == KindFailure {
		return fmt.Errorf("%s: %s", m.Kind, answer.Error)
	}
	if answer.Kind != KindDone {
		return fmt.Errorf("%s: the guest answered with %q, not done", m.Kind, answer.Kind)
	}

	return nil
}

// send writes m by ctx's deadline: a guest that stops reading would otherwise hold the write, and c.mu with it, for good.
// The deadline stays set after the write, since every write sets its own and a clear can fail on a peer that already closed.
func (c *Control) send(ctx context.Context, m Message) error {
	deadline, _ := ctx.Deadline()
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set the write deadline: %w", err)
	}
	if err := WriteMessage(c.conn, m); err != nil {
		c.torn = err

		return err
	}

	return nil
}

// closed is a write the peer's close refused, unlike a deadline, which a guest that stopped reading runs into.
func closed(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

// Exec runs one command over a fresh exec connection, moves its streams to the files in spec, and returns how it ended.
func Exec(ctx context.Context, dial Dialer, id string, header ExecHeader, spec models.ExecSpec) (models.ExitStatus, error) {
	// The guest's terminal does the line discipline, so a cooked replica eats Ctrl-C, holds keys until Enter and echoes twice; it closes with this exec, so nothing restores it (SHARD-760).
	if header.TTY && spec.Stdin != nil {
		if _, err := pty.MakeRaw(spec.Stdin); err != nil {
			return models.ExitStatus{}, fmt.Errorf("exec %q: relay the terminal: %w", header.Argv[0], err)
		}
	}
	conn, err := dial(ctx, ExecPort)
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("open an exec connection: %w", err)
	}
	defer conn.Close()

	// The bound runs from the header to the first frame, and goes on before the cancel is armed, so the cancel's own budget overrides it.
	if err := conn.SetDeadline(time.Now().Add(startTimeout)); err != nil {
		return models.ExitStatus{}, fmt.Errorf("exec %q: bound the start: %w", header.Argv[0], err)
	}
	var writes sync.Mutex
	canceled := make(chan error, 1)
	// A cancelled context cancels the exec and closes, which is what unblocks the header write and the frame reader below.
	stop := context.AfterFunc(ctx, func() { canceled <- cancelExec(conn, &writes) })
	defer stop()
	if err := WriteMessage(conn, header); err != nil {
		return models.ExitStatus{}, execFailure(ctx, header, err)
	}

	fed, unread := make(chan error, 1), make(chan error, 1)
	go func() { fed <- feedStdin(conn, &writes, spec.Stdin, unread) }()
	go feedResizes(ctx, conn, &writes, spec.Resizes)

	exit, err := readExec(ctx, conn, id, spec)
	if err != nil {
		return models.ExitStatus{}, errors.Join(execFailure(ctx, header, err), giveUp(stop, conn, &writes, canceled), stdinFault(fed))
	}
	// A command fed only part of its input may still exit 0, so the read that cut it short is the exec's answer.
	if err := stdinFault(unread); err != nil {
		return models.ExitStatus{}, fmt.Errorf("exec %q: %w", header.Argv[0], err)
	}

	return exit, nil
}

// giveUp ends the command of an exec the host stopped reading, or answers how the context's cancel of it went.
func giveUp(stop func() bool, conn net.Conn, writes *sync.Mutex, canceled <-chan error) error {
	// A host that gives up on the exec ends the command with it; only a daemon that dies leaves one running.
	if stop() {
		return cancelExec(conn, writes)
	}

	return <-canceled
}

// stdinFault is the stdin feed's error, if it has ended; one still reading the caller's stdin has none yet.
func stdinFault(fed <-chan error) error {
	select {
	case err := <-fed:
		return err
	default:
		return nil
	}
}

// execFailure names why an exec ended early: the caller's context, the start bound, or the guest's own error.
func execFailure(ctx context.Context, header ExecHeader, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("exec %q: %w", header.Argv[0], ctx.Err())
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("exec %q: the guest did not start it within %s: %w", header.Argv[0], startTimeout, err)
	}

	return err
}

// cancelExec tells the guest to kill the command, then closes: a connection that only drops is a host that went away, and the command runs on.
func cancelExec(conn net.Conn, writes *sync.Mutex) error {
	// The deadline comes first, so a write held by a guest that stopped reading frees the lock within the budget.
	err := conn.SetWriteDeadline(time.Now().Add(cancelBudget))
	writes.Lock()
	err = errors.Join(err, WriteFrame(conn, StreamCancel, nil))
	writes.Unlock()
	if err := errors.Join(err, conn.Close()); err != nil {
		return fmt.Errorf("the guest may not have the cancel, so the command may run on: %w", err)
	}

	return nil
}

// feedStdin frames stdin until it ends, then tells the guest so, at once for a nil one; a failed read lands in unread before the guest hears of the end.
func feedStdin(conn net.Conn, writes *sync.Mutex, stdin *os.File, unread chan<- error) error {
	if stdin == nil {
		return closeStdin(conn, writes, nil)
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			writes.Lock()
			werr := WriteFrame(conn, StreamStdin, buf[:n])
			writes.Unlock()
			if werr != nil {
				return stdinWrite(werr)
			}
		}
		if errors.Is(err, io.EOF) {
			return closeStdin(conn, writes, nil)
		}
		if err != nil {
			cause := fmt.Errorf("read the exec's stdin: %w", err)
			unread <- cause

			return closeStdin(conn, writes, cause)
		}
	}
}

// closeStdin tells the guest stdin ended, for the reason cause gives if it is not the end of the file.
func closeStdin(conn net.Conn, writes *sync.Mutex, cause error) error {
	writes.Lock()
	err := WriteFrame(conn, StreamStdinClose, nil)
	writes.Unlock()

	return errors.Join(cause, stdinWrite(err))
}

// stdinWrite names a failed stdin write, but not one the host's own close cut, which is the exec ending.
func stdinWrite(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return fmt.Errorf("feed the exec's stdin: %w", err)
}

// feedResizes frames each new window until the exec ends; a nil channel is an exec with no terminal to resize.
func feedResizes(ctx context.Context, conn net.Conn, writes *sync.Mutex, resizes <-chan models.TerminalSize) {
	for {
		select {
		case <-ctx.Done():
			return
		case size, ok := <-resizes:
			if !ok {
				return
			}
			writes.Lock()
			err := WriteJSONFrame(conn, StreamResize, ResizeFrame{Rows: size.Rows, Cols: size.Cols})
			writes.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// readExec takes the guest's frames until the exit one; the output files get their bytes as they come.
func readExec(ctx context.Context, conn net.Conn, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	r := bufio.NewReader(conn)
	for first := true; ; first = false {
		stream, payload, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return models.ExitStatus{}, errors.New("the guest closed the exec before it reported an exit")
		}
		if err != nil {
			return models.ExitStatus{}, err
		}
		// The guest took the exec, so from here the command and its stdin run as long as they run.
		if first {
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return models.ExitStatus{}, fmt.Errorf("clear the start bound: %w", err)
			}
			// A cancel that fired before the clear lost its write budget with it, so it gets the budget back.
			if ctx.Err() != nil {
				if err := conn.SetWriteDeadline(time.Now().Add(cancelBudget)); err != nil {
					return models.ExitStatus{}, fmt.Errorf("restore the cancel budget: %w", err)
				}
			}
		}

		switch stream {
		case StreamStarted:
			var started StartedFrame
			if err := DecodeFrame(payload, &started); err != nil {
				return models.ExitStatus{}, err
			}
			if spec.Report != nil {
				spec.Report(started.PID)
			}
		case StreamStdout:
			if err := writeTo(spec.Stdout, payload); err != nil {
				return models.ExitStatus{}, err
			}
		case StreamStderr:
			if err := writeTo(spec.Stderr, payload); err != nil {
				return models.ExitStatus{}, err
			}
		case StreamExit:
			var exit ExitFrame
			if err := DecodeFrame(payload, &exit); err != nil {
				return models.ExitStatus{}, err
			}
			if exit.Error != "" {
				return models.ExitStatus{}, &models.CommandNotStartedError{Sandbox: id, Reason: exit.Error, Code: exit.Code}
			}

			// The code alone, because runsc and runc exec report 128+n and no signal, and every provider sends one shape (SHARD-432).
			return models.ExitStatus{Code: exit.Code}, nil
		default:
			return models.ExitStatus{}, fmt.Errorf("the guest sent a frame of stream %d, which the host does not take", stream)
		}
	}
}

// writeTo lands output in the caller's file; a nil one is /dev/null, as the ExecSpec promises.
func writeTo(f *os.File, payload []byte) error {
	if f == nil {
		return nil
	}
	if _, err := f.Write(payload); err != nil {
		return fmt.Errorf("write the exec output: %w", err)
	}

	return nil
}
