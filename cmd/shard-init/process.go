package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// errTaken is a run of a name that still runs or waits to start again.
var errTaken = errors.New("a process of that name still runs")

// proc is one named process: what it runs, its policy, and where it stands.
type proc struct {
	name    string
	spec    spawnSpec
	restart restartPolicy
	// pid is the live process, zero between runs; the guest PID space wraps, so it is cleared once reaped.
	pid int
	// runStartedAt stamps the last start, which the reset window counts from.
	runStartedAt time.Time
	// count is the starts again since the last healthy run, which the backoff and the retries read.
	count  int
	status models.ProcessStatus
	seq    uint64
	// gen tells this run's timer from one a stop or a later start cancelled.
	gen   uint64
	timer *time.Timer
	// stopping marks a stop-process in flight, so the exit is a kill and never a start again.
	stopping bool
	// reaped are the stop-process requests that wait for the exit.
	reaped []chan struct{}
}

// timerDue is a backoff or a stop's grace that ran out, for one run of one process.
type timerDue struct {
	name string
	gen  uint64
	kill bool
}

// runSpec starts one named process as the host resolved it.
func (g *guest) runSpec(spec supervisor.RunSpec) error {
	if !models.ValidProcessName(spec.Name) {
		return fmt.Errorf("the process name %q is not lowercase letters, digits, '.', '_' or '-', at most %d long", spec.Name, models.MaxProcessName)
	}
	if len(spec.Argv) == 0 {
		return fmt.Errorf("the run of %q names no command", spec.Name)
	}
	credential, err := execCredential(supervisor.ExecHeader{User: spec.User, Groups: spec.Groups, Lookup: spec.Lookup})
	if err != nil {
		return err
	}
	env, err := withHome("/", spec.Env, credential)
	if err != nil {
		return err
	}
	restart, err := parseRestart(string(nonEmpty(spec.Restart, models.RestartNo)), spec.Retries, nonEmpty(spec.Backoff, defaultBackoff), nonEmpty(spec.Reset, defaultReset))
	if err != nil {
		return err
	}

	g.run(func() {
		err = g.runProcess(spec.Name, spawnSpec{argv: spec.Argv, env: env, dir: spec.WorkDir, credential: credential}, restart)
	})

	return err
}

func nonEmpty[T comparable](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}

	return value
}

// runProcess starts one named process and records it; a run of an ended name replaces that entry, and a full table drops its oldest ended one.
func (g *guest) runProcess(name string, spec spawnSpec, restart restartPolicy) error {
	if g.stopping {
		return errors.New("the sandbox is stopping")
	}
	if g.oom {
		return errors.New("the sandbox hit its memory bound and runs nothing until a start")
	}
	old := g.named(name)
	if old != nil && !old.status.State.Ended() {
		return fmt.Errorf("%q: %w", name, errTaken)
	}
	if old == nil && len(g.procs) >= models.MaxProcesses && g.oldestEnded() == nil {
		return fmt.Errorf("the sandbox runs %d processes, the most it holds", models.MaxProcesses)
	}
	out, err := g.report.output(name)
	if err != nil {
		return err
	}
	spec.out = out

	// Stamp before the start so the fork and exec latency counts as run time, not lost from the healthy window.
	startedAt := time.Now()
	pid, err := g.start(spec, nil, false)
	if err != nil {
		g.report.keep(g.names())

		return fmt.Errorf("%q: %w", spec.argv[0], err)
	}
	if old == nil && len(g.procs) >= models.MaxProcesses {
		old = g.oldestEnded()
	}
	if old != nil {
		g.procs = slices.DeleteFunc(g.procs, func(p *proc) bool { return p == old })
	}
	p := &proc{name: name, spec: spec, restart: restart, pid: pid, runStartedAt: startedAt, gen: g.nextGen()}
	p.status = models.ProcessStatus{State: models.ProcessRunning, StartedAt: startedAt.UTC()}
	g.procs = append(g.procs, p)
	g.report.keep(g.names())
	g.changed(p)

	return nil
}

// exited routes one process's exit: a stop-process ends it killed, the policy may start it again, and a sandbox stop says nothing.
func (g *guest) exited(p *proc, pid int, exit models.ExitStatus) {
	defer p.release()
	p.stopTimer(g)
	if g.stopping {
		return
	}
	// Neither the next run nor an ended process keeps what this run left in its group, so a child that ignored the TERM ends here.
	killGroup(pid)
	p.status.Exit = &exit
	if p.stopping {
		p.stopping = false
		p.status.State = models.ProcessKilled
		g.changed(p)

		return
	}
	// A run that lasted the reset window starts the count over, so a rare crash never spends the retries.
	if time.Since(p.runStartedAt) >= p.restart.reset {
		p.count = 0
	}
	wait, state := p.restart.next(exit, p.count)
	p.status.State = state
	if state == models.ProcessRestarting {
		g.arm(p, wait, false)
	}
	g.changed(p)
}

