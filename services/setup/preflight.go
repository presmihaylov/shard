package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/services/kernel"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// finding is what one preflight check found: a failure stops setup before any change, attention is shown and setup goes on.
type finding struct {
	check string
	lines []string
	// attention is a fact the person should know that does not stop setup.
	attention bool
	// provider marks a failure another provider may not have, so setup offers the provider question again.
	provider bool
}

func failed(lines ...string) *finding { return &finding{lines: lines} }

func providerFailed(lines ...string) *finding { return &finding{lines: lines, provider: true} }

func attention(lines ...string) *finding { return &finding{lines: lines, attention: true} }

type check struct {
	title string
	run   func(ctx context.Context, h Host, l Local) *finding
}

// The seams a test swaps, since the real ones read the host's mounts or reach the network.
var (
	checkChrootBase = fcapi.CheckChrootBase
	kernelURL       = kernel.URL
	// rootUID owns what a root service runs; a test sets its own uid, since it cannot chown to root.
	rootUID = 0
)

// The filesystems that clone a disk by reference, by their statfs magic.
const (
	xfsMagic   = 0x58465342
	btrfsMagic = 0x9123683e
)

// firecrackerImageRoom is twice the smallest data image, since datadir sizes the image at half the free space.
const firecrackerImageRoom = 20 << 30

// lowDisk is where the disk check warns: one image and a few sandboxes no longer fit.
const lowDisk = 2 << 30

// preflight is §8: it checks this host for l, changes nothing, and returns the first failure.
func (s *Setup) preflight(ctx context.Context, l Local) (*finding, error) {
	checks := []check{
		{"Supported operating system and CPU", supportedPlatform},
		{"Provider requirements", providerRequirements},
		{"Administrator access", administratorAccess},
		{"Installation paths and permissions", installPaths},
		{"Available disk space and filesystem support", diskSpace},
		{"Download access", downloadAccess},
		{"Existing Shard installation", existingSandboxes},
	}
	if l.StartAtBoot {
		checks = append(checks, check{"Background service support", serviceSupport})
	}

	titles := make([]string, 0, len(checks))
	for _, c := range checks {
		titles = append(titles, c.title)
	}
	list, err := s.UI.Checklist("Checking this machine", titles)
	if err != nil {
		return nil, err
	}
	for i, c := range checks {
		if err := list.Start(i); err != nil {
			return nil, err
		}
		f := c.run(ctx, s.Host, l)
		switch {
		case f == nil:
			err = list.Done(i)
		case f.attention:
			err = list.Attention(i, f.lines...)
		default:
			f.check = c.title
			return f, list.Fail(i, f.lines...)
		}
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}

// supportedPlatform is what a release ships for: shard and shard-init for Linux on x86_64, and the Mac build with VZ.
func supportedPlatform(_ context.Context, h Host, _ Local) *finding {
	if h.OS == "linux" && h.Arch == "amd64" || h.OS == "darwin" && h.Arch == "arm64" {
		return nil
	}

	return failed(
		"Shard runs on Linux on x86_64, and on Macs with Apple silicon.",
		fmt.Sprintf("This machine runs %s on %s.", osName(h.OS), h.Arch),
	)
}

func providerRequirements(ctx context.Context, h Host, l Local) *finding {
	title := providerTitle(l.Provider)
	if lk, ok := lacks(ctx, h, l.Provider); ok {
		return providerFailed(title+" requires "+lk.need+".", lk.fact)
	}
	m := missingTools(h, l.Provider)
	if len(m.manual) > 0 {
		return providerFailed(
			fmt.Sprintf("%s requires %s, which setup cannot install.", title, strings.Join(m.manual, ", ")),
			"Install it and run shard setup again.",
		)
	}
	if _, ok := lookPath(h, "apt-get"); m.apt() && !ok {
		return providerFailed(
			fmt.Sprintf("%s requires %s, and setup installs them with apt-get, which this machine does not have.", title, strings.Join(m.names(), ", ")),
			"Install them and run shard setup again.",
		)
	}
	if l.Provider != Firecracker {
		return nil
	}
	// Setup keeps a firecracker that is already there, so it must be one the daemon accepts.
	binary, ok := lookPath(h, "firecracker")
	if !ok {
		return nil
	}
	out, err := h.Run(ctx, rooted(h, binary), "--version")
	if err == nil {
		err = fcapi.CheckVersionOutput(binary, out)
	}
	if err != nil {
		return providerFailed(sentence(err), "Replace it with Firecracker 1.13.0 or newer, or remove it so setup installs one.")
	}

	return nil
}

func administratorAccess(ctx context.Context, h Host, _ Local) *finding {
	if h.OS == "darwin" && h.Euid == 0 && h.Env("SUDO_USER") == "" {
		return failed(
			"On a Mac the daemon runs as your user, and setup cannot tell who that is when it runs as root.",
			"Run shard setup as your user. It asks for administrator access when it needs it.",
		)
	}
	if h.Euid == 0 {
		return nil
	}
	if _, ok := lookPath(h, "sudo"); !ok {
		return failed("Setup needs administrator access, and this machine has no sudo.", "Run shard setup as root.")
	}

	// Setup runs its commands with sudo -n, so a rule that runs them without a password passes even where -v wants one.
	if _, err := h.Run(ctx, "sudo", "-n", "true"); err == nil {
		return nil
	}
	// LC_ALL=C keeps the words read below, and -v, unlike a command or -l, refuses a user outside sudoers before it wants a password.
	out, err := h.Run(ctx, "env", "LC_ALL=C", "sudo", "-n", "-v")
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "a password is required") {
		if terminal(h) {
			return nil
		}
		return failed(
			"Setup needs administrator access, and sudo needs a password it has no terminal to ask for.",
			"Run shard setup as root, or as a user with passwordless sudo.",
		)
	}

	said := strings.TrimPrefix(outputTail(out), ": ")
	if said == "" {
		said = err.Error()
	}
	return failed("Setup needs administrator access, and sudo does not allow this user: "+strings.TrimSuffix(said, ".")+".", notAllowedHint)
}

