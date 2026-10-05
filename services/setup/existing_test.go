package setup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/kernel"
)

// fakeHost runs file commands for real under a temp root and answers the service managers from canned output.
type fakeHost struct {
	t        *testing.T
	root     string
	calls    []string
	isActive string
	launchd  string
	// launchdErr makes launchctl print fail with launchd as its output.
	launchdErr bool
	// starting is how many state reads still find the daemon coming up.
	starting int
	// rebuilt are the files daemon status writes, as a provider build does.
	rebuilt []string
	// listeners is what ss prints for the proxy ports, and tables what nft list tables prints.
	listeners string
	tables    string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	return &fakeHost{t: t, root: t.TempDir(), isActive: "active"}
}

// host runs as root, so privileged runs each command as it is.
func (f *fakeHost) host(rs *releaseServer) Host {
	h := Host{Root: f.root, OS: "linux", Arch: "amd64", Executable: filepath.Join(f.root, "/home/u/.local/bin/shard"), Version: "v0.1.0", Env: f.env, Run: f.run}
	if rs != nil {
		h.Releases, h.HTTP = rs.URL+"/releases", rs.Client()
	}

	return h
}

func (f *fakeHost) env(name string) string {
	if name == "HOME" {
		return filepath.Join(f.root, "/home/u")
	}

	return ""
}

func (f *fakeHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))

	switch name {
	case shardBinary:
		for _, p := range f.rebuilt {
			f.write(f.t, p, "rebuilt")
		}
		return nil, nil
	case "systemctl":
		if args[0] != "is-active" {
			return nil, nil
		}
		if f.starting > 0 {
			f.starting--
			return []byte("activating\n"), errors.New("exit status 3")
		}
		if f.isActive == "active" {
			return []byte("active\n"), nil
		}
		return []byte(f.isActive + "\n"), errors.New("exit status 3")
	case "launchctl":
		if args[0] == "print" && f.launchdErr {
			return []byte(f.launchd), errors.New("exit status 113")
		}
		if args[0] == "print" && f.starting > 0 {
			f.starting--
			return []byte("state = spawn scheduled\n"), nil
		}
		return []byte(f.launchd), nil
	case "ss":
		return []byte(f.listeners), nil
	case "nft":
		if args[0] == "list" {
			return []byte(f.tables), nil
		}
		return nil, nil
	case "ip":
		return nil, nil
	case "mkdir", "install", "mv", "rm", "rmdir", "find", "sh":
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	if filepath.IsAbs(name) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	f.t.Errorf("unexpected command %s %v", name, args)

	return nil, errors.New("unexpected command")
}

func (f *fakeHost) called(prefix string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func (f *fakeHost) write(t *testing.T, path, body string) {
	t.Helper()
	full := filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *fakeHost) read(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, path))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data), true
}

// installed writes the files of a setup install, the manifest that names them, the provider's tools and the shard the user runs.
func (f *fakeHost) installed(t *testing.T, m Manifest) {
	t.Helper()
	f.write(t, "/home/u/.local/bin/shard", "old cli")
	for _, o := range m.Files {
		if o.Kind != KindData {
			f.write(t, o.Path, "old "+filepath.Base(o.Path))
		}
	}
	for _, tl := range toolsFor(f.host(nil), m.Provider) {
		if _, ok := lookPath(f.host(nil), tl.Name); !ok {
			f.write(t, "/usr/bin/"+tl.Name, "#!/bin/sh\n")
		}
	}
	if err := saveManifest(t.Context(), f.host(nil), m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	f.calls = nil
}

func linuxInstall(version string) Manifest {
	return Manifest{Version: version, Provider: "gvisor", StartAtBoot: true, Files: []Owned{
		{Path: "/usr/sbin/nft", Kind: KindTool, Package: "nftables"},
		{Path: "/usr/local/bin/shard", Kind: KindBinary},
		{Path: "/usr/local/bin/shard-init", Kind: KindBinary},
		{Path: "/etc/systemd/system/shard.service", Kind: KindService},
		{Path: "/etc/shard/serve.env", Kind: KindConfig},
	}}
}

// watchUI records the default of each Confirm and calls onPrint before it keeps a printed line.
type watchUI struct {
	*fakeUI
	defaults []bool
	onPrint  func(lines []string)
}

func (u *watchUI) Confirm(ctx context.Context, q Question, text string, yes bool) (bool, error) {
	u.defaults = append(u.defaults, yes)
	return u.fakeUI.Confirm(ctx, q, text, yes)
}

func (u *watchUI) Print(lines ...string) error {
	if u.onPrint != nil {
		u.onPrint(lines)
	}
	return u.fakeUI.Print(lines...)
}

func confirming(yes bool) *fakeUI {
	return &fakeUI{confirms: map[Question]bool{AskConfirm: yes}}
}

func said(t *testing.T, ui *fakeUI, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !slices.ContainsFunc(ui.printed, func(p string) bool { return strings.Contains(p, line) }) {
			t.Errorf("output %q lacks %q", ui.printed, line)
		}
	}
}

