package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const superBlockOffset = 1024

// Grow extends an unmounted image written by Writer to size bytes, adding empty block groups.
func Grow(path string, size int64) (err error) {
	if size%BlockSize != 0 {
		return fmt.Errorf("ext4: grow %s: size %d is not a multiple of %d", path, size, BlockSize)
	}
	if size > MaxDiskSize {
		return fmt.Errorf("ext4: grow %s: size %d exceeds the maximum of %d", path, size, MaxDiskSize)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("ext4: grow: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("ext4: grow %s: close: %w", path, cerr))
		}
	}()

	var sb SuperBlock
	if err := readAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("ext4: grow %s: read the superblock: %w", path, err)
	}
	if sb.Magic != SuperBlockMagic || sb.LogBlockSize != 2 || sb.BlocksPerGroup != blocksPerGroup {
		return fmt.Errorf("ext4: grow %s: not an image this package wrote", path)
	}
	oldBlocks := sb.BlocksCountLow
	newBlocks := uint32(size / BlockSize)
	if newBlocks < oldBlocks {
		return fmt.Errorf("ext4: grow %s: %d blocks is smaller than the current %d", path, newBlocks, oldBlocks)
	}
	if newBlocks == oldBlocks {
		return nil
	}
	oldGroups := (oldBlocks-1)/blocksPerGroup + 1
	newGroups := (newBlocks-1)/blocksPerGroup + 1
	if (newGroups-1)/groupsPerDescriptorBlock+1 > gdBlocks {
		return fmt.Errorf("ext4: grow %s: %d groups exceed the reserved descriptor table", path, newGroups)
	}
	if sb.InodesPerGroup != inodesPerGroup {
		return fmt.Errorf("ext4: grow %s: %d inodes per group, not the %d this package writes", path, sb.InodesPerGroup, inodesPerGroup)
	}
	tableBlocks := uint32(inodesPerGroup * inodeSize / BlockSize)
	if tail := newBlocks % blocksPerGroup; newGroups > oldGroups && tail != 0 && tail <= 2+tableBlocks {
		return fmt.Errorf("ext4: grow %s: the last group holds %d blocks, under its %d of metadata", path, tail, 2+tableBlocks)
	}
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("ext4: grow %s: %w", path, err)
	}

	// The old last group marked the blocks past the end of the disk as used; give them back.
	var gd GroupDescriptor
	if err := readAt(f, descriptorOffset(oldGroups-1), &gd); err != nil {
		return fmt.Errorf("ext4: grow %s: read descriptor %d: %w", path, oldGroups-1, err)
	}
	freed, err := setTail(f, gd.BlockBitmapLow, oldBlocks%blocksPerGroup, false)
	if err != nil {
		return fmt.Errorf("ext4: grow %s: %w", path, err)
	}
	gd.FreeBlocksCountLow += freed
	if err := writeAt(f, descriptorOffset(oldGroups-1), &gd); err != nil {
		return fmt.Errorf("ext4: grow %s: write descriptor %d: %w", path, oldGroups-1, err)
	}
	sb.FreeBlocksCountLow += uint32(freed)

	// The writer leaves the descriptor blocks it did not fill free; claim the ones the new groups need.
	oldGdBlocks := (oldGroups-1)/groupsPerDescriptorBlock + 1
	newGdBlocks := (newGroups-1)/groupsPerDescriptorBlock + 1
	if newGdBlocks > oldGdBlocks {
		if err := readAt(f, descriptorOffset(0), &gd); err != nil {
			return fmt.Errorf("ext4: grow %s: read descriptor 0: %w", path, err)
		}
		taken, err := claimBlocks(f, gd.BlockBitmapLow, 1+oldGdBlocks, 1+newGdBlocks)
		if err != nil {
			return fmt.Errorf("ext4: grow %s: %w", path, err)
		}
		gd.FreeBlocksCountLow -= taken
		if err := writeAt(f, descriptorOffset(0), &gd); err != nil {
			return fmt.Errorf("ext4: grow %s: write descriptor 0: %w", path, err)
		}
		sb.FreeBlocksCountLow -= uint32(taken)
	}

	// A new group carries its own bitmaps and inode table at its start; the table stays a hole of zeros.
	for g := oldGroups; g < newGroups; g++ {
		start := g * blocksPerGroup
		meta := 2 + tableBlocks
		var bitmaps [2 * BlockSize]byte
		for j := range meta {
			bitmaps[j/8] |= 1 << (j % 8)
		}
		setInodePadding(bitmaps[BlockSize:])
		if _, err := f.WriteAt(bitmaps[:], int64(start)*BlockSize); err != nil {
			return fmt.Errorf("ext4: grow %s: write the bitmaps of group %d: %w", path, g, err)
		}
		gd = GroupDescriptor{
			BlockBitmapLow:     start,
			InodeBitmapLow:     start + 1,
			InodeTableLow:      start + 2,
			FreeBlocksCountLow: uint16(blocksPerGroup - meta),
			FreeInodesCountLow: uint16(inodesPerGroup),
		}
		if err := writeAt(f, descriptorOffset(g), &gd); err != nil {
			return fmt.Errorf("ext4: grow %s: write descriptor %d: %w", path, g, err)
		}
		sb.FreeBlocksCountLow += blocksPerGroup - meta
	}

	// The new last group marks the blocks past the end of the disk as used, the way the writer does.
	if newBlocks%blocksPerGroup != 0 {
		if err := readAt(f, descriptorOffset(newGroups-1), &gd); err != nil {
			return fmt.Errorf("ext4: grow %s: read descriptor %d: %w", path, newGroups-1, err)
		}
		taken, err := setTail(f, gd.BlockBitmapLow, newBlocks%blocksPerGroup, true)
		if err != nil {
			return fmt.Errorf("ext4: grow %s: %w", path, err)
		}
		gd.FreeBlocksCountLow -= taken
		if err := writeAt(f, descriptorOffset(newGroups-1), &gd); err != nil {
			return fmt.Errorf("ext4: grow %s: write descriptor %d: %w", path, newGroups-1, err)
		}
		sb.FreeBlocksCountLow -= uint32(taken)
	}

	sb.BlocksCountLow = newBlocks
	sb.InodesCount = inodesPerGroup * newGroups
	sb.FreeInodesCount += inodesPerGroup * (newGroups - oldGroups)
	if err := writeAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("ext4: grow %s: write the superblock: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("ext4: grow %s: %w", path, err)
	}
	return nil
}

