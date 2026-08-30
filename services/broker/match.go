package broker

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/presmihaylov/shard/services/secret"
)

// matcher is a secret's Match compiled per route, so a bad expression fails the request loudly.
type matcher struct {
	path    string
	pathRe  *regexp.Regexp
	methods []string
	query   []secret.Pair
	headers []secret.Pair
}

func newMatcher(m *secret.Match) (*matcher, error) {
	if m == nil {
		return nil, nil //nolint:nilnil // no match means no gate, and matches handles the nil.
	}

	compiled := &matcher{path: m.Path, methods: m.Methods, query: m.Query, headers: m.Headers}
	if expr, ok := strings.CutPrefix(m.Path, "re:"); ok {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, err
		}
		compiled.pathRe = re
	}

	return compiled, nil
}

// matches says whether the request meets every set dimension, as the guest sent it.
func (m *matcher) matches(r *http.Request) bool {
	if m == nil {
		return true
	}
	if !m.pathMatches(r.URL.Path) {
		return false
	}
	if len(m.methods) > 0 && !slices.ContainsFunc(m.methods, func(method string) bool { return strings.EqualFold(method, r.Method) }) {
		return false
	}
	query := r.URL.Query()
	for _, pair := range m.query {
		if !slices.Contains(query[pair.Name], pair.Value) {
			return false
		}
	}
	for _, pair := range m.headers {
		if !slices.Contains(r.Header.Values(pair.Name), pair.Value) {
			return false
		}
	}

	return true
}

func (m *matcher) pathMatches(path string) bool {
	if m.pathRe != nil {
		return m.pathRe.MatchString(path)
	}
	if m.path == "" {
		return true
	}
	if prefix, ok := strings.CutSuffix(m.path, "*"); ok {
		return strings.HasPrefix(path, prefix)
	}

	return path == m.path
}
