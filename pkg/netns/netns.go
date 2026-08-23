// Package netns drives the host network stack: namespaces, veth pairs, a bridge and nftables rules.
// It runs iproute2 and nft, and it knows nothing about sandboxes. Which address a sandbox gets, and
// which packets it may send, is the caller's policy and never this driver's.
package netns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// RunDir is where iproute2 binds a namespace so it outlives the process that created it.
const RunDir = "/var/run/netns"

// ErrExists is what a create of something already there returns, which is how EnsureBridge is idempotent.
var ErrExists = errors.New("the object already exists")

// ErrNotFound is what a delete of something already gone returns. Match both with errors.Is.
var ErrNotFound = errors.New("no such object")

// ErrUnsupported keeps a developer Mac honest. pkg/ may not import models, so this is its own sentinel.
var ErrUnsupported = errors.New("the host network needs Linux")

// forwardingPath is the switch that lets the host route a sandbox's packets out of the box.
const forwardingPath = "/proc/sys/net/ipv4/ip_forward"

// waitDelay bounds how long a cancelled call waits for the output pipes after the kill signal.
const waitDelay = 2 * time.Second

// iproute2 and nft are not translated, so matching their text is stable. Each verb has its own
// phrasing for the same condition: ip addr says a duplicate is assigned where ip link says it exists.
var (
	existsMessages   = []string{"File exists", "Address already assigned"}
	notFoundMessages = []string{
		"Cannot find device",
		"does not exist",
		"No such file or directory",
		"Cannot remove namespace file",
		"No such process",
	}
)

// Manager runs one host's network commands. It holds no state between calls, so any shard process
// can tear down what another one built.
type Manager struct {
	ipPath  string
	nftPath string
}

// Option configures a Manager.
type Option func(*Manager)

// WithIP points at an iproute2 other than the one on PATH.
func WithIP(path string) Option {
	return func(m *Manager) { m.ipPath = path }
}

// WithNFT points at an nft other than the one on PATH.
func WithNFT(path string) Option {
	return func(m *Manager) { m.nftPath = path }
}

// New finds the binaries. It refuses off Linux rather than failing later with a missing executable.
func New(opts ...Option) (*Manager, error) {
	if !supported {
		return nil, ErrUnsupported
	}

	m := &Manager{ipPath: "ip", nftPath: "nft"}
	for _, opt := range opts {
		opt(m)
	}

	for _, binary := range []string{m.ipPath, m.nftPath} {
		if _, err := exec.LookPath(binary); err != nil {
			return nil, fmt.Errorf("find %s: %w", binary, err)
		}
	}

	return m, nil
}

// NamespacePath is what an OCI spec points at to join a namespace this manager made.
func NamespacePath(name string) string {
	return filepath.Join(RunDir, name)
}

// AddNamespace creates a named namespace, which survives until DeleteNamespace. A namespace the
// runtime makes for itself would die with the sandbox, and a sandbox outlives its entrypoint.
func (m *Manager) AddNamespace(ctx context.Context, name string) error {
	return m.run(ctx, "netns", "add", name)
}

