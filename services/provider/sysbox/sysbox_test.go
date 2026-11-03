package sysbox_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/runc"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/provider/sysbox"
)

// newProvider wires a provider over a sysbox-runc that is never called: these tests refuse before they run one.
func newProvider(t *testing.T) *sysbox.Provider {
	t.Helper()

	return newProviderOver(t, "exit 1")
}

// newProviderOver stands a fake sysbox-runc up from a shell script, so a hang and a state are both scriptable.
func newProviderOver(t *testing.T, script string) *sysbox.Provider {
	t.Helper()

	return newProviderIn(t, t.TempDir(), script)
}

// newProviderIn keeps each sandbox's state under dir, so a test can lay a bundle out where the provider opens it.
func newProviderIn(t *testing.T, dir, script string) *sysbox.Provider {
	t.Helper()

	binary := filepath.Join(dir, "sysbox-runc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write the fake sysbox-runc: %v", err)
	}

	runner, err := runc.New(filepath.Join(dir, "root"), runc.WithBinary(binary))
	if err != nil {
		t.Fatalf("open the sysbox-runc runner: %v", err)
	}

	bundles, err := bundle.New("/usr/local/bin/shard-init")
	if err != nil {
		t.Fatalf("open the bundle service: %v", err)
	}

	p, err := sysbox.New(runner, bundles, func(id string) (string, error) { return filepath.Join(dir, id), nil })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := sysbox.New(nil, nil, nil); err == nil {
		t.Fatal("New accepted a provider with nothing to drive")
	}
}

func TestTheProviderNamesItsSubstrate(t *testing.T) {
	if got := newProvider(t).Name(); got != "sysbox" {
		t.Errorf("got name %q, want sysbox", got)
	}
}

func TestOnlyThePortIsClaimed(t *testing.T) {
	if got := newProvider(t).Capabilities(); got != (models.Capabilities{Port: true}) {
		t.Errorf("got capabilities %+v, want only port", got)
	}
}

// The checkpoint verbs refuse by name before sysbox-runc runs, so the cli can say which provider lacks what.
func TestEveryOptionalVerbRefusesByName(t *testing.T) {
	p := newProvider(t)
	spec := models.SandboxSpec{ID: "amber-otter-2c3d", StateDir: t.TempDir()}

	verbs := map[string]error{
		models.VerbPause:  p.Pause(t.Context(), "amber-otter-1a2b", t.TempDir()),
		models.VerbResume: p.Resume(t.Context(), "amber-otter-1a2b", t.TempDir()),
		models.VerbFork:   p.Fork(t.Context(), "amber-otter-1a2b", spec),
	}

	for verb, err := range verbs {
		var unsupported *models.UnsupportedError
		if !errors.As(err, &unsupported) {
			t.Errorf("%s returned %v, want an UnsupportedError", verb, err)
			continue
		}
		if unsupported.Provider != "sysbox" || unsupported.Verb != verb {
			t.Errorf("%s refused as %+v, want provider sysbox and verb %s", verb, unsupported, verb)
		}
	}
}

func TestAWedgedRuncFailsWithTheContextRatherThanHanging(t *testing.T) {
	p := newProviderOver(t, "sleep 60")

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := p.Status(ctx, "amber-otter-1a2b"); err == nil {
		t.Fatal("Status answered for a sysbox-runc that never replied")
	}

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("Status took %s, so the context did not cut the sysbox-runc call short", elapsed)
	}
}

// A sandbox sysbox-runc never heard of reads as absent, not as an error, so a stale record can be read past.
func TestStatusOfAnUnknownSandboxIsAbsent(t *testing.T) {
	p := newProviderOver(t, `echo 'container "amber-otter-1a2b" does not exist' >&2; exit 1`)

	status, err := p.Status(t.Context(), "amber-otter-1a2b")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Exists || status.Alive() {
		t.Errorf("got %+v, want a sandbox that does not exist", status)
	}
}

// Exec refuses anything but a running sandbox, because runc reports its own startup failures as exit 1.
func TestExecTakesOnlyARunningSandbox(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)

	_, err := p.Exec(t.Context(), "amber-otter-1a2b", models.ExecSpec{Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Exec in a stopped sandbox returned %v, want a refusal that names the state", err)
	}

	_, err = p.Exec(t.Context(), "amber-otter-1a2b", models.ExecSpec{})
	if err == nil || !strings.Contains(err.Error(), "no command") {
		t.Errorf("Exec with no argv returned %v, want a refusal", err)
	}
}

