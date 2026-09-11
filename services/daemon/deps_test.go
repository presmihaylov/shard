package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The daemon supervises its tasks at once over one deps, so two of them can ask for the same layer at
// the same moment. Run this one under -race: without the lock the detector names the memoised fields.
func TestTheGettersBuildOneLayerUnderConcurrentAsks(t *testing.T) {
	d := &deps{cfg: Config{Root: t.TempDir()}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			repo, err := d.repo()
			if err != nil {
				t.Errorf("repo: %v", err)
			}

			secrets, err := d.secrets()
			if err != nil {
				t.Errorf("secrets: %v", err)
			}

			if _, err := d.egress(); err != nil {
				t.Errorf("egress: %v", err)
			}

			// One daemon holds one of each, or the proxy judges against a store the API never writes to.
			if repo != d.repoSvc || secrets != d.secretSvc {
				t.Error("a getter built a second copy of a layer the daemon already held")
			}
		})
	}
	wg.Wait()
}

// --provider picks the substrate by name. A name shard does not know fails the first ask, not a sandbox.
func TestTheProviderIsPickedByName(t *testing.T) {
	// Both runners look their binary up on PATH, and neither substrate is installed where the tests run.
	bin := t.TempDir()
	for _, binary := range []string{"runsc", "sysbox-runc"} {
		if err := os.WriteFile(filepath.Join(bin, binary), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatalf("write the fake %s: %v", binary, err)
		}
	}
	t.Setenv("PATH", bin)

	for _, name := range []string{"", "gvisor", "sysbox"} {
		d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: name}}

		provider, err := d.providerLocked()
		if err != nil {
			t.Fatalf("provider %q: %v", name, err)
		}

		want := name
		if want == "" {
			want = "gvisor"
		}
		if got := provider.Name(); got != want {
			t.Errorf("--provider %q built %s, want %s", name, got, want)
		}
	}

	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "firecracker"}}
	if _, err := d.providerLocked(); err == nil || !strings.Contains(err.Error(), `unknown provider "firecracker"`) {
		t.Errorf("an unknown provider built %v, want a refusal that names it", err)
	}
}

// SHARD-93: a Sysbox host has no runsc, and the daemon must not need one. What the runtime keeps under
// its root, and whether the guest owns its namespaces, are the provider's to say, not the daemon's.
func TestASysboxDaemonNeedsNoRunsc(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sysbox-runc"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write the fake sysbox-runc: %v", err)
	}
	t.Setenv("PATH", bin)

	d := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "sysbox"}}

	if _, err := d.substrateLocked(); err != nil {
		t.Fatalf("the substrate hook of a sysbox daemon: %v", err)
	}

	provider, err := d.providerLocked()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	owner, ok := provider.(usernsOwner)
	if !ok {
		t.Fatal("the sysbox provider does not say which user namespace its sandboxes own")
	}
	if got := owner.Userns(); got.HostID != 165536 || got.Size != 65536 {
		t.Errorf("sysbox userns is %+v, want the Sysbox CE mapping 165536+65536", got)
	}

	g := &deps{cfg: Config{Root: t.TempDir(), InitPath: "/usr/local/bin/shard-init", Provider: "gvisor"}}
	if _, err := g.substrateLocked(); err == nil || !strings.Contains(err.Error(), "runsc") {
		t.Errorf("a gvisor daemon without runsc built its substrate with %v, want a refusal naming runsc", err)
	}
}
