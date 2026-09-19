package vzvm

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// The guest writes its own resolver files, so the record carries what the network leased and the name the sandbox answers to.
func TestARecordCarriesTheResolverFilesTheGuestWrites(t *testing.T) {
	spec := models.SandboxSpec{
		ID: "sb-1", Name: "web", RootFS: t.TempDir(), Entrypoint: []string{"/bin/true"},
		Network: models.NetworkSpec{
			Address:     netip.MustParsePrefix("10.87.0.2/16"),
			Gateway:     netip.MustParseAddr("10.87.0.1"),
			Nameservers: []netip.Addr{netip.MustParseAddr("10.87.0.1")},
		},
	}

	r, err := recordOf(spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hostname != "web" || !reflect.DeepEqual(r.Nameservers, []string{"10.87.0.1"}) {
		t.Errorf("recorded hostname %q and nameservers %v, want web and the gateway", r.Hostname, r.Nameservers)
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
	if r.Hostname != "" || r.Nameservers != nil {
		t.Errorf("a sandbox without a network recorded %+v, want no resolver files", r)
	}
}
