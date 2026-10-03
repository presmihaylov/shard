package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// openable is a path this process can open read-write, which is what the probe asks of /dev/kvm.
func openable(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// pick names the substrate and fails the test when the selection itself could not be made.
func pick(t *testing.T, named, root, kvm string) Selection {
	t.Helper()

	selected, err := selectProvider(named, root, kvm)
	if err != nil {
		t.Fatalf("selectProvider(%q, %q, %q): %v", named, root, kvm, err)
	}

	return selected
}

// recordUnder writes one sandbox record of that provider, the way an older daemon left it.
func recordUnder(t *testing.T, provider string) string {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "sandboxes", "abcdef012345")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	record, err := json.Marshal(models.Sandbox{ID: "abcdef012345", Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox.json"), record, 0o640); err != nil {
		t.Fatal(err)
	}

	return root
}

// SHARD-46: without --provider the host picks, and a Mac picks before the probe, so it answers the same either way.
func TestTheHostWithAKVMPicksFirecracker(t *testing.T) {
	kvm := openable(t)

	want, reason := "firecracker", kvm+" opens"
	if runtime.GOOS == "darwin" {
		want, reason = "vz", "Virtualization.framework"
	}
	got := pick(t, "", t.TempDir(), kvm)
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
	got := pick(t, "", t.TempDir(), kvm)
	if got.Provider != want || !strings.Contains(got.Reason, reason) {
		t.Errorf("a host without %s picks %+v, want %s for %s", kvm, got, want, reason)
	}
}

// A node that is there but will not open runs no microVM, so the probe must not hand the host firecracker.
func TestAKVMThatDoesNotOpenPicksGvisor(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("a Mac picks vz before the probe")
	}
	// A directory refuses O_RDWR for every user, where a mode 0000 file still opens as root.
	kvm := filepath.Join(t.TempDir(), "kvm")
	if err := os.Mkdir(kvm, 0o750); err != nil {
		t.Fatal(err)
	}

	got := pick(t, "", t.TempDir(), kvm)
	if got.Provider != "gvisor" || !strings.Contains(got.Reason, "does not open") {
		t.Errorf("a %s that does not open picks %+v, want gvisor for a node that will not open", kvm, got)
	}
}

// A root keeps what made its records: no other substrate can read them, and an upgrade must not switch one.
func TestARootKeepsWhatMadeItsRecords(t *testing.T) {
	kvm := openable(t)

	for _, provider := range []string{"gvisor", "sysbox", "runc", "vz", "firecracker"} {
		root := recordUnder(t, provider)
		got := pick(t, "", root, kvm)
		if got.Provider != provider || !strings.Contains(got.Reason, "records") {
			t.Errorf("a root of %s records picks %+v, want %s for what made them", provider, got, provider)
		}
	}
}

// A firecracker root holds its records inside <root>.xfs, so an unmounted image leaves the mountpoint empty.
func TestAnUnmountedDataImageKeepsFirecracker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shard")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+".xfs", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := pick(t, "", root, filepath.Join(t.TempDir(), "gone"))
	if got.Provider != "firecracker" || !strings.Contains(got.Reason, root+".xfs") {
		t.Errorf("a root whose data image is not mounted picks %+v, want firecracker for the image", got)
	}
}

// --provider outranks the host: an operator who names one is not guessing.
func TestTheNamedProviderWinsOverTheHost(t *testing.T) {
	kvm := openable(t)

	for _, name := range []string{"gvisor", "sysbox", "runc", "vz", "firecracker"} {
		got := pick(t, name, t.TempDir(), kvm)
		if got.Provider != name || !strings.Contains(got.Reason, "--provider") {
			t.Errorf("--provider %s picks %+v, want %s named by the flag", name, got, name)
		}
	}
}

