package firecracker_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// The test binary plays firecracker when the provider execs it with this set; the guest is the real shard-init over unix sockets.
const (
	fakeVMMEnv  = "FIRECRACKER_FAKE_VMM"
	fakeInitEnv = "FIRECRACKER_FAKE_INIT"
)

// initBinary is the shard-init the fake vmm runs in place of a VM, built once per test run unless the env names one.
var initBinary string

func TestMain(m *testing.M) {
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

// fakeVMM is firecracker without KVM: its API on --api-sock, one shard-init process as the guest, and the vsock proxy in front of it.
func fakeVMM() error {
	flags := flag.NewFlagSet("fake-firecracker", flag.ContinueOnError)
	socket := flags.String("api-sock", "", "")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	fmt.Println("fake firecracker: api on", *socket)

	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	f := &fake{socket: *socket, state: "Not started", streams: map[net.Conn]struct{}{}}
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
	socket string

	mu    sync.Mutex
	state string
	boot  json.RawMessage
	vsock string
	cmd   *exec.Cmd
	// drives is every drive put, in order, which with the boot args is what a test reads back from bootFile.
	drives []json.RawMessage
	// streams is every host connection through the vsock device, so a drop can end them all; severed refuses the ones after it.
	streams map[net.Conn]struct{}
	severed bool
}

// bootFile is written beside the api socket at the start, with what the vmm was told to boot.
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
	refusal, err := f.apply(r.Method, r.URL.Path, body)
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
func (f *fake) apply(method, path string, body []byte) (string, error) {
	switch {
	case path == "/vm":
		return f.patchVM(body)
	case path == "/snapshot/create":
		return f.createSnapshot(body)
	case path == "/snapshot/load":
		return f.loadSnapshot(body)
	case strings.HasPrefix(path, "/drives/") && method == http.MethodPatch:
		return f.patchDrive(body)
	case path == "/machine-config":
		var m struct {
			VCPUs     int64 `json:"vcpu_count"`
			MemoryMiB int64 `json:"mem_size_mib"`
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
		if _, err := os.Stat(d.Path); err != nil {
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
		f.state = "Paused"

		return "", f.signal(syscall.SIGSTOP)
	case "Resumed":
		f.state = "Running"

		return "", f.signal(syscall.SIGCONT)
	}

	return "Invalid microVM state: " + v.State, nil
}

// createSnapshot writes what a load brings back, which without guest memory is the configuration the vmm holds.
func (f *fake) createSnapshot(body []byte) (string, error) {
	var c struct {
		StatePath  string `json:"snapshot_path"`
		MemoryPath string `json:"mem_file_path"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", err
	}
	if f.state != "Paused" {
		return "Cannot snapshot a running microVM.", nil
	}
	encoded, err := json.Marshal(vmstate{Boot: f.boot, Drives: f.drives, Vsock: f.vsock})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(c.StatePath, encoded, 0o600); err != nil {
		return "", err
	}
	// The fake has no guest memory, so the file is a blob: what a restore links to and the tests count the links of.
	return "", os.WriteFile(c.MemoryPath, []byte("fake guest memory\n"), 0o600)
}

// loadSnapshot brings a snapshot up in this fresh vmm: the state names the devices, and the overrides the host paths of this one.
func (f *fake) loadSnapshot(body []byte) (string, error) {
	var l struct {
		StatePath string `json:"snapshot_path"`
		Memory    struct {
			Path string `json:"backend_path"`
		} `json:"mem_backend"`
		ResumeVM bool `json:"resume_vm"`
		Vsock    *struct {
			Path string `json:"uds_path"`
		} `json:"vsock_override"`
	}
	if err := json.Unmarshal(body, &l); err != nil {
		return "", err
	}
	encoded, err := os.ReadFile(l.StatePath)
	if err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	var state vmstate
	if err := json.Unmarshal(encoded, &state); err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	if _, err := os.Stat(l.Memory.Path); err != nil {
		return "Load snapshot error: " + err.Error(), nil
	}
	// The load opens every drive the state names, which are the paths of the sandbox the snapshot was taken from.
	for _, raw := range state.Drives {
		var d struct {
			Path string `json:"path_on_host"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return "", err
		}
		if _, err := os.Stat(d.Path); err != nil {
			return "Load snapshot error: " + err.Error(), nil
		}
	}
	f.boot, f.drives, f.vsock = state.Boot, state.Drives, state.Vsock
	if l.Vsock != nil {
		f.vsock = l.Vsock.Path
	}
	if err := f.persist(); err != nil {
		return "", err
	}
	if err := f.start(); err != nil {
		return "", err
	}
	f.state = "Running"
	if !l.ResumeVM {
		f.state = "Paused"

		return "", f.signal(syscall.SIGSTOP)
	}

	return "", nil
}

// patchDrive points a drive the guest already has at another host file, which is how a restore takes its own overlay.
func (f *fake) patchDrive(body []byte) (string, error) {
	var patch struct {
		ID   string `json:"drive_id"`
		Path string `json:"path_on_host"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		return "", err
	}
	if _, err := os.Stat(patch.Path); err != nil {
		return "Unable to patch the block device: " + err.Error(), nil
	}
	for i, raw := range f.drives {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return "", err
		}
		if fields["drive_id"] != patch.ID {
			continue
		}
		fields["path_on_host"] = patch.Path
		swapped, err := json.Marshal(fields)
		if err != nil {
			return "", err
		}
		f.drives[i] = swapped

		return "", f.persist()
	}

	return "Invalid block device ID: " + patch.ID, nil
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
}

func (f *fake) persist() error {
	encoded, err := json.Marshal(boot{Source: f.boot, Drives: f.drives})
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(filepath.Dir(f.socket), bootFile), encoded, 0o600)
}

// start is the InstanceStart: the guest comes up behind the vsock proxy, its console on this process's stdout.
func (f *fake) start() error {
	dir := filepath.Join(filepath.Dir(f.socket), "guest")
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
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}()

	if f.vsock == "" {
		return nil
	}
	listener, err := net.Listen("unix", f.vsock)
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

// hold registers a stream for drop, and says whether the transport still carries any.
func (f *fake) hold(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.severed {
		return false
	}
	f.streams[conn] = struct{}{}

	return true
}

func (f *fake) let(conn net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.streams, conn)
}

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
	if !f.hold(conn) {
		return
	}
	defer f.let(conn)
	// Nothing listening in the guest ends the connection without a word, as firecracker does.
	guest, err := net.Dial("unix", filepath.Join(dir, fmt.Sprintf("%d.sock", port)))
	if err != nil {
		return
	}
	defer guest.Close()
	if _, err := fmt.Fprintf(conn, "OK %d\n", port); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(guest, reader)
		closeWrite(guest)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, guest)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
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
