package runc_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// tableLine is one table as shard-init writes it on its status channel.
func tableLine(t *testing.T, reports ...models.ProcessReport) []byte {
	t.Helper()

	line, err := json.Marshal(models.ProcessTable{Kind: models.ProcessTableKind, Processes: reports})
	if err != nil {
		t.Fatal(err)
	}

	return append(append([]byte("\n"), line...), '\n')
}

func reportOf(name string, state models.ProcessState, restarts int) models.ProcessReport {
	return models.ProcessReport{Name: name, ProcessStatus: models.ProcessStatus{State: state, Restarts: restarts}, Seq: 1}
}

// A stop leaves the table as the last run had it, so a read after one still names each process.
func TestProcessesReadsTheTableTheLastRunLeft(t *testing.T) {
	const id = "amber-otter-1a2b"
	dir := t.TempDir()
	p := newProviderIn(t, dir, "exit 1")

	if rows, err := p.Processes(t.Context(), id); err != nil || rows != nil {
		t.Fatalf("Processes before any run = %v, %v, want none", rows, err)
	}

	b, err := bundle.Open(filepath.Join(dir, id))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(b.ExitFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.ExitFile, tableLine(t, reportOf("web", models.ProcessGaveUp, 2)), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, err := p.Processes(t.Context(), id)
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "web" || rows[0].State != models.ProcessGaveUp || rows[0].Restarts != 2 {
		t.Errorf("Processes = %+v, want web gave up after 2 restarts", rows)
	}
}

func TestProcessLogPathRefusesANameThatIsNoValidOne(t *testing.T) {
	p := newProvider(t)

	_, err := p.ProcessLogPath("amber-otter-1a2b", "../web")
	if err == nil || !strings.Contains(err.Error(), "amber-otter-1a2b") {
		t.Errorf("ProcessLogPath of ../web returned %v, want a refusal that names the sandbox", err)
	}
}

// shard-init holds each process log as the runtime holds the output log, so the daemon bounds them all.
func TestHeldLogsNamesTheOutputLogAndEachProcessLog(t *testing.T) {
	const id = "amber-otter-1a2b"
	dir := t.TempDir()
	p := newProviderIn(t, dir, "exit 1")
	b, err := bundle.Open(filepath.Join(dir, id))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.MkdirAll(b.Logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Logs, "web.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(b.Logs, "link.log")); err != nil {
		t.Fatal(err)
	}

	held, err := p.HeldLogs(id)
	if err != nil {
		t.Fatalf("HeldLogs: %v", err)
	}
	want := []string{filepath.Join(dir, id, "output.log"), filepath.Join(b.Logs, "web.log")}
	if !slices.Equal(held, want) {
		t.Errorf("HeldLogs = %v, want %v", held, want)
	}
}
