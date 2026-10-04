package serve

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

// CheckScopes refuses a scope models.Scopes does not list, because the front would answer 403 to every request the token makes.
func CheckScopes(scopes []string) error {
	for _, s := range scopes {
		if !slices.ContainsFunc(models.Scopes, func(known models.Scope) bool { return known.Name == s }) {
			return fmt.Errorf("unknown scope %q: a scope is one of %s", s, strings.Join(scopeNames(), ", "))
		}
	}

	return nil
}

func scopeNames() []string {
	names := make([]string, 0, len(models.Scopes))
	for _, s := range models.Scopes {
		names = append(names, s.Name)
	}

	return names
}

// capMux resolves a request to the scope its route needs, over the daemon's own patterns, so the front and the daemon agree on what each request is.
type capMux struct {
	mux *http.ServeMux
}

// capHandler carries a scope so a matched route reports it; the mux never serves one.
type capHandler api.Scope

func (capHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// newCapMux registers every public route under its scope, and refuses when one has none. A local route stays out, so the front answers it as it answers an unknown one.
func newCapMux() (*capMux, error) {
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		if r.Class == api.Local {
			continue
		}
		if r.Class != api.Public {
			return nil, fmt.Errorf("route %s %s has no class, so the front cannot tell whether to forward it", r.Method, r.Pattern)
		}

		if r.Scope == "" {
			return nil, fmt.Errorf("route %s %s has no scope, so the front cannot check a token against it", r.Method, r.Pattern)
		}

		mux.Handle(r.Method+" "+r.Pattern, capHandler(r.Scope))
	}

	return &capMux{mux: mux}, nil
}

// scope matches a request to its route and answers the scope it needs; an unknown route or a method no route serves has none, so the front denies it.
func (c *capMux) scope(method string, target *url.URL) (api.Scope, bool) {
	h, _ := c.mux.Handler(&http.Request{Method: method, URL: target})
	matched, ok := h.(capHandler)
	if !ok {
		return "", false
	}

	return api.Scope(matched), true
}

// covers reports whether a token's scopes reach need. No scopes, or a "*" scope, reaches every one.
func covers(scopes []string, need api.Scope) bool {
	if len(scopes) == 0 || need == api.AnyToken {
		return true
	}

	for _, s := range scopes {
		if s == models.ScopeAll || s == string(need) {
			return true
		}
	}

	return false
}
