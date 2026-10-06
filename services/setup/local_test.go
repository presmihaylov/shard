package setup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// localHost is a Linux host under a temp root with gVisor's tools, systemd and a release in place, whose commands a test records.
type localHost struct {
	t     *testing.T
	root  string
	rs    *releaseServer
	env   map[string]string
	calls []string
	// fail makes a command whose line starts with a key fail, printing its value.
	fail map[string]string
	// answer makes a command whose line starts with a key succeed, printing its value.
	answer map[string]string
}

func newLocalHost(t *testing.T) *localHost {
	t.Helper()
	swap(t, &rootUID, os.Getuid())
	l := &localHost{t: t, root: t.TempDir(), rs: newReleaseServer(t), env: map[string]string{}, fail: map[string]string{}, answer: map[string]string{}}
	l.rs.add("v0.1.0", false, false, true, map[string]string{"shard-init-linux-amd64": "shard-init v0.1.0"})
	for _, tool := range []string{"/usr/sbin/ip", "/usr/sbin/nft", "/usr/sbin/mkfs.ext4", "/usr/local/bin/runsc", "/usr/bin/systemctl", "/usr/bin/systemd-analyze", "/usr/bin/apt-get", "/usr/bin/sudo"} {
		l.write(tool, "#!/bin/sh\n")
	}
	l.write("/home/u/.local/bin/shard", "shard v0.1.0")
	l.mkdir("/run/systemd/system")

	return l
}

func (l *localHost) host() Host {
	return Host{
		Root: l.root, OS: "linux", Arch: "amd64", Version: "v0.1.0",
		Executable: filepath.Join(l.root, "/home/u/.local/bin/shard"),
		Releases:   l.rs.URL + "/releases", HTTP: l.rs.Client(),
		Env: func(k string) string { return l.env[k] },
		Run: l.run,
	}
}

func (l *localHost) mac() Host {
	h := l.host()
	h.OS, h.Arch, h.Euid = "darwin", "arm64", os.Getuid()
	l.env["USER"] = "u"
	for _, tool := range []string{"/usr/bin/codesign", "/bin/launchctl", "/usr/bin/plutil"} {
		l.write(tool, "#!/bin/sh\n")
	}

	return h
}

// run does the file work for real, minus the chown a test cannot make, and answers every other command with success.
func (l *localHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	l.calls = append(l.calls, line)
	for prefix, out := range l.fail {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), errors.New("exit status 1")
		}
	}
	for prefix, out := range l.answer {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}

	return l.do(ctx, name, args...)
}

func (l *localHost) do(ctx context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "sudo":
		if len(args) > 2 && args[0] == "-n" && args[1] == "--" {
			return l.do(ctx, args[2], args[3:]...)
		}
		return nil, nil
	case "install":
		return exec.CommandContext(ctx, name, withoutOwner(args)...).CombinedOutput()
	case "mkdir", "mv", "rm", "rmdir":
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	case "sw_vers":
		return []byte("14.5\n"), nil
	}
	if strings.HasPrefix(name, l.root) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}

	return nil, nil
}

func withoutOwner(args []string) []string {
	var kept []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" || args[i] == "-g" {
			i++
			continue
		}
		kept = append(kept, args[i])
	}

	return kept
}

func (l *localHost) write(path, body string) {
	l.t.Helper()
	full := filepath.Join(l.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		l.t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
		l.t.Fatalf("write %s: %v", path, err)
	}
}

func (l *localHost) mkdir(path string) {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Join(l.root, path), 0o755); err != nil {
		l.t.Fatalf("mkdir %s: %v", path, err)
	}
}

func (l *localHost) remove(path string) {
	l.t.Helper()
	if err := os.Remove(filepath.Join(l.root, path)); err != nil {
		l.t.Fatalf("remove %s: %v", path, err)
	}
}

func (l *localHost) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(l.root, path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		l.t.Fatalf("stat %s: %v", path, err)
	}

	return err == nil
}

