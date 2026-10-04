package api_test

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
	"github.com/presmihaylov/shard/services/image"
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

// The SDK release gate probes each local route through the front, so its list is the table's, in order.
func TestTheGateProbesEveryLocalRoute(t *testing.T) {
	listed, err := os.ReadFile("../../sdks/gate/local-routes.txt")
	if err != nil {
		t.Fatalf("read sdks/gate/local-routes.txt: %v", err)
	}

	var want []string
	for _, r := range api.Routes() {
		if r.Class == api.Local {
			want = append(want, r.Method+" "+r.Pattern)
		}
	}

	got := strings.Split(strings.TrimSuffix(string(listed), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("sdks/gate/local-routes.txt lists %q, the table's local routes are %q", got, want)
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

// The gate rejects each key an internal record holds and its public form leaves out, so its list is the one the types make.
func TestTheGateKnowsEveryHostField(t *testing.T) {
	listed, err := os.ReadFile("../../sdks/gate/host-fields.txt")
	if err != nil {
		t.Fatalf("read sdks/gate/host-fields.txt: %v", err)
	}

	var want []string
	for _, pair := range [][2]any{{models.Sandbox{}, api.Sandbox{}}, {image.Event{}, api.Event{}}} {
		public := jsonKeys(reflect.TypeOf(pair[1]))
		for _, key := range jsonKeys(reflect.TypeOf(pair[0])) {
			if !slices.Contains(public, key) {
				want = append(want, key)
			}
		}
	}

	got := strings.Split(strings.TrimSuffix(string(listed), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("sdks/gate/host-fields.txt lists %q, the internal records hold %q beyond their public form", got, want)
	}
}

func jsonKeys(typ reflect.Type) []string {
	var keys []string
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		keys = append(keys, name)
	}

	return keys
}
