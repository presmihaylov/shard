package firecracker_test

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The test binary plays the jailer and firecracker when the provider execs it with this set; the guest is the real shard-init over unix sockets.
const (
	fakeVMMEnv  = "FIRECRACKER_FAKE_VMM"
	fakeInitEnv = "FIRECRACKER_FAKE_INIT"
	// fakeJailEnv is the chroot the fake jailer hands the fake vmm, which takes every path it is told as inside it.
	fakeJailEnv = "FIRECRACKER_FAKE_JAIL"
	// fakeStateEnv is the sandbox's state directory, where the fake keeps its guest and reads what a test asks of it.
	fakeStateEnv = "FIRECRACKER_FAKE_STATE"
)

// jailerFile is written beside the chroot with what the fake jailer was run with.
const jailerFile = "jailer.json"

// sessionsFile beside the jail base takes the pid of every vmm the fake jailer starts, which leads the session its whole guest stays in.
const sessionsFile = "vmm-sessions"

// Files a test puts in the state directory: controlsFile takes one line per reseed, freeze and thaw the guest reads, and an attach per control stream the host opens.
const (
	controlsFile = "controls"
	// refuseFreezeFile, while it exists, has the guest refuse every freeze, as one that cannot hold its root does.
	refuseFreezeFile = "refuse-freeze"
	// loseFreezeFile has the next freeze reach the guest and a drop take its answer, once.
	loseFreezeFile = "lose-freeze"
	// refuseReseedFile, while it exists, has the guest refuse every reseed.
	refuseReseedFile = "refuse-reseed"
	// oldGuestFile, while it exists, drops the overlay freeze from the guest's state, as a shard-init from before it sends.
	oldGuestFile = "old-guest"
	// snapshotsFile takes one line per snapshot the vmm writes: its type, and how many snapshots the file it wrote onto held.
	snapshotsFile = "snapshots"
	// refuseSnapshotFile, while it exists, has the vmm refuse every snapshot create, as one whose disk is full does.
	refuseSnapshotFile = "refuse-snapshot"
	// refuseResumeFile, while it exists, has the vmm refuse every resume of its vCPUs.
	refuseResumeFile = "refuse-resume"
	// severOnResetFile, while it exists, has a snapshot create's reset sever the transport too, until a USR2 lets streams through again.
	severOnResetFile = "sever-on-reset"
	// execInFreezeFile, once a test writes a sandbox id into it, has the next pause start an exec in the frozen guest first, and write how it ended to execResultFile.
	execInFreezeFile = "exec-in-freeze"
	execResultFile   = "exec-result"
	// holdSnapshotFile, while it exists, has a snapshot create hold the API as a large memory write does, and write heldSnapshotFile once it holds.
	holdSnapshotFile = "hold-snapshot"
	heldSnapshotFile = "held-snapshot"
	// fakeVersionEnv is the version the fake vmm names on --version, 1.17.0 when unset.
	fakeVersionEnv = "SHARD_FAKE_FIRECRACKER_VERSION"
)

// initBinary is the shard-init the fake vmm runs in place of a VM, built once per test run unless the env names one.
var initBinary string

