package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/pkg/registry"
	runccli "github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/kernel"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/runc"
	"github.com/presmihaylov/shard/services/provider/sysbox"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// deps is every layer the daemon drives. Each one is built on the first ask and kept, so a daemon on
// a host without runsc, netns or root still answers the reads and the store verbs.
type deps struct {
	cfg Config

	// The tasks run at once and share one deps, so every getter builds under this lock. A getter that
	// needs another one calls its locked form, because a Mutex taken twice by one goroutine deadlocks.
	mu sync.Mutex

	imageSvc *image.Service
	repoSvc  *sandboxstate.Repository
	netSvc   hostNetwork
	// addressesSvc is netSvc on a VM host, kept in its own type because the stack asks it to judge.
	addressesSvc *network.Addresses
	stackSvc     *netstack.Stack
	providerSvc  models.Provider
	secretSvc    *secret.Store
	policySvc    *egress.Store
	runnerSvc    *runsc.Runner
}

// hostNetwork leases every sandbox its address: the bridge on Linux, a pool alone on a VM host, and the proxy listens on its gateway.
type hostNetwork interface {
	sandbox.Network
	Gateway() netip.Addr
}

// front is where the proxy and the resolver listen: the bridge gateway on Linux, the userspace stack on a VM host.
type front interface {
	ListenTCP(port uint16) (net.Listener, error)
	ListenPacket(port uint16) (net.PacketConn, error)
}

// providerName is the substrate the daemon runs: --provider, or the platform's default when it is empty.
func (d *deps) providerName() string {
	if d.cfg.Provider != "" {
		return d.cfg.Provider
	}
	if runtime.GOOS == "darwin" {
		return vzvm.Name
	}

	return gvisor.Name
}

func (d *deps) imagesLocked() (*image.Service, error) {
	if d.imageSvc != nil {
		return d.imageSvc, nil
	}

	opts := []image.Option{image.WithRegistry(registry.WithInsecureRegistries(d.cfg.Insecure...))}
	// A VM boots from a disk, so the vz daemon builds one per image at the pull.
	if d.providerName() == vzvm.Name {
		opts = append(opts, image.WithDisks())
	}
	svc, err := image.New(filepath.Join(d.cfg.Root, "images"), opts...)
	if err != nil {
		return nil, err
	}
	d.imageSvc = svc

	return d.imageSvc, nil
}

func (d *deps) repoLocked() (*sandboxstate.Repository, error) {
	if d.repoSvc != nil {
		return d.repoSvc, nil
	}

	repo, err := sandboxstate.New(d.cfg.Root)
	if err != nil {
		return nil, err
	}
	d.repoSvc = repo

	return d.repoSvc, nil
}

func (d *deps) netLocked() (hostNetwork, error) {
	if d.netSvc != nil {
		return d.netSvc, nil
	}

	// A VM host has no bridge: the addresses are leased and the stack answers for the gateway.
	if d.providerName() == vzvm.Name {
		svc, err := d.addressesLocked()
		if err != nil {
			return nil, err
		}
		d.netSvc = svc

		return d.netSvc, nil
	}

	manager, err := netns.New()
	if err != nil {
		return nil, err
	}

	source, err := d.egressLocked()
	if err != nil {
		return nil, err
	}

	// The provider says whether its sandboxes own their namespaces, and it is asked at the first
	// Allocate, not here: the proxy builds the network at boot on a host that may have no substrate.
	svc, err := network.New(network.Config{Root: d.cfg.Root, Egress: source, Userns: d.userns}, manager)
	if err != nil {
		return nil, err
	}
	d.netSvc = svc

	return d.netSvc, nil
}

func (d *deps) providerLocked() (models.Provider, error) {
	if d.providerSvc != nil {
		return d.providerSvc, nil
	}

	repo, err := d.repoLocked()
	if err != nil {
		return nil, err
	}

	provider, err := d.newProvider(repo.Dir)
	if err != nil {
		return nil, err
	}
	d.providerSvc = provider

	return d.providerSvc, nil
}

// addressesLocked is the VM host's network: leases, and the judge the stack asks with the same chains the host rules compile from.
func (d *deps) addressesLocked() (*network.Addresses, error) {
	if d.addressesSvc != nil {
		return d.addressesSvc, nil
	}

	source, err := d.egressLocked()
	if err != nil {
		return nil, err
	}
	svc, err := network.NewAddresses(network.Config{Root: d.cfg.Root, Egress: source})
	if err != nil {
		return nil, err
	}
	d.addressesSvc = svc

	return d.addressesSvc, nil
}

