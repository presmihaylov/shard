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

// A link under the root that leads out of it is refused, and the file it leads to is left alone.
func TestWriteFileInRefusesALinkOutOfTheRoot(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := WriteFileIn(root, "etc/hosts", []byte("x"), 0o644); err == nil {
		t.Error("WriteFileIn followed a link out of the root")
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries outside the root, want none", len(entries))
	}
}

// The last element is replaced, never written through, so a link there cannot carry the data out either.
func TestWriteFileInReplacesALinkAtTheTarget(t *testing.T) {
	dir, outside := t.TempDir(), filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(outside, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "hosts")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := WriteFileIn(root, "hosts", []byte("guest"), 0o644); err != nil {
		t.Fatalf("WriteFileIn: %v", err)
	}

	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "host" {
		t.Errorf("the file behind the link is %q, want it untouched", got)
	}
	if info, err := os.Lstat(filepath.Join(dir, "hosts")); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the target is %v (%v), want a regular file in place of the link", info, err)
	}
}

func TestWriteFileInLeavesNoTempBehindWhenItFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "record.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := WriteFileIn(root, "record.json", []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFileIn over a directory returned no error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want only the directory: the temp file stayed behind", len(entries))
	}
}

// readOnly makes dir refuse a new file, the way a full disk refuses one, and gives it back for the cleanup.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("Chmod back: %v", err)
		}
	})
}

func TestWriteFileIfChangedWritesNothingWhenTheFileMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "initrd.cpio")
	if err := WriteFile(path, []byte("same"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	readOnly(t, dir)

	if err := WriteFileIfChanged(path, []byte("same"), 0o600); err != nil {
		t.Fatalf("WriteFileIfChanged of the bytes already there = %v, want no write at all", err)
	}
	if err := WriteFileIfChanged(path, []byte("else"), 0o600); err == nil {
		t.Fatal("WriteFileIfChanged of other bytes wrote into a directory that refuses a new file")
	}
}

func TestWriteFileIfChangedReplacesOtherBytesOrMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restarts.json")
	if err := WriteFileIfChanged(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("WriteFileIfChanged of an absent file: %v", err)
	}
	if err := WriteFileIfChanged(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("WriteFileIfChanged of other bytes: %v", err)
	}
	if err := WriteFileIfChanged(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFileIfChanged of another mode: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if string(got) != "second" || info.Mode().Perm() != 0o644 {
		t.Errorf("got %q at %v, want %q at 0644", got, info.Mode().Perm(), "second")
	}
}