func TestMain(m *testing.M) {
	// The vmm passes its whole environment to the guest, so only the -transport argv says which one this is.
	if os.Getenv(failingGuestEnv) == "1" && len(os.Args) == 3 && os.Args[1] == "-transport" {
		if err := failingGuest(strings.TrimPrefix(os.Args[2], "unix:")); err != nil {
			fmt.Fprintln(os.Stderr, "failing guest:", err)
			os.Exit(1)
		}
		os.Exit(models.SupervisorFailedExitCode)
	}
	if os.Getenv(bootFailingGuestEnv) == "1" && len(os.Args) == 3 && os.Args[1] == "-transport" {
		if err := bootFailingGuest(strings.TrimPrefix(os.Args[2], "unix:")); err != nil {
			fmt.Fprintln(os.Stderr, "boot failing guest:", err)
			os.Exit(1)
		}
		os.Exit(models.SupervisorFailedExitCode)
	}
	if os.Getenv(fakeVMMEnv) == "1" && slices.Contains(os.Args[1:], "--exec-file") {
		if err := fakeJailer(); err != nil {
			fmt.Fprintln(os.Stderr, "fake jailer:", err)
			os.Exit(1)
		}

		return
	}
	if os.Getenv(fakeVMMEnv) == "1" && slices.Equal(os.Args[1:], []string{"--version"}) {
		fmt.Printf("Firecracker v%s\n", cmp.Or(os.Getenv(fakeVersionEnv), "1.17.0"))

		return
	}
	if os.Getenv(fakeVMMEnv) == "1" {
		if err := fakeVMM(); err != nil {
			fmt.Fprintln(os.Stderr, "fake firecracker:", err)
			os.Exit(1)
		}

		return
	}

	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	initBinary = os.Getenv(fakeInitEnv)
	if initBinary == "" {
		dir, err := os.MkdirTemp("", "fcinit")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)

			return 1
		}
		defer os.RemoveAll(dir)
		initBinary = filepath.Join(dir, "shard-init")
		build := exec.Command("go", "build", "-o", initBinary, "../../../cmd/shard-init")
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build shard-init:", err)

			return 1
		}
	}
	// Every vmm the provider starts from here is this binary, and inherits the switch.
	os.Setenv(fakeVMMEnv, "1")
	os.Setenv(fakeInitEnv, initBinary)

	return m.Run()
}

// jailerArgs is what the fake jailer was run with.
type jailerArgs struct {
	ID           string `json:"id"`
	UID          int    `json:"uid"`
	GID          int    `json:"gid"`
	ParentCgroup string `json:"parentCgroup"`
}

// fakeJailer is the jailer without root: it runs the exec file in a session of its own over the chroot the provider filled, writes its pid there and exits.
func fakeJailer() error {
	flags := flag.NewFlagSet("fake-jailer", flag.ContinueOnError)
	var args jailerArgs
	flags.StringVar(&args.ID, "id", "", "")
	execFile := flags.String("exec-file", "", "")
	flags.IntVar(&args.UID, "uid", -1, "")
	flags.IntVar(&args.GID, "gid", -1, "")
	base := flags.String("chroot-base-dir", "", "")
	flags.String("cgroup-version", "", "")
	flags.StringVar(&args.ParentCgroup, "parent-cgroup", "", "")
	flags.Bool("new-pid-ns", false, "")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}

	chroot := filepath.Join(*base, filepath.Base(*execFile), args.ID, "root")
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(chroot), jailerFile), encoded, 0o600); err != nil {
		return err
	}
	// The harness keeps the state directories beside the jail base, as root/s/<id>.
	state := filepath.Join(filepath.Dir(*base), "s", args.ID)
	vmm := exec.Command(*execFile, append([]string{"--id", args.ID}, flags.Args()...)...)
	vmm.Env = append(os.Environ(), fakeJailEnv+"="+chroot, fakeStateEnv+"="+state)
	vmm.Stdout = os.Stdout
	vmm.Stderr = os.Stderr
	vmm.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := vmm.Start(); err != nil {
		return err
	}
	if err := note(filepath.Join(filepath.Dir(*base), sessionsFile), strconv.Itoa(vmm.Process.Pid)); err != nil {
		return err
	}
	pidFile, err := os.OpenFile(filepath.Join(chroot, filepath.Base(*execFile)+".pid"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(pidFile, vmm.Process.Pid); err != nil {
		return errors.Join(err, pidFile.Close())
	}

	return pidFile.Close()
}

// endSessions SIGKILLs what is left in every vmm session the file names: a VM's death ends its guest, but a kill of the vmm alone, or of its group, misses the guest here, whose entrypoint leads a group of its own.
func endSessions(t *testing.T, path string) {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read the vmm sessions: %v", err)

		return
	}
	sids := map[int]bool{}
	for field := range strings.FieldsSeq(string(blob)) {
		sid, err := strconv.Atoi(field)
		if err != nil {
			t.Errorf("read the vmm sessions: %v", err)

			return
		}
		sids[sid] = true
	}
	for deadline := time.Now().Add(stopGrace); ; time.Sleep(20 * time.Millisecond) {
		left, err := inSessions(sids)
		if err != nil {
			t.Errorf("list the vmm sessions: %v", err)

			return
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("processes %v are left in the vmm sessions %s after SIGKILL", left, stopGrace)

			return
		}
		if err := killAll(left); err != nil {
			t.Errorf("end the vmm sessions: %v", err)

			return
		}
	}
}

