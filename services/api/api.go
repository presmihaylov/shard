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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

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
	// CreateAndWait answers once the sandbox leaves pending, so a refusal the create meets after the pull reaches the caller.
	CreateAndWait(ctx context.Context, req sandbox.CreateRequest) (models.Sandbox, error)
	// WaitState blocks until the sandbox leaves pending, so a get with ?wait sees running or failed.
	WaitState(ctx context.Context, ref string) error
	Start(ctx context.Context, ref string) (models.Sandbox, error)
	Stop(ctx context.Context, ref string) (models.Sandbox, error)
	Remove(ctx context.Context, ref string, force bool) error
	Pause(ctx context.Context, ref string) (models.Sandbox, error)
	Resume(ctx context.Context, ref string) (models.Sandbox, error)
	Fork(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error)
	CreateSnapshot(ctx context.Context, req sandbox.SnapshotRequest) (models.Snapshot, error)
	ListSnapshots(ctx context.Context) ([]models.Snapshot, error)
	InspectSnapshot(ctx context.Context, ref string) (models.Snapshot, error)
	RemoveSnapshot(ctx context.Context, ref string) error
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
	AttachApp(ctx context.Context, ref string, open func() (io.Writer, error)) (models.AppExit, error)
	WaitApp(ctx context.Context, ref string) (models.AppExit, error)
	StopApp(ctx context.Context, ref string, force bool) error
	GrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error)
	UngrantSecret(ctx context.Context, ref, name string) (models.Sandbox, error)
	AttachPolicy(ctx context.Context, ref, name string) (models.Sandbox, error)
	DetachPolicy(ctx context.Context, ref string) (models.Sandbox, error)
}

// EgressLog is what shard policy logs prints: the newest decisions made for one sandbox, oldest first.
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
	// redact puts a secret's name in place of its value in a line the log keeps; nil keeps the line, which only a test does.
	redact func(string) string
}

// NewHandler builds the mux; out takes what a handler cannot return: a write the client hung up on, and the cause behind a public text.
func NewHandler(version string, process Process, repo sandbox.Reader, enforcer sandbox.Enforcer, lifecycle Lifecycle, stores Stores, egressLog EgressLog, redact func(string) string, out io.Writer) http.Handler {
	h := &Handler{
		version:   version,
		process:   process,
		repo:      repo,
		enforcer:  enforcer,
		lifecycle: lifecycle,
		stores:    stores,
		egressLog: egressLog,
		log:       log.New(out, "", log.LstdFlags),
		redact:    redact,
	}

	mux := http.NewServeMux()
	h.register(mux)

	return mux
}

// Class says who reaches a route: the TCP front forwards a public one, and only the daemon socket reaches a local one.
type Class string

const (
	Public Class = "public"
	Local  Class = "local"
)

// Scope is the one coarse right a token needs for a public route; the front enforces it and the spec names it.
type Scope string

const (
	// AnyToken is a route every valid token reaches whatever its scopes, so a client can learn what it speaks to before it acts.
	AnyToken      Scope = "any"
	SandboxRead   Scope = models.ScopeSandboxRead
	SandboxWrite  Scope = models.ScopeSandboxWrite
	SandboxDelete Scope = models.ScopeSandboxDelete
	Exec          Scope = models.ScopeExec
	Secret        Scope = models.ScopeSecret
	Policy        Scope = models.ScopePolicy
)

// Route is one method and pattern the daemon serves, and the scope a public one needs; a local one needs none.
type Route struct {
	Method  string
	Pattern string
	Class   Class
	Scope   Scope
}

type localKey struct{}

// markLocal tells the error path a request came in on a local route, so it answers the whole cause.
func markLocal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), localKey{}, true)))
	}
}

// isLocal says r came in on a local route; an unmarked request is public, so a path that misses the mark leaks nothing.
func isLocal(r *http.Request) bool {
	return r.Context().Value(localKey{}) != nil
}

// routeEntry binds a route to what serves it: a public one to the operation the spec names, a local one to a plain handler.
type routeEntry struct {
	Route
	op      huma.Operation
	serve   endpoint
	handler http.HandlerFunc
}

func public(method, pattern string, scope Scope, op huma.Operation, serve endpoint) routeEntry {
	return routeEntry{Route: Route{Method: method, Pattern: pattern, Class: Public, Scope: scope}, op: op, serve: serve}
}

