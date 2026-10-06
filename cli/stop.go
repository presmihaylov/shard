package cli

import "context"

// stopOptions is one parsed shard stop invocation.
type stopOptions struct {
	ids []string
}

// stop asks the daemon to end each sandbox and prints the id it acted on, which is never the name typed.
func (a App) stop(ctx context.Context, args []string) error {
	opts, err := parseStop(args)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	return a.each(ctx, opts.ids, func(ref string) error {
		sb, err := c.StopSandbox(ctx, ref)
		if err != nil {
			return err
		}

		return a.print(sb.ID)
	})
}

func parseStop(args []string) (stopOptions, error) {
	var opts stopOptions

	flags := newFlags("stop")

	if err := parseVerb(flags, args); err != nil {
		return stopOptions{}, err
	}

	ids, err := sandboxRefs("stop", flags.Args())
	if err != nil {
		return stopOptions{}, err
	}
	opts.ids = ids

	return opts, nil
}
