package supervisor

import (
	"bufio"
	"context"
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

// Control is the host end of the control connection. A request waits for the guest's answer; the events between them queue for Next.
type Control struct {
	conn net.Conn
	// mu orders the writes, and guards the request counter with them.
	mu     sync.Mutex
	nextID int

	pending   map[int]chan Message
	pendingMu sync.Mutex

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

// end wakes every waiter with the read error, so no request outlives the connection.
func (c *Control) end(err error) {
	c.pendingMu.Lock()
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
func (c *Control) Run(spec RunSpec) error {
	if err := c.request(Message{Kind: KindRun, Run: &spec}); err != nil {
		return fmt.Errorf("%w: %w", ErrEntrypointNotStarted, err)
	}

	return nil
}

// Signal sends one signal to a process shard-init started, the entrypoint or an exec, by its guest pid.
func (c *Control) Signal(pid int, signal string) error {
	return c.request(Message{Kind: KindSignal, PID: pid, Signal: signal})
}

// Stop asks shard-init to forward the stop to the entrypoint; the caller waits out the grace and kills the VM.
func (c *Control) Stop() error { return c.request(Message{Kind: KindStop}) }

// Readdress moves a restored guest onto its own address, and returns once it answers there and nowhere else.
func (c *Control) Readdress(a Address) error {
	return c.request(Message{Kind: KindReaddress, Address: &a})
}

func (c *Control) Close() error { return c.conn.Close() }

// request sends one message and waits for the guest's done, or its failure as an error.
func (c *Control) request(m Message) error {
	reply := make(chan Message, 1)

	c.mu.Lock()
	c.nextID++
	m.ID = c.nextID
	c.pendingMu.Lock()
	c.pending[m.ID] = reply
	c.pendingMu.Unlock()
	err := WriteMessage(c.conn, m)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, m.ID)
		c.pendingMu.Unlock()

		return fmt.Errorf("%s: %w", m.Kind, err)
	}

	answer, ok := <-reply
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

// Exec runs one command over a fresh exec connection, moves its streams to the files in spec, and returns how it ended.
func Exec(ctx context.Context, dial Dialer, id string, header ExecHeader, spec models.ExecSpec) (models.ExitStatus, error) {
	conn, err := dial(ctx, ExecPort)
	if err != nil {
		return models.ExitStatus{}, fmt.Errorf("open an exec connection: %w", err)
	}
	defer conn.Close()

	if err := WriteMessage(conn, header); err != nil {
		return models.ExitStatus{}, err
	}

	// A cancelled context closes the connection, which is what unblocks the frame reader below.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	var writes sync.Mutex
	if spec.Stdin != nil {
		go feedStdin(conn, &writes, spec.Stdin)
	}

	exit, err := readExec(conn, id, spec)
	if err != nil && ctx.Err() != nil {
		return models.ExitStatus{}, fmt.Errorf("exec %q: %w", header.Argv[0], ctx.Err())
	}
	if err != nil {
		return models.ExitStatus{}, err
	}

	return exit, nil
}

// feedStdin moves the caller's stdin into frames until it ends, then tells the guest so.
func feedStdin(conn net.Conn, writes *sync.Mutex, stdin *os.File) {
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

// readExec takes the guest's frames until the exit one; the output files get their bytes as they come.
func readExec(conn net.Conn, id string, spec models.ExecSpec) (models.ExitStatus, error) {
	r := bufio.NewReader(conn)
	for {
		stream, payload, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			return models.ExitStatus{}, errors.New("the guest closed the exec before it reported an exit")
		}
		if err != nil {
			return models.ExitStatus{}, err
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

			return models.ExitStatus{Code: exit.Code, Signal: exit.Signal}, nil
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

// Logs copies the entrypoint's output into w until the guest, or ctx, ends the connection.
func Logs(ctx context.Context, dial Dialer, w io.Writer) error {
	conn, err := dial(ctx, LogsPort)
	if err != nil {
		return fmt.Errorf("open the logs connection: %w", err)
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := io.Copy(w, conn); err != nil && ctx.Err() == nil {
		return fmt.Errorf("follow the guest logs: %w", err)
	}

	return nil
}