// DeleteNamespace drops the namespace and every interface in it, which includes one end of each veth
// pair, so the host end goes with it.
func (m *Manager) DeleteNamespace(ctx context.Context, name string) error {
	err := m.run(ctx, "netns", "delete", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	return err
}

// NamespaceExists asks the filesystem, because the bind mount is the namespace's only name.
func NamespaceExists(name string) (bool, error) {
	_, err := os.Stat(NamespacePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", NamespacePath(name), err)
	}

	return true, nil
}

// AddVeth creates the pair and puts the peer end straight into the namespace, so the host never
// carries an interface named after the guest side.
func (m *Manager) AddVeth(ctx context.Context, host, peer, namespace string) error {
	return m.run(ctx, "link", "add", host, "type", "veth", "peer", "name", peer, "netns", namespace)
}

// DeleteLink removes an interface from the host namespace. Deleting one end of a veth pair takes both.
func (m *Manager) DeleteLink(ctx context.Context, name string) error {
	err := m.run(ctx, "link", "delete", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	return err
}

// LinkExists reports whether the host namespace holds the interface.
func (m *Manager) LinkExists(ctx context.Context, name string) (bool, error) {
	err := m.run(ctx, "link", "show", name)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// EnsureBridge creates the bridge, gives it the gateway address and brings it up. It is idempotent,
// so every sandbox create may call it.
func (m *Manager) EnsureBridge(ctx context.Context, name string, gateway netip.Prefix) error {
	if err := m.run(ctx, "link", "add", "name", name, "type", "bridge"); err != nil && !errors.Is(err, ErrExists) {
		return err
	}

	if err := m.run(ctx, "addr", "add", gateway.String(), "dev", name); err != nil && !errors.Is(err, ErrExists) {
		return err
	}

	return m.SetUp(ctx, name)
}

// AttachBridge makes the interface a port of the bridge.
func (m *Manager) AttachBridge(ctx context.Context, link, bridge string) error {
	return m.run(ctx, "link", "set", link, "master", bridge)
}

// IsolatePort stops this port reaching any other isolated port on the same bridge, while it still
// reaches the bridge itself. Two sandboxes share one layer 2 segment, and netfilter's forward hook
// never sees traffic the bridge switches, so this is what keeps them apart.
func (m *Manager) IsolatePort(ctx context.Context, link string) error {
	return m.run(ctx, "link", "set", "dev", link, "type", "bridge_slave", "isolated", "on")
}

// SetUp brings a host interface up.
func (m *Manager) SetUp(ctx context.Context, link string) error {
	return m.run(ctx, "link", "set", link, "up")
}

// SetUpIn brings an interface inside a namespace up.
func (m *Manager) SetUpIn(ctx context.Context, namespace, link string) error {
	return m.run(ctx, "-netns", namespace, "link", "set", link, "up")
}

// AddAddressIn gives an interface inside a namespace its address and the prefix it is reached on.
func (m *Manager) AddAddressIn(ctx context.Context, namespace, link string, address netip.Prefix) error {
	return m.run(ctx, "-netns", namespace, "addr", "add", address.String(), "dev", link)
}

// AddDefaultRouteIn points everything the namespace does not know at the gateway.
func (m *Manager) AddDefaultRouteIn(ctx context.Context, namespace, link string, gateway netip.Addr) error {
	return m.run(ctx, "-netns", namespace, "route", "add", "default", "via", gateway.String(), "dev", link)
}

// AddressesIn lists the addresses an interface inside a namespace carries.
func (m *Manager) AddressesIn(ctx context.Context, namespace, link string) ([]netip.Prefix, error) {
	var out bytes.Buffer
	if err := m.output(ctx, &out, "-netns", namespace, "-brief", "addr", "show", link); err != nil {
		return nil, err
	}

	// The brief form is one line of "name state addr/bits addr/bits ...", so every field with a / is one.
	var addresses []netip.Prefix

	for field := range strings.FieldsSeq(out.String()) {
		if !strings.Contains(field, "/") {
			continue
		}

		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, fmt.Errorf("read the address %q of %s in %s: %w", field, link, namespace, err)
		}

		addresses = append(addresses, prefix)
	}

	return addresses, nil
}

// Routes lists the destination of every route the host holds, so a caller can see whether a subnet
// it wants to claim is already reachable somewhere else.
func (m *Manager) Routes(ctx context.Context) ([]netip.Prefix, error) {
	var out bytes.Buffer
	if err := m.output(ctx, &out, "-json", "route", "show"); err != nil {
		return nil, err
	}

	return parseRoutes(out.Bytes())
}

// parseRoutes reads what ip -json route show prints. The default route is dropped: it matches every
// address, so it says nothing about which ranges the host already uses.
func parseRoutes(data []byte) ([]netip.Prefix, error) {
	var entries []struct {
		Dst string `json:"dst"`
	}

	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("read the host route table: %w", err)
	}

	routes := make([]netip.Prefix, 0, len(entries))

	for _, entry := range entries {
		if entry.Dst == "default" {
			continue
		}

		route, err := parseDestination(entry.Dst)
		if err != nil {
			return nil, err
		}

		routes = append(routes, route)
	}

	return routes, nil
}

// parseDestination reads one dst, which is a prefix or the bare address of a host route.
func parseDestination(dst string) (netip.Prefix, error) {
	if strings.Contains(dst, "/") {
		route, err := netip.ParsePrefix(dst)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("read the route destination %q: %w", dst, err)
		}

		return route, nil
	}

	address, err := netip.ParseAddr(dst)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("read the route destination %q: %w", dst, err)
	}

	return netip.PrefixFrom(address, address.BitLen()), nil
}

