package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/term"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/client"
)

// fakeSandboxAPI answers the per-sandbox routes the bulk verbs call, over a table it changes as the verbs land.
type fakeSandboxAPI struct {
	mu        sync.Mutex
	sandboxes []client.Sandbox
	warnings  []string
	// calls is each verb the server ran, as "verb id".
	calls []string
	// failDelete answers a delete of these ids with an internal error, after it drops the record where the value is true.
	failDelete map[string]bool
}

// internalText is the text the daemon answers for a failure whose cause it keeps to its log.
const internalText = "the daemon could not complete the request; the daemon log has the cause"

func (f *fakeSandboxAPI) find(ref string) int {
	return slices.IndexFunc(f.sandboxes, func(sb client.Sandbox) bool { return sb.ID == ref || sb.Name == ref })
}

func (f *fakeSandboxAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		answer(w, http.StatusOK, client.ListResult{Sandboxes: f.sandboxes, Warnings: f.warnings})
	})
	mux.HandleFunc("GET /v0/sandboxes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.find(r.PathValue("ref"))
		if i < 0 {
			refuse(w, http.StatusNotFound, models.CodeNotFound, "no sandbox "+r.PathValue("ref"))

			return
		}
		answer(w, http.StatusOK, client.Inspection{Sandbox: f.sandboxes[i]})
	})
	mux.HandleFunc("POST /v0/sandboxes/{ref}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.find(r.PathValue("ref"))
		if i < 0 {
			refuse(w, http.StatusNotFound, models.CodeNotFound, "no sandbox "+r.PathValue("ref"))

			return
		}
		next := map[string]models.State{"stop": models.StateStopped, "pause": models.StatePaused, "resume": models.StateRunning}[r.PathValue("verb")]
		f.sandboxes[i].State = next
		f.calls = append(f.calls, r.PathValue("verb")+" "+f.sandboxes[i].ID)
		answer(w, http.StatusOK, f.sandboxes[i])
	})
	mux.HandleFunc("DELETE /v0/sandboxes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		i := f.find(r.PathValue("ref"))
		if i < 0 {
			refuse(w, http.StatusNotFound, models.CodeNotFound, "no sandbox "+r.PathValue("ref"))

			return
		}
		sb := f.sandboxes[i]
		if sb.State != models.StateStopped && r.URL.Query().Get("force") != "true" {
			refuse(w, http.StatusConflict, models.CodeSandboxLive, "sandbox "+sb.ID+" is "+string(sb.State)+": stop it first with shard stop "+sb.ID+", or pass --force")

			return
		}
		dropped, fails := f.failDelete[sb.ID]
		if fails && !dropped {
			refuse(w, http.StatusInternalServerError, models.CodeInternal, internalText)

			return
		}
		f.sandboxes = slices.Delete(f.sandboxes, i, i+1)
		f.calls = append(f.calls, "remove "+sb.ID)
		if fails {
			refuse(w, http.StatusInternalServerError, models.CodeInternal, internalText)

			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) //nolint:errcheck // the client under test reads what arrived
}

