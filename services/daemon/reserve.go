package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/presmihaylov/shard/pkg/store"
)

// ReserveFile is the space the daemon holds back under the root, for a start step that finds it full (SHARD-351).
const ReserveFile = ".reserve"

// reserveSize is room for a socket, an initrd and the records a start corrects.
const reserveSize = 64 << 20

// reserve is the file a full root gives back once, so a start can still bind the socket that rm needs.
type reserve struct {
	path string
	size int64
	log  *log.Logger
}

func newReserve(root string, logger *log.Logger) reserve {
	return reserve{path: filepath.Join(root, ReserveFile), size: reserveSize, log: logger}
}

// ensure writes the reserve when it is missing and the root has four times its size free; a fuller root starts without one.
func (r reserve) ensure() error {
	info, err := os.Stat(r.path)
	if err == nil && info.Size() == r.size {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat the reserve %s: %w", r.path, err)
	}

	free, err := store.Free(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	if free < 4*r.size {
		r.log.Printf("the root has %d bytes free, under four times the %d byte reserve, so the daemon holds none at %s", free, r.size, r.path)

		return nil
	}

	return store.WriteReserve(r.path, r.size)
}

// retry runs step, and when it finds the root full deletes the reserve and runs it once more at once.
func (r reserve) retry(step string, run func() error) error {
	err := run()
	if !errors.Is(err, syscall.ENOSPC) {
		return err
	}

	dir := filepath.Dir(r.path)
	before, freeErr := store.Free(dir)
	if freeErr != nil {
		return errors.Join(err, freeErr)
	}
	removeErr := os.Remove(r.path)
	if errors.Is(removeErr, fs.ErrNotExist) {
		return err
	}
	if removeErr != nil {
		return errors.Join(err, fmt.Errorf("delete the reserve %s: %w", r.path, removeErr))
	}
	after, freeErr := store.Free(dir)
	if freeErr != nil {
		return errors.Join(err, freeErr)
	}

	// Nothing of ours writes before the retry: a log on the same volume, or a guest disk, could take the space first.
	retryErr := run()

	short := ""
	if after-before < r.size {
		short = fmt.Sprintf(", %d bytes less than the reserve: a local snapshot may still hold its blocks", r.size-(after-before))
	}
	r.log.Printf("%s found the root full, so the daemon deleted %s and tried once more: %d bytes free before, %d after%s", step, r.path, before, after, short)

	if retryErr != nil {
		return fmt.Errorf("%s again, after the daemon deleted %s: %w", step, r.path, retryErr)
	}

	return nil
}