const notAllowedHint = "Ask an administrator to give your user sudo access, or run shard setup as root."

// notInSudoers says sudo refused the user itself, which no password and no terminal fixes.
func notInSudoers(said string) bool {
	return strings.Contains(said, "may not run sudo") || strings.Contains(said, "not in the sudoers file")
}

// terminal says /dev/tty opens, which is where sudo asks for a password.
func terminal(h Host) bool {
	f, err := os.OpenFile(rooted(h, "/dev/tty"), os.O_RDWR, 0)
	if err != nil {
		return false
	}

	return f.Close() == nil
}

// installPaths refuses a place a person other than root could change what the root daemon runs or keeps.
func installPaths(_ context.Context, h Host, _ Local) *finding {
	if h.OS == "linux" {
		if f := rootOnly(h, binDir); f != nil {
			return f
		}
	}
	info, err := os.Lstat(rooted(h, DataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return failed(fmt.Sprintf("Setup could not check %s: %v.", DataDir, err))
	}
	if !info.IsDir() {
		return failed(DataDir+" exists and is not a directory.", "Move it aside and run shard setup again.")
	}
	if h.OS != "darwin" {
		return nil
	}
	// On a Mac the daemon runs as the person, and an existing root they do not own refuses its writes.
	uid, err := personUID(h)
	if err != nil {
		return failed(fmt.Sprintf("Setup could not tell which user runs the daemon: %v.", err))
	}
	if owner(info) != uid {
		return failed(
			DataDir+" belongs to another user, and the daemon runs as "+daemonUser(h)+".",
			"Change its owner to "+daemonUser(h)+" and run shard setup again.",
		)
	}

	return nil
}

func rootOnly(h Host, dir string) *finding {
	info, err := os.Stat(rooted(h, dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return failed(fmt.Sprintf("Setup could not check %s: %v.", dir, err))
	}
	if !info.IsDir() {
		return failed(dir+" exists and is not a directory.", "Move it aside and run shard setup again.")
	}
	if owner(info) != rootUID || info.Mode().Perm()&0o022 != 0 {
		return failed(
			dir+" can be changed by a user other than root, and the root daemon runs Shard from it.",
			"Make root its owner and remove group and other write access, then run shard setup again.",
		)
	}

	return nil
}

func owner(info fs.FileInfo) int {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}

	return int(st.Uid)
}

// personUID is the uid of daemonUser: the caller, or the one sudo started setup for.
func personUID(h Host) (int, error) {
	if h.Euid != 0 {
		return h.Euid, nil
	}
	uid, err := strconv.Atoi(h.Env("SUDO_UID"))
	if err != nil {
		return 0, fmt.Errorf("read SUDO_UID: %w", err)
	}

	return uid, nil
}

func diskSpace(_ context.Context, h Host, l Local) *finding {
	dir, err := nearestDir(rooted(h, DataDir))
	if err != nil {
		return failed(fmt.Sprintf("Setup could not check %s: %v.", DataDir, err))
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return failed(fmt.Sprintf("Setup could not read the filesystem of %s: %v.", DataDir, err))
	}
	free := uint64(st.Bavail) * uint64(st.Bsize) //nolint:gosec // G115: a block size is never negative
	reflink := int64(st.Type) == xfsMagic || int64(st.Type) == btrfsMagic
	if h.OS == "linux" && l.Provider == Firecracker {
		if f := firecrackerDisk(h, dir, free, reflink); f != nil {
			return f
		}
	}
	if free < lowDisk {
		return attention(fmt.Sprintf("Only %s is free for %s.", gib(free), DataDir), "Images and sandboxes may not fit.")
	}

	return nil
}

// firecrackerDisk is datadir's rule ahead of time: a root that cannot clone gets an XFS image, which needs the room and an empty dir.
func firecrackerDisk(h Host, dir string, free uint64, reflink bool) *finding {
	// The type stands in for reflink.Probe, which writes a file and so needs root.
	if reflink {
		if err := checkChrootBase(dir); err != nil {
			return providerFailed("Firecracker cannot run its jailer under "+DataDir+".", sentence(err))
		}

		return nil
	}
	if free < firecrackerImageRoom {
		return providerFailed(
			"Firecracker needs "+DataDir+" on XFS or Btrfs, or "+gib(firecrackerImageRoom)+" free beside it for an XFS image.",
			fmt.Sprintf("%s has %s free, on a filesystem that cannot clone a disk.", strings.TrimPrefix(dir, strings.TrimSuffix(h.Root, "/")), gib(free)),
		)
	}
	entries, err := os.ReadDir(rooted(h, DataDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.Is(err, fs.ErrPermission):
		return attention("Setup cannot read "+DataDir+" without administrator access.", "Firecracker mounts an XFS image over it, and the daemon refuses if it holds files.")
	case err != nil:
		return failed(fmt.Sprintf("Setup could not read %s: %v.", DataDir, err))
	case len(entries) > 0:
		return providerFailed(
			"Firecracker mounts an XFS image over "+DataDir+", so it must be empty on a filesystem that cannot clone a disk.",
			DataDir+" already holds files.",
		)
	}

	return nil
}

// nearestDir is dir, or the closest directory above it that exists.
func nearestDir(dir string) (string, error) {
	for {
		_, err := os.Stat(dir)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || dir == filepath.Dir(dir) {
			return "", err
		}
		dir = filepath.Dir(dir)
	}
}

func gib(n uint64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// downloadAccess reaches every file setup or the first daemon start downloads, before it changes anything.
func downloadAccess(ctx context.Context, h Host, l Local) *finding {
	var urls []string
	for _, d := range missingTools(h, l.Provider).downloads {
		urls = append(urls, d.URL)
	}
	if h.OS == "linux" {
		u, err := AssetURL(ctx, h, h.Version, "shard-init-linux-"+h.Arch)
		if err != nil {
			return failed(fmt.Sprintf("Setup could not find shard-init for Shard %s: %v.", h.Version, err))
		}
		urls = append(urls, u)
	}
	if l.Provider == Firecracker || l.Provider == VZ {
		u, err := kernelURL(h.Arch)
		if err != nil {
			return failed(sentence(err))
		}
		urls = append(urls, u)
	}
	for _, u := range urls {
		if err := reach(ctx, h, u); err != nil {
			return failed(fmt.Sprintf("Could not download %s: %v.", path.Base(u), err), "Check the network connection and run shard setup again.")
		}
	}

	return nil
}

// reach asks for the first byte of u, which proves the file is there without downloading it.
func reach(ctx context.Context, h Host, u string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("the server answered %s", resp.Status)
	}

	return nil
}

// existingSandboxes refuses a provider other than the one the sandboxes already in the data dir use.
func existingSandboxes(_ context.Context, h Host, l Local) *finding {
	recorded, err := sandboxstate.RecordedProvider(rooted(h, DataDir))
	if errors.Is(err, fs.ErrPermission) {
		return attention("Setup cannot read "+DataDir+" without administrator access.", "The daemon refuses to start if its sandboxes use another provider.")
	}
	if err != nil {
		return failed(fmt.Sprintf("Setup could not read the sandboxes in %s: %v.", DataDir, err))
	}
	if recorded == "" || recorded == l.Provider {
		return nil
	}

	return providerFailed(
		"The sandboxes in "+DataDir+" use "+providerTitle(recorded)+".",
		"Setup never changes the provider of existing sandboxes.",
	)
}

func serviceSupport(_ context.Context, h Host, _ Local) *finding {
	manager, tools := "launchd", []string{"launchctl", "plutil"}
	if h.OS == "linux" {
		manager, tools = "systemd", []string{"systemctl", "systemd-analyze"}
		// systemd makes this directory when it boots as PID 1, and only then.
		if info, err := os.Stat(rooted(h, "/run/systemd/system")); err != nil || !info.IsDir() {
			return noService("Setup configures a systemd service, and systemd does not manage this machine.")
		}
	}
	for _, t := range tools {
		if _, ok := lookPath(h, t); !ok {
			return noService(fmt.Sprintf("Setup configures a %s service, and this machine has no %s.", manager, t))
		}
	}

	return nil
}

func noService(line string) *finding {
	return failed(line, "Run shard setup again and choose No for automatic startup.")
}

// sentence is an error worded as a line of output.
func sentence(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}

	return strings.ToUpper(s[:1]) + s[1:] + "."
}
