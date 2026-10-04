package serve

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/services/api"
)

// capability is the one coarse right the front checks a token's scopes against for a route.
type capability string

const (
	// capAnyToken is a route every valid token reaches whatever its scopes, so a client can learn what it speaks to before it acts.
	capAnyToken      capability = ""
	capSandboxRead   capability = "sandbox:read"
	capSandboxWrite  capability = "sandbox:write"
	capSandboxDelete capability = "sandbox:delete"
	capExec          capability = "exec"
	capSecret        capability = "secret:*"
	capPolicy        capability = "policy:*"
)

// capabilities are the six a scope can name besides "*", in the order docs/daemon.md lists them.
var capabilities = []capability{capSandboxRead, capSandboxWrite, capSandboxDelete, capExec, capSecret, capPolicy}

// CheckScopes refuses a scope that is neither "*" nor a capability, because the front would answer 403 to every request the token makes.
func CheckScopes(scopes []string) error {
	for _, s := range scopes {
		if s != "*" && !slices.Contains(capabilities, capability(s)) {
			return fmt.Errorf("unknown scope %q: a scope is * or one of %s", s, strings.Join(capabilityNames(), ", "))
		}
	}

	return nil
}

func capabilityNames() []string {
	names := make([]string, 0, len(capabilities))
	for _, c := range capabilities {
		names = append(names, string(c))
	}

	return names
}

// routeCapabilities maps each public route to the capability it needs. Every public api.Route has an entry, and the front refuses to start when one does not.
var routeCapabilities = map[string]capability{
	"GET /v0/version":                            capAnyToken,
	"GET /v0/capabilities":                       capAnyToken,
	"GET /v0/sandboxes":                          capSandboxRead,
	"GET /v0/sandboxes/{id}":                     capSandboxRead,
	"POST /v0/sandboxes":                         capSandboxWrite,
	"POST /v0/sandboxes/{id}/start":              capSandboxWrite,
	"POST /v0/sandboxes/{id}/stop":               capSandboxWrite,
	"DELETE /v0/sandboxes/{id}":                  capSandboxDelete,
	"POST /v0/sandboxes/{id}/pause":              capSandboxWrite,
	"POST /v0/sandboxes/{id}/resume":             capSandboxWrite,
	"POST /v0/sandboxes/{id}/fork":               capSandboxWrite,
	"POST /v0/sandboxes/{id}/exec":               capExec,
	"GET /v0/sandboxes/{id}/exec":                capExec,
	"GET /v0/sandboxes/{id}/exec/{exec}":         capExec,
	"POST /v0/sandboxes/{id}/exec/{exec}/kill":   capExec,
	"DELETE /v0/sandboxes/{id}/exec/{exec}":      capExec,
	"POST /v0/sandboxes/{id}/exec/{exec}/resize": capExec,
	"PUT /v0/sandboxes/{id}/files":               capExec,
	"GET /v0/sandboxes/{id}/files":               capExec,
	"HEAD /v0/sandboxes/{id}/files":              capExec,
	"DELETE /v0/sandboxes/{id}/files":            capExec,
	"GET /v0/sandboxes/{id}/ls":                  capExec,
	"POST /v0/sandboxes/{id}/mkdir":              capExec,
	"PUT /v0/sandboxes/{id}/archive":             capExec,
	"GET /v0/sandboxes/{id}/archive":             capExec,
	"GET /v0/sandboxes/{id}/logs":                capSandboxRead,
	"GET /v0/sandboxes/{id}/egress-log":          capSandboxRead,
	"GET /v0/sandboxes/{id}/attach":              capSandboxRead,
	"POST /v0/sandboxes/{id}/app/stop":           capSandboxWrite,
	"POST /v0/sandboxes/{id}/secrets/{name}":     capSecret,
	"DELETE /v0/sandboxes/{id}/secrets/{name}":   capSecret,
	"PUT /v0/sandboxes/{id}/policy":              capPolicy,
	"DELETE /v0/sandboxes/{id}/policy":           capPolicy,
	"POST /v0/snapshots":                         capSandboxWrite,
	"GET /v0/snapshots":                          capSandboxRead,
	"GET /v0/snapshots/{ref}":                    capSandboxRead,
	"DELETE /v0/snapshots/{ref}":                 capSandboxDelete,
	"GET /v0/policies":                           capPolicy,
	"GET /v0/policies/{name}":                    capPolicy,
	"PUT /v0/policies/{name}":                    capPolicy,
	"DELETE /v0/policies/{name}":                 capPolicy,
	"GET /v0/secrets":                            capSecret,
	"PUT /v0/secrets/{name}":                     capSecret,
	"DELETE /v0/secrets/{name}":                  capSecret,
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

// newCapMux registers every public route under its capability, and refuses when one has none. A local route stays out, so the front answers it as it answers an unknown one.
func newCapMux() (*capMux, error) {
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		if r.Class == api.Local {
			continue
		}
		if r.Class != api.Public {
			return nil, fmt.Errorf("route %s %s has no class, so the front cannot tell whether to forward it", r.Method, r.Pattern)
		}

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
	if len(scopes) == 0 || need == capAnyToken {
		return true
	}

	for _, s := range scopes {
		if s == "*" || s == string(need) {
			return true
		}
	}

	return false
}
