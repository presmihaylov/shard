package pty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pair that cannot be allocated must say why. The close that follows it succeeds, and an error
// nobody made is not part of the reason.
func TestOpenReportsOnlyTheFailureThatHappened(t *testing.T) {
	notAMultiplexer := filepath.Join(t.TempDir(), "ptmx")
	if err := os.WriteFile(notAMultiplexer, nil, 0o600); err != nil {
		t.Fatalf("write the stand-in for the multiplexer: %v", err)
	}

	previous := ptmx
	ptmx = notAMultiplexer
	t.Cleanup(func() { ptmx = previous })

	pair, err := Open()
	if err == nil {
		t.Fatalf("Open gave a pair %+v from a file that is no multiplexer", pair)
	}

	if strings.Contains(err.Error(), "%!") {
		t.Errorf("Open reported %q, and a formatting verb with nothing to print is not a reason", err)
	}
}