// changed is whether setup ran any command that changes the host.
func (l *localHost) changed() bool {
	return slices.ContainsFunc(l.calls, func(c string) bool {
		return !strings.HasPrefix(c, "sw_vers") && !strings.HasSuffix(c, "--version")
	})
}

// sandbox records one sandbox of provider in the data dir.
func (l *localHost) sandbox(provider string) {
	l.t.Helper()
	repo, err := sandboxstate.New(filepath.Join(l.root, DataDir))
	if err != nil {
		l.t.Fatalf("open the sandbox records: %v", err)
	}
	if _, err := repo.Create(models.Sandbox{Provider: provider, State: models.StateRunning}); err != nil {
		l.t.Fatalf("record a sandbox: %v", err)
	}
}

// pinGVisor serves a gVisor release from the fake server and drops runsc, so setup downloads and installs it.
func (l *localHost) pinGVisor() {
	l.t.Helper()
	files := map[string]string{"runsc": "/usr/local/bin/runsc", "gvisor-bin/gvisor_sentry": "/usr/local/bin/gvisor-bin/gvisor_sentry"}
	archive := tarZstd(l.t, files)
	sum := sha256.Sum256(archive)
	l.rs.files["pins/gvisor.tar.zstd"] = string(archive)
	swap(l.t, gvisorRelease, download{Title: "runsc", URL: l.rs.URL + "/download/pins/gvisor.tar.zstd", SHA256: hex.EncodeToString(sum[:]), Files: files})
	l.remove("/usr/local/bin/runsc")
}

func tarZstd(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := "binary " + name
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		t.Fatalf("close the archive: %v", err)
	}

	return buf.Bytes()
}

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// localUI answers the provider question from a queue, so a test can choose again after a failed preflight.
type localUI struct {
	*fakeUI
	providers []string
	shown     [][]term.Option
}

func (u *localUI) Select(ctx context.Context, q Question, title string, options []term.Option) (int, error) {
	if q == AskProvider {
		u.shown = append(u.shown, options)
		u.selects[q], u.providers = u.providers[0], u.providers[1:]
	}

	return u.fakeUI.Select(ctx, q, title, options)
}

func newLocalUI(startAtBoot string, confirm bool, providers ...string) *localUI {
	return &localUI{
		fakeUI:    &fakeUI{selects: map[Question]string{AskStartAtBoot: startAtBoot}, confirms: map[Question]bool{AskConfirm: confirm}},
		providers: providers,
	}
}

func marks(list *fakeChecklist) string { return strings.Join(list.marks, ", ") }

// stay is the handover of a run with no saved remote and nothing to say about one.
var stay = handover{finish: func(context.Context) ([]string, error) { return nil, nil }}

func finished(t *testing.T, list *fakeChecklist) {
	t.Helper()
	for i := range list.steps {
		if !slices.Contains(list.marks, fmt.Sprintf("done %d", i)) {
			t.Errorf("%s: step %d %q is not done: %s", list.title, i, list.steps[i], marks(list))
		}
	}
}

