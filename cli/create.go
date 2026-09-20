package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/sandbox"
)

// create asks the daemon for a sandbox and prints the id; the pull and start happen there, so the wait has no bound.
func (a App) create(ctx context.Context, args []string) error {
	req, err := parseCreate(args)
	if err != nil {
		return err
	}

	sb, err := a.client().CreateSandbox(ctx, req)
	if err != nil {
		return err
	}

	// The daemon creates in the background; the CLI blocks, so an operator sees a ready sandbox or the reason it failed.
	final, err := a.client().WaitSandbox(ctx, sb.ID)
	if err != nil {
		return err
	}
	if final.State == models.StateFailed {
		return fmt.Errorf("sandbox %s failed to start: %s", final.ID, final.FailedReason)
	}

	return a.print(sb.ID)
}

// parseCreate splits the flags, the image and the argv, and refuses a typo before the daemon is asked.
func parseCreate(args []string) (sandbox.CreateRequest, error) {
	var req sandbox.CreateRequest
	var err error

	flags := flag.NewFlagSet("shard create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&req.Name, "name", "", "a handle every verb takes in place of the id")
	flags.Var((*envList)(&req.Env), "env", "an environment variable as KEY=VALUE, repeatable")
	flags.Var((*secretList)(&req.Secrets), "secret", "a stored secret the guest gets a placeholder for, repeatable")
	flags.StringVar(&req.Policy, "policy", "", "the egress policy the host enforces")
	flags.StringVar(&req.WorkDir, "workdir", "", "the directory the entrypoint starts in")
	flags.StringVar(&req.User, "user", "", "the user the entrypoint runs as")
	flags.Int64Var(&req.Resources.MemoryMiB, "memory", 0, "the memory bound in MiB; 0 is unbounded on Linux and 512 on vz, the VM's memory")
	flags.IntVar(&req.Resources.VCPUs, "cpus", 0, "the vcpu bound; 0 is every host cpu")
	flags.Int64Var(&req.Resources.DiskMiB, "disk", 0, "the disk bound in MiB over the writable layer and /tmp, 0 for the default")
	flags.Var(oomRestartFlag{enabled: &req.RestartOnOOM, max: &req.MaxOOMRestarts}, "restart-on-oom", "start the sandbox again when the host ends it for its memory, never on vz; bare is unlimited, =N caps the starts in a row")
	var restart restartFlags
	flags.StringVar(&restart.policy, "restart", "", "when the entrypoint is started again inside the sandbox: no, on-failure or always")
	flags.IntVar(&restart.retries, "restart-retries", 0, "the starts again before the supervisor gives up")
	flags.DurationVar(&restart.backoff, "restart-backoff", 0, "the wait before the first start again, in whole seconds; it doubles each time")
	var health healthFlags
	flags.StringVar(&health.command, "health-command", "", "a shell command the daemon runs in the sandbox, which passes on exit 0")
	flags.DurationVar(&health.interval, "health-interval", 0, "the time between two probes, in whole seconds")
	flags.DurationVar(&health.timeout, "health-timeout", 0, "the time one probe gets to answer, in whole seconds")
	flags.IntVar(&health.retries, "health-retries", 0, "the failed probes in a row that make the sandbox unhealthy")

	if err := parseVerb(flags, args); err != nil {
		return sandbox.CreateRequest{}, fmt.Errorf("parse the create flags: %w", err)
	}

	if req.Health, err = health.request(); err != nil {
		return sandbox.CreateRequest{}, err
	}
	if req.Restart, err = restart.request(); err != nil {
		return sandbox.CreateRequest{}, err
	}

	// The spelling is checked here, so a name no verb could take back never costs the operator a pull.
	if named(flags) {
		if err := sandbox.ValidName(req.Name); err != nil {
			return sandbox.CreateRequest{}, err
		}
	}

	// A bound below zero is not a spelling of unbounded, and the substrate would drop it without a word.
	if req.Resources.MemoryMiB < 0 {
		return sandbox.CreateRequest{}, fmt.Errorf("--memory is a bound in MiB and cannot be negative, got %d", req.Resources.MemoryMiB)
	}
	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if req.Resources.MemoryMiB > sandbox.MaxMemoryMiB {
		return sandbox.CreateRequest{}, fmt.Errorf("--memory is a bound in MiB and no host holds that much, got %d", req.Resources.MemoryMiB)
	}
	if req.Resources.VCPUs < 0 {
		return sandbox.CreateRequest{}, fmt.Errorf("--cpus is a bound and cannot be negative, got %d", req.Resources.VCPUs)
	}
	if req.Resources.DiskMiB < 0 {
		return sandbox.CreateRequest{}, fmt.Errorf("--disk is a bound in MiB and cannot be negative, got %d", req.Resources.DiskMiB)
	}
	if req.Resources.DiskMiB > sandbox.MaxDiskMiB {
		return sandbox.CreateRequest{}, fmt.Errorf("--disk is a bound in MiB and no host holds that much, got %d", req.Resources.DiskMiB)
	}
	// Only a bound can be run out of: the host never counts an OOM against a sandbox that has none.
	if req.RestartOnOOM && req.Resources.MemoryMiB == 0 {
		return sandbox.CreateRequest{}, errors.New("--restart-on-oom needs a memory bound, set --memory")
	}

	if req.Policy != "" {
		if err := sandbox.ValidPolicyName(req.Policy); err != nil {
			return sandbox.CreateRequest{}, err
		}
	}

	// An --env of the same name would either hide the placeholder or be hidden by it, and either is a surprise.
	for _, entry := range req.Env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(req.Secrets, key) {
			return sandbox.CreateRequest{}, fmt.Errorf("--secret %s and --env %s name the same variable: the guest gets the placeholder as $%s, so drop the --env", key, key, key)
		}
	}

	rest := flags.Args()
	if len(rest) == 0 {
		return sandbox.CreateRequest{}, errors.New("create takes one image reference, got none")
	}

	req.Image, rest = rest[0], rest[1:]
	if len(rest) == 0 {
		return req, nil
	}

	if rest[0] != "--" {
		return sandbox.CreateRequest{}, fmt.Errorf("unexpected argument %q: the flags go before the image and the command after --", rest[0])
	}

	req.Command = rest[1:]
	if len(req.Command) == 0 {
		return sandbox.CreateRequest{}, errors.New("-- takes the command to run, and nothing followed it")
	}

	return req, nil
}

