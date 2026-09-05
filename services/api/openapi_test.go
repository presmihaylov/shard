package api_test

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/api"
)

// spec is the slice of OpenAPI the tests read: the routes with their statuses, and the record schemas.
type spec struct {
	Paths map[string]map[string]struct {
		Responses map[string]any `yaml:"responses"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]schema `yaml:"schemas"`
	} `yaml:"components"`
}

type schema struct {
	Required   []string       `yaml:"required"`
	Properties map[string]any `yaml:"properties"`
}

func readSpec(t *testing.T) spec {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}

	var doc spec
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the spec: %v", err)
	}

	return doc
}

// The spec is a promise to clients, so it must say exactly what the server registers, no more and no fewer.
func TestTheSpecDeclaresEveryRouteAndEveryStatus(t *testing.T) {
	doc := readSpec(t)

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

// jsonFields reads the json tags of a struct: every name, and the ones without omitempty, which always serialize.
func jsonFields(t *testing.T, typ reflect.Type) (names, always []string) {
	t.Helper()

	for field := range typ.Fields() {
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json tag", typ.Name(), field.Name)
		}
		name, opts, _ := strings.Cut(tag, ",")
		names = append(names, name)
		if !strings.Contains(opts, "omitempty") {
			always = append(always, name)
		}
	}

	return names, always
}

// A field added to the record must land in the spec, so the schemas are held to the structs by reflection.
func TestTheSpecDeclaresEveryFieldOfTheRecord(t *testing.T) {
	doc := readSpec(t)

	for name, typ := range map[string]reflect.Type{
		"Sandbox":    reflect.TypeFor[models.Sandbox](),
		"ExitStatus": reflect.TypeFor[models.ExitStatus](),
		"Resources":  reflect.TypeFor[models.Resources](),
	} {
		declared, ok := doc.Components.Schemas[name]
		if !ok {
			t.Errorf("the spec has no schema %s", name)

			continue
		}

		names, always := jsonFields(t, typ)
		properties := slices.Sorted(maps.Keys(declared.Properties))
		slices.Sort(names)
		if !slices.Equal(properties, names) {
			t.Errorf("%s declares the properties %v, the record has %v", name, properties, names)
		}

		required := slices.Clone(declared.Required)
		slices.Sort(required)
		slices.Sort(always)
		if !slices.Equal(required, always) {
			t.Errorf("%s requires %v, the record always serializes %v", name, required, always)
		}
	}
}