func local(method, pattern string, handler http.HandlerFunc) routeEntry {
	return routeEntry{Route: Route{Method: method, Pattern: pattern, Class: Local}, handler: handler}
}

// routeTable is the single source of the daemon's routes, less the catch-all that answers an unknown path.
func (h *Handler) routeTable() []routeEntry {
	return []routeEntry{
		public("GET", "/v0/version", AnyToken, operation("meta", "get-version", "Read the daemon and API versions", 0), typed(h.getVersion)),
		public("GET", "/v0/capabilities", AnyToken, operation("meta", "get-capabilities", "List the lifecycle verbs and whether this server supports each", 0), typed(h.getCapabilities)),
		public("GET", "/v0/scopes", AnyToken, operation("meta", "list-scopes", "List the scopes a token can carry", 0), typed(h.getScopes)),
		local("GET", "/v0/daemon", h.getDaemon),
		public("GET", "/v0/sandboxes", SandboxRead, operation("sandboxes", "list-sandboxes", "list active sandboxes", 0), typed(h.listSandboxes)),
		public("GET", "/v0/sandboxes/{id}", SandboxRead, operation("sandboxes", "get-sandbox", "Read a sandbox and the egress rules the host enforces for it", 0), typed(h.getSandbox)),
		public("POST", "/v0/sandboxes", SandboxWrite, operation("sandboxes", "create-sandbox", "create a sandbox", http.StatusCreated), documented(describeCreate, typed(h.createSandbox))),
		public("POST", "/v0/sandboxes/{id}/start", SandboxWrite, operation("sandboxes", "start-sandbox", "start a stopped sandbox with its saved files", 0), typed(h.startSandbox)),
		public("POST", "/v0/sandboxes/{id}/stop", SandboxWrite, operation("sandboxes", "stop-sandbox", "stop a sandbox and preserve its files", 0), typed(h.stopSandbox)),
		public("DELETE", "/v0/sandboxes/{id}", SandboxDelete, operation("sandboxes", "remove-sandbox", "delete a sandbox and its files", 0), typed(h.removeSandbox)),
		public("POST", "/v0/sandboxes/{id}/pause", SandboxWrite, operation("sandboxes", "pause-sandbox", "save a sandbox's state and suspend it", 0), typed(h.pauseSandbox)),
		public("POST", "/v0/sandboxes/{id}/resume", SandboxWrite, operation("sandboxes", "resume-sandbox", "resume a paused sandbox from its saved state", 0), typed(h.resumeSandbox)),
		public("POST", "/v0/sandboxes/{id}/fork", SandboxWrite, operation("sandboxes", "fork-sandbox", "create a sandbox from a running sandbox's memory and files", http.StatusCreated), typed(h.forkSandbox)),
		public("POST", "/v0/sandboxes/{id}/exec", Exec, operation("exec", "create-exec", "execute a command in a running sandbox", http.StatusCreated), typed(h.createExec)),
		public("GET", "/v0/sandboxes/{id}/exec", Exec, operation("exec", "list-execs", "List the execs of a sandbox", 0), typed(h.listExecs)),
		public("GET", "/v0/sandboxes/{id}/exec/{exec}", Exec, operation("exec", "get-exec", "Read, wait for or attach to an exec", 0), raw[getExecInput](h.getExec, describeGetExec)),
		public("POST", "/v0/sandboxes/{id}/exec/{exec}/kill", Exec, operation("exec", "kill-exec", "Send a signal to a running exec", 0), typed(h.killExec)),
		public("DELETE", "/v0/sandboxes/{id}/exec/{exec}", Exec, operation("exec", "delete-exec", "Forget an exec that ended", 0), typed(h.deleteExec)),
		public("POST", "/v0/sandboxes/{id}/exec/{exec}/resize", Exec, operation("exec", "resize-exec", "Resize the terminal of an exec", 0), typed(h.resizeExec)),
		public("PUT", "/v0/sandboxes/{id}/files", Exec, operation("files", "write-file", "copy a file into a running sandbox", http.StatusNoContent), raw[writeFileInput](h.putFile, describeWriteFile)),
		public("GET", "/v0/sandboxes/{id}/files", Exec, operation("files", "read-file", "copy a file out of a running sandbox", 0), raw[filePath](h.getFile, describeReadFile)),
		// A GET pattern also serves HEAD, so the stat needs its own, more specific one.
		public("HEAD", "/v0/sandboxes/{id}/files", Exec, operation("files", "stat-file", "Stat a path", 0), raw[filePath](h.statFile, describeStatFile)),
		public("DELETE", "/v0/sandboxes/{id}/files", Exec, operation("files", "delete-file", "Delete a path", 0), typed(h.deleteFile)),
		public("GET", "/v0/sandboxes/{id}/ls", Exec, operation("files", "list-dir", "List a directory", 0), raw[filePath](h.listDir, describeListDir)),
		public("POST", "/v0/sandboxes/{id}/mkdir", Exec, operation("files", "make-dir", "Make a directory", 0), typed(h.makeDir)),
		public("PUT", "/v0/sandboxes/{id}/archive", Exec, operation("files", "write-archive", "copy a directory into a running sandbox", http.StatusNoContent), raw[archiveInput](h.putArchive, describeWriteArchive)),
		public("GET", "/v0/sandboxes/{id}/archive", Exec, operation("files", "read-archive", "copy a directory out of a running sandbox", 0), raw[filePath](h.getArchive, describeReadArchive)),
		public("GET", "/v0/sandboxes/{id}/logs", SandboxRead, operation("sandboxes", "get-sandbox-logs", "Read or follow the output of a sandbox", 0), raw[followInput](h.sandboxLogs, describeLogs)),
		public("GET", "/v0/sandboxes/{id}/attach", SandboxRead, operation("app", "attach-app", "Wait for or attach to the app of a run", 0), raw[sandboxPath](h.attachApp, describeAttachApp)),
		public("POST", "/v0/sandboxes/{id}/app/stop", SandboxWrite, operation("app", "stop-app", "Stop the app of a run", 0), typed(h.stopApp)),
		public("GET", "/v0/sandboxes/{id}/egress-log", SandboxRead, operation("sandboxes", "get-sandbox-egress-log", "Read or follow the egress decisions of a sandbox", 0), raw[followInput](h.sandboxEgressLog, describeEgressLog)),
		public("POST", "/v0/sandboxes/{id}/secrets/{name}", Secret, operation("sandboxes", "grant-secret", "Grant a secret to a sandbox", 0), typed(h.grantSecret)),
		public("DELETE", "/v0/sandboxes/{id}/secrets/{name}", Secret, operation("sandboxes", "ungrant-secret", "Take a secret back from a sandbox", 0), typed(h.ungrantSecret)),
		public("PUT", "/v0/sandboxes/{id}/policy", Policy, operation("sandboxes", "attach-policy", "Attach a policy to a sandbox", 0), typed(h.attachPolicy)),
		public("DELETE", "/v0/sandboxes/{id}/policy", Policy, operation("sandboxes", "detach-policy", "Detach the policy of a sandbox", 0), typed(h.detachPolicy)),
		public("POST", "/v0/snapshots", SandboxWrite, operation("snapshots", "create-snapshot", "Snapshot a sandbox", http.StatusCreated), typed(h.createSnapshot)),
		public("GET", "/v0/snapshots", SandboxRead, operation("snapshots", "list-snapshots", "List snapshots", 0), typed(h.listSnapshots)),
		public("GET", "/v0/snapshots/{ref}", SandboxRead, operation("snapshots", "get-snapshot", "Read a snapshot", 0), typed(h.getSnapshot)),
		public("DELETE", "/v0/snapshots/{ref}", SandboxDelete, operation("snapshots", "remove-snapshot", "Remove a snapshot", 0), typed(h.removeSnapshot)),
		public("GET", "/v0/policies", Policy, operation("policies", "list-policies", "List policies", 0), typed(h.listPolicies)),
		public("GET", "/v0/policies/{name}", Policy, operation("policies", "get-policy", "Read a policy", 0), typed(h.getPolicy)),
		public("PUT", "/v0/policies/{name}", Policy, operation("policies", "put-policy", "Create or replace a policy", 0), typed(h.putPolicy)),
		public("DELETE", "/v0/policies/{name}", Policy, operation("policies", "remove-policy", "Remove a policy", 0), typed(h.removePolicy)),
		public("GET", "/v0/secrets", Secret, operation("secrets", "list-secrets", "List secrets, never their values", 0), typed(h.listSecrets)),
		public("PUT", "/v0/secrets/{name}", Secret, operation("secrets", "put-secret", "Create or rotate a secret", 0), typed(h.putSecret)),
		public("DELETE", "/v0/secrets/{name}", Secret, operation("secrets", "remove-secret", "Remove a secret", 0), typed(h.removeSecret)),
		local("GET", "/v0/images", h.listImages),
		local("POST", "/v0/images/pull", h.pullImage),
		local("POST", "/v0/images/prune", h.pruneImages),
		// An image reference carries slashes, so it is the rest of the path and not one segment of it.
		local("DELETE", "/v0/images/{ref...}", h.removeImage),
	}
}

