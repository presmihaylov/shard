package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// TestStopTakesAName proves the name is turned into an id before any verb sees it: the fakes answer
// for the id alone, so a name that reached them would fail.
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
