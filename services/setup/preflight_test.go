package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/sandboxstate"
)

// preflightOn runs the checks for l on h and returns the finding and the checklist it drew.
func preflightOn(t *testing.T, h Host, l Local) (*finding, *fakeChecklist) {
	t.Helper()
	ui := &fakeUI{}
	f, err := (&Setup{Host: h, UI: ui}).preflight(t.Context(), l)
	if err != nil {
		t.Fatalf("preflight = %v", err)
	}
	if len(ui.lists) != 1 {
		t.Fatalf("drew %d checklists, want 1", len(ui.lists))
	}

	return f, ui.lists[0]
}

func wantFinding(t *testing.T, f *finding, check string, provider bool, lines ...string) {
	t.Helper()
	if f == nil {
		t.Fatalf("passed, want %q to fail with %q", check, lines)
	}
	if f.check != check || f.provider != provider || !slices.Equal(f.lines, lines) {
		t.Fatalf("finding %+v, want check %q provider %v lines %q", *f, check, provider, lines)
	}
}

func TestPreflightPassesAReadyHost(t *testing.T) {
	l := newLocalHost(t)

	f, list := preflightOn(t, l.host(), Local{Provider: GVisor, StartAtBoot: true})
	if f != nil {
		t.Fatalf("a ready host fails %q: %q", f.check, f.lines)
	}
	want := []string{
		"Supported operating system and CPU", "Provider requirements", "Administrator access",
		"Installation paths and permissions", "Available disk space and filesystem support", "Download access",
		"Existing shard installation", "Background service support",
	}
	if list.title != "Checking this machine" || !slices.Equal(list.steps, want) {
		t.Fatalf("checklist %q %q", list.title, list.steps)
	}
	if slices.ContainsFunc(list.marks, func(m string) bool { return strings.HasPrefix(m, "fail") }) {
		t.Fatalf("marks %q", list.marks)
	}
	if l.changed() {
		t.Fatalf("preflight changed the host: %q", l.calls)
	}
}

func TestPreflightStopsAtTheFirstFailure(t *testing.T) {
	l := newLocalHost(t)
	h := l.host()
	h.Arch = "arm64"

	f, list := preflightOn(t, h, Local{Provider: GVisor, StartAtBoot: true})

	wantFinding(t, f, "Supported operating system and CPU", false,
		"Supported hosts: Linux on x86_64, and Macs with Apple silicon.", "This machine runs Linux on arm64.")
	want := []string{"start 0", "fail 0: Supported hosts: Linux on x86_64, and Macs with Apple silicon. / This machine runs Linux on arm64."}
	if !slices.Equal(list.marks, want) {
		t.Fatalf("marks %q, want %q", list.marks, want)
	}
}

func TestPreflightProviderRequirements(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		mac      bool
		setup    func(l *localHost)
		lines    []string
	}{
		{
			name: "no kvm", provider: Firecracker,
			lines: []string{"Firecracker requires access to /dev/kvm.", "This machine does not provide it."},
		},
		{
			name: "an old firecracker", provider: Firecracker,
			setup: func(l *localHost) {
				l.write("/dev/kvm", "")
				l.write("/usr/local/bin/firecracker", "#!/bin/sh\necho 'Firecracker v1.10.1'\n")
			},
			lines: []string{
				"/usr/local/bin/firecracker is firecracker 1.10.1, and shard needs 1.13.0 or newer, whose snapshot load keeps the dirty-page log on.",
				"Replace it with Firecracker 1.13.0 or newer, or remove it so setup installs one.",
			},
		},
		{
			name: "no apt-get for a missing package", provider: GVisor,
			setup: func(l *localHost) {
				l.remove("/usr/sbin/ip")
				l.remove("/usr/bin/apt-get")
			},
			lines: []string{
				"gVisor requires ip, and setup installs them with apt-get, which this machine does not have.",
				"Install them and run shard setup again.",
			},
		},
		{
			name: "no codesign on a Mac", provider: VZ, mac: true,
			setup: func(l *localHost) { l.remove("/usr/bin/codesign") },
			lines: []string{"macOS Virtualization requires codesign, which setup cannot install.", "Install it and run shard setup again."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			h := l.host()
			if tc.mac {
				h = l.mac()
			}
			if tc.setup != nil {
				tc.setup(l)
			}

			f, _ := preflightOn(t, h, Local{Provider: tc.provider})

			wantFinding(t, f, "Provider requirements", true, tc.lines...)
		})
	}
}

