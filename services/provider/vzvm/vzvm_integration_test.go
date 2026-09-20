//go:build integration && darwin && arm64

package vzvm_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/netstack"
	"github.com/presmihaylov/shard/pkg/vz"
	"github.com/presmihaylov/shard/pkg/vzshim"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

// The suite wants the shard kernel; a Mac without one skips it, and SHARD_KERNEL names one elsewhere.
const defaultKernel = "../../../bin/kernel/arm64/Image-arm64"

// The digest is alpine:3.20 as of 2026-09-20; a tag moves, and a rebuilt image changes the inode count the disk is sized to.
const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

var gateway = netip.MustParseAddr("10.200.0.1")

const (
	redirectPort = 30080
	tlsPort      = 30443
)

// vmHarness is the provider over real VMs: the shim, the kernel, a linux/arm64 shard-init and the image's disk.
type vmHarness struct {
	provider *vzvm.Provider
	root     string
	kernel   string
	image    image.Image
	stack    *netstack.Stack
	drops    chan netstack.Drop
	next     atomic.Int64
}

func newVMHarness(t *testing.T) *vmHarness {
	t.Helper()

	kernel := os.Getenv("SHARD_KERNEL")
	if kernel == "" {
		kernel = defaultKernel
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("no guest kernel at %s: build one with make kernel, or set SHARD_KERNEL", kernel)
	}

	root, err := os.MkdirTemp("", "vz") //nolint:usetesting // t.TempDir is too long for a socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	svc, err := image.New(filepath.Join(root, "images"), image.WithDisks())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	img, err := svc.Pull(ctx, testImage)
	if err != nil {
		t.Skipf("cannot pull %s: %v", testImage, err)
	}

	h := &vmHarness{root: root, image: img, kernel: kernel, drops: make(chan netstack.Drop, 64)}
	h.provider = h.open(t)

	return h
}

// open is a daemon start: a fresh stack and a provider over the root, which holds nothing of an earlier one in memory.
func (h *vmHarness) open(t *testing.T) *vzvm.Provider {
	t.Helper()

	// The daemon's redirect of 80 onto the proxy, here onto a listener the tests serve.
	stack, err := netstack.New(netstack.Config{
		Address:   gateway,
		Redirects: map[uint16]uint16{80: redirectPort, 443: tlsPort},
		Drops:     func(d netstack.Drop) { h.drops <- d },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stack.Close() })
	h.stack = stack

	p, err := vzvm.New(vzvm.Config{
		Shim:        shimBinary(t),
		Kernel:      h.kernel,
		Init:        guestInit(t),
		Dir:         h.root,
		Stack:       stack,
		Dirs:        h.stateDir,
		SaveRestore: vz.HostSaveRestore() && !sessionLocked(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	return p
}

// reopen is a daemon restart: the first provider lets go of its shims, and a second one adopts them.
func (h *vmHarness) reopen(t *testing.T) models.Provider {
	t.Helper()

	if err := h.provider.Close(); err != nil {
		t.Fatalf("close the provider: %v", err)
	}

	return h.open(t)
}

func (h *vmHarness) stateDir(id string) (string, error) {
	return filepath.Join(h.root, "s", id), nil
}

// newSpec gives every sandbox its own id, directory and address on the stack, and ends it when the test does.
func (h *vmHarness) newSpec(t *testing.T, entrypoint ...string) models.SandboxSpec {
	t.Helper()

	n := h.next.Add(1)
	id := fmt.Sprintf("vm-%d", n)
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

	return models.SandboxSpec{
		ID:         id,
		StateDir:   dir,
		RootFS:     h.image.RootFS,
		RootDisk:   h.image.Disk,
		Entrypoint: entrypoint,
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Network:    models.NetworkSpec{Address: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 200, 0, byte(n + 1)}), 24), Gateway: gateway},
		Resources:  models.Resources{MemoryMiB: 256, DiskMiB: 64},
	}
}

// The image disk holds a full group of inodes, so the smallest disk takes thousands of files (SHARD-254).
func TestASmallDiskHoldsThousandsOfFiles(t *testing.T) {
	h := newVMHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "mkdir /many && i=0; while [ $i -lt 2000 ]; do : > /many/f$i; i=$((i+1)); done && ls /many | wc -l")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	exit, err := h.provider.Wait(t.Context(), spec.ID)
	if err != nil || exit.Code != 0 {
		log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
		t.Fatalf("Wait = %+v, %v\nsandbox log:\n%s", exit, err, log)
	}
	log, err := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "2000") {
		t.Fatalf("the guest did not count 2000 files:\n%s", log)
	}
}

