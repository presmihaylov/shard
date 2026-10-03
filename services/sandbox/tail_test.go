package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/pkg/logfile"
)

// appender is a runtime holding the log with O_APPEND, as every substrate does.
func appender(t *testing.T, path string) *os.File {
	t.Helper()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	})

	return f
}

func writeLog(t *testing.T, f *os.File, text string) {
	t.Helper()

	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func openTestTail(t *testing.T, path string) *tail {
	t.Helper()

	tl, err := openTail(path)
	if err != nil {
		t.Fatalf("openTail: %v", err)
	}
	t.Cleanup(func() {
		if err := tl.close(); err != nil {
			t.Errorf("close the tail: %v", err)
		}
	})

	return tl
}

func followTail(t *testing.T, tl *tail, out *bytes.Buffer) {
	t.Helper()

	if err := tl.follow(out); err != nil {
		t.Fatalf("follow: %v", err)
	}
}

func TestTailReadsTheRotatedFileFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(logfile.Rotated(path), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := openTestTail(t, path).read(&out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.String() != "old\nnew\n" {
		t.Errorf("read %q", out.String())
	}
}

// The writer refills the log past the old offset before the follow looks, so only the rotated file tells the truncate.
func TestTailFollowsATruncateFromTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	runtime := appender(t, path)
	writeLog(t, runtime, "first\n")

	var out bytes.Buffer
	tl := openTestTail(t, path)
	followTail(t, tl, &out)

	if err := logfile.Truncate(path, 3); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	writeLog(t, runtime, "second\n")
	followTail(t, tl, &out)

	if out.String() != "first\nsecond\n" {
		t.Errorf("followed %q", out.String())
	}
}

// A follow between the truncate and its rotated file restarts once, not again when the rotated file lands.
func TestTailRestartsOnceForATruncateSeenBeforeItsRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	runtime := appender(t, path)
	writeLog(t, runtime, "first\n")

	var out bytes.Buffer
	tl := openTestTail(t, path)
	followTail(t, tl, &out)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	writeLog(t, runtime, "2\n")
	followTail(t, tl, &out)

	if err := os.WriteFile(logfile.Rotated(path), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLog(t, runtime, "3\n")
	followTail(t, tl, &out)

	if out.String() != "first\n2\n3\n" {
		t.Errorf("followed %q", out.String())
	}
}

// The daemon renames the log it writes, so the follow drains the renamed file, waits out the gap, then reads the next one.
func TestTailFollowsARenameToTheNextFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	before := appender(t, path)
	writeLog(t, before, "first\n")

	var out bytes.Buffer
	tl := openTestTail(t, path)
	followTail(t, tl, &out)

	writeLog(t, before, "second\n")
	if err := os.Rename(path, logfile.Rotated(path)); err != nil {
		t.Fatal(err)
	}
	followTail(t, tl, &out)

	after := appender(t, path)
	writeLog(t, after, "third\n")
	followTail(t, tl, &out)

	writeLog(t, after, "fourth\n")
	followTail(t, tl, &out)

	if out.String() != "first\nsecond\nthird\nfourth\n" {
		t.Errorf("followed %q", out.String())
	}
}