func TestPreflightAdministratorAccess(t *testing.T) {
	t.Run("root on a Mac without sudo", func(t *testing.T) {
		l := newLocalHost(t)
		h := l.mac()
		h.Euid = 0

		f, _ := preflightOn(t, h, Local{Provider: VZ})

		wantFinding(t, f, "Administrator access", false,
			"On a Mac the daemon runs as your user, and setup cannot tell who that is when it runs as root.",
			"Run shard setup as your user. It asks for administrator access when it needs it.")
	})
	t.Run("a user without sudo", func(t *testing.T) {
		l := newLocalHost(t)
		l.remove("/usr/bin/sudo")
		h := l.host()
		h.Euid = 1000

		f, _ := preflightOn(t, h, Local{Provider: GVisor})

		wantFinding(t, f, "Administrator access", false, "Setup needs administrator access, and this machine has no sudo.", "Run shard setup as root.")
	})
	t.Run("a user with sudo", func(t *testing.T) {
		l := newLocalHost(t)
		h := l.host()
		h.Euid = 1000

		if f, _ := preflightOn(t, h, Local{Provider: GVisor}); f != nil {
			t.Fatalf("fails %q: %q", f.check, f.lines)
		}
		if !slices.Contains(l.calls, "sudo -n true") {
			t.Fatalf("calls = %q, want sudo asked without a prompt", l.calls)
		}
	})
	// A cloud image's user has NOPASSWD:ALL beside the sudo group's rule, and -v wants a password from it.
	t.Run("passwordless commands where -v wants a password", func(t *testing.T) {
		l := newLocalHost(t)
		l.fail["env LC_ALL=C sudo -n -v"] = "sudo: a password is required\n"
		h := l.host()
		h.Euid = 1000

		if f, _ := preflightOn(t, h, Local{Provider: GVisor}); f != nil {
			t.Fatalf("fails %q: %q", f.check, f.lines)
		}
	})
	for _, c := range []struct {
		name, sudo string
		tty        bool
		lines      []string
	}{
		{name: "a user outside sudoers at a terminal", sudo: "Sorry, user nosudo may not run sudo on box.\n", tty: true, lines: []string{
			"Setup needs administrator access, and sudo does not allow this user: Sorry, user nosudo may not run sudo on box.",
			"Ask an administrator to give your user sudo access, or run shard setup as root.",
		}},
		{name: "a password and a terminal", sudo: "sudo: a password is required\n", tty: true},
		{name: "a password and no terminal", sudo: "sudo: a password is required\n", lines: []string{
			"Setup needs administrator access, and sudo needs a password it has no terminal to ask for.",
			"Run shard setup as root, or as a user with passwordless sudo.",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLocalHost(t)
			// A command wants a password from a user outside sudoers too, so only -v tells that user from one who has a password.
			l.fail["sudo -n true"] = "sudo: a password is required\n"
			l.fail["env LC_ALL=C sudo -n -v"] = c.sudo
			if c.tty {
				l.write("/dev/tty", "")
			}
			h := l.host()
			h.Euid = 1000

			f, _ := preflightOn(t, h, Local{Provider: GVisor})
			if c.lines == nil && f != nil {
				t.Fatalf("fails %q: %q", f.check, f.lines)
			}
			if c.lines != nil {
				wantFinding(t, f, "Administrator access", false, c.lines...)
			}
		})
	}
}

func TestPreflightInstallPaths(t *testing.T) {
	t.Run("a bin dir others can write", func(t *testing.T) {
		l := newLocalHost(t)
		if err := os.Chmod(filepath.Join(l.root, binDir), 0o777); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		f, _ := preflightOn(t, l.host(), Local{Provider: GVisor})

		wantFinding(t, f, "Installation paths and permissions", false,
			"/usr/local/bin can be changed by a user other than root, and the root daemon runs shard from it.",
			"Make root its owner and remove group and other write access, then run shard setup again.")
	})
	t.Run("a data dir that is a file", func(t *testing.T) {
		l := newLocalHost(t)
		l.write(DataDir, "")

		f, _ := preflightOn(t, l.host(), Local{Provider: GVisor})

		wantFinding(t, f, "Installation paths and permissions", false, "/var/lib/shard exists and is not a directory.", "Move it aside and run shard setup again.")
	})
	t.Run("a Mac data dir another user owns", func(t *testing.T) {
		l := newLocalHost(t)
		h := l.mac()
		h.Euid = os.Getuid() + 1
		l.mkdir(DataDir)

		f, _ := preflightOn(t, h, Local{Provider: VZ})

		wantFinding(t, f, "Installation paths and permissions", false,
			"/var/lib/shard belongs to another user, and the daemon runs as u.", "Change its owner to u and run shard setup again.")
	})
	t.Run("a Mac data dir the person owns", func(t *testing.T) {
		l := newLocalHost(t)
		h := l.mac()
		l.mkdir(DataDir)
		swap(t, &kernelURL, func(string) (string, error) { return l.rs.URL + "/download/v0.1.0/shard-init-linux-amd64", nil })

		if f, _ := preflightOn(t, h, Local{Provider: VZ}); f != nil {
			t.Fatalf("fails %q: %q", f.check, f.lines)
		}
	})
}

