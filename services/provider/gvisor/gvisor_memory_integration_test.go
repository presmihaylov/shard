//go:build integration

package gvisor_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/cgroup"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

// boundMiB is the bound the guest gets. It is small, because the only memory a guest can hold
// without CAP_SYS_ADMIN is the /dev/shm tmpfs, which the bundle sizes at 64 MiB.
const boundMiB = 64

// fillMiB is what the guest holds in that tmpfs. Tmpfs pages are sentry memory, so the host cgroup
// charges them, and this plus the sentry's own 26 to 31 MiB is well past the bound.
const fillMiB = 44

const filledMarker = "SHARD-97-FILLED"

const tmpfsMarker = "SHARD-97-TMPFS"

// TestAGuestThatHoldsMostOfItsBoundStaysAlive is the SHARD-97 acceptance criterion. Before the
// headroom the host cgroup charged the sentry against the same bound, so this guest was OOM-killed.
func TestAGuestThatHoldsMostOfItsBoundStaysAlive(t *testing.T) {
	h := newHarness(t)

	// The file goes in /dev/shm, because that is a tmpfs the sentry holds in memory. A write anywhere
	// else lands on the host disk through the gofer and costs the guest no memory at all.
	script := "head -1 /proc/meminfo; dd if=/dev/zero of=/dev/shm/fill bs=1M count=" + strconv.Itoa(fillMiB) +
		" 2>/dev/null && echo " + filledMarker + "; while true; do sleep 1; done"

	spec := h.newSpec(t, "/bin/sh", "-c", script)
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	h.startSpec(t, spec)
	awaitMarker(t, h, spec.ID, filledMarker)

	// The guest reads the sentry's budget as MemTotal, so headroom that reached the spec would show here.
	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if got := memTotalKiB(t, readFile(t, path)); got != boundMiB*1024 {
		t.Errorf("the guest reads MemTotal %d kB, want %d", got, boundMiB*1024)
	}

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != models.StateRunning {
		t.Fatalf("a sandbox that holds %d MiB of its %d MiB bound is %s", fillMiB, boundMiB, status.State)
	}

	// An exec proves the sandbox is usable and not merely recorded as running.
	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exit.Code != 0 {
		t.Errorf("exec in a sandbox at its bound exited %d", exit.Code)
	}
}

// TestTheCgroupCarriesTheThrottleAndTheCeiling proves shard wrote memory.high on the cgroup runsc made. The
// OCI spec has no field for it, so a create that skipped this step leaves the ceiling alone in place.
func TestTheCgroupCarriesTheThrottleAndTheCeiling(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	h.startSpec(t, spec)

	dir := filepath.Join(cgroup.Root, spec.ID)

	throttle, ceiling := gvisor.MemoryThrottle(spec.Resources), gvisor.MemoryCeiling(spec.Resources)

	// Both files are read raw, so the assertion does not lean on the driver that wrote them.
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "memory.high"))); got != strconv.FormatInt(throttle, 10) {
		t.Errorf("memory.high is %s, want the throttle %d", got, throttle)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "memory.max"))); got != strconv.FormatInt(ceiling, 10) {
		t.Errorf("memory.max is %s, want the ceiling %d", got, ceiling)
	}
}

// TestAStaleCgroupThatWouldUnboundTheSandboxIsRefused pins the one way a bound is silently lost:
// runsc applies no limit at all when the cgroup directory is already there, and the sentry then boots
// with the whole host's memory as its budget.
func TestAStaleCgroupThatWouldUnboundTheSandboxIsRefused(t *testing.T) {
	h := newHarness(t)

	spec := h.newSpec(t, "/bin/sh", "-c", "while true; do sleep 1; done")
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	dir := filepath.Join(cgroup.Root, spec.ID)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("forge a stale cgroup: %v", err)
	}

	// The forged directory outlives the failed create, so it goes back before the harness sweeps the id.
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove the forged cgroup: %v", err)
		}
	})

	err := h.provider.Create(t.Context(), spec)
	if err == nil {
		t.Fatal("Create over a stale cgroup succeeded, so the sandbox runs unbounded")
	}
	if !strings.Contains(err.Error(), "memory.max") {
		t.Errorf("Create failed with %v, which does not name the ceiling that is missing", err)
	}
}

