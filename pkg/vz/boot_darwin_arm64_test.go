//go:build darwin && arm64

package vz

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/vzshim"
)

// The boot tests want the shard kernel; a Mac without one skips them, and SHARD_KERNEL names one elsewhere.
const defaultKernel = "../../bin/kernel/arm64/Image-arm64"

const (
	roleEnv    = "SHARD_VZ_TEST_ROLE"
	roleHolder = "holder"
	guestPort  = 5000
)

func TestMain(m *testing.M) {
	if os.Getenv(roleEnv) == roleHolder {
		os.Exit(hold())
	}

	os.Exit(m.Run())
}

// fixtures is what a boot needs; each comes prebuilt from an env, so a Mac without Go runs the same test.
type fixtures struct {
	kernel, initrd, shim string
}

func prepare(t *testing.T) fixtures {
	t.Helper()

	kernel := os.Getenv("SHARD_KERNEL")
	if kernel == "" {
		kernel = defaultKernel
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("no guest kernel at %s: build one with make kernel, or set SHARD_KERNEL", kernel)
	}

	f := fixtures{kernel: kernel, shim: os.Getenv("SHARD_VZ_SHIM"), initrd: os.Getenv("SHARD_VZ_INITRD")}
	if f.shim == "" && vzshim.Embedded() {
		f.shim = installShim(t)
	}
	if f.shim == "" {
		f.shim = buildShim(t)
	}
	if f.initrd == "" {
		f.initrd = buildInitrd(t)
	}

	return f
}

// A test binary built after make build-shard-vz-shim carries the shim, the way make build-darwin's daemon does.
func installShim(t *testing.T) string {
	t.Helper()

	shim, err := vzshim.Install(t.TempDir())
	if err != nil {
		t.Fatalf("vzshim.Install: %v", err)
	}

	return shim
}

// The shim needs the virtualization entitlement, and an ad hoc signature is enough to carry it.
func buildShim(t *testing.T) string {
	t.Helper()

	shim := filepath.Join(t.TempDir(), "shard-vz-shim")
	run(t, "", "go", "build", "-o", shim, "../../cmd/shard-vz-shim")
	run(t, "", "codesign", "--sign", "-", "--force", "--entitlements", "shim/entitlements.plist", shim)

	return shim
}

// The fixture guest is a static Go PID 1 in a newc cpio, the one initrd format the kernel unpacks.
func buildInitrd(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "init"), "./testdata/guest")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the guest: %v: %s", err, out)
	}
	run(t, dir, "sh", "-c", "echo init | cpio -o --format newc --quiet > initrd")

	return filepath.Join(dir, "initrd")
}

func run(t *testing.T, dir string, argv ...string) string {
	t.Helper()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}

	return string(out)
}

func config(t *testing.T, f fixtures) Config {
	t.Helper()

	root := shortRoot(t)

	return Config{
		Kernel:  f.kernel,
		Initrd:  f.initrd,
		Cmdline: "console=hvc0",
		CPUs:    1,
		Memory:  256 << 20,
		Socket:  filepath.Join(root, "shim.sock"),
		Console: filepath.Join(root, "console.log"),
	}
}

// start boots one VM and makes sure its shim is gone when the test ends, whatever the test did.
func start(t *testing.T, shim string, cfg Config) (*Client, Info) {
	t.Helper()

	client, info, err := Start(context.Background(), shim, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reap(t, info.PID) })

	return client, info
}

func reap(t *testing.T, pid int) {
	t.Helper()

	if !alive(pid) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Errorf("kill the shim %d: %v", pid, err)
	}
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func awaitExit(t *testing.T, pid int) {
	t.Helper()

	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !alive(pid) {
			return
		}
	}
	t.Fatalf("the shim %d did not exit after the vm stopped", pid)
}

// A locked screen withholds the Secure Enclave key the restore needs (docs/provider-vz.md, spike item 9).
func sessionLocked(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("ioreg", "-n", "Root", "-d1", "-a").Output()
	if err != nil {
		t.Fatalf("ioreg: %v", err)
	}

	return regexp.MustCompile(`CGSSessionScreenIsLocked</key>\s*<true/>`).Match(out)
}

