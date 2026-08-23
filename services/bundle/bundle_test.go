package bundle_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/runspec"
)

const guestExitFile = "/.shard/exit.json"

const guestReadyFile = "/.shard/started"

// Build only records the supervisor path, so the tests never need a real binary there.
const supervisorPath = "/usr/local/bin/shard-init"

func TestBuildRunsTheEntrypointUnderTheSupervisor(t *testing.T) {
	_, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "true"}})

	want := []string{bundle.GuestInitPath, "-exit-file", guestExitFile, "-ready-file", guestReadyFile, "--", "/bin/sh", "-c", "true"}
	if !slices.Equal(got.Process.Args, want) {
		t.Errorf("got args %v, want %v", got.Process.Args, want)
	}
}

func TestBuildRefusesAnImageWithNothingToRun(t *testing.T) {
	_, err := newService(t).Build(newSpec(t))
	if err == nil {
		t.Fatal("Build accepted an image with no entrypoint and no cmd")
	}
}

func TestBuildBindsTheSupervisorReadOnly(t *testing.T) {
	b, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	init := mountAt(t, got, bundle.GuestInitPath)
	if init.Source != supervisorPath {
		t.Errorf("got the supervisor from %q, want %q", init.Source, supervisorPath)
	}
	if !slices.Contains(init.Options, "ro") {
		t.Errorf("the supervisor mount is not read-only: %v", init.Options)
	}

	// The bind of the parent must come first, or the runtime has nowhere to put the supervisor.
	shard := mountAt(t, got, "/.shard")
	if indexOfMount(got, "/.shard") > indexOfMount(got, bundle.GuestInitPath) {
		t.Error("/.shard is mounted after /.shard/init")
	}

	if want := filepath.Join(shard.Source, "exit.json"); b.ExitFile != want {
		t.Errorf("got the exit file at %q, want %q", b.ExitFile, want)
	}
	// The handshake lands beside it, and the host reads it to learn the entrypoint ever ran.
	if want := filepath.Join(shard.Source, "started"); b.ReadyFile != want {
		t.Errorf("got the ready file at %q, want %q", b.ReadyFile, want)
	}
}

func TestBuildRootIsWritableAndRelative(t *testing.T) {
	b, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if got.Root.Path != "rootfs" {
		t.Errorf("got root path %q, want rootfs", got.Root.Path)
	}
	// A read-only root is what runsc spec gives you, and it is what makes shard-init fail to record an exit.
	if got.Root.Readonly {
		t.Error("the root is read-only, so the sandbox cannot write its own layer")
	}
	if want := filepath.Join(b.Dir, "rootfs"); b.RootFS != want {
		t.Errorf("got rootfs %q, want %q", b.RootFS, want)
	}
}

func TestBuildCreatesTheOverlayLayers(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	for _, dir := range []string{b.RootFS, b.Upper, b.Work} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	// Overlay takes the mode of the guest root from the upper layer, so a non-root user must traverse it.
	info, err := os.Stat(b.Upper)
	if err != nil {
		t.Fatalf("stat the upper layer: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("got the upper layer mode %o, want 755", info.Mode().Perm())
	}
}

func TestBuildAddsAPathWhenTheImageHasNone(t *testing.T) {
	_, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, Env: []string{"TZ=UTC"}})

	if !slices.ContainsFunc(got.Process.Env, func(e string) bool { return strings.HasPrefix(e, "PATH=/usr/local/sbin:") }) {
		t.Errorf("got env %v, want a default PATH", got.Process.Env)
	}
}

func TestBuildResolvesANamedUserFromTheImage(t *testing.T) {
	_, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, User: "app"})

	if want := "1000:2000"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q", userArg(t, got), want)
	}
}

func TestBuildResolvesAUserAndGroupPair(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "app:staff"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, User: "root"})

	if want := "1000:50"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q", userArg(t, got), want)
	}
}

func TestBuildResolvesNumericIds(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "65534:65534"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if want := "65534:65534"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q", userArg(t, got), want)
	}
}

// A numeric USER must pick up the primary group of its passwd entry, the way runc does.
func TestBuildResolvesANumericUserThroughPasswd(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "1000"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if want := "1000:2000"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q from the passwd entry", userArg(t, got), want)
	}
}

func TestBuildAcceptsANumericUserTheImageDoesNotList(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "4242"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if want := "4242:0"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q", userArg(t, got), want)
	}
}

func TestBuildResolvesRoot(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "root"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if want := "0:0"; userArg(t, got) != want {
		t.Errorf("got -user %q, want %q", userArg(t, got), want)
	}
}

