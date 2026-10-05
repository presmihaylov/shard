// Package term draws the prompts and the live checklist of an interactive verb, and plain lines when
// the input or the output is not a terminal. It knows key codes and escape sequences, nothing of setup.
package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/presmihaylov/shard/pkg/pty"
)

// ErrNotTerminal is a prompt asked without a terminal, so the caller names the option that answers it.
var ErrNotTerminal = errors.New("this question needs a terminal")

// ErrInterrupted is Ctrl-C at a prompt in raw mode, where it reaches shard as a key and not a signal.
var ErrInterrupted = errors.New("interrupted")

// errInputEnded is the end of the input before the answer did.
var errInputEnded = errors.New("the input ended before an answer")

// The escape sequences the prompts draw with.
const (
	clearLine = "\x1b[2K"
	keyUp     = "\x1b[A"
	keyDown   = "\x1b[B"
	ctrlC     = 3
	ctrlD     = 4
	ctrlU     = 21
	esc       = 0x1b
	backspace = 8
	del       = 127
)

// The colors, each beside a symbol or a word, so a terminal without color loses no meaning.
const (
	green  = "32"
	blue   = "34"
	yellow = "33"
	red    = "31"
	gray   = "90"
	bold   = "1"
)

// Option is one row of a Select.
type Option struct {
	// Name is the word that picks the option from a command line, as gvisor for gVisor.
	Name  string
	Label string
	// Lines are the description under the label.
	Lines []string
	// Unavailable is the reason the row cannot be chosen; any line disables it.
	Unavailable []string
	// Default is the row the cursor starts on.
	Default bool
}

func (o Option) disabled() bool { return len(o.Unavailable) > 0 }

// Terminal is one person at one terminal, or a script when the input or the output is not one.
type Terminal struct {
	out         io.Writer
	interactive bool
	color       bool
	// input reads the keys; a cancel ends a read that would block.
	input func(ctx context.Context) io.Reader
	raw   func() (pty.Restore, error)
}

// New is the terminal on in and out; NO_COLOR set to anything drops the color, as no-color.org says.
func New(in *os.File, out io.Writer, getenv func(string) string) *Terminal {
	file, isFile := out.(*os.File)
	interactive := isFile && pty.IsTerminal(in) && pty.IsTerminal(file)

	return &Terminal{
		out:         out,
		interactive: interactive,
		color:       interactive && getenv("NO_COLOR") == "",
		input:       func(ctx context.Context) io.Reader { return pty.Input(ctx, in) },
		raw:         func() (pty.Restore, error) { return pty.MakeRaw(in) },
	}
}

// Interactive reports whether a person can answer, so a caller without one asks for an option instead.
func (t *Terminal) Interactive() bool { return t.interactive }

// Print writes each line as it is.
func (t *Terminal) Print(lines ...string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(t.out, line); err != nil {
			return fmt.Errorf("write to the terminal: %w", err)
		}
	}

	return nil
}

// Select draws the options with a cursor the arrow keys move past the disabled rows, and returns the chosen index.
func (t *Terminal) Select(ctx context.Context, title string, options []Option) (chosen int, err error) {
	if !t.interactive {
		return 0, ErrNotTerminal
	}
	at, ok := start(options)
	if !ok {
		return 0, errors.New("every option is unavailable")
	}

	restore, err := t.raw()
	if err != nil {
		return 0, err
	}
	defer func() {
		if restoreErr := restore(); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore the terminal: %w", restoreErr)
		}
	}()

	drawn := 0
	keys := t.input(ctx)
	for {
		if drawn, err = t.redraw(drawn, t.selectLines(title, options, at), "\r\n"); err != nil {
			return 0, err
		}
		key, err := readKey(keys)
		if err != nil {
			return 0, err
		}
		switch key {
		case "\r", "\n":
			return at, nil
		case string(rune(ctrlC)):
			return 0, ErrInterrupted
		case keyUp, "k":
			at = step(options, at, -1)
		case keyDown, "j":
			at = step(options, at, 1)
		}
	}
}

// start is the Default row when it can be chosen, or else the first that can.
func start(options []Option) (int, bool) {
	for i, o := range options {
		if o.Default && !o.disabled() {
			return i, true
		}
	}
	for i, o := range options {
		if !o.disabled() {
			return i, true
		}
	}

	return 0, false
}

// step moves the cursor to the next row in that direction that can be chosen, and stays at an end.
func step(options []Option, at, by int) int {
	for i := at + by; i >= 0 && i < len(options); i += by {
		if !options[i].disabled() {
			return i
		}
	}

	return at
}

func (t *Terminal) selectLines(title string, options []Option, at int) []string {
	lines := []string{title, ""}
	described := false
	for _, o := range options {
		described = described || len(o.Lines)+len(o.Unavailable) > 0
	}
	for i, o := range options {
		if described && i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, t.optionLines(o, i == at)...)
	}

	return lines
}

