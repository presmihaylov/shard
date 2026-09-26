package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/dns"
	"github.com/presmihaylov/shard/pkg/kmsg"
	"github.com/presmihaylov/shard/pkg/proxy"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/broker"
	"github.com/presmihaylov/shard/services/datadir"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/network"
	"github.com/presmihaylov/shard/services/provider/vzvm"
	"github.com/presmihaylov/shard/services/sandbox"
)

// Config is the wiring one resident daemon needs.
type Config struct {
	// Version is what the version route answers with.
	Version string
	Root    string
	Out     io.Writer
	// Insecure lists the registry hosts the daemon may reach over plaintext http. Every other host is https.
	Insecure []string
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
	// InitPath is the host path of the guest supervisor; empty on a Mac boots the linux one the daemon embeds.
	InitPath string
	// Provider names the substrate: gvisor.Name, sysbox.Name, runc.Name, vzvm.Name, firecracker.Name, or empty for the platform's default.
	Provider string
}

// Run supervises the daemon's tasks over one root until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	d := &deps{cfg: cfg}
	// Before the lock: the lock file would be the first entry the xfs mount hides.
	if err := datadir.Ensure(ctx, datadir.Config{Dir: cfg.Root, Provider: d.providerName(), Out: cfg.Out}); err != nil {
		return err
	}
	life := &lifecycle{deps: d, base: ctx}
	self := process{deps: d, startedAt: time.Now().UTC().Truncate(time.Second)}

	err := New(cfg.Root, cfg.Out, apiTask{deps: d, lifecycle: life, process: self}, proxyTask{deps: d}, dnsTask{deps: d}, egressLogRotation{deps: d}, egressLogTailer{deps: d}, liveness{deps: d, lifecycle: life, interval: livenessInterval}, healthCheck{deps: d, lifecycle: life, interval: healthInterval}, restartPolicy{deps: d, lifecycle: life, interval: restartInterval}).WithReconciler(reconciler{deps: d, lifecycle: life}).Run(ctx)

	// The tasks have stopped, so no new create starts; wait out the ones the daemon still runs in the background.
	life.wait()

	return err
}

// reconciler checks the records against the substrate at start. An empty root needs no provider, so a
// host without runsc still gets a daemon that answers the reads and the store verbs.
type reconciler struct {
	deps      *deps
	lifecycle *lifecycle
}

func (r reconciler) Reconcile(ctx context.Context, report func(string)) error {
	// Under the lock every exec scratch under the root is the last daemon's, and its drivers died with it.
	if err := sweepExecs(filepath.Join(r.deps.cfg.Root, execDir), report); err != nil {
		return err
	}

	repo, err := r.deps.repo()
	if err != nil {
		return err
	}

	sandboxes, unreadable := repo.List()
	if unreadable != nil {
		// A record shard cannot read is one it cannot correct either, and refusing to start would fix none.
		report(fmt.Sprintf("some records cannot be read, so they are not checked: %v", unreadable))
	}
	if len(sandboxes) == 0 {
		return nil
	}

	svc, err := r.lifecycle.service()
	if err != nil {
		return err
	}

	return svc.ReconcileAll(ctx, sandboxes, report)
}

// sweepExecs removes the exec scratch a daemon that is gone left under dir, and reports how much there was.
func sweepExecs(dir string, report func(string)) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the exec directory %s: %w", dir, err)
	}

	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sweep the exec directory %s: %w", dir, err)
	}
	if len(entries) > 0 {
		report(fmt.Sprintf("swept %d exec scratch directories the last daemon left under %s", len(entries), dir))
	}

	return nil
}

// apiTask serves the REST API on the socket under the root. The daemon restarts it when the listener dies.
type apiTask struct {
	deps      *deps
	lifecycle *lifecycle
	process   process
}

func (apiTask) Name() string { return "api" }

func (t apiTask) Run(ctx context.Context) error {
	cfg := t.deps.cfg

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	enforcer, err := t.deps.egress()
	if err != nil {
		return err
	}

	stores, err := t.deps.stores()
	if err != nil {
		return err
	}

	decisions, err := t.deps.egressReader()
	if err != nil {
		return err
	}

	listener, mode, group, err := api.Listen(cfg.Root)
	if err != nil {
		return err
	}

	owner := "no " + api.Group + " group on this host"
	if group != "" {
		owner = "group " + group
	}
	log.New(cfg.Out, "", log.LstdFlags).Printf("api listening on %s, mode %04o, %s", filepath.Join(cfg.Root, api.SocketFile), mode, owner)

	handler := api.NewHandler(cfg.Version, t.process, repo, enforcer, t.lifecycle, stores, decisions, cfg.Out)

	return api.Serve(ctx, listener, handler)
}

