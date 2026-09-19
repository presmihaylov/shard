package vzshim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestABinaryWithoutTheShimSaysSoInsteadOfInstallingNothing(t *testing.T) {
	if Embedded() {
		t.Skip("this test binary carries the shim")
	}
	if _, err := Install(t.TempDir()); !errors.Is(err, ErrNoShim) {
		t.Fatalf("Install without a shim: %v", err)
	}
}

// The daemon must reference this package, or the linker drops the embedded shim and make build-darwin ships nothing.
// The shim must not link it, or every build embeds the previous one and two builds of one tree never match.
func TestTheDaemonLinksTheEmbeddedShimAndTheShimDoesNot(t *testing.T) {
	daemon := filepath.Join(t.TempDir(), "shard")
	build := exec.Command("go", "build", "-o", daemon, "../../cmd/shard")
	build.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	symbols, err := exec.Command("go", "tool", "nm", daemon).Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	if !strings.Contains(string(symbols), "pkg/vzshim.shimFS") {
		t.Fatal("the darwin daemon carries no embedded shim: the linker dropped pkg/vzshim")
	}

	list := exec.Command("go", "list", "-deps", "../../cmd/shard-vz-shim")
	list.Env = append(os.Environ(), "GOOS=darwin", "CGO_ENABLED=0")
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	if strings.Contains(string(out), "github.com/presmihaylov/shard/pkg/vzshim\n") {
		t.Fatal("cmd/shard-vz-shim links pkg/vzshim, so it would embed its own previous build")
	}
}
