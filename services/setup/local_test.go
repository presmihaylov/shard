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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/term"
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
}

func newLocalHost(t *testing.T) *localHost {
	t.Helper()
	swap(t, &rootUID, os.Getuid())
	l := &localHost{t: t, root: t.TempDir(), rs: newReleaseServer(t), env: map[string]string{}, fail: map[string]string{}}
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
	l.pinGVisor()
	ui := newLocalUI("true", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context()); err != nil {
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
		"  Create Shard's data directory.",
		"  Configure and start a systemd service.",
		"Administrator access is required.",
		"Shard is set up, and the daemon is running.",
		"Local commands run with sudo, for example `sudo shard ls`.",
	)
	if len(ui.lists) != 2 || ui.lists[0].title != "Checking this machine" || ui.lists[1].title != "Setting up Shard" {
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

func TestLocalManualStartupPrintsTheDaemonCommand(t *testing.T) {
	l := newLocalHost(t)
	ui := newLocalUI("false", true, GVisor)

	if err := (&Setup{Host: l.host(), UI: ui}).local(t.Context()); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	said(t, ui.fakeUI, "Automatic startup: No", "  Leave daemon startup under your control.", "Start the daemon with:", "  sudo shard daemon --provider gvisor")
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

func TestLocalOnAMacUsesLaunchdAndNoSudo(t *testing.T) {
	l := newLocalHost(t)
	h := l.mac()
	swap(t, &kernelURL, func(string) (string, error) { return l.rs.URL + "/download/v0.1.0/shard-init-linux-amd64", nil })
	ui := newLocalUI("true", true, VZ)

	if err := (&Setup{Host: h, UI: ui}).local(t.Context()); err != nil {
		t.Fatalf("local = %v; printed %q", err, ui.printed)
	}

	said(t, ui.fakeUI, "Provider:          macOS Virtualization", "  Install shard in /usr/local/bin.", "  Configure and start a launchd service.", "Shard is set up, and the daemon is running.")
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

func TestADeclinedReviewChangesNothing(t *testing.T) {
	l := newLocalHost(t)
	l.pinGVisor()
	ui := newLocalUI("true", false, GVisor)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context())
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

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context())
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("local = %v, want the review after the second choice", err)
	}

	said(t, ui.fakeUI, "No installation changes were made.", "Choose another provider or exit.", "Provider:          gVisor")
	if len(ui.shown) != 2 {
		t.Fatalf("asked for the provider %d times, want 2", len(ui.shown))
	}
	again := ui.shown[1]
	runc := again[providerIndex(Runc)]
	if !slices.Equal(runc.Unavailable, []string{"The sandboxes in /var/lib/shard use gVisor.", "Setup never changes the provider of existing sandboxes."}) {
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

func TestExitAfterAProviderFailureChangesNothing(t *testing.T) {
	l := newLocalHost(t)
	l.sandbox(Runc)
	ui := newLocalUI("true", true, GVisor, exitOption)

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context())
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

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context())
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

	err := (&Setup{Host: h, UI: ui}).local(t.Context())
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

	err := (&Setup{Host: l.host(), UI: ui}).local(t.Context())
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
