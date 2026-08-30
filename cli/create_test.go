package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/network"
)

func TestParseCreateTheGoalCommand(t *testing.T) {
	opts, err := parseCreate([]string{"python:3.12", "--", "python", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}

	if opts.ref != "python:3.12" {
		t.Errorf("ref = %q, want python:3.12", opts.ref)
	}

	if want := []string{"python", "-c", "print(1)"}; !slices.Equal(opts.argv, want) {
		t.Errorf("argv = %v, want %v", opts.argv, want)
	}
}

func TestParseCreateFlags(t *testing.T) {
	args := []string{
		"--env", "A=1", "--env", "B=2",
		"--workdir", "/srv", "--user", "nobody",
		"--memory", "512", "--cpus", "2",
		"alpine:3.20",
	}

	opts, err := parseCreate(args)
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}

	if want := []string{"A=1", "B=2"}; !slices.Equal(opts.env, want) {
		t.Errorf("env = %v, want %v", opts.env, want)
	}

	if opts.workDir != "/srv" || opts.user != "nobody" {
		t.Errorf("workdir = %q, user = %q", opts.workDir, opts.user)
	}

	if opts.resources.MemoryMiB != 512 || opts.resources.VCPUs != 2 {
		t.Errorf("resources = %+v, want 512 MiB and 2 vcpus", opts.resources)
	}

	if len(opts.argv) != 0 {
		t.Errorf("argv = %v, want the image's own entrypoint", opts.argv)
	}
}

func TestInitPathFromEnv(t *testing.T) {
	cases := map[string]struct {
		env   string
		unset bool
		want  string
	}{
		"set":   {env: "/opt/shard-init", want: "/opt/shard-init"},
		"empty": {want: DefaultInitPath},
		"unset": {unset: true, want: DefaultInitPath},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// The set registers the restore, which an unset in the same test then still gets.
			t.Setenv(InitPathEnv, c.env)
			if c.unset {
				if err := os.Unsetenv(InitPathEnv); err != nil {
					t.Fatalf("unset %s: %v", InitPathEnv, err)
				}
			}

			if got := initPathFromEnv(); got != c.want {
				t.Errorf("initPathFromEnv() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseCreateRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":               {},
		"only flags":             {"--user", "nobody"},
		"a flag after the image": {"alpine:3.20", "--user", "nobody"},
		"an empty argv":          {"alpine:3.20", "--"},
		"an unknown flag":        {"--forever", "alpine:3.20"},
		"the old init flag":      {"--shard-init", "/opt/shard-init", "alpine:3.20"},
		"an env with no value":   {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":    {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":    {"--env", "=1", "alpine:3.20"},
		"a negative memory":      {"--memory", "-512", "alpine:3.20"},
		// A bound this large wraps the byte count it is turned into, and a wrapped bound reads as unbounded.
		"a memory that overflows": {"--memory", "17592186044416", "alpine:3.20"},
		"a negative cpu bound":    {"--cpus", "-2", "alpine:3.20"},
	}

	for name, args := range cases {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%s) returned no error", name)
		}
	}
}

// recorder is the shared log of what the fakes were asked to do and in which order.
type recorder struct {
	fail  []string
	calls []string
	live  map[string]bool
}

func (r *recorder) record(name string) error {
	r.calls = append(r.calls, name)

	if slices.Contains(r.fail, name) {
		return fmt.Errorf("forced failure at %s", name)
	}

	return nil
}

// cleanup also notes whether the teardown got a context the interrupt had not already cancelled.
func (r *recorder) cleanup(ctx context.Context, name string) error {
	r.live[name] = ctx.Err() == nil

	return r.record(name)
}

type fakeImages struct {
	imageService
	r *recorder
}

func (f fakeImages) Hold(context.Context) (func() error, error) {
	if err := f.r.record("images.Hold"); err != nil {
		return nil, err
	}

	return func() error { return f.r.record("images.Release") }, nil
}

func (f fakeImages) Pull(context.Context, string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}

	return image.Image{Reference: "alpine:3.20", RootFS: "/images/alpine"}, nil
}

type fakeRepo struct {
	sandboxRepo

	r       *recorder
	dir     string
	updates int
	// created is the record as Create was handed it, so a test says what the flags put in it.
	created models.Sandbox
	// last is the record as the final Update left it.
	last models.Sandbox
}

