package network

import (
	"net/netip"
	"path/filepath"
	"testing"
)

func newTestPool(t *testing.T, subnet string) *pool {
	t.Helper()

	prefix := netip.MustParsePrefix(subnet)

	p, err := newPool(filepath.Join(t.TempDir(), leasesDir), prefix, prefix.Addr().Next().Next())
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}

	return p
}

func allocate(t *testing.T, p *pool, id string) netip.Addr {
	t.Helper()

	address, _, err := p.allocate(id)
	if err != nil {
		t.Fatalf("allocate %s: %v", id, err)
	}

	return address
}

// The gateway holds the first address, so a sandbox starts at the second.
func TestTheFirstSandboxGetsTheAddressAfterTheGateway(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")

	if got := allocate(t, p, "amber-otter"); got != netip.MustParseAddr("10.87.0.2") {
		t.Errorf("got %s, want 10.87.0.2", got)
	}
}

func TestTwoSandboxesNeverShareAnAddress(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")

	first, second := allocate(t, p, "amber-otter"), allocate(t, p, "bitter-fox")
	if first == second {
		t.Fatalf("both sandboxes got %s", first)
	}
}

// A sandbox that stops and starts again must come back on the address its record already names, and
// the caller must be told the lease was already there: that is what says the host side is built.
func TestASecondAllocationForTheSameSandboxReturnsItsAddress(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")

	first, held, err := p.allocate("amber-otter")
	if err != nil || held {
		t.Fatalf("the first allocate reported held=%v err=%v, want a fresh claim", held, err)
	}

	second, held, err := p.allocate("amber-otter")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if !held {
		t.Error("the second allocate claimed a new address instead of reporting the lease")
	}
	if first != second {
		t.Errorf("got %s then %s, want the same address twice", first, second)
	}
}

func TestAReleasedAddressIsHandedOutAgain(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")

	first := allocate(t, p, "amber-otter")
	if err := p.release("amber-otter"); err != nil {
		t.Fatalf("release: %v", err)
	}

	if got := allocate(t, p, "bitter-fox"); got != first {
		t.Errorf("got %s, want the released %s", got, first)
	}
}

func TestReleasingASandboxWithNoLeaseIsNotAFailure(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")

	if err := p.release("amber-otter"); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// A /29 holds the network address, the gateway, five sandboxes and the broadcast address.
func TestThePoolRefusesToHandOutTheBroadcastAddress(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/29")

	for i := range 5 {
		address := allocate(t, p, string(rune('a'+i)))
		if address == netip.MustParseAddr("10.87.0.7") {
			t.Fatal("the pool handed out the broadcast address")
		}
	}

	if _, _, err := p.allocate("one-too-many"); err == nil {
		t.Fatal("the pool handed out an address it does not have")
	}
}

func TestFindReportsTheSandboxThatHoldsAnAddress(t *testing.T) {
	p := newTestPool(t, "10.87.0.0/16")
	want := allocate(t, p, "amber-otter")

	got, found, err := p.find("amber-otter")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || got != want {
		t.Errorf("got %s found=%v, want %s found=true", got, found, want)
	}
}
