package portforward_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/portforward"
)

// guests stands in for the sandboxes: each guest port is a loopback listener that answers what it read, upper cased, after the client's half close.
type guests struct {
	mu    sync.Mutex
	ports map[uint16]string
	dials []string
}

func newGuests(t *testing.T, ports ...uint16) *guests {
	t.Helper()
	g := &guests{ports: map[uint16]string{}}
	for _, port := range ports {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen for guest port %d: %v", port, err)
		}
		t.Cleanup(func() { l.Close() })
		g.ports[port] = l.Addr().String()
		go shout(l)
	}

	return g
}

func shout(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			got, err := io.ReadAll(conn)
			if err != nil {
				return
			}
			conn.Write([]byte(strings.ToUpper(string(got))))
		}()
	}
}

func (g *guests) dial(ctx context.Context, id string, port uint16) (net.Conn, error) {
	g.mu.Lock()
	addr, ok := g.ports[port]
	g.dials = append(g.dials, fmt.Sprintf("%s:%d", id, port))
	g.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("dial 127.0.0.1:%d: %w", port, syscall.ECONNREFUSED)
	}
	var d net.Dialer

	return d.DialContext(ctx, "tcp4", addr)
}

func newForwarder(t *testing.T, g *guests) *portforward.Forwarder {
	t.Helper()
	f := portforward.New(g.dial, func(line string) { t.Log(line) }, "shard0")
	t.Cleanup(func() {
		if err := f.Retain(nil); err != nil {
			t.Errorf("close every forward: %v", err)
		}
	})

	return f
}

// freePort answers a host port nothing listens on a moment ago.
func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return uint16(port)
}

// roundTrip sends text to the host port, half closes, and answers what came back.
func roundTrip(t *testing.T, host uint16, text string) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(host))), 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte(text)); err != nil {
		return "", err
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		return "", err
	}
	got, err := io.ReadAll(conn)

	return string(got), err
}

func TestOpenCarriesBytesBothWaysAcrossTheClientsHalfClose(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	host := freePort(t)

	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatalf("open: %v", err)
	}

	got, err := roundTrip(t, host, "ping")
	if err != nil || got != "PING" {
		t.Fatalf("round trip answered %q, %v; want PING", got, err)
	}
	if status := f.Status(host); !status.Listening || status.Sandbox != "sb-1" || status.Error != "" {
		t.Errorf("status %+v, want sb-1 listening with no error", status)
	}
}

// A guest port nothing listens on shows in the status, and the next connection that gets through clears it.
func TestADialTheSandboxRefusedShowsUntilOneGetsThrough(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	host := freePort(t)

	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 9999}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, err := roundTrip(t, host, "ping"); err == nil && got != "" {
		t.Fatalf("round trip to a closed guest port answered %q", got)
	}
	waitFor(t, func() bool { return f.Status(host).Error == "nothing listens on 127.0.0.1:9999 in the sandbox" }, "the refusal in the status")

	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatalf("move the guest port: %v", err)
	}
	if got, err := roundTrip(t, host, "ping"); err != nil || got != "PING" {
		t.Fatalf("round trip answered %q, %v; want PING", got, err)
	}
	if status := f.Status(host); status.Error != "" || !status.Listening {
		t.Errorf("status %+v, want listening with the refusal cleared", status)
	}
}

func TestAHostPortAnotherProcessHoldsIsABindErrorThatSetRetries(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := uint16(taken.Addr().(*net.TCPAddr).Port)
	spec := models.PortForward{HostPort: host, GuestPort: 8100}

	err = f.Open("sb-1", spec)
	var bind *portforward.BindError
	if !errors.As(err, &bind) || bind.Port != host || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("open on a taken port gave %v, want a BindError of EADDRINUSE", err)
	}
	if status := f.Status(host); status.Listening || !strings.Contains(status.Error, "is in use on the host") {
		t.Errorf("status %+v, want not listening with the bind error", status)
	}

	if err := taken.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("sb-1", []models.PortForward{spec}); err != nil {
		t.Fatalf("set once the port is free: %v", err)
	}
	if status := f.Status(host); !status.Listening || status.Error != "" {
		t.Errorf("status %+v, want listening with the bind error cleared", status)
	}
	if got, err := roundTrip(t, host, "ping"); err != nil || got != "PING" {
		t.Fatalf("round trip answered %q, %v; want PING", got, err)
	}
}

// A stop or a pause cuts what the forward carries too, since the sandbox it reaches is going.
func TestCloseSandboxEndsTheListenerAndEveryLiveConnection(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	host, other := freePort(t), freePort(t)
	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}
	if err := f.Open("sb-2", models.PortForward{HostPort: other, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}
	live, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(host))))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.Write([]byte("held")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(g.dialed()) == 1 }, "the live connection to reach the guest")

	if err := f.CloseSandbox("sb-1"); err != nil {
		t.Fatalf("close the sandbox: %v", err)
	}

	if err := live.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Read(make([]byte, 1)); err == nil {
		t.Error("the live connection still reads after the close")
	}
	if _, err := roundTrip(t, host, "ping"); err == nil {
		t.Error("the host port still answers after the close")
	}
	if status := f.Status(host); status != (portforward.Status{}) {
		t.Errorf("status %+v, want nothing left", status)
	}
	if got, err := roundTrip(t, other, "pong"); err != nil || got != "PONG" {
		t.Errorf("the other sandbox's forward answered %q, %v; want PONG", got, err)
	}
}

