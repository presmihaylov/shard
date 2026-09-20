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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
)

const usage = `shard-init - the guest supervisor, PID 1 inside a sandbox

Usage:
  shard-init -ready-file <path> [-user <uid>:<gid>] [-groups <gid>,...]
             [-restart no|on-failure|always -restart-file <path> [-retries <n>] [-backoff <duration>]] -- <entrypoint> [args...]
  shard-init -transport vsock [-root <device>]

The entrypoint exit status is reported to fd 0, which the host holds; the guest cannot reach it.
With -transport the host sends the entrypoint over vsock, and the exit status goes back the same way.`

// errSupervisor marks a failure of our own bookkeeping, which the host reads back as an exit code.
var errSupervisor = errors.New("the supervisor failed")

// errNoEntrypoint marks a broken image, not a broken supervisor, so the two do not share an exit code.
var errNoEntrypoint = errors.New("the entrypoint did not start")

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}

	fmt.Fprintln(os.Stderr, "shard-init:", err)
	os.Exit(exitCodeFor(err))
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
	restartFile := flags.String("restart-file", "", "file the count of starts again is written to, as JSON")
	retries := flags.Int("retries", 0, "how many starts again before the supervisor gives up, 0 for unlimited")
	backoff := flags.Duration("backoff", defaultBackoff, "the wait before the first start again; it doubles each time, up to a minute")
	reset := flags.Duration("restart-reset", defaultReset, "how long the entrypoint must run since its last start before an exit clears the count")
	transport := flags.String("transport", "", "vsock, or unix:<dir> in a test: the host sends the entrypoint, and every stream goes over it")
	root := flags.String("root", "", "the root disk to move onto before anything runs, with -transport")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *transport != "" {
		if flags.NArg() != 0 || *readyFile != "" || *restartFile != "" {
			return errors.New("-transport takes the entrypoint from the host, so no -ready-file, -restart-file or arguments")
		}

		return serveTransport(*transport, *root)
	}
	if *readyFile == "" {
		return errors.New("-ready-file is required")
	}
	if !filepath.IsAbs(*readyFile) {
		return fmt.Errorf("-ready-file must be an absolute path, got %q", *readyFile)
	}
	if flags.NArg() == 0 {
		return errors.New("no entrypoint given")
	}

	credential, err := parseCredential(*user, *groups)
	if err != nil {
		return err
	}

	restart, err := parseRestart(*policy, *retries, *backoff, *reset)
	if err != nil {
		return err
	}
	if err := checkRestartFile(restart, *restartFile); err != nil {
		return err
	}

	g := newGuest(fileReporter{readyFile: *readyFile, restartFile: *restartFile}, restart)
	err = g.launch(entrypoint{argv: flags.Args(), env: os.Environ(), credential: credential})
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

// entrypoint is the process the sandbox runs, as the host resolved it.
type entrypoint struct {
	argv       []string
	env        []string
	dir        string
	credential *syscall.Credential
	// out is where the entrypoint writes; nil keeps shard-init's own stdout and stderr, the log on gVisor.
	out *os.File
}

// reporter is where ready, the exit record and the restart count go: files on gVisor, the control connection in a VM.
type reporter interface {
	ready() error
	exited(models.ExitStatus) error
	restarted(models.RestartCount) error
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
	// Two channels, so a burst of child deaths can never push a stop signal out of the buffer.
	childDeaths chan os.Signal
	stopSignals chan os.Signal

	ep            entrypoint
	entrypointPID int
	runStartedAt  time.Time
	count         models.RestartCount
	startAgain    <-chan time.Time
	stopping      bool
	// waiters are the exec sessions, each keyed by the pid it waits for.
	waiters map[int]chan<- models.ExitStatus
	// started and lastExit are what a new control connection is told first.
	started  bool
	lastExit *models.ExitStatus
}

// newGuest watches for child deaths before anything forks, so no exit is ever missed.
func newGuest(report reporter, restart restartPolicy) *guest {
	g := &guest{
		report: report, restart: restart, commands: make(chan func()), waiters: map[int]chan<- models.ExitStatus{},
		childDeaths: make(chan os.Signal, 1), stopSignals: make(chan os.Signal, 4),
	}
	signal.Notify(g.childDeaths, syscall.SIGCHLD)
	signal.Notify(g.stopSignals, syscall.SIGTERM, syscall.SIGINT)

	return g
}

// launch starts the entrypoint once and says so, which is the host's only proof that it ran.
func (g *guest) launch(ep entrypoint) error {
	// Stamp before the start so the fork and exec latency counts as run time, not lost from the healthy window.
	g.runStartedAt = time.Now()
	pid, err := startProcess(ep, nil, false)
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
	for {
		select {
		case <-g.childDeaths:
			if done := g.collect(); done {
				return nil
			}
		case <-g.startAgain:
			g.startAgain = nil
			g.runStartedAt = time.Now()
			g.entrypointPID = g.restartEntrypoint()
		case received := <-g.stopSignals:
			if done, err := g.stop(received); done || err != nil {
				return err
			}
		case command := <-g.commands:
			command()
		}
	}
}

