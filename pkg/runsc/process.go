package runsc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
)

// writeProcess spells the whole exec, because runsc's exec flags hand every uid config.json's capabilities (SHARD-498).
func writeProcess(path string, opts ExecOptions) error {
	base, err := readProcess(opts.Bundle)
	if err != nil {
		return err
	}

	process, err := execProcess(base, opts)
	if err != nil {
		return err
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

// readProcess is config.json's process, which the sandbox's PID 1 started with.
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

// execProcess is config.json's process with the exec's own; runsc merges nothing into a process file, so Env is the whole env.
func execProcess(base specs.Process, opts ExecOptions) (specs.Process, error) {
	user, err := execUser(base.User, opts)
	if err != nil {
		return specs.Process{}, err
	}

	process := base
	process.Args = opts.Argv
	process.Env = opts.Env
	process.Terminal = opts.TTY
	process.User = user
	process.Capabilities = execCapabilities(base.Capabilities, user.UID)
	if opts.WorkDir != "" {
		process.Cwd = opts.WorkDir
	}

	return process, nil
}

// execUser is what runsc's --user and --additional-gids made of config.json's user: a uid alone keeps its gid.
func execUser(base specs.User, opts ExecOptions) (specs.User, error) {
	if opts.User == "" {
		return base, nil
	}

	uidText, gidText, hasGID := strings.Cut(opts.User, ":")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil {
		return specs.User{}, fmt.Errorf("read the uid of user %q: %w", opts.User, err)
	}
	user := base
	user.UID = uint32(uid)
	user.AdditionalGids = append(slices.Clone(base.AdditionalGids), opts.Groups...)
	if !hasGID {
		return user, nil
	}

	gid, err := strconv.ParseUint(gidText, 10, 32)
	if err != nil {
		return specs.User{}, fmt.Errorf("read the gid of user %q: %w", opts.User, err)
	}
	user.GID = uint32(gid)

	return user, nil
}

// execCapabilities gives a non-root exec what a setuid away from root leaves, the bounding set, and no exec an inheritable set (CVE-2022-24769).
func execCapabilities(base *specs.LinuxCapabilities, uid uint32) *specs.LinuxCapabilities {
	// Never nil: runsc reads a process file with no capabilities as config.json's whole set.
	caps := &specs.LinuxCapabilities{}
	if base == nil {
		return caps
	}

	caps.Bounding = slices.Clone(base.Bounding)
	if uid != 0 {
		return caps
	}

	caps.Effective = slices.Clone(base.Effective)
	caps.Permitted = slices.Clone(base.Permitted)

	return caps
}
