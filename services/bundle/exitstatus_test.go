package bundle_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
)

func TestReadExitStatus(t *testing.T) {
	const exit1 = "\n{\"kind\":\"exit\",\"code\":1,\"signal\":9}\n"
	const exit0 = "\n{\"kind\":\"exit\",\"code\":0,\"signal\":0}\n"

	cases := map[string]struct {
		content   string
		wantFound bool
		wantCode  int
		wantSig   int
	}{
		"one record":            {content: exit1, wantFound: true, wantCode: 1, wantSig: 9},
		"the last of many wins": {content: exit1 + exit0, wantFound: true, wantCode: 0},
		// A write still in flight has no closing newline, so the reader keeps the last complete record.
		"a torn write is skipped": {content: exit1 + "\n{\"kind\":\"exit\",\"code\":2", wantFound: true, wantCode: 1, wantSig: 9},
		// A foreign line is not an exit, so the reader waits rather than believe it.
		"a foreign kind is not an exit": {content: "\n{\"kind\":\"ready\"}\n", wantFound: false},
		// `echo > exit.json`, the ticket's forge, writes one empty line, which is not a record.
		"an empty line is not an exit": {content: "\n", wantFound: false},
		"no newline yet":               {content: "{\"kind\":\"exit\",\"code\":1}", wantFound: false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exit.json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("write the exit channel: %v", err)
			}

			exit, found, err := bundle.ReadExitStatus(path)
			if err != nil {
				t.Fatalf("ReadExitStatus: %v", err)
			}
			if found != c.wantFound {
				t.Fatalf("found = %v, want %v", found, c.wantFound)
			}
			if !c.wantFound {
				return
			}
			if exit.Code != c.wantCode || exit.Signal != c.wantSig {
				t.Errorf("exit = %+v, want code %d signal %d", exit, c.wantCode, c.wantSig)
			}
		})
	}
}

// A missing exit channel means the entrypoint has not exited, so the reader waits without an error.
func TestReadExitStatusMissingFile(t *testing.T) {
	exit, found, err := bundle.ReadExitStatus(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || found {
		t.Fatalf("ReadExitStatus of a missing file = %+v, %v, %v, want zero, false, nil", exit, found, err)
	}
}

// A complete line that is not JSON is a corrupt record, not a wait, so the reader surfaces the error.
func TestReadExitStatusRejectsACorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	if err := os.WriteFile(path, []byte("\nnot json\n"), 0o600); err != nil {
		t.Fatalf("write the exit channel: %v", err)
	}

	if _, _, err := bundle.ReadExitStatus(path); err == nil {
		t.Error("ReadExitStatus accepted a corrupt record, want an error")
	}
}

