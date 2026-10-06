package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
)

// A verify in a test trusts the first status that answers; the tests of the steady window set their own.
func TestMain(m *testing.M) {
	verifySteady = 0
	os.Exit(m.Run())
}

// The first choice preselects remote on a machine no provider runs on, and says why on local. (SHARD-666)
func TestRunAsksLocalOrRemoteFirst(t *testing.T) {
	cases := []struct {
		name    string
		host    Host
		want    string
		because []string
	}{
		{"linux", Host{OS: "linux", Arch: "amd64"}, "local", nil},
		{"intel mac", Host{OS: "darwin", Arch: "amd64"}, "remote", []string{
			"No provider runs on this machine.",
			"Requires an Apple silicon Mac with macOS 14 or later.",
			"This Mac has an Intel processor.",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, _ := testHost(t, nil)
			host.OS, host.Arch = c.host.OS, c.host.Arch
			ui := &fakeUI{}
			err := (&Setup{Host: host, UI: ui}).Run(t.Context())
			if !errors.Is(err, errUnscripted) {
				t.Fatalf("Run without an answer: %v", err)
			}

			if !slices.Equal(ui.asked, []Question{AskMode}) {
				t.Errorf("asked %v, want only %s", ui.asked, AskMode)
			}
			options := ui.options[AskMode]
			var names, defaults []string
			for _, o := range options {
				names = append(names, o.Name)
				if o.Default {
					defaults = append(defaults, o.Name)
				}
			}
			if !slices.Equal(names, []string{"local", "remote"}) || !slices.Equal(defaults, []string{c.want}) {
				t.Errorf("the first choice offers %v with %v preselected, want local then remote with %s", names, defaults, c.want)
			}
			if !slices.Equal(options[0].Lines, c.because) || len(options[0].Unavailable) > 0 {
				t.Errorf("local says %q and is unavailable for %q, want %q and still a choice", options[0].Lines, options[0].Unavailable, c.because)
			}
		})
	}
}

func TestApplyMarksEveryStepDone(t *testing.T) {
	ui := &fakeUI{}
	ran := 0
	step := func(context.Context) error { ran++; return nil }

	if err := (&Setup{UI: ui}).apply(t.Context(), "Setting up", []Step{{"one", step}, {"two", step}}); err != nil {
		t.Fatal(err)
	}

	if ran != 2 || !slices.Equal(ui.lists[0].steps, []string{"one", "two"}) {
		t.Errorf("ran %d steps of %v", ran, ui.lists[0].steps)
	}
	if want := []string{"start 0", "done 0", "start 1", "done 1"}; !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
	if len(ui.printed) > 0 {
		t.Errorf("a clean run printed %q", ui.printed)
	}
}

func TestApplyStopsAtTheFailedStepAndSaysWhatStays(t *testing.T) {
	ui := &fakeUI{}
	broke := errors.New("it broke")
	ran := []string{}
	step := func(name string, err error) Step {
		return Step{name, func(context.Context) error { ran = append(ran, name); return err }}
	}

	err := (&Setup{UI: ui}).apply(t.Context(), "Setting up", []Step{step("one", nil), step("two", broke), step("three", nil)})

	var stopped *StoppedError
	if !errors.As(err, &stopped) || stopped.Step != "two" || !errors.Is(err, broke) {
		t.Fatalf("apply returned %v, want the stop at two", err)
	}
	if !slices.Equal(ran, []string{"one", "two"}) {
		t.Errorf("ran %v, want it to stop after two", ran)
	}
	if want := []string{"start 0", "done 0", "start 1", "fail 1: it broke"}; !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
	if want := []string{"", "Setup stopped. Earlier completed steps remain in place.", "Run shard setup again to retry."}; !slices.Equal(ui.printed, want) {
		t.Errorf("printed %q, want %q", ui.printed, want)
	}
}

// The retry hint names shard when it is on PATH, and the full path of the running binary when it is not. (SHARD-743)
func TestTheRetryHintNamesAReachableCommand(t *testing.T) {
	broke := func(context.Context) error { return errors.New("nope") }
	for _, c := range []struct {
		name string
		host Host
		want string
	}{
		{"on PATH", Host{LookPath: func(string) (string, error) { return "/usr/local/bin/shard", nil }, Executable: "/tmp/build/shard"}, "Run shard setup again to retry."},
		{"not on PATH", Host{LookPath: func(string) (string, error) { return "", errors.New("not found") }, Executable: "/tmp/build/shard"}, "Run /tmp/build/shard setup again to retry."},
	} {
		t.Run(c.name, func(t *testing.T) {
			ui := &fakeUI{}
			if err := (&Setup{Host: c.host, UI: ui}).apply(t.Context(), "Setting up", []Step{{"only", broke}}); err == nil {
				t.Fatal("apply of a failing step returned no error")
			}
			if !slices.Contains(ui.printed, c.want) {
				t.Errorf("printed %q, want it to contain %q", ui.printed, c.want)
			}
		})
	}
}

// A no-TTY run sets RetrySuffix so the retry hint repeats the flags the run had, since a re-run cannot ask for them. (SHARD-743)
func TestTheRetryHintRepeatsTheRunsFlags(t *testing.T) {
	ui := &fakeUI{}
	broke := func(context.Context) error { return errors.New("nope") }
	host := Host{LookPath: func(string) (string, error) { return "/usr/local/bin/shard", nil }}
	s := &Setup{Host: host, UI: ui, RetrySuffix: " --local --provider gvisor --start-at-boot=true -y"}
	if err := s.apply(t.Context(), "Setting up", []Step{{"only", broke}}); err == nil {
		t.Fatal("apply of a failing step returned no error")
	}
	want := "Run shard setup --local --provider gvisor --start-at-boot=true -y again to retry."
	if !slices.Contains(ui.printed, want) {
		t.Errorf("printed %q, want it to contain %q", ui.printed, want)
	}
}

func TestApplyPrintsTheLinesOfAProblem(t *testing.T) {
	ui := &fakeUI{}
	problem := &Problem{Lines: []string{"Could not download runsc", "Check the network and run shard setup again."}}
	fail := func(context.Context) error { return fmt.Errorf("install gVisor: %w", problem) }

	err := (&Setup{UI: ui}).apply(t.Context(), "Setting up", []Step{{"Install gVisor", fail}})

	if !errors.Is(err, problem) {
		t.Fatalf("apply returned %v, want the problem", err)
	}
	if want := []string{"start 0", "fail 0: Could not download runsc / Check the network and run shard setup again."}; !slices.Equal(ui.lists[0].marks, want) {
		t.Errorf("marked %v, want %v", ui.lists[0].marks, want)
	}
}
