package cli

import (
	"context"
	"os"
	"path/filepath"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/registry"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/broker"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// imageService is the part of image.Service the commands drive. The service is a struct, so this is
// the only seam a test can put a fake behind.
type imageService interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
	List() ([]image.Image, error)
	Hold(ctx context.Context) (func() error, error)
	Orphaned(ref string) ([]string, error)
	Remove(ctx context.Context, ref string, free func() error) error
}

// sandboxRepo is the part of sandboxstate.Repository the commands drive.
type sandboxRepo interface {
	Create(sb models.Sandbox) (models.Sandbox, error)
	Get(id string) (models.Sandbox, error)
	Resolve(ref string) (string, error)
	List() ([]models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
	Hold(ctx context.Context, id string) (func() error, error)
	HoldShared(ctx context.Context, id string) (func() error, error)
	Dir(id string) (string, error)
	SnapshotDir(id string) (string, error)
}

// sandboxNetwork is the part of network.Service the commands drive.
type sandboxNetwork interface {
	Allocate(ctx context.Context, id string) (models.NetworkSpec, error)
	Release(ctx context.Context, id string) error
	Reapply(ctx context.Context, id string) error
	ReapplyAll(ctx context.Context) error
}

// secretStore is the part of secret.Store the commands drive. Only the proxy reads a value, per request.
type secretStore interface {
	Set(name, value string, up secret.Update) (secret.Secret, error)
	Get(name string) (secret.Secret, error)
	Value(name string) (string, error)
	List() ([]secret.Secret, error)
	Remove(name string) error
}

// substrate is what the runsc root holds for itself. It belongs to no sandbox, so no per-sandbox
// teardown gives it back.
type substrate interface {
	DropNullNetns() error
}

// deps is every layer a shard command can drive. Each one is built on the first ask and kept, so a
// command that never asks for the provider or the network never needs runsc, netns or root: that is
// what keeps version, pull and image working off Linux and off root.
type deps struct {
	app App

	imageSvc     imageService
	repoSvc      sandboxRepo
	netSvc       sandboxNetwork
	providerSvc  models.Provider
	substrateSvc substrate
	secretSvc    secretStore
	policySvc    *egress.Store
	runnerSvc    *runsc.Runner
	proxySvc     egressProxy

	// The terminal this shard process holds. A test replaces the three files: a pipe is not a terminal.
	inFile  *os.File
	outFile *os.File
	errFile *os.File
}

// deps builds what the command is about to drive, through the seam a test replaces.
func (a App) deps() *deps {
	if a.newDeps != nil {
		return a.newDeps(a)
	}

	return &deps{app: a}
}

func (d *deps) images() (imageService, error) {
	if d.imageSvc != nil {
		return d.imageSvc, nil
	}

	svc, err := image.New(filepath.Join(d.app.Root, "images"), registry.WithInsecureRegistries(d.app.Insecure...))
	if err != nil {
		return nil, err
	}
	d.imageSvc = svc

	return d.imageSvc, nil
}

func (d *deps) repo() (sandboxRepo, error) {
	if d.repoSvc != nil {
		return d.repoSvc, nil
	}

	repo, err := sandboxstate.New(d.app.Root)
	if err != nil {
		return nil, err
	}
	d.repoSvc = repo

	return d.repoSvc, nil
}

func (d *deps) net() (sandboxNetwork, error) {
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

	svc, err := network.New(network.Config{Root: d.app.Root, Egress: source}, manager)
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

	bundles, err := bundle.New(d.app.InitPath)
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
// created with, so every command builds it here and nowhere else.
func (d *deps) runner() (*runsc.Runner, error) {
	if d.runnerSvc != nil {
		return d.runnerSvc, nil
	}

	runner, err := runsc.New(filepath.Join(d.app.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox))
	if err != nil {
		return nil, err
	}
	d.runnerSvc = runner

	return d.runnerSvc, nil
}

// secrets is a plain file store, so a test drives the real one under a temporary root.
func (d *deps) secrets() (secretStore, error) {
	if d.secretSvc != nil {
		return d.secretSvc, nil
	}

	store, err := secret.New(filepath.Join(d.app.Root, "secrets"))
	if err != nil {
		return nil, err
	}
	d.secretSvc = store

	return d.secretSvc, nil
}

func (d *deps) substrate() (substrate, error) {
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

func (d *deps) stdin() *os.File {
	if d.inFile == nil {
		d.inFile = os.Stdin
	}

	return d.inFile
}

func (d *deps) stdout() *os.File {
	if d.outFile == nil {
		d.outFile = os.Stdout
	}

	return d.outFile
}

func (d *deps) stderr() *os.File {
	if d.errFile == nil {
		d.errFile = os.Stderr
	}

	return d.errFile
}

func (d *deps) policies() (*egress.Store, error) {
	if d.policySvc != nil {
		return d.policySvc, nil
	}

	svc, err := egress.NewStore(filepath.Join(d.app.Root, "policies"))
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

// proxy is what a verb asks for the CA and for a running proxy. It reads the layout the proxy verb
// reads, so the two agree on the ports.
func (d *deps) proxy() (egressProxy, error) {
	if d.proxySvc != nil {
		return d.proxySvc, nil
	}

	cfg, _, err := network.Layout(network.Config{Root: d.app.Root})
	if err != nil {
		return nil, err
	}
	d.proxySvc = launcher{root: d.app.Root, ports: cfg.Proxy}

	return d.proxySvc, nil
}

// broker is the proxy's director. It reads the same stores the host rules compile from, so a request
// is judged by what the chain would have judged it by.
func (d *deps) broker() (*broker.Service, error) {
	repo, err := d.repo()
	if err != nil {
		return nil, err
	}

	policies, err := d.egress()
	if err != nil {
		return nil, err
	}

	secrets, err := d.secrets()
	if err != nil {
		return nil, err
	}

	return broker.New(repo, policies, secrets, egress.NewEvents(repo.Dir)), nil
}
