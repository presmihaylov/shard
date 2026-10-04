package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// fakeLifecycle answers every verb with err, or with a record named after the reference it was given.
type fakeLifecycle struct {
	err error

	created sandbox.CreateRequest
	// createdID is the id Create answers, so a ?wait re-read can point at a record the test seeded.
	createdID string
	// repo is where a waited create reads the record it settled into.
	repo *sandboxstate.Repository
	// hold is how long Create takes, and heldErr what its context said at the end of it.
	hold    time.Duration
	heldErr error
	// copied is the body a fork sent, and snapshotted the body a snapshot create sent.
	copied      sandbox.CopyRequest
	snapshotted sandbox.SnapshotRequest
	ref         string
	// waited is the ref a get with ?wait blocked on.
	waited string
	// granted is the secret the grant or the ungrant named.
	granted string
	// attached is the policy the attach named.
	attached string
	force    bool

	// exec is the request the client sent, and input what it typed at the command.
	exec   sandbox.ExecRequest
	input  string
	execID string
	// attachedExec is the exec the client attached to, and resizedExec the one it resized.
	attachedExec string
	// out and errOut are what the command writes on each stream, exit how it ended, and lost what it evicted unread.
	out    string
	errOut string
	exit   models.ExitStatus
	lost   int64
	// stall detaches the client the way the buffer does once it took no output for the bound.
	stall bool
	// execErr is how the command failed, apart from err, which is a refusal before the 101.
	execErr     error
	resizedExec string
	size        sandbox.TerminalSize

	// execState is the state a get reports, and execStatus how a waited or a got exec ended.
	execState  models.ExecState
	execStatus *models.ExitStatus
	// execs is the page a list answers.
	execs []models.Exec
	// killedExec and killSignal are the exec and the signal a kill named; deletedExec the delete's.
	killedExec  string
	killSignal  string
	deletedExec string

	// lines is what the output holds, and stops is what ends a follow, with reason as why.
	lines    []string
	followed bool
	stops    chan struct{}
	reason   string
	// ended is closed when a verb that waited on stops or on the client returns.
	ended chan struct{}
	// pulled is what a create reports to the progress on its context.
	pulled []image.Event

	// appExit is how an attach or a wait says the app ended, appErr how it failed after the 101, and stoppedApp what an app/stop asked.
	appExit    models.AppExit
	appErr     error
	stoppedApp bool

	// file is what a put named and landed, and stat and content what a stat or a get answers.
	file      sandbox.FileWrite
	landed    string
	fileOp    string
	filePath  string
	stat      models.FileStat
	content   string
	bodyErr   error
	closedErr error
	// hangUpErr holds an archive open after its content until the request ends, then reads as the exec the hang-up shut.
	hangUpErr error
	// entries is what an ls answers, listErr how it fails after them; dir is the mkdir's body, recursive the delete's flag.
	entries   []models.FileEntry
	listErr   error
	dir       sandbox.MkdirRequest
	recursive bool
	// archive is what a put of an archive named.
	archive sandbox.ArchiveWrite
}

func (f *fakeLifecycle) StatFile(_ context.Context, ref, path string) (models.FileStat, error) {
	f.ref, f.fileOp, f.filePath = ref, "stat", path

	return f.stat, f.err
}

// ReadFile answers content as the body; bodyErr cuts it after the content, the way a guest that died midway does.
func (f *fakeLifecycle) ReadFile(_ context.Context, ref, path string) (models.FileStat, io.ReadCloser, error) {
	f.ref, f.fileOp, f.filePath = ref, "read", path
	if f.err != nil {
		return models.FileStat{}, nil, f.err
	}

	body := io.Reader(strings.NewReader(f.content))
	if f.bodyErr != nil {
		body = io.MultiReader(body, iotest.ErrReader(f.bodyErr))
	}

	return f.stat, fakeBody{Reader: body, err: f.closedErr}, nil
}

func (f *fakeLifecycle) WriteFile(_ context.Context, ref string, req sandbox.FileWrite, src io.Reader) error {
	f.ref, f.fileOp, f.file = ref, "write", req
	if f.err != nil {
		return f.err
	}

	landed, err := io.ReadAll(src)
	f.landed = string(landed)

	return err
}

