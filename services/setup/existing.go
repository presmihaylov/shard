package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/pkg/mountinfo"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/datadir"
	"github.com/presmihaylov/shard/services/kernel"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

const serviceName = "shard"

// manualPaths are where docs/daemon.md and docs/mac.md put an install by hand.
var manualPaths = []string{shardBinary, initBinary, systemdUnit, "/etc/systemd/system/shard-serve.service", launchdPlist, newsyslog}

// ServiceState is the Service line of the summary.
type ServiceState string

const (
	ServiceActive   ServiceState = "Active"
	ServiceInactive ServiceState = "Inactive"
	ServiceNone     ServiceState = "Not set up"
)

// Installation is what Detect found of an earlier local install.
type Installation struct {
	// Manifest is set when setup made the install; Manual lists what an install by hand left instead.
	Manifest *Manifest
	Manual   []string
	Service  ServiceState
}

// Detect reads the host without changing it and without administrator access.
func Detect(ctx context.Context, h Host) (Installation, bool, error) {
	m, ok, err := LoadManifest(h)
	if err != nil {
		return Installation{}, false, err
	}
	if ok {
		state, err := serviceState(ctx, h, m)
		if err != nil {
			return Installation{}, false, err
		}

		return Installation{Manifest: &m, Service: state}, true, nil
	}

	var found []string
	for _, p := range manualPaths {
		_, err := os.Lstat(filepath.Join(h.Root, p))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Installation{}, false, fmt.Errorf("check %s: %w", p, err)
		}
		found = append(found, p)
	}

	return Installation{Manual: found}, len(found) > 0, nil
}

// uninstallLabel is the §11 menu label, quoted by the delete hint so the two cannot drift. (SHARD-742)
const uninstallLabel = "Uninstall shard"

// existing is the §11 menu over an installation Detect found.
func (s *Setup) existing(ctx context.Context, inst Installation) error {
	if inst.Manifest == nil {
		return s.manual(ctx, inst)
	}

	m := *inst.Manifest
	if err := s.UI.Print("This machine already has shard installed", "", "Version:  "+m.Version, "Provider: "+providerTitle(m.Provider), "Service:  "+string(inst.Service), ""); err != nil {
		return err
	}
	choice, err := s.UI.Select(ctx, AskExisting, "What would you like to do?", []term.Option{
		{Name: "repair", Label: "Check or repair the installation", Default: true},
		{Name: "upgrade", Label: "Upgrade shard"},
		{Name: "uninstall", Label: uninstallLabel},
		{Name: "exit", Label: "Exit"},
	})
	if err != nil {
		return err
	}

	switch choice {
	case 0:
		return s.switched(ctx, func(ctx context.Context, removal string) error { return s.repair(ctx, m, inst.Service, removal) })
	case 1:
		return s.switched(ctx, func(ctx context.Context, removal string) error { return s.upgrade(ctx, m, inst.Service, removal) })
	case 2:
		return s.uninstall(ctx, m)
	}

	return nil
}

// repair re-runs the steps of the recorded choices, so the provider and the startup setting stay what they were; removal is the saved remote's review line, if any.
func (s *Setup) repair(ctx context.Context, m Manifest, service ServiceState, removal string) error {
	if err := s.sameProvider(ctx, m); err != nil {
		return err
	}
	var problems []string
	gone := map[string]bool{}
	for _, f := range m.Files {
		_, err := os.Lstat(filepath.Join(s.Host.Root, f.Path))
		if errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, f.Path+" is missing.")
			gone[filepath.Base(f.Path)] = true
			continue
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", f.Path, err)
		}
	}
	for _, name := range missingTools(s.Host, m.Provider).names() {
		if !gone[name] {
			problems = append(problems, name+" is missing.")
		}
	}
	runtime, err := s.missingRuntime(ctx, m.Provider)
	if err != nil {
		return err
	}
	for _, f := range runtime {
		problems = append(problems, f+" is missing.")
	}
	if m.StartAtBoot && service != ServiceActive {
		problems = append(problems, "The background service is not running.")
	}

	if len(problems) == 0 {
		return s.UI.Print("✓ No problems found", "", "Installed correctly: shard "+m.Version+" with "+providerTitle(m.Provider)+".")
	}

	steps, err := s.localSteps(ctx, Local{Provider: m.Provider, StartAtBoot: m.StartAtBoot})
	if err != nil {
		return err
	}
	// A running daemon built its provider already, and puts the runtime files back only when it builds it again.
	if len(runtime) > 0 && service == ServiceActive {
		restart := Step{Title: "Restart the daemon", Do: func(ctx context.Context) error { return restartService(ctx, s.Host, m) }}
		steps = slices.Insert(steps, len(steps)-1, restart)
	}
	if len(runtime) > 0 && service != ServiceNone {
		steps = append(steps, Step{Title: "Restore the provider's files", Do: func(ctx context.Context) error { return s.restoreRuntime(ctx, m.Provider) }})
	}
	lines := []string{"Setup found these problems:", ""}
	for _, p := range problems {
		lines = append(lines, "  "+p)
	}
	lines = append(lines, "", "Setup will run these steps again:")
	for _, st := range steps {
		lines = append(lines, "  "+st.Title)
	}
	if removal != "" {
		lines = append(lines, "", "Then setup will:", removal)
	}
	if len(runtime) > 0 && service == ServiceNone {
		lines = append(lines, "", "Then restart shard daemon and run shard daemon status, which puts back the files it keeps in "+DataDir+".")
	}
	lines = append(lines, "", "Your settings and sandbox data stay in place.", "")
	if err := s.UI.Print(lines...); err != nil {
		return err
	}
	if err := s.confirm(ctx, true); err != nil {
		return err
	}
	if err := s.admin(ctx); err != nil {
		return err
	}
	if err := s.apply(ctx, "Repairing shard", steps); err != nil {
		return err
	}

	done := "shard " + m.Version + " is repaired."
	if m.StartAtBoot {
		done = "shard " + m.Version + " is repaired, and the daemon is running."
	}
	return s.UI.Print("", done)
}