// stackLocked is the one userspace stack every VM's frames end in, and the front the proxy listens on.
func (d *deps) stackLocked() (*netstack.Stack, error) {
	if d.stackSvc != nil {
		return d.stackSvc, nil
	}

	addresses, err := d.addressesLocked()
	if err != nil {
		return nil, err
	}

	gateway, err := network.Gateway(network.Config{Root: d.cfg.Root})
	if err != nil {
		return nil, err
	}
	repo, err := d.repoLocked()
	if err != nil {
		return nil, err
	}
	logger := log.New(d.cfg.Out, "", log.LstdFlags)
	drops := &stackDrops{tailer: egress.NewTailer(d.cfg.Root, egress.NewLog(repo), repo, logger), gateway: gateway, out: logger}
	// The host chains dnat a guest's 80 and 443 onto the proxy, and the stack does the same with its own table; every other flow is judged by the same chains.
	stack, err := netstack.New(netstack.Config{
		Address:   gateway,
		Redirects: map[uint16]uint16{80: proxy.PlainPort, 443: proxy.TLSPort},
		Drops:     drops.report,
		Judge:     addresses.Judge,
	})
	if err != nil {
		return nil, err
	}
	d.stackSvc = stack

	return d.stackSvc, nil
}

// stackDrops lands every frame the stack refused in the sandbox's decision log, which is what the tailer does with the kernel ring on Linux.
type stackDrops struct {
	mu      sync.Mutex
	tailer  *egress.Tailer
	gateway netip.Addr
	out     *log.Logger
}

func (s *stackDrops) report(d netstack.Drop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.tailer.Drop(d.Guest, egress.StackDrop(s.gateway, d)); err != nil {
		// The frame is refused already, so a log that cannot be written closes no door; the daemon log carries it.
		s.out.Printf("egress log: sandbox at %s: %v", d.Guest, err)
	}
}

// frontLocked is what the proxy and the resolver listen through, so one task serves either host the same way.
func (d *deps) frontLocked() (front, error) {
	if d.providerName() == vzvm.Name {
		return d.stackLocked()
	}

	hostNet, err := d.netLocked()
	if err != nil {
		return nil, err
	}

	return gatewayFront{address: hostNet.Gateway()}, nil
}

// gatewayFront listens on the bridge gateway, which the bridge must carry first.
type gatewayFront struct {
	address netip.Addr
}

func (f gatewayFront) ListenTCP(port uint16) (net.Listener, error) {
	return net.Listen("tcp", netip.AddrPortFrom(f.address, port).String())
}

func (f gatewayFront) ListenPacket(port uint16) (net.PacketConn, error) {
	return net.ListenPacket("udp", netip.AddrPortFrom(f.address, port).String())
}

// newProvider picks the substrate --provider named. The daemon runs one; gVisor is the default on Linux and vz on a Mac.
func (d *deps) newProvider(dirs func(string) (string, error)) (models.Provider, error) {
	switch d.providerName() {
	case gvisor.Name:
		runner, err := d.runnerLocked()
		if err != nil {
			return nil, err
		}

		return d.onBundles(func(bundles *bundle.Service) (models.Provider, error) { return gvisor.New(runner, bundles, dirs) })
	case sysbox.Name:
		runner, err := runccli.New(filepath.Join(d.cfg.Root, "sysbox-runc"), runccli.WithBinary(sysbox.Binary), runccli.WithExecDir(filepath.Join(d.cfg.Root, execDir)))
		if err != nil {
			return nil, err
		}

		return d.onBundles(func(bundles *bundle.Service) (models.Provider, error) { return sysbox.New(runner, bundles, dirs) })
	case runc.Name:
		runner, err := runccli.New(filepath.Join(d.cfg.Root, "runc"), runccli.WithBinary(runc.Binary), runccli.WithExecDir(filepath.Join(d.cfg.Root, execDir)))
		if err != nil {
			return nil, err
		}

		return d.onBundles(func(bundles *bundle.Service) (models.Provider, error) { return runc.New(runner, bundles, dirs) })
	case vzvm.Name:
		return d.newVZ(dirs)
	default:
		return nil, fmt.Errorf("unknown provider %q: shard knows %s, %s, %s and %s", d.cfg.Provider, gvisor.Name, sysbox.Name, runc.Name, vzvm.Name)
	}
}

// onBundles builds a Linux substrate over the OCI bundle service; a VM has an initrd and a disk instead, so vz never comes here.
func (d *deps) onBundles(build func(*bundle.Service) (models.Provider, error)) (models.Provider, error) {
	bundles, err := bundle.New(d.cfg.InitPath)
	if err != nil {
		return nil, err
	}

	return build(bundles)
}

