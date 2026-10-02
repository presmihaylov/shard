package gvisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/pkg/store"
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

	return p.killNamed(ctx, dir, id, pids)
}

// sweep kills what a bring-up cut short left in the cgroup: runsc never saved that sandbox, so its delete reaches none of it.
func (p *Provider) sweep(ctx context.Context, id string) error {
	dir := cgroupDir(p.cgroupRoot, id)

	pids, err := cgroup.Procs(dir)
	if errors.Is(err, cgroup.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list the processes of sandbox %s: %w", id, err)
	}
	if len(pids) == 0 {
		return nil
	}

	return p.killNamed(ctx, dir, id, pids)
}

// killNamed SIGKILLs the processes in the cgroup that name the sandbox and waits for the cgroup to empty.
func (p *Provider) killNamed(ctx context.Context, dir, id string, pids []int) error {
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
		args, ok, err := p.argv(pid)
		if err != nil {
			return nil, err
		}
		if ok && slices.Contains(args, id) {
			ours = append(ours, pid)
		}
	}

	return ours, nil
}

// argv reads a process's command line; false is a process that went away under the read.
func (p *Provider) argv(pid int) ([]string, bool, error) {
	raw, err := os.ReadFile(filepath.Join(p.procRoot, strconv.Itoa(pid), "cmdline"))
	if vanished(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the command line of process %d: %w", pid, err)
	}

	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), true, nil
}

// lastRestore records the restore a fork or resume launched, so a later daemon can find it even after the runsc on disk changed.
const lastRestore = "restore.json"

// launch is a restore as the kernel publishes it: the binary /proc/<pid>/exe links, and the command line.
type launch struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
}

// restore records the launch before runsc restore runs, because a daemon that dies during it takes what it ran with it.
func (p *Provider) restore(ctx context.Context, id string, opts runsc.RestoreOptions) error {
	dir, err := p.dirs(id)
	if err != nil {
		return err
	}

	data, err := json.Marshal(launch{Executable: p.runsc.Executable(), Args: p.runsc.RestoreArgs(id, opts)})
	if err != nil {
		return fmt.Errorf("encode the restore of sandbox %s: %w", id, err)
	}
	if err := store.WriteFile(filepath.Join(dir, lastRestore), data, 0o600); err != nil {
		return fmt.Errorf("record the restore of sandbox %s: %w", id, err)
	}

	return p.runsc.Restore(ctx, id, opts)
}

// killRestores ends a restore a dropped fork left outside the cgroup, where no sweep looks; this daemon holds the root, so none starts after.
func (p *Provider) killRestores(ctx context.Context, id string) error {
	last, ok, err := p.lastRestore(id)
	if err != nil || !ok {
		return err
	}

	pids, err := p.restores(last)
	if err != nil || len(pids) == 0 {
		return err
	}

	for _, pid := range pids {
		err := p.killPinned(pid, func() (bool, error) { return p.runs(pid, last) })
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill the runsc restore %d of sandbox %s: %w", pid, id, err)
		}
	}

	kctx, cancel := context.WithTimeout(ctx, killGrace)
	defer cancel()

	// A killed process reads an empty command line until it is reaped, so it stops matching at once.
	for len(pids) > 0 {
		select {
		case <-kctx.Done():
			return fmt.Errorf("the runsc restore %v of sandbox %s still runs after SIGKILL: %w", pids, id, kctx.Err())
		case <-time.After(pollInterval):
		}

		if pids, err = p.restores(last); err != nil {
			return err
		}
	}

	return nil
}

// lastRestore reads the restore recorded for the sandbox; false is a sandbox no fork or resume ever brought up.
func (p *Provider) lastRestore(id string) (launch, bool, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return launch{}, false, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, lastRestore))
	if errors.Is(err, fs.ErrNotExist) {
		return launch{}, false, nil
	}
	if err != nil {
		return launch{}, false, fmt.Errorf("read the last restore of sandbox %s: %w", id, err)
	}

	var last launch
	if err := json.Unmarshal(raw, &last); err != nil {
		return launch{}, false, fmt.Errorf("decode the last restore of sandbox %s: %w", id, err)
	}

	return last, true, nil
}

// restores lists the host processes that are the recorded restore.
func (p *Provider) restores(last launch) ([]int, error) {
	entries, err := os.ReadDir(p.procRoot)
	if err != nil {
		return nil, fmt.Errorf("list the host processes: %w", err)
	}

	var pids []int
	for _, e := range entries {
		// /proc holds files that are not processes, such as meminfo and self.
		if strings.Trim(e.Name(), "0123456789") != "" {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			return nil, fmt.Errorf("read the process id %s: %w", e.Name(), err)
		}

		ok, err := p.runs(pid, last)
		if err != nil {
			return nil, err
		}
		if ok {
			pids = append(pids, pid)
		}
	}

	return pids, nil
}

// runs says whether a process is the recorded restore; a kernel thread links no binary, and an exited process none either.
func (p *Provider) runs(pid int, last launch) (bool, error) {
	exe, err := os.Readlink(filepath.Join(p.procRoot, strconv.Itoa(pid), "exe"))
	if vanished(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the binary of process %d: %w", pid, err)
	}

	// Any process may carry a restore's arguments, so the binary decides first; one replaced on disk reads as deleted.
	if exe != last.Executable && exe != last.Executable+" (deleted)" {
		return false, nil
	}

	args, ok, err := p.argv(pid)
	if err != nil || !ok {
		return false, err
	}

	return slices.Equal(args, last.Args), nil
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
