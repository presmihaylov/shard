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
		width:       func() (int, error) { return 80, nil },
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
	_, textErr := term.Text(t.Context(), "url", "")
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

// The line starts as the initial value, which Enter keeps and the editing keys change. (SHARD-667)
func TestTextStartsAsTheInitialLine(t *testing.T) {
	var out bytes.Buffer
	got, err := keyed(&out, "\r").Text(t.Context(), "URL", "https://a.example")
	if err != nil || got != "https://a.example" {
		t.Fatalf("Text = %q, %v; want the initial line", got, err)
	}
	if want := "URL\r\n> https://a.example\r\n\r\n"; out.String() != want {
		t.Errorf("drew %q, want %q", out.String(), want)
	}
}

func TestTextEditsTheInitialLine(t *testing.T) {
	var out bytes.Buffer
	got, err := keyed(&out, "\x15https://b.exa\x1b[3~\x1b[Dmpler\x7f\r").Text(t.Context(), "URL", "https://a.example")
	if err != nil || got != "https://b.example" {
		t.Fatalf("Text = %q, %v; want the edited line", got, err)
	}
	if strings.Contains(out.String(), "~") || strings.Contains(out.String(), "[D") {
		t.Errorf("an arrow or the delete key echoed in %q", out.String())
	}
}

func TestSecretEndsOnInterrupt(t *testing.T) {
	if _, err := keyed(&bytes.Buffer{}, "ab\x03").Secret(t.Context(), "API key"); !errors.Is(err, ErrInterrupted) {
		t.Errorf("Ctrl-C gave %v, want ErrInterrupted", err)
	}
}

func TestARedrawMovesUpPastEveryRowAWrappedLineTook(t *testing.T) {
	var out bytes.Buffer
	term := keyed(&out, "")
	term.width = func() (int, error) { return 20, nil }
	colored := "\x1b[32m" + strings.Repeat("y", 20) + "\x1b[0m"
	drawn, err := term.redraw(0, []string{colored, strings.Repeat("x", 50)}, "\n")
	if err != nil {
		t.Fatal(err)
	}
	if drawn != 4 {
		t.Fatalf("drew %d rows, want 4: one for the colored line that fits, three for the wrapped one", drawn)
	}

	out.Reset()
	if _, err := term.redraw(drawn, []string{"short"}, "\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\r\x1b[4A") {
		t.Errorf("the redraw began %q, want it to move up 4 rows", out.String())
	}
}

func TestAnAnsweredQuestionLeavesABlankLine(t *testing.T) {
	ask := map[string]func(*Terminal) error{
		"select": func(term *Terminal) error {
			_, err := term.Select(t.Context(), "pick", []Option{{Name: "a", Label: "A"}})
			return err
		},
		"confirm": func(term *Terminal) error { _, err := term.Confirm(t.Context(), "sure?", true); return err },
		"text":    func(term *Terminal) error { _, err := term.Text(t.Context(), "url", ""); return err },
		"secret":  func(term *Terminal) error { _, err := term.Secret(t.Context(), "key"); return err },
	}
	typed := map[string]string{"select": "\r", "confirm": "\n", "text": "u\r", "secret": "k\r"}
	ends := map[string]string{"select": "\r\n\r\n", "confirm": "[Y/n] \n", "text": "u\r\n\r\n", "secret": "•\r\n\r\n"}
	for name, question := range ask {
		var out bytes.Buffer
		if err := question(keyed(&out, typed[name])); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasSuffix(out.String(), ends[name]) {
			t.Errorf("%s printed %q, want it to end %q", name, out.String(), ends[name])
		}
	}
}
