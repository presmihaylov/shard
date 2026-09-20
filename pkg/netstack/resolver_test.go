package netstack

import (
	"context"
	"io"
	"log"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/presmihaylov/shard/pkg/dns"
)

type denyAll struct{ asked chan dns.Question }

func (d denyAll) Resolve(_ context.Context, q dns.Question) (bool, error) {
	d.asked <- q

	return false, nil
}

// The resolver serves over the stack as it does over the bridge, and judges the question by the guest's address.
func TestTheResolverServesOverTheStack(t *testing.T) {
	host := hostStack(t)
	guest := attach(t, host, guestA)

	udp, err := host.ListenPacket(dns.Port)
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := host.ListenTCP(dns.Port)
	if err != nil {
		t.Fatal(err)
	}

	director := denyAll{asked: make(chan dns.Question, 1)}
	// A denied question never reaches an upstream, so an unroutable one is enough to build the server.
	upstream := netip.MustParseAddrPort("192.0.2.1:53")
	server, err := dns.New(dns.Config{Address: gateway, Upstreams: []netip.AddrPort{upstream}, Director: director, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, udp, tcp) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})

	client, err := guest.dialUDP(netip.AddrPortFrom(gateway, dns.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	question, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("api.example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(question); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	var answer dnsmessage.Message
	if err := answer.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if answer.RCode != dnsmessage.RCodeNameError {
		t.Errorf("rcode %v, want NXDOMAIN", answer.RCode)
	}
	if q := <-director.asked; q.Source != guestA || q.Name != "api.example.com" {
		t.Errorf("the director was asked %+v", q)
	}
}
