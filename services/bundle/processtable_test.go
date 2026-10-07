package bundle_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

// heapBudget is far under the file the cap case plants, so a read that is not bounded shows.
const heapBudget = 1 << 20

func table(names ...string) string {
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"state":"running","restarts":1,"seq":3}`, name))
	}

	return "\n{\"kind\":\"processes\",\"processes\":[" + strings.Join(entries, ",") + "]}\n"
}

func TestReadProcessTable(t *testing.T) {
	cases := map[string]struct {
		content string
		want    []string
	}{
		"one table":             {content: table("web"), want: []string{"web"}},
		"the last of many wins": {content: table("web") + table("web", "worker"), want: []string{"web", "worker"}},
		// A write still in flight has no closing newline, so the reader keeps the last complete table.
		"a torn write is skipped": {content: table("web") + "\n{\"kind\":\"processes\",\"proc", want: []string{"web"}},
		"an empty table":          {content: table(), want: []string{}},
		// `echo > exit.json` writes one empty line, which is no table yet.
		"an empty line is no table": {content: "\n"},
		"no newline yet":            {content: strings.TrimSuffix(table("web"), "\n")[1:]},
		"an empty file":             {content: ""},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exit.json")
			write(t, path, c.content)

			got, err := bundle.ReadProcessTable(path)
			if err != nil {
				t.Fatalf("ReadProcessTable: %v", err)
			}
			if c.want == nil {
				if got != nil {
					t.Fatalf("ReadProcessTable = %+v, want no table", got)
				}

				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("ReadProcessTable = %+v, want %v", got, c.want)
			}
			for i, report := range got {
				if report.Name != c.want[i] || report.State != models.ProcessRunning || report.Restarts != 1 || report.Seq != 3 {
					t.Errorf("entry %d = %+v, want %s running with one restart at seq 3", i, report, c.want[i])
				}
			}
		})
	}
}

// A missing channel means shard-init has run nothing yet, so the reader answers no table without an error.
func TestReadProcessTableMissingFile(t *testing.T) {
	got, err := bundle.ReadProcessTable(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || got != nil {
		t.Fatalf("ReadProcessTable of a missing file = %+v, %v, want nil, nil", got, err)
	}
}

// Guest root can write the channel, so any whole line but a valid table is an error and never a table.
func TestReadProcessTableRejectsAForgedRecord(t *testing.T) {
	many := make([]string, models.MaxProcesses+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}

	cases := map[string]string{
		"a line that is not JSON":   "\nnot json\n",
		"a record of another kind":  "\n{\"kind\":\"exit\",\"code\":1}\n",
		"a record with no kind":     "\n{\"processes\":[]}\n",
		"more processes than a cap": table(many...),
		"a name with a slash":       table("../../etc/passwd"),
		"an empty name":             table(""),
		"a name past the longest":   table(strings.Repeat("a", models.MaxProcessName+1)),
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exit.json")
			write(t, path, content)

			got, err := bundle.ReadProcessTable(path)
			if err == nil {
				t.Fatalf("ReadProcessTable accepted %s as %+v, want an error", name, got)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal is %q, and it must name the file", err)
			}
		})
	}
}

// On sysbox guest root can append to the exit file through PID 1, so the daemon reads a bounded prefix and empties the rest (SHARD-365).
func TestReadProcessTableEmptiesAFileOverTheCap(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"a table and 8 MiB with no newline": func(t *testing.T, path string) {
			write(t, path, table("web")+strings.Repeat("x", 8<<20))
		},
		"8 MiB of empty lines after a table": func(t *testing.T, path string) {
			write(t, path, table("web")+strings.Repeat("\n", 8<<20))
		},
		"a sparse file of 64 GiB": func(t *testing.T, path string) {
			write(t, path, table("web"))
			if err := os.Truncate(path, 64<<30); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exit.json")
			plant(t, path)

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got, err := bundle.ReadProcessTable(path)
			runtime.ReadMemStats(&after)

			if !errors.Is(err, models.ErrExitFileTooLarge) || got != nil {
				t.Fatalf("ReadProcessTable = %+v, %v, want ErrExitFileTooLarge", got, err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > heapBudget {
				t.Errorf("ReadProcessTable allocated %d bytes, want under %d", allocated, heapBudget)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Fatalf("the exit file is %d bytes after the refusal, want it emptied", info.Size())
			}

			write(t, path, table("worker"))
			got, err = bundle.ReadProcessTable(path)
			if err != nil || len(got) != 1 || got[0].Name != "worker" {
				t.Fatalf("ReadProcessTable after the next table = %+v, %v, want worker", got, err)
			}
		})
	}
}

// A table at the very end of a file of exactly the cap is still read, so the bound refuses only what shard-init never writes.
func TestReadProcessTableReadsAFileAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	record := table("web")
	write(t, path, strings.Repeat("\n", models.ExitChannelSize-len(record))+record)

	got, err := bundle.ReadProcessTable(path)
	if err != nil || len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("ReadProcessTable = %+v, %v, want web", got, err)
	}
}

// A checkpoint carries the exit file through the same bounded read, so a pause never copies what the guest grew.
func TestExportRefusesAnExitFileOverTheCap(t *testing.T) {
	b, _ := build(t, newSpec(t), models.ImageConfig{})
	write(t, b.ExitFile, strings.Repeat("x", 1<<20))

	if err := b.Export(t.Context(), t.TempDir()); !errors.Is(err, models.ErrExitFileTooLarge) {
		t.Fatalf("Export = %v, want ErrExitFileTooLarge", err)
	}
}

// A sealed page is zero past the last table, as the memfd the daemon sized, and the exit file keeps it for a stopped sandbox.
func TestWriteProcessPageReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	page := make([]byte, models.ExitChannelSize)
	copy(page, table("web", "worker"))

	if err := bundle.WriteProcessPage(path, page); err != nil {
		t.Fatalf("WriteProcessPage: %v", err)
	}
	got, err := bundle.ReadProcessTable(path)
	if err != nil || len(got) != 2 || got[1].Name != "worker" || got[1].Restarts != 1 || got[1].Seq != 3 {
		t.Fatalf("ReadProcessTable = %+v, %v, want web and worker", got, err)
	}

	if err := bundle.WriteProcessPage(path, make([]byte, models.ExitChannelSize)); err != nil {
		t.Fatalf("WriteProcessPage of a zero page: %v", err)
	}
	if got, err := bundle.ReadProcessTable(path); err != nil || got != nil {
		t.Fatalf("ReadProcessTable of a zero page = %+v, %v, want no table", got, err)
	}
}

// The daemon keeps a forged page as it found it, so the read refuses it and a stop never does.
func TestWriteProcessPageLeavesAForgedPageToTheRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	page := make([]byte, models.ExitChannelSize)
	copy(page, "\n{\"kind\":\"exit\",\"code\":0}\n")

	if err := bundle.WriteProcessPage(path, page); err != nil {
		t.Fatalf("WriteProcessPage: %v", err)
	}
	if got, err := bundle.ReadProcessTable(path); err == nil {
		t.Fatalf("ReadProcessTable accepted a record of another kind as %+v", got)
	}

	if err := bundle.WriteProcessPage(path, make([]byte, models.ExitChannelSize+1)); err == nil {
		t.Fatal("WriteProcessPage kept a page past the channel")
	}
}