func TestLocalSetsUpGVisorWithAService(t *testing.T) {
	l := newLocalHost(t)
	l.env["SUDO_USER"] = "u"
	l.pinGVisor()
	ui := newLocalUI("true", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	if want := []Question{AskProvider, AskStartAtBoot, AskConfirm}; !slices.Equal(ui.asked, want) {
		t.Fatalf("asked %v, want %v", ui.asked, want)
	}
	said(t, ui.fakeUI,
		"Provider:          gVisor",
		"Automatic startup: Yes",
		"  Install the tools required by gVisor: runsc.",
		"  Install shard and shard-init in /usr/local/bin.",
		"  Create shard's data directory.",
		"  Configure and start a systemd service.",
		"Administrator access is required.",
		"shard v0.1.0 is set up, and the daemon is running.",
		"Local commands run with sudo, because the API socket belongs to root.",
		"Next steps:",
		"    sudo shard list",
		"    sudo shard create --name demo --memory 512MiB alpine:3.20",
		"    sudo shard exec demo echo hello",
		"    sudo shard remove --force demo",
		"Documentation: https://useshards.com/docs",
	)
	if len(ui.lists) != 2 || ui.lists[0].title != "Checking this machine" || ui.lists[1].title != "Setting up shard" {
		t.Fatalf("checklists %v", ui.lists)
	}
	finished(t, ui.lists[0])
	finished(t, ui.lists[1])
	for _, path := range []string{"/usr/local/bin/runsc", "/usr/local/bin/gvisor-bin/gvisor_sentry", shardBinary, initBinary, DataDir, systemdUnit, ManifestPath} {
		if !l.exists(path) {
			t.Errorf("%s is not installed", path)
		}
	}
	unit, err := os.ReadFile(filepath.Join(l.root, systemdUnit))
	if err != nil || !strings.Contains(string(unit), "ExecStart=/usr/local/bin/shard daemon --provider gvisor") {
		t.Fatalf("the unit names no provider: %v\n%s", err, unit)
	}
	if !slices.Contains(l.calls, "systemctl start shard.service") {
		t.Fatalf("the daemon never started: %q", l.calls)
	}
}

// The review names the saved connection setup removes, and the done text says it is gone before the next steps. (SHARD-741)
func TestLocalNamesTheSavedConnectionItRemoves(t *testing.T) {
	l := newLocalHost(t)
	l.pinGVisor()
	l.env[client.ConfigHomeEnv] = t.TempDir()
	path, err := client.ConfigPath(l.host().Env)
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	saveConnection(t, path, client.Config{Remote: "https://shard.example.com", APIKey: testKey})
	ui := newLocalUI("true", true, GVisor)
	ui.confirms[AskSwitch] = true

	if err := (&Setup{Host: l.host(), UI: ui}).runLocal(t.Context()); err != nil {
		t.Fatalf("runLocal = %v; printed %q", err, ui.printed)
	}

	order := []string{
		"  Configure and start a systemd service.", "  Remove the saved connection to https://shard.example.com.", "Administrator access is required.",
		"shard v0.1.0 is set up, and the daemon is running.", "✓ Connection removed", "From now on, shard commands use the local daemon.", "Next steps:", "Documentation: https://useshards.com/docs",
	}
	at := -1
	for _, line := range order {
		i := slices.Index(ui.printed, line)
		if i <= at {
			t.Fatalf("%q is missing or out of order in %q", line, ui.printed)
		}
		at = i
	}
	if got := savedConnection(t, path); got != (client.Config{}) {
		t.Errorf("the saved connection is still %v", got)
	}
}

func TestLocalManualStartupPrintsTheDaemonCommand(t *testing.T) {
	l := newLocalHost(t)
	l.env["SUDO_USER"] = "u"
	ui := newLocalUI("false", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	said(t, ui.fakeUI, "Automatic startup: No", "  Leave daemon startup under your control.", "shard v0.1.0 is set up.", "  Start the daemon, and run the next steps in another terminal:")
	daemon, list := slices.Index(ui.printed, "    sudo shard daemon --provider gvisor"), slices.Index(ui.printed, "    sudo shard list")
	if daemon < 0 || list < daemon {
		t.Fatalf("the daemon command does not come before the next steps: %q", ui.printed)
	}
	if slices.ContainsFunc(ui.printed, func(p string) bool { return strings.Contains(p, "Install the tools") }) {
		t.Fatalf("the review offers tools the host has: %q", ui.printed)
	}
	if len(ui.lists[0].steps) != 7 || slices.Contains(ui.lists[0].steps, "Background service support") {
		t.Fatalf("manual startup checks %q", ui.lists[0].steps)
	}
	if slices.ContainsFunc(l.calls, func(c string) bool { return strings.HasPrefix(c, "systemctl") }) || l.exists(systemdUnit) {
		t.Fatalf("manual startup touched systemd: %q", l.calls)
	}
}

// Root itself runs each printed command bare, so setup run as root names no sudo. (SHARD-727)
func TestLocalAsRootPrintsNoSudo(t *testing.T) {
	l := newLocalHost(t)
	ui := newLocalUI("false", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	said(t, ui.fakeUI, "    shard daemon --provider gvisor", "    shard list", "    shard remove --force demo")
	if slices.ContainsFunc(ui.printed, func(p string) bool { return strings.Contains(p, "sudo") }) {
		t.Fatalf("root is told to use sudo: %q", ui.printed)
	}
}

func TestLocalOnAMacUsesLaunchdAndNoSudo(t *testing.T) {
	l := newLocalHost(t)
	h := l.mac()
	swap(t, &kernelURL, func(string) (string, error) { return l.rs.URL + "/download/v0.1.0/shard-init-linux-amd64", nil })
	ui := newLocalUI("true", true, VZ)

	if err := (&Setup{Host: h, UI: ui}).local(t.Context(), stay); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	said(t, ui.fakeUI, "Provider:          macOS Virtualization", "  Install shard in /usr/local/bin.", "  Configure and start a launchd service.", "shard v0.1.0 is set up, and the daemon is running.",
		"Next steps:", "    shard list", "    shard remove --force demo", "Documentation: https://useshards.com/docs")
	if slices.ContainsFunc(ui.printed, func(p string) bool { return strings.Contains(p, "sudo") }) {
		t.Fatalf("a Mac is told to use sudo: %q", ui.printed)
	}
	plist, err := os.ReadFile(filepath.Join(l.root, launchdPlist))
	if err != nil || !strings.Contains(string(plist), "<string>u</string>") {
		t.Fatalf("the plist names no user: %v\n%s", err, plist)
	}
	if !l.exists(shardBinary) || !l.exists(DataDir) || l.exists(initBinary) {
		t.Fatal("a Mac needs shard and the data dir, and never shard-init, which its daemon embeds")
	}
}

// A supported Mac recommends macOS Virtualization in words, as Linux does its providers. (SHARD-664)
func TestASupportedMacRecommendsVZ(t *testing.T) {
	vz := Providers(t.Context(), newLocalHost(t).mac())[providerIndex(VZ)]
	if !vz.Recommended || !slices.Contains(vz.Lines, "Recommended on an Apple silicon Mac with macOS 14 or later.") {
		t.Errorf("the vz row is %+v, want it recommended in its lines", vz)
	}
}

func TestADeclinedReviewChangesNothing(t *testing.T) {
	l := newLocalHost(t)
	l.pinGVisor()
	ui := newLocalUI("true", false, GVisor)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want ErrDeclined", err)
	}
	if l.changed() || l.exists(shardBinary) || l.exists(DataDir) {
		t.Fatalf("a declined setup changed the host: %q", l.calls)
	}
}

func TestAProviderFailureOffersTheOthers(t *testing.T) {
	l := newLocalHost(t)
	l.sandbox(GVisor)
	ui := newLocalUI("true", false, Runc, GVisor)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want the review after the second choice", err)
	}

	said(t, ui.fakeUI, "No installation changes were made.", "Choose another provider or exit.", "Provider:          gVisor")
	if len(ui.shown) != 2 {
		t.Fatalf("asked for the provider %d times, want 2", len(ui.shown))
	}
	again := ui.shown[1]
	runc := again[providerIndex(Runc)]
	if !slices.Equal(runc.Unavailable, runcOverGVisor("", false)) {
		t.Fatalf("the failed provider shows %+v", runc)
	}
	if last := again[len(again)-1]; last.Name != exitOption {
		t.Fatalf("the second question has no Exit: %+v", last)
	}
	if want := []Question{AskProvider, AskStartAtBoot, AskProvider, AskConfirm}; !slices.Equal(ui.asked, want) {
		t.Fatalf("asked %v, want %v", ui.asked, want)
	}
	if l.changed() {
		t.Fatalf("a failed preflight changed the host: %q", l.calls)
	}
}

// A user's setup gets sudo before the checks when only root reads the records, and reads them as root (SHARD-742).
func TestANonRootSetupReadsTheRecordsAsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	l := newLocalHost(t)
	l.sandbox(GVisor)
	dir := filepath.Join(l.root, DataDir, "sandboxes")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})
	l.fail["sudo -n true"] = "sudo: a password is required"
	l.answer["sudo -n -- sh -c"] = "sb1\x00{\"provider\": \"gvisor\"}\n\x00"
	h := l.host()
	h.Euid = 1000
	ui := newLocalUI("true", false, Runc, GVisor)

	err := (&Setup{Host: h, UI: ui}).local(t.Context(), stay)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want the review after the second choice", err)
	}

	said(t, ui.fakeUI, "Setup needs administrator access. sudo may ask for your password.")
	if runc := ui.shown[1][providerIndex(Runc)]; !slices.Equal(runc.Unavailable, runcOverGVisor("sudo ", false)) {
		t.Fatalf("the failed provider shows %+v", runc)
	}
	reads := []string{"sudo -n true", "sudo -v", "env LC_ALL=C sudo -n -v", "sudo -n -- sh -c"}
	for _, c := range l.calls {
		if !slices.ContainsFunc(reads, func(r string) bool { return strings.HasPrefix(c, r) }) && !strings.HasSuffix(c, "--version") {
			t.Fatalf("a failed preflight ran %q", c)
		}
	}
}

