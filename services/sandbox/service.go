package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// DefaultStopGrace is how long the entrypoint gets to answer SIGTERM before shard kills it.
const DefaultStopGrace = 10 * time.Second

// DefaultStopSettle is how long past Provider.Stop a stop waits for the substrate to report the sandbox gone.
const DefaultStopSettle = 5 * time.Second

// MaxMemoryMiB is 16 TiB, which is past any host and far below the point where MiB times 2^20 wraps.
const MaxMemoryMiB = 1 << 24

// Repository is the part of sandboxstate.Repository the lifecycle verbs drive.
type Repository interface {
	Reader
	Create(sb models.Sandbox) (models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
	Dir(id string) (string, error)
	SnapshotDir(id string) (string, error)
}

// Images is the part of image.Service a create drives.
type Images interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
}

// Network is the part of network.Service the lifecycle verbs drive.
type Network interface {
	Allocate(ctx context.Context, id string) (models.NetworkSpec, error)
	Release(ctx context.Context, id string) error
	Reapply(ctx context.Context, id string) error
	ReapplyAll(ctx context.Context) error
}

// Secrets is the part of secret.Store a create reads. No verb reads a value: that is the proxy's.
type Secrets interface {
	Get(name string) (secret.Secret, error)
}

// Policies is the part of egress.Store a create reads.
type Policies interface {
	Get(name string) (models.Policy, error)
}

// Substrate is what the runtime keeps under its own root, which no per-sandbox teardown gives back.
// Each provider implements it for its runtime; one that keeps nothing answers nil.
type Substrate interface {
	ReleaseRoot() error
}

// Config is every layer the orchestrator drives. The daemon builds each one once.
type Config struct {
	Repo      Repository
	Images    Images
	Network   Network
	Provider  models.Provider
	Secrets   Secrets
	Policies  Policies
	Substrate Substrate
	// ProxyCA hands a fronted sandbox the certificate it must trust, so the proxy can terminate its TLS.
	ProxyCA func() ([]byte, error)
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
	// StopSettle overrides DefaultStopSettle, which only a test has a reason to do.
	StopSettle time.Duration
}

// Service owns create, start, stop and rm, and serializes them per sandbox in memory: one process holds it.
type Service struct {
	cfg Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// execs holds every exec from its create to its end, so an attach and a resize find it by id.
	execMu sync.Mutex
	execs  map[string]*execSession
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg, locks: map[string]*sync.Mutex{}, execs: map[string]*execSession{}}
}

