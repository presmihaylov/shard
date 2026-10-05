package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
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

// existing is the §11 menu over an installation Detect found.
func (s *Setup) existing(ctx context.Context, inst Installation) error {
	if inst.Manifest == nil {
		return s.manual(inst)
	}

	m := *inst.Manifest
	if err := s.UI.Print("Shard is already installed", "", "Version:  "+m.Version, "Provider: "+providerTitle(m.Provider), "Service:  "+string(inst.Service), ""); err != nil {
		return err
	}
	choice, err := s.UI.Select(ctx, AskExisting, "What would you like to do?", []term.Option{
		{Name: "repair", Label: "Check or repair the installation", Default: true},
		{Name: "upgrade", Label: "Upgrade Shard"},
		{Name: "uninstall", Label: "Uninstall Shard"},
		{Name: "exit", Label: "Exit"},
	})
	if err != nil {
		return err
	}

	switch choice {
	case 0:
		return s.switched(ctx, func(ctx context.Context) error { return s.repair(ctx, m, inst.Service) })
	case 1:
		return s.switched(ctx, func(ctx context.Context) error { return s.upgrade(ctx, m, inst.Service) })
	case 2:
		return s.uninstall(ctx, m)
	}

	return nil
}

// repair re-runs the steps of the recorded choices, so the provider and the startup setting stay what they were.
func (s *Setup) repair(ctx context.Context, m Manifest, service ServiceState) error {
	var problems []string
	for _, f := range m.Files {
		_, err := os.Lstat(filepath.Join(s.Host.Root, f.Path))
		if errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, f.Path+" is missing.")
			continue
		}
		if err != nil {
			return fmt.Errorf("check %s: %w", f.Path, err)
		}
	}
	if m.StartAtBoot && service != ServiceActive {
		problems = append(problems, "The background service is not running.")
	}

	if len(problems) == 0 {
		return s.UI.Print("✓ No problems found", "", "Shard "+m.Version+" with "+providerTitle(m.Provider)+" is installed correctly.")
	}

	steps, err := s.localSteps(ctx, Local{Provider: m.Provider, StartAtBoot: m.StartAtBoot})
	if err != nil {
		return err
	}
	lines := []string{"Setup found these problems:", ""}
	for _, p := range problems {
		lines = append(lines, "  "+p)
	}
	lines = append(lines, "", "Setup will run these steps again:")
	for _, st := range steps {
		lines = append(lines, "  "+st.Title)
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

	return s.apply(ctx, "Repairing Shard", steps)
}

// replacement is one installed binary and the release file that replaces it.
type replacement struct {
	path, asset string
	// user marks the CLI the user runs, which the user owns and replaces without administrator access.
	user bool
	tmp  string
}

