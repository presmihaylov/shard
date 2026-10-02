package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// diskHeadroom is the space a new disk leaves free past every bound on the root, for the daemon's own writes; SHARD-351 holds the same back.
const diskHeadroom = 64 * bytesPerMiB

// admitting keeps a second admission from counting the free space the first is about to take (SHARD-393).
var admitting sync.Mutex

// admitDisk runs write, which lays down a disk of bound bytes at dst, only when the disk fits the root beside every disk the other sandboxes there hold.
func admitDisk(dst string, bound int64, write func() error) error {
	admitting.Lock()
	defer admitting.Unlock()

	sandboxes := filepath.Dir(filepath.Dir(dst))
	held, err := heldDisks(sandboxes, filepath.Base(dst), filepath.Dir(dst))
	if err != nil {
		return err
	}
	free, err := freeBytes(sandboxes)
	if err != nil {
		return err
	}
	if err := fits(bound, held, free); err != nil {
		return err
	}

	return write()
}

// AdmitCopy is admitDisk for a disk write copies from src: the copy keeps the size of src, which is its bound.
func AdmitCopy(src, dst string, write func() error) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	return admitDisk(dst, st.Size(), write)
}

// fits counts every held disk at its full bound, never at what it has written: a fresh clone's blocks are shared, so its allocation undercounts.
func fits(bound, held, free int64) error {
	if bound <= free-diskHeadroom-held {
		return nil
	}

	return fmt.Errorf("a %d MiB disk does not fit on the root: it has %d MiB free, the disks of the other sandboxes on it are bound to %d MiB, and %d MiB stays free for the daemon; ask for a smaller --disk or remove a sandbox",
		bound/bytesPerMiB, free/bytesPerMiB, held/bytesPerMiB, diskHeadroom/bytesPerMiB)
}

// heldDisks sums the bound of the disk named name in every sandbox directory under sandboxes but self, in any state: a stopped one can start and write.
func heldDisks(sandboxes, name, self string) (int64, error) {
	entries, err := os.ReadDir(sandboxes)
	if err != nil {
		return 0, fmt.Errorf("list the sandboxes under %s: %w", sandboxes, err)
	}

	var held int64
	for _, entry := range entries {
		dir := filepath.Join(sandboxes, entry.Name())
		if !entry.IsDir() || dir == self {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("stat the disk of %s: %w", dir, err)
		}
		held += st.Size()
	}

	return held, nil
}
