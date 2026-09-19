//go:build darwin

package vzshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func embedded(t *testing.T) {
	t.Helper()
	if !Embedded() {
		t.Skip("this test binary carries no shim: run make build-shard-vz-shim first")
	}
}

func TestTheShimInstallsOnceAndCarriesTheEntitlement(t *testing.T) {
	embedded(t)
	dir := t.TempDir()

	shim, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	first, err := os.Stat(shim)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Install(dir); err != nil || again != shim {
		t.Fatalf("second Install: %s, %v", again, err)
	}
	second, err := os.Stat(shim)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatal("the second Install rewrote a shim that had not changed")
	}
	if out := run(t, "codesign", "-d", "--entitlements", "-", shim); !strings.Contains(out, "com.apple.security.virtualization") {
		t.Fatalf("the installed shim carries no virtualization entitlement:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, entitlementsName)); !os.IsNotExist(err) {
		t.Fatalf("the entitlements stayed on disk: %v", err)
	}
}

// Sixteen first-use callers at once is what a daemon that starts sixteen sandboxes does.
func TestConcurrentInstallersLeaveOneSignedShimAndNoDebris(t *testing.T) {
	embedded(t)
	dir := t.TempDir()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			if _, err := Install(dir); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Install: %v", err)
	}

	shim := filepath.Join(dir, shimName)
	run(t, "codesign", "--verify", "--strict", shim)
	if out := run(t, "codesign", "-d", "--entitlements", "-", shim); !strings.Contains(out, "com.apple.security.virtualization") {
		t.Fatalf("the shim carries no virtualization entitlement:\n%s", out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{shimName, shimName + ".sha256"}; strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("the install left %v, want %v", names, want)
	}
	if again, err := Install(dir); err != nil || again != shim {
		t.Fatalf("Install after the race: %s, %v", again, err)
	}
}

func run(t *testing.T, argv ...string) string {
	t.Helper()

	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}

	return string(out)
}
