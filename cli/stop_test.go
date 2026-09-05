package cli

import (
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/sandbox"
)

func TestParseStopFlags(t *testing.T) {
	opts, err := parseStop([]string{"--time", "45s", "sandbox1"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if opts.id != "sandbox1" || opts.grace != 45*time.Second {
		t.Errorf("parseStop gave %+v, want sandbox1 and 45s", opts)
	}
}

func TestParseStopDefaultsTheGrace(t *testing.T) {
	opts, err := parseStop([]string{"sandbox1"})
	if err != nil {
		t.Fatalf("parseStop: %v", err)
	}

	if opts.grace != sandbox.DefaultStopGrace {
		t.Errorf("the grace is %s, want %s", opts.grace, sandbox.DefaultStopGrace)
	}
}

func TestParseStopRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":            {},
		"two ids":          {"sandbox1", "sandbox2"},
		"a flag after id":  {"sandbox1", "--time", "5s"},
		"a negative grace": {"--time", "-5s", "sandbox1"},
		"an unknown flag":  {"--forever", "sandbox1"},
	}

	for name, args := range cases {
		if _, err := parseStop(args); err == nil {
			t.Errorf("parseStop(%s) returned no error", name)
		}
	}
}
