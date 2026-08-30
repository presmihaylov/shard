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
		{"inspect"},
		{"start"},
		{"start", "one", "two"},
		{"pause"},
		{"pause", "one", "two"},
		{"resume"},
		{"resume", "one", "two"},
		{"image"},
		{"image", "rm"},
		{"image", "prune", "extra"},
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

func TestRootMustBeAbsolute(t *testing.T) {
	// An empty or relative root would put the whole state tree under whatever directory shard ran in.
	for _, root := range []string{"", "images", "./images"} {
		var out bytes.Buffer

		err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", root, "image", "ls"})
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("--root %q: got %v, want a rejected relative root", root, err)
		}
	}
}

func TestTimeoutFlagIsParsed(t *testing.T) {
	var out bytes.Buffer
	app := App{Version: "test", Out: &out}

	if err := app.Run(t.Context(), []string{"--timeout", "5s", "--root", t.TempDir(), "image", "ls"}); err != nil {
		t.Fatalf("image ls: %v", err)
	}
}

func TestBadTimeoutIsRejected(t *testing.T) {
	var out bytes.Buffer

	err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--timeout", "never", "image", "ls"})
	if err == nil {
		t.Fatal("a bad --timeout returned no error")
	}
}

// TestImageLsBuildsOnlyTheImageService is the reason every layer is built on the first ask: the
// provider needs runsc on the host and the network service needs root, and image ls needs neither.
func TestImageLsBuildsOnlyTheImageService(t *testing.T) {
	var out bytes.Buffer

	d := &deps{}
	app := App{Version: "test", Root: t.TempDir(), Out: &out}
	app.newDeps = func(a App) *deps {
		d.app = a

		return d
	}

	if err := app.Run(t.Context(), []string{"image", "ls"}); err != nil {
		t.Fatalf("image ls: %v", err)
	}

	if d.imageSvc == nil {
		t.Error("image ls built no image service")
	}

	for name, built := range map[string]bool{"provider": d.providerSvc != nil, "network service": d.netSvc != nil, "repository": d.repoSvc != nil} {
		if built {
			t.Errorf("image ls built the %s, which needs a Linux host and root", name)
		}
	}
}
