package gvisor_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/provider/gvisor"
)

func TestZombieStatReadsTheStateAfterTheComm(t *testing.T) {
	cases := map[string]struct {
		stat string
		want bool
	}{
		"a zombie":                    {"1491697 (gvisor_sentry) Z 1 1491697 1491697 0 -1 4194560", true},
		"a sleeping process":          {"1491697 (gvisor_sentry) S 1 1491697 1491697 0 -1 4194560", false},
		"a comm with a parenthesis":   {"7 (a)b) Z 1 7 7 0 -1 0", true},
		"a comm with a space and a Z": {"7 (Z Z) S 1 7 7 0 -1 0", false},
		"a line cut before the state": {"7 (sh)", false},
		"nothing":                     {"", false},
	}

	for name, c := range cases {
		if got := gvisor.ZombieStat(c.stat); got != c.want {
			t.Errorf("%s: zombie is %v, want %v", name, got, c.want)
		}
	}
}

// After the entrypoint exits, the sentry sits as a zombie until PID 1 reaps it, and runsc keeps
// answering running for it. A stop that returned must not be followed by an rm that says running.
func TestStatusCallsAZombieSandboxStopped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is a Linux thing")
	}

	// A child nobody waits on is a zombie until the test ends.
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	t.Cleanup(func() {
		if err := child.Wait(); err != nil {
			t.Errorf("reap the child: %v", err)
		}
	})
	pid := child.Process.Pid
	awaitZombie(t, pid)

	p := newProviderOver(t, fmt.Sprintf(`echo '{"id":"amber-otter-1a2b","status":"running","pid":%d}'`, pid))
	p.SetCgroupRoot(t.TempDir())

	status, err := p.Status(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Alive() || status.State != models.StateStopped || !status.Exists {
		t.Errorf("a zombie sandbox reads as %+v, want it stopped and still known to runsc", status)
	}
}

func awaitZombie(t *testing.T, pid int) {
	t.Helper()

	for range 100 {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			t.Fatalf("read the child's stat: %v", err)
		}
		if gvisor.ZombieStat(string(stat)) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("the child %d never became a zombie", pid)
}
