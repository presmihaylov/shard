package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

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

	return App{Version: "test", Root: root, Out: out, Err: out}, root
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), nil); err != nil {
		t.Fatalf("Run(nil): %v", err)
	}

	if !strings.HasPrefix(out.String(), "Usage: shard ") {
		t.Errorf("Run(nil) printed %q, want the usage", out.String())
	}
}

// The daemon's help names every provider, and points at info for what a host picks.
func TestDaemonHelpNamesEveryProviderAndTheHostDefault(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"daemon", "--help"}); err != nil {
		t.Fatalf("daemon --help: %v", err)
	}
	// The help wraps at 80 columns, so the words are read back as one line.
	got := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(got, "gvisor, sysbox, runc, vz or firecracker") || !strings.Contains(got, "Use 'shard info' to see the default provider for this host.") {
		t.Errorf("daemon --help printed %q, want every provider and the host default", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"launch"}); err == nil {
		t.Fatal("an unknown command returned no error")
	}
}

func TestImageListOnAnEmptyRoot(t *testing.T) {
	var out bytes.Buffer

	app, _ := newStoreApp(t, &out)

	if err := app.Run(t.Context(), []string{"image", "list"}); err != nil {
		t.Fatalf("image list: %v", err)
	}

	if !strings.Contains(out.String(), "REFERENCE") {
		t.Errorf("image list printed %q, want the header", out.String())
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
		{"snapshot"},
		{"snapshot", "create"},
		{"snapshot", "create", "one", "two"},
		{"snapshot", "list", "extra"},
		{"snapshot", "inspect"},
		{"snapshot", "remove", "one", "two"},
		{"cp"},
		{"cp", "one", "two"},
		{"image"},
		{"image", "remove"},
		{"image", "prune", "extra"},
		{"image", "grow"},
		{"image", "list", "extra"},
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
	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", root, "image", "list"}); err != nil {
		t.Fatalf("image list: %v", err)
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

		err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", root, "image", "list"})
		if err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("--root %q: got %v, want a rejected relative root", root, err)
		}
	}
}

// The three daemon flags once came before the verb; there they are refused, by name, with the command that takes them.
func TestDaemonFlagsBeforeTheVerbNameWhereTheyGo(t *testing.T) {
	cases := map[string][]string{
		"--timeout is a shard daemon flag: shard daemon --timeout 5m":                            {"--timeout", "5m", "image", "list"},
		"--insecure-registry is a shard daemon flag: shard daemon --insecure-registry r.example": {"--insecure-registry", "r.example", "list"},
		"--provider is a shard daemon flag: shard daemon --provider gvisor":                      {"--provider", "gvisor", "daemon"},
	}

	for want, args := range cases {
		var out bytes.Buffer

		err := newApp(t, &out).Run(t.Context(), args)
		if err == nil || err.Error() != want {
			t.Errorf("%v returned %v, want %q", args, err, want)
		}
	}
}

func TestBadTimeoutIsRejected(t *testing.T) {
	var out bytes.Buffer

	err := newApp(t, &out).Run(t.Context(), []string{"daemon", "--timeout", "5"})
	if want := `invalid value "5" for --timeout: want a duration such as 10s`; err == nil || err.Error() != want {
		t.Errorf("daemon --timeout 5 returned %v, want %q", err, want)
	}
}

// A flag error names the flag the way the help does, and says the unit, with nothing of Go's flag package in it.
func TestFlagErrorsReadAsTheHelpSpellsThem(t *testing.T) {
	cases := map[string][]string{
		`invalid value "512m" for --memory: unknown unit "m"; want KiB, MiB, GiB, KB, MB or GB`:             {"create", "--memory", "512m", "alpine"},
		`invalid value "1.5GiB" for --disk: want a whole number; a fraction is never rounded`:               {"create", "--disk", "1.5GiB", "alpine"},
		`invalid value "512" for --memory: want a unit, such as 512MiB or 2GiB`:                             {"create", "--memory", "512", "alpine"},
		`--memory needs a value: a whole size with a unit, such as 512MiB or 2GiB; only 0 goes without one`: {"create", "--memory"},
		`invalid value "5" for --restart-backoff: want a duration such as 10s`:                              {"run", "--restart-backoff", "5", "alpine", "true"},
		`invalid value "x" for --restart-retries: want a whole number`:                                      {"run", "--restart-retries", "x", "alpine", "true"},
		`invalid value "maybe" for --all: want true or false`:                                               {"list", "--all=maybe"},
		`--restart-backoff needs a value: a duration such as 10s`:                                           {"run", "--restart-backoff"},
		`unknown flag --bogus; run shard create --help`:                                                     {"create", "--bogus", "alpine"},
		`unknown flag -x; run shard pause --help`:                                                           {"pause", "-x"},
		`unknown flag --bogus; run shard --help`:                                                            {"--bogus", "list"},
	}

	for want, args := range cases {
		var out bytes.Buffer

		err := newApp(t, &out).Run(t.Context(), args)
		if err == nil || err.Error() != want {
			t.Errorf("%v returned %v, want %q", args, err, want)
		}
	}
}

func TestVerbHelpPrintsItsFlagsAndExitsZero(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"create", "--help"}); err != nil {
		t.Fatalf("create --help: %v", err)
	}

	got := out.String()
	if !strings.HasPrefix(got, "Usage: shard create ") || !strings.Contains(got, "--memory <size>") {
		t.Errorf("create --help printed %q, want the create usage and its flags", got)
	}
}

func TestVersionFlagAfterGlobalsNeverFails(t *testing.T) {
	var out bytes.Buffer

	// A relative --root fails every other verb, but --version answers before that check.
	if err := (App{Version: "test", Out: &out}).Run(t.Context(), []string{"--root", "rel", "--version"}); err != nil {
		t.Fatalf("--root rel --version: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "client test" {
		t.Errorf("--version printed %q, want the client line alone", got)
	}
}

func TestUsageListsVerbHelp(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), nil); err != nil {
		t.Fatalf("Run(nil): %v", err)
	}
	if !strings.Contains(out.String(), "Run 'shard COMMAND --help' for options and examples.") {
		t.Errorf("the top-level usage does not mention shard COMMAND --help:\n%s", out.String())
	}
}

func TestTokensMintHelpStatesItsDefaultsAndFlags(t *testing.T) {
	var out bytes.Buffer

	if err := newApp(t, &out).Run(t.Context(), []string{"tokens", "mint", "--help"}); err != nil {
		t.Fatalf("tokens mint --help: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "(default 24h)") {
		t.Errorf("the tokens mint usage still claims a 24h default:\n%s", got)
	}
	for _, want := range []string{"--scopes <list>", "--tokens-file <path>", "default no expiry"} {
		if !strings.Contains(got, want) {
			t.Errorf("the tokens mint usage omits %q:\n%s", want, got)
		}
	}
}
