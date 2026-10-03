package cli

import (
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

func TestParseCreateTheGoalCommand(t *testing.T) {
	req, err := parseCreate([]string{"python:3.12", "python", "-c", "print(1)"})
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
		"--memory", "512", "--cpus", "2", "--disk", "64",
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

	if len(req.Command) != 0 {
		t.Errorf("argv = %v, want none: the image's own ENTRYPOINT and CMD never run", req.Command)
	}
}

func TestParseCreateTakesASizeWithAUnit(t *testing.T) {
	req, err := parseCreate([]string{"--memory", "512MiB", "--disk", "2GiB", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if req.Resources.MemoryMiB != 512 || req.Resources.DiskMiB != 2048 {
		t.Errorf("resources = %+v, want 512 MiB of memory and a 2048 MiB disk", req.Resources)
	}

	if req, err = parseCreate([]string{"--memory", "16384GiB", "--disk", "1KB", "alpine:3.20"}); err != nil {
		t.Fatalf("parseCreate at the memory bound: %v", err)
	}
	if req.Resources.MemoryMiB != 1<<24 || req.Resources.DiskMiB != 1 {
		t.Errorf("resources = %+v, want the 16777216 MiB bound and a 1KB disk rounded up to 1 MiB", req.Resources)
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

func TestParseCreateRestartFlags(t *testing.T) {
	req, err := parseCreate([]string{"--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "3s", "alpine:3.20", "sleep", "60"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	want := &models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 3}
	if !reflect.DeepEqual(req.Restart, want) {
		t.Errorf("restart = %+v, want %+v", req.Restart, want)
	}

	req, err = parseCreate([]string{"--restart", "always", "alpine:3.20", "sleep", "60"})
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
}

// The image's own command never runs, so a policy with no command after the image is refused by its flag.
func TestParseCreateRefusesARestartPolicyWithNoCommand(t *testing.T) {
	for _, flags := range [][]string{{"--restart", "always"}, {"--restart", "on-failure", "--restart-retries", "3"}} {
		_, err := parseCreate(append(flags, "alpine:3.20"))
		if err == nil || !strings.Contains(err.Error(), "--restart needs a command") {
			t.Errorf("parseCreate(%v) with no command = %v, want the refusal naming --restart", flags, err)
		}
	}

	req, err := parseCreate([]string{"--restart", "no", "alpine:3.20"})
	if err != nil {
		t.Fatalf("parseCreate(--restart no) with no command: %v", err)
	}
	if req.Restart.Set() {
		t.Errorf("restart = %+v, want no policy", req.Restart)
	}
}

func TestParseCreateRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":                {},
		"only flags":              {"--user", "nobody"},
		"an empty argv":           {"alpine:3.20", "--"},
		"an unknown flag":         {"--forever", "alpine:3.20"},
		"an env with no value":    {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":     {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":     {"--env", "=1", "alpine:3.20"},
		"a negative memory":       {"--memory", "-512", "alpine:3.20"},
		"a fraction of a GiB":     {"--memory", "0.5GiB", "alpine:3.20"},
		"a lower-case unit":       {"--disk", "2gib", "alpine:3.20"},
		"a memory past the bound": {"--memory", "16385GiB", "alpine:3.20"},
		// A bound this large wraps the byte count it is turned into, and a wrapped bound reads as unbounded.
		"a memory that overflows":   {"--memory", "17592186044416", "alpine:3.20"},
		"a negative cpu bound":      {"--cpus", "-2", "alpine:3.20"},
		"a negative disk bound":     {"--disk", "-1", "alpine:3.20"},
		"a disk that overflows":     {"--disk", "17592186044416", "alpine:3.20"},
		"a policy setting alone":    {"--restart-retries", "2", "alpine:3.20"},
		"a negative start count":    {"--restart", "on-failure", "--restart-retries", "-1", "alpine:3.20"},
		"always with a start count": {"--restart", "always", "--restart-retries", "2", "alpine:3.20"},
		"a sub-second backoff":      {"--restart", "always", "--restart-backoff", "500ms", "alpine:3.20"},
	}

	for name, args := range cases {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%s) returned no error", name)
		}
	}
}

func TestParseCreateNamesAFractionalCPUBound(t *testing.T) {
	for _, value := range []string{"0.5", "1.0"} {
		_, err := parseCreate([]string{"--cpus", value, "alpine:3.20"})
		if err == nil {
			t.Fatalf("parseCreate(--cpus %s) returned no error", value)
		}
		for _, want := range []string{"-cpus", value, "whole number", "never rounded"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("parseCreate(--cpus %s) = %q, want %q in it", value, err, want)
			}
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

func TestParseCreatePreservesArguments(t *testing.T) {
	command := []string{"sh", "-c", "echo ready", "", "--", "--help", "--name", "guest", "-it"}
	for _, separator := range [][]string{nil, {"--"}} {
		args := []string{"--name", "lab", "alpine:3.22"}
		args = append(args, separator...)
		args = append(args, command...)
		req, err := parseCreate(args)
		if err != nil {
			t.Fatalf("parseCreate: %v", err)
		}
		if req.Name != "lab" || req.Image != "alpine:3.22" || !slices.Equal(req.Command, command) {
			t.Errorf("parseCreate = %+v, want the guest arguments intact", req)
		}
	}
}

func TestParseCreateTakesACommandThatStartsWithAHyphen(t *testing.T) {
	req, err := parseCreate([]string{"alpine:3.22", "--guest", "-it"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if !slices.Equal(req.Command, []string{"--guest", "-it"}) {
		t.Errorf("command = %v, want the guest command intact", req.Command)
	}
}