// claimBlocks marks blocks [first, end) of group 0 used and zeroes them; a set bit means a mount used them.
func claimBlocks(f *os.File, bitmapBlock, first, end uint32) (uint16, error) {
	var bitmap [BlockSize]byte
	if _, err := f.ReadAt(bitmap[:], int64(bitmapBlock)*BlockSize); err != nil {
		return 0, fmt.Errorf("read block bitmap %d: %w", bitmapBlock, err)
	}
	for j := first; j < end; j++ {
		if bitmap[j/8]&(1<<(j%8)) != 0 {
			return 0, fmt.Errorf("descriptor block %d is in use: the image was mounted since it was written", j)
		}
		bitmap[j/8] |= 1 << (j % 8)
	}
	if _, err := f.WriteAt(make([]byte, (end-first)*BlockSize), int64(first)*BlockSize); err != nil {
		return 0, fmt.Errorf("zero the descriptor blocks: %w", err)
	}
	if _, err := f.WriteAt(bitmap[:], int64(bitmapBlock)*BlockSize); err != nil {
		return 0, fmt.Errorf("write block bitmap %d: %w", bitmapBlock, err)
	}
	return uint16(end - first), nil
}

func descriptorOffset(group uint32) int64 {
	return BlockSize + int64(group)*groupDescriptorSize
}

// setTail sets or clears every bit from first to the end of the group's block bitmap and returns how many changed.
func setTail(f *os.File, bitmapBlock, first uint32, used bool) (uint16, error) {
	if first == 0 {
		return 0, nil
	}
	var bitmap [BlockSize]byte
	if _, err := f.ReadAt(bitmap[:], int64(bitmapBlock)*BlockSize); err != nil {
		return 0, fmt.Errorf("read block bitmap %d: %w", bitmapBlock, err)
	}
	var changed uint16
	for j := first; j < blocksPerGroup; j++ {
		bit := byte(1 << (j % 8))
		if (bitmap[j/8]&bit != 0) == used {
			continue
		}
		bitmap[j/8] ^= bit
		changed++
	}
	if _, err := f.WriteAt(bitmap[:], int64(bitmapBlock)*BlockSize); err != nil {
		return 0, fmt.Errorf("write block bitmap %d: %w", bitmapBlock, err)
	}
	return changed, nil
}

func readAt(f *os.File, off int64, v any) error {
	b := make([]byte, binary.Size(v))
	if _, err := f.ReadAt(b, off); err != nil {
		return fmt.Errorf("read %d bytes at %d: %w", len(b), off, err)
	}
	if err := binary.Read(bytes.NewReader(b), binary.LittleEndian, v); err != nil {
		return fmt.Errorf("decode %T at %d: %w", v, off, err)
	}

	return nil
}

func writeAt(f *os.File, off int64, v any) error {
	var b bytes.Buffer
	if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
		return fmt.Errorf("encode %T: %w", v, err)
	}
	if _, err := f.WriteAt(b.Bytes(), off); err != nil {
		return fmt.Errorf("write %d bytes at %d: %w", b.Len(), off, err)
	}

	return nil
}
