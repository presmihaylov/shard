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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/peercred"
	"github.com/presmihaylov/shard/pkg/pidpin"
)

// Client speaks to one firecracker over its API socket. It holds no connection between calls, so a daemon restart loses nothing.
type Client struct {
	socket string
	vsock  string
}

// Start runs firecracker through the jailer, puts the microVM in over the API and boots it; the vmm leads its own session, so it outlives this process.
func Start(ctx context.Context, jail Jail, cfg Config) (*Client, Info, error) {
	client := jailed(jail, cfg.Socket, cfg.Vsock)
	pid, err := client.spawn(ctx, jail, cfg.Socket, cfg.Console)
	if err != nil {
		return nil, Info{}, err
	}
	if err := client.configure(cfg); err != nil {
		return nil, Info{}, errors.Join(err, end(pid))
	}

	return client.up(pid)
}

// Restore runs a fresh firecracker through the jailer and brings the snapshot back in it, running; a snapshot loads only into a process that booted nothing.
func Restore(ctx context.Context, jail Jail, snap Snapshot) (*Client, Info, error) {
	client := jailed(jail, snap.Socket, snap.Vsock)
	pid, err := client.spawn(ctx, jail, snap.Socket, snap.Console)
	if err != nil {
		return nil, Info{}, err
	}
	if err := client.load(snap); err != nil {
		return nil, Info{}, errors.Join(err, end(pid))
	}

	return client.up(pid)
}

// jailed is the client of a vmm in jail, which dials its sockets by their host paths.
func jailed(jail Jail, socket, vsock string) *Client {
	client := &Client{socket: jail.Host(socket)}
	if vsock != "" {
		client.vsock = jail.Host(vsock)
	}

	return client
}

// spawn runs the jailer on the socket and waits for the vmm's API; the vmm is the caller's to end when what follows fails.
func (c *Client) spawn(ctx context.Context, jail Jail, socket, console string) (int, error) {
	if err := c.claim(); err != nil {
		return 0, err
	}

	log, err := os.OpenFile(console, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open the console log: %w", err)
	}
	defer log.Close()

	pid, err := runJailer(ctx, jail, socket, log)
	if err != nil {
		return 0, err
	}
	vmm, err := watch(pid)
	if err != nil {
		return 0, errors.Join(err, end(pid))
	}
	err = c.await(ctx, vmm, console)

	return pid, errors.Join(err, vmm.release())
}

// runJailer runs the jailer to its exit, which comes once it cloned the vmm and wrote its pid; a refusal is on the jailer's stderr, the console.
func runJailer(ctx context.Context, jail Jail, socket string, log *os.File) (int, error) {
	// No --resource-limit: the jailer's 2048 open files outlast the vsock muxer's cap of 1023 connections, the one fd count a sandbox grows.
	args := []string{
		"--id", jail.ID, "--exec-file", jail.Exec, "--uid", strconv.Itoa(jail.UID), "--gid", strconv.Itoa(jail.UID),
		"--chroot-base-dir", jail.Base, "--cgroup-version", "2", "--parent-cgroup", jail.Cgroup, "--new-pid-ns",
	}
	if jail.Netns != "" {
		args = append(args, "--netns", jail.Netns)
	}
	cmd := exec.Command(jail.Jailer, append(args, "--", "--api-sock", socket)...)
	cmd.Stdout = log
	cmd.Stderr = log
	// The jailer has the vmm call setsid when it leads a session itself, so the vmm leads a group KillPID ends whole.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start the jailer: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		if err != nil {
			return 0, fmt.Errorf("the jailer exited before the vmm ran (%w): %s", err, tail(log.Name()))
		}
	case <-ctx.Done():
		return 0, errors.Join(ctx.Err(), abandon(jail, cmd.Process.Pid, exited))
	case <-time.After(startTimeout):
		return 0, errors.Join(fmt.Errorf("the jailer did not exit within %s: %s", startTimeout, tail(log.Name())), abandon(jail, cmd.Process.Pid, exited))
	}

	return pidOf(jail)
}