func TestDetect(t *testing.T) {
	t.Run("nothing installed", func(t *testing.T) {
		f := newFakeHost(t)
		f.write(t, "/var/lib/shard/images/index.json", "{}")

		_, ok, err := Detect(t.Context(), f.host(nil))
		if err != nil || ok {
			t.Fatalf("Detect = %v, %v; the data dir alone is no installation", ok, err)
		}
	})

	t.Run("setup install", func(t *testing.T) {
		f := newFakeHost(t)
		f.installed(t, linuxInstall("v0.1.0"))
		f.isActive = "failed"

		inst, ok, err := Detect(t.Context(), f.host(nil))
		if err != nil || !ok {
			t.Fatalf("Detect = %v, %v", ok, err)
		}
		if inst.Manifest == nil || inst.Manifest.Version != "v0.1.0" || inst.Service != ServiceInactive {
			t.Fatalf("Detect = %+v, want the manifest and an inactive service", inst)
		}
	})

	t.Run("manual startup asks no service manager", func(t *testing.T) {
		f := newFakeHost(t)
		m := linuxInstall("v0.1.0")
		m.StartAtBoot = false
		f.installed(t, m)

		inst, _, err := Detect(t.Context(), f.host(nil))
		if err != nil || inst.Service != ServiceNone || len(f.calls) != 0 {
			t.Fatalf("Detect = %+v, %v, calls %v", inst, err, f.calls)
		}
	})

	t.Run("manual install", func(t *testing.T) {
		f := newFakeHost(t)
		f.write(t, "/usr/local/bin/shard", "bin")
		f.write(t, "/etc/systemd/system/shard.service", "unit")

		inst, ok, err := Detect(t.Context(), f.host(nil))
		if err != nil || !ok || inst.Manifest != nil {
			t.Fatalf("Detect = %+v, %v, %v", inst, ok, err)
		}
		want := []string{"/usr/local/bin/shard", "/etc/systemd/system/shard.service"}
		if !slices.Equal(inst.Manual, want) {
			t.Fatalf("Manual = %v, want %v", inst.Manual, want)
		}
	})
}