// process answers GET /v0/daemon. The provider is built on the first ask, as every verb that needs it does.
type process struct {
	deps      *deps
	startedAt time.Time
}

func (p process) Daemon() (api.Daemon, error) {
	provider, err := p.deps.provider()
	if err != nil {
		return api.Daemon{}, err
	}

	return api.Daemon{
		PID:          os.Getpid(),
		StartedAt:    p.startedAt,
		Socket:       filepath.Join(p.deps.cfg.Root, api.SocketFile),
		Provider:     provider.Name(),
		Capabilities: provider.Capabilities(),
		Proxy:        api.Proxy{PlainPort: proxy.PlainPort, TLSPort: proxy.TLSPort},
	}, nil
}

// lifecycle builds the orchestrator on the first verb, so a daemon on a host without runsc still answers reads.
type lifecycle struct {
	deps *deps
	// base outlives one request: a create's background pull and start run under it, and it ends when the daemon stops.
	base context.Context

	mu  sync.Mutex
	svc *sandbox.Service
	// pending names each create the daemon still runs, so a wait knows when the sandbox leaves pending.
	pending map[string]chan struct{}
	// wg holds the background creates, so a shutdown does not leave one half-built.
	wg sync.WaitGroup
}

func (l *lifecycle) service() (*sandbox.Service, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.svc != nil {
		return l.svc, nil
	}

	svc, err := l.deps.lifecycle()
	if err != nil {
		return nil, err
	}
	l.svc = svc

	return l.svc, nil
}

// Create runs synchronously when the image is cached and answers running, so a create off a warm cache
// keeps its shape. An uncached image records the sandbox pending and pulls, builds and starts it in the
// background, where it lands running or failed. A wait blocks on the record leaving pending.
func (l *lifecycle) Create(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	images, err := l.deps.images()
	if err != nil {
		return models.Sandbox{}, err
	}

	// A cached image needs no pull, so the create finishes here and lands running; only an uncached one goes async.
	cached, err := images.Cached(req.Image)
	if err != nil {
		return models.Sandbox{}, err
	}
	if cached {
		return svc.Create(ctx, req)
	}

	sb, err := svc.Prepare(ctx, req)
	if err != nil {
		return models.Sandbox{}, err
	}

	done := make(chan struct{})
	l.mu.Lock()
	if l.pending == nil {
		l.pending = map[string]chan struct{}{}
	}
	l.pending[sb.ID] = done
	l.mu.Unlock()

	// The pull outlives the request, so it runs under base, not the caller's context, and ends with the daemon.
	l.wg.Go(func() {
		completeErr := svc.Complete(l.base, sb.ID, req)

		l.mu.Lock()
		delete(l.pending, sb.ID)
		l.mu.Unlock()
		close(done)

		if completeErr != nil {
			log.New(l.deps.cfg.Out, "", log.LstdFlags).Printf("create %s failed: %v", sb.ID, completeErr)
		}
	})

	return sb, nil
}

// WaitState blocks until the sandbox leaves pending, or answers at once when no create runs behind it.
func (l *lifecycle) WaitState(ctx context.Context, ref string) error {
	repo, err := l.deps.repo()
	if err != nil {
		return err
	}

	id, err := repo.Resolve(ref)
	if err != nil {
		return err
	}

	l.mu.Lock()
	done, ok := l.pending[id]
	l.mu.Unlock()
	if !ok {
		return nil
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait blocks until every background create has ended, so a stopped daemon leaves none half-built.
func (l *lifecycle) wait() { l.wg.Wait() }

func (l *lifecycle) GrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.GrantSecret(ctx, ref, name)
}

func (l *lifecycle) UngrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.UngrantSecret(ctx, ref, name)
}

func (l *lifecycle) AttachPolicy(ctx context.Context, ref, name string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.AttachPolicy(ctx, ref, name)
}

func (l *lifecycle) DetachPolicy(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.DetachPolicy(ctx, ref)
}