// sysbox-runc opens the guest's passwd and group before every exec, whatever the user, so a fifo there stalls an exec that names nobody (SHARD-653).
// The guest root is sysbox-runc's own overlay, so the check reads it through PID 1 and never the host's stale view of the layers.
func TestExecRefusesAUserDatabaseThatIsNotAFileWhenNoUserIsNamed(t *testing.T) {
	const id = "amber-otter-1a2b"
	dir := t.TempDir()
	p := newProviderIn(t, dir, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	stale := liveBundle(t, filepath.Join(dir, id))
	if err := os.WriteFile(filepath.Join(stale, "etc/group"), []byte("root:x:0:\n"), 0o600); err != nil {
		t.Fatalf("write the stale group: %v", err)
	}
	guest := placeInit(t, p, dir, id)
	if err := syscall.Mkfifo(filepath.Join(guest, "etc/group"), 0o600); err != nil {
		t.Fatalf("make the group fifo: %v", err)
	}

	_, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"true"}})
	refused, ok := errors.AsType[*bundle.UserDatabaseError](err)
	if !ok || !strings.HasPrefix(refused.Public(), "/etc/group is a named pipe") {
		t.Fatalf("Exec over a fifo group returned %v, want a user database refusal that names /etc/group", err)
	}
}

// A running state whose PID 1 its cgroup does not hold has no guest root to check, so the exec is refused.
func TestExecRefusesASandboxWhosePid1IsNotInItsCgroup(t *testing.T) {
	const id = "amber-otter-1a2b"
	dir := t.TempDir()
	p := newProviderIn(t, dir, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	liveBundle(t, filepath.Join(dir, id))
	p.SetCgroupRoot(filepath.Join(dir, "cgroup"))

	_, err := p.Exec(t.Context(), id, models.ExecSpec{Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "no PID 1") {
		t.Fatalf("Exec with no PID 1 in the cgroup returned %v, want a refusal that names PID 1", err)
	}
}

// placeInit puts PID 42 in the sandbox's cgroup and gives it a guest root under a fake /proc, and returns that root.
func placeInit(t *testing.T, p *sysbox.Provider, dir, id string) string {
	t.Helper()

	cgroupRoot := filepath.Join(dir, "cgroup")
	cg := filepath.Join(cgroupRoot, bundle.CgroupsPath(id))
	if err := os.MkdirAll(cg, 0o750); err != nil {
		t.Fatalf("create the cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte("42\n"), 0o600); err != nil {
		t.Fatalf("write cgroup.procs: %v", err)
	}
	p.SetCgroupRoot(cgroupRoot)

	procRoot := filepath.Join(dir, "proc")
	guest := filepath.Join(procRoot, "42", "root")
	if err := os.MkdirAll(filepath.Join(guest, "etc"), 0o750); err != nil {
		t.Fatalf("create the guest root: %v", err)
	}
	p.SetProcRoot(procRoot)

	return guest
}

// liveBundle lays out the config.json and rootfs an exec reads, and returns the rootfs.
func liveBundle(t *testing.T, stateDir string) string {
	t.Helper()

	rootfs := filepath.Join(stateDir, "bundle", "rootfs")
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create the rootfs: %v", err)
	}
	config := `{"process":{"args":["/usr/local/bin/shard-init"],"cwd":"/"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "bundle", "config.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	return rootfs
}

// A snapshot copies the layer, so a source that still writes it is refused before anything is copied.
func TestSnapshotRefusesASourceThatIsLive(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"running","pid":42}'`)
	dir := t.TempDir()

	err := p.Snapshot(t.Context(), "amber-otter-1a2b", dir)
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("Snapshot of a running source returned %v, want a refusal that names the stop", err)
	}
	if entries := readDir(t, dir); len(entries) != 0 {
		t.Errorf("Snapshot of a running source wrote into the snapshot: %v", entries)
	}
}

// A source sysbox-runc never held has no layers to copy, and the refusal comes before any write.
func TestSnapshotRefusesASourceThatWasNeverBuilt(t *testing.T) {
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
	dir := t.TempDir()

	if err := p.Snapshot(t.Context(), "amber-otter-1a2b", dir); err == nil {
		t.Error("Snapshot of a source with no bundle returned no error")
	}
	if entries := readDir(t, dir); len(entries) != 0 {
		t.Errorf("Snapshot of an empty source wrote into the snapshot: %v", entries)
	}
}

func readDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	return entries
}

