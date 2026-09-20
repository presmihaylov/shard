package ext4

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	// inodesPerGroup is mke2fs's default of one inode per 16 KiB of disk, so a 128 MiB group carries 8192.
	inodesPerGroup = blocksPerGroup * BlockSize / 16384
	tableBlocks    = inodesPerGroup * inodeSize / BlockSize

	modeOffset = 0
	modeType   = 0xf000
	modeDir    = 0x4000
)

// reserveInodes widens every group to inodesPerGroup, since tar2ext4 sizes the table to the tar and leaves a small disk no room for new files.
func reserveInodes(f *os.File) error {
	var sb SuperBlock
	if err := readAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("read the superblock: %w", err)
	}
	old := sb.InodesPerGroup
	if old >= inodesPerGroup {
		return nil
	}
	oldGroups := (sb.BlocksCountLow-1)/blocksPerGroup + 1
	oldTable := old * inodeSize / BlockSize
	gds := make([]GroupDescriptor, oldGroups)
	for g := range oldGroups {
		if err := readAt(f, descriptorOffset(g), &gds[g]); err != nil {
			return fmt.Errorf("read descriptor %d: %w", g, err)
		}
	}
	// The writer lays the tables of every group end to end, then the bitmaps, so the inodes number the table as one array.
	tableStart := gds[0].InodeTableLow
	bitmapStart := gds[0].BlockBitmapLow
	for g, gd := range gds {
		if gd.InodeTableLow != tableStart+uint32(g)*oldTable || gd.BlockBitmapLow != bitmapStart+2*uint32(g) || gd.InodeBitmapLow != gd.BlockBitmapLow+1 {
			return fmt.Errorf("group %d is not laid out as this package writes", g)
		}
	}
	if bitmapStart != tableStart+oldGroups*oldTable {
		return fmt.Errorf("the bitmaps do not follow the inode table")
	}
	table := make([]byte, int64(oldGroups)*int64(oldTable)*BlockSize)
	if _, err := f.ReadAt(table, int64(tableStart)*BlockSize); err != nil {
		return fmt.Errorf("read the inode table: %w", err)
	}
	oldBitmaps := make([]byte, int64(oldGroups)*2*BlockSize)
	if _, err := f.ReadAt(oldBitmaps, int64(bitmapStart)*BlockSize); err != nil {
		return fmt.Errorf("read the bitmaps: %w", err)
	}

	// Wider tables push the bitmaps out, which can open a group that needs a table of its own; iterate until the count settles.
	groups, valid, blocks, err := widen(oldGroups, tableStart, sb.BlocksCountLow)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(blocks) * BlockSize); err != nil {
		return fmt.Errorf("extend the image to %d blocks: %w", blocks, err)
	}
	if err := zero(f, int64(tableStart)*BlockSize, int64(valid-tableStart)*BlockSize); err != nil {
		return fmt.Errorf("clear the new table: %w", err)
	}
	if _, err := f.WriteAt(table, int64(tableStart)*BlockSize); err != nil {
		return fmt.Errorf("write the inode table: %w", err)
	}

	inodeUsed := func(ino uint32) bool {
		if ino >= oldGroups*old {
			return false
		}
		bit := ino % old
		return oldBitmaps[(ino/old)*2*BlockSize+BlockSize+bit/8]&(1<<(bit%8)) != 0
	}
	newBitmapStart := tableStart + groups*tableBlocks
	var freeBlocks, freeInodes uint32
	for g := range groups {
		var bitmaps [2 * BlockSize]byte
		if g < oldGroups {
			copy(bitmaps[:BlockSize], oldBitmaps[g*2*BlockSize:])
		}
		start := g * blocksPerGroup
		var used uint32
		for j := range uint32(blocksPerGroup) {
			b := start + j
			if b >= tableStart {
				bitmaps[j/8] &^= 1 << (j % 8)
				if b < valid || b >= blocks {
					bitmaps[j/8] |= 1 << (j % 8)
				}
			}
			if bitmaps[j/8]&(1<<(j%8)) != 0 {
				used++
			}
		}
		var dirs, usedInodes uint16
		for j := range uint32(inodesPerGroup) {
			ino := g*inodesPerGroup + j
			if !inodeUsed(ino) {
				continue
			}
			bitmaps[BlockSize+j/8] |= 1 << (j % 8)
			usedInodes++
			if binary.LittleEndian.Uint16(table[ino*inodeSize+modeOffset:])&modeType == modeDir {
				dirs++
			}
		}
		if _, err := f.WriteAt(bitmaps[:], int64(newBitmapStart+2*g)*BlockSize); err != nil {
			return fmt.Errorf("write the bitmaps of group %d: %w", g, err)
		}
		gd := GroupDescriptor{
			BlockBitmapLow:     newBitmapStart + 2*g,
			InodeBitmapLow:     newBitmapStart + 2*g + 1,
			InodeTableLow:      tableStart + g*tableBlocks,
			FreeBlocksCountLow: uint16(blocksPerGroup - used), //nolint:gosec // at most 32768, the bitmap block
			FreeInodesCountLow: inodesPerGroup - usedInodes,
			UsedDirsCountLow:   dirs,
		}
		if err := writeAt(f, descriptorOffset(g), &gd); err != nil {
			return fmt.Errorf("write descriptor %d: %w", g, err)
		}
		freeBlocks += blocksPerGroup - used
		freeInodes += uint32(gd.FreeInodesCountLow)
	}

	sb.InodesPerGroup = inodesPerGroup
	sb.InodesCount = inodesPerGroup * groups
	sb.FreeInodesCount = freeInodes
	sb.BlocksCountLow = blocks
	sb.FreeBlocksCountLow = freeBlocks
	if err := writeAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("write the superblock: %w", err)
	}

	return nil
}

// widen settles the group count in 64 bits, since the new metadata can carry a 32-bit block number past the last group Grow may reach.
func widen(groups, tableStart, size uint32) (uint32, uint32, uint32, error) {
	const limit = uint64(MaxDiskSize / BlockSize)
	g := uint64(groups)
	for {
		valid := uint64(tableStart) + g*tableBlocks + 2*g
		blocks := max(valid, uint64(size))
		if blocks > limit {
			return 0, 0, 0, fmt.Errorf("the widened image needs %d blocks, past the maximum of %d", blocks, limit)
		}
		next := (blocks-1)/blocksPerGroup + 1
		if next == g {
			return uint32(g), uint32(valid), uint32(blocks), nil //nolint:gosec // all three are under limit
		}
		g = next
	}
}

// zero clears a span through one small buffer, since the new metadata of a large image is a gigabyte.
func zero(f *os.File, off, n int64) error {
	buf := make([]byte, min(n, 1<<20))
	for n > 0 {
		chunk := min(n, int64(len(buf)))
		if _, err := f.WriteAt(buf[:chunk], off); err != nil {
			return err
		}
		off += chunk
		n -= chunk
	}

	return nil
}
