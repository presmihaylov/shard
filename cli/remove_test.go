package cli

import (
	"slices"
	"testing"
)

func TestParseRemoveFlags(t *testing.T) {
	opts, err := parseRemove([]string{"--force", "sandbox1", "sandbox2"})
	if err != nil {
		t.Fatalf("parseRemove: %v", err)
	}

	if !slices.Equal(opts.ids, []string{"sandbox1", "sandbox2"}) || !opts.force {
		t.Errorf("parseRemove gave %+v, want both sandboxes and force", opts)
	}
}

func TestParseRemoveRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":            {},
		"a flag after id":  {"sandbox1", "--force"},
		"a flag after two": {"sandbox1", "sandbox2", "--force"},
		"an unknown flag":  {"--recursive", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseRemove(args); err == nil {
			t.Errorf("parseRemove(%s) returned no error", name)
		}
	}
}
