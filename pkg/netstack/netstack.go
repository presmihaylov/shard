// Package netstack terminates a VM's frames in a userspace stack that answers for one address and forwards nothing, so a guest reaches its listeners, its redirected ports, and nothing else.
package netstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"

	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// MTU is what the VZ network device carries per frame, and the largest payload a link ever writes.
const MTU = 1500

// maxFrame bounds one datagram read; a frame over the MTU plus its header is a driver fault, not data.
const maxFrame = MTU + header.EthernetMinimumSize

// queueLen is how many outbound packets a link holds before the stack sees back-pressure.
const queueLen = 512

// Config is the one address the stack answers for, which every link's guest sees as its gateway.
type Config struct {
	Address netip.Addr
	// MAC is the link address every link answers ARP with; the zero value takes a fixed local one.
	MAC net.HardwareAddr
	// Redirects maps a TCP port a guest dials, wherever it dials it, to the stack's own listener that takes the flow.
	Redirects map[uint16]uint16
	// Drops receives every frame the stack refuses, on the link's own goroutine; nil keeps the refusals silent.
	Drops func(Drop)
}

// Stack is one userspace stack over any number of links, each a guest of its own.
type Stack struct {
	cfg   Config
	stack *stack.Stack

	mu     sync.Mutex
	nextID tcpip.NICID
	links  map[tcpip.NICID]*Link
	closed bool
	// tcpPorts and udpPorts are what the listeners opened, which is all a guest may reach on the address.
	tcpPorts map[uint16]bool
	udpPorts map[uint16]bool
	// open takes every frame for the address, which only a test stack playing a guest needs.
	open bool
}

// New builds a stack that holds the address, forwards nothing, and has no link yet.
func New(cfg Config) (*Stack, error) {
	if !cfg.Address.Is4() {
		return nil, fmt.Errorf("the stack address %s is not IPv4", cfg.Address)
	}
	if cfg.MAC == nil {
		cfg.MAC = net.HardwareAddr{0x02, 0x73, 0x68, 0x61, 0x72, 0x64}
	}
	if len(cfg.MAC) != 6 {
		return nil, fmt.Errorf("the mac %s is not 6 bytes", cfg.MAC)
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// A guest link drops frames under load, and without SACK one loss stalls the whole window.
	sack := tcpip.TCPSACKEnabled(true)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return nil, fmt.Errorf("enable sack: %s", err)
	}

	s.IPTables().ReplaceTable(stack.NATID, natTable(cfg.Redirects), false)

	return &Stack{cfg: cfg, stack: s, nextID: 1, links: map[tcpip.NICID]*Link{}, tcpPorts: map[uint16]bool{}, udpPorts: map[uint16]bool{}}, nil
}

// Address is the one address the stack answers for.
func (s *Stack) Address() netip.Addr { return s.cfg.Address }

// Link is one guest's frames: the datagram socket the VM writes Ethernet frames to, as a NIC of the stack.
type Link struct {
	stack   *Stack
	id      tcpip.NICID
	guest   netip.Addr
	frames  io.ReadWriteCloser
	ep      *channel.Endpoint
	cancel  context.CancelFunc
	done    chan struct{}
	pumpErr error
	limiter *rate.Limiter
}

// Attach puts a guest on the stack: one NIC over frames, one datagram per Ethernet frame, the address on it, and a route to guest alone.
func (s *Stack) Attach(frames io.ReadWriteCloser, guest netip.Addr) (*Link, error) {
	if !guest.Is4() {
		return nil, fmt.Errorf("the guest address %s is not IPv4", guest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("the stack is closed")
	}
	for _, l := range s.links {
		if l.guest == guest {
			return nil, fmt.Errorf("a link already carries %s", guest)
		}
	}

	id := s.nextID
	s.nextID++

	ep := channel.New(queueLen, MTU, tcpip.LinkAddress(s.cfg.MAC))
	if err := s.stack.CreateNIC(id, ethernet.New(ep)); err != nil {
		return nil, fmt.Errorf("create nic %d: %s", id, err)
	}
	// The same address on every NIC: the guest on each sees the same gateway, and a NIC answers ARP for its own.
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4(s.cfg.Address.As4()).WithPrefix()}
	if err := s.stack.AddProtocolAddress(id, addr, stack.AddressProperties{}); err != nil {
		return nil, errors.Join(fmt.Errorf("add %s to nic %d: %s", s.cfg.Address, id, err), s.remove(id))
	}
	// A route to the guest alone: a reply finds its NIC, and any other destination has no route and drops.
	s.stack.AddRoute(tcpip.Route{Destination: hostSubnet(guest), NIC: id})

	ctx, cancel := context.WithCancel(context.Background())
	l := &Link{stack: s, id: id, guest: guest, frames: frames, ep: ep, cancel: cancel, done: make(chan struct{}), limiter: newLimiter()}
	s.links[id] = l
	go l.pump(ctx)

	return l, nil
}

