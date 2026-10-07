package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// recordReporter keeps every status the guest reports, so a test reads each decision in order; the output lands in a log per name, as in a container.
type recordReporter struct {
	files *fileReporter
	// deaf is a reporter with no host attached, which the OOM report must say rather than drop.
	deaf bool

	mu      sync.Mutex
	reports []models.ProcessReport
	table   []models.ProcessReport
	ooms    int
	// read is, per name, how far an await already went, so the next one waits for a later status.
	read map[string]int
}

func newRecordReporter(t *testing.T) *recordReporter {
	t.Helper()

	r := &recordReporter{files: &fileReporter{logs: t.TempDir(), outputs: map[string]*os.File{}}, read: map[string]int{}}
	// The guest has ended by now, so nothing else holds the logs.
	t.Cleanup(func() { r.files.keep(nil) })

	return r
}

func (r *recordReporter) changed(p models.ProcessReport, table []models.ProcessReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, p)
	r.table = table

	return nil
}

func (r *recordReporter) oomKilled() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ooms++
	if r.deaf {
		return errNoHost
	}

	return nil
}

func (r *recordReporter) output(name string) (*os.File, error) { return r.files.output(name) }

func (r *recordReporter) keep(names []string) { r.files.keep(names) }

// await waits for the next status of name in state, past every status an earlier await of that name read.
func (r *recordReporter) await(t *testing.T, name string, state models.ProcessState) models.ProcessReport {
	t.Helper()

	return r.awaitWithin(t, 15*time.Second, name, state)
}

func (r *recordReporter) awaitWithin(t *testing.T, timeout time.Duration, name string, state models.ProcessState) models.ProcessReport {
	t.Helper()

	var found models.ProcessReport
	waitFor(t, timeout, fmt.Sprintf("%q to be %s", name, state), func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := r.read[name]; i < len(r.reports); i++ {
			if r.reports[i].Name == name && r.reports[i].State == state {
				found, r.read[name] = r.reports[i], i+1

				return true
			}
		}

		return false
	})

	return found
}

// of is every status of name reported so far.
func (r *recordReporter) of(name string) []models.ProcessReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.DeleteFunc(slices.Clone(r.reports), func(p models.ProcessReport) bool { return p.Name != name })
}

func (r *recordReporter) lastTable() []models.ProcessReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.table)
}

func (r *recordReporter) log(t *testing.T, name string) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(r.files.logs, supervisor.ProcessLogName(name)))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the log of %q: %v", name, err)
	}

	return string(blob)
}

// testGuest is a guest supervising in this process; it reaps every child of the test binary, so no two run at once and none outlives its test.
type testGuest struct {
	*guest
	done  chan error
	ended bool
}

func startGuest(t *testing.T) (*testGuest, *recordReporter) {
	t.Helper()

	report := newRecordReporter(t)

	return startGuestOver(t, report), report
}

func startGuestOver(t *testing.T, report reporter) *testGuest {
	t.Helper()

	g := &testGuest{guest: newGuest(report), done: make(chan error, 1)}
	go func() { g.done <- g.supervise() }()
	t.Cleanup(func() {
		defer signal.Stop(g.stopSignals)
		defer signal.Stop(g.childDeaths)
		if g.ended {
			return
		}
		g.stopSignals <- syscall.SIGTERM
		if ended, err := g.awaitEnd(5 * time.Second); ended {
			if err != nil {
				t.Errorf("supervise: %v", err)
			}

			return
		}
		// A process that ignores the TERM holds the stop, as it holds a host's until the grace.
		g.stopSignals <- syscall.SIGKILL
		if ended, err := g.awaitEnd(5 * time.Second); !ended || err != nil {
			t.Errorf("the guest did not end on a stop: ended %t, %v", ended, err)
		}
	})

	return g
}