// abandon ends a jailer that did not exit, and the vmm it may have cloned and named already.
func abandon(jail Jail, jailer int, exited <-chan error) error {
	if err := KillPID(jailer); err != nil {
		return fmt.Errorf("end the jailer: %w", err)
	}
	// The jailer leads the group the kill went to, so its wait error is that kill.
	<-exited
	pid, err := pidOf(jail)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	return end(pid)
}

// pidOf reads the vmm's host pid, which the jailer writes into the chroot once it cloned the vmm.
func pidOf(jail Jail) (int, error) {
	path := jail.Host(filepath.Base(jail.Exec) + ".pid")
	blob, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read the vmm pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("the jailer wrote %q to %s, not a pid", blob, path)
	}

	return pid, nil
}

// up reads the state the microVM settled in, which is what every spawn reports back.
func (c *Client) up(pid int) (*Client, Info, error) {
	info, err := c.State(context.Background())
	if err != nil {
		return nil, Info{}, errors.Join(fmt.Errorf("read the state after the boot: %w", err), end(pid))
	}

	return c, info, nil
}

// load puts the snapshot in, paused, then resumes the guest; each drive opens the in-jail path the snapshot recorded, which is this jail's own file.
func (c *Client) load(snap Snapshot) error {
	body := snapshotLoad{
		StatePath: snap.State,
		Memory:    memoryBackend{Type: "File", Path: snap.Memory},
		// A snapshot does not keep the dirty-page log, so each load turns it on again.
		TrackDirtyPages: true,
		// This moves only kvm-clock up to now, on x86_64; a guest on tsc keeps the time of its save until the reseed (SHARD-776).
		ClockRealtime: true,
	}
	if snap.Tap != "" {
		body.Network = []networkOverride{{ID: guestInterface, HostDev: snap.Tap}}
	}
	if snap.Vsock != "" {
		body.Vsock = &vsockOverride{Path: snap.Vsock}
	}
	if err := c.put("/snapshot/load", body); err != nil {
		return err
	}

	return c.Resume()
}

// Pause stops the vCPUs; the guest keeps its memory and its devices, and answers nothing until Resume.
func (c *Client) Pause() error {
	return c.patch("/vm", vmState{State: "Paused"})
}

// Resume starts the vCPUs of a paused microVM.
func (c *Client) Resume() error {
	return c.patch("/vm", vmState{State: "Resumed"})
}

// Snapshot writes the device state and the guest's memory to two files, a Diff merged into memory when that file is already the guest's size; firecracker wants the microVM paused first, and within bounds it, not callTimeout, as a cut create leaves the vmm writing on.
func (c *Client) Snapshot(within time.Duration, kind SnapshotType, state, memory string) error {
	conn, err := c.dial(context.Background(), c.socket, within)
	if err != nil {
		return fmt.Errorf("PUT /snapshot/create: %w", err)
	}
	defer conn.Close()

	return request(context.Background(), conn, http.MethodPut, "/snapshot/create", snapshotCreate{Type: kind, StatePath: state, MemoryPath: memory}, nil)
}

// claim refuses a socket a live vmm answers on, and clears the paths a dead one left, which firecracker refuses to reuse.
func (c *Client) claim() error {
	info, err := c.State(context.Background())
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

// absent is a socket with no vmm behind it: never made, or its owner exited, or is exiting, and left the path.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) || errors.Is(err, ErrExiting)
}

// await polls the API until the vmm answers; one that exits or stays silent is reported with its console.
func (c *Client) await(ctx context.Context, vmm *process, console string) error {
	deadline := time.After(startTimeout)
	for {
		info, err := c.State(ctx)
		if err == nil && info.PID != vmm.pid {
			return errors.Join(fmt.Errorf("%w: pid %d answers on %s", ErrSocketInUse, info.PID, c.socket), end(vmm.pid))
		}
		if err == nil {
			return nil
		}
		gone, watchErr := vmm.exited()
		if watchErr != nil {
			return errors.Join(watchErr, end(vmm.pid))
		}
		if gone {
			return fmt.Errorf("firecracker %d exited before its api answered: %s", vmm.pid, tail(console))
		}

		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), end(vmm.pid))
		case <-deadline:
			return errors.Join(fmt.Errorf("the api socket did not answer within %s: %s", startTimeout, tail(console)), end(vmm.pid))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// configure puts the machine, the boot source, the drives, the network and the vsock in, in the order the API wants them, then starts the instance.
