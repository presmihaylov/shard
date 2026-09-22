package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/daemon"
)

// SHARD-46: info answers off the host, so it needs no daemon and no socket.
func TestInfoPrintsTheProviderAndTheReason(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"info"}); err != nil {
		t.Fatalf("info: %v", err)
	}

	selected := daemon.SelectProvider("")
	if !strings.Contains(out.String(), "provider   "+selected.Provider) {
		t.Errorf("info printed %q, want the provider %s", out.String(), selected.Provider)
	}
	if !strings.Contains(out.String(), "reason     "+selected.Reason) {
		t.Errorf("info printed %q, want the reason %q", out.String(), selected.Reason)
	}
}

func TestInfoTakesNoArgument(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"info", "gvisor"}); err == nil {
		t.Fatal("info with an argument returned no error")
	}
}

// --provider is the answer, whatever the host holds.
func TestInfoFollowsTheNamedProvider(t *testing.T) {
	var out bytes.Buffer

	app := newApp(t, &out)
	app.Provider = "sysbox"
	if err := app.Run(t.Context(), []string{"info"}); err != nil {
		t.Fatalf("info: %v", err)
	}

	if !strings.Contains(out.String(), "sysbox") || !strings.Contains(out.String(), "--provider") {
		t.Errorf("info printed %q, want sysbox named by the flag", out.String())
	}
}
