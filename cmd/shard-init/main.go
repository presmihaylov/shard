// Command shard-init is PID 1 in every sandbox: it runs the named processes the host asks for, reaps children and stays up.
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
	"github.com/presmihaylov/shard/pkg/pgroup"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/pkg/termrelay"
	"github.com/presmihaylov/shard/services/supervisor"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -ready-file <path> [-workdir <dir>]
  shard-init -transport vsock [-root <device> | -base <device> -overlay <device>] [-console <device>] [-reboot] [-swap <MiB>]
  shard-init process

PID 1 runs nothing of its own: the host starts named processes in it, and each one's status goes to fd 0, which the host holds; the guest cannot reach it.
On Linux the host runs "shard-init process" to start or stop one, with the request on stdin and the answer on stdout.
With -transport the host sends every request over vsock, and the statuses go back the same way.
-root boots one ext4 disk; -base and -overlay boot a read-only EROFS image under an overlay whose upper layer is the second disk.
-reboot ends the VM with a reboot instead of a power off, for a vmm such as firecracker that only exits on one.
-swap makes a swap file of that many MiB on the disk the root writes to at each boot, and a clean stop removes it.`

// errSupervisor marks a failure of our own bookkeeping, which the host reads back as an exit code.
var errSupervisor = errors.New("the supervisor failed")

// errNoHost is a report with no host to take it; the kind that must land waits for the next connection's replay.
var errNoHost = errors.New("no host attached")

func init() {
	// runc before 1.2 hands an exec the daemon's umask, 0077 under setup's unit, so every process here starts from docker's 0022 (SHARD-764).
	syscall.Umask(0o022)
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
	// The daemon runs [/.shard/init process] as root through an exec, to hand PID 1 one process request.
	if len(os.Args) == 2 && os.Args[1] == supervisor.ProcessMode {
		// Undumpable, no guest process without CAP_SYS_PTRACE traces the helper into sending a request of its own.
		if err := setUndumpable(); err != nil {
			fmt.Fprintln(os.Stderr, "shard-init:", err)
			os.Exit(1)
		}
		os.Exit(runRequest(os.Stdin, os.Stdout))
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
	if errors.Is(err, errSupervisor) {
		return models.SupervisorFailedExitCode
	}

	return 1
}

func run(args []string) error {
	// Clear the dumpable flag first, so /proc/1/fd is root-owned before any process forks.
	if err := setUndumpable(); err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	flags := flag.NewFlagSet("shard-init", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprintln(flags.Output(), usage) }
	readyFile := flags.String("ready-file", "", "file written once the supervisor takes process requests")
	workDir := flags.String("workdir", "", "the directory every process and exec starts in by default, made 0755 if it is missing")
	transport := flags.String("transport", "", "vsock, or unix:<dir> in a test: the host sends every request, and every stream goes over it")
	root := flags.String("root", "", "the ext4 root disk to move onto before anything runs, with -transport")
	base := flags.String("base", "", "the read-only EROFS image to boot under an overlay, with -overlay and -transport")
	overlay := flags.String("overlay", "", "the ext4 disk the overlay's upper layer sits on, with -base")
	console := flags.String("console", "/dev/hvc0", "the console device the supervisor's stderr goes to once the root is in place")
	reboot := flags.Bool("reboot", false, "end the VM with a reboot instead of a power off, for a vmm that stays up after a power off")
	swap := flags.Int64("swap", 0, "the swap file in MiB to make on the disk the root writes to at each boot, 0 for none")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("shard-init runs no command of its own, so it takes no arguments, got %q", flags.Args())
	}
	boot := guestBoot{Root: *root, Base: *base, Overlay: *overlay, Console: *console, Reboot: *reboot, SwapMiB: *swap}
	if err := boot.check(); err != nil {
		return err
	}
	if *transport != "" {
		if *readyFile != "" || *workDir != "" {
			return errors.New("-transport takes its setup from the host, so no -ready-file or -workdir")
		}

		return serveTransport(*transport, boot)
	}
	if boot.set() {
		return errors.New("-root, -base and -overlay move onto a disk the host sends requests to, so they need -transport")
	}
	if *readyFile == "" {
		return errors.New("-ready-file is required")
	}
	if !filepath.IsAbs(*readyFile) {
		return fmt.Errorf("-ready-file must be an absolute path, got %q", *readyFile)
	}

	return serveRequests(&fileReporter{readyFile: *readyFile, logs: guestLogs, outputs: map[string]*os.File{}}, *workDir)
}

// guestLogs is where a container PID 1 writes each process's output, under the shard mount the host reads.
var guestLogs = filepath.Join(filepath.Dir(supervisor.InitPath), supervisor.ProcessLogs)

// serveRequests is the whole of a container PID 1: set up, take requests on the process socket, and supervise until the stop.
func serveRequests(report *fileReporter, workDir string) error {
	g := newGuest(report)
	if err := g.setUp(workDir); err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}
	//nolint:gosec // G301: the host reads these as root, and no guest user may.
	if err := os.MkdirAll(report.logs, 0o700); err != nil {
		return fmt.Errorf("%w: make the log directory: %w", errSupervisor, err)
	}
	// Bound before the ready file, so a host that reads it never dials an empty socket.
	l, err := listenRequests()
	if err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}
	defer l.Close()
	go g.acceptRequests(l, peerIsHost)
	// The host has no other proof the supervisor came up, so one that cannot say so is a failure.
	if err := store.WriteFile(report.readyFile, nil, 0o600); err != nil {
		return fmt.Errorf("%w: write %s: %w", errSupervisor, report.readyFile, err)
	}
	if err := g.supervise(); err != nil {
		return fmt.Errorf("%w: %w", errSupervisor, err)
	}

	return nil
}

// unrunnable is a command the lookup or the kernel refused, apart from the supervisor's own setup failing.
type unrunnable struct{ err error }

func (u unrunnable) Error() string { return u.err.Error() }

func (u unrunnable) Unwrap() error { return u.err }

// spawnSpec is one process to start, as the host resolved it.
type spawnSpec struct {
	argv       []string
	env        []string
	dir        string
	credential *syscall.Credential
	// out is where a named process writes; an exec brings its own files.
	out *os.File
	// bound is the cgroup the child is born into; nil leaves it in shard-init's own.
	bound *os.File
}

// reporter is where the statuses go: fd 0 on Linux, the control connection in a VM; it also owns where each process writes.
type reporter interface {
	// changed carries one process's new status, and table every process the guest holds with it.
	changed(p models.ProcessReport, table []models.ProcessReport) error
	// oomKilled says the sandbox hit its memory bound and every guest process is gone; errNoHost means nobody heard it yet.
	oomKilled() error
	// output is where the process of that name writes; it stays open while keep names it.
	output(name string) (*os.File, error)
	// keep lets go of the output of every name not in names.
	keep(names []string)
}

// death is one reaped child.
type death struct {
	pid  int
	exit models.ExitStatus
}

// guest is the supervisor's state. One goroutine owns it, and everything else reaches it over commands.
type guest struct {
	report reporter
	// commands run on the owning goroutine, so a transport starts, signals and waits for children without a lock.
	commands chan func()
	// Separate channels, so a burst of child deaths can never push a stop signal out of the buffer.
	childDeaths chan os.Signal
	stopSignals chan os.Signal
	// due carries each timer that ran out, a start again or a stop's kill.
	due chan timerDue

	// procs are the named processes in the order they were run.
	procs []*proc
	// seq numbers every status, and gens every run's timers.
	seq      uint64
	gens     uint64
	stopping bool
	// waiters are the exec sessions, each keyed by the pid it waits for.
	waiters map[int]chan<- models.ExitStatus
	// ready says the guest took its setup, which a new control connection is told first.
	ready bool
	// oomProbe says whether an OOM kill took the guest; nil is a guest with no bound, where a SIGKILL is a signal.
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

// frozenRetry is how often a start again due while the bound is frozen looks again.
const frozenRetry = 100 * time.Millisecond

// reapEvery backs up SIGCHLD, which darwin can drop under load, so a dead child waits at most this long (SHARD-481).
const reapEvery = time.Second

// newGuest watches for child deaths before anything forks, so no exit is ever missed.
func newGuest(report reporter) *guest {
	g := &guest{
		report: report, commands: make(chan func()), waiters: map[int]chan<- models.ExitStatus{},
		childDeaths: make(chan os.Signal, 1), stopSignals: make(chan os.Signal, 4), due: make(chan timerDue, 2*models.MaxProcesses),
	}
	signal.Notify(g.childDeaths, syscall.SIGCHLD)
	signal.Notify(g.stopSignals, syscall.SIGTERM, syscall.SIGINT)

	return g
}

// setUp makes the work directory every process and exec defaults to, as docker does at a start, and marks the guest ready.
func (g *guest) setUp(workDir string) error {
	if err := makeWorkDir(workDir); err != nil {
		return err
	}
	g.ready = true

	return nil
}

// supervise returns only after a stop signal, because a sandbox outlives its processes and nothing
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
		case due := <-g.due:
			g.wake(due)
		case received := <-g.stopSignals:
			if done, err := g.stop(received); done || err != nil {
				return err
			}
		case command := <-g.commands:
			command()
		}
	}
}

// collect reaps what died and routes each exit: a named process's to its policy, an exec's to its session.
// A kill the memory bound made ends the guest, as it ends a whole Linux sandbox, after every exec has its exit.
func (g *guest) collect() bool {
	oom, reaped := false, false
	for _, d := range collectDeadChildren() {
		if d.exit.Signal == int(syscall.SIGKILL) && g.oomProbe != nil && !oom {
			hit, err := g.oomProbe()
			if err != nil {
				fmt.Fprintln(os.Stderr, "shard-init:", err)
			}
			oom = hit
		}
		if waiter, ok := g.waiters[d.pid]; ok {
			delete(g.waiters, d.pid)
			waiter <- d.exit

			continue
		}
		p := g.byPID(d.pid)
		if p == nil {
			continue
		}
		p.pid, reaped = 0, true
		if oom {
			killGroup(d.pid)
			p.release()

			continue
		}
		g.exited(p, d.pid, d.exit)
	}
	if !oom {
		// Only a process's reap ends a stop, so a kill that found nothing to forward to leaves the guest up for the cut.
		return reaped && g.stopping && !g.running()
	}
	// No process starts again after the bound took them all.
	for _, p := range g.procs {
		p.stopTimer(g)
	}
	// The guest stays up until the host has the reason on disk and says stop, so a host that missed the report reads the replay.
	g.oom = true
	if err := g.report.oomKilled(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}

	return false
}

// stop cancels every start again and forwards the signal to every process's group. Nothing left to forward to ends the supervisor at once.
func (g *guest) stop(received os.Signal) (bool, error) {
	g.stopping = true
	sig, ok := received.(syscall.Signal)
	if !ok {
		return true, fmt.Errorf("cannot forward signal %v to the processes", received)
	}
	var errs []error
	for _, p := range g.procs {
		p.stopTimer(g)
		if p.pid != 0 {
			errs = append(errs, signalGroup(p.pid, sig))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return true, err
	}

	return !g.running(), nil
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
func (g *guest) spawn(spec spawnSpec, files []*os.File, tty bool) (int, <-chan models.ExitStatus, error) {
	var (
		pid  int
		err  error
		exit = make(chan models.ExitStatus, 1)
	)
	g.run(func() {
		pid, err = g.start(spec, files, tty)
		if err == nil {
			g.waiters[pid] = exit
		}
	})

	return pid, exit, err
}

// signal reaches only a process the supervisor started, so a guest pid the host guessed is refused.
func (g *guest) signal(pid int, sig syscall.Signal) error {
	// kill(0) or a negative pid reaches a process group that holds PID 1.
	if pid <= 0 {
		return fmt.Errorf("pid %d names a process group, not a process shard-init started", pid)
	}

	var err error
	g.run(func() {
		_, isExec := g.waiters[pid]
		if g.byPID(pid) == nil && !isExec {
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

// PID 1 in a namespace has no default disposition, so a stop is passed on, to the group the process leads with what it forked.
func signalGroup(leader int, sig syscall.Signal) error {
	err := pgroup.Kill(leader, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("send %s to the group of process %d: %w", sig, leader, err)
}

// killGroup ends what a run left in its group; PID 1 survives a failed kill, so it is reported and never fatal (AGENTS.md).
func killGroup(leader int) {
	if err := signalGroup(leader, syscall.SIGKILL); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}
}

// It collects every dead child, not only the named processes: orphaned grandchildren land on PID 1.
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

// A signalled process has no exit code of its own, so report the 128+n that a shell reports.
func exitStatusFrom(waitStatus syscall.WaitStatus) models.ExitStatus {
	if waitStatus.Signaled() {
		return models.ExitStatus{Code: 128 + int(waitStatus.Signal()), Signal: int(waitStatus.Signal())}
	}

	return models.ExitStatus{Code: waitStatus.ExitStatus()}
}

// fileReporter is the container transport: the ready file and each process's log under the shard mount, and the process table on fd 0.
type fileReporter struct {
	readyFile string
	logs      string
	// outputs are the open logs, by process name, which only the guest's goroutine touches.
	outputs map[string]*os.File
}

// oomKilled never reaches a file: a Linux sandbox dies whole with its cgroup, and the host reads the cgroup's own count.
func (*fileReporter) oomKilled() error {
	return errors.New("a file reporter has no memory bound to report a kill under")
}

func (*fileReporter) changed(_ models.ProcessReport, table []models.ProcessReport) error {
	return writeReport(models.ProcessTable{Kind: models.ProcessTableKind, Processes: table})
}

// output appends, so a process run again and a sandbox started again add to the log the host bounds by copy and truncate.
func (r *fileReporter) output(name string) (*os.File, error) {
	if out, ok := r.outputs[name]; ok {
		return out, nil
	}
	path := filepath.Join(r.logs, supervisor.ProcessLogName(name))
	// O_NOFOLLOW: the log directory is the host's, but a guest root may still have planted a link in it.
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the log of %q: %w", name, err)
	}
	r.outputs[name] = out

	return out, nil
}

func (r *fileReporter) keep(names []string) {
	for name, out := range r.outputs {
		if slices.Contains(names, name) {
			continue
		}
		delete(r.outputs, name)
		// The process that wrote it holds its own copy, so a failed close loses nothing the host reads (AGENTS.md).
		if err := out.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "shard-init: close the log of %q: %v\n", name, err)
		}
	}
}

// writeReport frames one record onto fd 0, the channel the host holds; the newlines let a reader take whole lines only.
func writeReport(table models.ProcessTable) error {
	encoded, err := json.Marshal(table)
	if err != nil {
		return fmt.Errorf("marshal the process table: %w", err)
	}

	sealed, err := memfd.Fixed(os.Stdin)
	if err != nil {
		return fmt.Errorf("read the seals of fd 0: %w", err)
	}
	if sealed {
		return writePage(os.Stdin, encoded)
	}

	// One record at a time keeps the file under the host's read bound; a failed clear still appends, so the host reads the table.
	cleared := os.Stdin.Truncate(0)
	framed := append(append([]byte{'\n'}, encoded...), '\n')
	if _, err := os.Stdin.Write(framed); err != nil {
		return errors.Join(fmt.Errorf("report the process table on fd 0: %w", err), cleared)
	}
	if cleared != nil {
		return fmt.Errorf("the process table is on fd 0, but the records before it stay: %w", cleared)
	}

	return nil
}

// writePage fills the sealed page from offset 0 in one write, the record then NULs, since no write can resize it.
func writePage(f *os.File, encoded []byte) error {
	framed := append(append([]byte{'\n'}, encoded...), '\n')
	if len(framed) > models.ExitChannelSize {
		return fmt.Errorf("the process table is %d bytes, past the %d byte channel", len(framed), models.ExitChannelSize)
	}

	page := make([]byte, models.ExitChannelSize)
	copy(page, framed)
	if _, err := f.WriteAt(page, 0); err != nil {
		return fmt.Errorf("report the process table on fd 0: %w", err)
	}

	return nil
}

// credentialOf takes the ids the host resolved; an empty user keeps the supervisor's own, root, which it needs to report.
func credentialOf(user string, groups []uint32) (*syscall.Credential, error) {
	if user == "" {
		if len(groups) != 0 {
			return nil, fmt.Errorf("the groups %v name no user to give them to", groups)
		}

		return nil, nil
	}

	uidField, gidField, hasGroup := strings.Cut(user, ":")
	if !hasGroup {
		return nil, fmt.Errorf("the user must be uid:gid, got %q", user)
	}

	uid, err := parseID(uidField)
	if err != nil {
		return nil, fmt.Errorf("the user has an unreadable uid: %w", err)
	}

	gid, err := parseID(gidField)
	if err != nil {
		return nil, fmt.Errorf("the user has an unreadable gid: %w", err)
	}

	// NoSetGroups stays false, so the fork calls setgroups even for an empty set: a process that
	// drops to a user must never inherit the group set of PID 1, which is root.
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}, nil
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
func (g *guest) start(spec spawnSpec, files []*os.File, tty bool) (int, error) {
	if verb := g.frozen.Load(); verb != nil {
		return 0, fmt.Errorf("a %s %w", *verb, errFrozen)
	}
	spec.bound = g.bound

	return startProcess(spec, files, tty)
}

// ForkExec, not os/exec: an os/exec Wait would race the wait4(-1) that collects every other child.
// files are the child's fds, or nil for a named process's: /dev/null and its log.
func startProcess(ep spawnSpec, files []*os.File, tty bool) (int, error) {
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

	// No process may inherit our fd 0: that is shard-init's status channel to the host. It gets
	// an in-guest /dev/null instead, so a read returns EOF and the channel stays the supervisor's alone.
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s for a process's stdin: %w", os.DevNull, err)
	}

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
		fmt.Fprintln(os.Stderr, "shard-init: close the stdin template:", closeErr)
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

// lookPath resolves argv[0] on the process's own PATH, in its own directory: in a VM shard-init's environ is the kernel's, which has none.
func lookPath(ep spawnSpec) (string, error) {
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

// inheritedCapabilities names what a process must be handed as ambient. A uid change away from
// root clears the permitted and the effective set, and config.json would then advertise a set the
// process never receives. A child that keeps our own ids keeps them without any of this.
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
