package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/provider/firecracker"
	"github.com/presmihaylov/shard/services/provider/gvisor"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

// rootOf is an absolute root of exactly n bytes.
func rootOf(n int) string {
	return "/" + strings.Repeat("r", n-1)
}

func TestTheDaemonRefusesARootItsLongestSocketDoesNotFitUnder(t *testing.T) {
	// What each provider adds to the root: "/jail/firecracker/", a 24-byte id and "/root/api.sock"; "/sandboxes/", the id and vz's longest socket; or "/shard.sock".
	for provider, suffix := range map[string]int{firecracker.Name: 56, vzvm.Name: 45, gvisor.Name: 11} {
		t.Run(provider, func(t *testing.T) {
			most := maxSocketPath - suffix

			if err := checkSocketPaths(rootOf(most), provider); err != nil {
				t.Errorf("a root of %d bytes was refused: %v", most, err)
			}

			long := rootOf(most + 1)
			err := checkSocketPaths(long, provider)
			if err == nil {
				t.Fatalf("a root of %d bytes was taken, want a refusal", most+1)
			}
			for _, want := range []string{long, provider, strconv.Itoa(maxSocketPath), "at most " + strconv.Itoa(most) + " bytes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

// The check runs before the datadir, so a refused firecracker root gets no image, no mount and no dir.
func TestRunRefusesALongRootBeforeItWritesAnything(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, strings.Repeat("r", maxSocketPath))

	err := Run(t.Context(), Config{Root: root, Provider: firecracker.Name, Out: os.Stderr})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Run over a %d-byte root returned %v, want the refusal", len(root), err)
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read %s: %v", parent, err)
	}
	if len(entries) != 0 {
		t.Errorf("Run left %d entries beside the refused root, want none", len(entries))
	}
}