// guestPID asks PID 1 over vsock who it is, which is the whole proof that the boot reached it.
func guestPID(t *testing.T, client *Client) int {
	t.Helper()

	conn, err := connectWhenListening(client)
	if err != nil {
		t.Fatalf("Connect(%d): %v", guestPort, err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the guest's answer: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "pid="))
	if err != nil {
		t.Fatalf("the guest answered %q", line)
	}

	return pid
}

// The guest listens a moment after the vm runs, so a reset means try again, and only a timeout means failure.
func connectWhenListening(client *Client) (net.Conn, error) {
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var conn net.Conn
		conn, err = client.Connect(guestPort)
		if err == nil {
			return conn, nil
		}
	}

	return nil, err
}

func TestABootReachesPID1OverVsock(t *testing.T) {
	f := prepare(t)
	client, info := start(t, f.shim, config(t, f))

	if info.State != StateRunning || info.MachineID == "" {
		t.Fatalf("Start: %+v", info)
	}
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d", pid)
	}

	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)
}

func TestAZeroValuedConfigBootsOnTheDefaults(t *testing.T) {
	f := prepare(t)
	cfg := config(t, f)
	cfg.CPUs = 0
	cfg.Memory = 0
	client, info := start(t, f.shim, cfg)

	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d", pid)
	}
	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)
}

func TestASecondStartOnTheSameSocketIsRefusedAndTheFirstVMStays(t *testing.T) {
	f := prepare(t)
	cfg := config(t, f)
	client, info := start(t, f.shim, cfg)

	_, _, err := Start(context.Background(), f.shim, cfg)
	if !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("a second Start on the socket: %v", err)
	}
	_, again, err := Adopt(cfg.Socket)
	if err != nil || again.PID != info.PID {
		t.Fatalf("the first shim after the refused start: %+v, %v", again, err)
	}
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d", pid)
	}
	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)
}

// The framework keeps the descriptors and not the files, so a collection in the shim must not close a live device.
func TestTheDeviceFilesOutliveACollectionInTheShim(t *testing.T) {
	f := prepare(t)
	cfg := config(t, f)
	cfg.Network = true
	client, info := start(t, f.shim, cfg)
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d", pid)
	}
	before := openFiles(t, info.PID)

	logPath := strings.TrimSuffix(cfg.Console, ".log") + ".shim.log"
	for i := range 3 {
		if err := syscall.Kill(info.PID, syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		awaitLine(t, logPath, "collected", i+1)
	}

	if after := openFiles(t, info.PID); after != before {
		t.Fatalf("the collection changed the shim's files:\nbefore: %s\nafter:  %s", before, after)
	}
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d after the collection", pid)
	}
	awaitLine(t, cfg.Console, "pid=1", 2)

	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)
}

// openFiles is the shim's console log and its unix sockets, which is where every device file would go missing.
func openFiles(t *testing.T, pid int) string {
	t.Helper()

	out := run(t, "", "lsof", "-p", strconv.Itoa(pid), "-Ftn")
	sockets, console := 0, 0
	for line := range strings.SplitSeq(out, "\n") {
		if line == "tunix" {
			sockets++
		}
		if strings.HasSuffix(line, "console.log") {
			console++
		}
	}
	if sockets == 0 || console == 0 {
		t.Fatalf("lsof shows no unix socket or console log on the shim:\n%s", out)
	}

	return fmt.Sprintf("%d unix sockets, console open %d", sockets, console)
}

// awaitLine waits for the file to hold at least n lines with the text; the guest and the shim both write asynchronously.
func awaitLine(t *testing.T, path, text string, n int) {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b, err := os.ReadFile(path)
		if err == nil && strings.Count(string(b), text) >= n {
			return
		}
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("%s never held %q %d times; it holds %d, and ends with: %s", path, text, n, strings.Count(string(b), text), tailOf(string(b)))
}

func tailOf(s string) string {
	if len(s) > 600 {
		return s[len(s)-600:]
	}

	return s
}

