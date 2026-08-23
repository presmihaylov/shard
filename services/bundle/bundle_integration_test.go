//go:build integration

package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/runspec"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// TestWritesSurviveAStopAndStart is the SHARD-11 acceptance criterion, run against a real runsc.
func TestWritesSurviveAStopAndStart(t *testing.T) {
	requireRunsc(t)

	stateDir := t.TempDir()
	b, lower := buildBundle(t, stateDir, []string{"/bin/sh", "-c", "echo written-by-the-first-run > /root/marker"})

	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	runSandbox(t, b, "shard-11-first")

	// The upper layer is the sandbox's own, so a guest write must be visible on the host.
	marker := filepath.Join(b.Upper, "root/marker")
	if got := readFile(t, marker); !strings.Contains(got, "written-by-the-first-run") {
		t.Fatalf("the first run wrote %q into the upper layer", got)
	}

	// A stop drops the merged view and nothing else. This is exactly what shard stop will do.
	if err := b.Unmount(); err != nil {
		t.Fatalf("unmount after the first run: %v", err)
	}

	second, lower := buildBundle(t, stateDir, []string{"/bin/sh", "-c", "cp /root/marker /root/read-back"})
	if err := second.Mount(lower); err != nil {
		t.Fatalf("mount the overlay again: %v", err)
	}
	t.Cleanup(func() { second.Unmount() })

	runSandbox(t, second, "shard-11-second")

	if got := readFile(t, filepath.Join(second.Upper, "root/read-back")); !strings.Contains(got, "written-by-the-first-run") {
		t.Errorf("the second run read back %q, want what the first run wrote", got)
	}
}

// TestTheSandboxOutlivesItsEntrypoint proves the one line this ticket exists for.
func TestTheSandboxOutlivesItsEntrypoint(t *testing.T) {
	requireRunsc(t)

	b, lower := buildBundle(t, t.TempDir(), []string{"/bin/true"})
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	id := "shard-11-keepalive"
	runscRoot := start(t, b, id)
	requireCleanExit(t, b)

	// The entrypoint is gone and the supervisor is not, so exec must still land in a live sandbox.
	out, err := runsc(runscRoot, "exec", id, "/bin/echo", "still-here").CombinedOutput()
	if err != nil {
		t.Fatalf("exec after the entrypoint exited: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "still-here") {
		t.Errorf("got %q from the exec, want still-here", out)
	}
}

// buildBundle returns the bundle and the image rootfs Mount stacks it over.
func buildBundle(t *testing.T, stateDir string, entrypoint []string) (bundle.Bundle, string) {
	t.Helper()

	if _, err := os.Stat(hostInitPath); err != nil {
		t.Skipf("no supervisor at %s: run make devbox-sync first", hostInitPath)
	}

	img := pullTestImage(t)

	svc, err := bundle.New(hostInitPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	spec := models.SandboxSpec{
		ID:         "shard-11",
		StateDir:   stateDir,
		RootFS:     img.RootFS,
		Entrypoint: entrypoint,
	}

	b, err := svc.Build(runspec.Resolve(spec, img.Config))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return b, img.RootFS
}

func pullTestImage(t *testing.T) image.Image {
	t.Helper()

	svc, err := image.New(t.TempDir())
	if err != nil {
		t.Fatalf("open the image service: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	img, err := svc.Pull(ctx, testImage)
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}

	return img
}

// runSandbox starts one, waits for the entrypoint to finish, then ends it the way Provider.Stop will.
func runSandbox(t *testing.T, b bundle.Bundle, id string) {
	t.Helper()

	runscRoot := start(t, b, id)
	requireCleanExit(t, b)
	stop(t, runscRoot, id)
}

func start(t *testing.T, b bundle.Bundle, id string) string {
	t.Helper()

	dir := t.TempDir()
	runscRoot := filepath.Join(dir, "runsc")

	// A restart reuses the state directory, so the previous run's status must not answer for this one.
	if err := os.Remove(b.ExitFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("clear %s: %v", b.ExitFile, err)
	}

	// The detached sandbox keeps the inherited stdio open, so a pipe here would never reach EOF.
	logPath := filepath.Join(dir, "run.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create %s: %v", logPath, err)
	}

	cmd := runsc(runscRoot, "run", "--detach", "--bundle", b.Dir, id)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	if err := logFile.Close(); err != nil {
		t.Fatalf("close %s: %v", logPath, err)
	}
	if runErr != nil {
		t.Fatalf("runsc run: %v: %s", runErr, readFile(t, logPath))
	}

	t.Cleanup(func() {
		// A failed test leaves a sandbox behind, so this is best effort and its errors say nothing new.
		runsc(runscRoot, "kill", "--all", id, "KILL").Run()
		runsc(runscRoot, "delete", "--force", id).Run()
		// --network=none leaves a bind mount in the runsc root that TempDir removal would trip over.
		exec.Command("umount", "-l", filepath.Join(runscRoot, "null-netns")).Run()
	})

	return runscRoot
}

func stop(t *testing.T, runscRoot, id string) {
	t.Helper()

	if out, err := runsc(runscRoot, "kill", "--all", id, "KILL").CombinedOutput(); err != nil {
		t.Fatalf("runsc kill: %v: %s", err, out)
	}
	if out, err := runsc(runscRoot, "delete", "--force", id).CombinedOutput(); err != nil {
		t.Fatalf("runsc delete: %v: %s", err, out)
	}
}

// This is what Provider.Wait will do: runsc wait would block forever on a supervisor.
func requireCleanExit(t *testing.T, b bundle.Bundle) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		blob, err := os.ReadFile(b.ExitFile)
		if err != nil {
			time.Sleep(100 * time.Millisecond)

			continue
		}

		var status models.ExitStatus
		if err := json.Unmarshal(blob, &status); err != nil {
			t.Fatalf("read the exit status from %s: %v", b.ExitFile, err)
		}
		if status.Code != 0 {
			t.Fatalf("the entrypoint exited with %+v, want code 0", status)
		}

		return
	}

	t.Fatalf("the entrypoint exit status never appeared at %s", b.ExitFile)
}

// The sandbox needs no network here, and no cgroup: SHARD-13 and the provider own those.
// --overlay2=none matters: runsc defaults to root:self, whose writes land in a filestore a stop throws away.
func runsc(root string, args ...string) *exec.Cmd {
	base := []string{"--root", root, "--network=none", "--ignore-cgroups", "--overlay2=none"}

	return exec.Command("runsc", append(base, args...)...)
}

func requireRunsc(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("runsc needs root")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("no runsc on this host")
	}
}
