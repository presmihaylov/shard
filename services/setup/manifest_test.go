package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRecordOwnedMergesByPath(t *testing.T) {
	f := newFakeHost(t)
	h := f.host(nil)
	local := Local{Provider: "gvisor", StartAtBoot: true}

	if err := RecordOwned(t.Context(), h, local, Owned{Path: "/usr/sbin/nft", Kind: KindTool}, Owned{Path: "/usr/local/bin/shard", Kind: KindBinary}); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}
	h.Version = "v0.2.0"
	if err := RecordOwned(t.Context(), h, Local{Provider: "runc"}, Owned{Path: "/usr/sbin/nft", Kind: KindTool, Package: "nftables"}); err != nil {
		t.Fatalf("RecordOwned: %v", err)
	}

	m, ok, err := LoadManifest(h)
	if err != nil || !ok {
		t.Fatalf("LoadManifest = %v, %v", ok, err)
	}
	want := []Owned{{Path: "/usr/sbin/nft", Kind: KindTool, Package: "nftables"}, {Path: "/usr/local/bin/shard", Kind: KindBinary}}
	if !slices.Equal(m.Files, want) || m.Version != "v0.2.0" || m.Provider != "runc" || m.StartAtBoot {
		t.Fatalf("manifest = %+v, want %v at v0.2.0 on runc without start at boot", m, want)
	}
}

func TestRecordOwnedRefusesAnUncleanPath(t *testing.T) {
	f := newFakeHost(t)

	err := RecordOwned(t.Context(), f.host(nil), Local{}, Owned{Path: "/usr/local/../bin/shard", Kind: KindBinary})
	if err == nil || !strings.Contains(err.Error(), "not a clean absolute path") {
		t.Fatalf("RecordOwned = %v, want the path refused", err)
	}
	if _, ok, err := LoadManifest(f.host(nil)); err != nil || ok {
		t.Fatalf("a refused record wrote a manifest: %v, %v", ok, err)
	}
}

func TestLoadManifestRefusesWhatARootRemoveCouldMisread(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"relative path": {body: `{"files":[{"path":"usr/local/bin/shard","kind":"binary"}]}`, want: "not a clean absolute path"},
		"the root":      {body: `{"files":[{"path":"/","kind":"binary"}]}`, want: "not a clean absolute path"},
		"unknown kind":  {body: `{"files":[{"path":"/usr/local/bin/shard","kind":"cache"}]}`, want: `unknown kind "cache"`},
		"not json":      {body: `{`, want: "decode the installation manifest"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			f.write(t, ManifestPath, tc.body)

			_, _, err := LoadManifest(f.host(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadManifest = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRemoveManifestTakesItsDirectory(t *testing.T) {
	f := newFakeHost(t)
	f.installed(t, linuxInstall("v0.1.0"))

	if err := removeManifest(t.Context(), f.host(nil)); err != nil {
		t.Fatalf("removeManifest: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, filepath.Dir(ManifestPath))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the manifest directory is still there: %v", err)
	}
}
