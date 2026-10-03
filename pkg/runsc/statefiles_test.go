package runsc_test

import (
	"os"
	"path/filepath"
	"testing"
)

// stateFile writes a flat runsc state file for id under root, named the way runsc names its own.
func stateFile(t *testing.T, root, id, body string) string {
	t.Helper()

	path := filepath.Join(root, id+"_sandbox:"+id+".state")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the state file: %v", err)
	}

	return path
}

func TestForgetRemovesOnlyTheNamedSandbox(t *testing.T) {
	r, _ := fake(t, "", "", 0)
	root := r.Root()

	mine := stateFile(t, root, "amber-otter-1a2b", `{"goferPid":4343}`)
	other := stateFile(t, root, "coral-finch-9z8y", `{"goferPid":5151}`)

	if err := r.Forget("amber-otter-1a2b"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatalf("Forget left the named state file: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("Forget removed another sandbox's state file: %v", err)
	}
}

func TestForgetIsNilWhenNothingIsSaved(t *testing.T) {
	r, _ := fake(t, "", "", 0)

	if err := r.Forget("amber-otter-1a2b"); err != nil {
		t.Fatalf("Forget of a missing sandbox: %v", err)
	}
}

// runscFile writes one of runsc's per-container files under root and returns its path.
func runscFile(t *testing.T, root, name string) string {
	t.Helper()

	path := filepath.Join(root, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	return path
}

// Forget drops the lock and the control socket too, not only the state, so a teardown leaves nothing delete --force used to remove (SHARD-440).
func TestForgetRemovesTheLockAndSocketToo(t *testing.T) {
	r, _ := fake(t, "", "", 0)
	root := r.Root()

	state := stateFile(t, root, "amber-otter-1a2b", `{"goferPid":4343}`)
	lock := runscFile(t, root, "amber-otter-1a2b_sandbox:amber-otter-1a2b.lock")
	sock := runscFile(t, root, "runsc-amber-otter-1a2b.sock")
	otherSock := runscFile(t, root, "runsc-coral-finch-9z8y.sock")

	if err := r.Forget("amber-otter-1a2b"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	for _, gone := range []string{state, lock, sock} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("Forget left %s behind: %v", filepath.Base(gone), err)
		}
	}
	if _, err := os.Stat(otherSock); err != nil {
		t.Errorf("Forget removed another sandbox's socket: %v", err)
	}
}