func (f *fakeRepo) Create(sb models.Sandbox) (models.Sandbox, error) {
	if err := f.r.record("repo.Create"); err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = "sandbox1"
	f.created = sb

	return sb, nil
}

func (f *fakeRepo) Dir(string) (string, error) {
	if err := f.r.record("repo.Dir"); err != nil {
		return "", err
	}

	return f.dir, nil
}

func (f *fakeRepo) Update(id string, mutate func(*models.Sandbox) error) error {
	f.updates++

	if err := f.r.record(fmt.Sprintf("repo.Update%d", f.updates)); err != nil {
		return err
	}

	sb := models.Sandbox{ID: id}
	if err := mutate(&sb); err != nil {
		return err
	}
	f.last = sb

	return nil
}

func (f *fakeRepo) Delete(string) error { return f.r.record("repo.Delete") }

type fakeNet struct {
	r *recorder
	// allocateErr is what Allocate answers with, so a test can hand create the pool's own refusal.
	allocateErr error
}

func (f fakeNet) Allocate(context.Context, string) (models.NetworkSpec, error) {
	if err := f.r.record("net.Allocate"); err != nil {
		return models.NetworkSpec{}, err
	}
	if f.allocateErr != nil {
		return models.NetworkSpec{}, f.allocateErr
	}

	return models.NetworkSpec{HostInterface: "shardv2"}, nil
}

func (f fakeNet) Release(ctx context.Context, _ string) error {
	return f.r.cleanup(ctx, "net.Release")
}

// Create never reaches Reapply, because Allocate applied the rules over its fresh netns.
func (f fakeNet) Reapply(context.Context, string) error {
	return errors.New("create must not reapply the rules")
}

type fakeProvider struct {
	r *recorder
	// spec is what Create was handed, so a test says what reached the substrate.
	spec models.SandboxSpec
}

func (f *fakeProvider) Name() string                                      { return "fake" }
func (f *fakeProvider) Capabilities() models.Capabilities                 { return models.Capabilities{} }
func (f *fakeProvider) LogPath(string) (string, error)                    { return "", nil }
func (f *fakeProvider) Stop(context.Context, string, time.Duration) error { return nil }

func (f *fakeProvider) Create(_ context.Context, spec models.SandboxSpec) error {
	f.spec = spec

	return f.r.record("provider.Create")
}

func (f *fakeProvider) Start(context.Context, string) error {
	return f.r.record("provider.Start")
}

func (f *fakeProvider) Remove(ctx context.Context, _ string) error {
	return f.r.cleanup(ctx, "provider.Remove")
}

func (f *fakeProvider) Status(context.Context, string) (models.Status, error) {
	if err := f.r.record("provider.Status"); err != nil {
		return models.Status{}, err
	}

	return models.Status{Exists: true, State: models.StateCreated, PID: 42}, nil
}

func (f *fakeProvider) Exec(context.Context, string, models.ExecSpec) (models.ExitStatus, error) {
	return models.ExitStatus{}, nil
}

func (f *fakeProvider) Wait(context.Context, string) (models.ExitStatus, error) {
	return models.ExitStatus{}, nil
}

func (f *fakeProvider) Pause(context.Context, string, string) error {
	return models.Unsupported("fake", models.VerbPause)
}

func (f *fakeProvider) Resume(context.Context, string, string) error {
	return models.Unsupported("fake", models.VerbResume)
}

func (f *fakeProvider) Fork(context.Context, string, models.SandboxSpec) error {
	return models.Unsupported("fake", models.VerbFork)
}

func (f *fakeProvider) Clone(context.Context, string, models.SandboxSpec) error {
	return errors.New("the create tests never clone")
}

// newFakeApp wires create onto fakes, so the whole order and the whole teardown are testable off Linux.
func newFakeApp(t *testing.T, out *bytes.Buffer, r *recorder) (App, *deps) {
	t.Helper()

	r.live = map[string]bool{}
	dir := t.TempDir()

	d := &deps{
		app:         App{Root: dir},
		imageSvc:    fakeImages{r: r},
		repoSvc:     &fakeRepo{r: r, dir: dir},
		netSvc:      fakeNet{r: r},
		providerSvc: &fakeProvider{r: r},
	}

	return App{
		Version: "test",
		Root:    dir,
		Out:     out,
		Err:     out,
		Timeout: time.Minute,
		newDeps: func(App) *deps { return d },
	}, d
}

