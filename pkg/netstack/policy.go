package netstack

import (
	"net/netip"
	"strconv"
	"time"

	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Drop is one frame the stack refused: the guest reached for a port no listener serves, or for anything off the stack.
type Drop struct {
	Time        time.Time
	Guest       netip.Addr
	Destination netip.Addr
	// Protocol is tcp, udp, icmp or the IP protocol number as text; Port is the transport destination, or 0 without one.
	Protocol string
	Port     int
}

// The host chains log two drops a second with a burst of ten, and a link reports at the same bound.
const (
	dropRate  = 2
	dropBurst = 10
)

// judge says whether the stack takes the frame: from the guest, and ARP, a redirected port or a served port on the address, nothing else.
func (l *Link) judge(frame []byte) (Drop, bool) {
	s := l.stack
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open {
		return Drop{}, true
	}
	// The link is one guest's wire, so a frame it did not send is still charged to it.
	if !l.fromGuest(frame) {
		return l.refused(frame), false
	}
	eth := header.Ethernet(frame)
	if eth.Type() != header.IPv4ProtocolNumber {
		return Drop{}, true
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	proto := ip.TransportProtocol()
	port := transportPort(ip)
	if proto == header.TCPProtocolNumber {
		if _, redirected := s.cfg.Redirects[port]; redirected {
			return Drop{}, true
		}
	}
	if ip.DestinationAddress() == tcpip.AddrFrom4(s.cfg.Address.As4()) && s.serves(proto, port) {
		return Drop{}, true
	}

	return Drop{Time: time.Now().UTC(), Guest: l.guest, Destination: netip.AddrFrom4(ip.DestinationAddress().As4()), Protocol: protocolName(proto), Port: int(port)}, false
}

// refused names a frame the link took from something other than its guest: IPv6, a forged source, or nothing the stack speaks.
func (l *Link) refused(frame []byte) Drop {
	d := Drop{Time: time.Now().UTC(), Guest: l.guest, Protocol: "runt"}
	if len(frame) < header.EthernetMinimumSize {
		return d
	}
	eth := header.Ethernet(frame)
	body := frame[header.EthernetMinimumSize:]
	d.Protocol = "ethertype " + strconv.Itoa(int(eth.Type()))
	switch eth.Type() {
	case header.IPv6ProtocolNumber:
		d.Protocol = "ipv6"
		if len(body) >= header.IPv6MinimumSize {
			d.Destination = netip.AddrFrom16(header.IPv6(body).DestinationAddress().As16())
		}
	case header.IPv4ProtocolNumber, header.ARPProtocolNumber:
		d.Protocol = "forged"
		if eth.Type() == header.IPv4ProtocolNumber && len(body) >= header.IPv4MinimumSize {
			ip := header.IPv4(body)
			d.Destination = netip.AddrFrom4(ip.DestinationAddress().As4())
			d.Protocol = "forged " + protocolName(ip.TransportProtocol())
			d.Port = int(transportPort(ip))
		}
	}

	return d
}

// serves reports a listener on the port; the stack answers a closed port with a reset, which a guest must not learn from.
func (s *Stack) serves(proto tcpip.TransportProtocolNumber, port uint16) bool {
	switch proto {
	case header.TCPProtocolNumber:
		return s.tcpPorts[port]
	case header.UDPProtocolNumber:
		return s.udpPorts[port]
	}

	return false
}

// transportPort is the destination port of a first fragment that carries one, and 0 for anything else.
func transportPort(ip header.IPv4) uint16 {
	if ip.FragmentOffset() != 0 || len(ip) < int(ip.HeaderLength()) {
		return 0
	}
	body := ip[ip.HeaderLength():]
	switch ip.TransportProtocol() {
	case header.TCPProtocolNumber:
		if len(body) >= header.TCPMinimumSize {
			return header.TCP(body).DestinationPort()
		}
	case header.UDPProtocolNumber:
		if len(body) >= header.UDPMinimumSize {
			return header.UDP(body).DestinationPort()
		}
	}

	return 0
}

func protocolName(proto tcpip.TransportProtocolNumber) string {
	switch proto {
	case header.TCPProtocolNumber:
		return "tcp"
	case header.UDPProtocolNumber:
		return "udp"
	case header.ICMPv4ProtocolNumber:
		return "icmp"
	}

	return strconv.Itoa(int(proto))
}

// report hands a refused frame to the configured reporter, at the bound.
func (l *Link) report(drop Drop) {
	if l.stack.cfg.Drops == nil || !drop.Guest.IsValid() || !l.limiter.Allow() {
		return
	}
	l.stack.cfg.Drops(drop)
}

func newLimiter() *rate.Limiter { return rate.NewLimiter(dropRate, dropBurst) }

// natTable sends a guest's TCP flow to a redirected port onto the stack's own listener, wherever the guest dialed, the way the host chains dnat 80 and 443.
func natTable(redirects map[uint16]uint16) stack.Table {
	var rules []stack.Rule
	for from, to := range redirects {
		filter := stack.EmptyFilter4()
		filter.Protocol, filter.CheckProtocol = header.TCPProtocolNumber, true
		rules = append(rules, stack.Rule{
			Filter:   filter,
			Matchers: []stack.Matcher{portMatcher(from)},
			Target:   &stack.RedirectTarget{Port: to, NetworkProtocol: header.IPv4ProtocolNumber},
		})
	}
	accept := stack.Rule{Filter: stack.EmptyFilter4(), Target: &stack.AcceptTarget{NetworkProtocol: header.IPv4ProtocolNumber}}
	// One accept per remaining hook, and the prerouting chain falls through to the first of them.
	underflow := len(rules)
	rules = append(rules, accept, accept, accept, accept, stack.Rule{Filter: stack.EmptyFilter4(), Target: &stack.ErrorTarget{NetworkProtocol: header.IPv4ProtocolNumber}})

	return stack.Table{
		Rules:         rules,
		BuiltinChains: [stack.NumHooks]int{stack.Prerouting: 0, stack.Input: underflow + 1, stack.Forward: stack.HookUnset, stack.Output: underflow + 2, stack.Postrouting: underflow + 3},
		Underflows:    [stack.NumHooks]int{stack.Prerouting: underflow, stack.Input: underflow + 1, stack.Forward: stack.HookUnset, stack.Output: underflow + 2, stack.Postrouting: underflow + 3},
	}
}

// portMatcher matches a TCP segment by destination port; the stack's own port matchers live in the sentry.
type portMatcher uint16

func (m portMatcher) Match(_ stack.Hook, pkt *stack.PacketBuffer, _, _ string) (bool, bool) {
	tcp := header.TCP(pkt.TransportHeader().Slice())
	if len(tcp) < header.TCPMinimumSize {
		return false, false
	}

	return tcp.DestinationPort() == uint16(m), false
}
