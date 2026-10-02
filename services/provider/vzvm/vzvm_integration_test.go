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
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/provider/conformance"
	"github.com/presmihaylov/shard/services/provider/vzvm"
)

// The suite wants the shard kernel; a Mac without one skips it, and SHARD_KERNEL names one elsewhere.
const defaultKernel = "../../../bin/kernel/arm64/Image-arm64"

// The digest is alpine:3.20 as of 2026-09-20; a tag moves, and a rebuilt image changes the inode count the disk is sized to.
const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// dindImage ships dockerd and its runtime; the Docker-inside test boots it as the entrypoint.
const dindImage = "docker:28-dind"

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

	return newVMHarnessFor(t, testImage)
}

func newVMHarnessFor(t *testing.T, ref string) *vmHarness {
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
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	img, err := svc.Pull(ctx, ref)
	if err != nil {
		t.Skipf("cannot pull %s: %v", ref, err)
	}

	h := &vmHarness{root: root, image: img, kernel: kernel, drops: make(chan netstack.Drop, 64)}
	h.open(t)

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
	// The newest provider holds the live shims, so a spec's cleanup must stop through it.
	h.provider = p

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
		// Stop and Remove answer nil for a sandbox a subtest already removed, so an error here is a VM left running.
		ctx := context.Background()
		if err := h.provider.Stop(ctx, id, stopGrace); err != nil {
			t.Errorf("stop sandbox %s at cleanup: %v", id, err)
		}
		if err := h.provider.Remove(ctx, id); err != nil {
			t.Errorf("remove sandbox %s at cleanup: %v", id, err)
		}
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

// Docker runs inside a VM: dockerd is the entrypoint, a container runs, and a container's request crosses the stack as the guest's own does.
func TestDockerRunsInsideAVM(t *testing.T) {
	h := newVMHarnessFor(t, dindImage)

	caPEM, leaf := testCA(t, net.ParseIP("93.184.216.34"))
	inner, err := h.stack.ListenTCP(tlsPort)
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(inner, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				fmt.Fprint(conn, "HTTP/1.0 200 OK\r\nContent-Length: 8\r\n\r\ntrusted\n")
			}()
		}
	}()

	spec := h.newSpec(t, "dockerd", "--host=unix:///var/run/docker.sock")
	spec.ProxyCA = caPEM
	spec.Resources.MemoryMiB = 512
	spec.Resources.DiskMiB = 1024
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	// The image the container runs is the guest's own userland, since a pull would need a registry behind the redirect.
	script := `for i in $(seq 1 90); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info >/dev/null 2>&1 || { echo "dockerd never answered"; exit 1; }
tar -C / -c bin sbin lib usr/lib usr/bin etc | docker import - local/base >/dev/null || exit 1
docker run --rm local/base /bin/true && echo ran-true
docker run --rm local/base wget -qO- -T 15 https://93.184.216.34/
docker run --rm local/base wget -qO- -T 3 http://93.184.216.34:8080/ 2>&1`
	out, err := os.CreateTemp(t.TempDir(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", script}, Stdout: out, Stderr: out}); err != nil {
		log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
		read, _ := os.ReadFile(out.Name())
		t.Fatalf("Exec: %v\nexec output:\n%s\nsandbox log:\n%s", err, read, log)
	}
	read, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(read), "ran-true\ntrusted\n") {
		log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
		t.Fatalf("the containers wrote %q, want a true exit and the listener's body over TLS\nsandbox log:\n%s", read, log)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case drop := <-h.drops:
			if drop.Guest == spec.Network.Address.Addr() && drop.Protocol == "tcp" && drop.Port == 8080 {
				return
			}
		case <-deadline:
			t.Fatal("no drop reported for the container's dial of 8080")
		}
	}
}