// killAll SIGKILLs every pid; one that has exited since the scan is no error.
func killAll(pids []int) error {
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill %d: %w", pid, err)
		}
	}

	return nil
}

// inJail is where a path the fake vmm is told lives on the host, the way a chroot resolves it.
func inJail(path string) string {
	return filepath.Join(os.Getenv(fakeJailEnv), path)
}

// fakeVMM is firecracker without KVM: its API on --api-sock, one shard-init process as the guest, and the vsock proxy in front of it.
func fakeVMM() error {
	flags := flag.NewFlagSet("fake-firecracker", flag.ContinueOnError)
	flags.String("id", "", "")
	apiSock := flags.String("api-sock", "", "")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	socket := inJail(*apiSock)
	fmt.Println("fake firecracker: api on", socket)

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	f := &fake{dir: os.Getenv(fakeStateEnv), state: "Not started", streams: map[net.Conn]*stream{}, sending: map[chan struct{}]struct{}{}}
	server := &http.Server{Handler: f} //nolint:gosec // a fake behind a unix socket needs no timeouts
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGUSR2)
	// USR1 severs the guest's transport for good and USR2 drops the streams once, so a test can lose the control stream of a live VM.
	sig := <-signals
	for sig == syscall.SIGUSR1 || sig == syscall.SIGUSR2 {
		f.drop(sig == syscall.SIGUSR1)
		sig = <-signals
	}
	if f.stop() {
		// The guest's exit ends the group, this process with it.
		select {}
	}
	err = server.Close()
	if closed := <-served; !errors.Is(closed, http.ErrServerClosed) {
		err = errors.Join(err, closed)
	}

	return err
}

// fake is the microVM's state as the API sees it, and the guest process once it is started.
type fake struct {
	// dir is the sandbox's state directory, which holds the guest, the bootFile and the files a test puts there.
	dir string

	mu    sync.Mutex
	state string
	boot  json.RawMessage
	vsock string
	cmd   *exec.Cmd
	// drives is every drive put, in order, which with the boot args is what a test reads back from bootFile.
	drives []json.RawMessage
	// streams is every host connection through the vsock device, so a drop can end them all; severed refuses the ones after it.
	streams map[net.Conn]*stream
	severed bool
	// frozen is what the guest was last told, freeze or thaw, which a snapshot keeps the way its memory would.
	frozen bool
	// tracking is the dirty-page log the machine config or the load turned on; a Diff without it takes every resident page.
	tracking bool
	// sending is every guest-to-host copy still open, which a guest that powers off drains through before the vmm dies.
	sending map[chan struct{}]struct{}
}

// bootFile is written in the state directory at the start, with what the vmm was told to boot.
const bootFile = "boot.json"

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"fake","state":%q,"vmm_version":"0.0.0","app_name":"Firecracker"}`, f.state)

		return
	}
	// The vmm takes a configuration before the boot and the snapshot verbs after it, and refuses each in the other half.
	booted := f.state != "Not started"
	postBoot := r.Method == http.MethodPatch || r.URL.Path == "/snapshot/create"
	if booted && !postBoot {
		fault(w, http.StatusBadRequest, "The requested operation is not supported after starting the microVM.")

		return
	}
	if !booted && postBoot {
		fault(w, http.StatusBadRequest, "The requested operation is not supported before starting the microVM.")

		return
	}
	refusal, err := f.apply(r.URL.Path, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	if refusal != "" {
		fault(w, http.StatusBadRequest, refusal)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apply takes one request the way firecracker would; a refusal is in its words, an error is the fake's own.
func (f *fake) apply(path string, body []byte) (string, error) {
	switch {
	case path == "/vm":
		return f.patchVM(body)
	case path == "/snapshot/create":
		return f.createSnapshot(body)
	case path == "/snapshot/load":
		return f.loadSnapshot(body)
	case path == "/machine-config":
		var m struct {
			VCPUs           int64 `json:"vcpu_count"`
			MemoryMiB       int64 `json:"mem_size_mib"`
			TrackDirtyPages bool  `json:"track_dirty_pages"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return "", err
		}
		if m.VCPUs < 1 || m.VCPUs > 32 {
			return "The vCPU number is invalid!", nil
		}
		if m.MemoryMiB < 1 {
			return "The memory size (MiB) is invalid.", nil
		}
		f.tracking = m.TrackDirtyPages
	case path == "/boot-source":
		f.boot = body
	case strings.HasPrefix(path, "/drives/"):
		// Firecracker opens every drive file when it is put, so a missing overlay is refused before the boot.
		var d struct {
			Path string `json:"path_on_host"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			return "", err
		}
		if _, err := os.Stat(inJail(d.Path)); err != nil {
			return "Unable to create the block device: " + err.Error(), nil
		}
		f.drives = append(f.drives, body)
	case path == "/vsock":
		var v struct {
			Path string `json:"uds_path"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return "", err
		}
		f.vsock = v.Path
	case path == "/actions":
		if f.boot == nil {
			return "Cannot start microvm without kernel configuration.", nil
		}
		if err := f.persist(); err != nil {
			return "", err
		}
		if err := f.start(); err != nil {
			return "", err
		}
		f.state = "Running"
	default:
		return "no route for " + path, nil
	}

	return "", nil
}

