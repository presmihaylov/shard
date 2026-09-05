package cli

import (
	"context"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/pkg/store"
	"github.com/presmihaylov/shard/services/daemon"
	"github.com/presmihaylov/shard/services/network"
)

func TestDaemonTakesNoArguments(t *testing.T) {
	err := App{Out: io.Discard}.Run(t.Context(), []string{"daemon", "extra"})
	if err == nil || !strings.Contains(err.Error(), "daemon takes no arguments") {
		t.Errorf("daemon with an argument got %v", err)
	}
}

// daemon needs no provider and no repository, so it runs anywhere the stores can live.
func TestDaemonHoldsTheRootUntilTheContextEnds(t *testing.T) {
	root := t.TempDir()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- App{Out: io.Discard}.Run(ctx, []string{"--root", root, "daemon"}) }()

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
			t.Fatal("daemon never took the lock")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon ended with %v", err)
	}
}

// While a one-shot proxy holds the lock the task reports it, and the daemon's retry is the takeover.
func TestProxyTaskReportsAForeignHolder(t *testing.T) {
	root := t.TempDir()

	lock, err := store.TryAcquire(filepath.Join(root, proxyDir, "lock"), proxyLockPerm)
	if err != nil || lock == nil {
		t.Fatalf("hold the proxy lock first: %v, %v", lock, err)
	}
	defer lock.Release() //nolint:errcheck

	task := proxyTask{app: App{Root: root, Out: io.Discard}, gateway: netip.MustParseAddr("127.0.0.1")}
	err = task.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "a proxy already runs") {
		t.Errorf("the task got %v, want the foreign-holder refusal", err)
	}
}

// With the lock free the task is the proxy: it serves until the context ends and that is not a failure.
func TestProxyTaskServesUntilTheContextEnds(t *testing.T) {
	root := t.TempDir()
	task := proxyTask{
		app:     App{Root: root, Out: io.Discard},
		gateway: netip.MustParseAddr("127.0.0.1"),
		ports:   network.ProxyPorts{HTTP: 0, HTTPS: 0},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- task.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, proxyDir, "pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task never became the proxy")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the task ended with %v", err)
	}
}
