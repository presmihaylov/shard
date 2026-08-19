// Package bundle builds the OCI runtime bundle a gVisor sandbox runs from.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

// guestShardDir is the guest mount point of the per-sandbox host directory shard-init writes to.
const guestShardDir = "/.shard"

// GuestInitPath is where the supervisor binary appears inside every sandbox.
const GuestInitPath = guestShardDir + "/init"

const exitFileName = "exit.json"

// Bundle is one sandbox on disk: a bundle directory, and the overlay layers its rootfs is mounted from.
type Bundle struct {
	// Dir holds config.json and the rootfs mount point. It is what runsc is pointed at.
	Dir    string
	RootFS string
	// ShardDir is bind mounted at guestShardDir, and ExitFile is what shard-init writes into it.
	ShardDir string
	ExitFile string

	// Lower is the shared read-only image rootfs; Upper and Work belong to this sandbox alone.
	Lower string
	Upper string
	Work  string
}

// Service builds bundles. One per shard process, because the supervisor path never changes.
type Service struct {
	// initPath is the host shard-init binary, bind mounted read-only into every sandbox.
	initPath string
}

// New takes the host path of the shard-init binary, which is /usr/local/bin/shard-init on the box.
func New(initPath string) (*Service, error) {
	if initPath == "" {
		return nil, errors.New("no shard-init path: every sandbox needs the supervisor")
	}

	return &Service{initPath: initPath}, nil
}

// Build lays out the bundle for spec over the image config and writes config.json. It does not mount.
func (s *Service) Build(spec models.SandboxSpec) (Bundle, error) {
	if err := validate(spec); err != nil {
		return Bundle{}, err
	}

	b, err := newBundle(spec.StateDir, spec.RootFS)
	if err != nil {
		return Bundle{}, err
	}

	if err := layout(b); err != nil {
		return Bundle{}, err
	}

	runtimeSpec, err := s.runtimeSpec(spec, b)
	if err != nil {
		return Bundle{}, err
	}

	encoded, err := json.MarshalIndent(runtimeSpec, "", "\t")
	if err != nil {
		return Bundle{}, fmt.Errorf("marshal the runtime spec: %w", err)
	}

	configPath := filepath.Join(b.Dir, "config.json")
	if err := store.WriteFile(configPath, encoded, 0o644); err != nil { // #nosec G306
		return Bundle{}, fmt.Errorf("write %s: %w", configPath, err)
	}

	return b, nil
}

// Open derives an existing sandbox's paths from its state directory alone, so a later shard process
// can unmount it and read its exit status without the image the bundle was built from.
func Open(stateDir string) (Bundle, error) {
	if stateDir == "" {
		return Bundle{}, errors.New("no state directory: nothing names the bundle")
	}

	return newBundle(stateDir, "")
}

func validate(spec models.SandboxSpec) error {
	if spec.ID == "" {
		return errors.New("the sandbox spec has no id")
	}
	if spec.StateDir == "" {
		return errors.New("the sandbox spec has no state directory")
	}
	if spec.RootFS == "" {
		return errors.New("the sandbox spec has no image rootfs")
	}
	// Refuse rather than build a sandbox that silently trusts nothing. The proxy CA lands in chunk 4.
	if len(spec.CACert) > 0 {
		return errors.New("the bundle builder cannot install a CA certificate yet")
	}

	return nil
}

// newBundle derives every path this sandbox uses. It touches no disk, so the layout is testable anywhere.
func newBundle(stateDir, lower string) (Bundle, error) {
	shardDir := filepath.Join(stateDir, "shard")
	b := Bundle{
		Dir:      filepath.Join(stateDir, "bundle"),
		RootFS:   filepath.Join(stateDir, "bundle", "rootfs"),
		ShardDir: shardDir,
		ExitFile: filepath.Join(shardDir, exitFileName),
		Lower:    lower,
		Upper:    filepath.Join(stateDir, "overlay", "upper"),
		Work:     filepath.Join(stateDir, "overlay", "work"),
	}

	// A colon or a comma would be read as a separator in the mount options, and overlayfs has no escape.
	for _, dir := range []string{b.Lower, b.Upper, b.Work} {
		// Lower is empty when Open derives the paths of a bundle that already exists.
		if dir != "" && strings.ContainsAny(dir, ":,") {
			return Bundle{}, fmt.Errorf("the layer path %q contains a character overlayfs uses as a separator", dir)
		}
	}

	return b, nil
}

