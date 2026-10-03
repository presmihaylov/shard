// Package api is the REST surface the daemon serves over its unix socket; the rules live in services/.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/image"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/secret"
)

// Lifecycle is the part of sandbox.Service the routes that change a sandbox call.
type Lifecycle interface {
	Create(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error)
	// WaitState blocks until the sandbox leaves pending, so a get with ?wait sees running or failed.
	WaitState(ctx context.Context, ref string) error
	Start(ctx context.Context, ref string) (models.Sandbox, error)
	Stop(ctx context.Context, ref string) (models.Sandbox, error)
	Remove(ctx context.Context, ref string, force bool) error
	Pause(ctx context.Context, ref string) (models.Sandbox, error)
	Resume(ctx context.Context, ref string) (models.Sandbox, error)
	Fork(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error)
	Clone(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error)
	CreateExec(ctx context.Context, ref string, req sandbox.ExecRequest) (models.Exec, error)
	Attach(ctx context.Context, ref, execID string, streams sandbox.Streams) (sandbox.Attached, error)
	ListExecs(ctx context.Context, ref string) ([]models.Exec, error)
	GetExec(ctx context.Context, ref, execID string) (models.Exec, error)
	WaitExec(ctx context.Context, ref, execID string) (models.Exec, error)
	KillExec(ctx context.Context, ref, execID, signal string) error
	DeleteExec(ctx context.Context, ref, execID string) error
	ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error
	StatFile(ctx context.Context, ref, path string) (models.FileStat, error)
	ReadFile(ctx context.Context, ref, path string) (models.FileStat, io.ReadCloser, error)
	WriteFile(ctx context.Context, ref string, req sandbox.FileWrite, src io.Reader) error
	ListDir(ctx context.Context, ref, path string) (sandbox.Listing, error)
	MakeDir(ctx context.Context, ref string, req sandbox.MkdirRequest) error
	DeleteFile(ctx context.Context, ref, path string, recursive bool) error
	ReadArchive(ctx context.Context, ref, path string) (models.FileStat, io.ReadCloser, error)
	WriteArchive(ctx context.Context, ref string, req sandbox.ArchiveWrite, src io.Reader) error
	Logs(ctx context.Context, ref string, w io.Writer) error
	FollowLogs(ctx context.Context, ref string, w io.Writer) (string, error)
	GrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error)
	UngrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error)
	AttachPolicy(ctx context.Context, ref, name string) (models.Sandbox, error)
	DetachPolicy(ctx context.Context, ref string) (models.Sandbox, error)
}

// EgressLog is what shard logs --egress prints: the newest decisions made for one sandbox, oldest first.
type EgressLog interface {
	// Read returns the newest records and how many older ones it left out.
	Read(sb models.Sandbox) ([]egress.Record, int, error)
	// Follow yields the newest records the log holds and then every one appended after, until ctx ends.
	Follow(ctx context.Context, sb models.Sandbox, yield func(egress.Record) error) error
}

// EgressCutHeader counts the older records an egress log read left out, and is absent when it left out none.
const EgressCutHeader = "Shard-Egress-Cut"

// Daemon is what GET /v0/daemon answers: the process on this socket, its substrate, its proxy ports and its tasks.
type Daemon struct {
	Version      string              `json:"version"`
	PID          int                 `json:"pid"`
	StartedAt    time.Time           `json:"started_at"`
	Socket       string              `json:"socket"`
	Provider     string              `json:"provider"`
	Capabilities models.Capabilities `json:"capabilities"`
	Proxy        Proxy               `json:"proxy"`
	Tasks        []TaskState         `json:"tasks"`
}