// Routes lists every route the daemon serves, which the front and the spec both read.
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

// APIVersion is the path prefix of every route, which a client checks before it trusts the shape of an answer.
const APIVersion = "v0"

type versionResponse struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

// Capabilities is every lifecycle verb and whether this server supports it, the same eight keys for every provider.
type Capabilities struct {
	Create bool `json:"create"`
	Start  bool `json:"start"`
	Stop   bool `json:"stop"`
	Remove bool `json:"remove"`
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
	Fork   bool `json:"fork"`
	// Snapshot is the copy of a stopped sandbox's files, which every provider makes.
	Snapshot bool `json:"snapshot"`
}

// ScopesResponse lists every scope a token can carry, never the caller's own.
type ScopesResponse struct {
	Scopes []models.Scope `json:"scopes"`
}

// sandboxesResponse is the page: the rows, the cursor of the next page or null, and what could not be read.
type sandboxesResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
	Next      *string   `json:"next"`
	// Warnings names the records the daemon could not read, one string each, beside the ones it could.
	Warnings []string `json:"warnings,omitempty"`
}

// ErrorObject is a code for a program, a line for a human, the holders an in_use names, and the shell code a command_not_started carries.
type ErrorObject struct {
	Code     models.Code `json:"code"`
	Message  string      `json:"message"`
	Holders  []string    `json:"holders,omitempty"`
	ExitCode int         `json:"exit_code,omitempty"`
}