// Guest is the address the link's guest was given.
func (l *Link) Guest() netip.Addr { return l.guest }

// Close takes the guest off the stack and closes its frames; a pump that failed reports why.
func (l *Link) Close() error {
	l.cancel()
	closeErr := l.frames.Close()
	<-l.done

	l.stack.mu.Lock()
	defer l.stack.mu.Unlock()
	delete(l.stack.links, l.id)

	return errors.Join(l.stack.remove(l.id), closeErr, l.pumpErr)
}

func (s *Stack) remove(id tcpip.NICID) error {
	s.stack.RemoveRoutes(func(r tcpip.Route) bool { return r.NIC == id })
	if err := s.stack.RemoveNIC(id); err != nil {
		return fmt.Errorf("remove nic %d: %s", id, err)
	}

	return nil
}

// pump moves frames both ways until the context ends or the frames do.
func (l *Link) pump(ctx context.Context) {
	defer close(l.done)

	inbound := make(chan error, 1)
	go func() { inbound <- l.receive() }()

	outbound := l.send(ctx)
	// The datagram read has no deadline; closing the frames is what ends it, and Close does that first.
	l.pumpErr = errors.Join(outbound, <-inbound)
}

func (l *Link) receive() error {
	buf := make([]byte, maxFrame)
	for {
		n, err := l.frames.Read(buf)
		if quiet(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read a frame from the guest: %w", err)
		}
		if drop, taken := l.judge(buf[:n]); !taken {
			l.report(drop)

			continue
		}
		// The stack owns the packet's bytes, so each frame is copied out of the read buffer.
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
		l.ep.InjectInbound(0, pkt)
		pkt.DecRef()
	}
}

// fromGuest reports a frame whose network source is the link's guest; anything else is a forged sibling, or a protocol the stack has no policy for, and drops.
func (l *Link) fromGuest(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize {
		return false
	}
	eth := header.Ethernet(frame)
	body := frame[header.EthernetMinimumSize:]
	guest := tcpip.AddrFrom4(l.guest.As4())
	switch eth.Type() {
	case header.IPv4ProtocolNumber:
		return len(body) >= header.IPv4MinimumSize && header.IPv4(body).SourceAddress() == guest
	case header.ARPProtocolNumber:
		return len(body) >= header.ARPSize && tcpip.AddrFromSlice(header.ARP(body).ProtocolAddressSender()) == guest
	}

	return false
}

func (l *Link) send(ctx context.Context) error {
	for {
		pkt := l.ep.ReadContext(ctx)
		if pkt == nil {
			return nil
		}
		// The view is the caller's to release; the packet's own reference does not cover it.
		view := pkt.ToView()
		_, err := l.frames.Write(view.AsSlice())
		view.Release()
		pkt.DecRef()
		if quiet(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("write a frame to the guest: %w", err)
		}
	}
}

// quiet reports the ends a closed link produces on either side, Linux answers a dead datagram peer with ECONNREFUSED, which are how a pump stops and not a fault.
func quiet(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.EDESTADDRREQ)
}

// ListenTCP opens a listener on the stack address, which every link's guest can reach.
func (s *Stack) ListenTCP(port uint16) (net.Listener, error) {
	ln, err := gonet.ListenTCP(s.stack, s.full(port), ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("listen on %s:%d: %w", s.cfg.Address, port, err)
	}
	s.mu.Lock()
	s.tcpPorts[port] = true
	s.mu.Unlock()

	return ln, nil
}

// ListenPacket opens a UDP socket on the stack address; a reply goes out the link that carries its guest.
func (s *Stack) ListenPacket(port uint16) (net.PacketConn, error) {
	local := s.full(port)
	conn, err := gonet.DialUDP(s.stack, &local, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("listen on udp %s:%d: %w", s.cfg.Address, port, err)
	}
	s.mu.Lock()
	s.udpPorts[port] = true
	s.mu.Unlock()

	return conn, nil
}

// Close ends every link and the stack; the listeners on it end with it.
func (s *Stack) Close() error {
	s.mu.Lock()
	links := make([]*Link, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.closed = true
	s.mu.Unlock()

	var err error
	for _, l := range links {
		err = errors.Join(err, l.Close())
	}
	s.stack.Close()

	return err
}

// A listener binds the port alone: the address lives on the links, and there may be none yet.
func (s *Stack) full(port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{Port: port}
}

func hostSubnet(addr netip.Addr) tcpip.Subnet {
	return tcpip.AddrFrom4(addr.As4()).WithPrefix().Subnet()
}

// File wraps a datagram socket the frames arrive on, non-blocking so a Close ends a read in flight.
func File(fd int, name string) (*os.File, error) {
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("set %s non-blocking: %w", name, err)
	}

	return os.NewFile(uintptr(fd), name), nil //nolint:gosec // fd is a descriptor this process owns
}