// TaskState is one supervised background task: whether it runs, how many times it restarted, and its last error.
type TaskState struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"last_error,omitempty"`
}

// Proxy is where the egress proxy listens on the bridge gateway.
type Proxy struct {
	PlainPort int `json:"plain_port"`
	TLSPort   int `json:"tls_port"`
}

// Process is what the daemon says about itself, less the version the handler holds; it needs the provider.
type Process interface {
	Daemon() (Daemon, error)
}

// Handler answers the routes over one repository, the rules the host enforces, and the one orchestrator.
type Handler struct {
	version   string
	process   Process
	repo      sandbox.Reader
	enforcer  sandbox.Enforcer
	lifecycle Lifecycle
	stores    Stores
	egressLog EgressLog
	log       *log.Logger
}

// NewHandler builds the mux; out takes the one thing a handler cannot return, a write the client hung up on.
func NewHandler(version string, process Process, repo sandbox.Reader, enforcer sandbox.Enforcer, lifecycle Lifecycle, stores Stores, egressLog EgressLog, out io.Writer) http.Handler {
	h := &Handler{
		version:   version,
		process:   process,
		repo:      repo,
		enforcer:  enforcer,
		lifecycle: lifecycle,
		stores:    stores,
		egressLog: egressLog,
		log:       log.New(out, "", log.LstdFlags),
	}

	mux := http.NewServeMux()
	for _, e := range h.routeTable() {
		mux.HandleFunc(e.Method+" "+e.Pattern, e.handler)
	}
	// The mux answers an unknown path with a JSON error, like every other error body on this socket.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeJSON(w, http.StatusNotFound, errorResponse{Error: ErrorObject{Code: models.CodeNotFound, Message: fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path)}})
	})

	return mux
}

// Route is one method and pattern the daemon serves. The front maps each to the capability it enforces.
type Route struct {
	Method  string
	Pattern string
}

// routeEntry binds a route to its handler; routeTable is the one list NewHandler registers and Routes reports.
type routeEntry struct {
	Route
	handler http.HandlerFunc
}

// routeTable is the single source of the daemon's routes, less the catch-all that answers an unknown path.
func (h *Handler) routeTable() []routeEntry {
	return []routeEntry{
		{Route{"GET", "/v0/version"}, h.getVersion},
		{Route{"GET", "/v0/daemon"}, h.getDaemon},
		{Route{"GET", "/v0/sandboxes"}, h.listSandboxes},
		{Route{"GET", "/v0/sandboxes/{id}"}, h.getSandbox},
		{Route{"POST", "/v0/sandboxes"}, h.createSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/start"}, h.startSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/stop"}, h.stopSandbox},
		{Route{"DELETE", "/v0/sandboxes/{id}"}, h.removeSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/pause"}, h.pauseSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/resume"}, h.resumeSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/fork"}, h.forkSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/clone"}, h.cloneSandbox},
		{Route{"POST", "/v0/sandboxes/{id}/exec"}, h.createExec},
		{Route{"GET", "/v0/sandboxes/{id}/exec"}, h.listExecs},
		{Route{"GET", "/v0/sandboxes/{id}/exec/{exec}"}, h.getExec},
		{Route{"POST", "/v0/sandboxes/{id}/exec/{exec}/kill"}, h.killExec},
		{Route{"DELETE", "/v0/sandboxes/{id}/exec/{exec}"}, h.deleteExec},
		{Route{"POST", "/v0/sandboxes/{id}/exec/{exec}/resize"}, h.resizeExec},
		{Route{"PUT", "/v0/sandboxes/{id}/files"}, h.putFile},
		{Route{"GET", "/v0/sandboxes/{id}/files"}, h.getFile},
		// A GET pattern also serves HEAD, so the stat needs its own, more specific one.
		{Route{"HEAD", "/v0/sandboxes/{id}/files"}, h.statFile},
		{Route{"DELETE", "/v0/sandboxes/{id}/files"}, h.deleteFile},
		{Route{"GET", "/v0/sandboxes/{id}/ls"}, h.listDir},
		{Route{"POST", "/v0/sandboxes/{id}/mkdir"}, h.makeDir},
		{Route{"PUT", "/v0/sandboxes/{id}/archive"}, h.putArchive},
		{Route{"GET", "/v0/sandboxes/{id}/archive"}, h.getArchive},
		{Route{"GET", "/v0/sandboxes/{id}/logs"}, h.sandboxLogs},
		{Route{"GET", "/v0/sandboxes/{id}/egress-log"}, h.sandboxEgressLog},
		{Route{"POST", "/v0/sandboxes/{id}/secrets/{name}"}, h.grantSecret},
		{Route{"DELETE", "/v0/sandboxes/{id}/secrets/{name}"}, h.ungrantSecret},
		{Route{"PUT", "/v0/sandboxes/{id}/policy"}, h.attachPolicy},
		{Route{"DELETE", "/v0/sandboxes/{id}/policy"}, h.detachPolicy},
		{Route{"GET", "/v0/policies"}, h.listPolicies},
		{Route{"GET", "/v0/policies/{name}"}, h.getPolicy},
		{Route{"PUT", "/v0/policies/{name}"}, h.putPolicy},
		{Route{"DELETE", "/v0/policies/{name}"}, h.removePolicy},
		{Route{"GET", "/v0/secrets"}, h.listSecrets},
		{Route{"PUT", "/v0/secrets/{name}"}, h.putSecret},
		{Route{"DELETE", "/v0/secrets/{name}"}, h.removeSecret},
		{Route{"GET", "/v0/images"}, h.listImages},
		{Route{"POST", "/v0/images/pull"}, h.pullImage},
		{Route{"POST", "/v0/images/prune"}, h.pruneImages},
		// An image reference carries slashes, so it is the rest of the path and not one segment of it.
		{Route{"DELETE", "/v0/images/{ref...}"}, h.removeImage},
	}
}

// Routes lists every method and pattern the daemon serves. The front covers each with a capability.
func Routes() []Route {
	// The zero Handler is enough: Routes reads only each method and pattern, never a handler.
	var h Handler
	entries := h.routeTable()
	routes := make([]Route, 0, len(entries))
	for _, e := range entries {
		routes = append(routes, e.Route)
	}

	return routes
}

type versionResponse struct {
	Version string `json:"version"`
}

// listResponse is the page: the rows, the cursor of the next page or null, and what could not be read.
type listResponse struct {
	Sandboxes []models.Sandbox `json:"sandboxes"`
	Next      *string          `json:"next"`
	// Warnings names the records the daemon could not read, one string each, beside the ones it could.
	Warnings []string `json:"warnings,omitempty"`
}

// errorResponse is every refusal, one object under error and nothing else at the root.
type errorResponse struct {
	Error ErrorObject `json:"error"`
}

// ErrorObject is a code for a program, a line for a human, and the holders an in_use names.
type ErrorObject struct {
	Code    models.Code `json:"code"`
	Message string      `json:"message"`
	Holders []string    `json:"holders,omitempty"`
}

func (h *Handler) getVersion(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, versionResponse{Version: h.version})
}

func (h *Handler) getDaemon(w http.ResponseWriter, _ *http.Request) {
	d, err := h.process.Daemon()
	if err != nil {
		h.writeError(w, err)

		return
	}
	d.Version = h.version

	h.writeJSON(w, http.StatusOK, d)
}

func (h *Handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	all, err := boolQuery(r, "all")
	if err != nil {
		h.writeError(w, err)

		return
	}

	q, err := pageOf(r, sandboxstate.ValidID)
	if err != nil {
		h.writeError(w, err)

		return
	}

	sandboxes, unreadable := sandbox.List(h.repo, all)

	warnings, err := partial(unreadable)
	if err != nil {
		h.writeError(w, err)

		return
	}

	sandboxes, next := page(sandboxes, q, func(sb models.Sandbox) string { return sb.ID })

	h.writeJSON(w, http.StatusOK, listResponse{Sandboxes: sandboxes, Next: next, Warnings: warnings})
}

// getSandbox answers the record two ways: ?wait=true blocks until a pending create lands, the default reads now.
func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	wait, err := boolQuery(r, "wait")
	if err != nil {
		h.writeError(w, err)

		return
	}

	ref := r.PathValue("id")
	if wait {
		if err := h.lifecycle.WaitState(r.Context(), ref); err != nil {
			h.writeError(w, err)

			return
		}
	}

	sb, err := sandbox.Inspect(h.repo, h.enforcer, ref)
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) sandboxEgressLog(w http.ResponseWriter, r *http.Request) {
	id, err := h.repo.Resolve(r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	sb, err := h.repo.Get(id)
	if err != nil {
		h.writeError(w, err)

		return
	}

	// This handler reads the log around the Service, so it repeats the guard the lifecycle verbs get for free.
	if err := sandbox.FailedGuard(id, sb); err != nil {
		h.writeError(w, err)

		return
	}

	follow, err := boolQuery(r, "follow")
	if err != nil {
		h.writeError(w, err)

		return
	}

	if follow {
		h.followEgressLog(w, r, sb)

		return
	}

	records, cut, err := h.egressLog.Read(sb)
	if err != nil {
		h.writeError(w, err)

		return
	}

	if cut > 0 {
		w.Header().Set(EgressCutHeader, strconv.Itoa(cut))
	}
	h.writeJSON(w, http.StatusOK, records)
}

func (h *Handler) grantSecret(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.GrantSecret(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) ungrantSecret(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.UngrantSecret(r.Context(), r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) attachPolicy(w http.ResponseWriter, r *http.Request) {
	var req sandbox.PolicyAttachRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	sb, err := h.lifecycle.AttachPolicy(r.Context(), r.PathValue("id"), req.Policy)
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) detachPolicy(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.DetachPolicy(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

// createSandbox answers the new record at once, or with ?wait=true once it leaves pending, streaming the pull when asked.
func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	wait, err := boolQuery(r, "wait")
	if err != nil {
		h.writeError(w, err)

		return
	}

	var req sandbox.CreateRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	if err := checkCreateScopes(r.Header, req); err != nil {
		h.writeError(w, err)

		return
	}

	if wait && streamed(r) {
		h.streamProgress(w, r, http.StatusCreated, "create", func(ctx context.Context) (ProgressLine, error) {
			sb, err := h.lifecycle.Create(ctx, req)
			if err != nil {
				return ProgressLine{}, err
			}

			if err := h.lifecycle.WaitState(ctx, sb.ID); err != nil {
				return ProgressLine{}, err
			}

			sb, err = sandbox.Get(h.repo, sb.ID)

			return ProgressLine{Sandbox: &sb}, err
		})

		return
	}

	sb, err := h.lifecycle.Create(r.Context(), req)
	if err != nil {
		h.writeError(w, err)

		return
	}

	if wait {
		if err := h.lifecycle.WaitState(r.Context(), sb.ID); err != nil {
			h.writeError(w, err)

			return
		}

		sb, err = sandbox.Get(h.repo, sb.ID)
		if err != nil {
			h.writeError(w, err)

			return
		}
	}

	h.writeJSON(w, http.StatusCreated, sb)
}

// ScopesHeader carries the token's scopes from the TCP front to the daemon. The front stamps it on every request it forwards and strips any client copy; a request with no such header reached the socket directly.
const ScopesHeader = "X-Shard-Scopes"

// scopeError is a create that names a secret or a policy the token's scopes do not reach; classify maps it to 403.
type scopeError struct {
	scope string
	named string
}

func (e *scopeError) Error() string {
	return fmt.Sprintf("the token does not carry the %q scope, which a create that names a %s needs", e.scope, e.named)
}

// checkCreateScopes refuses a create that names a secret or a policy the stamped scopes do not reach. No header means the request reached the daemon socket directly, which keeps every right.
func checkCreateScopes(header http.Header, req sandbox.CreateRequest) error {
	scopes, stamped := stampedScopes(header)
	if !stamped {
		return nil
	}

	if len(req.Secrets) > 0 && !scopesCover(scopes, "secret:*") {
		return &scopeError{scope: "secret:*", named: "secret"}
	}
	if req.Policy != "" && !scopesCover(scopes, "policy:*") {
		return &scopeError{scope: "policy:*", named: "policy"}
	}

	return nil
}

// stampedScopes reads the scopes the front stamped; stamped is false when no header is present, the local socket.
func stampedScopes(header http.Header) ([]string, bool) {
	values, ok := header[http.CanonicalHeaderKey(ScopesHeader)]
	if !ok {
		return nil, false
	}

	var scopes []string
	for _, value := range values {
		for s := range strings.SplitSeq(value, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}

	return scopes, true
}

// scopesCover reports whether the stamped scopes reach need; no scopes, or a "*" scope, reaches every one, as the front's covers() does.
func scopesCover(scopes []string, need string) bool {
	if len(scopes) == 0 {
		return true
	}

	for _, s := range scopes {
		if s == "*" || s == need {
			return true
		}
	}

	return false
}

func (h *Handler) startSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Start(r.Context(), r.PathValue("id"))
	if err != nil {
		// A start the substrate broke, not one it refused, is named in the daemon log beside the client's answer (SHARD-416).
		if status, _ := classify(err); status >= http.StatusInternalServerError {
			h.log.Printf("api: start sandbox %s: %v", r.PathValue("id"), err)
		}
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

// stopRequest is the body of a stop, which carries nothing; the grace it once took is a 400 as an unknown field (SHARD-460).
type stopRequest struct{}

func (h *Handler) stopSandbox(w http.ResponseWriter, r *http.Request) {
	var req stopRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	sb, err := h.lifecycle.Stop(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) removeSandbox(w http.ResponseWriter, r *http.Request) {
	force, err := boolQuery(r, "force")
	if err != nil {
		h.writeError(w, err)

		return
	}

	// A grace an old client still sends is refused by name, never ignored, so it cannot read as honoured (SHARD-460).
	if r.URL.Query().Has("grace") {
		h.writeError(w, &sandbox.RequestError{Err: fmt.Errorf("the query grace is removed: a stop gives the entrypoint a fixed %s", models.StopGrace)})

		return
	}

	if err := h.lifecycle.Remove(r.Context(), r.PathValue("id"), force); err != nil {
		h.writeError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Pause(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Resume(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) forkSandbox(w http.ResponseWriter, r *http.Request) {
	h.copySandbox(w, r, h.lifecycle.Fork)
}

func (h *Handler) cloneSandbox(w http.ResponseWriter, r *http.Request) {
	h.copySandbox(w, r, h.lifecycle.Clone)
}

// copySandbox is the body a fork and a clone share: both name the new sandbox and answer its record.
func (h *Handler) copySandbox(w http.ResponseWriter, r *http.Request, verb func(context.Context, string, sandbox.CopyRequest) (models.Sandbox, error)) {
	var req sandbox.CopyRequest
	if err := decode(w, r, &req); err != nil {
		h.writeError(w, err)

		return
	}

	sb, err := verb(r.Context(), r.PathValue("id"), req)
	if err != nil {
		h.writeError(w, err)

		return
	}

	h.writeJSON(w, http.StatusCreated, sb)
}

// classify maps what a typed error refused to the status and the code that say so; anything untyped broke.
func classify(err error) (int, models.Code) {
	var invalid *sandboxstate.ValidationError
	var request *sandbox.RequestError
	var nameTaken *sandboxstate.NameTakenError
	var state *sandbox.StateError
	var unavailable *sandbox.UnavailableError
	var held *sandbox.HeldError
	var attached *sandbox.AttachedError
	var execExited *sandbox.ExecExitedError
	var execRunning *sandbox.ExecRunningError
	var substrateTimeout *sandbox.SubstrateTimeoutError
	var tooLarge *http.MaxBytesError
	var scope *scopeError
	var fileNotFound *sandbox.FileNotFoundError

	switch {
	case errors.As(err, &scope):
		return http.StatusForbidden, models.CodeForbidden
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge, models.CodeBodyTooLarge
	case errors.As(err, &invalid), errors.As(err, &request), errors.Is(err, image.ErrBadReference):
		return http.StatusBadRequest, models.CodeInvalidRequest
	case errors.Is(err, sandboxstate.ErrNotFound), errors.Is(err, egress.ErrNotFound),
		errors.Is(err, secret.ErrNotFound), errors.Is(err, image.ErrNotFound), errors.As(err, &fileNotFound):
		return http.StatusNotFound, models.CodeNotFound
	case errors.As(err, &nameTaken):
		return http.StatusConflict, models.CodeNameTaken
	case errors.As(err, &state):
		return http.StatusConflict, state.Code
	case errors.As(err, &unavailable):
		return http.StatusConflict, models.CodeSandboxNotRunning
	case errors.As(err, &execExited):
		return http.StatusConflict, models.CodeExecExited
	case errors.As(err, &execRunning):
		return http.StatusConflict, models.CodeExecRunning
	case errors.As(err, &held), errors.As(err, &attached):
		return http.StatusConflict, models.CodeInUse
	case errors.Is(err, models.ErrUnsupported):
		return http.StatusConflict, models.CodeUnsupported
	case errors.As(err, &substrateTimeout):
		return http.StatusGatewayTimeout, models.CodeSubstrateTimeout
	default:
		return http.StatusInternalServerError, models.CodeInternal
	}
}

// maxBody caps a JSON body, which the decoder holds whole; no route needs more than a few KiB.
const maxBody = 1 << 20

// decode reads a JSON body into out. An empty body is the zero value; a field no route knows is refused.
func decode(w http.ResponseWriter, r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	err := dec.Decode(out)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return &sandbox.RequestError{Err: fmt.Errorf("decode the request body: %w", err)}
	}

	// The decoder stops after one value, so the rest is read to its end and padding meets the cap before a verb runs.
	err = dec.Decode(&json.RawMessage{})
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return &sandbox.RequestError{Err: fmt.Errorf("decode the request body: %w", err)}
	}

	return &sandbox.RequestError{Err: errors.New("decode the request body: it holds more than one JSON value")}
}

// boolQuery reads a flag like ?all=true. An absent flag is false; a value that is not a bool is refused.
func boolQuery(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, &sandbox.RequestError{Err: fmt.Errorf("the query %s=%q is not a boolean", name, raw)}
	}

	return value, nil
}

// partial turns List's joined error into one warning per unreadable record; anything else failed the list itself.
func partial(err error) ([]string, error) {
	if err == nil {
		return nil, nil
	}

	errs := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	}

	warnings := make([]string, 0, len(errs))
	for _, e := range errs {
		var unreadable *sandboxstate.UnreadableError
		if !errors.As(e, &unreadable) {
			return nil, err
		}
		warnings = append(warnings, e.Error())
	}

	return warnings, nil
}

// writeError answers err with the status and the code its type says, and the holders when a store entry is held.
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	status, body := errorBody(err)
	h.writeJSON(w, status, body)
}

// errorBody is the status and the object err answers; a stream that already sent its status writes only the object.
func errorBody(err error) (int, errorResponse) {
	status, code := classify(err)
	body := errorResponse{Error: ErrorObject{Code: code, Message: err.Error()}}

	var held *sandbox.HeldError
	if errors.As(err, &held) {
		body.Error.Holders = held.Users
	}

	return status, body
}

// writeJSON encodes first, so a value that cannot be encoded never leaves a 200 with half a body.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		body = fmt.Appendf(nil, `{"error":{"code":%q,"message":%q}}`, models.CodeInternal, "encode the response: "+err.Error())
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is on its way, so the one thing left to do with a client that hung up is to say so.
	if _, err := w.Write(body); err != nil {
		h.log.Printf("api: write the response: %v", err)
	}
}
