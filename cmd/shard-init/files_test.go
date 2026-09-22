package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// startFiles brings the guest up and attaches a control connection first, as a host does, so the files port is listening.
func startFiles(t *testing.T) supervisor.Dialer {
	t.Helper()
	_, dial := startTransport(t)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	return dial
}

// payload crosses the frame bound several times, so a copy that lands whole did not fit in one write.
func payload(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 3*supervisor.MaxPayload+17)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("fill the payload: %v", err)
	}

	return b
}

func TestTransportStatReportsTheShape(t *testing.T) {
	dial := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}

	stat, err := supervisor.Stat(testContext(t), dial, path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Name != "hello.txt" || stat.Size != 5 || fs.FileMode(stat.Mode).Perm() != 0o640 || stat.Dir {
		t.Fatalf("stat = %+v, want hello.txt, 5 bytes, 0640, a file", stat)
	}
	folder, err := supervisor.Stat(testContext(t), dial, dir)
	if err != nil {
		t.Fatalf("stat the dir: %v", err)
	}
	if !folder.Dir {
		t.Fatalf("stat = %+v, want a directory", folder)
	}
}

func TestTransportPutLandsAWholeFile(t *testing.T) {
	dial := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	want := payload(t)

	if err := supervisor.Put(testContext(t), dial, path, 0o600, int64(len(want)), bytes.NewReader(want)); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the file holds %d bytes, want %d equal ones", len(got), len(want))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode())
	}
	assertNoTemp(t, dir)
}

func TestTransportGetStreamsTheFile(t *testing.T) {
	dial := startFiles(t)
	path := filepath.Join(t.TempDir(), "blob")
	want := payload(t)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	stat, err := supervisor.Get(testContext(t), dial, path, &got)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stat.Size != int64(len(want)) || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("get gave %d bytes with stat %+v, want %d equal ones", got.Len(), stat, len(want))
	}
}

func TestTransportFilesRefuseWhatTheyCannotCopy(t *testing.T) {
	dial := startFiles(t)
	ctx := testContext(t)
	dir := t.TempDir()

	if _, err := supervisor.Get(ctx, dial, dir, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("get of a directory gave %v, want it named as one", err)
	}
	if _, err := supervisor.Stat(ctx, dial, "relative/name"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("stat of a relative path gave %v, want a refusal", err)
	}
	if _, err := supervisor.Stat(ctx, dial, filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("stat of a missing path gave %v, want the guest's not-exist", err)
	}
	// A fifo would block the guest's open forever, so the get refuses it before the reply.
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Get(ctx, dial, fifo, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("get of a fifo gave %v, want a refusal", err)
	}
	if got, err := supervisor.Stat(ctx, dial, fifo); err != nil || fs.FileMode(got.Mode).Type() != fs.ModeNamedPipe || got.Dir {
		t.Fatalf("stat of a fifo gave %+v, %v, want its mode", got, err)
	}
	// The guest refuses the header, so its reason must beat the broken pipe the rest of the payload meets.
	want := payload(t)
	err := supervisor.Put(ctx, dial, filepath.Join(dir, "nowhere", "blob"), 0o600, int64(len(want)), bytes.NewReader(want))
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("put under a missing dir gave %v, want the guest's not-exist", err)
	}
}

func TestTransportPutThatDiesMidwayLeavesTheOldFile(t *testing.T) {
	dial := startFiles(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "blob")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The source runs out before the promised size, so the host hangs up with the guest's copy short.
	want := payload(t)
	err := supervisor.Put(testContext(t), dial, path, 0o600, int64(len(want))+1, bytes.NewReader(want))
	if err == nil {
		t.Fatal("a short put succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old" {
		t.Fatalf("the old file reads %q (%v), want it untouched", got, err)
	}
	// The guest removes its temp name once the connection drops; give it the moment that takes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && hasTemp(t, dir) {
		time.Sleep(20 * time.Millisecond)
	}
	assertNoTemp(t, dir)
}

func hasTemp(t *testing.T, dir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".shard-put-*"))
	if err != nil {
		t.Fatal(err)
	}

	return len(matches) > 0
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	if hasTemp(t, dir) {
		t.Fatalf("a .shard-put temp name is left under %s", dir)
	}
}