func TestAnOutOfRangeRequestIsRefusedByNameBeforeTheBoot(t *testing.T) {
	f := prepare(t)
	cfg := config(t, f)
	cfg.Memory = 1

	_, _, err := Start(context.Background(), f.shim, cfg)
	if err == nil || !strings.Contains(err.Error(), "1 bytes of memory is outside the host's range") {
		t.Fatalf("Start with 1 byte of memory: %v", err)
	}
}

func TestASavedStateRestoresUnderTheSameIdentifier(t *testing.T) {
	if !HostSaveRestore() {
		t.Skip("this Mac cannot save a vm")
	}
	if sessionLocked(t) {
		t.Skip("the login session is locked, and the framework refuses a restore without its key")
	}
	f := prepare(t)
	cfg := config(t, f)
	client, info := start(t, f.shim, cfg)
	guestPID(t, client)

	state := filepath.Join(filepath.Dir(cfg.Socket), "vm.vzvmstate")
	if _, err := client.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := client.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)

	cfg.Restore = state
	cfg.MachineID = info.MachineID
	restored, again := start(t, f.shim, cfg)
	if again.State != StateRunning || again.MachineID != info.MachineID {
		t.Fatalf("restored: %+v", again)
	}
	if pid := guestPID(t, restored); pid != 1 {
		t.Fatalf("the restored guest answered as pid %d", pid)
	}
	if _, err := restored.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, again.PID)
}

// The daemon dies with SIGKILL; the shim is in its own process group and keeps the vm; a new client re-adopts it.
func TestTheVMOutlivesItsStarterAndANewClientReAdoptsIt(t *testing.T) {
	f := prepare(t)
	cfg := config(t, f)

	holder := exec.Command(os.Args[0], "-test.run=^$")
	holder.Env = append(os.Environ(), roleEnv+"="+roleHolder, "SHARD_VZ_SHIM="+f.shim, "SHARD_VZ_INITRD="+f.initrd,
		"SHARD_KERNEL="+f.kernel, "SHARD_VZ_SOCKET="+cfg.Socket, "SHARD_VZ_CONSOLE="+cfg.Console)
	holder.Stderr = os.Stderr
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the holder said nothing: %v", err)
	}
	shimPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("the holder said %q", line)
	}
	t.Cleanup(func() { reap(t, shimPID) })

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := holder.Wait(); err == nil {
		t.Fatal("the holder was not killed")
	}
	if !alive(shimPID) {
		t.Fatal("the shim died with its starter")
	}

	client, info, err := Adopt(cfg.Socket)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if info.State != StateRunning || info.PID != shimPID {
		t.Fatalf("adopted: %+v, want running under %d", info, shimPID)
	}
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the adopted guest answered as pid %d", pid)
	}
	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, shimPID)
}

// hold is the holder role: start a vm, say the shim's pid, and wait to be killed.
func hold() int {
	cfg := Config{
		Kernel:  os.Getenv("SHARD_KERNEL"),
		Initrd:  os.Getenv("SHARD_VZ_INITRD"),
		Cmdline: "console=hvc0",
		CPUs:    1,
		Memory:  256 << 20,
		Socket:  os.Getenv("SHARD_VZ_SOCKET"),
		Console: os.Getenv("SHARD_VZ_CONSOLE"),
	}
	_, info, err := Start(context.Background(), os.Getenv("SHARD_VZ_SHIM"), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "holder:", err)

		return 1
	}
	fmt.Println(info.PID)
	select {}
}

// The install itself is proven in pkg/vzshim; this is the boot half of the SHARD-214 AC, over the embedded shim.
func TestTheEmbeddedShimBootsAVM(t *testing.T) {
	if !vzshim.Embedded() {
		t.Skip("this test binary carries no shim: run make build-shard-vz-shim first")
	}
	f := prepare(t)
	f.shim = installShim(t)
	client, info := start(t, f.shim, config(t, f))
	if pid := guestPID(t, client); pid != 1 {
		t.Fatalf("the guest answered as pid %d", pid)
	}
	if _, err := client.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitExit(t, info.PID)
}
