package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// The supervisor is a process, so the tests re-execute this binary as both halves of the pair.
const (
	childPrefix    = "child:"
	roleEnv        = "SHARD_INIT_TEST_ROLE"
	roleSupervisor = "supervisor"
	// A supervisor a test starts takes requests and writes logs where that test says, never at the sandbox paths.
	testAddrEnv = "SHARD_INIT_TEST_ADDR"
	testLogsEnv = "SHARD_INIT_TEST_LOGS"
)

func TestMain(m *testing.M) {
	// macOS has no /proc, so the files path runs the test binary, which answers it below as main does.
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locate the test binary:", err)
		os.Exit(1)
	}
	selfBinary = exe
	if len(os.Args) == 2 && os.Args[1] == supervisor.FilesMode {
		os.Exit(runFiles())
	}

	// The child inherits the supervisor environment, so its role comes from argv and wins here.
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], childPrefix) {
		os.Exit(runChild(strings.TrimPrefix(os.Args[1], childPrefix)))
	}

	if os.Getenv(roleEnv) == roleSupervisor {
		requestAddr = os.Getenv(testAddrEnv)
		guestLogs = os.Getenv(testLogsEnv)
		os.Exit(runSupervisor())
	}

	os.Exit(m.Run())
}

// It mirrors main, exit code included, or a test would pin a code the real binary never returns.
func runSupervisor() int {
	err := run(os.Args[1:])
	if err == nil {
		return 0
	}

	fmt.Fprintln(os.Stderr, "shard-init:", err)

	return exitCodeFor(err)
}

func runChild(spec string) int {
	kind, arg, _ := strings.Cut(spec, ":")
	switch kind {
	case "exit":
		return atoi(arg)
	case "sigkill":
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			fmt.Fprintln(os.Stderr, "kill self:", err)
			return 2
		}

		select {}
	case "term":
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		fmt.Println("ready")
		<-sigs
		return atoi(arg)
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		return 0
	case "sleep":
		sleepWhileParented(time.Duration(atoi(arg)) * time.Millisecond)
		return 0
	case "run":
		// A run of MS milliseconds then exit CODE, so a test can make a run outlast the reset window.
		ms, code, _ := strings.Cut(arg, ":")
		time.Sleep(time.Duration(atoi(ms)) * time.Millisecond)
		return atoi(code)
	case "echo":
		// Stdin comes back on stdout and a marker on stderr, then the exit code the transport tests check.
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 2
		}
		fmt.Fprint(os.Stderr, "echo-err")
		return atoi(arg)
	case "say":
		fmt.Println(arg)
		return 0
	case "pwd":
		dir, err := os.Getwd()
		if err != nil {
			return 2
		}
		fmt.Println(dir)
		return 0
	case "spew":
		// A MiB of stdout, more than a pipe holds, then the marker file ARG, so a test sees the output never held it.
		if _, err := os.Stdout.Write(make([]byte, 1<<20)); err != nil {
			return 2
		}
		if err := os.WriteFile(arg, nil, 0o600); err != nil {
			return 2
		}
		time.Sleep(time.Minute)
		return 0
	}

	fmt.Fprintln(os.Stderr, "unknown child role:", spec)
	return 2
}

// sleepWhileParented ends early once the supervisor is gone, because a test's cleanup SIGKILLs it and would leave the sleep behind (SHARD-485).
func sleepWhileParented(d time.Duration) {
	parent := os.Getppid()
	// A parent of 1 is a supervisor already gone, unless the test binary itself is PID 1, as in docker.
	if parent == 1 && !testBinaryIsPID1() {
		return
	}
	deadline := time.After(d)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for os.Getppid() == parent {
		select {
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

func testBinaryIsPID1() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	pid1, err := os.Readlink("/proc/1/exe")

	return err == nil && pid1 == exe
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad number:", s)
		os.Exit(2)
	}

	return n
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out after %s while waiting for %s", timeout, what)
}

func TestRunRejectsBadArguments(t *testing.T) {
	const readyFlag, readyPath = "-ready-file", "/tmp/started"

	cases := map[string][]string{
		"no ready file":             {},
		"relative ready file":       {readyFlag, "started"},
		"a command":                 {readyFlag, readyPath, "--", "/bin/true"},
		"ready file eats --":        {readyFlag, "--", "/bin/true"},
		"transport with ready file": {"-transport", "unix:/tmp/x", readyFlag, readyPath},
		"transport with workdir":    {"-transport", "unix:/tmp/x", "-workdir", "/app"},
		"root with base":            {"-transport", "unix:/tmp/x", "-root", "/dev/vda", "-base", "/dev/vdb", "-overlay", "/dev/vdc"},
		"base with no overlay":      {"-transport", "unix:/tmp/x", "-base", "/dev/vda"},
		"overlay with no base":      {"-transport", "unix:/tmp/x", "-overlay", "/dev/vdb"},
		"root without transport":    {readyFlag, readyPath, "-root", "/dev/vda"},
		"base without transport":    {readyFlag, readyPath, "-base", "/dev/vda", "-overlay", "/dev/vdb"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Errorf("run(%q) returned no error", args)
			}
		})
	}
}

