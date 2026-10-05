package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestRunAsksLocalOrRemoteFirst(t *testing.T) {
	ui := &fakeUI{}
	err := (&Setup{UI: ui}).Run(t.Context())
	if !errors.Is(err, errUnscripted) {
		t.Fatalf("Run without an answer: %v", err)
	}

	if !slices.Equal(ui.asked, []Question{AskMode}) {
		t.Errorf("asked %v, want only %s", ui.asked, AskMode)
	}
	var names []string
	for _, o := range ui.options[AskMode] {
		names = append(names, o.Name)
	}
	if !slices.Equal(names, []string{"local", "remote"}) || !ui.options[AskMode][0].Default {
		t.Errorf("the first choice offers %v, want local (the default) then remote", names)
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
	if want := []string{"", "Setup stopped. Earlier completed steps remain in place.", "Run `shard setup` again to retry."}; !slices.Equal(ui.printed, want) {
		t.Errorf("printed %q, want %q", ui.printed, want)
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
