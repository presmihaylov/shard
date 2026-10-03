package cli

import (
	"context"
	"fmt"

	"github.com/presmihaylov/shard/services/sandbox"
)

// fork asks the daemon for a new sandbox from a capture of a running one, and prints the new id.
func (a App) fork(ctx context.Context, args []string) error {
	source, req, err := parseCopy("fork", args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	sb, err := c.ForkSandbox(ctx, source, req)
	if err != nil {
		return err
	}

	return a.print(sb.ID)
}

// parseCopy reads the one flag fork and clone share, and refuses a name no verb could take back.
func parseCopy(verb string, args []string) (string, sandbox.CopyRequest, error) {
	var req sandbox.CopyRequest

	flags := newFlags(verb)
	flags.StringVar(&req.Name, "name", "", "")

	if err := parseVerb(flags, args); err != nil {
		return "", sandbox.CopyRequest{}, err
	}
	if named(flags) {
		if err := sandbox.ValidName(req.Name); err != nil {
			return "", sandbox.CopyRequest{}, err
		}
	}
	if flags.NArg() != 1 {
		return "", sandbox.CopyRequest{}, fmt.Errorf("%s takes one sandbox id, got %d", verb, flags.NArg())
	}

	return flags.Arg(0), req, nil
}
