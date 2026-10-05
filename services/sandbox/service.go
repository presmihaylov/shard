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
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/runspec"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// DefaultStopSettle is how long past Provider.Stop a stop waits for the substrate to report the sandbox gone.
const DefaultStopSettle = 5 * time.Second

// DefaultProbeBudget bounds one daemon- or verb-initiated Provider.Status, so a wedged substrate call
// cannot pin a sandbox's lock or run a verb past its own timeout.
const DefaultProbeBudget = 10 * time.Second

// DefaultStartBudget bounds one start's substrate work, past gvisor's start grace so a slow but live start is not cut short.
const DefaultStartBudget = 60 * time.Second

// DefaultPauseBudget bounds a pause the client no longer holds, so a wedged checkpoint cannot pin the sandbox lock.
const DefaultPauseBudget = 10 * time.Minute

// MaxMemoryMiB is 16 TiB, which is past any host and far below the point where MiB times 2^20 wraps.
const MaxMemoryMiB = 1 << 24

// MaxDiskMiB is what the ext4 writer's 32-bit block count holds, 128 MiB short of 16 TiB.
const MaxDiskMiB = ext4.MaxDiskSize >> 20

// Repository is the part of sandboxstate.Repository the lifecycle verbs drive.
type Repository interface {
	Reader
	Create(sb models.Sandbox, admit ...func(dir string) error) (models.Sandbox, error)
	Update(id string, mutate func(*models.Sandbox) error) error
	Delete(id string) error
	Dir(id string) (string, error)
	CheckpointDir(id string) (string, error)
}

// Images is the part of image.Service a create drives.
type Images interface {
	Pull(ctx context.Context, ref string) (image.Image, error)
	Lookup(ref string) (image.Image, bool, error)
}

