package cli

import "testing"

func TestParseRmFlags(t *testing.T) {
	opts, err := parseRm([]string{"--force", "sandbox1"})
	if err != nil {
		t.Fatalf("parseRm: %v", err)
	}

	if opts.id != "sandbox1" || !opts.force {
		t.Errorf("parseRm gave %+v, want sandbox1 and force", opts)
	}
}

func TestParseRmRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":           {},
		"two ids":         {"sandbox1", "sandbox2"},
		"a flag after id": {"sandbox1", "--force"},
		"an unknown flag": {"--recursive", "sandbox1"},
		// --force stops with the fixed grace, so the flag that set it is gone (SHARD-460).
		"the removed --time": {"--force", "--time", "5s", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseRm(args); err == nil {
			t.Errorf("parseRm(%s) returned no error", name)
		}
	}
}
