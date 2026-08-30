package cli

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
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

func listed() []models.Sandbox {
	now := time.Now()

	return []models.Sandbox{
		{ID: "up-1", Name: "web", Image: "alpine:3.20", State: models.StateRunning, CreatedAt: now.Add(-90 * time.Second), Address: netip.MustParsePrefix("10.44.0.2/24")},
		{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: now.Add(-time.Hour), Address: netip.MustParsePrefix("10.44.0.3/24")},
	}
}

func TestLsShowsWhatIsUp(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.repoSvc.(*fakeLifecycleRepo).left = listed()

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
	for _, want := range []string{"up-1", "web", "alpine:3.20", "running", "1m30s", "10.44.0.2"} {
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

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	deps.repoSvc.(*fakeLifecycleRepo).left = listed()

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

	r := &recorder{}
	app, _ := newLifecycleApp(t, &out, r, running())

	if err := app.Run(t.Context(), []string{"ls"}); err != nil {
		t.Fatalf("ls: %v", err)
	}

	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, "ID") || strings.Contains(got, "\n") {
		t.Errorf("ls printed %q, want the header alone", got)
	}
}

// One unreadable record must not hide the others: the sandbox behind each still holds a process and
// an address, and this command is the only way an operator finds them.
func TestLsPrintsTheReadableOnesAndReportsTheRest(t *testing.T) {
	var out bytes.Buffer

	r := &recorder{}
	app, deps := newLifecycleApp(t, &out, r, running())
	repo := deps.repoSvc.(*fakeLifecycleRepo)
	repo.left = listed()
	repo.unreadable = errors.New("decode sandbox.json of bad-3: unexpected end of JSON input")

	err := app.Run(t.Context(), []string{"ls"})
	if err == nil || !strings.Contains(err.Error(), "bad-3") {
		t.Errorf("ls returned %v, want the unreadable record named", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("ls hid the readable sandbox:\n%s", out.String())
	}
}

func TestUptimeOfAStoppedSandboxIsADash(t *testing.T) {
	sb := models.Sandbox{State: models.StateStopped, CreatedAt: time.Now().Add(-time.Hour)}
	if got := uptime(sb, time.Now()); got != "-" {
		t.Errorf("uptime is %q, want -", got)
	}
}
