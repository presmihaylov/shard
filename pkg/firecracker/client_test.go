package firecracker_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/firecracker"
)

// shortRoot is a directory a unix socket path fits under: t.TempDir is too long for one.
func shortRoot(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "fc") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	return root
}

// jail puts the fake vmm in a chroot under root, which the fake jailer names the way the real one does.
func jail(root, id string) firecracker.Jail {
	return firecracker.Jail{Jailer: os.Args[0], Exec: os.Args[0], ID: id, UID: 1879048192, Base: root, Cgroup: "shard/" + id}
}

func config(root string) firecracker.Config {
	return firecracker.Config{
		Kernel:    "/vmlinux",
		Initrd:    "/initrd",
		Cmdline:   "console=ttyS0 -- -transport vsock",
		VCPUs:     2,
		MemoryMiB: 256,
		Drives: []firecracker.Drive{
			{ID: "base", Path: "/base.erofs", ReadOnly: true},
			{ID: "overlay", Path: "/overlay.raw"},
		},
		Network: firecracker.Network{Tap: "shardv2", MAC: "02:fc:0a:57:00:02"},
		Vsock:   "/v.sock",
		Socket:  "/api.sock",
		Console: filepath.Join(root, "console.log"),
	}
}

// start boots the fake and kills it after the test, so no fake outlives its root.
func start(t *testing.T, j firecracker.Jail, cfg firecracker.Config) (*firecracker.Client, firecracker.Info) {
	t.Helper()

	client, info, err := firecracker.Start(t.Context(), j, cfg)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Kill(); err != nil {
			t.Errorf("Kill = %v", err)
		}
	})

	return client, info
}

func readSeen(t *testing.T, j firecracker.Jail, socket string) seen {
	t.Helper()

	blob, err := os.ReadFile(j.Host(socket) + ".seen.json")
	if err != nil {
		t.Fatal(err)
	}
	var s seen
	if err := json.Unmarshal(blob, &s); err != nil {
		t.Fatal(err)
	}

	return s
}

