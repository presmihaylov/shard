package firecracker_test

import (
	"bufio"
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

func config(root string) firecracker.Config {
	return firecracker.Config{
		Kernel:    "/kernels/vmlinux",
		Initrd:    "/kernels/initrd.cpio",
		Cmdline:   "console=ttyS0 -- -transport vsock",
		VCPUs:     2,
		MemoryMiB: 256,
		Drives: []firecracker.Drive{
			{ID: "base", Path: "/images/base.erofs", ReadOnly: true},
			{ID: "overlay", Path: filepath.Join(root, "overlay.raw")},
		},
		Network: firecracker.Network{Tap: "shardv2", MAC: "02:fc:0a:57:00:02"},
		Vsock:   filepath.Join(root, "vsock.sock"),
		Socket:  filepath.Join(root, "firecracker.sock"),
		Console: filepath.Join(root, "console.log"),
	}
}

// start boots the fake and kills it after the test, so no fake outlives its root.
func start(t *testing.T, cfg firecracker.Config) (*firecracker.Client, firecracker.Info) {
	t.Helper()

	client, info, err := firecracker.Start(t.Context(), os.Args[0], cfg)
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

func readSeen(t *testing.T, cfg firecracker.Config) seen {
	t.Helper()

	blob, err := os.ReadFile(cfg.Socket + ".seen.json")
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
	cfg := config(root)
	_, info := start(t, cfg)

	if info.State != firecracker.StateRunning {
		t.Fatalf("Start reported %q, want %q", info.State, firecracker.StateRunning)
	}
	pidBlob, err := os.ReadFile(cfg.Socket + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := strconv.Atoi(string(pidBlob)); info.PID != want {
		t.Fatalf("Start reported pid %d, want the vmm's own %d", info.PID, want)
	}

	s := readSeen(t, cfg)
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
			"boot":    `{"kernel_image_path":"/kernels/vmlinux","initrd_path":"/kernels/initrd.cpio","boot_args":"console=ttyS0 -- -transport vsock"}`,
			"base":    `{"drive_id":"base","path_on_host":"/images/base.erofs","is_root_device":false,"is_read_only":true}`,
			"overlay": `{"drive_id":"overlay","path_on_host":"` + filepath.Join(root, "overlay.raw") + `","is_root_device":false,"is_read_only":false}`,
			"network": `{"iface_id":"eth0","host_dev_name":"shardv2","guest_mac":"02:fc:0a:57:00:02"}`,
			"vsock":   `{"guest_cid":3,"uds_path":"` + cfg.Vsock + `"}`,
		}[name]
		if got != want {
			t.Fatalf("the %s put = %s, want %s", name, got, want)
		}
	}
}

func TestConnectHandsBackTheGuestStreamAfterTheHandshake(t *testing.T) {
	root := shortRoot(t)
	client, _ := start(t, config(root))

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
	cfg := config(root)
	client, info := start(t, cfg)

	adopted, again, err := firecracker.Adopt(cfg.Socket, cfg.Vsock)
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := client.State()
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("State after Kill = %v, want the socket refused", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A second kill finds nobody, and says nothing of it.
	if err := client.Kill(); err != nil {
		t.Fatalf("Kill of an ended vmm = %v, want nil", err)
	}
}

func TestStartRefusesASocketALiveVmmAnswersOn(t *testing.T) {
	root := shortRoot(t)
	cfg := config(root)
	_, info := start(t, cfg)

	_, _, err := firecracker.Start(t.Context(), os.Args[0], cfg)
	if !errors.Is(err, firecracker.ErrSocketInUse) || !strings.Contains(err.Error(), strconv.Itoa(info.PID)) {
		t.Fatalf("a second Start = %v, want ErrSocketInUse naming pid %d", err, info.PID)
	}
}

func TestStartReplacesThePathsADeadVmmLeft(t *testing.T) {
	root := shortRoot(t)
	cfg := config(root)
	for _, path := range []string{cfg.Socket, cfg.Vsock} {
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

	start(t, cfg)
}

func TestStartReportsAVmmThatDiesWithItsConsole(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(fakeDieEnv, "KVM is not available")

	_, _, err := firecracker.Start(t.Context(), os.Args[0], config(root))
	if err == nil || !strings.Contains(err.Error(), "exited before") || !strings.Contains(err.Error(), "KVM is not available") {
		t.Fatalf("Start of a dying vmm = %v, want the exit and the console named", err)
	}
}

func TestARefusalCarriesTheVmmsOwnWordsAndEndsIt(t *testing.T) {
	root := shortRoot(t)
	cfg := config(root)
	cfg.VCPUs = 0

	_, _, err := firecracker.Start(t.Context(), os.Args[0], cfg)
	if err == nil || !strings.Contains(err.Error(), "PUT /machine-config") || !strings.Contains(err.Error(), "vCPU number is invalid") {
		t.Fatalf("Start with 0 vcpus = %v, want the call and the fault named", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, err := firecracker.Adopt(cfg.Socket, cfg.Vsock)
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused vmm still answers: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// resetting is a socket whose owner ends every connection unanswered, as a vmm mid-exit does; gone closes the listener after the first.
func resetting(t *testing.T, socket string, gone bool) {
	t.Helper()

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
			if gone {
				listener.Close()

				return
			}
		}
	}()
}

func TestKillOutwaitsAVmmThatResetsTheCallOnItsWayOut(t *testing.T) {
	cfg := config(shortRoot(t))
	resetting(t, cfg.Socket, true)

	if err := firecracker.Over(cfg.Socket, cfg.Vsock).Kill(); err != nil {
		t.Fatalf("Kill over a vmm that left after the reset = %v, want nil", err)
	}
}

func TestKillReportsASocketThatKeepsResetting(t *testing.T) {
	cfg := config(shortRoot(t))
	resetting(t, cfg.Socket, false)

	err := firecracker.Over(cfg.Socket, cfg.Vsock).Kill()
	if err == nil || !strings.Contains(err.Error(), "kill: GET /") {
		t.Fatalf("Kill over a socket that never answers = %v, want the failed call named", err)
	}
}