func refuse(w http.ResponseWriter, status int, code models.Code, message string) {
	answer(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// newBulkApp serves the table on the socket under a fresh root, with stdout and stderr kept apart.
func newBulkApp(t *testing.T, sandboxes ...client.Sandbox) (App, *fakeSandboxAPI, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	f := &fakeSandboxAPI{sandboxes: sandboxes}
	root := shortRoot(t)
	listener, err := net.Listen("unix", filepath.Join(root, api.SocketFile))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(f.handler())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer

	return App{Version: "test", Root: root, Out: &stdout, Err: &stderr}, f, &stdout, &stderr
}

func sandboxIn(id, name string, state models.State) client.Sandbox {
	return client.Sandbox{ID: id, Name: name, Image: "alpine:3.20", Provider: "fake", State: state}
}

// main prints Run's error as one more shard: line, so the test reads stderr the way a person would.
func shown(app App, err error, stderr *bytes.Buffer) string {
	if err == nil {
		return stderr.String()
	}

	return stderr.String() + "shard: " + err.Error() + "\n"
}

func TestStopActsOnEverySandboxAndPrintsEachID(t *testing.T) {
	app, f, stdout, stderr := newBulkApp(t, sandboxIn("aaa111", "web", models.StateRunning), sandboxIn("bbb222", "", models.StateRunning))

	if err := app.Run(t.Context(), []string{"stop", "web", "bbb222"}); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if got := stdout.String(); got != "aaa111\nbbb222\n" {
		t.Errorf("stop printed %q, want each id on its own line", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stop wrote %q to stderr, want nothing", stderr.String())
	}
	if want := []string{"stop aaa111", "stop bbb222"}; !slices.Equal(f.calls, want) {
		t.Errorf("the server ran %v, want %v", f.calls, want)
	}
}

// One failure leaves the rest to run, its error prints after them on stderr, and the exit is 1 (Run returns an error).
func TestRemoveGoesOnPastAFailureAndReportsItLast(t *testing.T) {
	app, f, stdout, stderr := newBulkApp(t, sandboxIn("aaa111", "a", models.StateStopped), sandboxIn("bbb222", "b", models.StateStopped))

	err := app.Run(t.Context(), []string{"remove", "a", "missing", "b"})
	if err == nil {
		t.Fatal("remove with a missing sandbox returned no error, want one for the exit code")
	}

	if got := stdout.String(); got != "aaa111\nbbb222\n" {
		t.Errorf("remove printed %q, want the two removed ids", got)
	}
	if got := shown(app, err, stderr); got != "shard: no sandbox missing\n" {
		t.Errorf("stderr read %q, want the one failure", got)
	}
	if want := []string{"remove aaa111", "remove bbb222"}; !slices.Equal(f.calls, want) {
		t.Errorf("the server ran %v, want %v", f.calls, want)
	}
}

// Every failure gets its own line, in the order typed, and none of them hides another.
func TestEveryFailurePrintsOnItsOwnLine(t *testing.T) {
	app, _, stdout, stderr := newBulkApp(t, sandboxIn("aaa111", "a", models.StateRunning), sandboxIn("bbb222", "b", models.StateStopped))

	err := app.Run(t.Context(), []string{"remove", "ghost", "a", "b"})
	if err == nil {
		t.Fatal("remove returned no error, want one for the exit code")
	}

	if got := stdout.String(); got != "bbb222\n" {
		t.Errorf("remove printed %q, want only the stopped one", got)
	}
	want := "shard: no sandbox ghost\nshard: sandbox aaa111 is running: stop it first with shard stop aaa111, or pass --force\n"
	if got := shown(app, err, stderr); got != want {
		t.Errorf("stderr read %q, want %q", got, want)
	}
}

func TestRemoveForceAppliesToEverySandbox(t *testing.T) {
	app, f, stdout, _ := newBulkApp(t, sandboxIn("aaa111", "a", models.StateRunning), sandboxIn("bbb222", "b", models.StatePaused))

	if err := app.Run(t.Context(), []string{"remove", "--force", "a", "b"}); err != nil {
		t.Fatalf("remove --force: %v", err)
	}

	if got := stdout.String(); got != "aaa111\nbbb222\n" {
		t.Errorf("remove printed %q, want both ids", got)
	}
	if len(f.sandboxes) != 0 {
		t.Errorf("the server still holds %v", f.sandboxes)
	}
}

func TestARemoveThatFailsAfterTheRecordWentSucceedsWithAWarning(t *testing.T) {
	app, f, stdout, stderr := newBulkApp(t, sandboxIn("aaa111", "broken", models.StateFailed), sandboxIn("bbb222", "b", models.StateStopped))
	f.failDelete = map[string]bool{"aaa111": true}

	if err := app.Run(t.Context(), []string{"remove", "--force", "broken", "b"}); err != nil {
		t.Fatalf("remove --force: %v; stderr %q", err, stderr.String())
	}

	if got := stdout.String(); got != "aaa111\nbbb222\n" {
		t.Errorf("remove printed %q, want both ids", got)
	}
	if got, want := stderr.String(), "shard: warning: sandbox aaa111 is removed, but "+internalText+"\n"; got != want {
		t.Errorf("stderr read %q, want %q", got, want)
	}
	if len(f.sandboxes) != 0 {
		t.Errorf("the server still holds %v", f.sandboxes)
	}
}

func TestARemoveThatFailsWithTheRecordStillThereFails(t *testing.T) {
	app, f, stdout, stderr := newBulkApp(t, sandboxIn("aaa111", "broken", models.StateFailed), sandboxIn("bbb222", "b", models.StateStopped))
	f.failDelete = map[string]bool{"aaa111": false}

	err := app.Run(t.Context(), []string{"remove", "--force", "broken", "b"})
	if err == nil {
		t.Fatal("remove --force of a sandbox whose record stayed succeeded")
	}

	if got := stdout.String(); got != "bbb222\n" {
		t.Errorf("remove printed %q, want only the removed id", got)
	}
	if got, want := shown(app, err, stderr), "shard: "+internalText+"\n"; got != want {
		t.Errorf("stderr read %q, want %q", got, want)
	}
	if len(f.sandboxes) != 1 || f.sandboxes[0].ID != "aaa111" {
		t.Errorf("the server holds %v, want only aaa111", f.sandboxes)
	}
}

func TestPauseAndResumeTakeSeveralSandboxes(t *testing.T) {
	app, f, stdout, _ := newBulkApp(t, sandboxIn("aaa111", "a", models.StateRunning), sandboxIn("bbb222", "b", models.StateRunning))

	if err := app.Run(t.Context(), []string{"pause", "a", "b"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := app.Run(t.Context(), []string{"resume", "a", "b"}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if got := stdout.String(); got != "aaa111\nbbb222\naaa111\nbbb222\n" {
		t.Errorf("the two verbs printed %q, want each id per verb", got)
	}
	if want := []string{"pause aaa111", "pause bbb222", "resume aaa111", "resume bbb222"}; !slices.Equal(f.calls, want) {
		t.Errorf("the server ran %v, want %v", f.calls, want)
	}
}

// With no daemon no later sandbox can fare better, so the verb stops at the first and prints that one line.
func TestABulkVerbWithNoDaemonFailsOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer

	root := shortRoot(t)
	app := App{Version: "test", Root: root, Out: &stdout, Err: &stderr}

	err := app.Run(t.Context(), []string{"stop", "a", "b", "c"})
	if err == nil || !strings.HasPrefix(err.Error(), "cannot connect to the shard daemon") {
		t.Fatalf("stop with no daemon returned %v, want the connect error", err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stop printed %q and %q, want only the one error Run returns", stdout.String(), stderr.String())
	}
}

func TestListQuietPrintsOnlyTheIDs(t *testing.T) {
	app, _, stdout, stderr := newBulkApp(t,
		sandboxIn("aaa111", "web", models.StateRunning),
		sandboxIn("bbb222", "", models.StatePaused),
		sandboxIn("ccc333", "old", models.StateStopped),
	)

	if err := app.Run(t.Context(), []string{"list", "-q"}); err != nil {
		t.Fatalf("list -q: %v", err)
	}
	if got := stdout.String(); got != "aaa111\nbbb222\n" {
		t.Errorf("list -q printed %q, want the active ids", got)
	}
	// A script asked, so no note about the stopped one it left out.
	if stderr.Len() != 0 {
		t.Errorf("list -q wrote %q to stderr, want nothing", stderr.String())
	}

	stdout.Reset()
	if err := app.Run(t.Context(), []string{"list", "--quiet", "--all"}); err != nil {
		t.Fatalf("list --quiet --all: %v", err)
	}
	if got := stdout.String(); got != "aaa111\nbbb222\nccc333\n" {
		t.Errorf("list --quiet --all printed %q, want every id", got)
	}
}

func TestListQuietOfNothingPrintsNothing(t *testing.T) {
	app, _, stdout, _ := newBulkApp(t)

	if err := app.Run(t.Context(), []string{"list", "-q"}); err != nil {
		t.Fatalf("list -q: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("list -q printed %q, want nothing", stdout.String())
	}
}

func TestListRefusesQuietWithFormat(t *testing.T) {
	app, _, _, _ := newBulkApp(t)

	err := app.Run(t.Context(), []string{"list", "-q", "--format", "json"})
	if err == nil || err.Error() != "list takes --quiet or --format, not both" {
		t.Errorf("list -q --format json returned %v, want the refusal", err)
	}
}

// pruneTable is two stopped sandboxes beside one of each state prune must never take.
func pruneTable() []client.Sandbox {
	return []client.Sandbox{
		sandboxIn("aaa111", "old", models.StateStopped),
		sandboxIn("bbb222", "web", models.StateRunning),
		sandboxIn("ccc333", "", models.StateStopped),
		sandboxIn("ddd444", "held", models.StatePaused),
	}
}

func TestPruneAsksAndRemovesOnlyTheStoppedOnes(t *testing.T) {
	app, f, stdout, _ := newBulkApp(t, pruneTable()...)
	var asked string
	app.confirm = func(_ context.Context, question string, yes bool) (bool, error) {
		if yes {
			t.Error("prune defaulted to yes, want [y/N]")
		}
		asked = question

		return true, nil
	}

	if err := app.Run(t.Context(), []string{"prune"}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if want := "These stopped sandboxes will be removed:\n  aaa111 (old)\n  ccc333\nRemove 2 stopped sandboxes?"; asked != want {
		t.Errorf("prune asked %q, want %q", asked, want)
	}
	if got := stdout.String(); got != "aaa111\nccc333\n" {
		t.Errorf("prune printed %q, want the two stopped ids", got)
	}
	if want := []string{"remove aaa111", "remove ccc333"}; !slices.Equal(f.calls, want) {
		t.Errorf("the server ran %v, want %v", f.calls, want)
	}
}

func TestPruneRemovesNothingOnANo(t *testing.T) {
	app, f, stdout, _ := newBulkApp(t, pruneTable()...)
	app.confirm = func(context.Context, string, bool) (bool, error) { return false, nil }

	err := app.Run(t.Context(), []string{"prune"})
	if err == nil || err.Error() != "prune cancelled; nothing was removed" {
		t.Errorf("prune answered no returned %v, want the cancel", err)
	}
	if stdout.Len() != 0 || len(f.calls) != 0 {
		t.Errorf("prune printed %q and ran %v after a no, want nothing", stdout.String(), f.calls)
	}
}

// With no terminal and no --force there is nobody to say yes, so prune refuses and names the flag.
func TestPruneWithNoTerminalRefusesAndNamesForce(t *testing.T) {
	app, f, _, _ := newBulkApp(t, pruneTable()...)
	app.confirm = func(context.Context, string, bool) (bool, error) { return false, term.ErrNotTerminal }

	err := app.Run(t.Context(), []string{"prune"})
	if err == nil || !strings.Contains(err.Error(), "pass --force") {
		t.Errorf("prune with no terminal returned %v, want a refusal that names --force", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("prune with no terminal ran %v, want nothing", f.calls)
	}
}

// The real terminal check: a buffer for stdout is no terminal, so this needs no hook.
func TestPruneOverAPipeRefuses(t *testing.T) {
	app, f, _, _ := newBulkApp(t, pruneTable()...)

	err := app.Run(t.Context(), []string{"prune"})
	if err == nil || !strings.Contains(err.Error(), "pass --force") {
		t.Errorf("prune over a pipe returned %v, want a refusal that names --force", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("prune over a pipe ran %v, want nothing", f.calls)
	}
}

func TestPruneForceRemovesWithoutAsking(t *testing.T) {
	app, f, stdout, _ := newBulkApp(t, pruneTable()...)
	app.confirm = func(context.Context, string, bool) (bool, error) {
		t.Error("prune --force asked")

		return false, errors.New("asked")
	}

	if err := app.Run(t.Context(), []string{"prune", "--force"}); err != nil {
		t.Fatalf("prune --force: %v", err)
	}
	if got := stdout.String(); got != "aaa111\nccc333\n" {
		t.Errorf("prune --force printed %q, want the two stopped ids", got)
	}
	if len(f.sandboxes) != 2 {
		t.Errorf("the server holds %v, want the running and the paused one", f.sandboxes)
	}
}

func TestPruneOfNothingSaysSoAndSucceeds(t *testing.T) {
	app, _, stdout, stderr := newBulkApp(t, sandboxIn("bbb222", "web", models.StateRunning))

	if err := app.Run(t.Context(), []string{"prune"}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("prune printed %q on stdout, want nothing", stdout.String())
	}
	if got := stderr.String(); got != "shard: no stopped sandboxes to remove\n" {
		t.Errorf("prune wrote %q to stderr, want the one line", got)
	}
}

func TestPruneCountsARemoveThatFailedAfterTheRecordWent(t *testing.T) {
	app, f, stdout, stderr := newBulkApp(t, pruneTable()...)
	f.failDelete = map[string]bool{"aaa111": true}

	if err := app.Run(t.Context(), []string{"prune", "--force"}); err != nil {
		t.Fatalf("prune --force: %v; stderr %q", err, stderr.String())
	}
	if got := stdout.String(); got != "aaa111\nccc333\n" {
		t.Errorf("prune --force printed %q, want the two stopped ids", got)
	}
	if got, want := stderr.String(), "shard: warning: sandbox aaa111 is removed, but "+internalText+"\n"; got != want {
		t.Errorf("stderr read %q, want %q", got, want)
	}
	if len(f.sandboxes) != 2 {
		t.Errorf("the server holds %v, want the running and the paused one", f.sandboxes)
	}
}
