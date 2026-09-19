package vzvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/ext4"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

const stopGrace = 5 * time.Second

// harness is one provider over one short root: a unix socket path is 104 bytes at most, and t.TempDir is longer.
type harness struct {
	provider    *vzvm.Provider
	root        string
	disk        string
	saveRestore bool

	next atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessOn(t, true)
}

// newHarnessOn is a Mac that saves a VM, or one that only pauses it in place.
func newHarnessOn(t *testing.T, saveRestore bool) *harness {
	t.Helper()

	root, err := os.MkdirTemp("", "vz") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	h := &harness{root: root, disk: baseDisk(t, root), saveRestore: saveRestore}
	h.provider = h.open(t)

	return h
}

// open is a daemon start: a provider over the root, which holds nothing of an earlier one in memory.
func (h *harness) open(t *testing.T) *vzvm.Provider {
	t.Helper()

	p, err := vzvm.New(vzvm.Config{
		Shim:        os.Args[0],
		Kernel:      "kernel",
		Init:        initBinary,
		Dir:         h.root,
		Dirs:        h.stateDir,
		SaveRestore: h.saveRestore,
	})
	if err != nil {
		t.Fatalf("open the provider: %v", err)
	}

	return p
}

// reopen is a daemon restart: the first provider lets go of its shims, and a second one adopts them.
func (h *harness) reopen(t *testing.T) models.Provider {
	t.Helper()

	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}

	return h.open(t)
}

// stateDir answers for any id, as the repository does; only a spec's directory exists.
func (h *harness) stateDir(id string) (string, error) {
	return filepath.Join(h.root, "s", id), nil
}

func (h *harness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	id := fmt.Sprintf("sb-%d", h.next.Add(1))
	dir, _ := h.stateDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		// Best effort: a subtest may have stopped and removed this one already, and its errors say nothing new.
		ctx := context.Background()
		h.provider.Stop(ctx, id, stopGrace)
		h.provider.Remove(ctx, id)
	})

	return models.SandboxSpec{ID: id, StateDir: dir, RootDisk: h.disk, Entrypoint: entrypoint, Resources: models.Resources{DiskMiB: 16}}
}

// baseDisk is the smallest ext4 image the clone accepts; the fake guest never mounts it.
func baseDisk(t *testing.T, root string) string {
	t.Helper()

	path := filepath.Join(root, "base.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := ext4.NewWriter(f)
	if err := w.Create("etc", &ext4.File{Mode: ext4.S_IFDIR | 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestConformance(t *testing.T) {
	h := newHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/sh", "-c", "exit 0") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
		// The fake guest is a host process, so the suite writes under the root; a clone here proves the verbs and not the disk.
		Scratch: h.root,
		Reopen:  h.reopen,
	})
}

func TestCreateRefusesAnImageWithoutARootDisk(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 0")
	spec.RootDisk = ""

	err := h.provider.Create(t.Context(), spec)
	if err == nil || !strings.Contains(err.Error(), spec.ID) || !strings.Contains(err.Error(), "root disk") {
		t.Fatalf("Create = %v, want a refusal that names the sandbox and the disk", err)
	}
}

// An image with no PATH gets the OCI default, as the bundle gives it on Linux, so a named entrypoint resolves in the guest.
func TestCreateGivesAnImageWithoutAPathTheDefault(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "sh", "-c", "exit 0")
	spec.Env = []string{"HOME=/root"}

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Run struct {
			Env []string `json:"env"`
		} `json:"run"`
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/root", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	if !slices.Equal(r.Run.Env, want) {
		t.Fatalf("the record's env = %q, want %q", r.Run.Env, want)
	}
}

