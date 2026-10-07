//go:build integration

package cli

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// portListener greets each connection to port 8000 on the sandbox's own loopback, then echoes it, so only a forward reaches it.
var portListener = []string{"/bin/sh", "-c", "exec nc -lk -p 8000 -s 127.0.0.1 -e /bin/sh -c 'echo hello; exec cat'"}

// TestAForwardOutlivesAStopAStartAndADaemonRestartAndGoesWithItsSandbox drives shard port on the provider the suite's daemon runs (SHARD-789).
func TestAForwardOutlivesAStopAStartAndADaemonRestartAndGoesWithItsSandbox(t *testing.T) {
	app, out := newCreateApp(t)

	id := runDetached(t, app, out, portListener...)
	t.Cleanup(func() { cleanUp(t, app, id) })

	hostPort := freePort(t)
	if err := app.Run(t.Context(), []string{"port", "add", id, fmt.Sprintf("%d:8000", hostPort)}); err != nil {
		t.Fatalf("port add: %v", err)
	}
	echoesThrough(t, hostPort)

	if err := app.Run(t.Context(), []string{"stop", id}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	refuses(t, hostPort, "while its sandbox was stopped")
	if err := app.Run(t.Context(), []string{"start", id}); err != nil {
		t.Fatalf("start: %v", err)
	}
	echoesThrough(t, hostPort)

	if err := daemonUnderTest.halt(); err != nil {
		t.Fatalf("halt the daemon: %v", err)
	}
	refuses(t, hostPort, "with no daemon to serve it")
	resumed, err := daemonOver(daemonUnderTest.root)
	if err != nil {
		t.Fatalf("serve the root again: %v", err)
	}
	daemonUnderTest = resumed
	echoesThrough(t, hostPort)

	if err := app.Run(t.Context(), []string{"remove", "--force", id}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	refuses(t, hostPort, "after its sandbox was removed")
}

// echoesThrough waits for the guest's greeting on the host port, then sends 256 KiB and reads the same bytes back.
func echoesThrough(t *testing.T, hostPort uint16) {
	t.Helper()

	conn := greeted(t, hostPort)
	defer conn.Close()

	sent := make([]byte, 256<<10)
	if _, err := rand.Read(sent); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(waitBudget)); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write(sent)
		wrote <- err
	}()
	got := make([]byte, len(sent))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read the echo back: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("write into the sandbox: %v", err)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("the echo differs from what went in")
	}
}

// greeted dials until the guest greets, since a start or a daemon restart opens the host port in its own time.
func greeted(t *testing.T, hostPort uint16) net.Conn {
	t.Helper()

	var last error
	for deadline := time.Now().Add(waitBudget); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		conn, err := net.DialTimeout("tcp", hostAddress(hostPort), 5*time.Second)
		if err != nil {
			last = err
			continue
		}
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(errors.Join(err, conn.Close()))
		}
		line := make([]byte, len("hello\n"))
		if _, err := io.ReadFull(conn, line); err != nil {
			last = errors.Join(fmt.Errorf("read the greeting: %w", err), conn.Close())
			continue
		}
		if string(line) == "hello\n" {
			return conn
		}
		last = errors.Join(fmt.Errorf("the guest greeted with %q", line), conn.Close())
	}
	t.Fatalf("host port %d never carried the guest's greeting: %v", hostPort, last)

	return nil
}

// refuses proves nothing answers on the host port, which a forward that outlived its run would.
func refuses(t *testing.T, hostPort uint16, when string) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", hostAddress(hostPort), time.Second)
	if err != nil {
		return
	}
	t.Fatalf("host port %d answered %s: %v", hostPort, when, conn.Close())
}

// freePort is a host port nothing listens on now; port add binds it a moment later.
func freePort(t *testing.T) uint16 {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return uint16(port)
}

func hostAddress(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}
