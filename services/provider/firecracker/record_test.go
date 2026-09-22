package firecracker

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/presmihaylov/shard/models"
	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
)

// The record is tested on its own: a spec with a network through the fake would readdress the real shard-init it runs, which on a root test host is the host's eth0.
func TestARecordCarriesTheTapAndTheLeaseTheGuestIsAddressedWith(t *testing.T) {
	spec := models.SandboxSpec{
		ID: "sb-1", Name: "web", RootFS: t.TempDir(), Entrypoint: []string{"/bin/true"},
		Network: models.NetworkSpec{
			Address:       netip.MustParsePrefix("10.87.0.2/16"),
			Gateway:       netip.MustParseAddr("10.87.0.1"),
			HostInterface: "shardv2",
			Nameservers:   []netip.Addr{netip.MustParseAddr("10.87.0.1")},
		},
	}

	r, err := recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := record{Tap: "shardv2", Address: "10.87.0.2/16", Gateway: "10.87.0.1", Nameservers: []string{"10.87.0.1"}, Hostname: "web"}
	got := record{Tap: r.Tap, Address: r.Address, Gateway: r.Gateway, Nameservers: r.Nameservers, Hostname: r.Hostname}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded network %+v, want %+v", got, want)
	}

	spec.Name = ""
	r, err = recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "sb-1" {
		t.Errorf("an unnamed sandbox recorded hostname %q, want its id", r.Hostname)
	}

	spec.Network = models.NetworkSpec{}
	r, err = recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tap != "" || r.Address != "" || r.Hostname != "" || r.Nameservers != nil {
		t.Errorf("a sandbox without a network recorded %+v, want no tap and no resolver files", r)
	}
}

// The device the vmm attaches is the tap by name, with a MAC the lease fixes, so the bridge learns one address per sandbox.
func TestTheDeviceIsTheTapWithAMACDerivedFromTheLease(t *testing.T) {
	device, err := record{Tap: "shardv2", Address: "10.87.0.2/16"}.device()
	if err != nil {
		t.Fatal(err)
	}
	if want := (fcapi.Network{Tap: "shardv2", MAC: "02:fc:0a:57:00:02"}); device != want {
		t.Errorf("device = %+v, want %+v", device, want)
	}

	device, err = record{}.device()
	if err != nil {
		t.Fatal(err)
	}
	if device != (fcapi.Network{}) {
		t.Errorf("a record without a tap attached %+v", device)
	}

	if _, err := (record{Tap: "shardv2", Address: "not-a-prefix"}).device(); err == nil {
		t.Error("a tap over an address that does not parse made a device")
	}
}