func TestCreatePrintsTheIDAndTearsNothingDown(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newFakeApp(t, &out, r)

	if err := app.Run(t.Context(), []string{"create", "alpine:3.20", "--", "echo", "1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The id is the command's output, so a shell can take it: id=$(shard create alpine).
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("create printed %q, want the bare sandbox id", out.String())
	}

	for _, call := range r.calls {
		if call == "repo.Delete" || call == "net.Release" || call == "provider.Remove" {
			t.Errorf("a successful create tore down %s; the sandbox outlives the command", call)
		}
	}
}

// TestCreateNeverWaitsForTheEntrypoint: create reports whether the create succeeded, and it never
// waits for a process that a sandbox is allowed to outlive.
func TestCreateNeverWaitsForTheEntrypoint(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newFakeApp(t, &out, r)

	if err := app.Run(t.Context(), []string{"create", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if slices.Contains(r.calls, "provider.Wait") {
		t.Errorf("create waited for the entrypoint: %v", r.calls)
	}
}

// TestCreateTearsDownWhatItBuilt forces a failure at each claim and asserts exactly what is given back.
func TestCreateTearsDownWhatItBuilt(t *testing.T) {
	cases := []struct {
		failAt  string
		cleanup []string
	}{
		{"images.Hold", nil},
		{"images.Pull", nil},
		{"repo.Create", nil},
		{"repo.Dir", []string{"repo.Delete"}},
		{"net.Allocate", []string{"net.Release", "repo.Delete"}},
		// Create rolls back its own mount only, so the substrate claim is given back here too.
		{"provider.Create", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"provider.Status", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"repo.Update1", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"provider.Start", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		// The second update comes after a successful start, and a live sandbox is never given back.
		{"repo.Update2", nil},
	}

	for _, c := range cases {
		t.Run(c.failAt, func(t *testing.T) {
			var out bytes.Buffer

			r := &recorder{fail: []string{c.failAt}}
			app, _ := newFakeApp(t, &out, r)

			if err := app.Run(t.Context(), []string{"create", "alpine:3.20"}); err == nil {
				t.Fatal("a forced failure returned no error")
			}

			if got := r.calls[len(r.calls)-len(c.cleanup):]; !slices.Equal(got, c.cleanup) {
				t.Errorf("tore down %v, want %v", got, c.cleanup)
			}

			// An empty tail matches anything, so the cases that give nothing back need their own assertion.
			if len(c.cleanup) > 0 {
				return
			}

			for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
				if slices.Contains(r.calls, step) {
					t.Errorf("tore down %v, want nothing", r.calls)
				}
			}
		})
	}
}

// TestCreateTearsDownAfterAnInterrupt is the reason unwind builds its own context: a cancelled
// command context would fail every give-back at once and leave the netns, the lease and the mount.
func TestCreateTearsDownAfterAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Create"}}
	app, _ := newFakeApp(t, &out, r)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"create", "alpine:3.20"}); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, step := range []string{"provider.Remove", "net.Release"} {
		if !r.live[step] {
			t.Errorf("%s ran on the cancelled command context, so it could not give anything back", step)
		}
	}
}

// TestCreateKeepsALiveSandboxWhenTheRecordWriteFails pins the keep-alive rule at the one place that
// nearly broke it: the record write after a successful start. Only stop ends a sandbox.
func TestCreateKeepsALiveSandboxWhenTheRecordWriteFails(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"repo.Update2"}}
	app, _ := newFakeApp(t, &out, r)

	err := app.Run(t.Context(), []string{"create", "alpine:3.20"})
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if !strings.Contains(out.String(), "sandbox1") {
		t.Errorf("the id of the live sandbox was not printed: %q", out.String())
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s on a started sandbox; only stop ends one", step)
		}
	}
}

// TestCreateStopsUnwindingAtTheFirstFailure: the stack is LIFO, so a step that failed still holds
// what the steps below it name. Deleting the record would leave that state with no handle.
func TestCreateStopsUnwindingAtTheFirstFailure(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Start", "provider.Remove"}}
	app, _ := newFakeApp(t, &out, r)

	if err := app.Run(t.Context(), []string{"create", "alpine:3.20"}); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if last := r.calls[len(r.calls)-1]; last != "provider.Remove" {
		t.Fatalf("the unwind went on to %q after provider.Remove failed", last)
	}

	for _, step := range []string{"net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after a failed give-back; the sandbox is still on the host", step)
		}
	}
}