// ListDir answers entries, then listErr in place of the end when it is set; closedErr is what the close says.
func (f *fakeLifecycle) ListDir(_ context.Context, ref, path string) (sandbox.Listing, error) {
	f.ref, f.fileOp, f.filePath = ref, "ls", path
	if f.err != nil {
		return nil, f.err
	}

	return &fakeListing{entries: f.entries, err: f.listErr, closeErr: f.closedErr}, nil
}

func (f *fakeLifecycle) MakeDir(_ context.Context, ref string, req sandbox.MkdirRequest) error {
	f.ref, f.fileOp, f.dir = ref, "mkdir", req

	return f.err
}

func (f *fakeLifecycle) DeleteFile(_ context.Context, ref, path string, recursive bool) error {
	f.ref, f.fileOp, f.filePath, f.recursive = ref, "delete", path, recursive

	return f.err
}

// ReadArchive answers content as the tar, cut by bodyErr the way ReadFile's is.
func (f *fakeLifecycle) ReadArchive(ctx context.Context, ref, path string) (models.FileStat, io.ReadCloser, error) {
	f.ref, f.fileOp, f.filePath = ref, "pack", path
	if f.err != nil {
		return models.FileStat{}, nil, f.err
	}

	body := io.Reader(strings.NewReader(f.content))
	if f.bodyErr != nil {
		body = io.MultiReader(body, iotest.ErrReader(f.bodyErr))
	}
	if f.hangUpErr != nil {
		body = io.MultiReader(body, untilDone{ctx: ctx, err: f.hangUpErr})
	}

	return f.stat, fakeBody{Reader: body, err: f.closedErr}, nil
}

func (f *fakeLifecycle) WriteArchive(_ context.Context, ref string, req sandbox.ArchiveWrite, src io.Reader) error {
	f.ref, f.fileOp, f.archive = ref, "unpack", req
	if f.err != nil {
		return f.err
	}

	landed, err := io.ReadAll(src)
	f.landed = string(landed)

	return err
}

type fakeListing struct {
	entries  []models.FileEntry
	err      error
	closeErr error
}

func (l *fakeListing) Next() (models.FileEntry, error) {
	if len(l.entries) > 0 {
		entry := l.entries[0]
		l.entries = l.entries[1:]

		return entry, nil
	}
	if l.err != nil {
		return models.FileEntry{}, l.err
	}

	return models.FileEntry{}, io.EOF
}

func (l *fakeListing) Close() error { return l.closeErr }

type fakeBody struct {
	io.Reader
	err error
}

func (b fakeBody) Close() error { return b.err }

// untilDone blocks until ctx ends, then fails with err.
type untilDone struct {
	ctx context.Context
	err error
}

func (u untilDone) Read([]byte) (int, error) {
	<-u.ctx.Done()

	return 0, u.err
}

func (f *fakeLifecycle) Create(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	f.created = req
	if f.hold > 0 {
		time.Sleep(f.hold)
		f.heldErr = ctx.Err()
	}
	for _, e := range f.pulled {
		image.ProgressFrom(ctx).Add(e)
	}

	id := f.createdID
	if id == "" {
		id = "sandbox1"
	}

	return models.Sandbox{ID: id, Name: req.Name, Image: req.Image, State: models.StatePending}, f.err
}

// CreateAndWait records the id it waited on, then answers the record the test seeded under it.
func (f *fakeLifecycle) CreateAndWait(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error) {
	sb, err := f.Create(ctx, req)
	if err != nil {
		return models.Sandbox{}, err
	}
	f.waited = sb.ID

	return sandbox.Get(f.repo, sb.ID)
}

// WaitState records the ref a get with ?wait blocked on, and refuses like any verb.
func (f *fakeLifecycle) WaitState(_ context.Context, ref string) error {
	f.waited = ref

	return f.err
}

func (f *fakeLifecycle) Start(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref = ref

	return models.Sandbox{ID: ref, State: models.StateRunning}, f.err
}

func (f *fakeLifecycle) GrantSecret(_ context.Context, ref, name string) (models.Sandbox, error) {
	f.ref, f.granted = ref, name

	return models.Sandbox{ID: ref, Secrets: []string{name}}, f.err
}

func (f *fakeLifecycle) UngrantSecret(_ context.Context, ref, name string) (models.Sandbox, error) {
	f.ref, f.granted = ref, name

	return models.Sandbox{ID: ref}, f.err
}

func (f *fakeLifecycle) AttachPolicy(_ context.Context, ref, name string) (models.Sandbox, error) {
	f.ref, f.attached = ref, name

	return models.Sandbox{ID: ref, Policy: name}, f.err
}

