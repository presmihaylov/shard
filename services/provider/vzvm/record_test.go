package vzvm

import (
	"path/filepath"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/supervisor"
)

// A replayed state lands the exit and the restart count, and a later replay with a newer exit replaces what a read sees.
func TestARepliedStateLandsTheLastExitAndTheRestarts(t *testing.T) {
	p := &Provider{}
	m := &machine{dir: t.TempDir()}
	first := supervisor.Message{Kind: supervisor.KindState, Ready: true, Exit: &models.ExitStatus{Code: 3}, Restarts: &models.RestartCount{Count: 1}}
	second := supervisor.Message{Kind: supervisor.KindState, Ready: true, Exit: &models.ExitStatus{Code: 5}, Restarts: &models.RestartCount{Count: 2}}

	if err := p.record(m, first); err != nil {
		t.Fatal(err)
	}
	if !m.started {
		t.Error("the state did not mark the machine started")
	}
	exit, found, err := bundle.ReadExitStatus(filepath.Join(m.dir, exitFile))
	if err != nil || !found || exit.Code != 3 {
		t.Fatalf("exit = %+v, %v, %v; want code 3", exit, found, err)
	}
	// The guest died again while no daemon held the shim, so the next adoption must show that exit, not the first.
	if err := p.record(m, second); err != nil {
		t.Fatal(err)
	}
	exit, found, err = bundle.ReadExitStatus(filepath.Join(m.dir, exitFile))
	if err != nil || !found || exit.Code != 5 {
		t.Fatalf("exit after the second replay = %+v, %v, %v; want code 5", exit, found, err)
	}
	count, err := bundle.Bundle{RestartFile: filepath.Join(m.dir, restartsFile)}.RestartCount()
	if err != nil || count.Count != 2 {
		t.Fatalf("restarts = %+v, %v; want 2", count, err)
	}
}
