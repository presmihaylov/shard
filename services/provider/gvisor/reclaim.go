package gvisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/cgroup"
)

// Reclaim is the raw kill behind rm --force once runsc stops answering: SIGKILL to the sandbox's own processes, by cgroup and id.
func (p *Provider) Reclaim(ctx context.Context, id string) error {
	dir := cgroupDir(p.cgroupRoot, id)

	pids, err := cgroup.Procs(dir)
	if errors.Is(err, cgroup.ErrNotFound) {
		return fmt.Errorf("sandbox %s has no cgroup left to reclaim from, and runsc still does not answer for it", id)
	}
	if err != nil {
		return fmt.Errorf("list the processes of sandbox %s: %w", id, err)
	}
	if len(pids) == 0 {
		return fmt.Errorf("sandbox %s holds no process to kill, and runsc still does not answer for it", id)
	}

	ours, err := p.named(pids, id)
	if err != nil {
		return err
	}
	if len(ours) == 0 {
		return fmt.Errorf("no process of %v in the cgroup of sandbox %s names it, so none was killed", pids, id)
	}

	for _, pid := range ours {
		// ESRCH is a process that went between the list and the kill, which is the outcome wanted anyway.
		if err := p.killProcess(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill process %d of sandbox %s: %w", pid, id, err)
		}
	}

	return p.awaitEmpty(ctx, dir, id)
}

// named keeps the processes whose command line carries the id as one whole argument, so a sibling sharing a prefix never matches.
func (p *Provider) named(pids []int, id string) ([]int, error) {
	var ours []int
	for _, pid := range pids {
		raw, err := os.ReadFile(filepath.Join(p.procRoot, strconv.Itoa(pid), "cmdline"))
		if vanished(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the command line of process %d: %w", pid, err)
		}
		if slices.Contains(strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), id) {
			ours = append(ours, pid)
		}
	}

	return ours, nil
}

// awaitEmpty proves the kill landed: an exited process leaves its cgroup before it is reaped, so a zombie never holds this up.
func (p *Provider) awaitEmpty(ctx context.Context, dir, id string) error {
	kctx, cancel := context.WithTimeout(ctx, killGrace)
	defer cancel()

	for {
		left, err := cgroup.Procs(dir)
		if errors.Is(err, cgroup.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list the processes of sandbox %s: %w", id, err)
		}
		if len(left) == 0 {
			return nil
		}

		select {
		case <-kctx.Done():
			return fmt.Errorf("sandbox %s still holds processes %v after SIGKILL: %w", id, left, kctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