func TestExitAfterAProviderFailureChangesNothing(t *testing.T) {
	l := newLocalHost(t)
	l.sandbox(Runc)
	ui := newLocalUI("true", true, GVisor, exitOption)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want ErrDeclined", err)
	}
	if slices.Contains(ui.asked, AskConfirm) || l.changed() {
		t.Fatalf("exit went on: asked %v, ran %q", ui.asked, l.calls)
	}
}

func TestAHostFailureStopsWithoutAnotherChoice(t *testing.T) {
	l := newLocalHost(t)
	l.remove("/run/systemd/system")
	ui := newLocalUI("true", true, GVisor)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Step != "Background service support" {
		t.Fatalf("local = %v, want a stop at the service check", err)
	}
	said(t, ui.fakeUI, "No installation changes were made.")
	if slices.Contains(ui.printed, "Choose another provider or exit.") || len(ui.shown) != 1 {
		t.Fatalf("a host failure offered another provider: %q", ui.printed)
	}
	want := "fail 7: Setup configures a systemd service, and systemd does not manage this machine. / Run shard setup again and choose No for automatic startup."
	if got := ui.lists[0].marks[len(ui.lists[0].marks)-1]; got != want {
		t.Fatalf("last mark %q, want %q", got, want)
	}
}

func TestEveryProviderUnavailableStillOffersExit(t *testing.T) {
	l := newLocalHost(t)
	h := l.host()
	h.OS = "darwin"
	ui := newLocalUI("true", true, exitOption)

	err := (&Setup{Host: h, UI: ui}).local(t.Context(), stay)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want ErrDeclined", err)
	}
	options := ui.shown[0]
	for _, o := range options[:len(options)-1] {
		if len(o.Unavailable) == 0 {
			t.Errorf("%s is available on an Intel Mac", o.Name)
		}
	}
	if options[len(options)-1].Name != exitOption {
		t.Fatalf("no Exit among %+v", options)
	}
}

