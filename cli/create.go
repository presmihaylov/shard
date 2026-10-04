package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/client"
	"github.com/presmihaylov/shard/services/sandbox"
)

// create asks the daemon for a sandbox that runs only shard-init, and prints the id; the pull and start happen there, so the wait has no bound.
func (a App) create(ctx context.Context, args []string) error {
	req, err := parseCreate(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := a.createAndWait(ctx, c, req)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// createAndWait blocks while the daemon creates in the background, so an operator sees the pull, then a ready sandbox or the reason it failed.
func (a App) createAndWait(ctx context.Context, c *client.Client, req sandbox.CreateRequest) (models.Sandbox, error) {
	sb, err := c.CreateSandboxAndWait(ctx, req, a.pullProgress())
	if err != nil {
		return models.Sandbox{}, err
	}
	if sb.State == models.StateFailed {
		return models.Sandbox{}, fmt.Errorf("sandbox %s failed to start: %s", sb.ID, sb.FailedReason)
	}

	return sb, nil
}

// parseCreate splits the flags and the image, and refuses a typo before the daemon is asked.
func parseCreate(args []string) (sandbox.CreateRequest, error) {
	var req sandbox.CreateRequest

	flags := newFlags("create")
	sandboxFlags(flags, &req)
	flags.StringVar(&req.Snapshot, "snapshot", "", "")
	var refused error
	runFlags(flags, &refused)

	if err := parseVerb(flags, args); err != nil {
		return sandbox.CreateRequest{}, err
	}
	if refused != nil {
		return sandbox.CreateRequest{}, refused
	}
	if err := checkSandbox(flags, req); err != nil {
		return sandbox.CreateRequest{}, err
	}

	rest := flags.Args()
	if len(rest) > 1 {
		return sandbox.CreateRequest{}, errors.New("create takes no command: shard run [flags] <image> <command> [args...]")
	}
	if req.Snapshot != "" && len(rest) == 1 {
		return sandbox.CreateRequest{}, fmt.Errorf("create takes an image or --snapshot, never both: snapshot %s already names its image", req.Snapshot)
	}
	if req.Snapshot != "" {
		return req, nil
	}
	if len(rest) == 0 {
		return sandbox.CreateRequest{}, errors.New("create takes one image reference or --snapshot <id|name>, got neither")
	}

	req.Image = rest[0]

	return req, nil
}

// sandboxFlags are the flags create and run share: everything about the sandbox, nothing about an app.
func sandboxFlags(flags *flag.FlagSet, req *sandbox.CreateRequest) {
	flags.StringVar(&req.Name, "name", "", "")
	flags.Var((*envList)(&req.Env), "env", "")
	flags.Var((*secretList)(&req.Secrets), "secret", "")
	flags.StringVar(&req.Policy, "policy", "", "")
	flags.StringVar(&req.WorkDir, "workdir", "", "")
	flags.StringVar(&req.User, "user", "", "")
	flags.Var(optionalMiB{&req.Resources.MemoryMiB}, "memory", "")
	flags.Var((*cpuCount)(&req.Resources.VCPUs), "cpus", "")
	flags.Var(sizeMiB{&req.Resources.DiskMiB}, "disk", "")
}

// checkSandbox refuses what sandboxFlags parsed and the daemon would refuse only after a pull.
func checkSandbox(flags *flag.FlagSet, req sandbox.CreateRequest) error {
	// The spelling is checked here, so a name no verb could take back never costs the operator a pull.
	if named(flags) {
		if err := sandbox.ValidName(req.Name); err != nil {
			return err
		}
	}

	// A bound this large overflows the byte count it is turned into, and an overflow reads as unbounded.
	if req.Resources.MemoryMiB != nil && *req.Resources.MemoryMiB > sandbox.MaxMemoryMiB {
		return fmt.Errorf("--memory is a bound in MiB and no host holds that much, got %d", *req.Resources.MemoryMiB)
	}
	if req.Resources.VCPUs < 0 {
		return fmt.Errorf("--cpus is a bound and cannot be negative, got %d", req.Resources.VCPUs)
	}
	if req.Resources.DiskMiB > sandbox.MaxDiskMiB {
		return fmt.Errorf("--disk is a bound in MiB and no host holds that much, got %d", req.Resources.DiskMiB)
	}

	if req.Policy != "" {
		if err := sandbox.ValidPolicyName(req.Policy); err != nil {
			return err
		}
	}

	// An --env of the same name would either hide the placeholder or be hidden by it, and either is a surprise.
	for _, entry := range req.Env {
		key, _, _ := strings.Cut(entry, "=")
		if slices.Contains(req.Secrets, key) {
			return fmt.Errorf("--secret %s and --env %s name the same variable: the guest gets the placeholder as $%s, so drop the --env", key, key, key)
		}
	}

	return nil
}

// restartFlagNames are the flags of the restart policy, which only run takes, because only a run has an app.
var restartFlagNames = []string{"restart", "restart-retries", "restart-backoff"}

// runFlags takes the restart flags on a verb that has no app, and keeps the refusal that points to run.
func runFlags(flags *flag.FlagSet, refused *error) {
	for _, name := range restartFlagNames {
		flags.Var(runFlag{name: name, refusal: refused}, name, "")
	}
}

// runFlag is one restart flag given to create or exec.
type runFlag struct {
	name    string
	refusal *error
}

func (r runFlag) String() string { return "" }

func (r runFlag) refused() {}

func (r runFlag) Set(value string) error {
	if *r.refusal == nil {
		*r.refusal = fmt.Errorf("--%s is a run flag: shard run --%s %s <image> <command>", r.name, r.name, value)
	}

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

// cpuCount refuses a fraction by name, since vz hands a VM whole cpus and a rounded bound is not the one asked for.
type cpuCount int

func (c *cpuCount) String() string { return strconv.Itoa(int(*c)) }

func (c *cpuCount) Set(value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return errors.New("want a whole number of cpus; a fraction is never rounded")
	}
	*c = cpuCount(n)

	return nil
}

// envList refuses anything that is not an assignment, because a merge drops such an entry and
// create would then report success with the variable absent.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }

func (e *envList) Set(value string) error {
	key, _, found := strings.Cut(value, "=")
	if !found {
		return errors.New("want KEY=VALUE")
	}
	if key == "" {
		return errors.New("want a name before the =")
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
		return errors.New("the same secret was given twice")
	}

	*s = append(*s, value)

	return nil
}