// Snapshots is the part of sandboxstate.Snapshots the snapshot verbs drive.
type Snapshots interface {
	Create(snap models.Snapshot, fill func(files string) error) (models.Snapshot, error)
	Resolve(ref string) (string, error)
	Get(id string) (models.Snapshot, error)
	List() ([]models.Snapshot, error)
	Delete(id string) error
	Files(id string) (string, error)
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

// Environments answers where a provider keeps the guest environment of a sandbox, which a grant and an attach rewrite between a stop and the next start.
type Environments interface {
	Environment(id string) (models.Environment, error)
}

// Config is every layer the orchestrator drives. The daemon builds each one once.
type Config struct {
	Repo         Repository
	Snapshots    Snapshots
	Images       Images
	Network      Network
	Provider     models.Provider
	Secrets      Secrets
	Policies     Policies
	Substrate    Substrate
	Environments Environments
	// ProxyCA hands a fronted sandbox the certificate it must trust, so the proxy can terminate its TLS.
	ProxyCA func() ([]byte, error)
	// PullTimeout bounds one pull; zero is no bound.
	PullTimeout time.Duration
	// HostMemoryMiB is the most memory a create may ask for, the host's total; zero is no bound, which only a test sets.
	HostMemoryMiB int64
	// HostCPUs is the most CPUs a create may ask for, the ones the daemon may run on; zero is no bound, which only a test sets.
	HostCPUs int
	// StopSettle overrides DefaultStopSettle, which only a test has a reason to do.
	StopSettle time.Duration
	// ProbeBudget overrides DefaultProbeBudget, which only a test has a reason to do.
	ProbeBudget time.Duration
	// StartBudget overrides DefaultStartBudget, which only a test has a reason to do.
	StartBudget time.Duration
	// PauseBudget overrides DefaultPauseBudget, which only a test has a reason to do.
	PauseBudget time.Duration
	// ExecStartBudget overrides DefaultExecStartBudget, which only a test has a reason to do.
	ExecStartBudget time.Duration
	// ExecCleanupGrace overrides DefaultExecCleanupGrace, which only a test has a reason to do.
	ExecCleanupGrace time.Duration
	// Report takes a transition a verb records on its own, as the background loops report theirs; only a test leaves it nil.
	Report func(string)
	// PutCleanupGrace overrides DefaultPutCleanupGrace, which only a test has a reason to do.
	PutCleanupGrace time.Duration
	// Redact hides every secret value in a failed reason before the record keeps it; nil keeps the text, which only a test does.
	Redact func(string) string
}

// Service owns create, start, stop and rm, and serializes them per sandbox in memory: one process holds it.
type Service struct {
	cfg Config

	mu    sync.Mutex
	locks map[string]*sandboxLock
	// pulls ends the pull of each create still in Complete, so rm and stop need not wait for the download.
	pulls map[string]context.CancelCauseFunc

	// execs holds every exec from its create to its end, so an attach and a resize find it by id.
	execMu sync.Mutex
	execs  map[string]*execSession
	// running counts each sandbox's admitted execs until their commands end, and runningAll their sum; both under execMu.
	running    map[string]int
	runningAll int
}

func New(cfg Config) *Service {
	return &Service{cfg: cfg, locks: map[string]*sandboxLock{}, pulls: map[string]context.CancelCauseFunc{}, execs: map[string]*execSession{}, running: map[string]int{}}
}

// CreateRequest is what a create names. It is the JSON body of POST /v0/sandboxes.
type CreateRequest struct {
	Image    string   `json:"image,omitempty" doc:"The image to create from. A create names exactly one of image and snapshot."`
	Snapshot string   `json:"snapshot,omitempty" doc:"The snapshot id or name to create from; it takes no command and no restart. A create names exactly one of image and snapshot."`
	Name     string   `json:"name,omitempty"`
	Command  []string `json:"command,omitempty"`
	Env      []string `json:"env,omitempty"`
	WorkDir  string   `json:"workdir,omitempty"`
	User     string   `json:"user,omitempty"`
	// Secrets is what the guest gets a placeholder for, each under its own name.
	Secrets []string `json:"secrets,omitempty"`
	// Policy is what the host enforces for the sandbox.
	Policy    string          `json:"policy,omitempty"`
	Resources ResourceRequest `json:"resources" required:"false"`
	// Restart is when the supervisor starts the entrypoint again inside the sandbox, nil for never.
	Restart *models.RestartSpec `json:"restart,omitempty"`
}

// ResourceRequest is the bounds a create asks for. A nil memory is an omitted --memory, which a snapshot fills, and 0 is no bound.
type ResourceRequest struct {
	MemoryMiB *int64 `json:"memory_mib,omitempty" minimum:"0" maximum:"16777216"`
	VCPUs     int    `json:"vcpus" required:"false" minimum:"0"`
	DiskMiB   int64  `json:"disk_mib" required:"false" minimum:"0" maximum:"16777088"`
}

// bounds is what the record keeps, where an omitted memory is no bound.
func (r ResourceRequest) bounds() models.Resources {
	res := models.Resources{VCPUs: r.VCPUs, DiskMiB: r.DiskMiB}
	if r.MemoryMiB != nil {
		res.MemoryMiB = *r.MemoryMiB
	}

	return res
}

// fronted says the sandbox's web traffic goes through the proxy, which a policy and a grant both need.
func (r CreateRequest) fronted() bool { return r.Policy != "" || len(r.Secrets) != 0 }

// RequestError is a request refused before anything was claimed, and never the state of a sandbox.
type RequestError struct {
	Err error
}

func (e *RequestError) Error() string { return e.Err.Error() }

func (e *RequestError) Unwrap() error { return e.Err }

// Public answers the whole text: every site wraps only what the caller sent, or a typed refusal of it.
func (e *RequestError) Public() string { return e.Err.Error() }

// StateError is a verb refused for the state the sandbox is in. Fix says what the operator does instead.
type StateError struct {
	ID    string
	State models.State
	Fix   string
	// Code names the state the verb wanted, for the program that reads the API body.
	Code models.Code
	// Detail is host context, such as the pid that missed its probe, which only the local text carries.
	Detail string
}

func (e *StateError) Error() string {
	if e.Detail == "" {
		return e.Public()
	}

	return fmt.Sprintf("sandbox %s is %s: %s: %s", e.ID, e.State, e.Detail, e.Fix)
}

func (e *StateError) Public() string {
	return fmt.Sprintf("sandbox %s is %s: %s", e.ID, e.State, e.Fix)
}

// ImageGoneError is a verb that found the sandbox's image files gone from the host; a pull of Image brings them back.
type ImageGoneError struct {
	ID string
	// Image is the record's reference pinned to its digest, so the pull restores the files the sandbox stacks over.
	Image string
	Verb  string
	Err   error
}

func (e *ImageGoneError) Error() string { return e.Public() + ": " + e.Err.Error() }

func (e *ImageGoneError) Unwrap() error { return e.Err }

// Public names the image and the verb to run again, never the host path the substrate found empty.
func (e *ImageGoneError) Public() string {
	return fmt.Sprintf("sandbox %s: its image %s is no longer on this host; pull that image, then %s the sandbox again", e.ID, e.Image, e.Verb)
}

// imageGone types a substrate's gone image so a public route names the pull; any other error passes through.
func imageGone(id, ref, digest, verb string, err error) error {
	if !errors.Is(err, models.ErrImageGone) {
		return err
	}
	gone := &ImageGoneError{ID: id, Image: ref, Verb: verb, Err: err}
	if digest == "" {
		return gone
	}
	pinned, pinErr := image.Pinned(ref, digest)
	if pinErr != nil {
		gone.Err = errors.Join(err, pinErr)

		return gone
	}
	gone.Image = pinned

	return gone
}

// wrongState refuses a verb on the record's state, and names why an unresponsive one is silent, as docs/state-machine.md promises.
func wrongState(id string, sb models.Sandbox, fix string, code models.Code) *StateError {
	refused := &StateError{ID: id, State: sb.State, Fix: fix, Code: code}
	if sb.State == models.StateUnresponsive {
		refused.Detail = sb.UnresponsiveReason
	}

	return refused
}

// FailedGuard refuses every verb but get and rm on a failed sandbox, with the one code that names it.
// A create that never reached running is terminal, so an operator reads the reason and then removes it.
func FailedGuard(id string, sb models.Sandbox) error {
	if sb.State != models.StateFailed {
		return nil
	}

	public := PublicReason(sb)
	refused := &StateError{ID: id, State: sb.State, Fix: fmt.Sprintf("%s; remove it with shard remove %s", public, id), Code: models.CodeSandboxFailed}
	if sb.FailedReason != public {
		refused.Detail = sb.FailedReason
	}

	return refused
}

// SubstrateTimeoutError is our own deadline on a Provider.Status the substrate never answered, so a verb
// fails fast within budget instead of pinning on a wedged runtime. Op names the verb the operator ran.
type SubstrateTimeoutError struct {
	ID     string
	Op     string
	Budget time.Duration
}

func (e *SubstrateTimeoutError) Error() string {
	return fmt.Sprintf("the provider did not answer within %s for sandbox %s", e.Budget, e.ID)
}

func (e *SubstrateTimeoutError) Public() string {
	return e.Error() + "; retry the request when the provider answers"
}

// sandboxLock is the lock of one sandbox. It counts its holder and its waiters, so the last of them frees it.
type sandboxLock struct {
	slot chan struct{}
	refs int
}

// lock serializes the verbs on one sandbox, and gives up when ctx ends, so a verb keeps its own deadline.
func (s *Service) lock(ctx context.Context, id string) (func(), error) {
	l := s.ref(id)

	// A free lock is taken even on a dead ctx: a select with both ready picks at random.
	select {
	case l.slot <- struct{}{}:
		return func() { s.release(id, l) }, nil
	default:
	}

	select {
	case l.slot <- struct{}{}:
		return func() { s.release(id, l) }, nil
	case <-ctx.Done():
		s.unref(id)

		return nil, fmt.Errorf("sandbox %s is busy with another verb: %w", id, context.Cause(ctx))
	}
}

// tryLock is the lock for a background loop, which skips a sandbox a verb holds rather than stall every other sandbox behind it (SHARD-339).
func (s *Service) tryLock(id string) (func(), bool) {
	l := s.ref(id)

	select {
	case l.slot <- struct{}{}:
		return func() { s.release(id, l) }, true
	default:
		s.unref(id)

		return nil, false
	}
}

func (s *Service) ref(id string) *sandboxLock {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.locks[id]
	if !ok {
		l = &sandboxLock{slot: make(chan struct{}, 1)}
		s.locks[id] = l
	}
	l.refs++

	return l
}

// release empties the slot before the count drops, so a lock freed at zero is never one a holder still fills.
func (s *Service) release(id string, l *sandboxLock) {
	<-l.slot
	s.unref(id)
}

func (s *Service) unref(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.locks[id]
	l.refs--
	if l.refs == 0 {
		delete(s.locks, id)
	}
}

// errCreateCancelled is the reason a create fails when rm or stop ends its pull.
var errCreateCancelled = errors.New("the create was cancelled")

// cancellable gives the pull of a create a context that cancelPull ends; done forgets it.
func (s *Service) cancellable(ctx context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)

	s.mu.Lock()
	s.pulls[id] = cancel
	s.mu.Unlock()

	return ctx, func() {
		s.mu.Lock()
		delete(s.pulls, id)
		s.mu.Unlock()
		cancel(nil)
	}
}

