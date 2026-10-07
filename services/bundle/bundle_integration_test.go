//go:build integration

package bundle_test

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/presmihaylov/shard/services/supervisor"
)

// hostInitPath is where make devbox-sync installs the supervisor.
const hostInitPath = "/usr/local/bin/shard-init"

const testImage = "alpine:3.20"

// TestWritesSurviveAStopAndStart is the SHARD-11 acceptance criterion, run against a real runsc.
func TestWritesSurviveAStopAndStart(t *testing.T) {
	requireRunsc(t)

	stateDir := t.TempDir()
	b, lower := buildBundle(t, stateDir)

	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	runSandbox(t, b, "shard-11-first", "/bin/sh", "-c", "echo written-by-the-first-run > /root/marker")

	// The upper layer is the sandbox's own, so a guest write must be visible on the host.
	marker := filepath.Join(b.Upper, "root/marker")
	if got := readFile(t, marker); !strings.Contains(got, "written-by-the-first-run") {
		t.Fatalf("the first run wrote %q into the upper layer", got)
	}

	// A stop drops the merged view and nothing else. This is exactly what shard stop will do.
	if err := b.Unmount(); err != nil {
		t.Fatalf("unmount after the first run: %v", err)
	}

	second, lower := buildBundle(t, stateDir)
	if err := second.Mount(lower); err != nil {
		t.Fatalf("mount the overlay again: %v", err)
	}
	t.Cleanup(func() { second.Unmount() })

	runSandbox(t, second, "shard-11-second", "/bin/sh", "-c", "cp /root/marker /root/read-back")

	if got := readFile(t, filepath.Join(second.Upper, "root/read-back")); !strings.Contains(got, "written-by-the-first-run") {
		t.Errorf("the second run read back %q, want what the first run wrote", got)
	}
}

// TestTheSandboxOutlivesItsProcess proves the one line this ticket exists for.
func TestTheSandboxOutlivesItsProcess(t *testing.T) {
	requireRunsc(t)

	b, lower := buildBundle(t, t.TempDir())
	if err := b.Mount(lower); err != nil {
		t.Fatalf("mount the overlay: %v", err)
	}
	t.Cleanup(func() { b.Unmount() })

	id := "shard-11-keepalive"
	runscRoot, logPath := start(t, b, id)
	runToEnd(t, b, runscRoot, id, logPath, "/bin/true")

	// The process is gone and the supervisor is not, so exec must still land in a live sandbox.
	out, err := runsc(runscRoot, "exec", id, "/bin/echo", "still-here").CombinedOutput()
	if err != nil {
		t.Fatalf("exec after the process exited: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "still-here") {
		t.Errorf("got %q from the exec, want still-here", out)
	}
}

// buildBundle returns the bundle and the image rootfs Mount stacks it over.
func buildBundle(t *testing.T, stateDir string) (bundle.Bundle, string) {
	t.Helper()

	return buildBoundedBundle(t, stateDir, 0)
}

// buildBoundedBundle provisions the disk the way the provider does before Build, sized to diskMiB, then builds over it.
func buildBoundedBundle(t *testing.T, stateDir string, diskMiB int64) (bundle.Bundle, string) {
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
		ID:        "shard-11",
		StateDir:  stateDir,
		RootFS:    img.RootFS,
		Resources: models.Resources{DiskMiB: diskMiB},
	}

	// Build writes the layers into the disk, so the disk is up first.
	existing, err := bundle.Open(stateDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := existing.Provision(spec.Resources); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { existing.UnmountDisk() })

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

// runSandbox starts one, runs argv in it to a clean exit, then ends it the way Provider.Stop will.
func runSandbox(t *testing.T, b bundle.Bundle, id string, argv ...string) {
	t.Helper()

	runscRoot, logPath := start(t, b, id)
	runToEnd(t, b, runscRoot, id, logPath, argv...)
	stop(t, runscRoot, id)
}

// start returns the runsc root and the log that holds shard-init's console.
func start(t *testing.T, b bundle.Bundle, id string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	runscRoot := filepath.Join(dir, "runsc")

	// A restart reuses the state directory, so the previous run's table must not answer for this one.
	if err := os.Remove(b.ExitFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("clear %s: %v", b.ExitFile, err)
	}

	// The detached sandbox keeps the inherited stdio open, so a pipe here would never reach EOF.
	logPath := filepath.Join(dir, "run.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create %s: %v", logPath, err)
	}

	// shard-init reports its process table on its fd 0, so the exit file is its stdin, as every provider hands it.
	exit, err := os.OpenFile(b.ExitFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the exit channel %s: %v", b.ExitFile, err)
	}

	cmd := runsc(runscRoot, "run", "--detach", "--bundle", b.Dir, id)
	cmd.Stdin = exit
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	if err := errors.Join(logFile.Close(), exit.Close()); err != nil {
		t.Fatalf("close the console and the exit channel: %v", err)
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

	return runscRoot, logPath
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

// runToEnd hands argv to shard-init as the providers do, and polls the table until it exited 0.
func runToEnd(t *testing.T, b bundle.Bundle, runscRoot, id, logPath string, argv ...string) {
	t.Helper()

	run := supervisor.RunSpec{Name: "probe", Argv: argv, WorkDir: "/", User: "0:0"}
	if err := supervisor.RunProcess(t.Context(), execIn(runscRoot, id), run); err != nil {
		t.Fatalf("run the process: %v; the console said: %s", err, readFile(t, logPath))
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := bundle.ReadProcessTable(b.ExitFile)
		if err != nil {
			t.Fatalf("read the process table: %v", err)
		}
		for _, row := range rows {
			if row.Name != run.Name || !row.State.Ended() {
				continue
			}
			if row.Exit == nil || row.Exit.Code != 0 {
				t.Fatalf("the process ended with %+v, want code 0; the console said: %s", row, readFile(t, logPath))
			}

			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("the process never ended in %s; the console said: %s", b.ExitFile, readFile(t, logPath))
}

// execIn is the exec a provider gives the supervisor, over bare runsc.
func execIn(runscRoot, id string) supervisor.ExecFunc {
	return func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		args := append([]string{"exec", "--user", spec.User, "--cwd", spec.WorkDir, id}, spec.Argv...)
		cmd := exec.CommandContext(ctx, "runsc", runscArgs(runscRoot, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr

		err := cmd.Run()
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return models.ExitStatus{Code: exit.ExitCode()}, nil
		}
		if err != nil {
			return models.ExitStatus{}, fmt.Errorf("runsc exec: %w", err)
		}

		return models.ExitStatus{}, nil
	}
}

// The sandbox needs no network here, and no cgroup: SHARD-13 and the provider own those.
// --overlay2=none matters: runsc defaults to root:self, whose writes land in a filestore a stop throws away.
func runsc(root string, args ...string) *exec.Cmd {
	return exec.Command("runsc", runscArgs(root, args...)...)
}

func runscArgs(root string, args ...string) []string {
	return append([]string{"--root", root, "--network=none", "--ignore-cgroups", "--overlay2=none"}, args...)
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