// healthFlags is the probe as the flags spell it, before the daemon's seconds and argv.
type healthFlags struct {
	command           string
	interval, timeout time.Duration
	retries           int
}

// request turns the flags into the create body's probe, or nil when no command names one.
func (h healthFlags) request() (*models.HealthCheck, error) {
	if h.command == "" {
		if h.interval != 0 || h.timeout != 0 || h.retries != 0 {
			return nil, errors.New("--health-interval, --health-timeout and --health-retries tune a probe, set --health-command")
		}

		return nil, nil
	}

	hc := &models.HealthCheck{Command: []string{"/bin/sh", "-c", h.command}, Retries: h.retries}
	var err error
	if hc.Interval, err = wholeSeconds("--health-interval", h.interval); err != nil {
		return nil, err
	}
	if hc.Timeout, err = wholeSeconds("--health-timeout", h.timeout); err != nil {
		return nil, err
	}
	if h.retries < 0 {
		return nil, fmt.Errorf("--health-retries is a count and cannot be negative, got %d", h.retries)
	}

	return hc, nil
}

// oomRestartFlag reads --restart-on-oom as a bare bool, which is unlimited, or as =N, which caps the starts in a row.
type oomRestartFlag struct {
	enabled *bool
	max     *int
}

func (o oomRestartFlag) String() string { return "" }

// IsBoolFlag lets the bare flag parse with no value, which then means unlimited.
func (o oomRestartFlag) IsBoolFlag() bool { return true }

func (o oomRestartFlag) Set(value string) error {
	*o.enabled = true
	if value == "true" {
		*o.max = 0

		return nil
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("--restart-on-oom takes a count, got %q", value)
	}
	if n < 0 {
		return fmt.Errorf("--restart-on-oom is a count and cannot be negative, got %d", n)
	}
	*o.max = n

	return nil
}

// restartFlags is the policy as the flags spell it, before the daemon's seconds.
type restartFlags struct {
	policy  string
	retries int
	backoff time.Duration
}

// request turns the flags into the create body's policy, or nil when none names one.
func (r restartFlags) request() (*models.RestartSpec, error) {
	if r.policy == "" {
		if r.retries != 0 || r.backoff != 0 {
			return nil, errors.New("--restart-retries and --restart-backoff tune a policy, set --restart")
		}

		return nil, nil
	}

	if r.retries < 0 {
		return nil, fmt.Errorf("--restart-retries is a count and cannot be negative, got %d", r.retries)
	}
	if models.RestartPolicy(r.policy) == models.RestartAlways && r.retries != 0 {
		return nil, errors.New("--restart always never gives up, so it takes no --restart-retries")
	}

	spec := &models.RestartSpec{Policy: models.RestartPolicy(r.policy), Retries: r.retries}
	var err error
	if spec.Backoff, err = wholeSeconds("--restart-backoff", r.backoff); err != nil {
		return nil, err
	}

	return spec, nil
}

// wholeSeconds refuses what the daemon's seconds cannot carry; zero stays zero and takes the default.
func wholeSeconds(name string, d time.Duration) (int, error) {
	if d < 0 || d%time.Second != 0 {
		return 0, fmt.Errorf("%s is in whole seconds, got %s", name, d)
	}

	return int(d / time.Second), nil
}

// named says --name was given, so an explicit empty one is refused rather than read as no name.
func named(flags *flag.FlagSet) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			set = true
		}
	})

	return set
}

// envList refuses anything that is not an assignment, because a merge drops such an entry and
// create would then report success with the variable absent.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }

func (e *envList) Set(value string) error {
	key, _, found := strings.Cut(value, "=")
	if !found {
		return fmt.Errorf("%q is not KEY=VALUE", value)
	}
	if key == "" {
		return fmt.Errorf("%q has no name", value)
	}

	*e = append(*e, value)

	return nil
}

// secretList refuses a name the store could not hold, so a typo is caught before anything is pulled.
type secretList []string

func (s *secretList) String() string { return strings.Join(*s, ",") }

func (s *secretList) Set(value string) error {
	if err := sandbox.ValidSecretName(value); err != nil {
		return err
	}
	if slices.Contains(*s, value) {
		return fmt.Errorf("--secret %s was given twice", value)
	}

	*s = append(*s, value)

	return nil
}