// cancelPull ends the pull of a create, which then fails with the verb that ended it as the reason.
func (s *Service) cancelPull(id, verb string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cancel, ok := s.pulls[id]; ok {
		cancel(cancelled(verb))
	}
}

func cancelled(verb string) error { return &cancelledError{verb: verb} }

// cancelledError is a create that rm or stop ended, named by the verb.
type cancelledError struct {
	verb string
}

func (e *cancelledError) Error() string { return fmt.Sprintf("%s by %s", errCreateCancelled, e.verb) }

func (e *cancelledError) Unwrap() error { return errCreateCancelled }

func (e *cancelledError) Public() string { return e.Error() }

func (s *Service) redact(text string) string {
	if s.cfg.Redact == nil {
		return text
	}

	return s.cfg.Redact(text)
}

// report logs a transition a verb recorded, where the daemon logs the ones its loops record.
func (s *Service) report(line string) {
	if s.cfg.Report == nil {
		return
	}
	s.cfg.Report(line)
}

// probeBudget is how long one daemon- or verb-initiated Provider.Status gets before we treat it as wedged.
func (s *Service) probeBudget() time.Duration {
	if s.cfg.ProbeBudget != 0 {
		return s.cfg.ProbeBudget
	}

	return DefaultProbeBudget
}

// startBudget is how long one start's substrate work gets before we treat the runtime as wedged.
func (s *Service) startBudget() time.Duration {
	if s.cfg.StartBudget != 0 {
		return s.cfg.StartBudget
	}

	return DefaultStartBudget
}

// pauseBudget is how long one pause's checkpoint and delete get, whether or not the client still waits.
func (s *Service) pauseBudget() time.Duration {
	if s.cfg.PauseBudget != 0 {
		return s.cfg.PauseBudget
	}

	return DefaultPauseBudget
}

