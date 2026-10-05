package setup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/presmihaylov/shard/pkg/tarball"
	"github.com/presmihaylov/shard/services/client"
)

// The paths a local setup installs; a root service only ever runs root-owned files from them.
const (
	binDir       = "/usr/local/bin"
	shardBinary  = binDir + "/shard"
	initBinary   = binDir + "/shard-init"
	systemdUnit  = "/etc/systemd/system/shard.service"
	launchdPlist = "/Library/LaunchDaemons/shard.daemon.plist"
	newsyslog    = "/etc/newsyslog.d/shard.conf"
	macLogDir    = "/var/log/shard"
	launchdLabel = "system/shard.daemon"
)

// The bounds on one pinned archive, far above what any of them holds.
const (
	maxArchiveBytes   = 1 << 30
	maxArchiveEntries = 256
)

// verifyWait covers a first Firecracker start, which fetches the guest kernel and formats the data image.
var (
	verifyWait = 2 * time.Minute
	verifyPoll = time.Second
)

// rooted is p on the host setup works on, which a test moves under a temp dir.
func rooted(h Host, p string) string { return filepath.Join(h.Root, p) }

// privileged runs one command as root: as it is when setup runs as root, else under sudo, which admin has already let in.
func privileged(ctx context.Context, h Host, name string, args ...string) ([]byte, error) {
	if h.Euid == 0 {
		return run(ctx, h, name, args...)
	}

	return run(ctx, h, "sudo", append([]string{"-n", "--", name}, args...)...)
}

// run is one command whose failure carries what it printed.
func run(ctx context.Context, h Host, name string, args ...string) ([]byte, error) {
	out, err := h.Run(ctx, name, args...)
	if err != nil {
		return out, fmt.Errorf("%s: %w%s", strings.Join(append([]string{name}, args...), " "), err, outputTail(out))
	}

	return out, nil
}

// outputTail is the last line a command printed, which is where a tool says why it failed.
func outputTail(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return ""
	}

	return ": " + last
}

// admin makes sure the privileged steps that follow run: sudo asks for the password once, on the terminal, before any checklist draws.
func (s *Setup) admin(ctx context.Context) error {
	if s.Host.Euid == 0 {
		return nil
	}
	if _, err := s.Host.Run(ctx, "sudo", "-n", "true"); err == nil {
		return nil
	}
	if err := s.UI.Print("Setup needs administrator access. sudo may ask for your password."); err != nil {
		return err
	}
	if _, err := run(ctx, s.Host, "sudo", "-v"); err != nil {
		return &Problem{Lines: []string{"Administrator access failed: " + strings.TrimSuffix(err.Error(), ".") + ".", sudoHint(s.Host, err)}}
	}

	return nil
}

// sudoHint names the fix for the cause sudo gave, since a terminal fixes only a missing one.
func sudoHint(h Host, err error) string {
	if notInSudoers(err.Error()) {
		return notAllowedHint
	}
	if !terminal(h) {
		return "Run shard setup in a terminal where sudo can ask for your password, or as root."
	}

	return "Run shard setup again and give sudo your password, or run it as root."
}

// localPlan is one local setup worked out against the host before any change, which its steps then carry out.
type localPlan struct {
	h       Host
	local   Local
	missing missing
	// initAsset is the release file shard-init comes from, and "" on a Mac, where the daemon embeds it.
	initAsset string
	// user runs the daemon on a Mac and owns its root there.
	user string
	// stage holds the verified downloads between the download step and the install step.
	stage string
}

func newLocalPlan(h Host, l Local) *localPlan {
	p := &localPlan{h: h, local: l, missing: missingTools(h, l.Provider)}
	if h.OS == "linux" {
		p.initAsset = "shard-init-linux-" + h.Arch
	}
	if h.OS == "darwin" {
		p.user = daemonUser(h)
	}

	return p
}

// daemonUser is the person setup runs for, also when they started it with sudo.
func daemonUser(h Host) string {
	if h.Euid == 0 {
		return h.Env("SUDO_USER")
	}

	return h.Env("USER")
}