func (f *fakeLifecycle) DetachPolicy(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref, f.attached = ref, ""

	return models.Sandbox{ID: ref}, f.err
}

func (f *fakeLifecycle) Stop(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref = ref

	return models.Sandbox{ID: ref, State: models.StateStopped}, f.err
}

func (f *fakeLifecycle) Remove(_ context.Context, ref string, force bool) error {
	f.ref, f.force = ref, force

	return f.err
}

func (f *fakeLifecycle) Pause(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref = ref

	return models.Sandbox{ID: ref, State: models.StatePaused}, f.err
}

func (f *fakeLifecycle) Resume(_ context.Context, ref string) (models.Sandbox, error) {
	f.ref = ref

	return models.Sandbox{ID: ref, State: models.StateRunning}, f.err
}

func (f *fakeLifecycle) Fork(_ context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error) {
	f.ref, f.copied = ref, req

	return models.Sandbox{ID: "sandbox2", Name: req.Name, State: models.StateRunning}, f.err
}

func (f *fakeLifecycle) CreateSnapshot(_ context.Context, req sandbox.SnapshotRequest) (models.Snapshot, error) {
	f.snapshotted = req

	return models.Snapshot{ID: "snap1", Name: req.Name, Source: req.Sandbox}, f.err
}

func (f *fakeLifecycle) ListSnapshots(context.Context) ([]models.Snapshot, error) {
	return []models.Snapshot{{ID: "snap1"}, {ID: "snap2"}, {ID: "snap3"}}, f.err
}

func (f *fakeLifecycle) InspectSnapshot(_ context.Context, ref string) (models.Snapshot, error) {
	f.ref = ref

	return models.Snapshot{ID: "snap1", Name: "base"}, f.err
}

func (f *fakeLifecycle) RemoveSnapshot(_ context.Context, ref string) error {
	f.ref = ref

	return f.err
}

// CreateExec starts the exec the way the orchestrator does, running from the moment it returns.
func (f *fakeLifecycle) CreateExec(_ context.Context, ref string, req sandbox.ExecRequest) (models.Exec, error) {
	f.ref, f.exec = ref, req

	if f.err != nil {
		return models.Exec{}, f.err
	}

	return f.record(f.execID, models.ExecRunning), nil
}

// record is the exec the fake reports, named after the reference and the request it last saw.
func (f *fakeLifecycle) record(execID string, state models.ExecState) models.Exec {
	return models.Exec{ID: execID, Sandbox: f.ref, Command: f.exec.Command, State: state, ExitStatus: f.execStatus}
}

// ListExecs answers the page the test loaded, the way the orchestrator lists a sandbox's execs.
func (f *fakeLifecycle) ListExecs(_ context.Context, ref string) ([]models.Exec, error) {
	f.ref = ref

	if f.err != nil {
		return nil, f.err
	}

	return f.execs, nil
}

// GetExec answers the record as it stands, and refuses like any verb.
func (f *fakeLifecycle) GetExec(_ context.Context, ref, execID string) (models.Exec, error) {
	f.ref = ref

	if f.err != nil {
		return models.Exec{}, f.err
	}

	return f.record(execID, f.execState), nil
}

// WaitExec answers the record once it has ended, so its state is exited and it carries its status.
func (f *fakeLifecycle) WaitExec(_ context.Context, ref, execID string) (models.Exec, error) {
	f.ref = ref

	if f.err != nil {
		return models.Exec{}, f.err
	}

	return f.record(execID, models.ExecExited), nil
}

// KillExec records the exec and the signal it was sent, and refuses like any verb.
func (f *fakeLifecycle) KillExec(_ context.Context, ref, execID, signal string) error {
	f.ref, f.killedExec, f.killSignal = ref, execID, signal

	return f.err
}

// DeleteExec records the exec it forgot, and refuses like any verb.
func (f *fakeLifecycle) DeleteExec(_ context.Context, ref, execID string) error {
	f.ref, f.deletedExec = ref, execID

	return f.err
}

