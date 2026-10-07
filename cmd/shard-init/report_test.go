package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// asStdin makes f the guest's fd 0 for one test, as the host's status channel.
func asStdin(t *testing.T, f *os.File) {
	t.Helper()

	stdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = stdin })
}

// lastTable decodes the last whole line of a status channel, cut at the first NUL a sealed page pads with.
func lastTable(t *testing.T, blob []byte) models.ProcessTable {
	t.Helper()

	if end := bytes.IndexByte(blob, 0); end >= 0 {
		blob = blob[:end]
	}
	var line []byte
	for candidate := range bytes.SplitSeq(blob, []byte{'\n'}) {
		if trimmed := bytes.TrimSpace(candidate); len(trimmed) > 0 {
			line = trimmed
		}
	}
	var table models.ProcessTable
	if err := json.Unmarshal(line, &table); err != nil {
		t.Fatalf("the status channel's last line %q is not a table: %v", line, err)
	}
	if table.Kind != models.ProcessTableKind {
		t.Fatalf("the status channel holds a %q record, want %q", table.Kind, models.ProcessTableKind)
	}

	return table
}

// fd 0 is the host's status file, opened for append; on sysbox guest root can append to it too (SHARD-365).
func TestFileReporterKeepsOneTableOnFd0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	asStdin(t, f)

	if _, err := f.WriteString(strings.Repeat("guest bytes with no newline ", 1<<15)); err != nil {
		t.Fatal(err)
	}
	var table []models.ProcessReport
	for i := range 300 {
		p := models.ProcessReport{Name: fmt.Sprintf("p%d", i%3), ProcessStatus: models.ProcessStatus{State: models.ProcessRunning}, Seq: uint64(i + 1)}
		table = append(table[:min(len(table), 2)], p)
		if err := (&fileReporter{}).changed(p, table); err != nil {
			t.Fatal(err)
		}
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(models.ProcessTable{Kind: models.ProcessTableKind, Processes: table})
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "\n"+string(want)+"\n" {
		t.Fatalf("fd 0 holds %d bytes, want only the last table %s", len(blob), want)
	}
	if got := lastTable(t, blob); len(got.Processes) != 3 || got.Processes[2].Seq != 300 {
		t.Errorf("fd 0 reads %+v, want the last table", got)
	}
}

// A status channel the guest cannot write is reported, so PID 1 logs it and lives on.
func TestFileReporterSaysWhenFd0TakesNothing(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "status.json"), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	asStdin(t, f)

	err = (&fileReporter{}).changed(models.ProcessReport{}, nil)
	if err == nil || !strings.Contains(err.Error(), "report the process table on fd 0") {
		t.Fatalf("a read-only fd 0 gave %v, want the failed report named", err)
	}
}

// worstTable is the largest table the guest can hold: every name at its longest, every number at its most, an exit by signal.
func worstTable() models.ProcessTable {
	startedAt := time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.FixedZone("", -12*3600))
	table := models.ProcessTable{Kind: models.ProcessTableKind}
	for i := range models.MaxProcesses {
		name := fmt.Sprintf("%02d%s", i, strings.Repeat("x", models.MaxProcessName-2))
		exit := models.ExitStatus{Code: math.MinInt, Signal: math.MinInt}
		table.Processes = append(table.Processes, models.ProcessReport{
			Name:          name,
			ProcessStatus: models.ProcessStatus{State: models.ProcessRestarting, Restarts: math.MinInt, Exit: &exit, StartedAt: startedAt},
			Seq:           math.MaxUint64,
		})
	}

	return table
}

// MaxProcesses and MaxProcessName exist so the whole table fits the sysbox page; a status that did not fit would never reach the host.
func TestTheWorstTableFitsTheSealedPage(t *testing.T) {
	table := worstTable()
	for _, p := range table.Processes {
		if !models.ValidProcessName(p.Name) {
			t.Fatalf("%q is not a name the guest takes", p.Name)
		}
	}
	encoded, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	if framed := len(encoded) + 2; framed > models.ExitChannelSize {
		t.Fatalf("the worst table frames to %d bytes, past the %d byte page", framed, models.ExitChannelSize)
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "page"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := writePage(f, encoded); err != nil {
		t.Fatalf("write the worst table: %v", err)
	}
	if err := writePage(f, append(encoded, bytes.Repeat([]byte{' '}, models.ExitChannelSize)...)); err == nil {
		t.Fatal("a table past the page was written")
	}
}

// The log directory is the host's, but guest root may plant a link in it to make PID 1 write elsewhere.
func TestFileReporterRefusesALinkPlantedAsALog(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "logs")
	if err := os.Mkdir(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(logs, supervisor.ProcessLogName("web"))); err != nil {
		t.Fatal(err)
	}

	r := &fileReporter{logs: logs, outputs: map[string]*os.File{}}
	out, err := r.output("web")
	if err == nil {
		t.Cleanup(func() { r.keep(nil) })
		t.Fatalf("the reporter opened %s through a planted link", out.Name())
	}
	if !errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), `"web"`) {
		t.Errorf("the refusal reads %v, want ELOOP naming the process", err)
	}
}

// keep drops only the logs of names the guest let go of; one still named stays open and is the same file again.
func TestFileReporterKeepsTheLogsOfTheNamesItHolds(t *testing.T) {
	r := &fileReporter{logs: t.TempDir(), outputs: map[string]*os.File{}}
	t.Cleanup(func() { r.keep(nil) })

	web, err := r.output("web")
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.output("job")
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.output("web")
	if err != nil || again != web {
		t.Fatalf("a second output of web gave %v, %v; want the open log", again, err)
	}

	r.keep([]string{"web"})
	if _, err := job.WriteString("late"); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a write to job's log after keep gave %v, want it closed", err)
	}
	if _, err := web.WriteString("still\n"); err != nil {
		t.Errorf("web's log closed with web still held: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(r.logs, supervisor.ProcessLogName("web")))
	if err != nil || string(blob) != "still\n" {
		t.Errorf("web's log holds %q, %v", blob, err)
	}
}

// A run of a name appends to its log, so the host's copy and truncate bound the file and never a run.
func TestFileReporterAppendsToALogThatIsThere(t *testing.T) {
	r := &fileReporter{logs: t.TempDir(), outputs: map[string]*os.File{}}
	t.Cleanup(func() { r.keep(nil) })
	path := filepath.Join(r.logs, supervisor.ProcessLogName("web"))
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := r.output("web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil || string(blob) != "before\nafter\n" {
		t.Errorf("the log holds %q, %v; want the new run appended", blob, err)
	}
}
