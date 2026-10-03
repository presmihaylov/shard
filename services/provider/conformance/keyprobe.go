package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// KeyProbePath is where InstallKeyProbe puts the probe in the guest.
const KeyProbePath = "/keyprobe"

// InstallKeyProbe builds a static probe that calls each keyring syscall once and prints the answer,
// as "add_key: ENOSYS", and copies it into rootfs. No test image ships a keyctl of its own.
func InstallKeyProbe(t *testing.T, rootfs string) {
	t.Helper()

	built := filepath.Join(t.TempDir(), "keyprobe")
	cmd := exec.Command(goTool(), "build", "-o", built, "github.com/presmihaylov/shard/services/provider/conformance/testdata/keyprobe")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the key probe: %v: %s", err, out)
	}

	probe, err := os.ReadFile(built)
	if err != nil {
		t.Fatalf("read the key probe: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, KeyProbePath), probe, 0o755); err != nil { //nolint:gosec // the guest entrypoint, which the guest root must execute
		t.Fatalf("install the key probe: %v", err)
	}
}

// goTool finds go for a root run, whose PATH sudo has usually reset.
func goTool() string {
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}

	return "/usr/local/go/bin/go"
}
