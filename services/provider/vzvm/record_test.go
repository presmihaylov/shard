package vzvm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// A replayed state lands the exit and the restart count in the files, and a second replay adds no second exit.
func TestARepliedStateLandsTheExitAndTheRestartsOnce(t *testing.T) {
	p := &Provider{}
	m := &machine{dir: t.TempDir()}
	state := supervisor.Message{Kind: supervisor.KindState, Ready: true, Exit: &models.ExitStatus{Code: 3}, Restarts: &models.RestartCount{Count: 2}}

	for range 2 {
		if err := p.record(m, state); err != nil {
			t.Fatal(err)
		}
	}
	if !m.started {
		t.Error("the state did not mark the machine started")
	}
	exit, found, err := bundle.ReadExitStatus(filepath.Join(m.dir, exitFile))
	if err != nil || !found || exit.Code != 3 {
		t.Fatalf("exit = %+v, %v, %v; want code 3", exit, found, err)
	}
	blob, err := os.ReadFile(filepath.Join(m.dir, exitFile))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(blob, []byte(`"kind"`)); n != 1 {
		t.Errorf("the exit file holds %d exits after two replays, want one", n)
	}
	count, err := bundle.Bundle{RestartFile: filepath.Join(m.dir, restartsFile)}.RestartCount()
	if err != nil || count.Count != 2 {
		t.Fatalf("restarts = %+v, %v; want 2", count, err)
	}
}