// TestCreateKeepsASandboxAnInterruptedStartMayHaveStarted: runsc reports the cancellation, not what
// it did, so a force-delete here could end a live guest. Only stop ends a sandbox.
func TestCreateKeepsASandboxAnInterruptedStartMayHaveStarted(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Start"}}
	app, _ := newFakeApp(t, &out, r)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := app.Run(ctx, []string{"create", "alpine:3.20"})
	if err == nil {
		t.Fatal("an interrupted start returned no error")
	}

	if !strings.Contains(err.Error(), "sandbox1") {
		t.Errorf("the error names no kept sandbox: %v", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after an interrupted start; the entrypoint may already be live", step)
		}
	}
}

// TestCreateRecordsWhatTheSubstrateDecided: a later shard process reaches the sandbox through the
// record alone, so the netns, the address and the host interface have to land in it.
func TestCreateRecordsWhatTheSubstrateDecided(t *testing.T) {
	var out bytes.Buffer

	app, deps := newFakeApp(t, &out, &recorder{fail: []string{"repo.Update2"}})

	if err := app.Run(t.Context(), []string{"create", "alpine:3.20"}); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	repo, ok := deps.repoSvc.(*fakeRepo)
	if !ok {
		t.Fatal("the fake repository was replaced")
	}

	if repo.last.PID != 42 {
		t.Errorf("the record holds pid %d, want the one the substrate reported", repo.last.PID)
	}
	if repo.last.HostInterface != "shardv2" {
		t.Errorf("the record holds host interface %q, want shardv2", repo.last.HostInterface)
	}
}

// The pool is the one thing nothing frees on a timer, so its refusal names the verbs that do.
func TestCreateNamesLsWhenNoAddressIsFree(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newFakeApp(t, &out, r)
	deps.netSvc = fakeNet{r: r, allocateErr: network.ErrNoFreeAddress}

	err := app.Run(t.Context(), []string{"create", "alpine:3.20", "--", "echo", "1"})
	if !errors.Is(err, network.ErrNoFreeAddress) || !strings.Contains(err.Error(), "shard ls --all") {
		t.Errorf("create failed with %v, want the pool's refusal naming shard ls --all", err)
	}
}

func TestCreateHandsTheGuestThePlaceholderAndRecordsTheGrant(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, d := newFakeApp(t, &out, r)

	store, err := d.secrets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("API_KEY", "sk-live-1234567890", []string{"api.example.com"}, ""); err != nil {
		t.Fatal(err)
	}

	if err := app.Run(t.Context(), []string{"create", "--secret", "API_KEY", "--env", "OTHER=1", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	spec := d.providerSvc.(*fakeProvider).spec
	if !slices.Contains(spec.Env, "API_KEY=mock-API_KEY") {
		t.Errorf("the guest env %v holds no placeholder", spec.Env)
	}
	if slices.ContainsFunc(spec.Env, func(e string) bool { return strings.Contains(e, "sk-live") }) {
		t.Fatalf("the guest env %v holds the value", spec.Env)
	}

	created := d.repoSvc.(*fakeRepo).created
	if strings.Join(created.Secrets, ",") != "API_KEY" {
		t.Errorf("the record grants %v, want API_KEY", created.Secrets)
	}
}

func TestCreateRefusesASecretTheStoreDoesNotHoldBeforeThePull(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newFakeApp(t, &out, r)

	err := app.Run(t.Context(), []string{"create", "--secret", "NOPE", "alpine:3.20"})
	if err == nil || !strings.Contains(err.Error(), "secret NOPE does not exist") {
		t.Fatalf("create = %v", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a missing secret still cost a pull: %v", r.calls)
	}
}

func TestParseCreateRefusesABadOrDoubledSecret(t *testing.T) {
	for _, args := range [][]string{
		{"--secret", "api_key", "alpine"},
		{"--secret", "KEY", "--secret", "KEY", "alpine"},
		{"--secret", "KEY", "--env", "KEY=1", "alpine"},
	} {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%v) accepted", args)
		}
	}
}
