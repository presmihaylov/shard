package bundle_test

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/bundle"
)

const hostname = "shard-clone-test-box"

func baseDisk(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "base.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the base: %v", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc", Mode: 0o755}); err != nil {
		t.Fatalf("write etc: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/hostname", Mode: 0o644, Size: int64(len(hostname))}); err != nil {
		t.Fatalf("write hostname: %v", err)
	}
	if _, err := tw.Write([]byte(hostname)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close the tar: %v", err)
	}
	if err := ext4.Write(&buf, f); err != nil {
		t.Fatalf("Write: %v", err)
	}

	return path
}

func TestCloneRootDiskGrowsTheCloneToTheBound(t *testing.T) {
	base := baseDisk(t)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")

	shared, err := bundle.CloneRootDisk(base, dst, models.Resources{DiskMiB: 64})
	if err != nil {
		t.Fatalf("CloneRootDisk: %v", err)
	}
	if shared != reflinks(t, filepath.Dir(dst)) {
		t.Errorf("shared = %v, want %v on this filesystem", shared, !shared)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat the clone: %v", err)
	}
	if info.Size() != 64<<20 {
		t.Errorf("the clone is %d bytes, want 64 MiB", info.Size())
	}

	// The base keeps its size and its bytes: the clone has its own copy, shared or not.
	baseInfo, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat the base: %v", err)
	}
	if baseInfo.Size() >= info.Size() {
		t.Errorf("the base grew to %d bytes", baseInfo.Size())
	}
	clone, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read the clone: %v", err)
	}
	if !bytes.Contains(clone, []byte(hostname)) {
		t.Error("the clone lost the base's file")
	}
}

// reflinks says whether the filesystem under dir shares blocks, which is what CloneRootDisk reports as shared there.
func reflinks(t *testing.T, dir string) bool {
	t.Helper()
	src := filepath.Join(dir, "probe")
	if err := os.WriteFile(src, []byte("probe"), 0o600); err != nil {
		t.Fatalf("write the probe: %v", err)
	}
	err := bundle.Reflink(src, filepath.Join(dir, "probe-clone"))
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Reflink the probe: %v", err)
	}

	return err == nil
}

func TestReflinkRefusesAnExistingTarget(t *testing.T) {
	base := baseDisk(t)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(dst, []byte("taken"), 0o600); err != nil {
		t.Fatalf("plant the target: %v", err)
	}

	if err := bundle.Reflink(base, dst); err == nil {
		t.Fatal("the reflink took an existing target")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "taken" {
		t.Fatalf("the target after a refused reflink: %q, %v", got, err)
	}
}

func TestReflinkSharesOrRefusesAndNeverCopies(t *testing.T) {
	base := baseDisk(t)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")

	err := bundle.Reflink(base, dst)
	if errors.Is(err, errors.ErrUnsupported) {
		if _, statErr := os.Stat(dst); statErr == nil {
			t.Fatal("a refused reflink left a copy behind")
		}

		return
	}
	if err != nil {
		t.Fatalf("Reflink: %v", err)
	}
	want, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("read the base: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read the clone: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the clone differs from the base")
	}
}

func TestCloneRootDiskRefusesABoundUnderTheImage(t *testing.T) {
	base := baseDisk(t)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")

	_, err := bundle.CloneRootDisk(base, dst, models.Resources{DiskMiB: 1})
	if err == nil {
		t.Fatal("a 1 MiB bound took a bigger image")
	}
	if !strings.Contains(err.Error(), "MiB or more") || strings.Contains(err.Error(), "blocks") {
		t.Errorf("CloneRootDisk = %v, want the bound to set with its unit and no ext4 internals", err)
	}
	if _, ok := errors.AsType[*bundle.BoundError](err); !ok {
		t.Errorf("CloneRootDisk = %v, want a BoundError a public route answers", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("the refused clone stayed behind")
	}
}

func TestCloneRootDiskRefusesAnExistingTarget(t *testing.T) {
	base := baseDisk(t)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(dst, []byte("taken"), 0o600); err != nil {
		t.Fatalf("plant the target: %v", err)
	}

	if _, err := bundle.CloneRootDisk(base, dst, models.Resources{DiskMiB: 64}); err == nil {
		t.Fatal("the clone took an existing target")
	}
}

// seedDisk is the disk of a snapshot, 64 MiB of ext4 as the writer left it.
func seedDisk(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)
	if err := bundle.WriteOverlayDisk(src, models.Resources{DiskMiB: 64}); err != nil {
		t.Fatalf("write the seed: %v", err)
	}

	return src
}

// markDirty sets the bit a guest cut without an unmount or a freeze leaves on its disk.
func markDirty(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var sb ext4.SuperBlock
	raw := make([]byte, binary.Size(sb))
	if _, err := f.ReadAt(raw, 1024); err != nil {
		t.Fatal(err)
	}
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &sb); err != nil {
		t.Fatal(err)
	}
	sb.FeatureIncompat |= ext4.IncompatRecover
	var out bytes.Buffer
	if err := binary.Write(&out, binary.LittleEndian, &sb); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(out.Bytes(), 1024); err != nil {
		t.Fatal(err)
	}
}

func cloneFrom(src, dst string) func() error {
	return func() error {
		_, err := bundle.CloneFile(src, dst)
		return err
	}
}

func TestGrowSeedGrowsTheSnapshotsDiskToTheBound(t *testing.T) {
	src := seedDisk(t)
	dst := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)

	if err := bundle.GrowSeed(dst, models.Resources{DiskMiB: 128}, cloneFrom(src, dst)); err != nil {
		t.Fatalf("GrowSeed: %v", err)
	}

	for path, want := range map[string]int64{dst: 128 << 20, src: 64 << 20} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != want {
			t.Errorf("%s is %d bytes, want %d", filepath.Base(filepath.Dir(path)), info.Size(), want)
		}
	}
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the size stands alone")

		return
	}
	if out, err := exec.Command("e2fsck", "-fn", dst).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

func TestGrowSeedRefusesADiskAForceStopLeftDirty(t *testing.T) {
	src := seedDisk(t)
	markDirty(t, src)
	dst := filepath.Join(t.TempDir(), bundle.OverlayDiskFile)

	err := bundle.GrowSeed(dst, models.Resources{DiskMiB: 128}, cloneFrom(src, dst))
	if !errors.Is(err, ext4.ErrNeedsRecovery) {
		t.Fatalf("GrowSeed = %v, want ErrNeedsRecovery", err)
	}
	for _, want := range []string{"not stopped clean", "128 MiB", "omit resources.disk_mib", "let its entrypoint exit", "shard exec", "then stop it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("the refused disk stayed behind: %v", err)
	}
}