// patchVM stops or starts the vCPUs, which here is the guest process stopped or continued.
func (f *fake) patchVM(body []byte) (string, error) {
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	switch v.State {
	case "Paused":
		if err := f.execInFreeze(); err != nil {
			return "", err
		}
		f.state = "Paused"

		return "", f.signal(syscall.SIGSTOP)
	case "Resumed":
		if _, err := os.Stat(filepath.Join(f.dir, refuseResumeFile)); err == nil {
			return "Cannot resume microVM: refused by the test", nil
		}
		f.state = "Running"

		return "", f.signal(syscall.SIGCONT)
	}

	return "Invalid microVM state: " + v.State, nil
}

// createSnapshot writes what a load brings back, which without guest memory is the configuration the vmm holds.
func (f *fake) createSnapshot(body []byte) (string, error) {
	var c struct {
		Type       string `json:"snapshot_type"`
		StatePath  string `json:"snapshot_path"`
		MemoryPath string `json:"mem_file_path"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", err
	}
	if f.state != "Paused" {
		return "Cannot snapshot a running microVM.", nil
	}
	if _, err := os.Stat(filepath.Join(f.dir, refuseSnapshotFile)); err == nil {
		return "Cannot create snapshot: No space left on device", nil
	}
	if err := f.holdSnapshot(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(vmstate{Boot: f.boot, Drives: f.drives, Vsock: f.vsock, Frozen: f.frozen})
	if err != nil {
		return "", err
	}
	// 0o644 is what a real vmm writes under the daemon's shell umask, so a test proves the pause tightens it.
	if err := os.WriteFile(inJail(c.StatePath), encoded, 0o644); err != nil {
		return "", err
	}
	// The fake's memory is one line per snapshot, and a Diff adds its line to a file already there, as firecracker merges into one of the guest's size.
	memory := inJail(c.MemoryPath)
	found, err := os.ReadFile(memory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// A Diff without the log is noted apart, so a test that wants a Diff proves the boot or the load turned the log on.
	kind := c.Type
	if kind == "Diff" && !f.tracking {
		kind = "Resident Diff"
	}
	if err := note(filepath.Join(f.dir, snapshotsFile), fmt.Sprintf("%s onto %d", kind, strings.Count(string(found), "\n"))); err != nil {
		return "", err
	}
	if c.Type != "Diff" {
		found = nil
	}
	if err := os.WriteFile(memory, append(found, c.Type+"\n"...), 0o644); err != nil {
		return "", err
	}

	return "", f.reset()
}

// holdSnapshot keeps f.mu, and so every request, for as long as holdSnapshotFile stays.
func (f *fake) holdSnapshot() error {
	hold := filepath.Join(f.dir, holdSnapshotFile)
	_, err := os.Stat(hold)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.dir, heldSnapshotFile), []byte("held\n"), 0o600); err != nil {
		return err
	}
	for {
		_, err := os.Stat(hold)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reset kills the guest end of every stream, as the TRANSPORT_RESET of a snapshot create does; f.mu is held.
func (f *fake) reset() error {
	var errs []error
	for _, s := range f.streams {
		if s.guest == nil || s.reset.Swap(true) {
			continue
		}
		errs = append(errs, s.guest.Close())
	}
	_, err := os.Stat(filepath.Join(f.dir, severOnResetFile))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.Join(errs...)
	}
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	f.severed = true

	return errors.Join(errs...)
}

// execInFreeze starts the exec a test asked for in the guest, which the freeze before the pause holds.
func (f *fake) execInFreeze() error {
	path := filepath.Join(f.dir, execInFreezeFile)
	id, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	dial := func(ctx context.Context, port uint32) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "unix", filepath.Join(f.dir, "guest", fmt.Sprintf("%d.sock", port)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := "ran"
	if _, err := supervisor.Exec(ctx, dial, string(id), supervisor.ExecHeader{Argv: []string{"/bin/sh", "-c", "exit 0"}, WorkDir: "/"}, models.ExecSpec{}); err != nil {
		result = err.Error()
	}

	return os.WriteFile(filepath.Join(f.dir, execResultFile), []byte(result), 0o600)
}

// loadSnapshot brings a snapshot up in this fresh vmm: the state names the devices by their paths in the jail, and the overrides the tap and the vsock.
func (f *fake) loadSnapshot(body []byte) (string, error) {
	var l struct {
		StatePath string `json:"snapshot_path"`
		Memory    struct {
			Path string `json:"backend_path"`
		} `json:"mem_backend"`
		TrackDirtyPages bool `json:"track_dirty_pages"`
		ResumeVM        bool `json:"resume_vm"`
		Vsock           *struct {
			Path string `json:"uds_path"`
		} `json:"vsock_override"`
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return "", err
	}
	encoded, err := os.ReadFile(inJail(l.StatePath))
	if err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	var state vmstate
	if err := json.Unmarshal(encoded, &state); err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	if _, err := os.Stat(inJail(l.Memory.Path)); err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	// The load opens every drive the state names, in this vmm's own jail.
	for _, raw := range state.Drives {
		var d struct {
			Path string `json:"path_on_host"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return "", err
		}
		if _, err := os.Stat(inJail(d.Path)); err != nil {
			return "Load snapshot error: " + err.Error(), nil
		}
	}
	f.boot, f.drives, f.vsock, f.tracking = state.Boot, state.Drives, state.Vsock, l.TrackDirtyPages
	if l.Vsock != nil {
		f.vsock = l.Vsock.Path
	}
	if err := f.persist(); err != nil {
		return "", err
	}
	if err := f.start(); err != nil {
		return "", err
	}
	// The fresh guest has no memory of the freeze, so it is frozen again before any host attaches.
	if state.Frozen {
		if err := refreeze(filepath.Join(f.dir, "guest")); err != nil {
			return "", fmt.Errorf("freeze the restored guest: %w", err)
		}
	}
	f.frozen = state.Frozen
	f.state = "Running"
	if !l.ResumeVM {
		f.state = "Paused"

		return "", f.signal(syscall.SIGSTOP)
	}

	return "", nil
}