// localSteps are the steps that set up l on this host, in checklist order; each is safe to run again.
func (s *Setup) localSteps(_ context.Context, l Local) ([]Step, error) {
	p := newLocalPlan(s.Host, l)
	steps := []Step{{Title: "Check host compatibility", Do: p.compatible}}
	if len(p.missing.downloads) > 0 || p.initAsset != "" {
		steps = append(steps, Step{Title: "Download and verify required tools", Do: p.download})
	}
	steps = append(steps,
		Step{Title: "Install " + providerTitle(l.Provider), Do: p.install},
		Step{Title: "Create Shard's data directory", Do: p.dataDir},
	)
	if !l.StartAtBoot {
		return steps, nil
	}

	return append(steps,
		Step{Title: "Configure the background service", Do: p.service},
		Step{Title: "Start the daemon", Do: p.start},
		Step{Title: "Verify the daemon connection", Do: func(ctx context.Context) error { return verifyDaemon(ctx, p.h) }},
	), nil
}

// compatible repeats the checks a change could have undone since preflight: the provider still runs here, and root is still in reach.
func (p *localPlan) compatible(ctx context.Context) error {
	if l, ok := lacks(ctx, p.h, p.local.Provider); ok {
		return &Problem{Lines: []string{providerTitle(p.local.Provider) + " requires " + l.need + ".", l.fact}}
	}
	if _, err := privileged(ctx, p.h, "true"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Administrator access failed: %v.", err)}}
	}

	return nil
}

func (p *localPlan) download(ctx context.Context) (err error) {
	stage, err := os.MkdirTemp("", "shard-setup-")
	if err != nil {
		return fmt.Errorf("create a download directory: %w", err)
	}
	p.stage = stage
	defer func() {
		if err != nil {
			err = errors.Join(err, p.clean())
		}
	}()

	for _, d := range p.missing.downloads {
		if err := fetchPinned(ctx, p.h, d, stage); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not download %s: %s.", d.Title, downloadCause(err))}}
		}
	}
	if p.initAsset == "" {
		return nil
	}
	if err := FetchAsset(ctx, p.h, p.h.Version, p.initAsset, filepath.Join(stage, "shard-init"), 0o755); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not download shard-init: %s.", downloadCause(err))}}
	}

	return nil
}

// downloadCause words a network failure as client.DialCause does, so no socket address reaches the screen and every timeout reads the same.
func downloadCause(err error) string {
	if _, ok := errors.AsType[net.Error](err); ok {
		return client.DialCause(err)
	}

	return err.Error()
}

func (p *localPlan) clean() error {
	if p.stage == "" {
		return nil
	}
	stage := p.stage
	p.stage = ""
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove the download directory: %w", err)
	}

	return nil
}

// fetchPinned downloads d into stage and refuses it unless it hashes to its pin; an archive is unpacked under stage/<Title>.
func fetchPinned(ctx context.Context, h Host, d *download, stage string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", d.URL, resp.Status)
	}

	file := filepath.Join(stage, path.Base(d.URL))
	if err := saveVerified(resp.Body, file, d.SHA256); err != nil {
		return err
	}
	if d.deb() {
		return nil
	}

	return unpack(file, filepath.Join(stage, d.Title))
}

func saveVerified(r io.Reader, file, want string) (err error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(r, maxArchiveBytes)); err != nil {
		return fmt.Errorf("save %s: %w", filepath.Base(file), err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("its sha256 is %s, and setup expects %s", got, want)
	}

	return nil
}

// unpack extracts a .tgz or a .tar.zstd, which is how the pinned releases ship.
func unpack(file, dst string) (err error) {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	var tar io.Reader
	switch {
	case strings.HasSuffix(file, ".tgz"):
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(file), gzErr)
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		tar = gz
	case strings.HasSuffix(file, ".tar.zstd"):
		zr, zErr := zstd.NewReader(f)
		if zErr != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(file), zErr)
		}
		defer zr.Close()
		tar = zr
	default:
		return fmt.Errorf("%s is no archive setup can unpack", filepath.Base(file))
	}

	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	if err := tarball.Unpack(tar, dst, tarball.Options{ConfineLinks: true, MaxBytes: maxArchiveBytes, MaxEntries: maxArchiveEntries}); err != nil {
		return fmt.Errorf("unpack %s: %w", filepath.Base(file), err)
	}

	return nil
}