// A pause keeps the save, the disk and the identifier together; a stop of a paused sandbox leaves it stopped and the snapshot whole.
func TestPauseKeepsWhatAResumeAndAForkNeed(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot.json", "vm.vzvmstate", "disk.img", "checkpoint.img"} {
		if _, err := os.Stat(filepath.Join(snap, name)); err != nil {
			t.Errorf("the snapshot lacks %s: %v", name, err)
		}
	}
	if _, err := os.Stat(snap + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the pause left its staging directory: %v", err)
	}
	// The save ends the shim, so the substrate says stopped and the snapshot marker is what says paused.
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after Pause = %+v, %v", status, err)
	}
	if err := h.provider.Pause(t.Context(), spec.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "already paused") {
		t.Fatalf("a second Pause = %v, want a refusal", err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "resume it first") {
		t.Fatalf("Start of a paused sandbox = %v, want a refusal", err)
	}

	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), fork.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status of the fork = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || !status.Alive() {
		t.Fatalf("Status after Resume = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Fatalf("Resume of a live sandbox = %v, want a refusal", err)
	}

	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateStopped {
		t.Fatalf("Status after a Stop of a paused sandbox = %+v, %v", status, err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err == nil {
		t.Fatal("Resume after a Stop succeeded, and only a paused sandbox resumes")
	}
}

// A stopped sandbox starts again over the disk the stop kept, and a wait then answers the new run.
func TestStartBootsAgainAfterAStop(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 4")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != 4 {
		t.Fatalf("Wait = %+v, %v", exit, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}

	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err = h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != 4 {
		t.Fatalf("Wait after the second Start = %+v, %v", exit, err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("Start with the entrypoint already run = %v, want a refusal", err)
	}
}

// A host that cannot save has no optional verb: each is refused by name, and none freezes a VM in its shim.
func TestAHostWithoutSaveRefusesTheOptionalVerbs(t *testing.T) {
	h := newHarnessOn(t, false)
	if caps := h.provider.Capabilities(); caps.Pause || caps.Resume || caps.Fork {
		t.Fatalf("Capabilities = %+v, want none", caps)
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	refused := map[string]error{
		models.VerbPause:  h.provider.Pause(t.Context(), spec.ID, snap),
		models.VerbResume: h.provider.Resume(t.Context(), spec.ID, snap),
		models.VerbFork:   h.provider.Fork(t.Context(), snap, h.newSpec(t)),
	}
	for verb, err := range refused {
		var refusal *models.UnsupportedError
		if !errors.As(err, &refusal) || refusal.Verb != verb || refusal.Provider != vzvm.Name {
			t.Errorf("%s = %v, want unsupported on %s", verb, err, vzvm.Name)
		}
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the refusals = %+v, %v", status, err)
	}
}

// An exit the loop could not land is an error on every read, not a wait that never ends.
func TestALostExitSurfacesInsteadOfAnEndlessWait(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "exit 3")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spec.StateDir, 0o700) })
	if err := os.Chmod(spec.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := h.provider.Wait(ctx, spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Wait = %v, want the lost exit", err)
	}
	if _, err := h.provider.ExitStatus(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("ExitStatus = %v, want the lost exit", err)
	}
	if _, err := h.provider.Restarts(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "lost its lifecycle state") {
		t.Fatalf("Restarts = %v, want the lost exit", err)
	}
}

// A log that cannot open fails the attach, so no verb reports a sandbox whose output has nowhere to go.
func TestAnAdoptFailsWhenTheLogCannotOpen(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(spec.StateDir, "output.log")
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(log, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := h.open(t).Status(t.Context(), spec.ID); err == nil || !strings.Contains(err.Error(), "open the log") {
		t.Fatalf("Status over a fresh provider = %v, want the log open failure", err)
	}

	// The failed adopt took the guest's one control connection, so the stop goes through a provider that adopts it again.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := h.open(t).Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A fronted VM gets the same trust store the bundle plants on Linux, handed to the guest to write, and the variables that point at it.
func TestCreateHandsAFrontedGuestTheMergedTrustStore(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/true")
	spec.RootFS = t.TempDir()
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.ProxyCA = []byte("proxy-ca\n")

	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Run struct {
			Env   []string `json:"env"`
			Trust struct {
				Path  string `json:"path"`
				Roots []byte `json:"roots"`
			} `json:"trust"`
		} `json:"run"`
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		t.Fatal(err)
	}
	if r.Run.Trust.Path != "/etc/ssl/certs/ca-certificates.crt" || string(r.Run.Trust.Roots) != "image-roots\nproxy-ca\n" {
		t.Errorf("the record's trust = %q at %q, want the image roots then the proxy CA at the image path", r.Run.Trust.Roots, r.Run.Trust.Path)
	}
	for _, key := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"} {
		if !slices.Contains(r.Run.Env, key+"=/etc/ssl/certs/ca-certificates.crt") {
			t.Errorf("the record's env lacks %s: %q", key, r.Run.Env)
		}
	}
}

// A grant after the create edits the record the next start sends: the placeholder, the trust store, and the ungrant that takes the placeholder back.
func TestTheEnvironmentIsTheRecordTheNextStartSends(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/true")
	spec.RootFS = t.TempDir()
	if err := os.MkdirAll(filepath.Join(spec.RootFS, "etc/ssl/certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.RootFS, "etc/ssl/certs/ca-certificates.crt"), []byte("image-roots\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec.Env = []string{"HELD=1"}
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	env, err := h.provider.Environment(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.CanSetEnv("HELD"); err == nil {
		t.Error("CanSetEnv let a held name through")
	}
	if err := env.TrustProxy([]byte("proxy-ca\n")); err != nil {
		t.Fatal(err)
	}
	if err := env.SetEnv("TOKEN", "shard-placeholder"); err != nil {
		t.Fatal(err)
	}
	if err := env.SetEnv("TOKEN", "again"); err == nil {
		t.Error("SetEnv set a name the guest already holds")
	}

	read := func() (env []string, trust string) {
		t.Helper()
		blob, err := os.ReadFile(filepath.Join(spec.StateDir, "vm.json"))
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Run struct {
				Env   []string `json:"env"`
				Trust *struct {
					Roots []byte `json:"roots"`
				} `json:"trust"`
			} `json:"run"`
		}
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatal(err)
		}
		if r.Run.Trust != nil {
			trust = string(r.Run.Trust.Roots)
		}

		return r.Run.Env, trust
	}
	got, trust := read()
	if !slices.Contains(got, "TOKEN=shard-placeholder") || !slices.Contains(got, "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt") || !slices.Contains(got, "HELD=1") {
		t.Errorf("the record's env = %q, want the placeholder, the trust variables and what the create set", got)
	}
	if trust != "image-roots\nproxy-ca\n" {
		t.Errorf("the record's trust = %q, want the image roots then the proxy CA", trust)
	}

	if err := env.RemoveEnv("TOKEN"); err != nil {
		t.Fatal(err)
	}
	got, trust = read()
	if slices.Contains(got, "TOKEN=shard-placeholder") || trust != "image-roots\nproxy-ca\n" {
		t.Errorf("after the ungrant env = %q, trust = %q; want the placeholder gone and the trust kept", got, trust)
	}

	if _, err := h.provider.Environment("sb-none"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Environment of an unknown sandbox = %v, want does not exist", err)
	}
}

// A reset of the transport, as a sleep of the host can cause, ends every stream; the provider dials again and the sandbox goes on.
func TestADroppedStreamIsDialedAgainWhileTheVMRuns(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do echo tick; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	logged := awaitLog(t, h.provider, spec.ID, 0)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGUSR1); err != nil {
		t.Fatalf("reset the fake shim's streams: %v", err)
	}

	// The log must keep flowing on the stream the provider opened again, and the control stream must answer an exec.
	awaitLog(t, h.provider, spec.ID, logged)
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo again"}, Stdout: out})
	written, _ := os.ReadFile(out.Name())
	if err != nil || exit.Code != 0 || !strings.Contains(string(written), "again") {
		t.Fatalf("Exec after the reset = %+v, %q, %v", exit, written, err)
	}
	status, err = h.provider.Status(t.Context(), spec.ID)
	if err != nil || status.State != models.StateRunning {
		t.Fatalf("Status after the reset = %+v, %v; want running", status, err)
	}
	if err := h.provider.Stop(t.Context(), spec.ID, stopGrace); err != nil {
		t.Fatal(err)
	}
}

// A daemon that starts over a root whose shim is gone finds the sandbox stopped, which the reconcile then records.
func TestANewProviderFindsASandboxWhoseShimIsGoneStopped(t *testing.T) {
	h := newHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(status.PID, syscall.SIGTERM); err != nil {
		t.Fatalf("end the fake shim: %v", err)
	}
	awaitExit(t, status.PID)

	status, err = h.open(t).Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exists || status.Alive() || status.PID != 0 {
		t.Fatalf("the new provider sees %+v, want the sandbox stopped with no pid", status)
	}
}

// awaitLog blocks until the sandbox log holds more than seen bytes, and answers how many it holds.
func awaitLog(t *testing.T, p *vzvm.Provider, id string, seen int) int {
	t.Helper()

	path, err := p.LogPath(id)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		out, _ := os.ReadFile(path)
		if len(out) > seen {
			return len(out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the log of %s did not grow past %d bytes", id, seen)

	return seen
}

// awaitExit blocks until the process is gone, which for the fake shim is its socket gone too.
func awaitExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit", pid)
}
