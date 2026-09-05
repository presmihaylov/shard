package cli

import (
	"testing"
	"time"
)

func TestParseRmFlags(t *testing.T) {
	opts, err := parseRm([]string{"--force", "--time", "5s", "sandbox1"})
	if err != nil {
		t.Fatalf("parseRm: %v", err)
	}

	if opts.id != "sandbox1" || !opts.force || opts.grace != 5*time.Second {
		t.Errorf("parseRm gave %+v, want sandbox1, force and 5s", opts)
	}
}

func TestParseRmRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":            {},
		"two ids":          {"sandbox1", "sandbox2"},
		"a flag after id":  {"sandbox1", "--force"},
		"a negative grace": {"--time", "-5s", "sandbox1"},
		"an unknown flag":  {"--recursive", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseRm(args); err == nil {
			t.Errorf("parseRm(%s) returned no error", name)
		}
	}
}
