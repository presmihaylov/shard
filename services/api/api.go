// Package api is the REST surface the daemon serves over its unix socket; the rules live in services/.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// Handler answers the routes over one repository and the egress rules the host enforces over it.
type Handler struct {
	version  string
	repo     sandbox.Reader
	enforcer sandbox.Enforcer
	log      *log.Logger
}

// NewHandler builds the mux; out takes the one thing a handler cannot return, a write the client hung up on.
func NewHandler(version string, repo sandbox.Reader, enforcer sandbox.Enforcer, out io.Writer) http.Handler {
	h := &Handler{version: version, repo: repo, enforcer: enforcer, log: log.New(out, "", log.LstdFlags)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/version", h.getVersion)
	mux.HandleFunc("GET /v0/sandboxes", h.listSandboxes)
	mux.HandleFunc("GET /v0/sandboxes/{id}", h.getSandbox)
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

	var invalid *sandboxstate.ValidationError

	switch {
	case err == nil:
		h.writeJSON(w, http.StatusOK, sb)
	case errors.As(err, &invalid):
		h.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sandboxstate.ErrNotFound):
		h.writeError(w, http.StatusNotFound, err.Error())
	default:
		h.writeError(w, http.StatusInternalServerError, err.Error())
	}
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
