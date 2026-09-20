package netstack

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

var (
	// remote is where the guest dials: an address nothing on the test host answers, which Dial swaps for the loopback listener.
	remote  = netip.MustParseAddrPort("203.0.113.10:8080")
	allowed = Verdict{Allow: true, Rule: "r1"}
	denied  = Verdict{Allow: false, Rule: "default"}
)

// judged builds a host stack whose judge answers as told, whose host side lands on target, and whose drops land on the channel.
func judged(t *testing.T, verdict Verdict, target string) (*Stack, chan Drop, chan Flow) {
	t.Helper()

	drops := make(chan Drop, 16)
	flows := make(chan Flow, 16)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	s, err := New(Config{
		Address: gateway,
		Drops:   func(d Drop) { drops <- d },
		Judge:   func(f Flow) Verdict { flows <- f; return verdict },
		Dial:    dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close the host stack: %v", err)
		}
	})

	return s, drops, flows
}

func wantFlow(t *testing.T, flows chan Flow, protocol string) {
	t.Helper()
	select {
	case got := <-flows:
		want := Flow{Guest: guestA, Protocol: protocol, Destination: remote}
		if got != want {
			t.Errorf("judged %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the judge never saw the flow")
	}
}

func wantDrop(t *testing.T, drops chan Drop, protocol string) {
	t.Helper()
	select {
	case got := <-drops:
		want := Drop{Guest: guestA, Destination: remote.Addr(), Protocol: protocol, Port: int(remote.Port()), Rule: "default"}
		got.Time = time.Time{}
		if got != want {
			t.Errorf("reported %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no drop reported")
	}
}

// echoTCP answers every connection with what it reads, and counts the connections it took.
func echoTCP(t *testing.T) (string, *atomic.Int32) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var taken atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			taken.Add(1)
			go func() {
				defer conn.Close()
				buf := make([]byte, 64)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()

	return ln.Addr().String(), &taken
}

// A flow the judge allows reaches the host through Dial, with the guest's own bytes both ways.
func TestAnAllowedTCPFlowIsSplicedToTheHost(t *testing.T) {
	target, _ := echoTCP(t)
	host, drops, flows := judged(t, allowed, target)
	guest := attach(t, host, guestA)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := guest.dialTCP(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	wantFlow(t, flows, "tcp")

	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := client.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
	select {
	case got := <-drops:
		t.Fatalf("an allowed flow was reported as a drop: %+v", got)
	default:
	}
}

// A flow the judge refuses gets no answer at all, and the drop record names the rule.
func TestADeniedTCPFlowIsDroppedAndNamesTheRule(t *testing.T) {
	target, taken := echoTCP(t)
	host, drops, flows := judged(t, denied, target)
	guest := attach(t, host, guestA)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if conn, err := guest.dialTCP(ctx, remote); err == nil {
		conn.Close()
		t.Fatal("a denied flow connected")
	}
	wantFlow(t, flows, "tcp")
	wantDrop(t, drops, "tcp")
	if n := taken.Load(); n != 0 {
		t.Errorf("the host listener took %d connections for a denied flow", n)
	}
}

// A UDP flow the judge allows carries datagrams both ways.
func TestAnAllowedUDPFlowIsSplicedToTheHost(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := server.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := server.WriteTo(buf[:n], from); err != nil {
				return
			}
		}
	}()
	host, _, flows := judged(t, allowed, server.LocalAddr().String())
	guest := attach(t, host, guestA)
	if err := guest.knows(gateway, host.cfg.MAC); err != nil {
		t.Fatal(err)
	}

	client, err := guest.dialUDP(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	wantFlow(t, flows, "udp")
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := client.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// A UDP flow the judge refuses is a drop that names the rule, and the guest hears nothing back.
func TestADeniedUDPFlowIsDroppedAndNamesTheRule(t *testing.T) {
	host, drops, flows := judged(t, denied, "127.0.0.1:9")
	guest := attach(t, host, guestA)
	if err := guest.knows(gateway, host.cfg.MAC); err != nil {
		t.Fatal(err)
	}

	client, err := guest.dialUDP(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("out")); err != nil {
		t.Fatal(err)
	}
	wantFlow(t, flows, "udp")
	wantDrop(t, drops, "udp")
}

// A port a listener serves stays closed off the address even with a judge: the listener binds the port alone, so the judge never sees it.
func TestAServedPortOffTheAddressIsNotJudged(t *testing.T) {
	host, drops, flows := judged(t, allowed, "127.0.0.1:9")
	conn, err := host.ListenPacket(remote.Port())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	guest := attach(t, host, guestA)
	if err := guest.knows(gateway, host.cfg.MAC); err != nil {
		t.Fatal(err)
	}

	client, err := guest.dialUDP(remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("out")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-drops:
		got.Time = time.Time{}
		want := Drop{Guest: guestA, Destination: remote.Addr(), Protocol: "udp", Port: int(remote.Port())}
		if got != want {
			t.Errorf("reported %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no drop reported")
	}
	select {
	case f := <-flows:
		t.Fatalf("the judge saw %+v", f)
	default:
	}
}

// A link that closes takes its forwarded flows with it, so the host side ends.
func TestAClosedLinkEndsItsFlows(t *testing.T) {
	target, _ := echoTCP(t)
	host, _, _ := judged(t, allowed, target)
	hostEnd, guestEnd := wire(t)
	link, err := host.Attach(hostEnd, guestA)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := New(Config{Address: guestA, MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	if _, err := guest.Attach(guestEnd, gateway); err != nil {
		t.Fatal(err)
	}
	guest.defaultRoute(gateway)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := guest.dialTCP(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := client.Read(buf); err != nil {
		t.Fatal(err)
	}

	if err := link.Close(); err != nil {
		t.Fatalf("close the link: %v", err)
	}
	host.mu.Lock()
	left := len(link.flows)
	host.mu.Unlock()
	if left != 0 {
		t.Errorf("%d flow sides still tracked after the close", left)
	}
}

// A link that holds its share of flows gets the next one dropped as limit, and a flow that ends gives its place back.
func TestALinkAtItsFlowLimitDropsTheNextFlow(t *testing.T) {
	target, taken := echoTCP(t)
	host, drops, _ := judged(t, allowed, target)
	guest := attach(t, host, guestA)
	link := host.linkOf(guestA)

	host.mu.Lock()
	link.active = maxLinkFlows
	host.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if conn, err := guest.dialTCP(ctx, remote); err == nil {
		conn.Close()
		t.Fatal("a flow over the limit connected")
	}
	select {
	case got := <-drops:
		if got.Rule != RuleLimit {
			t.Errorf("the drop names %q, want %q", got.Rule, RuleLimit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no drop reported")
	}
	if n := taken.Load(); n != 0 {
		t.Errorf("the host listener took %d connections over the limit", n)
	}

	host.mu.Lock()
	link.active = 0
	host.mu.Unlock()
	client, err := guest.dialTCP(t.Context(), remote)
	if err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	during := link.active + host.active
	host.mu.Unlock()
	if during != 2 {
		t.Errorf("an open flow counts %d on the link and the stack together, want 2", during)
	}
	client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		after := link.active + host.active
		host.mu.Unlock()
		if after == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed flow still counts %d", after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
