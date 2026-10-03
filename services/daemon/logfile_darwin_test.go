//go:build darwin

package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// logChildEnv names the log a re-exec'd test binary writes to, so the test never moves its own stdout.
const logChildEnv = "SHARD_DAEMON_LOG_CHILD"

// A rotation renames the log aside and sends a SIGHUP: the next line lands in the fresh file, and the process stays up.
func TestALogRenamedAsideIsReopenedOnSIGHUP(t *testing.T) {
	if path := os.Getenv(logChildEnv); path != "" {
		logChild(path)
	}

	path := filepath.Join(t.TempDir(), "daemon.log")
	child := exec.Command(os.Args[0], "-test.run=^TestALogRenamedAsideIsReopenedOnSIGHUP$")
	child.Env = append(os.Environ(), logChildEnv+"="+path)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("open the child's stdin: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}

	awaitLine(t, path, "one")
	if err := os.Rename(path, path+".0"); err != nil {
		t.Fatalf("rename the log aside: %v", err)
	}
	if err := child.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}
	awaitLine(t, path, "daemon reopened "+path+" on SIGHUP")

	if err := stdin.Close(); err != nil {
		t.Fatalf("close the child's stdin: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("the child did not outlive the SIGHUP: %v", err)
	}

	rotated, err := os.ReadFile(path + ".0")
	if err != nil {
		t.Fatalf("read the rotated log: %v", err)
	}
	if string(rotated) != "one\n" {
		t.Errorf("the rotated log holds %q, want only the line before the SIGHUP", rotated)
	}
	fresh, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the fresh log: %v", err)
	}
	if !strings.HasSuffix(string(fresh), "two\n") {
		t.Errorf("the fresh log holds %q, want the line after the SIGHUP", fresh)
	}
}

// logChild is the daemon's half: it opens the log, reopens it on SIGHUP, and writes once more when stdin closes.
func logChild(path string) {
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	if err := openLog(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("one")

	go func() {
		if err := (logReopen{path: path, hangups: hangups, out: os.Stdout, reopen: openLog}).Run(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()

	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("two")
	os.Exit(0)
}

func awaitLine(t *testing.T, path, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no line %q in %s", want, path)
}