// Attach answers the client the way the orchestrator does: it starts the session, writes, and then exits.
func (f *fakeLifecycle) Attach(ctx context.Context, ref, execID string, streams sandbox.Streams) (sandbox.Attached, error) {
	f.ref, f.attachedExec = ref, execID

	if f.err != nil {
		return sandbox.Attached{}, f.err
	}

	if streams.Started != nil {
		if err := streams.Started(execID); err != nil {
			return sandbox.Attached{}, err
		}
	}

	// A command that never ran reads nothing, the way the substrate answers one it could not start.
	if f.execErr != nil {
		return sandbox.Attached{}, f.execErr
	}

	if f.stall {
		streams.Detach()

		return sandbox.Attached{}, &sandbox.StalledError{ID: execID}
	}

	if f.out != "" {
		if _, err := streams.Stdout.Write([]byte(f.out)); err != nil {
			return sandbox.Attached{}, err
		}
	}
	if f.errOut != "" {
		if _, err := streams.Stderr.Write([]byte(f.errOut)); err != nil {
			return sandbox.Attached{}, err
		}
	}

	// A command that waits ends the way a cancelled exec does: when the client goes away.
	if f.stops != nil {
		defer close(f.ended)

		select {
		case <-f.stops:
		case <-ctx.Done():
			return sandbox.Attached{}, ctx.Err()
		}
	}

	// A command with no stdin reads none, the way the orchestrator drains what such a client types.
	if f.exec.Stdin && streams.Stdin != nil {
		read, err := io.ReadAll(streams.Stdin)
		if err != nil {
			return sandbox.Attached{}, err
		}
		f.input = string(read)
	}

	return sandbox.Attached{Exit: f.exit, LostBytes: f.lost}, nil
}

func (f *fakeLifecycle) ResizeExec(_ context.Context, ref, execID string, size sandbox.TerminalSize) error {
	f.ref, f.resizedExec, f.size = ref, execID, size

	return f.err
}

func (f *fakeLifecycle) Logs(_ context.Context, ref string, w io.Writer) error {
	f.ref = ref

	if f.err != nil {
		return f.err
	}

	return f.write(w)
}

// FollowLogs ends when the sandbox stops; this one ends when the test says the sandbox has, or the client left.
func (f *fakeLifecycle) FollowLogs(ctx context.Context, ref string, w io.Writer) (string, error) {
	f.ref, f.followed = ref, true

	if f.err != nil {
		return "", f.err
	}
	if err := f.write(w); err != nil {
		return "", err
	}

	if f.stops != nil {
		defer close(f.ended)

		select {
		case <-f.stops:
		case <-ctx.Done():
			return "", nil
		}
	}

	return f.reason, nil
}

// AttachApp refuses before the 101 on err; otherwise it writes the lines and ends like the app did, or on the client.
func (f *fakeLifecycle) AttachApp(ctx context.Context, ref string, open func() (io.Writer, error)) (models.AppExit, error) {
	f.ref = ref

	if f.err != nil {
		return models.AppExit{}, f.err
	}
	w, err := open()
	if err != nil {
		return models.AppExit{}, err
	}
	if err := f.write(w); err != nil {
		return models.AppExit{}, err
	}

	if f.stops != nil {
		defer close(f.ended)

		select {
		case <-f.stops:
		case <-ctx.Done():
			return models.AppExit{}, ctx.Err()
		}
	}

	return f.appExit, f.appErr
}

func (f *fakeLifecycle) WaitApp(_ context.Context, ref string) (models.AppExit, error) {
	f.ref, f.waited = ref, ref

	if f.err != nil {
		return models.AppExit{}, f.err
	}

	return f.appExit, f.appErr
}

func (f *fakeLifecycle) StopApp(_ context.Context, ref string, force bool) error {
	f.ref, f.stoppedApp, f.force = ref, true, force

	return f.err
}

func (f *fakeLifecycle) write(w io.Writer) error {
	for _, line := range f.lines {
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}

	return nil
}

func head(t *testing.T, server *httptest.Server, path string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+path, nil)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	defer resp.Body.Close()

	return resp.StatusCode
}

// send answers with the status and the decoded body, or a nil body on a 204.
func send(t *testing.T, server *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		raw, err := io.ReadAll(resp.Body)
		if err != nil || len(raw) != 0 {
			t.Errorf("%s %s answered 204 with a body %q: %v", method, path, raw, err)
		}

		return resp.StatusCode, nil
	}

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s %s: decode the body: %v", method, path, err)
	}

	return resp.StatusCode, decoded
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestCreateAnswers201WithTheRecord(t *testing.T) {
	s := seed(t)

	body := `{"image":"alpine:3.20","name":"web","command":["sh","-c","sleep 600"],"env":["A=1"],"secrets":["TOKEN"],"policy":"locked","resources":{"memory_mib":512,"vcpus":2}}`
	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", body)
	if status != http.StatusCreated || got["id"] != "sandbox1" || got["state"] != "pending" {
		t.Fatalf("POST /v0/sandboxes answered %d %v, want 201 with the pending record", status, got)
	}

	var want sandbox.CreateRequest
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustJSON(t, s.verbs.created), mustJSON(t, want)) {
		t.Errorf("the orchestrator got %+v, want %+v", s.verbs.created, want)
	}
}

