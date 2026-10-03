package serve

import (
	"net"
	"net/netip"
	"testing"
)

// The front speaks plain HTTP, so by default only a proxy on the same host can reach it.
func TestTheDefaultAddressIsLoopback(t *testing.T) {
	addr, err := netip.ParseAddrPort(DefaultListen)
	if err != nil {
		t.Fatalf("parse %q: %v", DefaultListen, err)
	}

	if !addr.Addr().IsLoopback() || addr.Port() != 2376 {
		t.Errorf("DefaultListen is %s, want a loopback address on port 2376", addr)
	}
}

func TestLoopbackNamesOnlyThisHost(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want bool
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2376}, true},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 2376}, true},
		{&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 2376}, false},
		{&net.TCPAddr{IP: net.ParseIP("::"), Port: 2376}, false},
		{&net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 2376}, false},
		{&net.UnixAddr{Name: "/run/shard.sock", Net: "unix"}, false},
	}

	for _, c := range cases {
		if got := loopback(c.addr); got != c.want {
			t.Errorf("loopback(%s) = %t, want %t", c.addr, got, c.want)
		}
	}
}
