package runspec_test

import (
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/runspec"
)

func TestResolveTakesTheImageEntrypointAndCmdTogether(t *testing.T) {
	got := runspec.Resolve(models.SandboxSpec{}, models.ImageConfig{
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "true"},
	})

	if want := []string{"/bin/sh", "-c", "true"}; !slices.Equal(got.Entrypoint, want) {
		t.Errorf("got entrypoint %v, want %v", got.Entrypoint, want)
	}
}

// The spec's entrypoint replaces both halves, because argv after -- is the whole command.
func TestResolvePrefersTheSpecEntrypointOverBoth(t *testing.T) {
	spec := models.SandboxSpec{Entrypoint: []string{"/bin/echo", "hello"}}
	got := runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "true"}})

	if want := []string{"/bin/echo", "hello"}; !slices.Equal(got.Entrypoint, want) {
		t.Errorf("got entrypoint %v, want %v", got.Entrypoint, want)
	}
}

func TestResolveKeepsTheImageEnvironmentOrderAndAppliesOverrides(t *testing.T) {
	spec := models.SandboxSpec{Env: []string{"LANG=C.UTF-8", "APP=shard"}}
	got := runspec.Resolve(spec, models.ImageConfig{Env: []string{"PATH=/usr/bin", "LANG=en_US", "TZ=UTC"}})

	want := []string{"PATH=/usr/bin", "LANG=C.UTF-8", "TZ=UTC", "APP=shard"}
	if !slices.Equal(got.Env, want) {
		t.Errorf("got env %v, want %v", got.Env, want)
	}
}

// A spec that sets one key twice must resolve to one entry, whether or not the image sets it.
func TestResolveKeepsTheLastValueOfARepeatedKey(t *testing.T) {
	spec := models.SandboxSpec{Env: []string{"A=1", "A=2", "B=1", "B=2"}}
	got := runspec.Resolve(spec, models.ImageConfig{Env: []string{"A=image"}})

	if want := []string{"A=2", "B=2"}; !slices.Equal(got.Env, want) {
		t.Errorf("got env %v, want %v", got.Env, want)
	}
}

// An entry with no "=" is not an assignment, so no runtime would accept it.
func TestResolveDropsAnEntryThatIsNotAnAssignment(t *testing.T) {
	spec := models.SandboxSpec{Env: []string{"BROKEN"}}
	got := runspec.Resolve(spec, models.ImageConfig{Env: []string{"ALSO_BROKEN", "TZ=UTC"}})

	if want := []string{"TZ=UTC"}; !slices.Equal(got.Env, want) {
		t.Errorf("got env %v, want %v", got.Env, want)
	}
}

func TestResolveFallsBackToTheImageWorkDirAndUser(t *testing.T) {
	cfg := models.ImageConfig{WorkDir: "/app", User: "app"}

	got := runspec.Resolve(models.SandboxSpec{}, cfg)
	if got.WorkDir != "/app" || got.User != "app" {
		t.Errorf("got workdir %q user %q, want /app and app", got.WorkDir, got.User)
	}

	got = runspec.Resolve(models.SandboxSpec{WorkDir: "/srv", User: "root"}, cfg)
	if got.WorkDir != "/srv" || got.User != "root" {
		t.Errorf("got workdir %q user %q, want /srv and root", got.WorkDir, got.User)
	}
}

// The name is the guest hostname, so every sandbox has one and a later process rebuilds the same spec.
func TestResolveNamesAnUnnamedSandboxAfterItsID(t *testing.T) {
	got := runspec.Resolve(models.SandboxSpec{ID: "amber-otter-1a2b"}, models.ImageConfig{})
	if got.Name != "amber-otter-1a2b" {
		t.Errorf("got name %q, want the id", got.Name)
	}

	got = runspec.Resolve(models.SandboxSpec{ID: "amber-otter-1a2b", Name: "web"}, models.ImageConfig{})
	if got.Name != "web" {
		t.Errorf("got name %q, want web", got.Name)
	}
}

// Resolve is pure, so a caller may resolve the same spec against a second image.
func TestResolveLeavesTheGivenSpecAlone(t *testing.T) {
	spec := models.SandboxSpec{ID: "amber-otter-1a2b"}

	runspec.Resolve(spec, models.ImageConfig{Entrypoint: []string{"/bin/sh"}})

	if len(spec.Entrypoint) != 0 || spec.Name != "" {
		t.Errorf("Resolve changed the spec it was given: %+v", spec)
	}
}
