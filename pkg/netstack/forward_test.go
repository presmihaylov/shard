package netstack

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// A guest that resets its flow ended it, which is no fault of the link: gVisor sends the reset for a socket closed over unread bytes.
func TestAGuestResetEndsItsFlowWithoutAFault(t *testing.T) {
	target, _ := echoTCP(t)
	host, _, _ := judged(t, allowed, target)
	guest := attach(t, host, guestA)
	link := host.linkOf(guestA)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := guest.dialTCP(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("pingping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		host.mu.Lock()
		left, fault := len(link.flows), link.flowErr
		host.mu.Unlock()
		if left == 0 {
			if fault != nil {
				t.Fatalf("the guest's reset was kept as the link's fault: %v", fault)
			}

			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reset flow still tracks %d sides", left)
		}
		time.Sleep(20 * time.Millisecond)
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

// A flow the listener takes spends the guest's share like a forwarded one: past it the listener drops it as limit, and a close gives the place back (SHARD-350).
func TestAListenerFlowPastTheGuestsShareIsDroppedAsLimit(t *testing.T) {
	target, _ := echoTCP(t)
	host, drops, accepted := redirecting(t, func(netip.Addr) bool { return true }, func() Verdict { return allowed }, target)
	guest := attach(t, host, guestA)
	link := host.linkOf(guestA)
	https := netip.AddrPortFrom(remote.Addr(), 443)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	host.mu.Lock()
	link.active = maxLinkFlows
	host.mu.Unlock()
	if refused, err := guest.dialTCP(ctx, https); err == nil {
		defer refused.Close()
	}
	select {
	case got := <-drops:
		got.Time = time.Time{}
		want := Drop{Guest: guestA, Destination: remote.Addr(), Protocol: "tcp", Port: 443, Rule: RuleLimit}
		if got != want {
			t.Errorf("reported %+v, want %+v", got, want)
		}
	case server := <-accepted:
		server.Close()
		t.Fatal("the listener handed on a flow past the guest's share")
	case <-time.After(5 * time.Second):
		t.Fatal("no drop reported")
	}

	host.mu.Lock()
	link.active = 0
	host.mu.Unlock()
	client, err := guest.dialTCP(ctx, https)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never took a flow under the share")
	}
	if during := counted(host, link); during != 2 {
		t.Errorf("an accepted flow counts %d on the link and the stack together, want 2", during)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if after := counted(host, link); after != 0 {
		t.Errorf("a closed flow still counts %d", after)
	}
}

func counted(host *Stack, link *Link) int {
	host.mu.Lock()
	defer host.mu.Unlock()

	return link.active + host.active
}

// redirecting builds a host stack that redirects 443 onto a listener for the guests redirected names, and dials every allowed flow to target.
func redirecting(t *testing.T, redirected func(netip.Addr) bool, verdict func() Verdict, target string) (*Stack, chan Drop, chan net.Conn) {
	t.Helper()

	drops := make(chan Drop, 16)
	s, err := New(Config{
		Address:    gateway,
		Redirects:  map[uint16]uint16{443: 30443},
		Redirected: redirected,
		Drops:      func(d Drop) { drops <- d },
		Judge:      func(Flow) Verdict { return verdict() },
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close the host stack: %v", err)
		}
	})
	ln, err := s.ListenTCP(30443)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	return s, drops, accepted
}

// A guest the stack does not redirect speaks TLS to the destination itself and sees its own certificate, while a redirected one lands on the listener (SHARD-294).
func TestAGuestTheStackDoesNotRedirectReachesTheRedirectedPortDirectly(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	host, drops, accepted := redirecting(t, func(g netip.Addr) bool { return g == guestB }, func() Verdict { return allowed }, upstream.Listener.Addr().String())
	plain := attach(t, host, guestA)
	fronted := attach(t, host, guestB)
	https := netip.AddrPortFrom(remote.Addr(), 443)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := plain.dialTCP(ctx, https)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	client := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12})
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatalf("the guest's TLS to the destination: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(client); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("the destination answered %d", resp.StatusCode)
	}
	select {
	case server := <-accepted:
		server.Close()
		t.Fatal("the listener took the flow of a guest the stack does not redirect")
	default:
	}

	redirected, err := fronted.dialTCP(ctx, https)
	if err != nil {
		t.Fatal(err)
	}
	defer redirected.Close()
	select {
	case server := <-accepted:
		defer server.Close()
		if got := server.RemoteAddr().(*net.TCPAddr).IP.String(); got != guestB.String() {
			t.Errorf("the listener saw source %s, want %s", got, guestB)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never took the redirected guest's flow")
	}
	select {
	case got := <-drops:
		t.Fatalf("a flow was reported as a drop: %+v", got)
	default:
	}
}

// A SYN resent after its guest became redirected keeps the tuple's first NAT answer, and the forwarder refuses it rather than dial past the listener.
func TestAFlowThatMissedTheRedirectIsDropped(t *testing.T) {
	target, taken := echoTCP(t)
	var redirected, allow atomic.Bool
	verdict := func() Verdict {
		if allow.Load() {
			return allowed
		}

		return denied
	}
	host, drops, accepted := redirecting(t, func(netip.Addr) bool { return redirected.Load() }, verdict, target)
	guest := attach(t, host, guestA)

	dialed := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		conn, err := guest.dialTCP(ctx, netip.AddrPortFrom(remote.Addr(), 443))
		if err == nil {
			conn.Close()
		}
		dialed <- err
	}()
	rules := func() string {
		select {
		case got := <-drops:
			return got.Rule
		case <-time.After(5 * time.Second):
			t.Fatal("no drop reported")
		}

		return ""
	}
	if got := rules(); got != denied.Rule {
		t.Fatalf("the first SYN was dropped as %q, want %q", got, denied.Rule)
	}
	redirected.Store(true)
	allow.Store(true)
	// A slow runner can resend before the flip, and that SYN is still denied.
	for got := rules(); got != RuleRedirect; got = rules() {
		if got != denied.Rule {
			t.Fatalf("the resent SYN was dropped as %q, want %q", got, RuleRedirect)
		}
	}
	cancel()
	if err := <-dialed; err == nil {
		t.Error("a flow that missed the redirect connected")
	}
	if n := taken.Load(); n != 0 {
		t.Errorf("the host dialed %d flows past the listener", n)
	}
	select {
	case server := <-accepted:
		server.Close()
		t.Error("the listener took a flow whose first SYN missed the redirect")
	default:
	}
}

