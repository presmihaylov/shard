package api_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/api"
)

// A client generates from the committed file, so a route change that skips make openapi fails here and not in an SDK.
func TestTheCommittedSpecIsTheOneTheRoutesMake(t *testing.T) {
	spec, err := api.Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}

	committed, err := os.ReadFile("../../docs/openapi.json")
	if err != nil {
		t.Fatalf("read docs/openapi.json: %v", err)
	}

	if string(spec) != string(committed) {
		t.Errorf("docs/openapi.json differs from the routes; run make openapi")
	}
}

// The front checks a token against api.Routes, so the spec names each public route once, with the same scope, and no local one.
func TestTheSpecNamesEveryPublicRouteWithItsScope(t *testing.T) {
	spec, err := api.Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}

	var doc struct {
		Paths map[string]map[string]struct {
			Scope string `json:"x-shard-scope"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatalf("decode the spec: %v", err)
	}

	var got []string
	for path, ops := range doc.Paths {
		for method, op := range ops {
			got = append(got, strings.ToUpper(method)+" "+path+" "+op.Scope)
		}
	}

	var want []string
	for _, r := range api.Routes() {
		if r.Class == api.Public {
			want = append(want, r.Method+" "+r.Pattern+" "+string(r.Scope))
		}
	}

	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the spec names\n%s\nwant the public routes\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
