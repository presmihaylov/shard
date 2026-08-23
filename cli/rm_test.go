package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

func TestParseRmFlags(t *testing.T) {
	opts, err := parseRm([]string{"--force", "--time", "5s", "sandbox1"})
	if err != nil {
		t.Fatalf("parseRm: %v", err)
	}

	if opts.id != "sandbox1" || !opts.force || opts.grace != 5*time.Second {
		t.Errorf("parseRm gave %+v, want sandbox1, force and 5s", opts)
	}
}

func TestParseRmRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":            {},
		"two ids":          {"sandbox1", "sandbox2"},
		"a flag after id":  {"sandbox1", "--force"},
		"a negative grace": {"--time", "-5s", "sandbox1"},
		"an unknown flag":  {"--recursive", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseRm(args); err == nil {
			t.Errorf("parseRm(%s) returned no error", name)
		}
	}
}

// TestRmFreesEveryHoldingInOrder: the record dies last, because it is the only handle by which the
// mount and the namespace can be found again.
func TestRmFreesEveryHoldingInOrder(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, sb)
	deps.providerSvc.(*fakeLifecycleProvider).status = models.Status{Exists: true, State: models.StateStopped}

	if err := app.Run(t.Context(), []string{"rm", "sandbox1"}); err != nil {
		t.Fatalf("rm: %v", err)
	}

	want := []string{"provider.Remove", "net.Release", "repo.Delete"}
	if got := r.calls[len(r.calls)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("rm freed %v, want %v", got, want)
	}

	if got := strings.TrimSpace(out.String()); got != "sandbox1" {
		t.Errorf("rm printed %q, want the bare id", out.String())
	}
}

func TestRmRefusesARunningSandbox(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, _ := newLifecycleApp(t, &out, r, running())

	err := app.Run(t.Context(), []string{"rm", "sandbox1"})
	if err == nil {
		t.Fatal("rm of a running sandbox returned no error")
	}
	if !strings.Contains(err.Error(), "shard stop sandbox1") {
		t.Errorf("rm failed with %v, want it to say to stop the sandbox first", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("the refused rm still freed %s: %v", step, r.calls)
		}
	}
}

// TestRmForceStopsThenRemoves: --force is the shorthand for the stop the operator would type first.
func TestRmForceStopsThenRemoves(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())

	if err := app.Run(t.Context(), []string{"rm", "--force", "sandbox1"}); err != nil {
		t.Fatalf("rm --force: %v", err)
	}

	provider := deps.providerSvc.(*fakeLifecycleProvider)
	if !provider.stopped || !provider.removed {
		t.Errorf("rm --force stopped=%v removed=%v, want both", provider.stopped, provider.removed)
	}

	stopped := slices.Index(r.calls, "provider.Stop")
	removed := slices.Index(r.calls, "provider.Remove")
	if stopped < 0 || removed < stopped {
		t.Errorf("rm --force ran %v, want the stop before the remove", r.calls)
	}
}

// TestRmIsIdempotent: the record dies last, so an id with no record has nothing else left either.
func TestRmIsIdempotent(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.repoSvc.(*fakeLifecycleRepo).missing = true

	if err := app.Run(t.Context(), []string{"rm", "sandbox1"}); err != nil {
		t.Fatalf("rm of an id that is already gone: %v", err)
	}

	for _, step := range []string{"provider.Remove", "net.Release", "repo.Delete"} {
		if slices.Contains(r.calls, step) {
			t.Errorf("rm of an id that is already gone freed %s: %v", step, r.calls)
		}
	}
}

// TestRmNamesWhatIsLeftOnTheHost: a partial rm must be legible, so the error lists every holding
// from the one that failed onwards.
func TestRmNamesWhatIsLeftOnTheHost(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.State = models.StateStopped

	r := &recorder{fail: []string{"net.Release"}}
	app, deps := newLifecycleApp(t, &out, r, sb)
	deps.providerSvc.(*fakeLifecycleProvider).status = models.Status{Exists: true, State: models.StateStopped}

	err := app.Run(t.Context(), []string{"rm", "sandbox1"})
	if err == nil {
		t.Fatal("a forced failure returned no error")
	}

	for _, left := range []string{"netns, veth and address lease", "record and state directory"} {
		if !strings.Contains(err.Error(), left) {
			t.Errorf("rm failed with %v, want it to name the %s it left behind", err, left)
		}
	}
	if strings.Contains(err.Error(), "runsc state and rootfs mount") {
		t.Errorf("rm failed with %v, but it did free the runsc state", err)
	}

	if deps.repoSvc.(*fakeLifecycleRepo).deleted {
		t.Error("rm deleted the record past a failure, so nothing can reach what is left")
	}
}
