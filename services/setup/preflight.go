package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	fcapi "github.com/presmihaylov/shard/pkg/firecracker"
	"github.com/presmihaylov/shard/services/datadir"
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

// firecrackerImageRoom is the smallest data image and the space the host keeps beside it.
const firecrackerImageRoom = (datadir.MinImageMiB + datadir.HostReserveMiB) << 20

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
		{"Existing shard installation", existingSandboxes},
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
		"Supported hosts: Linux on x86_64, and Macs with Apple silicon.",
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
			dir+" can be changed by a user other than root, and the root daemon runs shard from it.",
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
	d, err := statDisk(dir)
	if err != nil {
		return failed(fmt.Sprintf("Setup could not read the filesystem of %s: %v.", DataDir, err))
	}
	free := d.avail
	if h.OS == "linux" && l.Provider == Firecracker {
		if f := firecrackerDisk(h, dir, free, d.reflink); f != nil {
			return f
		}
	}
	if free < lowDisk {
		return attention(fmt.Sprintf("Only %s is free for %s.", gib(free), DataDir), "Images and sandboxes may not fit.")
	}

	return nil
}

// firecrackerDisk is datadir's rule ahead of time: a root that cannot clone gets an XFS image, which needs the room and an empty dir.
func firecrackerDisk(h Host, dir string, free int64, reflink bool) *finding {
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

func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// downloadAccess reaches every file setup or the first daemon start downloads, before it changes anything.
func downloadAccess(ctx context.Context, h Host, l Local) *finding {
	var urls []string
	for _, d := range missingTools(h, l.Provider).downloads {
		urls = append(urls, d.URL)
	}
	if h.OS == "linux" {
		u, err := AssetURL(ctx, h, h.Version, "shard-init-linux-"+h.Arch)
		if err != nil {
			return failed(fmt.Sprintf("Setup could not find shard-init for shard %s: %s.", h.Version, downloadCause(err)))
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
			return failed(fmt.Sprintf("Could not download %s: %s.", path.Base(u), downloadCause(err)), "Check the network connection and run shard setup again.")
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

// existingSandboxes refuses a provider other than the one that made the data dir, since the daemon would refuse to start over it.
func existingSandboxes(ctx context.Context, h Host, l Local) *finding {
	owner, fact, err := rootProvider(ctx, h)
	if err != nil {
		return failed(fmt.Sprintf("Setup could not read %s: %v.", DataDir, err))
	}
	if owner == "" || owner == l.Provider {
		return nil
	}
	remove, err := deleteDataLines(h, owner)
	if err != nil {
		return failed(fmt.Sprintf("Setup could not read %s: %v.", DataDir, err))
	}

	return providerFailed(slices.Concat(
		[]string{
			fact,
			"The daemon cannot start with " + providerTitle(l.Provider) + " over that data, and setup never changes its provider.",
			"To keep the data, choose " + providerTitle(owner) + ".",
		},
		remove,
		[]string{"Then run shard setup again."},
	)...)
}

// rootProvider is the provider the daemon finds in the data dir and the line that names it: its records, else firecracker's data image, which hides them while unmounted.
func rootProvider(ctx context.Context, h Host) (string, string, error) {
	recorded, err := sandboxstate.RecordedProvider(rooted(h, DataDir))
	// A root daemon's records are root's alone, and setup has administrator access by now (rootAccess).
	if errors.Is(err, fs.ErrPermission) && h.Euid != 0 {
		recorded, err = privilegedProvider(ctx, h)
	}
	// The daemon chooses past a record it cannot read, so setup does too.
	var unreadable *sandboxstate.UnreadableError
	if err != nil && !errors.As(err, &unreadable) {
		return "", "", err
	}
	if recorded != "" {
		return recorded, "The sandboxes in " + DataDir + " use " + providerTitle(recorded) + ".", nil
	}
	image := datadir.ImagePath(DataDir)
	_, err = os.Stat(rooted(h, image))
	if err == nil {
		return Firecracker, "The data in " + DataDir + " belongs to " + providerTitle(Firecracker) + ".", nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("check %s: %w", image, err)
	}

	return "", "", nil
}

// recordsScript prints the id and the record of each sandbox in $1, each followed by a NUL.
const recordsScript = `[ -d "$1" ] || exit 0
cd "$1" || exit 1
for d in */; do
	d=${d%/}
	[ -f "$d/sandbox.json" ] || continue
	printf '%s\0' "$d" && cat -- "$d/sandbox.json" && printf '\0' || exit 1
done`

// privilegedProvider is the provider of the first record that names one, read as root.
func privilegedProvider(ctx context.Context, h Host) (string, error) {
	dir := path.Join(DataDir, "sandboxes")
	out, err := privileged(ctx, h, "sh", "-c", recordsScript, "sh", rooted(h, dir))
	if err != nil {
		return "", fmt.Errorf("read %s as root: %w", dir, err)
	}
	fields := strings.Split(string(out), "\x00")
	var unreadable error
	for i := 0; i+1 < len(fields); i += 2 {
		id := fields[i]
		if sandboxstate.ValidID(id) != nil {
			continue
		}
		var record struct {
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal([]byte(fields[i+1]), &record); err != nil {
			unreadable = errors.Join(unreadable, &sandboxstate.UnreadableError{ID: id, Err: err})
			continue
		}
		if record.Provider != "" {
			return record.Provider, nil
		}
	}

	return "", unreadable
}

// rootAccess asks for administrator access before the checks when only root can read the sandbox records; any other read error is preflight's to report.
func (s *Setup) rootAccess(ctx context.Context) error {
	_, err := sandboxstate.RecordedProvider(rooted(s.Host, DataDir))
	if !errors.Is(err, fs.ErrPermission) {
		return nil
	}

	return s.admin(ctx)
}

// deleteDataLines are the commands that delete the data dir, after which the daemon starts over it with any provider.
func deleteDataLines(h Host, owner string) ([]string, error) {
	where, free, err := dataImageFacts(h)
	if err != nil {
		return nil, err
	}
	installed, err := installedInBin(h)
	if err != nil {
		return nil, err
	}
	sudo := sudoFor(h)
	// Without the installed binaries the daemon cannot start, so setup must install them before anything can remove the sandboxes. (SHARD-742)
	if !installed {
		return deleteAfterReinstall(owner, where, free, sudo), nil
	}

	// A plain delete leaves a stopped sandbox's netns, veth and cgroup behind, so remove the sandboxes through their own daemon first.
	lines := append(where,
		"To delete the saved data, first remove its sandboxes so their network and cgroups go too:", "",
		"  Start the daemon on that data:", "    "+sudo+"shard daemon --provider "+owner, "",
		"  List sandboxes:", "    "+sudo+"shard list --all", "",
		"  Remove a sandbox:", "    "+sudo+"shard remove --force <name>", "",
	)
	if where == nil {
		return append(lines, "  Then stop that daemon and delete the data:", "    "+sudo+"rm -r "+DataDir), nil
	}

	// An image-backed root has sandboxes too, and an active mount would block the umount, so stop the daemon before the free.
	lines = append(lines, "  Then stop that daemon and free the disk:")
	for _, c := range free {
		lines = append(lines, "    "+c)
	}

	return lines, nil
}

// installedInBin reports whether setup already put shard in /usr/local/bin, so the daemon the delete steps start is on root's PATH and can run. (SHARD-742)
func installedInBin(h Host) (bool, error) {
	if _, err := os.Lstat(rooted(h, shardBinary)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check %s: %w", shardBinary, err)
	}

	return true, nil
}

// deleteAfterReinstall is the delete path when shard is not installed: choose the owner so setup installs it, then remove the sandboxes, uninstall, and free the disk. (SHARD-742)
func deleteAfterReinstall(owner string, where, free []string, sudo string) []string {
	lines := append(where,
		"To delete the saved data, let setup install shard first, so its daemon can remove the sandboxes:", "",
		"  Choose "+providerTitle(owner)+". Setup installs shard and starts its daemon over this data.", "",
		"  Remove every sandbox:", "    "+sudo+"shard list --all", "    "+sudo+"shard remove --force <name>", "",
		"  Then uninstall shard and free the disk:", "    shard setup   (choose "+uninstallLabel+")",
	)
	if where == nil {
		return append(lines, "    "+sudo+"rm -r "+DataDir)
	}
	for _, c := range free {
		lines = append(lines, "    "+c)
	}

	return lines
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
