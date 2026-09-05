package api_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/presmihaylov/shard/services/api"
)

// spec is the slice of OpenAPI the test reads: the paths, their methods and the statuses each declares.
type spec struct {
	Paths map[string]map[string]struct {
		Responses map[string]any `yaml:"responses"`
	} `yaml:"paths"`
}

// The spec is a promise to clients, so it must say exactly what the server registers: every route,
// and every status a handler can answer with, no more and no fewer.
func TestTheSpecDeclaresEveryRouteAndEveryStatus(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}

	var doc spec
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the spec: %v", err)
	}

	declared := map[string][]int{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			statuses := make([]int, 0, len(op.Responses))
			for code := range op.Responses {
				status, err := strconv.Atoi(code)
				if err != nil {
					t.Fatalf("%s %s declares the response %q, which is not a status", method, path, code)
				}
				statuses = append(statuses, status)
			}
			slices.Sort(statuses)
			declared[strings.ToUpper(method)+" "+path] = statuses
		}
	}

	registered := map[string][]int{}
	for _, route := range api.Routes() {
		statuses := slices.Clone(route.Statuses)
		slices.Sort(statuses)
		registered[route.Method+" "+route.Path] = statuses
	}

	for key, want := range registered {
		got, ok := declared[key]
		if !ok {
			t.Errorf("the server registers %s, which the spec does not declare", key)

			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s declares the statuses %v, the handler answers with %v", key, got, want)
		}
	}
	for key := range declared {
		if _, ok := registered[key]; !ok {
			t.Errorf("the spec declares %s, which the server does not register", key)
		}
	}
}