func TestServiceStateOnMac(t *testing.T) {
	cases := map[string]struct {
		out     string
		fails   bool
		want    ServiceState
		wantErr bool
	}{
		"running":   {out: "system/shard.daemon = {\n\tstate = running\n}", want: ServiceActive},
		"loaded":    {out: "system/shard.daemon = {\n\tstate = not running\n}", want: ServiceInactive},
		"not found": {out: "Could not find service \"shard.daemon\" in domain for system", fails: true, want: ServiceInactive},
		"other":     {out: "Operation not permitted", fails: true, wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			f.launchd, f.launchdErr = tc.out, tc.fails
			h := f.host(nil)
			h.OS = "darwin"

			got, err := serviceState(t.Context(), h, Manifest{StartAtBoot: true})
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("serviceState = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestExistingShowsTheSummaryAndExits(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := &fakeUI{selects: map[Question]string{AskExisting: "exit"}}

	err := (&Setup{Host: f.host(nil), UI: ui}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive})
	if err != nil {
		t.Fatalf("existing: %v", err)
	}
	want := []string{"Shard is already installed", "", "Version:  v0.1.0", "Provider: " + providerTitle("gvisor"), "Service:  Active", ""}
	if !slices.Equal(ui.printed, want) {
		t.Fatalf("summary = %q, want %q", ui.printed, want)
	}
	var names []string
	for _, o := range ui.options[AskExisting] {
		names = append(names, o.Name)
	}
	if !slices.Equal(names, []string{"repair", "upgrade", "uninstall", "exit"}) || !ui.options[AskExisting][0].Default {
		t.Fatalf("the menu offers %v, want repair (the default), upgrade, uninstall, exit", names)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Exit ran %v", f.calls)
	}
}

func TestManualInstallChangesNothing(t *testing.T) {
	f := newFakeHost(t)
	f.write(t, "/usr/local/bin/shard", "bin")
	f.write(t, "/etc/systemd/system/shard.service", "unit")
	inst, _, err := Detect(t.Context(), f.host(nil))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	ui := &fakeUI{}

	if err := (&Setup{Host: f.host(nil), UI: ui}).existing(t.Context(), inst); err != nil {
		t.Fatalf("existing: %v", err)
	}
	said(t, ui, "Manual installation detected.", "sudo systemctl disable --now shard", "sudo rm /etc/systemd/system/shard.service /usr/local/bin/shard")
	if len(f.calls) != 0 || len(ui.asked) != 0 {
		t.Fatalf("a manual install ran %v and asked %v", f.calls, ui.asked)
	}
	if _, ok := f.read(t, "/usr/local/bin/shard"); !ok {
		t.Fatal("a manual install lost its binary")
	}
}

func TestRepairFindsNothingToDo(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := &fakeUI{}

	if err := (&Setup{Host: f.host(nil), UI: ui}).repair(t.Context(), m, ServiceActive); err != nil {
		t.Fatalf("repair: %v", err)
	}
	said(t, ui, "No problems found")
	if len(ui.asked) != 0 {
		t.Fatalf("repair asked %v", ui.asked)
	}
}

func TestRepairShowsTheProblemsBeforeItAsks(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	if err := os.Remove(filepath.Join(f.root, "/usr/local/bin/shard-init")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ui := confirming(false)

	err := (&Setup{Host: f.host(nil), UI: ui}).repair(t.Context(), m, ServiceInactive)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("repair = %v, want ErrDeclined", err)
	}
	said(t, ui, "/usr/local/bin/shard-init is missing.", "The background service is not running.")
}

func TestRepairFindsADeletedVMShimAndKernel(t *testing.T) {
	f := newFakeHost(t)
	h := f.host(nil)
	h.OS, h.Arch, h.Env = "darwin", "arm64", func(string) string { return "u" }
	m := Manifest{Version: "v0.1.0", Provider: VZ, StartAtBoot: true, Files: []Owned{
		{Path: "/usr/local/bin/shard", Kind: KindBinary},
		{Path: launchdPlist, Kind: KindService},
	}}
	f.installed(t, m)
	f.write(t, "/var/lib/shard/vz/shard-init", "init")
	k, err := kernel.Path(DataDir, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	ui := confirming(false)

	if err := (&Setup{Host: h, UI: ui}).repair(t.Context(), m, ServiceActive); !errors.Is(err, ErrDeclined) {
		t.Fatalf("repair = %v, want ErrDeclined", err)
	}
	said(t, ui, "/var/lib/shard/vz/shard-vz-shim is missing.", k+" is missing.")
	if slices.Contains(ui.printed, "  /var/lib/shard/vz/shard-init is missing.") {
		t.Errorf("repair reports the guest init it has: %q", ui.printed)
	}
	restart, verify := slices.Index(ui.printed, "  Restart the daemon"), slices.Index(ui.printed, "  Verify the daemon connection")
	if restart < 0 || verify < restart {
		t.Fatalf("a running daemon is not restarted before the verify: %q", ui.printed)
	}
	if restore := slices.Index(ui.printed, "  Restore the provider's files"); restore != verify+1 {
		t.Fatalf("the files are not restored right after the verify: %q", ui.printed)
	}
}

func TestRepairRestoresTheRuntimeFiles(t *testing.T) {
	k, err := kernel.Path(DataDir, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	files := append(vzshim.Paths(filepath.Join(DataDir, vzshim.Dir)), k)
	for _, c := range []struct {
		name    string
		rebuilt []string
		want    string
	}{
		{name: "all back", rebuilt: files},
		{name: "kernel still gone", rebuilt: files[:len(files)-1], want: "The daemon did not put back " + k + "."},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeHost(t)
			f.rebuilt = c.rebuilt
			h := f.host(nil)
			h.OS, h.Arch = "darwin", "arm64"

			err := (&Setup{Host: h, UI: &fakeUI{}}).restoreRuntime(t.Context(), VZ)
			if !f.called(shardBinary + " --remote  daemon status") {
				t.Fatalf("calls = %v, want a daemon status that builds the provider", f.calls)
			}
			if c.want == "" && err != nil {
				t.Fatalf("restore = %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("restore = %v, want %q", err, c.want)
			}
		})
	}
}

func TestRestartServiceWaitsForTheDaemon(t *testing.T) {
	wait, poll := restartWait, verifyPoll
	restartWait, verifyPoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { restartWait, verifyPoll = wait, poll })
	for _, c := range []struct {
		name, os, restart string
		starting          int
		// up is the state the daemon settles in once it stops starting.
		up   bool
		want string
	}{
		{name: "systemd", os: "linux", restart: "systemctl restart shard", starting: 3, up: true},
		{name: "launchd", os: "darwin", restart: "launchctl kickstart -k " + launchdLabel, starting: 3, up: true},
		{name: "systemd never up", os: "linux", restart: "systemctl restart shard", want: "the daemon is inactive 50ms after the restart"},
		{name: "launchd never up", os: "darwin", restart: "launchctl kickstart -k " + launchdLabel, want: "the daemon is inactive 50ms after the restart"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeHost(t)
			f.starting = c.starting
			f.isActive, f.launchd = "failed", "state = not running"
			if c.up {
				f.isActive, f.launchd = "active", "state = running"
			}
			h := f.host(nil)
			h.OS = c.os

			err := restartService(t.Context(), h, Manifest{StartAtBoot: true})
			if !f.called(c.restart) {
				t.Fatalf("calls = %v, want %q", f.calls, c.restart)
			}
			if c.want == "" && err != nil {
				t.Fatalf("restart = %v", err)
			}
			if c.want != "" && (err == nil || err.Error() != c.want) {
				t.Fatalf("restart = %v, want %q", err, c.want)
			}
			if f.starting != 0 {
				t.Fatalf("restart returned with %d state reads left of the start", f.starting)
			}
		})
	}
}

func TestRepairFindsADeletedTool(t *testing.T) {
	k, err := kernel.Path(DataDir, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		provider, tool string
		// restart is the runtime file the daemon puts back once it is restarted, if the provider keeps one.
		restart string
	}{
		{provider: GVisor, tool: "runsc"},
		{provider: Sysbox, tool: "sysbox-runc"},
		{provider: Runc, tool: "runc"},
		{provider: Firecracker, tool: "jailer", restart: k},
	} {
		t.Run(c.provider, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			m.Provider = c.provider
			f.installed(t, m)
			if err := os.Remove(filepath.Join(f.root, "/usr/bin", c.tool)); err != nil {
				t.Fatalf("remove: %v", err)
			}
			ui := confirming(false)

			if err := (&Setup{Host: f.host(nil), UI: ui}).repair(t.Context(), m, ServiceActive); !errors.Is(err, ErrDeclined) {
				t.Fatalf("repair = %v, want ErrDeclined", err)
			}
			said(t, ui, "  "+c.tool+" is missing.", c.restart)
			if restarts := slices.Contains(ui.printed, "  Restart the daemon"); restarts != (c.restart != "") {
				t.Errorf("restart the daemon = %t, want %t: %q", restarts, c.restart != "", ui.printed)
			}
			if restores := slices.Contains(ui.printed, "  Restore the provider's files"); restores != (c.restart != "") {
				t.Errorf("restore the provider's files = %t, want %t: %q", restores, c.restart != "", ui.printed)
			}
		})
	}
}