// runtimeFiles are what the daemon of provider writes under its root when it builds the provider: the vz shim and guest init, and a microVM's kernel.
func runtimeFiles(h Host, provider string) ([]string, error) {
	var files []string
	if provider == VZ {
		files = vzshim.Paths(filepath.Join(DataDir, vzshim.Dir))
	}
	if provider != VZ && provider != Firecracker {
		return files, nil
	}
	k, err := kernel.Path(DataDir, h.Arch)
	if err != nil {
		return nil, err
	}

	return append(files, k), nil
}

// restoreRuntime asks the daemon for its status, which builds its provider and so writes the runtime files, then checks each is back.
func (s *Setup) restoreRuntime(ctx context.Context, provider string) error {
	ask := privileged
	if s.Host.OS == "darwin" {
		ask = run
	}
	if out, err := ask(ctx, s.Host, shardBinary, "--remote", "", "daemon", "status"); err != nil {
		return &Problem{Lines: []string{fmt.Sprintf("The daemon could not start %s: %s.", providerTitle(provider), notReady(out, err))}}
	}
	missing, err := s.missingRuntime(ctx, provider)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &Problem{Lines: []string{"The daemon did not put back " + strings.Join(missing, ", ") + "."}}
	}

	return nil
}

// missingRuntime is each runtime file that is gone; on Linux the data root is root's, so the check runs with administrator access.
func (s *Setup) missingRuntime(ctx context.Context, provider string) ([]string, error) {
	h := s.Host
	files, err := runtimeFiles(h, provider)
	if err != nil || len(files) == 0 {
		return nil, err
	}
	check := run
	if h.OS == "linux" {
		if err := s.admin(ctx); err != nil {
			return nil, err
		}
		check = privileged
	}
	args := []string{"-c", `for f in "$@"; do [ -e "$f" ] || echo "$f"; done`, "sh"}
	for _, f := range files {
		args = append(args, rooted(h, f))
	}
	out, err := check(ctx, h, "sh", args...)
	if err != nil {
		return nil, fmt.Errorf("check the files of %s: %w", providerTitle(provider), err)
	}

	gone := strings.Split(strings.TrimSpace(string(out)), "\n")
	var missing []string
	for _, f := range files {
		if slices.Contains(gone, rooted(h, f)) {
			missing = append(missing, f)
		}
	}

	return missing, nil
}

// replacement is one installed binary and the release file that replaces it.
type replacement struct {
	path, asset string
	// user marks the CLI the user runs, which the user owns and replaces without administrator access.
	user bool
	tmp  string
}

// sameProvider stops before any change when another provider made the data dir, since the daemon cannot start over it.
func (s *Setup) sameProvider(ctx context.Context, m Manifest) error {
	if err := s.rootAccess(ctx); err != nil {
		return err
	}
	owner, fact, err := rootProvider(ctx, s.Host)
	if err != nil {
		return fmt.Errorf("read the provider of %s: %w", DataDir, err)
	}
	if owner == "" || owner == m.Provider {
		return nil
	}
	remove, err := deleteDataLines(s.Host, owner)
	if err != nil {
		return err
	}
	lines := slices.Concat(
		[]string{
			fact,
			"This installation uses " + providerTitle(m.Provider) + ", so the daemon cannot start over that data.",
			"To keep the data, uninstall shard, which keeps it, and run shard setup again with " + providerTitle(owner) + ".",
		},
		remove,
		[]string{"Then run shard setup again and check or repair the installation."},
	)
	if err := s.UI.Print(append(lines, "", "No installation changes were made.")...); err != nil {
		return err
	}

	return &StoppedError{Step: "Existing shard installation", Err: &Problem{Lines: lines}}
}

