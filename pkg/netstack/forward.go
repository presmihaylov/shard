package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Flow is one connection a guest opens off the stack address, which the judge rules on before the stack answers the guest.
type Flow struct {
	Guest       netip.Addr
	Protocol    string
	Destination netip.AddrPort
}

// Verdict is the judge's answer for a flow, and the rule it came from, which the drop record carries when the answer is no.
type Verdict struct {
	Allow bool
	Rule  string
}

const (
	// dialTimeout bounds the host side of a flow; a guest that dials a black hole gets its reset well inside its own SYN retries.
	dialTimeout = 10 * time.Second
	// udpIdle ends a UDP flow neither side has used, the way a conntrack entry ages out.
	udpIdle = 30 * time.Second
	// maxInFlight is how many TCP handshakes the forwarder holds open per stack.
	maxInFlight = 1024
	// A flow holds a host socket and two goroutines for its life, so one guest gets a share and the stack a ceiling under the daemon's descriptors.
	maxLinkFlows  = 1024
	maxStackFlows = 4096
	datagramMax   = 64 * 1024
)

// RuleLimit names the drop of a flow the judge allowed but the link or the stack has no room for.
const RuleLimit = "limit"

// forward installs the transport handlers that take a flow to any address no listener serves, which the judge already allowed through.
func (s *Stack) forward() {
	s.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s.stack, 0, maxInFlight, s.forwardTCP).HandlePacket)
	s.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s.stack, s.forwardUDP).HandlePacket)
}

// forwardable says the judge gets the frame: TCP or UDP to a port no listener serves, so the flow lands in a forwarder and not on a listener.
func (s *Stack) forwardable(proto tcpip.TransportProtocolNumber, port uint16) bool {
	return s.cfg.Judge != nil && (proto == tcp.ProtocolNumber || proto == udp.ProtocolNumber) && port != 0 && !s.serves(proto, port)
}

func (s *Stack) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if s.cfg.Dial != nil {
		return s.cfg.Dial(ctx, network, address)
	}

	return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, network, address)
}

func (s *Stack) linkOf(guest netip.Addr) *Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if l.guest == guest {
			return l
		}
	}

	return nil
}

func flowOf(id stack.TransportEndpointID, protocol string) Flow {
	return Flow{Guest: netip.AddrFrom4(id.RemoteAddress.As4()), Protocol: protocol, Destination: netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)}
}

func (f Flow) drop(rule string) Drop {
	return Drop{Time: time.Now().UTC(), Guest: f.Guest, Destination: f.Destination.Addr(), Protocol: f.Protocol, Port: int(f.Destination.Port()), Rule: rule}
}

// forwardTCP rules on a SYN the guest sent off the address: a no is silent, like the host chains, and a yes dials the host before the guest sees a SYN-ACK.
func (s *Stack) forwardTCP(r *tcp.ForwarderRequest) {
	flow := flowOf(r.ID(), "tcp")
	l := s.linkOf(flow.Guest)
	if l == nil {
		r.Complete(false)

		return
	}
	v := s.cfg.Judge(flow)
	if !v.Allow {
		l.report(flow.drop(v.Rule))
		r.Complete(false)

		return
	}
	if !l.admit() {
		l.report(flow.drop(RuleLimit))
		r.Complete(false)

		return
	}
	go l.spliceTCP(r, flow)
}

func (l *Link) spliceTCP(r *tcp.ForwarderRequest, flow Flow) {
	defer l.release()
	host, err := l.stack.dial(l.ctx, "tcp", flow.Destination.String())
	if err != nil {
		// A destination the host cannot reach resets the guest, the same answer a refused connect gives on Linux.
		r.Complete(true)

		return
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		l.fail(errors.Join(fmt.Errorf("accept the guest side of %s: %s", flow.Destination, tcpErr), host.Close()))
		r.Complete(true)

		return
	}
	r.Complete(false)
	guest := gonet.NewTCPConn(&wq, ep)
	if !l.track(guest, host) {
		return
	}
	defer l.untrack(guest, host)

	var wg sync.WaitGroup
	copyErrs := make([]error, 2)
	half := func(i int, dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		copyErrs[i] = quietOr(err)
		// A half-close carries one side's end to the other, so a request whose writer is done still gets its answer.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			copyErrs[i] = errors.Join(copyErrs[i], quietOr(cw.CloseWrite()))
		}
	}
	wg.Add(2)
	go half(0, host, guest)
	go half(1, guest, host)
	wg.Wait()
	l.fail(errors.Join(copyErrs[0], copyErrs[1], closeQuietly(guest), closeQuietly(host)))
}

