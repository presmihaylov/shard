package supervisor_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// logsGuest serves the logs port of a fake guest: the output it holds of each name, and a hang-up for a name it holds none of.
type logsGuest struct {
	mu     sync.Mutex
	output map[string]string
	// opens counts the logs connections each name was opened by.
	opens map[string]int
}

func (g *logsGuest) hold(name, output string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.output[name] = output
}

func (g *logsGuest) opened(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.opens[name]
}

func (g *logsGuest) dial(_ context.Context, port uint32) (net.Conn, error) {
	if port != supervisor.LogsPort {
		return nil, fmt.Errorf("dialed port %d, want the logs port", port)
	}
	host, guest := net.Pipe()
	go g.serve(guest)

	return host, nil
}

func (g *logsGuest) serve(conn net.Conn) {
	defer conn.Close()
	var open supervisor.LogsOpen
	if err := supervisor.ReadHeader(conn, &open); err != nil {
		return
	}
	g.mu.Lock()
	g.opens[open.Name]++
	output, ok := g.output[open.Name]
	g.mu.Unlock()
	if !ok {
		return
	}
	if _, err := conn.Write(supervisor.LogsHeader(0, uint64(len(output)))); err != nil {
		return
	}
	var at uint64
	if err := binary.Read(conn, binary.BigEndian, &at); err != nil || at == supervisor.LogsStopped {
		return
	}
	if _, err := conn.Write([]byte(output[at:])); err != nil {
		return
	}
	// The guest holds the connection until the host lets go of it, as a live process's sink does.
	for {
		if err := binary.Read(conn, binary.BigEndian, &at); err != nil {
			return
		}
	}
}

// newHost answers the host and what it lost so far, read under the lock its Lost writes under.
func newHost(t *testing.T, dir string, dial supervisor.Dialer) (*supervisor.Host, func() []error) {
	t.Helper()
	var mu sync.Mutex
	var lost []error
	h := supervisor.NewHost(supervisor.HostConfig{
		Dir:   dir,
		Dial:  dial,
		Again: func(ctx context.Context) bool { return ctx.Err() == nil },
		Lost: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			lost = append(lost, err)
		},
		Warn: func(err error) { t.Log(err) },
		Pace: 10 * time.Millisecond,
	})
	t.Cleanup(h.Close)

	return h, func() []error {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(lost)
	}
}

