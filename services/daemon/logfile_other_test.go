//go:build !darwin

package daemon

import (
	"io"
	"strings"
	"testing"
)

func TestRunRefusesALogOffAMac(t *testing.T) {
	err := Run(t.Context(), Config{Root: t.TempDir(), Out: io.Discard, LogPath: "/var/log/shard/daemon.log"})
	if err == nil || !strings.Contains(err.Error(), "--log is for the daemon on a Mac") {
		t.Fatalf("Run = %v, want the refusal", err)
	}
}
