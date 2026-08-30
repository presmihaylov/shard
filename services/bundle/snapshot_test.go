package bundle_test

import (
	"encoding/json"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

func TestForkIsTheSourceUnderANewIdentity(t *testing.T) {
	source := newSpec(t)
	source.Name = "web"
	source.Network = models.NetworkSpec{
		NetnsPath:   "/run/netns/s-test",
		Address:     netip.MustParsePrefix("10.87.0.2/16"),
		Nameservers: []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	}
	source.Resources = models.Resources{MemoryMiB: 512}
	b, _ := build(t, source, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	write(t, filepath.Join(b.Upper, "marker"), "written before the pause\n")
	write(t, filepath.Join(b.Tmp, "scratch"), "tmp\n")
	write(t, b.ReadyFile, "")

	snapshot := t.TempDir()
	if err := b.Export(snapshot); err != nil {
		t.Fatalf("Export: %v", err)
	}

	fork := models.SandboxSpec{
		ID:       "s-fork",
		Name:     "web-2",
		StateDir: t.TempDir(),
		Network: models.NetworkSpec{
			NetnsPath:   "/run/netns/s-fork",
			Address:     netip.MustParsePrefix("10.87.0.3/16"),
			Nameservers: []netip.Addr{netip.MustParseAddr("1.1.1.1")},
		},
	}
	c, err := newService(t).Fork(snapshot, fork)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	var got specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(c.Dir, "config.json"))), &got); err != nil {
		t.Fatalf("the clone's config.json does not parse: %v", err)
	}

	if got.Hostname != "web-2" {
		t.Errorf("hostname %q, want the fork's name", got.Hostname)
	}
	if got.Linux.CgroupsPath != bundle.CgroupsPath("s-fork") {
		t.Errorf("cgroups path %q, want the fork's", got.Linux.CgroupsPath)
	}
	for _, ns := range got.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace && ns.Path != "/run/netns/s-fork" {
			t.Errorf("netns %q, want the fork's", ns.Path)
		}
	}

	// The restore checks the process and the mounts by destination, so those must be the source's.
	var want specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(b.Dir, "config.json"))), &want); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Process.Args, " ") != strings.Join(want.Process.Args, " ") {
		t.Errorf("args %v, want the source's %v", got.Process.Args, want.Process.Args)
	}
	if len(got.Mounts) != len(want.Mounts) {
		t.Fatalf("%d mounts, want the source's %d", len(got.Mounts), len(want.Mounts))
	}
	for i := range got.Mounts {
		if got.Mounts[i].Destination != want.Mounts[i].Destination || got.Mounts[i].Type != want.Mounts[i].Type ||
			!slices.Equal(got.Mounts[i].Options, want.Mounts[i].Options) {
			t.Errorf("mount %d is %+v, want %+v", i, got.Mounts[i], want.Mounts[i])
		}
		if strings.HasPrefix(got.Mounts[i].Source, source.StateDir) {
			t.Errorf("mount %d still points into the source's state directory: %s", i, got.Mounts[i].Source)
		}
	}
	if got.Linux.Resources.Memory == nil || *got.Linux.Resources.Memory.Limit != 512<<20 {
		t.Errorf("the clone lost the source's memory bound: %+v", got.Linux.Resources)
	}

	if readFile(t, filepath.Join(c.Upper, "marker")) != "written before the pause\n" {
		t.Error("the clone did not get the source's writable layer")
	}
	if readFile(t, filepath.Join(c.Tmp, "scratch")) != "tmp\n" {
		t.Error("the clone did not get the source's tmp")
	}
	if _, err := os.Stat(c.ReadyFile); err != nil {
		t.Errorf("the clone did not get the supervisor's files: %v", err)
	}
	if hosts := readFile(t, filepath.Join(c.Upper, "etc", "hosts")); !strings.Contains(hosts, "10.87.0.3\tweb-2") {
		t.Errorf("the clone's hosts file is %q, want the fork's address and name", hosts)
	}
}