func (c *Client) configure(cfg Config) error {
	// The dirty-page log keeps a Diff to the pages the guest wrote; without it firecracker counts every resident page (SHARD-458).
	if err := c.put("/machine-config", machineConfig{VCPUs: cfg.VCPUs, MemoryMiB: cfg.MemoryMiB, TrackDirtyPages: true}); err != nil {
		return err
	}
	if err := c.put("/boot-source", bootSource{Kernel: cfg.Kernel, Initrd: cfg.Initrd, Args: cfg.Cmdline}); err != nil {
		return err
	}
	for _, d := range cfg.Drives {
		wire := drive{ID: d.ID, Path: d.Path, ReadOnly: d.ReadOnly}
		// A writable drive honours a guest flush only with Writeback; a read-only one never writes, so it keeps the default.
		if !d.ReadOnly {
			wire.CacheType = cacheWriteback
		}
		if err := c.put("/drives/"+d.ID, wire); err != nil {
			return err
		}
	}
	if cfg.Network.Tap != "" {
		iface := networkInterface{ID: guestInterface, HostDev: cfg.Network.Tap, MAC: cfg.Network.MAC}
		if err := c.put("/network-interfaces/"+guestInterface, iface); err != nil {
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

// Open names the vmm on its sockets and asks it nothing, so a caller can still kill one that never answers.
func Open(socket, vsock string) *Client { return &Client{socket: socket, vsock: vsock} }

// Adopt takes a firecracker that is already running, by its sockets, and proves it answers by ctx's deadline; one that fails after the dial is still named in the Info.
func Adopt(ctx context.Context, socket, vsock string) (*Client, Info, error) {
	client, info, silent, err := AdoptPinned(ctx, socket, vsock)
	if silent != nil {
		return nil, info, errors.Join(err, silent.Close())
	}

	return client, info, err
}

// AdoptPinned is Adopt that pins the peer of its dial before it asks, and hands the pin back when that peer stays silent to the deadline; the caller closes it.
func AdoptPinned(ctx context.Context, socket, vsock string) (*Client, Info, *pidpin.Process, error) {
	client := Open(socket, vsock)
	info, pin, err := client.StatePinned(ctx)
	if err != nil {
		return nil, info, pin, fmt.Errorf("adopt the vmm on %s: %w", socket, err)
	}
	if err := pin.Close(); err != nil {
		return nil, info, nil, fmt.Errorf("adopt the vmm on %s: %w", socket, err)
	}

	return client, info, nil, nil
}

// StatePinned is State on a connection whose peer is pinned before the request; the pin comes back when the vmm answers or stays silent to the deadline, and the caller closes it.
func (c *Client) StatePinned(ctx context.Context) (Info, *pidpin.Process, error) {
	conn, pid, err := c.peer(ctx, http.MethodGet, "/")
	if err != nil {
		return Info{}, nil, err
	}
	defer conn.Close()
	pin, err := pidpin.Open(pid)
	if err != nil {
		return Info{PID: pid}, nil, fmt.Errorf("GET /: %w", err)
	}
	var got instance
	err = request(ctx, conn, http.MethodGet, "/", nil, &got)
	// Only a peer that still holds the connection answers or lets the request time out, so either way it outlived the pin, which is therefore that vmm.
	if err == nil {
		return Info{State: got.State, PID: pid}, pin, nil
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return Info{PID: pid}, pin, err
	}

	return Info{PID: pid}, nil, errors.Join(err, pin.Close())
}

// State asks the vmm what the microVM is doing and who answers, the pid being the socket's peer, by ctx's deadline if it comes first; a read that fails after the dial still names that peer.
func (c *Client) State(ctx context.Context) (Info, error) {
	var got instance
	pid, err := c.call(ctx, http.MethodGet, "/", nil, &got)
	if err != nil {
		return Info{PID: pid}, err
	}

	return Info{State: got.State, PID: pid}, nil
}

// Kill ends the vmm by the pid behind its socket, the only forced stop firecracker has; a socket nobody listens on is already ended.
func (c *Client) Kill() error {
	deadline := time.Now().Add(resetGrace)
	for {
		pid, err := c.owner()
		if err == nil {
			return KillPID(pid)
		}
		if absent(err) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("kill: %w", err)
		}
		time.Sleep(resetPoll)
	}
}

// owner is the pid that listens on the API socket, which the kernel attests at the dial: a vmm too wedged to answer HTTP still owns it (SHARD-339).
func (c *Client) owner() (int, error) {
	conn, err := c.dial(context.Background(), c.socket, callTimeout)
	if err != nil {
		return 0, err
	}
	pid, err := peercred.PID(conn)
	if err != nil {
		return 0, errors.Join(fmt.Errorf("read the peer of the api socket: %w", err), conn.Close())
	}
	if err := conn.Close(); err != nil {
		return 0, fmt.Errorf("close the api socket: %w", err)
	}

	return pid, nil
}

// KillPID sends SIGKILL and no other signal, to the group the vmm leads or else to it alone: as PID 1 of its pid namespace it drops any signal it has no handler for.
func KillPID(pid int) error {
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
	conn, err := c.dial(context.Background(), c.vsock, callTimeout)
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
	_, err := c.call(context.Background(), http.MethodPut, path, body, nil)

	return err
}

func (c *Client) patch(path string, body any) error {
	_, err := c.call(context.Background(), http.MethodPatch, path, body, nil)

	return err
}

// call is one request on its own connection, bounded from dial to reply, and the pid of the peer that took the dial, kept on a later failure.
func (c *Client) call(ctx context.Context, method, path string, body, reply any) (int, error) {
	conn, pid, err := c.peer(ctx, method, path)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	return pid, request(ctx, conn, method, path, body, reply)
}

// peer dials the API socket and names the process behind the connection, which the kernel attests; a peer already in its exit is ErrExiting, since it may never answer nor end the connection.
func (c *Client) peer(ctx context.Context, method, path string) (net.Conn, int, error) {
	conn, err := c.dial(ctx, c.socket, callTimeout)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	pid, err := peercred.PID(conn)
	if err != nil {
		return nil, 0, errors.Join(fmt.Errorf("%s %s: read the peer of the api socket: %w", method, path, err), conn.Close())
	}
	leaving, err := exiting(pid)
	if err != nil {
		return nil, 0, errors.Join(fmt.Errorf("%s %s: %w", method, path, err), conn.Close())
	}
	if leaving {
		return nil, 0, errors.Join(fmt.Errorf("%s %s: pid %d: %w", method, path, pid, ErrExiting), conn.Close())
	}

	return conn, pid, nil
}

// request sends one request on conn and reads its reply, by conn's deadline.
func request(ctx context.Context, conn net.Conn, method, path string, body, reply any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: marshal the request: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, payload)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	blob, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read the reply: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, faultOf(blob))
	}
	if reply == nil {
		return nil
	}
	if err := json.Unmarshal(blob, reply); err != nil {
		return fmt.Errorf("%s %s: decode the reply: %w", method, path, err)
	}

	return nil
}

// dial opens one connection to a unix socket, bounded by within or ctx's deadline, whichever comes first.
func (c *Client) dial(ctx context.Context, socket string, within time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(within)
	if due, ok := ctx.Deadline(); ok && due.Before(deadline) {
		deadline = due
	}
	dialer := net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socket, err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
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
func end(pid int) error {
	if err := KillPID(pid); err != nil {
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
