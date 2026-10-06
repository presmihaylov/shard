package cli

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

func TestParseRunTheGoalCommand(t *testing.T) {
	opts, err := parseRun([]string{"python:3.12", "python", "-c", "print(1)"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}

	if opts.req.Image != "python:3.12" || opts.detach {
		t.Errorf("parseRun = %+v, want python:3.12 attached", opts)
	}

	if want := []string{"python", "-c", "print(1)"}; !slices.Equal(opts.req.Command, want) {
		t.Errorf("argv = %v, want %v", opts.req.Command, want)
	}
}

func TestParseRunDetachesOnEitherSpelling(t *testing.T) {
	for _, flag := range []string{"-d", "--detach"} {
		opts, err := parseRun([]string{flag, "alpine:3.20", "sleep", "60"})
		if err != nil {
			t.Fatalf("parseRun(%s): %v", flag, err)
		}
		if !opts.detach {
			t.Errorf("parseRun(%s) = %+v, want it detached", flag, opts)
		}
	}
}

// A sandbox with no app is create's, so run with no command says what it misses rather than run nothing.
func TestParseRunNeedsACommand(t *testing.T) {
	for _, args := range [][]string{{"alpine:3.20"}, {"alpine:3.20", "--"}} {
		_, err := parseRun(args)
		if want := "run needs a command after the image"; err == nil || err.Error() != want {
			t.Errorf("parseRun(%v) = %v, want %q", args, err, want)
		}
	}
}

func TestParseCreateRefusesACommand(t *testing.T) {
	for _, args := range [][]string{{"alpine:3.20", "sleep", "600"}, {"alpine:3.20", "--", "sleep", "600"}} {
		_, err := parseCreate(args)
		if want := "create takes no command: shard run [OPTIONS] IMAGE COMMAND [ARGS...]"; err == nil || err.Error() != want {
			t.Errorf("parseCreate(%v) = %v, want %q", args, err, want)
		}
	}
}

// A snapshot names its own image, so create takes one or the other, and a refusal of neither names both ways.
func TestParseCreateTakesAnImageOrASnapshot(t *testing.T) {
	req, err := parseCreate([]string{"--snapshot", "web-base", "--memory", "512MiB"})
	if err != nil {
		t.Fatalf("parseCreate --snapshot: %v", err)
	}
	if req.Snapshot != "web-base" || req.Image != "" {
		t.Errorf("parseCreate --snapshot = %+v, want the snapshot and no image", req)
	}

	for args, want := range map[string]string{
		"--snapshot web-base alpine:3.20": "create takes an image or --snapshot, never both: snapshot web-base already names its image",
		"":                                "create takes one image reference or --snapshot SNAPSHOT, got none",
	} {
		if _, err := parseCreate(strings.Fields(args)); err == nil || err.Error() != want {
			t.Errorf("parseCreate(%q) = %v, want %q", args, err, want)
		}
	}
}

// run starts an app over an image, so --snapshot is create's alone.
func TestParseRunRefusesASnapshot(t *testing.T) {
	if _, err := parseRun([]string{"--snapshot", "web-base", "alpine:3.20", "sleep", "600"}); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Errorf("parseRun --snapshot = %v, want a refusal of the flag", err)
	}
}

