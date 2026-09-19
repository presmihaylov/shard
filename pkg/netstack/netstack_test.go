package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	gateway = netip.MustParseAddr("10.200.0.1")
	guestA  = netip.MustParseAddr("10.200.0.2")
	guestB  = netip.MustParseAddr("10.200.0.3")
)

// wire is one datagram socketpair: the host end the stack pumps, and the guest end a second stack plays the VM with.
func wire(t *testing.T) (host, guest *os.File) {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	host, err = File(fds[1], "host")
	if err != nil {
		t.Fatal(err)
	}
	guest, err = File(fds[0], "guest")
	if err != nil {
		t.Fatal(err)
	}

	return host, guest
}

// attach puts a guest on the host stack and gives back the guest's own stack, routed to the host through the wire.
func attach(t *testing.T, host *Stack, addr netip.Addr) *Stack {
	t.Helper()

	hostEnd, guestEnd := wire(t)
	if _, err := host.Attach(hostEnd, addr); err != nil {
		t.Fatalf("attach %s: %v", addr, err)
	}

	guest, err := New(Config{Address: addr, MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, byte(addr.As4()[3])}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Attach(guestEnd, host.Address()); err != nil {
		t.Fatalf("attach the guest side: %v", err)
	}
	guest.defaultRoute(host.Address())
	t.Cleanup(func() {
		if err := guest.Close(); err != nil {
			t.Errorf("close the guest stack: %v", err)
		}
	})

	return guest
}

func hostStack(t *testing.T) *Stack {
	t.Helper()

	s, err := New(Config{Address: gateway})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close the host stack: %v", err)
		}
	})

	return s
}

// A guest reaches a TCP listener on the stack address, and the listener sees the guest's own address as the source.
func TestAGuestReachesATCPListenerOnTheAddress(t *testing.T) {
	host := hostStack(t)
	guest := attach(t, host, guestA)

	ln, err := host.ListenTCP(30080)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("accept: %v", err)
			close(accepted)

			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := guest.dialTCP(ctx, netip.AddrPortFrom(gateway, 30080))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()

	if got := server.RemoteAddr().(*net.TCPAddr).IP.String(); got != guestA.String() {
		t.Errorf("the listener saw source %s, want %s", got, guestA)
	}

	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := server.Read(buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if _, err := server.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(buf); err != nil || string(buf) != "world" {
		t.Fatalf("read back %q, %v", buf, err)
	}
}

// A UDP reply goes back out the link that carries the guest it answers.
func TestAGuestReachesAUDPSocketAndGetsTheReply(t *testing.T) {
	host := hostStack(t)
	guest := attach(t, host, guestA)

	pc, err := host.ListenPacket(53)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	client, err := guest.dialUDP(netip.AddrPortFrom(gateway, 53))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("question")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, from, err := pc.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "question" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	if got := from.(*net.UDPAddr).IP.String(); got != guestA.String() {
		t.Errorf("the socket saw source %s, want %s", got, guestA)
	}
	if _, err := pc.WriteTo([]byte("answer"), from); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err = client.Read(buf)
	if err != nil || string(buf[:n]) != "answer" {
		t.Fatalf("read back %q, %v", buf[:n], err)
	}
}

// Two guests share the one listener and keep their own source addresses; neither reaches the other or anything beyond.
func TestTwoGuestsShareTheAddressAndReachNothingElse(t *testing.T) {
	host := hostStack(t)
	a := attach(t, host, guestA)
	b := attach(t, host, guestB)

	ln, err := host.ListenTCP(30080)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	for _, guest := range []*Stack{a, b} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		conn, err := guest.dialTCP(ctx, netip.AddrPortFrom(gateway, 30080))
		cancel()
		if err != nil {
			t.Fatalf("guest %s: %v", guest.Address(), err)
		}
		conn.Close()
	}

	for _, remote := range []netip.AddrPort{netip.AddrPortFrom(guestB, 30080), netip.MustParseAddrPort("1.1.1.1:80")} {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		conn, err := a.dialTCP(ctx, remote)
		cancel()
		if err == nil {
			conn.Close()
			t.Fatalf("guest A reached %s", remote)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dial %s: %v, want a silent drop", remote, err)
		}
	}
}

