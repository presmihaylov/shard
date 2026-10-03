//go:build integration && linux

package hostclean

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/xfs"
)

// The mounts come back deepest first, because an overlay pins the root it lives under.
func TestMountPointsReadsTheOnesUnderARootOfOurs(t *testing.T) {
	listed := "24 1 0:22 / /proc rw shared:5 - proc proc rw\n" +
		"31 24 0:28 / /tmp/shard-itest99/sandboxes/one/bundle rw - overlay overlay rw\n" +
		"32 24 0:29 / /tmp/shard-other/mnt rw - overlay overlay rw\n"

	points := mountPoints(listed, []string{"/tmp/shard-itest"})
	if want := []string{"/tmp/shard-itest99/sandboxes/one/bundle"}; !slices.Equal(points, want) {
		t.Errorf("mountPoints = %v, want %v", points, want)
	}
}

// The record is the only handle on the veth and on who holds the sandbox, so an unfinished record names neither.
func TestReadRecordReadsTheHandlesOffTheRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"id":"amber-otter-1a2b","provider":"gvisor","host_interface":"shardv7"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := readRecord(path); got != (record{Provider: "gvisor", HostInterface: "shardv7"}) {
		t.Errorf("readRecord = %+v, want gvisor and shardv7", got)
	}
	if got := readRecord(filepath.Join(dir, "missing.json")); got != (record{}) {
		t.Errorf("a record that is not there named %+v", got)
	}
}

