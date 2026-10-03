package vz

import (
	"errors"
	"os"
	"testing"
)

// Each end of the frames pair holds a burst of full frames nobody reads, where the macOS default refuses the third (SHARD-384).
func TestTheFramesPairHoldsABurstEitherWay(t *testing.T) {
	guest, host, err := frames()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(guest.Close(), host.Close()); err != nil {
			t.Errorf("close the frames pair: %v", err)
		}
	})

	frame := make([]byte, 1514)
	for _, end := range []struct {
		name string
		from *os.File
	}{{"to the guest", host}, {"to the host", guest}} {
		for i := range 1000 {
			if _, err := end.from.Write(frame); err != nil {
				t.Fatalf("frame %d %s: %v", i, end.name, err)
			}
		}
	}
}
