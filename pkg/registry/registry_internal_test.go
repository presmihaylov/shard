package registry

import (
	"runtime"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestOpenDefaultsToTheArchitectureOfThisHost(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	if store.platform.String() != want.String() {
		t.Errorf("got platform %s, want %s", store.platform, want)
	}
}