func (p *localPlan) install(ctx context.Context) (err error) {
	defer func() { err = errors.Join(err, p.clean()) }()

	var owned []Owned
	if p.missing.apt() {
		if err := p.aptInstall(ctx); err != nil {
			return err
		}
	}
	for _, d := range p.missing.downloads {
		for _, entry := range slices.Sorted(maps.Keys(d.Files)) {
			dst := d.Files[entry]
			if err := installFile(ctx, p.h, filepath.Join(p.stage, d.Title, entry), dst, "0755"); err != nil {
				return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", path.Base(dst), err)}}
			}
			owned = append(owned, Owned{Path: dst, Kind: KindTool})
		}
	}
	for _, t := range p.missing.tools {
		if t.Download != nil && !t.Download.deb() {
			continue
		}
		found, ok := lookPath(p.h, t.Name)
		if !ok {
			return &Problem{Lines: []string{fmt.Sprintf("%s is still missing after its package installed.", t.Name)}}
		}
		owned = append(owned, Owned{Path: found, Kind: KindTool, Package: toolPackage(t)})
	}

	binaries, err := p.installShard(ctx)
	if err != nil {
		return err
	}

	return RecordOwned(ctx, p.h, p.local, append(owned, binaries...)...)
}

// toolPackage is the apt package uninstall names for a tool setup installed.
func toolPackage(t tool) string {
	if t.Download != nil && t.Download.deb() {
		return strings.TrimSuffix(strings.Split(path.Base(t.Download.URL), "_")[0], ".deb")
	}

	return t.Package
}

func (p *localPlan) aptInstall(ctx context.Context) error {
	if _, err := privileged(ctx, p.h, "apt-get", "update"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not update the package lists: %v.", err)}}
	}
	args := []string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "--no-install-recommends"}
	args = append(args, p.missing.packages...)
	for _, d := range p.missing.downloads {
		if d.deb() {
			args = append(args, filepath.Join(p.stage, path.Base(d.URL)))
		}
	}
	if _, err := privileged(ctx, p.h, "env", args...); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", strings.Join(p.missing.names(), ", "), err)}}
	}

	return nil
}

// installShard puts shard, and shard-init on Linux, where the service runs them: root-owned, since a root service never runs a file its user can change.
func (p *localPlan) installShard(ctx context.Context) ([]Owned, error) {
	var owned []Owned
	if p.h.Executable != shardBinary {
		if err := installFile(ctx, p.h, p.h.Executable, shardBinary, "0755"); err != nil {
			return nil, &Problem{Lines: []string{fmt.Sprintf("Could not install shard in %s: %v.", binDir, err)}}
		}
		owned = append(owned, Owned{Path: shardBinary, Kind: KindBinary})
	}
	if p.initAsset == "" {
		return owned, nil
	}
	if err := installFile(ctx, p.h, filepath.Join(p.stage, "shard-init"), initBinary, "0755"); err != nil {
		return nil, &Problem{Lines: []string{fmt.Sprintf("Could not install shard-init in %s: %v.", binDir, err)}}
	}

	return append(owned, Owned{Path: initBinary, Kind: KindBinary}), nil
}

// installFile copies src to dst as root, with the directory above it; install replaces a file in use without writing into it.
func installFile(ctx context.Context, h Host, src, dst, mode string) error {
	if _, err := privileged(ctx, h, "install", "-d", "-o", "0", "-g", "0", "-m", "0755", rooted(h, path.Dir(dst))); err != nil {
		return err
	}
	_, err := privileged(ctx, h, "install", "-o", "0", "-g", "0", "-m", mode, src, rooted(h, dst))

	return err
}

