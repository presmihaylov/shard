//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// container is shard-init as runc starts it: fd 0 is the host's status file, and requests come over the abstract socket.
type container struct {
	proc   *supervisorProcess
	status string
	logs   string
	stderr string
}

var containers atomic.Int64

// startContainer runs this binary in container mode with fd 0 opened by flag, and waits for its ready file.
func startContainer(t *testing.T, flag int, args ...string) *container {
	t.Helper()

	// The process socket admits root alone, which is what the daemon's exec runs as.
	if os.Geteuid() != 0 {
		t.Skip("the process socket admits only root")
	}
	dir := t.TempDir()
	c := &container{status: filepath.Join(dir, "status.json"), logs: filepath.Join(dir, "logs"), stderr: filepath.Join(dir, "stderr")}
	status, err := os.OpenFile(c.status, os.O_CREATE|flag, 0o600)
	if err != nil {
		t.Fatalf("open the status file: %v", err)
	}
	stderr, err := os.Create(c.stderr)
	if err != nil {
		t.Fatalf("open the supervisor's stderr: %v", err)
	}
	addr := fmt.Sprintf("@shard-init-test/%d/%d", os.Getpid(), containers.Add(1))
	ready := filepath.Join(dir, "ready")

	cmd := exec.Command(selfBinary, append([]string{"-ready-file", ready}, args...)...)
	cmd.Env = append(os.Environ(), roleEnv+"="+roleSupervisor, testAddrEnv+"="+addr, testLogsEnv+"="+c.logs)
	cmd.Stdin, cmd.Stderr = status, stderr
	c.proc = startSupervisor(t, cmd)
	// The supervisor holds its own copies now.
	if err := errors.Join(status.Close(), stderr.Close()); err != nil {
		t.Fatalf("close the harness's ends: %v", err)
	}
	setRequestAddr(t, addr)
	waitFor(t, 15*time.Second, "the ready file", func() bool {
		_, err := os.Stat(ready)

		return err == nil
	})

	return c
}

func (c *container) run(t *testing.T, spec supervisor.RunSpec) {
	t.Helper()

	reply, err := forward(supervisor.Message{Kind: supervisor.KindRun, ID: 1, Run: &spec})
	if err != nil || reply.Kind != supervisor.KindDone {
		t.Fatalf("the run of %s answered %+v, %v; want done", spec.Name, reply, err)
	}
}

// table reads the last whole table on fd 0, as the host does; a write in progress reads as none yet.
func (c *container) table(t *testing.T) (models.ProcessTable, bool) {
	t.Helper()

	blob, err := os.ReadFile(c.status)
	if err != nil {
		t.Fatalf("read the status file: %v", err)
	}
	var table models.ProcessTable
	lines := bytes.Split(bytes.TrimSpace(blob), []byte{'\n'})
	if err := json.Unmarshal(lines[len(lines)-1], &table); err != nil {
		return table, false
	}

	return table, table.Kind == models.ProcessTableKind
}

// await waits until fd 0 holds name in state.
func (c *container) await(t *testing.T, name string, state models.ProcessState) models.ProcessReport {
	t.Helper()

	var found models.ProcessReport
	waitFor(t, 15*time.Second, fmt.Sprintf("%s to read %s on fd 0", name, state), func() bool {
		table, ok := c.table(t)
		if !ok {
			return false
		}
		for _, p := range table.Processes {
			if p.Name == name && p.State == state {
				found = p

				return true
			}
		}

		return false
	})

	return found
}

func (c *container) log(t *testing.T, name string) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(c.logs, supervisor.ProcessLogName(name)))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the log of %s: %v", name, err)
	}

	return string(blob)
}

// Each status lands on fd 0, an exit by signal as the shell's 128+n, and each process writes into its own log.
func TestContainerReportsEachProcessOnFd0(t *testing.T) {
	c := startContainer(t, os.O_WRONLY|os.O_APPEND)

	c.run(t, named("job", "exit:7"))
	c.run(t, named("victim", "sigkill:0"))
	c.run(t, named("hello", "say:hi"))

	if job := c.await(t, "job", models.ProcessExited); job.Exit == nil || job.Exit.Code != 7 || job.Exit.Signal != 0 {
		t.Fatalf("job ended as %+v, want code 7", job)
	}
	if victim := c.await(t, "victim", models.ProcessExited); victim.Exit == nil || victim.Exit.Code != 137 || victim.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("victim ended as %+v, want code 137 by signal 9", victim)
	}
	c.await(t, "hello", models.ProcessExited)
	if out := c.log(t, "hello"); out != "hi\n" {
		t.Fatalf("hello's log holds %q, want its line", out)
	}
	table, _ := c.table(t)
	if names := reportNames(table.Processes); strings.Join(names, ",") != "job,victim,hello" {
		t.Fatalf("fd 0 holds %q, want the three in the order they ran", names)
	}
}

// A stop forwards the TERM to every process and ends the supervisor once they are reaped.
func TestContainerTermEndsOnceEveryProcessIsReaped(t *testing.T) {
	c := startContainer(t, os.O_WRONLY|os.O_APPEND)
	c.run(t, named("web", "term:0"))
	waitFor(t, 15*time.Second, "web to take TERM", func() bool { return c.log(t, "web") == "ready\n" })

	if err := c.proc.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the supervisor: %v", err)
	}
	awaitCleanExit(testContext(t), t, c.proc, "the TERM")
}

// With nothing to forward the TERM to, a process that ended long ago or none at all, the supervisor ends at once.
func TestContainerTermWithNothingRunningEndsAtOnce(t *testing.T) {
	for name, specs := range map[string][]supervisor.RunSpec{"none": nil, "ended": {named("once", "exit:0")}} {
		t.Run(name, func(t *testing.T) {
			c := startContainer(t, os.O_WRONLY|os.O_APPEND)
			for _, spec := range specs {
				c.run(t, spec)
				c.await(t, spec.Name, models.ProcessExited)
			}

			if err := c.proc.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("signal the supervisor: %v", err)
			}
			awaitCleanExit(testContext(t), t, c.proc, "the TERM")
		})
	}
}

// A status the host can never read is reported on stderr, and the sandbox outlives it.
func TestContainerOutlivesALostFd0(t *testing.T) {
	c := startContainer(t, os.O_RDONLY)

	c.run(t, named("job", "exit:0"))
	waitFor(t, 15*time.Second, "the failed report on stderr", func() bool {
		blob, err := os.ReadFile(c.stderr)
		if err != nil {
			t.Fatalf("read the supervisor's stderr: %v", err)
		}

		return strings.Contains(string(blob), "report the process table on fd 0")
	})
	if err := c.proc.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the supervisor died on a lost status: %v", err)
	}
}

// SHARD-764: runc made the work directory under the daemon's umask 0077, so a user other than root could not enter it.
func TestContainerMakesTheWorkDirectory0755AndRunsThere(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	workDir := filepath.Join(t.TempDir(), "work", "deep")
	c := startContainer(t, os.O_WRONLY|os.O_APPEND, "-workdir", workDir)

	for _, dir := range []string{filepath.Dir(workDir), workDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s is %o, want 755", dir, info.Mode().Perm())
		}
	}
	spec := named("where", "pwd")
	spec.WorkDir = workDir
	c.run(t, spec)
	c.await(t, "where", models.ProcessExited)
	if got := strings.TrimSpace(c.log(t, "where")); got != workDir {
		t.Fatalf("the process ran in %q, want %q", got, workDir)
	}
}
