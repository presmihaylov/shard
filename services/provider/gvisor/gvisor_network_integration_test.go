//go:build integration

package gvisor_test

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// dialGrace bounds every connection this file makes, from the host and from a sandbox alike.
const dialGrace = 5 * time.Second

// A sandbox reaches the internet through the bridge, the default route and the masquerade rule. The
// address is dialled raw, so nothing here depends on the resolver.
func TestASandboxReachesTheInternet(t *testing.T) {
	h := newNetworkedHarness(t)

	code, output := h.runNetworked(t, "nc -w 5 1.1.1.1 80 < /dev/null")
	if code != 0 {
		t.Errorf("the sandbox could not reach 1.1.1.1:80, exit %d: %s", code, output)
	}
}

// gVisor's netstack resolves no name itself, so this passes only because the bundle wrote resolv.conf.
func TestASandboxResolvesANameThroughTheWrittenResolverConfig(t *testing.T) {
	h := newNetworkedHarness(t)

	code, output := h.runNetworked(t, "cat /etc/resolv.conf; nslookup example.com")
	if code != 0 {
		t.Fatalf("the sandbox could not resolve example.com, exit %d: %s", code, output)
	}
	if !strings.Contains(output, "nameserver 1.1.1.1") {
		t.Errorf("the sandbox resolver config is %q, want the nameserver shard wrote", output)
	}
}

// A sandbox with no network at all must still create and run, because most tests ask for none.
func TestASandboxWithNoNetworkHasNoResolverConfigOfItsOwn(t *testing.T) {
	h := newHarness(t)
	spec := h.start(t, "/bin/sh", "-c", "nc -w 3 1.1.1.1 80 < /dev/null")

	status, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status.Code == 0 {
		t.Error("a sandbox with --network=none reached the internet")
	}
}

// Two sandboxes share one bridge, and the isolated flag on each port is what keeps them apart. The
// host dial is the control: without it a broken listener would look like isolation working.
func TestOneSandboxCannotReachAnother(t *testing.T) {
	h := newNetworkedHarness(t)

	listener := h.newSpec(t, "/bin/sh", "-c", "while true; do echo served | nc -l -p 8080; done")
	address := listener.Network.Address.Addr().String()
	h.startSpec(t, listener)

	awaitDial(t, net.JoinHostPort(address, "8080"))

	code, output := h.runNetworked(t, fmt.Sprintf("nc -w 3 %s 8080 < /dev/null", address))
	if code == 0 {
		t.Errorf("a sandbox reached another sandbox on %s:8080: %s", address, output)
	}
}

// The bridge puts the host's own services one hop from every sandbox, and the nft input chain is
// what takes them back out of reach. SHARD-71 opens this chain for the proxy, and nothing else.
func TestASandboxCannotReachTheHost(t *testing.T) {
	h := newNetworkedHarness(t)

	gateway := h.net.Gateway().String()
	listener, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatalf("listen on the gateway address: %v", err)
	}
	defer listener.Close()

	go acceptForever(listener)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", listener.Addr(), err)
	}

	// The control: the listener is up and reachable from the host it runs on.
	awaitDial(t, listener.Addr().String())

	code, output := h.runNetworked(t, fmt.Sprintf("nc -w 3 %s %s < /dev/null", gateway, port))
	if code == 0 {
		t.Errorf("a sandbox reached the host on %s:%s: %s", gateway, port, output)
	}
}

// runNetworked runs one shell command in a fresh networked sandbox and reports its exit code and log.
func (h *harness) runNetworked(t *testing.T, command string) (int, string) {
	t.Helper()

	spec := h.start(t, "/bin/sh", "-c", command)

	status, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	return status.Code, readFile(t, path)
}

// startSpec runs a sandbox the caller already built, which a test that needs its address must do.
func (h *harness) startSpec(t *testing.T, spec models.SandboxSpec) {
	t.Helper()

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// awaitDial waits for a listener to answer, because a sandbox's entrypoint starts after Start returns.
func awaitDial(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, dialGrace)
		if err == nil {
			conn.Close()

			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("nothing answered on %s: %v", address, err)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func acceptForever(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}