// signal reaches the guest, which stands in for the vCPUs; a vmm that booted nothing has none.
func (f *fake) signal(sig syscall.Signal) error {
	if f.cmd == nil {
		return nil
	}

	return f.cmd.Process.Signal(sig)
}

// boot is the bootFile: the boot source as put, and the drives in the order the guest sees them.
type boot struct {
	Source json.RawMessage   `json:"source"`
	Drives []json.RawMessage `json:"drives"`
}

// vmstate is the snapshot file: the devices the vmm held, which a load puts back before the overrides.
type vmstate struct {
	Boot   json.RawMessage   `json:"boot"`
	Drives []json.RawMessage `json:"drives"`
	Vsock  string            `json:"vsock"`
	Frozen bool              `json:"frozen"`
}

func (f *fake) persist() error {
	encoded, err := json.Marshal(boot{Source: f.boot, Drives: f.drives})
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(f.dir, bootFile), encoded, 0o600)
}

// start is the InstanceStart: the guest comes up behind the vsock proxy, its console on this process's stdout.
func (f *fake) start() error {
	dir := filepath.Join(f.dir, "guest")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	// The guest stays in the vmm's group, so the group kill the driver ends a vmm with takes the guest along, as a VM's death does.
	cmd := exec.Command(os.Getenv(fakeInitEnv), "-transport", "unix:"+dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shard-init: %w", err)
	}
	f.cmd = cmd
	go func() {
		// The exit is the guest powering off, which ends firecracker; the group kill takes an entrypoint that ignored TERM along.
		_ = cmd.Wait()
		f.drain()
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}()

	if f.vsock == "" {
		return nil
	}
	listener, err := net.Listen("unix", inJail(f.vsock))
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.proxy(conn, dir)
		}
	}()

	return nil
}

