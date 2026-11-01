package linkfwd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/splice"
	"github.com/presmihaylov/shard/services/sandbox"
)

// acceptBackoff spaces the accepts after one the host refused, as for a full fd table, so the loop does not spin.
const acceptBackoff = 100 * time.Millisecond

// Serve carries every connection l accepts, which the host rules dnat'd from a link address, until ctx ends; it then ends every flow and waits out its carriers and ssh sessions.
func (f *Forwarder) Serve(ctx context.Context, l net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	var carriers sync.WaitGroup
	err := f.accept(ctx, l, &carriers)
	cancel()
	carriers.Wait()
	f.sessions.Wait()

	return err
}

func (f *Forwarder) accept(ctx context.Context, l net.Listener, carriers *sync.WaitGroup) error {
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()

	for {
		conn, err := l.Accept()
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("the link listener closed: %w", err)
		}
		if err != nil {
			f.report(fmt.Sprintf("link: accept: %v", err))
			time.Sleep(acceptBackoff)

			continue
		}
		carriers.Go(func() { f.carry(ctx, conn) })
	}
}

// carry reaches the target for one guest connection; the guest's handshake is done already, so a refusal is a reset.
func (f *Forwarder) carry(ctx context.Context, guest net.Conn) {
	// The splice closes the target end once the guest's is gone, even where the target's half already ended.
	stop := context.AfterFunc(ctx, func() { _ = guest.Close() })
	defer stop()

	source, dst, err := f.addresses(guest)
	if err != nil {
		f.report(fmt.Sprintf("link: %v", err))
		f.reset(guest)

		return
	}

	host, err := f.Dial(ctx, source, dst)
	if err != nil {
		// A guest that retries until its target is up would fill the log with the ordinary refusals.
		if !refused(err) {
			f.report(fmt.Sprintf("link: from %s: %v", source, err))
		}
		f.reset(guest)

		return
	}

	if err := splice.Conns(guest, host); err != nil {
		f.report(fmt.Sprintf("link: from %s to %s broke: %v", source, dst, err))
	}
}

// addresses are the guest that dialed and the link address it dialed, before the dnat.
func (f *Forwarder) addresses(guest net.Conn) (netip.Addr, netip.AddrPort, error) {
	remote, err := netip.ParseAddrPort(guest.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}, netip.AddrPort{}, fmt.Errorf("read the guest address: %w", err)
	}
	dst, err := f.original(guest)
	if err != nil {
		return netip.Addr{}, netip.AddrPort{}, fmt.Errorf("from %s: %w", remote.Addr(), err)
	}

	return remote.Addr().Unmap(), dst, nil
}

// reset closes with an RST, the nearest a completed handshake comes to a refused connect.
func (f *Forwarder) reset(guest net.Conn) {
	if tcp, ok := guest.(*net.TCPConn); ok {
		if err := tcp.SetLinger(0); err != nil {
			f.report(fmt.Sprintf("link: reset a refused connection: %v", err))
		}
	}
	if err := quiet(guest.Close()); err != nil {
		f.report(fmt.Sprintf("link: close a refused connection: %v", err))
	}
}

// refused is what a guest meets when nothing is wrong with the host: no link, a stopped target, or nothing listening.
func refused(err error) bool {
	_, stopped := errors.AsType[*sandbox.StateError](err)

	return stopped || errors.Is(err, ErrNoLink) || errors.Is(err, syscall.ECONNREFUSED)
}
