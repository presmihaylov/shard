package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
		"an env with no value":   {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":    {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":    {"--env", "=1", "alpine:3.20"},
		"a negative memory":      {"--memory", "-512", "alpine:3.20"},
		"a negative cpu bound":   {"--cpus", "-2", "alpine:3.20"},
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
	// last is the record as the final Update left it, which is where the exit status lands.
	last models.Sandbox
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
	if err := mutate(&sb); err != nil {
		return err
	}
	f.last = sb

	return nil
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
	// blocks makes Wait behave the way the product rule allows: an entrypoint may run forever.
	blocks bool
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

func (f *fakeProvider) Wait(ctx context.Context, _ string) (models.ExitStatus, error) {
	if err := f.r.record("provider.Wait"); err != nil {
		return models.ExitStatus{}, err
	}

	if f.blocks {
		<-ctx.Done()

		return models.ExitStatus{}, ctx.Err()
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
func newFakeApp(t *testing.T, out *bytes.Buffer, r *recorder, exit models.ExitStatus) (App, runDeps) {
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
	}, deps
}

func TestRunSucceedsAndTearsNothingDown(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newFakeApp(t, &out, r, models.ExitStatus{Code: 0})

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

	app, _ := newFakeApp(t, &out, &recorder{}, models.ExitStatus{Code: 3})

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
			app, _ := newFakeApp(t, &out, r, models.ExitStatus{})

			if err := app.Run(t.Context(), []string{"run", "alpine:3.20"}); err == nil {
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

// TestRunTearsDownAfterAnInterrupt is the reason unwind builds its own context: a cancelled run
// context would fail every give-back at once and leave the netns, the lease and the mount behind.
func TestRunTearsDownAfterAnInterrupt(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Create"}}
	app, _ := newFakeApp(t, &out, r, models.ExitStatus{})

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

// TestRunKeepsALiveSandboxWhenTheRecordWriteFails pins the keep-alive rule at the one place that
// nearly broke it: the record write after a successful start. Only stop ends a sandbox.
func TestRunKeepsALiveSandboxWhenTheRecordWriteFails(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"repo.Update2"}}
	app, _ := newFakeApp(t, &out, r, models.ExitStatus{})

	err := app.Run(t.Context(), []string{"run", "alpine:3.20"})
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}

	if !bytes.Contains(out.Bytes(), []byte("sandbox sandbox1")) {
		t.Errorf("the id of the live sandbox was not reported: %q", out.String())
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s on a started sandbox; only stop ends one", step)
		}
	}
}

// TestRunStopsUnwindingAtTheFirstFailure: the stack is LIFO, so a step that failed still holds what
// the steps below it name. Deleting the record would leave that state with no handle to reach it by.
func TestRunStopsUnwindingAtTheFirstFailure(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Start", "provider.Remove"}}
	app, _ := newFakeApp(t, &out, r, models.ExitStatus{})

	if err := app.Run(t.Context(), []string{"run", "alpine:3.20"}); err == nil {
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

// TestTailDetachesWhileTheGuestKeepsWriting: a guest that outruns the terminal must not hold the
// follower past an interrupt, or the first Ctrl-C does nothing.
func TestTailDetachesWhileTheGuestKeepsWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan error, 1)
	go func() { done <- tail(ctx, io.Discard, endless{}, make(chan struct{})) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tail: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tail did not detach from a log that never stops growing")
	}
}

// endless is a log the guest never stops adding to.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}

	return len(p), nil
}

