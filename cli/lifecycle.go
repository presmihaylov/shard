package cli

import (
	"context"
	"path/filepath"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netns"
	"github.com/presmihaylov/shard/pkg/runsc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// lifecycleRepo is the part of sandboxstate.Repository that stop and rm drive.
type lifecycleRepo interface {
	Get(id string) (models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
}

// lifecycleNetwork is the part of network.Service that rm drives. A stop keeps the address.
type lifecycleNetwork interface {
	Release(ctx context.Context, id string) error
}

// lifecycleProvider is the part of models.Provider that stop and rm drive.
type lifecycleProvider interface {
	Status(ctx context.Context, id string) (models.Status, error)
	Stop(ctx context.Context, id string, grace time.Duration) error
	Remove(ctx context.Context, id string) error
	Wait(ctx context.Context, id string) (models.ExitStatus, error)
}

// lifecycleDeps is what stop and rm wire together. They share one set, because rm --force is a stop
// followed by a remove. A test replaces the factory, because each real part needs Linux and root.
type lifecycleDeps struct {
	repo     lifecycleRepo
	net      lifecycleNetwork
	provider lifecycleProvider
}

// defaultLifecycleDeps builds the real layers, which all refuse off Linux.
func defaultLifecycleDeps(a App) (lifecycleDeps, error) {
	repo, err := sandboxstate.New(a.Root)
	if err != nil {
		return lifecycleDeps{}, err
	}

	manager, err := netns.New()
	if err != nil {
		return lifecycleDeps{}, err
	}

	net, err := network.New(network.Config{Root: a.Root}, manager)
	if err != nil {
		return lifecycleDeps{}, err
	}

	// The mode is fixed on the runner, and it must match the one the sandbox was created with.
	runner, err := runsc.New(filepath.Join(a.Root, "runsc"), runsc.WithNetwork(runsc.NetworkSandbox))
	if err != nil {
		return lifecycleDeps{}, err
	}

	bundles, err := bundle.New(a.InitPath)
	if err != nil {
		return lifecycleDeps{}, err
	}

	provider, err := gvisor.New(runner, bundles, repo.Dir)
	if err != nil {
		return lifecycleDeps{}, err
	}

	return lifecycleDeps{repo: repo, net: net, provider: provider}, nil
}

// lifecycle builds what stop and rm drive, through the factory a test replaces.
func (a App) lifecycle() (lifecycleDeps, error) {
	build := a.newLifecycleDeps
	if build == nil {
		build = defaultLifecycleDeps
	}

	return build(a)
}
