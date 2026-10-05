package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

func TestRefusalsLogTheFirstAtOnceAndCountTheRest(t *testing.T) {
	var out bytes.Buffer
	// The window outlasts the test, so only a tally the test calls names the count.
	r := &Refusals{log: log.New(&out, "", 0), id: "sb-1", window: time.Hour}
	refused := fmt.Errorf("read a message: %w", ErrMessageTooLong)

	steps := []struct {
		name string
		do   func()
		want []string
	}{
		{"a reset", func() { r.Note(io.EOF) }, nil},
		{"the first refusal", func() { r.Note(refused) }, []string{"sandbox sb-1: refused a control message", "1 MiB"}},
		{"three more", func() { r.Note(refused); r.Note(refused); r.Note(refused) }, nil},
		{"the tally", r.tally, []string{"sandbox sb-1: refused 3 more control messages", "1 MiB"}},
		{"a quiet window", r.tally, nil},
		{"a refusal after the quiet window", func() { r.Note(refused) }, []string{"sandbox sb-1: refused a control message"}},
	}
	for _, step := range steps {
		out.Reset()
		step.do()
		if len(step.want) == 0 && out.Len() != 0 {
			t.Fatalf("%s logged %q, want nothing", step.name, out.String())
		}
		if strings.Count(out.String(), "\n") > 1 {
			t.Fatalf("%s logged %q, want one line", step.name, out.String())
		}
		for _, part := range step.want {
			if !strings.Contains(out.String(), part) {
				t.Fatalf("%s logged %q, want it to name %q", step.name, out.String(), part)
			}
		}
	}
}

func TestRefusalsHoldOffTheRedialLongerEachTimeUntilAQuietWindow(t *testing.T) {
	r := &Refusals{log: log.New(io.Discard, "", 0), id: "sb-1", window: time.Hour}
	refused := fmt.Errorf("read a message: %w", ErrMessageTooLong)
	ms := time.Millisecond

	steps := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"a reset", io.EOF, 0},
		{"the first refusal", refused, 100 * ms},
		{"the second", refused, 200 * ms},
		{"the third", refused, 400 * ms},
		{"a reset between them", io.EOF, 0},
		{"the fourth", refused, 800 * ms},
		{"the fifth", refused, 1600 * ms},
		{"the sixth, at the cap", refused, 2000 * ms},
		{"the seventh, still at the cap", refused, 2000 * ms},
	}
	for _, step := range steps {
		if got := r.Note(step.err); got != step.want {
			t.Fatalf("%s waits %s, want %s", step.name, got, step.want)
		}
	}

	r.tally()
	if got := r.Note(refused); got != redialCap {
		t.Fatalf("a refusal after a window that held some waits %s, want the cap %s", got, redialCap)
	}
	r.tally()
	r.tally()
	if got := r.Note(refused); got != redialFloor {
		t.Fatalf("a refusal after a quiet window waits %s, want the floor %s", got, redialFloor)
	}
}

// An event flood is a refusal too: it is logged by name, and the redial after it waits like one (SHARD-550).
func TestAnEventFloodHoldsOffTheRedial(t *testing.T) {
	var out bytes.Buffer
	r := &Refusals{log: log.New(&out, "", 0), id: "sb-1", window: time.Hour}
	flood := errors.Join(ErrEventFlood, io.ErrClosedPipe)

	if got := r.Note(flood); got != redialFloor {
		t.Fatalf("the first flood waits %s, want %s", got, redialFloor)
	}
	if got := r.Note(flood); got != 2*redialFloor {
		t.Fatalf("the second flood waits %s, want %s", got, 2*redialFloor)
	}
	r.tally()
	if logged := out.String(); strings.Count(logged, "queued more than") != 2 {
		t.Fatalf("the log %q, want the flood named on the first line and the tally", logged)
	}
}