func (g *testGuest) awaitEnd(within time.Duration) (bool, error) {
	select {
	case err := <-g.done:
		g.ended = true

		return true, err
	case <-time.After(within):
		return false, nil
	}
}

func named(name, role string) supervisor.RunSpec {
	return supervisor.RunSpec{Name: name, Argv: childArgv(role), Env: os.Environ()}
}

func mustRun(t *testing.T, g *testGuest, spec supervisor.RunSpec) {
	t.Helper()

	if err := g.runSpec(spec); err != nil {
		t.Fatalf("run %q: %v", spec.Name, err)
	}
}

// stopNamed is the stop-process request as either transport hands it over: it answers once the process is reaped.
func stopNamed(t *testing.T, g *testGuest, name string, grace time.Duration) {
	t.Helper()

	answered := make(chan supervisor.Message, 1)
	go func() {
		answered <- g.request(supervisor.Message{Kind: supervisor.KindStopProcess, ID: 9, Name: name, Grace: grace})
	}()
	select {
	case reply := <-answered:
		if reply.Kind != supervisor.KindDone || reply.ID != 9 {
			t.Fatalf("stop %q answered %+v, want done", name, reply)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("stop %q never answered", name)
	}
}

func (g *testGuest) procsNamed() []string {
	var names []string
	g.run(func() { names = g.names() })

	return names
}

func TestTwoNamedProcessesRunSideBySide(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("web", "term:0"))
	mustRun(t, g, named("worker", "say:hello"))
	report.await(t, "worker", models.ProcessExited)

	table := report.lastTable()
	if len(table) != 2 || table[0].Name != "web" || table[1].Name != "worker" {
		t.Fatalf("table = %+v, want web then worker", table)
	}
	if table[0].State != models.ProcessRunning || table[1].Exit == nil || table[1].Exit.Code != 0 {
		t.Fatalf("table = %+v, want web running and worker exited 0", table)
	}
	if table[0].Seq >= table[1].Seq {
		t.Errorf("web's status is numbered %d, after worker's %d", table[0].Seq, table[1].Seq)
	}
	waitFor(t, 5*time.Second, "each process's output in its own log", func() bool {
		return report.log(t, "worker") == "hello\n" && report.log(t, "web") == "ready\n"
	})
}

func TestARunOfALiveNameIsTaken(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("web", "sleep:60000"))
	err := g.runSpec(named("web", "say:twice"))
	if !errors.Is(err, errTaken) {
		t.Fatalf("a second web gave %v, want it taken", err)
	}
	if reply := answerOf(4, err); !reply.Taken || reply.Kind != supervisor.KindFailure || reply.Code != 0 {
		t.Errorf("the answer is %+v, want a failure that says taken", reply)
	}

	// A process waiting out its backoff still holds its name.
	flaky := named("flaky", "exit:1")
	flaky.Restart, flaky.Backoff = models.RestartOnFailure, time.Minute
	mustRun(t, g, flaky)
	report.await(t, "flaky", models.ProcessRestarting)
	if err := g.runSpec(named("flaky", "say:twice")); !errors.Is(err, errTaken) {
		t.Fatalf("a run of a restarting name gave %v, want it taken", err)
	}
	if names := g.procsNamed(); !slices.Equal(names, []string{"web", "flaky"}) {
		t.Errorf("the guest holds %q, want web and flaky once each", names)
	}
}

func TestARunOfAnEndedNameReplacesIt(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("job", "say:first"))
	first := report.await(t, "job", models.ProcessExited)
	mustRun(t, g, named("other", "sleep:60000"))
	mustRun(t, g, named("job", "say:second"))
	second := report.await(t, "job", models.ProcessExited)

	if second.Seq <= first.Seq || second.Restarts != 0 {
		t.Errorf("the second run reads %+v after %+v, want a later status with no restarts", second, first)
	}
	// The new run goes to the end of the table, as a run of a new name would.
	if names := g.procsNamed(); !slices.Equal(names, []string{"other", "job"}) {
		t.Errorf("the guest holds %q, want other then job", names)
	}
	waitFor(t, 5*time.Second, "both runs in one log", func() bool { return report.log(t, "job") == "first\nsecond\n" })
}

