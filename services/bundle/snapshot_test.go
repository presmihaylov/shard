package bundle_test

import (
	"encoding/json"
	"errors"
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
	b, _ := build(t, source, models.ImageConfig{})

	write(t, filepath.Join(b.Upper, "marker"), "written before the pause\n")
	write(t, filepath.Join(b.Tmp, "scratch"), "tmp\n")
	write(t, b.ReadyFile, "")

	checkpoint := t.TempDir()
	if err := b.Export(t.Context(), checkpoint); err != nil {
		t.Fatalf("Export: %v", err)
	}

	fork := models.SandboxSpec{
		ID:       "s-fork",
		Name:     "web-2",
		StateDir: t.TempDir(),
		Network: models.NetworkSpec{
			NetnsPath:   "/run/netns/s-fork",
			Userns:      models.UserNamespace{Path: "/run/shard/userns/s-fork", HostID: 165536, Size: 65536},
			Address:     netip.MustParsePrefix("10.87.0.3/16"),
			Nameservers: []netip.Addr{netip.MustParseAddr("1.1.1.1")},
		},
	}
	c, err := newService(t).Fork(checkpoint, fork)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	var got specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(c.Dir, "config.json"))), &got); err != nil {
		t.Fatalf("the fork's config.json does not parse: %v", err)
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
		if ns.Type == specs.UserNamespace && ns.Path != "/run/shard/userns/s-fork" {
			t.Errorf("userns %q, want the fork's", ns.Path)
		}
	}
	if len(got.Linux.UIDMappings) != 1 || got.Linux.UIDMappings[0].HostID != 165536 {
		t.Errorf("uid mappings %v, want the fork's single mapping", got.Linux.UIDMappings)
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
		t.Errorf("the fork lost the source's memory bound: %+v", got.Linux.Resources)
	}

	if readFile(t, filepath.Join(c.Upper, "marker")) != "written before the pause\n" {
		t.Error("the fork did not get the source's writable layer")
	}
	if readFile(t, filepath.Join(c.Tmp, "scratch")) != "tmp\n" {
		t.Error("the fork did not get the source's tmp")
	}
	if _, err := os.Stat(c.ReadyFile); err != nil {
		t.Errorf("the fork did not get the supervisor's files: %v", err)
	}
	if hosts := readFile(t, filepath.Join(c.Upper, "etc", "hosts")); !strings.Contains(hosts, "10.87.0.3\tweb-2") {
		t.Errorf("the fork's hosts file is %q, want the fork's address and name", hosts)
	}
}

func TestForkRefusesACheckpointWithNoConfig(t *testing.T) {
	spec := models.SandboxSpec{ID: "s-fork", StateDir: t.TempDir()}
	if _, err := newService(t).Fork(t.TempDir(), spec); err == nil {
		t.Error("Fork accepted an empty checkpoint")
	}
}

// A fork of an exited sandbox must answer Wait at once, so Export carries the exit record and Fork lays it back.
func TestForkCarriesTheExitRecord(t *testing.T) {
	source := newSpec(t)
	b, _ := build(t, source, models.ImageConfig{})

	write(t, b.ExitFile, "{\"kind\":\"exit\",\"code\":7,\"signal\":0}\n")

	checkpoint := t.TempDir()
	if err := b.Export(t.Context(), checkpoint); err != nil {
		t.Fatalf("Export: %v", err)
	}

	fork := models.SandboxSpec{ID: "s-fork", Name: "web-2", StateDir: t.TempDir(),
		Network: models.NetworkSpec{NetnsPath: "/run/netns/s-fork"}}
	c, err := newService(t).Fork(checkpoint, fork)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	exit, found, err := bundle.ReadExitStatus(c.ExitFile)
	if err != nil || !found {
		t.Fatalf("ReadExitStatus(%q) = %+v, %v, %v, want the carried record", c.ExitFile, exit, found, err)
	}
	if exit.Code != 7 {
		t.Errorf("the fork carried exit %+v, want code 7", exit)
	}
}

