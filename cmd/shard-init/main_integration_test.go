//go:build linux && integration

package main

import (
	"os"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/supervisor"
)

// A process drops to its ids, and PID 1 keeps its own so the status still lands.
func TestAProcessRunsAsTheGivenUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping to another user needs root")
	}
	c := startContainer(t, os.O_WRONLY|os.O_APPEND)

	// A real /bin/sh, not this test binary: the go build cache is not readable by another user.
	c.run(t, supervisor.RunSpec{Name: "who", Argv: []string{"/bin/sh", "-c", "id -u"}, User: "65534:65534"})

	// fd 0 is the whole point: a supervisor that dropped too could not write it.
	if who := c.await(t, "who", models.ProcessExited); who.Exit == nil || who.Exit.Code != 0 {
		t.Fatalf("who ended as %+v, want code 0", who)
	}
	if got := c.log(t, "who"); got != "65534\n" {
		t.Fatalf("the process reported uid %q, want 65534", got)
	}
}