func (h *Handler) getVersion(context.Context, *struct{}) (*reply[versionResponse], error) {
	return answer(versionResponse{Version: h.version, APIVersion: APIVersion}, nil)
}

func (h *Handler) getCapabilities(context.Context, *struct{}) (*reply[Capabilities], error) {
	d, err := h.process.Daemon()
	if err != nil {
		return nil, fail(err)
	}

	return answer(capabilitiesOf(d.Capabilities), nil)
}

func (h *Handler) getScopes(context.Context, *struct{}) (*reply[ScopesResponse], error) {
	return answer(ScopesResponse{Scopes: models.Scopes}, nil)
}

// capabilitiesOf answers true for the verbs every provider runs, and the provider's own answer for the optional ones.
func capabilitiesOf(c models.Capabilities) Capabilities {
	return Capabilities{Create: true, Start: true, Stop: true, Remove: true, Pause: c.Pause, Resume: c.Resume, Fork: c.Fork, Snapshot: true}
}

func (h *Handler) getDaemon(w http.ResponseWriter, r *http.Request) {
	d, err := h.process.Daemon()
	if err != nil {
		h.writeError(w, r, err)

		return
	}
	d.Version = h.version

	h.writeJSON(w, http.StatusOK, d)
}

type listSandboxesInput struct {
	All    bool   `query:"all" doc:"List stopped sandboxes too."`
	Limit  int    `query:"limit" minimum:"1" doc:"The most rows a page holds; none answers the whole list."`
	Cursor string `query:"cursor" doc:"The next of the page before; this page starts after it."`
}

func (h *Handler) listSandboxes(_ context.Context, in *listSandboxesInput) (*reply[sandboxesResponse], error) {
	q, err := paged(in.Limit, in.Cursor, sandboxstate.ValidID)
	if err != nil {
		return nil, fail(err)
	}

	sandboxes, unreadable := sandbox.List(h.repo, in.All)

	warnings, err := partial[*sandboxstate.UnreadableError](unreadable, h.warning)
	if err != nil {
		return nil, fail(err)
	}

	sandboxes, next := page(sandboxes, q, func(sb models.Sandbox) string { return sb.ID })

	rows := make([]Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		rows = append(rows, PublicSandbox(sb))
	}

	return answer(sandboxesResponse{Sandboxes: rows, Next: next, Warnings: warnings}, nil)
}

