package supervisor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/runspec"
)

// ProcessTable is the file in a VM's state directory that holds the guest's last report of each process, so a stopped sandbox still answers for them.
const ProcessTable = "processes.json"

// ProcessLogPath is the log a VM host lands one process's output in, under the sandbox's state directory dir.
func ProcessLogPath(dir, name string) string {
	return filepath.Join(dir, ProcessLogs, ProcessLogName(name))
}

// processCursor places the guest's output of one process in its log.
func processCursor(dir, name string) string {
	return filepath.Join(dir, ProcessLogs, name+".cursor")
}

// ReadProcesses is the table a VM host keeps in dir, in the order the guest numbered the reports; none before the first.
func ReadProcesses(dir string) ([]models.ProcessReport, error) {
	path := filepath.Join(dir, ProcessTable)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the process table: %w", err)
	}
	var table []models.ProcessReport
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, fmt.Errorf("parse the process table %s: %w", path, err)
	}

	return table, nil
}

// ForgetBoot drops the table and every log cursor in dir: a fresh guest numbers its reports and its output from zero, so an old cursor would skip new output.
func ForgetBoot(dir string) error {
	cursors, err := filepath.Glob(filepath.Join(dir, ProcessLogs, "*.cursor"))
	if err != nil {
		return fmt.Errorf("list the log cursors: %w", err)
	}
	for _, stale := range append(cursors, filepath.Join(dir, ProcessTable)) {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("forget the last boot: %w", err)
		}
	}

	return nil
}

// BoundProcessLogs bounds every process log in dir that a daemon before the bound left past max.
func BoundProcessLogs(dir string, max int64) error {
	logs, err := filepath.Glob(filepath.Join(dir, ProcessLogs, "*.log"))
	if err != nil {
		return fmt.Errorf("list the process logs: %w", err)
	}
	var errs []error
	for _, path := range logs {
		name := strings.TrimSuffix(filepath.Base(path), ".log")
		if err := BoundLog(path, processCursor(dir, name), max); err != nil {
			errs = append(errs, fmt.Errorf("bound the log of process %s: %w", name, err))
		}
	}

	return errors.Join(errs...)
}

// Base is what every process and exec of a VM sandbox starts from, and the trust each boot's setup writes.
type Base struct {
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	// User is uid:gid with Groups the supplementary set, or a bare name a seed's guest has not resolved yet.
	User   string   `json:"user,omitempty"`
	Groups []uint32 `json:"groups,omitempty"`
	Trust  *Trust   `json:"trust,omitempty"`
}

// Setup is what one boot of the guest takes before any process.
func (b Base) Setup() Setup {
	return Setup{WorkDir: b.WorkDir, Trust: b.Trust}
}

// RunOf puts a process where an exec runs: the sandbox's env under its own, its workdir and its user unless it names one.
func (b Base) RunOf(spec models.ProcessSpec) RunSpec {
	run := RunSpec{
		Name:    spec.Name,
		Argv:    spec.Argv,
		Env:     runspec.MergeEnv(b.Env, spec.Env),
		WorkDir: cmp.Or(spec.WorkDir, b.WorkDir, "/"),
		User:    b.User,
		Groups:  b.Groups,
		Restart: spec.Restart.Policy,
		Retries: spec.Restart.Retries,
		Backoff: time.Duration(spec.Restart.Backoff) * time.Second,
	}
	// The guest resolves a named user, because the image on the host misses a user the sandbox added (SHARD-356).
	if spec.User != "" {
		run.User, run.Groups, run.Lookup = spec.User, nil, true
	}

	return run
}

// ProcessError is the error the service reads for the guest's refusal of a run or a stop-process; anything else is wrapped as it is.
func ProcessError(id string, err error) error {
	refusal, ok := errors.AsType[*Refusal](err)
	if !ok {
		return fmt.Errorf("sandbox %s: %w", id, err)
	}
	answer := refusal.Answer
	if answer.Outdated {
		return &models.SupervisorTooOldError{Sandbox: id}
	}
	if answer.Taken {
		return fmt.Errorf("sandbox %s: %s: %w", id, OneLine(answer.Error), models.ErrProcessRunning)
	}
	if answer.Code == 126 || answer.Code == 127 {
		return &models.CommandNotStartedError{Sandbox: id, Reason: OneLine(answer.Error), Code: answer.Code}
	}

	return fmt.Errorf("sandbox %s: %w", id, err)
}

