package network

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/pkg/store"
)

const (
	leaseDirPerm  = 0o750
	leaseFilePerm = 0o640
)

// ErrNoFreeAddress is what allocate returns when every address of the subnet is leased. Match it with errors.Is.
var ErrNoFreeAddress = errors.New("no free address left")

// pool hands out one address per sandbox. It takes no lock: the file that holds a lease is created
// with O_EXCL, so the kernel decides who wins, the same way sandboxstate lets mkdir claim an id.
type pool struct {
	dir string
	// subnet bounds the pool, and first is the lowest address it may hand out.
	subnet netip.Prefix
	first  netip.Addr
}

func newPool(dir string, subnet netip.Prefix, first netip.Addr) (*pool, error) {
	if err := os.MkdirAll(dir, leaseDirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}

	return &pool{dir: dir, subnet: subnet, first: first}, nil
}

// allocate returns the address the sandbox already holds, or claims the lowest free one. It reports
// which of the two happened, because only a lease that was already there proves the host side was
// built by an earlier call. A sandbox that stops and starts again keeps its address.
func (p *pool) allocate(id string) (address netip.Addr, held bool, err error) {
	address, held, err = p.find(id)
	if err != nil || held {
		return address, held, err
	}

	for candidate := p.first; p.subnet.Contains(candidate); candidate = candidate.Next() {
		// The last address of the subnet is its broadcast address, so nothing may hold it.
		if !p.subnet.Contains(candidate.Next()) {
			break
		}

		claimed, err := p.claim(candidate, id)
		if err != nil {
			return netip.Addr{}, false, err
		}
		if claimed {
			return candidate, false, nil
		}
	}

	return netip.Addr{}, false, fmt.Errorf("%w in %s", ErrNoFreeAddress, p.subnet)
}

// claim reports whether it took the address. A taken one is not a failure, it is the next candidate.
func (p *pool) claim(address netip.Addr, id string) (bool, error) {
	path := p.path(address)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, leaseFilePerm)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim %s: %w", path, err)
	}

	if err := write(f, id); err != nil {
		return false, errors.Join(err, os.Remove(path))
	}

	if err := store.SyncDir(p.dir); err != nil {
		return false, err
	}

	return true, nil
}

func write(f *os.File, id string) error {
	if _, err := f.WriteString(id + "\n"); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", f.Name(), err), f.Close())
	}

	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", f.Name(), err), f.Close())
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", f.Name(), err)
	}

	return nil
}

// release gives the address back. A sandbox with no lease is already released.
func (p *pool) release(id string) error {
	address, found, err := p.find(id)
	if err != nil || !found {
		return err
	}

	path := p.path(address)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return store.SyncDir(p.dir)
}

// find reports which address a sandbox holds. The lease directory is small: one entry per sandbox.
func (p *pool) find(id string) (netip.Addr, bool, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return netip.Addr{}, false, fmt.Errorf("read %s: %w", p.dir, err)
	}

	for _, entry := range entries {
		address, err := netip.ParseAddr(entry.Name())
		// Anything outside this pool's subnet is not a lease it wrote, which a reconfigured subnet
		// leaves behind. Honouring one would name a host interface after an offset into the wrong pool.
		if err != nil || !p.subnet.Contains(address) {
			continue
		}

		holder, err := p.holder(address)
		if err != nil {
			return netip.Addr{}, false, err
		}

		if holder == id {
			return address, true, nil
		}
	}

	return netip.Addr{}, false, nil
}

// holder is the sandbox a lease belongs to, or the empty string once the lease is gone.
func (p *pool) holder(address netip.Addr) (string, error) {
	path := p.path(address)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return strings.TrimRight(string(data), "\r\n"), nil
}

func (p *pool) path(address netip.Addr) string {
	return filepath.Join(p.dir, address.String())
}