func TestParseCreateFlags(t *testing.T) {
	args := []string{
		"--env", "A=1", "--env", "B=2",
		"--workdir", "/srv", "--user", "nobody",
		"--memory", "512MiB", "--vcpus", "2", "--disk", "64MiB",
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

	if memoryOf(req) != 512 || req.Resources.VCPUs != 2 || req.Resources.DiskMiB != 64 {
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
	if memoryOf(req) != 512 || req.Resources.DiskMiB != 2048 {
		t.Errorf("resources = %+v, want 512 MiB of memory and a 2048 MiB disk", req.Resources)
	}

	if req, err = parseCreate([]string{"--memory", "16384GiB", "--disk", "1KB", "alpine:3.20"}); err != nil {
		t.Fatalf("parseCreate at the memory bound: %v", err)
	}
	if memoryOf(req) != 1<<24 || req.Resources.DiskMiB != 1 {
		t.Errorf("resources = %+v, want the 16777216 MiB bound and a 1KB disk rounded up to 1 MiB", req.Resources)
	}
}

// An omitted --memory stays absent for a snapshot to fill, and --memory 0 is an explicit request for no bound.
func TestParseCreateTellsAnOmittedMemoryFromZero(t *testing.T) {
	req, err := parseCreate([]string{"--snapshot", "base"})
	if err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if req.Resources.MemoryMiB != nil {
		t.Errorf("memory = %d with no --memory, want it absent", *req.Resources.MemoryMiB)
	}

	if req, err = parseCreate([]string{"--snapshot", "base", "--memory", "0"}); err != nil {
		t.Fatalf("parseCreate: %v", err)
	}
	if req.Resources.MemoryMiB == nil || *req.Resources.MemoryMiB != 0 {
		t.Errorf("memory = %v with --memory 0, want an explicit 0", req.Resources.MemoryMiB)
	}
}

func TestInFlagsWordsARefusalInTheFlags(t *testing.T) {
	small := int64(64)
	tests := []struct {
		name    string
		message string
		memory  *int64
		want    string
	}{
		{"no memory on a VM", "provider vz needs resources.memory_mib, as a VM's memory is real memory on the host; set it to 128 MiB or more", nil,
			"provider vz needs --memory, as a VM's memory is real memory on the host; set it to 128 MiB or more, for example --memory 512MiB"},
		{"too little memory", "resources.memory_mib is 64 MiB, under the 128 MiB provider vz needs; set it to 128 MiB or more", &small,
			"--memory is 64 MiB, under the 128 MiB provider vz needs; set it to 128 MiB or more"},
		{"the disk", "the image takes a 900 MiB disk, more than the 512 MiB disk bound; set resources.disk_mib to 900 MiB or more", nil,
			"the image takes a 900 MiB disk, more than the 512 MiB disk bound; set --disk to 900 MiB or more"},
		{"a snapshot's disk stopped unclean", "the snapshot's disk was not stopped clean, so it cannot grow to 4096 MiB; omit resources.disk_mib, or start the sandbox it came from, let its entrypoint exit or end it with shard exec, then stop it and snapshot it again", nil,
			"the snapshot's disk was not stopped clean, so it cannot grow to 4096 MiB; omit --disk, or start the sandbox it came from, let its entrypoint exit or end it with shard exec, then stop it and snapshot it again"},
		{"a snapshot's disk that grows only so far", "the snapshot's disk grows to at most 16384 MiB, as a mount took the room a larger one needs; set resources.disk_mib to 16384 MiB or less", nil,
			"the snapshot's disk grows to at most 16384 MiB, as a mount took the room a larger one needs; set --disk to 16384 MiB or less"},
		{"the cpus", "resources.vcpus is 9, more than the 8 CPUs on this host; set it to 8 or less", nil,
			"--vcpus is 9, more than the 8 CPUs on this host; set it to 8 or less"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req sandbox.CreateRequest
			req.Resources.MemoryMiB = tt.memory
			err := inFlags(&client.APIError{Status: 400, Code: models.CodeInvalidRequest, Message: tt.message}, req)

			var refused *client.APIError
			if !errors.As(err, &refused) || refused.Code != models.CodeInvalidRequest {
				t.Fatalf("inFlags = %v, want an invalid_request APIError", err)
			}
			if refused.Message != tt.want {
				t.Errorf("message = %q, want %q", refused.Message, tt.want)
			}
		})
	}

	other := &client.APIError{Status: 404, Code: models.CodeNotFound, Message: "image resources.memory_mib not found"}
	if err := inFlags(other, sandbox.CreateRequest{}); err.Error() != other.Message {
		t.Errorf("inFlags(not_found) = %v, want it untouched", err)
	}
}

// memoryOf reads an absent --memory as -1, which no flag parses to.
func memoryOf(req sandbox.CreateRequest) int64 {
	if req.Resources.MemoryMiB == nil {
		return -1
	}

	return *req.Resources.MemoryMiB
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

func TestParseRunRestartFlags(t *testing.T) {
	opts, err := parseRun([]string{"--restart", "on-failure", "--restart-retries", "2", "--restart-backoff", "3s", "alpine:3.20", "sleep", "60"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	want := &models.RestartSpec{Policy: models.RestartOnFailure, Retries: 2, Backoff: 3}
	if !reflect.DeepEqual(opts.req.Restart, want) {
		t.Errorf("restart = %+v, want %+v", opts.req.Restart, want)
	}

	opts, err = parseRun([]string{"--restart", "always", "alpine:3.20", "sleep", "60"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	want = &models.RestartSpec{Policy: models.RestartAlways}
	if !reflect.DeepEqual(opts.req.Restart, want) {
		t.Errorf("restart = %+v, want %+v with the settings left for the daemon's defaults", opts.req.Restart, want)
	}

	opts, err = parseRun([]string{"alpine:3.20", "sleep", "60"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	if opts.req.Restart != nil {
		t.Errorf("restart = %+v, want none when no flag asked for a policy", opts.req.Restart)
	}
}

// Only a run has an app to start again, so create and exec point every restart flag at run, with its value.
func TestCreateAndExecPointARestartFlagToRun(t *testing.T) {
	cases := map[string][]string{
		"--restart is a run flag: shard run --restart always IMAGE COMMAND":             {"create", "--restart", "always", "alpine:3.20"},
		"--restart-retries is a run flag: shard run --restart-retries 2 IMAGE COMMAND":  {"create", "--restart-retries", "2", "alpine:3.20"},
		"--restart-backoff is a run flag: shard run --restart-backoff 5s IMAGE COMMAND": {"create", "--restart-backoff=5s", "alpine:3.20"},
		"--restart is a run flag: shard run --restart on-failure IMAGE COMMAND":         {"exec", "--restart", "on-failure", "web", "true"},
	}

	for want, args := range cases {
		var out bytes.Buffer

		err := newApp(t, &out).Run(t.Context(), args)
		if err == nil || err.Error() != want {
			t.Errorf("%v returned %v, want %q", args, err, want)
		}
	}
}

func TestParseCreateRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":                {},
		"only flags":              {"--user", "nobody"},
		"a command":               {"alpine:3.20", "true"},
		"an unknown flag":         {"--forever", "alpine:3.20"},
		"an env with no value":    {"--env", "DEBUG", "alpine:3.20"},
		"an env with a colon":     {"--env", "DEBUG:1", "alpine:3.20"},
		"an env with no name":     {"--env", "=1", "alpine:3.20"},
		"a negative memory":       {"--memory", "-512", "alpine:3.20"},
		"a memory with no unit":   {"--memory", "512", "alpine:3.20"},
		"a disk with no unit":     {"--disk", "64", "alpine:3.20"},
		"a fraction of a GiB":     {"--memory", "0.5GiB", "alpine:3.20"},
		"a lower-case unit":       {"--disk", "2gib", "alpine:3.20"},
		"a memory past the bound": {"--memory", "16385GiB", "alpine:3.20"},
		// A bound this large wraps the byte count it is turned into, and a wrapped bound reads as unbounded.
		"a memory that overflows": {"--memory", "17592186044416MiB", "alpine:3.20"},
		"a negative cpu bound":    {"--vcpus", "-2", "alpine:3.20"},
		"a negative disk bound":   {"--disk", "-1", "alpine:3.20"},
		"a disk that overflows":   {"--disk", "17592186044416MiB", "alpine:3.20"},
	}

	for name, args := range cases {
		if _, err := parseCreate(args); err == nil {
			t.Errorf("parseCreate(%s) returned no error", name)
		}
	}
}

func TestParseRunRejections(t *testing.T) {
	cases := map[string][]string{
		"no image":                  {},
		"a policy setting alone":    {"--restart-retries", "2", "alpine:3.20", "true"},
		"a negative start count":    {"--restart", "on-failure", "--restart-retries", "-1", "alpine:3.20", "true"},
		"always with a start count": {"--restart", "always", "--restart-retries", "2", "alpine:3.20", "true"},
		"a sub-second backoff":      {"--restart", "always", "--restart-backoff", "500ms", "alpine:3.20", "true"},
		"a memory with no unit":     {"--memory", "512", "alpine:3.20", "true"},
		"a bad secret":              {"--secret", "api_key", "alpine:3.20", "true"},
	}

	for name, args := range cases {
		if _, err := parseRun(args); err == nil {
			t.Errorf("parseRun(%s) returned no error", name)
		}
	}
}

func TestParseCreateNamesAFractionalCPUBound(t *testing.T) {
	for _, value := range []string{"0.5", "1.0"} {
		_, err := parseCreate([]string{"--vcpus", value, "alpine:3.20"})
		if err == nil {
			t.Fatalf("parseCreate(--vcpus %s) returned no error", value)
		}
		for _, want := range []string{"-vcpus", value, "whole number", "never rounded"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("parseCreate(--vcpus %s) = %q, want %q in it", value, err, want)
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

func TestParseRunPreservesArguments(t *testing.T) {
	command := []string{"sh", "-c", "echo ready", "", "--", "--help", "--name", "guest", "-it"}
	for _, separator := range [][]string{nil, {"--"}} {
		args := []string{"--name", "lab", "alpine:3.22"}
		args = append(args, separator...)
		args = append(args, command...)
		opts, err := parseRun(args)
		if err != nil {
			t.Fatalf("parseRun: %v", err)
		}
		if opts.req.Name != "lab" || opts.req.Image != "alpine:3.22" || !slices.Equal(opts.req.Command, command) {
			t.Errorf("parseRun = %+v, want the guest arguments intact", opts.req)
		}
	}
}

func TestParseRunTakesACommandThatStartsWithAHyphen(t *testing.T) {
	opts, err := parseRun([]string{"alpine:3.22", "--guest", "-it"})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	if !slices.Equal(opts.req.Command, []string{"--guest", "-it"}) {
		t.Errorf("command = %v, want the guest command intact", opts.req.Command)
	}
}
