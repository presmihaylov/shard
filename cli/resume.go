package cli

import "context"

// resume asks the daemon to run each paused sandbox again from its checkpoint.
func (a App) resume(ctx context.Context, args []string) error {
	rest, err := parseArgs("resume", args)
	if err != nil {
		return err
	}
	ids, err := sandboxRefs("resume", rest)
	if err != nil {
		return err
	}

	c, err := a.client()
	if err != nil {
		return err
	}

	return a.each(ctx, ids, func(ref string) error {
		sb, err := c.ResumeSandbox(ctx, ref)
		if err != nil {
			return err
		}

		return a.print(sb.ID)
	})
}
