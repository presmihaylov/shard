package sysbox_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/memfd"
	"github.com/presmihaylov/shard/services/bundle"
)

func sealedPage(t *testing.T) *os.File {
	t.Helper()

	f, err := memfd.Create("shard-exit", models.ExitChannelSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})

	return f
}

func writeRecord(t *testing.T, f *os.File, exit models.ExitStatus) {
	t.Helper()

	encoded, err := json.Marshal(models.ExitReport{Kind: models.ExitReportKind, Code: exit.Code, Signal: exit.Signal})
	if err != nil {
		t.Fatal(err)
	}
	page := make([]byte, models.ExitChannelSize)
	copy(page, "\n"+string(encoded)+"\n")
	if _, err := f.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
}

// After a restart the daemon finds the page through PID 1's fd 0 and copies its record into the exit file.
func TestAReopenReadsTheSealedPageCreateRecorded(t *testing.T) {
	lab := newChannelLab(t)
	page := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", page.Fd())))
	writeRecord(t, page, models.ExitStatus{Code: 5})

	exit, err := lab.exitStatus(t)
	if err != nil || exit == nil || *exit != (models.ExitStatus{Code: 5}) {
		t.Fatalf("ExitStatus returned %+v, %v, want {code:5}", exit, err)
	}

	// A guest write that is no record leaves the last one standing.
	if _, err := page.WriteAt([]byte("not a record\n"), 0); err != nil {
		t.Fatal(err)
	}
	exit, err = lab.exitStatus(t)
	if err != nil || exit == nil || *exit != (models.ExitStatus{Code: 5}) {
		t.Errorf("ExitStatus after a guest write returned %+v, %v, want {code:5} still", exit, err)
	}

	writeRecord(t, page, models.ExitStatus{Code: 7})
	if exit, err := lab.exitStatus(t); err != nil || exit == nil || *exit != (models.ExitStatus{Code: 7}) {
		t.Errorf("ExitStatus after the next exit returned %+v, %v, want {code:7}", exit, err)
	}
	got, found, err := bundle.ReadExitStatus(lab.b.ExitFile)
	if err != nil || !found || got != (models.ExitStatus{Code: 7}) {
		t.Errorf("the exit file reads %+v (found %v, %v), want {code:7}", got, found, err)
	}
}

// Guest root can make its own memfd with the same seals and size; only the inode create recorded tells it apart.
func TestAReopenNamesTheGuestsOwnSealedPageReplaced(t *testing.T) {
	lab := newChannelLab(t)
	ours := sealedPage(t)
	theirs := sealedPage(t)
	lab.pointFd0(t, fmt.Sprintf("/proc/self/fd/%d", theirs.Fd()))
	lab.record(t, inodeOf(t, fmt.Sprintf("/proc/self/fd/%d", ours.Fd())))

	_, err := lab.exitStatus(t)
	if !errors.Is(err, models.ErrExitChannelReplaced) {
		t.Errorf("ExitStatus over the guest's own memfd returned %v, want %v", err, models.ErrExitChannelReplaced)
	}
}
