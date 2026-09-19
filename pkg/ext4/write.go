package ext4

import (
	"fmt"
	"io"
	"os"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
)

// Write lays a tar stream down as an ext4 image the guest can mount read-write and Grow can extend to MaxDiskSize.
func Write(r io.Reader, f *os.File) error {
	if err := tar2ext4.Convert(r, f, tar2ext4.MaximumDiskSize(MaxDiskSize)); err != nil {
		return err
	}

	return writable(f)
}

// writable undoes what tar2ext4 assumes of a read-only image: the flag that makes the kernel mount it so, and the inode bitmap tails e2fsck expects set.
func writable(f *os.File) error {
	var sb SuperBlock
	if err := readAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("read the superblock: %w", err)
	}
	if sb.Magic != SuperBlockMagic {
		return fmt.Errorf("magic %#x is not ext4", sb.Magic)
	}
	sb.FeatureRoCompat &^= RoCompatReadonly
	if err := writeAt(f, superBlockOffset, &sb); err != nil {
		return fmt.Errorf("write the superblock: %w", err)
	}

	groups := (sb.BlocksCountLow-1)/blocksPerGroup + 1
	for g := range groups {
		var gd GroupDescriptor
		if err := readAt(f, descriptorOffset(g), &gd); err != nil {
			return fmt.Errorf("read descriptor %d: %w", g, err)
		}
		var bitmap [BlockSize]byte
		off := int64(gd.InodeBitmapLow) * BlockSize
		if _, err := f.ReadAt(bitmap[:], off); err != nil {
			return fmt.Errorf("read inode bitmap %d: %w", g, err)
		}
		setInodePadding(bitmap[:], sb.InodesPerGroup)
		if _, err := f.WriteAt(bitmap[:], off); err != nil {
			return fmt.Errorf("write inode bitmap %d: %w", g, err)
		}
	}

	return nil
}

// setInodePadding sets the bits past inodesPerGroup, which e2fsck expects.
func setInodePadding(bitmap []byte, inodesPerGroup uint32) {
	for j := inodesPerGroup; j < BlockSize*8; j++ {
		bitmap[j/8] |= 1 << (j % 8)
	}
}
