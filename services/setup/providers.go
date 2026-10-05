package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/presmihaylov/shard/pkg/vz"
)

// The names shard daemon --provider takes.
const (
	Firecracker = "firecracker"
	GVisor      = "gvisor"
	Sysbox      = "sysbox"
	Runc        = "runc"
	VZ          = "vz"
)

// ProviderChoice is one row of the provider question.
type ProviderChoice struct {
	Name  string
	Title string
	Lines []string
	// Unavailable says what this host lacks, and is empty when the provider runs here.
	Unavailable []string
	Recommended bool
}

type providerText struct {
	name, title string
	lines       []string
}

const macNeed = "an Apple silicon Mac with macOS 14 or later"

var providerTexts = []providerText{
	{Firecracker, "Firecracker", []string{
		"Run each sandbox in a small virtual machine.",
		"Recommended on Linux when hardware virtualization is available.",
	}},
	{GVisor, "gVisor", []string{
		"Run sandboxes with a protective layer between their programs and Linux.",
		"Recommended on Linux without hardware virtualization.",
	}},
	{Sysbox, "Sysbox", []string{
		"Run Docker and system services inside your sandboxes.",
		"Choose this when your workload needs its own Docker environment.",
	}},
	{Runc, "runc", []string{
		"Run standard Linux containers that share the host kernel.",
		"Choose this for trusted workloads that need standard container behavior.",
	}},
	{VZ, "macOS Virtualization", []string{
		"Run each sandbox in a small Linux virtual machine on your Mac.",
		"Requires " + macNeed + ".",
	}},
}

// recommendOrder is the first available provider that wins the recommendation.
var recommendOrder = []string{VZ, Firecracker, GVisor}

// Providers lists all five providers in the order the question shows them.
func Providers(ctx context.Context, h Host) []ProviderChoice {
	rows := make([]ProviderChoice, 0, len(providerTexts))
	for _, p := range providerTexts {
		row := ProviderChoice{Name: p.name, Title: p.title, Lines: p.lines}
		if l, ok := lacks(ctx, h, p.name); ok {
			row.Lines = nil
			row.Unavailable = []string{"Requires " + l.need + ".", l.fact}
		}
		rows = append(rows, row)
	}
	for _, name := range recommendOrder {
		i := providerIndex(name)
		if len(rows[i].Unavailable) == 0 {
			rows[i].Recommended = true
			break
		}
	}

	return rows
}

func providerIndex(name string) int {
	for i, p := range providerTexts {
		if p.name == name {
			return i
		}
	}

	return -1
}

// providerTitle is the name a reader sees, and the name itself for one setup does not know.
func providerTitle(name string) string {
	i := providerIndex(name)
	if i < 0 {
		return name
	}

	return providerTexts[i].title
}

// lack is a host capability a provider needs and this host does not have; software setup can install is never one.
type lack struct{ need, fact string }

func lacks(ctx context.Context, h Host, name string) (lack, bool) {
	if name == VZ {
		return macLacks(ctx, h)
	}
	if h.OS != "linux" {
		return lack{"a Linux host", "This machine runs " + osName(h.OS) + "."}, true
	}
	if name != Firecracker {
		return lack{}, false
	}

	return kvmLacks(h)
}

func macLacks(ctx context.Context, h Host) (lack, bool) {
	if h.OS != "darwin" {
		return lack{macNeed, "This machine runs " + osName(h.OS) + "."}, true
	}
	if h.Arch != "arm64" {
		return lack{macNeed, "This Mac has an Intel processor."}, true
	}
	out, err := h.Run(ctx, "sw_vers", "-productVersion")
	if err != nil {
		return lack{macNeed, fmt.Sprintf("Setup could not read the macOS version: %v.", err)}, true
	}
	version := strings.TrimSpace(string(out))
	if !vz.SaveRestoreSupported(h.OS, h.Arch, version) {
		return lack{macNeed, "This Mac runs macOS " + version + "."}, true
	}

	return lack{}, false
}

func kvmLacks(h Host) (lack, bool) {
	const need = "access to /dev/kvm"
	f, err := os.OpenFile(filepath.Join(h.Root, "dev", "kvm"), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return lack{need, "This machine does not provide it."}, true
	}
	// Setup runs as the user, and only the root daemon has to open it.
	if errors.Is(err, fs.ErrPermission) {
		return lack{}, false
	}
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		return lack{need, fmt.Sprintf("/dev/kvm does not open: %v.", pathErr.Err)}, true
	}
	if err != nil {
		return lack{need, fmt.Sprintf("/dev/kvm does not open: %v.", err)}, true
	}
	if err := f.Close(); err != nil {
		return lack{need, fmt.Sprintf("/dev/kvm does not close: %v.", err)}, true
	}

	return lack{}, false
}

func osName(goos string) string {
	switch goos {
	case "linux":
		return "Linux"
	case "darwin":
		return "macOS"
	}

	return goos
}

// servicePath is where systemd and sudo look for a binary, which is what the daemon finds a runtime on.
var servicePath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// lookPath finds name where the root daemon looks, never on the user's own PATH.
func lookPath(h Host, name string) (string, bool) {
	for _, dir := range servicePath {
		p := path.Join(dir, name)
		info, err := os.Stat(filepath.Join(h.Root, p))
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}

	return "", false
}