// refreeze freezes the guest under dir over a control connection of its own, which it drops before the host dials.
func refreeze(dir string) error {
	socket := filepath.Join(dir, fmt.Sprintf("%d.sock", supervisor.ControlPort))
	conn, err := net.Dial("unix", socket)
	for deadline := time.Now().Add(5 * time.Second); err != nil && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		conn, err = net.Dial("unix", socket)
	}
	if err != nil {
		return err
	}
	control := supervisor.ControlOver(conn)
	if _, err := control.Next(); err != nil {
		return errors.Join(err, control.Close())
	}

	return errors.Join(control.Freeze(context.Background(), models.VerbPause), control.Close())
}

// stop is what a signal does to firecracker: the VM is gone with it, which here is the guest killed; false when none was started.
func (f *fake) stop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmd == nil {
		return false
	}
	_ = f.cmd.Process.Kill()

	return true
}

// drop ends every stream through the vsock device, as a transport reset would, and with severed refuses every one after.
func (f *fake) drop(severed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.severed = severed
	for conn := range f.streams {
		_ = conn.Close()
	}
}

// stream is one host connection through the vsock device, and the guest end it was put through to.
type stream struct {
	guest net.Conn
	// reset is a snapshot create that killed the guest end, after which the host end closes only once the host writes, as firecracker's does.
	reset atomic.Bool
}

// hold registers a stream for drop, or nil once the transport carries none.
func (f *fake) hold(conn net.Conn) *stream {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.severed {
		return nil
	}
	s := &stream{}
	f.streams[conn] = s

	return s
}

// through puts a stream through to its guest end, which a reset from then on kills.
func (f *fake) through(s *stream, guest net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.guest = guest
}

func (f *fake) let(conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.streams, conn)
}

// floodEveryFile in the state directory floods every control stream past its state line, for as long as it stays there.
const floodEveryFile = "flood-every-control"

// dialsFile in the state directory, once a test creates it, takes one line per control stream the host dials.
const dialsFile = "control-dials"