func TestFirecrackerDisk(t *testing.T) {
	const gibs = 1 << 30
	jailer := errors.New("/var/lib/shard is on a filesystem mounted nosuid")
	cases := []struct {
		name    string
		reflink bool
		chroot  error
		free    uint64
		files   bool
		want    *finding
	}{
		{name: "a reflink root the jailer accepts", reflink: true},
		{
			name: "a reflink root the jailer refuses", reflink: true, chroot: jailer,
			want: providerFailed("Firecracker cannot run its jailer under /var/lib/shard.", "/var/lib/shard is on a filesystem mounted nosuid."),
		},
		{
			name: "too little room for an image", free: gibs,
			want: providerFailed(
				"Firecracker needs /var/lib/shard on XFS or Btrfs, or 20.0 GiB free beside it for an XFS image.",
				"/var/lib has 1.0 GiB free, on a filesystem that cannot clone a disk.",
			),
		},
		{
			name: "files where the image mounts", free: 30 * gibs, files: true,
			want: providerFailed(
				"Firecracker mounts an XFS image over /var/lib/shard, so it must be empty on a filesystem that cannot clone a disk.",
				"/var/lib/shard already holds files.",
			),
		},
		{name: "room and no data dir", free: 30 * gibs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLocalHost(t)
			l.mkdir("/var/lib")
			if tc.files {
				l.write(DataDir+"/state", "")
			}
			swap(t, &checkChrootBase, func(string) error { return tc.chroot })

			got := firecrackerDisk(l.host(), filepath.Join(l.root, "/var/lib"), tc.free, tc.reflink)

			if tc.want == nil && got != nil || tc.want != nil && (got == nil || got.provider != tc.want.provider || !slices.Equal(got.lines, tc.want.lines)) {
				t.Fatalf("firecrackerDisk = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPreflightDownloadAccess(t *testing.T) {
	t.Run("no shard-init for this version", func(t *testing.T) {
		l := newLocalHost(t)
		h := l.host()
		h.Version = "v0.2.0"

		f, _ := preflightOn(t, h, Local{Provider: GVisor})

		if f == nil || f.check != "Download access" || !strings.HasPrefix(f.lines[0], "Setup could not find shard-init for shard v0.2.0: ") {
			t.Fatalf("finding %+v", f)
		}
	})
	t.Run("a pinned tool the server lacks", func(t *testing.T) {
		l := newLocalHost(t)
		l.remove("/usr/local/bin/runsc")
		swap(t, gvisorRelease, download{Title: "runsc", URL: l.rs.URL + "/download/pins/gvisor.tar.zstd"})

		f, _ := preflightOn(t, l.host(), Local{Provider: GVisor})

		wantFinding(t, f, "Download access", false,
			"Could not download gvisor.tar.zstd: the server answered 404 Not Found.", "Check the network connection and run shard setup again.")
	})
	t.Run("a kernel the server lacks", func(t *testing.T) {
		l := newLocalHost(t)
		swap(t, &kernelURL, func(arch string) (string, error) { return l.rs.URL + "/download/v0.1.0/vmlinux-" + arch, nil })

		f, _ := preflightOn(t, l.mac(), Local{Provider: VZ})

		wantFinding(t, f, "Download access", false,
			"Could not download vmlinux-arm64: the server answered 404 Not Found.", "Check the network connection and run shard setup again.")
	})
}

// runcOverGVisor is the refusal of runc over a data dir whose sandboxes use gVisor, with both ways out: the daemon steps when shard is installed, else the reinstall path, prefixing root commands with sudo (SHARD-742).
func runcOverGVisor(sudo string, installed bool) []string {
	head := []string{
		"The sandboxes in /var/lib/shard use gVisor.",
		"The daemon cannot start with runc over that data, and setup never changes its provider.",
		"To keep the data, choose gVisor.",
	}
	tail := []string{"Then run shard setup again."}
	if installed {
		return slices.Concat(head, []string{
			"To delete the saved data, first remove its sandboxes so their network and cgroups go too:", "",
			"  Start the daemon on that data:", "    " + sudo + "shard daemon --provider gvisor", "",
			"  List sandboxes:", "    " + sudo + "shard list --all", "",
			"  Remove a sandbox:", "    " + sudo + "shard remove --force <name>", "",
			"  Then stop that daemon and delete the data:", "    " + sudo + "rm -r /var/lib/shard",
		}, tail)
	}

	return slices.Concat(head, []string{
		"To delete the saved data, let setup install shard first, so its daemon can remove the sandboxes:", "",
		"  Choose gVisor. Setup installs shard and starts its daemon over this data.", "",
		"  Remove every sandbox:", "    " + sudo + "shard list --all", "    " + sudo + "shard remove --force <name>", "",
		"  Then uninstall shard and free the disk:", "    shard setup   (choose " + uninstallLabel + ")",
		"    " + sudo + "rm -r /var/lib/shard",
	}, tail)
}

func TestPreflightExistingSandboxes(t *testing.T) {
	l := newLocalHost(t)
	l.sandbox(GVisor)

	f, _ := preflightOn(t, l.host(), Local{Provider: Runc})
	wantFinding(t, f, "Existing shard installation", true, runcOverGVisor("", false)...)

	if f, _ := preflightOn(t, l.host(), Local{Provider: GVisor}); f != nil {
		t.Fatalf("the recorded provider fails %q: %q", f.check, f.lines)
	}
}

// An installed host can run the daemon, so the delete steps drive it directly instead of reinstalling first (SHARD-742).
func TestPreflightExistingSandboxesInstalled(t *testing.T) {
	l := newLocalHost(t)
	l.sandbox(GVisor)
	l.write(shardBinary, "bin")

	f, _ := preflightOn(t, l.host(), Local{Provider: Runc})
	wantFinding(t, f, "Existing shard installation", true, runcOverGVisor("", true)...)
}

// A firecracker root hides its records in a data image that uninstall leaves unmounted, so the image alone names it, and the free steps also remove its start lock (SHARD-742, SHARD-734).
func TestPreflightExistingDataImage(t *testing.T) {
	l := newLocalHost(t)
	l.write("/var/lib/shard.xfs", "")
	l.write("/var/lib/shard.xfs.lock", "")

	f, _ := preflightOn(t, l.host(), Local{Provider: GVisor})
	wantFinding(t, f, "Existing shard installation", true,
		"The data in /var/lib/shard belongs to Firecracker.",
		"The daemon cannot start with gVisor over that data, and setup never changes its provider.",
		"To keep the data, choose Firecracker.",
		"It lives in the 0.0 GiB disk image /var/lib/shard.xfs.",
		"To delete the saved data, let setup install shard first, so its daemon can remove the sandboxes:", "",
		"  Choose Firecracker. Setup installs shard and starts its daemon over this data.", "",
		"  Remove every sandbox:", "    shard list --all", "    shard remove --force <name>", "",
		"  Then uninstall shard and free the disk:", "    shard setup   (choose "+uninstallLabel+")",
		"    umount /var/lib/shard",
		"    rm /var/lib/shard.xfs",
		"    rm -r /var/lib/shard /var/lib/shard.xfs.lock",
		"Then run shard setup again.",
	)
}

// The read as root skips a name that is no id and an undecodable record, as the daemon does (SHARD-742).
func TestPrivilegedProviderReadsTheRecords(t *testing.T) {
	f := newFakeHost(t)
	h := f.host(nil)
	if got, err := privilegedProvider(t.Context(), h); got != "" || err != nil {
		t.Fatalf("no records = %q, %v", got, err)
	}
	f.write(t, "/var/lib/shard/sandboxes/a1/sandbox.json", "{")
	f.write(t, "/var/lib/shard/sandboxes/.b2/sandbox.json", `{"provider": "runc"}`)
	if got, err := privilegedProvider(t.Context(), h); got != "" || !errors.As(err, new(*sandboxstate.UnreadableError)) {
		t.Fatalf("an undecodable record = %q, %v", got, err)
	}
	f.write(t, "/var/lib/shard/sandboxes/c3/sandbox.json", "{\n  \"provider\": \"gvisor\"\n}\n")
	if got, err := privilegedProvider(t.Context(), h); got != GVisor || err != nil {
		t.Fatalf("records = %q, %v, want gvisor", got, err)
	}
}

func TestPreflightServiceSupportOnAMac(t *testing.T) {
	l := newLocalHost(t)
	h := l.mac()
	l.remove("/usr/bin/plutil")
	swap(t, &kernelURL, func(string) (string, error) { return l.rs.URL + "/download/v0.1.0/shard-init-linux-amd64", nil })

	f, _ := preflightOn(t, h, Local{Provider: VZ, StartAtBoot: true})

	wantFinding(t, f, "Background service support", false,
		"Setup configures a launchd service, and this machine has no plutil.", "Run shard setup again and choose No for automatic startup.")
}
