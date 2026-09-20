package vz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Client speaks to one shim over its socket. It holds no connection between verbs, so a daemon restart loses nothing.
type Client struct {
	socket string
	// Zero means callTimeout; a test shortens it.
	timeout time.Duration
}

// Info is what every verb reports back: the VM's state, the shim's pid and the identifier a restore must reuse.
type Info struct {
	State     State
	PID       int
	MachineID string
}

// The shim answers on its socket once the VM is up; a boot that takes longer than this is a failure to report.
const startTimeout = 30 * time.Second

// Every verb, dial to reply, is bounded, so a shim that accepts and never answers cannot hold the daemon.
const callTimeout = 30 * time.Second

// Start launches one shim, detached in its own process group, and returns once its socket answers.
// The shim's stdout and stderr go to cfg.Console's sibling shim.log, so a start that dies has its reason on disk.
func Start(ctx context.Context, shim string, cfg Config) (*Client, Info, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, Info{}, fmt.Errorf("marshal the shim config: %w", err)
	}

	logPath := strings.TrimSuffix(cfg.Console, ".log") + ".shim.log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, Info{}, fmt.Errorf("open the shim log: %w", err)
	}
	defer logFile.Close()

	// The shim outlives this process on purpose: its own group keeps a signal to the daemon off it.
	cmd := exec.Command(shim, "-config", string(encoded))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, Info{}, fmt.Errorf("start the shim: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	client := &Client{socket: cfg.Socket}
	deadline := time.After(startTimeout)
	for {
		info, err := client.State()
		if err == nil && info.PID != cmd.Process.Pid {
			return nil, Info{}, errors.Join(fmt.Errorf("%w: pid %d answers on %s", ErrSocketInUse, info.PID, cfg.Socket), client.end(cmd))
		}
		if err == nil {
			return client, info, nil
		}

		select {
		case <-ctx.Done():
			return nil, Info{}, errors.Join(ctx.Err(), client.end(cmd))
		case waitErr := <-exited:
			return nil, Info{}, fmt.Errorf("the shim exited before its socket answered (%w): %s", waitErr, tail(logPath))
		case <-deadline:
			return nil, Info{}, errors.Join(fmt.Errorf("the shim socket did not answer within %s: %s", startTimeout, tail(logPath)), client.end(cmd))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Adopt takes a shim that is already running, by its socket, and proves it answers.
func Adopt(socket string) (*Client, Info, error) {
	client := &Client{socket: socket}
	info, err := client.State()
	if err != nil {
		return nil, Info{}, fmt.Errorf("adopt the shim on %s: %w", socket, err)
	}

	return client, info, nil
}

func (c *Client) State() (Info, error)           { return c.call(request{Verb: "state"}) }
func (c *Client) Pause() (Info, error)           { return c.call(request{Verb: "pause"}) }
func (c *Client) Resume() (Info, error)          { return c.call(request{Verb: "resume"}) }
func (c *Client) Save(path string) (Info, error) { return c.call(request{Verb: "save", Path: path}) }
func (c *Client) Stop() (Info, error)            { return c.call(request{Verb: "stop"}) }

// Connect opens one vsock connection to a guest port; the returned stream is that connection.
func (c *Client) Connect(port uint32) (net.Conn, error) {
	conn, _, err := c.send(request{Verb: "connect", Port: port})
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// Network takes the host end of the VM's frames socket: one datagram is one Ethernet frame, both ways.
func (c *Client) Network() (*os.File, error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = callTimeout
	}
	conn, err := net.DialTimeout("unix", c.socket, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial the shim: %w", err)
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("a file rides on a unix socket only, not %T", conn)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	if err := writeFrame(conn, request{Verb: "network"}); err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}

	reply, file, err := readFrameWithFile(unixConn)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	if err := reply.err(); err != nil {
		return nil, errors.Join(fmt.Errorf("network: %w", err), closeIfAny(file))
	}
	if file == nil {
		return nil, errors.New("network: the shim sent no file with its reply")
	}

	return file, nil
}

// readFrameWithFile reads one reply frame and the descriptor riding on its first bytes, non-blocking so a close ends a read.
func readFrameWithFile(conn *net.UnixConn) (response, *os.File, error) {
	buf := make([]byte, 4+maxFrame)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return response{}, nil, fmt.Errorf("read the frame with the file: %w", err)
	}

	var file *os.File
	if oobn > 0 {
		fd, err := fdOf(oob[:oobn])
		if err != nil {
			return response{}, nil, err
		}
		if err := syscall.SetNonblock(fd, true); err != nil {
			return response{}, nil, fmt.Errorf("set the frames socket non-blocking: %w", err)
		}
		file = os.NewFile(uintptr(fd), "vmnet-host") //nolint:gosec // fd came in over SCM_RIGHTS and is ours now
	}

	// A stream may split the frame; the rest follows on the same connection, with nothing riding on it.
	var reply response
	if err := readFrame(io.MultiReader(bytes.NewReader(buf[:n]), conn), &reply); err != nil {
		return response{}, nil, errors.Join(err, closeIfAny(file))
	}

	return reply, file, nil
}

func fdOf(oob []byte) (int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, fmt.Errorf("parse the control message: %w", err)
	}
	if len(msgs) != 1 {
		return 0, fmt.Errorf("%d control messages, want one", len(msgs))
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil {
		return 0, fmt.Errorf("parse the rights: %w", err)
	}
	if len(fds) != 1 {
		return 0, fmt.Errorf("%d descriptors, want one", len(fds))
	}

	return fds[0], nil
}

func closeIfAny(file *os.File) error {
	if file == nil {
		return nil
	}

	return file.Close()
}

func (c *Client) call(req request) (Info, error) {
	conn, info, err := c.send(req)
	if err != nil {
		return Info{}, err
	}
	if err := conn.Close(); err != nil {
		return Info{}, fmt.Errorf("close the shim socket: %w", err)
	}

	return info, nil
}

func (c *Client) send(req request) (net.Conn, Info, error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = callTimeout
	}
	conn, err := net.DialTimeout("unix", c.socket, timeout)
	if err != nil {
		return nil, Info{}, fmt.Errorf("dial the shim: %w", err)
	}

	var reply response
	err = conn.SetDeadline(time.Now().Add(timeout))
	if err == nil {
		err = writeFrame(conn, req)
	}
	if err == nil {
		err = readFrame(conn, &reply)
	}
	if err == nil {
		err = reply.err()
	}
	if err == nil {
		// A connect stream lives as long as the guest side; the deadline covered the handshake only.
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		return nil, Info{}, errors.Join(fmt.Errorf("%s: %w", req.Verb, err), conn.Close())
	}

	return conn, Info{State: reply.State, PID: reply.PID, MachineID: reply.MachineID}, nil
}

// end kills a shim that never answered, so a failed start leaves no VM behind.
func (c *Client) end(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill the shim that did not answer: %w", err)
	}

	return nil
}

// tail is the last of the shim log, for an error message that says why a start failed.
func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}

	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}

	return strings.Join(lines, " | ")
}
