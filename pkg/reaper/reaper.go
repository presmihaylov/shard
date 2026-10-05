// Package reaper ends the processes a test run marked, from a process that outlives the run, so a test binary killed mid-run leaves none of them behind.
package reaper

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/pidpin"
)

// roleEnv runs the test binary as the launcher or the reaper, and indexEnv names the index to the reaper.
const (
	roleEnv    = "SHARD_TEST_REAPER"
	indexEnv   = "SHARD_TEST_REAPER_INDEX"
	launchRole = "launch"
	runRole    = "run"
)

// Grace is how long End goes on with SIGKILL before it fails.
const Grace = 5 * time.Second

// Marks names processes to end, each with a start time so a pid reused since is spared: whole sessions by their leader's, single processes by their own.
type Marks struct {
	Sessions  map[int]string
	Processes map[int]string
}

// proc is one process of the table that a SIGKILL can still end.
type proc struct {
	pid   int
	sid   int
	start string
}

func (m *Marks) add(o Marks) {
	if m.Sessions == nil {
		m.Sessions = map[int]string{}
	}
	if m.Processes == nil {
		m.Processes = map[int]string{}
	}
	maps.Copy(m.Sessions, o.Sessions)
	maps.Copy(m.Processes, o.Processes)
}

// Role runs this process as the launcher or the reaper when Start started it as one, and then says so with its exit code; TestMain asks it first.
func Role() (bool, int) {
	switch os.Getenv(roleEnv) {
	case launchRole:
		if err := launch(); err != nil {
			fmt.Fprintln(os.Stderr, "launch the reaper:", err)

			return true, 1
		}

		return true, 0
	case runRole:
		// reap writes its error to the test binary, the one reader there is.
		if err := reap(); err != nil {
			return true, 1
		}

		return true, 0
	}

	return false, 0
}

// Start starts the reaper through a launcher that exits, so a kill of this process, its group or its tree misses it; once this process calls the func it returns, or dies, the reaper ends what the notes files that index lists still mark, and the func returns any failure of that.
func Start(index string) (func() error, error) {
	alive, held, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	report, reported, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, alive.Close(), held.Close())
	}
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), roleEnv+"="+launchRole, indexEnv+"="+index)
	launcher.ExtraFiles = []*os.File{alive, reported}
	launcher.Stderr = os.Stderr
	// Once the launcher has run, only the reaper holds these two ends, so its exit closes them.
	if err := errors.Join(launcher.Run(), alive.Close(), reported.Close()); err != nil {
		return nil, errors.Join(err, held.Close(), report.Close())
	}

	return func() error {
		if err := held.Close(); err != nil {
			return errors.Join(err, report.Close())
		}
		out, err := io.ReadAll(report)
		if err := errors.Join(err, report.Close()); err != nil {
			return err
		}
		if len(out) > 0 {
			return errors.New(strings.TrimSpace(string(out)))
		}

		return nil
	}, nil
}

// launch starts the reaper in a session of its own, over fds 3 and 4, and exits, which leaves init its parent.
func launch() error {
	reaper := exec.Command(os.Args[0])
	reaper.Env = append(os.Environ(), roleEnv+"="+runRole)
	reaper.ExtraFiles = []*os.File{os.NewFile(3, "alive"), os.NewFile(4, "report")}
	reaper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := reaper.Start(); err != nil {
		return err
	}

	return reaper.Process.Release()
}

// reap waits until the test binary closes fd 3, at the end of its run or by its death, then ends what the index marks, and writes any failure to fd 4.
func reap() error {
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	report := os.NewFile(4, "report")
	err := awaitAndEnd(os.NewFile(3, "alive"), os.Getenv(indexEnv))
	if err != nil {
		_, werr := fmt.Fprintln(report, err)
		err = errors.Join(err, werr)
	}

	return errors.Join(err, report.Close())
}

func awaitAndEnd(alive *os.File, index string) error {
	// The test binary never writes to the pipe, so the copy returns only once the binary closes it or dies.
	if _, err := io.Copy(io.Discard, alive); err != nil {
		return fmt.Errorf("wait for the test binary: %w", err)
	}

	return End(func() (Marks, error) { return Live(index) })
}

// Session marks the whole session that the live process sid leads.
func Session(sid int) (Marks, error) {
	start, err := started(sid)
	if err != nil {
		return Marks{}, fmt.Errorf("read the start time of %d: %w", sid, err)
	}

	return Marks{Sessions: map[int]string{sid: start}}, nil
}

// Process marks the live process pid alone.
func Process(pid int) (Marks, error) {
	start, err := started(pid)
	if err != nil {
		return Marks{}, fmt.Errorf("read the start time of %d: %w", pid, err)
	}

	return Marks{Processes: map[int]string{pid: start}}, nil
}

// String is m as the lines of a notes file: s<sid>@<start> for a session, p<pid>@<start> for a process.
func (m Marks) String() string {
	var lines []string
	for sid, start := range m.Sessions {
		lines = append(lines, fmt.Sprintf("s%d@%s", sid, start))
	}
	for pid, start := range m.Processes {
		lines = append(lines, fmt.Sprintf("p%d@%s", pid, start))
	}

	return strings.Join(lines, "\n")
}

