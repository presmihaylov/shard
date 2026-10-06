package termrelay_test

import (
	"errors"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/presmihaylov/shard/pkg/launch"
	"github.com/presmihaylov/shard/pkg/termrelay"
)

func TestArgsPutTheModeAndWorkDirBeforeTheCommand(t *testing.T) {
	got := termrelay.Args("/.shard/init", "/srv", []string{"sh", "-l"})

	want := []string{"/.shard/init", "terminal", "/srv", "sh", "-l"}
	if !slices.Equal(got, want) {
		t.Errorf("Args returned %q, want %q", got, want)
	}
}

func TestAwaitRunsOnStartForACommandThatStarted(t *testing.T) {
	started := 0
	if err := termrelay.Await(strings.NewReader("S"), func() { started++ }); err != nil {
		t.Fatalf("Await: %v", err)
	}

	if started != 1 {
		t.Errorf("onStart ran %d times, want once", started)
	}
}

func TestAwaitReturnsTheErrnoOfACommandThatNeverRan(t *testing.T) {
	cases := []struct {
		record string
		want   launch.NotStartedError
	}{
		{"E2", launch.NotStartedError{Errno: syscall.ENOENT}},
		{"E13", launch.NotStartedError{Errno: syscall.EACCES}},
		{"D20", launch.NotStartedError{Errno: syscall.ENOTDIR, Chdir: true}},
	}

	for _, c := range cases {
		err := termrelay.Await(strings.NewReader(c.record), func() { t.Errorf("%s ran onStart", c.record) })

		failed, ok := errors.AsType[*launch.NotStartedError](err)
		if !ok || *failed != c.want {
			t.Errorf("%s: Await returned %v, want %+v", c.record, err, c.want)
		}
	}
}

// A relay that died before it wrote is no command that failed, so it never reads as one.
func TestAwaitRefusesARelayThatSentNoRecord(t *testing.T) {
	if err := termrelay.Await(strings.NewReader(""), func() { t.Error("an empty record ran onStart") }); !errors.Is(err, termrelay.ErrNoRecord) {
		t.Errorf("Await returned %v, want ErrNoRecord", err)
	}
}

func TestAwaitRefusesARecordItCannotRead(t *testing.T) {
	for _, record := range []string{"X2", "E", "Eabc", "E0", "E99999", "D-1"} {
		err := termrelay.Await(strings.NewReader(record), func() { t.Errorf("%q ran onStart", record) })

		if err == nil || errors.Is(err, termrelay.ErrNoRecord) {
			t.Errorf("%q: Await returned %v, want a refusal of the record", record, err)
		}
		if _, ok := errors.AsType[*launch.NotStartedError](err); ok {
			t.Errorf("%q: Await returned %v, want no command that never ran", record, err)
		}
	}
}

func TestAwaitKeepsTheReadError(t *testing.T) {
	cause := errors.New("deadline")

	if err := termrelay.Await(iotest.ErrReader(cause), func() { t.Error("a failed read ran onStart") }); !errors.Is(err, cause) {
		t.Errorf("Await returned %v, want the read error", err)
	}
}