func (l *lifecycle) Start(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Start(ctx, ref)
}

func (l *lifecycle) Stop(ctx context.Context, ref string, grace time.Duration) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Stop(ctx, ref, grace)
}

func (l *lifecycle) Remove(ctx context.Context, ref string, force bool, grace time.Duration) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.Remove(ctx, ref, force, grace)
}

func (l *lifecycle) Pause(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Pause(ctx, ref)
}

func (l *lifecycle) Resume(ctx context.Context, ref string) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Resume(ctx, ref)
}

func (l *lifecycle) Fork(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Fork(ctx, ref, req)
}

func (l *lifecycle) Clone(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	svc, err := l.service()
	if err != nil {
		return models.Sandbox{}, err
	}

	return svc.Clone(ctx, ref, req)
}

func (l *lifecycle) CreateExec(ctx context.Context, ref string, req sandbox.ExecRequest) (models.Exec, error) {
	svc, err := l.service()
	if err != nil {
		return models.Exec{}, err
	}

	return svc.CreateExec(ctx, ref, req)
}

func (l *lifecycle) Attach(ctx context.Context, ref, execID string, streams sandbox.Streams) (models.ExitStatus, error) {
	svc, err := l.service()
	if err != nil {
		return models.ExitStatus{}, err
	}

	return svc.Attach(ctx, ref, execID, streams)
}

func (l *lifecycle) ListExecs(ctx context.Context, ref string) ([]models.Exec, error) {
	svc, err := l.service()
	if err != nil {
		return nil, err
	}

	return svc.ListExecs(ctx, ref)
}

func (l *lifecycle) GetExec(ctx context.Context, ref, execID string) (models.Exec, error) {
	svc, err := l.service()
	if err != nil {
		return models.Exec{}, err
	}

	return svc.GetExec(ctx, ref, execID)
}

func (l *lifecycle) WaitExec(ctx context.Context, ref, execID string) (models.Exec, error) {
	svc, err := l.service()
	if err != nil {
		return models.Exec{}, err
	}

	return svc.WaitExec(ctx, ref, execID)
}

func (l *lifecycle) KillExec(ctx context.Context, ref, execID, signal string) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.KillExec(ctx, ref, execID, signal)
}

func (l *lifecycle) DeleteExec(ctx context.Context, ref, execID string) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.DeleteExec(ctx, ref, execID)
}

func (l *lifecycle) ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.ResizeExec(ctx, ref, execID, size)
}

func (l *lifecycle) Logs(ctx context.Context, ref string, w io.Writer) error {
	svc, err := l.service()
	if err != nil {
		return err
	}

	return svc.Logs(ctx, ref, w)
}

func (l *lifecycle) FollowLogs(ctx context.Context, ref string, w io.Writer) (string, error) {
	svc, err := l.service()
	if err != nil {
		return "", err
	}

	return svc.FollowLogs(ctx, ref, w)
}

// proxyTask runs the egress proxy every fronted sandbox's web traffic is turned to, on the bridge gateway.
type proxyTask struct {
	deps *deps
}

func (proxyTask) Name() string { return "proxy" }

func (t proxyTask) Run(ctx context.Context) error {
	cfg := t.deps.cfg

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	secrets, err := t.deps.secrets()
	if err != nil {
		return err
	}

	source, err := t.deps.egress()
	if err != nil {
		return err
	}

	decisions, err := t.deps.egressLog()
	if err != nil {
		return err
	}

	// Nothing can listen on the gateway before the bridge carries it, so the host side is built first.
	hostNet, err := t.deps.net()
	if err != nil {
		return err
	}
	if err := hostNet.ReapplyAll(ctx); err != nil {
		return err
	}

	ca, err := proxy.LoadCA(filepath.Join(cfg.Root, "proxy"))
	if err != nil {
		return err
	}

	logger := log.New(cfg.Out, "", log.LstdFlags)
	server, err := proxy.New(proxy.Config{
		Address:  hostNet.Gateway(),
		CA:       ca,
		Director: broker.New(repo, source, secrets, decisions),
		Log:      logger,
	})
	if err != nil {
		return err
	}

	front, err := t.deps.front()
	if err != nil {
		return err
	}
	plain, err := front.ListenTCP(proxy.PlainPort)
	if err != nil {
		return fmt.Errorf("listen for plain http: %w", err)
	}
	secure, err := front.ListenTCP(proxy.TLSPort)
	if err != nil {
		return errors.Join(fmt.Errorf("listen for tls: %w", err), plain.Close())
	}

	logger.Printf("proxy listening on %s, plain %d and tls %d", hostNet.Gateway(), proxy.PlainPort, proxy.TLSPort)

	return server.Serve(ctx, plain, secure)
}