// A snapshot keeps the writable layer and /tmp, and none of the last run's /.shard files.
func TestSnapshotKeepsTheLayersAndNotTheRunFiles(t *testing.T) {
	b, _ := build(t, newSpec(t), models.ImageConfig{})
	write(t, filepath.Join(b.Upper, "marker"), "kept\n")
	write(t, filepath.Join(b.Tmp, "scratch"), "tmp\n")
	write(t, b.ReadyFile, "")

	dir := t.TempDir()
	if err := b.Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "upper", "marker")); got != "kept\n" {
		t.Errorf("the snapshot holds %q in the upper, want the source's file", got)
	}
	if got := readFile(t, filepath.Join(dir, "tmp", "scratch")); got != "tmp\n" {
		t.Errorf("the snapshot holds %q in /tmp, want the source's file", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "shard")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the snapshot holds a /.shard copy (%v), want none", err)
	}
}

// A bundle that was never built has no layer to copy, and the refusal comes before any write.
func TestSnapshotRefusesABundleThatWasNeverBuilt(t *testing.T) {
	b, err := bundle.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	dir := t.TempDir()
	if err := b.Snapshot(t.Context(), dir); err == nil {
		t.Error("Snapshot of a bundle with no layers returned no error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Snapshot of a bundle with no layers wrote %v into the snapshot", entries)
	}
}

// The copy never follows a symlink out of the tree: a link to a host path stays a link.
func TestSnapshotKeepsASymlinkToTheHostAsALink(t *testing.T) {
	b, _ := build(t, newSpec(t), models.ImageConfig{})
	host := filepath.Join(t.TempDir(), "host-secret")
	write(t, host, "host only\n")
	if err := os.Symlink(host, filepath.Join(b.Upper, "escape")); err != nil {
		t.Fatalf("plant the symlink: %v", err)
	}

	dir := t.TempDir()
	if err := b.Snapshot(t.Context(), dir); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	link := filepath.Join(dir, "upper", "escape")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the snapshot lost the symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the snapshot holds %s as %v, want the link and not its target", link, info.Mode())
	}
	if target, err := os.Readlink(link); err != nil || target != host {
		t.Errorf("the link points at %q (%v), want %q", target, err, host)
	}
}

// A seeded build starts from the snapshot's layers, and writes its own network files over them.
func TestBuildStartsFromTheSeed(t *testing.T) {
	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "upper", "etc"), 0o755); err != nil {
		t.Fatalf("create the seed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "tmp"), 0o755); err != nil {
		t.Fatalf("create the seed: %v", err)
	}
	write(t, filepath.Join(seed, "upper", "marker"), "seeded\n")
	write(t, filepath.Join(seed, "upper", "etc", "resolv.conf"), "nameserver 192.0.2.1\n")
	write(t, filepath.Join(seed, "tmp", "scratch"), "tmp\n")

	spec := newSpec(t)
	spec.Name = "web-2"
	spec.Seed = seed
	spec.Network = models.NetworkSpec{
		NetnsPath:   "/run/netns/s-test",
		Address:     netip.MustParsePrefix("10.87.0.3/16"),
		Nameservers: []netip.Addr{netip.MustParseAddr("1.1.1.1")},
	}
	b, _ := build(t, spec, models.ImageConfig{})

	if got := readFile(t, filepath.Join(b.Upper, "marker")); got != "seeded\n" {
		t.Errorf("the upper holds %q, want the seed's file", got)
	}
	if got := readFile(t, filepath.Join(b.Tmp, "scratch")); got != "tmp\n" {
		t.Errorf("/tmp holds %q, want the seed's file", got)
	}
	if got := readFile(t, filepath.Join(b.Upper, "etc", "resolv.conf")); !strings.Contains(got, "1.1.1.1") {
		t.Errorf("resolv.conf holds %q, want this sandbox's nameserver over the seed's", got)
	}
	if hosts := readFile(t, filepath.Join(b.Upper, "etc", "hosts")); !strings.Contains(hosts, "10.87.0.3\tweb-2") {
		t.Errorf("the hosts file is %q, want this sandbox's address and name", hosts)
	}
}
