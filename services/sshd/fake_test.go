package sshd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// command plays the guest side of one exec: it reads and writes the attach's streams and says how it ended.
type command func(e *fakeExec, streams sandbox.Streams) models.ExitStatus

type fakeExec struct {
	id      string
	req     sandbox.ExecRequest
	signals chan string
	resizes chan sandbox.TerminalSize
	done    chan struct{}

	mu      sync.Mutex
	streams sandbox.Streams
	exit    models.ExitStatus
	deleted bool
}

// fakeSandboxes is the sandbox service as the exec path shows it to sshd, with each command a function.
type fakeSandboxes struct {
	command   command
	createErr error
	dial      func(port uint16) (net.Conn, error)

	mu       sync.Mutex
	execs    []*fakeExec
	dialed   []uint16
	attached atomic.Int32
}

func (f *fakeSandboxes) CreateExec(_ context.Context, _ string, req sandbox.ExecRequest) (models.Exec, error) {
	if f.createErr != nil {
		return models.Exec{}, f.createErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	e := &fakeExec{
		id:      fmt.Sprintf("%016x", len(f.execs)+1),
		req:     req,
		signals: make(chan string, 8),
		resizes: make(chan sandbox.TerminalSize, 8),
		done:    make(chan struct{}),
	}
	f.execs = append(f.execs, e)

	return models.Exec{ID: e.id, State: models.ExecRunning}, nil
}

func (f *fakeSandboxes) Attach(ctx context.Context, _, execID string, streams sandbox.Streams) (sandbox.Attached, error) {
	f.attached.Add(1)
	defer f.attached.Add(-1)
	e, err := f.exec(execID)
	if err != nil {
		return sandbox.Attached{}, err
	}

	e.mu.Lock()
	e.streams = streams
	e.mu.Unlock()
	go func() {
		exit := f.command(e, streams)
		e.mu.Lock()
		e.exit = exit
		e.mu.Unlock()
		close(e.done)
	}()

	select {
	case <-e.done:
	case <-ctx.Done():
	}
	if err := streams.StopStdin(); err != nil {
		return sandbox.Attached{}, err
	}
	if ctx.Err() != nil {
		return sandbox.Attached{}, ctx.Err()
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	return sandbox.Attached{Exit: e.exit}, nil
}

func (f *fakeSandboxes) ResizeExec(_ context.Context, _, execID string, size sandbox.TerminalSize) error {
	e, err := f.exec(execID)
	if err != nil {
		return err
	}
	e.resizes <- size

	return nil
}

// KillExec delivers the signal to the command, and a KILL also cuts its stdin, which ends any command that reads it.
func (f *fakeSandboxes) KillExec(_ context.Context, _, execID, signal string) error {
	e, err := f.exec(execID)
	if err != nil {
		return err
	}
	select {
	case <-e.done:
		return &sandbox.ExecExitedError{ID: execID}
	default:
	}

	e.signals <- signal
	if signal != "KILL" {
		return nil
	}
	e.mu.Lock()
	stop := e.streams.StopStdin
	e.mu.Unlock()
	if stop == nil {
		return nil
	}

	return stop()
}

func (f *fakeSandboxes) WaitExec(ctx context.Context, _, execID string) (models.Exec, error) {
	e, err := f.exec(execID)
	if err != nil {
		return models.Exec{}, err
	}
	select {
	case <-e.done:
		return models.Exec{ID: execID, State: models.ExecExited}, nil
	case <-ctx.Done():
		return models.Exec{}, ctx.Err()
	}
}

func (f *fakeSandboxes) DeleteExec(_ context.Context, _, execID string) error {
	e, err := f.exec(execID)
	if err != nil {
		return err
	}
	select {
	case <-e.done:
	default:
		return &sandbox.ExecRunningError{ID: execID}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleted = true

	return nil
}

func (f *fakeSandboxes) DialPort(_ context.Context, _ string, port uint16) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, port)
	f.mu.Unlock()

	return f.dial(port)
}

func (f *fakeSandboxes) exec(execID string) (*fakeExec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.execs, func(e *fakeExec) bool { return e.id == execID })
	if i < 0 {
		return nil, models.NotFound(nil, "exec "+execID+" not found")
	}

	return f.execs[i], nil
}

func (f *fakeSandboxes) dials() []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.dialed)
}

// only is the one exec the test's session made.
func (f *fakeSandboxes) only(t *testing.T) *fakeExec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execs) != 1 {
		t.Fatalf("execs = %d, want 1", len(f.execs))
	}

	return f.execs[0]
}

// served is one ssh connection to a Server over loopback TCP, since net.Pipe deadlocks on the version exchange.
type served struct {
	client *ssh.Client
	cancel context.CancelFunc
	result chan error
}

func serve(t *testing.T, sandboxes Sandboxes, user string) *served {
	t.Helper()
	signer, err := LoadHostKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(signer, sandboxes, func(msg string) { t.Errorf("report: %s", msg) })

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err := errors.Join(err, l.Close()); err != nil {
			result <- err

			return
		}
		result <- srv.Serve(ctx, conn, Options{Sandbox: "web1", Explain: func(err error) string { return err.Error() }})
	}()

	conn, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ClientConfig{User: user, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second}
	sconn, chans, reqs, err := ssh.NewClientConn(conn, l.Addr().String(), config)
	if err != nil {
		t.Fatal(err)
	}
	s := &served{client: ssh.NewClient(sconn, chans, reqs), cancel: cancel, result: result}
	t.Cleanup(func() {
		cancel()
		if err := s.client.Close(); err != nil && !gone(err) {
			t.Errorf("close the client: %v", err)
		}
		s.wait(t)
	})

	return s
}

// wait is Serve's return, which comes only once every channel it opened has ended.
func (s *served) wait(t *testing.T) {
	t.Helper()
	select {
	case err, ok := <-s.result:
		if ok && err != nil {
			t.Errorf("Serve = %v", err)
		}
		if ok {
			close(s.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its client left")
	}
}

// settles waits for the goroutine count to come back to before, so a leak fails the test rather than slowing the next.
func settles(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines rose from %d to %d:\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