func TestAFailedStepSaysWhatStays(t *testing.T) {
	l := newLocalHost(t)
	l.fail["systemctl start"] = "Job for shard.service failed."
	ui := newLocalUI("true", true, GVisor)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context(), stay)
	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Step != "Start the daemon" {
		t.Fatalf("local = %v, want a stop at the start", err)
	}
	said(t, ui.fakeUI, "Setup stopped. Earlier completed steps remain in place.")
	if !l.exists(systemdUnit) {
		t.Fatal("the stop undid the service it had installed")
	}
}

// The templates are copies of the packaged files, so a change to one must reach the other.
func TestServiceTemplatesMatchPackaging(t *testing.T) {
	for file, template := range map[string]string{
		"../../packaging/systemd/shard.service":        systemdUnitTemplate,
		"../../packaging/launchd/shard.daemon.plist":   launchdPlistTemplate,
		"../../packaging/launchd/shard.newsyslog.conf": newsyslogTemplate,
	} {
		packaged, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if string(packaged) != template {
			t.Errorf("%s differs from its template in apply_local.go", file)
		}
	}
}

// Setup writes the provider into the daemon's command line, so its names must be the ones the daemon takes.
func TestProvidersAreTheDaemons(t *testing.T) {
	var names []string
	for _, p := range providerTexts {
		names = append(names, p.name)
	}
	if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(daemon.Providers))) {
		t.Fatalf("setup offers %q, and the daemon takes %q", names, daemon.Providers)
	}
}