// status asks the substrate about a sandbox on a bounded context, so a wedged runtime cannot pin a verb.
// A deadline we set, not the caller's own cancel, becomes the SubstrateTimeoutError a verb fails fast on.
func (s *Service) status(ctx context.Context, id, op string) (models.Status, error) {
	budget := s.probeBudget()
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	status, err := s.cfg.Provider.Status(bctx, id)
	if err == nil {
		return status, nil
	}
	if bctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return models.Status{}, &SubstrateTimeoutError{ID: id, Op: op, Budget: budget}
	}

	return models.Status{}, err
}

// Prepare writes the pending record and answers at once. The pull and the start run later, in Complete,
// so a create returns before the download and the sandbox reaches running or failed in the background.
func (s *Service) Prepare(ctx context.Context, req CreateRequest) (models.Sandbox, error) {
	if err := validate(req); err != nil {
		return models.Sandbox{}, err
	}
	snapshot := ""
	if req.Snapshot != "" {
		seed, err := s.seed(ctx, req.Snapshot, req)
		if err != nil {
			return models.Sandbox{}, err
		}
		defer seed.unlock()
		req, snapshot = seed.req, seed.id
	}
	res := req.Resources.bounds()
	// A bound the substrate refuses is the request's fault, and it must not leave a failed record behind.
	if err := s.cfg.Provider.CheckResources(res); err != nil {
		return models.Sandbox{}, &RequestError{Err: err}
	}
	// A bound past the host's memory never binds: the host runs out of memory first.
	if s.cfg.HostMemoryMiB > 0 && res.MemoryMiB > s.cfg.HostMemoryMiB {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("resources.memory_mib is %d MiB, more than the %d MiB of memory on this host; set it to %d MiB or less", res.MemoryMiB, s.cfg.HostMemoryMiB, s.cfg.HostMemoryMiB)}
	}
	// A quota past the host's CPUs never binds, and a large enough one overflows the quota to no bound at all.
	if s.cfg.HostCPUs > 0 && res.VCPUs > s.cfg.HostCPUs {
		return models.Sandbox{}, &RequestError{Err: fmt.Errorf("resources.vcpus is %d, more than the %d CPUs on this host; set it to %d or less", res.VCPUs, s.cfg.HostCPUs, s.cfg.HostCPUs)}
	}
	// Record the disk bound the sandbox will actually run under, so inspect shows the enforced value, not a bare 0.
	res.DiskMiB = bundle.DiskBound(res)

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
			return models.Sandbox{}, policyRefused(req.Policy, err)
		}
	}
	if req.fronted() {
		if _, err := s.proxyCA(); err != nil {
			return models.Sandbox{}, err
		}
	}

	var admit []func(dir string) error
	reserved := ""
	disks, admits := s.cfg.Provider.(diskAdmitter)
	if admits {
		admit = append(admit, func(dir string) error {
			// A disk the root has no room for is the request's fault, refused before the record a later failure would leave.
			if err := disks.AdmitDisk(dir, res); err != nil {
				return diskRefused(err)
			}
			reserved = dir

			return nil
		})
	}

	sb, err := s.cfg.Repo.Create(models.Sandbox{
		Name:      req.Name,
		Image:     ref,
		Snapshot:  snapshot,
		Provider:  s.cfg.Provider.Name(),
		State:     models.StatePending,
		Resources: res,
		Secrets:   req.Secrets,
		Policy:    req.Policy,
		Command:   slices.Clone(req.Command),
		Restart:   withRestartDefaults(req.Restart),
		CreatedAt: time.Now().UTC(),
	}, admit...)
	if err != nil {
		// The record or the name failed after the admission, so nothing will ever write that disk.
		if reserved != "" {
			disks.ReleaseDisk(reserved)
		}

		return models.Sandbox{}, err
	}

	return sb, nil
}

// policyRefused makes the request's fault only a policy name that is malformed or names none; a store it could not read broke the verb.
func policyRefused(name string, err error) error {
	if errors.Is(err, egress.ErrNotFound) || egress.ValidName(name) != nil {
		return &RequestError{Err: err}
	}

	return err
}

// diskRefused makes the request's fault only a disk that does not fit; a root it could not read broke the create.
func diskRefused(err error) error {
	if _, ok := errors.AsType[*bundle.NoRoomError](err); ok {
		return &RequestError{Err: err}
	}

	return err
}

// userRefused makes the request's fault a user the guest's tree cannot resolve; the substrate's wrapping says nothing the caller can fix.
func userRefused(err error) error {
	if refused, ok := userRefusal(err); ok {
		return refused
	}

	return err
}

// userRefusal is a user or group the tree does not list, or a passwd or group the guest made other than a regular file.
func userRefusal(err error) (*RequestError, bool) {
	if unknown, ok := errors.AsType[*bundle.UnknownUserError](err); ok {
		return &RequestError{Err: unknown}, true
	}
	if database, ok := errors.AsType[*bundle.UserDatabaseError](err); ok {
		return &RequestError{Err: database}, true
	}

	return nil, false
}

// diskAdmitter reserves the disk of a new sandbox before its record exists; only the VM substrates hold a disk file.
type diskAdmitter interface {
	AdmitDisk(dir string, res models.Resources) error
	ReleaseDisk(dir string)
}

