package bundle_test

import (
	"os"
	"path/filepath"
	"testing"

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