// A create with ?wait=true blocks until the sandbox leaves pending, then answers the record it settled into.
func TestCreateWithWaitAnswersTheSettledRecord(t *testing.T) {
	s := seed(t)
	settled := create(t, s.repo, "cold", models.StateRunning)
	s.verbs.createdID = settled.ID

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes?wait=true", `{"image":"alpine"}`)
	if status != http.StatusCreated || got["state"] != "running" {
		t.Fatalf("POST ?wait answered %d %v, want 201 with the settled record", status, got)
	}
	if s.verbs.waited != settled.ID {
		t.Errorf("the create waited on %q, want the new id %s", s.verbs.waited, settled.ID)
	}
}

// A create that never reaches running settles failed, and ?wait answers that, never the pending record.
func TestCreateWithWaitSurfacesAFailedCreate(t *testing.T) {
	s := seed(t)
	settled := create(t, s.repo, "broken", models.StateFailed)
	s.verbs.createdID = settled.ID

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes?wait=true", `{"image":"alpine"}`)
	if status != http.StatusCreated || got["state"] != "failed" {
		t.Fatalf("POST ?wait on a failing create answered %d %v, want 201 with the failed record", status, got)
	}
}

// An app that never started is refused with the code a shell answers, on a plain create and a waited one alike.
func TestCreateRefusesAnAppThatNeverStarted(t *testing.T) {
	for _, path := range []string{"/v0/sandboxes", "/v0/sandboxes?wait=true"} {
		s := seed(t)
		refused := &models.CommandNotStartedError{Sandbox: "sandbox1", Command: "/no/such/app", Reason: "no such file or directory", Code: models.CommandNotFoundExitCode}
		s.verbs.err = refused

		status, got := send(t, s.server, http.MethodPost, path, `{"image":"alpine"}`)
		refusal := errorOf(t, got)
		if status != http.StatusUnprocessableEntity || refusal.code != string(models.CodeCommandNotStarted) || exitCodeOf(got) != models.CommandNotFoundExitCode {
			t.Errorf("POST %s answered %d %v, want 422 command_not_started with exit_code 127", path, status, got)
		}
		if refusal.message != refused.Error() {
			t.Errorf("POST %s answered the message %q, want %q, which names the command", path, refusal.message, refused.Error())
		}
	}
}

// The streamed create carries the same refusal as its last line, after the pull it already streamed.
func TestCreateStreamsTheRefusalOfAnAppThatNeverStarted(t *testing.T) {
	s := seed(t)
	s.verbs.pulled = []image.Event{{Status: image.StatusCached, Reference: "docker.io/library/alpine:3.20", Path: "/images/alpine"}}
	refused := &models.CommandNotStartedError{Sandbox: "sandbox1", Command: "/srv/app", Reason: "permission denied", Code: models.CommandNotExecutableExitCode}
	s.verbs.err = refused

	status, _, lines := sendStreamed(t, s.server, "/v0/sandboxes?wait=true", `{"image":"alpine:3.20"}`)
	if status != http.StatusCreated || len(lines) != 2 {
		t.Fatalf("the create answered %d with %v, want 201, the event and the refusal", status, lines)
	}
	refusal := errorOf(t, lines[1])
	if refusal.code != string(models.CodeCommandNotStarted) || exitCodeOf(lines[1]) != models.CommandNotExecutableExitCode {
		t.Errorf("the last line is %v, want command_not_started with exit_code 126", lines[1])
	}
	if refusal.message != refused.Error() {
		t.Errorf("the last line carries the message %q, want %q, which names the command", refusal.message, refused.Error())
	}
}