// The supervisor writes the exit file into a root owned directory, so it must never drop its own ids.
func TestBuildLeavesTheSupervisorAsRoot(t *testing.T) {
	_, got := build(t, models.SandboxSpec{User: "app"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if got.Process.User.UID != 0 || got.Process.User.GID != 0 {
		t.Errorf("the OCI process runs as %d:%d, want root", got.Process.User.UID, got.Process.User.GID)
	}
}

func TestBuildPassesNoUserWhenNothingAsksForOne(t *testing.T) {
	_, got := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	for _, flag := range []string{"-user", "-groups"} {
		if slices.Contains(got.Process.Args, flag) {
			t.Errorf("got args %v, want no %s", got.Process.Args, flag)
		}
	}
}

// Dropping to a user means adopting its whole identity, so the image's group file comes along too.
func TestBuildPassesTheSecondaryGroups(t *testing.T) {
	cases := map[string]struct{ user, want string }{
		// The primary gid leads the set, then the groups whose member list names the user.
		"a name":         {user: "app", want: "2000,50,10"},
		"a named group":  {user: "app:staff", want: "50,10"},
		"an unlisted id": {user: "4242", want: "0"},
		"root":           {user: "root", want: "0"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, got := build(t, models.SandboxSpec{User: c.user}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

			if flagArg(t, got, "-groups") != c.want {
				t.Errorf("got -groups %q, want %q", flagArg(t, got, "-groups"), c.want)
			}
		})
	}
}

// userArg reads the ids the supervisor is told to drop its child to.
func userArg(t *testing.T, got specs.Spec) string {
	t.Helper()

	return flagArg(t, got, "-user")
}

func flagArg(t *testing.T, got specs.Spec, flag string) string {
	t.Helper()

	i := slices.Index(got.Process.Args, flag)
	if i < 0 || i+1 >= len(got.Process.Args) {
		t.Fatalf("got args %v, want a %s flag with a value", got.Process.Args, flag)
	}
	// Every flag must precede the separator, or shard-init reads it as part of the entrypoint.
	if separator := slices.Index(got.Process.Args, "--"); separator >= 0 && separator < i {
		t.Errorf("got args %v, want %s before the -- separator", got.Process.Args, flag)
	}

	return got.Process.Args[i+1]
}

func TestBuildRefusesALayerPathOverlayfsCannotParse(t *testing.T) {
	spec := newSpec(t)
	spec.StateDir = filepath.Join(spec.StateDir, "state:dir")
	spec = runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if _, err := newService(t).Build(spec); err == nil {
		t.Fatal("Build accepted a state directory overlayfs would read as two layers")
	}
}

func TestNewRefusesAnEmptySupervisorPath(t *testing.T) {
	if _, err := bundle.New(""); err == nil {
		t.Fatal("New accepted a service with no supervisor")
	}
}

func TestBuildRefusesAUserTheImageDoesNotHave(t *testing.T) {
	spec := newSpec(t)
	spec = runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, User: "ghost"})

	_, err := newService(t).Build(spec)
	if err == nil {
		t.Fatal("Build accepted a user that is not in the image passwd")
	}
}

func TestBuildJoinsTheNetworkNamespace(t *testing.T) {
	spec := models.SandboxSpec{Network: models.NetworkSpec{NetnsPath: "/var/run/netns/shard-1"}}
	_, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	i := slices.IndexFunc(got.Linux.Namespaces, func(n specs.LinuxNamespace) bool { return n.Type == specs.NetworkNamespace })
	if i < 0 {
		t.Fatal("the bundle has no network namespace")
	}
	if got.Linux.Namespaces[i].Path != "/var/run/netns/shard-1" {
		t.Errorf("got netns path %q, want /var/run/netns/shard-1", got.Linux.Namespaces[i].Path)
	}
}

func TestBuildCarriesTheResourceLimits(t *testing.T) {
	spec := models.SandboxSpec{Resources: models.Resources{MemoryMiB: 512, VCPUs: 2}}
	_, got := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if got.Linux.Resources.Memory == nil || *got.Linux.Resources.Memory.Limit != 512*1024*1024 {
		t.Errorf("got memory %v, want 536870912", got.Linux.Resources.Memory)
	}
	if got.Linux.Resources.CPU == nil || *got.Linux.Resources.CPU.Quota != 200000 {
		t.Errorf("got cpu %v, want a quota of 200000", got.Linux.Resources.CPU)
	}
}

