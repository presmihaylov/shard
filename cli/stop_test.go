package cli

import (
	"slices"
	"testing"
)

func TestParseStopTakesEveryID(t *testing.T) {
	opts, err := parseStop([]string{"sandbox1", "sandbox2"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if !slices.Equal(opts.ids, []string{"sandbox1", "sandbox2"}) {
		t.Errorf("parseStop gave %+v, want both sandboxes", opts)
	}
}

// After a -- every argument is a sandbox, the ones that look like flags too.
func TestParseStopTakesANameThatLooksLikeAFlagAfterADoubleDash(t *testing.T) {
	opts, err := parseStop([]string{"web", "--", "-x"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if !slices.Equal(opts.ids, []string{"web", "-x"}) {
		t.Errorf("parseStop gave %+v, want web and -x", opts)
	}
}

func TestParseStopRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":           {},
		"a flag after id": {"sandbox1", "-x"},
		"an unknown flag": {"--forever", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseStop(args); err == nil {
			t.Errorf("parseStop(%s) returned no error", name)
		}
	}
}