// awaitLost waits for the host's first loss.
func awaitLost(t *testing.T, lost func() []error) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := lost(); len(got) > 0 {
			return got[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("the host lost nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func report(name string, seq uint64, state models.ProcessState) *models.ProcessReport {
	return &models.ProcessReport{Name: name, Seq: seq, ProcessStatus: models.ProcessStatus{State: state}}
}

func states(t *testing.T, dir string) string {
	t.Helper()
	table, err := supervisor.ReadProcesses(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range table {
		got = append(got, fmt.Sprintf("%s:%d:%s", p.Name, p.Seq, p.State))
	}

	return strings.Join(got, " ")
}

// awaitLog waits for a process's log to hold want.
func awaitLog(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := os.ReadFile(path)
		if err == nil && string(got) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %s = %q, %v; want %q", path, got, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Each name keeps its latest report by seq, an older one arriving late changes nothing, and the table reads in seq order.
func TestTheTableKeepsEachNamesLatestReport(t *testing.T) {
	dir := t.TempDir()
	h, _ := newHost(t, dir, (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	for _, r := range []*models.ProcessReport{
		report("web", 1, models.ProcessRunning),
		report("worker", 2, models.ProcessRunning),
		report("web", 4, models.ProcessExited),
		report("web", 3, models.ProcessRestarting),
	} {
		if err := h.Report(r); err != nil {
			t.Fatal(err)
		}
	}
	if got := states(t, dir); got != "worker:2:running web:4:exited" {
		t.Fatalf("table = %q", got)
	}
}

// A replay replaces the table: a name the guest let go of goes, and a later report the host already holds stays.
func TestAReplayReplacesTheTable(t *testing.T) {
	dir := t.TempDir()
	h, _ := newHost(t, dir, (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	for _, r := range []*models.ProcessReport{report("gone", 1, models.ProcessExited), report("web", 5, models.ProcessExited)} {
		if err := h.Report(r); err != nil {
			t.Fatal(err)
		}
	}
	state := supervisor.Message{
		Kind: supervisor.KindState, Version: supervisor.ProcessVersion, Logs: supervisor.LogsVersion,
		Processes: []models.ProcessReport{*report("web", 2, models.ProcessRunning), *report("cron", 3, models.ProcessRunning)},
	}
	if err := h.Replay(state); err != nil {
		t.Fatal(err)
	}
	if got := states(t, dir); got != "cron:3:running web:5:exited" {
		t.Fatalf("table = %q", got)
	}
	if err := h.Outdated("sb"); err != nil {
		t.Fatalf("Outdated after a replay of version %d = %v", supervisor.ProcessVersion, err)
	}
}

// A replay of what the host holds writes nothing, so a full disk fails no attach (SHARD-341).
func TestAReplayOfTheSameTableWritesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	h, _ := newHost(t, dir, (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	state := supervisor.Message{Kind: supervisor.KindState, Version: supervisor.ProcessVersion, Logs: supervisor.LogsVersion, Processes: []models.ProcessReport{*report("web", 1, models.ProcessRunning)}}
	if err := h.Replay(state); err != nil {
		t.Fatal(err)
	}
	h.Close()

	// A directory that refuses a new file fails the write the way a full disk does.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := h.Replay(state); err != nil {
		t.Fatalf("a replay of the same table = %v, want no write at all", err)
	}
	if err := h.Report(report("web", 2, models.ProcessExited)); err == nil {
		t.Fatal("a new report wrote into a directory that refuses a new file")
	}
}

// A guest from before named processes replays version zero, and every process verb on it is refused as too old.
func TestAGuestFromBeforeProcessesIsTooOld(t *testing.T) {
	h, _ := newHost(t, t.TempDir(), (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	if err := h.Replay(supervisor.Message{Kind: supervisor.KindState, Ready: true, Logs: 1}); err != nil {
		t.Fatal(err)
	}
	err := h.Outdated("sb")
	if _, ok := errors.AsType[*models.SupervisorTooOldError](err); !ok {
		t.Fatalf("Outdated = %v, want SupervisorTooOldError", err)
	}
}

// A name the guest could forge into a path, or a report with none, is refused before it touches the table.
func TestAReportOfABadNameIsRefused(t *testing.T) {
	dir := t.TempDir()
	h, _ := newHost(t, dir, (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	if err := h.Report(report("../vm", 1, models.ProcessRunning)); err == nil {
		t.Fatal("a report named ../vm was taken")
	}
	if err := h.Report(nil); err == nil {
		t.Fatal("a process event with no report was taken")
	}
	if err := h.Replay(supervisor.Message{Kind: supervisor.KindState, Processes: []models.ProcessReport{*report("A", 1, models.ProcessRunning)}}); err == nil {
		t.Fatal("a replay of a process named A was taken")
	}
	if got := states(t, dir); got != "" {
		t.Fatalf("table = %q, want none", got)
	}
}

// A table past the bound drops its oldest ended report, as the guest drops it from its own.
func TestTheTableDropsItsOldestEndedReportPastTheBound(t *testing.T) {
	dir := t.TempDir()
	h, _ := newHost(t, dir, (&logsGuest{output: map[string]string{}, opens: map[string]int{}}).dial)
	for i := range models.MaxProcesses {
		state := models.ProcessRunning
		if i == 3 || i == 5 {
			state = models.ProcessExited
		}
		if err := h.Report(report(fmt.Sprintf("p%d", i), uint64(i+1), state)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Report(report("late", 100, models.ProcessRunning)); err != nil {
		t.Fatal(err)
	}
	table, err := supervisor.ReadProcesses(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(table))
	for _, p := range table {
		names = append(names, p.Name)
	}
	if len(table) != models.MaxProcesses || slices.Contains(names, "p3") || !slices.Contains(names, "p5") || !slices.Contains(names, "late") {
		t.Fatalf("table = %v, want p3 dropped for late", names)
	}
}

// A report starts the follower of its name, whose output lands in its own log; one the guest holds nothing of is let go, and a later report follows it again.
func TestEachProcessLandsInItsOwnLog(t *testing.T) {
	dir := t.TempDir()
	guest := &logsGuest{output: map[string]string{"web": "web says hi\n", "worker": "worker says hi\n"}, opens: map[string]int{}}
	h, lost := newHost(t, dir, guest.dial)
	if err := h.Replay(supervisor.Message{Kind: supervisor.KindState, Version: supervisor.ProcessVersion, Logs: supervisor.LogsVersion}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*models.ProcessReport{report("web", 1, models.ProcessRunning), report("worker", 2, models.ProcessRunning), report("later", 3, models.ProcessRunning)} {
		if err := h.Report(r); err != nil {
			t.Fatal(err)
		}
	}
	awaitLog(t, supervisor.ProcessLogPath(dir, "web"), "web says hi\n")
	awaitLog(t, supervisor.ProcessLogPath(dir, "worker"), "worker says hi\n")

	// The guest held nothing of later, so its follower left after one dial and never dials again on its own.
	deadline := time.Now().Add(5 * time.Second)
	for guest.opened("later") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if opens := guest.opened("later"); opens != 1 {
		t.Fatalf("later was opened %d times, want once", opens)
	}
	guest.hold("later", "now it speaks\n")
	if err := h.Report(report("later", 4, models.ProcessRunning)); err != nil {
		t.Fatal(err)
	}
	awaitLog(t, supervisor.ProcessLogPath(dir, "later"), "now it speaks\n")
	if got := lost(); len(got) != 0 {
		t.Fatalf("lost = %v", got)
	}
}

// A guest whose logs protocol this host cannot read loses the sandbox, since a redial meets the same guest.
func TestAnUnknownLogsVersionLosesTheSandbox(t *testing.T) {
	guest := &logsGuest{output: map[string]string{"web": "hi\n"}, opens: map[string]int{}}
	h, lost := newHost(t, t.TempDir(), guest.dial)
	state := supervisor.Message{Kind: supervisor.KindState, Version: supervisor.ProcessVersion, Logs: supervisor.LogsVersion + 1, Processes: []models.ProcessReport{*report("web", 1, models.ProcessRunning)}}
	if err := h.Replay(state); err != nil {
		t.Fatal(err)
	}
	if err := awaitLost(t, lost); !errors.Is(err, supervisor.ErrLogsVersion) {
		t.Fatalf("lost %v, want ErrLogsVersion", err)
	}
}

// A log whose cursor cannot be written loses the sandbox, and its follower dials no more, since a redial meets the same file.
func TestALogThatRefusesItsCursorLosesTheSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, supervisor.ProcessLogs, "web.cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	guest := &logsGuest{output: map[string]string{"web": "hi\n"}, opens: map[string]int{}}
	h, lost := newHost(t, dir, guest.dial)
	if err := h.Replay(supervisor.Message{Kind: supervisor.KindState, Version: supervisor.ProcessVersion, Logs: supervisor.LogsVersion}); err != nil {
		t.Fatal(err)
	}
	if err := h.Report(report("web", 1, models.ProcessRunning)); err != nil {
		t.Fatal(err)
	}
	if err := awaitLost(t, lost); !strings.Contains(err.Error(), "the log of process web stopped") {
		t.Fatalf("lost %v, want the log of web stopped", err)
	}
	time.Sleep(100 * time.Millisecond)
	if opens := guest.opened("web"); opens != 1 {
		t.Fatalf("web was opened %d times, want once", opens)
	}
}

// A fresh boot forgets the table and every cursor, and keeps the logs, which outlive the run.
func TestForgetBootKeepsTheLogsAndDropsTheCursors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, supervisor.ProcessLogs), 0o700); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, supervisor.ProcessLogs)
	for _, name := range []string{"web.log", "web.cursor", "worker.cursor"} {
		if err := os.WriteFile(filepath.Join(logs, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, supervisor.ProcessTable), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.ForgetBoot(dir); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(logs)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != "web.log" {
		t.Fatalf("left %v, want web.log alone", left)
	}
	if _, err := os.Stat(filepath.Join(dir, supervisor.ProcessTable)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the table is still there: %v", err)
	}
	if err := supervisor.ForgetBoot(t.TempDir()); err != nil {
		t.Fatalf("ForgetBoot of a directory that holds none = %v", err)
	}
}

// Each process log a daemon before the bound left past it is cut to its last bytes.
func TestBoundProcessLogsBoundsEachLog(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, supervisor.ProcessLogs)
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supervisor.ProcessLogPath(dir, "web"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supervisor.ProcessLogPath(dir, "worker"), []byte("0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.BoundProcessLogs(dir, 4); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"web": 0, "worker": 4} {
		info, err := os.Stat(supervisor.ProcessLogPath(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != want {
			t.Errorf("%s is %d bytes, want %d", name, info.Size(), want)
		}
	}
}

// A process takes the sandbox's env under its own, its workdir and its user, unless it names its own; a named user is resolved in the guest.
func TestRunOfPutsAProcessWhereAnExecRuns(t *testing.T) {
	base := supervisor.Base{Env: []string{"PATH=/bin", "A=1"}, WorkDir: "/app", User: "1000:1000", Groups: []uint32{27}}
	run := base.RunOf(models.ProcessSpec{Name: "web", Argv: []string{"serve"}, Env: []string{"A=2"}, Restart: models.RestartSpec{Policy: models.RestartOnFailure, Retries: 3, Backoff: 2}})
	if run.Name != "web" || !slices.Equal(run.Env, []string{"PATH=/bin", "A=2"}) || run.WorkDir != "/app" || run.User != "1000:1000" || run.Lookup || !slices.Equal(run.Groups, []uint32{27}) {
		t.Fatalf("run = %+v", run)
	}
	if run.Restart != models.RestartOnFailure || run.Retries != 3 || run.Backoff != 2*time.Second {
		t.Fatalf("restart = %s %d %s", run.Restart, run.Retries, run.Backoff)
	}
	named := supervisor.Base{}.RunOf(models.ProcessSpec{Name: "web", Argv: []string{"serve"}, User: "app", WorkDir: "/srv"})
	if named.User != "app" || !named.Lookup || named.Groups != nil || named.WorkDir != "/srv" || named.Backoff != 0 {
		t.Fatalf("run = %+v", named)
	}
	if root := (supervisor.Base{}).RunOf(models.ProcessSpec{Name: "web", Argv: []string{"serve"}}); root.WorkDir != "/" {
		t.Fatalf("workdir = %q, want /", root.WorkDir)
	}
}
