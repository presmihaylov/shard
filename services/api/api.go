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
	Start(ctx context.Context, ref string) (models.Sandbox, error)
	Stop(ctx context.Context, ref string, grace time.Duration) (models.Sandbox, error)
	Remove(ctx context.Context, ref string, force bool, grace time.Duration) error
	Pause(ctx context.Context, ref string) (models.Sandbox, error)
	Resume(ctx context.Context, ref string) (models.Sandbox, error)
	Fork(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error)
	Clone(ctx context.Context, ref string, req sandbox.CopyRequest) (models.Sandbox, error)
	Exec(ctx context.Context, ref string, req sandbox.ExecRequest, streams sandbox.Streams) (models.ExitStatus, error)
	ResizeExec(ctx context.Context, ref, execID string, size sandbox.TerminalSize) error
	Logs(ctx context.Context, ref string, follow bool, w io.Writer) error
}

// Handler answers the routes over one repository, the rules the host enforces, and the one orchestrator.
type Handler struct {
	version   string
	repo      sandbox.Reader
	enforcer  sandbox.Enforcer
	lifecycle Lifecycle
	stores    Stores
	log       *log.Logger
}

// NewHandler builds the mux; out takes the one thing a handler cannot return, a write the client hung up on.
func NewHandler(version string, repo sandbox.Reader, enforcer sandbox.Enforcer, lifecycle Lifecycle, stores Stores, out io.Writer) http.Handler {
	h := &Handler{
		version:   version,
		repo:      repo,
		enforcer:  enforcer,
		lifecycle: lifecycle,
		stores:    stores,
		log:       log.New(out, "", log.LstdFlags),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/version", h.getVersion)
	mux.HandleFunc("GET /v0/sandboxes", h.listSandboxes)
	mux.HandleFunc("GET /v0/sandboxes/{id}", h.getSandbox)
	mux.HandleFunc("POST /v0/sandboxes", h.createSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/start", h.startSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/stop", h.stopSandbox)
	mux.HandleFunc("DELETE /v0/sandboxes/{id}", h.removeSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/pause", h.pauseSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/resume", h.resumeSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/fork", h.forkSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/clone", h.cloneSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/exec", h.execSandbox)
	mux.HandleFunc("POST /v0/sandboxes/{id}/exec/{exec}/resize", h.resizeExec)
	mux.HandleFunc("GET /v0/sandboxes/{id}/logs", h.sandboxLogs)
	mux.HandleFunc("GET /v0/policies", h.listPolicies)
	mux.HandleFunc("GET /v0/policies/{name}", h.getPolicy)
	mux.HandleFunc("PUT /v0/policies/{name}", h.putPolicy)
	mux.HandleFunc("DELETE /v0/policies/{name}", h.removePolicy)
	mux.HandleFunc("GET /v0/secrets", h.listSecrets)
	mux.HandleFunc("PUT /v0/secrets/{name}", h.putSecret)
	mux.HandleFunc("DELETE /v0/secrets/{name}", h.removeSecret)
	mux.HandleFunc("GET /v0/images", h.listImages)
	mux.HandleFunc("POST /v0/images/pull", h.pullImage)
	mux.HandleFunc("POST /v0/images/prune", h.pruneImages)
	// An image reference carries slashes, so it is the rest of the path and not one segment of it.
	mux.HandleFunc("DELETE /v0/images/{ref...}", h.removeImage)
	// The mux answers an unknown path in plain text; every error body on this socket is JSON.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError(w, http.StatusNotFound, fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path))
	})

	return mux
}

type versionResponse struct {
	Version string `json:"version"`
}

type listResponse struct {
	Sandboxes []models.Sandbox `json:"sandboxes"`
	// Warnings names the records the daemon could not read, one string each, beside the ones it could.
	Warnings []string `json:"warnings,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *Handler) getVersion(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, versionResponse{Version: h.version})
}

func (h *Handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	all, err := boolQuery(r, "all")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	sandboxes, unreadable := sandbox.List(h.repo, all)

	warnings, err := partial(unreadable)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, listResponse{Sandboxes: sandboxes, Warnings: warnings})
}

func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := sandbox.Inspect(h.repo, h.enforcer, r.PathValue("id"))
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req sandbox.CreateRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	sb, err := h.lifecycle.Create(r.Context(), req)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusCreated, sb)
}

func (h *Handler) startSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Start(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

// stopRequest is the body of a stop. Grace is in seconds; absent, the entrypoint gets the default.
type stopRequest struct {
	Grace *float64 `json:"grace,omitempty"`
}

func (h *Handler) stopSandbox(w http.ResponseWriter, r *http.Request) {
	var req stopRequest
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	grace, err := graceOf(req.Grace)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	sb, err := h.lifecycle.Stop(r.Context(), r.PathValue("id"), grace)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) removeSandbox(w http.ResponseWriter, r *http.Request) {
	force, err := boolQuery(r, "force")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	grace, err := graceQuery(r)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	if err := h.lifecycle.Remove(r.Context(), r.PathValue("id"), force, grace); err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Pause(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusOK, sb)
}

func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := h.lifecycle.Resume(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, status(err), err.Error())

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
	if err := decode(r, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	sb, err := verb(r.Context(), r.PathValue("id"), req)
	if err != nil {
		h.writeError(w, status(err), err.Error())

		return
	}

	h.writeJSON(w, http.StatusCreated, sb)
}

// status maps what the orchestrator refused to the one code that says so; anything else broke.
func status(err error) int {
	var invalid *sandboxstate.ValidationError
	var request *sandbox.RequestError
	var state *sandbox.StateError
	var unavailable *sandbox.UnavailableError
	var held *sandbox.HeldError

	switch {
	case errors.As(err, &invalid), errors.As(err, &request):
		return http.StatusBadRequest
	case errors.Is(err, sandboxstate.ErrNotFound), errors.Is(err, egress.ErrNotFound),
		errors.Is(err, secret.ErrNotFound), errors.Is(err, image.ErrNotFound):
		return http.StatusNotFound
	case errors.As(err, &state), errors.As(err, &unavailable), errors.As(err, &held),
		errors.Is(err, models.ErrUnsupported):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// decode reads a JSON body into out. An empty body is the zero value; a field no route knows is refused.
func decode(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(out)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode the request body: %w", err)
	}

	return nil
}

// graceQuery reads ?grace= in seconds, for the stop an rm --force does; absent is the default.
func graceQuery(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("grace")
	if raw == "" {
		return sandbox.DefaultStopGrace, nil
	}

	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("the query grace=%q is not a number of seconds", raw)
	}

	return graceOf(&seconds)
}

func graceOf(seconds *float64) (time.Duration, error) {
	if seconds == nil {
		return sandbox.DefaultStopGrace, nil
	}
	if *seconds < 0 {
		return 0, fmt.Errorf("the grace is how long the entrypoint gets and cannot be negative, got %v", *seconds)
	}

	return time.Duration(*seconds * float64(time.Second)), nil
}

// boolQuery reads a flag like ?all=true. An absent flag is false; a value that is not a bool is refused.
func boolQuery(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("the query %s=%q is not a boolean", name, raw)
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

func (h *Handler) writeError(w http.ResponseWriter, status int, message string) {
	h.writeJSON(w, status, errorResponse{Error: message})
}

// writeJSON encodes first, so a value that cannot be encoded never leaves a 200 with half a body.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		body = fmt.Appendf(nil, `{"error":%q}`, "encode the response: "+err.Error())
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is on its way, so the one thing left to do with a client that hung up is to say so.
	if _, err := w.Write(body); err != nil {
		h.log.Printf("api: write the response: %v", err)
	}
}