// A failed verify says what the daemon reported, never the sudo command line that asked it.
func TestNotReadyIsTheDaemonsReason(t *testing.T) {
	err := errors.New("sudo -n -- /usr/local/bin/shard --remote  daemon status: exit status 1")
	for out, want := range map[string]string{
		"shard: tasks in backoff: egress-log-tailer\n": "tasks in backoff: egress-log-tailer",
		"":         err.Error(),
		"  \n\n  ": err.Error(),
	} {
		if got := notReady([]byte(out), err); got != want {
			t.Errorf("notReady(%q) = %q, want %q", out, got, want)
		}
	}
}

// A daemon systemd stopped restarting ends the verify at once, with the error it printed on its last start. (SHARD-753)
func TestVerifyStopsWhenTheUnitFails(t *testing.T) {
	wait, poll := verifyWait, verifyPoll
	verifyWait, verifyPoll = time.Hour, time.Millisecond
	t.Cleanup(func() { verifyWait, verifyPoll = wait, poll })

	refusal := "--provider gvisor contradicts the root, which is firecracker's: /var/lib/shard.xfs exists; name firecracker or leave --provider out"
	hint := "Read its log with: sudo journalctl -u shard.service"
	for _, c := range []struct {
		name, journal string
		want          []string
	}{
		{"the daemon's error", "starting\nshard: " + refusal + "\n", []string{"The daemon failed to start: " + refusal + ".", hint}},
		{"no error line", "signal: killed\n", []string{"The daemon failed to start, and systemd stopped restarting it.", hint}},
	} {
		t.Run(c.name, func(t *testing.T) {
			states, asked := []string{"activating", "activating", "failed"}, 0
			h := Host{OS: "linux", Euid: 1000, Env: func(string) string { return "" }, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch call := strings.Join(append([]string{name}, args...), " "); call {
				case "sudo -n -- " + shardBinary + " --remote  daemon status":
					asked++
					return []byte("shard: the daemon is not running\n"), errors.New("exit status 1")
				case "systemctl is-active shard":
					state := states[0]
					states = states[1:]
					return []byte(state + "\n"), errors.New("exit status 3")
				case "systemctl show --property=InvocationID --value shard":
					return []byte("inv-1\n"), nil
				case "sudo -n -- journalctl _SYSTEMD_INVOCATION_ID=inv-1 --output=cat --no-pager":
					return []byte(c.journal), nil
				default:
					t.Errorf("unexpected command %s", call)
					return nil, errors.New("unexpected command")
				}
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			err := verifyDaemon(ctx, h)
			if p, ok := errors.AsType[*Problem](err); !ok || !slices.Equal(p.Lines, c.want) {
				t.Fatalf("verify = %v, want the lines %q", err, c.want)
			}
			if asked != 3 {
				t.Fatalf("daemon status asked %d times, want 3: once per state read", asked)
			}
		})
	}
}

// transportFunc answers a request without a network.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// cutBody hands out a few bytes, then the read error of a connection the network dropped, socket addresses and all.
type cutBody struct{ sent bool }

func (b *cutBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true

		return copy(p, "partial"), nil
	}

	return 0, &net.OpError{
		Op:     "read",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 51234},
		Addr:   &net.TCPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 443},
		Err:    os.NewSyscallError("read", syscall.ETIMEDOUT),
	}
}

