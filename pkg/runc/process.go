package runc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/pkg/pty"
)

// writeProcess spells the whole exec, because only a process file carries the window runc sets before the command starts (SHARD-514).
func writeProcess(path string, opts ExecOptions) error {
	base, err := readProcess(opts.Bundle)
	if err != nil {
		return err
	}

	process, err := execProcess(base, opts)
	if err != nil {
		return err
	}

	if opts.TTY {
		size, err := pty.SizeOf(opts.Stdin)
		if err != nil {
			return fmt.Errorf("read the window of the exec terminal: %w", err)
		}
		// runc leaves a pty it is given no window for at 0x0, so a zero window is one nobody set.
		if size.Rows != 0 && size.Cols != 0 {
			process.ConsoleSize = &specs.Box{Height: uint(size.Rows), Width: uint(size.Cols)}
		}
	}

	blob, err := json.Marshal(process)
	if err != nil {
		return fmt.Errorf("encode the exec process: %w", err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return fmt.Errorf("write the exec process: %w", err)
	}

	return nil
}

// readProcess is config.json's process, which the container's PID 1 started with.
func readProcess(bundle string) (specs.Process, error) {
	path := filepath.Join(bundle, "config.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		return specs.Process{}, fmt.Errorf("read the bundle config: %w", err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(blob, &spec); err != nil {
		return specs.Process{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if spec.Process == nil {
		return specs.Process{}, fmt.Errorf("%s names no process", path)
	}

	return *spec.Process, nil
}

// execProcess is what runc's exec flags would make of config.json's process; the capabilities and the rest stay as create set them.
func execProcess(base specs.Process, opts ExecOptions) (specs.Process, error) {
	process := base
	process.Args = opts.Argv
	process.Env = append(slices.Clone(base.Env), opts.Env...)
	process.Terminal = opts.TTY
	if opts.WorkDir != "" {
		process.Cwd = opts.WorkDir
	}
	if opts.User == "" {
		return process, nil
	}

	uidText, gidText, hasGID := strings.Cut(opts.User, ":")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil {
		return specs.Process{}, fmt.Errorf("read the uid of user %q: %w", opts.User, err)
	}
	process.User.UID = uint32(uid)
	process.User.AdditionalGids = append(slices.Clone(base.User.AdditionalGids), opts.Groups...)
	if !hasGID {
		return process, nil
	}

	gid, err := strconv.ParseUint(gidText, 10, 32)
	if err != nil {
		return specs.Process{}, fmt.Errorf("read the gid of user %q: %w", opts.User, err)
	}
	process.User.GID = uint32(gid)

	return process, nil
}
