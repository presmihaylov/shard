package cli

import "testing"

func TestParseRemoveFlags(t *testing.T) {
	opts, err := parseRemove([]string{"--force", "sandbox1"})
	if err != nil {
		t.Fatalf("parseRemove: %v", err)
	}

	if opts.id != "sandbox1" || !opts.force {
		t.Errorf("parseRemove gave %+v, want sandbox1 and force", opts)
	}
}

func TestParseRemoveRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":           {},
		"two ids":         {"sandbox1", "sandbox2"},
		"a flag after id": {"sandbox1", "--force"},
		"an unknown flag": {"--recursive", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseRemove(args); err == nil {
			t.Errorf("parseRemove(%s) returned no error", name)
		}
	}
}