// A closed link frees its guest address, and a stack close ends the listeners on it.
func TestACloseFreesTheGuestAndEndsTheListeners(t *testing.T) {
	host := hostStack(t)

	hostEnd, guestEnd := wire(t)
	defer guestEnd.Close()
	link, err := host.Attach(hostEnd, guestA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Attach(guestEnd, guestA); err == nil || !strings.Contains(err.Error(), "already carries") {
		t.Fatalf("a second link took the address: %v", err)
	}
	if err := link.Close(); err != nil {
		t.Fatalf("close the link: %v", err)
	}
	again, err := host.Attach(guestEnd, guestA)
	if err != nil {
		t.Fatalf("attach after the close: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}

	ln, err := host.ListenTCP(30080)
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		ended <- err
	}()
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("accept returned a connection after the close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept outlived the stack")
	}
}

// A frame whose source is not the link's guest never reaches a listener, whether it is IP or the ARP before it.
func TestAFrameFromAnotherAddressIsDropped(t *testing.T) {
	host := hostStack(t)
	conn, err := host.ListenPacket(5353)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// One wire is attached as guestA; a stack claiming guestB on its far end is the forged sibling.
	forge := func(t *testing.T, seed bool) {
		t.Helper()
		hostEnd, guestEnd := wire(t)
		link, err := host.Attach(hostEnd, guestA)
		if err != nil {
			t.Fatal(err)
		}
		defer link.Close()
		guest, err := New(Config{Address: guestB, MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 3}})
		if err != nil {
			t.Fatal(err)
		}
		defer guest.Close()
		if _, err := guest.Attach(guestEnd, gateway); err != nil {
			t.Fatal(err)
		}
		guest.defaultRoute(gateway)
		if seed {
			if err := guest.knows(gateway, host.cfg.MAC); err != nil {
				t.Fatal(err)
			}
		}
		client, err := guest.dialUDP(netip.AddrPortFrom(gateway, 5353))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.Write([]byte("forged")); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, from, err := conn.ReadFrom(buf)
		if err == nil {
			t.Fatalf("the listener got %q from %s, want nothing", buf[:n], from)
		}
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal(err)
		}
	}
	t.Run("ip", func(t *testing.T) { forge(t, true) })
	t.Run("arp", func(t *testing.T) { forge(t, false) })

	// The real guest on a fresh wire still gets through, so the drop is the source and not the port.
	guest := attach(t, host, guestA)
	client, err := guest.dialUDP(netip.AddrPortFrom(gateway, 5353))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("real")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, from, err := conn.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "real" {
		t.Fatalf("read %q from %v, %v", buf[:n], from, err)
	}
}

// A guest that dials port 80 anywhere lands on the redirected listener, and the listener still sees the guest as the source.
func TestARedirectedPortLandsOnTheListenerWhereverTheGuestDialed(t *testing.T) {
	host, err := New(Config{Address: gateway, Redirects: map[uint16]uint16{80: 30080}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest := attach(t, host, guestA)

	ln, err := host.ListenTCP(30080)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)

			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := guest.dialTCP(ctx, netip.MustParseAddrPort("93.184.216.34:80"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()
	if got := server.RemoteAddr().(*net.TCPAddr).IP.String(); got != guestA.String() {
		t.Errorf("the listener saw source %s, want %s", got, guestA)
	}
	if _, err := client.Write([]byte("GET /")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := server.Read(buf); err != nil || string(buf) != "GET /" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

// A frame the stack refuses is reported once, naming the guest, where it reached for and on which port.
func TestARefusedFrameIsReportedAsADrop(t *testing.T) {
	drops := make(chan Drop, 16)
	host, err := New(Config{Address: gateway, Drops: func(d Drop) { drops <- d }})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest := attach(t, host, guestA)
	if err := guest.knows(gateway, host.cfg.MAC); err != nil {
		t.Fatal(err)
	}

	for _, remote := range []netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:443"), netip.AddrPortFrom(gateway, 22)} {
		client, err := guest.dialUDP(remote)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Write([]byte("out")); err != nil {
			t.Fatal(err)
		}
		client.Close()

		select {
		case got := <-drops:
			want := Drop{Guest: guestA, Destination: remote.Addr(), Protocol: "udp", Port: int(remote.Port())}
			got.Time = time.Time{}
			if got != want {
				t.Errorf("reported %+v, want %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no drop reported for %s", remote)
		}
	}

	// A served port on the address is not a drop.
	conn, err := host.ListenPacket(5353)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client, err := guest.dialUDP(netip.AddrPortFrom(gateway, 5353))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("in")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if n, _, err := conn.ReadFrom(buf); err != nil || string(buf[:n]) != "in" {
		t.Fatalf("read %q, %v", buf[:n], err)
	}
	select {
	case got := <-drops:
		t.Fatalf("a served port was reported as a drop: %+v", got)
	default:
	}
}
