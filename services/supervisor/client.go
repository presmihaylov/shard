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
	"time"

	"github.com/presmihaylov/shard/models"
)

// ErrEntrypointNotStarted is a run the guest refused, with the guest's own words for why behind it.
var ErrEntrypointNotStarted = errors.New("the entrypoint did not start")

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

	// events is unbounded, so a host that reads Next late never stalls the guest's answers behind them.
	events   []Message
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
		if err := ReadMessage(r, &m); err != nil {
			c.end(err)

			return
		}
		if m.ID == 0 {
			c.push(m)

			continue
		}
		c.pendingMu.Lock()
		reply, ok := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.pendingMu.Unlock()
		if ok {
			reply <- m
		}
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

func (c *Control) push(m Message) {
	c.eventsMu.Lock()
	c.events = append(c.events, m)
	c.eventsMu.Unlock()
	c.arrived.Broadcast()
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
	m := c.events[0]
	c.events = c.events[1:]

	return m, nil
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

// Reseed gives a restored guest fresh host entropy and rekeys its crng from it, so two restores of one save draw different bytes.
func (c *Control) Reseed(ctx context.Context) error {
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("draw the seed: %w", err)
	}

	return c.request(ctx, Message{Kind: KindReseed, Seed: seed})
}

// Freeze flushes the guest's root and holds every write to it, so a disk copied while the VM is paused is whole.
func (c *Control) Freeze(ctx context.Context) error { return c.request(ctx, Message{Kind: KindFreeze}) }

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

		return fmt.Errorf("%s: the guest went away: %w", m.Kind, ended)
	}
	err := c.send(ctx, m)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, m.ID)
		c.pendingMu.Unlock()

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
		return fmt.Errorf("%s: the guest went away before it answered", m.Kind)
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

// Exec runs one command over a fresh exec connection, moves its streams to the files in spec, and returns how it ended.
func Exec(ctx context.Context, dial Dialer, id string, header ExecHeader, spec models.ExecSpec) (models.ExitStatus, error) {
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
	// A cancelled context cancels the exec and closes, which is what unblocks the header write and the frame reader below.
	stop := context.AfterFunc(ctx, func() { cancelExec(conn, &writes) })
	defer stop()
	if err := WriteMessage(conn, header); err != nil {
		return models.ExitStatus{}, execFailure(ctx, header, err)
	}

	go feedStdin(conn, &writes, spec.Stdin)
	go feedResizes(ctx, conn, &writes, spec.Resizes)

	exit, err := readExec(ctx, conn, id, spec)
	if err != nil && stop() {
		// A host that gives up on the exec ends the command with it; only a daemon that dies leaves one running.
		cancelExec(conn, &writes)
	}
	if err != nil {
		return models.ExitStatus{}, execFailure(ctx, header, err)
	}

	return exit, nil
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
func cancelExec(conn net.Conn, writes *sync.Mutex) {
	// The deadline comes first, so a write held by a guest that stopped reading frees the lock within the budget.
	_ = conn.SetWriteDeadline(time.Now().Add(cancelBudget))
	writes.Lock()
	_ = WriteFrame(conn, StreamCancel, nil)
	writes.Unlock()
	_ = conn.Close()
}

// feedStdin frames stdin until it ends, then tells the guest so, at once for a nil one; a failed write is the guest gone, which the frame reader reports.
func feedStdin(conn net.Conn, writes *sync.Mutex, stdin *os.File) {
	if stdin == nil {
		writes.Lock()
		_ = WriteFrame(conn, StreamStdinClose, nil)
		writes.Unlock()

		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			writes.Lock()
			werr := WriteFrame(conn, StreamStdin, buf[:n])
			writes.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			writes.Lock()
			_ = WriteFrame(conn, StreamStdinClose, nil)
			writes.Unlock()

			return
		}
	}
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
