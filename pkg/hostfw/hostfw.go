// Package hostfw lets one interface through the host's own firewall as Docker does, through iptables and firewall-cmd only.
package hostfw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Run runs one command and returns its stdout. A failure's error carries the exit code, as *exec.ExitError does.
type Run func(ctx context.Context, name string, args ...string) ([]byte, error)

// Rule is one accept at the top of a filter chain, with Match in the order iptables -S prints it so a quiet pass finds it.
type Rule struct {
	Chain string
	Match []string
}

// Hole is what one interface needs through the host firewall, and the name every part of it carries.
type Hole struct {
	// Name is the comment on every rule and the firewalld zone, which is how Close finds them again.
	Name string
	// Interface is what the zone holds.
	Interface string
	// Policy lets the zone forward out on firewalld 1.x, where a zone's target no longer covers it.
	Policy string
	Rules  []Rule
}

// Manager drives one host's iptables and firewall-cmd. An empty path is a host without that tool, where every call about it does nothing.
type Manager struct {
	run         Run
	iptables    string
	firewallCmd string
}

// New takes the runner and the two binaries as the caller found them.
func New(run Run, iptables, firewallCmd string) *Manager {
	return &Manager{run: run, iptables: iptables, firewallCmd: firewallCmd}
}

// Local is this host's Manager, which runs each tool as the caller and finds it on PATH.
func Local() *Manager {
	return New(local, found("iptables"), found("firewall-cmd"))
}

func found(name string) string {
	if _, err := exec.LookPath(name); err != nil {
		return ""
	}

	return name
}

// waitDelay bounds how long a cancelled call waits for the output pipes after the kill signal.
const waitDelay = 2 * time.Second

func local(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

const (
	// exitNoRule is iptables -C on a rule it did not find, and exitNotRunning is firewall-cmd --state with firewalld off.
	exitNoRule     = 1
	exitNotRunning = 252
	accept         = "ACCEPT"
)

func exitCode(err error) (int, bool) {
	var coded interface{ ExitCode() int }
	if !errors.As(err, &coded) {
		return 0, false
	}

	return coded.ExitCode(), true
}

// EnsureRules puts back whichever of the hole's rules a chain lacks, and only on a host whose chains filter.
func (m *Manager) EnsureRules(ctx context.Context, hole Hole) error {
	if m.iptables == "" {
		return nil
	}

	lines, err := m.listRules(ctx)
	if err != nil {
		return err
	}
	if !filters(lines, hole) {
		return nil
	}

	// Each goes in at the top, so the last one first keeps the hole's order.
	for _, rule := range slices.Backward(hole.Rules) {
		if slices.Contains(lines, "-A "+rule.Chain+" "+strings.Join(rule.args(hole.Name), " ")) {
			continue
		}
		present, err := m.holds(ctx, rule, hole.Name)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if _, err := m.run(ctx, m.iptables, slices.Concat([]string{"-w", "-I", rule.Chain, "1"}, rule.args(hole.Name))...); err != nil {
			return fmt.Errorf("let %s through the %s chain: %w", hole.Interface, rule.Chain, err)
		}
	}

	return nil
}

func (r Rule) args(name string) []string {
	return slices.Concat(r.Match, []string{"-m", "comment", "--comment", name, "-j", accept})
}

func (m *Manager) listRules(ctx context.Context) ([]string, error) {
	out, err := m.run(ctx, m.iptables, "-w", "-S")
	if err != nil {
		return nil, fmt.Errorf("list the iptables rules: %w", err)
	}

	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// filters is whether a chain the hole uses could drop: a policy other than ACCEPT, or a rule that is not the hole's.
func filters(lines []string, hole Hole) bool {
	for _, rule := range hole.Rules {
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[1] != rule.Chain {
				continue
			}
			if fields[0] == "-P" && fields[2] != accept {
				return true
			}
			if fields[0] == "-A" && !tagged(fields, hole.Name) {
				return true
			}
		}
	}

	return false
}

func tagged(fields []string, name string) bool {
	at := slices.Index(fields, "--comment")

	return at >= 0 && at+1 < len(fields) && fields[at+1] == name
}

// holds asks iptables itself, for a version that prints a rule in another order than Match.
func (m *Manager) holds(ctx context.Context, rule Rule, name string) (bool, error) {
	_, err := m.run(ctx, m.iptables, slices.Concat([]string{"-w", "-C", rule.Chain}, rule.args(name))...)
	if err == nil {
		return true, nil
	}
	if code, ok := exitCode(err); ok && code == exitNoRule {
		return false, nil
	}

	return false, fmt.Errorf("check the %s chain for %s: %w", rule.Chain, name, err)
}

// EnsureZone puts the interface in a permanent zone that accepts it, so a firewalld reload brings the zone back.
func (m *Manager) EnsureZone(ctx context.Context, hole Hole) error {
	running, err := m.firewalldRunning(ctx)
	if err != nil || !running {
		return err
	}

	changed, err := m.ensureZone(ctx, hole)
	if err != nil {
		return err
	}
	policy, err := m.ensurePolicy(ctx, hole)
	if err != nil {
		return err
	}
	live, err := m.firewall(ctx, "--get-zones")
	if err != nil {
		return err
	}
	if !changed && !policy && slices.Contains(strings.Fields(live), hole.Name) {
		return nil
	}

	return m.reload(ctx)
}

func (m *Manager) ensureZone(ctx context.Context, hole Hole) (bool, error) {
	zones, err := m.firewall(ctx, "--permanent", "--get-zones")
	if err != nil {
		return false, err
	}
	created := !slices.Contains(strings.Fields(zones), hole.Name)
	if created {
		if _, err := m.firewall(ctx, "--permanent", "--new-zone="+hole.Name); err != nil {
			return false, err
		}
	}

	info, err := m.firewall(ctx, "--permanent", "--info-zone="+hole.Name)
	if err != nil {
		return false, err
	}
	var steps [][]string
	if field(info, "target") != accept {
		steps = append(steps, []string{"--permanent", "--zone=" + hole.Name, "--set-target=" + accept})
	}
	if !slices.Contains(strings.Fields(field(info, "interfaces")), hole.Interface) {
		steps = append(steps, []string{"--permanent", "--zone=" + hole.Name, "--add-interface=" + hole.Interface})
	}

	return created || len(steps) > 0, m.firewallEach(ctx, steps)
}

// ensurePolicy is a no-op before firewalld 1.0, whose zone target still covers what the zone forwards.
func (m *Manager) ensurePolicy(ctx context.Context, hole Hole) (bool, error) {
	policies, supported, err := m.policies(ctx)
	if err != nil || !supported {
		return false, err
	}
	created := !slices.Contains(policies, hole.Policy)
	if created {
		if _, err := m.firewall(ctx, "--permanent", "--new-policy="+hole.Policy); err != nil {
			return false, err
		}
	}

	info, err := m.firewall(ctx, "--permanent", "--info-policy="+hole.Policy)
	if err != nil {
		return false, err
	}
	var steps [][]string
	if field(info, "target") != accept {
		steps = append(steps, []string{"--permanent", "--policy=" + hole.Policy, "--set-target=" + accept})
	}
	if !slices.Contains(strings.Fields(field(info, "ingress-zones")), hole.Name) {
		steps = append(steps, []string{"--permanent", "--policy=" + hole.Policy, "--add-ingress-zone=" + hole.Name})
	}
	if !slices.Contains(strings.Fields(field(info, "egress-zones")), "ANY") {
		steps = append(steps, []string{"--permanent", "--policy=" + hole.Policy, "--add-egress-zone=ANY"})
	}

	return created || len(steps) > 0, m.firewallEach(ctx, steps)
}

// policies lists the permanent policies, and reports none supported before firewalld 1.0.
func (m *Manager) policies(ctx context.Context) ([]string, bool, error) {
	version, err := m.firewall(ctx, "--version")
	if err != nil {
		return nil, false, err
	}
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return nil, false, fmt.Errorf("read the firewalld version %q: %w", strings.TrimSpace(version), err)
	}
	if n < 1 {
		return nil, false, nil
	}

	listed, err := m.firewall(ctx, "--permanent", "--get-policies")
	if err != nil {
		return nil, false, err
	}

	return strings.Fields(listed), true, nil
}

