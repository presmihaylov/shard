//go:build linux

package memfd

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// seals leave writes in place open and close every change of size, and F_SEAL_SEAL keeps a holder from adding F_SEAL_WRITE.
const seals = unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL

func create(name string, size int64) (*os.File, error) {
	fd, err := unix.MemfdCreate(name, unix.MFD_ALLOW_SEALING|unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)

	if err := f.Truncate(size); err != nil {
		return nil, errors.Join(fmt.Errorf("size the memfd %s to %d bytes: %w", name, size, err), f.Close())
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, errors.Join(fmt.Errorf("seal the memfd %s: %w", name, err), f.Close())
	}

	return f, nil
}

func fixed(f *os.File) (bool, error) {
	got, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	// EINVAL is the kernel's answer for a file that does not take seals at all.
	if errors.Is(err, unix.EINVAL) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the seals of %s: %w", f.Name(), err)
	}

	return got == seals, nil
}