// upgrade fetches and verifies every binary before it asks, and replaces none until all of them passed; removal is the saved remote's review line, if any.
func (s *Setup) upgrade(ctx context.Context, m Manifest, service ServiceState, removal string) (err error) {
	if err := s.sameProvider(ctx, m); err != nil {
		return err
	}
	h := s.Host
	rel, err := LatestRelease(ctx, h)
	if err != nil {
		return err
	}
	if err := s.UI.Print("Latest release: " + rel.Tag); err != nil {
		return err
	}
	latest, _ := stableVersion(rel.Tag)
	if installed, ok := stableVersion(m.Version); ok && !newer(latest, installed) {
		return s.UI.Print("", "Up to date: shard "+m.Version+".")
	}

	targets, err := upgradeTargets(h, m)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "shard-upgrade-*")
	if err != nil {
		return fmt.Errorf("upgrade shard: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	fetch := Step{Title: "Download and verify shard " + rel.Tag, Do: func(ctx context.Context) error {
		fetched := map[string]bool{}
		for i := range targets {
			t := &targets[i]
			t.tmp = filepath.Join(dir, t.asset)
			if fetched[t.asset] {
				continue
			}
			if err := rel.Fetch(ctx, h, t.asset, t.tmp, 0o755); err != nil {
				return err
			}
			fetched[t.asset] = true
		}

		return verifyVersion(ctx, h, targets, rel.Tag)
	}}
	if err := s.apply(ctx, "Preparing the upgrade", []Step{fetch}); err != nil {
		return err
	}

	lines := []string{"", "Setup will replace:"}
	for _, t := range targets {
		lines = append(lines, "  "+t.path)
	}
	if removal != "" {
		lines = append(lines, "", "Then setup will:", removal)
	}
	lines = append(lines, "", "The provider stays "+providerTitle(m.Provider)+".")
	switch service {
	case ServiceActive:
		lines = append(lines, "The background service restarts to run "+rel.Tag+".",
			"Your sandboxes keep running while the daemon restarts.",
			"Open shard exec sessions disconnect. Their commands keep running.")
	case ServiceInactive:
		lines = append(lines, "The background service is not running. Setup does not start it.")
	case ServiceNone:
		lines = append(lines, "You start the daemon yourself. Restart shard daemon to run "+rel.Tag+".")
	}
	if err := s.UI.Print(append(lines, "")...); err != nil {
		return err
	}
	if err := s.confirm(ctx, true); err != nil {
		return err
	}
	if err := s.admin(ctx); err != nil {
		return err
	}

	steps := []Step{
		{Title: "Replace the shard binaries", Do: func(ctx context.Context) error { return replaceAll(ctx, h, targets) }},
		{Title: "Record shard " + rel.Tag, Do: func(ctx context.Context) error {
			m.Version = rel.Tag
			return saveManifest(ctx, h, m)
		}},
	}
	done := "Upgraded to shard " + rel.Tag + "."
	if service == ServiceActive {
		steps = append(steps,
			Step{Title: "Restart the daemon", Do: func(ctx context.Context) error { return restartService(ctx, h, m) }},
			Step{Title: "Verify the daemon connection", Do: func(ctx context.Context) error { return verifyDaemon(ctx, h) }},
		)
		done = "Upgraded to shard " + rel.Tag + ", and the daemon is running."
	}
	if err := s.apply(ctx, "Upgrading shard", steps); err != nil {
		return err
	}

	return s.UI.Print("", done)
}

// upgradeTargets is every binary the manifest owns, and the CLI that runs setup when the manifest does not own it.
func upgradeTargets(h Host, m Manifest) ([]replacement, error) {
	var targets []replacement
	for _, f := range m.Files {
		if f.Kind != KindBinary {
			continue
		}
		asset, err := releaseAsset(h, filepath.Base(f.Path))
		if err != nil {
			return nil, err
		}
		targets = append(targets, replacement{path: f.Path, asset: asset})
	}

	owned := slices.ContainsFunc(targets, func(r replacement) bool { return filepath.Join(h.Root, r.path) == h.Executable })
	if !owned {
		targets = append(targets, replacement{path: h.Executable, asset: "shard-" + h.OS + "-" + h.Arch, user: true})
	}

	return targets, nil
}

func releaseAsset(h Host, name string) (string, error) {
	switch name {
	case "shard", "shard-init":
		return name + "-" + h.OS + "-" + h.Arch, nil
	}

	return "", fmt.Errorf("upgrade shard: no release file replaces %s", name)
}

// verifyVersion runs each new CLI before anything is replaced; shard-init is a guest PID 1, so its checksum is its proof.
func verifyVersion(ctx context.Context, h Host, targets []replacement, tag string) error {
	for _, t := range targets {
		if strings.HasPrefix(t.asset, "shard-init-") {
			continue
		}
		out, err := run(ctx, h, t.tmp, "--version")
		if err != nil {
			return fmt.Errorf("run the new %s: %w", t.asset, err)
		}
		if got := strings.TrimSpace(string(out)); got != "client "+tag {
			return fmt.Errorf("the new %s reports %q, want %q", t.asset, got, "client "+tag)
		}
	}

	return nil
}

