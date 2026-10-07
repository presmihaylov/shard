// Package bundle builds the OCI runtime bundle a gVisor sandbox runs from.
package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/supervisor"
)

// guestShardDir is the guest mount point of the per-sandbox host directory shard-init writes to.
const guestShardDir = "/.shard"

// GuestInitPath is where the supervisor binary appears inside every sandbox.
const GuestInitPath = guestShardDir + "/init"

const exitFileName = "exit.json"

// exitChannelFileName names the sealed memfd a sysbox create gave PID 1. Only the daemon writes it.
const exitChannelFileName = "exit-channel.json"

// readyFileName is written once shard-init takes process requests: the only proof it came up, as runsc start reads nothing back.
const readyFileName = "started"

// changedFileName marks a config.json written since the substrate last created the container from it.
const changedFileName = "spec-changed"

// Bundle is one sandbox on disk: a bundle directory, and the overlay layers its rootfs is mounted from.
type Bundle struct {
	// Dir holds config.json and the rootfs mount point. It is what runsc is pointed at.
	Dir    string
	RootFS string
	// ShardDir is bind mounted at guestShardDir, and shard-init writes ReadyFile into it.
	ShardDir string
	// ExitFile holds shard-init's process table, off every bind mount, so the guest reaches it only through its fd 0.
	ExitFile string
	// ExitChannelFile names the sealed memfd a sysbox PID 1 holds as fd 0, beside ExitFile for the same reason.
	ExitChannelFile string
	ReadyFile       string
	// ChangedFile sits beside ExitFile, off every bind mount, so the guest cannot clear it.
	ChangedFile string

	// Upper and Work belong to this sandbox alone. The lower layer is passed to Mount.
	Upper string
	Work  string

	// Tmp is bind mounted at /tmp, on the disk, so a guest that fills it hits its own bound and not the host's.
	Tmp string
	// Logs is bind mounted at /.shard/logs and holds each process's log; it sits off ShardDir, so guest root cannot swap it for a link.
	Logs string

	// Disk is where Image, a sparse ext4 file sized to the bound, mounts; Upper, Work, Tmp, Logs and ShardDir live on it, so one bound covers every guest write.
	Disk  string
	Image string

	// Userns is the namespace sysbox-runc chowns Upper into while a container holds it; the zero value is a substrate that never does.
	Userns netns.IDMapping
}

// Service builds bundles. One per shard process, because the supervisor path never changes.
type Service struct {
	// initPath is the host shard-init binary, bind mounted read-only into every sandbox.
	initPath string
	// seccomp and apparmor are the substrate's confinement; gVisor sets neither, because its sentry is the boundary.
	seccomp  func(*specs.Spec) (*specs.LinuxSeccomp, error)
	apparmor string
}

// New takes the host path of the shard-init binary, which is /usr/local/bin/shard-init on the box.
func New(initPath string, opts ...Option) (*Service, error) {
	if initPath == "" {
		return nil, errors.New("no shard-init path: every sandbox needs the supervisor")
	}

	s := &Service{initPath: initPath}
	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// Build lays out the bundle for spec over the image config and writes config.json. It does not mount.
func (s *Service) Build(spec models.SandboxSpec) (Bundle, error) {
	if err := validate(spec); err != nil {
		return Bundle{}, err
	}
	if err := CheckImage(spec.RootFS); err != nil {
		return Bundle{}, fmt.Errorf("sandbox %s: %w", spec.ID, err)
	}

	b, err := newBundle(spec.StateDir)
	if err != nil {
		return Bundle{}, err
	}

	if err := layout(b); err != nil {
		return Bundle{}, err
	}

	// The seed goes first, so the network files and the trust written next are this sandbox's own.
	if err := seed(b, spec.Seed); err != nil {
		return Bundle{}, err
	}

	if err := writeNetworkFiles(b, spec); err != nil {
		return Bundle{}, err
	}

	var runtimeSpec *specs.Spec
	err = withGuest(b, spec, func(guest, name string) error {
		if spec.ProxyCA != nil {
			store, err := trustIn(guest, name, spec.Env, spec.ProxyCA)
			if err != nil {
				return err
			}
			trust, err := plantTrust(b.Upper, store, idShift{})
			if err != nil {
				return err
			}
			spec.Env = runspec.MergeEnv(spec.Env, trust)
		}
		built, err := s.runtimeSpec(spec, b, guest)
		runtimeSpec = built

		return err
	})
	if err != nil {
		return Bundle{}, err
	}

	if err := b.writeSpec(runtimeSpec); err != nil {
		return Bundle{}, err
	}

	return b, nil
}

// Runtime is what every process and exec runs with. Nothing records it but config.json, so an exec into a
// live sandbox reads it back from there.
type Runtime struct {
	// RootFS is the image tree the writable layer stacks over, so a start after a stop mounts it again.
	RootFS    string
	Resources models.Resources
	Env       []string
	WorkDir   string
	// User is the uid:gid a process or an exec drops to by default, and empty when nobody named one.
	User string
	// Groups is the supplementary set that goes with User, so an exec adopts the same identity.
	Groups []uint32
	// NetnsPath is the network namespace the sandbox joined, empty for one the runtime made; a port forward dials in it.
	NetnsPath string
}

// Runtime reads config.json back, so every process in the sandbox starts where the sandbox says.
func (b Bundle) Runtime() (Runtime, error) {
	spec, err := b.readSpec()
	if err != nil {
		return Runtime{}, err
	}

	groups, err := parseGroups(spec.Annotations[groupsAnnotation])
	if err != nil {
		return Runtime{}, fmt.Errorf("read the sandbox groups back from %s: %w", b.configPath(), err)
	}

	resources := resourcesOf(spec.Linux)
	resources.DiskMiB, err = diskOf(spec.Annotations)
	if err != nil {
		return Runtime{}, fmt.Errorf("read the disk bound back from %s: %w", b.configPath(), err)
	}

	return Runtime{
		RootFS:    spec.Annotations[rootfsAnnotation],
		Resources: resources,
		Env:       spec.Process.Env,
		WorkDir:   supervisorFlag(spec.Process.Args, "-workdir"),
		User:      spec.Annotations[userAnnotation],
		Groups:    groups,
		NetnsPath: netnsOf(spec.Linux),
	}, nil
}

// RunOf puts a named process where an exec into this sandbox runs by default.
func (r Runtime) RunOf(spec models.ProcessSpec) supervisor.RunSpec {
	return supervisor.Base{Env: r.Env, WorkDir: r.WorkDir, User: r.User, Groups: r.Groups}.RunOf(spec)
}

func netnsOf(linux *specs.Linux) string {
	if linux == nil {
		return ""
	}
	for _, ns := range linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			return ns.Path
		}
	}

	return ""
}

