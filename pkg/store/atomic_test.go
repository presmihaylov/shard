package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")

	if err := WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFile over an existing file: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(got) != "second" {
		t.Errorf("got %q, want %q", got, "second")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if info.Mode().Perm() != 0o644 {
		t.Errorf("got mode %v, want 0644", info.Mode().Perm())
	}
}

func TestWriteFileLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()

	if err := WriteFile(filepath.Join(dir, "record.json"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 1 || entries[0].Name() != "record.json" {
		t.Errorf("got %d entries, want only record.json", len(entries))
	}
}

// The success path renames the temp file away by itself, so only a failed write proves the cleanup.
func TestWriteFileLeavesNoTempBehindWhenItFails(t *testing.T) {
	dir := t.TempDir()

	// A directory at the target makes the rename fail, and nothing else on the way there does.
	target := filepath.Join(dir, "record.json")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := WriteFile(target, []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile over a directory returned no error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("got %d entries, want only the directory: the temp file stayed behind", len(entries))
	}
}

func TestWriteFileFailsOnMissingDirectory(t *testing.T) {
	if err := WriteFile(filepath.Join(t.TempDir(), "absent", "record.json"), []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile into a missing directory returned no error")
	}
}