func TestAFullTableRefusesUnlessAnEntryEnded(t *testing.T) {
	g, report := startGuest(t)

	for i := range models.MaxProcesses {
		mustRun(t, g, named(fmt.Sprintf("p%02d", i), "sleep:60000"))
	}
	err := g.runSpec(named("extra", "say:hi"))
	if err == nil || errors.Is(err, errTaken) {
		t.Fatalf("a run past %d live processes gave %v, want a refusal", models.MaxProcesses, err)
	}
	if !strings.Contains(err.Error(), "the most it holds") {
		t.Errorf("the refusal reads %q, want it to say the table is full", err)
	}

	stopNamed(t, g, "p07", time.Second)
	stopNamed(t, g, "p03", time.Second)
	report.await(t, "p03", models.ProcessKilled)
	mustRun(t, g, named("extra", "say:hi"))

	names := g.procsNamed()
	if len(names) != models.MaxProcesses || slices.Contains(names, "p03") || !slices.Contains(names, "p07") || names[len(names)-1] != "extra" {
		t.Fatalf("the guest holds %q, want the oldest ended, p03, evicted for extra", names)
	}
}

func TestAProcessWithNoPolicyEndsAtItsExit(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("once", "exit:1"))
	exit := report.await(t, "once", models.ProcessExited)
	if exit.Exit == nil || exit.Exit.Code != 1 || exit.Restarts != 0 {
		t.Fatalf("once = %+v, want exited 1 with no restarts", exit)
	}
}

func TestOnFailureGivesUpOnceTheRetriesAreSpent(t *testing.T) {
	g, report := startGuest(t)

	spec := named("flaky", "exit:3")
	spec.Restart, spec.Retries, spec.Backoff = models.RestartOnFailure, 2, 10*time.Millisecond
	mustRun(t, g, spec)

	gaveUp := report.await(t, "flaky", models.ProcessGaveUp)
	if gaveUp.Restarts != 2 || gaveUp.Exit == nil || gaveUp.Exit.Code != 3 {
		t.Fatalf("flaky = %+v, want it given up after 2 restarts on code 3", gaveUp)
	}
	var restarts []int
	for _, p := range report.of("flaky") {
		if p.State == models.ProcessRunning {
			restarts = append(restarts, p.Restarts)
		}
	}
	if !slices.Equal(restarts, []int{0, 1, 2}) {
		t.Errorf("flaky ran with the restart counts %v, want 0, 1, 2", restarts)
	}
}

func TestOnFailureLeavesACleanExitAlone(t *testing.T) {
	g, report := startGuest(t)

	spec := named("clean", "exit:0")
	spec.Restart, spec.Backoff = models.RestartOnFailure, 10*time.Millisecond
	mustRun(t, g, spec)
	if exit := report.await(t, "clean", models.ProcessExited); exit.Restarts != 0 {
		t.Fatalf("clean = %+v, want it exited with no restart", exit)
	}
}

// Inside the guest unless-stopped is always; only the daemon's start tells them apart.
func TestAlwaysStartsACleanExitAgainAfterTheBackoff(t *testing.T) {
	for _, policy := range []models.RestartPolicy{models.RestartAlways, models.RestartUnlessStopped} {
		t.Run(string(policy), func(t *testing.T) {
			g, report := startGuest(t)

			const backoff = 200 * time.Millisecond
			spec := named("loop", "exit:0")
			spec.Restart, spec.Backoff = policy, backoff
			mustRun(t, g, spec)

			first := report.await(t, "loop", models.ProcessRunning)
			report.await(t, "loop", models.ProcessRestarting)
			second := report.await(t, "loop", models.ProcessRunning)
			if second.Restarts != 1 {
				t.Errorf("the second run counts %d restarts, want 1", second.Restarts)
			}
			if gap := second.StartedAt.Sub(first.StartedAt); gap < backoff {
				t.Errorf("the second run started %s after the first, inside the %s backoff", gap, backoff)
			}
			// The backoff doubles, so the third start waits twice as long.
			report.await(t, "loop", models.ProcessRestarting)
			if third := report.await(t, "loop", models.ProcessRunning); third.Restarts != 2 {
				t.Errorf("the third run counts %d restarts, want 2", third.Restarts)
			}
		})
	}
}

