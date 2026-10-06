//go:build linux

package termrelay

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/pty"
)

// defaultPath is the OCI image spec default, which execvp falls back to as well when the env names no PATH.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// drainIdle is how long the terminal may stay quiet once the command ended, since a background job can hold it open.
const drainIdle = 100 * time.Millisecond

// drainBudget bounds the whole drain, so a background job that keeps writing never holds the exec open.
const drainBudget = 2 * time.Second

// forwarded are the signals runsc and a host Signal send the relay, which belong to the command.
var forwarded = []os.Signal{unix.SIGTERM, unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2, unix.SIGCONT}

// Relay runs the command on a guest pty and returns its exit code, 128 plus the signal for one a signal ended. args is what Args put after Mode.
func Relay(args []string) (code int, err error) {
	if len(args) < 2 {
		return 0, errors.New("the terminal relay needs a work directory and a command")
	}
	workDir, argv := args[0], args[1:]

	unix.CloseOnExec(FD)
	// The chdir runs as the exec's user, as the runtime's own would, and a refusal there is never the command's.
	if err := unix.Chdir(workDir); err != nil {
		return 0, report(unentered, errnoOf(err))
	}

	pair, err := pty.Open()
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := pair.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if err := resizeFrom(os.Stdin, pair); err != nil {
		return 0, err
	}
	// The guest pty echoes and edits lines, so the host's tty passes every byte through untouched.
	if _, err := pty.MakeRaw(os.Stdin); err != nil {
		return 0, err
	}

	// Notify before the start, so no signal meant for the command ends the relay first.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, append([]os.Signal{unix.SIGWINCH}, forwarded...)...)

	child, errno := start(argv, pair.Replica)
	if errno != 0 {
		return 0, report(failed, errno)
	}
	replica := pair.Replica
	pair.Replica = nil
	if err := replica.Close(); err != nil {
		return 0, fmt.Errorf("close the relay's copy of the guest terminal: %w", err)
	}
	if _, err := unix.Write(FD, []byte{started}); err != nil {
		return 0, fmt.Errorf("tell the host the command started: %w", err)
	}
	if err := unix.Close(FD); err != nil {
		return 0, fmt.Errorf("close the record of the terminal relay: %w", err)
	}

	return relay(child, pair, signals)
}

// relay copies both ways and forwards signals until the command ends, then drains what it left on the terminal.
func relay(child *os.Process, pair *pty.Pty, signals <-chan os.Signal) (int, error) {
	// A host that went away is a terminal that hung up, which the command hears as SIGHUP.
	hangUp := make(chan struct{}, 2)
	go copyInput(pair.Master, os.Stdin, hangUp)

	ended := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		copyOutput(os.Stdout, pair.Master, ended, hangUp)
	}()

	waited := make(chan waitResult, 1)
	go func() {
		state, err := child.Wait()
		waited <- waitResult{state: state, err: err}
	}()

	for {
		var err error
		select {
		case sig := <-signals:
			err = pass(child, pair, sig)
		case <-hangUp:
			err = hangUpCommand(child)
		case result := <-waited:
			return drain(result, pair.Master, ended, drained)
		}
		if err != nil {
			return 0, err
		}
	}
}

type waitResult struct {
	state *os.ProcessState
	err   error
}

// drain gives the copy a bounded time for what the command left on the terminal, then reports how the command ended.
func drain(result waitResult, master *os.File, ended chan<- struct{}, drained <-chan struct{}) (int, error) {
	if result.err != nil {
		return 0, fmt.Errorf("wait for the command: %w", result.err)
	}

	close(ended)
	if err := master.SetReadDeadline(time.Now().Add(drainIdle)); err != nil {
		return 0, fmt.Errorf("bound the drain of the guest terminal: %w", err)
	}
	select {
	case <-drained:
	case <-time.After(drainBudget):
	}

	return exitCode(result.state), nil
}

// pass hands one signal to the command; SIGWINCH is the host tty's new size, which the guest pty takes on.
func pass(child *os.Process, pair *pty.Pty, sig os.Signal) error {
	if sig == unix.SIGWINCH {
		return resizeFrom(os.Stdin, pair)
	}
	// A command that just ended is the wait's to report.
	if err := child.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("pass %v to the command: %w", sig, err)
	}

	return nil
}

// hangUpCommand tells the command its terminal went away; one that just ended is the wait's to report.
func hangUpCommand(child *os.Process) error {
	if err := child.Signal(unix.SIGHUP); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("hang up the command: %w", err)
	}

	return nil
}