// memTotalKiB reads the first field of the guest's own /proc/meminfo, which the entrypoint printed.
func memTotalKiB(t *testing.T, log string) int {
	t.Helper()

	for line := range strings.SplitSeq(log, "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}

		value, err := strconv.Atoi(strings.Fields(line)[1])
		if err != nil {
			t.Fatalf("read MemTotal from %q: %v", line, err)
		}

		return value
	}

	t.Fatalf("the guest never printed MemTotal, log: %s", log)

	return 0
}

// awaitMarker waits for the entrypoint to say it finished, because Start returns before it runs.
func awaitMarker(t *testing.T, h *harness, id, marker string) {
	t.Helper()

	path, err := h.provider.LogPath(id)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		blob, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(blob), marker) {
			return
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("sandbox %s never printed %s, log: %s", id, marker, readFile(t, path))
}

// TestTheGuestCannotWriteToDev is the hole a size= cannot close. gVisor drops it on /dev and mounts
// its own devtmpfs, which reports half the host's memory, so a guest ended its own 64 MiB sandbox
// with dd in three and a half minutes. Read-only is the only bound left, and a device node still
// opens for write through one, which is what keeps /dev/null working.
func TestTheGuestCannotWriteToDev(t *testing.T) {
	h := newHarness(t)

	script := "echo probe > /dev/fill 2>/dev/null && echo " + tmpfsMarker + " writable; echo hello > /dev/null &&" +
		" echo " + tmpfsMarker + " devnull; df -m /dev/shm | tail -1 | awk '{print \"" + tmpfsMarker + " shm \" $2}';" +
		" echo " + tmpfsMarker + " done; while true; do sleep 1; done"

	spec := h.newSpec(t, "/bin/sh", "-c", script)
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	h.startSpec(t, spec)
	awaitMarker(t, h, spec.ID, tmpfsMarker+" done")

	path, err := h.provider.LogPath(spec.ID)
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}

	log := readFile(t, path)
	if strings.Contains(log, tmpfsMarker+" writable") {
		t.Error("the guest wrote a file to /dev, so it can still fill the host and end its own sandbox")
	}
	if !strings.Contains(log, tmpfsMarker+" devnull") {
		t.Error("the guest cannot write to /dev/null, so a read-only /dev broke the device nodes")
	}

	// The sizes come from inside the guest, so this is the mount the sentry made and not our config.json.
	if got := guestTmpfsMiB(t, log, "shm"); got > boundMiB {
		t.Errorf("a %d MiB sandbox holds %d MiB of /dev/shm, which is more than its own bound", boundMiB, got)
	}
}

// TestAGuestThatFillsItsTmpfsKeepsItsSandbox is the same hole from the other side. The fill is not
// waited on: a guest that holds its whole bound is throttled to a crawl by memory.high, which is the
// point. What matters is that the sandbox is still there and still usable while it happens.
func TestAGuestThatFillsItsTmpfsKeepsItsSandbox(t *testing.T) {
	h := newHarness(t)

	script := "dd if=/dev/zero of=/dev/shm/fill bs=1M 2>/dev/null; while true; do sleep 1; done"

	spec := h.newSpec(t, "/bin/sh", "-c", script)
	spec.Resources = models.Resources{MemoryMiB: boundMiB}

	h.startSpec(t, spec)
	time.Sleep(90 * time.Second)

	status, err := h.provider.Status(t.Context(), spec.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != models.StateRunning {
		t.Fatalf("a sandbox whose guest filled its tmpfs is %s, and OOMKilled is %v", status.State, status.OOMKilled)
	}

	exit, err := h.provider.Exec(t.Context(), spec.ID, models.ExecSpec{Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if exit.Code != 0 {
		t.Errorf("exec in a sandbox with a full tmpfs exited %d", exit.Code)
	}
}

// guestTmpfsMiB reads back a size the entrypoint printed, in MiB.
func guestTmpfsMiB(t *testing.T, log, mount string) int64 {
	t.Helper()

	prefix := tmpfsMarker + " " + mount + " "
	for line := range strings.SplitSeq(log, "\n") {
		size, found := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !found {
			continue
		}

		mib, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			t.Fatalf("the guest says %s is %q MiB: %v", mount, size, err)
		}

		return mib
	}

	t.Fatalf("the guest never printed a size for %s, log: %s", mount, log)

	return 0
}
