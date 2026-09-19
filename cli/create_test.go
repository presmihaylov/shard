package cli

import (
	"os"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/presmihaylov/shard/models"
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
		"--memory", "512", "--cpus", "2", "--disk", "64", "--restart-on-oom",
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

	if req.Resources.MemoryMiB != 512 || req.Resources.VCPUs != 2 || req.Resources.DiskMiB != 64 {
		t.Errorf("resources = %+v, want 512 MiB, 2 vcpus and a 64 MiB disk", req.Resources)
	}
	if !req.RestartOnOOM || req.MaxOOMRestarts != 0 {
		t.Errorf("oom restart = %v with a limit of %d, want asked-for and unlimited", req.RestartOnOOM, req.MaxOOMRestarts)
	}

	if len(req.Command) != 0 {
		t.Errorf("argv = %v, want the image's own entrypoint", req.Command)
	}
}

func TestInitPathFromEnv(t *testing.T) {
	// A Mac daemon installs the guest init it embeds, so nothing on the host names one.
	platformDefault := DefaultInitPath
	if runtime.GOOS == "darwin" {
		platformDefault = ""
	}
	cases := map[string]struct {
		env   string
		unset bool
		want  string
	}{
		"set":   {env: "/opt/shard-init", want: "/opt/shard-init"},
		"empty": {want: platformDefault},
		"unset": {unset: true, want: platformDefault},
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

func TestParseCreateHealthFlags(t *testing.T) {
	req, err := parseCreate([]string{"--health-command", "test -e /ready", "--health-interval", "5s", "--health-timeout", "2s", "--health-retries", "2", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	want := &models.HealthCheck{Command: []string{"/bin/sh", "-c", "test -e /ready"}, Interval: 5, Timeout: 2, Retries: 2}
	if !reflect.DeepEqual(req.Health, want) {
		t.Errorf("health = %+v, want %+v", req.Health, want)
	}

	req, err = parseCreate([]string{"alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if req.Health != nil {
		t.Errorf("health = %+v, want none when no flag asked for a probe", req.Health)
	}
}

func TestParseCreateRestartFlags(t *testing.T) {
	req, err := parseCreate([]string{"--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "3s", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	want := &models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 3}
	if !reflect.DeepEqual(req.Restart, want) {
		t.Errorf("restart = %+v, want %+v", req.Restart, want)
	}

	req, err = parseCreate([]string{"--restart", "always", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	want = &models.RestartSpec{Policy: models.RestartAlways}
	if !reflect.DeepEqual(req.Restart, want) {
		t.Errorf("restart = %+v, want %+v with the settings left for the daemon's defaults", req.Restart, want)
	}

	req, err = parseCreate([]string{"alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if req.Restart != nil {
		t.Errorf("restart = %+v, want none when no flag asked for a policy", req.Restart)
	}

	// A count on --restart-on-oom caps the starts in a row; the bare flag left it unlimited above.
	req, err = parseCreate([]string{"--memory", "64", "--restart-on-oom=3", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if !req.RestartOnOOM || req.MaxOOMRestarts != 3 {
		t.Errorf("oom restart = %v with a limit of %d, want asked-for with a limit of 3", req.RestartOnOOM, req.MaxOOMRestarts)
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
		"the dropped http probe": {"--health-http", "8080/healthz", "alpine:3.20"},
		"an env with no value":   {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":    {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":    {"--env", "=1", "alpine:3.20"},
		"a negative memory":      {"--memory", "-512", "alpine:3.20"},
		// A bound this large wraps the byte count it is turned into, and a wrapped bound reads as unbounded.
		"a memory that overflows":   {"--memory", "17592186044416", "alpine:3.20"},
		"a negative cpu bound":      {"--cpus", "-2", "alpine:3.20"},
		"a negative disk bound":     {"--disk", "-1", "alpine:3.20"},
		"a disk that overflows":     {"--disk", "17592186044416", "alpine:3.20"},
		"a restart with no bound":   {"--restart-on-oom", "alpine:3.20"},
		"a probe setting alone":     {"--health-retries", "2", "alpine:3.20"},
		"a sub-second interval":     {"--health-command", "true", "--health-interval", "500ms", "alpine:3.20"},
		"a negative timeout":        {"--health-command", "true", "--health-timeout", "-1s", "alpine:3.20"},
		"a negative retry count":    {"--health-command", "true", "--health-retries", "-1", "alpine:3.20"},
		"a policy setting alone":    {"--restart-retries", "2", "alpine:3.20"},
		"a negative start count":    {"--restart", "on-failure", "--restart-retries", "-1", "alpine:3.20"},
		"always with a start count": {"--restart", "always", "--restart-retries", "2", "alpine:3.20"},
		"a sub-second backoff":      {"--restart", "always", "--restart-backoff", "500ms", "alpine:3.20"},
		"a negative oom limit":      {"--memory", "64", "--restart-on-oom=-1", "alpine:3.20"},
		"a non-number oom limit":    {"--memory", "64", "--restart-on-oom=lots", "alpine:3.20"},
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