func TestAStartAgainMakesTheWorkDirectoryAgain(t *testing.T) {
	g, report := startGuest(t)

	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := named("loop", "exit:0")
	spec.Restart, spec.Backoff, spec.WorkDir = models.RestartAlways, 500*time.Millisecond, dir
	mustRun(t, g, spec)

	report.await(t, "loop", models.ProcessRestarting)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if again := report.await(t, "loop", models.ProcessRunning); again.Restarts != 1 {
		t.Fatalf("the start again counts %d restarts, want 1", again.Restarts)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the work directory was not made again: %v", err)
	}
}

// A run that lasts the reset window starts the count over, so a rare crash never spends the retries.
func TestAHealthyRunClearsTheRestartCount(t *testing.T) {
	g, report := startGuest(t)

	spec := named("rare", "run:400:1")
	spec.Restart, spec.Retries, spec.Backoff, spec.Reset = models.RestartOnFailure, 1, 10*time.Millisecond, 200*time.Millisecond
	mustRun(t, g, spec)

	for want := 1; want <= 3; want++ {
		if again := report.await(t, "rare", models.ProcessRunning); again.Restarts != want-1 {
			t.Fatalf("run %d counts %d restarts, want %d", want, again.Restarts, want-1)
		}
	}
	for _, p := range report.of("rare") {
		if p.State == models.ProcessGaveUp {
			t.Fatalf("rare gave up although every run outlasted the reset window: %+v", p)
		}
	}
}

func TestAShortRunSpendsTheRetries(t *testing.T) {
	g, report := startGuest(t)

	spec := named("crash", "exit:1")
	spec.Restart, spec.Retries, spec.Backoff, spec.Reset = models.RestartOnFailure, 1, 10*time.Millisecond, time.Minute
	mustRun(t, g, spec)
	if gaveUp := report.await(t, "crash", models.ProcessGaveUp); gaveUp.Restarts != 1 {
		t.Fatalf("crash = %+v, want it given up after one restart", gaveUp)
	}
}

func TestStopProcessTermsItAndKeepsTheGuest(t *testing.T) {
	g, report := startGuest(t)

	spec := named("polite", "term:5")
	spec.Restart = models.RestartAlways
	mustRun(t, g, spec)
	mustRun(t, g, named("other", "sleep:60000"))
	waitFor(t, 5*time.Second, "polite to take TERM", func() bool { return report.log(t, "polite") == "ready\n" })

	stopNamed(t, g, "polite", 10*time.Second)
	killed := report.await(t, "polite", models.ProcessKilled)
	if killed.Exit == nil || killed.Exit.Code != 5 || killed.Exit.Signal != 0 {
		t.Fatalf("polite = %+v, want it killed on its own exit 5", killed)
	}
	// A stopped process is never started again, whatever its policy.
	time.Sleep(1200 * time.Millisecond)
	if last := report.of("polite"); last[len(last)-1].State != models.ProcessKilled {
		t.Fatalf("polite went on to %+v after its stop", last[len(last)-1])
	}
	if table := report.lastTable(); len(table) != 2 || table[1].State != models.ProcessRunning {
		t.Errorf("table = %+v, want other still running", table)
	}
}