// dnsTask runs the resolver every policy sandbox's lookups are turned to, on the bridge gateway beside the proxy.
type dnsTask struct {
	deps *deps
}

func (dnsTask) Name() string { return "dns" }

func (t dnsTask) Run(ctx context.Context) error {
	cfg := t.deps.cfg

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	secrets, err := t.deps.secrets()
	if err != nil {
		return err
	}

	source, err := t.deps.egress()
	if err != nil {
		return err
	}

	decisions, err := t.deps.egressLog()
	if err != nil {
		return err
	}

	hostNet, err := t.deps.net()
	if err != nil {
		return err
	}
	if err := hostNet.ReapplyAll(ctx); err != nil {
		return err
	}

	// The resolver forwards to the nameservers the host compiles a name rule through, so guest and host agree.
	upstreams := make([]netip.AddrPort, 0, len(network.DefaultNameservers))
	for _, nameserver := range network.DefaultNameservers {
		upstreams = append(upstreams, netip.AddrPortFrom(nameserver, dns.Port))
	}

	logger := log.New(cfg.Out, "", log.LstdFlags)
	server, err := dns.New(dns.Config{
		Address:   hostNet.Gateway(),
		Upstreams: upstreams,
		Director:  broker.New(repo, source, secrets, decisions),
		Log:       logger,
	})
	if err != nil {
		return err
	}

	front, err := t.deps.front()
	if err != nil {
		return err
	}
	udp, err := front.ListenPacket(dns.Port)
	if err != nil {
		return fmt.Errorf("listen for udp questions: %w", err)
	}
	tcp, err := front.ListenTCP(dns.Port)
	if err != nil {
		return errors.Join(fmt.Errorf("listen for tcp questions: %w", err), udp.Close())
	}

	logger.Printf("dns resolver listening on %s, udp and tcp %d", hostNet.Gateway(), dns.Port)

	return server.Serve(ctx, udp, tcp)
}

// egressLogTailer moves the host's drops out of the kernel ring and into the sandbox's own log, where
// they are as durable as the proxy's decisions and a follow is a tail of one file.
type egressLogTailer struct {
	deps *deps
}

func (egressLogTailer) Name() string { return "egress-log-tailer" }

func (t egressLogTailer) Run(ctx context.Context) error {
	// A VM host drops in the userspace stack, which writes each drop into the log as it refuses the frame, so there is no ring to tail.
	if t.deps.providerName() == vzvm.Name {
		return nil
	}

	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	decisions, err := t.deps.egressLog()
	if err != nil {
		return err
	}

	ring, err := kmsg.Open()
	if err != nil {
		return err
	}
	defer ring.Close()

	logger := log.New(t.deps.cfg.Out, "", log.LstdFlags)

	if err := egress.NewTailer(t.deps.cfg.Root, decisions, repo, logger).Run(ctx, ring); err != nil {
		return err
	}

	if lost := ring.Overwritten(); lost > 0 {
		logger.Printf("egress log: the ring overwrote %d records under the tailer", lost)
	}

	return nil
}

// egressLogRotation keeps every sandbox's decision log bounded. It renames rather than truncates, so an
// O_APPEND writer that holds the old file keeps writing into a file the reader still prints.
type egressLogRotation struct {
	deps *deps
}

const (
	// maxEgressLog is what one log may reach before it is renamed, and one renamed file is kept behind it.
	maxEgressLog      = 8 << 20
	egressLogInterval = time.Minute
)

func (egressLogRotation) Name() string { return "egress-log-rotation" }

func (t egressLogRotation) Run(ctx context.Context) error {
	repo, err := t.deps.repo()
	if err != nil {
		return err
	}

	decisions, err := t.deps.egressLog()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(egressLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		sandboxes, err := repo.List()
		if err != nil {
			return err
		}

		for _, sb := range sandboxes {
			if err := decisions.Rotate(sb.ID, maxEgressLog); err != nil {
				return err
			}
		}
	}
}
