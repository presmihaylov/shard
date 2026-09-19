package vzshim

import (
	"errors"
	"os"
	"os/exec"
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

// The daemon must link this package, or the linker drops the embedded shim and make build-darwin ships nothing.
// The shim must not, or every build embeds the previous one and two builds of one tree never match.
func TestTheDaemonLinksTheEmbeddedShimAndTheShimDoesNot(t *testing.T) {
	if !strings.Contains(deps(t, "../../cmd/shard"), self) {
		t.Fatal("cmd/shard on darwin does not link pkg/vzshim")
	}
	if strings.Contains(deps(t, "../../cmd/shard-vz-shim"), self) {
		t.Fatal("cmd/shard-vz-shim links pkg/vzshim, so it would embed its own previous build")
	}
}

const self = "github.com/presmihaylov/shard/pkg/vzshim\n"

func deps(t *testing.T, pkg string) string {
	t.Helper()

	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Env = append(os.Environ(), "GOOS=darwin", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v: %s", pkg, err, out)
	}

	return string(out)
}