// replaceAll stages every file beside its target first, so one failed copy leaves no mix of versions.
func replaceAll(ctx context.Context, h Host, targets []replacement) error {
	for _, t := range targets {
		dst := filepath.Join(h.Root, t.path)
		if t.user {
			dst = t.path
		}
		if err := stage(ctx, h, t, dst+".new"); err != nil {
			return fmt.Errorf("stage %s: %w", t.path, err)
		}
	}

	for _, t := range targets {
		dst := filepath.Join(h.Root, t.path)
		if t.user {
			if err := os.Rename(t.path+".new", t.path); err != nil {
				return fmt.Errorf("replace %s: %w", t.path, err)
			}
			continue
		}
		if _, err := privileged(ctx, h, "mv", "-f", dst+".new", dst); err != nil {
			return fmt.Errorf("replace %s: %w", t.path, err)
		}
	}

	return nil
}

func stage(ctx context.Context, h Host, t replacement, next string) error {
	if t.user {
		data, err := os.ReadFile(t.tmp)
		if err != nil {
			return err
		}

		return os.WriteFile(next, data, 0o755) //nolint:gosec // the CLI the user runs must stay executable
	}

	_, err := privileged(ctx, h, "install", "-m", "0755", t.tmp, next)

	return err
}

// restartWait covers the moment after the restart command returns, while the service manager still brings the daemon up.
var restartWait = 30 * time.Second

func restartService(ctx context.Context, h Host, m Manifest) error {
	cmd := []string{"systemctl", "restart", serviceName}
	if h.OS == "darwin" {
		cmd = []string{"launchctl", "kickstart", "-k", launchdLabel}
	}
	if _, err := privileged(ctx, h, cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("restart the daemon: %w", err)
	}

	deadline := time.Now().Add(restartWait)
	for {
		state, err := serviceState(ctx, h, m)
		if err != nil {
			return err
		}
		if state == ServiceActive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon is %s %s after the restart", strings.ToLower(string(state)), restartWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyPoll):
		}
	}
}

// uninstall refuses while a sandbox is left, and keeps saved data and shared tools.
func (s *Setup) uninstall(ctx context.Context, m Manifest) error {
	h := s.Host
	if err := s.admin(ctx); err != nil {
		return err
	}
	n, err := countSandboxes(ctx, h)
	if err != nil {
		return err
	}
	if n > 0 {
		left := fmt.Sprintf("%d %s", n, plural(n, "sandbox", "sandboxes"))
		sudo := socketSudo(h)
		return errors.Join(s.UI.Print("This machine still has "+left+".",
			"Remove "+plural(n, "it", "them")+" before you uninstall shard:", "",
			"  List sandboxes:", "    "+sudo+"shard list --all", "",
			"  Remove a sandbox:", "    "+sudo+"shard remove --force <name>"),
			fmt.Errorf("uninstall stopped: %s left", left))
	}

	var steps []Step
	var clauses []string
	if slices.ContainsFunc(m.Files, func(f Owned) bool { return f.Kind == KindService }) {
		steps = append(steps, Step{Title: "Stop and remove the background service", Do: func(ctx context.Context) error { return stopService(ctx, h, m) }})
		clauses = append(clauses, "stop and remove the background service")
	}
	netHeld := false
	if h.OS == "linux" {
		steps = append(steps, Step{Title: "Remove the network bridge and firewall tables", Do: func(ctx context.Context) (err error) {
			netHeld, err = removeNetwork(ctx, h)
			return err
		}})
		clauses = append(clauses, "remove shard's network bridge and firewall tables")
	}
	// Last, since it removes the manifest that lets a failed uninstall run again.
	steps = append(steps, Step{Title: "Remove files installed by shard setup", Do: func(ctx context.Context) error { return removeOwned(ctx, h, m) }})
	clauses = append(clauses, "remove files installed by shard setup")

	if err := s.UI.Print(slices.Concat([]string{"Uninstall shard?", ""}, clauseLines(clauses), []string{"", "Your saved data will remain.", "Shared tools will remain.", ""})...); err != nil {
		return err
	}
	if err := s.confirm(ctx, false); err != nil {
		return err
	}
	if err := s.apply(ctx, "Uninstalling shard", steps); err != nil {
		return err
	}

	lines, err := uninstalled(h, m, netHeld)
	if err != nil {
		return err
	}

	return s.UI.Print(lines...)
}