func TestBuildIsAValidRuntimeSpec(t *testing.T) {
	_, got := build(t, models.SandboxSpec{ID: "s-1", Name: "web"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if got.Version != specs.Version {
		t.Errorf("got ociVersion %q, want %q", got.Version, specs.Version)
	}
	if got.Root == nil || got.Process == nil || got.Linux == nil {
		t.Fatal("the spec is missing root, process or linux")
	}
	if got.Hostname != "web" {
		t.Errorf("got hostname %q, want web", got.Hostname)
	}
	if got.Process.Cwd != "/" {
		t.Errorf("got cwd %q, want /", got.Process.Cwd)
	}
	// Every destination must be absolute, and the runtime applies them in order.
	for _, m := range got.Mounts {
		if !filepath.IsAbs(m.Destination) {
			t.Errorf("mount destination %q is not absolute", m.Destination)
		}
	}
}

// An exec with no user of its own runs as the entrypoint does, and config.json is the only record of it.
func TestRuntimeReadsBackTheUserTheEntrypointRunsAs(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{User: "app"}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	runtime, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	if runtime.User != "1000:2000" {
		t.Errorf("Runtime reports the user %q, want 1000:2000", runtime.User)
	}
	// An exec into the sandbox adopts the same identity, and the groups are half of what that means.
	if want := []uint32{2000, 50, 10}; !slices.Equal(runtime.Groups, want) {
		t.Errorf("Runtime reports the groups %v, want %v", runtime.Groups, want)
	}
}

// Nobody named a user, so nothing may claim one: an empty answer is what makes the exec run as root.
func TestRuntimeReportsNoUserWhenNobodyNamedOne(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh", "-user", "1000:1000"}})

	runtime, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}

	if runtime.User != "" {
		t.Errorf("Runtime reports the user %q, and the -user in it is the command's own argument", runtime.User)
	}
	if runtime.Groups != nil {
		t.Errorf("Runtime reports the groups %v, want none", runtime.Groups)
	}
}

func TestBuildTwiceLeavesTheSameBundle(t *testing.T) {
	svc := newService(t)
	spec := runspec.Resolve(newSpec(t), models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	first, err := svc.Build(spec)
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	before := readFile(t, filepath.Join(first.Dir, "config.json"))

	if _, err := svc.Build(spec); err != nil {
		t.Fatalf("second Build: %v", err)
	}

	if after := readFile(t, filepath.Join(first.Dir, "config.json")); after != before {
		t.Error("a second Build changed config.json")
	}
}

func build(t *testing.T, spec models.SandboxSpec, cfg models.ImageConfig) (bundle.Bundle, specs.Spec) {
	t.Helper()

	base := newSpec(t)
	if spec.ID == "" {
		spec.ID = base.ID
	}
	if spec.StateDir == "" {
		spec.StateDir = base.StateDir
	}
	if spec.RootFS == "" {
		spec.RootFS = base.RootFS
	}

	b, err := newService(t).Build(runspec.Resolve(spec, cfg))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got specs.Spec
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(b.Dir, "config.json"))), &got); err != nil {
		t.Fatalf("the bundle config.json does not parse as a runtime spec: %v", err)
	}

	return b, got
}

func newService(t *testing.T) *bundle.Service {
	t.Helper()

	svc, err := bundle.New(supervisorPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return svc
}

// newSpec gives a state directory and an image rootfs whose passwd and group name one user.
func newSpec(t *testing.T) models.SandboxSpec {
	t.Helper()

	rootfs := filepath.Join(t.TempDir(), "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create the image rootfs: %v", err)
	}

	write(t, filepath.Join(rootfs, "etc/passwd"), "root:x:0:0:root:/root:/bin/sh\napp:x:1000:2000::/home/app:/bin/sh\n")
	write(t, filepath.Join(rootfs, "etc/group"), "root:x:0:\nstaff:x:50:app\nwheel:x:10:app\n")

	return models.SandboxSpec{ID: "s-test", StateDir: t.TempDir(), RootFS: rootfs}
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(content)
}

func mountAt(t *testing.T, spec specs.Spec, destination string) specs.Mount {
	t.Helper()

	i := indexOfMount(spec, destination)
	if i < 0 {
		t.Fatalf("the bundle has no mount at %s", destination)
	}

	return spec.Mounts[i]
}

func indexOfMount(spec specs.Spec, destination string) int {
	return slices.IndexFunc(spec.Mounts, func(m specs.Mount) bool { return m.Destination == destination })
}

// The guest resolver reads these two files, and neither netstack nor a microVM resolves a name.
func TestBuildWritesTheResolverConfigIntoTheWritableLayer(t *testing.T) {
	spec := models.SandboxSpec{Name: "amber-otter", Network: models.NetworkSpec{
		Address:     netip.MustParsePrefix("10.87.0.2/16"),
		Nameservers: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")},
	}}

	b, _ := build(t, spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if got := readFile(t, filepath.Join(b.Upper, "etc/resolv.conf")); got != "nameserver 1.1.1.1\nnameserver 8.8.8.8\n" {
		t.Errorf("got resolv.conf %q", got)
	}

	hosts := readFile(t, filepath.Join(b.Upper, "etc/hosts"))
	for _, want := range []string{"127.0.0.1\tlocalhost", "10.87.0.2\tamber-otter"} {
		if !strings.Contains(hosts, want) {
			t.Errorf("the hosts file has no %q:\n%s", want, hosts)
		}
	}
}

// A sandbox with no network keeps whatever the image shipped, rather than an empty resolv.conf.
func TestBuildLeavesTheImageResolverConfigAloneWithoutANetwork(t *testing.T) {
	b, _ := build(t, models.SandboxSpec{}, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if _, err := os.Stat(filepath.Join(b.Upper, "etc/resolv.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %v, want no resolv.conf in the writable layer", err)
	}
}