func TestTheBackoffDoublesUpToTheCap(t *testing.T) {
	policy := restartPolicy{backoff: time.Second}
	cases := map[int]time.Duration{0: time.Second, 1: 2 * time.Second, 5: 32 * time.Second, 6: 60 * time.Second, 100: 60 * time.Second}

	for started, want := range cases {
		if got := policy.wait(started); got != want {
			t.Errorf("wait(%d) = %s, want %s", started, got, want)
		}
	}
}

func TestParseRestartRefusesWhatItCannotRun(t *testing.T) {
	cases := map[string]struct {
		policy  string
		retries int
		backoff time.Duration
	}{
		"an unknown policy": {policy: "sometimes", backoff: time.Second},
		"negative retries":  {policy: "on-failure", retries: -1, backoff: time.Second},
		"a zero backoff":    {policy: "always"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRestart(c.policy, c.retries, c.backoff, time.Second); err == nil {
				t.Errorf("parseRestart(%q, %d, %s) returned no error", c.policy, c.retries, c.backoff)
			}
		})
	}
}

// The orphans stand in for reparented grandchildren, which macOS cannot produce without a subreaper.
func TestNoZombiesAfterManyChildren(t *testing.T) {
	g, _ := startGuest(t)

	pids := make([]int, 0, 100)
	g.run(func() {
		for range 100 {
			pid, err := startProcess(spawnSpec{argv: childArgv("sleep:300"), env: os.Environ()}, nil, false)
			if err != nil {
				t.Errorf("start an orphan: %v", err)

				return
			}
			pids = append(pids, pid)
		}
	})

	// A zombie still answers signal 0, so only ESRCH proves the reaper collected the pid.
	waitFor(t, 15*time.Second, "every orphan to be reaped", func() bool {
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return false
			}
		}

		return true
	})
}

// darwin can drop a SIGCHLD under load, so a death no signal announced must still reach the host (SHARD-481).
func TestADeathNoSignalAnnouncedIsStillReaped(t *testing.T) {
	g, report := startGuest(t)
	signal.Stop(g.childDeaths)
	if err := g.runSpec(supervisor.RunSpec{Name: "seven", Argv: childArgv("exit:7"), Env: os.Environ()}); err != nil {
		t.Fatal(err)
	}

	exit := report.awaitWithin(t, 5*reapEvery, "seven", models.ProcessExited)
	if exit.Exit == nil || exit.Exit.Code != 7 {
		t.Fatalf("exit = %+v, want code 7", exit.Exit)
	}
}

// The host resolves the name, so the supervisor only ever reads ids.
func TestCredentialOf(t *testing.T) {
	cases := map[string]struct {
		user   string
		groups []uint32
		want   *syscall.Credential
	}{
		"none":     {user: "", want: nil},
		"root":     {user: "0:0", groups: []uint32{0}, want: &syscall.Credential{Uid: 0, Gid: 0, Groups: []uint32{0}}},
		"a set":    {user: "1000:1000", groups: []uint32{1000, 10, 999}, want: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000, 10, 999}}},
		"the most": {user: "4294967295:4294967295", want: &syscall.Credential{Uid: 4294967295, Gid: 4294967295}},
		// An image that says nothing about the user drops the process to no supplementary group at all.
		"no groups": {user: "1000:1000", want: &syscall.Credential{Uid: 1000, Gid: 1000}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := credentialOf(c.user, c.groups)
			if err != nil {
				t.Fatalf("credentialOf(%q, %v): %v", c.user, c.groups, err)
			}
			if c.want == nil {
				if got != nil {
					t.Fatalf("got %+v, want no credential", got)
				}

				return
			}
			if got == nil {
				t.Fatalf("got no credential, want %+v", c.want)
			}
			if got.Uid != c.want.Uid || got.Gid != c.want.Gid || !slices.Equal(got.Groups, c.want.Groups) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
			// NoSetGroups false is what makes an empty set a setgroups(0, NULL) and not an inheritance.
			if got.NoSetGroups {
				t.Error("the credential skips setgroups, so the process keeps the group set of PID 1")
			}
		})
	}
}

func TestCredentialOfRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]struct {
		user   string
		groups []uint32
	}{
		"no gid":              {user: "1000"},
		"a name":              {user: "nobody:nobody"},
		"no ids":              {user: ":"},
		"an extra field":      {user: "1000:1000:10"},
		"an id past 32 bits":  {user: "4294967296:0"},
		"a negative id":       {user: "-1:0"},
		"groups with no user": {groups: []uint32{1000}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := credentialOf(c.user, c.groups); err == nil {
				t.Errorf("credentialOf(%q, %v) returned no error", c.user, c.groups)
			}
		})
	}
}