// A tuple conntrack redirected keeps that answer after its guest stops being redirected, and the listener closes the flow rather than proxy it (SHARD-294).
func TestAStaleRedirectOfAGuestNoLongerRedirectedIsDropped(t *testing.T) {
	target, _ := echoTCP(t)
	var fronted atomic.Bool
	fronted.Store(true)
	host, drops, accepted := redirecting(t, func(netip.Addr) bool { return fronted.Load() }, func() Verdict { return allowed }, target)
	https := netip.AddrPortFrom(remote.Addr(), 443)
	const port = 40443

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first, err := attach(t, host, guestB).dialTCPFrom(ctx, port, https)
	if err != nil {
		t.Fatal(err)
	}
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never took the redirected guest's flow")
	}
	// The guest closes first, so the listener's side ends closed and not in a TIME-WAIT that would swallow the next SYN.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(server); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for host.connected() != 0 {
		if ctx.Err() != nil {
			t.Fatal("the redirected flow never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The guest restarts without a redirect and takes its old source port again.
	if err := host.linkOf(guestB).Close(); err != nil {
		t.Fatal(err)
	}
	fronted.Store(false)
	restarted := attach(t, host, guestB)
	if second, err := restarted.dialTCPFrom(ctx, port, https); err == nil {
		defer second.Close()
	}
	select {
	case got := <-drops:
		got.Time = time.Time{}
		want := Drop{Guest: guestB, Destination: remote.Addr(), Protocol: "tcp", Port: 443, Rule: RuleRedirect}
		if got != want {
			t.Errorf("reported %+v, want %+v", got, want)
		}
	case server := <-accepted:
		server.Close()
		t.Fatal("the listener handed on a stale redirect of a guest no longer redirected")
	case <-time.After(5 * time.Second):
		t.Fatal("no drop reported")
	}

	// A dial of the listener itself passes conntrack as a no-op NAT, and stays the guest's to make.
	direct, err := restarted.dialTCP(ctx, netip.AddrPortFrom(gateway, 30443))
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	select {
	case server := <-accepted:
		server.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never took a flow the guest dialed to it")
	}
}