// clauseLines joins the clauses into one sentence, a clause per line: "This will a," "b," "and c."
func clauseLines(clauses []string) []string {
	lines := slices.Clone(clauses)
	last := len(lines) - 1
	if last > 1 {
		for i := range last {
			lines[i] += ","
		}
	}
	if last > 0 {
		lines[last] = "and " + lines[last]
	}
	lines[0] = "This will " + lines[0]
	lines[last] += "."

	return lines
}

// countSandboxes reads the records with administrator access, since the data root is not the user's on Linux.
func countSandboxes(ctx context.Context, h Host) (int, error) {
	root := filepath.Join(h.Root, DataDir)
	_, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("check %s: %w", DataDir, err)
	}

	out, err := privileged(ctx, h, "find", root, "-mindepth", "3", "-maxdepth", "3", "-name", "sandbox.json")
	if err != nil {
		return 0, fmt.Errorf("list the sandboxes in %s: %w", DataDir, err)
	}

	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		dir := filepath.Dir(line)
		if filepath.Base(filepath.Dir(dir)) == "sandboxes" && sandboxstate.ValidID(filepath.Base(dir)) == nil {
			n++
		}
	}

	return n, nil
}

// stopService checks before each stop, so a retry after a half-done uninstall stops nothing twice.
func stopService(ctx context.Context, h Host, m Manifest) error {
	var units []string
	for _, f := range m.Files {
		if f.Kind != KindService {
			continue
		}
		_, err := os.Lstat(filepath.Join(h.Root, f.Path))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", f.Path, err)
		}
		units = append(units, f.Path)
	}
	if len(units) == 0 {
		return nil
	}

	if h.OS == "darwin" {
		out, err := h.Run(ctx, "launchctl", "print", launchdLabel)
		if err != nil && bytes.Contains(out, []byte("Could not find service")) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check the background service: %w%s", err, outputTail(out))
		}
		if _, err := privileged(ctx, h, "launchctl", "bootout", launchdLabel); err != nil {
			return fmt.Errorf("stop the background service: %w", err)
		}

		return nil
	}

	for _, u := range units {
		name := strings.TrimSuffix(filepath.Base(u), ".service")
		if _, err := privileged(ctx, h, "systemctl", "disable", "--now", name); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}

	return nil
}

// removeOwned removes shard's own files newest first, and the manifest last, so a failed run can run again.
func removeOwned(ctx context.Context, h Host, m Manifest) error {
	service := false
	for _, f := range slices.Backward(m.Files) {
		if f.Kind != KindBinary && f.Kind != KindService && f.Kind != KindConfig {
			continue
		}
		if _, err := privileged(ctx, h, "rm", "-f", filepath.Join(h.Root, f.Path)); err != nil {
			return fmt.Errorf("remove %s: %w", f.Path, err)
		}
		service = service || f.Kind == KindService
	}
	if service && h.OS == "linux" {
		if _, err := privileged(ctx, h, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd: %w", err)
		}
	}

	return removeManifest(ctx, h)
}

// The bridge and the nft tables every daemon on the host shares, as pkg/hostclean names them.
const (
	hostBridge = "shard0"
	hostTable  = "shard"
	ipForward  = "/proc/sys/net/ipv4/ip_forward"
)

var hostTableFamilies = []string{"inet", "bridge"}

// removeNetwork takes the shared bridge and tables only while no sandbox has a port on the bridge and no daemon serves the proxy (SHARD-272).
func removeNetwork(ctx context.Context, h Host) (held bool, err error) {
	ports, err := os.ReadDir(filepath.Join(h.Root, "/sys/class/net", hostBridge, "brif"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("list the ports of the bridge %s: %w", hostBridge, err)
	}
	if len(ports) > 0 {
		return true, nil
	}
	listeners, err := run(ctx, h, "ss", "-Hltn", fmt.Sprintf("( sport = :%d or sport = :%d )", proxy.PlainPort, proxy.TLSPort))
	if err != nil {
		return false, fmt.Errorf("list the listeners on the proxy ports: %w", err)
	}
	if strings.TrimSpace(string(listeners)) != "" {
		return true, nil
	}

	// A host without nft never had the tables.
	if _, ok := lookPath(h, "nft"); ok {
		listed, err := privileged(ctx, h, "nft", "list", "tables")
		if err != nil {
			return false, fmt.Errorf("list the nft tables: %w", err)
		}
		tables := strings.Split(string(listed), "\n")
		for _, family := range hostTableFamilies {
			if !slices.Contains(tables, "table "+family+" "+hostTable) {
				continue
			}
			if _, err := privileged(ctx, h, "nft", "delete", "table", family, hostTable); err != nil {
				return false, fmt.Errorf("delete the nft table %s %s: %w", family, hostTable, err)
			}
		}
	}

	_, err = os.Lstat(filepath.Join(h.Root, "/sys/class/net", hostBridge))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the bridge %s: %w", hostBridge, err)
	}
	if _, err := privileged(ctx, h, "ip", "link", "delete", hostBridge); err != nil {
		return false, fmt.Errorf("delete the bridge %s: %w", hostBridge, err)
	}

	return false, nil
}