func TestStartPutsTheMachineInThenBootsIt(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "otter-1a2b"), config(root)
	j.Netns = "/var/run/netns/otter-1a2b"
	_, info := start(t, j, cfg)

	if info.State != firecracker.StateRunning {
		t.Fatalf("Start reported %q, want %q", info.State, firecracker.StateRunning)
	}
	pidBlob, err := os.ReadFile(j.Host(cfg.Socket) + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := strconv.Atoi(string(pidBlob)); info.PID != want {
		t.Fatalf("Start reported pid %d, want the vmm's own %d", info.PID, want)
	}
	argsBlob, err := os.ReadFile(filepath.Join(filepath.Dir(j.Root()), "jailer.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := `{"id":"otter-1a2b","uid":1879048192,"gid":1879048192,"cgroupVersion":"2","parentCgroup":"shard/otter-1a2b",` +
		`"newPidNS":true,"netns":"/var/run/netns/otter-1a2b","limits":null,"vmm":["--api-sock","/api.sock"]}`
	if string(argsBlob) != wantArgs {
		t.Fatalf("the jailer ran with %s, want %s", argsBlob, wantArgs)
	}

	s := readSeen(t, j, cfg.Socket)
	wantCalls := []string{
		"PUT /machine-config", "PUT /boot-source", "PUT /drives/base", "PUT /drives/overlay", "PUT /network-interfaces/eth0", "PUT /vsock", "PUT /actions",
	}
	var puts []string
	for _, call := range s.Calls {
		if strings.HasPrefix(call, "PUT ") {
			puts = append(puts, call)
		}
	}
	if strings.Join(puts, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("the vmm was told %q, want %q", puts, wantCalls)
	}
	for name, got := range map[string]string{
		"machine": string(s.Machine),
		"boot":    string(s.Boot),
		"base":    string(s.Drives[0]),
		"overlay": string(s.Drives[1]),
		"network": string(s.Network),
		"vsock":   string(s.Vsock),
	} {
		want := map[string]string{
			"machine": `{"vcpu_count":2,"mem_size_mib":256}`,
			"boot":    `{"kernel_image_path":"/vmlinux","initrd_path":"/initrd","boot_args":"console=ttyS0 -- -transport vsock"}`,
			"base":    `{"drive_id":"base","path_on_host":"/base.erofs","is_root_device":false,"is_read_only":true}`,
			"overlay": `{"drive_id":"overlay","path_on_host":"/overlay.raw","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`,
			"network": `{"iface_id":"eth0","host_dev_name":"shardv2","guest_mac":"02:fc:0a:57:00:02"}`,
			"vsock":   `{"guest_cid":3,"uds_path":"/v.sock"}`,
		}[name]
		if got != want {
			t.Fatalf("the %s put = %s, want %s", name, got, want)
		}
	}
}

func TestConnectHandsBackTheGuestStreamAfterTheHandshake(t *testing.T) {
	root := shortRoot(t)
	client, _ := start(t, jail(root, "a"), config(root))

	conn, err := client.Connect(echoPort)
	if err != nil {
		t.Fatalf("Connect(%d) = %v", echoPort, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("the guest echoed %q, %v; want ping", line, err)
	}

	// Nothing listens on the other port, so the vmm ends the connection instead of answering.
	_, err = client.Connect(echoPort + 1)
	if err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("Connect to a port nobody listens on = %v, want the refusal named", err)
	}
}

func TestAdoptFindsTheRunningVmmAndKillEndsIt(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	client, info := start(t, j, cfg)

	adopted, again, err := firecracker.Adopt(t.Context(), j.Host(cfg.Socket), j.Host(cfg.Vsock))
	if err != nil {
		t.Fatalf("Adopt = %v", err)
	}
	if again != info {
		t.Fatalf("Adopt reported %+v, want %+v", again, info)
	}
	if _, err := adopted.Connect(echoPort); err != nil {
		t.Fatalf("Connect over the adopted client = %v", err)
	}

	if err := client.Kill(); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	awaitRefused(t, client)
	// A second kill finds nobody, and says nothing of it.
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill of an ended vmm = %v, want nil", err)
	}
}

// An adopt that a vmm takes and never answers hands back a pin on that peer, which ends it, and an adopt it answers holds no pin past its return (SHARD-392).
func TestAdoptPinnedHoldsAVmmSilentToTheDeadline(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	client, info := start(t, j, cfg)

	_, answered, pin, err := firecracker.AdoptPinned(t.Context(), j.Host(cfg.Socket), j.Host(cfg.Vsock))
	if err != nil || pin != nil {
		t.Fatalf("AdoptPinned of an answering vmm = pin %v, %v; want no pin and no error", pin, err)
	}
	if answered != info {
		t.Fatalf("AdoptPinned reported %+v, want %+v", answered, info)
	}
	freeze(t, info.PID)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, silent, pin, err := firecracker.AdoptPinned(ctx, j.Host(cfg.Socket), j.Host(cfg.Vsock))
	if !errors.Is(err, os.ErrDeadlineExceeded) || pin == nil {
		t.Fatalf("AdoptPinned of a stopped vmm = pin %v, %v; want a pin and the deadline", pin, err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	if silent.PID != info.PID || pin.PID() != info.PID {
		t.Fatalf("the silent adopt named pid %d and pinned %d, want the peer %d", silent.PID, pin.PID(), info.PID)
	}

	if err := pin.Kill(); err != nil {
		t.Fatalf("Kill through the pin = %v", err)
	}
	awaitRefused(t, client)
}

func awaitRefused(t *testing.T, client *firecracker.Client) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := client.State(t.Context())
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("State after Kill = %v, want the socket refused", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartRefusesASocketALiveVmmAnswersOn(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	_, info := start(t, j, cfg)

	_, _, err := firecracker.Start(t.Context(), j, cfg)
	if !errors.Is(err, firecracker.ErrSocketInUse) || !strings.Contains(err.Error(), strconv.Itoa(info.PID)) {
		t.Fatalf("a second Start = %v, want ErrSocketInUse naming pid %d", err, info.PID)
	}
}

func TestStartReplacesThePathsADeadVmmLeft(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	if err := os.MkdirAll(j.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{j.Host(cfg.Socket), j.Host(cfg.Vsock)} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		unixListener, ok := listener.(*net.UnixListener)
		if !ok {
			t.Fatalf("a %T is not a unix listener", listener)
		}
		unixListener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}

	start(t, j, cfg)
}

func TestStartReportsAVmmThatDiesWithItsConsole(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(fakeDieEnv, "KVM is not available")

	_, _, err := firecracker.Start(t.Context(), jail(root, "a"), config(root))
	if err == nil || !strings.Contains(err.Error(), "exited before its api answered") || !strings.Contains(err.Error(), "KVM is not available") {
		t.Fatalf("Start of a dying vmm = %v, want the exit and the console named", err)
	}
}

func TestStartReportsAJailerRefusalWithItsWords(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(fakeJailerDieEnv, "Failed to create a new pid namespace")

	_, _, err := firecracker.Start(t.Context(), jail(root, "a"), config(root))
	if err == nil || !strings.Contains(err.Error(), "the jailer exited") || !strings.Contains(err.Error(), "Failed to create a new pid namespace") {
		t.Fatalf("Start with a refusing jailer = %v, want the exit and its words named", err)
	}
}

func TestARefusalCarriesTheVmmsOwnWordsAndEndsIt(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	cfg.VCPUs = 0

	_, _, err := firecracker.Start(t.Context(), j, cfg)
	if err == nil || !strings.Contains(err.Error(), "PUT /machine-config") || !strings.Contains(err.Error(), "vCPU number is invalid") {
		t.Fatalf("Start with 0 vcpus = %v, want the call and the fault named", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, err := firecracker.Adopt(t.Context(), j.Host(cfg.Socket), j.Host(cfg.Vsock))
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused vmm still answers: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// snapshot pauses the fake and writes its snapshot at the root of its jail.
func snapshot(t *testing.T, client *firecracker.Client) (string, string) {
	t.Helper()

	if err := client.Pause(); err != nil {
		t.Fatalf("Pause = %v", err)
	}
	state, memory := "/vmstate", "/memory"
	if err := client.Snapshot(state, memory); err != nil {
		t.Fatalf("Snapshot = %v", err)
	}

	return state, memory
}

func TestPauseStopsTheVCPUsAndResumeStartsThem(t *testing.T) {
	root := shortRoot(t)
	client, _ := start(t, jail(root, "a"), config(root))

	if err := client.Pause(); err != nil {
		t.Fatalf("Pause = %v", err)
	}
	if info, err := client.State(t.Context()); err != nil || info.State != firecracker.StatePaused {
		t.Fatalf("State after Pause = %+v, %v; want %q", info, err, firecracker.StatePaused)
	}
	if err := client.Resume(); err != nil {
		t.Fatalf("Resume = %v", err)
	}
	if info, err := client.State(t.Context()); err != nil || info.State != firecracker.StateRunning {
		t.Fatalf("State after Resume = %+v, %v; want %q", info, err, firecracker.StateRunning)
	}
}

func TestSnapshotWritesTheStateAndTheMemoryOfAPausedMicroVM(t *testing.T) {
	root := shortRoot(t)
	j, cfg := jail(root, "a"), config(root)
	client, _ := start(t, j, cfg)

	err := client.Snapshot("/vmstate", "/memory")
	if err == nil || !strings.Contains(err.Error(), "PUT /snapshot/create") {
		t.Fatalf("Snapshot of a running microVM = %v, want the refusal named", err)
	}

	state, memory := snapshot(t, client)
	for _, path := range []string{state, memory} {
		if _, err := os.Stat(j.Host(path)); err != nil {
			t.Fatalf("Snapshot left no %s in the jail: %v", path, err)
		}
	}
	want := `{"snapshot_type":"Diff","snapshot_path":"/vmstate","mem_file_path":"/memory"}`
	if got := string(readSeen(t, j, cfg.Socket).Snapshot); got != want {
		t.Fatalf("the snapshot put = %s, want %s", got, want)
	}
}

func TestRestoreBringsTheSnapshotUpInAFreshVmmWithItsOwnTapAndVsock(t *testing.T) {
	root := shortRoot(t)
	source, cfg := jail(root, "a"), config(root)
	sourceClient, _ := start(t, source, cfg)
	state, memory := snapshot(t, sourceClient)

	fresh := jail(root, "b")
	if err := os.MkdirAll(fresh.Root(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{state, memory} {
		blob, err := os.ReadFile(source.Host(path))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fresh.Host(path), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap := firecracker.Snapshot{State: state, Memory: memory, Tap: "shardv3", Vsock: "/v.sock", Socket: "/api.sock", Console: filepath.Join(root, "b.log")}
	fork, info, err := firecracker.Restore(t.Context(), fresh, snap)
	if err != nil {
		t.Fatalf("Restore = %v", err)
	}
	t.Cleanup(func() {
		if err := fork.Kill(); err != nil {
			t.Errorf("Kill = %v", err)
		}
	})
	if info.State != firecracker.StateRunning {
		t.Fatalf("Restore reported %q, want %q", info.State, firecracker.StateRunning)
	}

	s := readSeen(t, fresh, snap.Socket)
	var calls []string
	for _, call := range s.Calls {
		if !strings.HasPrefix(call, "GET ") {
			calls = append(calls, call)
		}
	}
	wantCalls := []string{"PUT /snapshot/load", "PATCH /vm"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("the fresh vmm was told %q, want %q", calls, wantCalls)
	}
	wantLoad := `{"snapshot_path":"/vmstate","mem_backend":{"backend_type":"File","backend_path":"/memory"},"resume_vm":false,` +
		`"network_overrides":[{"iface_id":"eth0","host_dev_name":"shardv3"}],"vsock_override":{"uds_path":"/v.sock"},"clock_realtime":true}`
	if string(s.Load) != wantLoad {
		t.Fatalf("the load put = %s, want %s", s.Load, wantLoad)
	}
	// Each drive keeps the in-jail path it booted with, which names the fresh jail's own file.
	for name, got := range map[string]string{
		"network": string(s.Network),
		"vsock":   string(s.Vsock),
		"base":    string(s.Drives[0]),
		"overlay": string(s.Drives[1]),
	} {
		want := map[string]string{
			"network": `{"guest_mac":"02:fc:0a:57:00:02","host_dev_name":"shardv3","iface_id":"eth0"}`,
			"vsock":   `{"guest_cid":3,"uds_path":"/v.sock"}`,
			"base":    `{"drive_id":"base","path_on_host":"/base.erofs","is_root_device":false,"is_read_only":true}`,
			"overlay": `{"drive_id":"overlay","path_on_host":"/overlay.raw","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`,
		}[name]
		if got != want {
			t.Fatalf("the restored %s = %s, want %s", name, got, want)
		}
	}

	// The vsock proxy answers in the fresh jail, and the source still stands, paused, in its own.
	if _, err := fork.Connect(echoPort); err != nil {
		t.Fatalf("Connect over the restored vmm = %v", err)
	}
	if info, err := sourceClient.State(t.Context()); err != nil || info.State != firecracker.StatePaused {
		t.Fatalf("the source after the restore = %+v, %v; want still %q", info, err, firecracker.StatePaused)
	}
}

func TestRestoreReportsARefusedLoadAndEndsTheVmm(t *testing.T) {
	root := shortRoot(t)
	j := jail(root, "a")
	snap := firecracker.Snapshot{State: "/vmstate", Memory: "/memory", Vsock: "/v.sock", Socket: "/api.sock", Console: filepath.Join(root, "console.log")}

	_, _, err := firecracker.Restore(t.Context(), j, snap)
	if err == nil || !strings.Contains(err.Error(), "PUT /snapshot/load") || !strings.Contains(err.Error(), "Load snapshot error") {
		t.Fatalf("Restore without a snapshot = %v, want the call and the fault named", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, err := firecracker.Adopt(t.Context(), j.Host(snap.Socket), j.Host(snap.Vsock))
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused vmm still answers: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// freeze returns once the kernel reports every thread of the vmm stopped: SIGSTOP lands on each thread on its own (SHARD-436), and the vmm is no child to wait for.
func freeze(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		done, err := stopped(pid)
		if err != nil {
			t.Fatalf("read the state of the vmm: %v", err)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the vmm %d did not stop within 5s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestKillEndsAVmmTooWedgedToAnswer is SHARD-339: a stopped vmm takes the dial and never the call, so the kill must not wait for an answer.
func TestKillEndsAVmmTooWedgedToAnswer(t *testing.T) {
	root := shortRoot(t)
	client, info := start(t, jail(root, "a"), config(root))
	freeze(t, info.PID)

	begun := time.Now()
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill of a stopped vmm = %v", err)
	}
	if took := time.Since(begun); took > 5*time.Second {
		t.Errorf("Kill took %s, want it done at the dial", took)
	}
	awaitRefused(t, client)
}

// A state read ends by its context's deadline and names the peer it waited on, so a kill reaches that vmm and no owner since (SHARD-388, SHARD-392).
func TestStateEndsByItsDeadlineOnAVmmThatNeverAnswers(t *testing.T) {
	root := shortRoot(t)
	client, info := start(t, jail(root, "a"), config(root))
	freeze(t, info.PID)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	begun := time.Now()
	got, err := client.State(ctx)
	if err == nil {
		t.Fatal("State of a stopped vmm answered")
	}
	if took := time.Since(begun); took > 5*time.Second {
		t.Errorf("State took %s on a deadline of 200ms", took)
	}
	if got.PID != info.PID {
		t.Errorf("State timed out naming pid %d, want the peer %d", got.PID, info.PID)
	}
}