func TestForkRefusesASnapshotWithNoConfig(t *testing.T) {
	spec := models.SandboxSpec{ID: "s-fork", StateDir: t.TempDir()}
	if _, err := newService(t).Fork(t.TempDir(), spec); err == nil {
		t.Error("Fork accepted an empty snapshot")
	}
}

// A clone of a bundle is the clone of its snapshot would be, read from the state directory instead.
func TestCloneIsTheSourceUnderANewIdentity(t *testing.T) {
	source := newSpec(t)
	source.Name = "web"
	source.Network = models.NetworkSpec{NetnsPath: "/run/netns/s-test", Address: netip.MustParsePrefix("10.87.0.2/16")}
	source.Resources = models.Resources{MemoryMiB: 512}
	b, _ := build(t, source, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	write(t, filepath.Join(b.Upper, "marker"), "written before the stop\n")
	write(t, b.ExitFile, `{"code":3}`)

	opened, err := bundle.Open(source.StateDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	clone := models.SandboxSpec{
		ID:       "s-clone",
		Name:     "web-2",
		StateDir: t.TempDir(),
		Network:  models.NetworkSpec{NetnsPath: "/run/netns/s-clone", Address: netip.MustParsePrefix("10.87.0.3/16")},
	}
	c, err := newService(t).Clone(opened, clone)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}

	var got specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(c.Dir, "config.json"))), &got); err != nil {
		t.Fatalf("the clone's config.json does not parse: %v", err)
	}

	var want specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(b.Dir, "config.json"))), &want); err != nil {
		t.Fatal(err)
	}

	if got.Hostname != "web-2" || got.Linux.CgroupsPath != bundle.CgroupsPath("s-clone") {
		t.Errorf("hostname %q under cgroup %q, want the clone's own", got.Hostname, got.Linux.CgroupsPath)
	}
	for _, ns := range got.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace && ns.Path != "/run/netns/s-clone" {
			t.Errorf("netns %q, want the clone's", ns.Path)
		}
	}
	if strings.Join(got.Process.Args, " ") != strings.Join(want.Process.Args, " ") {
		t.Errorf("args %v, want the source's %v", got.Process.Args, want.Process.Args)
	}
	// The rootfs annotation is what the provider mounts the clone over, so it must be the source's.
	if !maps.Equal(got.Annotations, want.Annotations) {
		t.Errorf("annotations %v, want the source's %v", got.Annotations, want.Annotations)
	}
	for i := range got.Mounts {
		if strings.HasPrefix(got.Mounts[i].Source, source.StateDir) {
			t.Errorf("mount %d still points into the source's state directory: %s", i, got.Mounts[i].Source)
		}
	}
	if got.Linux.Resources.Memory == nil || *got.Linux.Resources.Memory.Limit != 512<<20 {
		t.Errorf("the clone lost the source's memory bound: %+v", got.Linux.Resources)
	}

	if readFile(t, filepath.Join(c.Upper, "marker")) != "written before the stop\n" {
		t.Error("the clone did not get the source's writable layer")
	}
	// The provider clears the exit before the clone runs, the way a create does: the copy carries it.
	if _, err := os.Stat(c.ExitFile); err != nil {
		t.Errorf("the clone did not get the source's shard layer: %v", err)
	}
	if hosts := readFile(t, filepath.Join(c.Upper, "etc", "hosts")); !strings.Contains(hosts, "10.87.0.3\tweb-2") {
		t.Errorf("the clone's hosts file is %q, want the clone's address and name", hosts)
	}
}

func TestCloneRefusesASourceWithNoConfig(t *testing.T) {
	opened, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := models.SandboxSpec{ID: "s-clone", StateDir: t.TempDir()}
	if _, err := newService(t).Clone(opened, spec); err == nil {
		t.Error("Clone accepted a source that was never built")
	}
}
