package vzshim

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
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

func TestABinaryWithoutTheGuestInitSaysSo(t *testing.T) {
	if _, err := fs.Stat(shimFS, "shim/"+initName); err == nil {
		t.Skip("this test binary carries the guest init")
	}
	if _, err := InstallInit(t.TempDir()); !errors.Is(err, ErrNoInit) {
		t.Fatalf("InstallInit without an init: %v", err)
	}
}

// The daemon must reference this package or the linker drops the embed; the shim must not, or it embeds its previous build.
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

// A host crash can cut an installed file under an intact stamp, so the next install writes it again (SHARD-355).
func TestACutInstallIsWrittenAgain(t *testing.T) {
	dir := t.TempDir()
	body := []byte("a guest init build")
	path, err := place(dir, initName, body, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(path, int64(len(body)/2)); err != nil {
		t.Fatal(err)
	}
	if _, err := place(dir, initName, body, nil); err != nil {
		t.Fatal(err)
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(held, body) {
		t.Fatalf("the install holds %q, want %q", held, body)
	}
}

// A stamp an older daemon wrote names no installed bytes, so it never vouches for the file beside it.
func TestAStampOfTheOldShapeInstallsAgain(t *testing.T) {
	dir := t.TempDir()
	body := []byte("a guest init build")
	if err := os.WriteFile(filepath.Join(dir, initName), body, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(dir, initName+".sha256")
	if err := os.WriteFile(stamp, fmt.Appendf(nil, "%x\n", sha256.Sum256(body)), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := place(dir, initName, body, nil); err != nil {
		t.Fatal(err)
	}
	held, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(held, stampOf(body, body)) {
		t.Fatalf("the stamp says %q, want both hashes", held)
	}
}