// kernelBooter is a VM substrate, which boots a guest kernel of shard's.
type kernelBooter interface {
	GuestKernel() string
}

// guestKernel is the tag of the kernel a fresh boot on provider runs, empty on a container substrate.
func guestKernel(provider models.Provider) string {
	booter, ok := provider.(kernelBooter)
	if !ok {
		return ""
	}

	return booter.GuestKernel()
}

// Complete pulls the image, builds the sandbox and starts it, then moves the record from pending to
// running. A failure that is not a shutdown leaves the record failed with the reason, so a get reads why
// and rm still frees it. It pushes every claim before the commit point onto the teardown stack.
func (s *Service) Complete(ctx context.Context, id string, req CreateRequest) (err error) {
	// Registered before the lock, so an rm that lands while this waits still ends the pull.
	pullCtx, forget := s.cancellable(ctx, id)
	defer forget()

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	// An rm that took the lock first has freed the record, and a stop has failed it, so nothing is left to create.
	sb, err := s.cfg.Repo.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sb.State != models.StatePending {
		return nil
	}

	// Past the commit point the sandbox is live, so a later error names it and never marks it failed.
	committed := false

	// A shutdown cancels the work rather than fails the create: the next daemon reconciles the pending
	// record against the substrate, so an interrupted start that took is found running, not lost.
	defer func() {
		if err != nil && ctx.Err() == nil && !committed {
			err = s.fail(ctx, id, err)
		}
	}()

	// The record pins the snapshot by id, and the lock holds it until the copy out of it is done.
	var seed seeded
	if sb.Snapshot != "" {
		if seed, err = s.seed(ctx, sb.Snapshot, req); err != nil {
			return err
		}
		defer seed.unlock()
		req = seed.req
	}

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

	img := seed.img
	if sb.Snapshot == "" {
		// Only the pull can be cancelled: the teardown and the fail below run under ctx, which rm never ends.
		if img, err = s.pull(pullCtx, req); err != nil {
			return err
		}
	}

	dir, err := s.cfg.Repo.Dir(id)
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
		RootDisk:   img.Disk,
		BaseDisk:   img.Erofs,
		StateDir:   dir,
		Entrypoint: req.Command,
		Env:        env,
		WorkDir:    req.WorkDir,
		User:       req.User,
		Network:    resolvedThrough(netSpec, req.Policy),
		Resources:  req.Resources.bounds(),
		Restart:    restartSpecOf(withRestartDefaults(req.Restart)),
		Seed:       seed.files,
		ProxyCA:    proxyCA,
	}, img.Config)

	// Create rolls back its own mount only, and an interrupt can leave the sandbox process runsc
	// already forked, so the push goes above the call. Remove tolerates an id runsc never held.
	td.Push(func(ctx context.Context) error { return s.cfg.Provider.Remove(ctx, id) })

	if err := s.cfg.Provider.Create(ctx, spec); err != nil {
		return imageGone(id, sb.Image, img.Digest, "create", userRefused(err))
	}

	if err := s.recordCreated(ctx, spec, img.Digest); err != nil {
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

		return imageGone(id, sb.Image, img.Digest, "create", nameCommand(err, spec.Entrypoint))
	}

	// The commit point. The entrypoint is live, so nothing below this line gives anything back: only
	// stop ends a sandbox.
	td.Discard()
	committed = true

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateRunning
		sb.StartedAt = time.Now().UTC()

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

	if err := s.Settle(ctx, sb.ID, req); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(sb.ID)
}

// discardBudget bounds the removal of a sandbox whose app never started, on a context its caller cannot cancel.
const discardBudget = 30 * time.Second

// Settle is Complete for a caller that waits on the outcome, so an app that never started leaves no sandbox behind its refusal.
func (s *Service) Settle(ctx context.Context, id string, req CreateRequest) error {
	err := s.Complete(ctx, id, req)
	var refused *models.CommandNotStartedError
	if !errors.As(err, &refused) {
		return err
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardBudget)
	defer cancel()
	removeErr := s.Remove(cleanupCtx, id, true)
	if removeErr == nil || errors.Is(removeErr, sandboxstate.ErrNotFound) {
		return err
	}

	return &NotRemovedError{Refusal: refused, Err: removeErr}
}

// NotRemovedError is a refused app whose sandbox stayed; it unwraps to nothing, so no cause inside picks the code of what is a plain failure.
type NotRemovedError struct {
	Refusal *models.CommandNotStartedError
	Err     error
}

func (e *NotRemovedError) Error() string {
	return fmt.Sprintf("%s, and the sandbox was not removed: %s", e.Refusal, e.Err)
}

// Public names the refusal and the sandbox it left, never the removal's cause, which only the daemon log reads.
func (e *NotRemovedError) Public() string {
	return e.Refusal.Public() + ", and the sandbox was not removed"
}

// nameCommand gives a refused start the program it was to run, which the provider does not know.
func nameCommand(err error, argv []string) error {
	var refused *models.CommandNotStartedError
	if !errors.As(err, &refused) || len(argv) == 0 {
		return err
	}

	named := *refused
	named.Command = argv[0]

	return &named
}

