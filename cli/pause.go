package cli

import "context"

// pause asks the daemon to write each sandbox into its checkpoint and prints the id it acted on.
func (a App) pause(ctx context.Context, args []string) error {
	rest, err := parseArgs("pause", args)
	if err != nil {
		return err
	}
	ids, err := sandboxRefs("pause", rest)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	return a.each(ctx, ids, func(ref string) error {
		sb, err := c.PauseSandbox(ctx, ref)
		if err != nil {
			return err
		}

		return a.print(sb.ID)
	})
}
