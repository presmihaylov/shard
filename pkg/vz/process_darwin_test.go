//go:build darwin

package vz

import (
	"testing"

	"golang.org/x/sys/unix"
)

// A process in its exit answers nothing again, so it reads ended before its zombie does, as pgrep sees it (SHARD-618).
func TestAProcessInItsExitOrAZombieHasEnded(t *testing.T) {
	const running = 2
	cases := []struct {
		name  string
		stat  int8
		flag  int32
		ended bool
	}{
		{"running", running, 0, false},
		{"running in its exit", running, exiting, true},
		{"a zombie", zombie, 0, true},
	}
	for _, c := range cases {
		var proc unix.KinfoProc
		proc.Proc.P_stat = c.stat
		proc.Proc.P_flag = c.flag
		if got := ended(&proc); got != c.ended {
			t.Errorf("%s: ended = %t, want %t", c.name, got, c.ended)
		}
	}
}