type getSandboxInput struct {
	ID   string `path:"id"`
	Wait bool   `query:"wait" doc:"Block until a pending create lands."`
}

// getSandbox answers the public record two ways: wait blocks until a pending create lands, the default reads now.
func (h *Handler) getSandbox(ctx context.Context, in *getSandboxInput) (*reply[Inspection], error) {
	if in.Wait {
		if err := h.lifecycle.WaitState(ctx, in.ID); err != nil {
			return nil, fail(err)
		}
	}

	insp, err := sandbox.Inspect(h.repo, h.enforcer, in.ID)
	if err != nil {
		return nil, fail(err)
	}

	return answer(PublicInspection(insp), nil)
}

func (h *Handler) sandboxEgressLog(w http.ResponseWriter, r *http.Request) {
	id, err := h.repo.Resolve(r.PathValue("id"))
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	sb, err := h.repo.Get(id)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	// This handler reads the log around the Service, so it repeats the guard the lifecycle verbs get for free.
	if err := sandbox.FailedGuard(id, sb); err != nil {
		h.writeError(w, r, err)

		return
	}

	follow, err := boolQuery(r, "follow")
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	if follow {
		h.followEgressLog(w, r, sb)

		return
	}

	records, cut, err := h.egressLog.Read(sb)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	if cut > 0 {
		w.Header().Set(EgressCutHeader, strconv.Itoa(cut))
	}
	h.writeJSON(w, http.StatusOK, listOf(records))
}

// describeEgressLog names the three answers of sandboxEgressLog: the records, a line each with follow, or a message each over a WebSocket.
func describeEgressLog(registry huma.Registry, op *huma.Operation) {
	op.Responses["200"] = &huma.Response{
		Description: "The egress decisions, oldest first; with follow one record per line until the sandbox stops.",
		Headers:     map[string]*huma.Header{EgressCutHeader: {Description: "The older records the read left out; absent when it left out none.", Schema: &huma.Schema{Type: huma.TypeInteger}}},
		Content: map[string]*huma.MediaType{
			"application/json":     {Schema: schemaOf[[]egress.Record](registry)},
			"application/x-ndjson": {Schema: schemaOf[egress.Record](registry)},
		},
	}
	op.Responses["101"] = upgrade("A WebSocket follow: one egress record per text message, until the sandbox stops.")
}

type grantInput struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}

func (h *Handler) grantSecret(ctx context.Context, in *grantInput) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.GrantSecret(ctx, in.ID, in.Name))
}

func (h *Handler) ungrantSecret(ctx context.Context, in *grantInput) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.UngrantSecret(ctx, in.ID, in.Name))
}

func (h *Handler) attachPolicy(ctx context.Context, in *sandboxBody[sandbox.PolicyAttachRequest]) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.AttachPolicy(ctx, in.ID, value(in.Body).Policy))
}

func (h *Handler) detachPolicy(ctx context.Context, in *sandboxPath) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.DetachPolicy(ctx, in.ID))
}

// publicReply answers the record a verb returns, less its host side.
func publicReply(sb models.Sandbox, err error) (*reply[Sandbox], error) {
	if err != nil {
		return nil, fail(err)
	}

	return &reply[Sandbox]{Body: PublicSandbox(sb)}, nil
}

type createInput struct {
	Wait bool `query:"wait" doc:"Answer once the sandbox leaves pending; with Accept: application/x-ndjson the pull streams first."`
	Body *sandbox.CreateRequest
}

// createSandbox checks the scopes before anything is created, then answers in one of the two shapes describeCreate names.
func (h *Handler) createSandbox(ctx context.Context, in *createInput) (*rawReply, error) {
	req := value(in.Body)
	if err := checkCreateScopes(ctx, req); err != nil {
		return nil, fail(err)
	}

	return &rawReply{Body: func(hctx huma.Context) {
		r, w := humago.Unwrap(hctx)
		h.create(w, r, in.Wait, req)
	}}, nil
}

// create answers the new record at once, or with wait once it leaves pending, streaming the pull when asked.
func (h *Handler) create(w http.ResponseWriter, r *http.Request, wait bool, req sandbox.CreateRequest) {
	if wait && streamed(r) {
		streamProgress(h, w, r, http.StatusCreated, "create", createLines, func(ctx context.Context) (CreateLine, error) {
			sb, err := h.lifecycle.CreateAndWait(ctx, req)
			if err != nil {
				return CreateLine{}, err
			}
			out := PublicSandbox(sb)

			return CreateLine{Sandbox: &out}, nil
		})

		return
	}

	create := h.lifecycle.Create
	if wait {
		create = h.lifecycle.CreateAndWait
	}

	sb, err := create(r.Context(), req)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, http.StatusCreated, PublicSandbox(sb))
}