// layout creates the directories, parent first. Upper carries the mode the guest root shows.
func layout(b Bundle) error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{b.Dir, 0o750},
		{b.RootFS, 0o755},
		// Overlay takes the merged root's mode from the upper layer, not from the mount point.
		{b.Upper, 0o755},
		{b.Work, 0o750},
		{b.ShardDir, 0o750},
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("create %s: %w", dir.path, err)
		}
		// MkdirAll leaves an existing directory alone and a fresh one trimmed by the umask.
		if err := os.Chmod(dir.path, dir.mode); err != nil { // #nosec G302
			return fmt.Errorf("chmod %s: %w", dir.path, err)
		}
	}

	return nil
}

func (s *Service) runtimeSpec(spec models.SandboxSpec, b Bundle) (*specs.Spec, error) {
	argv, err := supervisorArgv(spec)
	if err != nil {
		return nil, err
	}

	user, err := resolveUser(b.Lower, firstNonEmpty(spec.User, spec.ImageConfig.User))
	if err != nil {
		return nil, err
	}

	return &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path: "rootfs",
			// The overlay upper layer is what makes this writable, and what survives a stop and start.
			Readonly: false,
		},
		Hostname: firstNonEmpty(spec.Name, spec.ID),
		Process: &specs.Process{
			Args: argv,
			Env:  environment(spec.Env, spec.ImageConfig.Env),
			Cwd:  firstNonEmpty(spec.WorkDir, spec.ImageConfig.WorkDir, "/"),
			User: user,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    defaultCapabilities,
				Effective:   defaultCapabilities,
				Permitted:   defaultCapabilities,
				Inheritable: defaultCapabilities,
			},
			// The supervisor must not gain privileges the sandbox did not grant it.
			NoNewPrivileges: true,
			Rlimits: []specs.POSIXRlimit{
				{Type: "RLIMIT_NOFILE", Hard: defaultNoFile, Soft: defaultNoFile},
			},
		},
		Mounts: mounts(b.ShardDir, s.initPath),
		Linux: &specs.Linux{
			Namespaces:        namespaces(spec.Network.NetnsPath),
			Resources:         resources(spec.Resources),
			MaskedPaths:       maskedPaths,
			ReadonlyPaths:     readonlyPaths,
			RootfsPropagation: "rprivate",
		},
	}, nil
}

// supervisorArgv is the whole point of this ticket: PID 1 is shard-init, and the entrypoint is its child.
func supervisorArgv(spec models.SandboxSpec) ([]string, error) {
	entrypoint := spec.Entrypoint
	if len(entrypoint) == 0 {
		entrypoint = slices.Concat(spec.ImageConfig.Entrypoint, spec.ImageConfig.Cmd)
	}
	if len(entrypoint) == 0 {
		return nil, errors.New("nothing to run: the spec has no entrypoint and neither does the image")
	}

	argv := []string{GuestInitPath, "-exit-file", path.Join(guestShardDir, exitFileName), "--"}

	return append(argv, entrypoint...), nil
}

// environment keeps the image order so an image that sets a variable twice still resolves the same way.
func environment(overrides map[string]string, imageEnv []string) []string {
	env := make([]string, 0, len(imageEnv)+len(overrides)+1)
	applied := make(map[string]bool, len(overrides))

	for _, entry := range imageEnv {
		key, _, found := strings.Cut(entry, "=")
		// An image entry with no "=" is not an assignment, so no runtime would accept it.
		if !found {
			continue
		}

		value, override := overrides[key]
		if !override {
			env = append(env, entry)

			continue
		}

		env = append(env, key+"="+value)
		applied[key] = true
	}

	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		if applied[key] {
			continue
		}

		env = append(env, key+"="+overrides[key])
	}

	if !slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "PATH=") }) {
		env = append(env, defaultPath)
	}

	return env
}

// resources is advisory here: gVisor may ignore both, and Firecracker needs them to boot at all.
func resources(r models.Resources) *specs.LinuxResources {
	out := &specs.LinuxResources{}

	if r.MemoryMiB > 0 {
		limit := r.MemoryMiB * 1024 * 1024
		out.Memory = &specs.LinuxMemory{Limit: &limit}
	}

	if r.VCPUs > 0 {
		period := uint64(100000)
		quota := int64(r.VCPUs) * int64(period)
		out.CPU = &specs.LinuxCPU{Quota: &quota, Period: &period}
	}

	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