// dataDir makes the root the daemon serves, and leaves one that is there as it is.
func (p *localPlan) dataDir(ctx context.Context) error {
	var owned []Owned
	dirs := []string{DataDir}
	if p.h.OS == "darwin" {
		dirs = append(dirs, macLogDir)
	}
	for _, dir := range dirs {
		made, err := p.makeDir(ctx, dir)
		if err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not create %s: %v.", dir, err)}}
		}
		if made {
			owned = append(owned, Owned{Path: dir, Kind: KindData})
		}
	}

	return RecordOwned(ctx, p.h, p.local, owned...)
}

func (p *localPlan) makeDir(ctx context.Context, dir string) (bool, error) {
	_, err := os.Lstat(rooted(p.h, dir))
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	// The daemon's own MkdirAll makes 0750 root on Linux; on a Mac the daemon runs as the person, who owns its root.
	args := []string{"-d", "-o", "0", "-g", "0", "-m", "0750", rooted(p.h, dir)}
	if p.h.OS == "darwin" {
		args = []string{"-d", "-o", p.user, "-m", "0755", rooted(p.h, dir)}
	}
	if _, err := privileged(ctx, p.h, "install", args...); err != nil {
		return false, err
	}

	return true, nil
}

// service writes the unit or the LaunchDaemon, and has the service manager check it before it is enabled.
func (p *localPlan) service(ctx context.Context) (err error) {
	dir, err := os.MkdirTemp("", "shard-setup-")
	if err != nil {
		return fmt.Errorf("create a staging directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove the staging directory: %w", rmErr))
		}
	}()

	if p.h.OS == "darwin" {
		return p.launchdService(ctx, dir)
	}

	return p.systemdService(ctx, dir)
}

func (p *localPlan) systemdService(ctx context.Context, dir string) error {
	unit := filepath.Join(dir, "shard.service")
	if err := os.WriteFile(unit, []byte(systemdUnitText(p.local.Provider)), 0o600); err != nil {
		return err
	}
	if _, err := run(ctx, p.h, "systemd-analyze", "verify", unit); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("systemd refused the service definition: %v.", err)}}
	}
	if err := installFile(ctx, p.h, unit, systemdUnit, "0644"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", systemdUnit, err)}}
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "shard.service"}} {
		if _, err := privileged(ctx, p.h, "systemctl", args...); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not enable the service: %v.", err)}}
		}
	}

	return RecordOwned(ctx, p.h, p.local, Owned{Path: systemdUnit, Kind: KindService})
}

func (p *localPlan) launchdService(ctx context.Context, dir string) error {
	plist := filepath.Join(dir, "shard.daemon.plist")
	if err := os.WriteFile(plist, []byte(launchdPlistText(p.user)), 0o600); err != nil {
		return err
	}
	if _, err := run(ctx, p.h, "plutil", "-lint", plist); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("launchd refused the service definition: %v.", err)}}
	}
	rotate := filepath.Join(dir, "shard.conf")
	if err := os.WriteFile(rotate, []byte(newsyslogText(p.user)), 0o600); err != nil {
		return err
	}
	for _, f := range []struct{ src, dst string }{{plist, launchdPlist}, {rotate, newsyslog}} {
		if err := installFile(ctx, p.h, f.src, f.dst, "0644"); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not install %s: %v.", f.dst, err)}}
		}
	}

	return RecordOwned(ctx, p.h, p.local, Owned{Path: launchdPlist, Kind: KindService}, Owned{Path: newsyslog, Kind: KindConfig})
}

func (p *localPlan) start(ctx context.Context) error {
	if p.h.OS != "darwin" {
		if _, err := privileged(ctx, p.h, "systemctl", "start", "shard.service"); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not start the daemon: %v.", err)}}
		}

		return nil
	}

	// A retry finds the job already loaded, and bootstrap refuses a loaded one.
	if _, err := privileged(ctx, p.h, "launchctl", "print", launchdLabel); err != nil {
		if _, err := privileged(ctx, p.h, "launchctl", "bootstrap", "system", rooted(p.h, launchdPlist)); err != nil {
			return &Problem{Lines: []string{fmt.Sprintf("Could not start the daemon: %v.", err)}}
		}

		return nil
	}
	if _, err := privileged(ctx, p.h, "launchctl", "kickstart", launchdLabel); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("Could not start the daemon: %v.", err)}}
	}

	return nil
}