// WaitState answers at once: this service's Create is synchronous, so a sandbox it holds never sits in pending.
// The daemon composes Prepare and Complete in the background and overrides this with a wait that blocks.
func (s *Service) WaitState(_ context.Context, _ string) error { return nil }

// CreateAndWait is Create: the record this service's Create answers has already left pending.
func (s *Service) CreateAndWait(ctx context.Context, req CreateRequest) (models.Sandbox, error) {
	return s.Create(ctx, req)
}

// fail records why a create never reached running. It keeps the record so a get reads the reason and rm
// frees it, and it returns the cause so the synchronous caller still sees the failure.
func (s *Service) fail(ctx context.Context, id string, cause error) error {
	if err := s.cfg.Repo.Update(id, s.failed(cause)); err != nil {
		return errors.Join(cause, fmt.Errorf("sandbox %s failed but its record was not updated: %w", id, err))
	}

	return cause
}

// failed is the record of a create that ended in cause, with the raw text and the part a public route may answer.
func (s *Service) failed(cause error) func(*models.Sandbox) error {
	public, ok := PublicText(cause)
	if !ok {
		public = FailedGeneric
	}

	return func(sb *models.Sandbox) error {
		sb.State = models.StateFailed
		sb.FailedReason = s.redact(cause.Error())
		sb.FailedPublic = s.redact(public)
		sb.PID = 0
		sb.Pausing = false

		return nil
	}
}

// ValidName, ValidSecretName and ValidPolicyName let a client refuse a spelling before it asks the daemon.
func ValidName(name string) error { return sandboxstate.ValidName(name) }

func ValidSnapshotName(name string) error { return sandboxstate.ValidSnapshotName(name) }

func ValidSecretName(name string) error { return secret.ValidName(name) }

func ValidPolicyName(name string) error { return egress.ValidName(name) }

// validate refuses what no store could hold or no verb could take back, before anything is pulled.
func validate(req CreateRequest) error {
	if req.Image == "" && req.Snapshot == "" {
		return &RequestError{Err: errors.New("the request names no image and no snapshot")}
	}
	if req.Image != "" && req.Snapshot != "" {
		return &RequestError{Err: errors.New("the request names both an image and a snapshot: a snapshot already names its image")}
	}
	if req.Snapshot != "" && (len(req.Command) != 0 || req.Restart != nil) {
		return &RequestError{Err: errors.New("a sandbox from a snapshot cannot take command or restart; omit both fields")}
	}

	if req.Name != "" {
		if err := sandboxstate.ValidName(req.Name); err != nil {
			return err
		}
	}

	// A bound below zero is not a spelling of unbounded, and the substrate would drop it without a word.
	memory := req.Resources.bounds().MemoryMiB
	if memory < 0 {
		return &RequestError{Err: fmt.Errorf("the memory bound is in MiB and cannot be negative, got %d", memory)}
	}
	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if memory > MaxMemoryMiB {
		return &RequestError{Err: fmt.Errorf("the memory bound is in MiB and no host holds that much, got %d", memory)}
	}
	if req.Resources.VCPUs < 0 {
		return &RequestError{Err: fmt.Errorf("the vcpu bound cannot be negative, got %d", req.Resources.VCPUs)}
	}
	if req.Resources.DiskMiB < 0 {
		return &RequestError{Err: fmt.Errorf("the disk bound is in MiB and cannot be negative, got %d", req.Resources.DiskMiB)}
	}
	if req.Resources.DiskMiB > MaxDiskMiB {
		return &RequestError{Err: fmt.Errorf("the disk bound is in MiB and no host holds that much, got %d", req.Resources.DiskMiB)}
	}
	if req.Restart != nil {
		if err := validRestart(*req.Restart, req.Command); err != nil {
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
		// The trust merge writes over a variable of this name, so the placeholder would never reach the guest.
		if slices.Contains(bundle.TrustEnv, name) {
			return &RequestError{Err: fmt.Errorf("the secret %s cannot be granted to a sandbox: the proxy sets that variable to the trust store", name)}
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
			return nil, &RequestError{Err: fmt.Errorf("secret %s does not exist: run shard secret set --destination <host> %s first", name, name)}
		}
		if err != nil {
			return nil, err
		}

		env = append(env, name+"="+sec.Placeholder)
	}

	return env, nil
}

// The pending record prevents prune from removing the rootfs during the pull.
func (s *Service) pull(ctx context.Context, req CreateRequest) (image.Image, error) {
	// A registry that accepts the connection and then stalls would otherwise pin the create forever.
	if s.cfg.PullTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.PullTimeout)
		defer cancel()
	}

	img, err := s.cfg.Images.Pull(ctx, req.Image)
	// A cached image answers even on an ended context, so a cancel that landed before the pull still fails the create.
	if cause := context.Cause(ctx); errors.Is(cause, errCreateCancelled) {
		return image.Image{}, cause
	}
	if err != nil {
		return image.Image{}, err
	}

	return img, nil
}