// The ports task retries every tick, so a host port another process holds logs once, not once a tick.
func TestSetKeepsARefusedPortInItsStatusAndReportsItOnce(t *testing.T) {
	var reports []string
	f := portforward.New(newGuests(t).dial, func(line string) { reports = append(reports, line) }, "shard0")
	t.Cleanup(func() {
		if err := f.Retain(nil); err != nil {
			t.Errorf("close every forward: %v", err)
		}
	})
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	host := uint16(taken.Addr().(*net.TCPAddr).Port)

	for range 3 {
		if err := f.Set("sb-1", []models.PortForward{{HostPort: host, GuestPort: 8100}}); err != nil {
			t.Fatalf("set gave %v, want the refusal kept in the status", err)
		}
	}

	if status := f.Status(host); status.Listening || !strings.Contains(status.Error, "is in use on the host") {
		t.Errorf("status %+v, want not listening with the bind error", status)
	}
	if len(reports) != 1 || !strings.Contains(reports[0], "address already in use") {
		t.Errorf("reports %q, want the host's refusal once", reports)
	}
}

func TestProbeRefusesAHostPortTakenAndLetsGoOfAFreeOne(t *testing.T) {
	f := newForwarder(t, newGuests(t))
	taken, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	free := freePort(t)

	if err := f.Probe(models.PortForward{HostPort: uint16(taken.Addr().(*net.TCPAddr).Port), GuestPort: 1}); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("probe of a taken port gave %v, want EADDRINUSE", err)
	}
	if err := f.Probe(models.PortForward{HostPort: free, GuestPort: 1}); err != nil {
		t.Fatalf("probe of a free port: %v", err)
	}
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(free))))
	if err != nil {
		t.Fatalf("the probe kept the port: %v", err)
	}
	l.Close()
}

func TestSetClosesTheSandboxsOwnForwardsItNoLongerWants(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	keep, gone := freePort(t), freePort(t)
	if err := f.Set("sb-1", []models.PortForward{{HostPort: keep, GuestPort: 8100}, {HostPort: gone, GuestPort: 8100}}); err != nil {
		t.Fatal(err)
	}

	if err := f.Set("sb-1", []models.PortForward{{HostPort: keep, GuestPort: 8100}}); err != nil {
		t.Fatal(err)
	}

	if !f.Status(keep).Listening || f.Status(gone).Listening {
		t.Errorf("keep %+v gone %+v, want only keep listening", f.Status(keep), f.Status(gone))
	}
}

func TestCloseLeavesAnotherSandboxsForwardAlone(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	host := freePort(t)
	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}

	if err := f.Close("sb-2", host); err != nil {
		t.Fatal(err)
	}

	if !f.Status(host).Listening {
		t.Error("a close for sb-2 ended the forward of sb-1")
	}
}

func TestRetainEndsTheForwardsOfASandboxWhoseRecordIsGone(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	kept, gone := freePort(t), freePort(t)
	if err := f.Open("sb-1", models.PortForward{HostPort: kept, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}
	if err := f.Open("sb-2", models.PortForward{HostPort: gone, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}

	if err := f.Retain(map[string]bool{"sb-1": true}); err != nil {
		t.Fatal(err)
	}

	if !f.Status(kept).Listening || f.Status(gone).Listening {
		t.Errorf("kept %+v gone %+v, want only sb-1 listening", f.Status(kept), f.Status(gone))
	}
}

// Going public rebinds on every interface, which a loopback listener of the same port would refuse, so the old one goes first.
func TestOpenAgainFlipsTheForwardToPublicAndBack(t *testing.T) {
	g := newGuests(t, 8100)
	f := newForwarder(t, g)
	host := freePort(t)
	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatal(err)
	}

	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100, Public: true}); err != nil {
		t.Fatalf("go public: %v", err)
	}
	if got, err := roundTrip(t, host, "ping"); err != nil || got != "PING" {
		t.Fatalf("public round trip answered %q, %v; want PING", got, err)
	}

	if err := f.Open("sb-1", models.PortForward{HostPort: host, GuestPort: 8100}); err != nil {
		t.Fatalf("go private: %v", err)
	}
	if got, err := roundTrip(t, host, "ping"); err != nil || got != "PING" {
		t.Fatalf("private round trip answered %q, %v; want PING", got, err)
	}
}

func TestReachableOfAPrivateForwardIsTheLoopbackAlone(t *testing.T) {
	f := newForwarder(t, newGuests(t))

	got, err := f.Reachable(false)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Address != "127.0.0.1" {
		t.Errorf("reachable on %+v, want 127.0.0.1 alone", got)
	}
}

func TestReachableOfAPublicForwardIsEveryIPv4OfAnInterfaceThatIsUp(t *testing.T) {
	f := newForwarder(t, newGuests(t))

	got, err := f.Reachable(true)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) == 0 || got[0].Address != "127.0.0.1" {
		t.Errorf("reachable on %+v, want the loopback first", got)
	}
	for _, e := range got {
		if net.ParseIP(e.Address).To4() == nil || e.Interface == "shard0" {
			t.Errorf("reachable on %+v, which is no IPv4 or is the sandbox bridge", e)
		}
	}
}

func (g *guests) dialed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]string(nil), g.dials...)
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
