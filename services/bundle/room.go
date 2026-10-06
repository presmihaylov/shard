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

// writing is the size of every checkpoint memory AdmitMemory let start and that has not ended, which the free space does not show yet.
var writing int64

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
	held, free, err := room(dst, filepath.Dir(dst))
	if err != nil {
		return err
	}

	return fits(bound, held, free)
}

// room is the free space of the root under disk's sandbox, and what every disk there but self's holds, with every memory write in flight.
func room(disk, self string) (held, free int64, err error) {
	sandboxes := filepath.Dir(filepath.Dir(disk))
	held, err = heldDisks(sandboxes, filepath.Base(disk), self)
	if err != nil {
		return 0, 0, err
	}
	free, err = freeBytes(sandboxes)
	if err != nil {
		return 0, 0, err
	}

	return held + writing, free, nil
}

// AdmitMemory runs write, which lays down a checkpoint memory of size bytes for the sandbox whose disk is disk, only when it fits beside every disk on the root, its own too (SHARD-562).
func AdmitMemory(disk string, size int64, write func() error) error {
	if err := holdMemory(disk, size); err != nil {
		return err
	}
	defer func() {
		admitting.Lock()
		writing -= size
		admitting.Unlock()
	}()

	return write()
}

// holdMemory counts size as written until AdmitMemory's write ends, so a second admission meanwhile does not take the space it is about to fill.
func holdMemory(disk string, size int64) error {
	admitting.Lock()
	defer admitting.Unlock()

	held, free, err := room(disk, "")
	if err != nil {
		return err
	}
	if fits(size, held, free) != nil {
		return &NoRoomError{Bound: size, Free: free, Held: held, Memory: true}
	}
	writing += size

	return nil
}

// AdmitCopy is admitDisk for a disk write copies from src: the copy keeps the size of src, which is its bound.
func AdmitCopy(src, dst string, write func() error) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	return copied(admitDisk(dst, st.Size(), write))
}

// ReserveCopy is Reserve for the disk a later AdmitCopy copies from src, so a copy that cannot fit is refused before the work that stages src.
func ReserveCopy(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	return copied(Reserve(dst, st.Size()))
}

// copied marks a refusal as one of a copy, whose size no flag of the request changes.
func copied(err error) error {
	if room, ok := errors.AsType[*NoRoomError](err); ok {
		room.Copy = true
	}

	return err
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
	// Memory says Bound is a checkpoint's memory, which no smaller --disk makes room for.
	Memory bool
	// Copy says Bound is a copy of a disk, which keeps its size whatever the request names.
	Copy bool
}

func (e *NoRoomError) Error() string {
	if e.Memory {
		return fmt.Sprintf("the %d MiB of sandbox memory to save does not fit on the host disk: it has %d MiB free, the disks of the sandboxes on it are bound to %d MiB, and %d MiB stays free for the daemon; remove a sandbox",
			e.Bound/bytesPerMiB, e.Free/bytesPerMiB, e.Held/bytesPerMiB, diskHeadroom/bytesPerMiB)
	}
	if e.Copy {
		return fmt.Sprintf("a copy of the %d MiB disk does not fit on the host disk: it has %d MiB free, the disks of the other sandboxes on it are bound to %d MiB, and %d MiB stays free for the daemon; remove a sandbox",
			e.Bound/bytesPerMiB, e.Free/bytesPerMiB, e.Held/bytesPerMiB, diskHeadroom/bytesPerMiB)
	}

	return fmt.Sprintf("a %d MiB disk does not fit on the host disk: it has %d MiB free, the disks of the other sandboxes on it are bound to %d MiB, and %d MiB stays free for the daemon; set a smaller resources.disk_mib or remove a sandbox",
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
