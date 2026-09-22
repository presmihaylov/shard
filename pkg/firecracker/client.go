package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Client speaks to one firecracker over its API socket. It holds no connection between calls, so a daemon restart loses nothing.
type Client struct {
	socket string
	vsock  string
	// Zero means callTimeout; a test shortens it.
	timeout time.Duration
}

// Start spawns firecracker in its own group, so it outlives this process, puts the microVM in over the API and boots it; the console goes to cfg.Console.
func Start(ctx context.Context, binary string, cfg Config) (*Client, Info, error) {
	client := &Client{socket: cfg.Socket, vsock: cfg.Vsock}
	if err := client.claim(); err != nil {
		return nil, Info{}, err
	}

	console, err := os.OpenFile(cfg.Console, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, Info{}, fmt.Errorf("open the console log: %w", err)
	}
	defer console.Close()

	cmd := exec.Command(binary, "--api-sock", cfg.Socket)
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, Info{}, fmt.Errorf("start firecracker: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	if err := client.await(ctx, cmd, exited, cfg.Console); err != nil {
		return nil, Info{}, err
	}
	if err := client.configure(cfg); err != nil {
		return nil, Info{}, errors.Join(err, end(cmd))
	}
	info, err := client.State()
	if err != nil {
		return nil, Info{}, errors.Join(fmt.Errorf("read the state after the boot: %w", err), end(cmd))
	}

	return client, info, nil
}

// claim refuses a socket a live vmm answers on, and clears the paths a dead one left, which firecracker refuses to reuse.
func (c *Client) claim() error {
	info, err := c.State()
	if err == nil {
		return fmt.Errorf("%w: pid %d answers on %s", ErrSocketInUse, info.PID, c.socket)
	}
	if !absent(err) {
		return fmt.Errorf("probe the api socket: %w", err)
	}
	for _, path := range []string{c.socket, c.vsock} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the stale %s: %w", path, err)
		}
	}

	return nil
}

// absent is a socket with no vmm behind it: never made, or its owner exited and left the path.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// await polls the API until this process answers; one that exits or stays silent is reported with its console.
func (c *Client) await(ctx context.Context, cmd *exec.Cmd, exited <-chan error, console string) error {
	deadline := time.After(startTimeout)
	for {
		info, err := c.State()
		if err == nil && info.PID != cmd.Process.Pid {
			return errors.Join(fmt.Errorf("%w: pid %d answers on %s", ErrSocketInUse, info.PID, c.socket), end(cmd))
		}
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), end(cmd))
		case waitErr := <-exited:
			return fmt.Errorf("firecracker exited before its api answered (%w): %s", waitErr, tail(console))
		case <-deadline:
			return errors.Join(fmt.Errorf("the api socket did not answer within %s: %s", startTimeout, tail(console)), end(cmd))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// configure puts the machine, the boot source, the drives and the vsock in, in the order the API wants them, then starts the instance.
func (c *Client) configure(cfg Config) error {
	if err := c.put("/machine-config", machineConfig{VCPUs: cfg.VCPUs, MemoryMiB: cfg.MemoryMiB}); err != nil {
		return err
	}
	if err := c.put("/boot-source", bootSource{Kernel: cfg.Kernel, Initrd: cfg.Initrd, Args: cfg.Cmdline}); err != nil {
		return err
	}
	for _, d := range cfg.Drives {
		if err := c.put("/drives/"+d.ID, drive{ID: d.ID, Path: d.Path, ReadOnly: d.ReadOnly}); err != nil {
			return err
		}
	}
	if cfg.Vsock != "" {
		if err := c.put("/vsock", vsockDevice{CID: GuestCID, Path: cfg.Vsock}); err != nil {
			return err
		}
	}

	return c.put("/actions", action{Type: "InstanceStart"})
}

// Adopt takes a firecracker that is already running, by its sockets, and proves it answers.
func Adopt(socket, vsock string) (*Client, Info, error) {
	client := &Client{socket: socket, vsock: vsock}
	info, err := client.State()
	if err != nil {
		return nil, Info{}, fmt.Errorf("adopt the vmm on %s: %w", socket, err)
	}

	return client, info, nil
}