func TestStopProcessKillsOnceTheGraceRunsOut(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("stubborn", "ignoreterm"))
	waitFor(t, 5*time.Second, "stubborn to ignore TERM", func() bool { return report.log(t, "stubborn") == "ready\n" })

	const grace = 300 * time.Millisecond
	began := time.Now()
	stopNamed(t, g, "stubborn", grace)
	if took := time.Since(began); took < grace {
		t.Errorf("the stop answered after %s, inside the %s grace", took, grace)
	}
	if killed := report.await(t, "stubborn", models.ProcessKilled); killed.Exit == nil || killed.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("stubborn = %+v, want it killed by SIGKILL", killed)
	}
}

func TestStopProcessWithNoGraceKills(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("polite", "term:5"))
	waitFor(t, 5*time.Second, "polite to take TERM", func() bool { return report.log(t, "polite") == "ready\n" })

	stopNamed(t, g, "polite", 0)
	if killed := report.await(t, "polite", models.ProcessKilled); killed.Exit == nil || killed.Exit.Signal != int(syscall.SIGKILL) {
		t.Fatalf("polite = %+v, want SIGKILL and not its TERM exit", killed)
	}
}

func TestStopProcessInTheBackoffEndsIt(t *testing.T) {
	g, report := startGuest(t)

	spec := named("flaky", "exit:1")
	spec.Restart, spec.Backoff = models.RestartOnFailure, 500*time.Millisecond
	mustRun(t, g, spec)
	report.await(t, "flaky", models.ProcessRestarting)

	stopNamed(t, g, "flaky", 10*time.Second)
	killed := report.await(t, "flaky", models.ProcessKilled)
	if killed.Exit == nil || killed.Exit.Code != 1 {
		t.Fatalf("flaky = %+v, want killed with the last run's exit", killed)
	}
	time.Sleep(time.Second)
	if last := report.of("flaky"); last[len(last)-1].State != models.ProcessKilled {
		t.Fatalf("flaky started again after its stop: %+v", last[len(last)-1])
	}
}

func TestStopProcessOfNoLiveProcessAnswersAtOnce(t *testing.T) {
	g, report := startGuest(t)

	stopNamed(t, g, "ghost", time.Minute)
	mustRun(t, g, named("done", "exit:0"))
	report.await(t, "done", models.ProcessExited)
	stopNamed(t, g, "done", time.Minute)
	if last := report.of("done"); last[len(last)-1].State != models.ProcessExited {
		t.Fatalf("a stop of an ended process changed it to %+v", last[len(last)-1])
	}
}

// A sandbox stop TERMs every process's group and waits for every reap; it reports nothing, since the host records the stop.
func TestASandboxStopWaitsForEveryProcess(t *testing.T) {
	g, report := startGuest(t)

	pidFile := filepath.Join(t.TempDir(), "forked")
	mustRun(t, g, supervisor.RunSpec{Name: "tree", Argv: treeArgv(pidFile, "wait"), Env: os.Environ()})
	mustRun(t, g, named("stubborn", "ignoreterm"))
	waitFor(t, 5*time.Second, "stubborn to ignore TERM", func() bool { return report.log(t, "stubborn") == "ready\n" })
	forked := forkedPIDs(t, pidFile, 1)[0]
	before := len(report.of("tree")) + len(report.of("stubborn"))

	g.stopSignals <- syscall.SIGTERM
	waitFor(t, 10*time.Second, fmt.Sprintf("the sleep tree forked, pid %d, to end", forked), func() bool { return gone(forked) })
	if ended, err := g.awaitEnd(time.Second); ended {
		t.Fatalf("the guest ended with stubborn still running: %v", err)
	}
	g.stopSignals <- syscall.SIGKILL
	if ended, err := g.awaitEnd(10 * time.Second); !ended || err != nil {
		t.Fatalf("the guest did not end once every process was reaped: ended %t, %v", ended, err)
	}
	if after := len(report.of("tree")) + len(report.of("stubborn")); after != before {
		t.Errorf("the stop reported %d statuses, want none", after-before)
	}
}

