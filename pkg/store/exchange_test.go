package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExchangeSwapsTwoDirectories(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	write(t, a, "first")
	write(t, b, "second")

	if err := Exchange(a, b); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got := read(t, a); got != "second" {
		t.Errorf("a holds %q, want the contents of b", got)
	}
	if got := read(t, b); got != "first" {
		t.Errorf("b holds %q, want the contents of a", got)
	}
}

func TestExchangeSaysSoWhenAPathIsMissing(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	write(t, a, "first")

	if err := Exchange(a, filepath.Join(root, "gone")); err == nil {
		t.Error("Exchange with a missing path = nil, want an error")
	}
}

func TestSwapDirInstallsOverNothing(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, src, "new")

	if err := SwapDir(src, dst); err != nil {
		t.Fatalf("SwapDir: %v", err)
	}
	if got := read(t, dst); got != "new" {
		t.Errorf("dst holds %q, want the contents of src", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src is still there after the install: %v", err)
	}
}

func TestSwapDirDropsWhatTheDestinationHeld(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, src, "new")
	write(t, dst, "old")

	if err := SwapDir(src, dst); err != nil {
		t.Fatalf("SwapDir: %v", err)
	}
	if got := read(t, dst); got != "new" {
		t.Errorf("dst holds %q, want the contents of src", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src still holds what dst gave up: %v", err)
	}
}

func write(t *testing.T, dir, content string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mark"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir string) string {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(dir, "mark"))
	if err != nil {
		t.Fatal(err)
	}

	return string(blob)
}