// describeCreate names both shapes of the 201: the record, or the pull's events and then the record, one JSON line each.
func describeCreate(registry huma.Registry, op *huma.Operation) {
	op.Responses["201"] = &huma.Response{Description: "The sandbox, or with wait and Accept: application/x-ndjson one CreateLine per pull event and then the sandbox.", Content: map[string]*huma.MediaType{
		"application/json":     {Schema: schemaOf[Sandbox](registry)},
		"application/x-ndjson": {Schema: schemaOf[CreateLine](registry)},
	}}
}

// ScopesHeader carries the token's scopes from the TCP front to the daemon. The front stamps it on every request it forwards and strips any client copy; a request with no such header reached the socket directly.
const ScopesHeader = "X-Shard-Scopes"

// scopeError is a create that names a secret or a policy the token's scopes do not reach; classify maps it to 403.
type scopeError struct {
	scope Scope
	named string
}

func (e *scopeError) Error() string {
	return fmt.Sprintf("the token does not carry the %q scope, which a create that names a %s needs", e.scope, e.named)
}

func (e *scopeError) Public() string { return e.Error() }

// checkCreateScopes refuses a create that names a secret or a policy the stamped scopes do not reach. No header means the request reached the daemon socket directly, which keeps every right.
func checkCreateScopes(ctx context.Context, req sandbox.CreateRequest) error {
	scopes, stamped := ctx.Value(scopesKey{}).([]string)
	if !stamped {
		return nil
	}

	if len(req.Secrets) > 0 && !scopesCover(scopes, Secret) {
		return &scopeError{scope: Secret, named: "secret"}
	}
	if req.Policy != "" && !scopesCover(scopes, Policy) {
		return &scopeError{scope: Policy, named: "policy"}
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
func scopesCover(scopes []string, need Scope) bool {
	if len(scopes) == 0 {
		return true
	}

	for _, s := range scopes {
		if s == models.ScopeAll || s == string(need) {
			return true
		}
	}

	return false
}

func (h *Handler) startSandbox(ctx context.Context, in *sandboxPath) (*reply[Sandbox], error) {
	sb, err := h.lifecycle.Start(ctx, in.ID)
	// A start the substrate broke, not one it refused, is named in the daemon log beside the client's answer (SHARD-416).
	if status, _ := classify(err); err != nil && status >= http.StatusInternalServerError {
		h.log.Printf("api: start sandbox %s: %v", in.ID, err)
	}

	return publicReply(sb, err)
}

// stopRequest is the body of a stop, which carries nothing.
type stopRequest struct{}

func (h *Handler) stopSandbox(ctx context.Context, in *sandboxBody[stopRequest]) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.Stop(ctx, in.ID))
}

type removeInput struct {
	ID    string `path:"id"`
	Force bool   `query:"force" doc:"Stop a sandbox that is still up or paused first."`
}

func (h *Handler) removeSandbox(ctx context.Context, in *removeInput) (*struct{}, error) {
	return done(h.lifecycle.Remove(ctx, in.ID, in.Force))
}

func (h *Handler) pauseSandbox(ctx context.Context, in *sandboxPath) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.Pause(ctx, in.ID))
}

func (h *Handler) resumeSandbox(ctx context.Context, in *sandboxPath) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.Resume(ctx, in.ID))
}

