package supervisor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/services/supervisor"
)

// A guest the cursor has not seen lands from the oldest byte it holds, a host that comes back resumes where the file ends, and a guest that holds less than the file says starts over.
func TestAFileLogResumesWhereTheFileEnds(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	log := &supervisor.FileLog{File: f, Cursor: filepath.Join(dir, "output.cursor")}
	// An earlier boot left its output in the file.
	if _, err := log.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}

	resume := func(from, to, want uint64) {
		t.Helper()
		at, err := log.Resume(from, to)
		if err != nil || at != want {
			t.Fatalf("Resume(%d, %d) = %d, %v, want %d", from, to, at, err, want)
		}
	}
	resume(0, 10, 0)
	if _, err := log.Write([]byte("0123")); err != nil {
		t.Fatal(err)
	}
	resume(2, 10, 4)
	resume(0, 3, 0)
	if log.Err != nil {
		t.Fatalf("Err = %v, want none", log.Err)
	}
}
