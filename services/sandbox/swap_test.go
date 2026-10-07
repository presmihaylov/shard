package sandbox_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// A create that names no swap gets the default where the substrate makes one, and none where it cannot (SHARD-787).
func TestCreateWithNoSwapGetsTheProvidersDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		swap bool
		want int64
	}{
		"a provider with swap":    {true, sandbox.DefaultSwapMiB},
		"a provider without swap": {false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			svc, l := newService(t, &recorder{}, models.Sandbox{})
			l.provider.swap = tc.swap

			sb, err := svc.Create(t.Context(), alpine())
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if sb.Resources.SwapMiB != tc.want || l.repo.sb.Resources.SwapMiB != tc.want || l.provider.spec.Resources.SwapMiB != tc.want {
				t.Errorf("create answered swap %d, the record holds %d and the guest ran with %d, want %d", sb.Resources.SwapMiB, l.repo.sb.Resources.SwapMiB, l.provider.spec.Resources.SwapMiB, tc.want)
			}
		})
	}
}

func TestCreateKeepsAnExplicitSwap(t *testing.T) {
	for _, swap := range []int64{0, 512} {
		svc, l := newService(t, &recorder{}, models.Sandbox{})
		l.provider.swap = true
		req := alpine()
		req.Resources.SwapMiB = new(swap)

		sb, err := svc.Create(t.Context(), req)
		if err != nil {
			t.Fatalf("create with swap %d: %v", swap, err)
		}
		if sb.Resources.SwapMiB != swap || l.provider.spec.Resources.SwapMiB != swap {
			t.Errorf("the record holds swap %d and the guest ran with %d, want the request's %d", sb.Resources.SwapMiB, l.provider.spec.Resources.SwapMiB, swap)
		}
	}
}

// A substrate on the host kernel cannot swapon, so a swap is refused by name before anything is pulled, and a 0 still runs.
func TestCreateRefusesASwapTheProviderLacks(t *testing.T) {
	r := &recorder{}
	svc, l := newService(t, r, models.Sandbox{})
	req := alpine()
	req.Resources.SwapMiB = new(int64(512))

	_, err := svc.Create(t.Context(), req)
	if !errors.Is(err, models.ErrUnsupported) || !strings.Contains(err.Error(), "fake") || !strings.Contains(err.Error(), models.VerbSwap) {
		t.Fatalf("create = %v, want ErrUnsupported naming the provider and swap", err)
	}
	if slices.Contains(r.calls, "repo.Create") || slices.Contains(r.calls, "images.Pull") || l.repo.sb.ID != "" {
		t.Errorf("a refused swap reached the store: %v", r.calls)
	}

	req.Resources.SwapMiB = new(int64(0))
	if _, err := svc.Create(t.Context(), req); err != nil {
		t.Errorf("create with swap 0 on a provider without swap: %v", err)
	}
}

// The swap file sits on the disk, so a swap the disk cannot hold is the request's fault and leaves no record.
func TestCreateRefusesASwapTheDiskCannotHold(t *testing.T) {
	for name, res := range map[string]sandbox.ResourceRequest{
		"the default swap on a disk its size": {DiskMiB: sandbox.DefaultSwapMiB},
		"a swap past the disk":                {DiskMiB: 1024, SwapMiB: new(int64(4096))},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			svc, l := newService(t, r, models.Sandbox{})
			l.provider.swap = true
			req := alpine()
			req.Resources = res

			_, err := svc.Create(t.Context(), req)
			var refused *sandbox.RequestError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "set resources.disk_mib above") {
				t.Fatalf("create = %v, want a request error that names the disk bound", err)
			}
			if slices.Contains(r.calls, "repo.Create") || l.repo.sb.ID != "" {
				t.Errorf("a refused swap reached the store: %v", r.calls)
			}
		})
	}
}