func TestConformanceOnVMs(t *testing.T) {
	h := newVMHarness(t)

	conformance.Run(t, conformance.Subject{
		Provider: h.provider,
		NewSpec:  func(t *testing.T) models.SandboxSpec { return h.newSpec(t, "/bin/true") },
		NewIgnoresTermSpec: func(t *testing.T) models.SandboxSpec {
			script := fmt.Sprintf("trap '' TERM; echo %s; while true; do sleep 1; done", conformance.ReadyMarker)

			return h.newSpec(t, "/bin/sh", "-c", script)
		},
		SnapshotDir: func(t *testing.T) string { return t.TempDir() },
		Shell:       func(script string) []string { return []string{"/bin/sh", "-c", script} },
		Reopen:      h.reopen,
	})
}

// A guest's request to an outside address on 80 lands on the redirected port, Host header intact, and the stack drops the rest and says so.
func TestAGuestReachesTheRedirectedPortAndNothingElse(t *testing.T) {
	h := newVMHarness(t)

	listener, err := h.stack.ListenTCP(redirectPort)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	hosts := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request, _ := http.ReadRequest(bufio.NewReader(conn))
		hosts <- request.Host
		fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Length: 8\r\n\r\nproxied\n")
	}()

	spec := h.newSpec(t, "/bin/sh", "-c", "sleep 300")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	out, err := os.CreateTemp(t.TempDir(), "wget")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	script := "wget -qO- -T 10 http://93.184.216.34/; wget -qO- -T 3 http://93.184.216.34:8080/ 2>&1"
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", script}, Stdout: out, Stderr: out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	read, _ := os.ReadFile(out.Name())
	if !strings.HasPrefix(string(read), "proxied") {
		t.Fatalf("the guest read %q through port 80, want the listener's body", read)
	}
	if host := <-hosts; host != "93.184.216.34" {
		t.Fatalf("the listener saw Host %q, want the address the guest dialed", host)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case drop := <-h.drops:
			if drop.Guest == spec.Network.Address.Addr() && drop.Protocol == "tcp" && drop.Port == 8080 {
				return
			}
		case <-deadline:
			t.Fatal("no drop reported for the guest's dial of 8080")
		}
	}
}

// A fronted guest trusts the proxy CA: its TLS client verifies a leaf the CA signed, read through the 443 redirect.
func TestAFrontedGuestTrustsTheProxyCA(t *testing.T) {
	h := newVMHarness(t)

	caPEM, leaf := testCA(t, net.ParseIP("93.184.216.34"))
	inner, err := h.stack.ListenTCP(tlsPort)
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Length: 8\r\n\r\ntrusted\n")
	}()

	spec := h.newSpec(t, "/bin/sh", "-c", "sleep 300")
	spec.ProxyCA = caPEM
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	out, err := os.CreateTemp(t.TempDir(), "wget")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"wget", "-qO-", "-T", "15", "https://93.184.216.34/"}, Stdout: out, Stderr: out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	read, _ := os.ReadFile(out.Name())
	if !strings.HasPrefix(string(read), "trusted") {
		t.Fatalf("the guest read %q over TLS, want the listener's body", read)
	}
}

// testCA mints a CA and a leaf for ip it signed, the shape the proxy presents to a guest.
func testCA(t *testing.T, ip net.IP) ([]byte, tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "shard test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: ip.String()}, IPAddresses: []net.IP{ip},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), leaf
}

// The unbounded default is the host count held inside the framework's range, the same count HostCPUs reports.
func TestAnUnboundedCPUCountIsEveryHostCPUTheFrameworkAllows(t *testing.T) {
	h := newVMHarness(t)
	spec := h.newSpec(t, "/bin/sh", "-c", "sleep 300")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	out, err := os.CreateTemp(t.TempDir(), "nproc")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"nproc"}, Stdout: out, Stderr: out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	read, _ := os.ReadFile(out.Name())
	if got, want := strings.TrimSpace(string(read)), strconv.FormatUint(uint64(vz.HostCPUs()), 10); got != want {
		t.Fatalf("the guest sees %s cpus, want the host's %s", got, want)
	}
}

// A resumed VM carries its memory: the counter the entrypoint kept goes on from where the pause froze it.
func TestAResumeAndAForkCarryTheGuestMemory(t *testing.T) {
	h := newVMHarness(t)
	if !h.provider.Capabilities().Fork {
		t.Skip("this Mac does not save a VM")
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "i=0; while true; do i=$((i+1)); echo $i > /count; sleep 0.2; done")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	snap := t.TempDir()
	if err := h.provider.Pause(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	fork := h.newSpec(t)
	fork.Name = "twin"
	fork.Network.Nameservers = []netip.Addr{gateway}
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{spec.ID, fork.ID} {
		out, err := os.CreateTemp(t.TempDir(), "count")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		if _, err := h.provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "cat /count; ip -4 -o addr show eth0"}, Stdout: out, Stderr: out}); err != nil {
			t.Fatalf("Exec in %s: %v", id, err)
		}
		written, _ := os.ReadFile(out.Name())
		fields := strings.Fields(string(written))
		if len(fields) < 1 || fields[0] == "" || fields[0] == "0" {
			t.Fatalf("%s counted %q after the restore, want a count the pause froze", id, written)
		}
		t.Logf("%s: %s", id, strings.TrimSpace(string(written)))
	}
	// The fork is a new sandbox: it answers to its own name and resolves through its own lease, not the source's.
	out, err := os.CreateTemp(t.TempDir(), "resolver")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := h.provider.Exec(t.Context(), fork.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "hostname; cat /etc/resolv.conf"}, Stdout: out, Stderr: out}); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(out.Name())
	if want := "twin\nnameserver " + gateway.String() + "\n"; string(written) != want {
		t.Fatalf("the fork's hostname and resolv.conf = %q, want %q", written, want)
	}
}

