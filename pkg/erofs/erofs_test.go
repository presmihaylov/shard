package erofs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTool puts a mkfs.erofs stand-in first on PATH that runs script with the argv it was given.
func fakeTool(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Tool), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBuildRunsTheToolOverTheTree(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_LOG", log)
	fakeTool(t, `printf '%s\n' "$@" > "$FAKE_LOG" && printf EROFS > "$3"`)
	dst := filepath.Join(t.TempDir(), "image.erofs")
	src := t.TempDir()

	if err := Build(t.Context(), dst, src); err != nil {
		t.Fatalf("Build: %v", err)
	}

	argv, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"-b", "4096", dst, src}, "\n") + "\n"
	if string(argv) != want {
		t.Errorf("argv:\n%s\nwant:\n%s", argv, want)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "EROFS" {
		t.Errorf("the image holds %q", body)
	}
}

func TestBuildCarriesTheToolsOutputInItsError(t *testing.T) {
	fakeTool(t, `echo 'cannot read the tree' >&2; exit 1`)

	err := Build(t.Context(), filepath.Join(t.TempDir(), "image.erofs"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot read the tree") || !strings.Contains(err.Error(), Tool) {
		t.Fatalf("Build: %v", err)
	}
}

func TestBuildNamesTheMissingTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := Build(t.Context(), filepath.Join(t.TempDir(), "image.erofs"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), Tool) {
		t.Fatalf("Build: %v", err)
	}
}