// recordCreated copies what the substrate decided into the record, so a later process can reach the
// sandbox without asking the provider again. The state stays created until the start.
func (s *Service) recordCreated(ctx context.Context, spec models.SandboxSpec, digest string) error {
	status, err := s.cfg.Provider.Status(ctx, spec.ID)
	if err != nil {
		return err
	}

	return s.cfg.Repo.Update(spec.ID, func(sb *models.Sandbox) error {
		sb.PID = status.PID
		sb.NetnsPath = spec.Network.NetnsPath
		sb.Address = spec.Network.Address
		sb.HostInterface = spec.Network.HostInterface
		sb.Digest = digest
		sb.Kernel = guestKernel(s.cfg.Provider)

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
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if sb.State != models.StateStopped {
		return models.Sandbox{}, wrongState(id, sb, "start takes a stopped sandbox", models.CodeSandboxNotStopped)
	}

	if err := s.start(ctx, id); err != nil {
		return models.Sandbox{}, imageGone(id, sb.Image, sb.Digest, "start", err)
	}

	return s.record(id)
}

// start bounds the whole run so a wedged runtime fails fast and typed, never pinning the sandbox lock.
func (s *Service) start(ctx context.Context, id string) error {
	budget := s.startBudget()
	bctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	err := s.startWithin(bctx, id)
	if bctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return &SubstrateTimeoutError{ID: id, Op: "start", Budget: budget}
	}

	return err
}

func (s *Service) startWithin(ctx context.Context, id string) error {
	// The lease survived the stop, so this hands back the same address over a namespace built again.
	if _, err := s.cfg.Network.Allocate(ctx, id); err != nil {
		return err
	}

	if err := s.cfg.Provider.Start(ctx, id); err != nil {
		if reconcileErr := Reconcile(ctx, s.cfg.Repo, s.cfg.Provider, id, false); reconcileErr != nil {
			return errors.Join(err, reconcileErr)
		}

		return errors.Join(err, s.recordFailedStart(ctx, id))
	}

	return RecordRunning(ctx, s.cfg.Repo, s.cfg.Provider, id, false)
}

// recordFailedStart lands a shard-init that died before the start ran anything, so inspect shows its 125 and its reason (SHARD-416).
func (s *Service) recordFailedStart(ctx context.Context, id string) error {
	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Alive() || status.SupervisorFailed == "" {
		return nil
	}
	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		supervisorFailed(rec, status.SupervisorFailed)

		return nil
	})
	if err != nil {
		return fmt.Errorf("sandbox %s: %s, but its record was not updated: %w", id, SupervisorFailedReason, err)
	}

	return nil
}

// Stop ends the processes and keeps everything rm frees: the record, the lease, the address and the
// writable layer all outlive it, so a start can follow.
func (s *Service) Stop(ctx context.Context, ref string) (models.Sandbox, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Sandbox{}, err
	}

	// A create still pulling holds the lock; ended, it leaves a failed record that only rm takes.
	s.cancelPull(id, "shard stop")

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Sandbox{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Sandbox{}, err
	}

	// A create that has not taken the lock yet builds nothing once its record is failed, not stopped.
	if sb.State == models.StatePending {
		if err := s.cfg.Repo.Update(id, s.failed(cancelled("shard stop"))); err != nil {
			return models.Sandbox{}, err
		}
		if sb, err = s.cfg.Repo.Get(id); err != nil {
			return models.Sandbox{}, err
		}
	}

	if err := FailedGuard(id, sb); err != nil {
		return models.Sandbox{}, err
	}

	if err := s.stop(ctx, id, false); err != nil {
		return models.Sandbox{}, err
	}

	return s.record(id)
}

