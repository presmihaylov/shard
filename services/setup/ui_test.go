package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
)

// fakeUI answers each question from a script and records what it was shown, so a test drives one path of setup.
type fakeUI struct {
	// selects names the option each question picks, by its Name.
	selects  map[Question]string
	confirms map[Question]bool
	texts    map[Question]string
	secrets  map[Question]string

	asked   []Question
	options map[Question][]term.Option
	printed []string
	lists   []*fakeChecklist
}

var errUnscripted = errors.New("the test gave no answer")

func (f *fakeUI) ask(q Question) { f.asked = append(f.asked, q) }

func (f *fakeUI) Select(_ context.Context, q Question, _ string, options []term.Option) (int, error) {
	f.ask(q)
	if f.options == nil {
		f.options = map[Question][]term.Option{}
	}
	f.options[q] = options
	name, ok := f.selects[q]
	if !ok {
		return 0, fmt.Errorf("select %s: %w", q, errUnscripted)
	}
	for i, o := range options {
		if o.Name == name && len(o.Unavailable) == 0 {
			return i, nil
		}
	}

	return 0, fmt.Errorf("select %s: no option %q can be chosen", q, name)
}

func (f *fakeUI) Confirm(_ context.Context, q Question, _ string, _ bool) (bool, error) {
	f.ask(q)
	answer, ok := f.confirms[q]
	if !ok {
		return false, fmt.Errorf("confirm %s: %w", q, errUnscripted)
	}

	return answer, nil
}

func (f *fakeUI) Text(_ context.Context, q Question, _ string) (string, error) {
	f.ask(q)
	answer, ok := f.texts[q]
	if !ok {
		return "", fmt.Errorf("text %s: %w", q, errUnscripted)
	}

	return answer, nil
}

func (f *fakeUI) Secret(_ context.Context, q Question, _ string) (string, error) {
	f.ask(q)
	answer, ok := f.secrets[q]
	if !ok {
		return "", fmt.Errorf("secret %s: %w", q, errUnscripted)
	}

	return answer, nil
}

func (f *fakeUI) Checklist(title string, steps []string) (Checklist, error) {
	list := &fakeChecklist{title: title, steps: steps}
	f.lists = append(f.lists, list)

	return list, nil
}

func (f *fakeUI) Print(lines ...string) error {
	f.printed = append(f.printed, lines...)

	return nil
}

// fakeChecklist records each mark as one line, such as "done 0" or "fail 1: it broke".
type fakeChecklist struct {
	title string
	steps []string
	marks []string
}

func (c *fakeChecklist) mark(verb string, i int, detail []string) error {
	line := fmt.Sprintf("%s %d", verb, i)
	if len(detail) > 0 {
		line += ": " + strings.Join(detail, " / ")
	}
	c.marks = append(c.marks, line)

	return nil
}

func (c *fakeChecklist) Start(i int) error { return c.mark("start", i, nil) }

func (c *fakeChecklist) Done(i int) error { return c.mark("done", i, nil) }

func (c *fakeChecklist) Attention(i int, detail ...string) error {
	return c.mark("attention", i, detail)
}

func (c *fakeChecklist) Fail(i int, detail ...string) error { return c.mark("fail", i, detail) }
