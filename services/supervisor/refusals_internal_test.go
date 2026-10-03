package supervisor

import (
	"bytes"
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