// SHARD-275: a root is bound to what made it, so --provider may only name that one again.
func TestTheNamedProviderMustMatchTheRecords(t *testing.T) {
	kvm := openable(t)

	for _, made := range []string{"gvisor", "sysbox", "runc", "vz", "firecracker"} {
		root := recordUnder(t, made)
		if got := pick(t, made, root, kvm); got.Provider != made {
			t.Errorf("--provider %s over its own records picks %+v", made, got)
		}

		for _, name := range []string{"gvisor", "sysbox", "runc", "vz", "firecracker"} {
			if name == made {
				continue
			}
			_, err := selectProvider(name, root, kvm)
			if err == nil || !strings.Contains(err.Error(), "--provider "+name) || !strings.Contains(err.Error(), made+"'s") {
				t.Errorf("--provider %s over %s records: %v, want a refusal naming both", name, made, err)
			}
		}
	}
}

// The data image names firecracker even while it is unmounted, so another --provider is refused there too.
func TestTheNamedProviderMustMatchTheDataImage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shard")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+".xfs", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := selectProvider("gvisor", root, filepath.Join(t.TempDir(), "gone"))
	if err == nil || !strings.Contains(err.Error(), "firecracker's") || !strings.Contains(err.Error(), root+".xfs") {
		t.Errorf("--provider gvisor beside a data image: %v, want a refusal naming firecracker and the image", err)
	}
	if got := pick(t, "firecracker", root, filepath.Join(t.TempDir(), "gone")); got.Provider != "firecracker" {
		t.Errorf("--provider firecracker beside its own data image picks %+v", got)
	}
}

// sysbox is single-tenant and runc is a plain container, so no probe of a fresh root hands a host either.
func TestNoFreshRootIsHandedSysboxOrRunc(t *testing.T) {
	for _, kvm := range []string{openable(t), filepath.Join(t.TempDir(), "gone")} {
		if got := pick(t, "", t.TempDir(), kvm).Provider; got == "sysbox" || got == "runc" {
			t.Errorf("the probe of %s picked %s, which only --provider names", kvm, got)
		}
	}
}

// The line shard info prints names the substrate and the reason, in that order.
func TestASelectionPrintsTheProviderAndTheReason(t *testing.T) {
	if got := (Selection{Provider: "firecracker", Reason: "/dev/kvm opens"}).String(); got != "firecracker: /dev/kvm opens" {
		t.Errorf("the selection printed %q", got)
	}
}

// SHARD-343: a record an operator corrupted must not stop selection; a good record still names the substrate and the note names the bad one.
func TestARootWithAnUnreadableRecordStillSelectsAndNotes(t *testing.T) {
	root := recordUnder(t, "gvisor")

	// A record that will not decode sits beside the good one, under an id that sorts first so it is read first.
	dir := filepath.Join(root, "sandboxes", "0a0000000000")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}

	got := pick(t, "", root, openable(t))
	if got.Provider != "gvisor" || !strings.Contains(got.Reason, "records") {
		t.Errorf("a root with one unreadable record picks %+v, want gvisor for what made the rest", got)
	}
	if !strings.Contains(got.Unreadable, "0a0000000000") {
		t.Errorf("the selection note is %q, want it to name the unreadable record", got.Unreadable)
	}
}

// SHARD-343: when no record reads, the root's substrate is unknown, so a probe refuses rather than relabel a runc or gvisor root; only --provider recovers it.
func TestARootWhereNoRecordReadsRefusesAutoSelection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sandboxes", "abcdef012345")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := selectProvider("", root, openable(t))
	if err == nil || !strings.Contains(err.Error(), "--provider") {
		t.Fatalf("auto-selection over an all-unreadable root: %v, want a refusal that points at --provider", err)
	}

	got := pick(t, "runc", root, openable(t))
	if got.Provider != "runc" {
		t.Errorf("--provider over an all-unreadable root picks %+v, want runc as the recovery", got)
	}
	if !strings.Contains(got.Unreadable, "abcdef012345") {
		t.Errorf("the recovery selection note is %q, want it to name the unreadable record", got.Unreadable)
	}
}
