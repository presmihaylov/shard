//go:build linux

package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// pidPoll is how often Await looks for the pid the runtime writes, which can land after the shim is ready.
const pidPoll = 2 * time.Millisecond

// yamaScope is the knob whose value 3 forbids every ptrace attach on the host.
const yamaScope = "/proc/sys/kernel/yama/ptrace_scope"

// pfExiting is the kernel's task flag for a process inside its exit.
const pfExiting = 0x4

// Channel is one launch. The runtime hands Guest to the shim as fd 3, and the host keeps the other end.
type Channel struct {
	host  *os.File
	guest *os.File
	// inode names this launch: once the shim is ready, only it holds the guest end at fd 3 under the pid the runtime wrote.
	inode uint64

	// mu hands a cancel the pidfd the trace opens once it proved which process the shim is; -1 is none yet.
	mu        sync.Mutex
	pidfd     int
	cancelled bool
}

// Open makes the channel for one launch.
func Open() (*Channel, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open a launch channel: %w", err)
	}

	// A nonblocking end goes in the poller, so a cancelled wait can end a read the runtime never answers.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		return nil, errors.Join(fmt.Errorf("make the launch channel nonblocking: %w", err), unix.Close(fds[0]), unix.Close(fds[1]))
	}

	var st unix.Stat_t
	if err := unix.Fstat(fds[1], &st); err != nil {
		return nil, errors.Join(fmt.Errorf("stat the launch channel: %w", err), unix.Close(fds[0]), unix.Close(fds[1]))
	}

	return &Channel{host: os.NewFile(uintptr(fds[0]), "launch"), guest: os.NewFile(uintptr(fds[1]), "launch-guest"), inode: st.Ino, pidfd: -1}, nil
}

// Guest is the end the runtime passes to the shim as its first preserved fd.
func (c *Channel) Guest() *os.File { return c.guest }

// CloseGuest drops the host's copy of the shim's end once the runtime holds its own.
func (c *Channel) CloseGuest() error {
	if err := c.guest.Close(); err != nil {
		return fmt.Errorf("close the guest end of the launch channel: %w", err)
	}

	return nil
}

// Close ends the launch; a shim still waiting for the host reads EOF and ends without running anything.
func (c *Channel) Close() error {
	var errs []error
	if err := c.guest.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		errs = append(errs, fmt.Errorf("close the guest end of the launch channel: %w", err))
	}
	if err := c.host.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close the launch channel: %w", err))
	}
	if err := c.unpin(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Await confirms execve; cancellation retains ownership until the traced shim ends.
func (c *Channel) Await(ctx context.Context, pidOf func() (int, error)) (pid int, err error) {
	// A shim the host never let go waits for the go byte, and the runtime waits on it; the hang-up ends both.
	defer func() { err = errors.Join(err, c.hangUp()) }()

	if err := c.awaitReady(ctx); err != nil {
		return 0, err
	}

	pid, err = pollPID(ctx, pidOf)
	if err != nil {
		return 0, err
	}

	type traced struct {
		pid int
		err error
	}
	done := make(chan traced, 1)
	go func() {
		// ptrace answers only the thread that attached, and a thread that ends locked takes every trace it holds off with it.
		runtime.LockOSThread()
		pid, err := c.trace(pid)
		done <- traced{pid, err}
	}()

	var t traced
	select {
	case t = <-done:
	case <-ctx.Done():
		// The trace hears only the shim, so the kill is what ends it.
		kerr := c.Kill()
		t = <-done
		// A shim the cancel killed never had its chance to start, so the cancel is the verdict.
		var notStarted *NotStartedError
		if errors.As(t.err, &notStarted) {
			t.err = fmt.Errorf("wait for the launch shim: %w", ctx.Err())
		}
		t.err = errors.Join(t.err, kerr)
	}

	return t.pid, t.err
}

func (c *Channel) hangUp() error {
	raw, err := c.host.SyscallConn()
	if err != nil {
		return fmt.Errorf("reach the launch channel: %w", err)
	}

	var serr error
	if err := raw.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_RDWR) }); err != nil {
		return fmt.Errorf("reach the launch channel: %w", err)
	}
	if serr != nil && !errors.Is(serr, unix.ENOTCONN) {
		return fmt.Errorf("hang up the launch channel: %w", serr)
	}

	return nil
}

// awaitReady waits for the shim's ready byte, which it sends once it is the process the runtime wrote down.
func (c *Channel) awaitReady(ctx context.Context) error {
	// The read outlives a cancelled wait until Close ends it.
	got := make(chan error, 1)
	go func() { got <- c.readReady() }()

	select {
	case err := <-got:
		return err
	case <-ctx.Done():
		return fmt.Errorf("wait for the launch shim: %w", ctx.Err())
	}
}