// proxy is one host connection through the vsock device: CONNECT <port> in, OK back, and then the guest's own stream.
func (f *fake) proxy(conn net.Conn, dir string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT ")))
	if err != nil {
		return
	}
	s := f.hold(conn)
	if s == nil {
		return
	}
	defer f.let(conn)
	// Nothing listening in the guest ends the connection without a word, as firecracker does.
	guest, err := net.Dial("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	if err != nil {
		return
	}
	defer guest.Close()
	f.through(s, guest)
	if _, err := fmt.Fprintf(conn, "OK %d\n", port); err != nil {
		return
	}
	answers, err := f.answers(port, guest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)

		return
	}

	var toGuest, toHost io.Writer = guest, conn
	if port == int(supervisor.ControlPort) {
		if err := note(filepath.Join(f.dir, controlsFile), "attach"); err != nil {
			return
		}
		c := &control{f: f, dir: f.dir, guest: guest, host: conn}
		toGuest, toHost = writeFunc(c.intoGuest), writeFunc(c.intoHost)
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(toGuest, reader)
		closeWrite(guest)
		done <- struct{}{}
	}()
	sent := make(chan struct{})
	f.mu.Lock()
	f.sending[sent] = struct{}{}
	f.mu.Unlock()
	go func() {
		_, _ = io.Copy(toHost, answers)
		// A reset leaves the host end open, so the host hears of it on its next write only, as firecracker's muxer does.
		if !s.reset.Load() {
			closeWrite(conn)
		}
		f.mu.Lock()
		delete(f.sending, sent)
		f.mu.Unlock()
		close(sent)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// drain lets what a guest wrote before it powered off reach the host, as the vsock device delivers it before firecracker exits.
func (f *fake) drain() {
	f.mu.Lock()
	pending := make([]chan struct{}, 0, len(f.sending))
	for sent := range f.sending {
		pending = append(pending, sent)
	}
	f.mu.Unlock()
	deadline := time.After(time.Second)
	for _, sent := range pending {
		select {
		case <-sent:
		case <-deadline:
			return
		}
	}
}

// writeFunc lets a method stand in for one direction of a stream.
type writeFunc func([]byte) (int, error)

func (w writeFunc) Write(p []byte) (int, error) { return w(p) }

// control is one control stream through the vsock device: it notes what the guest reads, and plays the freeze faults a test asks for.
type control struct {
	f     *fake
	dir   string
	guest io.Writer
	host  io.Writer
	// losing is a freeze passed to the guest whose answer the drop takes instead of the host.
	losing atomic.Bool
}

func (c *control) intoGuest(p []byte) (int, error) {
	for _, kind := range []string{supervisor.KindReseed, supervisor.KindFreeze, supervisor.KindThaw} {
		if !carries(p, kind) {
			continue
		}
		if err := note(filepath.Join(c.dir, controlsFile), kind); err != nil {
			return 0, err
		}
	}
	if carries(p, supervisor.KindThaw) || carries(p, supervisor.KindStop) {
		c.f.freeze(false)
	}
	if _, err := os.Stat(filepath.Join(c.dir, refuseReseedFile)); err == nil && carries(p, supervisor.KindReseed) {
		return c.refuse(p, supervisor.KindReseed)
	}
	if !carries(p, supervisor.KindFreeze) {
		return c.guest.Write(p)
	}
	if _, err := os.Stat(filepath.Join(c.dir, refuseFreezeFile)); err == nil {
		return c.refuse(p, supervisor.KindFreeze)
	}
	err := os.Remove(filepath.Join(c.dir, loseFreezeFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	c.losing.Store(err == nil)
	c.f.freeze(true)

	return c.guest.Write(p)
}

// refuse hands the guest a kind it does not take, which draws a failure on the request's id.
func (c *control) refuse(p []byte, kind string) (int, error) {
	if _, err := io.WriteString(c.guest, strings.Replace(string(p), `"kind":"`+kind+`"`, `"kind":"refused-`+kind+`"`, 1)); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *control) intoHost(p []byte) (int, error) {
	if c.losing.Load() && carries(p, supervisor.KindDone) {
		c.f.drop(false)

		return 0, errors.New("the drop took the answer to the freeze")
	}
	if _, err := os.Stat(filepath.Join(c.dir, oldGuestFile)); err == nil && carries(p, supervisor.KindState) {
		if _, err := io.WriteString(c.host, strings.Replace(string(p), `,"freezes_overlay":true`, "", 1)); err != nil {
			return 0, err
		}

		return len(p), nil
	}

	return c.host.Write(p)
}

func (f *fake) freeze(frozen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen = frozen
}

func carries(p []byte, kind string) bool {
	return strings.Contains(string(p), `"kind":"`+kind+`"`)
}

func note(path, kind string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(kind + "\n")

	return errors.Join(err, f.Close())
}

// answers is what the host reads of one guest stream: the guest itself, or a flood past its state line while a test asks for one.
func (f *fake) answers(port int, guest net.Conn) (io.Reader, error) {
	if port != int(supervisor.ControlPort) {
		return guest, nil
	}
	if err := note(filepath.Join(f.dir, dialsFile), "control"); err != nil {
		return nil, err
	}
	_, err := os.Stat(filepath.Join(f.dir, floodEveryFile))
	if errors.Is(err, fs.ErrNotExist) {
		return guest, nil
	}
	if err != nil {
		return nil, err
	}

	return &flooded{guest: guest}, nil
}

// flooded passes the guest's state line, then reads as one line that never ends.
type flooded struct {
	guest  io.Reader
	passed bool
}

func (f *flooded) Read(p []byte) (int, error) {
	if f.passed {
		return copy(p, bytes.Repeat([]byte{'x'}, len(p))), nil
	}
	n, err := f.guest.Read(p)
	if end := bytes.IndexByte(p[:n], '\n'); end >= 0 {
		f.passed = true

		return end + 1, err
	}

	return n, err
}

// closeWrite passes a half-close through, so a guest that reads to EOF sees the host's, and the host the guest's.
func closeWrite(conn net.Conn) {
	if unixConn, ok := conn.(*net.UnixConn); ok {
		_ = unixConn.CloseWrite()
	}
}

func fault(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": message})
}
