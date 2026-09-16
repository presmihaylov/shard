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