// State asks the vmm what the microVM is doing, and who answers: the pid is the peer of the socket.
func (c *Client) State() (Info, error) {
	var got instance
	pid, err := c.call(http.MethodGet, "/", nil, &got)
	if err != nil {
		return Info{}, err
	}

	return Info{State: got.State, PID: pid}, nil
}

// Kill ends the vmm by the pid behind its socket, the only forced stop firecracker has; a socket nobody answers is already ended.
func (c *Client) Kill() error {
	info, err := c.State()
	if absent(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	return kill(info.PID)
}

// kill ends the vmm with the group Start made it lead, so nothing it spawned outlives it; one that leads no group dies alone.
func kill(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill firecracker %d: %w", pid, err)
	}

	return nil
}

// Connect opens one vsock connection to a guest port over the socket firecracker proxies them on; the returned stream is that connection.
func (c *Client) Connect(port uint32) (net.Conn, error) {
	if c.vsock == "" {
		return nil, errors.New("connect: the microVM has no vsock device")
	}
	conn, err := c.dial(c.vsock)
	if err != nil {
		return nil, fmt.Errorf("connect to guest port %d: %w", port, err)
	}
	if err := handshake(conn, port); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to guest port %d: %w", port, err), conn.Close())
	}
	// The stream lives as long as the guest side; the deadline covered the handshake only.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to guest port %d: %w", port, err), conn.Close())
	}

	return conn, nil
}

// handshake is firecracker's own: CONNECT <port> in, OK <host port> back, and then the stream is the guest's.
func handshake(conn net.Conn, port uint32) error {
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return err
	}
	// One byte at a time: the guest may have written past the reply already, and a buffered read would take that too.
	var line []byte
	for {
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			// The vmm ends the connection instead of answering when nothing in the guest listens on the port.
			return fmt.Errorf("the guest did not accept: %w", err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
		if len(line) > 64 {
			break
		}
	}
	if !strings.HasPrefix(string(line), "OK ") {
		return fmt.Errorf("the vmm answered %q, not OK", line)
	}

	return nil
}

func (c *Client) put(path string, body any) error {
	_, err := c.call(http.MethodPut, path, body, nil)

	return err
}

// call is one request on its own connection, bounded from dial to reply, and the pid of the process that answered it.
func (c *Client) call(method, path string, body, reply any) (int, error) {
	conn, err := c.dial(c.socket)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer conn.Close()
	pid, err := peerPID(conn)
	if err != nil {
		return 0, fmt.Errorf("%s %s: read the peer of the api socket: %w", method, path, err)
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%s %s: marshal the request: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://localhost"+path, payload)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := req.Write(conn); err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("%s %s: read the reply: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, faultOf(blob))
	}
	if reply == nil {
		return pid, nil
	}
	if err := json.Unmarshal(blob, reply); err != nil {
		return 0, fmt.Errorf("%s %s: decode the reply: %w", method, path, err)
	}

	return pid, nil
}

// dial opens one bounded connection to a unix socket.
func (c *Client) dial(socket string) (net.Conn, error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = callTimeout
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socket, err)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, errors.Join(fmt.Errorf("dial %s: %w", socket, err), conn.Close())
	}

	return conn, nil
}

// faultOf is the vmm's reason for a refusal, or its raw reply when it gave none.
func faultOf(blob []byte) string {
	var f fault
	if err := json.Unmarshal(blob, &f); err == nil && f.Message != "" {
		return f.Message
	}

	return strings.TrimSpace(string(blob))
}

// end kills a firecracker that never came up, so a failed start leaves no microVM behind.
func end(cmd *exec.Cmd) error {
	if err := kill(cmd.Process.Pid); err != nil {
		return fmt.Errorf("the firecracker that did not come up: %w", err)
	}

	return nil
}

// tail is the last of the console, for an error message that says why a boot failed.
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
