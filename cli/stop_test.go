package cli

import "testing"

func TestParseStopTakesTheID(t *testing.T) {
	opts, err := parseStop([]string{"sandbox1"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if opts.id != "sandbox1" {
		t.Errorf("parseStop gave %+v, want sandbox1", opts)
	}
}

func TestParseStopRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":           {},
		"two ids":         {"sandbox1", "sandbox2"},
		"a flag after id": {"sandbox1", "-x"},
		"an unknown flag": {"--forever", "sandbox1"},
		// The grace is fixed, so the flag that set it is gone (SHARD-460).
		"the removed --time": {"--time", "45s", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseStop(args); err == nil {
			t.Errorf("parseStop(%s) returned no error", name)
		}
	}
}
