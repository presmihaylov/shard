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

// Control is the host end of the control connection. Next runs in one goroutine; the senders take a lock.
type Control struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
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
	return &Control{conn: conn, r: bufio.NewReader(conn)}
}

// Next blocks for the guest's next message. A guest that went away reads as io.EOF.
func (c *Control) Next() (Message, error) {
	var m Message
	if err := ReadMessage(c.r, &m); err != nil {
		return Message{}, err
	}

	return m, nil
}

// Run sends the entrypoint and reads until the guest says it forked, or says why it could not.
func (c *Control) Run(spec RunSpec) error {
	if err := c.send(Message{Kind: KindRun, Run: &spec}); err != nil {
		return err
	}

	for {
		m, err := c.Next()
		if err != nil {
			return fmt.Errorf("wait for the entrypoint to start: %w", err)
		}
		switch m.Kind {
		case KindReady:
			return nil
		case KindFailure:
			return fmt.Errorf("%w: %s", ErrEntrypointNotStarted, m.Error)
		case KindState:
			// A guest that already ran once replays its state before the answer; the caller reads it again from Next.
		}
	}
}

// Signal sends one signal to a process shard-init started, the entrypoint or an exec, by its guest pid.
func (c *Control) Signal(pid int, signal string) error {
	return c.send(Message{Kind: KindSignal, PID: pid, Signal: signal})
}

// Stop asks shard-init to forward the stop to the entrypoint; the caller waits out the grace and kills the VM.
func (c *Control) Stop() error { return c.send(Message{Kind: KindStop}) }

// Readdress moves a restored guest onto its own address, so a fork stops answering on the source's.
func (c *Control) Readdress(a Address) error {
	return c.send(Message{Kind: KindReaddress, Address: &a})
}

func (c *Control) Close() error { return c.conn.Close() }

func (c *Control) send(m Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return WriteMessage(c.conn, m)
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