// Note appends line to the notes file at path; a file already gone is no error, since its owner removes it only once it has ended what it marked.
func Note(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(line + "\n")

	return errors.Join(err, f.Close())
}

// Read is what the notes file at path marks.
func Read(path string) (Marks, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return Marks{}, err
	}
	marks := Marks{Sessions: map[int]string{}, Processes: map[int]string{}}
	kinds := map[rune]map[int]string{'s': marks.Sessions, 'p': marks.Processes}
	for line := range strings.FieldsSeq(string(blob)) {
		var kind rune
		var id int
		var start string
		if _, err := fmt.Sscanf(line, "%c%d@%s", &kind, &id, &start); err != nil {
			return Marks{}, fmt.Errorf("parse the mark %q in %s: %w", line, path, err)
		}
		into, known := kinds[kind]
		if !known {
			return Marks{}, fmt.Errorf("the mark %q in %s names no session and no process", line, path)
		}
		into[id] = start
	}

	return marks, nil
}

// Live is every mark of the notes files that index lists and that are still there.
func Live(index string) (Marks, error) {
	blob, err := os.ReadFile(index)
	// A run removes its index only once its marks are ended.
	if errors.Is(err, fs.ErrNotExist) {
		return Marks{}, nil
	}
	if err != nil {
		return Marks{}, err
	}
	var marks Marks
	for path := range strings.FieldsSeq(string(blob)) {
		noted, err := Read(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Marks{}, err
		}
		marks.add(noted)
	}

	return marks, nil
}

// Left is every process that m marks and that a SIGKILL can still end; a zombie holds nothing, and its parent reaps it.
func Left(m Marks) ([]int, error) {
	procs, err := table()
	if err != nil {
		return nil, err
	}
	starts := make(map[int]string, len(procs))
	for _, p := range procs {
		starts[p.pid] = p.start
	}
	var pids []int
	for _, p := range procs {
		start, one := m.Processes[p.pid]
		if marked(m, p.sid, starts) || (one && start == p.start) {
			pids = append(pids, p.pid)
		}
	}

	return pids, nil
}

// marked says whether m marks the session sid; a pid held by a leader of another start is free again only because the marked session emptied, so this is another session.
func marked(m Marks, sid int, starts map[int]string) bool {
	start, session := m.Sessions[sid]
	leader, led := starts[sid]

	return session && (!led || leader == start)
}

// End SIGKILLs every process that marks reads, round after round, as a mark may land while it runs, until none is left or Grace passes.
func End(marks func() (Marks, error)) error {
	for deadline := time.Now().Add(Grace); ; time.Sleep(20 * time.Millisecond) {
		m, err := marks()
		if err != nil {
			return fmt.Errorf("read the marks: %w", err)
		}
		left, err := Left(m)
		if err != nil {
			return fmt.Errorf("list the marked processes: %w", err)
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %v are left %s after SIGKILL", left, Grace)
		}
		if err := kill(m, left); err != nil {
			return err
		}
	}
}

// kill pins each pid before Left reads the table again, so a pid a later process took since the first read is never signaled.
func kill(m Marks, pids []int) error {
	var pins []*pidpin.Process
	for _, pid := range pids {
		pin, err := pidpin.Open(pid)
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return errors.Join(fmt.Errorf("pin %d: %w", pid, err), release(pins))
		}
		pins = append(pins, pin)
	}
	still, err := Left(m)
	if err != nil {
		return errors.Join(fmt.Errorf("list the marked processes: %w", err), release(pins))
	}
	var errs []error
	for _, pin := range pins {
		if slices.Contains(still, pin.PID()) {
			errs = append(errs, pin.Kill())
		}
	}

	return errors.Join(append(errs, release(pins))...)
}

func release(pins []*pidpin.Process) error {
	var errs []error
	for _, pin := range pins {
		errs = append(errs, pin.Close())
	}

	return errors.Join(errs...)
}

// Require skips t only where this host refuses to list the session of a live child, as a seatbelt sandbox does; any other failure fails t.
func Require(t testing.TB) {
	t.Helper()
	refusal, err := tableRefusal()
	if err != nil {
		t.Fatalf("list the session of a live child: %v", err)
	}
	if refusal != "" {
		t.Skip(refusal)
	}
}

// tableRefusal is why this host refuses to list the session of a live child, or "" where it lists one; a run asks once, as every harness would pay a full scan.
var tableRefusal = sync.OnceValues(func() (string, error) {
	child := exec.Command("sleep", "60")
	// A session of its own, so the child alone is in it.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return "", err
	}
	refusal, err := listLiveChild(child.Process.Pid)
	if kill := child.Process.Kill(); kill != nil {
		return "", errors.Join(err, kill)
	}
	var exit *exec.ExitError
	if wait := child.Wait(); wait != nil && !errors.As(wait, &exit) {
		return "", errors.Join(err, wait)
	}

	return refusal, err
})

func listLiveChild(pid int) (string, error) {
	mark, err := Session(pid)
	var left []int
	if err == nil {
		left, err = Left(mark)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Sprintf("this host refuses to list the session of a live child, so no test can end what it marked: %v", err), nil
	}
	if err != nil {
		return "", err
	}
	if len(left) != 1 || left[0] != pid {
		return "", fmt.Errorf("the session of live child %d lists %v", pid, left)
	}

	return "", nil
}