func TestUpgradeVerifiesBeforeItReplaces(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.1.0", false, false, true, nil)
	rs.add("v0.2.0", false, false, true, map[string]string{
		"shard-linux-amd64":      "#!/bin/sh\necho client v0.2.0\n",
		"shard-init-linux-amd64": "new init",
	})
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := &watchUI{fakeUI: confirming(true)}
	ui.onPrint = func(lines []string) {
		if slices.Contains(lines, "Latest release: v0.2.0") && rs.downloads.Load() != 0 {
			t.Errorf("the version printed after %d downloads", rs.downloads.Load())
		}
	}

	if err := (&Setup{Host: f.host(rs), UI: ui}).upgrade(t.Context(), m, ServiceActive); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for path, want := range map[string]string{
		"/usr/local/bin/shard":      "#!/bin/sh\necho client v0.2.0\n",
		"/usr/local/bin/shard-init": "new init",
		"/home/u/.local/bin/shard":  "#!/bin/sh\necho client v0.2.0\n",
	} {
		if got, _ := f.read(t, path); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	got, _, err := LoadManifest(f.host(nil))
	if err != nil || got.Version != "v0.2.0" || got.Provider != "gvisor" {
		t.Fatalf("manifest = %+v, %v", got, err)
	}
	if !f.called("systemctl restart shard") || f.called("systemctl enable") {
		t.Fatalf("calls = %v, want one restart and no unit change", f.calls)
	}
	said(t, ui.fakeUI, "Your sandboxes keep running while the daemon restarts.")
}

func TestUpgradeKeepsTheOldBinaries(t *testing.T) {
	cases := map[string]struct {
		cli     string
		confirm bool
		want    error
		wantMsg string
	}{
		"wrong version": {cli: "#!/bin/sh\necho client v0.1.9\n", wantMsg: `reports "client v0.1.9"`},
		"declined":      {cli: "#!/bin/sh\necho client v0.2.0\n", want: ErrDeclined},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rs := newReleaseServer(t)
			rs.add("v0.2.0", false, false, true, map[string]string{"shard-linux-amd64": tc.cli, "shard-init-linux-amd64": "new init"})
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			f.installed(t, m)

			err := (&Setup{Host: f.host(rs), UI: confirming(tc.confirm)}).upgrade(t.Context(), m, ServiceActive)
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("upgrade = %v, want %v", err, tc.want)
			}
			if tc.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMsg)) {
				t.Fatalf("upgrade = %v, want %q", err, tc.wantMsg)
			}
			if got, _ := f.read(t, "/usr/local/bin/shard"); got != "old shard" {
				t.Fatalf("the old binary became %q", got)
			}
			if got, _, err := LoadManifest(f.host(nil)); err != nil || got.Version != "v0.1.0" {
				t.Fatalf("manifest = %+v, %v", got, err)
			}
			if f.called("systemctl restart") {
				t.Fatalf("calls = %v", f.calls)
			}
		})
	}
}