// A nested container that hits its own bound counts only in the hierarchical memory.events, so a later clean exit is no OOM (SHARD-364).
func TestOnlyTheSandboxsOwnBoundIsAnOOM(t *testing.T) {
	const id = "amber-otter-1a2b"

	for name, tc := range map[string]struct {
		local string
		want  bool
	}{
		"a nested container's OOM": {local: "oom 0\noom_kill 0\n", want: false},
		"the sandbox's own OOM":    {local: "oom 1\noom_kill 1\n", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, bundle.CgroupsPath(id))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for file, body := range map[string]string{"memory.events": "oom 1\noom_kill 1\n", "memory.events.local": tc.local} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
			p.SetCgroupRoot(root)

			status, err := p.Status(t.Context(), id)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.OOMKilled != tc.want {
				t.Errorf("OOMKilled = %v, want %v", status.OOMKilled, tc.want)
			}
		})
	}
}

// A count that does not parse is a read to report, never a sandbox that no OOM ended (SHARD-615).
func TestAnOOMCountThatDoesNotParseFailsTheStatus(t *testing.T) {
	const id = "amber-otter-1a2b"

	root := t.TempDir()
	dir := filepath.Join(root, bundle.CgroupsPath(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("oom x\noom_kill 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newProviderOver(t, `echo '{"id":"amber-otter-1a2b","status":"stopped","pid":0}'`)
	p.SetCgroupRoot(root)

	if status, err := p.Status(t.Context(), id); err == nil {
		t.Errorf("Status over an OOM count that does not parse returned %+v, want an error", status)
	}
}

// HOME comes from the root PID 1 sees, never the host's stale view of the layers, as the user database check does (SHARD-752).
func TestExecOptionsCarryTheHomeOfTheExecUserFromTheGuest(t *testing.T) {
	cases := map[string]struct {
		spec models.ExecSpec
		want []string
	}{
		"nobody named": {spec: models.ExecSpec{}, want: []string{"HOME=/root"}},
		"a named user": {spec: models.ExecSpec{User: "build"}, want: []string{"HOME=/home/build"}},
		"a HOME given": {spec: models.ExecSpec{Env: []string{"HOME=/work"}}, want: []string{"HOME=/work"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			writeUsers(t, liveBundle(t, stateDir), "root:x:0:0::/stale:/bin/sh\nbuild:x:1000:1000::/stale:/bin/sh\n", guestGroup)
			guest := t.TempDir()
			writeUsers(t, guest, guestPasswd, guestGroup)
			b, err := bundle.Open(stateDir)
			if err != nil {
				t.Fatalf("open the bundle: %v", err)
			}

			c.spec.Argv = []string{"true"}
			opts, err := sysbox.ExecOptions(b, guest, c.spec)
			if err != nil {
				t.Fatalf("ExecOptions: %v", err)
			}
			if got := homes(opts.Env); !slices.Equal(got, c.want) {
				t.Errorf("the exec sets %q, want %q", got, c.want)
			}
		})
	}
}

const (
	guestPasswd = "root:x:0:0:root:/root:/bin/sh\nbuild:x:1000:1000::/home/build:/bin/sh\n"
	guestGroup  = "root:x:0:\nbuild:x:1000:\n"
)

// writeUsers gives a rootfs the passwd and group an exec resolves its user and HOME against.
func writeUsers(t *testing.T, rootfs, passwd, group string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatalf("create etc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/passwd"), []byte(passwd), 0o600); err != nil {
		t.Fatalf("write the passwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/group"), []byte(group), 0o600); err != nil {
		t.Fatalf("write the group: %v", err)
	}
}

// homes is every HOME an env sets, in order.
func homes(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool { return !strings.HasPrefix(entry, "HOME=") })
}

// A work directory the exec cannot enter is refused by name with docker's 126, never as a missing command.
func TestAnExecThatCannotEnterItsWorkDirectoryNamesIt(t *testing.T) {
	err := sysbox.NotStarted("amber-otter-1a2b", "/missing", &launch.NotStartedError{Errno: syscall.ENOENT, Chdir: true})

	var notStarted *models.CommandNotStartedError
	if !errors.As(err, &notStarted) {
		t.Fatalf("got %v, want a CommandNotStartedError", err)
	}
	if notStarted.Code != models.CommandNotExecutableExitCode {
		t.Errorf("the code is %d, want %d", notStarted.Code, models.CommandNotExecutableExitCode)
	}
	if want := `the work directory "/missing" does not exist`; notStarted.Reason != want {
		t.Errorf("the reason is %q, want %q", notStarted.Reason, want)
	}
}