// On sysbox guest root can append to the exit file through PID 1, so the daemon reads a bounded prefix and empties the rest (SHARD-365).
func TestReadExitStatusEmptiesAFileOverTheCap(t *testing.T) {
	const exit1 = "\n{\"kind\":\"exit\",\"code\":1,\"signal\":0}\n"

	cases := map[string]func(t *testing.T, path string){
		"a record and 8 MiB with no newline": func(t *testing.T, path string) {
			write(t, path, exit1+strings.Repeat("x", 8<<20))
		},
		"8 MiB of empty lines after a record": func(t *testing.T, path string) {
			write(t, path, exit1+strings.Repeat("\n", 8<<20))
		},
		"a sparse file of 64 GiB": func(t *testing.T, path string) {
			write(t, path, exit1)
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
			_, found, err := bundle.ReadExitStatus(path)
			runtime.ReadMemStats(&after)

			if !errors.Is(err, models.ErrExitFileTooLarge) || found {
				t.Fatalf("ReadExitStatus = %v, %v, want ErrExitFileTooLarge", found, err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > heapBudget {
				t.Errorf("ReadExitStatus allocated %d bytes, want under %d", allocated, heapBudget)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Fatalf("the exit file is %d bytes after the refusal, want it emptied", info.Size())
			}

			// The writer appends, so the next exit lands at the start of the emptied file.
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(exit1)
			if closeErr := f.Close(); err != nil || closeErr != nil {
				t.Fatalf("append the next exit: %v %v", err, closeErr)
			}
			exit, found, err := bundle.ReadExitStatus(path)
			if err != nil || !found || exit.Code != 1 {
				t.Fatalf("ReadExitStatus after the next exit = %+v, %v, %v, want code 1", exit, found, err)
			}
		})
	}
}

// A record at the very end of a file of exactly the cap is still read, so the bound refuses only what shard-init never writes.
func TestReadExitStatusReadsAFileAtTheCap(t *testing.T) {
	const exit1 = "\n{\"kind\":\"exit\",\"code\":1,\"signal\":0}\n"
	path := filepath.Join(t.TempDir(), "exit.json")
	write(t, path, strings.Repeat("\n", 4<<10-len(exit1))+exit1)

	exit, found, err := bundle.ReadExitStatus(path)
	if err != nil || !found || exit.Code != 1 {
		t.Fatalf("ReadExitStatus = %+v, %v, %v, want code 1", exit, found, err)
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

func TestReadNotStarted(t *testing.T) {
	cases := map[string]struct {
		content  string
		wantCode int
	}{
		"a missing command is 127":    {content: "\n{\"kind\":\"not-started\",\"errno\":2}\n", wantCode: models.CommandNotFoundExitCode},
		"a command that cannot run":   {content: "\n{\"kind\":\"not-started\",\"errno\":13}\n", wantCode: models.CommandNotExecutableExitCode},
		"an exit is no refusal":       {content: "\n{\"kind\":\"exit\",\"code\":127}\n"},
		"a torn refusal is none yet":  {content: "\n{\"kind\":\"not-started\",\"errno\":2"},
		"an empty channel is nothing": {content: ""},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exit.json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("write the exit channel: %v", err)
			}

			refused, err := bundle.ReadNotStarted("sb-1", path)
			if err != nil {
				t.Fatalf("ReadNotStarted: %v", err)
			}
			if c.wantCode == 0 {
				if refused != nil {
					t.Fatalf("ReadNotStarted = %+v, want no refusal", refused)
				}
				return
			}
			if refused == nil {
				t.Fatal("ReadNotStarted found no refusal")
			}
			if refused.Code != c.wantCode || refused.Sandbox != "sb-1" {
				t.Errorf("refusal = %+v, want code %d in sandbox sb-1", refused, c.wantCode)
			}
		})
	}
}

// The guest writes only an errno, so a reason it puts in the record never reaches the message.
func TestReadNotStartedTakesTheReasonFromTheKernel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	if err := os.WriteFile(path, []byte("\n{\"kind\":\"not-started\",\"errno\":2,\"reason\":\"/srv/host/path\"}\n"), 0o600); err != nil {
		t.Fatalf("write the exit channel: %v", err)
	}

	refused, err := bundle.ReadNotStarted("sb-1", path)
	if err != nil {
		t.Fatalf("ReadNotStarted: %v", err)
	}
	if refused == nil || refused.Reason != "no such file or directory" {
		t.Fatalf("ReadNotStarted = %+v, want the reason of ENOENT", refused)
	}
}

func TestReadNotStartedMissingFile(t *testing.T) {
	refused, err := bundle.ReadNotStarted("sb-1", filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || refused != nil {
		t.Fatalf("ReadNotStarted of a missing file = %+v, %v, want nil, nil", refused, err)
	}
}

func TestReadNotStartedRejectsACorruptRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.json")
	if err := os.WriteFile(path, []byte("\nnot json\n"), 0o600); err != nil {
		t.Fatalf("write the exit channel: %v", err)
	}

	if _, err := bundle.ReadNotStarted("sb-1", path); err == nil {
		t.Error("ReadNotStarted accepted a corrupt record, want an error")
	}
}

// A sealed page is zero past the last record, as the memfd the daemon sized.
func TestDecodeNotStartedPage(t *testing.T) {
	page := make([]byte, models.ExitChannelSize)
	copy(page, "\n{\"kind\":\"not-started\",\"errno\":2}\n")

	refused := bundle.DecodeNotStartedPage("sb-1", page)
	if refused == nil || refused.Code != models.CommandNotFoundExitCode {
		t.Fatalf("DecodeNotStartedPage = %+v, want code %d", refused, models.CommandNotFoundExitCode)
	}

	copy(page, "\n{\"kind\":\"exit\",\"code\":0}\n\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	if refused := bundle.DecodeNotStartedPage("sb-1", page); refused != nil {
		t.Fatalf("DecodeNotStartedPage of an exit = %+v, want no refusal", refused)
	}
}