// CreateRequest is what a create names. It is the JSON body of POST /v0/sandboxes.
type CreateRequest struct {
	Image   string   `json:"image"`
	Name    string   `json:"name,omitempty"`
	Command []string `json:"command,omitempty"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// Secrets is what the guest gets a placeholder for, each under its own name.
	Secrets []string `json:"secrets,omitempty"`
	// Policy is what the host enforces for the sandbox.
	Policy    string           `json:"policy,omitempty"`
	Resources models.Resources `json:"resources"`
	// RestartOnOOM asks the daemon to start the sandbox again when the host ends it for its memory.
	RestartOnOOM bool `json:"restart_on_oom,omitempty"`
	// MaxOOMRestarts caps those starts in a row, 0 for unlimited.
	MaxOOMRestarts int `json:"max_oom_restarts,omitempty"`
	// Health is the probe the daemon runs while the sandbox runs, nil for none.
	Health *models.HealthCheck `json:"health,omitempty"`
	// Restart is when the supervisor starts the entrypoint again inside the sandbox, nil for never.
	Restart *models.RestartSpec `json:"restart,omitempty"`
}

// fronted says the sandbox's web traffic goes through the proxy, which a policy and a grant both need.
func (r CreateRequest) fronted() bool { return r.Policy != "" || len(r.Secrets) != 0 }

// RequestError is a request refused before anything was claimed, and never the state of a sandbox.
type RequestError struct {
	Err error
}

func (e *RequestError) Error() string { return e.Err.Error() }

func (e *RequestError) Unwrap() error { return e.Err }

// StateError is a verb refused for the state the sandbox is in. Fix says what the operator does instead.
type StateError struct {
	ID    string
	State models.State
	Fix   string
	// Code names the state the verb wanted, for the program that reads the API body.
	Code models.Code
}

func (e *StateError) Error() string { return fmt.Sprintf("sandbox %s is %s: %s", e.ID, e.State, e.Fix) }

// FailedGuard refuses every verb but get and rm on a failed sandbox, with the one code that names it.
// A create that never reached running is terminal, so an operator reads the reason and then removes it.
func FailedGuard(id string, sb models.Sandbox) error {
	if sb.State != models.StateFailed {
		return nil
	}

	return &StateError{ID: id, State: sb.State, Fix: fmt.Sprintf("%s; remove it with shard rm %s", sb.FailedReason, id), Code: models.CodeSandboxFailed}
}

// lock serializes the verbs on one sandbox; a mutex outlives its id, which is small and never contended.
func (s *Service) lock(id string) func() {
	s.mu.Lock()
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	s.mu.Unlock()

	m.Lock()

	return m.Unlock
}

// Prepare writes the pending record and answers at once. The pull and the start run later, in Complete,
// so a create returns before the download and the sandbox reaches running or failed in the background.
func (s *Service) Prepare(ctx context.Context, req CreateRequest) (models.Sandbox, error) {
	if err := validate(req); err != nil {
		return models.Sandbox{}, err
	}

	// The canonical reference is what a prune keys a hold on, so the pending record must carry it before
	// the pull: a prune between the record and the pull would otherwise delete the rootfs the create needs.
	ref, err := image.Canonical(req.Image)
	if err != nil {
		return models.Sandbox{}, &RequestError{Err: err}
	}

	// Before the pull: a secret or a policy that does not exist should cost no create, and a fronted
	// sandbox with no proxy CA has no bundle to build.
	if _, err := s.grantSecrets(req); err != nil {
		return models.Sandbox{}, err
	}
	if req.Policy != "" {
		if _, err := s.cfg.Policies.Get(req.Policy); err != nil {
			return models.Sandbox{}, &RequestError{Err: err}
		}
	}
	if req.fronted() {
		if _, err := s.proxyCA(); err != nil {
			return models.Sandbox{}, err
		}
	}

	sb, err := s.cfg.Repo.Create(models.Sandbox{
		Name:           req.Name,
		Image:          ref,
		Provider:       s.cfg.Provider.Name(),
		State:          models.StatePending,
		Resources:      req.Resources,
		Secrets:        req.Secrets,
		Policy:         req.Policy,
		RestartOnOOM:   req.RestartOnOOM,
		MaxOOMRestarts: req.MaxOOMRestarts,
		HealthCheck:    withHealthDefaults(req.Health),
		Health:         startingHealth(req.Health),
		Restart:        withRestartDefaults(req.Restart),
		CreatedAt:      time.Now().UTC(),
	})
	if err != nil {
		return models.Sandbox{}, err
	}

	return sb, nil
}

// Complete pulls the image, builds the sandbox and starts it, then moves the record from pending to
// running. A failure that is not a shutdown leaves the record failed with the reason, so a get reads why
// and rm still frees it. It pushes every claim before the commit point onto the teardown stack.
func (s *Service) Complete(ctx context.Context, id string, req CreateRequest) (err error) {
	unlock := s.lock(id)
	defer unlock()

	// Past the commit point the sandbox is live, so a later error names it and never marks it failed.
	committed := false

	// A shutdown cancels the work rather than fails the create: the next daemon reconciles the pending
	// record against the substrate, so an interrupted start that took is found running, not lost.
	defer func() {
		if err != nil && ctx.Err() == nil && !committed {
			err = s.fail(ctx, id, err)
		}
	}()

	env, err := s.grantSecrets(req)
	if err != nil {
		return err
	}

	var proxyCA []byte
	if req.fronted() {
		if proxyCA, err = s.proxyCA(); err != nil {
			return err
		}
	}

	var td Teardown

	defer func() {
		if err != nil {
			err = errors.Join(err, td.Unwind(ctx))
		}
	}()

	img, dir, err := s.claim(ctx, id, req)
	if err != nil {
		return err
	}

	// Allocate rolls back its own attach only: a failure between the lease claim and the attach leaks
	// the lease file, so the push goes above the call. Release tolerates a lease that was never taken.
	td.Push(func(ctx context.Context) error { return s.cfg.Network.Release(ctx, id) })

	// The id names the netns, the lease holder and the runsc container, so it must exist first.
	netSpec, err := AllocateNetwork(ctx, s.cfg.Network, id)
	if err != nil {
		return err
	}

	spec := runspec.Resolve(models.SandboxSpec{
		ID:         id,
		Name:       req.Name,
		RootFS:     img.RootFS,
		StateDir:   dir,
		Entrypoint: req.Command,
		Env:        env,
		WorkDir:    req.WorkDir,
		User:       req.User,
		Network:    netSpec,
		Resources:  req.Resources,
		Restart:    restartSpecOf(withRestartDefaults(req.Restart)),
		ProxyCA:    proxyCA,
	}, img.Config)

	// Create rolls back its own mount only, and an interrupt can leave the sandbox process runsc
	// already forked, so the push goes above the call. Remove tolerates an id runsc never held.
	td.Push(func(ctx context.Context) error { return s.cfg.Provider.Remove(ctx, id) })

	if err := s.cfg.Provider.Create(ctx, spec); err != nil {
		return err
	}

	if err := s.recordCreated(ctx, spec); err != nil {
		return err
	}

	// The rules are keyed by the address, which the record holds only now, so the host learns it before the guest runs.
	if req.fronted() {
		if err := s.cfg.Network.Reapply(ctx, id); err != nil {
			return err
		}
	}

	if err := s.cfg.Provider.Start(ctx, id); err != nil {
		// An interrupt kills the start process, not what it may already have started, and the substrate
		// cannot tell the two apart. Only stop ends a sandbox, so an unknown outcome is kept.
		if ctx.Err() != nil {
			td.Discard()

			return fmt.Errorf("the start of sandbox %s was interrupted, so it may be running and it stays on the host: %w", id, err)
		}

		return err
	}

	// The commit point. The entrypoint is live, so nothing below this line gives anything back: only
	// stop ends a sandbox.
	td.Discard()
	committed = true

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s is running but its record was not updated: %w", id, err)
	}

	return nil
}

// Create is Prepare then Complete, for a caller that wants the sandbox running before it returns. The
// daemon splits the two for an uncached image, so a create over the API can answer pending and finish
// in the background.
func (s *Service) Create(ctx context.Context, req CreateRequest) (models.Sandbox, error) {
	sb, err := s.Prepare(ctx, req)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := s.Complete(ctx, sb.ID, req); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(sb.ID)
}

// WaitState answers at once: this service's Create is synchronous, so a sandbox it holds never sits in pending.
// The daemon composes Prepare and Complete in the background and overrides this with a wait that blocks.
func (s *Service) WaitState(_ context.Context, _ string) error { return nil }

// fail records why a create never reached running. It keeps the record so a get reads the reason and rm
// frees it, and it returns the cause so the synchronous caller still sees the failure.
func (s *Service) fail(ctx context.Context, id string, cause error) error {
	err := s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateFailed
		sb.FailedReason = cause.Error()
		sb.PID = 0

		return nil
	})
	if err != nil {
		return errors.Join(cause, fmt.Errorf("sandbox %s failed but its record was not updated: %w", id, err))
	}

	return cause
}

// ValidName, ValidSecretName and ValidPolicyName let a client refuse a spelling before it asks the daemon.
func ValidName(name string) error { return sandboxstate.ValidName(name) }

func ValidSecretName(name string) error { return secret.ValidName(name) }

func ValidPolicyName(name string) error { return egress.ValidName(name) }

// validate refuses what no store could hold or no verb could take back, before anything is pulled.
func validate(req CreateRequest) error {
	if req.Image == "" {
		return &RequestError{Err: errors.New("the request names no image")}
	}

	if req.Name != "" {
		if err := sandboxstate.ValidName(req.Name); err != nil {
			return err
		}
	}

	// A bound below zero is not a spelling of unbounded, and the substrate would drop it without a word.
	if req.Resources.MemoryMiB < 0 {
		return &RequestError{Err: fmt.Errorf("the memory bound is in MiB and cannot be negative, got %d", req.Resources.MemoryMiB)}
	}
	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if req.Resources.MemoryMiB > MaxMemoryMiB {
		return &RequestError{Err: fmt.Errorf("the memory bound is in MiB and no host holds that much, got %d", req.Resources.MemoryMiB)}
	}
	if req.Resources.VCPUs < 0 {
		return &RequestError{Err: fmt.Errorf("the vcpu bound cannot be negative, got %d", req.Resources.VCPUs)}
	}
	// Only a bound can be run out of: the host never counts an OOM against a sandbox that has none.
	if req.RestartOnOOM && req.Resources.MemoryMiB == 0 {
		return &RequestError{Err: errors.New("restart_on_oom needs a memory bound, and the request sets none")}
	}
	if req.MaxOOMRestarts < 0 {
		return &RequestError{Err: fmt.Errorf("max_oom_restarts is a count and cannot be negative, got %d", req.MaxOOMRestarts)}
	}
	if req.Health != nil {
		if err := validHealthCheck(*req.Health); err != nil {
			return &RequestError{Err: err}
		}
	}
	if req.Restart != nil {
		if err := validRestart(*req.Restart); err != nil {
			return &RequestError{Err: err}
		}
	}

	if req.Policy != "" {
		if err := egress.ValidName(req.Policy); err != nil {
			return &RequestError{Err: err}
		}
	}

	if req.fronted() {
		if err := bundle.TrustsUser(req.Env); err != nil {
			return &RequestError{Err: err}
		}
	}

	// An entry that is not an assignment is dropped by the merge, and the guest then lacks it without a word.
	for _, entry := range req.Env {
		key, _, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return &RequestError{Err: fmt.Errorf("the environment entry %q is not KEY=VALUE", entry)}
		}
		// An env of the same name would either hide the placeholder or be hidden by it, and either is a surprise.
		if slices.Contains(req.Secrets, key) {
			return &RequestError{Err: fmt.Errorf("the secret %s and the environment entry %s name the same variable: the guest gets the placeholder as $%s, so drop the entry", key, key, key)}
		}
	}

	for i, name := range req.Secrets {
		if err := secret.ValidName(name); err != nil {
			return &RequestError{Err: err}
		}
		if slices.Contains(req.Secrets[:i], name) {
			return &RequestError{Err: fmt.Errorf("the secret %s was named twice", name)}
		}
	}

	return nil
}

// grantSecrets checks every secret against the store and hands the guest the placeholder of each as
// an environment variable. The value never comes near this: the proxy substitutes it on the way out.
func (s *Service) grantSecrets(req CreateRequest) ([]string, error) {
	env := slices.Clone(req.Env)

	for _, name := range req.Secrets {
		sec, err := s.cfg.Secrets.Get(name)
		if errors.Is(err, secret.ErrNotFound) {
			return nil, &RequestError{Err: fmt.Errorf("secret %s does not exist: run shard secret set --to <host> %s first", name, name)}
		}
		if err != nil {
			return nil, err
		}

		env = append(env, name+"="+sec.Placeholder)
	}

	return env, nil
}

// claim pulls the image the pending record already references and answers the state dir. The record
// exists before the pull, so a prune keyed on that reference cannot delete the rootfs the create runs.
func (s *Service) claim(ctx context.Context, id string, req CreateRequest) (image.Image, string, error) {
	// A registry that accepts the connection and then stalls would otherwise pin the create forever.
	if s.cfg.PullTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.PullTimeout)
		defer cancel()
	}

	img, err := s.cfg.Images.Pull(ctx, req.Image)
	if err != nil {
		return image.Image{}, "", err
	}

	dir, err := s.cfg.Repo.Dir(id)
	if err != nil {
		return image.Image{}, "", err
	}

	return img, dir, nil
}

// recordCreated copies what the substrate decided into the record, so a later process can reach the
// sandbox without asking the provider again. The state stays created until the start.
func (s *Service) recordCreated(ctx context.Context, spec models.SandboxSpec) error {
	status, err := s.cfg.Provider.Status(ctx, spec.ID)
	if err != nil {
		return err
	}

	return s.cfg.Repo.Update(spec.ID, func(sb *models.Sandbox) error {
		sb.PID = status.PID
		sb.NetnsPath = spec.Network.NetnsPath
		sb.Address = spec.Network.Address
		sb.HostInterface = spec.Network.HostInterface

		return nil
	})
}

// Start runs a stopped sandbox again. Its address, its writable layer and its record all survived
// the stop, so the provider builds the new run over them and the record loses only the old exit.
func (s *Service) Start(ctx context.Context, ref string) (models.Sandbox, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// Two starts of one sandbox would each build the netns; the second waits and then sees it running.
	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StateStopped {
		return models.Sandbox{}, &StateError{ID: id, State: sb.State, Fix: "start takes a stopped sandbox", Code: models.CodeSandboxNotStopped}
	}

	if err := s.start(ctx, id); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

// start is the run itself, for a caller that holds the lock and checked the record says stopped.
func (s *Service) start(ctx context.Context, id string) error {
	// The lease survived the stop, so this hands back the same address over a namespace built again.
	if _, err := s.cfg.Network.Allocate(ctx, id); err != nil {
		return err
	}

	if err := s.cfg.Provider.Start(ctx, id); err != nil {
		return errors.Join(err, Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, false))
	}

	return RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, id, false)
}

// Stop ends the processes and keeps everything rm frees: the record, the lease, the address and the
// writable layer all outlive it, so a start can follow.
func (s *Service) Stop(ctx context.Context, ref string, grace time.Duration) (models.Sandbox, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	unlock := s.lock(id)
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if err := s.stop(ctx, id, grace); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

func (s *Service) stop(ctx context.Context, id string, grace time.Duration) error {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	// A second stop changes nothing: the exit status the first one recorded is the one that happened.
	// Unless a start failed after the substrate came up, which is the one way a stopped record lies.
	if sb.State == models.StateStopped {
		status, err := s.cfg.Provider.Status(ctx, id)
		if err != nil {
			return err
		}
		if !status.Alive() {
			return nil
		}
	}

	if err := s.cfg.Provider.Stop(ctx, id, grace); err != nil {
		return err
	}

	if err := s.awaitStopped(ctx, id); err != nil {
		return err
	}

	// A stop takes the sandbox's execs with it: their buffers go and their commands end.
	s.dropExecs(id)

	// The liveness task may have recorded the exit already; only a still-running entrypoint needs a wait.
	exit := sb.ExitStatus
	if exit == nil {
		exit, err = s.lastExit(ctx, id)
		if err != nil {
			return err
		}
	}
	// The count is read once the run is over, so a start again between two ticks never goes unrecorded.
	restarts, err := s.lastRestarts(ctx, sb)
	if err != nil {
		return err
	}

	return s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateStopped
		sb.PID = 0
		if exit != nil {
			sb.ExitStatus = exit
		}
		if sb.Restart != nil {
			sb.Restart.RestartCount = restarts
		}

		return nil
	})
}

// awaitStopped makes stop mean stopped. runsc can report a sandbox alive for a moment after a clean
// stop, and a rm that lands in that moment would refuse it. The record is written only after this.
func (s *Service) awaitStopped(ctx context.Context, id string) error {
	// The bound excludes the grace on purpose: Provider.Stop already spent it, and the client's own
	// timeout is DefaultTimeout plus the grace, which counting it twice would run past.
	bound := s.cfg.StopSettle
	if bound == 0 {
		bound = DefaultStopSettle
	}
	deadline := time.Now().Add(bound)

	for {
		status, err := s.cfg.Provider.Status(ctx, id)
		if err != nil {
			return err
		}
		if !status.Alive() {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("sandbox %s did not stop within %s: the substrate still reports %s", id, bound, status.State)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// lastExit reads how the entrypoint ended, once the sandbox is already stopped. A sandbox the grace
// ran out on was killed, so its supervisor never recorded one, and that is an outcome not a failure.
func (s *Service) lastExit(ctx context.Context, id string) (*models.ExitStatus, error) {
	status, err := s.cfg.Provider.Wait(ctx, id)
	if errors.Is(err, models.ErrNoExitStatus) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &status, nil
}

// Remove frees everything a stopped sandbox holds. A sandbox that is still up is refused unless
// force says to stop it first, with grace as the stop's.
func (s *Service) Remove(ctx context.Context, ref string, force bool, grace time.Duration) error {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	unlock := s.lock(id)
	defer unlock()

	// The record dies last below, so an id with no record has nothing else left on the host either.
	if _, err := s.cfg.Repo.Get(id); err != nil {
		return err
	}

	if err := s.endIfAlive(ctx, id, force, grace); err != nil {
		return err
	}

	if err := s.free(ctx, id); err != nil {
		return err
	}

	if err := s.dropSubstrateRoot(); err != nil {
		return fmt.Errorf("sandbox %s is removed, but %w", id, err)
	}

	return nil
}

// endIfAlive refuses a sandbox that is still up, because rm frees the writable layer a stop keeps.
// force is the shorthand for the stop the operator would otherwise type first.
func (s *Service) endIfAlive(ctx context.Context, id string, force bool, grace time.Duration) error {
	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	if !force {
		return &StateError{ID: id, State: status.State, Fix: fmt.Sprintf("stop it first with shard stop %s, or pass --force", id), Code: models.CodeSandboxNotStopped}
	}

	return s.stop(ctx, id, grace)
}

// holding is one of the things a stopped sandbox still holds on the host.
type holding struct {
	what string
	free func() error
}

// free gives back everything a stop kept, and stops at the first failure: a step that failed still
// holds what the steps below it name. The record comes after the mount and the namespace, because it
// is the only handle by which either can be found again.
func (s *Service) free(ctx context.Context, id string) error {
	held := []holding{
		{"runtime state and rootfs mount", func() error { return s.cfg.Provider.Remove(ctx, id) }},
		{"netns, veth and address lease", func() error { return s.cfg.Network.Release(ctx, id) }},
		{"record and state directory", func() error { return s.cfg.Repo.Delete(id) }},
		// The ruleset is rendered from the records and the leases, so it is right only once this sandbox is in neither.
		{"host rules", func() error { return s.cfg.Network.ReapplyAll(ctx) }},
	}

	for i, h := range held {
		err := h.free()
		if err == nil {
			continue
		}

		left := make([]string, 0, len(held)-i)
		for _, rest := range held[i:] {
			left = append(left, rest.what)
		}

		return fmt.Errorf("remove sandbox %s: %w: its %s are left on the host", id, err, strings.Join(left, ", its "))
	}

	return nil
}

// dropSubstrateRoot gives back what the substrate keeps for itself once no sandbox is left to use
// it. An operator otherwise meets it as an rm -rf of the root that fails with EBUSY.
// A create that runs beside this one is no reason to keep it: the runtime takes it again on its
// next create, and a live sandbox does not need it to stay up.
func (s *Service) dropSubstrateRoot() error {
	left, err := s.cfg.Repo.List()
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return nil
	}

	return s.cfg.Substrate.ReleaseRoot()
}

// record reads the record back once the verb is done, which is what the caller prints.
func (s *Service) record(id string) (models.Sandbox, error) {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, fmt.Errorf("sandbox %s is done but its record could not be read: %w", id, err)
	}

	return sb, nil
}

// proxyCA is what a fronted sandbox is built to trust. A shard without one fronts nothing, and says so.
func (s *Service) proxyCA() ([]byte, error) {
	if s.cfg.ProxyCA == nil {
		return nil, &RequestError{Err: errors.New("this shard has no proxy CA, so it cannot front a sandbox")}
	}

	return s.cfg.ProxyCA()
}