func (h *Handler) forkSandbox(ctx context.Context, in *sandboxBody[sandbox.CopyRequest]) (*reply[Sandbox], error) {
	return publicReply(h.lifecycle.Fork(ctx, in.ID, value(in.Body)))
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
	var notStarted *models.CommandNotStartedError
	var fileInvalid *sandbox.FileInvalidError

	switch {
	case errors.As(err, &scope):
		return http.StatusForbidden, models.CodeForbidden
	case errors.As(err, &tooLarge):
		return http.StatusRequestEntityTooLarge, models.CodeBodyTooLarge
	case errors.As(err, &invalid), errors.As(err, &request), errors.As(err, &fileInvalid), errors.Is(err, image.ErrBadReference):
		return http.StatusBadRequest, models.CodeInvalidRequest
	case errors.Is(err, sandboxstate.ErrNotFound), errors.Is(err, sandboxstate.ErrSnapshotNotFound), errors.Is(err, egress.ErrNotFound),
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
	case errors.As(err, &notStarted):
		return http.StatusUnprocessableEntity, models.CodeCommandNotStarted
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

// partial turns a list's joined error into one warning per record of type U it could not read, each through warn; anything else failed the list itself.
func partial[U error](err error, warn func(error) string) ([]string, error) {
	if err == nil {
		return nil, nil
	}

	errs := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs = joined.Unwrap()
	}

	warnings := make([]string, 0, len(errs))
	for _, e := range errs {
		if _, ok := errors.AsType[U](e); !ok {
			return nil, err
		}
		warnings = append(warnings, warn(e))
	}

	return warnings, nil
}

// warning is a public list's line for a record it could not read: what its type made public, with the cause in the log.
func (h *Handler) warning(err error) string {
	public := publicText(models.CodeInternal, err)
	if public != err.Error() {
		h.logCause("a list warning", err)
	}

	return public
}

// writeError answers err with the status and the code its type says, and the holders when a store entry is held.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	body := h.errorBody(r, err)
	h.writeJSON(w, body.status, body)
}

// errorBody is what err answers on r's route, with the cause in the log; a stream that already sent its status writes only the object.
func (h *Handler) errorBody(r *http.Request, err error) *apiError {
	body := refusal(err, isLocal(r))
	h.logCause(r.Method+" "+r.URL.Path, body.cause)

	return body
}

// refusal is the status and the object err answers: the whole of it on a local route, else only what its type made public, and the cause it left out.
func refusal(err error, local bool) *apiError {
	status, code := classify(err)
	message := err.Error()
	if !local {
		message = publicText(code, err)
	}

	body := &apiError{status: status, Object: ErrorObject{Code: code, Message: message}}
	if message != err.Error() {
		body.cause = err
	}

	var held *sandbox.HeldError
	if errors.As(err, &held) {
		body.Object.Holders = held.Users
	}

	var notStarted *models.CommandNotStartedError
	if errors.As(err, &notStarted) {
		body.Object.ExitCode = notStarted.Code
	}

	return body
}

// internalText is what a public route answers for a failure no error type made public.
const internalText = "the daemon could not complete the request; its log has the cause"

// genericText answers a refusal whose error made nothing public; a code it lacks answers internalText.
var genericText = map[models.Code]string{
	models.CodeInvalidRequest: "the request is not valid",
	models.CodeBodyTooLarge:   "the request body is larger than the route accepts",
	models.CodeNotFound:       "what the request names does not exist",
	models.CodeForbidden:      "the token does not cover this request",
	models.CodeUnsupported:    "the provider does not support this verb",
}

// message is what the caller reads about err: the whole of it on a local route, else only what its type made public, with the cause in the log.
func (h *Handler) message(r *http.Request, code models.Code, err error) string {
	if isLocal(r) {
		return err.Error()
	}

	public := publicText(code, err)
	if public != err.Error() {
		h.logCause(r.Method+" "+r.URL.Path, err)
	}

	return public
}

// publicText is what err's type made public, else the fixed text its code answers.
func publicText(code models.Code, err error) string {
	if public, ok := sandbox.PublicText(err); ok {
		return public
	}
	if public, ok := genericText[code]; ok {
		return public
	}

	return internalText
}

// logCause keeps what a public text left out, less every secret value; a nil cause left nothing out.
func (h *Handler) logCause(what string, cause error) {
	if cause == nil {
		return
	}

	h.log.Printf("api: %s: %s", what, h.redacted(cause.Error()))
}

func (h *Handler) redacted(text string) string {
	if h.redact == nil {
		return text
	}

	return h.redact(text)
}

// writeJSON encodes first, so a value that cannot be encoded never leaves a 200 with half a body.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		h.log.Printf("api: encode the response: %v", err)
		body = fmt.Appendf(nil, `{"error":{"code":%q,"message":%q}}`, models.CodeInternal, internalText)
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is on its way, so the one thing left to do with a client that hung up is to say so.
	if _, err := w.Write(body); err != nil {
		h.log.Printf("api: write the response: %v", err)
	}
}
