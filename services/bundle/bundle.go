// Package bundle builds the OCI runtime bundle a gVisor sandbox runs from.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
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

// readyFileName is written once the entrypoint is forked. runsc start unblocks the task and reads
// nothing back, so this file is the only proof the entrypoint ever ran.
const readyFileName = "started"

// Bundle is one sandbox on disk: a bundle directory, and the overlay layers its rootfs is mounted from.
type Bundle struct {
	// Dir holds config.json and the rootfs mount point. It is what runsc is pointed at.
	Dir    string
	RootFS string
	// ShardDir is bind mounted at guestShardDir, and shard-init writes both files below into it.
	ShardDir  string
	ExitFile  string
	ReadyFile string

	// Upper and Work belong to this sandbox alone. The lower layer is passed to Mount.
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

	b, err := newBundle(spec.StateDir)
	if err != nil {
		return Bundle{}, err
	}

	if err := layout(b); err != nil {
		return Bundle{}, err
	}

	if err := writeNetworkFiles(b, spec); err != nil {
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

// Runtime is what the entrypoint runs with. Nothing records it but config.json, so an exec into a
// live sandbox reads it back from there.
type Runtime struct {
	Env     []string
	WorkDir string
	// User is the uid:gid the supervisor drops the entrypoint to, and empty when nobody named one.
	User string
	// Groups is the supplementary set that goes with User, so an exec adopts the same identity.
	Groups []uint32
}

// Runtime reads config.json back, so a second process in the sandbox starts where the entrypoint did.
func (b Bundle) Runtime() (Runtime, error) {
	configPath := filepath.Join(b.Dir, "config.json")

	blob, err := os.ReadFile(configPath)
	if err != nil {
		return Runtime{}, fmt.Errorf("read %s: %w", configPath, err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(blob, &spec); err != nil {
		return Runtime{}, fmt.Errorf("decode %s: %w", configPath, err)
	}

	if spec.Process == nil {
		return Runtime{}, fmt.Errorf("%s names no process, so nothing says what the entrypoint runs with", configPath)
	}

	groups, err := parseGroups(supervisorFlag(spec.Process.Args, "-groups"))
	if err != nil {
		return Runtime{}, fmt.Errorf("read the entrypoint groups back from %s: %w", configPath, err)
	}

	return Runtime{
		Env:     spec.Process.Env,
		WorkDir: spec.Process.Cwd,
		User:    supervisorFlag(spec.Process.Args, "-user"),
		Groups:  groups,
	}, nil
}

// supervisorFlag reads back a flag the supervisor was given. Its own process user is root, so the
// argv is the only record of which identity the entrypoint runs as.
func supervisorFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == "--" {
			return ""
		}
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// Open derives an existing sandbox's paths from its state directory alone, so a later shard process
// can unmount it and read its exit status without the image the bundle was built from.
func Open(stateDir string) (Bundle, error) {
	if stateDir == "" {
		return Bundle{}, errors.New("no state directory: nothing names the bundle")
	}

	return newBundle(stateDir)
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

	return nil
}

// newBundle derives every path this sandbox uses. It touches no disk, so the layout is testable anywhere.
func newBundle(stateDir string) (Bundle, error) {
	shardDir := filepath.Join(stateDir, "shard")
	b := Bundle{
		Dir:       filepath.Join(stateDir, "bundle"),
		RootFS:    filepath.Join(stateDir, "bundle", "rootfs"),
		ShardDir:  shardDir,
		ExitFile:  filepath.Join(shardDir, exitFileName),
		ReadyFile: filepath.Join(shardDir, readyFileName),
		Upper:     filepath.Join(stateDir, "overlay", "upper"),
		Work:      filepath.Join(stateDir, "overlay", "work"),
	}

	// A colon or a comma would be read as a separator in the mount options, and overlayfs has no escape.
	for _, dir := range []string{b.Upper, b.Work} {
		if strings.ContainsAny(dir, ":,") {
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

	return &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path: "rootfs",
			// The overlay upper layer is what makes this writable, and what survives a stop and start.
			Readonly: false,
		},
		Hostname: spec.Name,
		// No User here: PID 1 stays root to write the exit file, and drops only the entrypoint.
		Process: &specs.Process{
			Args: argv,
			Env:  environment(spec.Env),
			Cwd:  firstNonEmpty(spec.WorkDir, "/"),
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
		return nil, errors.New("nothing to run: the spec has no entrypoint and neither does the image")
	}

	argv := []string{
		GuestInitPath,
		"-exit-file", path.Join(guestShardDir, exitFileName),
		"-ready-file", path.Join(guestShardDir, readyFileName),
	}

	// runspec.Resolve already folded the image USER in, so an empty one here means nobody asked for a user.
	if spec.User != "" {
		identity, err := ResolveUser(spec.RootFS, spec.User)
		if err != nil {
			return nil, err
		}

		// The name is resolved on the host, against the image rootfs: the supervisor cannot read a passwd.
		argv = append(argv, "-user", fmt.Sprintf("%d:%d", identity.UID, identity.GID))
		argv = append(argv, "-groups", formatGroups(identity.Groups))
	}

	return append(append(argv, "--"), entrypoint...), nil
}

// environment adds the one default that is runtime policy rather than image data.
func environment(env []string) []string {
	if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "PATH=") }) {
		return env
	}

	return append(slices.Clone(env), defaultPath)
}

// resources bind on gVisor, as a host cgroup and again in the sentry's argv, and Firecracker needs them to boot.
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
