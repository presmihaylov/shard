package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
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
		"an unknown flag": {"--recursive"},
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
		{ID: "up-1", Name: "web", Image: "alpine:3.20", State: models.StateRunning, CreatedAt: time.Now()},
		{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: time.Now()},
	}
}

func TestListShowsWhatIsUp(t *testing.T) {
	var out, stderr bytes.Buffer

	app := newListApp(t, &out, listed(), nil)
	app.Err = &stderr

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("list printed %d lines, want the header and the one sandbox that is up:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "UPTIME") {
		t.Errorf("the header is %q", lines[0])
	}
	for _, want := range []string{"up-1", "web", "alpine:3.20", "running"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("the line %q lacks %q", lines[1], want)
		}
	}
	if want := "1 stopped sandbox; shard list --all\n"; stderr.String() != want {
		t.Errorf("list said %q on stderr, want %q", stderr.String(), want)
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

// The daemon starts this one again on its own, so it is up as far as an operator is concerned.
func TestListShowsAStoppedSandboxTheDaemonStartsAgain(t *testing.T) {
	var out, stderr bytes.Buffer

	owed := models.Sandbox{ID: "oom-3", Image: "alpine:3.20", State: models.StateStopped, StoppedReason: sandbox.OOMKilledReason, CreatedAt: time.Now(),
		OOM: &models.OOM{Kills: 1, InARow: 1, KilledAt: time.Now(), RestartAt: time.Now().Add(time.Minute)}}
	app := newListApp(t, &out, append(listed(), owed), nil)
	app.Err = &stderr

	if err := app.Run(t.Context(), []string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}

	if !strings.Contains(out.String(), "oom-3") || strings.Contains(out.String(), "down-2") {
		t.Errorf("list printed\n%s\nwant oom-3 and not down-2", out.String())
	}
	if want := "1 stopped sandbox; shard list --all\n"; stderr.String() != want {
		t.Errorf("list said %q on stderr, want %q", stderr.String(), want)
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
		"an old record counts from creation": {models.StateRunning, time.Time{}, "1h"},
		"a stopped sandbox is not up":        {models.StateStopped, started, "-"},
		"a paused sandbox is not up":         {models.StatePaused, started, "-"},
		"a pending sandbox has not started":  {models.StatePending, time.Time{}, "-"},
		"a created sandbox has not started":  {models.StateCreated, time.Time{}, "-"},
		"a failed sandbox is not up":         {models.StateFailed, time.Time{}, "-"},
	}

	for name, c := range cases {
		sb := client.Sandbox{State: c.state, CreatedAt: created, StartedAt: c.started}
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

// The kill count outlives the stop it caused, so ls still says the memory ran out once the daemon started it again (SHARD-786).
func TestListStateCountsTheMemoryKillsAndTheStartAgain(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		sb   client.Sandbox
		want string
	}{
		"a start again owed in a while": {client.Sandbox{State: models.StateStopped, StoppedReason: sandbox.OOMKilledReason,
			OOM: &client.OOM{Kills: 3, KilledAt: now, RestartAt: now.Add(40 * time.Second)}}, "stopped (ran out of memory 3 times, starts again in 40s)"},
		"a start again due": {client.Sandbox{State: models.StateStopped, StoppedReason: sandbox.OOMKilledReason,
			OOM: &client.OOM{Kills: 1, KilledAt: now, RestartAt: now}}, "stopped (ran out of memory once, starts again now)"},
		"a stop called it off": {client.Sandbox{State: models.StateStopped, StoppedReason: sandbox.OOMKilledReason,
			OOM: &client.OOM{Kills: 2, KilledAt: now}}, "stopped (ran out of memory 2 times)"},
		"running again": {client.Sandbox{State: models.StateRunning, OOM: &client.OOM{Kills: 1, KilledAt: now}},
			"running (ran out of memory once)"},
		"died after an earlier kill": {client.Sandbox{State: models.StateStopped, StoppedReason: sandbox.DiedReason,
			OOM: &client.OOM{Kills: 1, KilledAt: now}}, "stopped (the sandbox process died; ran out of memory once)"},
	}

	for name, c := range cases {
		if got := state(c.sb, now); got != c.want {
			t.Errorf("%s: the state reads %q, want %q", name, got, c.want)
		}
	}
}

// PROCESSES counts the ones still up against all the sandbox holds, so an ended process shows without a ps.
func TestListPrintsHowManyProcessesAreUp(t *testing.T) {
	var out bytes.Buffer

	sandboxes := listed()
	sandboxes[0].Processes = []models.Process{
		{Name: "api", Status: models.ProcessStatus{State: models.ProcessRunning}},
		{Name: "worker", Status: models.ProcessStatus{State: models.ProcessRestarting}},
		{Name: "migrate", Status: models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{}}},
	}

	app := newListApp(t, &out, sandboxes, nil)

	if err := app.Run(t.Context(), []string{"list", "--all"}); err != nil {
		t.Fatalf("list --all: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if got := strings.Fields(lines[1]); !slices.Contains(got, "2/3") {
		t.Errorf("the line %q does not count two of three processes up", lines[1])
	}
	if got := strings.Fields(lines[2]); !slices.Contains(got, "-") || slices.Contains(got, "0/0") {
		t.Errorf("the line %q shows processes for a sandbox that has none", lines[2])
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
	want := []string{"ID", "NAME", "IMAGE", "STATE", "UPTIME", "PROCESSES", "POLICY"}
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
