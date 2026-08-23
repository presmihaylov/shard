package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/image"
)

func TestParseRunTheGoalCommand(t *testing.T) {
	opts, err := parseRun([]string{"python:3.12", "--", "python", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}

	if opts.ref != "python:3.12" {
		t.Errorf("ref = %q, want python:3.12", opts.ref)
	}

	if want := []string{"python", "-c", "print(1)"}; !slices.Equal(opts.argv, want) {
		t.Errorf("argv = %v, want %v", opts.argv, want)
	}
}

func TestParseRunFlags(t *testing.T) {
	args := []string{
		"--env", "A=1", "--env", "B=2",
		"--workdir", "/srv", "--user", "nobody",
		"--memory", "512", "--cpus", "2",
		"--shard-init", "/opt/shard-init",
		"alpine:3.20",
	}

	opts, err := parseRun(args)
	if err != nil {
		t.Fatalf("parseRun: %v", err)
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

	if opts.initPath != "/opt/shard-init" {
		t.Errorf("shard-init = %q", opts.initPath)
	}

	if len(opts.argv) != 0 {
		t.Errorf("argv = %v, want the image's own entrypoint", opts.argv)
	}
}

func TestParseRunDefaultInitPath(t *testing.T) {
	opts, err := parseRun([]string{"alpine:3.20"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}

	if opts.initPath != DefaultInitPath {
		t.Errorf("shard-init = %q, want %q", opts.initPath, DefaultInitPath)
	}
}

func TestParseRunRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":               {},
		"only flags":             {"--user", "nobody"},
		"a flag after the image": {"alpine:3.20", "--user", "nobody"},
		"an empty argv":          {"alpine:3.20", "--"},
		"an unknown flag":        {"--forever", "alpine:3.20"},
	}

	for name, args := range cases {
		if _, err := parseRun(args); err == nil {
			t.Errorf("parseRun(%s) returned no error", name)
		}
	}
}

func TestExitErrorSurvivesAJoin(t *testing.T) {
	var exit ExitError
	if !errors.As(errors.Join(errors.New("noise"), ExitError{Code: 7}), &exit) || exit.Code != 7 {
		t.Fatalf("the exit code did not survive the join: %+v", exit)
	}
}

// recorder is the shared log of what the fakes were asked to do and in which order.
type recorder struct {
	failAt string
	calls  []string
	live   map[string]bool
}

func (r *recorder) record(name string) error {
	r.calls = append(r.calls, name)

	if r.failAt == name {
		return fmt.Errorf("forced failure at %s", name)
	}

	return nil
}

// cleanup also notes whether the teardown got a context the interrupt had not already cancelled.
func (r *recorder) cleanup(ctx context.Context, name string) error {
	r.live[name] = ctx.Err() == nil

	return r.record(name)
}

type fakeImages struct{ r *recorder }

func (f fakeImages) Pull(context.Context, string) (image.Image, error) {
	if err := f.r.record("images.Pull"); err != nil {
		return image.Image{}, err
	}

	return image.Image{Reference: "alpine:3.20", RootFS: "/images/alpine"}, nil
}

type fakeRepo struct {
	r       *recorder
	dir     string
	updates int
}

func (f *fakeRepo) Create(sb models.Sandbox) (models.Sandbox, error) {
	if err := f.r.record("repo.Create"); err != nil {
		return models.Sandbox{}, err
	}

	sb.ID = "sandbox1"

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

	return mutate(&sb)
}

func (f *fakeRepo) Delete(string) error { return f.r.record("repo.Delete") }

type fakeNet struct{ r *recorder }

func (f fakeNet) Allocate(context.Context, string) (models.NetworkSpec, error) {
	if err := f.r.record("net.Allocate"); err != nil {
		return models.NetworkSpec{}, err
	}

	return models.NetworkSpec{HostInterface: "shardv2"}, nil
}

func (f fakeNet) Release(ctx context.Context, _ string) error {
	return f.r.cleanup(ctx, "net.Release")
}

type fakeProvider struct {
	r       *recorder
	logPath string
	output  string
	exit    models.ExitStatus
}

func (f *fakeProvider) Name() string                                      { return "fake" }
func (f *fakeProvider) Capabilities() models.Capabilities                 { return models.Capabilities{} }
func (f *fakeProvider) LogPath(string) (string, error)                    { return f.logPath, nil }
func (f *fakeProvider) Stop(context.Context, string, time.Duration) error { return nil }

func (f *fakeProvider) Create(context.Context, models.SandboxSpec) error {
	return f.r.record("provider.Create")
}

func (f *fakeProvider) Start(context.Context, string) error {
	if err := f.r.record("provider.Start"); err != nil {
		return err
	}

	return os.WriteFile(f.logPath, []byte(f.output), 0o600)
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

func (f *fakeProvider) Wait(context.Context, string) (models.ExitStatus, error) {
	if err := f.r.record("provider.Wait"); err != nil {
		return models.ExitStatus{}, err
	}

	return f.exit, nil
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

// newFakeApp wires run onto fakes, so the whole order and the whole teardown are testable off Linux.
func newFakeApp(t *testing.T, out *bytes.Buffer, r *recorder, exit models.ExitStatus) App {
	t.Helper()

	r.live = map[string]bool{}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "output.log")

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatalf("seed the log: %v", err)
	}

	deps := runDeps{
		images:   fakeImages{r: r},
		repo:     &fakeRepo{r: r, dir: dir},
		net:      fakeNet{r: r},
		provider: &fakeProvider{r: r, logPath: logPath, output: "1\n", exit: exit},
	}

	return App{
		Version:    "test",
		Root:       dir,
		Out:        out,
		Err:        out,
		Timeout:    time.Minute,
		newRunDeps: func(App, runOptions) (runDeps, error) { return deps, nil },
	}
}

func TestRunSucceedsAndTearsNothingDown(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app := newFakeApp(t, &out, r, models.ExitStatus{Code: 0})

	err := app.Run(t.Context(), []string{"run", "alpine:3.20", "--", "echo", "1"})

	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 0 {
		t.Fatalf("Run returned %v, want an exit code of 0", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("1\n")) {
		t.Errorf("the guest output was not streamed: %q", out.String())
	}

	if !bytes.Contains(out.Bytes(), []byte("sandbox sandbox1")) {
		t.Errorf("the sandbox id was not reported: %q", out.String())
	}

	for _, call := range r.calls {
		if call == "repo.Delete" || call == "net.Release" || call == "provider.Remove" {
			t.Errorf("a successful run tore down %s; the sandbox outlives the command", call)
		}
	}
}

func TestRunPropagatesTheEntrypointExitCode(t *testing.T) {
	var out bytes.Buffer

	app := newFakeApp(t, &out, &recorder{}, models.ExitStatus{Code: 3})

	var exit ExitError
	if err := app.Run(t.Context(), []string{"run", "alpine:3.20"}); !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("Run returned %v, want an exit code of 3", err)
	}
}

// TestRunTearsDownWhatItBuilt forces a failure at each claim and asserts exactly what is given back.
func TestRunTearsDownWhatItBuilt(t *testing.T) {
	cases := []struct {
		failAt  string
		cleanup []string
	}{
		{"images.Pull", nil},
		{"repo.Create", nil},
		{"repo.Dir", []string{"repo.Delete"}},
		{"net.Allocate", []string{"repo.Delete"}},
		{"provider.Create", []string{"net.Release", "repo.Delete"}},
		{"provider.Status", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"repo.Update1", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"provider.Start", []string{"provider.Remove", "net.Release", "repo.Delete"}},
		{"repo.Update2", []string{"provider.Remove", "net.Release", "repo.Delete"}},
	}

	for _, c := range cases {
		t.Run(c.failAt, func(t *testing.T) {
			var out bytes.Buffer

			r := &recorder{failAt: c.failAt}
			app := newFakeApp(t, &out, r, models.ExitStatus{})

			if err := app.Run(t.Context(), []string{"run", "alpine:3.20"}); err == nil {
				t.Fatal("a forced failure returned no error")
			}

			if got := r.calls[len(r.calls)-len(c.cleanup):]; !slices.Equal(got, c.cleanup) {
				t.Errorf("tore down %v, want %v", got, c.cleanup)
			}

			// An empty tail matches anything, so the cases that claim nothing need their own assertion.
			if len(c.cleanup) == 0 && slices.Contains(r.calls, "repo.Delete") {
				t.Errorf("tore down %v, want nothing", r.calls)
			}
		})
	}
}

// TestRunTearsDownAfterAnInterrupt is the reason unwind builds its own context: a cancelled run
// context would fail every give-back at once and leave the netns, the lease and the mount behind.
func TestRunTearsDownAfterAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{failAt: "provider.Start"}
	app := newFakeApp(t, &out, r, models.ExitStatus{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := app.Run(ctx, []string{"run", "alpine:3.20"}); err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, step := range []string{"provider.Remove", "net.Release"} {
		if !r.live[step] {
			t.Errorf("%s ran on the cancelled run context, so it could not give anything back", step)
		}
	}
}
