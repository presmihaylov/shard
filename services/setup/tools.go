package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// tool is one program a provider's daemon runs, which setup finds where the root daemon looks or installs.
type tool struct {
	// Name is the command the daemon runs.
	Name string
	// Package is the apt package that gives it, and "" when a pinned download does or setup never installs it.
	Package string
	// Download is the pinned release that gives it.
	Download *download
}

// download is one pinned upstream release; setup checks its sha256 before it installs any file of it.
type download struct {
	// Title names it in a message, as the tool it gives.
	Title  string
	URL    string
	SHA256 string
	// Files maps an entry of the archive to the path it installs at; a .deb has none, apt installs it whole.
	Files map[string]string
}

func (d *download) deb() bool { return strings.HasSuffix(d.URL, ".deb") }

// The pins: bump each one with its sha256 from the release page, never by name alone.
var (
	gvisorRelease = &download{
		Title:  "runsc",
		URL:    "https://storage.googleapis.com/gvisor/releases/release/20260928.0/x86_64/gvisor.tar.zstd",
		SHA256: "1f2a732d12072eda6b8a64eccf9829fe9f4a1893d3292a7e8005a82089811d8d",
		// runsc execs its sidecars from gvisor-bin beside itself, so they move together; the containerd shim is for containerd only.
		Files: map[string]string{
			"runsc":                              "/usr/local/bin/runsc",
			"gvisor-bin/checkpointgofer":         "/usr/local/bin/gvisor-bin/checkpointgofer",
			"gvisor-bin/gvisor-sentry-prewarmer": "/usr/local/bin/gvisor-bin/gvisor-sentry-prewarmer",
			"gvisor-bin/gvisor_sentry":           "/usr/local/bin/gvisor-bin/gvisor_sentry",
			"gvisor-bin/runsc-fd-parking":        "/usr/local/bin/gvisor-bin/runsc-fd-parking",
			"gvisor-bin/runsc-metric-server":     "/usr/local/bin/gvisor-bin/runsc-metric-server",
		},
	}
	firecrackerRelease = &download{
		Title:  "firecracker",
		URL:    "https://github.com/firecracker-microvm/firecracker/releases/download/v1.17.0/firecracker-v1.17.0-x86_64.tgz",
		SHA256: "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
		Files: map[string]string{
			"release-v1.17.0-x86_64/firecracker-v1.17.0-x86_64": "/usr/local/bin/firecracker",
			"release-v1.17.0-x86_64/jailer-v1.17.0-x86_64":      "/usr/local/bin/jailer",
		},
	}
	sysboxRelease = &download{
		Title:  "sysbox",
		URL:    "https://github.com/nestybox/sysbox/releases/download/v0.7.1/sysbox-ce_0.7.1.linux_amd64.deb",
		SHA256: "9d6d5484f980d0a17f86c492c1262015c2afb66280bdb97215b79fde6a0261c5",
	}
)

// linuxTools are what every Linux provider runs: the sandbox network, its policy and the sandbox disks.
var linuxTools = []tool{
	{Name: "ip", Package: "iproute2"},
	{Name: "nft", Package: "nftables"},
	{Name: "mkfs.ext4", Package: "e2fsprogs"},
}

// providerTools are what the daemon of each provider runs beyond linuxTools.
var providerTools = map[string][]tool{
	Firecracker: {
		{Name: "firecracker", Download: firecrackerRelease},
		{Name: "jailer", Download: firecrackerRelease},
		{Name: "mkfs.erofs", Package: "erofs-utils"},
		{Name: "mkfs.xfs", Package: "xfsprogs"},
	},
	GVisor: {{Name: "runsc", Download: gvisorRelease}},
	Sysbox: {{Name: "sysbox-runc", Download: sysboxRelease}},
	Runc:   {{Name: "runc", Package: "runc"}},
	// codesign signs the VM shim on first use, and it ships with macOS.
	VZ: {{Name: "codesign"}},
}

// apparmorTool loads the profile runc confines a sandbox with, on a kernel that enforces AppArmor.
var apparmorTool = tool{Name: "apparmor_parser", Package: "apparmor"}

// toolsFor are the tools the daemon of provider runs on this host.
func toolsFor(h Host, provider string) []tool {
	var tools []tool
	if provider != VZ {
		tools = append(tools, linuxTools...)
	}
	tools = append(tools, providerTools[provider]...)
	if provider == Runc && apparmorEnabled(h) {
		tools = append(tools, apparmorTool)
	}

	return tools
}

func apparmorEnabled(h Host) bool {
	enabled, err := os.ReadFile(filepath.Join(h.Root, "sys", "module", "apparmor", "parameters", "enabled"))

	return err == nil && strings.HasPrefix(string(enabled), "Y")
}

// missing is what setup has to install for a provider: the absent tools, the packages and the downloads that give them.
type missing struct {
	tools     []tool
	packages  []string
	downloads []*download
	// manual are absent tools setup cannot install.
	manual []string
}

func missingTools(h Host, provider string) missing {
	var m missing
	for _, t := range toolsFor(h, provider) {
		if _, ok := lookPath(h, t.Name); ok {
			continue
		}
		m.tools = append(m.tools, t)
		switch {
		case t.Download != nil:
			if !slices.Contains(m.downloads, t.Download) {
				m.downloads = append(m.downloads, t.Download)
			}
		case t.Package != "":
			m.packages = append(m.packages, t.Package)
		default:
			m.manual = append(m.manual, t.Name)
		}
	}

	return m
}

func (m missing) names() []string {
	names := make([]string, 0, len(m.tools))
	for _, t := range m.tools {
		names = append(names, t.Name)
	}

	return names
}

// apt is whether installing m runs apt-get: a package, or a .deb it resolves the dependencies of.
func (m missing) apt() bool {
	if len(m.packages) > 0 {
		return true
	}

	return slices.ContainsFunc(m.downloads, (*download).deb)
}
