package term

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/pkg/pty"
)

// keyed is an interactive terminal without color whose keys are typed.
func keyed(out io.Writer, typed string) *Terminal {
	keys := strings.NewReader(typed)

	return &Terminal{
		out:         out,
		interactive: true,
		input:       func(context.Context) io.Reader { return keys },
		raw:         func() (pty.Restore, error) { return func() error { return nil }, nil },
	}
}

func TestOffATerminalEveryQuestionNeedsOne(t *testing.T) {
	in, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(in.Close(), w.Close()); err != nil {
			t.Error(err)
		}
	})
	term := New(in, &bytes.Buffer{}, func(string) string { return "" })
	if term.Interactive() {
		t.Fatal("a pipe is interactive")
	}

	_, selectErr := term.Select(t.Context(), "pick", []Option{{Name: "a", Label: "A"}})
	_, confirmErr := term.Confirm(t.Context(), "sure?", true)
	_, textErr := term.Text(t.Context(), "url")
	_, secretErr := term.Secret(t.Context(), "key")
	for name, err := range map[string]error{"select": selectErr, "confirm": confirmErr, "text": textErr, "secret": secretErr} {
		if !errors.Is(err, ErrNotTerminal) {
			t.Errorf("%s off a terminal: %v, want ErrNotTerminal", name, err)
		}
	}
}

func TestOffATerminalTheChecklistPrintsEachStepOnceItEnds(t *testing.T) {
	var out bytes.Buffer
	list, err := (&Terminal{out: &out}).Checklist("Setting up", []string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mark := range []error{list.Start(0), list.Done(0), list.Start(1), list.Fail(1, "it broke")} {
		if mark != nil {
			t.Fatal(mark)
		}
	}

	if want := "Setting up\n\n✓ one\n✗ two\n  it broke\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

func TestSelectMovesPastTheUnavailableRows(t *testing.T) {
	options := []Option{
		{Name: "a", Label: "A", Default: true},
		{Name: "b", Label: "B", Unavailable: []string{"needs /dev/kvm"}},
		{Name: "c", Label: "C"},
	}
	for _, tc := range []struct {
		typed string
		want  int
	}{
		{"\r", 0},
		{"\x1b[B\r", 2},
		{"jj\r", 2},
		{"j\x1b[Ak\r", 0},
	} {
		var out bytes.Buffer
		chosen, err := keyed(&out, tc.typed).Select(t.Context(), "pick", options)
		if err != nil {
			t.Fatalf("typed %q: %v", tc.typed, err)
		}
		if chosen != tc.want {
			t.Errorf("typed %q chose %d, want %d", tc.typed, chosen, tc.want)
		}
		if !strings.Contains(out.String(), "  B [Unavailable]") || !strings.Contains(out.String(), "    needs /dev/kvm") {
			t.Errorf("typed %q drew %q without the unavailable row and its reason", tc.typed, out.String())
		}
	}
}

func TestSelectRefusesWhenNoRowCanBeChosen(t *testing.T) {
	_, err := keyed(&bytes.Buffer{}, "\r").Select(t.Context(), "pick", []Option{{Label: "A", Unavailable: []string{"no"}}})
	if err == nil {
		t.Fatal("a select with every row unavailable chose one")
	}
}

func TestConfirmTakesTheDefaultOnAnEmptyReply(t *testing.T) {
	for _, tc := range []struct {
		typed string
		yes   bool
		want  bool
	}{
		{"\n", true, true},
		{"\n", false, false},
		{"maybe\nn\n", true, false},
		{"YES\n", false, true},
	} {
		got, err := keyed(&bytes.Buffer{}, tc.typed).Confirm(t.Context(), "sure?", tc.yes)
		if err != nil {
			t.Fatalf("typed %q: %v", tc.typed, err)
		}
		if got != tc.want {
			t.Errorf("typed %q with default %v answered %v", tc.typed, tc.yes, got)
		}
	}
}

func TestSecretEchoesOneDotPerCharacter(t *testing.T) {
	var out bytes.Buffer
	got, err := keyed(&out, "kéy\x7fy\x15ab\r").Secret(t.Context(), "API key")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ab" {
		t.Errorf("read %q, want ab", got)
	}
	echo, ok := strings.CutPrefix(out.String(), "API key\r\n> ")
	if rest := strings.NewReplacer("•", "", "\b \b", "", "\r\n", "").Replace(echo); !ok || rest != "" {
		t.Errorf("the secret echoed in %q", out.String())
	}
	if dots := strings.Count(echo, "•"); dots != 6 {
		t.Errorf("echoed %d dots, want 6", dots)
	}
}

func TestSecretEndsOnInterrupt(t *testing.T) {
	if _, err := keyed(&bytes.Buffer{}, "ab\x03").Secret(t.Context(), "API key"); !errors.Is(err, ErrInterrupted) {
		t.Errorf("Ctrl-C gave %v, want ErrInterrupted", err)
	}
}