func (c *Channel) readReady() error {
	buf := make([]byte, 1)
	n, err := c.host.Read(buf)
	if errors.Is(err, io.EOF) {
		return ErrNoShim
	}
	if err != nil {
		return fmt.Errorf("wait for the launch shim: %w", err)
	}
	if n != 1 || buf[0] != ready {
		return fmt.Errorf("the launch shim sent %q where it says it is ready", buf[:n])
	}

	return nil
}

// pollPID waits for the pid file, which runc writes once its start returns, before or after the shim is ready.
func pollPID(ctx context.Context, pidOf func() (int, error)) (int, error) {
	for {
		pid, err := pidOf()
		if err == nil {
			return pid, nil
		}

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for the pid of the launch shim: %w", ctx.Err())
		case <-time.After(pidPoll):
		}
	}
}

// trace attaches to the shim, lets it go and follows it to its exec. Every path out leaves it untraced.
func (c *Channel) trace(pid int) (int, error) {
	if err := c.holds(pid); err != nil {
		return 0, err
	}

	if err := ptrace(unix.PTRACE_SEIZE, pid, unix.PTRACE_O_TRACEEXEC); err != nil {
		return 0, c.refused(pid, err)
	}

	// The trace pins the pid, so a second look proves the traced process is this launch's shim and no reuse of its pid.
	if err := c.holds(pid); err != nil {
		return 0, errors.Join(err, release(pid))
	}
	if err := c.pin(pid); err != nil {
		return 0, errors.Join(err, abandon(pid))
	}

	if _, err := c.host.Write([]byte{proceed}); err != nil {
		return 0, errors.Join(fmt.Errorf("let the launch shim %d go: %w", pid, err), abandon(pid))
	}

	return c.watch(pid)
}

// pin opens a pidfd on the shim while the trace holds it, so a Kill until Close can never reach a reuse of its pid.
func (c *Channel) pin(pid int) error {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("open a pidfd on the launch shim %d: %w", pid, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.pidfd = pidfd
	if c.cancelled {
		return c.kill()
	}

	return nil
}

// Kill ends the pinned process, the shim or the command it became, or has the trace kill it the moment it pins one.
func (c *Channel) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelled = true
	if c.pidfd < 0 {
		return nil
	}

	return c.kill()
}

func (c *Channel) kill() error {
	if err := unix.PidfdSendSignal(c.pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill the launch shim: %w", err)
	}

	return nil
}

func (c *Channel) unpin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pidfd < 0 {
		return nil
	}

	err := unix.Close(c.pidfd)
	c.pidfd = -1
	if err != nil {
		return fmt.Errorf("close the pidfd of the launch shim: %w", err)
	}

	return nil
}

// holds proves pid is this launch's shim. A pid that names nothing, or a process without the channel, is a shim that is gone.
func (c *Channel) holds(pid int) error {
	link, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + strconv.Itoa(fd))
	if errors.Is(err, fs.ErrNotExist) {
		return &NotStartedError{}
	}
	// A tracer that is not root reads the fds of a process inside its exit as root's, so only the state tells.
	if errors.Is(err, fs.ErrPermission) {
		return ending(pid, err)
	}
	if err != nil {
		return fmt.Errorf("read the channel of the launch shim %d: %w", pid, err)
	}
	if link != "socket:["+strconv.FormatUint(c.inode, 10)+"]" {
		return &NotStartedError{}
	}

	return nil
}

// ending is a shim on its way out, or the denial when pid is a live process whose fds this host may not read.
func ending(pid int, denied error) error {
	fields, err := procStat(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return &NotStartedError{}
	}
	if err != nil {
		return err
	}

	flags, err := strconv.ParseUint(fields[6], 10, 64)
	if err != nil {
		return fmt.Errorf("read the flags of the launch shim %d: %w", pid, err)
	}
	if fields[0] == "Z" || fields[0] == "X" || flags&pfExiting != 0 {
		return &NotStartedError{}
	}

	return fmt.Errorf("read the channel of the launch shim %d: %w", pid, denied)
}

// procStat is /proc/<pid>/stat from the state on, which anyone may read; the name before it may hold any byte.
func procStat(pid int) ([]string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, fmt.Errorf("read the state of process %d: %w", pid, err)
	}

	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 7 {
		return nil, fmt.Errorf("the state of process %d has %d fields", pid, len(fields))
	}

	return fields, nil
}

// refused names why the attach failed. A zombie refuses with EPERM too, so only a shim still alive is a denial.
func (c *Channel) refused(pid int, err error) error {
	if errors.Is(err, unix.ESRCH) {
		return &NotStartedError{}
	}
	if !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("trace the launch shim %d: %w", pid, err)
	}
	if gone := c.holds(pid); gone != nil {
		return gone
	}

	scope, rerr := os.ReadFile(yamaScope)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return fmt.Errorf("%w: attach to the launch shim %d: %w; read %s: %w", ErrTraceDenied, pid, err, yamaScope, rerr)
	}
	if strings.TrimSpace(string(scope)) == "3" {
		return fmt.Errorf("%w: kernel.yama.ptrace_scope is 3, which forbids every attach", ErrTraceDenied)
	}

	return fmt.Errorf("%w: attach to the launch shim %d: %w", ErrTraceDenied, pid, err)
}

