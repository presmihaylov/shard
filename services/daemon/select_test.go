package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// SHARD-46: without --provider the host picks, and a Mac picks before the probe, so it answers the same either way.
func TestTheHostWithAKVMPicksFirecracker(t *testing.T) {
	kvm := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	want, reason := "firecracker", kvm+" is present"
	if runtime.GOOS == "darwin" {
		want, reason = "vz", "Virtualization.framework"
	}
	got := selectProvider("", kvm)
	if got.Provider != want || !strings.Contains(got.Reason, reason) {
		t.Errorf("a host holding %s picks %+v, want %s for %s", kvm, got, want, reason)
	}
}

func TestTheHostWithoutAKVMPicksGvisor(t *testing.T) {
	kvm := filepath.Join(t.TempDir(), "kvm")

	want, reason := "gvisor", "no "+kvm
	if runtime.GOOS == "darwin" {
		want, reason = "vz", "Virtualization.framework"
	}
	got := selectProvider("", kvm)
	if got.Provider != want || !strings.Contains(got.Reason, reason) {
		t.Errorf("a host without %s picks %+v, want %s for %s", kvm, got, want, reason)
	}
}

// A named provider is the provider, on any host: a probe never overrides what the flag says.
func TestTheNamedProviderWinsOverTheHost(t *testing.T) {
	kvm := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(kvm, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"gvisor", "sysbox", "runc", "vz", "firecracker"} {
		got := selectProvider(name, kvm)
		if got.Provider != name || !strings.Contains(got.Reason, "--provider") {
			t.Errorf("--provider %s picks %+v, want %s named by the flag", name, got, name)
		}
	}
}

// sysbox is single-tenant and runc is a plain container, so neither is ever handed to a host by a probe.
func TestNoHostIsHandedSysboxOrRunc(t *testing.T) {
	held := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(held, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, kvm := range []string{held, filepath.Join(t.TempDir(), "gone")} {
		if got := selectProvider("", kvm).Provider; got == "sysbox" || got == "runc" {
			t.Errorf("the probe of %s picked %s, which only --provider names", kvm, got)
		}
	}
}

// The line shard info prints names the substrate and the reason, in that order.
func TestASelectionPrintsTheProviderAndTheReason(t *testing.T) {
	if got := (Selection{Provider: "firecracker", Reason: "/dev/kvm is present"}).String(); got != "firecracker: /dev/kvm is present" {
		t.Errorf("the selection printed %q", got)
	}
}
