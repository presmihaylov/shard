package logfile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestTruncateKeepsTheTailAndTheWriterAppendsAtTheNewEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the writer: %v", err)
	}
	defer writer.Close()

	if _, err := writer.Write(bytes.Repeat([]byte("a"), 60)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte("b"), 40)); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Truncate(path, 40); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	rotated, err := os.ReadFile(Rotated(path))
	if err != nil {
		t.Fatalf("read the rotated file: %v", err)
	}
	if want := bytes.Repeat([]byte("b"), 40); !bytes.Equal(rotated, want) {
		t.Errorf("rotated holds %q, want %q", rotated, want)
	}

	// A writer without O_APPEND would land this at offset 100, behind a 100 byte hole.
	if _, err := writer.Write([]byte("next")); err != nil {
		t.Fatalf("write after the truncate: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if string(got) != "next" {
		t.Errorf("the log holds %q, want %q", got, "next")
	}
}

func TestTruncateLeavesALogAtTheBoundAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 40), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Truncate(path, 40); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 40 {
		t.Errorf("the log holds %d bytes, want 40", info.Size())
	}
	if _, err := os.Stat(Rotated(path)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rotated file exists: %v", err)
	}
}

func TestTruncateReplacesTheRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	for _, fill := range []string{"a", "b"} {
		if err := os.WriteFile(path, bytes.Repeat([]byte(fill), 50), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := Truncate(path, 10); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
	}

	rotated, err := os.ReadFile(Rotated(path))
	if err != nil {
		t.Fatalf("read the rotated file: %v", err)
	}
	if want := bytes.Repeat([]byte("b"), 10); !bytes.Equal(rotated, want) {
		t.Errorf("rotated holds %q, want %q", rotated, want)
	}
}

func TestTruncateOfALogThatIsGoneIsNothing(t *testing.T) {
	if err := Truncate(filepath.Join(t.TempDir(), "output.log"), 10); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
}