func (s *Service) stop(ctx context.Context, id string, force bool) error {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	// force arrives from rm, after its probe answered live, its kill freed the runtime or the record said paused, so it skips the probe.
	if !force {
		// The opening probe is bounded like rm's, so a plain stop of a wedged sandbox fails fast and typed, not at the client timeout.
		status, err := s.status(ctx, id, "stop")
		var timeout *SubstrateTimeoutError
		wedged := errors.As(err, &timeout)
		switch {
		case wedged && sb.State != models.StateStopped:
			return err
		case wedged:
			// A stopped record cannot confirm it is gone under a wedge, so it falls through to the kill.
		case err != nil:
			return err
		case sb.State == models.StateStopped && !status.Alive():
			// A second stop changes nothing but the checkpoint, which an earlier stop's failed drop leaves behind (SHARD-592).
			return s.dropCheckpoint(id)
		}
	}

	if err := s.cfg.Provider.Stop(ctx, id, models.StopGrace); err != nil {
		return err
	}

	status, err := s.awaitStopped(ctx, id)
	if err != nil {
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

	err = s.cfg.Repo.Update(id, func(sb *models.Sandbox) error {
		sb.State = models.StateStopped
		sb.PID = 0
		sb.UnresponsiveReason = ""
		// A stop ends the sandbox, so no resume can read its checkpoint again (SHARD-592).
		sb.Checkpoint = ""
		if exit != nil {
			sb.ExitStatus = exit
		}
		// shard-init died on the way down, so its 125 outranks an entrypoint exit the record already took.
		if status.SupervisorFailed != "" {
			supervisorFailed(sb, status.SupervisorFailed)
		}
		if sb.Restart != nil {
			sb.Restart.RestartCount = restarts
		}

		return nil
	})
	if err != nil {
		return err
	}

	// The record no longer names the checkpoint, so its memory and disk copy would leak until rm takes the sandbox (SHARD-592).
	return s.dropCheckpoint(id)
}

// awaitStopped makes stop mean stopped. runsc can report a sandbox alive for a moment after a clean
// stop, and a rm that lands in that moment would refuse it. The record is written only after this.
func (s *Service) awaitStopped(ctx context.Context, id string) (models.Status, error) {
	// The bound excludes the grace on purpose: Provider.Stop already spent it, and the client's own
	// timeout is DefaultTimeout plus the grace, which counting it twice would run past.
	bound := s.cfg.StopSettle
	if bound == 0 {
		bound = DefaultStopSettle
	}
	deadline := time.Now().Add(bound)
	// The settle bounds the poll too, so a Status the substrate wedges cannot hold the stop past it.
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for {
		status, err := s.cfg.Provider.Status(sctx, id)
		if err != nil && ctx.Err() != nil {
			return models.Status{}, ctx.Err()
		}
		if err != nil && sctx.Err() != nil {
			return models.Status{}, fmt.Errorf("sandbox %s did not stop within %s: the provider did not answer", id, bound)
		}
		if err != nil {
			return models.Status{}, err
		}
		if !status.Alive() {
			return status, nil
		}
		if !time.Now().Before(deadline) {
			return models.Status{}, fmt.Errorf("sandbox %s did not stop within %s: the provider still reports %s", id, bound, status.State)
		}

		select {
		case <-ctx.Done():
			return models.Status{}, ctx.Err()
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

// Remove frees everything a stopped sandbox holds. A sandbox that is still up or paused is refused
// unless force says to stop it first, with the grace a stop gives.
func (s *Service) Remove(ctx context.Context, ref string, force bool) error {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return err
	}

	// A pending sandbox runs nothing yet, so rm ends its pull rather than wait for it to come up.
	s.cancelPull(id, "shard remove")

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	// The record dies last below, so an id with no record has nothing else left on the host either.
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	if err := s.endIfAlive(ctx, id, sb.State, force); err != nil {
		return err
	}

	// A paused, dead or OOM-killed sandbox reaches here with no stop behind it, and no route finds its execs once the record goes.
	s.dropExecs(id)

	if err := s.free(ctx, id); err != nil {
		return err
	}

	if err := s.dropSubstrateRoot(); err != nil {
		return fmt.Errorf("sandbox %s is removed, but %w", id, err)
	}

	return nil
}

// reclaimer is the raw kill a substrate offers for a sandbox its runtime stopped answering for; only gVisor has one.
type reclaimer interface {
	Reclaim(ctx context.Context, id string) error
}

// endIfAlive refuses a sandbox that is still up or paused, because rm frees the writable layer and the checkpoint a stop keeps; --force stops it first, and on a wedge kills it first.
func (s *Service) endIfAlive(ctx context.Context, id string, state models.State, force bool) error {
	// A pause ends the process on gVisor, Firecracker and vz, so only the record says a resume still needs the checkpoint, and no probe can wedge that answer.
	if state == models.StatePaused {
		return s.refuseOrStop(ctx, id, state, force)
	}

	status, err := s.status(ctx, id, "remove")
	var timeout *SubstrateTimeoutError
	if force && errors.As(err, &timeout) {
		if err := s.reclaim(ctx, id, err); err != nil {
			return err
		}

		return s.stop(ctx, id, force)
	}
	if err != nil {
		return err
	}
	if !status.Alive() {
		return nil
	}

	return s.refuseOrStop(ctx, id, status.State, force)
}

// refuseOrStop ends a live or paused sandbox for rm when force says so, and refuses it otherwise.
func (s *Service) refuseOrStop(ctx context.Context, id string, state models.State, force bool) error {
	if !force {
		return &StateError{ID: id, State: state, Fix: fmt.Sprintf("stop it first with shard stop %s, or pass --force", id), Code: models.CodeSandboxNotStopped}
	}

	return s.stop(ctx, id, force)
}

// reclaim is the kill rm --force falls back to on a wedge; no kill on offer, or one that misses, keeps the typed wedge so the API still answers 504.
func (s *Service) reclaim(ctx context.Context, id string, wedge error) error {
	r, ok := s.cfg.Provider.(reclaimer)
	if !ok {
		return wedge
	}

	if err := r.Reclaim(ctx, id); err != nil {
		return fmt.Errorf("%w, and the kill behind --force failed: %w", wedge, err)
	}

	return nil
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
	// List direct, not ListReadable: an unreadable record may name this substrate, so keep its root until an operator fixes it (SHARD-343).
	left, err := s.cfg.Repo.List()
	var unreadable *sandboxstate.UnreadableError
	if errors.As(err, &unreadable) {
		return nil
	}
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
		return nil, &RequestError{Err: errors.New("this server needs a proxy certificate authority for policies and secrets; ask its administrator to configure one")}
	}

	return s.cfg.ProxyCA()
}