// verifyDaemon asks the daemon for its status until it answers, through sudo on Linux where the socket is root's; --remote "" keeps any remote out of it.
func verifyDaemon(ctx context.Context, h Host) error {
	ask := privileged
	if h.OS == "darwin" {
		ask = run
	}
	deadline := time.Now().Add(verifyWait)
	for {
		out, err := ask(ctx, h, shardBinary, "--remote", "", "daemon", "status")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return &Problem{Lines: []string{
				fmt.Sprintf("The daemon is not ready after %s: %s.", verifyWait, notReady(out, err)),
				logHint(h),
			}}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyPoll):
		}
	}
}

// notReady is the reason daemon status printed, without the command line behind it, else the error whole.
func notReady(out []byte, err error) string {
	if reason := strings.TrimPrefix(strings.TrimPrefix(outputTail(out), ": "), "shard: "); reason != "" {
		return reason
	}

	return err.Error()
}

func logHint(h Host) string {
	if h.OS == "darwin" {
		return "Read its log in " + macLogDir + "/daemon.log."
	}

	return "Read its log with: sudo journalctl -u shard.service"
}

// systemdUnitText is packaging/systemd/shard.service with the provider named, so the daemon never probes for one.
func systemdUnitText(provider string) string {
	return strings.Replace(systemdUnitTemplate, systemdExecStart, systemdExecStart+" --provider "+provider, 1)
}

const systemdExecStart = "ExecStart=" + shardBinary + " daemon"

// launchdPlistText is packaging/launchd/shard.daemon.plist with the person's name put in.
func launchdPlistText(user string) string {
	return strings.ReplaceAll(launchdPlistTemplate, "__USER__", user)
}

func newsyslogText(user string) string {
	return strings.ReplaceAll(newsyslogTemplate, "__USER__", user)
}

const systemdUnitTemplate = `# shard daemon is a resident root process, installed on purpose: no one-shot verb ever spawns it.
[Unit]
Description=shard daemon, the background work beside the CLI
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/shard daemon
# A unix socket needs write to connect, so it must never sit at 0755 between listen and chmod.
UMask=0077
# runsc sits in its own session, and a sandbox outlives the daemon: a restart or a stop kills the daemon alone.
KillMode=process
Restart=on-failure
RestartSec=1

[Install]
WantedBy=multi-user.target
`

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- shard daemon is a resident process, installed on purpose: no one-shot verb ever spawns it. The LaunchDaemon mirror of packaging/systemd/shard.service. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>shard.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/shard</string>
		<string>daemon</string>
		<!-- launchd opens the log once, so the daemon opens it itself and reopens it on the SIGHUP newsyslog sends. -->
		<string>--log</string>
		<string>/var/log/shard/daemon.log</string>
	</array>
	<!-- The root under /var/lib/shard belongs to this user, so nothing runs as root; docs/mac.md puts the name in. -->
	<key>UserName</key>
	<string>__USER__</string>
	<key>RunAtLoad</key>
	<true/>
	<!-- Restart=on-failure: a clean exit stays down, a crash comes back after ThrottleInterval, as RestartSec=1. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>1</integer>
	<!-- KillMode=process: a sandbox outlives the daemon, so a stop or a restart ends the daemon and leaves every VM shim to be adopted. -->
	<key>AbandonProcessGroup</key>
	<true/>
	<!-- UMask=0077, as a decimal: a unix socket needs write to connect, so it must never sit at 0755 between listen and chmod. -->
	<key>Umask</key>
	<integer>63</integer>
	<!-- The same file, for what the daemon prints before --log takes over, and a refusal to start. -->
	<key>StandardOutPath</key>
	<string>/var/log/shard/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/shard/daemon.log</string>
</dict>
</plist>
`

const newsyslogTemplate = `# logfilename                [owner:group]   mode count size(KB) when flags pid_file                   sig_num
# macOS newsyslog keeps .0 to .count, so a count of 6 keeps seven old files.
/var/log/shard/daemon.log    __USER__:staff  600  6     10240    *    -     /var/lib/shard/daemon.pid  1
`
