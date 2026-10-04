package cli

import (
	"bytes"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

func TestParseListFlags(t *testing.T) {
	opts, err := parseList([]string{"--all"})
	if err != nil {
		t.Fatalf("parseList: %v", err)
	}
	if !opts.all {
		t.Error("--all was not read")
	}

	for name, args := range map[string][]string{
		"an argument":     {"sandbox1"},
		"an unknown flag": {"--quiet"},
	} {
		if _, err := parseList(args); err == nil {
			t.Errorf("parseList(%s) returned no error", name)
		}
	}
}

// newListApp puts a daemon over a repository that answers List with left, and with unreadable beside it.
func newListApp(t *testing.T, out *bytes.Buffer, left []models.Sandbox, unreadable error) App {
	t.Helper()

	app, deps := newLifecycleApp(t, out, &recorder{}, models.Sandbox{})
	repo := deps.repoSvc.(*fakeLifecycleRepo)
	repo.left, repo.unreadable = left, unreadable

	return app
}

func listed() []models.Sandbox {
	return []models.Sandbox{
		{ID: "up-1", Name: "web", Image: "alpine:3.20", State: models.StateRunning, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.2/24")},
		{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.3/24")},
	}
}

func TestListShowsWhatIsUp(t *testing.T) {
	var out bytes.Buffer

	app := newListApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("list printed %d lines, want the header and the one sandbox that is up:\n%s", len(lines), out.String())
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

func TestListAllShowsTheStoppedOnesToo(t *testing.T) {
	var out bytes.Buffer

	app := newListApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("list --all printed %d lines, want the header and both sandboxes:\n%s", len(lines), out.String())
	}
	for _, want := range []string{"down-2", "stopped"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("the line %q lacks %q", lines[2], want)
		}
	}
}

// ls is an alias, so it runs list itself and prints the same table.
func TestLsPrintsWhatListPrints(t *testing.T) {
	var list, ls bytes.Buffer
	if err := newListApp(t, &list, listed(), nil).Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}
	if err := newListApp(t, &ls, listed(), nil).Run(t.Context(), []string{"ls", "--all"}); err != nil {
		t.Fatalf("ls --all: %v", err)
	}

	if ls.String() != list.String() {
		t.Errorf("ls --all printed\n%s\nand list --all printed\n%s", ls.String(), list.String())
	}
}

func TestListOnAnEmptyRootPrintsTheHeader(t *testing.T) {
	var out bytes.Buffer

	app := newListApp(t, &out, nil, nil)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if got := strings.TrimSpace(out.String()); !strings.HasPrefix(got, "ID") || strings.Contains(got, "\n") {
		t.Errorf("list printed %q, want the header alone", got)
	}
}

// One unreadable record must not hide the others: their sandboxes still hold a process and an address.
func TestListPrintsTheReadableOnesAndReportsTheRest(t *testing.T) {
	var out bytes.Buffer

	unreadable := &sandboxstate.UnreadableError{ID: "bad-3", Err: errors.New("decode sandbox.json of bad-3: unexpected end of JSON input")}
	app := newListApp(t, &out, listed(), unreadable)

	err := app.Run(t.Context(), []string{"list"})
	if err == nil || !strings.Contains(err.Error(), "bad-3") {
		t.Errorf("list returned %v, want the unreadable record named", err)
	}
	if !strings.Contains(out.String(), "up-1") {
		t.Errorf("list hid the readable sandbox:\n%s", out.String())
	}
}

func TestUptime(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	started := now.Add(-90*time.Second - 300*time.Millisecond)

	cases := map[string]struct {
		state   models.State
		started time.Time
		want    string
	}{
		"counts from the last start":         {models.StateRunning, started, "1m30s"},
		"an old record counts from creation": {models.StateRunning, time.Time{}, "1h0m0s"},
		"a stopped sandbox is not up":        {models.StateStopped, started, "-"},
		"a paused sandbox is not up":         {models.StatePaused, started, "-"},
		"a pending sandbox has not started":  {models.StatePending, time.Time{}, "-"},
		"a created sandbox has not started":  {models.StateCreated, time.Time{}, "-"},
		"a failed sandbox is not up":         {models.StateFailed, time.Time{}, "-"},
	}

	for name, c := range cases {
		sb := models.Sandbox{State: c.state, CreatedAt: created, StartedAt: c.started}
		if got := uptime(sb, now); got != c.want {
			t.Errorf("%s: uptime is %q, want %q", name, got, c.want)
		}
	}
}

func TestListGivesTheReasonASandboxNobodyStoppedIsStopped(t *testing.T) {
	var out bytes.Buffer

	left := []models.Sandbox{{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped,
		StoppedReason: "daemon restarted and found no process", CreatedAt: time.Now()}}

	app := newListApp(t, &out, left, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	if !strings.Contains(out.String(), "stopped (daemon restarted and found no process)") {
		t.Errorf("list printed %q, want the state and the reason beside it", out.String())
	}
}

func TestListGivesTheReasonASandboxIsUnresponsive(t *testing.T) {
	var out bytes.Buffer

	sandboxes := []models.Sandbox{{ID: "silent-1", Image: "alpine:3.20", State: models.StateUnresponsive,
		UnresponsiveReason: "its shim (pid 42) did not answer within 5s", CreatedAt: time.Now()}}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if !strings.Contains(out.String(), "unresponsive (its shim (pid 42) did not answer within 5s)") {
		t.Errorf("list printed %q, want the state and the reason beside it", out.String())
	}
}

func TestListShowsTheEntrypointExitOfAStillRunningSandbox(t *testing.T) {
	var out bytes.Buffer

	sandboxes := []models.Sandbox{{ID: "up-7", Image: "alpine:3.20", State: models.StateRunning,
		ExitStatus: &models.ExitStatus{Code: 7}, CreatedAt: time.Now()}}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if !strings.Contains(out.String(), "running (exited 7)") {
		t.Errorf("list printed %q, want the running state and the entrypoint exit beside it", out.String())
	}
}

func TestListPrintsTheRestartPolicyAndWhatItSpent(t *testing.T) {
	var out bytes.Buffer

	sandboxes := listed()
	sandboxes[0].Restart = &models.Restart{
		RestartSpec:  models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1},
		RestartCount: models.RestartCount{Count: 2},
	}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[0], "RESTART") {
		t.Errorf("the header is %q, want a RESTART column", lines[0])
	}
	if !strings.Contains(lines[1], "on-failure 2/5") {
		t.Errorf("the line %q does not show the policy and the starts it spent", lines[1])
	}
	if strings.Contains(lines[2], "on-failure") {
		t.Errorf("the line %q shows a policy the sandbox never asked for", lines[2])
	}
}

