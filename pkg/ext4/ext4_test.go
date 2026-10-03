package ext4

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const mib = 1 << 20

// writeImage lays down a small tree with what a directory unpack loses: an owner, an xattr, a device node and a hard link.
func writeImage(t *testing.T, path string) {
	t.Helper()
	writeImageWith(t, path, 3*mib)
}

// writeImageWith is writeImage with a big file of the given size, which sets how many block groups the data spans.
func writeImageWith(t *testing.T, path string, bigSize int) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc", Mode: 0o755}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/hostname", Mode: 0o644, Size: 6, Uid: 1000, Gid: 1000, PAXRecords: map[string]string{"SCHILY.xattr.user.shard": "yes"}}))
	_, err := tw.Write([]byte("shard\n"))
	must(err)
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/link", Linkname: "hostname"}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "etc/hard", Linkname: "etc/hostname"}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "dev", Mode: 0o755}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666, Devmajor: 1, Devminor: 3}))
	big := make([]byte, bigSize)
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "big", Mode: 0o644, Size: int64(len(big))}))
	_, err = tw.Write(big)
	must(err)
	must(tw.Close())

	f, err := os.Create(path)
	must(err)
	must(Write(&buf, f))
	must(f.Close())
}

// writeEmptyImage lays down the smallest image Write makes, the shape WriteOverlayDisk grows from.
func writeEmptyImage(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := tar.NewWriter(&buf).Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

type summary struct {
	sb     SuperBlock
	groups uint32
	free   uint32
	inodes uint32
}

// summarize reads the superblock and every descriptor and recounts the free blocks from the bitmaps.
func summarize(t *testing.T, path string) summary {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var s summary
	if err := readAt(f, superBlockOffset, &s.sb); err != nil {
		t.Fatal(err)
	}
	if s.sb.Magic != SuperBlockMagic {
		t.Fatalf("magic %#x", s.sb.Magic)
	}
	s.groups = (s.sb.BlocksCountLow-1)/blocksPerGroup + 1
	for g := range s.groups {
		var gd GroupDescriptor
		if err := readAt(f, descriptorOffset(g), &gd); err != nil {
			t.Fatal(err)
		}
		var bitmap [BlockSize]byte
		if _, err := f.ReadAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
			t.Fatal(err)
		}
		var used int
		for _, b := range bitmap {
			used += bits.OnesCount8(b)
		}
		if uint32(blocksPerGroup-used) != uint32(gd.FreeBlocksCountLow) {
			t.Fatalf("group %d: bitmap says %d free, the descriptor says %d", g, blocksPerGroup-used, gd.FreeBlocksCountLow)
		}
		s.free += uint32(gd.FreeBlocksCountLow)
		s.inodes += uint32(gd.FreeInodesCountLow)
	}
	if s.free != s.sb.FreeBlocksCountLow {
		t.Fatalf("descriptors say %d free blocks, the superblock says %d", s.free, s.sb.FreeBlocksCountLow)
	}
	if s.inodes != s.sb.FreeInodesCount {
		t.Fatalf("descriptors say %d free inodes, the superblock says %d", s.inodes, s.sb.FreeInodesCount)
	}
	return s
}

// fsck runs e2fsck when the host has one; macOS does not, the CI runner does.
func fsck(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("e2fsck"); err != nil {
		t.Logf("no e2fsck on PATH, the bitmap recount stands alone")
		return
	}
	out, err := exec.Command("e2fsck", "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck: %v\n%s", err, out)
	}
}

func TestWriteMakesAReadWriteImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	s := summarize(t, img)
	if s.sb.FeatureRoCompat&RoCompatReadonly != 0 {
		t.Fatal("the image is marked read-only")
	}
	if s.sb.FeatureCompat&CompatHasJournal != 0 {
		t.Fatal("the image has a journal")
	}
	fsck(t, img)
}

func TestGrowAddsGroupsAndKeepsTheCountsConsistent(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	before := summarize(t, img)
	for _, size := range []int64{203 * mib, 300 * mib, 20 << 30} {
		if err := Grow(img, size); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(img)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() != size {
			t.Fatalf("size %d, want %d", st.Size(), size)
		}
		s := summarize(t, img)
		if int64(s.sb.BlocksCountLow)*BlockSize != size {
			t.Fatalf("superblock counts %d blocks for %d bytes", s.sb.BlocksCountLow, size)
		}
		if s.sb.InodesCount != s.groups*s.sb.InodesPerGroup {
			t.Fatalf("%d inodes for %d groups", s.sb.InodesCount, s.groups)
		}
		if s.free <= before.free {
			t.Fatalf("grow to %d left %d free blocks, before %d", size, s.free, before.free)
		}
		if s.sb.FeatureCompat&CompatHasJournal == 0 {
			t.Fatalf("grow to %d left no journal", size)
		}
		fsck(t, img)
		before = s
	}
}

