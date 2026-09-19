package ext4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const mib = 1 << 20

func writeImage(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriter(f)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(w.Create("etc", &File{Mode: S_IFDIR | 0o755}))
	must(w.Create("etc/hostname", &File{Mode: S_IFREG | 0o644, Size: 6, Uid: 1000, Gid: 1000, Xattrs: map[string][]byte{"user.shard": []byte("yes")}}))
	_, err = w.Write([]byte("shard\n"))
	must(err)
	must(w.Create("etc/link", &File{Mode: S_IFLNK, Linkname: "hostname"}))
	must(w.Link("etc/hostname", "etc/hard"))
	must(w.Create("dev", &File{Mode: S_IFDIR | 0o755}))
	must(w.Create("dev/null", &File{Mode: S_IFCHR | 0o666, Devmajor: 1, Devminor: 3}))
	big := make([]byte, 3*mib)
	must(w.Create("big", &File{Mode: S_IFREG | 0o644, Size: int64(len(big))}))
	_, err = w.Write(big)
	must(err)
	must(w.Close())
	must(f.Close())
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

func TestWriterMakesAReadWriteImage(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	writeImage(t, img)
	s := summarize(t, img)
	if s.sb.FeatureRoCompat&RoCompatReadonly != 0 {
		t.Fatal("the image is marked read-only")
	}
	if s.sb.FeatureCompat&CompatHasJournal != 0 {
		t.Fatal("the image has a journal")
	}
	if s.sb.InodesPerGroup != inodesPerGroup {
		t.Fatalf("%d inodes per group", s.sb.InodesPerGroup)
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
		if s.sb.InodesCount != s.groups*inodesPerGroup {
			t.Fatalf("%d inodes for %d groups", s.sb.InodesCount, s.groups)
		}
		if s.free <= before.free {
			t.Fatalf("grow to %d left %d free blocks, before %d", size, s.free, before.free)
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
	if err := Grow(img, st.Size()); err != nil {
		t.Fatalf("grow to the same size: %v", err)
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

func TestStatReadsBackWhatCreateWrote(t *testing.T) {
	img := filepath.Join(t.TempDir(), "rootfs.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := NewWriter(f)
	if err := w.Create("node", &File{Mode: S_IFBLK | 0o600, Devmajor: 8, Devminor: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Stat("node")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != S_IFBLK|0o600 || got.Devmajor != 8 || got.Devminor != 1 {
		t.Fatalf("got %+v", got)
	}
}

// A file past 340 extents spills into a second leaf; the tree must stay contiguous and each leaf must hold its own count.
func TestWriteExtentsSpillsIntoASecondLeaf(t *testing.T) {
	const extentsPerBlock = BlockSize/12 - 1
	const extents = extentsPerBlock + 1
	const blocks = extents * maxBlocksPerExtent
	const startBlock = 1 + gdBlocks

	var sink memFile
	w := NewWriter(&sink)
	w.pos = int64(startBlock+blocks) * BlockSize
	w.dataWritten = int64(blocks) * BlockSize
	node := &inode{}
	if err := w.writeExtents(node); err != nil {
		t.Fatal(err)
	}
	if err := w.bw.Flush(); err != nil {
		t.Fatal(err)
	}

	var root struct {
		Hdr   ExtentHeader
		Nodes [4]ExtentIndexNode
	}
	if err := binary.Read(bytes.NewReader(node.Data), binary.LittleEndian, &root); err != nil {
		t.Fatal(err)
	}
	if root.Hdr.Depth != 1 || root.Hdr.Entries != 2 {
		t.Fatalf("root is depth %d with %d entries, want depth 1 with 2", root.Hdr.Depth, root.Hdr.Entries)
	}
	if node.BlockCount != blocks+2 {
		t.Errorf("block count is %d, want the data plus two leaves, %d", node.BlockCount, blocks+2)
	}

	var next uint32
	for i, idx := range root.Nodes[:2] {
		if idx.Block != next {
			t.Errorf("leaf %d covers from block %d, want %d", i, idx.Block, next)
		}
		var leaf struct {
			Hdr     ExtentHeader
			Extents [extentsPerBlock]ExtentLeafNode
		}
		off := int64(idx.LeafLow-startBlock-blocks) * BlockSize
		if err := binary.Read(bytes.NewReader(sink.data[off:]), binary.LittleEndian, &leaf); err != nil {
			t.Fatal(err)
		}
		want := uint16(min(extents-i*extentsPerBlock, extentsPerBlock))
		if leaf.Hdr.Depth != 0 || leaf.Hdr.Entries != want {
			t.Fatalf("leaf %d is depth %d with %d entries, want depth 0 with %d", i, leaf.Hdr.Depth, leaf.Hdr.Entries, want)
		}
		for _, e := range leaf.Extents[:leaf.Hdr.Entries] {
			if e.Block != next || e.StartLow != startBlock+next || e.Length != maxBlocksPerExtent {
				t.Fatalf("extent at %d starts at %d for %d blocks, want %d at %d for %d", e.Block, e.StartLow, e.Length, next, startBlock+next, maxBlocksPerExtent)
			}
			next += uint32(e.Length)
		}
	}
	if next != blocks {
		t.Errorf("the leaves cover %d blocks, want %d", next, blocks)
	}
}

// memFile is the sink writeExtents lays its leaf blocks into, from offset zero.
type memFile struct {
	data []byte
}

func (m *memFile) Write(b []byte) (int, error) {
	m.data = append(m.data, b...)

	return len(b), nil
}

func (m *memFile) Read([]byte) (int, error) { return 0, io.EOF }

func (m *memFile) Seek(off int64, _ int) (int64, error) { return off, nil }