func TestARunWhileStoppingIsRefused(t *testing.T) {
	g, report := startGuest(t)

	mustRun(t, g, named("stubborn", "ignoreterm"))
	waitFor(t, 5*time.Second, "stubborn to ignore TERM", func() bool { return report.log(t, "stubborn") == "ready\n" })
	g.stopSignals <- syscall.SIGTERM
	if err := g.runSpec(named("late", "say:hi")); err == nil {
		t.Fatal("a run during the stop started")
	}
}

func TestRunSpecRefusesWhatItCannotRun(t *testing.T) {
	g, _ := startGuest(t)

	cases := map[string]supervisor.RunSpec{
		"an upper case name": named("Web", "say:hi"),
		"a long name":        named(strings.Repeat("a", models.MaxProcessName+1), "say:hi"),
		"no command":         {Name: "empty"},
		"a bad user":         {Name: "user", Argv: childArgv("say:hi"), User: "nobody"},
		"a bad policy":       {Name: "policy", Argv: childArgv("say:hi"), Restart: "sometimes"},
		"no such binary":     {Name: "missing", Argv: []string{"/nonexistent/binary"}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if err := g.runSpec(spec); err == nil {
				t.Errorf("runSpec(%+v) started", spec)
			}
		})
	}
	if names := g.procsNamed(); len(names) != 0 {
		t.Errorf("the guest holds %q after only refusals", names)
	}
}

func TestAnswerOfNamesWhyARunFailed(t *testing.T) {
	cases := map[string]struct {
		err   error
		kind  string
		code  int
		taken bool
	}{
		"done":             {kind: supervisor.KindDone},
		"a missing binary": {err: fmt.Errorf("%q: %w", "x", unrunnable{fs.ErrNotExist}), kind: supervisor.KindFailure, code: models.CommandNotFoundExitCode},
		"not executable":   {err: fmt.Errorf("%q: %w", "x", unrunnable{syscall.EACCES}), kind: supervisor.KindFailure, code: models.CommandNotExecutableExitCode},
		"a bad workdir":    {err: &workDirError{dir: "/app", errno: syscall.ENOTDIR}, kind: supervisor.KindFailure, code: models.CommandNotExecutableExitCode},
		"taken":            {err: fmt.Errorf("%q: %w", "web", errTaken), kind: supervisor.KindFailure, taken: true},
		"anything else":    {err: errors.New("the sandbox is stopping"), kind: supervisor.KindFailure},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := answerOf(7, c.err)
			if got.ID != 7 || got.Kind != c.kind || got.Code != c.code || got.Taken != c.taken {
				t.Errorf("answerOf(%v) = %+v, want kind %s, code %d, taken %t", c.err, got, c.kind, c.code, c.taken)
			}
			if c.err != nil && got.Error != c.err.Error() {
				t.Errorf("the answer reads %q, want %q", got.Error, c.err)
			}
		})
	}
}

// A real start failure lands in the answer as the shell's code, so the host maps it without parsing the text.
func TestARunThatCannotStartAnswersTheShellCode(t *testing.T) {
	g, _ := startGuest(t)

	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "data")
	if err := os.WriteFile(notExecutable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		argv []string
		code int
	}{
		"missing":        {argv: []string{filepath.Join(dir, "missing")}, code: models.CommandNotFoundExitCode},
		"not executable": {argv: []string{notExecutable}, code: models.CommandNotExecutableExitCode},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			reply := g.request(supervisor.Message{Kind: supervisor.KindRun, ID: 2, Run: &supervisor.RunSpec{Name: "x", Argv: c.argv}})
			if reply.Kind != supervisor.KindFailure || reply.Code != c.code || !strings.Contains(reply.Error, c.argv[0]) {
				t.Errorf("the answer is %+v, want code %d naming %s", reply, c.code, c.argv[0])
			}
		})
	}
}