// The wait query picks the waited create, the only one that removes a refused sandbox.
func TestCreatePassesTheWaitToTheOrchestrator(t *testing.T) {
	for path, want := range map[string]bool{"/v0/sandboxes": false, "/v0/sandboxes?wait=true": true} {
		s := seed(t)
		s.verbs.createdID = s.running.ID

		if status, got := send(t, s.server, http.MethodPost, path, `{"image":"alpine"}`); status != http.StatusCreated {
			t.Fatalf("POST %s answered %d %v, want 201", path, status, got)
		}
		if waited := s.verbs.waited != ""; waited != want {
			t.Errorf("POST %s waited %t, want %t", path, waited, want)
		}
	}
}

func exitCodeOf(body map[string]any) int {
	object, ok := body["error"].(map[string]any)
	if !ok {
		return 0
	}
	code, ok := object["exit_code"].(float64)
	if !ok {
		return 0
	}

	return int(code)
}

// The plain create answers at once with the pending record and never blocks on the state leaving pending.
func TestCreateWithoutWaitDoesNotBlock(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", `{"image":"alpine"}`)
	if status != http.StatusCreated || got["state"] != "pending" {
		t.Fatalf("POST answered %d %v, want 201 with the pending record", status, got)
	}
	if s.verbs.waited != "" {
		t.Errorf("a create with no ?wait blocked on %q, want it to answer at once", s.verbs.waited)
	}
}

func TestCreateIs400ForABodyItCannotDecode(t *testing.T) {
	s := seed(t)

	for name, body := range map[string]string{"not json": "{not json", "an unknown field": `{"image":"alpine","imagee":"x"}`} {
		status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes", body)
		if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
			t.Errorf("POST with %s answered %d %v, want 400 invalid_request", name, status, got)
		}
	}
	if s.verbs.created.Image != "" {
		t.Errorf("a body that did not decode still reached the orchestrator: %+v", s.verbs.created)
	}
}

// 400 for the request, 404 for the reference, 409 for the state, 500 for the host; the code says which refusal.
func TestTheStatusAndTheCodeFollowTheError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		text   string
	}{
		{"a request error", &sandbox.RequestError{Err: errors.New("secret NOPE does not exist")}, http.StatusBadRequest, "invalid_request", "secret NOPE"},
		{"a body past the cap", &sandbox.RequestError{Err: fmt.Errorf("decode the request body: %w", &http.MaxBytesError{Limit: 1 << 20})}, http.StatusRequestEntityTooLarge, "body_too_large", "too large"},
		{"a bad name", &sandboxstate.ValidationError{Reason: "the name is a slash"}, http.StatusBadRequest, "invalid_request", "slash"},
		{"not found", &models.NotFoundError{Err: fmt.Errorf("sandbox ghost: %w", sandboxstate.ErrNotFound)}, http.StatusNotFound, "not_found", "ghost"},
		{"a name taken", &sandboxstate.NameTakenError{Noun: "sandbox", Name: "web", Holder: "quiet-heron-3f0a"}, http.StatusConflict, "name_taken", "taken by sandbox quiet-heron-3f0a"},
		{"not running", &sandbox.StateError{ID: "sandbox1", State: models.StateStopped, Fix: "pause takes a running sandbox", Code: models.CodeSandboxNotRunning}, http.StatusConflict, "sandbox_not_running", "sandbox sandbox1 is stopped: pause takes a running sandbox"},
		{"not stopped", &sandbox.StateError{ID: "sandbox1", State: models.StateRunning, Fix: "stop it first with shard stop sandbox1, or pass --force", Code: models.CodeSandboxNotStopped}, http.StatusConflict, "sandbox_not_stopped", "sandbox sandbox1 is running: stop it first with shard stop sandbox1, or pass --force"},
		{"not paused", &sandbox.StateError{ID: "sandbox1", State: models.StateRunning, Fix: "resume takes a paused sandbox", Code: models.CodeSandboxNotPaused}, http.StatusConflict, "sandbox_not_paused", "resume takes a paused sandbox"},
		{"live", &sandbox.StateError{ID: "sandbox1", State: models.StateRunning, Fix: "stop it first", Code: models.CodeSandboxLive}, http.StatusConflict, "sandbox_live", "stop it first"},
		{"no checkpoint", &sandbox.StateError{ID: "sandbox1", State: models.StatePaused, Fix: "its record names no checkpoint to resume from", Code: models.CodeNoCheckpoint}, http.StatusConflict, "no_checkpoint", "no checkpoint"},
		{"gone from the substrate", &sandbox.UnavailableError{ID: "sandbox1", Why: "is gone from gvisor", Fix: "remove it with shard remove sandbox1 and create another"}, http.StatusConflict, "sandbox_not_running", "gone from gvisor"},
		{"an unclaimed verb", models.Unsupported("gvisor", "fork"), http.StatusConflict, "unsupported", "provider gvisor does not support fork on this host"},
		{"anything else", errors.New("runsc: boom"), http.StatusInternalServerError, "internal", "its log has the cause"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := seed(t)
			s.verbs.err = c.err

			for _, route := range []struct{ method, path, body string }{
				{http.MethodPost, "/v0/sandboxes", `{"image":"alpine"}`},
				{http.MethodPost, "/v0/sandboxes/sandbox1/start", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/stop", ""},
				{http.MethodDelete, "/v0/sandboxes/sandbox1", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/pause", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/resume", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/fork", `{"name":"web-2"}`},
				{http.MethodPost, "/v0/snapshots", `{"sandbox":"sandbox1"}`},
				{http.MethodGet, "/v0/snapshots/base", ""},
				{http.MethodDelete, "/v0/snapshots/base", ""},
				{http.MethodPost, "/v0/sandboxes/sandbox1/secrets/TOKEN", ""},
				{http.MethodDelete, "/v0/sandboxes/sandbox1/secrets/TOKEN", ""},
				{http.MethodPut, "/v0/sandboxes/sandbox1/policy", `{"policy":"locked"}`},
				{http.MethodDelete, "/v0/sandboxes/sandbox1/policy", ""},
			} {
				status, got := send(t, s.server, route.method, route.path, route.body)
				if status != c.status || errorOf(t, got).code != c.code || !strings.Contains(errorOf(t, got).message, c.text) {
					t.Errorf("%s %s answered %d %v, want %d %s with %q", route.method, route.path, status, got, c.status, c.code, c.text)
				}
			}
		})
	}
}

