package daemon

import (
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/registry"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// deps is every layer the daemon drives. Each one is built on the first ask and kept, so a daemon on
// a host without runsc, netns or root still answers the reads and the store verbs.
type deps struct {
	cfg Config

	imageSvc     *image.Service
	repoSvc      *sandboxstate.Repository
	netSvc       *network.Service
	providerSvc  models.Provider
	substrateSvc *runsc.Runner
	secretSvc    *secret.Store
	policySvc    *egress.Store
	runnerSvc    *runsc.Runner
}

func (d *deps) images() (*image.Service, error) {
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

func (d *deps) repo() (*sandboxstate.Repository, error) {
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

func (d *deps) net() (*network.Service, error) {
	if d.netSvc != nil {
		return d.netSvc, nil
	}

	manager, err := netns.New()
	if err != nil {
		return nil, err
	}

	source, err := d.egress()
	if err != nil {
		return nil, err
	}

	svc, err := network.New(network.Config{Root: d.cfg.Root, Egress: source}, manager)
	if err != nil {
		return nil, err
	}
	d.netSvc = svc

	return d.netSvc, nil
}

func (d *deps) provider() (models.Provider, error) {
	if d.providerSvc != nil {
		return d.providerSvc, nil
	}

	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	runner, err := d.runner()
	if err != nil {
		return nil, err
	}

	bundles, err := bundle.New(d.cfg.InitPath)
	if err != nil {
		return nil, err
	}

	provider, err := gvisor.New(runner, bundles, repo.Dir)
	if err != nil {
		return nil, err
	}
	d.providerSvc = provider

	return d.providerSvc, nil
}

// runner drives the runsc binary. The mode is fixed on it and must match the one the sandbox was
// created with, so every verb builds it here and nowhere else.
func (d *deps) runner() (*runsc.Runner, error) {
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

func (d *deps) secrets() (*secret.Store, error) {
	if d.secretSvc != nil {
		return d.secretSvc, nil
	}

	store, err := secret.New(filepath.Join(d.cfg.Root, "secrets"))
	if err != nil {
		return nil, err
	}
	d.secretSvc = store

	return d.secretSvc, nil
}

// substrate is what the runsc root holds for itself. It belongs to no sandbox, so no per-sandbox
// teardown gives it back.
func (d *deps) substrate() (*runsc.Runner, error) {
	if d.substrateSvc != nil {
		return d.substrateSvc, nil
	}

	runner, err := d.runner()
	if err != nil {
		return nil, err
	}
	d.substrateSvc = runner

	return d.substrateSvc, nil
}

func (d *deps) policies() (*egress.Store, error) {
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

// egress is what the network service compiles the host rules from. It reads the records, the policies
// and the grants, so it needs the stores and never the substrate.
func (d *deps) egress() (*egress.Service, error) {
	policies, err := d.policies()
	if err != nil {
		return nil, err
	}

	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secrets()
	if err != nil {
		return nil, err
	}

	return egress.New(policies, repo, secrets, network.DefaultNameservers, nil), nil
}

// lifecycle wires the orchestrator over every layer the sandbox verbs drive, once per daemon.
func (d *deps) lifecycle() (*sandbox.Service, error) {
	images, err := d.images()
	if err != nil {
		return nil, err
	}

	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	net, err := d.net()
	if err != nil {
		return nil, err
	}

	provider, err := d.provider()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secrets()
	if err != nil {
		return nil, err
	}

	policies, err := d.policies()
	if err != nil {
		return nil, err
	}

	sub, err := d.substrate()
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
		PullTimeout: d.cfg.PullTimeout,
	}), nil
}

// stores wires the policy, secret and image verbs. They read and write files, so they need no substrate.
func (d *deps) stores() (*sandbox.Stores, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	images, err := d.images()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secrets()
	if err != nil {
		return nil, err
	}

	policies, err := d.policies()
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
