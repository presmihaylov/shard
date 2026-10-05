package main

import (
	"os"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/memfd"
	"github.com/presmihaylov/shard/services/bundle"
)

// On sysbox fd 0 is a sealed page, so each exit overwrites it in place and the page never changes size (SHARD-419).
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
	stdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = stdin })

	for _, code := range []int{255, 5, 7} {
		if err := (&fileReporter{}).exited(models.ExitStatus{Code: code}); err != nil {
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
	if report, found := bundle.DecodeExitPage(page); !found || report != (models.ExitReport{Kind: models.ExitReportKind, Code: 7}) {
		t.Errorf("the page reads %+v (found %v), want only the last exit {code:7}", report, found)
	}
}