func TestGrantAndUngrantNameTheSandboxAndTheSecret(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/web/secrets/TOKEN", "")
	if status != http.StatusOK || s.verbs.ref != "web" || s.verbs.granted != "TOKEN" {
		t.Errorf("the grant answered %d %v over ref %q and secret %q", status, got, s.verbs.ref, s.verbs.granted)
	}

	status, got = send(t, s.server, http.MethodDelete, "/v0/sandboxes/web/secrets/TOKEN", "")
	if status != http.StatusOK || got["secrets"] != nil {
		t.Errorf("the ungrant answered %d %v, want 200 with a record that holds none", status, got)
	}
}

func TestAttachAndDetachNameTheSandboxAndThePolicy(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPut, "/v0/sandboxes/web/policy", `{"policy":"locked"}`)
	if status != http.StatusOK || s.verbs.ref != "web" || s.verbs.attached != "locked" {
		t.Errorf("the attach answered %d %v over ref %q and policy %q", status, got, s.verbs.ref, s.verbs.attached)
	}
	if got["policy"] != "locked" {
		t.Errorf("the attach answered a record that holds %v", got["policy"])
	}

	status, got = send(t, s.server, http.MethodDelete, "/v0/sandboxes/web/policy", "")
	if status != http.StatusOK || got["policy"] != nil {
		t.Errorf("the detach answered %d %v, want 200 with a record that holds none", status, got)
	}
}

// A body the handler cannot read is a 400, and the orchestrator is never asked.
func TestAttachRefusesABodyItCannotRead(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPut, "/v0/sandboxes/web/policy", "{")
	if status != http.StatusBadRequest {
		t.Errorf("the attach answered %d %v, want 400", status, got)
	}
	if s.verbs.ref != "" {
		t.Errorf("the handler asked the orchestrator over ref %q", s.verbs.ref)
	}
}

func TestStartAnswersTheRecord(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/web/start", "")
	if status != http.StatusOK || got["id"] != "web" || got["state"] != "running" {
		t.Errorf("POST /v0/sandboxes/web/start answered %d %v, want 200 with the record", status, got)
	}
	if s.verbs.ref != "web" {
		t.Errorf("the orchestrator got the reference %q, want web", s.verbs.ref)
	}
}

