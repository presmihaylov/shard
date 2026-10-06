package api

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// deadlines records each read deadline an idle body sets.
type deadlines []time.Time

func (d *deadlines) SetReadDeadline(deadline time.Time) error {
	*d = append(*d, deadline)

	return nil
}

// Each read moves the deadline to the idle bound past the clock, so only a gap between two reads can reach it, and the end stops the moves.
func TestAnIdleBodyMovesItsDeadlineBeforeEachRead(t *testing.T) {
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	SetClock(t, func() time.Time { return at })
	var set deadlines
	body := &idleBody{body: iotest.OneByteReader(strings.NewReader("abc")), control: &set, idle: time.Second}

	var read []byte
	var want []time.Time
	for {
		at = at.Add(time.Minute)
		want = append(want, at.Add(time.Second))
		p := make([]byte, 8)
		n, err := body.Read(p)
		read = append(read, p[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read %q, then %v", read, err)
		}
	}
	n, err := body.Read(make([]byte, 8))

	if string(read) != "abc" || !slices.Equal(set, want) {
		t.Errorf("read %q with deadlines %v, want abc with %v, one minute apart", read, set, want)
	}
	if n != 0 || !errors.Is(err, io.EOF) || len(set) != len(want) {
		t.Errorf("a read after the end gave %d, %v and %d deadlines, want 0, EOF and still %d", n, err, len(set), len(want))
	}
}