// upgrade fetches and verifies every binary before it asks, and replaces none until all of them passed.
func (s *Setup) upgrade(ctx context.Context, m Manifest, service ServiceState) (err error) {
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
		return s.UI.Print("", "Shard "+m.Version+" is up to date.")
	}

	targets, err := upgradeTargets(h, m)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "shard-upgrade-*")
	if err != nil {
		return fmt.Errorf("upgrade Shard: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()

	fetch := Step{Title: "Download and verify Shard " + rel.Tag, Do: func(ctx context.Context) error {
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
	lines = append(lines, "", "The provider stays "+providerTitle(m.Provider)+".")
	switch service {
	case ServiceActive:
		lines = append(lines, "The background service restarts to run "+rel.Tag+".",
			"Your sandboxes keep running while the daemon restarts.",
			"Open `shard exec` sessions disconnect. Their commands keep running.")
	case ServiceInactive:
		lines = append(lines, "The background service is not running. Setup does not start it.")
	case ServiceNone:
		lines = append(lines, "You start the daemon yourself. Restart `shard daemon` to run "+rel.Tag+".")
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
		{Title: "Replace the Shard binaries", Do: func(ctx context.Context) error { return replaceAll(ctx, h, targets) }},
		{Title: "Record Shard " + rel.Tag, Do: func(ctx context.Context) error {
			m.Version = rel.Tag
			return saveManifest(ctx, h, m)
		}},
	}
	if service == ServiceActive {
		steps = append(steps, Step{Title: "Restart the daemon", Do: func(ctx context.Context) error { return restartService(ctx, h, m) }})
	}

	return s.apply(ctx, "Upgrading Shard", steps)
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

	return "", fmt.Errorf("upgrade Shard: no release file replaces %s", name)
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

func restartService(ctx context.Context, h Host, m Manifest) error {
	cmd := []string{"systemctl", "restart", serviceName}
	if h.OS == "darwin" {
		cmd = []string{"launchctl", "kickstart", "-k", launchdLabel}
	}
	if _, err := privileged(ctx, h, cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("restart the daemon: %w", err)
	}

	state, err := serviceState(ctx, h, m)
	if err != nil {
		return err
	}
	if state != ServiceActive {
		return fmt.Errorf("the daemon is %s after the restart", strings.ToLower(string(state)))
	}

	return nil
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
		return errors.Join(s.UI.Print("Shard has "+left+" on this machine.",
			"Remove them before you uninstall Shard:", "",
			"  List sandboxes:", "    shard list --all", "",
			"  Remove a sandbox:", "    shard remove --force <name>"),
			fmt.Errorf("uninstall stopped: %s left", left))
	}

	if err := s.UI.Print("Uninstall Shard?", "",
		"This will stop and remove the background service",
		"and remove files installed by Shard setup.", "",
		"Your saved data will remain.",
		"Shared tools will remain.", ""); err != nil {
		return err
	}
	if err := s.confirm(ctx, false); err != nil {
		return err
	}

	steps := []Step{
		{Title: "Stop and remove the background service", Do: func(ctx context.Context) error { return stopService(ctx, h, m) }},
		{Title: "Remove files installed by Shard setup", Do: func(ctx context.Context) error { return removeOwned(ctx, h, m) }},
	}
	if err := s.apply(ctx, "Uninstalling Shard", steps); err != nil {
		return err
	}

	return s.UI.Print(uninstalled(h, m)...)
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

// uninstalled names what stays and how to remove it, since uninstall removes no shared tool and no data.
func uninstalled(h Host, m Manifest) []string {
	lines := []string{"", "Shard was uninstalled.", "", "Your saved data remains in " + DataDir + "."}

	var tools []string
	for _, f := range m.Files {
		if f.Kind != KindTool {
			continue
		}
		if f.Package != "" {
			tools = append(tools, "  "+f.Package, "    Remove it with: sudo apt-get remove "+f.Package)
			continue
		}
		tools = append(tools, "  "+f.Path, "    Remove it with: sudo rm "+f.Path)
	}
	if len(tools) > 0 {
		lines = append(lines, "", "These shared tools remain:")
		lines = append(lines, tools...)
	}

	return append(lines, "", "The shard command remains at "+h.Executable+".", "Remove it with: rm "+h.Executable)
}

// manual reports an install setup did not make, and changes nothing in it.
func (s *Setup) manual(inst Installation) error {
	lines := []string{"Manual installation detected.", "", "Found:"}
	var bins, units, others []string
	for _, p := range inst.Manual {
		lines = append(lines, "  "+p)
		switch {
		case strings.HasSuffix(p, ".service"):
			units = append(units, p)
		case strings.HasPrefix(p, "/usr/local/bin/"):
			bins = append(bins, p)
		default:
			others = append(others, p)
		}
	}
	lines = append(lines, "", "Setup did not install these files, so it does not change or remove them.", "", "Inspect the installation:", "  shard version")
	if s.Host.OS == "darwin" {
		lines = append(lines, "  sudo launchctl print "+launchdLabel)
	}
	if s.Host.OS == "linux" {
		lines = append(lines, "  systemctl status shard")
	}

	lines = append(lines, "", "To remove it, remove every sandbox first (shard list --all), then run:")
	for _, u := range units {
		lines = append(lines, "  sudo systemctl disable --now "+strings.TrimSuffix(filepath.Base(u), ".service"))
	}
	if slices.Contains(others, "/Library/LaunchDaemons/shard.daemon.plist") {
		lines = append(lines, "  sudo launchctl bootout "+launchdLabel)
	}
	if files := slices.Concat(units, others, bins); len(files) > 0 {
		lines = append(lines, "  sudo rm "+strings.Join(files, " "))
	}
	if len(units) > 0 {
		lines = append(lines, "  sudo systemctl daemon-reload")
	}

	return s.UI.Print(append(lines, "", "Your saved data in "+DataDir+" is not part of this.")...)
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
