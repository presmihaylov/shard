package ext4

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	// journalInode is the inode ext4 reserves for the jbd2 journal.
	journalInode = 8
	// journalMode is the mode jbd2 gives that inode: a regular file, 0600.
	journalMode = 0x8180

	// jbd2Magic heads a jbd2 journal superblock, which, unlike every ext4 structure, is big-endian.
	jbd2Magic = 0xc03b3998
	// jbd2SuperblockV2 is the superblock type mke2fs writes, carrying the uuid and feature words.
	jbd2SuperblockV2 = 4

	// extentsFlag marks an inode whose i_block holds an extent tree.
	extentsFlag = 0x80000
	// extentMagic heads an extent node.
	extentMagic = 0xf30a

	// roCompatMetadataCsum is the feature this package never writes; the journal code refuses an image that carries it, since it computes no checksums.
	roCompatMetadataCsum RoCompatFeature = 0x400
)

// ensureJournal adds a jbd2 journal to an image that has none, so an fsync on a crashing guest commits the dentry, not only the inode and the data (SHARD-382).
// Grow is not crash-atomic and need not be: every caller builds the disk fresh and records the sandbox only after Grow returns, so a half-built image is never consumed.
func ensureJournal(f *os.File, sb *SuperBlock) error {
	if sb.FeatureRoCompat&roCompatMetadataCsum != 0 {
		return fmt.Errorf("the image carries metadata_csum, which this package does not write")
	}
	// A populated inode 8 with has_journal clear is a crashed grow's half-built image; refuse it so a re-run never allocates a second run.
	populated, err := journalInodePopulated(f)
	if err != nil {
		return err
	}
	if populated {
		return fmt.Errorf("inode %d already holds a journal while has_journal is clear: a crashed grow left the image half-built, rebuild it", journalInode)
	}
	n := journalBlocks(sb.BlocksCountLow)
	groups := (sb.BlocksCountLow-1)/blocksPerGroup + 1

	group, start, err := findRun(f, groups, n)
	if err != nil {
		return err
	}
	phys := group*blocksPerGroup + start

	if err := writeJbd2Super(f, phys, n, sb.UUID); err != nil {
		return err
	}
	iblock, err := writeJournalInode(f, sb, phys, n)
	if err != nil {
		return err
	}
	if err := claimRun(f, group, start, n); err != nil {
		return err
	}
	sb.FreeBlocksCountLow -= n

	sb.JournalInum = journalInode
	sb.JournalBackupType = 1
	copy(sb.JournalBlocks[:15], iblock[:])
	sb.JournalBlocks[15] = 0
	sb.JournalBlocks[16] = n * BlockSize

	return nil
}

// journalInodePopulated says whether inode 8 already carries a journal; with has_journal clear that marks an image a crashed grow left half-built.
func journalInodePopulated(f *os.File) (bool, error) {
	var gd GroupDescriptor
	if err := readAt(f, descriptorOffset(0), &gd); err != nil {
		return false, fmt.Errorf("read descriptor 0: %w", err)
	}
	var mode [2]byte
	if _, err := f.ReadAt(mode[:], int64(gd.InodeTableLow)*BlockSize+(journalInode-1)*inodeSize); err != nil {
		return false, fmt.Errorf("read the journal inode mode: %w", err)
	}

	return binary.LittleEndian.Uint16(mode[:]) != 0, nil
}

// journalBlocks picks the journal length from mke2fs's default table on the final block count, capped at 16384 so one extent fits one group.
func journalBlocks(total uint32) uint32 {
	switch {
	case total < 32768:
		return 1024
	case total < 256*1024:
		return 4096
	case total < 512*1024:
		return 8192
	default:
		return 16384
	}
}

// findRun returns the first group and in-group offset with n contiguous free blocks, lowest group first, so the journal never overwrites a used block.
func findRun(f *os.File, groups, n uint32) (uint32, uint32, error) {
	for g := range groups {
		var gd GroupDescriptor
		if err := readAt(f, descriptorOffset(g), &gd); err != nil {
			return 0, 0, fmt.Errorf("read descriptor %d: %w", g, err)
		}
		if uint32(gd.FreeBlocksCountLow) < n {
			continue
		}
		var bitmap [BlockSize]byte
		if _, err := f.ReadAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
			return 0, 0, fmt.Errorf("read block bitmap %d: %w", g, err)
		}
		if start, ok := freeRun(bitmap[:], n); ok {
			return g, start, nil
		}
	}

	return 0, 0, fmt.Errorf("no run of %d contiguous free blocks for the journal; the disk is too small", n)
}

