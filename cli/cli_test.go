package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/image"
)

func newApp(t *testing.T, out *bytes.Buffer) App {
	t.Helper()

	return App{Version: "test", Root: t.TempDir(), Out: out}
}

// newStoreApp puts a daemon on the real image store up, for the verbs that only read one.
func newStoreApp(t *testing.T, out *bytes.Buffer) (App, string) {
	t.Helper()

	root := shortRoot(t)

	images, err := image.New(filepath.Join(root, "images"))
	if err != nil {
		t.Fatalf("image.New: %v", err)
	}

	serveDaemon(t, &fakeDaemon{app: App{Root: root}, imageSvc: images})

	return App{Version: "test", Root: root, Out: out, Err: out, Timeout: time.Minute}, root
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

	app, _ := newStoreApp(t, &out)

	if err := app.Run(t.Context(), []string{"image", "ls"}); err != nil {
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
		{"fork"},
		{"fork", "one", "two"},
		{"clone"},
		{"clone", "one", "two"},
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

	_, root := newStoreApp(t, &out)

	// The daemon answers on the socket under that root, and the default root has none.
	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", root, "image", "ls"}); err != nil {
		t.Fatalf("image ls: %v", err)
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

	app, _ := newStoreApp(t, &out)

	if err := app.Run(t.Context(), []string{"--timeout", "5s", "image", "ls"}); err != nil {
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