// An unlimited policy shows the count with no limit beside it.
func TestListPrintsAnUnlimitedRestartWithoutALimit(t *testing.T) {
	var out bytes.Buffer

	sandboxes := listed()
	sandboxes[0].Restart = &models.Restart{
		RestartSpec:  models.RestartSpec{Policy: models.RestartAlways, Backoff: 1},
		RestartCount: models.RestartCount{Count: 3},
	}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[1], "always 3") || strings.Contains(lines[1], "always 3/") {
		t.Errorf("the line %q does not show the count with no limit", lines[1])
	}
}

func TestListPrintsTheRestartPolicyOfTheSupervisor(t *testing.T) {
	var out bytes.Buffer

	sandboxes := listed()
	sandboxes[0].Restart = &models.Restart{
		RestartSpec:  models.RestartSpec{Policy: models.RestartOnFailure, Retries: 5, Backoff: 1},
		RestartCount: models.RestartCount{Count: 5, GaveUp: true},
	}
	sandboxes[1].Restart = &models.Restart{RestartSpec: models.RestartSpec{Policy: models.RestartAlways, Retries: 5, Backoff: 1}}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[1], "on-failure 5/5 gave up") || strings.Contains(lines[1], "on-oom") {
		t.Errorf("the line %q does not show the policy, the starts spent and the give-up alone", lines[1])
	}
	if !strings.Contains(lines[2], "always") || strings.Contains(lines[2], "always 0") {
		t.Errorf("the line %q does not show a policy that has not started again yet as the policy alone", lines[2])
	}
}

// The daemon runs no probe of its own, so the table has no column for one (SHARD-455).
func TestListPrintsNoHealthColumn(t *testing.T) {
	var out bytes.Buffer
	app := newListApp(t, &out, listed(), nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	header := strings.Fields(strings.SplitN(out.String(), "\n", 2)[0])
	want := []string{"ID", "NAME", "IMAGE", "STATE", "UPTIME", "IP", "RESTART", "POLICY"}
	if !slices.Equal(header, want) {
		t.Errorf("the header is %q, want %q", header, want)
	}
}

func TestListPrintsThePolicyEachSandboxHolds(t *testing.T) {
	var out bytes.Buffer

	fronted := listed()
	fronted[0].Policy = "web"

	app := newListApp(t, &out, fronted, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[0], "POLICY") {
		t.Errorf("the header is %q, want a POLICY column", lines[0])
	}
	if !strings.HasSuffix(lines[1], "web") {
		t.Errorf("the line %q does not end in the policy it holds", lines[1])
	}
	if !strings.HasSuffix(lines[2], "-") {
		t.Errorf("the line %q does not end in a dash for the sandbox that holds no policy", lines[2])
	}
}
