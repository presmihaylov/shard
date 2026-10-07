package main

import (
	"fmt"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/memfd"
)

// On sysbox fd 0 is a sealed page, so each table overwrites it in place and the page never changes size (SHARD-419).
func TestFileReporterOverwritesTheSealedPage(t *testing.T) {
	f, err := memfd.Create("shard-exit", models.ExitChannelSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	asStdin(t, f)

	// The worst table first, so a shorter one after it must clear the tail.
	if err := (&fileReporter{}).changed(models.ProcessReport{}, worstTable().Processes); err != nil {
		t.Fatalf("write the worst table to the page: %v", err)
	}
	var table []models.ProcessReport
	for i := range 3 {
		p := models.ProcessReport{Name: fmt.Sprintf("p%d", i), ProcessStatus: models.ProcessStatus{State: models.ProcessExited, Exit: &models.ExitStatus{Code: i}}, Seq: uint64(i + 1)}
		table = append(table, p)
		if err := (&fileReporter{}).changed(p, table); err != nil {
			t.Fatal(err)
		}
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != models.ExitChannelSize {
		t.Errorf("the page holds %d bytes, want %d", info.Size(), models.ExitChannelSize)
	}
	page := make([]byte, models.ExitChannelSize)
	if _, err := f.ReadAt(page, 0); err != nil {
		t.Fatal(err)
	}
	got := lastTable(t, page)
	if len(got.Processes) != 3 || got.Processes[2].Name != "p2" || got.Processes[2].Exit.Code != 2 {
		t.Errorf("the page reads %+v, want only the last table", got)
	}
}
