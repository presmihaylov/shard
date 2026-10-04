package conformance

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// killWindowDir is where the lease holder lives in the guest, beside the copy of itself it leases.
const killWindowDir = "/tmp/killwindow"

// RequireAKillBeforeTheCommandIsNoLaunch kills an exec while its execve waits on a lease of the command, so the command never runs, and requires the provider to say so (SHARD-505).
func RequireAKillBeforeTheCommandIsNoLaunch(t *testing.T, p models.Provider, id string) {
	t.Helper()

	installKillWindow(t, p, id)

	held, err := os.CreateTemp(t.TempDir(), "killwindow-output")
	if err != nil {
		t.Fatalf("create a file for the lease holder's output: %v", err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Errorf("close the lease holder's output file: %v", err)
		}
	}()

	holder := make(chan error, 1)
	go func() {
		status, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{path.Join(killWindowDir, "killwindow"), killWindowDir}, Stdout: held, Stderr: held})
		if err == nil && status != (models.ExitStatus{}) {
			err = fmt.Errorf("the lease holder ended %+v", status)
		}
		holder <- err
	}()

	deadline := time.Now().Add(waitSlack)
	for !strings.Contains(readOutput(t, held.Name()), "leased") {
		if time.Now().After(deadline) {
			t.Fatalf("the lease holder in %s took no lease within %s: %s", id, waitSlack, readOutput(t, held.Name()))
		}
		select {
		case err := <-holder:
			t.Fatalf("the lease holder in %s ended before its lease: %v: %s", id, err, readOutput(t, held.Name()))
		case <-time.After(readyPoll):
		}
	}

	status, launchErr := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{path.Join(killWindowDir, "target")}})
	if err := <-holder; err != nil || !strings.Contains(readOutput(t, held.Name()), "killed") {
		t.Fatalf("the lease holder in %s killed nothing, so the window was never hit: %v: %s", id, err, readOutput(t, held.Name()))
	}
	if _, ok := errors.AsType[*models.CommandNotStartedError](launchErr); !ok {
		t.Fatalf("an exec killed before its command started ended %+v, %v; want a CommandNotStartedError", status, launchErr)
	}
}

// installKillWindow builds the lease holder for the guest and copies it in over an exec's stdin, since a VM's rootfs is no host directory.
func installKillWindow(t *testing.T, p models.Provider, id string) {
	t.Helper()

	built := filepath.Join(t.TempDir(), "killwindow")
	cmd := exec.Command(goTool(), "build", "-o", built, "github.com/presmihaylov/shard/services/provider/conformance/testdata/killwindow")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the lease holder: %v: %s", err, out)
	}

	src, err := os.Open(built)
	if err != nil {
		t.Fatalf("open the lease holder: %v", err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			t.Errorf("close the lease holder: %v", err)
		}
	}()

	out, err := os.CreateTemp(t.TempDir(), "install-output")
	if err != nil {
		t.Fatalf("create a file for the install output: %v", err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			t.Errorf("close the install output file: %v", err)
		}
	}()

	binary := path.Join(killWindowDir, "killwindow")
	script := fmt.Sprintf("mkdir -p %s && cat > %s && chmod 755 %s", killWindowDir, binary, binary)
	status, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", script}, Stdin: src, Stdout: out, Stderr: out})
	if err != nil || status != (models.ExitStatus{}) {
		t.Fatalf("copy the lease holder into %s ended %+v, %v: %s", id, status, err, readOutput(t, out.Name()))
	}
}

func readOutput(t *testing.T, name string) string {
	t.Helper()

	out, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(out)
}