// HostConfig is what the host side of one boot needs from its provider.
type HostConfig struct {
	// Dir is the sandbox's state directory, which holds the table and the logs.
	Dir  string
	Dial Dialer
	// Again says whether a dropped logs connection is dialed again: the VM still runs, once a verb that holds it lets go.
	Again func(context.Context) bool
	// Lost takes a failure no redial mends, which every later verb on the sandbox then reports.
	Lost func(error)
	// Warn takes the error a dropped logs connection ended with, before the redial.
	Warn func(error)
	// Pace is the wait before each redial.
	Pace time.Duration
}

// Host is a VM host's side of the guest's processes for one boot: the table it keeps on disk, and one log follower per name.
type Host struct {
	cfg    HostConfig
	ctx    context.Context
	cancel context.CancelFunc
	// followers is every follower still running, which Close waits out so no write lands after it.
	followers sync.WaitGroup

	// tableMu orders the table's writes, which the event loop and a save's redial both make.
	tableMu sync.Mutex

	mu sync.Mutex
	// process and logs are the protocols the guest's last state named.
	process int
	logs    int
	follows map[string]*follower
}

// follower is the one goroutine that lands a process's output.
type follower struct {
	// round ends the connection in use, so a stream a reset killed is dialed again.
	round context.CancelFunc
	// again is a report of the process while its follower ended a round, so the follower dials once more instead of leaving.
	again bool
}

func NewHost(cfg HostConfig) *Host {
	ctx, cancel := context.WithCancel(context.Background())

	return &Host{cfg: cfg, ctx: ctx, cancel: cancel, follows: map[string]*follower{}}
}

// Outdated refuses a process verb on a guest whose shard-init predates named processes, which would take a run for an entrypoint.
func (h *Host) Outdated(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.process < ProcessVersion {
		return &models.SupervisorTooOldError{Sandbox: id}
	}

	return nil
}

// Replay lands a state the guest replayed: its table replaces the host's, each name keeping the later report, and every name it holds is followed.
func (h *Host) Replay(state Message) error {
	h.mu.Lock()
	h.process, h.logs = state.Version, state.Logs
	h.mu.Unlock()
	for _, p := range state.Processes {
		if !models.ValidProcessName(p.Name) {
			return fmt.Errorf("the guest replayed a process named %q", p.Name)
		}
	}

	err := h.update(func(table []models.ProcessReport) []models.ProcessReport {
		replayed := make([]models.ProcessReport, 0, len(state.Processes))
		for _, p := range state.Processes {
			if i := slices.IndexFunc(table, named(p.Name)); i >= 0 && table[i].Seq > p.Seq {
				p = table[i]
			}
			replayed = latest(replayed, p)
		}

		return replayed
	})
	for _, p := range state.Processes {
		h.Follow(p.Name)
	}

	return err
}

// Report lands one process's new status, unless the table holds a later one, and follows its output.
func (h *Host) Report(p *models.ProcessReport) error {
	if p == nil {
		return errors.New("a process event carries no report")
	}
	if !models.ValidProcessName(p.Name) {
		return fmt.Errorf("the guest reported a process named %q", p.Name)
	}
	report := *p
	err := h.update(func(table []models.ProcessReport) []models.ProcessReport {
		return bounded(latest(table, report))
	})
	h.Follow(report.Name)

	return err
}

// update writes the table f makes of the one on disk, and nothing when it is the same, so a replay of what the host holds fails no attach on a full disk (SHARD-341).
func (h *Host) update(f func([]models.ProcessReport) []models.ProcessReport) error {
	h.tableMu.Lock()
	defer h.tableMu.Unlock()
	table, err := ReadProcesses(h.cfg.Dir)
	if err != nil {
		return err
	}
	next := f(slices.Clone(table))
	slices.SortFunc(next, func(a, b models.ProcessReport) int { return cmp.Compare(a.Seq, b.Seq) })
	// One boot numbers each report once, so a name and its seq say the whole report.
	if slices.EqualFunc(table, next, func(a, b models.ProcessReport) bool { return a.Name == b.Name && a.Seq == b.Seq }) {
		return nil
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("marshal the process table: %w", err)
	}
	if err := store.WriteFile(filepath.Join(h.cfg.Dir, ProcessTable), encoded, 0o600); err != nil {
		return fmt.Errorf("write the process table: %w", err)
	}

	return nil
}

