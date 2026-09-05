package cli

import (
	"os"
	"slices"
	"testing"
)

func TestParseCreateTheGoalCommand(t *testing.T) {
	req, err := parseCreate([]string{"python:3.12", "--", "python", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}

	if req.Image != "python:3.12" {
		t.Errorf("ref = %q, want python:3.12", req.Image)
	}

	if want := []string{"python", "-c", "print(1)"}; !slices.Equal(req.Command, want) {
		t.Errorf("argv = %v, want %v", req.Command, want)
	}
}

func TestParseCreateFlags(t *testing.T) {
	args := []string{
		"--env", "A=1", "--env", "B=2",
		"--workdir", "/srv", "--user", "nobody",
		"--memory", "512", "--cpus", "2",
		"alpine:3.20",
	}

	req, err := parseCreate(args)
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}

	if want := []string{"A=1", "B=2"}; !slices.Equal(req.Env, want) {
		t.Errorf("env = %v, want %v", req.Env, want)
	}

	if req.WorkDir != "/srv" || req.User != "nobody" {
		t.Errorf("workdir = %q, user = %q", req.WorkDir, req.User)
	}

	if req.Resources.MemoryMiB != 512 || req.Resources.VCPUs != 2 {
		t.Errorf("resources = %+v, want 512 MiB and 2 vcpus", req.Resources)
	}

	if len(req.Command) != 0 {
		t.Errorf("argv = %v, want the image's own entrypoint", req.Command)
	}
}

func TestInitPathFromEnv(t *testing.T) {
	cases := map[string]struct {
		env   string
		unset bool
		want  string
	}{
		"set":   {env: "/opt/shard-init", want: "/opt/shard-init"},
		"empty": {want: DefaultInitPath},
		"unset": {unset: true, want: DefaultInitPath},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// The set registers the restore, which an unset in the same test then still gets.
			t.Setenv(InitPathEnv, c.env)
			if c.unset {
				if err := os.Unsetenv(InitPathEnv); err != nil {
					t.Fatalf("unset %s: %v", InitPathEnv, err)
				}
			}

			if got := initPathFromEnv(); got != c.want {
				t.Errorf("initPathFromEnv() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseCreateRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":               {},
		"only flags":             {"--user", "nobody"},
		"a flag after the image": {"alpine:3.20", "--user", "nobody"},
		"an empty argv":          {"alpine:3.20", "--"},
		"an unknown flag":        {"--forever", "alpine:3.20"},
		"the old init flag":      {"--shard-init", "/opt/shard-init", "alpine:3.20"},
		"an env with no value":   {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":    {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":    {"--env", "=1", "alpine:3.20"},
		"a negative memory":      {"--memory", "-512", "alpine:3.20"},
		// A bound this large wraps the byte count it is turned into, and a wrapped bound reads as unbounded.
		"a memory that overflows": {"--memory", "17592186044416", "alpine:3.20"},
		"a negative cpu bound":    {"--cpus", "-2", "alpine:3.20"},
	}

	for name, args := range cases {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%s) returned no error", name)
		}
	}
}

func TestParseCreateRefusesABadOrDoubledSecret(t *testing.T) {
	for _, args := range [][]string{
		{"--secret", "api_key", "alpine"},
		{"--secret", "KEY", "--secret", "KEY", "alpine"},
		{"--secret", "KEY", "--env", "KEY=1", "alpine"},
	} {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%v) accepted", args)
		}
	}
}

func TestParseCreateRefusesABadPolicyName(t *testing.T) {
	if _, err := parseCreate([]string{"--policy", "Bad Name", "alpine"}); err == nil {
		t.Error("parseCreate accepted a bad policy name")
	}
}