func TestUpgradeStopsAtTheLatestRelease(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, nil)
	f := newFakeHost(t)
	m := linuxInstall("v0.2.0")
	f.installed(t, m)
	ui := &fakeUI{}

	if err := (&Setup{Host: f.host(rs), UI: ui}).upgrade(t.Context(), m, ServiceActive); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	said(t, ui, "Shard v0.2.0 is up to date.")
	if len(ui.asked) != 0 || rs.downloads.Load() != 0 {
		t.Fatalf("upgrade asked %v and downloaded %d files", ui.asked, rs.downloads.Load())
	}
}

func TestUpgradeLeavesAnInactiveServiceStopped(t *testing.T) {
	rs := newReleaseServer(t)
	rs.add("v0.2.0", false, false, true, map[string]string{"shard-linux-amd64": "#!/bin/sh\necho client v0.2.0\n", "shard-init-linux-amd64": "new init"})
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := confirming(true)

	if err := (&Setup{Host: f.host(rs), UI: ui}).upgrade(t.Context(), m, ServiceInactive); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	said(t, ui, "Setup does not start it.")
	if f.called("systemctl restart") || f.called("systemctl start") {
		t.Fatalf("calls = %v", f.calls)
	}
}

// The commands the refusal names reach the daemon: on Linux its socket is root's, so they carry sudo. (SHARD-675)
func TestUninstallRefusesWhileASandboxRemains(t *testing.T) {
	for _, tt := range []struct {
		name string
		os   string
		ids  []string
		left string
		want []string
	}{
		{"one on linux", "linux", []string{"sb_1"}, "1 sandbox left", []string{
			"Shard has 1 sandbox on this machine.", "Remove it before you uninstall Shard:",
			"    sudo shard list --all", "    sudo shard remove --force <name>",
		}},
		{"two on darwin", "darwin", []string{"sb_1", "sb_2"}, "2 sandboxes left", []string{
			"Shard has 2 sandboxes on this machine.", "Remove them before you uninstall Shard:",
			"    shard list --all", "    shard remove --force <name>",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			f.installed(t, m)
			for _, id := range tt.ids {
				f.write(t, "/var/lib/shard/sandboxes/"+id+"/sandbox.json", "{}")
			}
			f.write(t, "/var/lib/shard/images/sb_3/sandbox.json", "{}")
			ui := &fakeUI{}
			h := f.host(nil)
			h.OS = tt.os

			err := (&Setup{Host: h, UI: ui}).uninstall(t.Context(), m)
			if err == nil || !strings.Contains(err.Error(), tt.left) {
				t.Fatalf("uninstall = %v, want %q", err, tt.left)
			}
			for _, line := range tt.want {
				if !slices.Contains(ui.printed, line) {
					t.Errorf("output %q lacks the line %q", ui.printed, line)
				}
			}
			if _, ok := f.read(t, "/usr/local/bin/shard"); !ok || len(ui.asked) != 0 {
				t.Fatalf("uninstall removed a file or asked %v while a sandbox remains", ui.asked)
			}
		})
	}
}

