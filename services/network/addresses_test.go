package network

import (
	"errors"
	"net/netip"
	"testing"
)

func TestAddressesLeaseOnePerSandboxAndKeepItAcrossReleases(t *testing.T) {
	a, err := NewAddresses(Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.200.0.0/29")})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Gateway(); got != netip.MustParseAddr("10.200.0.1") {
		t.Fatalf("gateway %s, want 10.200.0.1", got)
	}

	first, err := a.Allocate(t.Context(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParsePrefix("10.200.0.2/29")
	if first.Address != want || first.Gateway != a.Gateway() {
		t.Fatalf("first lease %+v, want %s behind %s", first, want, a.Gateway())
	}
	if len(first.Nameservers) != 1 || first.Nameservers[0] != a.Gateway() {
		t.Fatalf("nameservers %v, want the gateway alone", first.Nameservers)
	}
	if first.NetnsPath != "" || first.HostInterface != "" {
		t.Fatalf("the spec names a host side %+v, and the stack has none", first)
	}

	again, err := a.Allocate(t.Context(), "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.Address != first.Address {
		t.Fatalf("a second Allocate moved sb-1 from %s to %s", first.Address, again.Address)
	}

	second, err := a.Allocate(t.Context(), "sb-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Address == first.Address {
		t.Fatalf("two sandboxes share %s", second.Address)
	}

	if err := a.Release(t.Context(), "sb-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(t.Context(), "sb-1"); err != nil {
		t.Fatalf("a second Release: %v", err)
	}
	third, err := a.Allocate(t.Context(), "sb-3")
	if err != nil {
		t.Fatal(err)
	}
	if third.Address != first.Address {
		t.Fatalf("the released %s was not leased again, sb-3 got %s", first.Address, third.Address)
	}
}

func TestAddressesRefuseAFullSubnetAndABadName(t *testing.T) {
	// A /30 holds the gateway and one sandbox.
	a, err := NewAddresses(Config{Root: t.TempDir(), Subnet: netip.MustParsePrefix("10.200.0.0/30")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate(t.Context(), "sb-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate(t.Context(), "sb-2"); !errors.Is(err, ErrNoFreeAddress) {
		t.Fatalf("the second sandbox got %v, want ErrNoFreeAddress", err)
	}
	if _, err := a.Allocate(t.Context(), "../sb"); err == nil {
		t.Fatal("a path as an id was leased")
	}
	if err := a.Reapply(t.Context(), "sb-1"); err != nil {
		t.Fatal(err)
	}
}
