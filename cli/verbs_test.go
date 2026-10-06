package cli

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/bundle"
)

// newDaemonCreateApp is newClientApp with an image service that pulls nothing, so a create round trip needs no registry.
func newDaemonCreateApp(t *testing.T, out *bytes.Buffer) (App, *fakeDaemon, *recorder) {
	t.Helper()

	r := &recorder{}
	app, d := newLifecycleApp(t, out, r, models.Sandbox{})
	d.imageSvc = fakeImages{r: r}

	return app, d, r
}

// The id is the command's output, so a shell can take it: id=$(shard create alpine).
func TestCreatePrintsTheIDTheDaemonAnswered(t *testing.T) {
	var out bytes.Buffer

	app, d, r := newDaemonCreateApp(t, &out)
	var stderr bytes.Buffer
	app.Err = &stderr

	if err := app.Run(t.Context(), []string{"create", "--name", "builder", "--memory", "512MiB", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("create printed %q, want the bare sandbox id", out.String())
	}
	// A person who named the sandbox sees the name, on stderr so the id stays the only output. (SHARD-728)
	if got := stderr.String(); got != "created sandbox builder\n" {
		t.Errorf("create wrote %q to stderr, want the name", got)
	}

	// The daemon ran the verb: the pull, the record and the start all happened behind the socket.
	want := []string{"repo.Create", "images.Pull", "net.Allocate", "provider.Create", "provider.Start"}
	if got := keep(r.seen(), want...); !slices.Equal(got, want) {
		t.Errorf("the daemon drove %v, want %v", got, want)
	}

	created := d.repoSvc.(*fakeLifecycleRepo).created
	if created.Name != "builder" || created.Resources.MemoryMiB != 512 || created.State != models.StateRunning {
		t.Errorf("the record is %+v, want builder with 512 MiB and running", created)
	}
	spec := d.providerSvc.(*fakeLifecycleProvider).created
	if spec.ID != "sandbox2" || spec.Name != "builder" || len(spec.Entrypoint) != 0 {
		t.Errorf("the substrate got %+v, want sandbox2 named builder with no app", spec)
	}
}

func TestCreatePrintsTheDaemonsRefusalAsItCame(t *testing.T) {
	var out bytes.Buffer

	app, _, r := newDaemonCreateApp(t, &out)

	err := app.Run(t.Context(), []string{"create", "--secret", "NOPE", "alpine:3.20"})
	if err == nil || err.Error() != "secret NOPE does not exist: run shard secret set --destination <host> NOPE first" {
		t.Errorf("create = %v, want the daemon's refusal as it came", err)
	}
	if slices.Contains(r.seen(), "images.Pull") {
		t.Errorf("a refused create still cost a pull: %v", r.seen())
	}
}

// A disk the substrate refuses is the operator's to fix, so create names the flag and keeps the path in the daemon log. (SHARD-750, SHARD-751)
func TestCreatePrintsADiskRefusalInTheFlag(t *testing.T) {
	var out bytes.Buffer

	app, d, _ := newDaemonCreateApp(t, &out)
	d.providerSvc.(*fakeLifecycleProvider).createErr = fmt.Errorf("seed /var/lib/shard/sandboxes/sandbox2/disk.img: %w", &bundle.BoundError{
		Fix: "the image takes a 900 MiB disk, more than the 512 MiB disk bound; set resources.disk_mib to 900 MiB or more",
	})

	err := app.Run(t.Context(), []string{"create", "--disk", "512MiB", "alpine:3.20"})
	if err == nil || err.Error() != "the image takes a 900 MiB disk, more than the 512 MiB disk bound; set --disk to 900 MiB or more" {
		t.Errorf("create = %v, want the fix in the flag alone", err)
	}
}

// A refusal for the state is a 409, and the operator reads the state and the fix, not the status.
func TestRemovePrintsTheStateAndTheFix(t *testing.T) {
	var out bytes.Buffer

	app, _ := newClientApp(t, &out, running())

	err := app.Run(t.Context(), []string{"remove", "sandbox1"})
	if err == nil || err.Error() != "sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force" {
		t.Errorf("remove = %v, want the state and the fix", err)
	}
}

func TestRemoveForceStopsThenRemovesThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"remove", "--force", "sandbox1"}); err != nil {
		t.Fatalf("remove --force: %v", err)
	}

	provider := d.providerSvc.(*fakeLifecycleProvider)
	if !provider.stopped || !provider.removed || provider.grace != models.StopGrace {
		t.Errorf("remove --force stopped=%v removed=%v grace=%s, want both with the fixed %s", provider.stopped, provider.removed, provider.grace, models.StopGrace)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("remove printed %q, want the bare id", out.String())
	}
}

// A plain remove of an id with no record fails like every other verb (SHARD-282).
func TestRemoveOfAMissingSandboxFails(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	err := app.Run(t.Context(), []string{"remove", "ghost"})
	if err == nil || err.Error() != "no sandbox ghost" {
		t.Errorf("remove returned %v, want 'no sandbox ghost'", err)
	}
	if out.Len() != 0 {
		t.Errorf("remove printed %q, want nothing", out.String())
	}
}

