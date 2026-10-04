package cli

import "testing"

func TestParseForkFlags(t *testing.T) {
	source, req, err := parseFork([]string{"--name", "web-2", "sandbox1"})
	if err != nil {
		t.Fatalf("parseFork: %v", err)
	}

	if source != "sandbox1" || req.Name != "web-2" {
		t.Errorf("parseFork gave %q and %+v, want sandbox1 and web-2", source, req)
	}
}

func TestParseForkRejections(t *testing.T) {
	cases := map[string][]string{
		"no id":               {},
		"two ids":             {"sandbox1", "sandbox2"},
		"a flag after id":     {"sandbox1", "--name", "web-2"},
		"an empty name":       {"--name", "", "sandbox1"},
		"a name with a space": {"--name", "web 2", "sandbox1"},
		"an unknown flag":     {"--fast", "sandbox1"},
	}

	for name, args := range cases {
		if _, _, err := parseFork(args); err == nil {
			t.Errorf("parseFork(%s) returned no error", name)
		}
	}
}
