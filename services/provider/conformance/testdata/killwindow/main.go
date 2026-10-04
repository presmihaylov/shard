//go:build linux

// Command killwindow leases a copy of itself, so an execve of the copy waits in its open, and kills the process that waits.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// targetName is the copy, which exits 0 at once under this name, as a command that started would.
const targetName = "target"

// budget is how long the lease waits for an opener; the kernel's own break time is 45 s, so this ends first.
const budget = 30 * time.Second

func main() {
	if filepath.Base(os.Args[0]) == targetName {
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: killwindow <dir>")
		os.Exit(2)
	}
	if err := run(filepath.Join(os.Args[1], targetName)); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}

func run(target string) error {
	// The break notice is SIGIO, which ends a process unless it is handled.
	signal.Ignore(unix.SIGIO)

	if err := copySelf(target); err != nil {
		return err
	}
	leased, err := os.Open(target)
	if err != nil {
		return err
	}
	defer leased.Close()

	if _, err := unix.FcntlInt(leased.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		return fmt.Errorf("take a write lease on %s: %w", target, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(leased.Fd()), &stat); err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	fmt.Println("leased")

	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		pid, err := waiter(stat.Ino)
		if err != nil {
			return err
		}
		if pid == 0 {
			time.Sleep(5 * time.Millisecond)

			continue
		}
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			return fmt.Errorf("kill %d, which waits to open %s: %w", pid, target, err)
		}
		fmt.Println("killed", pid)

		return nil
	}

	return fmt.Errorf("nothing opened %s within %s", target, budget)
}

func copySelf(target string) error {
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer self.Close()

	copied, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(copied, self); err != nil {
		return errors.Join(fmt.Errorf("copy this binary to %s: %w", target, err), copied.Close())
	}

	return copied.Close()
}

// waiter is the pid /proc/locks shows blocked on the lease of inode ino; a waiter line names no inode, only its lease's ordinal.
func waiter(ino uint64) (int, error) {
	locks, err := os.ReadFile("/proc/locks")
	if err != nil {
		return 0, err
	}
	suffix := ":" + strconv.FormatUint(ino, 10)
	ordinal := ""
	for line := range strings.Lines(string(locks)) {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[1] == "LEASE" && strings.HasSuffix(fields[5], suffix) {
			ordinal = fields[0]

			continue
		}
		if ordinal == "" || len(fields) < 6 || fields[0] != ordinal || fields[1] != "->" || fields[2] != "LEASE" {
			continue
		}

		return strconv.Atoi(fields[5])
	}

	return 0, nil
}