// The record dies last, so an id with no record has nothing else left either: remove --force of it is a warning, as rm -f is.
func TestRemoveForceOfAMissingSandboxWarnsAndExitsZero(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())
	d.repoSvc.(*fakeLifecycleRepo).missing = true

	if err := app.Run(t.Context(), []string{"remove", "--force", "ghost"}); err != nil {
		t.Fatalf("remove --force of an id that is already gone: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "shard: warning: sandbox ghost does not exist, so there is nothing to remove" {
		t.Errorf("remove --force printed %q, want the warning alone", out.String())
	}
}

func TestStopGivesTheFixedGraceThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"stop", "sandbox1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := d.providerSvc.(*fakeLifecycleProvider).grace; got != models.StopGrace {
		t.Errorf("the provider got the grace %s, want the fixed %s", got, models.StopGrace)
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

func TestPauseAndResumeRunThroughTheDaemon(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, running())

	if err := app.Run(t.Context(), []string{"pause", "sandbox1"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := d.providerSvc.(*fakeLifecycleProvider).checkpoint; got != "/checkpoints/sandbox1" {
		t.Errorf("the provider was told to write %q, want the repository's checkpoint directory", got)
	}

	if err := app.Run(t.Context(), []string{"resume", "sandbox1"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sb := d.repoSvc.(*fakeLifecycleRepo).sb; sb.State != models.StateRunning {
		t.Errorf("the record is %s after the resume, want running", sb.State)
	}
	if got := strings.TrimSpace(out.String()); got != "sandbox1\nsandbox1" {
		t.Errorf("the two verbs printed %q, want the bare id each time", out.String())
	}
}

func TestForkPrintsTheNewIDTheDaemonAnswered(t *testing.T) {
	var out bytes.Buffer

	source := running()
	app, d := newClientApp(t, &out, source)
	var stderr bytes.Buffer
	app.Err = &stderr

	if err := app.Run(t.Context(), []string{"fork", "--name", "web-2", "sandbox1"}); err != nil {
		t.Fatalf("fork: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox2" {
		t.Errorf("fork printed %q, want the new id", got)
	}
	if got := stderr.String(); got != "created sandbox web-2\n" {
		t.Errorf("fork wrote %q to stderr, want the name", got)
	}
	if got := d.repoSvc.(*fakeLifecycleRepo).created; got.Name != "web-2" || got.Image != source.Image {
		t.Errorf("fork created %+v, want the source's image under the new name", got)
	}
}

// A refusal for the state is a 409, and the operator reads the state and the fix, not the status.
func TestPausePrintsTheStateAndTheFix(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped
	app, _ := newClientApp(t, &out, sb)

	err := app.Run(t.Context(), []string{"pause", "sandbox1"})
	if err == nil || err.Error() != "sandbox sandbox1 is stopped: pause takes a running sandbox" {
		t.Errorf("pause = %v, want the state and the fix", err)
	}
}

// A verb the provider does not claim is refused in the daemon, and the CLI prints that refusal as it came.
func TestForkPrintsTheDaemonsRefusalOfAnUnclaimedVerb(t *testing.T) {
	var out bytes.Buffer

	app, d := newClientApp(t, &out, paused())
	d.providerSvc.(*fakeLifecycleProvider).noFork = true

	err := app.Run(t.Context(), []string{"fork", "sandbox1"})
	if err == nil || err.Error() != "provider fake does not support fork on this host; use a server that supports fork" {
		t.Errorf("fork = %v, want the provider and the verb", err)
	}
}

// Once a verb speaks the socket it never falls back to the files: with no daemon it fails on one line.
func TestTheLifecycleVerbsWithNoDaemonFailFast(t *testing.T) {
	for _, args := range [][]string{
		{"create", "alpine:3.20"},
		{"start", "sandbox1"},
		{"stop", "sandbox1"},
		{"remove", "sandbox1"},
		{"pause", "sandbox1"},
		{"resume", "sandbox1"},
		{"fork", "sandbox1"},
		{"snapshot", "create", "sandbox1"},
		{"snapshot", "list"},
		{"snapshot", "inspect", "web-base"},
		{"snapshot", "remove", "web-base"},
		{"create", "--snapshot", "web-base"},
		{"cp", "sandbox1:/srv/app", "/tmp/app"},
	} {
		var out bytes.Buffer

		root := shortRoot(t)
		app := App{Version: "test", Root: root, Out: &out, Err: &out}

		err := app.Run(t.Context(), args)
		if want := "cannot connect to the shard daemon at " + filepath.Join(root, api.SocketFile) + ": is it running? shard --root " + root + " daemon"; err == nil || err.Error() != want {
			t.Errorf("%v with no daemon returned %v, want %q", args, err, want)
		}
		if out.Len() != 0 {
			t.Errorf("%v printed %q before it failed", args, out.String())
		}
	}
}