func TestFailTellsTheAttachedHostBeforeTheExit(t *testing.T) {
	host, guest := net.Pipe()
	defer host.Close()
	tr := &transport{control: guest, attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})

	cause := errors.New("power off: no such device")
	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errors.Join(errSupervisor, cause)) }()

	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m supervisor.Message
	if err := supervisor.ReadMessage(bufio.NewReader(host), &m); err != nil {
		t.Fatalf("read the death: %v", err)
	}
	if m.Kind != supervisor.KindSupervisorFailed || m.Exit == nil || m.Exit.Code != models.SupervisorFailedExitCode || !strings.Contains(m.Error, cause.Error()) {
		t.Fatalf("the host read %+v, want supervisor-failed with code 125 and the cause", m)
	}
	err := <-failed
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("fail returned %v, which maps to %d, want 125", err, exitCodeFor(err))
	}
}

// hasControl says whether a host is attached, which a test waits on after a hang-up.
func (t *transport) hasControl() bool {
	t.controlMu.Lock()
	defer t.controlMu.Unlock()

	return t.control != nil
}

// deadTransport is a guest whose loop has ended: the listeners are up, the control port accepts, and nobody runs supervise.
func deadTransport(t *testing.T) (*transport, supervisor.Dialer) {
	t.Helper()
	dir := shortDir(t)
	l, err := net.Listen("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.ControlPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tr := &transport{attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})
	go tr.acceptControl(l)

	return tr, func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	}
}

// expectDeath reads the state replay and then the death a host attaching after the supervisor failed must hear.
func expectDeath(t *testing.T, c *supervisor.Control) {
	t.Helper()
	for _, want := range []string{supervisor.KindState, supervisor.KindSupervisorFailed} {
		m, err := c.Next()
		if err != nil || m.Kind != want {
			t.Fatalf("the host read %+v (%v), want %s", m, err, want)
		}
	}
}

func TestFailWaitsForTheFirstHost(t *testing.T) {
	tr, dial := deadTransport(t)
	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errSupervisor) }()

	// The host attaches a moment later, the way one does while the guest still boots, through the real attach.
	time.Sleep(50 * time.Millisecond)
	c, err := supervisor.Connect(testContext(t), dial)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	expectDeath(t, c)
	if err := <-failed; !errors.Is(err, errSupervisor) {
		t.Fatalf("fail returned %v, want the supervisor error", err)
	}
}

func TestFailWaitsPastAHostThatLeft(t *testing.T) {
	tr, dial := deadTransport(t)
	ctx := testContext(t)
	// The guest loop is still alive while the first host comes and goes.
	alive := make(chan struct{})
	go func() {
		for {
			select {
			case command := <-tr.g.commands:
				command()
			case <-alive:
				return
			}
		}
	}()
	first, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := first.Next(); err != nil || m.Kind != supervisor.KindState {
		t.Fatalf("the first host read %+v, %v", m, err)
	}
	// The first host hangs up before the death; the guest must forget it and wait for the next.
	first.Close()
	for tr.hasControl() {
		time.Sleep(10 * time.Millisecond)
	}
	close(alive)

	failed := make(chan error, 1)
	go func() { failed <- tr.fail(errSupervisor) }()
	time.Sleep(50 * time.Millisecond)
	second, err := supervisor.Connect(ctx, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	expectDeath(t, second)
	if err := <-failed; !errors.Is(err, errSupervisor) {
		t.Fatalf("fail returned %v, want the supervisor error", err)
	}
}

func TestFailGivesUpWhenNoHostComes(t *testing.T) {
	old := failureGrace
	failureGrace = 100 * time.Millisecond
	t.Cleanup(func() { failureGrace = old })
	tr := &transport{attached: make(chan struct{}, 1)}
	tr.g = newGuest(tr, restartPolicy{})

	err := tr.fail(errSupervisor)
	if !errors.Is(err, errSupervisor) || !strings.Contains(err.Error(), "no host attached") {
		t.Fatalf("fail returned %v, want the supervisor error and no host", err)
	}
	if exitCodeFor(err) != models.SupervisorFailedExitCode {
		t.Fatalf("the exit code is %d, want 125", exitCodeFor(err))
	}
}