func TestGrowRefusesTheWrongSizes(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	st, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		size int64
		want string
	}{
		{st.Size() + 1, "not a multiple"},
		{BlockSize, "smaller than the current"},
		{MaxDiskSize + BlockSize, "exceeds the maximum"},
		{128*mib + BlockSize, "under its"},
	} {
		err := Grow(img, tc.size)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("grow to %d: %v, want %q", tc.size, err, tc.want)
		}
	}
	// A packed image has no run for a journal at its own size; a grow that makes room succeeds.
	if err := Grow(img, 64*mib); err != nil {
		t.Fatalf("grow to a valid size: %v", err)
	}
	if err := Grow(img, 64*mib); err != nil {
		t.Fatalf("grow to the same size, journal already present: %v", err)
	}
}

func TestGrowRefusesADescriptorBlockAMountTookOver(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var gd GroupDescriptor
	if err := readAt(f, descriptorOffset(0), &gd); err != nil {
		t.Fatal(err)
	}
	// Block 2 is the second descriptor block; a kernel that mounted the image may have handed it out.
	var bitmap [BlockSize]byte
	if _, err := f.ReadAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
		t.Fatal(err)
	}
	bitmap[0] |= 1 << 2
	if _, err := f.WriteAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = Grow(img, 17<<30)
	if err == nil || !strings.Contains(err.Error(), "mounted since") {
		t.Fatalf("got %v", err)
	}
}

func TestWriteReservesAFullGroupOfInodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		big    int
		groups uint32
		grow   int64
	}{
		{"one group", 3 * mib, 1, 64 * mib},
		// 126 MiB of data leaves the wider table no room in the first group, so the bitmaps open a second.
		{"the table crosses into a second group", 126 * mib, 2, 300 * mib},
		{"two groups of data", 130 * mib, 2, 300 * mib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "rootfs.ext4")
			writeImageWith(t, img, tc.big)
			s := summarize(t, img)
			if s.sb.InodesPerGroup != inodesPerGroup || s.groups != tc.groups || s.sb.InodesCount != inodesPerGroup*tc.groups {
				t.Fatalf("%d inodes per group over %d groups, %d in all; want %d over %d", s.sb.InodesPerGroup, s.groups, s.sb.InodesCount, inodesPerGroup, tc.groups)
			}
			if s.sb.FreeInodesCount < inodesPerGroup*tc.groups-64 {
				t.Fatalf("%d free inodes of %d", s.sb.FreeInodesCount, s.sb.InodesCount)
			}
			st, err := os.Stat(img)
			if err != nil {
				t.Fatal(err)
			}
			if int64(s.sb.BlocksCountLow)*BlockSize != st.Size() {
				t.Fatalf("superblock counts %d blocks for %d bytes", s.sb.BlocksCountLow, st.Size())
			}
			fsck(t, img)
			if err := Grow(img, tc.grow); err != nil {
				t.Fatal(err)
			}
			fsck(t, img)
		})
	}
}