// uninstalled names what stays and how to remove it, since uninstall removes no shared tool and no data.
func uninstalled(h Host, m Manifest, netHeld bool) ([]string, error) {
	lines := []string{"", "Uninstalled shard.", "", "Your saved data remains in " + DataDir + "."}
	if h.OS == "darwin" {
		lines = append(lines, "To delete the saved data, run: "+sudoFor(h)+"rm -r "+DataDir)
		logs, err := macLogsLeft(h)
		if err != nil {
			return nil, err
		}
		lines = append(lines, logs...)
	}
	if h.OS == "linux" {
		data, err := dataLeft(h)
		if err != nil {
			return nil, err
		}
		lines = append(lines, data...)
	}

	var tools []string
	for _, f := range m.Files {
		if f.Kind != KindTool {
			continue
		}
		if f.Package != "" {
			tools = append(tools, "  "+f.Package, "    Remove it with: "+sudoFor(h)+"apt-get remove "+f.Package)
			continue
		}
		tools = append(tools, "  "+f.Path, "    Remove it with: "+sudoFor(h)+"rm "+f.Path)
	}
	if len(tools) > 0 {
		lines = append(lines, "", "These shared tools remain:")
		lines = append(lines, tools...)
	}

	if h.OS == "linux" {
		network, err := networkLeft(h, netHeld)
		if err != nil {
			return nil, err
		}
		lines = append(lines, network...)
	}

	commands, err := leftCommands(h)
	if err != nil {
		return nil, err
	}
	switch len(commands) {
	case 0:
		return lines, nil
	case 1:
		return append(lines, "", "The shard command remains at "+commands[0]+".", "Remove it with: rm "+commands[0]), nil
	}
	lines = append(lines, "", "These shard commands remain:")
	for _, c := range commands {
		lines = append(lines, "  "+c, "    Remove it with: rm "+c)
	}

	return lines, nil
}

// networkLeft names the shared bridge and tables a daemon still uses, or else IP forwarding, which other software may need on.
func networkLeft(h Host, held bool) ([]string, error) {
	if held {
		return []string{"", "A shard daemon still uses the network bridge " + hostBridge + " and its firewall tables, so they remain."}, nil
	}
	forward, err := os.ReadFile(filepath.Join(h.Root, ipForward))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ipForward, err)
	}
	if strings.TrimSpace(string(forward)) != "1" {
		return nil, nil
	}

	return []string{"", "IP forwarding remains on (net.ipv4.ip_forward = 1). Other software may need it.", "Turn it off with: " + sudoFor(h) + "sysctl -w net.ipv4.ip_forward=0"}, nil
}

const fstabPath = "/etc/fstab"

// dataImageFacts describes the disk image the daemon made for a data dir that cannot clone, with the fstab line that mounts it and the ordered commands that free it. (SHARD-742)
func dataImageFacts(h Host) (where, free []string, err error) {
	image := datadir.ImagePath(DataDir)
	info, err := os.Lstat(rooted(h, image))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("check %s: %w", image, err)
	}
	fstab, err := os.ReadFile(rooted(h, fstabPath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("read %s: %w", fstabPath, err)
	}
	var mounts []string
	for line := range strings.SplitSeq(string(fstab), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == image {
			mounts = append(mounts, "  "+strings.TrimSpace(line))
		}
	}

	sudo := sudoFor(h)
	// The lock a Firecracker start takes outlives the image. (SHARD-734)
	remove := sudo + "rm -r " + DataDir
	lock := image + ".lock"
	_, err = os.Lstat(rooted(h, lock))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("check %s: %w", lock, err)
	}
	if err == nil {
		remove += " " + lock
	}

	free = []string{sudo + "umount " + DataDir}
	if len(mounts) > 0 {
		// systemd keeps the mount unit it made from the fstab line until a reload. (SHARD-730)
		free = append(free, sudo+"sed -i '\\|^"+regexp.QuoteMeta(image)+"[[:space:]]|d' "+fstabPath, sudo+"systemctl daemon-reload")
	}
	free = append(free, sudo+"rm "+image, remove)

	size := fmt.Sprintf("It lives in the %.1f GiB disk image %s", float64(info.Size())/(1<<30), image)
	if len(mounts) == 0 {
		return []string{size + "."}, free, nil
	}
	return slices.Concat([]string{size + ", which this line in " + fstabPath + " mounts at boot:"}, mounts), free, nil
}

