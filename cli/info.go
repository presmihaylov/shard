package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/presmihaylov/shard/services/daemon"
)

// info prints the provider a daemon started now over this root with no --provider would pick, and why; it asks the host, not the socket.
func (a App) info(_ context.Context, args []string) error {
	rest, err := parseArgs("info", args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("info takes no argument, got %s", strings.Join(rest, " "))
	}

	selected, err := daemon.SelectProvider("", a.Root)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.Out, 0, 0, 3, ' ', 0)
	fmt.Fprintf(w, "provider\t%s\n", selected.Provider)
	fmt.Fprintf(w, "reason\t%s\n", selected.Reason)
	if selected.Unreadable != "" {
		fmt.Fprintf(w, "unreadable\t%s\n", selected.Unreadable)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write the output: %w", err)
	}

	return nil
}
