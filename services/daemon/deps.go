package daemon

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/pkg/registry"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/pkg/sysboxrunc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/sysbox"
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

	imageSvc    *image.Service
	repoSvc     *sandboxstate.Repository
	netSvc      *network.Service
	providerSvc models.Provider
	secretSvc   *secret.Store
	policySvc   *egress.Store
	runnerSvc   *runsc.Runner
}

func (d *deps) imagesLocked() (*image.Service, error) {
	if d.imageSvc != nil {
		return d.imageSvc, nil
	}

	svc, err := image.New(filepath.Join(d.cfg.Root, "images"), registry.WithInsecureRegistries(d.cfg.Insecure...))
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

func (d *deps) netLocked() (*network.Service, error) {
	if d.netSvc != nil {
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

	bundles, err := bundle.New(d.cfg.InitPath)
	if err != nil {
		return nil, err
	}

	provider, err := d.newProvider(bundles, repo.Dir)
	if err != nil {
		return nil, err
	}
	d.providerSvc = provider

	return d.providerSvc, nil
}

// newProvider picks the substrate --provider named. The daemon runs one; gVisor is the default.
func (d *deps) newProvider(bundles *bundle.Service, dirs func(string) (string, error)) (models.Provider, error) {
	switch d.cfg.Provider {
	case "", gvisor.Name:
		runner, err := d.runnerLocked()
		if err != nil {
			return nil, err
		}

		return gvisor.New(runner, bundles, dirs)
	case sysbox.Name:
		runner, err := sysboxrunc.New(filepath.Join(d.cfg.Root, "sysbox-runc"))
		if err != nil {
			return nil, err
		}

		return sysbox.New(runner, bundles, dirs)
	default:
		return nil, fmt.Errorf("unknown provider %q: shard knows %s and %s", d.cfg.Provider, gvisor.Name, sysbox.Name)
	}
}

// runner drives the runsc binary. The mode is fixed on it and must match the one the sandbox was
// created with, so every verb builds it here and nowhere else.
func (d *deps) runnerLocked() (*runsc.Runner, error) {
	if d.runnerSvc != nil {
		return d.runnerSvc, nil
	}

	runner, err := runsc.New(filepath.Join(d.cfg.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox))
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

	return egress.New(policies, repo, network.DefaultNameservers, nil), nil
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

	return sandbox.New(sandbox.Config{
		Repo:        repo,
		Images:      images,
		Network:     net,
		Provider:    provider,
		Secrets:     secrets,
		Policies:    policies,
		Substrate:   sub,
		ProxyCA:     d.proxyCA,
		PullTimeout: d.cfg.PullTimeout,
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

func (d *deps) provider() (models.Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.providerLocked()
}

func (d *deps) net() (*network.Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.netLocked()
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