// vzDir is where under the root the vz daemon keeps the signed shim, the guest init and the initrd.
const vzDir = "vz"

// kernelFetchTimeout bounds the first-use download, which runs under deps.mu and would otherwise hold every verb on a dead release endpoint.
const kernelFetchTimeout = 5 * time.Minute

// newVZ builds the Virtualization.framework provider: the embedded shim signed under the root, the guest kernel fetched once, and the stack.
func (d *deps) newVZ(dirs vzvm.StateDirs) (models.Provider, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("provider %s runs on macOS only, not %s", vzvm.Name, runtime.GOOS)
	}

	dir := filepath.Join(d.cfg.Root, vzDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	shim, err := vzshim.Install(dir)
	if err != nil {
		return nil, err
	}
	// SHARD_INIT_PATH names a guest init of its own; without one the daemon installs the linux build it carries beside the shim.
	init := d.cfg.InitPath
	if init == "" {
		if init, err = vzshim.InstallInit(dir); err != nil {
			return nil, err
		}
	}

	opts, err := kernel.FromEnv()
	if err != nil {
		return nil, err
	}
	opts = append(opts, kernel.WithLogger(log.New(d.cfg.Out, "", log.LstdFlags)))
	ctx, cancel := context.WithTimeout(context.Background(), kernelFetchTimeout)
	defer cancel()
	// The guest runs the host's arch: the framework virtualises, it never emulates.
	guest, err := kernel.New(d.cfg.Root, opts...).Ensure(ctx, runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	stack, err := d.stackLocked()
	if err != nil {
		return nil, err
	}

	return vzvm.New(vzvm.Config{
		Shim:        shim,
		Kernel:      guest.Path,
		Init:        init,
		Dir:         dir,
		Stack:       stack,
		Dirs:        dirs,
		SaveRestore: vz.HostSaveRestore(),
	})
}

// execDir is where under the root the driver keeps each exec's scratch, so a restart can sweep what the last daemon left.
const execDir = "exec"

// runner drives the runsc binary. The mode is fixed on it and must match the one the sandbox was
// created with, so every verb builds it here and nowhere else.
func (d *deps) runnerLocked() (*runsc.Runner, error) {
	if d.runnerSvc != nil {
		return d.runnerSvc, nil
	}

	runner, err := runsc.New(filepath.Join(d.cfg.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox), runsc.WithExecDir(filepath.Join(d.cfg.Root, execDir)))
	if err != nil {
		return nil, err
	}
	d.runnerSvc = runner

	return d.runnerSvc, nil
}

func (d *deps) secretsLocked() (*secret.Store, error) {
	if d.secretSvc != nil {
		return d.secretSvc, nil
	}

	store, err := secret.New(filepath.Join(d.cfg.Root, "secrets"), d.holders)
	if err != nil {
		return nil, err
	}
	d.secretSvc = store

	return d.secretSvc, nil
}

// holders names the sandboxes whose record grants the secret, so the store can refuse to move a placeholder under them.
// It takes no lock of its own: it calls the locking d.repo(), and d.mu taken twice by one goroutine deadlocks.
func (d *deps) holders(name string) ([]string, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	return sandbox.SecretHolders(repo, name)
}

// usernsOwner is a provider whose sandboxes' namespaces must belong to a user namespace of its mapping.
type usernsOwner interface {
	Userns() netns.IDMapping
}

// userns is what the network service asks before it builds a namespace. The daemon does not know
// substrates, so an unset mapping is the answer for a provider that is not an owner.
func (d *deps) userns() (netns.IDMapping, error) {
	provider, err := d.provider()
	if err != nil {
		return netns.IDMapping{}, err
	}

	owner, ok := provider.(usernsOwner)
	if !ok {
		return netns.IDMapping{}, nil
	}

	return owner.Userns(), nil
}

// substrateLocked is the provider's own hook for what its runtime keeps under its root. Every
// provider shard knows implements it, so a missing one is a bug, not a host.
func (d *deps) substrateLocked() (sandbox.Substrate, error) {
	provider, err := d.providerLocked()
	if err != nil {
		return nil, err
	}

	sub, ok := provider.(sandbox.Substrate)
	if !ok {
		return nil, fmt.Errorf("provider %s cannot release its runtime root", provider.Name())
	}

	return sub, nil
}

// environmentsLocked is the provider's own hook for where it keeps a guest environment, and every provider has one.
func (d *deps) environmentsLocked() (sandbox.Environments, error) {
	provider, err := d.providerLocked()
	if err != nil {
		return nil, err
	}

	envs, ok := provider.(sandbox.Environments)
	if !ok {
		return nil, fmt.Errorf("provider %s cannot open a guest environment", provider.Name())
	}

	return envs, nil
}

func (d *deps) policiesLocked() (*egress.Store, error) {
	if d.policySvc != nil {
		return d.policySvc, nil
	}

	svc, err := egress.NewStore(filepath.Join(d.cfg.Root, "policies"))
	if err != nil {
		return nil, err
	}
	d.policySvc = svc

	return d.policySvc, nil
}

// egress is what the network service compiles the host rules from. It reads the records and the
// policies, so it needs the stores and never the substrate.
func (d *deps) egressLocked() (*egress.Service, error) {
	policies, err := d.policiesLocked()
	if err != nil {
		return nil, err
	}

	repo, err := d.repoLocked()
	if err != nil {
		return nil, err
	}

	// The free function keeps the compiler off the network service, which is built over the compiler.
	gateway, err := network.Gateway(network.Config{Root: d.cfg.Root})
	if err != nil {
		return nil, err
	}

	return egress.New(policies, repo, gateway, network.DefaultNameservers, nil), nil
}

// egressLog is the decision log every fronted sandbox gets one file of, under its own state directory.
func (d *deps) egressLog() (*egress.Log, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	return egress.NewLog(repo), nil
}

// egressReader is what shard logs --egress reads: the sandbox's own file, which the daemon writes
// both halves into.
func (d *deps) egressReader() (*egress.LogReader, error) {
	decisions, err := d.egressLog()
	if err != nil {
		return nil, err
	}

	return egress.NewLogReader(decisions), nil
}

// proxyCA is the certificate a fronted sandbox is built to trust, minted on the first ask and read back after.
func (d *deps) proxyCA() ([]byte, error) {
	ca, err := proxy.LoadCA(filepath.Join(d.cfg.Root, "proxy"))
	if err != nil {
		return nil, err
	}

	return ca.CertPEM(), nil
}

// lifecycle wires the orchestrator over every layer the sandbox verbs drive, once per daemon.
func (d *deps) lifecycle() (*sandbox.Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	images, err := d.imagesLocked()
	if err != nil {
		return nil, err
	}

	repo, err := d.repoLocked()
	if err != nil {
		return nil, err
	}

	net, err := d.netLocked()
	if err != nil {
		return nil, err
	}

	provider, err := d.providerLocked()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secretsLocked()
	if err != nil {
		return nil, err
	}

	policies, err := d.policiesLocked()
	if err != nil {
		return nil, err
	}

	sub, err := d.substrateLocked()
	if err != nil {
		return nil, err
	}

	envs, err := d.environmentsLocked()
	if err != nil {
		return nil, err
	}

	return sandbox.New(sandbox.Config{
		Repo:         repo,
		Images:       images,
		Network:      net,
		Provider:     provider,
		Secrets:      secrets,
		Policies:     policies,
		Substrate:    sub,
		Environments: envs,
		ProxyCA:      d.proxyCA,
		PullTimeout:  d.cfg.PullTimeout,
	}), nil
}

// stores wires the policy, secret and image verbs. They read and write files, so they need no substrate.
func (d *deps) stores() (*sandbox.Stores, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	repo, err := d.repoLocked()
	if err != nil {
		return nil, err
	}

	images, err := d.imagesLocked()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secretsLocked()
	if err != nil {
		return nil, err
	}

	policies, err := d.policiesLocked()
	if err != nil {
		return nil, err
	}

	return sandbox.NewStores(sandbox.StoresConfig{
		Repo:        repo,
		Policies:    policies,
		Secrets:     secrets,
		Images:      images,
		Network:     func() (sandbox.Reapplier, error) { return d.net() },
		PullTimeout: d.cfg.PullTimeout,
	}), nil
}

// A getter with no lock of its own is reachable only from one that holds it. These four are not.
func (d *deps) repo() (*sandboxstate.Repository, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.repoLocked()
}

func (d *deps) images() (*image.Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.imagesLocked()
}

func (d *deps) provider() (models.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.providerLocked()
}

func (d *deps) net() (hostNetwork, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.netLocked()
}

func (d *deps) front() (front, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.frontLocked()
}

func (d *deps) secrets() (*secret.Store, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.secretsLocked()
}

func (d *deps) egress() (*egress.Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.egressLocked()
}
