package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
)

// capabilities prints every lifecycle verb and whether the server it asks supports it, the daemon here or the one --remote names.
func (a App) capabilities(ctx context.Context, args []string) error {
	rest, format, err := parseFormatArgs("capabilities", args, formatTable)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("capabilities takes no arguments, got %s", gotArgs(rest))
	}

	c, err := a.client()
	if err != nil {
		return err
	}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	if format == formatJSON {
		return writeJSON(a.Out, caps)
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CAPABILITY\tSUPPORTED")
	for _, verb := range []struct {
		name      string
		supported bool
	}{
		{"create", caps.Create}, {"start", caps.Start}, {"stop", caps.Stop}, {"remove", caps.Remove},
		{"pause", caps.Pause}, {"resume", caps.Resume}, {"fork", caps.Fork}, {"snapshot", caps.Snapshot},
	} {
		fmt.Fprintf(w, "%s\t%t\n", verb.name, verb.supported)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