// dataLeft names the commands that delete the data uninstall keeps on Linux, with the disk image and fstab line of a data dir that cannot clone.
func dataLeft(h Host) ([]string, error) {
	where, free, err := dataImageFacts(h)
	if err != nil {
		return nil, err
	}
	if where != nil {
		lines := append(where, "To free the disk and delete the saved data, run:")
		for _, c := range free {
			lines = append(lines, "  "+c)
		}
		return lines, nil
	}

	// No image, but the data dir may still carry a stale fstab line, a mount, or a Firecracker lock. (SHARD-730, SHARD-734)
	sudo := sudoFor(h)
	image := datadir.ImagePath(DataDir)
	remove := sudo + "rm -r " + DataDir
	lock := image + ".lock"
	if _, lerr := os.Lstat(rooted(h, lock)); lerr == nil {
		remove += " " + lock
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return nil, fmt.Errorf("check %s: %w", lock, lerr)
	}
	fstab, err := os.ReadFile(rooted(h, fstabPath))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", fstabPath, err)
	}
	var mounts []string
	for line := range strings.SplitSeq(string(fstab), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == image {
			mounts = append(mounts, "  "+strings.TrimSpace(line))
		}
	}
	_, mounted, err := mountinfo.Under(h.Root, DataDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("check the mount at %s: %w", DataDir, err)
	}
	// A mount outlives its deleted image, and rm -r would empty it, then fail on the busy mount point.
	var unmount []string
	if mounted {
		unmount = []string{"  " + sudo + "umount " + DataDir}
	}
	if len(mounts) == 0 && !mounted {
		return []string{"To delete the saved data, run: " + remove}, nil
	}
	if len(mounts) == 0 {
		return slices.Concat([]string{"To delete the saved data, run:"}, unmount, []string{"  " + remove}), nil
	}
	dropLine := []string{"  " + sudo + "sed -i '\\|^" + regexp.QuoteMeta(image) + "[[:space:]]|d' " + fstabPath, "  " + sudo + "systemctl daemon-reload"}
	// A line left for a deleted image fails its mount at every boot.
	return slices.Concat(
		[]string{"The disk image " + image + " is gone, but this line in " + fstabPath + " still mounts it at boot:"}, mounts,
		[]string{"To remove the line and delete the saved data, run:"}, unmount, dropLine, []string{"  " + remove},
	), nil
}

// macLogsLeft names the log directory the LaunchDaemon wrote, which uninstall keeps as it keeps the data.
func macLogsLeft(h Host) ([]string, error) {
	_, err := os.Lstat(rooted(h, macLogDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", macLogDir, err)
	}

	return []string{"The daemon's logs remain in " + macLogDir + ".", "Remove them with: " + sudoFor(h) + "rm -r " + macLogDir}, nil
}

// leftCommands are the shard commands that still exist: the one that ran setup, and the copy the installer put in ~/.local/bin.
func leftCommands(h Host) ([]string, error) {
	paths := []string{h.Executable}
	if home := h.Env("HOME"); home != "" {
		paths = append(paths, filepath.Join(home, ".local", "bin", "shard"))
	}

	var left []string
	for _, p := range paths {
		if slices.Contains(left, p) {
			continue
		}
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check %s: %w", p, err)
		}
		left = append(left, p)
	}

	return left, nil
}

// manual reports an install setup did not make, changes nothing in it, and names the route that moves it to setup.
func (s *Setup) manual(ctx context.Context, inst Installation) error {
	h := s.Host
	versions, err := manualVersions(ctx, h, inst.Manual)
	if err != nil {
		return err
	}
	sudo := socketSudo(h)
	sandboxes, err := manualSandboxes(h, inst.Manual, sudo)
	if err != nil {
		return err
	}

	lines := slices.Concat([]string{"Manual installation detected.", ""}, versions, []string{"", "Found:"})
	for _, p := range inst.Manual {
		lines = append(lines, "  "+p)
	}
	lines = slices.Concat(lines,
		[]string{"", "Setup did not install these files, so it does not change or remove them.", "", "Inspect the installation:"},
		manualInspect(h, sudo),
		sandboxes,
		[]string{"", "To move it to setup, run:"},
		manualMove(h, inst.Manual),
		[]string{
			"", "To remove it instead, remove every sandbox first (" + sudo + "shard list --all), then run the lines above without the last.",
			"", "Your saved data in " + DataDir + " is not part of this.",
		},
	)

	return s.UI.Print(lines...)
}

// manualVersions names the version the found binary reports beside the one setup installs.
func manualVersions(ctx context.Context, h Host, found []string) ([]string, error) {
	installs := "Setup installs: shard " + h.Version
	if !slices.Contains(found, shardBinary) {
		return []string{installs}, nil
	}
	out, err := run(ctx, h, shardBinary, "--version")
	if err != nil {
		return nil, fmt.Errorf("read the installed version of %s: %w", shardBinary, err)
	}

	return []string{"Installed:      shard " + strings.TrimPrefix(strings.TrimSpace(string(out)), "client "), installs}, nil
}

