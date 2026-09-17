package serve

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/presmihaylov/shard/services/api"
)

// capability is the one coarse right the front checks a token's scopes against for a route.
type capability string

const (
	capDaemonRead    capability = "daemon:read"
	capSandboxRead   capability = "sandbox:read"
	capSandboxWrite  capability = "sandbox:write"
	capSandboxDelete capability = "sandbox:delete"
	capExec          capability = "exec"
	capImage         capability = "image:*"
	capSecret        capability = "secret:*"
	capPolicy        capability = "policy:*"
)

// routeCapabilities maps each daemon route to the capability it needs. Every api.Route has an entry, and the front refuses to start when one does not.
var routeCapabilities = map[string]capability{
	"GET /v0/version":                            capDaemonRead,
	"GET /v0/daemon":                             capDaemonRead,
	"GET /v0/sandboxes":                          capSandboxRead,
	"GET /v0/sandboxes/{id}":                     capSandboxRead,
	"POST /v0/sandboxes":                         capSandboxWrite,
	"POST /v0/sandboxes/{id}/start":              capSandboxWrite,
	"POST /v0/sandboxes/{id}/stop":               capSandboxWrite,
	"DELETE /v0/sandboxes/{id}":                  capSandboxDelete,
	"POST /v0/sandboxes/{id}/pause":              capSandboxWrite,
	"POST /v0/sandboxes/{id}/resume":             capSandboxWrite,
	"POST /v0/sandboxes/{id}/fork":               capSandboxWrite,
	"POST /v0/sandboxes/{id}/clone":              capSandboxWrite,
	"POST /v0/sandboxes/{id}/exec":               capExec,
	"GET /v0/sandboxes/{id}/exec":                capExec,
	"GET /v0/sandboxes/{id}/exec/{exec}":         capExec,
	"POST /v0/sandboxes/{id}/exec/{exec}/kill":   capExec,
	"DELETE /v0/sandboxes/{id}/exec/{exec}":      capExec,
	"POST /v0/sandboxes/{id}/exec/{exec}/resize": capExec,
	"GET /v0/sandboxes/{id}/logs":                capSandboxRead,
	"GET /v0/sandboxes/{id}/egress-log":          capSandboxRead,
	"POST /v0/sandboxes/{id}/secrets/{name}":     capSecret,
	"DELETE /v0/sandboxes/{id}/secrets/{name}":   capSecret,
	"PUT /v0/sandboxes/{id}/policy":              capPolicy,
	"DELETE /v0/sandboxes/{id}/policy":           capPolicy,
	"GET /v0/policies":                           capPolicy,
	"GET /v0/policies/{name}":                    capPolicy,
	"PUT /v0/policies/{name}":                    capPolicy,
	"DELETE /v0/policies/{name}":                 capPolicy,
	"GET /v0/secrets":                            capSecret,
	"PUT /v0/secrets/{name}":                     capSecret,
	"DELETE /v0/secrets/{name}":                  capSecret,
	"GET /v0/images":                             capImage,
	"POST /v0/images/pull":                       capImage,
	"POST /v0/images/prune":                      capImage,
	"DELETE /v0/images/{ref...}":                 capImage,
}

// capabilityOf answers the capability a route needs, and whether the front knows the route at all.
func capabilityOf(r api.Route) (capability, bool) {
	c, ok := routeCapabilities[r.Method+" "+r.Pattern]

	return c, ok
}

// capMux resolves a request to the capability its route needs, over the daemon's own patterns, so the front and the daemon agree on what each request is.
type capMux struct {
	mux *http.ServeMux
}

// capHandler carries a capability so a matched route reports it; the mux never serves one.
type capHandler capability

func (capHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// newCapMux registers every daemon route under its capability, and refuses when a route has none.
func newCapMux() (*capMux, error) {
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		c, ok := capabilityOf(r)
		if !ok {
			return nil, fmt.Errorf("the front has no capability for route %s %s", r.Method, r.Pattern)
		}

		mux.Handle(r.Method+" "+r.Pattern, capHandler(c))
	}

	return &capMux{mux: mux}, nil
}

// capability matches a request to its route and answers the capability it needs; an unknown route or a method no route serves has none, so the front denies it.
func (c *capMux) capability(method string, target *url.URL) (capability, bool) {
	h, _ := c.mux.Handler(&http.Request{Method: method, URL: target})
	matched, ok := h.(capHandler)
	if !ok {
		return "", false
	}

	return capability(matched), true
}

// covers reports whether a token's scopes reach the capability. No scopes, or a "*" scope, reaches every one.
func covers(scopes []string, need capability) bool {
	if len(scopes) == 0 {
		return true
	}

	for _, s := range scopes {
		if s == "*" || s == string(need) {
			return true
		}
	}

	return false
}
