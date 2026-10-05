package sandbox_test

import (
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// kernelProvider is a VM substrate, which names the guest kernel a fresh boot runs.
type kernelProvider struct {
	models.Provider

	tag string
}

func (k *kernelProvider) GuestKernel() string { return k.tag }

func withKernel(tag string) func(*sandbox.Config) {
	return func(c *sandbox.Config) {
		c.Provider = &kernelProvider{Provider: c.Provider, tag: tag}
	}
}

// A create on a VM substrate records the kernel its guest booted, which a get then reports (SHARD-745).
func TestCreateRecordsTheKernelTheGuestBooted(t *testing.T) {
	svc, _ := newService(t, &recorder{}, models.Sandbox{}, withKernel("kernel-6.12.110-3"))

	sb, err := svc.Create(t.Context(), alpine())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.Kernel != "kernel-6.12.110-3" {
		t.Errorf("create recorded the kernel %q, want kernel-6.12.110-3", sb.Kernel)
	}
}

// A container substrate boots no kernel of shard's, so its record names none (SHARD-745).
func TestCreateOnAContainerSubstrateRecordsNoKernel(t *testing.T) {
	svc, _ := newService(t, &recorder{}, models.Sandbox{})

	sb, err := svc.Create(t.Context(), alpine())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.Kernel != "" {
		t.Errorf("create recorded the kernel %q on a container substrate, want none", sb.Kernel)
	}
}

// A start after a stop boots the guest again, so the record names this boot's kernel, not the last one's (SHARD-745).
func TestStartRecordsTheKernelItBoots(t *testing.T) {
	old := stopped()
	old.Kernel = "kernel-6.12.110-2"
	svc, _ := newService(t, &recorder{}, old, withKernel("kernel-6.12.110-3"))

	sb, err := svc.Start(t.Context(), "web")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sb.Kernel != "kernel-6.12.110-3" {
		t.Errorf("start recorded the kernel %q, want kernel-6.12.110-3", sb.Kernel)
	}
}

// A resume runs the memory image of the kernel the pause held, so the record keeps its tag (SHARD-745).
func TestResumeKeepsTheKernelThePauseHeld(t *testing.T) {
	paused := pausedSandbox()
	paused.Kernel = "kernel-6.12.110-2"
	svc, l := newService(t, &recorder{}, paused, withKernel("kernel-6.12.110-3"))
	l.provider.status = models.Status{}

	sb, err := svc.Resume(t.Context(), "web")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sb.Kernel != "kernel-6.12.110-2" {
		t.Errorf("resume recorded the kernel %q, want the paused kernel-6.12.110-2", sb.Kernel)
	}
}
