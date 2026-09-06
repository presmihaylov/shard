//go:build integration

package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer is what the follow writes into while the test reads it, on two goroutines.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.String()
}

// The floor drops the metadata address whatever the policy says, so one ping is a host drop, and a
// follow that is live prints it without the test asking the log again.
func TestLogsFollowsTheEgressLogLive(t *testing.T) {
	app, out := newCreateApp(t)

	id := create(t, app, out, "/bin/sleep", "600")
	t.Cleanup(func() { cleanUp(t, app, id) })

	printed := &safeBuffer{}
	follow := app
	follow.Out, follow.Err = printed, printed

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- follow.Run(ctx, []string{"logs", "-f", "--egress", id}) }()

	if err := app.Run(t.Context(), []string{"exec", id, "--", "/bin/sh", "-c", "ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 || true"}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	out.Reset()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(printed.String(), `"source":"host"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(printed.String(), `"source":"host"`) {
		t.Fatalf("the follow printed %q, want the host drop the floor made", printed.String())
	}

	// An operator leaves a follow with Ctrl-C, which is this context ending, and shard says nothing about it.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the follow ended with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the follow outlived the context that ended it")
	}
}