// manualInspect lists the commands that show the manual install and its service.
func manualInspect(h Host, sudo string) []string {
	lines := []string{"  " + sudo + "shard version"}
	if h.OS == "darwin" {
		return append(lines, "  "+sudoFor(h)+"launchctl print "+launchdLabel)
	}
	if h.OS == "linux" {
		return append(lines, "  systemctl status shard")
	}

	return lines
}

// manualMove lists the commands that stop and remove the found files, then run setup.
func manualMove(h Host, found []string) []string {
	var bins, units, others []string
	for _, p := range found {
		switch {
		case strings.HasSuffix(p, ".service"):
			units = append(units, p)
		case strings.HasPrefix(p, "/usr/local/bin/"):
			bins = append(bins, p)
		default:
			others = append(others, p)
		}
	}

	sudo := sudoFor(h)
	var lines []string
	for _, u := range units {
		lines = append(lines, "  "+sudo+"systemctl disable --now "+strings.TrimSuffix(filepath.Base(u), ".service"))
	}
	if slices.Contains(others, launchdPlist) {
		lines = append(lines, "  "+sudo+"launchctl bootout "+launchdLabel)
	}
	if files := slices.Concat(units, others, bins); len(files) > 0 {
		lines = append(lines, "  "+sudo+"rm "+strings.Join(files, " "))
	}
	if len(units) > 0 {
		lines = append(lines, "  "+sudo+"systemctl daemon-reload")
	}
	// The rm above takes a setup run from the manual binary with it, so the installer fetches one again.
	next := h.Executable + " setup"
	if h.Executable == rooted(h, shardBinary) {
		next = "curl -fsSL https://useshards.com/install | sh"
	}

	return append(lines, "  "+next)
}

// manualSandboxes names the provider the recorded sandboxes use and whether they outlive the manual daemon, and says nothing when there are none.
func manualSandboxes(h Host, found []string, sudo string) ([]string, error) {
	recorded, err := sandboxstate.RecordedProvider(rooted(h, DataDir))
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("read the sandboxes in %s: %w", DataDir, err)
	}
	if err == nil && recorded == "" {
		return nil, nil
	}
	provider := "In setup, choose " + providerTitle(recorded) + ", the provider your sandboxes use."
	if err != nil {
		provider = "In setup, choose the provider your sandboxes use: " + sudo + "shard daemon status names it."
	}

	keeps, err := manualKeeps(h, found)
	if err != nil {
		return nil, err
	}

	return []string{"", provider, keeps}, nil
}

// manualKeeps reads the found daemon service for the setting that leaves the sandboxes running when it stops.
func manualKeeps(h Host, found []string) (string, error) {
	const keep = "Your sandboxes keep running: the service ends only the daemon, and the new daemon adopts them."
	if slices.Contains(found, systemdUnit) {
		body, err := os.ReadFile(rooted(h, systemdUnit))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", systemdUnit, err)
		}
		mode := ""
		for line := range strings.SplitSeq(string(body), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(key) == "KillMode" {
				mode = strings.TrimSpace(value)
			}
		}
		if mode == "process" {
			return keep, nil
		}

		return "Your sandboxes stop with the daemon, because the service does not keep them (no KillMode=process).", nil
	}
	if slices.Contains(found, launchdPlist) {
		body, err := os.ReadFile(rooted(h, launchdPlist))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", launchdPlist, err)
		}
		if strings.Contains(strings.Join(strings.Fields(string(body)), ""), "<key>AbandonProcessGroup</key><true/>") {
			return keep, nil
		}

		return "Your sandboxes stop with the daemon, because the service does not keep them (no AbandonProcessGroup).", nil
	}

	return "Stop the daemon you started by hand first. Setup cannot tell whether your sandboxes outlive it.", nil
}

func serviceState(ctx context.Context, h Host, m Manifest) (ServiceState, error) {
	if !m.StartAtBoot {
		return ServiceNone, nil
	}

	if h.OS == "darwin" {
		out, err := h.Run(ctx, "launchctl", "print", launchdLabel)
		if err != nil && bytes.Contains(out, []byte("Could not find service")) {
			return ServiceInactive, nil
		}
		if err != nil {
			return "", fmt.Errorf("check the background service: %w%s", err, outputTail(out))
		}
		if bytes.Contains(out, []byte("state = running")) {
			return ServiceActive, nil
		}

		return ServiceInactive, nil
	}

	// is-active exits non-zero for every state but active, and still prints the state.
	out, err := h.Run(ctx, "systemctl", "is-active", serviceName)
	state := strings.TrimSpace(string(out))
	if state == "active" {
		return ServiceActive, nil
	}
	if err != nil && state == "" {
		return "", fmt.Errorf("check the background service: %w", err)
	}

	return ServiceInactive, nil
}

// confirm asks before a change; a no is ErrDeclined, so the run ends having changed nothing.
func (s *Setup) confirm(ctx context.Context, yes bool) error {
	ok, err := s.UI.Confirm(ctx, AskConfirm, "Continue?", yes)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeclined
	}

	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}
