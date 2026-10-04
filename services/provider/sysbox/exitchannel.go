package sysbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/memfd"
	"github.com/presmihaylov/shard/services/bundle"
)

// pageReads bounds the tries for two equal reads of the page, which a guest write between them makes differ.
const pageReads = 3

// exitChannel is one sandbox's sealed page. opened says create or a reopen settled it; file is nil when nothing is left to read.
type exitChannel struct {
	mu     sync.Mutex
	opened bool
	file   *os.File
	last   *models.ExitStatus
}

// exitChannels is every channel this daemon holds, by sandbox id.
type exitChannels struct {
	mu   sync.Mutex
	byID map[string]*exitChannel
}

func (c *exitChannels) get(id string) *exitChannel {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.byID == nil {
		c.byID = map[string]*exitChannel{}
	}
	ch, ok := c.byID[id]
	if !ok {
		ch = &exitChannel{}
		c.byID[id] = ch
	}

	return ch
}

func (c *exitChannels) put(id string, f *os.File) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.byID == nil {
		c.byID = map[string]*exitChannel{}
	}
	c.byID[id] = &exitChannel{opened: true, file: f}
}

// drop closes the channel of a sandbox that is gone or about to be created again.
func (c *exitChannels) drop(id string) error {
	c.mu.Lock()
	ch, ok := c.byID[id]
	delete(c.byID, id)
	c.mu.Unlock()
	if !ok {
		return nil
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()

	f := ch.file
	ch.opened, ch.file = true, nil
	if f == nil {
		return nil
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close the exit channel of sandbox %s: %w", id, err)
	}

	return nil
}

// newExitChannel makes the page create hands PID 1 as fd 0, and records its inode where only the daemon writes.
func newExitChannel(b bundle.Bundle) (*os.File, error) {
	f, err := memfd.Create("shard-exit", models.ExitChannelSize)
	if err != nil {
		return nil, fmt.Errorf("make the exit channel: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat the exit channel: %w", err), f.Close())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.Join(errors.New("the exit channel has no inode"), f.Close())
	}
	if err := b.RecordExitChannel(bundle.ExitChannel{Inode: st.Ino}); err != nil {
		return nil, errors.Join(err, f.Close())
	}

	return f, nil
}

// collect copies a new record from the page into the exit file, which every reader then reads.
func (p *Provider) collect(ctx context.Context, id string, b bundle.Bundle) error {
	ch := p.exits.get(id)
	ch.mu.Lock()
	defer ch.mu.Unlock()

	if !ch.opened {
		f, err := p.reopen(ctx, id, b)
		if err != nil {
			return err
		}
		ch.opened, ch.file = true, f
	}
	if ch.file == nil {
		return nil
	}

	exit, found, err := readPage(ch.file)
	if err != nil {
		return fmt.Errorf("read the exit channel of sandbox %s: %w", id, err)
	}
	if !found || (ch.last != nil && *ch.last == exit) {
		return nil
	}
	if err := bundle.WriteExitStatus(b.ExitFile, exit); err != nil {
		return err
	}
	ch.last = &exit

	return nil
}

// reopen finds the page again after a daemon restart, through PID 1's fd 0, which guest root can replace.
// If PID 1 died while the daemon was down, the record it left there is lost with it.
func (p *Provider) reopen(ctx context.Context, id string, b bundle.Bundle) (*os.File, error) {
	want, recorded, err := b.ExitChannel()
	if err != nil {
		return nil, err
	}
	// A sandbox created before the sealed channel reports into the exit file itself, under its 4 KiB bound.
	if !recorded {
		return nil, nil
	}

	pid, err := p.initPid(ctx, id)
	if err != nil || pid == 0 {
		return nil, err
	}

	f, err := openPage(filepath.Join(p.procRoot, strconv.Itoa(pid), "fd", "0"), want.Inode)
	if err != nil || f == nil {
		return nil, err
	}

	// The pid may have died and gone to another process between the check and the open.
	again, err := p.initPid(ctx, id)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if again != pid {
		return nil, f.Close()
	}

	return f, nil
}

// initPid is the sandbox's PID 1 on the host, confirmed in its cgroup as SHARD-411 does, or 0 once there is none.
func (p *Provider) initPid(ctx context.Context, id string) (int, error) {
	status, err := p.Status(ctx, id)
	if err != nil {
		return 0, err
	}
	if !status.Alive() || status.PID == 0 {
		return 0, nil
	}

	pids, err := cgroup.Procs(cgroupDir(p.cgroupRoot, id))
	if errors.Is(err, cgroup.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list the processes of sandbox %s: %w", id, err)
	}
	if !slices.Contains(pids, status.PID) {
		return 0, nil
	}

	return status.PID, nil
}

// openPage opens fd 0 of PID 1 only while it is still the page create made. Nothing but a regular file is opened.
func openPage(path string, inode uint64) (*os.File, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat the exit channel: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, replaced("is not a regular file")
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the exit channel: %w", err)
	}
	if err := samePage(f, inode); err != nil {
		return nil, errors.Join(err, f.Close())
	}

	return f, nil
}

// samePage checks the open file, since the guest can swap fd 0 between the stat and the open.
func samePage(f *os.File, inode uint64) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat the open exit channel: %w", err)
	}
	if !info.Mode().IsRegular() {
		return replaced("is not a regular file")
	}
	if info.Size() != models.ExitChannelSize {
		return replaced(fmt.Sprintf("holds %d bytes, not %d", info.Size(), models.ExitChannelSize))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino != inode {
		return replaced("is not the inode create recorded")
	}

	fixed, err := memfd.Fixed(f)
	if err != nil {
		return fmt.Errorf("read the seals of the exit channel: %w", err)
	}
	if !fixed {
		return replaced("does not carry the seals create added")
	}

	return nil
}

func replaced(why string) error {
	return fmt.Errorf("fd 0 of PID 1 %s: %w", why, models.ErrExitChannelReplaced)
}

// readPage reads the exit record off a stable page.
func readPage(f *os.File) (models.ExitStatus, bool, error) {
	page, err := stablePage(f)
	if err != nil {
		return models.ExitStatus{}, false, err
	}
	exit, found := bundle.DecodeExitPage(page)

	return exit, found, nil
}

// refusal reads the not-started record off the page, which the daemon still holds after PID 1 died.
func (p *Provider) refusal(id string) (*models.CommandNotStartedError, error) {
	ch := p.exits.get(id)
	ch.mu.Lock()
	defer ch.mu.Unlock()

	if ch.file == nil {
		return nil, nil
	}
	page, err := stablePage(ch.file)
	if err != nil {
		return nil, fmt.Errorf("read the exit channel of sandbox %s: %w", id, err)
	}

	return bundle.DecodeNotStartedPage(id, page), nil
}

// stablePage takes the page only from two equal reads, because a guest write can tear one, and never reads past it; nil is no stable read.
func stablePage(f *os.File) ([]byte, error) {
	for range pageReads {
		first, err := pageOf(f)
		if err != nil {
			return nil, err
		}
		second, err := pageOf(f)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(first, second) {
			return first, nil
		}
	}

	return nil, nil
}

func pageOf(f *os.File) ([]byte, error) {
	page := make([]byte, models.ExitChannelSize)
	n, err := f.ReadAt(page, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	return page[:n], nil
}
