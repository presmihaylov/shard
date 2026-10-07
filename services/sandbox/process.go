package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
)

// RunRequest is one named process to start in a sandbox that already runs. It is the body of POST /v0/sandboxes/{id}/processes.
type RunRequest struct {
	Name    string   `json:"name,omitempty" doc:"The process name: lowercase letters, digits, '.', '_' and '-', at most 32, starting with a letter or a digit. Absent is the base name of command[0]."`
	Command []string `json:"command" minItems:"1"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// Restart is when shard-init starts the process again, and whether a start of the sandbox does; absent is unless-stopped.
	Restart *models.RestartSpec `json:"restart,omitempty"`
}

// Run starts one named process under shard-init in a running sandbox and answers its entry.
func (s *Service) Run(ctx context.Context, ref string, req RunRequest) (models.Process, error) {
	proc, err := newProcess(req)
	if err != nil {
		return models.Process{}, err
	}

	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Process{}, err
	}

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Process{}, err
	}
	defer unlock()

	id, sb, err := s.readyForExec(ctx, id)
	if err != nil {
		return models.Process{}, err
	}

	procs, err := s.seen(ctx, id, sb)
	if err != nil {
		return models.Process{}, err
	}
	procs, err = admitted(id, sb, procs, proc.Name)
	if err != nil {
		return models.Process{}, err
	}

	return s.startProcess(ctx, id, sb, procs, proc)
}

// newProcess is the entry a run asks for, or the refusal of what no guest could start.
func newProcess(req RunRequest) (models.Process, error) {
	if len(req.Command) == 0 {
		return models.Process{}, &RequestError{Err: errors.New("the request names no command to run")}
	}

	name := req.Name
	if name == "" {
		name = path.Base(req.Command[0])
	}
	if !models.ValidProcessName(name) {
		return models.Process{}, &RequestError{Err: fmt.Errorf("the process name %q is not lowercase letters, digits, '.', '_' and '-', at most %d and starting with a letter or a digit; pass one with --name, or name in the request", name, models.MaxProcessName)}
	}

	restart, err := restartOf(req.Restart)
	if err != nil {
		return models.Process{}, &RequestError{Err: err}
	}

	// An entry that is not an assignment is dropped by the merge, and the process then lacks it without a word.
	for _, entry := range req.Env {
		if key, _, found := strings.Cut(entry, "="); !found || key == "" {
			return models.Process{}, &RequestError{Err: fmt.Errorf("the environment entry %q is not KEY=VALUE", entry)}
		}
	}

	return models.Process{
		Name:    name,
		Command: slices.Clone(req.Command),
		Env:     slices.Clone(req.Env),
		WorkDir: req.WorkDir,
		User:    req.User,
		Restart: restart,
	}, nil
}

// admitted drops the entry a run of name replaces, and the oldest ended one when the table is full; a name that still runs is taken.
func admitted(id string, sb models.Sandbox, procs []models.Process, name string) ([]models.Process, error) {
	if i := slices.IndexFunc(procs, named(name)); i >= 0 {
		if !procs[i].Status.State.Ended() {
			return nil, nameTaken(id, sb, name)
		}
		procs = slices.Delete(procs, i, i+1)
	}
	if len(procs) < models.MaxProcesses {
		return procs, nil
	}

	// A run appends, so the first ended entry is the one that ran longest ago.
	oldest := slices.IndexFunc(procs, func(p models.Process) bool { return p.Status.State.Ended() })
	if oldest < 0 {
		return nil, &StateError{Sandbox: nameOf(id, sb), State: sb.State, Fix: fmt.Sprintf("its %d processes all still run, the most a sandbox holds; shard kill %s <name> ends one", models.MaxProcesses, nameOf(id, sb)), Code: models.CodeProcessLimit}
	}

	return slices.Delete(procs, oldest, oldest+1), nil
}

// startProcess writes p as running before the guest starts it, so a daemon that dies between the two still owns the name.
func (s *Service) startProcess(ctx context.Context, id string, before models.Sandbox, procs []models.Process, p models.Process) (models.Process, error) {
	start, err := s.logSize(id, p.Name)
	if err != nil {
		return models.Process{}, err
	}

	p.Status = models.ProcessStatus{State: models.ProcessRunning, StartedAt: time.Now().UTC()}
	procs = withProcess(procs, p)
	err = s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.Processes = procs
		rec.LogStarts = withLogStart(rec.LogStarts, p.Name, start)

		return nil
	})
	if err != nil {
		return models.Process{}, fmt.Errorf("sandbox %s: record process %s: %w", id, p.Name, err)
	}

	err = s.cfg.Provider.StartProcess(ctx, id, models.ProcessSpec{Name: p.Name, Argv: p.Command, Env: p.Env, WorkDir: p.WorkDir, User: p.User, Restart: p.Restart})
	if err == nil {
		return p, nil
	}

	return s.unstarted(id, before, p, err)
}

// unstarted puts the entries back when the guest kept or never knew the name, and otherwise leaves the process exited, which the guest's own word overrides if it runs after all.
func (s *Service) unstarted(id string, before models.Sandbox, p models.Process, startErr error) (models.Process, error) {
	if errors.Is(startErr, models.ErrProcessRunning) || errors.Is(startErr, models.ErrUnsupported) {
		err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
			rec.Processes = before.Processes
			rec.LogStarts = before.LogStarts

			return nil
		})
		if err != nil {
			return models.Process{}, errors.Join(startErr, fmt.Errorf("sandbox %s: put back the processes a refused run of %s replaced: %w", id, p.Name, err))
		}
		if errors.Is(startErr, models.ErrProcessRunning) {
			return models.Process{}, nameTaken(id, before, p.Name)
		}

		return models.Process{}, startErr
	}

	p.Status.State = models.ProcessExited
	if refused, ok := errors.AsType[*models.CommandNotStartedError](startErr); ok {
		p.Status.Exit = &models.ExitStatus{Code: refused.Code}
		startErr = nameCommand(startErr, nameOf(id, before), p.Command)
	}
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		rec.Processes = withProcess(slices.Clone(rec.Processes), p)

		return nil
	})
	if err != nil {
		return models.Process{}, errors.Join(startErr, fmt.Errorf("sandbox %s: record that process %s did not start: %w", id, p.Name, err))
	}

	return p, startErr
}

// logSize is where the next run of a process begins in its log, which outlives the runs before it.
func (s *Service) logSize(id, name string) (int64, error) {
	logPath, err := s.cfg.Provider.ProcessLogPath(id, name)
	if err != nil {
		return 0, err
	}

	info, err := os.Stat(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sandbox %s: stat the log of process %s: %w", id, name, err)
	}

	return info.Size(), nil
}

// Processes answers every process of the sandbox, as shard-init says it stands while the sandbox runs and as the record last read it otherwise.
func (s *Service) Processes(ctx context.Context, ref string) ([]models.Process, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return nil, err
	}

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return nil, err
	}

	return s.seen(ctx, id, sb)
}

// Process answers one process as Processes shows it.
func (s *Service) Process(ctx context.Context, ref, name string) (models.Process, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Process{}, err
	}

	p, sb, ok, err := s.processOf(ctx, id, name)
	if err != nil {
		return models.Process{}, err
	}
	if !ok {
		return models.Process{}, noProcess(id, sb, name)
	}

	return p, nil
}

// processOf reads the record again, so a verb that polls sees a run, a kill or a stop that landed since.
func (s *Service) processOf(ctx context.Context, id, name string) (models.Process, models.Sandbox, bool, error) {
	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Process{}, models.Sandbox{}, false, err
	}

	procs, err := s.seen(ctx, id, sb)
	if err != nil {
		return models.Process{}, models.Sandbox{}, false, err
	}

	i := slices.IndexFunc(procs, named(name))
	if i < 0 {
		return models.Process{}, sb, false, nil
	}

	return procs[i], sb, true, nil
}

// sighting is one poll of a followed process: the substrate's word, then the process while the sandbox is alive.
type sighting struct {
	status models.Status
	p      models.Process
	sb     models.Sandbox
	found  bool
}

// sight reads the substrate, not the record, because a record saying running outlives an OOM kill.
func (s *Service) sight(ctx context.Context, id, name string) (sighting, error) {
	status, err := s.cfg.Provider.Status(ctx, id)
	if err != nil {
		return sighting{}, err
	}
	if !status.Alive() {
		return sighting{status: status}, nil
	}

	p, sb, found, err := s.processOf(ctx, id, name)
	if err != nil {
		return sighting{}, err
	}

	return sighting{status: status, p: p, sb: sb, found: found}, nil
}

// seen lays shard-init's table over the record's entries while the sandbox runs; a table the guest broke leaves the record's.
func (s *Service) seen(ctx context.Context, id string, sb models.Sandbox) ([]models.Process, error) {
	if !sb.State.Live() {
		return slices.Clone(sb.Processes), nil
	}

	reports, _, err := s.table(ctx, id)
	if err != nil {
		return nil, err
	}

	return merged(sb.Processes, reports), nil
}

// table reads shard-init's table, and answers why instead for a channel only guest root can break, which the record names as ExitChannel.
func (s *Service) table(ctx context.Context, id string) ([]models.ProcessReport, string, error) {
	reports, err := s.cfg.Provider.Processes(ctx, id)
	if errors.Is(err, models.ErrExitChannelReplaced) || errors.Is(err, models.ErrExitFileTooLarge) {
		return nil, err.Error(), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sandbox %s: read its processes: %w", id, err)
	}

	return reports, "", nil
}

// merged is a copy of procs with the guest's latest report on each; a name the record never ran is dropped, since guest root can forge one.
func merged(procs []models.Process, reports []models.ProcessReport) []models.Process {
	latest := latestReports(reports)
	out := slices.Clone(procs)
	for i := range out {
		if r, ok := latest[out[i].Name]; ok {
			out[i].Status = r.ProcessStatus
		}
	}

	return out
}

// latestReports is the guest's last word on each name, since a table may hold several reports of one.
func latestReports(reports []models.ProcessReport) map[string]models.ProcessReport {
	latest := map[string]models.ProcessReport{}
	for _, r := range reports {
		if prev, ok := latest[r.Name]; ok && prev.Seq > r.Seq {
			continue
		}
		latest[r.Name] = r
	}

	return latest
}

// endProcesses is a run that ended under its processes: whatever still ran or waited to start again is stopped.
func endProcesses(procs []models.Process) []models.Process {
	out := slices.Clone(procs)
	for i := range out {
		if !out[i].Status.State.Ended() {
			out[i].Status.State = models.ProcessStopped
		}
	}

	return out
}

// Kill ends one process and cancels its restarts, which also keeps an unless-stopped one down on the next start; the sandbox stays up.
func (s *Service) Kill(ctx context.Context, ref, name string, force bool) (models.Process, error) {
	id, err := s.cfg.Repo.Resolve(ref)
	if err != nil {
		return models.Process{}, err
	}

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return models.Process{}, err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return models.Process{}, err
	}
	if err := FailedGuard(id, sb); err != nil {
		return models.Process{}, err
	}
	if !slices.ContainsFunc(sb.Processes, named(name)) {
		return models.Process{}, noProcess(id, sb, name)
	}

	// Nothing runs it, so the mark is the whole kill: it keeps the next start from bringing it back.
	if sb.State == models.StateStopped || sb.State == models.StateCreated {
		return s.recordKill(id, name, nil, false)
	}

	if _, _, err := s.readyForExec(ctx, id); err != nil {
		return models.Process{}, err
	}

	grace := models.StopGrace
	if force {
		grace = 0
	}
	if err := s.cfg.Provider.StopProcess(ctx, id, name, grace); err != nil {
		return models.Process{}, fmt.Errorf("sandbox %s: kill process %s: %w", id, name, err)
	}

	reports, _, err := s.table(ctx, id)
	if err != nil {
		return models.Process{}, err
	}

	return s.recordKill(id, name, reports, true)
}

// recordKill marks one process killed with the guest's last word on it; reaped says the guest ended it, whatever a table that lags still says.
func (s *Service) recordKill(id, name string, reports []models.ProcessReport, reaped bool) (models.Process, error) {
	var killed models.Process
	err := s.cfg.Repo.Update(id, func(rec *models.Sandbox) error {
		procs := merged(rec.Processes, reports)
		i := slices.IndexFunc(procs, named(name))
		if i < 0 {
			return fmt.Errorf("sandbox %s holds no process %s", id, name)
		}
		procs[i].Killed = true
		if reaped && !procs[i].Status.State.Ended() {
			procs[i].Status.State = models.ProcessKilled
		}
		rec.Processes = procs
		killed = procs[i]

		return nil
	})
	if err != nil {
		return models.Process{}, fmt.Errorf("sandbox %s: record the kill of process %s: %w", id, name, err)
	}

	return killed, nil
}

// AttachProcess copies one process's output from the start of its current run into open's writer, and answers the process once its policy ended it; open runs after every refusal, so a refusal precedes the answer.
func (s *Service) AttachProcess(ctx context.Context, ref, name string, open func() (io.Writer, error)) (p models.Process, err error) {
	id, sb, err := s.readyForExec(ctx, ref)
	if err != nil {
		return models.Process{}, err
	}
	if !slices.ContainsFunc(sb.Processes, named(name)) {
		return models.Process{}, noProcess(id, sb, name)
	}

	t, err := s.openProcessLog(id, name, sb.LogStarts[name])
	if err != nil {
		return models.Process{}, err
	}
	defer func() { err = errors.Join(err, t.close()) }()

	w, err := open()
	if err != nil {
		return models.Process{}, err
	}

	for {
		// Everything is read before the copy, so what the process wrote on its way out is drained.
		seen, err := s.sight(ctx, id, name)
		if err != nil {
			return models.Process{}, err
		}

		if err := t.follow(w); err != nil {
			return models.Process{}, err
		}

		if !seen.status.Alive() {
			return models.Process{}, &StateError{Sandbox: nameOf(id, sb), State: seen.status.State, Fix: fmt.Sprintf("its process %s had not ended; shard start %s brings it back if its policy says so", name, nameOf(id, sb)), Code: models.CodeSandboxNotRunning}
		}
		// A run of another name filled the table and evicted this one, which had ended.
		if !seen.found {
			return models.Process{}, noProcess(id, seen.sb, name)
		}
		if seen.p.Status.State.Ended() {
			// The tick copies the table a second later, so a ps right after the end would still read it running (SHARD-479).
			return seen.p, s.recordProcessesNow(ctx, id)
		}

		select {
		case <-ctx.Done():
			return models.Process{}, ctx.Err()
		case <-time.After(followInterval):
		}
	}
}

// recordProcessesNow makes the tick's write under the lock the tick only tries.
func (s *Service) recordProcessesNow(ctx context.Context, id string) error {
	unlock, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	sb, err := s.cfg.Repo.Get(id)
	if err != nil {
		return err
	}

	return s.writeProcesses(ctx, id, sb, s.report)
}

// openProcessLog opens one process's log at offset, which the provider names per sandbox and process.
func (s *Service) openProcessLog(id, name string, offset int64) (*tail, error) {
	logPath, err := s.cfg.Provider.ProcessLogPath(id, name)
	if err != nil {
		return nil, err
	}

	t, err := openTailFrom(logPath, offset)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: open the output of process %s: %w", id, name, err)
	}

	return t, nil
}

func named(name string) func(models.Process) bool {
	return func(p models.Process) bool { return p.Name == name }
}

// withProcess replaces the entry of p's name in place, or appends p.
func withProcess(procs []models.Process, p models.Process) []models.Process {
	if i := slices.IndexFunc(procs, named(p.Name)); i >= 0 {
		procs[i] = p

		return procs
	}

	return append(procs, p)
}

// withLogStart is a copy, since the map may be the one a record read before shares.
func withLogStart(starts map[string]int64, name string, at int64) map[string]int64 {
	out := make(map[string]int64, len(starts)+1)
	maps.Copy(out, starts)
	out[name] = at

	return out
}

func nameTaken(id string, sb models.Sandbox, name string) error {
	return &StateError{Sandbox: nameOf(id, sb), State: sb.State, Fix: fmt.Sprintf("its process %s still runs; shard kill %s %s ends it, or run this one under another --name", name, nameOf(id, sb), name), Code: models.CodeNameTaken}
}

func noProcess(id string, sb models.Sandbox, name string) error {
	return &StateError{Sandbox: nameOf(id, sb), State: sb.State, Fix: fmt.Sprintf("it has no process %s; shard ps %s lists the ones it has", name, nameOf(id, sb)), Code: models.CodeNoProcess}
}
