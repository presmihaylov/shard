package vz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

// Info is what every verb reports back: the VM's state, the shim's pid and the identifier a restore must reuse.
type Info struct {
	State     State
	PID       int
	MachineID string
}

// The shim answers on its socket once the VM is up; a boot that takes longer than this is a failure to report.
const startTimeout = 30 * time.Second

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
	conn, err := net.Dial("unix", c.socket)
	if err != nil {
		return nil, Info{}, fmt.Errorf("dial the shim: %w", err)
	}

	var reply response
	err = writeFrame(conn, req)
	if err == nil {
		err = readFrame(conn, &reply)
	}
	if err == nil {
		err = reply.err()
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
