// Package api answers the REST API the daemon serves on its socket. A handler calls services/ and
// holds no verb logic of its own, so the CLI and the API cannot drift apart.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
	"github.com/presmihaylov/shard/services/sandboxstate"
)

// Version is the API version a client negotiates against, separate from the binary's.
const Version = "0.1.0"

// Records is the part of sandboxstate.Repository the handlers read.
type Records interface {
	Get(id string) (models.Sandbox, error)
	Resolve(ref string) (string, error)
	List() ([]models.Sandbox, error)
}

// Egress is the part of egress.Service the handlers read: what the host enforces for a sandbox.
type Egress interface {
	Effective(sb models.Sandbox) (egress.Effective, error)
}

// Service holds what the handlers drive.
type Service struct {
	// Binary is the version of the daemon, which the CLI compares to its own.
	Binary  string
	Records Records
	Egress  Egress
}

// VersionInfo is the body of GET /v0/version.
type VersionInfo struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

// Sandbox is the record plus what the host enforces for it, which the record only names.
type Sandbox struct {
	models.Sandbox
	Egress *egress.Effective `json:"egress,omitempty"`
}

// Error is the body of every non-2xx answer.
type Error struct {
	Error string `json:"error"`
}

// Routes is every pattern the mux serves, spelled once so the spec test can hold it to the spec.
var Routes = []string{
	"GET /v0/version",
	"GET /v0/sandboxes",
	"GET /v0/sandboxes/{id}",
}

// Handler is the mux over every route.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Routes[0], s.version)
	mux.HandleFunc(Routes[1], s.list)
	mux.HandleFunc(Routes[2], s.get)

	return mux
}

func (s *Service) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, VersionInfo{Version: s.Binary, APIVersion: Version})
}

// list answers what shard ls shows: the sandboxes that are up, and with ?all=true the stopped ones too.
func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	sandboxes, unreadable := s.Records.List()
	if unreadable != nil {
		writeError(w, http.StatusInternalServerError, unreadable)

		return
	}

	if r.URL.Query().Get("all") != "true" {
		sandboxes = slices.DeleteFunc(sandboxes, func(sb models.Sandbox) bool { return sb.State == models.StateStopped })
	}

	writeJSON(w, http.StatusOK, sandboxes)
}

// get answers what shard inspect prints, for an id or a name.
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	id, err := s.Records.Resolve(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)

		return
	}

	sb, err := s.Records.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)

		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)

		return
	}

	out := Sandbox{Sandbox: sb}
	if sb.Policy != "" {
		effective, err := s.Egress.Effective(sb)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)

			return
		}
		out.Egress = &effective
	}

	writeJSON(w, http.StatusOK, out)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, Error{Error: err.Error()})
}

// writeJSON encodes first, so a value that will not encode turns into a 500 and never a torn 200.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "encode the answer: "+err.Error()), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failed write means the client hung up, and there is nobody left to tell.
	w.Write(body) //nolint:errcheck,gosec
}