// wake runs a timer that came due on the guest's goroutine; one a stop or a later start cancelled is stale and does nothing.
func (g *guest) wake(due timerDue) {
	p := g.named(due.name)
	if p == nil || p.gen != due.gen || g.stopping || g.oom {
		return
	}
	p.timer = nil
	if due.kill {
		if p.pid != 0 {
			killGroup(p.pid)
		}

		return
	}
	// A refused start again would give up for good, so one due under a freeze waits it out.
	if g.frozen.Load() != nil {
		g.arm(p, frozenRetry, false)

		return
	}
	p.runStartedAt = time.Now()
	// A start again is a start like the first, so a work directory the last run removed is made again, as docker makes it at every start.
	err := makeWorkDir(p.spec.dir)
	pid := 0
	if err == nil {
		pid, err = g.start(p.spec, nil, false)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "shard-init: start %q again: %v\n", p.name, err)
		p.status.State = models.ProcessGaveUp
		g.changed(p)

		return
	}
	p.pid, p.count = pid, p.count+1
	p.status.State, p.status.Restarts, p.status.StartedAt = models.ProcessRunning, p.status.Restarts+1, p.runStartedAt.UTC()
	g.changed(p)
}

// stopProcess cancels every start again of the named process and terms its group, which a kill follows once grace passes; the channel closes once it is reaped.
func (g *guest) stopProcess(name string, grace time.Duration) <-chan struct{} {
	reaped := make(chan struct{})
	p := g.named(name)
	if p == nil || p.status.State.Ended() {
		close(reaped)

		return reaped
	}
	p.stopTimer(g)
	// A process in its backoff wait ends there.
	if p.pid == 0 {
		p.status.State = models.ProcessKilled
		g.changed(p)
		close(reaped)

		return reaped
	}
	p.stopping = true
	p.reaped = append(p.reaped, reaped)
	sig := syscall.SIGTERM
	if grace <= 0 {
		sig = syscall.SIGKILL
	}
	// PID 1 must survive a failed signal, and the grace's kill still follows, so it is reported and never fatal (AGENTS.md).
	if err := signalGroup(p.pid, sig); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}
	if grace > 0 {
		g.arm(p, grace, true)
	}

	return reaped
}

// arm wakes the guest for p's current run once wait passes.
func (g *guest) arm(p *proc, wait time.Duration, kill bool) {
	due := timerDue{name: p.name, gen: p.gen, kill: kill}
	p.timer = time.AfterFunc(wait, func() { g.due <- due })
}

// stopTimer cancels p's timer, and a firing already on its way reads as stale.
func (p *proc) stopTimer(g *guest) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.gen = g.nextGen()
}

// release wakes every stop-process that waits for this exit.
func (p *proc) release() {
	for _, reaped := range p.reaped {
		close(reaped)
	}
	p.reaped = nil
}

func (g *guest) nextGen() uint64 {
	g.gens++

	return g.gens
}

// changed numbers p's new status and hands it to the reporter with every other process's last one.
func (g *guest) changed(p *proc) {
	g.seq++
	p.seq = g.seq
	// A sandbox outlives its processes, so a lost status is reported and never fatal (AGENTS.md).
	if err := g.report.changed(p.report(), g.table()); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)
	}
}

func (p *proc) report() models.ProcessReport {
	return models.ProcessReport{Name: p.name, ProcessStatus: p.status, Seq: p.seq}
}

// table is every process the guest holds, in the order they were run.
func (g *guest) table() []models.ProcessReport {
	table := make([]models.ProcessReport, 0, len(g.procs))
	for _, p := range g.procs {
		table = append(table, p.report())
	}

	return table
}

func (g *guest) names() []string {
	names := make([]string, 0, len(g.procs))
	for _, p := range g.procs {
		names = append(names, p.name)
	}

	return names
}

func (g *guest) named(name string) *proc {
	for _, p := range g.procs {
		if p.name == name {
			return p
		}
	}

	return nil
}

func (g *guest) byPID(pid int) *proc {
	for _, p := range g.procs {
		if p.pid == pid {
			return p
		}
	}

	return nil
}

func (g *guest) oldestEnded() *proc {
	for _, p := range g.procs {
		if p.status.State.Ended() {
			return p
		}
	}

	return nil
}

// running says a process still runs, which a stop waits for.
func (g *guest) running() bool {
	return slices.ContainsFunc(g.procs, func(p *proc) bool { return p.pid != 0 })
}

// answerOf is the guest's answer to request id: done, or a failure whose fields say a name taken or a command that could not start.
func answerOf(id int, err error) supervisor.Message {
	if err == nil {
		return supervisor.Message{Kind: supervisor.KindDone, ID: id}
	}
	reply := supervisor.Message{Kind: supervisor.KindFailure, ID: id, Error: err.Error(), Taken: errors.Is(err, errTaken)}
	_, refused := errors.AsType[unrunnable](err)
	_, badDir := errors.AsType[*workDirError](err)
	if refused || badDir {
		reply.Code = startFailureCode(err)
	}

	return reply
}