// TestRunKeepsASandboxAnInterruptedStartMayHaveStarted: runsc reports the cancellation, not what it
// did, so a force-delete here could end a live guest. Only stop ends a sandbox.
func TestRunKeepsASandboxAnInterruptedStartMayHaveStarted(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{fail: []string{"provider.Start"}}
	app, _ := newFakeApp(t, &out, r, models.ExitStatus{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var exit ExitError
	if err := app.Run(ctx, []string{"run", "alpine:3.20"}); !errors.As(err, &exit) || exit.Code != InterruptedExitCode {
		t.Fatalf("Run returned %v, want the interrupted exit code", err)
	}

	if !bytes.Contains(out.Bytes(), []byte("sandbox sandbox1")) {
		t.Errorf("the id of the kept sandbox was not reported: %q", out.String())
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("ran %s after an interrupted start; the entrypoint may already be live", step)
		}
	}
}

// TestRunRecordsTheExitStatusWhenTheStreamFails: a nil exit status in the record means the
// entrypoint never exited, and here it did. The status is in hand, so it must land.
func TestRunRecordsTheExitStatusWhenTheStreamFails(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newFakeApp(t, &out, r, models.ExitStatus{Code: 0})
	app.Out = fullDisk{}

	if err := app.Run(t.Context(), []string{"run", "alpine:3.20"}); err == nil {
		t.Fatal("a run that could not show the guest's output returned no error")
	}

	repo, ok := deps.repo.(*fakeRepo)
	if !ok {
		t.Fatal("the fake repository was replaced")
	}

	if repo.last.ExitStatus == nil {
		t.Fatal("the record says the entrypoint never exited")
	}

	if repo.last.ExitStatus.Code != 0 {
		t.Errorf("the record holds exit code %d, want 0", repo.last.ExitStatus.Code)
	}
}

// TestRunDetachesWhenTheFollowerDies: an entrypoint may run forever, so a dead follower must end the
// attach itself. Nothing else would ever read its error.
func TestRunDetachesWhenTheFollowerDies(t *testing.T) {
	var out bytes.Buffer

	app, deps := newFakeApp(t, &out, &recorder{}, models.ExitStatus{})
	app.Out = fullDisk{}

	provider, ok := deps.provider.(*fakeProvider)
	if !ok {
		t.Fatal("the fake provider was replaced")
	}
	provider.blocks = true

	done := make(chan error, 1)
	go func() { done <- app.Run(t.Context(), []string{"run", "alpine:3.20"}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a run that could not show the guest's output returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run held the terminal after its follower died")
	}
}

// TestRunPropagatesTheExitCodeWhenTheRecordWriteFails: the status is already known by then, so the
// write is not a claim shard holds and the entrypoint's code is what the caller scripts on.
func TestRunPropagatesTheExitCodeWhenTheRecordWriteFails(t *testing.T) {
	var out bytes.Buffer

	app, _ := newFakeApp(t, &out, &recorder{fail: []string{"repo.Update3"}}, models.ExitStatus{Code: 3})

	err := app.Run(t.Context(), []string{"run", "alpine:3.20"})

	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatalf("Run returned %v, want an exit code of 3", err)
	}

	if !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("the error does not say the entrypoint had exited: %v", err)
	}
}

// fullDisk is stdout on a filesystem that has no space left, which is how a run loses its stream.
type fullDisk struct{}

func (fullDisk) Write(p []byte) (int, error) {
	return 0, errors.New("no space left on device")
}

// TestTheKeepAliveNotesNameNoAbsentVerb: an operator who is handed a command shard cannot dispatch
// is worse off than one who is handed none. The stop verb lands with SHARD-24.
func TestTheKeepAliveNotesNameNoAbsentVerb(t *testing.T) {
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  shard ([a-z]+)`).FindAllStringSubmatch(usage, -1) {
		documented[m[1]] = true
	}

	notes := map[string]string{
		"an interrupted start":  keepAliveNote(t, []string{"provider.Start"}, false),
		"an interrupted attach": keepAliveNote(t, nil, true),
	}

	for name, note := range notes {
		if !strings.Contains(note, "sandbox sandbox1") {
			t.Errorf("%s reported no sandbox id: %q", name, note)
		}

		for _, m := range regexp.MustCompile(`shard ([a-z]+)`).FindAllStringSubmatch(note, -1) {
			if !documented[m[1]] {
				t.Errorf("%s names shard %s, which shard cannot dispatch: %q", name, m[1], note)
			}
		}
	}
}

// keepAliveNote runs a sandbox an interrupt catches, and returns what run told the operator.
func keepAliveNote(t *testing.T, fail []string, blocks bool) string {
	t.Helper()

	var out bytes.Buffer
	app, deps := newFakeApp(t, &out, &recorder{fail: fail}, models.ExitStatus{})

	provider, ok := deps.provider.(*fakeProvider)
	if !ok {
		t.Fatal("the fake provider was replaced")
	}
	provider.blocks = blocks

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var exit ExitError
	if err := app.Run(ctx, []string{"run", "alpine:3.20"}); !errors.As(err, &exit) || exit.Code != InterruptedExitCode {
		t.Fatalf("Run returned %v, want the interrupted exit code", err)
	}

	return out.String()
}