func TestUninstallRemovesOnlyWhatSetupOwns(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	f.write(t, "/var/lib/shard/images/index.json", "{}")
	ui := &watchUI{fakeUI: confirming(true)}

	if err := (&Setup{Host: f.host(nil), UI: ui}).uninstall(t.Context(), m); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !slices.Equal(ui.defaults, []bool{false}) {
		t.Fatalf("Confirm defaults = %v, want one No", ui.defaults)
	}
	for _, gone := range []string{"/usr/local/bin/shard", "/usr/local/bin/shard-init", "/etc/systemd/system/shard.service", "/etc/shard/serve.env", ManifestPath, filepath.Dir(ManifestPath)} {
		if _, err := os.Lstat(filepath.Join(f.root, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
	for _, kept := range []string{"/usr/sbin/nft", "/var/lib/shard/images/index.json"} {
		if _, ok := f.read(t, kept); !ok {
			t.Fatalf("uninstall removed %s", kept)
		}
	}
	if !f.called("systemctl disable --now shard") || !f.called("systemctl daemon-reload") {
		t.Fatalf("calls = %v", f.calls)
	}
	said(t, ui.fakeUI, "Your saved data remains in /var/lib/shard.", "sudo apt-get remove nftables", "rm "+f.host(nil).Executable)
}

func TestUninstallDeclinedChangesNothing(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	ui := confirming(false)

	err := (&Setup{Host: f.host(nil), UI: ui}).uninstall(t.Context(), m)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("uninstall = %v, want ErrDeclined", err)
	}
	want := []string{"Uninstall Shard?", "",
		"This will stop and remove the background service,",
		"remove Shard's network bridge and firewall tables,",
		"and remove files installed by Shard setup.", "",
		"Your saved data will remain.",
		"Shared tools will remain.", ""}
	if !slices.Equal(ui.printed, want) {
		t.Fatalf("output = %q, want %q", ui.printed, want)
	}
	if _, ok, err := LoadManifest(f.host(nil)); err != nil || !ok || len(f.calls) != 0 {
		t.Fatalf("a declined uninstall ran %v (manifest %v, %v)", f.calls, ok, err)
	}
}

func TestUninstallRunsAgainAfterAHalfDoneOne(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	if err := os.Remove(filepath.Join(f.root, "/etc/systemd/system/shard.service")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := (&Setup{Host: f.host(nil), UI: confirming(true)}).uninstall(t.Context(), m); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if f.called("systemctl disable") {
		t.Fatalf("uninstall stopped a unit that is gone: %v", f.calls)
	}
	if _, ok, err := LoadManifest(f.host(nil)); err != nil || ok {
		t.Fatalf("the manifest outlived the uninstall: %v, %v", ok, err)
	}
}

// A setup that chose No for automatic startup installed no service, so uninstall neither offers nor runs a service step. (SHARD-664)
func TestUninstallAfterABootNoSetupHasNoServiceStep(t *testing.T) {
	f := newFakeHost(t)
	m := Manifest{Version: "v0.1.0", Provider: VZ, Files: []Owned{
		{Path: "/usr/local/bin/shard", Kind: KindBinary},
		{Path: "/usr/local/bin/shard-init", Kind: KindBinary},
	}}
	f.installed(t, m)
	ui := confirming(true)
	h := f.host(nil)
	h.OS, h.Arch = "darwin", "arm64"

	if err := (&Setup{Host: h, UI: ui}).uninstall(t.Context(), m); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	want := []string{"Uninstall Shard?", "", "This will remove files installed by Shard setup.", "", "Your saved data will remain.", "Shared tools will remain.", ""}
	if !slices.Equal(ui.printed[:len(want)], want) {
		t.Fatalf("output = %q, want it to start %q", ui.printed, want)
	}
	if steps := ui.lists[0].steps; !slices.Equal(steps, []string{"Remove files installed by Shard setup"}) {
		t.Fatalf("the steps are %q, want only the file removal", steps)
	}
	if f.called("launchctl") {
		t.Fatalf("uninstall touched launchd: %v", f.calls)
	}
}

// With no daemon left on the host, uninstall takes the shared bridge and tables as pkg/hostclean does, and leaves IP forwarding as it is. (SHARD-669)
func TestUninstallRemovesTheSharedNetwork(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	f.write(t, "/sys/class/net/shard0/address", "02:00:00:00:00:01")
	f.write(t, ipForward, "1\n")
	f.tables = "table inet filter\ntable inet shard\ntable bridge shard\n"
	ui := confirming(true)

	if err := (&Setup{Host: f.host(nil), UI: ui}).uninstall(t.Context(), m); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	stop := slices.Index(f.calls, "systemctl disable --now shard")
	inet := slices.Index(f.calls, "nft delete table inet shard")
	if stop < 0 || inet < stop || !f.called("nft delete table bridge shard") || !f.called("ip link delete shard0") {
		t.Fatalf("calls = %v, want the service stopped, then both tables and the bridge deleted", f.calls)
	}
	if f.called("nft delete table inet filter") {
		t.Fatalf("uninstall deleted a table it does not own: %v", f.calls)
	}
	said(t, ui, "IP forwarding remains on (net.ipv4.ip_forward = 1). Other software may need it.", "Turn it off with: sudo sysctl -w net.ipv4.ip_forward=0")
}

// A daemon that still has a sandbox port on the bridge or serves the proxy keeps the network, and uninstall says so. (SHARD-272, SHARD-669)
func TestUninstallLeavesANetworkADaemonStillUses(t *testing.T) {
	cases := map[string]func(f *fakeHost){
		"a port on the bridge": func(f *fakeHost) { f.write(t, "/sys/class/net/shard0/brif/veth1", "") },
		"the proxy listening":  func(f *fakeHost) { f.listeners = "LISTEN 0 4096 *:30080 *:*\n" },
	}
	for name, held := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			f.installed(t, m)
			f.write(t, "/sys/class/net/shard0/address", "02:00:00:00:00:01")
			f.write(t, ipForward, "1\n")
			f.tables = "table inet shard\ntable bridge shard\n"
			held(f)
			ui := confirming(true)

			if err := (&Setup{Host: f.host(nil), UI: ui}).uninstall(t.Context(), m); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if f.called("nft") || f.called("ip link delete") {
				t.Fatalf("uninstall took a network a daemon uses: %v", f.calls)
			}
			said(t, ui, "A Shard daemon still uses the network bridge shard0 and its firewall tables, so they remain.")
			if slices.ContainsFunc(ui.printed, func(l string) bool { return strings.Contains(l, "ip_forward") }) {
				t.Fatalf("output %q names IP forwarding while a daemon uses it", ui.printed)
			}
		})
	}
}

