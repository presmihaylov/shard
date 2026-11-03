package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandbox"
)

// resizeEnv records the bounds each resize handed the substrate, and refuses with err.
type resizeEnv struct {
	models.Environment

	got []models.Resources
	err error
}

func (e *resizeEnv) Resize(res models.Resources) error {
	e.got = append(e.got, res)

	return e.err
}

type resizeEnvs struct{ env *resizeEnv }

func (r resizeEnvs) Environment(string) (models.Environment, error) { return r.env, nil }

// resizing wires the service over sb, with an environment that records what reached the substrate.
func resizing(t *testing.T, sb models.Sandbox) (*sandbox.Service, layers, *resizeEnv) {
	t.Helper()

	env := &resizeEnv{}
	svc, l := newService(t, &recorder{}, sb, func(cfg *sandbox.Config) { cfg.Environments = resizeEnvs{env} })

	return svc, l, env
}

func sized(res models.Resources) models.Sandbox {
	sb := stopped()
	sb.Resources = res

	return sb
}

func bounds(memory int64, vcpus int, disk int64) sandbox.ResizeRequest {
	return sandbox.ResizeRequest{Resources: sandbox.ResizeResources{MemoryMiB: &memory, VCPUs: &vcpus, DiskMiB: &disk}}
}

func TestResizeWritesTheBoundsToTheRecordAndTheBundle(t *testing.T) {
	svc, l, b := granted(t, &recorder{}, stopped())
	memory, vcpus := int64(1024), 2

	sb, err := svc.Resize(context.Background(), "web", sandbox.ResizeRequest{Resources: sandbox.ResizeResources{MemoryMiB: &memory, VCPUs: &vcpus}})
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}

	want := models.Resources{MemoryMiB: 1024, VCPUs: 2, DiskMiB: 10240}
	if sb.Resources != want || l.repo.sb.Resources != want {
		t.Errorf("the record holds %+v and the store %+v, want %+v", sb.Resources, l.repo.sb.Resources, want)
	}
	rt, err := b.Runtime()
	if err != nil {
		t.Fatalf("Runtime: %v", err)
	}
	if rt.Resources != want {
		t.Errorf("the next start runs under %+v, want %+v", rt.Resources, want)
	}
}

func TestResizeKeepsEveryBoundItIsNotGiven(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 2048, VCPUs: 4, DiskMiB: 20480}))
	vcpus := 1

	if _, err := svc.Resize(context.Background(), "web", sandbox.ResizeRequest{Resources: sandbox.ResizeResources{VCPUs: &vcpus}}); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	want := models.Resources{MemoryMiB: 2048, VCPUs: 1, DiskMiB: 20480}
	if len(env.got) != 1 || env.got[0] != want {
		t.Errorf("the substrate got %+v, want %+v", env.got, want)
	}
	if l.repo.sb.Resources != want {
		t.Errorf("the store holds %+v, want %+v", l.repo.sb.Resources, want)
	}
}

func TestResizeGrowsTheDiskWhereTheProviderCan(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 512, DiskMiB: 10240}))
	l.provider.diskGrow = true

	if _, err := svc.Resize(context.Background(), "web", bounds(512, 0, 11264)); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	if len(env.got) != 1 || env.got[0].DiskMiB != 11264 || l.repo.sb.Resources.DiskMiB != 11264 {
		t.Errorf("the substrate got %+v and the store holds %+v, want an 11264 MiB disk", env.got, l.repo.sb.Resources)
	}
}

func TestResizeRefusesADiskGrowTheProviderLacks(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 512, DiskMiB: 10240}))

	_, err := svc.Resize(context.Background(), "web", bounds(512, 0, 11264))
	if err == nil || !strings.Contains(err.Error(), models.VerbDiskGrow) {
		t.Fatalf("Resize returned %v, want a refusal that names the disk grow", err)
	}
	if len(env.got) != 0 || l.repo.sb.Resources.DiskMiB != 10240 {
		t.Errorf("a refused grow reached the substrate %+v or the store %+v", env.got, l.repo.sb.Resources)
	}
}