// A start the substrate broke is named in the daemon log, and one it refused stays the client's alone (SHARD-416).
func TestAFailedStartIsLoggedOnlyWhenTheSubstrateBrokeIt(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		logged bool
	}{
		"broke":   {err: errors.New("shard-init failed at boot with exit 125: mount /dev/vdb on /overlay: read-only file system"), status: http.StatusInternalServerError, logged: true},
		"refused": {err: sandboxstate.ErrNotFound, status: http.StatusNotFound},
	}

	for name, c := range cases {
		s := seed(t)
		s.verbs.err = c.err
		var out bytes.Buffer
		handler := api.NewHandler("v-test", fakeProcess{}, s.repo, nil, s.verbs, s.stores, s.egress, nil, &out)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v0/sandboxes/web/start", nil))

		if w.Code != c.status {
			t.Errorf("%s: the start answered %d, want %d", name, w.Code, c.status)
		}
		if logged := strings.Contains(out.String(), "start sandbox web: "+c.err.Error()); logged != c.logged {
			t.Errorf("%s: the daemon log holds %q, want the failure logged %t", name, out.String(), c.logged)
		}
	}
}

// A pause and a resume act on the sandbox that is there, so each answers 200 with its record.
func TestPauseAndResumeAnswerTheRecord(t *testing.T) {
	cases := map[string]string{"pause": "paused", "resume": "running"}

	for verb, state := range cases {
		s := seed(t)

		status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/web/"+verb, "")
		if status != http.StatusOK || got["id"] != "web" || got["state"] != state {
			t.Errorf("POST /v0/sandboxes/web/%s answered %d %v, want 200 with the record", verb, status, got)
		}
		if s.verbs.ref != "web" {
			t.Errorf("the orchestrator got the reference %q, want web", s.verbs.ref)
		}
	}
}

// A fork makes a sandbox, so it answers 201 with the new record and never the source's.
func TestForkAnswers201WithTheNewRecord(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/fork", `{"name":"web-2"}`)
	if status != http.StatusCreated || got["id"] != "sandbox2" || got["name"] != "web-2" {
		t.Errorf("POST fork answered %d %v, want 201 with the new record", status, got)
	}
	if s.verbs.ref != "sandbox1" || s.verbs.copied.Name != "web-2" {
		t.Errorf("the orchestrator got ref=%q name=%q, want sandbox1 and web-2", s.verbs.ref, s.verbs.copied.Name)
	}

	// A fork with no name is the common one, and an empty body is how the CLI sends it.
	if _, _ = send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/fork", ""); s.verbs.copied.Name != "" {
		t.Errorf("an empty body gave the name %q, want none", s.verbs.copied.Name)
	}
}

func TestForkIs400ForABodyItCannotDecode(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/fork", `{"named":"web-2"}`)
	if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
		t.Errorf("POST fork with an unknown field answered %d %v, want 400", status, got)
	}
	if s.verbs.ref != "" {
		t.Error("a body that did not decode still reached the orchestrator")
	}
}

func TestStopAnswers200WithTheRecordForAnEmptyBody(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/stop", "")
	if status != http.StatusOK || got["state"] != "stopped" || s.verbs.ref != "sandbox1" {
		t.Errorf("POST stop answered %d %v for %q, want 200 with the record of sandbox1", status, got, s.verbs.ref)
	}
}

func TestStopIs400ForAnUnknownField(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodPost, "/v0/sandboxes/sandbox1/stop", `{"bogus":1}`)
	refusal := errorOf(t, got)
	if status != http.StatusBadRequest || refusal.code != "invalid_request" || !strings.Contains(refusal.message, "body.bogus") {
		t.Errorf("POST stop with an unknown field answered %d %v, want 400 naming it", status, got)
	}
	if s.verbs.ref != "" {
		t.Error("a stop with an unknown field still reached the orchestrator")
	}
}

func TestDeleteAnswers204AndPassesForce(t *testing.T) {
	s := seed(t)

	status, _ := send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1?force=true", "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE answered %d, want 204", status)
	}
	if s.verbs.ref != "sandbox1" || !s.verbs.force {
		t.Errorf("the orchestrator got ref=%q force=%v, want sandbox1 true", s.verbs.ref, s.verbs.force)
	}

	send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1", "")
	if s.verbs.force {
		t.Error("a bare delete gave force=true, want false")
	}
}

func TestDeleteIs400ForAQueryItCannotRead(t *testing.T) {
	s := seed(t)

	status, got := send(t, s.server, http.MethodDelete, "/v0/sandboxes/sandbox1?force=yes", "")
	if status != http.StatusBadRequest || errorOf(t, got).code != "invalid_request" {
		t.Errorf("DELETE ?force=yes answered %d %v, want 400", status, got)
	}
	if s.verbs.ref != "" {
		t.Error("a query that did not parse still reached the orchestrator")
	}
}
