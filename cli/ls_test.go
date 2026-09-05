package cli

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func TestParseLsFlags(t *testing.T) {
	opts, err := parseLs([]string{"--all"})
	if err != nil {
		t.Fatalf("parseLs: %v", err)
	}
	if !opts.all {
		t.Error("--all was not read")
	}

	for name, args := range map[string][]string{
		"an argument":     {"sandbox1"},
		"an unknown flag": {"--quiet"},
	} {
		if _, err := parseLs(args); err == nil {
			t.Errorf("parseLs(%s) returned no error", name)
		}
	}
}

// newLsApp puts a daemon over a repository that answers List with left, and with unreadable beside it.
func newLsApp(t *testing.T, out *bytes.Buffer, left []models.Sandbox, unreadable error) App {
	t.Helper()

	app, deps := newLifecycleApp(t, out, &recorder{}, models.Sandbox{})
	repo := deps.repoSvc.(*fakeLifecycleRepo)
	repo.left, repo.unreadable = left, unreadable
	serveDaemon(t, deps)

	return app
}

func listed() []models.Sandbox {
	return []models.Sandbox{
		{ID: "up-1", Name: "web", Image: "alpine:3.20", State: models.StateRunning, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.2/24")},
		{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.3/24")},
	}
}

func TestLsShowsWhatIsUp(t *testing.T) {
	var out bytes.Buffer

	app := newLsApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ls printed %d lines, want the header and the one sandbox that is up:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "UPTIME") || !strings.Contains(lines[0], "IP") {
		t.Errorf("the header is %q", lines[0])
	}
	for _, want := range []string{"up-1", "web", "alpine:3.20", "running", "10.44.0.2"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("the line %q lacks %q", lines[1], want)
		}
	}
	if strings.Contains(lines[1], "/24") {
		t.Errorf("the line %q shows the prefix, want the bare address", lines[1])
	}
}

func TestLsAllShowsTheStoppedOnesToo(t *testing.T) {
	var out bytes.Buffer

	app := newLsApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"ls", "--all"}); err != nil {
		t.Fatalf("ls --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("ls --all printed %d lines, want the header and both sandboxes:\n%s", len(lines), out.String())
	}
	for _, want := range []string{"down-2", "stopped"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("the line %q lacks %q", lines[2], want)
		}
	}
}

func TestLsOnAnEmptyRootPrintsTheHeader(t *testing.T) {
	var out bytes.Buffer

	app := newLsApp(t, &out, nil, nil)

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls: %v", err)
	}

	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, "ID") || strings.Contains(got, "\n") {
		t.Errorf("ls printed %q, want the header alone", got)
	}
}

// One unreadable record must not hide the others: their sandboxes still hold a process and an address.
func TestLsPrintsTheReadableOnesAndReportsTheRest(t *testing.T) {
	var out bytes.Buffer

	unreadable := &sandboxstate.UnreadableError{ID: "bad-3", Err: errors.New("decode sandbox.json of bad-3: unexpected end of JSON input")}
	app := newLsApp(t, &out, listed(), unreadable)

	err := app.Run(t.Context(), []string{"ls"})
	if err == nil || !strings.Contains(err.Error(), "bad-3") {
		t.Errorf("ls returned %v, want the unreadable record named", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls hid the readable sandbox:\n%s", out.String())
	}
}

func TestUptime(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	created := now.Add(-90*time.Second - 300*time.Millisecond)

	cases := map[string]struct {
		state models.State
		want  string
	}{
		"a running sandbox counts from its creation":     {models.StateRunning, "1m30s"},
		"a stopped sandbox is not up":                    {models.StateStopped, "-"},
		"a paused sandbox holds no memory and is not up": {models.StatePaused, "-"},
	}

	for name, c := range cases {
		sb := models.Sandbox{State: c.state, CreatedAt: created}
		if got := uptime(sb, now); got != c.want {
			t.Errorf("%s: uptime is %q, want %q", name, got, c.want)
		}
	}
}
