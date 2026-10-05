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

	"github.com/presmihaylov/shard/services/client"
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
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	return &fakeHost{t: t, root: t.TempDir(), isActive: "active"}
}

// host runs as root, so privileged runs each command as it is.
func (f *fakeHost) host(rs *releaseServer) Host {
	h := Host{Root: f.root, OS: "linux", Arch: "amd64", Executable: filepath.Join(f.root, "/home/u/.local/bin/shard"), Version: "v0.1.0", Run: f.run}
	if rs != nil {
		h.Releases, h.HTTP = rs.URL+"/releases", rs.Client()
	}

	return h
}

func (f *fakeHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))

	switch name {
	case "systemctl":
		if args[0] != "is-active" {
			return nil, nil
		}
		if f.isActive == "active" {
			return []byte("active\n"), nil
		}
		return []byte(f.isActive + "\n"), errors.New("exit status 3")
	case "launchctl":
		if args[0] == "print" && f.launchdErr {
			return []byte(f.launchd), errors.New("exit status 113")
		}
		return []byte(f.launchd), nil
	case "mkdir", "install", "mv", "rm", "rmdir", "find":
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

// installed writes the files of a setup install, the manifest that names them, and the shard the user runs.
func (f *fakeHost) installed(t *testing.T, m Manifest) {
	t.Helper()
	f.write(t, "/home/u/.local/bin/shard", "old cli")
	for _, o := range m.Files {
		if o.Kind != KindData {
			f.write(t, o.Path, "old "+filepath.Base(o.Path))
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

func TestUninstallRefusesWhileASandboxRemains(t *testing.T) {
	f := newFakeHost(t)
	m := linuxInstall("v0.1.0")
	f.installed(t, m)
	f.write(t, "/var/lib/shard/sandboxes/sb_1/sandbox.json", "{}")
	f.write(t, "/var/lib/shard/sandboxes/sb_2/sandbox.json", "{}")
	f.write(t, "/var/lib/shard/images/sb_3/sandbox.json", "{}")
	ui := &fakeUI{}

	err := (&Setup{Host: f.host(nil), UI: ui}).uninstall(t.Context(), m)
	if err == nil || !strings.Contains(err.Error(), "2 sandboxes left") {
		t.Fatalf("uninstall = %v, want 2 sandboxes named", err)
	}
	said(t, ui, "Shard has 2 sandboxes on this machine.", "shard list --all", "shard remove --force <name>")
	if _, ok := f.read(t, "/usr/local/bin/shard"); !ok || len(ui.asked) != 0 {
		t.Fatalf("uninstall removed a file or asked %v while a sandbox remains", ui.asked)
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
		"This will stop and remove the background service",
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
