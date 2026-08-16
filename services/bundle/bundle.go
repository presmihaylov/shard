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
	"github.com/presmihaylov/shard/services/image"
)

// GuestInitPath is where the supervisor binary appears inside every sandbox.
const GuestInitPath = "/.shard/init"

// guestShardDir is the guest mount point of the per-sandbox host directory shard-init writes to.
const guestShardDir = "/.shard"

const exitFileName = "exit.json"

// defaultPath is what the OCI image spec says to use when the image config sets no PATH.
const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Bundle is one sandbox on disk: a bundle directory, and the overlay layers its rootfs is mounted from.
type Bundle struct {
	// Dir holds config.json and the rootfs mount point. It is what runsc is pointed at.
	Dir    string
	RootFS string
	// ExitFile is the host path shard-init writes the entrypoint exit status to.
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
func New(initPath string) *Service {
	return &Service{initPath: initPath}
}

// Build lays out the bundle for spec over the image config and writes config.json. It does not mount.
func (s *Service) Build(spec models.SandboxSpec, cfg image.Config) (Bundle, error) {
	if s.initPath == "" {
		return Bundle{}, errors.New("no shard-init path: every sandbox needs the supervisor")
	}
	if spec.StateDir == "" {
		return Bundle{}, errors.New("the sandbox spec has no state directory")
	}
	if spec.RootFS == "" {
		return Bundle{}, errors.New("the sandbox spec has no image rootfs")
	}
	// Refuse rather than build a sandbox that silently trusts nothing. The proxy CA lands in chunk 4.
	if len(spec.CACert) > 0 {
		return Bundle{}, errors.New("the bundle builder cannot install a CA certificate yet")
	}

	hostShardDir := filepath.Join(spec.StateDir, "shard")
	b := Bundle{
		Dir:      filepath.Join(spec.StateDir, "bundle"),
		RootFS:   filepath.Join(spec.StateDir, "bundle", "rootfs"),
		ExitFile: filepath.Join(hostShardDir, exitFileName),
		Lower:    spec.RootFS,
		Upper:    filepath.Join(spec.StateDir, "overlay", "upper"),
		Work:     filepath.Join(spec.StateDir, "overlay", "work"),
	}

	if err := layout(b, hostShardDir); err != nil {
		return Bundle{}, err
	}

	runtimeSpec, err := s.runtimeSpec(spec, cfg, b, hostShardDir)
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

// layout creates the directories. Upper carries the mode the guest root shows, because overlay takes it from there.
func layout(b Bundle, hostShardDir string) error {
	dirs := map[string]os.FileMode{
		b.Dir:        0o750,
		b.RootFS:     0o755,
		b.Upper:      0o755,
		b.Work:       0o750,
		hostShardDir: 0o750,
	}

	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		if err := os.MkdirAll(dir, dirs[dir]); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		// MkdirAll leaves an existing directory alone and a fresh one trimmed by the umask.
		if err := os.Chmod(dir, dirs[dir]); err != nil { // #nosec G302
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}

	return nil
}

func (s *Service) runtimeSpec(spec models.SandboxSpec, cfg image.Config, b Bundle, hostShardDir string) (*specs.Spec, error) {
	argv, err := supervisorArgv(spec, cfg)
	if err != nil {
		return nil, err
	}

	user, err := resolveUser(b.Lower, firstNonEmpty(spec.User, cfg.User))
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
			Env:  environment(spec.Env, cfg.Env),
			Cwd:  firstNonEmpty(spec.WorkDir, cfg.WorkDir, "/"),
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
				{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
			},
		},
		Mounts: mounts(hostShardDir, s.initPath),
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
func supervisorArgv(spec models.SandboxSpec, cfg image.Config) ([]string, error) {
	entrypoint := spec.Entrypoint
	if len(entrypoint) == 0 {
		entrypoint = slices.Concat(cfg.Entrypoint, cfg.Cmd)
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
	hasPath := false

	for _, entry := range imageEnv {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if key == "PATH" {
			hasPath = true
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
		if key == "PATH" {
			hasPath = true
		}

		env = append(env, key+"="+overrides[key])
	}

	if !hasPath {
		env = append(env, defaultPath)
	}

	return env
}

func mounts(hostShardDir, initPath string) []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		// The host side of this one is where shard-init writes the exit status the provider watches.
		{Destination: guestShardDir, Type: "bind", Source: hostShardDir, Options: []string{"rbind", "rw", "nosuid", "nodev"}},
		// Mounted after its parent, and read-only: the guest may run the supervisor and never replace it.
		{Destination: GuestInitPath, Type: "bind", Source: initPath, Options: []string{"rbind", "ro", "nosuid", "nodev"}},
	}
}

// namespaces gives the sandbox its own everything. An empty netns path means one the runtime creates.
func namespaces(netnsPath string) []specs.LinuxNamespace {
	return []specs.LinuxNamespace{
		{Type: specs.PIDNamespace},
		{Type: specs.IPCNamespace},
		{Type: specs.UTSNamespace},
		{Type: specs.MountNamespace},
		{Type: specs.NetworkNamespace, Path: netnsPath},
	}
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