// A guest that outgrows its bound dies as a whole and the status blames the bound, as a Linux sandbox does; nothing records an exit.
func TestAGuestThatOutgrowsItsBoundIsOOMKilled(t *testing.T) {
	h := newVMHarness(t)

	// The tmpfs is charged to the writer, and its default size sits under the bound, so the remount lifts it first.
	script := "while [ ! -e /tmp/go ]; do sleep 0.2; done; mount -o remount,size=1G /dev/shm && dd if=/dev/zero of=/dev/shm/fill bs=1M; while true; do sleep 1; done"
	spec := h.newSpec(t, "/bin/sh", "-c", script)
	if err := h.provider.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := h.provider.Start(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}

	// Only PID 1 is exempt from the killer: a guest process, and what it forks, as any user, is exposed before it runs.
	out, err := os.CreateTemp(t.TempDir(), "adj")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	probe := "cat /proc/1/oom_score_adj /proc/self/oom_score_adj; sh -c 'cat /proc/self/oom_score_adj'; touch /tmp/go"
	if _, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/sh", "-c", probe}, User: "nobody", Stdout: out, Stderr: out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if read, _ := os.ReadFile(out.Name()); string(read) != "-1000\n0\n0\n" {
		t.Fatalf("oom_score_adj of PID 1, an exec and its child = %q, want -1000, 0 and 0", read)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, err := h.provider.Status(t.Context(), spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == models.StateStopped {
			if !status.OOMKilled {
				log, _ := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
				t.Fatalf("the guest stopped without the bound blamed\nsandbox log:\n%s", log)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guest is still %s two minutes into the fill", status.State)
		}
		time.Sleep(time.Second)
	}
	if _, found, err := bundle.ReadExitStatus(filepath.Join(spec.StateDir, "exit.json")); err != nil || found {
		t.Fatalf("exit record after the kill: found=%v err=%v; want none", found, err)
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

// Every restore of one save wakes with the same crng key, so the resumed source and its forks each read their own bytes only after a reseed (SHARD-293).
func TestTheRestoresOfOneSaveReadDifferentRandomBytes(t *testing.T) {
	h := newVMHarness(t)
	if !h.provider.Capabilities().Fork {
		t.Skip("this Mac does not save a VM")
	}
	spec := h.newSpec(t, "/bin/sh", "-c", "sleep 3600")
	// One vcpu means one per-cpu crng, so no copy reads other bytes only because its exec ran on another cpu.
	spec.Resources.VCPUs = 1
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
	// A timed reseed splits some copies by chance; among five, two shared a key in every run without the host's reseed.
	ids := []string{spec.ID}
	for range 4 {
		fork := h.newSpec(t)
		if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fork.ID)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	for _, id := range ids {
		out, err := os.CreateTemp(t.TempDir(), "urandom")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		if _, err := h.provider.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", "head -c 32 /dev/urandom | od -An -tx1"}, Stdout: out, Stderr: out}); err != nil {
			t.Fatalf("Exec in %s: %v", id, err)
		}
		written, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		drawn := strings.Join(strings.Fields(string(written)), "")
		if len(drawn) != 64 {
			t.Fatalf("%s read %q from /dev/urandom, want 32 bytes in hex", id, written)
		}
		t.Logf("%s read %s", id, drawn)
		if other, ok := seen[drawn]; ok {
			t.Fatalf("%s and %s read the same /dev/urandom bytes %s after a restore of one save", other, id, drawn)
		}
		seen[drawn] = id
	}
}

// A process that runs across the save draws its next bytes after the restore, so two forks share no draw past the first line they differ on (SHARD-310).
func TestTheForksOfOneSaveShareNoDrawPastTheFirstTheyDifferOn(t *testing.T) {
	h := newVMHarness(t)
	if !h.provider.Capabilities().Fork {
		t.Skip("this Mac does not save a VM")
	}
	spec := h.newSpec(t, "/bin/sh", "-c", `while :; do echo "$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"; done`)
	// One vcpu means one per-cpu crng, so no fork reads other bytes only because a draw ran on another cpu.
	spec.Resources.VCPUs = 1
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
	var forks []models.SandboxSpec
	for range 2 {
		fork := h.newSpec(t)
		if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
			t.Fatal(err)
		}
		forks = append(forks, fork)
	}
	time.Sleep(time.Second)
	var draws [2][]string
	for i, fork := range forks {
		data, err := os.ReadFile(filepath.Join(fork.StateDir, "output.log"))
		if err != nil {
			t.Fatal(err)
		}
		draws[i] = strings.Fields(string(data))
	}

	// Both forks print the lines the save held in one order, so the first line they differ on, and every line after it, came after the restore.
	past := 0
	for past < min(len(draws[0]), len(draws[1])) && draws[0][past] == draws[1][past] {
		past++
	}
	if len(draws[0])-past < 10 || len(draws[1])-past < 10 {
		t.Fatalf("the forks drew %d and %d lines past line %d, the first they differ on, want 10 or more each", len(draws[0])-past, len(draws[1])-past, past)
	}
	drawn := map[string]int{}
	for i, d := range draws[0][past:] {
		drawn[d] = past + i
	}
	shared := 0
	for i, d := range draws[1][past:] {
		if at, ok := drawn[d]; ok {
			shared++
			t.Logf("fork 0 line %d and fork 1 line %d are both %s", at, past+i, d)
		}
	}
	if shared > 0 {
		t.Fatalf("the two forks share %d draws past line %d, the first they differ on, want none", shared, past)
	}
}

// A clone boots from the disk alone, so a pause freezes the root under a writer in mid-loop: the clone holds every count the writer printed, and the source and a fork write again after (SHARD-296).
func TestAPauseFreezesTheRootUnderALoopingWriter(t *testing.T) {
	h := newVMHarness(t)
	if !h.provider.Capabilities().Fork {
		t.Skip("this Mac does not save a VM")
	}
	// Each count reaches the disk before the log, and a cold boot of the disk finds the file and only sleeps.
	spec := h.newSpec(t, "/bin/sh", "-c", `[ -e /root/log ] && exec sleep 1000000; i=0; while :; do i=$((i+1)); echo $i >> /root/log || exit 1; echo $i; done`)
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
	log, err := os.ReadFile(filepath.Join(spec.StateDir, "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(log), "\n")
	printed, err := strconv.Atoi(lines[max(len(lines)-2, 0)])
	if err != nil {
		t.Fatalf("the writer printed no count before the pause: %v", err)
	}

	clone := h.newSpec(t)
	if err := h.provider.Clone(t.Context(), spec.ID, clone); err != nil {
		t.Fatal(err)
	}
	onDisk, err := strconv.Atoi(execIn(t, h, clone.ID, `awk 'NR != $1 { print "a gap at line " NR ": " $0; exit 1 } END { print NR }' /root/log`))
	if err != nil {
		t.Fatal(err)
	}
	if onDisk < printed {
		t.Fatalf("the clone's log ends at %d, and the source printed %d before its pause", onDisk, printed)
	}
	t.Logf("the source printed %d before its pause, and the clone holds %d", printed, onDisk)

	fork := h.newSpec(t)
	if err := h.provider.Fork(t.Context(), snap, fork); err != nil {
		t.Fatal(err)
	}
	if got := execIn(t, h, fork.ID, "echo forked > /root/forked && cat /root/forked"); got != "forked" {
		t.Fatalf("the fork wrote %q, want forked", got)
	}
	if err := h.provider.Resume(t.Context(), spec.ID, snap); err != nil {
		t.Fatal(err)
	}
	counted, err := strconv.Atoi(execIn(t, h, spec.ID, "sleep 1; wc -l < /root/log"))
	if err != nil {
		t.Fatal(err)
	}
	if counted <= onDisk {
		t.Fatalf("the resumed writer is still at %d, where its pause froze it", counted)
	}
}

// execIn runs one shell line under a deadline, since a write to a root left frozen never returns.
func execIn(t *testing.T, h *vmHarness, id, line string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	exit, err := h.provider.Exec(ctx, id, models.ExecSpec{Argv: []string{"/bin/sh", "-c", line}, Stdout: out, Stderr: out})
	if err != nil {
		t.Fatalf("exec %q in %s: %v", line, id, err)
	}
	written, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if exit != (models.ExitStatus{}) {
		t.Fatalf("exec %q in %s ended %+v: %s", line, id, exit, written)
	}

	return strings.TrimSpace(string(written))
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
