package sandbox

import (
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestLaunchableFollowsDockerRestartPolicies(t *testing.T) {
	cases := []struct {
		name     string
		policy   models.RestartPolicy
		killed   bool
		stopped  bool
		by       trigger
		launched bool
	}{
		{"always on an operator start", models.RestartAlways, false, false, operatorStart, true},
		{"always after a kill and a stop, on a daemon start", models.RestartAlways, true, true, daemonStart, true},
		{"unless-stopped on an operator start", models.RestartUnlessStopped, false, true, operatorStart, true},
		{"unless-stopped on a daemon start after a lost run", models.RestartUnlessStopped, false, false, daemonStart, true},
		{"unless-stopped on a daemon start after a shard stop", models.RestartUnlessStopped, false, true, daemonStart, false},
		{"unless-stopped after a kill", models.RestartUnlessStopped, true, false, operatorStart, false},
		{"on-failure never starts with the sandbox", models.RestartOnFailure, false, false, operatorStart, false},
		{"no never starts with the sandbox", models.RestartNo, false, false, daemonStart, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			process := models.Process{Name: "api", Restart: models.RestartSpec{Policy: c.policy}, Killed: c.killed}
			sb := models.Sandbox{StoppedByOperator: c.stopped}
			if got := launchable(process, sb, c.by); got != c.launched {
				t.Fatalf("launchable = %v, want %v", got, c.launched)
			}
		})
	}
}