func (t *Terminal) optionLines(o Option, current bool) []string {
	label := "  " + o.Label
	if o.disabled() {
		label += " [Unavailable]"
	}
	switch {
	case current:
		label = t.paint(bold, "❯ "+o.Label)
	case o.disabled():
		label = t.paint(gray, label)
	}

	lines := []string{label}
	for _, line := range append(append([]string(nil), o.Lines...), o.Unavailable...) {
		text := "    " + line
		if o.disabled() {
			text = t.paint(gray, text)
		}
		lines = append(lines, text)
	}

	return lines
}

// Confirm asks a yes or no question; an empty answer takes yes, which the prompt shows as [Y/n] or [y/N].
func (t *Terminal) Confirm(ctx context.Context, question string, yes bool) (bool, error) {
	if !t.interactive {
		return false, ErrNotTerminal
	}
	hint := "[y/N]"
	if yes {
		hint = "[Y/n]"
	}

	keys := t.input(ctx)
	for {
		if _, err := fmt.Fprintf(t.out, "%s %s ", question, hint); err != nil {
			return false, fmt.Errorf("write to the terminal: %w", err)
		}
		answer, err := readLine(keys)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "":
			return yes, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

// Text asks for one line under the prompt, after a > the way the spec draws it; the line starts as initial, which Enter keeps.
func (t *Terminal) Text(ctx context.Context, prompt, initial string) (string, error) {
	answer, err := t.line(ctx, prompt, initial, false)

	return strings.TrimSpace(answer), err
}

// Secret asks for a value that echoes as one dot per character, so it lands on no screen and no scrollback.
func (t *Terminal) Secret(ctx context.Context, prompt string) (string, error) {
	return t.line(ctx, prompt, "", true)
}

// line reads one line in raw mode, which is what lets it start as initial and mask what is typed.
func (t *Terminal) line(ctx context.Context, prompt, initial string, masked bool) (line string, err error) {
	if !t.interactive {
		return "", ErrNotTerminal
	}
	restore, err := t.raw()
	if err != nil {
		return "", err
	}
	defer func() {
		if restoreErr := restore(); restoreErr != nil && err == nil {
			err = fmt.Errorf("restore the terminal: %w", restoreErr)
		}
	}()

	value := []byte(initial)
	var drawn strings.Builder
	drawn.WriteString(prompt + "\r\n> ")
	for _, b := range value {
		drawn.WriteString(shown(b, masked))
	}
	if _, err := io.WriteString(t.out, drawn.String()); err != nil {
		return "", wrapWrite(err)
	}

	keys := t.input(ctx)
	for {
		b, err := readByte(keys)
		if err != nil {
			return "", err
		}
		echo := ""
		switch {
		case b == '\r' || b == '\n':
			_, err := io.WriteString(t.out, "\r\n")

			return string(value), wrapWrite(err)
		case b == ctrlC:
			return "", ErrInterrupted
		case b == ctrlD && len(value) == 0:
			return "", errInputEnded
		case b == esc:
			if err := skipEscape(keys); err != nil {
				return "", err
			}
		case b == ctrlU:
			echo = strings.Repeat("\b \b", utf8.RuneCount(value))
			value = value[:0]
		case b == del || b == backspace:
			if len(value) > 0 {
				_, size := utf8.DecodeLastRune(value)
				value = value[:len(value)-size]
				echo = "\b \b"
			}
		case b >= ' ':
			value = append(value, b)
			echo = shown(b, masked)
		}
		if _, err := io.WriteString(t.out, echo); err != nil {
			return "", wrapWrite(err)
		}
	}
}

// shown is what the screen shows of one typed byte: the byte, or one dot per character so the dots count runes and not bytes.
func shown(b byte, masked bool) string {
	if !masked {
		return string([]byte{b})
	}
	if !utf8.RuneStart(b) {
		return ""
	}

	return "•"
}

// skipEscape reads the rest of an escape sequence, such as an arrow key, so none of it lands in the line.
func skipEscape(r io.Reader) error {
	b, err := readByte(r)
	if err != nil || (b != '[' && b != 'O') {
		return err
	}
	for {
		// A sequence ends on its final byte, @ through ~.
		if b, err = readByte(r); err != nil || (b >= '@' && b <= '~') {
			return err
		}
	}
}

func wrapWrite(err error) error {
	if err != nil {
		return fmt.Errorf("write to the terminal: %w", err)
	}

	return nil
}

// redraw replaces the lines it drew last time, and returns how many it drew now.
func (t *Terminal) redraw(drawn int, lines []string, newline string) (int, error) {
	var b strings.Builder
	if drawn > 0 {
		fmt.Fprintf(&b, "\r\x1b[%dA", drawn)
	}
	for _, line := range lines {
		b.WriteString(clearLine + line + newline)
	}
	if _, err := io.WriteString(t.out, b.String()); err != nil {
		return 0, wrapWrite(err)
	}

	return len(lines), nil
}

func (t *Terminal) paint(color, s string) string {
	if !t.color {
		return s
	}

	return "\x1b[" + color + "m" + s + "\x1b[0m"
}

// readKey reads one key: a byte, or the three bytes of an arrow.
func readKey(r io.Reader) (string, error) {
	b, err := readByte(r)
	if err != nil || b != 0x1b {
		return string(rune(b)), err
	}
	seq := []byte{b}
	for range 2 {
		next, err := readByte(r)
		if err != nil {
			return "", err
		}
		seq = append(seq, next)
	}

	return string(seq), nil
}

// readLine reads up to a newline one byte at a time, so nothing past the answer is buffered away from the next prompt.
func readLine(r io.Reader) (string, error) {
	var line []byte
	for {
		b, err := readByte(r)
		if errors.Is(err, errInputEnded) && len(line) > 0 {
			return string(line), nil
		}
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		line = append(line, b)
	}
}

func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	n, err := r.Read(b[:])
	if n == 1 {
		return b[0], nil
	}
	if errors.Is(err, io.EOF) {
		return 0, errInputEnded
	}
	if err != nil {
		return 0, fmt.Errorf("read the terminal: %w", err)
	}

	return 0, errInputEnded
}

// Checklist is the live list of the steps of one job: a spinner on the step that runs, a mark on each one done.
type Checklist struct {
	t     *Terminal
	mu    sync.Mutex
	steps []checkStep
	drawn int
	frame int
	// spin stops the spinner; nil while no step runs.
	spin chan struct{}
	wg   sync.WaitGroup
	// err is the first failed write of the spinner, returned by the next call.
	err error
}

type checkState int

const (
	pending checkState = iota
	running
	done
	attention
	failed
)

type checkStep struct {
	title  string
	state  checkState
	detail []string
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinInterval = 80 * time.Millisecond

func newTicker() *time.Ticker { return time.NewTicker(spinInterval) }

// Checklist prints the title and every step as pending; off a terminal it prints each step once, as it ends.
func (t *Terminal) Checklist(title string, steps []string) (*Checklist, error) {
	list := &Checklist{t: t}
	for _, s := range steps {
		list.steps = append(list.steps, checkStep{title: s})
	}
	if err := t.Print(title, ""); err != nil {
		return nil, err
	}
	if !t.interactive {
		return list, nil
	}

	return list, list.draw()
}

// Start marks step i as the one that runs.
func (c *Checklist) Start(i int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps[i].state = running
	if !c.t.interactive {
		return c.err
	}
	if c.spin == nil {
		c.spin = make(chan struct{})
		c.wg.Add(1)
		go c.turn(c.spin)
	}

	return errors.Join(c.err, c.draw())
}

// Done marks step i complete.
func (c *Checklist) Done(i int) error { return c.end(i, done, nil) }

// Attention marks step i complete with something the reader must see, in the lines under it.
func (c *Checklist) Attention(i int, detail ...string) error { return c.end(i, attention, detail) }

// Fail marks step i failed, with the reason in the lines under it.
func (c *Checklist) Fail(i int, detail ...string) error { return c.end(i, failed, detail) }

func (c *Checklist) end(i int, state checkState, detail []string) error {
	c.stopSpinner()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps[i].state, c.steps[i].detail = state, detail
	if c.t.interactive {
		return errors.Join(c.err, c.draw())
	}

	return errors.Join(c.err, c.t.Print(c.stepLines(c.steps[i])...))
}

func (c *Checklist) stopSpinner() {
	c.mu.Lock()
	spin := c.spin
	c.spin = nil
	c.mu.Unlock()
	if spin != nil {
		close(spin)
		c.wg.Wait()
	}
}

func (c *Checklist) turn(stop chan struct{}) {
	defer c.wg.Done()
	tick := newTicker()
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			c.mu.Lock()
			c.frame++
			if err := c.draw(); err != nil && c.err == nil {
				c.err = err
			}
			c.mu.Unlock()
		}
	}
}

// draw repaints the whole list in place; the caller holds the lock.
func (c *Checklist) draw() error {
	var lines []string
	for _, s := range c.steps {
		lines = append(lines, c.stepLines(s)...)
	}
	drawn, err := c.t.redraw(c.drawn, lines, "\n")
	c.drawn = drawn

	return err
}

func (c *Checklist) stepLines(s checkStep) []string {
	mark := map[checkState]string{
		pending:   c.t.paint(gray, "○"),
		running:   c.t.paint(blue, spinner[c.frame%len(spinner)]),
		done:      c.t.paint(green, "✓"),
		attention: c.t.paint(yellow, "!"),
		failed:    c.t.paint(red, "✗"),
	}[s.state]
	lines := []string{mark + " " + s.title}
	for _, d := range s.detail {
		lines = append(lines, "  "+d)
	}

	return lines
}
