// Command shard-init is PID 1 in every sandbox: it runs the entrypoint, reaps children and stays up.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/memfd"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/termrelay"
	"github.com/presmihaylov/shard/services/supervisor"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -ready-file <path> [-user <uid>:<gid>] [-groups <gid>,...]
             [-restart no|on-failure|always] [-retries <n>] [-backoff <duration>] -- [<entrypoint> [args...]]
  shard-init -transport vsock [-root <device> | -base <device> -overlay <device>] [-console <device>] [-reboot]

The entrypoint exit status, the restart count and the end of the app are reported to fd 0, which the host holds; the guest cannot reach it.
SIGUSR1 cancels every start again and terms the entrypoint, SIGUSR2 kills it; the supervisor stays up for both.
With -transport the host sends the entrypoint over vsock, and the exit status goes back the same way.
-root boots one ext4 disk; -base and -overlay boot a read-only EROFS image under an overlay whose upper layer is the second disk.
-reboot ends the VM with a reboot instead of a power off, for a vmm such as firecracker that only exits on one.`

// errSupervisor marks a failure of our own bookkeeping, which the host reads back as an exit code.
var errSupervisor = errors.New("the supervisor failed")

// errNoEntrypoint marks a broken image, not a broken supervisor, so the two do not share an exit code.
var errNoEntrypoint = errors.New("the entrypoint did not start")

// errNoHost is a report with no host to take it; the kind that must land waits for the next connection's replay.
var errNoHost = errors.New("no host attached")

func init() {
	// The host traces the launch shim's main thread alone, and the relay's command dies with the thread that forked it.
	if len(os.Args) > 1 && (os.Args[1] == launch.Mode || os.Args[1] == termrelay.Mode) {
		runtime.LockOSThread()
	}
}

func main() {
	// The daemon runs [/.shard/init launch <dir> <argv>] as an exec's own process, to prove the command's execve took.
	if len(os.Args) > 1 && os.Args[1] == launch.Mode {
		os.Exit(runLaunch(os.Args[2:]))
	}
	// gVisor runs [/.shard/init terminal <dir> <argv>] as a terminal exec's own process, to give the command a guest pty.
	if len(os.Args) > 1 && os.Args[1] == termrelay.Mode {
		os.Exit(runTerminal(os.Args[2:]))
	}
	// The daemon runs [/.shard/init files] through an exec for one file operation, as the user that exec runs as.
	if len(os.Args) == 2 && os.Args[1] == supervisor.FilesMode {
		os.Exit(runFiles())
	}
	err := run(os.Args[1:])
	if err == nil {
		return
	}

	fmt.Fprintln(os.Stderr, "shard-init:", err)
	os.Exit(exitCodeFor(err))
}

// runLaunch returns only when the command did not start.
func runLaunch(args []string) int {
	return notStartedCode(launch.Shim(args))
}

// runTerminal returns the command's exit code, or a shell's code for one that never ran.
func runTerminal(args []string) int {
	code, err := termrelay.Relay(args)
	if err == nil {
		return code
	}

	return notStartedCode(err)
}

// notStartedCode is a shell's code for a command that never ran; the host has the errno already, so the code is enough here.
func notStartedCode(err error) int {
	var failed *launch.NotStartedError
	if !errors.As(err, &failed) {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
		return models.SupervisorFailedExitCode
	}
	if failed.NotFound() {
		return models.CommandNotFoundExitCode
	}

	return models.CommandNotExecutableExitCode
}

// The host reads this back with runsc wait, so a dead supervisor is diagnosable and not a mystery.
func exitCodeFor(err error) int {
	if errors.Is(err, errNoEntrypoint) {
		return models.EntrypointNotStartedExitCode
	}
	if errors.Is(err, errSupervisor) {
		return models.SupervisorFailedExitCode
	}

	return 1
}

func run(args []string) error {
	// Clear the dumpable flag first, so /proc/1/fd is root-owned before the entrypoint ever forks.
	if err := setUndumpable(); err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	flags := flag.NewFlagSet("shard-init", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(flags.Output(), usage) }
	readyFile := flags.String("ready-file", "", "file written once the entrypoint is forked")
	user := flags.String("user", "", "uid:gid the entrypoint drops to; the supervisor keeps its own ids")
	groups := flags.String("groups", "", "comma separated supplementary gids the entrypoint is given")
	policy := flags.String("restart", string(models.RestartNo), "when the entrypoint is started again: no, on-failure or always")
	retries := flags.Int("retries", 0, "how many starts again before the supervisor gives up, 0 for unlimited")
	backoff := flags.Duration("backoff", defaultBackoff, "the wait before the first start again; it doubles each time, up to a minute")
	reset := flags.Duration("restart-reset", defaultReset, "how long the entrypoint must run since its last start before an exit clears the count")
	transport := flags.String("transport", "", "vsock, or unix:<dir> in a test: the host sends the entrypoint, and every stream goes over it")
	root := flags.String("root", "", "the ext4 root disk to move onto before anything runs, with -transport")
	base := flags.String("base", "", "the read-only EROFS image to boot under an overlay, with -overlay and -transport")
	overlay := flags.String("overlay", "", "the ext4 disk the overlay's upper layer sits on, with -base")
	console := flags.String("console", "/dev/hvc0", "the console device the supervisor's stderr goes to once the root is in place")
	reboot := flags.Bool("reboot", false, "end the VM with a reboot instead of a power off, for a vmm that stays up after a power off")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	boot := guestBoot{Root: *root, Base: *base, Overlay: *overlay, Console: *console, Reboot: *reboot}
	if err := boot.check(); err != nil {
		return err
	}
	if *transport != "" {
		if flags.NArg() != 0 || *readyFile != "" {
			return errors.New("-transport takes the entrypoint from the host, so no -ready-file or arguments")
		}

		return serveTransport(*transport, boot)
	}
	if boot.set() {
		return errors.New("-root, -base and -overlay move onto a disk the host sends the entrypoint to, so they need -transport")
	}
	if *readyFile == "" {
		return errors.New("-ready-file is required")
	}
	if !filepath.IsAbs(*readyFile) {
		return fmt.Errorf("-ready-file must be an absolute path, got %q", *readyFile)
	}
	credential, err := parseCredential(*user, *groups)
	if err != nil {
		return err
	}

	restart, err := parseRestart(*policy, *retries, *backoff, *reset)
	if err != nil {
		return err
	}
	g := newGuest(&fileReporter{readyFile: *readyFile}, restart)
	err = g.launch(entrypoint{argv: flags.Args(), env: os.Environ(), credential: credential})
	if errors.Is(err, errNoEntrypoint) {
		return errors.Join(err, reportNotStarted(err))
	}
	if err == nil {
		err = g.supervise()
	}
	if errors.Is(err, errNoEntrypoint) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	return nil
}

// execErrnos are what execve(2) answers for a command that cannot run; a fork or a credential failure is none of them.
var execErrnos = []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.ENOEXEC, syscall.ENOTDIR, syscall.ELOOP, syscall.ENAMETOOLONG, syscall.EISDIR, syscall.ETXTBSY}

// unrunnable is a command the lookup or the kernel refused, apart from the supervisor's own setup failing.
type unrunnable struct{ err error }

func (u unrunnable) Error() string { return u.err.Error() }

func (u unrunnable) Unwrap() error { return u.err }

// reportNotStarted leaves the host the errno of an entrypoint that cannot run; any other failure is the supervisor's.
func reportNotStarted(err error) error {
	errno := execErrno(err)
	if errno == 0 {
		return nil
	}

	return writeReport(models.ExitReport{Kind: models.NotStartedReportKind, Errno: int(errno)})
}

// execErrno answers why the command could not run, or zero when what failed was not the command.
func execErrno(err error) syscall.Errno {
	var refused unrunnable
	if !errors.As(err, &refused) {
		return 0
	}
	if errors.Is(err, exec.ErrNotFound) {
		return syscall.ENOENT
	}

	var errno syscall.Errno
	// executable answers a directory or a file with no execute bit as fs.ErrPermission, which execve says as EACCES.
	if !errors.As(err, &errno) && errors.Is(err, fs.ErrPermission) {
		return syscall.EACCES
	}
	if slices.Contains(execErrnos, errno) {
		return errno
	}

	return 0
}

// entrypoint is the process the sandbox runs, as the host resolved it.
type entrypoint struct {
	argv       []string
	env        []string
	dir        string
	credential *syscall.Credential
	// out is where the entrypoint writes; nil keeps shard-init's own stdout and stderr, the log on gVisor.
	out *os.File
	// bound is the cgroup the child is born into; nil leaves it in shard-init's own.
	bound *os.File
}

// reporter is where ready, the exit record and the restart count go: fd 0 and the ready file on Linux, the control connection in a VM.
type reporter interface {
	ready() error
	exited(models.ExitStatus) error
	restarted(models.RestartCount) error
	// oomKilled says the sandbox hit its memory bound and every guest process is gone; errNoHost means nobody heard it yet.
	oomKilled() error
}

// death is one reaped child.
type death struct {
	pid  int
	exit models.ExitStatus
}

// guest is the supervisor's state. One goroutine owns it, and everything else reaches it over commands.
type guest struct {
	report  reporter
	restart restartPolicy
	// commands run on the owning goroutine, so a transport starts, signals and waits for children without a lock.
	commands chan func()
	// Separate channels, so a burst of child deaths can never push a stop signal out of the buffer.
	childDeaths chan os.Signal
	stopSignals chan os.Signal
	// appSignals stop the app and leave the sandbox up: USR1 terms it, USR2 kills it, and both cancel every start again.
	appSignals chan os.Signal

	ep            entrypoint
	entrypointPID int
	runStartedAt  time.Time
	count         models.RestartCount
	startAgain    <-chan time.Time
	stopping      bool
	// cancelled says the app was stopped, so no exit of it starts it again.
	cancelled bool
	// waiters are the exec sessions, each keyed by the pid it waits for.
	waiters map[int]chan<- models.ExitStatus
	// started and lastExit are what a new control connection is told first.
	started  bool
	lastExit *models.ExitStatus
	// oomProbe says whether the guest's own memory bound was hit; nil is a guest with no bound, where a SIGKILL is a signal.
	oomProbe func() (bool, error)
	// bound is the sandbox cgroup every child is born into, fixed before anything forks; nil off a VM.
	bound *os.File
	// oom says the bound took every guest process; the guest holds it until the host, with the reason on disk, says stop.
	oom bool
	// frozen names the verb that holds the bound frozen, nil while it runs; a child forked into it would hold this goroutine until the thaw.
	frozen atomic.Pointer[string]
}

// errFrozen is a start refused while a verb holds the bound frozen.
var errFrozen = errors.New("holds the sandbox frozen, and nothing starts in it until that ends: run the command again")

// frozenRetry is how often a restart due while the bound is frozen looks again.
const frozenRetry = 100 * time.Millisecond

// reapEvery backs up SIGCHLD, which darwin can drop under load, so a dead child waits at most this long (SHARD-481).
const reapEvery = time.Second

// newGuest watches for child deaths before anything forks, so no exit is ever missed.
func newGuest(report reporter, restart restartPolicy) *guest {
	g := &guest{
		report: report, restart: restart, commands: make(chan func()), waiters: map[int]chan<- models.ExitStatus{},
		childDeaths: make(chan os.Signal, 1), stopSignals: make(chan os.Signal, 4), appSignals: make(chan os.Signal, 4),
	}
	signal.Notify(g.childDeaths, syscall.SIGCHLD)
	signal.Notify(g.stopSignals, syscall.SIGTERM, syscall.SIGINT)
	signal.Notify(g.appSignals, syscall.SIGUSR1, syscall.SIGUSR2)

	return g
}

// launch starts the entrypoint once and says so, which is the host's only proof that it ran.
func (g *guest) launch(ep entrypoint) error {
	// Every exec defaults to the work directory, so it is made even when no command runs.
	if err := makeWorkDir(ep.dir); err != nil {
		return fmt.Errorf("%w: %w", errNoEntrypoint, err)
	}
	// The image's own command never runs, so with none the supervisor runs alone and is ready at once.
	if len(ep.argv) == 0 {
		g.started = true

		return g.report.ready()
	}

	// Stamp before the start so the fork and exec latency counts as run time, not lost from the healthy window.
	g.runStartedAt = time.Now()
	pid, err := g.start(ep, nil, false)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", errNoEntrypoint, ep.argv[0], err)
	}
	g.ep, g.entrypointPID, g.started = ep, pid, true

	if err := g.report.ready(); err != nil {
		return err
	}

	return nil
}

// supervise returns only after a stop signal, because a sandbox outlives its entrypoint and nothing
// else may end one. The host sends that signal, waits out the grace and then kills what is left.
func (g *guest) supervise() error {
	reap := time.NewTicker(reapEvery)
	defer reap.Stop()
	for {
		select {
		case <-g.childDeaths:
			if done := g.collect(); done {
				return nil
			}
		case <-reap.C:
			if done := g.collect(); done {
				return nil
			}
		case <-g.startAgain:
			// A refused restart would give up for good, so a restart due under a freeze waits it out.
			if g.frozen.Load() != nil {
				g.startAgain = time.After(frozenRetry)

				continue
			}
			g.startAgain = nil
			g.runStartedAt = time.Now()
			g.entrypointPID = g.restartEntrypoint()
		case received := <-g.stopSignals:
			if done, err := g.stop(received); done || err != nil {
				return err
			}
		case received := <-g.appSignals:
			// PID 1 must survive a failed signal, so it is reported and never fatal (AGENTS.md).
			if err := g.stopApp(received == syscall.SIGUSR2); err != nil {
				fmt.Fprintln(os.Stderr, "shard-init:", err)
			}
		case command := <-g.commands:
			command()
		}
	}
}

// collect reaps what died and routes each exit: the entrypoint's to the policy, an exec's to its session.
// A kill the memory bound made ends the guest, as it ends a whole Linux sandbox, after every exec has its exit.
func (g *guest) collect() bool {
	done := false
	for _, d := range collectDeadChildren() {
		if d.exit.Signal == int(syscall.SIGKILL) && g.oomProbe != nil && !done {
			oom, err := g.oomProbe()
			if err != nil {
				fmt.Fprintln(os.Stderr, "shard-init:", err)
			}
			done = oom
		}
		if waiter, ok := g.waiters[d.pid]; ok {
			delete(g.waiters, d.pid)
			waiter <- d.exit

			continue
		}
		if d.pid != g.entrypointPID || g.entrypointPID == 0 {
			continue
		}
		if done {
			g.entrypointPID = 0
			killGroup(d.pid)

			continue
		}

		// The guest PID space wraps at 65536, so stop watching the PID once it has been reaped.
		g.entrypointPID = 0
		g.lastExit = &d.exit
		// A sandbox outlives its entrypoint, so a lost exit status is reported and never fatal (AGENTS.md).
		if err := g.report.exited(d.exit); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init:", err)
		}
		// The exit status is written by now, so a stop that was waiting for it may finish.
		if g.stopping {
			return true
		}
		// Neither the next run nor an ended app keeps what this run left in its group, so a child that ignored the TERM ends here.
		killGroup(d.pid)
		if g.cancelled {
			g.end()

			continue
		}
		// A run that lasted the reset window starts the count over, so a rare crash never spends the retries.
		if time.Since(g.runStartedAt) >= g.restart.reset {
			g.count.Count = 0
		}
		g.startAgain = g.restart.schedule(d.exit, &g.count)
		if g.startAgain == nil {
			g.end()
		}
	}
	if !done {
		return false
	}
	// The guest stays up until the host has the reason on disk and says stop, so a host that missed the report reads the replay.
	g.oom = true
	if err := g.report.oomKilled(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}

	return false
}

// stop forwards the signal to the entrypoint. Nothing left to forward to ends the supervisor at once.
func (g *guest) stop(received os.Signal) (bool, error) {
	g.stopping = true
	if g.entrypointPID == 0 {
		return true, nil
	}
	if err := forwardToEntrypoint(g.entrypointPID, received); err != nil {
		return true, err
	}

	return false, nil
}

// stopApp cancels every start again and signals the app, and the sandbox stays up; a cancel in the backoff wait ends the app there.
func (g *guest) stopApp(force bool) error {
	if g.ep.argv == nil || g.count.Ended {
		return nil
	}
	g.cancelled = true
	if g.startAgain != nil {
		g.startAgain = nil
		g.end()

		return nil
	}
	// kill(0) reaches the process group that holds PID 1.
	if g.entrypointPID == 0 {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}

	return forwardToEntrypoint(g.entrypointPID, sig)
}

// end records that no start again follows the last exit, which is what a run waits for.
func (g *guest) end() {
	g.count.Ended = true
	g.record()
}

// restartEntrypoint forks it once more and records the count; an image that no longer starts is a give-up.
func (g *guest) restartEntrypoint() int {
	pid, err := g.startEntrypointAgain()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shard-init: start %q again: %v\n", g.ep.argv[0], err)
		g.count.GaveUp = true
		g.end()

		return 0
	}
	g.count.Count++
	g.count.LastAt = time.Now().UTC()
	g.record()

	return pid
}

// startEntrypointAgain is a start like the first, so the work directory is made again as docker makes it at every start.
func (g *guest) startEntrypointAgain() (int, error) {
	if err := makeWorkDir(g.ep.dir); err != nil {
		return 0, err
	}

	return g.start(g.ep, nil, false)
}

// A sandbox outlives its entrypoint, so a lost count is reported and never fatal (AGENTS.md).
func (g *guest) record() {
	if err := g.report.restarted(g.count); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}
}

// run hands a closure to the owning goroutine and waits for it, so a session reads consistent state.
func (g *guest) run(command func()) {
	done := make(chan struct{})
	g.commands <- func() {
		command()
		close(done)
	}
	<-done
}

// spawn starts one exec's process under the supervisor and returns the channel its exit arrives on.
func (g *guest) spawn(ep entrypoint, files []*os.File, tty bool) (int, <-chan models.ExitStatus, error) {
	var (
		pid  int
		err  error
		exit = make(chan models.ExitStatus, 1)
	)
	g.run(func() {
		pid, err = g.start(ep, files, tty)
		if err == nil {
			g.waiters[pid] = exit
		}
	})

	return pid, exit, err
}

// signal reaches only a process the supervisor started, so a guest pid the host guessed is refused.
func (g *guest) signal(pid int, sig syscall.Signal) error {
	// With no command the entrypoint pid is 0, and kill(0) or a negative pid reaches a process group that holds PID 1.
	if pid <= 0 {
		return fmt.Errorf("pid %d names a process group, not a process shard-init started", pid)
	}

	var err error
	g.run(func() {
		_, isExec := g.waiters[pid]
		if pid != g.entrypointPID && !isExec {
			err = fmt.Errorf("pid %d is not a process shard-init started", pid)

			return
		}
		err = syscall.Kill(pid, sig)
	})

	return err
}

// kill ends an exec the host let go of; a pid already reaped is nothing to kill, and never a reused one.
func (g *guest) kill(pid int) {
	g.run(func() {
		if _, isExec := g.waiters[pid]; !isExec {
			return
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			fmt.Fprintf(os.Stderr, "shard-init: kill exec %d: %v\n", pid, err)
		}
	})
}

// PID 1 in a namespace has no default disposition, so a stop is passed on, to the group the entrypoint leads with what it forked.
func forwardToEntrypoint(entrypointPID int, received os.Signal) error {
	unixSignal, ok := received.(syscall.Signal)
	if !ok {
		return fmt.Errorf("cannot forward signal %v to the entrypoint", received)
	}

	err := syscall.Kill(-entrypointPID, unixSignal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("forward %s to the entrypoint: %w", unixSignal, err)
}

// killGroup ends what a run of the app left in its group; PID 1 survives a failed kill, so it is reported and never fatal (AGENTS.md).
func killGroup(entrypointPID int) {
	if err := syscall.Kill(-entrypointPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(os.Stderr, "shard-init: kill the group of entrypoint %d: %v\n", entrypointPID, err)
	}
}

// It collects every dead child, not only the entrypoint: orphaned grandchildren land on PID 1.
func collectDeadChildren() []death {
	var deaths []death
	for {
		var waitStatus syscall.WaitStatus
		deadPID, err := syscall.Wait4(-1, &waitStatus, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return deaths
		}
		// PID 1 must survive, so an unexpected wait error is reported and the next SIGCHLD tries again.
		if err != nil {
			fmt.Fprintln(os.Stderr, "shard-init: wait for a child:", err)

			return deaths
		}
		// Zero means children are alive but none has died, so nothing is left to collect right now.
		if deadPID <= 0 {
			return deaths
		}

		deaths = append(deaths, death{pid: deadPID, exit: exitStatusFrom(waitStatus)})
	}
}

// A signalled entrypoint has no exit code of its own, so report the 128+n that a shell reports.
func exitStatusFrom(waitStatus syscall.WaitStatus) models.ExitStatus {
	if waitStatus.Signaled() {
		return models.ExitStatus{Code: 128 + int(waitStatus.Signal()), Signal: int(waitStatus.Signal())}
	}

	return models.ExitStatus{Code: waitStatus.ExitStatus()}
}

// fileReporter is the gVisor transport: the ready file under the bind mount, and one record on fd 0 that holds the exit and the count.
type fileReporter struct {
	readyFile string
	record    models.ExitReport
}

// The host has no other proof the entrypoint ran, so a supervisor that cannot say so is a failure.
func (r *fileReporter) ready() error {
	if err := store.WriteFile(r.readyFile, nil, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", r.readyFile, err)
	}

	return nil
}

// oomKilled never reaches a file: a Linux sandbox dies whole with its cgroup, and the host reads the cgroup's own count.
func (*fileReporter) oomKilled() error {
	return errors.New("a file reporter has no memory bound to report a kill under")
}

func (r *fileReporter) exited(exit models.ExitStatus) error {
	r.record.Kind, r.record.Code, r.record.Signal = models.ExitReportKind, exit.Code, exit.Signal

	return writeReport(r.record)
}

// writeReport frames one record onto fd 0, the channel the host holds; the newlines let a reader take whole lines only.
func writeReport(report models.ExitReport) error {
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal the %s report: %w", report.Kind, err)
	}

	sealed, err := memfd.Fixed(os.Stdin)
	if err != nil {
		return fmt.Errorf("read the seals of fd 0: %w", err)
	}
	if sealed {
		return writePage(os.Stdin, encoded)
	}

	// One record at a time keeps the file under the host's read bound; a failed clear still appends, so the host reads the code.
	cleared := os.Stdin.Truncate(0)
	framed := append(append([]byte{'\n'}, encoded...), '\n')
	if _, err := os.Stdin.Write(framed); err != nil {
		return errors.Join(fmt.Errorf("report the %s record on fd 0: %w", report.Kind, err), cleared)
	}
	if cleared != nil {
		return fmt.Errorf("the %s record is on fd 0, but the records before it stay: %w", report.Kind, cleared)
	}

	return nil
}

// writePage fills the sealed page from offset 0 in one write, the record then NULs, since no write can resize it.
func writePage(f *os.File, encoded []byte) error {
	framed := append(append([]byte{'\n'}, encoded...), '\n')
	if len(framed) > models.ExitChannelSize {
		return fmt.Errorf("the exit record is %d bytes, past the %d byte channel", len(framed), models.ExitChannelSize)
	}

	page := make([]byte, models.ExitChannelSize)
	copy(page, framed)
	if _, err := f.WriteAt(page, 0); err != nil {
		return fmt.Errorf("report the exit status on fd 0: %w", err)
	}

	return nil
}

// The count only moves after an exit, so the record it rewrites already carries that exit.
func (r *fileReporter) restarted(count models.RestartCount) error {
	r.record.Restarts = count

	return writeReport(r.record)
}

// PID 1 keeps its own ids, so it can always report the exit and the restart count.
// The host resolved the name against the image rootfs, so only numbers ever reach these flags.
func parseCredential(user, groups string) (*syscall.Credential, error) {
	if user == "" {
		if groups != "" {
			return nil, fmt.Errorf("-groups %q names no user to give them to", groups)
		}

		return nil, nil
	}

	uidField, gidField, hasGroup := strings.Cut(user, ":")
	if !hasGroup {
		return nil, fmt.Errorf("-user must be uid:gid, got %q", user)
	}

	uid, err := parseID(uidField)
	if err != nil {
		return nil, fmt.Errorf("-user has an unreadable uid: %w", err)
	}

	gid, err := parseID(gidField)
	if err != nil {
		return nil, fmt.Errorf("-user has an unreadable gid: %w", err)
	}

	supplementary, err := parseGroups(groups)
	if err != nil {
		return nil, err
	}

	// NoSetGroups stays false, so the fork calls setgroups even for an empty set: an entrypoint that
	// drops to a user must never inherit the group set of PID 1, which is root.
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: supplementary}, nil
}

// parseGroups reads the supplementary set the host resolved out of the image's own group file.
func parseGroups(groups string) ([]uint32, error) {
	if groups == "" {
		return nil, nil
	}

	var out []uint32
	for field := range strings.SplitSeq(groups, ",") {
		gid, err := parseID(field)
		if err != nil {
			return nil, fmt.Errorf("-groups has an unreadable gid: %w", err)
		}

		out = append(out, gid)
	}

	return out, nil
}

// ParseUint with a bit size of 32 is the bound check: a uid the kernel cannot hold is not an id.
func parseID(field string) (uint32, error) {
	id, err := strconv.ParseUint(field, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an id: %w", field, err)
	}

	return uint32(id), nil
}

// start forks a guest process into the bound.
func (g *guest) start(ep entrypoint, files []*os.File, tty bool) (int, error) {
	if verb := g.frozen.Load(); verb != nil {
		return 0, fmt.Errorf("a %s %w", *verb, errFrozen)
	}
	ep.bound = g.bound

	return startProcess(ep, files, tty)
}

// ForkExec, not os/exec: an os/exec Wait would race the wait4(-1) that collects every other child.
// files are the child's fds, or nil for the entrypoint's: /dev/null and the log.
func startProcess(ep entrypoint, files []*os.File, tty bool) (int, error) {
	// The fork's chdir fails with the same ENOENT as a missing binary, and the binary is what its error names.
	if err := checkWorkDir(ep.dir); err != nil {
		return 0, err
	}
	binary, err := lookPath(ep)
	if err != nil {
		return 0, fmt.Errorf("look up %q: %w", ep.argv[0], unrunnable{err})
	}

	ambient, err := inheritedCapabilities(ep.credential)
	if err != nil {
		return 0, err
	}

	// The entrypoint must not inherit our fd 0: that is shard-init's exit channel to the host. It gets
	// an in-guest /dev/null instead, so a read returns EOF and the channel stays the supervisor's alone.
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s for the entrypoint stdin: %w", os.DevNull, err)
	}

	// The entrypoint keeps our own stdout and stderr, the output log: shard streams them through, not proxies.
	fds := []uintptr{devNull.Fd(), os.Stdout.Fd(), os.Stderr.Fd()}
	if ep.out != nil {
		fds = []uintptr{devNull.Fd(), ep.out.Fd(), ep.out.Fd()}
	}
	if files != nil {
		fds = []uintptr{files[0].Fd(), files[1].Fd(), files[2].Fd()}
	}

	pid, forkErr := syscall.ForkExec(binary, ep.argv, &syscall.ProcAttr{
		Dir:   ep.dir,
		Env:   ep.env,
		Files: fds,
		Sys:   sysProcAttr(ep.credential, ambient, tty, ep.bound),
	})
	// The child holds its own copy of fd 0 now, so our template is spent whichever way the fork went.
	closeErr := devNull.Close()
	if forkErr != nil {
		return 0, fmt.Errorf("fork and exec %q: %w", binary, unrunnable{forkErr})
	}
	// The fork succeeded, so a failed close of our own /dev/null copy must not end the sandbox (AGENTS.md).
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "shard-init: close the entrypoint stdin template:", closeErr)
	}

	// The exec's error pipe also reads EOF when the child dies before its exec, so only the kernel's flag proves the command ran (SHARD-505).
	ran, err := execed(pid)
	if err != nil {
		// The owning goroutine is the only reaper and it is busy here, so the pid cannot be reused yet.
		return 0, errors.Join(fmt.Errorf("prove %q started: %w", ep.argv[0], err), syscall.Kill(pid, syscall.SIGKILL))
	}
	if !ran {
		return 0, fmt.Errorf("%q died before its exec finished, so it never ran", ep.argv[0])
	}

	return pid, nil
}

// pfForkNoExec is the kernel's PF_FORKNOEXEC: set at the fork, cleared once an exec passes its point of no return.
const pfForkNoExec = 0x40

// statExeced reads the flags of a /proc/<pid>/stat line, counted from the last ')' since the name may hold spaces and parentheses.
func statExeced(stat string) (bool, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return false, fmt.Errorf("stat line %q names no process", stat)
	}
	// state, ppid, pgrp, session, tty_nr and tpgid come before the flags.
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 7 {
		return false, fmt.Errorf("stat line %q has no flags field", stat)
	}
	flags, err := strconv.ParseUint(fields[6], 10, 64)
	if err != nil {
		return false, fmt.Errorf("stat line %q: read the flags: %w", stat, err)
	}

	return flags&pfForkNoExec == 0, nil
}

// workDirError is a work directory a process cannot start in, which an exec never creates.
type workDirError struct {
	dir   string
	errno syscall.Errno
}

func (e *workDirError) Error() string { return launch.WorkDirReason(e.dir, e.errno) }

// makeWorkDir makes the work directory as docker does at a start: every missing level, root-owned, 0755.
func makeWorkDir(dir string) error {
	if dir == "" {
		return nil
	}
	//nolint:gosec // G301: docker's mode for a work directory, which the sandbox's own users enter.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("make the work directory %q: %w", dir, err)
	}

	return nil
}

// checkWorkDir refuses a work directory that is not there now, whether an exec named it or the sandbox lost it.
func checkWorkDir(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return &workDirError{dir: dir, errno: errno}
	}
	if err != nil {
		return fmt.Errorf("check the work directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return &workDirError{dir: dir, errno: syscall.ENOTDIR}
	}

	return nil
}

// lookPath resolves argv[0] on the entrypoint's own PATH, in the entrypoint's own directory: in a VM shard-init's environ is the kernel's, which has none.
func lookPath(ep entrypoint) (string, error) {
	// A VM mounts no /.shard/init, so the daemon's files exec there runs this binary.
	if ep.argv[0] == supervisor.InitPath {
		return selfBinary, nil
	}
	if strings.Contains(ep.argv[0], "/") {
		return executable(ep.dir, ep.argv[0])
	}
	for _, entry := range ep.env {
		if path, found := strings.CutPrefix(entry, "PATH="); found {
			return lookPathIn(ep.dir, ep.argv[0], path)
		}
	}

	return exec.LookPath(ep.argv[0])
}

func lookPathIn(workDir, name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate, err := executable(workDir, filepath.Join(dir, name))
		if err != nil {
			continue
		}

		return candidate, nil
	}

	return "", fmt.Errorf("%w in %q", exec.ErrNotFound, path)
}

// executable answers the path ForkExec runs from workDir, which is what a relative one means before the chdir.
func executable(workDir, name string) (string, error) {
	candidate := name
	if !filepath.IsAbs(name) && workDir != "" {
		candidate = filepath.Join(workDir, name)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s: %w", name, fs.ErrPermission)
	}

	return name, nil
}

// statusFile is where the kernel, and the sentry that emulates it, prints this process's own sets.
const statusFile = "/proc/self/status"

// permittedField names the set config.json granted the supervisor, which is the ceiling it may pass on.
const permittedField = "CapPrm:"

// inheritedCapabilities names what the entrypoint must be handed as ambient. A uid change away from
// root clears the permitted and the effective set, and config.json would then advertise a set the
// entrypoint never receives. A child that keeps our own ids keeps them without any of this.
func inheritedCapabilities(credential *syscall.Credential) ([]uintptr, error) {
	if credential == nil || credential.Uid == 0 {
		return nil, nil
	}

	blob, err := os.ReadFile(statusFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", statusFile, err)
	}

	mask, err := permittedMask(string(blob))
	if err != nil {
		return nil, err
	}

	return capabilitiesIn(mask), nil
}

// permittedMask pulls the permitted set out of the process status, where it is one hex word.
func permittedMask(status string) (uint64, error) {
	for line := range strings.Lines(status) {
		if !strings.HasPrefix(line, permittedField) {
			continue
		}

		value := strings.TrimSpace(strings.TrimPrefix(line, permittedField))

		mask, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("%s holds an unreadable %s %q: %w", statusFile, permittedField, value, err)
		}

		return mask, nil
	}

	return 0, fmt.Errorf("%s names no %s", statusFile, permittedField)
}

// capabilitiesIn turns the mask into the numbers prctl raises one at a time.
func capabilitiesIn(mask uint64) []uintptr {
	var caps []uintptr
	for bit := range uintptr(64) {
		if mask&(1<<bit) != 0 {
			caps = append(caps, bit)
		}
	}

	return caps
}
