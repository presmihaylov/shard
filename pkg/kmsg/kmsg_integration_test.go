//go:build integration

package kmsg

import (
	"os"
	"testing"
	"time"
)

func TestReadDrainsTheRingAndReturns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("reading /dev/kmsg needs root")
	}

	done := make(chan struct{})
	var lines []Line
	var err error
	go func() {
		lines, err = Read()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Read did not return once the ring was drained")
	}
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("Read found nothing in the ring")
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].Time.Before(lines[i-1].Time) {
			t.Fatalf("line %d is older than line %d", i, i-1)
		}
	}
}