// Uninstall names each shard command still on disk, with its rm line, and none that is gone. (SHARD-668)
func TestUninstallNamesTheShardCommandsLeft(t *testing.T) {
	cases := map[string]struct {
		executable string
		keepCopy   bool
		want       []string
	}{
		"the installer copy":       {"/usr/local/bin/shard", true, []string{"", "The shard command remains at {home}.", "Remove it with: rm {home}"}},
		"the copy and another one": {"/opt/shard/shard", true, []string{"", "These shard commands remain:", "  {opt}", "    Remove it with: rm {opt}", "  {home}", "    Remove it with: rm {home}"}},
		"none":                     {"/usr/local/bin/shard", false, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			f.installed(t, m)
			f.write(t, "/opt/shard/shard", "cli")
			home := filepath.Join(f.root, "/home/u/.local/bin/shard")
			if !tc.keepCopy {
				if err := os.Remove(home); err != nil {
					t.Fatalf("remove: %v", err)
				}
			}
			ui := confirming(true)
			h := f.host(nil)
			h.Executable = filepath.Join(f.root, tc.executable)

			if err := (&Setup{Host: h, UI: ui}).uninstall(t.Context(), m); err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			want := make([]string, 0, len(tc.want))
			for _, l := range tc.want {
				want = append(want, strings.NewReplacer("{home}", home, "{opt}", filepath.Join(f.root, "/opt/shard/shard")).Replace(l))
			}
			rmLine := func(l string) bool { return strings.Contains(l, "Remove it with: rm") }
			if got := ui.printed[len(ui.printed)-len(want):]; !slices.Equal(got, want) || slices.ContainsFunc(ui.printed[:len(ui.printed)-len(want)], rmLine) {
				t.Fatalf("output = %q, want its only rm lines at the end, as %q", ui.printed, want)
			}
			if slices.ContainsFunc(ui.printed, func(l string) bool { return strings.Contains(l, "/usr/local/bin/shard") }) {
				t.Fatalf("output %q names the removed /usr/local/bin/shard", ui.printed)
			}
		})
	}
}