// A guest that panics before anything listens on vsock fails the create within its grace and leaves no shim behind (SHARD-255).
func TestACreateWhoseGuestNeverAnswersLeavesNoShim(t *testing.T) {
	h := newVMHarness(t)
	// An empty /init is a kernel panic before the supervisor exists.
	empty := filepath.Join(t.TempDir(), "shard-init")
	if err := os.WriteFile(empty, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := vzvm.New(vzvm.Config{Shim: shimBinary(t), Kernel: h.kernel, Init: empty, Dir: t.TempDir(), Stack: h.stack, Dirs: h.stateDir})
	if err != nil {
		t.Fatal(err)
	}
	spec := h.newSpec(t, "sleep", "3600")

	started := time.Now()
	err = p.Create(t.Context(), spec)
	if err == nil {
		t.Fatal("Create over a guest that never answers succeeded")
	}
	t.Logf("Create failed after %s: %v", time.Since(started).Round(time.Second), err)
	if shims := shimsOf(t, spec.StateDir); len(shims) != 0 {
		t.Fatalf("shims left after the failed create: %v", shims)
	}
	if _, err := os.Stat(filepath.Join(spec.StateDir, "vm.json")); !os.IsNotExist(err) {
		t.Fatalf("the record of the failed create is still there: %v", err)
	}
}

// SIGTERM is what an operator sends first, so a shim ends its VM and exits on it (SHARD-255).
func TestAShimEndsItsVMOnSIGTERM(t *testing.T) {
	h := newVMHarness(t)
	spec := h.newSpec(t, "sleep", "3600")
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	shims := shimsOf(t, spec.StateDir)
	if len(shims) != 1 {
		t.Fatalf("shims of the running sandbox = %v, want one", shims)
	}
	if err := syscall.Kill(shims[0], syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(shimsOf(t, spec.StateDir)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the shim %d still runs 15s after SIGTERM", shims[0])
		}
		time.Sleep(200 * time.Millisecond)
	}
	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State == models.StateRunning {
		t.Fatalf("the sandbox is still running after its shim ended: %+v", status)
	}
}

// shimsOf is the pid of every shim whose config names the sandbox's socket.
func shimsOf(t *testing.T, stateDir string) []int {
	t.Helper()

	out, err := exec.Command("pgrep", "-f", filepath.Join(stateDir, "shim.sock")).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		t.Fatalf("pgrep: %v", err)
	}
	var pids []int
	for field := range strings.FieldsSeq(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}

	return pids
}

// The shim needs the virtualization entitlement, and the embedded one is signed on install; a build without it is signed here.
func shimBinary(t *testing.T) string {
	t.Helper()

	if shim := os.Getenv("SHARD_VZ_SHIM"); shim != "" {
		return shim
	}
	if vzshim.Embedded() {
		shim, err := vzshim.Install(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		return shim
	}
	shim := filepath.Join(t.TempDir(), "shard-vz-shim")
	run(t, "go", "build", "-o", shim, "../../../cmd/shard-vz-shim")
	run(t, "codesign", "--sign", "-", "--force", "--entitlements", "../../../pkg/vzshim/shim/entitlements.plist", shim)

	return shim
}

// guestInit is shard-init for the VM: static linux/arm64, which the provider packs as the initrd's /init.
func guestInit(t *testing.T) string {
	t.Helper()

	if init := os.Getenv("SHARD_VZ_INIT"); init != "" {
		return init
	}
	path := filepath.Join(t.TempDir(), "shard-init")
	build := exec.Command("go", "build", "-o", path, "../../../cmd/shard-init")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shard-init: %v: %s", err, out)
	}

	return path
}

// On macOS 14 a locked screen withholds the key a restore needs (docs/provider-vz.md, item 9); 26.6 restores locked, so only 14 runs without the optional verbs.
func sessionLocked(t *testing.T) bool {
	t.Helper()

	version, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		t.Fatalf("sw_vers: %v", err)
	}
	if !strings.HasPrefix(string(version), "14.") {
		return false
	}
	out, err := exec.Command("ioreg", "-n", "Root", "-d1", "-a").Output()
	if err != nil {
		t.Fatalf("ioreg: %v", err)
	}

	return regexp.MustCompile(`CGSSessionScreenIsLocked</key>\s*<true/>`).Match(out)
}

func run(t *testing.T, argv ...string) {
	t.Helper()

	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", strings.Join(argv, " "), err, out)
	}
}
