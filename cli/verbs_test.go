package cli

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

// newDaemonCreateApp is newClientApp with an image service that pulls nothing, so a create round trip needs no registry.
func newDaemonCreateApp(t *testing.T, out *bytes.Buffer) (App, *deps, *recorder) {
	t.Helper()

	r := &recorder{}
	app, d := newLifecycleApp(t, out, r, models.Sandbox{})
	d.imageSvc = fakeImages{r: r}
	serveDaemon(t, d)

	return app, d, r
}

// The id is the command's output, so a shell can take it: id=$(shard create alpine).
func TestCreatePrintsTheIDTheDaemonAnswered(t *testing.T) {
	var out bytes.Buffer

	app, d, r := newDaemonCreateApp(t, &out)

	if err := app.Run(t.Context(), []string{"create", "--name", "builder", "--memory", "512", "alpine:3.20", "--", "echo", "1"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("create printed %q, want the bare sandbox id", out.String())
	}

	// The daemon ran the verb: the pull, the record and the start all happened behind the socket.
	want := []string{"images.Pull", "repo.Create", "net.Allocate", "provider.Create", "provider.Start"}
	if got := keep(r.calls, want...); !slices.Equal(got, want) {
		t.Errorf("the daemon drove %v, want %v", got, want)
	}

	created := d.repoSvc.(*fakeLifecycleRepo).created
	if created.Name != "builder" || created.Resources.MemoryMiB != 512 || created.State != models.StateRunning {
		t.Errorf("the record is %+v, want builder with 512 MiB and running", created)
	}
	spec := d.providerSvc.(*fakeLifecycleProvider).created
	if spec.ID != "sandbox2" || spec.Name != "builder" || !slices.Equal(spec.Entrypoint, []string{"echo", "1"}) {
		t.Errorf("the substrate got %+v, want sandbox2 named builder with the command", spec)
	}
}

func TestCreatePrintsTheDaemonsRefusalAsItCame(t *testing.T) {
	var out bytes.Buffer

	app, _, r := newDaemonCreateApp(t, &out)

	err := app.Run(t.Context(), []string{"create", "--secret", "NOPE", "alpine:3.20"})
	if err == nil || err.Error() != "secret NOPE does not exist: run shard secret set --to <host> NOPE first" {
		t.Errorf("create = %v, want the daemon's refusal as it came", err)
	}
	if slices.Contains(r.calls, "images.Pull") {
		t.Errorf("a refused create still cost a pull: %v", r.calls)
	}
}

// A refusal for the state is a 409, and the operator reads the state and the fix, not the status.
func TestRmPrintsTheStateAndTheFix(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, running())

	err := app.Run(t.Context(), []string{"rm", "sandbox1"})
	if err == nil || err.Error() != "sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force" {
		t.Errorf("rm = %v, want the state and the fix", err)
	}
}

func TestRmForceStopsThenRemovesThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"rm", "--force", "--time", "3s", "sandbox1"}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	provider := d.providerSvc.(*fakeLifecycleProvider)
	if !provider.stopped || !provider.removed || provider.grace != 3*time.Second {
		t.Errorf("rm --force stopped=%v removed=%v grace=%s, want both with 3s", provider.stopped, provider.removed, provider.grace)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("rm printed %q, want the bare id", out.String())
	}
}

// The record dies last, so an id with no record has nothing else left either: rm of it is a warning, not a failure.
func TestRmOfAMissingSandboxWarnsAndExitsZero(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	if err := app.Run(t.Context(), []string{"rm", "ghost"}); err != nil {
		t.Fatalf("rm of an id that is already gone: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "shard: warning: sandbox ghost does not exist, so there is nothing to remove" {
		t.Errorf("rm printed %q, want the warning alone", out.String())
	}
}

func TestStopPassesTheGraceThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"stop", "--time", "3s", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := d.providerSvc.(*fakeLifecycleProvider).grace; got != 3*time.Second {
		t.Errorf("the provider got the grace %s, want 3s", got)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("stop printed %q, want the bare id", out.String())
	}
}

func TestStartRunsAStoppedSandboxThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	app, d := newClientApp(t, &out, sb)

	if err := app.Run(t.Context(), []string{"start", "sandbox1"}); err != nil {
		t.Fatalf("start: %v", err)
	}

	if !d.providerSvc.(*fakeLifecycleProvider).started {
		t.Error("the provider was never asked to start")
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("start printed %q, want the bare id", out.String())
	}
}

// Once a verb speaks the socket it never falls back to the files: with no daemon it fails on one line.
func TestTheLifecycleVerbsWithNoDaemonFailFast(t *testing.T) {
	for _, args := range [][]string{
		{"create", "alpine:3.20"},
		{"start", "sandbox1"},
		{"stop", "sandbox1"},
		{"rm", "sandbox1"},
	} {
		var out bytes.Buffer

		root := shortRoot(t)
		app := App{Version: "test", Root: root, Out: &out, Err: &out}

		err := app.Run(t.Context(), args)
		if want := "cannot connect to shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? systemctl status shard"; err == nil || err.Error() != want {
			t.Errorf("%s with no daemon returned %v, want %q", args[0], err, want)
		}
		if out.Len() != 0 {
			t.Errorf("%s printed %q before it failed", args[0], out.String())
		}
	}
}