// EnableForwarding lets the host route between the bridge and the outside. It is a host-wide switch
// and shard turns it on for good, the way every container runtime does.
func (m *Manager) EnableForwarding() error {
	if err := os.WriteFile(forwardingPath, []byte("1\n"), 0o644); err != nil { // #nosec G306
		return fmt.Errorf("write %s: %w", forwardingPath, err)
	}

	return nil
}

// ApplyRuleset feeds nft one ruleset on stdin, which the kernel applies as a single transaction.
// The text is the caller's: this driver holds no policy of its own.
func (m *Manager) ApplyRuleset(ctx context.Context, ruleset string) error {
	return m.nftRun(ctx, strings.NewReader(ruleset), "-f", "-")
}

// TableExists reports whether nft holds the table, which is how a caller checks its own ruleset landed.
func (m *Manager) TableExists(ctx context.Context, family, table string) (bool, error) {
	err := m.nftRun(ctx, nil, "list", "table", family, table)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// DeleteTable drops a whole nft table, which takes every rule in it with it.
func (m *Manager) DeleteTable(ctx context.Context, family, table string) error {
	err := m.nftRun(ctx, nil, "delete", "table", family, table)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	return err
}

func (m *Manager) run(ctx context.Context, args ...string) error {
	return m.execute(ctx, m.ipPath, nil, nil, args...)
}

func (m *Manager) output(ctx context.Context, stdout *bytes.Buffer, args ...string) error {
	return m.execute(ctx, m.ipPath, nil, stdout, args...)
}

func (m *Manager) nftRun(ctx context.Context, stdin io.Reader, args ...string) error {
	return m.execute(ctx, m.nftPath, stdin, nil, args...)
}

// execute runs one host binary, collecting stderr so a failure can be classified. Every verb goes
// through it, so an nft failure is reported and matched the same way an ip one is.
func (m *Manager) execute(ctx context.Context, binary string, stdin io.Reader, stdout *bytes.Buffer, args ...string) error {
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin, cmd.Stderr = stdin, &stderr
	cmd.WaitDelay = waitDelay

	if stdout != nil {
		cmd.Stdout = stdout
	}

	if err := cmd.Run(); err != nil {
		called := filepath.Base(binary) + " " + strings.Join(args, " ")
		// A cancelled call says nothing about the host, so report the context and not what the kill looked like.
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", called, ctx.Err())
		}

		message := strings.TrimSpace(stderr.String())

		return fmt.Errorf("%s: %w: %s", called, sentinel(message, err), message)
	}

	return nil
}

// sentinel turns the two failures a caller must act on into errors it can match.
func sentinel(message string, err error) error {
	reported := func(phrase string) bool { return strings.Contains(message, phrase) }

	if slices.ContainsFunc(existsMessages, reported) {
		return ErrExists
	}

	if slices.ContainsFunc(notFoundMessages, reported) {
		return ErrNotFound
	}

	return err
}