// CheckImage refuses an image file or tree that left the host, by the sentinel a public route names.
func CheckImage(path string) error {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the image at %s is gone: %w: %w", path, models.ErrImageGone, err)
	}
	if err != nil {
		return fmt.Errorf("stat the image at %s: %w", path, err)
	}

	return nil
}

// supervisorFlag reads back a flag the supervisor was given.
func supervisorFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// Open derives an existing sandbox's paths from its state directory alone, so a later shard process
// can unmount it and read its processes without the image the bundle was built from.
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
	disk := filepath.Join(stateDir, "disk")
	shardDir := filepath.Join(disk, "shard")
	b := Bundle{
		Dir:             filepath.Join(stateDir, "bundle"),
		RootFS:          filepath.Join(stateDir, "bundle", "rootfs"),
		ShardDir:        shardDir,
		ExitFile:        filepath.Join(stateDir, exitFileName),
		ExitChannelFile: filepath.Join(stateDir, exitChannelFileName),
		ReadyFile:       filepath.Join(shardDir, readyFileName),
		ChangedFile:     filepath.Join(stateDir, changedFileName),
		Upper:           filepath.Join(disk, "upper"),
		Work:            filepath.Join(disk, "work"),
		Tmp:             filepath.Join(disk, "tmp"),
		Logs:            filepath.Join(disk, "logs"),
		Disk:            disk,
		Image:           filepath.Join(stateDir, "disk.img"),
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
		// Others may only traverse it, as sysbox looks /.shard/init up as the exec user; the files in it stay 0600.
		{b.ShardDir, 0o751},
		// The mount point of Logs, made here so no runtime makes it through a guest-written /.shard.
		{filepath.Join(b.ShardDir, supervisor.ProcessLogs), 0o700},
		{b.Tmp, 0o777 | os.ModeSticky},
		{b.Logs, 0o700},
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

// withGuest runs fn over the tree a create reads the user and the CA roots from, which a refusal calls name: the image, or what a seed's upper layer shows over it (SHARD-784).
func withGuest(b Bundle, spec models.SandboxSpec, fn func(guest, name string) error) error {
	if spec.Seed == "" {
		return fn(spec.RootFS, "the image rootfs "+spec.RootFS)
	}

	tree, err := layeredTree([]string{b.Upper, spec.RootFS}, GuestPaths(spec.Env, spec.User))
	if err != nil {
		return err
	}

	return errors.Join(fn(tree, fmt.Sprintf("the seed %s over the image rootfs %s", spec.Seed, spec.RootFS)), os.RemoveAll(tree))
}

// runtimeSpec resolves the user in guest, the tree that holds the sandbox's own passwd and group.
func (s *Service) runtimeSpec(spec models.SandboxSpec, b Bundle, guest string) (*specs.Spec, error) {
	annotations := map[string]string{
		// Nothing else records which image tree the overlay stacks over, and a start after a stop needs it.
		rootfsAnnotation: spec.RootFS,
		// The disk is no cgroup resource, so the bound rides here for inspect and fork to read back.
		diskAnnotation: strconv.FormatInt(DiskBound(spec.Resources), 10),
	}
	// runspec.Resolve already folded the image USER in, so an empty one here means nobody asked for a user.
	if spec.User != "" {
		identity, err := ResolveUser(guest, spec.User)
		if err != nil {
			return nil, err
		}
		// PID 1 stays root, so these are the one record of the user every process and exec drops to.
		annotations[userAnnotation] = fmt.Sprintf("%d:%d", identity.UID, identity.GID)
		annotations[groupsAnnotation] = formatGroups(identity.Groups)
	}

	rs := &specs.Spec{
		Version: specs.Version,
		Root: &specs.Root{
			Path: "rootfs",
			// The overlay upper layer is what makes this writable, and what survives a stop and start.
			Readonly: false,
		},
		Hostname: firstNonEmpty(spec.Name, spec.ID),
		// No User here: PID 1 stays root to reap and report its processes, and drops each of them.
		Process: &specs.Process{
			Args: supervisorArgv(spec),
			Env:  Environment(spec.Env),
			// runc makes a missing cwd before it sets the umask, so shard-init makes the work directory instead (SHARD-764).
			Cwd: "/",
			// No Inheritable, as containerd since CVE-2022-24769: a file's inheritable bits then find nothing to raise.
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  defaultCapabilities,
				Effective: defaultCapabilities,
				Permitted: defaultCapabilities,
			},
			// The supervisor must not gain privileges the sandbox did not grant it.
			NoNewPrivileges: true,
			Rlimits: []specs.POSIXRlimit{
				{Type: "RLIMIT_NOFILE", Hard: defaultNoFile, Soft: defaultNoFile},
			},
		},
		Mounts:      mounts(b.ShardDir, b.Logs, b.Tmp, s.initPath, spec.Resources),
		Annotations: annotations,
		Linux: &specs.Linux{
			CgroupsPath:       CgroupsPath(spec.ID),
			Namespaces:        namespaces(spec.Network),
			UIDMappings:       idMappings(spec.Network.Userns),
			GIDMappings:       idMappings(spec.Network.Userns),
			Resources:         resources(spec.Resources),
			MaskedPaths:       maskedPaths,
			ReadonlyPaths:     readonlyPaths,
			RootfsPropagation: "rprivate",
		},
	}
	if features, ok := cpuFeaturesFor(runtime.GOARCH); ok {
		rs.Annotations[cpuFeaturesAnnotation] = features
	}
	rs.Process.ApparmorProfile = s.apparmor
	if s.seccomp == nil {
		return rs, nil
	}

	filter, err := s.seccomp(rs)
	if err != nil {
		return nil, fmt.Errorf("build the seccomp filter: %w", err)
	}
	rs.Linux.Seccomp = filter

	return rs, nil
}

