package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A VM-wide OOM leaves oom at 0 and counts only oom_kill, and memory.oom.group still took every guest process.
func TestAnOOMKillOfTheVMReadsAsTheBounds(t *testing.T) {
	for _, c := range []struct {
		name   string
		events string
		want   bool
	}{
		{"the bound", "low 0\nhigh 0\nmax 9\noom 1\noom_kill 1\n", true},
		{"the VM", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 1\n", true},
		{"none", "low 0\nhigh 0\nmax 4\noom 0\noom_kill 0\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte(c.events), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := oomKilledIn(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("oomKilledIn = %v, want %v", got, c.want)
			}
		})
	}
}
