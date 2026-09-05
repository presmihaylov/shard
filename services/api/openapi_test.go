package api

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spec is the contract the CLI client is generated from, so it must name exactly what the mux serves.
func TestSpecListsWhatTheMuxServes(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}

	var spec struct {
		Info  struct{ Version string }
		Paths map[string]map[string]any
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse the spec: %v", err)
	}
	if spec.Info.Version != Version {
		t.Errorf("the spec is version %q, the code is %q", spec.Info.Version, Version)
	}

	var declared []string
	for path, operations := range spec.Paths {
		for method := range operations {
			declared = append(declared, strings.ToUpper(method)+" "+path)
		}
	}
	slices.Sort(declared)

	served := slices.Sorted(slices.Values(Routes))
	if !slices.Equal(declared, served) {
		t.Errorf("the spec declares %v, the mux serves %v", declared, served)
	}
}