// forwardUDP rules on the first datagram of a flow; it takes the packet either way, so a refused one gets no ICMP the guest could learn from.
func (s *Stack) forwardUDP(r *udp.ForwarderRequest) bool {
	flow := flowOf(r.ID(), "udp")
	l := s.linkOf(flow.Guest)
	if l == nil {
		return false
	}
	v := s.cfg.Judge(flow)
	if !v.Allow {
		l.report(flow.drop(v.Rule))

		return true
	}
	if !l.admit() {
		l.report(flow.drop(RuleLimit))

		return true
	}
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		l.release()
		l.fail(fmt.Errorf("accept the guest side of udp %s: %s", flow.Destination, tcpErr))

		return true
	}
	go l.spliceUDP(gonet.NewUDPConn(&wq, ep), flow)

	return true
}

func (l *Link) spliceUDP(guest net.Conn, flow Flow) {
	defer l.release()
	host, err := l.stack.dial(l.ctx, "udp", flow.Destination.String())
	if err != nil {
		l.fail(errors.Join(fmt.Errorf("dial udp %s: %w", flow.Destination, err), guest.Close()))

		return
	}
	if !l.track(guest, host) {
		return
	}
	defer l.untrack(guest, host)

	var wg sync.WaitGroup
	copyErrs := make([]error, 2)
	half := func(i int, dst, src net.Conn) {
		defer wg.Done()
		copyErrs[i] = datagrams(dst, src)
		// The first side to end takes the other with it, which is what turns one idle deadline into the end of the flow.
		copyErrs[i] = errors.Join(copyErrs[i], closeQuietly(src), closeQuietly(dst))
	}
	wg.Add(2)
	go half(0, host, guest)
	go half(1, guest, host)
	wg.Wait()
	l.fail(errors.Join(copyErrs[0], copyErrs[1]))
}

// datagrams moves one datagram at a time until the source goes idle or either side ends.
func datagrams(dst, src net.Conn) error {
	buf := make([]byte, datagramMax)
	for {
		if err := src.SetReadDeadline(time.Now().Add(udpIdle)); err != nil {
			return quietOr(err)
		}
		n, err := src.Read(buf)
		if err != nil {
			return quietOr(err)
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return quietOr(err)
		}
	}
}

// quietOr keeps the errors a closed or idle flow does not produce.
func quietOr(err error) error {
	if quiet(err) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return nil
	}

	return err
}

// closeQuietly is the second Close of a flow's side, which the first half already ended.
func closeQuietly(c io.Closer) error {
	return quietOr(c.Close())
}

// admit takes one of the link's flows and one of the stack's for a flow's whole life; a link with none left, or a stack with none, refuses it.
func (l *Link) admit() bool {
	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	if l.closing || l.active >= maxLinkFlows || l.stack.active >= maxStackFlows {
		return false
	}
	l.active++
	l.stack.active++

	return true
}

// release gives back what admit took, once the splice is over.
func (l *Link) release() {
	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	l.active--
	l.stack.active--
}

// track keeps a flow's two sides for the link's close; a link already closing takes none and ends the sides here.
func (l *Link) track(conns ...io.Closer) bool {
	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	if l.closing {
		for _, c := range conns {
			l.flowErr = errors.Join(l.flowErr, closeQuietly(c))
		}

		return false
	}
	for _, c := range conns {
		l.flows[c] = struct{}{}
	}
	l.splices.Add(1)

	return true
}

func (l *Link) untrack(conns ...io.Closer) {
	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	for _, c := range conns {
		delete(l.flows, c)
	}
	l.splices.Done()
}

// fail keeps the first fault of a flow while the link lives; what ends when the link does is the close, not a fault.
func (l *Link) fail(err error) {
	if err == nil || l.ctx.Err() != nil {
		return
	}
	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	if l.flowErr == nil {
		l.flowErr = err
	}
}

// closeFlows ends every flow the link still carries and waits for their splices, so nothing of the link runs on after its close.
func (l *Link) closeFlows() error {
	l.stack.mu.Lock()
	l.closing = true
	conns := make([]io.Closer, 0, len(l.flows))
	for c := range l.flows {
		conns = append(conns, c)
	}
	l.stack.mu.Unlock()

	var err error
	for _, c := range conns {
		err = errors.Join(err, closeQuietly(c))
	}
	l.splices.Wait()

	return err
}