// supervisorArgv makes shard-init PID 1, which runs nothing until the daemon asks it to.
func supervisorArgv(spec models.SandboxSpec) []string {
	argv := []string{GuestInitPath, "-ready-file", path.Join(guestShardDir, readyFileName)}
	if spec.WorkDir != "" {
		argv = append(argv, "-workdir", spec.WorkDir)
	}

	return argv
}

// Environment adds the one default that is runtime policy rather than image data, which every substrate applies.
func Environment(env []string) []string {
	if slices.ContainsFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "PATH=") }) {
		return env
	}

	return append(slices.Clone(env), defaultPath)
}

// resourcesOf reads the bound back out of the spec, which is the inverse of resources.
func resourcesOf(l *specs.Linux) models.Resources {
	var r models.Resources
	if l == nil || l.Resources == nil {
		return r
	}

	if m := l.Resources.Memory; m != nil && m.Limit != nil {
		r.MemoryMiB = *m.Limit / bytesPerMiB
	}
	if c := l.Resources.CPU; c != nil && c.Quota != nil && c.Period != nil && *c.Period > 0 && *c.Quota > 0 {
		// The quota is vcpus times the period, so the vcpu count can never overflow an int here.
		r.VCPUs = int(*c.Quota / int64(*c.Period)) //nolint:gosec
	}

	return r
}

// PidsMax is the pids.max every sandbox cgroup gets, fixed because on Sysbox a guest fork bomb is a host one.
const PidsMax = 4096

// resources bind on gVisor, as a host cgroup and again in the sentry's argv, and Firecracker needs them to boot.
func resources(r models.Resources) *specs.LinuxResources {
	out := &specs.LinuxResources{}

	// The spec carries the bound the operator typed and no headroom, because runsc hands this number to
	// the sentry as its budget and the guest reads it back as MemTotal.
	if limit := MemoryBound(r); limit > 0 {
		out.Memory = &specs.LinuxMemory{Limit: &limit}
	}

	if r.VCPUs > 0 {
		period := uint64(100000)
		quota := int64(r.VCPUs) * int64(period)
		out.CPU = &specs.LinuxCPU{Quota: &quota, Period: &period}
	}

	pids := int64(PidsMax)
	out.Pids = &specs.LinuxPids{Limit: &pids}

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