// freeRun returns the offset of the first run of n contiguous free blocks in a group's block bitmap, and whether it found one.
func freeRun(bitmap []byte, n uint32) (uint32, bool) {
	var run uint32
	for j := range uint32(blocksPerGroup) {
		if bitmap[j/8]&(1<<(j%8)) != 0 {
			run = 0
			continue
		}
		run++
		if run == n {
			return j - n + 1, true
		}
	}

	return 0, false
}

// claimRun marks blocks [start, start+n) of the group used and drops its descriptor's free count to match.
func claimRun(f *os.File, group, start, n uint32) error {
	var gd GroupDescriptor
	if err := readAt(f, descriptorOffset(group), &gd); err != nil {
		return fmt.Errorf("read descriptor %d: %w", group, err)
	}
	var bitmap [BlockSize]byte
	if _, err := f.ReadAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
		return fmt.Errorf("read block bitmap %d: %w", group, err)
	}
	for j := start; j < start+n; j++ {
		bitmap[j/8] |= 1 << (j % 8)
	}
	if _, err := f.WriteAt(bitmap[:], int64(gd.BlockBitmapLow)*BlockSize); err != nil {
		return fmt.Errorf("write block bitmap %d: %w", group, err)
	}
	gd.FreeBlocksCountLow -= uint16(n) //nolint:gosec // n is at most 16384
	if err := writeAt(f, descriptorOffset(group), &gd); err != nil {
		return fmt.Errorf("write descriptor %d: %w", group, err)
	}

	return nil
}

// writeJournalInode writes inode 8 as a regular file of one extent that covers the journal, and returns its i_block words for the superblock backup.
func writeJournalInode(f *os.File, sb *SuperBlock, phys, n uint32) ([15]uint32, error) {
	var iblock [15]uint32
	iblock[0] = extentMagic | 1<<16 // eh_magic | eh_entries 1
	iblock[1] = 4                   // eh_max 4, eh_depth 0
	iblock[4] = n                   // ee_len n, ee_start_hi 0
	iblock[5] = phys                // ee_start_lo

	var gd GroupDescriptor
	if err := readAt(f, descriptorOffset(0), &gd); err != nil {
		return iblock, fmt.Errorf("read descriptor 0: %w", err)
	}
	// Copy i_extra_isize from the root inode, so the journal inode matches what the fs expects.
	var extra [2]byte
	if _, err := f.ReadAt(extra[:], int64(gd.InodeTableLow)*BlockSize+inodeSize+0x80); err != nil {
		return iblock, fmt.Errorf("read the root inode extra size: %w", err)
	}

	var inode [inodeSize]byte
	binary.LittleEndian.PutUint16(inode[0x00:], journalMode)
	binary.LittleEndian.PutUint32(inode[0x04:], n*BlockSize) // i_size_lo
	binary.LittleEndian.PutUint16(inode[0x1a:], 1)           // i_links_count
	binary.LittleEndian.PutUint32(inode[0x1c:], n*8)         // i_blocks_lo, in 512-byte units
	binary.LittleEndian.PutUint32(inode[0x20:], extentsFlag) // i_flags
	for i, w := range iblock {
		binary.LittleEndian.PutUint32(inode[0x28+i*4:], w)
	}
	copy(inode[0x80:], extra[:]) // i_extra_isize

	off := int64(gd.InodeTableLow)*BlockSize + (journalInode-1)*inodeSize
	if _, err := f.WriteAt(inode[:], off); err != nil {
		return iblock, fmt.Errorf("write the journal inode: %w", err)
	}

	return iblock, nil
}

// writeJbd2Super lays an empty jbd2 journal superblock at its first block; every field is big-endian, unlike the ext4 structures.
func writeJbd2Super(f *os.File, phys, n uint32, uuid [16]uint8) error {
	var block [BlockSize]byte
	be := binary.BigEndian
	be.PutUint32(block[0:], jbd2Magic)        // h_magic
	be.PutUint32(block[4:], jbd2SuperblockV2) // h_blocktype
	be.PutUint32(block[12:], BlockSize)       // s_blocksize
	be.PutUint32(block[16:], n)               // s_maxlen
	be.PutUint32(block[20:], 1)               // s_first, the block after this superblock
	be.PutUint32(block[24:], 1)               // s_sequence, the first commit id
	// s_start at 28 stays 0: an empty journal needs no recovery.
	copy(block[48:64], uuid[:]) // s_uuid
	be.PutUint32(block[64:], 1) // s_nr_users
	if _, err := f.WriteAt(block[:], int64(phys)*BlockSize); err != nil {
		return fmt.Errorf("write the jbd2 superblock: %w", err)
	}

	return nil
}