func TestResizeRefusesADiskThatShrinks(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 512, DiskMiB: 20480}))
	l.provider.diskGrow = true

	_, err := svc.Resize(context.Background(), "web", bounds(512, 0, 10240))
	if _, ok := errors.AsType[*sandbox.RequestError](err); !ok || !strings.Contains(err.Error(), "only grows") {
		t.Fatalf("Resize returned %v, want a request error that says a disk only grows", err)
	}
	if len(env.got) != 0 {
		t.Errorf("a shrink reached the substrate: %+v", env.got)
	}
}

// A grow the host disk has no room for is the request's fault, as it is on a create.
func TestResizeTheHostHasNoRoomForIsABadRequest(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 512, DiskMiB: 10240}))
	l.provider.diskGrow = true
	env.err = fmt.Errorf("vz: grow the disk at /var/lib/shard/sandboxes/web/disk.img: %w", &bundle.NoRoomError{Bound: 11264 << 20})

	_, err := svc.Resize(context.Background(), "web", bounds(512, 0, 11264))
	if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
		t.Fatalf("Resize returned %v, want a request error", err)
	}
	if public, _ := sandbox.PublicText(err); strings.Contains(public, "/var/lib/shard") || !strings.Contains(public, "does not fit on the host disk") {
		t.Errorf("public text = %q, want the refusal without the host path", public)
	}
}

func TestResizeRefusesARunningSandbox(t *testing.T) {
	svc, _, env := resizing(t, running())
	memory := int64(1024)

	_, err := svc.Resize(context.Background(), "sandbox1", sandbox.ResizeRequest{Resources: sandbox.ResizeResources{MemoryMiB: &memory}})
	if _, ok := errors.AsType[*sandbox.StateError](err); !ok || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("Resize returned %v, want a state error that names the fix", err)
	}
	if len(env.got) != 0 {
		t.Errorf("a running sandbox's resize reached the substrate: %+v", env.got)
	}
}

func TestResizeRefusesARequestThatNamesNoBound(t *testing.T) {
	svc, _, env := resizing(t, stopped())

	_, err := svc.Resize(context.Background(), "web", sandbox.ResizeRequest{})
	if _, ok := errors.AsType[*sandbox.RequestError](err); !ok {
		t.Fatalf("Resize returned %v, want a request error", err)
	}
	if len(env.got) != 0 {
		t.Errorf("an empty resize reached the substrate: %+v", env.got)
	}
}

func TestResizeHoldsTheBoundsToCreatesRules(t *testing.T) {
	svc, l, env := resizing(t, sized(models.Resources{MemoryMiB: 512, DiskMiB: 10240}))
	l.provider.refuse = errors.New("the fake takes no more than 4096 MiB")
	memory, swap := int64(8192), int64(1024)

	for name, req := range map[string]sandbox.ResizeResources{
		"a negative bound":             {MemoryMiB: new(int64(-1))},
		"a bound the provider refuses": {MemoryMiB: &memory},
		"a swap it withholds":          {SwapMiB: &swap},
	} {
		_, err := svc.Resize(context.Background(), "web", sandbox.ResizeRequest{Resources: req})
		if err == nil {
			t.Errorf("%s: the resize went through", name)
		}
	}
	if len(env.got) != 0 {
		t.Errorf("a refused bound reached the substrate: %+v", env.got)
	}
}

func TestResizeThatChangesNothingTouchesNothing(t *testing.T) {
	svc, _, env := resizing(t, sized(models.Resources{MemoryMiB: 512, VCPUs: 1, DiskMiB: 10240}))

	sb, err := svc.Resize(context.Background(), "web", bounds(512, 1, 10240))
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if len(env.got) != 0 || sb.Resources.MemoryMiB != 512 {
		t.Errorf("a resize to the same bounds reached the substrate %+v, or answered %+v", env.got, sb.Resources)
	}
}

func TestResizeTheSubstrateRefusedLeavesTheRecordAsItWas(t *testing.T) {
	was := models.Resources{MemoryMiB: 512, DiskMiB: 10240}
	svc, l, env := resizing(t, sized(was))
	env.err = errors.New("the disk is busy")

	if _, err := svc.Resize(context.Background(), "web", bounds(1024, 0, 10240)); err == nil {
		t.Fatal("the resize went through over a refusing substrate")
	}
	if l.repo.sb.Resources != was {
		t.Errorf("the store holds %+v after a refused resize, want %+v", l.repo.sb.Resources, was)
	}
}
