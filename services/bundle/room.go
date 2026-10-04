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

// reserved is the bound of each disk a create admitted before its sandbox directory held it, by that directory.
var reserved = map[string]int64{}

// admitDisk runs write, which lays down a disk of bound bytes at dst, only when the disk fits the root beside every disk the other sandboxes there hold.
func admitDisk(dst string, bound int64, write func() error) error {
	admitting.Lock()
	defer admitting.Unlock()

	// Reserve admitted this disk before the record, and every admission since has counted it.
	granted, ok := reserved[filepath.Dir(dst)]
	delete(reserved, filepath.Dir(dst))
	if ok && bound <= granted {
		return write()
	}
	if err := admissible(dst, bound); err != nil {
		return err
	}

	return write()
}

// Reserve admits a disk of bound bytes at dst before its sandbox has a record, and holds the bound until the disk lands there or Release.
func Reserve(dst string, bound int64) error {
	admitting.Lock()
	defer admitting.Unlock()

	if err := admissible(dst, bound); err != nil {
		return err
	}
	reserved[filepath.Dir(dst)] = bound

	return nil
}

// Release gives back what Reserve held for the sandbox directory dir, for a sandbox that goes before its disk lands.
func Release(dir string) {
	admitting.Lock()
	defer admitting.Unlock()

	delete(reserved, dir)
}

// admissible refuses a disk of bound bytes at dst that does not fit the root beside every disk the other sandboxes there hold or reserved.
func admissible(dst string, bound int64) error {
	sandboxes := filepath.Dir(filepath.Dir(dst))
	held, err := heldDisks(sandboxes, filepath.Base(dst), filepath.Dir(dst))
	if err != nil {
		return err
	}
	free, err := freeBytes(sandboxes)
	if err != nil {
		return err
	}

	return fits(bound, held, free)
}

// AdmitCopy is admitDisk for a disk write copies from src: the copy keeps the size of src, which is its bound.
func AdmitCopy(src, dst string, write func() error) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	return admitDisk(dst, st.Size(), write)
}

// ReplaceDisk runs write, which swaps a disk in place for one of the same bound, under the admission lock: an admission inside the swap would miss that bound.
func ReplaceDisk(write func() error) error {
	admitting.Lock()
	defer admitting.Unlock()

	return write()
}

// fits counts every held disk at its full bound, never at what it has written: a fresh clone's blocks are shared, so its allocation undercounts.
func fits(bound, held, free int64) error {
	if bound <= free-diskHeadroom-held {
		return nil
	}

	return &NoRoomError{Bound: bound, Free: free, Held: held}
}

// NoRoomError is a disk refused because the root has no room for it, the one admission failure that is the request's fault.
type NoRoomError struct {
	Bound, Free, Held int64
}

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("a %d MiB disk does not fit on the root: it has %d MiB free, the disks of the other sandboxes on it are bound to %d MiB, and %d MiB stays free for the daemon; set a smaller resources.disk_mib or remove a sandbox",
		e.Bound/bytesPerMiB, e.Free/bytesPerMiB, e.Held/bytesPerMiB, diskHeadroom/bytesPerMiB)
}

func (e *NoRoomError) Public() string { return e.Error() }

// heldDisks sums the bound of the disk named name, or of the one reserved there, in every sandbox directory under sandboxes but self, in any state: a stopped one can start and write.
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
			held += reserved[dir]

			continue
		}
		if err != nil {
			return 0, fmt.Errorf("stat the disk of %s: %w", dir, err)
		}
		held += st.Size()
	}

	return held, nil
}
