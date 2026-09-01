package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/daemon"
)

func TestServeTakesNoArguments(t *testing.T) {
	err := App{Out: io.Discard}.Run(t.Context(), []string{"serve", "extra"})
	if err == nil || !strings.Contains(err.Error(), "serve takes no arguments") {
		t.Errorf("serve with an argument got %v", err)
	}
}

// serve needs no provider and no repository, so it runs anywhere the stores can live.
func TestServeHoldsTheRootUntilTheContextEnds(t *testing.T) {
	root := t.TempDir()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- App{Out: io.Discard}.Run(ctx, []string{"--root", root, "serve"}) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		alive, err := daemon.Alive(root)
		if err != nil {
			t.Fatalf("Alive: %v", err)
		}
		if alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never took the lock")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve ended with %v", err)
	}
}