// field reads one "  key: value" line of what --info-zone and --info-policy print.
func field(info, key string) string {
	for line := range strings.SplitSeq(info, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+":"); ok {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

// Close drops every rule the hole's name tags, its policy and its zone. It is idempotent.
func (m *Manager) Close(ctx context.Context, hole Hole) error {
	if err := m.closeRules(ctx, hole); err != nil {
		return err
	}

	running, err := m.firewalldRunning(ctx)
	if err != nil || !running {
		return err
	}

	policies, _, err := m.policies(ctx)
	if err != nil {
		return err
	}
	var steps [][]string
	if slices.Contains(policies, hole.Policy) {
		steps = append(steps, []string{"--permanent", "--delete-policy=" + hole.Policy})
	}
	zones, err := m.firewall(ctx, "--permanent", "--get-zones")
	if err != nil {
		return err
	}
	if slices.Contains(strings.Fields(zones), hole.Name) {
		steps = append(steps, []string{"--permanent", "--delete-zone=" + hole.Name})
	}
	if len(steps) == 0 {
		return nil
	}
	if err := m.firewallEach(ctx, steps); err != nil {
		return err
	}

	return m.reload(ctx)
}

func (m *Manager) closeRules(ctx context.Context, hole Hole) error {
	if m.iptables == "" {
		return nil
	}

	lines, err := m.listRules(ctx)
	if err != nil {
		return err
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" || !tagged(fields, hole.Name) {
			continue
		}
		if _, err := m.run(ctx, m.iptables, slices.Concat([]string{"-w", "-D"}, fields[1:])...); err != nil {
			return fmt.Errorf("drop the %s rule %q: %w", hole.Name, line, err)
		}
	}

	return nil
}

func (m *Manager) firewalldRunning(ctx context.Context) (bool, error) {
	if m.firewallCmd == "" {
		return false, nil
	}

	_, err := m.run(ctx, m.firewallCmd, "--state")
	if err == nil {
		return true, nil
	}
	if code, ok := exitCode(err); ok && code == exitNotRunning {
		return false, nil
	}

	return false, fmt.Errorf("ask firewalld whether it runs: %w", err)
}

func (m *Manager) reload(ctx context.Context) error {
	_, err := m.firewall(ctx, "--reload")

	return err
}

func (m *Manager) firewallEach(ctx context.Context, steps [][]string) error {
	for _, args := range steps {
		if _, err := m.firewall(ctx, args...); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) firewall(ctx context.Context, args ...string) (string, error) {
	out, err := m.run(ctx, m.firewallCmd, args...)
	if err != nil {
		return "", fmt.Errorf("firewalld: %w", err)
	}

	return string(out), nil
}