// watch follows the shim until the kernel reports its exec, or its end before one.
func (c *Channel) watch(pid int) (int, error) {
	for {
		ws, err := wait(pid)
		if err != nil {
			return 0, err
		}

		if ws.Exited() || ws.Signaled() {
			errno, err := c.errno()
			if err != nil {
				return 0, err
			}

			return 0, &NotStartedError{Errno: errno}
		}

		switch event(ws) {
		case unix.PTRACE_EVENT_EXEC:
			return detach(pid)
		case unix.PTRACE_EVENT_STOP:
			err = resumeStop(pid, ws.StopSignal())
		default:
			// A signal-delivery stop: pass the signal on, so the trace changes nothing the command sees.
			err = ptrace(unix.PTRACE_CONT, pid, int(ws.StopSignal()))
		}
		// ESRCH is a SIGKILL that took the shim out of its stop; the next wait sees its end.
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return 0, errors.Join(fmt.Errorf("resume the launch shim %d: %w", pid, err), abandon(pid))
		}
	}
}

// resumeStop keeps a group stop a stop, as the shim's real parent would see it, and resumes any other event stop.
func resumeStop(pid int, sig syscall.Signal) error {
	switch sig {
	case unix.SIGSTOP, unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU:
		return ptrace(unix.PTRACE_LISTEN, pid, 0)
	}

	return ptrace(unix.PTRACE_CONT, pid, 0)
}

// detach lets the command run on its own, and the host reports the launch only after it.
func detach(pid int) (int, error) {
	err := ptrace(unix.PTRACE_DETACH, pid, 0)
	if err == nil {
		return pid, nil
	}

	// ESRCH is a command killed after its exec, which still took; the runtime reaps it once the trace is off.
	if errors.Is(err, unix.ESRCH) {
		if err := release(pid); err != nil {
			return 0, err
		}

		return pid, nil
	}

	return 0, errors.Join(fmt.Errorf("detach from the command %d after its exec: %w", pid, err), abandon(pid))
}

// abandon ends a shim this launch proved its own and can no longer follow; the trace still pins the pid, so the kill reaches it alone.
func abandon(pid int) error {
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return errors.Join(fmt.Errorf("kill the launch shim %d: %w", pid, err), release(pid))
	}

	return release(pid)
}

// release takes the trace off a shim this launch gives up on: a stop is detached and an end is reaped, so the runtime gets it back.
func release(pid int) error {
	// A shim that is already dying refuses the interrupt, and the wait below sees its end either way.
	if err := ptrace(unix.PTRACE_INTERRUPT, pid, 0); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EIO) {
		return fmt.Errorf("interrupt the launch shim %d: %w", pid, err)
	}

	for {
		ws, err := wait(pid)
		if err != nil {
			return err
		}
		if ws.Exited() || ws.Signaled() {
			return nil
		}

		// A signal-delivery stop hands its signal back with the detach, so the shim still gets it.
		sig := 0
		if event(ws) == 0 {
			sig = int(ws.StopSignal())
		}

		err = ptrace(unix.PTRACE_DETACH, pid, sig)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("detach from the launch shim %d: %w", pid, err)
		}
	}
}

// errno reads the record the shim sent before it ended, if it sent one; the socket keeps it after the shim is gone.
func (c *Channel) errno() (syscall.Errno, error) {
	conn, err := c.host.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("read the launch shim's record: %w", err)
	}

	buf := make([]byte, 32)
	var n int
	var rerr error
	if err := conn.Read(func(fd uintptr) bool {
		n, rerr = unix.Read(int(fd), buf)

		return true
	}); err != nil {
		return 0, fmt.Errorf("read the launch shim's record: %w", err)
	}
	if errors.Is(rerr, unix.EAGAIN) {
		return 0, nil
	}
	if rerr != nil {
		return 0, fmt.Errorf("read the launch shim's record: %w", rerr)
	}

	return parseRecord(buf[:n]), nil
}

func wait(pid int) (unix.WaitStatus, error) {
	for {
		var ws unix.WaitStatus
		_, err := unix.Wait4(pid, &ws, unix.WALL, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("wait for the launch shim %d: %w", pid, err)
		}

		return ws, nil
	}
}

// event is the ptrace event of a stop, zero for a signal-delivery stop.
func event(ws unix.WaitStatus) int { return int(uint32(ws)>>16) & 0xff }

// ptrace issues one request whose argument goes in data, where SEIZE takes its options and CONT and DETACH a signal.
func ptrace(request, pid, data int) error {
	if _, _, errno := unix.Syscall6(unix.SYS_PTRACE, uintptr(request), uintptr(pid), 0, uintptr(data), 0, 0); errno != 0 {
		return errno
	}

	return nil
}