// copyInput feeds the host's keystrokes to the guest pty; its end, by EOF or by error, is the host hanging up.
func copyInput(master *os.File, host io.Reader, hangUp chan<- struct{}) {
	buf := make([]byte, 32<<10)
	for {
		n, err := host.Read(buf)
		if werr := forward(master, buf[:n]); werr != nil || err != nil {
			break
		}
	}
	hangUp <- struct{}{}
}

// copyOutput copies the guest pty out until no process holds it, or it idles once the command ended.
func copyOutput(host io.Writer, master *os.File, ended <-chan struct{}, hangUp chan<- struct{}) {
	sink := &hostSink{host: host, hangUp: hangUp}
	buf := make([]byte, 32<<10)
	for {
		n, err := master.Read(buf)
		sink.write(buf[:n])
		if err != nil || extendDrain(master, ended) != nil {
			return
		}
	}
}

// hostSink tells a host that hung up once, then drops the rest so the command never blocks on it.
type hostSink struct {
	host   io.Writer
	hangUp chan<- struct{}
	hungUp bool
}

func (s *hostSink) write(chunk []byte) {
	if s.hungUp || forward(s.host, chunk) == nil {
		return
	}
	s.hungUp = true
	s.hangUp <- struct{}{}
}

// forward writes what one read returned; a read that returned nothing writes nothing.
func forward(dst io.Writer, chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	_, err := dst.Write(chunk)

	return err
}

// extendDrain pushes the idle out on more output once the command ended, until the budget runs out.
func extendDrain(master *os.File, ended <-chan struct{}) error {
	select {
	case <-ended:
		return master.SetReadDeadline(time.Now().Add(drainIdle))
	default:
		return nil
	}
}

func resizeFrom(host *os.File, pair *pty.Pty) error {
	size, err := pty.SizeOf(host)
	if err != nil {
		return err
	}

	return pair.Resize(size)
}

// start runs the command as the session leader of the guest pty, searching PATH the way execvp does.
func start(argv []string, replica *os.File) (*os.Process, syscall.Errno) {
	name := argv[0]
	if strings.Contains(name, "/") {
		return spawn(name, argv, replica)
	}

	dirs, ok := os.LookupEnv("PATH")
	if !ok {
		dirs = defaultPath
	}

	denied := false
	for dir := range strings.SplitSeq(dirs, ":") {
		if dir == "" {
			continue
		}

		candidate := path.Join(dir, name)
		// A stat first keeps a miss from forking a session that takes the terminal.
		info, err := os.Stat(candidate)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			continue
		}
		if err != nil || info.IsDir() {
			denied = true

			continue
		}

		child, errno := spawn(candidate, argv, replica)
		if errno == 0 {
			return child, 0
		}
		if errno == unix.EACCES {
			denied = true

			continue
		}
		// A missing interpreter is ENOENT too, and execvp moves on to the next entry for it.
		if errno != unix.ENOENT && errno != unix.ENOTDIR {
			return nil, errno
		}
	}

	if denied {
		return nil, unix.EACCES
	}

	return nil, unix.ENOENT
}

func spawn(name string, argv []string, replica *os.File) (*os.Process, syscall.Errno) {
	child, err := os.StartProcess(name, argv, &os.ProcAttr{
		Files: []*os.File{replica, replica, replica},
		// The command dies with the relay, so the SIGKILL of a cancel ends both.
		Sys: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Pdeathsig: unix.SIGKILL},
	})
	if err != nil {
		return nil, errnoOf(err)
	}

	return child, 0
}

// report sends the host the errno that ended the launch and returns it as the relay's own failure.
func report(kind byte, errno syscall.Errno) error {
	notStarted := &launch.NotStartedError{Errno: errno, Chdir: kind == unentered}
	if _, err := unix.Write(FD, record(kind, errno)); err != nil {
		return errors.Join(notStarted, fmt.Errorf("report the errno to the host: %w", err))
	}

	return notStarted
}

func record(kind byte, errno syscall.Errno) []byte {
	return strconv.AppendUint([]byte{kind}, uint64(errno), 10)
}

// errnoOf is the errno of a failed exec or chdir; anything else reads as an exec format error, as the launch shim's does.
func errnoOf(err error) syscall.Errno {
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return errno
	}

	return unix.ENOEXEC
}

// exitCode is what a shell reports for the command: its code, or 128 plus the signal that ended it.
func exitCode(state *os.ProcessState) int {
	status, ok := state.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return 128 + int(status.Signal())
	}

	return state.ExitCode()
}
