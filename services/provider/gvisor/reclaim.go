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
	"github.com/presmihaylov/shard/services/bundle"
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

// safeDelete ends a sandbox: it SIGKILLs the sandbox's own cgroup members through a pidfd, removes the now-idle cgroup, then drops runsc's saved state, so a pid runsc stored and the kernel reused is never force-killed and the state outlives a removal that fails (SHARD-440).
func (p *Provider) safeDelete(ctx context.Context, id string) error {
	if err := p.sweep(ctx, id); err != nil {
		return err
	}
	if err := p.removeCgroup(ctx, id); err != nil {
		return err
	}

	return p.runsc.Forget(id)
}

// removeCgroup removes the sandbox's cgroup once it is safe to: the sweep leaves cgroup.procs empty, but a killed task can still hold the cgroup for a moment, so this waits for cgroup.events to report populated 0 and retries a transient EBUSY, all under one kill-grace bound (SHARD-440).
func (p *Provider) removeCgroup(ctx context.Context, id string) error {
	dir := cgroupDir(p.cgroupRoot, id)

	kctx, cancel := context.WithTimeout(ctx, killGrace)
	defer cancel()

	for {
		populated, err := cgroup.Populated(dir)
		if errors.Is(err, cgroup.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the events of the cgroup of sandbox %s: %w", id, err)
		}

		if !populated {
			err := cgroup.Remove(dir)
			if err == nil {
				return nil
			}
			if !errors.Is(err, syscall.EBUSY) {
				return err
			}
		}

		select {
		case <-kctx.Done():
			return fmt.Errorf("the cgroup of sandbox %s stayed busy after SIGKILL: %w", id, kctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// sweep kills the processes the cgroup still holds from a bring-up cut short; runsc may already have saved that sandbox, so safeDelete forgets its state only after this clears the cgroup (SHARD-440).
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

// killNamed SIGKILLs the processes in the cgroup that name the sandbox, and does it again on each round until the cgroup is empty, because a probe exec can join the cgroup after the last read and would otherwise race the caller's remove (SHARD-440).
func (p *Provider) killNamed(ctx context.Context, dir, id string, pids []int) error {
	ours, err := p.named(pids, id)
	if err != nil {
		return err
	}
	if len(ours) == 0 {
		// A sentry mid-exit still holds the cgroup with an empty cmdline, so it names nothing yet leaves within the grace; only a pid that stays past the grace is foreign (SHARD-411).
		if err := p.awaitEmpty(ctx, dir, id); err != nil {
			return fmt.Errorf("no process of %v in the cgroup of sandbox %s names it, so none was killed", pids, id)
		}

		return nil
	}

	kctx, cancel := context.WithTimeout(ctx, killGrace)
	defer cancel()

	for {
		for _, pid := range ours {
			// A pidfd pins the process, then rechecks both its cgroup membership and its id before it signals, so a pid reused between the scan and the kill, inside the cgroup or out, is never hit (SHARD-440); ESRCH is a process already gone, the outcome wanted anyway.
			if err := p.killPinned(pid, func() (bool, error) { return p.own(dir, pid, id) }); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill process %d of sandbox %s: %w", pid, id, err)
			}
		}

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

		// A probe exec may have joined the cgroup since the last read, so kill whatever names the sandbox now.
		if ours, err = p.named(left, id); err != nil {
			return err
		}
	}
}

// own says whether the pinned pid is still the sandbox's: still in its cgroup, and still naming it; the recheck a kill makes after it pins the process, so a reused pid outside the cgroup is never signalled (SHARD-440).
func (p *Provider) own(dir string, pid int, id string) (bool, error) {
	member, err := p.member(dir, pid)
	if err != nil || !member {
		return false, err
	}

	return p.names(pid, id)
}

// member says whether the pid is still a process of the cgroup, read back after the pin because the scan that listed it may be stale.
func (p *Provider) member(dir string, pid int) (bool, error) {
	pids, err := cgroup.Procs(dir)
	if errors.Is(err, cgroup.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list the processes of the cgroup %s: %w", dir, err)
	}

	return slices.Contains(pids, pid), nil
}

// named keeps the processes whose command line carries the id as one whole argument, so a sibling sharing a prefix never matches.
func (p *Provider) named(pids []int, id string) ([]int, error) {
	var ours []int
	for _, pid := range pids {
		ok, err := p.names(pid, id)
		if err != nil {
			return nil, err
		}
		if ok {
			ours = append(ours, pid)
		}
	}

	return ours, nil
}

// names says whether a process still carries the id as one whole argument, the recheck a pinned kill makes after it pins the process and before it signals.
func (p *Provider) names(pid int, id string) (bool, error) {
	args, ok, err := p.argv(pid)
	if err != nil || !ok {
		return false, err
	}

	return slices.Contains(args, id), nil
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
	// unrecorded is a restore a daemon from before restore.json launched: nothing recorded its binary or its checkpoint path.
	unrecorded bool
}

// matches says whether a command line is this launch; without a record only the checkpoint path may differ.
func (l launch) matches(args []string) bool {
	want := l.Args
	if i := slices.Index(want, imagePathFlag); l.unrecorded && i >= 0 && len(args) == len(want) {
		want = slices.Clone(want)
		want[i+1] = args[i+1]
	}

	return slices.Equal(args, want)
}

const imagePathFlag = "--image-path"

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
	last, err := p.lastRestore(id)
	if err != nil {
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

// lastRestore reads the recorded restore; with no restore.json it is an older daemon's, from any checkpoint and any runsc.
func (p *Provider) lastRestore(id string) (launch, error) {
	dir, err := p.dirs(id)
	if err != nil {
		return launch{}, err
	}

	raw, err := os.ReadFile(filepath.Join(dir, lastRestore))
	if errors.Is(err, fs.ErrNotExist) {
		b, err := bundle.Open(dir)
		if err != nil {
			return launch{}, err
		}

		return launch{Args: p.runsc.RestoreArgs(id, runsc.RestoreOptions{Bundle: b.Dir}), unrecorded: true}, nil
	}
	if err != nil {
		return launch{}, fmt.Errorf("read the last restore of sandbox %s: %w", id, err)
	}

	var last launch
	if err := json.Unmarshal(raw, &last); err != nil {
		return launch{}, fmt.Errorf("decode the last restore of sandbox %s: %w", id, err)
	}

	return last, nil
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

// runs says whether a process is the recorded restore.
func (p *Provider) runs(pid int, last launch) (bool, error) {
	ok, err := p.launched(pid, last)
	if err != nil || !ok {
		return false, err
	}

	args, ok, err := p.argv(pid)
	if err != nil || !ok {
		return false, err
	}

	return last.matches(args), nil
}

// launched says whether a process could be the restore, before its arguments are read; a kernel thread links no binary.
func (p *Provider) launched(pid int, last launch) (bool, error) {
	exe, err := os.Readlink(filepath.Join(p.procRoot, strconv.Itoa(pid), "exe"))
	if vanished(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the binary of process %d: %w", pid, err)
	}

	// A deploy may have repointed runsc since an unrecorded restore, so its starter decides: only root can start a root process.
	if last.unrecorded {
		uid, ok, err := p.realUID(pid)

		return ok && uid == os.Getuid(), err
	}

	// Any process may carry a restore's arguments, so the binary decides first; one replaced on disk reads as deleted.
	return exe == last.Executable || exe == last.Executable+" (deleted)", nil
}

// realUID reads the user that started a process; false is a process that went away under the read.
func (p *Provider) realUID(pid int) (int, bool, error) {
	raw, err := os.ReadFile(filepath.Join(p.procRoot, strconv.Itoa(pid), "status"))
	if vanished(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the status of process %d: %w", pid, err)
	}

	for line := range strings.Lines(string(raw)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "Uid:" {
			continue
		}

		uid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false, fmt.Errorf("read the user of process %d: %w", pid, err)
		}

		return uid, true, nil
	}

	return 0, false, fmt.Errorf("the status of process %d names no user", pid)
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