func (b *cutBody) Close() error { return nil }

// A download the network cuts says why as a person reads it, never with the socket addresses. (SHARD-673)
func TestACutDownloadNamesTheCause(t *testing.T) {
	h := newLocalHost(t).host()
	h.HTTP = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: &cutBody{}, Request: r}, nil
	})}
	p := &localPlan{h: h, missing: missing{downloads: []*download{{Title: "gVisor", URL: "https://releases.example.com/gvisor.tar.zstd", SHA256: "00"}}}}

	err := p.download(t.Context())

	if got, want := problemLines(err), []string{"Could not download gVisor: connection timed out."}; !slices.Equal(got, want) {
		t.Errorf("the download failed with %q, want %q", got, want)
	}
}

func TestAdminEndsSudosRefusalOnce(t *testing.T) {
	for _, c := range []struct {
		name, sudo string
		tty        bool
		hint       string
	}{
		{name: "a user outside sudoers at a terminal", sudo: "Sorry, user nosudo may not run sudo on box.", tty: true, hint: "Ask an administrator to give your user sudo access, or run shard setup as root."},
		{name: "a wrong password at a terminal", sudo: "sudo: 3 incorrect password attempts", tty: true, hint: "Run shard setup again and give sudo your password, or run it as root."},
		{name: "no terminal", sudo: "sudo: a terminal is required to read the password", hint: "Run shard setup in a terminal where sudo can ask for your password, or as root."},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLocalHost(t)
			l.fail["sudo -n true"] = "sudo: a password is required\n"
			l.fail["sudo -v"] = c.sudo + "\n"
			if c.tty {
				l.write("/dev/tty", "")
			}
			h := l.host()
			h.Euid = 1000

			err := (&Setup{Host: h, UI: &fakeUI{}}).admin(t.Context())
			var p *Problem
			if !errors.As(err, &p) || !slices.Equal(p.Lines, []string{"Administrator access failed: sudo -v: exit status 1: " + strings.TrimSuffix(c.sudo, ".") + ".", c.hint}) {
				t.Fatalf("admin = %v, want the refusal with one period and %q", err, c.hint)
			}
		})
	}
}

// A repair ends on a sentence, as setup and the upgrade do, not on its last ✓ line. (SHARD-735)
func TestRepairSaysItIsDone(t *testing.T) {
	for _, c := range []struct{ startAtBoot, want string }{
		{"true", "shard v0.1.0 is repaired, and the daemon is running."},
		{"false", "shard v0.1.0 is repaired."},
	} {
		t.Run(c.startAtBoot, func(t *testing.T) {
			l := newLocalHost(t)
			h := l.host()
			if err := (&Setup{Host: h, UI: newLocalUI(c.startAtBoot, true, GVisor)}).local(t.Context(), stay); err != nil {
				t.Fatalf("local = %v", err)
			}
			m, _, err := LoadManifest(h)
			if err != nil {
				t.Fatal(err)
			}
			l.remove("/usr/local/bin/shard-init")
			ui := newLocalUI(c.startAtBoot, true)

			if err := (&Setup{Host: h, UI: ui}).repair(t.Context(), m, ServiceActive, ""); err != nil {
				t.Fatalf("repair = %v; printed %q", err, ui.printed)
			}
			if last := ui.printed[len(ui.printed)-1]; last != c.want {
				t.Fatalf("repair ends on %q, want %q", last, c.want)
			}
		})
	}
}
