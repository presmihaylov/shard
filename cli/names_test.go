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

	app, _ := newClientApp(t, &out, sb)

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

	app, _ := newClientApp(t, &out, sb)

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
	if opts.Name != "builder" {
		t.Fatalf("name = %q, want builder", opts.Name)
	}
}

func TestParseCreateRefusesANameNoVerbCouldTakeBack(t *testing.T) {
	for _, name := range []string{"a/b", "", "dawn-flower-0c28"} {
		if _, err := parseCreate([]string{"--name", name, "alpine:3.20"}); err == nil {
			t.Fatalf("parseCreate took the name %q", name)
		}
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
