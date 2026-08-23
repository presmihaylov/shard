package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// TestStopTakesAName proves the name is turned into an id: stop prints what it acted on, and that is
// the id, never the name the operator typed.
func TestStopTakesAName(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.Name = "builder"

	app, _ := newLifecycleApp(t, &out, &recorder{}, sb)

	if err := app.Run(context.Background(), []string{"stop", "builder"}); err != nil {
		t.Fatalf("stop by name: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != sb.ID {
		t.Fatalf("stop printed %q, want the id %q", got, sb.ID)
	}
}

func TestRmTakesAName(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.Name = "builder"
	sb.State = models.StateStopped

	app, d := newLifecycleApp(t, &out, &recorder{}, sb)
	d.providerSvc.(*fakeLifecycleProvider).status.State = sb.State

	if err := app.Run(context.Background(), []string{"rm", "builder"}); err != nil {
		t.Fatalf("rm by name: %v", err)
	}

	if got := strings.TrimSpace(out.String()); got != sb.ID {
		t.Fatalf("rm printed %q, want the id %q", got, sb.ID)
	}
}

func TestParseCreateTakesAName(t *testing.T) {
	opts, err := parseCreate([]string{"--name", "builder", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if opts.name != "builder" {
		t.Fatalf("name = %q, want builder", opts.name)
	}
}

func TestParseCreateRefusesANameNoVerbCouldTakeBack(t *testing.T) {
	for _, name := range []string{"a/b", "", "dawn-flower-0c28"} {
		if _, err := parseCreate([]string{"--name", name, "alpine:3.20"}); err == nil {
			t.Fatalf("parseCreate took the name %q", name)
		}
	}
}

// TestCreateGivesTheNameToTheRecordAndToTheGuest proves both halves: the record carries the name so
// a later verb resolves it, and the spec carries it so the guest hostname is what the operator chose.
func TestCreateGivesTheNameToTheRecordAndToTheGuest(t *testing.T) {
	var out bytes.Buffer

	app, d := newFakeApp(t, &out, &recorder{})

	if err := app.Run(context.Background(), []string{"create", "--name", "builder", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := d.repoSvc.(*fakeRepo).created.Name; got != "builder" {
		t.Fatalf("the record carries the name %q, want builder", got)
	}
	if got := d.providerSvc.(*fakeProvider).spec.Name; got != "builder" {
		t.Fatalf("the spec carries the name %q, want builder", got)
	}
}

func TestCreateWithoutANameGivesTheGuestTheID(t *testing.T) {
	var out bytes.Buffer

	app, d := newFakeApp(t, &out, &recorder{})

	if err := app.Run(context.Background(), []string{"create", "alpine:3.20"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := d.repoSvc.(*fakeRepo).created.Name; got != "" {
		t.Fatalf("an unnamed create put %q in the record", got)
	}

	spec := d.providerSvc.(*fakeProvider).spec
	if spec.Name != spec.ID {
		t.Fatalf("the guest hostname is %q, want the id %q", spec.Name, spec.ID)
	}
}

// strictRepo answers for one id and refuses every other, so an unresolved name reaches nothing.
type strictRepo struct {
	sandboxRepo

	id   string
	name string
}

func (s strictRepo) Resolve(ref string) (string, error) {
	if ref == s.name {
		return s.id, nil
	}

	return ref, nil
}

func (s strictRepo) Get(id string) (models.Sandbox, error) {
	if id != s.id {
		return models.Sandbox{}, fmt.Errorf("sandbox %s: sandbox not found", id)
	}

	return models.Sandbox{ID: id, Name: s.name, State: models.StateRunning}, nil
}

func TestExecTakesAName(t *testing.T) {
	var out bytes.Buffer

	provider := &fakeExecProvider{state: models.StateRunning}

	app := App{
		Version: "test",
		Out:     &out,
		Err:     &out,
		newDeps: func(App) *deps {
			return &deps{
				repoSvc:     strictRepo{id: "sandbox1", name: "builder"},
				providerSvc: provider,
			}
		},
	}

	if err := app.Run(context.Background(), []string{"exec", "builder", "--", "/bin/true"}); err != nil {
		t.Fatalf("exec by name: %v", err)
	}

	if provider.id != "sandbox1" {
		t.Fatalf("the provider was given %q, want the resolved id sandbox1", provider.id)
	}
}
