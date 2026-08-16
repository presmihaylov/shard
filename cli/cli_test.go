package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newApp(t *testing.T, out *bytes.Buffer) App {
	t.Helper()

	return App{Version: "test", Root: t.TempDir(), Out: out}
}

func TestRunVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var out bytes.Buffer

		if err := newApp(t, &out).Run(t.Context(), []string{arg}); err != nil {
			t.Fatalf("Run(%q): %v", arg, err)
		}

		if got := strings.TrimSpace(out.String()); got != "test" {
			t.Errorf("Run(%q) printed %q, want the version", arg, got)
		}
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), nil); err != nil {
		t.Fatalf("Run(nil): %v", err)
	}

	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("Run(nil) printed %q, want the usage", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"launch"}); err == nil {
		t.Fatal("an unknown command returned no error")
	}
}

func TestImageLsOnAnEmptyRoot(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"image", "ls"}); err != nil {
		t.Fatalf("image ls: %v", err)
	}

	if !strings.Contains(out.String(), "REFERENCE") {
		t.Errorf("image ls printed %q, want the header", out.String())
	}
}

func TestCommandsThatNeedAnArgument(t *testing.T) {
	commands := [][]string{
		{"pull"},
		{"pull", "one", "two"},
		{"image"},
		{"image", "rm"},
		{"image", "grow"},
		{"image", "ls", "extra"},
	}

	for _, args := range commands {
		var out bytes.Buffer

		if err := newApp(t, &out).Run(t.Context(), args); err == nil {
			t.Errorf("Run(%v) returned no error", args)
		}
	}
}

func TestRootFlagPrecedesTheCommand(t *testing.T) {
	var out bytes.Buffer
	root := t.TempDir()

	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", root, "image", "ls"}); err != nil {
		t.Fatalf("image ls: %v", err)
	}

	// The default root is /var/lib/shard, so a layout under the temporary one proves the flag landed.
	if _, err := os.Stat(filepath.Join(root, "images", "layout", "index.json")); err != nil {
		t.Errorf("the root flag was ignored: %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 7_700_000: "7.7 MB"}

	for bytes, want := range cases {
		if got := humanSize(bytes); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}
