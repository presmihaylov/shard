package runc

import (
	"errors"
	"os"
	"testing"

	"github.com/presmihaylov/shard/pkg/launch"
)

func TestExecHandlesRefuseAnotherSandboxAndNeverRepeat(t *testing.T) {
	r := &Runner{}
	first, err := r.trackExec("sandbox-a", &launch.Channel{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Signal(t.Context(), "sandbox-b", first, "TERM"); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("another sandbox's handle returned %v, want an ended exec", err)
	}
	r.forgetExec(first)
	second, err := r.trackExec("sandbox-a", &launch.Channel{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.forgetExec(second) })
	if first == second {
		t.Fatal("a new exec reused the retired handle")
	}
	if err := r.Signal(t.Context(), "sandbox-a", first, "KILL"); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("a retired handle returned %v, want an ended exec", err)
	}
}