// latest puts p in table in place of an earlier report of its name, and keeps a later one.
func latest(table []models.ProcessReport, p models.ProcessReport) []models.ProcessReport {
	i := slices.IndexFunc(table, named(p.Name))
	if i < 0 {
		return append(table, p)
	}
	if table[i].Seq <= p.Seq {
		table[i] = p
	}

	return table
}

// bounded drops the oldest ended report of a table past MaxProcesses, as the guest drops it from its own.
func bounded(table []models.ProcessReport) []models.ProcessReport {
	for len(table) > models.MaxProcesses {
		i := oldestEnded(table)
		if i < 0 {
			i = lowestSeq(table)
		}
		table = slices.Delete(table, i, i+1)
	}

	return table
}

// oldestEnded is the index of the ended report with the lowest sequence, or -1 when every one still runs.
func oldestEnded(table []models.ProcessReport) int {
	oldest := -1
	for i, p := range table {
		if p.State.Ended() && (oldest < 0 || p.Seq < table[oldest].Seq) {
			oldest = i
		}
	}

	return oldest
}

// lowestSeq is the index of the report with the lowest sequence in a table that is not empty.
func lowestSeq(table []models.ProcessReport) int {
	lowest := 0
	for i, p := range table {
		if p.Seq < table[lowest].Seq {
			lowest = i
		}
	}

	return lowest
}

func named(name string) func(models.ProcessReport) bool {
	return func(p models.ProcessReport) bool { return p.Name == name }
}

// Follow lands the named process's output in its log, from a follower of its own unless one already runs.
func (h *Host) Follow(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil {
		return
	}
	if f, ok := h.follows[name]; ok {
		f.again = true

		return
	}
	f := &follower{}
	h.follows[name] = f
	h.followers.Go(func() { h.follow(name, f) })
}

// Kick ends every logs connection in use, which a host that only reads would wait on for good once a reset killed it.
func (h *Host) Kick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.follows {
		if f.round != nil {
			f.round()
		}
	}
}

// Close ends every follower and waits for it; the logs stay for the next boot's host.
func (h *Host) Close() {
	h.mu.Lock()
	h.cancel()
	h.mu.Unlock()
	h.followers.Wait()
}

// follow lands one process's output, and dials again after a drop while the VM runs, until the guest holds no process of the name.
func (h *Host) follow(name string, f *follower) {
	defer h.leave(name, f)
	out, err := openProcessLog(h.cfg.Dir, name)
	if err != nil {
		h.cfg.Lost(err)

		return
	}
	defer out.Close()

	for {
		round, end := context.WithCancel(h.ctx)
		h.mu.Lock()
		f.round, f.again = end, false
		logs := h.logs
		h.mu.Unlock()
		err := Logs(round, h.cfg.Dial, name, out, logs)
		end()
		if h.ctx.Err() != nil {
			return
		}
		// A file that refuses the log blocks the process on its output pipe, so every read of the sandbox says so; a redial would not help.
		if out.Err != nil {
			h.cfg.Lost(fmt.Errorf("the log of process %s stopped: %w", name, out.Err))

			return
		}
		if errors.Is(err, ErrLogsVersion) {
			h.cfg.Lost(fmt.Errorf("the log of process %s stopped: %w", name, err))

			return
		}
		if errors.Is(err, ErrNoProcess) && h.done(name, f) {
			return
		}
		// EOF is the guest powering off or letting go of the process, the normal end of a log.
		if err != nil && !errors.Is(err, ErrNoProcess) && !errors.Is(err, io.EOF) {
			h.cfg.Warn(fmt.Errorf("the log of process %s: %w", name, err))
		}
		if !h.cfg.Again(h.ctx) {
			return
		}
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(h.cfg.Pace):
		}
	}
}

// done lets the follower go, unless a report named the process meanwhile.
func (h *Host) done(name string, f *follower) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.again {
		f.again = false

		return false
	}
	delete(h.follows, name)

	return true
}

func (h *Host) leave(name string, f *follower) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.follows[name] == f {
		delete(h.follows, name)
	}
}

func openProcessLog(dir, name string) (*FileLog, error) {
	if err := os.MkdirAll(filepath.Join(dir, ProcessLogs), 0o700); err != nil {
		return nil, fmt.Errorf("make the process logs directory: %w", err)
	}
	file, err := os.OpenFile(ProcessLogPath(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the log of process %s: %w", name, err)
	}

	return &FileLog{File: file, Cursor: processCursor(dir, name), Max: MaxLog}, nil
}