func TestGrowAddsAJournal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, string)
	}{
		{"empty", writeEmptyImage},
		{"with data", writeImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "rootfs.ext4")
			tc.write(t, img)
			if err := Grow(img, 64*mib); err != nil {
				t.Fatal(err)
			}
			s := summarize(t, img)
			if s.sb.FeatureCompat&CompatHasJournal == 0 {
				t.Fatal("no has_journal flag")
			}
			if s.sb.JournalInum != journalInode || s.sb.JournalBackupType != 1 {
				t.Fatalf("journal inode %d, backup type %d", s.sb.JournalInum, s.sb.JournalBackupType)
			}

			f, err := os.Open(img)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var gd GroupDescriptor
			if err := readAt(f, descriptorOffset(0), &gd); err != nil {
				t.Fatal(err)
			}
			var inode [inodeSize]byte
			if _, err := f.ReadAt(inode[:], int64(gd.InodeTableLow)*BlockSize+(journalInode-1)*inodeSize); err != nil {
				t.Fatal(err)
			}
			n := journalBlocks(s.sb.BlocksCountLow)
			if got := binary.LittleEndian.Uint16(inode[0x00:]); got != journalMode {
				t.Fatalf("journal inode mode %#x, want %#x", got, journalMode)
			}
			if got := binary.LittleEndian.Uint16(inode[0x1a:]); got != 1 {
				t.Fatalf("journal inode links %d, want 1", got)
			}
			if got := binary.LittleEndian.Uint32(inode[0x20:]); got&extentsFlag == 0 {
				t.Fatalf("journal inode flags %#x lack extents", got)
			}
			if got := binary.LittleEndian.Uint32(inode[0x04:]); got != n*BlockSize {
				t.Fatalf("journal inode size %d, want %d", got, n*BlockSize)
			}
			if got := binary.LittleEndian.Uint16(inode[0x28:]); got != extentMagic {
				t.Fatalf("extent header magic %#x, want %#x", got, extentMagic)
			}
			if got := binary.LittleEndian.Uint16(inode[0x38:]); uint32(got) != n {
				t.Fatalf("extent length %d, want %d", got, n)
			}
			phys := binary.LittleEndian.Uint32(inode[0x3c:])

			var jsb [64]byte
			if _, err := f.ReadAt(jsb[:], int64(phys)*BlockSize); err != nil {
				t.Fatal(err)
			}
			if got := binary.BigEndian.Uint32(jsb[0:]); got != jbd2Magic {
				t.Fatalf("jbd2 magic %#x, want %#x", got, jbd2Magic)
			}
			if got := binary.BigEndian.Uint32(jsb[4:]); got != jbd2SuperblockV2 {
				t.Fatalf("jbd2 blocktype %d, want %d", got, jbd2SuperblockV2)
			}
			if got := binary.BigEndian.Uint32(jsb[16:]); got != n {
				t.Fatalf("jbd2 maxlen %d, want %d", got, n)
			}
			if got := binary.BigEndian.Uint32(jsb[28:]); got != 0 {
				t.Fatalf("jbd2 start %d, want 0 for an empty journal", got)
			}
			fsck(t, img)
		})
	}
}

// A crashed Grow leaves inode 8 populated with has_journal clear; a re-run must refuse, not allocate a second journal (SHARD-382).
func TestGrowRefusesAHalfAddedJournal(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	// A clean grow journals the image; clearing has_journal on disk forges the state a crash leaves: inode 8 populated, the flag not yet written.
	if err := Grow(img, 64*mib); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sb SuperBlock
	if err := readAt(f, superBlockOffset, &sb); err != nil {
		t.Fatal(err)
	}
	sb.FeatureCompat &^= CompatHasJournal
	if err := writeAt(f, superBlockOffset, &sb); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	free := sumFreeBlocks(t, img)

	err = Grow(img, st.Size())
	if err == nil || !strings.Contains(err.Error(), "half-built") {
		t.Fatalf("Grow of a half-built image = %v, want it refused and named half-built", err)
	}
	if got := sumFreeBlocks(t, img); got != free {
		t.Fatalf("free blocks moved from %d to %d; the refused grow claimed a second run", free, got)
	}
}

// sumFreeBlocks totals the free-block count across every group descriptor on disk.
func sumFreeBlocks(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var sb SuperBlock
	if err := readAt(f, superBlockOffset, &sb); err != nil {
		t.Fatal(err)
	}
	groups := (sb.BlocksCountLow-1)/blocksPerGroup + 1
	var total uint32
	for g := range groups {
		var gd GroupDescriptor
		if err := readAt(f, descriptorOffset(g), &gd); err != nil {
			t.Fatal(err)
		}
		total += uint32(gd.FreeBlocksCountLow)
	}
	return total
}

func TestWidenRefusesALayoutPastTheLastGroup(t *testing.T) {
	limit := uint32(MaxDiskSize / BlockSize)
	if _, _, _, err := widen(limit/blocksPerGroup, limit-tableBlocks, limit); err == nil || !strings.Contains(err.Error(), "past the maximum") {
		t.Fatalf("widen with a table at the end = %v, want the limit named", err)
	}
	groups, valid, blocks, err := widen(1, 300, 16384)
	if err != nil || groups != 1 || valid != 300+tableBlocks+2 || blocks != 16384 {
		t.Fatalf("widen of a small image = %d, %d, %d, %v", groups, valid, blocks, err)
	}
}