// The record naming a provider is what names a sandbox to delete: runsc keeps no <root>/runsc/<id>, so a stat there let a live sentry outlive a killed daemon (SHARD-226).
func TestSandboxOfNamesTheSandboxTheRecordHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
		named  bool
	}{
		{"gvisor, with no runtime dir", `{"provider":"gvisor"}`, true},
		{"sysbox", `{"provider":"sysbox"}`, true},
		{"runc", `{"provider":"runc"}`, true},
		{"a record a crashed run never finished", `{"provider":`, false},
		{"firecracker, whose vmm the scan of /proc names instead", `{"provider":"firecracker"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, sandboxDir, "amber-otter-1a2b")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, recordFile), []byte(tc.record), 0o600); err != nil {
				t.Fatal(err)
			}

			if left := sandboxOf(root, "amber-otter-1a2b"); names(left, "the sandbox") != tc.named {
				t.Errorf("the record %s names the sandbox = %v, want %v: %v", tc.record, !tc.named, tc.named, left)
			}
		})
	}
}

// Only the argument after --api-sock names the socket: a process that only mentions a socket path is not a vmm (SHARD-377).
func TestAPISocketReadsOnlyTheFlagArgument(t *testing.T) {
	const sock = "/tmp/shard-itest1/sandboxes/amber-otter-1a2b/firecracker.sock"
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"the vmm", []string{"firecracker", apiSockFlag, sock, ""}, sock},
		{"a process that names the path in one argument", []string{"grep", apiSockFlag + " " + sock, ""}, ""},
		{"a process that names the path with no flag", []string{"tail", sock, ""}, ""},
		{"the flag with no argument", []string{"firecracker", apiSockFlag}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := apiSocket(tc.argv); got != tc.want {
				t.Errorf("apiSocket(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// A line is ours only when both its image and its point sit under a root of ours.
func TestLeftFstabNamesOnlyTheLinesOfOurRoots(t *testing.T) {
	xfs.FstabPath = filepath.Join(t.TempDir(), "fstab")
	start := "/var/lib/shard.xfs /var/lib/shard xfs loop 0 0\n/tmp/shard-daemon7.xfs /tmp/shard-daemon7 xfs loop 0 0\n/var/lib/x.xfs /tmp/shard-daemon8 xfs loop 0 0\n"
	if err := os.WriteFile(xfs.FstabPath, []byte(start), 0o644); err != nil {
		t.Fatal(err)
	}

	left, err := leftFstab([]string{"/tmp/shard-daemon"})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Path != "/tmp/shard-daemon7" {
		t.Fatalf("leftFstab = %v, want the line of /tmp/shard-daemon7 only", left)
	}
	if err := removeEach(left); err != nil {
		t.Fatal(err)
	}
	loops, err := xfs.FstabLoops()
	if err != nil {
		t.Fatal(err)
	}
	if len(loops) != 2 {
		t.Errorf("the sweep left %v, want the two lines that are not ours", loops)
	}
}

// The fix above rests on this: every runtime takes a forced delete of a sandbox it does not hold as done.
func TestAForcedDeleteOfASandboxTheRuntimeDoesNotHoldIsDone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the runtimes want root")
	}

	for provider, binary := range runtimes {
		t.Run(provider, func(t *testing.T) {
			if _, err := exec.LookPath(binary); err != nil {
				t.Skipf("no %s on PATH", binary)
			}

			state := filepath.Join(t.TempDir(), binary)
			if err := run(binary, "--root", state, "delete", "--force", "amber-otter-1a2b")(); err != nil {
				t.Errorf("%s refused to delete a sandbox it does not hold: %v", binary, err)
			}
		})
	}
}

// The namespace delete takes the veth pair with it, so the link delete after it finds nothing and Sweep must still come back clean (SHARD-227).
func TestALinkThatIsGoneIsSwept(t *testing.T) {
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip on PATH")
	}

	if err := deleteLink("shardv-none")(); err != nil {
		t.Errorf("a link that is gone was not taken as swept: %v", err)
	}
}

// A link that is there is still taken, and a second take of it is the no-op the first left.
func TestALinkThatIsThereIsTaken(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ip link add wants root")
	}

	const name = "shardvt227"
	if err := run("ip", "link", "add", name, "type", "veth", "peer", "name", name+"p")(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := deleteLink(name)(); err != nil {
			t.Error(err)
		}
	})

	for range 2 {
		if err := deleteLink(name)(); err != nil {
			t.Fatalf("deleteLink %s: %v", name, err)
		}
		if shown(name) {
			t.Fatalf("%s is still on the host", name)
		}
	}
}

// SHARD-272: every root shares the bridge and the tables, so a port on the bridge keeps them, and once it goes they go.
func TestSweepTakesTheHostNetOnlyOnceNothingHoldsIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ip link and nft want root")
	}
	for _, binary := range []string{"ip", "nft", "ss"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("no %s on PATH", binary)
		}
	}
	held, err := hostNetHeld()
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Skip("a sandbox or a daemon on this host holds the bridge and the tables")
	}

	const port = "shardvt272"
	if !shown(hostBridge) {
		mustRun(t, "ip", "link", "add", hostBridge, "type", "bridge")
	}
	for _, family := range hostTableFamilies {
		mustRun(t, "nft", "add", "table", family, hostTable)
	}
	mustRun(t, "ip", "link", "add", port, "type", "veth", "peer", "name", port+"p")
	t.Cleanup(func() {
		if err := errors.Join(deleteLink(port)(), sweepShared()); err != nil {
			t.Error(err)
		}
	})
	mustRun(t, "ip", "link", "set", port, "master", hostBridge)

	if err := sweepShared(); err != nil {
		t.Fatalf("sweep with a port on the bridge: %v", err)
	}
	if !shown(hostBridge) || tablesListed(t) != len(hostTableFamilies) {
		t.Fatalf("the sweep took the bridge or a table while %s was a port of %s", port, hostBridge)
	}

	if err := deleteLink(port)(); err != nil {
		t.Fatal(err)
	}
	if err := sweepShared(); err != nil {
		t.Fatalf("sweep with no port on the bridge: %v", err)
	}
	if shown(hostBridge) || tablesListed(t) != 0 {
		t.Errorf("the bridge or a table outlived a sweep that nothing held")
	}
}

// tablesListed counts the shard tables the host lists.
func tablesListed(t *testing.T) int {
	t.Helper()

	count := 0
	for _, family := range hostTableFamilies {
		if exec.Command("nft", "list", "table", family, hostTable).Run() == nil {
			count++
		}
	}

	return count
}

func mustRun(t *testing.T, binary string, args ...string) {
	t.Helper()

	if err := run(binary, args...)(); err != nil {
		t.Fatal(err)
	}
}

func names(left []Leftover, what string) bool {
	return slices.ContainsFunc(left, func(l Leftover) bool { return l.What == what })
}

// A firecracker root keeps its data image and the image lock beside it, under the same prefix: they are files to take, not roots to read (SHARD-377).
func TestFindTakesAFileBesideARootAsAFile(t *testing.T) {
	useFstab(t, "")
	prefix := filepath.Join(t.TempDir(), "shard-itest")
	root := prefix + "1"
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{root + ".xfs", root + ".xfs.lock"} {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	left, err := Find(prefix)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	want := []string{"the temp root " + root, "the temp file " + root + ".xfs", "the temp file " + root + ".xfs.lock"}
	if got := described(left); !slices.Equal(got, want) {
		t.Fatalf("Find = %q, want %q", got, want)
	}
	if err := removeEach(left); err != nil {
		t.Fatal(err)
	}
	if rest, err := filepath.Glob(prefix + "*"); err != nil || len(rest) != 0 {
		t.Errorf("the sweep left %v (%v)", rest, err)
	}
}

// The image goes last: the mount over it, the loop under that mount and the fstab line that names it all go first (SHARD-377).
func TestFindTakesAnImageOnlyAfterItsMountLoopAndLine(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("a loop mount wants root")
	}
	if err := xfs.Have(); err != nil {
		t.Skip(err)
	}
	prefix := filepath.Join(t.TempDir(), "shard-itest")
	root := prefix + "1"
	image := root + ".xfs"
	useFstab(t, image+" "+root+" xfs loop 0 0\n")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := xfs.MakeImage(t.Context(), image, 320<<20); err != nil {
		t.Fatal(err)
	}
	if err := xfs.Mount(t.Context(), image, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unmount(t, root) })

	left, err := Find(prefix)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	order := []string{"the mount", "the loop", "the fstab line", "the temp file"}
	at := make([]int, len(order))
	for i, what := range order {
		at[i] = slices.IndexFunc(left, func(l Leftover) bool { return l.What == what })
	}
	if slices.Contains(at, -1) || !slices.IsSorted(at) {
		t.Fatalf("Find = %q, want %v in that order", described(left), order)
	}

	if err := removeEach(left); err != nil {
		t.Fatal(err)
	}
	if rest, err := Find(prefix); err != nil || len(rest) != 0 {
		t.Errorf("after the sweep Find = %q (%v), want nothing", described(rest), err)
	}
}

// unmount gives back a mount a failed test left; the unmount clears the loop under it too.
func unmount(t *testing.T, point string) {
	t.Helper()

	listed, err := os.ReadFile(mountinfo)
	if err != nil {
		t.Error(err)
		return
	}
	if len(mountPoints(string(listed), []string{point})) == 0 {
		return
	}
	if err := run("umount", point)(); err != nil {
		t.Error(err)
	}
}

// useFstab points the fstab this package reads at a file of the test's own, holding lines.
func useFstab(t *testing.T, lines string) {
	t.Helper()

	orig := xfs.FstabPath
	t.Cleanup(func() { xfs.FstabPath = orig })
	xfs.FstabPath = filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(xfs.FstabPath, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

func described(left []Leftover) []string {
	out := make([]string, 0, len(left))
	for _, l := range left {
		out = append(out, l.String())
	}

	return out
}

// A cgroup parent the host had before the run stays as it was, and one the run made goes once it is idle (SHARD-377).
func TestSweepDropsOnlyACgroupParentTheRunMade(t *testing.T) {
	useFstab(t, "")
	prefix := filepath.Join(t.TempDir(), "shard-itest")
	orig := parentPath
	t.Cleanup(func() { parentPath, parentMade = orig, false })

	for _, tc := range []struct {
		name string
		had  bool
	}{{"the host had it", true}, {"the run made it", false}} {
		t.Run(tc.name, func(t *testing.T) {
			parentPath = filepath.Join(t.TempDir(), cgroupParent)
			if tc.had {
				if err := os.Mkdir(parentPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := Refuse(prefix); err != nil {
				t.Fatalf("Refuse: %v", err)
			}
			if !tc.had {
				if err := os.Mkdir(parentPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			left, err := leftParent()
			if err != nil {
				t.Fatalf("leftParent: %v", err)
			}
			if named := len(left) == 1 && left[0].Path == parentPath; named == tc.had {
				t.Errorf("leftParent = %v, want the parent named %v", described(left), !tc.had)
			}
		})
	}
}

// A parent Refuse cannot stat is no proof that it is absent, so the run fails rather than take the parent as its own (SHARD-377).
func TestRefuseFailsOnACgroupParentItCannotStat(t *testing.T) {
	useFstab(t, "")
	orig := parentPath
	t.Cleanup(func() { parentPath, parentMade = orig, false })
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	parentPath = filepath.Join(file, cgroupParent)

	err := Refuse(filepath.Join(t.TempDir(), "shard-itest"))
	if err == nil || !strings.Contains(err.Error(), parentPath) || parentMade {
		t.Errorf("Refuse = %v with parentMade %v, want an error naming %s and the parent not taken", err, parentMade, parentPath)
	}
}
