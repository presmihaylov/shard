package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/network"
)

const (
	proxyDir      = "proxy"
	caDir         = "ca"
	proxyFilePerm = 0o644
	proxyLockPerm = 0o600
	// proxyStartWait is how long a verb gives the proxy it started to listen before it refuses the sandbox.
	proxyStartWait = 5 * time.Second
	proxyPoll      = 100 * time.Millisecond
)

// egressProxy stands in front of every fronted sandbox. It is one process per root, started by the
// first verb that needs it, and the lock it holds says whether it runs.
type egressProxy interface {
	// Ensure has the proxy listening on the gateway, and starts one when nothing does.
	Ensure(ctx context.Context, gateway netip.Addr) error
	// CA is the certificate a guest must trust for the proxy to speak for every host.
	CA() ([]byte, error)
}

// launcher starts shard proxy as a process of its own, detached, because a sandbox outlives the verb
// that created it and so must what fronts it.
type launcher struct {
	root  string
	ports network.ProxyPorts
}

func (l launcher) CA() ([]byte, error) {
	ca, err := proxy.LoadOrCreate(filepath.Join(l.root, caDir))
	if err != nil {
		return nil, err
	}

	return ca.PEM(), nil
}

func (l launcher) Ensure(ctx context.Context, gateway netip.Addr) error {
	lock, err := store.TryAcquire(filepath.Join(l.root, proxyDir, "lock"), proxyLockPerm)
	if err != nil {
		return err
	}

	// Held, so a proxy runs: it listens, or it is still coming up.
	if lock == nil {
		return awaitProxy(ctx, gateway, l.ports)
	}

	// Released so the child takes it; a second child in the window fails on the lock, and both verbs wait for the winner.
	if err := lock.Release(); err != nil {
		return err
	}

	if err := l.spawn(); err != nil {
		return err
	}

	return awaitProxy(ctx, gateway, l.ports)
}

func (l launcher) spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the shard binary: %w", err)
	}

	logPath := filepath.Join(l.root, proxyDir, "log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, proxyFilePerm) // #nosec G304
	if err != nil {
		return fmt.Errorf("open the proxy log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "--root", l.root, "proxy") // #nosec G204
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own session, so the signal that ends this verb does not end the proxy.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the proxy: %w", err)
	}

	// Nobody waits for it: init reaps it when it exits.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("detach the proxy: %w", err)
	}

	return nil
}

// awaitProxy connects to both ports, because a proxy that listens on neither is a sandbox that reaches no host.
func awaitProxy(ctx context.Context, gateway netip.Addr, ports network.ProxyPorts) error {
	ctx, cancel := context.WithTimeout(ctx, proxyStartWait)
	defer cancel()

	for _, port := range []int{ports.HTTP, ports.HTTPS} {
		if err := awaitPort(ctx, netip.AddrPortFrom(gateway, uint16(port))); err != nil { //nolint:gosec
			return err
		}
	}

	return nil
}

func awaitPort(ctx context.Context, addr netip.AddrPort) error {
	var dialer net.Dialer
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr.String())
		if err == nil {
			return conn.Close()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("the proxy is not listening on %s: %w", addr, err)
		case <-time.After(proxyPoll):
		}
	}
}

// proxy runs the egress proxy in the foreground until a stop signal. A verb starts it on its own when
// a fronted sandbox needs one, so this is for an operator who wants to watch it.
func (a App) proxy(ctx context.Context, args []string) error {
	cfg, gateway, err := network.Layout(network.Config{Root: a.Root})
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("shard proxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", gateway.String(), "the address to listen on")
	flags.IntVar(&cfg.Proxy.HTTP, "http-port", cfg.Proxy.HTTP, "the port a sandbox's 80 is turned to")
	flags.IntVar(&cfg.Proxy.HTTPS, "https-port", cfg.Proxy.HTTPS, "the port a sandbox's 443 is turned to")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse the proxy flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("proxy takes no arguments, got %v", flags.Args())
	}

	addr, err := netip.ParseAddr(*listen)
	if err != nil {
		return fmt.Errorf("parse --listen: %w", err)
	}

	return a.serveProxy(ctx, a.deps(), addr, cfg.Proxy)
}

func (a App) serveProxy(ctx context.Context, d *deps, addr netip.Addr, ports network.ProxyPorts) (err error) {
	lockPath := filepath.Join(a.Root, proxyDir, "lock")
	lock, err := store.TryAcquire(lockPath, proxyLockPerm)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("a proxy already runs for %s: it holds %s", a.Root, lockPath)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	pidPath := filepath.Join(a.Root, proxyDir, "pid")
	if err := store.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), proxyFilePerm); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", pidPath, err)
	}

	ca, err := proxy.LoadOrCreate(filepath.Join(a.Root, caDir))
	if err != nil {
		return err
	}

	director, err := d.broker()
	if err != nil {
		return err
	}

	server := proxy.New(ca, director)

	plain, err := listen(addr, ports.HTTP)
	if err != nil {
		return err
	}
	defer plain.Close()

	secure, err := listen(addr, ports.HTTPS)
	if err != nil {
		return err
	}
	defer secure.Close()

	if err := a.print(fmt.Sprintf("proxy listening on %s and %s", plain.Addr(), secure.Addr())); err != nil {
		return err
	}

	served := make(chan error, 2)
	go func() { served <- server.ServePlain(plain) }()
	go func() { served <- server.ServeTLS(secure) }()

	select {
	case <-ctx.Done():
		// The listeners close on the way out, and Serve returns ErrClosed for that, which is not a failure.
		return nil
	case err := <-served:
		return fmt.Errorf("the proxy stopped: %w", err)
	}
}

func listen(addr netip.Addr, port int) (net.Listener, error) {
	l, err := net.Listen("tcp", netip.AddrPortFrom(addr, uint16(port)).String()) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("listen for the proxy: %w", err)
	}

	return l, nil
}
