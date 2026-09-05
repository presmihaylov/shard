package cli

import (
	"bytes"
	"context"
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

func TestExecTakesAName(t *testing.T) {
	var out bytes.Buffer

	sb := running()
	sb.Name = "builder"

	app, d := newClientApp(t, &out, sb)

	if err := app.Run(context.Background(), []string{"exec", "builder", "--", "/bin/true"}); err != nil {
		t.Fatalf("exec by name: %v", err)
	}

	if got := d.providerSvc.(*fakeLifecycleProvider).execID; got != sb.ID {
		t.Fatalf("the provider was given %q, want the resolved id %q", got, sb.ID)
	}
}