// A repair is local setup too, so it offers to drop a saved remote and drops it once the repair succeeds; Exit asks nothing.
func TestTheExistingMenuOffersToDropASavedRemote(t *testing.T) {
	saved := client.Config{Remote: "https://shard.example.com", APIKey: testKey}

	for _, tc := range []struct {
		choice string
		asks   bool
		want   client.Config
	}{
		{choice: "repair", asks: true, want: client.Config{}},
		{choice: "exit", want: saved},
	} {
		t.Run(tc.choice, func(t *testing.T) {
			f := newFakeHost(t)
			m := linuxInstall("v0.1.0")
			f.installed(t, m)
			env, path := testHost(t, nil)
			saveConnection(t, path, saved)
			host := f.host(nil)
			host.Env = env.Env
			ui := &fakeUI{selects: map[Question]string{AskExisting: tc.choice}, confirms: map[Question]bool{AskSwitch: true}}

			if err := (&Setup{Host: host, UI: ui}).existing(t.Context(), Installation{Manifest: &m, Service: ServiceActive}); err != nil {
				t.Fatalf("existing: %v", err)
			}
			if got := slices.Contains(ui.asked, AskSwitch); got != tc.asks {
				t.Errorf("asked %v, want the switch asked: %v", ui.asked, tc.asks)
			}
			if got := savedConnection(t, path); got != tc.want {
				t.Errorf("the saved connection is %v, want %v", got, tc.want)
			}
		})
	}
}