// The error pipe of a fork reads EOF on a death before the exec too, so the flag the kernel clears at the exec is the proof it ran (SHARD-505).
func TestStatExeced(t *testing.T) {
	line := func(name string, flags uint64) string {
		return fmt.Sprintf("42 (%s) Z 1 42 42 0 -1 %d 0 0 0 0 0 0 0 0 20 0 1 0", name, flags)
	}
	cases := map[string]struct {
		stat string
		want bool
	}{
		"an exec":                       {stat: line("sleep", 0x400000), want: true},
		"a death before the exec":       {stat: line("shard-init", 0x400000|pfForkNoExec), want: false},
		"a name that holds a stat line": {stat: line("a) Z 1 42 42 0 -1 64 (b", 0x400000), want: true},
		"a name that holds a ')'":       {stat: line("x) S 1", 0x400000|pfForkNoExec), want: false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := statExeced(c.stat)
			if err != nil {
				t.Fatalf("statExeced(%q): %v", c.stat, err)
			}
			if got != c.want {
				t.Errorf("statExeced(%q) = %t, want %t", c.stat, got, c.want)
			}
		})
	}
}

func TestStatExecedRefusesWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no name":          "42 S 1 42 42 0 -1 0",
		"no flags":         "42 (sleep) S 1 42 42 0",
		"unreadable flags": "42 (sleep) S 1 42 42 0 -1 x",
	}

	for name, stat := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := statExeced(stat); err == nil {
				t.Errorf("statExeced(%q) returned no error", stat)
			}
		})
	}
}

// The permitted set is the ceiling config.json granted the supervisor, and each process is handed
// exactly it: a uid change away from root clears the set the sandbox spec advertises.
func TestPermittedMask(t *testing.T) {
	const status = "Name:\tshard-init\nCapInh:\t00000000a80425fb\nCapPrm:\t00000000a80425fb\nCapEff:\t0000000000000000\n"

	mask, err := permittedMask(status)
	if err != nil {
		t.Fatalf("permittedMask: %v", err)
	}
	if want := uint64(0xa80425fb); mask != want {
		t.Errorf("got mask %#x, want %#x", mask, want)
	}

	// CAP_NET_BIND_SERVICE is 10, and it is the one a --user process most visibly loses without this.
	if got := capabilitiesIn(mask); !slices.Contains(got, uintptr(10)) {
		t.Errorf("got the capabilities %v, want CAP_NET_BIND_SERVICE among them", got)
	}
	if got := capabilitiesIn(0); got != nil {
		t.Errorf("an empty set gave %v, want none", got)
	}
}

func TestPermittedMaskRefusesAStatusItCannotRead(t *testing.T) {
	cases := map[string]string{
		"no field":     "Name:\tshard-init\n",
		"not a number": "CapPrm:\tnot-a-mask\n",
	}

	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := permittedMask(status); err == nil {
				t.Errorf("permittedMask(%q) returned no error", status)
			}
		})
	}
}

// A child that keeps the supervisor's own ids loses nothing, so it needs no ambient set at all.
func TestNoAmbientSetWithoutADrop(t *testing.T) {
	cases := map[string]*syscall.Credential{
		"no credential": nil,
		"still root":    {Uid: 0, Gid: 0},
	}

	for name, credential := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := inheritedCapabilities(credential)
			if err != nil {
				t.Fatalf("inheritedCapabilities: %v", err)
			}
			if got != nil {
				t.Errorf("got the ambient set %v, want none", got)
			}
		})
	}
}

// A VM's shard-init has no PATH of its own, so argv[0] resolves on the process's PATH and never on ours.
func TestLookPathUsesThePathOfTheProcess(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fixture the test executes
		t.Fatal(err)
	}
	t.Setenv("PATH", "/nonexistent")

	got, err := lookPath(spawnSpec{argv: []string{"tool"}, env: []string{"HOME=/", "PATH=/nonexistent:" + dir}})
	if err != nil || got != tool {
		t.Fatalf("lookPath = %q, %v; want %q", got, err, tool)
	}
	if _, err := lookPath(spawnSpec{argv: []string{"tool"}, env: []string{"PATH=/nonexistent"}}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("a missing tool resolved: %v", err)
	}
	if got, err := lookPath(spawnSpec{argv: []string{tool}}); err != nil || got != tool {
		t.Fatalf("an absolute argv[0] = %q, %v", got, err)
	}
}

// ForkExec changes into the workdir before the exec, so a relative argv[0] and a relative PATH entry mean the workdir.
func TestLookPathResolvesRelativeNamesAgainstTheWorkDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "server"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fixture the test executes
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	if got, err := lookPath(spawnSpec{argv: []string{"./bin/server"}, dir: dir}); err != nil || got != "./bin/server" {
		t.Fatalf("a relative argv[0] = %q, %v", got, err)
	}
	if got, err := lookPath(spawnSpec{argv: []string{"server"}, env: []string{"PATH=/nonexistent:bin"}, dir: dir}); err != nil || got != "bin/server" {
		t.Fatalf("a relative PATH entry = %q, %v", got, err)
	}
	if _, err := lookPath(spawnSpec{argv: []string{"./bin/server"}, dir: t.TempDir()}); err == nil {
		t.Fatal("a name outside the workdir resolved")
	}
}