// collect reaps what died and routes each exit: the entrypoint's to the policy, an exec's to its session.
func (g *guest) collect() bool {
	for _, d := range collectDeadChildren() {
		if waiter, ok := g.waiters[d.pid]; ok {
			delete(g.waiters, d.pid)
			waiter <- d.exit

			continue
		}
		if d.pid != g.entrypointPID || g.entrypointPID == 0 {
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
		// A run that lasted the reset window starts the count over, so a rare crash never spends the retries.
		if time.Since(g.runStartedAt) >= g.restart.reset {
			g.count.Count = 0
		}
		g.startAgain = g.restart.schedule(d.exit, &g.count)
		if g.count.GaveUp {
			g.record()
		}
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

// restartEntrypoint forks it once more and records the count; an image that no longer starts is a give-up.
func (g *guest) restartEntrypoint() int {
	pid, err := startProcess(g.ep, nil, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shard-init: start %q again: %v\n", g.ep.argv[0], err)
		g.count.GaveUp = true
		g.record()

		return 0
	}
	g.count.Count++
	g.count.LastAt = time.Now().UTC()
	g.record()

	return pid
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
		pid, err = startProcess(ep, files, tty)
		if err == nil {
			g.waiters[pid] = exit
		}
	})

	return pid, exit, err
}

// signal reaches only a process the supervisor started, so a guest pid the host guessed is refused.
func (g *guest) signal(pid int, sig syscall.Signal) error {
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

// PID 1 in a namespace has no default disposition, so a stop only works if we pass it on ourselves.
func forwardToEntrypoint(entrypointPID int, received os.Signal) error {
	unixSignal, ok := received.(syscall.Signal)
	if !ok {
		return fmt.Errorf("cannot forward signal %v to the entrypoint", received)
	}

	err := syscall.Kill(entrypointPID, unixSignal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return fmt.Errorf("forward %s to the entrypoint: %w", unixSignal, err)
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

// fileReporter is the gVisor transport: two files under the bind mount, and the exit record on fd 0.
type fileReporter struct {
	readyFile   string
	restartFile string
}

// The host has no other proof the entrypoint ran, so a supervisor that cannot say so is a failure.
func (r fileReporter) ready() error {
	if err := store.WriteFile(r.readyFile, nil, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", r.readyFile, err)
	}

	return nil
}

// exited frames the exit record onto fd 0, shard-init's host-held stdin the guest cannot reach.
// The newlines let a reader take whole lines only; shard-init is the sole writer, so appends never interleave.
func (fileReporter) exited(exit models.ExitStatus) error {
	report := models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal the exit report: %w", err)
	}

	framed := append(append([]byte{'\n'}, encoded...), '\n')
	if _, err := os.Stdin.Write(framed); err != nil {
		return fmt.Errorf("report the exit status on fd 0: %w", err)
	}

	return nil
}

func (r fileReporter) restarted(count models.RestartCount) error {
	return writeJSON(r.restartFile, "the restart count", count)
}

// A full disk is usually transient, so the budget is tens of seconds and not the length of one hiccup.
const (
	writeAttempts = 60
	writeBackoff  = 500 * time.Millisecond
)

// permanentErrnos names the faults no amount of waiting clears, so retrying them only delays the message.
var permanentErrnos = []syscall.Errno{
	syscall.EROFS, syscall.EACCES, syscall.EPERM, syscall.ENOENT, syscall.ENOTDIR, syscall.ENOTEMPTY,
}

// Retry first: a transient full disk must not cost the restart count of an otherwise healthy sandbox.
func writeJSON(path, what string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", what, err)
	}

	var last error
	for attempt := range writeAttempts {
		if attempt > 0 {
			time.Sleep(writeBackoff)
		}

		// pkg/store lands it through a random temp name and an fsync, so no planted path and no lost write.
		last = store.WriteFile(path, encoded, 0o600)
		if last == nil {
			return nil
		}
		if slices.ContainsFunc(permanentErrnos, func(code syscall.Errno) bool { return errors.Is(last, code) }) {
			return fmt.Errorf("write %s: %w", what, last)
		}
	}

	return fmt.Errorf("write %s after %d attempts: %w", what, writeAttempts, last)
}

// PID 1 keeps its own ids, so it can always report the exit and write the restart count as root.
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

// ForkExec, not os/exec: an os/exec Wait would race the wait4(-1) that collects every other child.
// files are the child's fds, or nil for the entrypoint's: /dev/null and the log.
func startProcess(ep entrypoint, files []*os.File, tty bool) (int, error) {
	binary, err := lookPath(ep)
	if err != nil {
		return 0, fmt.Errorf("look up %q: %w", ep.argv[0], err)
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
		Sys:   sysProcAttr(ep.credential, ambient, tty),
	})
	// The child holds its own copy of fd 0 now, so our template is spent whichever way the fork went.
	closeErr := devNull.Close()
	if forkErr != nil {
		return 0, fmt.Errorf("fork and exec %q: %w", binary, forkErr)
	}
	// The fork succeeded, so a failed close of our own /dev/null copy must not end the sandbox (AGENTS.md).
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "shard-init: close the entrypoint stdin template:", closeErr)
	}

	return pid, nil
}

// lookPath resolves argv[0] on the entrypoint's own PATH, in the entrypoint's own directory: in a VM shard-init's environ is the kernel's, which has none.
func lookPath(ep entrypoint) (string, error) {
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
