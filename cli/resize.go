package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/presmihaylov/shard/services/sandbox"
)

// resize asks the daemon for new bounds on a stopped sandbox, and prints its id.
func (a App) resize(ctx context.Context, args []string) error {
	ref, req, err := parseResize(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.ResizeSandbox(ctx, ref, req)
	if err != nil {
		return inFlags(err)
	}

	return a.print(sb.ID)
}

// parseResize keeps every bound the operator left out nil, so the daemon keeps the sandbox's own.
func parseResize(args []string) (string, sandbox.ResizeRequest, error) {
	var req sandbox.ResizeRequest
	res := &req.Resources

	flags := newFlags("resize")
	flags.Var(optionalMiB{&res.MemoryMiB}, "memory", "")
	flags.Var(optionalCPUs{&res.VCPUs}, "vcpus", "")
	flags.Var(optionalMiB{&res.DiskMiB}, "disk", "")
	flags.Var(optionalMiB{&res.SwapMiB}, "swap", "")

	if err := parseVerb(flags, args); err != nil {
		return "", sandbox.ResizeRequest{}, err
	}
	if flags.NArg() != 1 {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("resize takes one sandbox id or name, got %s", gotArgs(flags.Args()))
	}
	if flags.NFlag() == 0 {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("resize takes at least one of --memory, --vcpus, --disk or --swap")
	}
	if res.MemoryMiB != nil && *res.MemoryMiB > sandbox.MaxMemoryMiB {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("--memory is a bound in MiB and no host holds that much, got %d", *res.MemoryMiB)
	}
	if res.VCPUs != nil && *res.VCPUs < 0 {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("--vcpus is a bound and cannot be negative, got %d", *res.VCPUs)
	}
	if res.DiskMiB != nil && *res.DiskMiB > sandbox.MaxDiskMiB {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("--disk is a bound in MiB and no host holds that much, got %d", *res.DiskMiB)
	}
	if res.SwapMiB != nil && *res.SwapMiB > sandbox.MaxDiskMiB {
		return "", sandbox.ResizeRequest{}, fmt.Errorf("--swap is in MiB and no host holds that much, got %d", *res.SwapMiB)
	}

	return flags.Arg(0), req, nil
}

// optionalCPUs is --vcpus that stays nil until it is given, so an explicit 0 differs from an omitted flag.
type optionalCPUs struct{ n **int }

func (o optionalCPUs) String() string {
	if o.n == nil || *o.n == nil {
		return ""
	}

	return strconv.Itoa(**o.n)
}

func (o optionalCPUs) Set(value string) error {
	var c cpuCount
	if err := c.Set(value); err != nil {
		return err
	}
	n := int(c)
	*o.n = &n

	return nil
}
