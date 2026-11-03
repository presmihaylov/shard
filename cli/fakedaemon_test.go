package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// imageService is the part of image.Service the daemon drives, behind which a test puts a fake.
type imageService interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
	Claim(ctx context.Context, ref string, record func(image.Image) error) (image.Image, error)
	Lookup(ref string) (image.Image, bool, error)
	List() ([]image.Image, error)
	Orphaned(ref string) ([]string, error)
	Remove(ctx context.Context, ref string, free func() error) error
}

// sandboxRepo is the part of sandboxstate.Repository the daemon drives.
type sandboxRepo interface {
	Create(sb models.Sandbox, admit ...func(dir string) error) (models.Sandbox, error)
	Get(id string) (models.Sandbox, error)
	Resolve(ref string) (string, error)
	List() ([]models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
	Dir(id string) (string, error)
	CheckpointDir(id string) (string, error)
}

// sandboxNetwork is the part of network.Service the daemon drives.
type sandboxNetwork interface {
	Allocate(ctx context.Context, id string) (models.NetworkSpec, error)
	Release(ctx context.Context, id string) error
	Reapply(ctx context.Context, id string) error
	ReapplyAll(ctx context.Context) error
}

// substrate is what the runtime keeps under its own root, which belongs to no sandbox.
type substrate interface {
	ReleaseRoot() error
}

// fakeDaemon is a daemon whose layers are fakes, on the socket under its root. The CLI reaches it the
// way it reaches the real one, so a verb is tested over the wire and off Linux.
type fakeDaemon struct {
	app App

	imageSvc     imageService
	repoSvc      sandboxRepo
	netSvc       sandboxNetwork
	providerSvc  models.Provider
	substrateSvc substrate
	secretSvc    *secret.Store
	policySvc    *egress.Store
	snapshotSvc  *sandboxstate.Snapshots
	// egressLog is what shard policy logs prints, canned: the real one reads the kernel ring.
	egressLog []egress.Record
	// proxyCA is what a grant plants in the guest, and nil is a shard that fronts nothing.
	proxyCA []byte

	// The verbs hold exec state between calls, so both are built once and on the first request:
	// a test replaces a layer after the server is up and before the command runs.
	once   sync.Once
	svc    *sandbox.Service
	stores *sandbox.Stores
	life   api.Lifecycle
}

func (f *fakeDaemon) build() {
	f.once.Do(func() {
		f.svc = sandbox.New(sandbox.Config{
			Repo:      f.repoSvc,
			Images:    f.imageSvc,
			Network:   f.netSvc,
			Provider:  f.providerSvc,
			Secrets:   f.secretSvc,
			Policies:  f.policySvc,
			Substrate: f.substrateSvc,
			Snapshots: f.snapshotSvc,
			// A verb test without a repo never grants, so the opener asks the repo only when called.
			Environments: bundle.Opener(func(id string) (string, error) { return f.repoSvc.Dir(id) }),
			ProxyCA: func() ([]byte, error) {
				if f.proxyCA == nil {
					return nil, errors.New("this server needs a proxy certificate authority for policies and secrets; ask its administrator to configure one")
				}

				return f.proxyCA, nil
			},
			PullTimeout: time.Minute,
		})
		f.life = f.svc
		f.stores = sandbox.NewStores(sandbox.StoresConfig{
			Repo:        f.repoSvc,
			Policies:    f.policySvc,
			Compiler:    egress.New(f.policySvc, f.repoSvc, netip.MustParseAddr("10.87.0.1"), network.DefaultNameservers, docsResolver{}),
			Secrets:     f.secretSvc,
			Images:      f.imageSvc,
			Snapshots:   f.snapshotSvc,
			Network:     func() (sandbox.Reapplier, error) { return f.netSvc, nil },
			PullTimeout: time.Minute,
		})
	})
}

func (f *fakeDaemon) policies() (*egress.Store, error) { return f.policySvc, nil }

// docsResolver answers every name with a documentation address and .invalid with nothing, as RFC 6761 has it, so no test asks the network.
type docsResolver struct{}

func (docsResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if strings.HasSuffix(host, ".invalid") {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}

	return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
}

// Daemon answers as the real process does, over the provider the test gave the daemon.
func (f *fakeDaemon) Daemon() (api.Daemon, error) {
	return api.Daemon{
		PID:          4123,
		StartedAt:    time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC),
		Socket:       filepath.Join(f.app.Root, api.SocketFile),
		Provider:     f.providerSvc.Name(),
		Capabilities: f.providerSvc.Capabilities(),
		Proxy:        api.Proxy{PlainPort: 30080, TLSPort: 30443},
	}, nil
}

// fakeEgressLog stands in for the kernel ring, which no unit test on a developer host can read.
type fakeEgressLog struct {
	records []egress.Record
}

func (f fakeEgressLog) Read(models.Sandbox) ([]egress.Record, int, error) { return f.records, 0, nil }

// Follow hands over what the log holds and then ends as a removed sandbox does.
func (f fakeEgressLog) Follow(_ context.Context, _ models.Sandbox, yield func(egress.Record) error) error {
	for _, record := range f.records {
		if err := yield(record); err != nil {
			return err
		}
	}

	return egress.ErrSandboxGone
}

// handler answers a request over the layers the test holds now, not the ones it held at the listen.
func (f *fakeDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.build()

		enforcer := egress.New(f.policySvc, f.repoSvc, netip.MustParseAddr("10.87.0.1"), network.DefaultNameservers, nil)
		api.NewHandler("v-daemon", f, f.repoSvc, enforcer, f.life, f.stores, fakeEgressLog{records: f.egressLog}, nil, io.Discard).ServeHTTP(w, r)
	})
}

// serveDaemon answers on the socket under the app's root, the way the daemon does over the real stores.
func serveDaemon(t *testing.T, f *fakeDaemon) {
	t.Helper()

	// Every image verb reads the snapshots, so each fake daemon keeps a real store under its root.
	if f.snapshotSvc == nil {
		snaps, err := sandboxstate.NewSnapshots(f.app.Root)
		if err != nil {
			t.Fatalf("NewSnapshots: %v", err)
		}
		f.snapshotSvc = snaps
	}

	listener, err := net.Listen("unix", filepath.Join(f.app.Root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := httptest.NewUnstartedServer(f.handler())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
}
